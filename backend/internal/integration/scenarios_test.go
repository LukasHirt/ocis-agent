//go:build integration && unix

package integration

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// jobTimeout bounds how long a scenario waits for a job: the fake model
// answers at once, so this is the worker's claim, oCIS calls and polls.
const jobTimeout = 60 * time.Second

type statusDoc struct {
	Enrolled    bool   `json:"enrolled"`
	Paused      bool   `json:"paused"`
	PauseReason string `json:"pause_reason"`
	User        struct {
		ID    string `json:"id"`
		Login string `json:"login"`
	} `json:"user"`
	Token struct {
		Status    string  `json:"status"`
		ExpiresAt *string `json:"expires_at"`
	} `json:"token"`
	PendingApprovals int `json:"pending_approvals"`
}

type jobDoc struct {
	ID     string `json:"id"`
	State  string `json:"state"`
	Error  string `json:"error"`
	Answer string `json:"answer"`
	Steps  []struct {
		Kind   string `json:"kind"`
		Tool   string `json:"tool"`
		Status string `json:"status"`
	} `json:"steps"`
}

type approvalDoc struct {
	ID       string          `json:"id"`
	JobID    string          `json:"job_id"`
	Tool     string          `json:"tool"`
	Risk     string          `json:"risk"`
	Args     json.RawMessage `json:"args"`
	ArgsHash string          `json:"args_hash"`
	State    string          `json:"state"`
	Outcome  *struct {
		State string `json:"state"`
	} `json:"outcome"`
}

// TestDevStack runs the scenarios against one sidecar. Each scenario enrols
// (if it needs to) and cleans up after itself, so they do not depend on each
// other's order or outcome.
func TestDevStack(t *testing.T) {
	h := startHarness(t)

	t.Run("enrol and list files through the app token", func(t *testing.T) { scenarioEnrol(t, h) })
	t.Run("chat answer and events", func(t *testing.T) { scenarioChat(t, h) })
	t.Run("approval approve, reject and stale hash", func(t *testing.T) { scenarioApproval(t, h) })
	t.Run("pause and resume", func(t *testing.T) { scenarioPause(t, h) })
	t.Run("token renewal", func(t *testing.T) { scenarioRenew(t, h) })
	t.Run("unenrol", func(t *testing.T) { scenarioUnenrol(t, h) })
	t.Run("audit verify and export", func(t *testing.T) { scenarioAudit(t, h) })
	t.Run("memory edit and conflict", func(t *testing.T) { scenarioMemory(t, h) })

	if errs := h.llm.errors(); len(errs) > 0 {
		t.Errorf("the fake model got requests it could not answer: %v", errs)
	}
}

// --- helpers ---

func marker() string { return "[it-" + randomString(10) + "]" }

func (h *harness) status(t *testing.T) statusDoc {
	t.Helper()
	var s statusDoc
	h.expect(t, http.StatusOK, http.MethodGet, "/status", nil, &s)
	return s
}

// enrol enrols the demo user and registers the unenrolment as cleanup (which
// also checks that oCIS holds no token of the sidecar afterwards).
func (h *harness) enrol(t *testing.T) statusDoc {
	t.Helper()
	r := h.callRetry(t, http.MethodPost, "/enroll", map[string]any{})
	if r.status != http.StatusCreated && r.status != http.StatusOK {
		t.Fatalf("enrol = %s", r)
	}
	t.Cleanup(func() { h.unenrolCleanup(t) })
	var s statusDoc
	if err := json.Unmarshal(r.body, &s); err != nil {
		t.Fatal(err)
	}
	// the prefix recorded from the database must be the one the sidecar
	// marks as its own; the README the first enrolment created is noted
	h.learnPrefix(t)
	h.noteReadme(t)
	return s
}

func (h *harness) unenrolCleanup(t *testing.T) {
	t.Helper()
	r := h.call(t, http.MethodPost, "/unenroll", map[string]any{}, nil)
	if r.status != http.StatusNoContent && r.status != http.StatusNotFound {
		t.Errorf("unenrol in the cleanup = %s", r)
	}
	if n, err := h.deleteOurTokens(); err != nil || n > 0 {
		t.Errorf("after the unenrolment %d app token(s) of the sidecar were still in oCIS (%v)", n, err)
	}
}

func (h *harness) conversation(t *testing.T) string {
	t.Helper()
	var c struct {
		ID string `json:"id"`
	}
	h.expect(t, http.StatusCreated, http.MethodPost, "/conversations", map[string]any{}, &c)
	t.Cleanup(func() { h.call(t, http.MethodDelete, "/conversations/"+c.ID, nil, nil) })
	return c.ID
}

type sentDoc struct {
	Message struct {
		ID string `json:"id"`
	} `json:"message"`
	Job struct {
		ID    string `json:"id"`
		State string `json:"state"`
	} `json:"job"`
	Agent struct {
		Paused     bool `json:"paused"`
		KillSwitch bool `json:"kill_switch"`
	} `json:"agent"`
}

func (h *harness) send(t *testing.T, conv, text string) sentDoc {
	t.Helper()
	var s sentDoc
	h.expect(t, http.StatusAccepted, http.MethodPost, "/conversations/"+conv+"/messages", map[string]string{"content": text}, &s)
	return s
}

func (h *harness) job(t *testing.T, id string) jobDoc {
	t.Helper()
	var j jobDoc
	h.expect(t, http.StatusOK, http.MethodGet, "/jobs/"+id, nil, &j)
	return j
}

// waitJob waits until the job is in state (failing early when it ends in
// another one).
func (h *harness) waitJob(t *testing.T, id, state string) jobDoc {
	t.Helper()
	var j jobDoc
	ok := eventually(jobTimeout, func() bool {
		j = h.job(t, id)
		switch j.State {
		case state:
			return true
		case "succeeded", "failed", "cancelled":
			t.Fatalf("job ended %s (%q), want %s\n%s", j.State, j.Error, state, h.logTail())
		}
		return false
	})
	if !ok {
		t.Fatalf("job still %s after %v, want %s\n%s", j.State, jobTimeout, state, h.logTail())
	}
	return j
}

func (h *harness) jobApprovals(t *testing.T, jobID string) []approvalDoc {
	t.Helper()
	var page struct {
		Approvals []approvalDoc `json:"approvals"`
	}
	h.expect(t, http.StatusOK, http.MethodGet, "/approvals?state=all&job_id="+jobID, nil, &page)
	return page.Approvals
}

func (h *harness) approval(t *testing.T, id string) approvalDoc {
	t.Helper()
	var a approvalDoc
	h.expect(t, http.StatusOK, http.MethodGet, "/approvals/"+id, nil, &a)
	return a
}

// davDelete removes a file the test created (cleanup); remove refuses
// anything else.
func (h *harness) davDelete(t *testing.T, p string) {
	t.Helper()
	st, err := h.remove(p)
	if err != nil || (st != http.StatusNoContent && st != http.StatusNotFound) {
		t.Errorf("delete %s: %d %v", p, st, err)
	}
}

// --- scenarios ---

// (a) status before and after enrolment; a chat job lists the user's files
// with the app token and gets a file that only oCIS knows about.
func scenarioEnrol(t *testing.T, h *harness) {
	before := h.status(t)
	if before.Enrolled || before.Token.Status != "none" {
		t.Fatalf("status before enrolment: %+v", before)
	}
	s := h.enrol(t)
	if !s.Enrolled || s.Token.Status != "valid" || s.Token.ExpiresAt == nil || s.User.Login != demoUser {
		t.Fatalf("status after enrolment: %+v", s)
	}
	if got := h.status(t); got.Token.Status != "valid" || got.Paused {
		t.Fatalf("status: %+v", got)
	}
	labels, err := h.ourLabels()
	if err != nil || len(labels) != 1 {
		t.Fatalf("oCIS lists %d token(s) of the sidecar after enrolment (%v), want 1", len(labels), err)
	}

	// a file put there directly, so the listing must come from oCIS
	probe := "/ocis-agent-it-" + strings.Trim(marker(), "[]") + ".txt"
	h.track(probe)
	if st, _, err := h.dav(http.MethodPut, probe, []byte("probe"), nil); err != nil || st/100 != 2 {
		t.Fatalf("put %s: %d %v", probe, st, err)
	}
	t.Cleanup(func() { h.davDelete(t, probe) })

	m := marker()
	h.llm.on(m, callThenAnswer("files.list", map[string]string{"path": "/"}, "Here are your files."))
	conv := h.conversation(t)
	sent := h.send(t, conv, "List my files "+m)
	j := h.waitJob(t, sent.Job.ID, "succeeded")
	if !strings.Contains(j.Answer, "Here are your files.") {
		t.Fatalf("answer: %q", j.Answer)
	}
	listed := false
	for _, st := range j.Steps {
		if st.Tool == "files.list" {
			listed = true
		}
	}
	if !listed {
		t.Fatalf("no files.list step: %+v", j.Steps)
	}

	reqs := h.llm.requests(m)
	if len(reqs) != 2 {
		t.Fatalf("the model was asked %d time(s), want 2", len(reqs))
	}
	sys := reqs[0].system()
	if !strings.Contains(sys, "untrusted") || !strings.Contains(sys, "<data") {
		t.Fatalf("the system prompt does not fence untrusted data: %q", sys)
	}
	tools := strings.Join(reqs[0].toolNames(), ",")
	for _, want := range []string{"files.list", "files.write_new", "files.read_text", "memory.read"} {
		if !strings.Contains(tools, want) {
			t.Fatalf("tool %s missing from the request: %s", want, tools)
		}
	}
	res := strings.Join(reqs[1].toolResults(), "\n")
	if !strings.Contains(res, `<data source="listing"`) || !strings.Contains(res, strings.TrimPrefix(probe, "/")) {
		t.Fatalf("the tool result is not the fenced listing with the probe file: %q", res)
	}
}

// (b) a question gets an answer that is stored as the assistant message; the
// event stream reports the changes.
func scenarioChat(t *testing.T, h *harness) {
	h.enrol(t)
	stream, err := h.openEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.close()
	if !stream.waitFor("ready", `{"v":1}`, 10*time.Second) {
		t.Fatalf("no ready frame: %v", stream.frames())
	}

	m := marker()
	reply := "The answer is 42. " + m
	h.llm.on(m, answer(reply))
	conv := h.conversation(t)
	sent := h.send(t, conv, "What is the answer? "+m)
	h.waitJob(t, sent.Job.ID, "succeeded")

	var msgs struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
			JobID   string `json:"job_id"`
			Job     *struct {
				State string `json:"state"`
			} `json:"job"`
		} `json:"messages"`
	}
	ok := eventually(10*time.Second, func() bool {
		h.expect(t, http.StatusOK, http.MethodGet, "/conversations/"+conv+"/messages", nil, &msgs)
		return len(msgs.Messages) == 2
	})
	if !ok {
		t.Fatalf("messages: %+v", msgs.Messages)
	}
	var user, assistant bool
	for _, msg := range msgs.Messages {
		switch msg.Role {
		case "user":
			user = msg.JobID == sent.Job.ID && msg.Job != nil && msg.Job.State == "succeeded"
		case "assistant":
			assistant = msg.Content == reply && msg.JobID == sent.Job.ID
		}
	}
	if !user || !assistant {
		t.Fatalf("messages: %+v", msgs.Messages)
	}
	for _, kind := range []string{`"jobs"`, `"messages"`, `"activity"`} {
		if !stream.waitFor("changed", kind, 10*time.Second) {
			t.Fatalf("no changed frame with %s: %v", kind, stream.frames())
		}
	}
}

// (c) a write needs the user's approval: a stale hash is refused, the
// approved file really lands in oCIS, a rejected one does not.
func scenarioApproval(t *testing.T, h *harness) {
	h.enrol(t)
	conv := h.conversation(t)

	run := func(decision string) (approvalDoc, jobDoc, string, string) {
		m := marker()
		path := "/ocis-agent-it-" + strings.Trim(m, "[]") + ".txt"
		content := "written by the integration test " + m
		h.track(path) // the sidecar creates it once approved
		t.Cleanup(func() { h.davDelete(t, path) })
		h.llm.on(m, callThenAnswer("files.write_new", map[string]string{"path": path, "content": content}, "Done."))
		sent := h.send(t, conv, "Write a file "+m)
		h.waitJob(t, sent.Job.ID, "waiting_approval")
		list := h.jobApprovals(t, sent.Job.ID)
		if len(list) != 1 {
			t.Fatalf("%d approvals for the job, want 1", len(list))
		}
		a := list[0]
		if a.State != "pending" || a.Tool != "files.write_new" || a.ArgsHash == "" {
			t.Fatalf("approval: %+v", a)
		}
		var args struct{ Path, Content string }
		if err := json.Unmarshal(a.Args, &args); err != nil || args.Path != path || args.Content != content {
			t.Fatalf("the approval's arguments are not the call's: %s", a.Args)
		}
		if s := h.status(t); s.PendingApprovals < 1 {
			t.Fatalf("status counts %d pending approvals", s.PendingApprovals)
		}
		if h.exists(t, path) {
			t.Fatalf("%s exists before the decision", path)
		}

		stale := h.callRetry(t, http.MethodPost, "/approvals/"+a.ID+"/decision",
			map[string]string{"decision": decision, "args_hash": strings.Repeat("0", 64)})
		if stale.status != http.StatusConflict || stale.errorCode() != "stale_arguments" {
			t.Fatalf("decision with a stale hash = %s", stale)
		}
		if got := h.approval(t, a.ID); got.State != "pending" {
			t.Fatalf("a stale decision changed the approval: %s", got.State)
		}

		var decided approvalDoc
		h.expect(t, http.StatusOK, http.MethodPost, "/approvals/"+a.ID+"/decision",
			map[string]string{"decision": decision, "args_hash": a.ArgsHash}, &decided)
		j := h.waitJob(t, sent.Job.ID, "succeeded")
		return h.approval(t, a.ID), j, path, m
	}

	a, _, path, _ := run("approve")
	if a.State != "executed" || a.Outcome == nil || a.Outcome.State != "done" {
		t.Fatalf("approved approval: state %s, outcome %+v", a.State, a.Outcome)
	}
	st, body, err := h.dav(http.MethodGet, path, nil, nil)
	if err != nil || st != http.StatusOK || !strings.HasPrefix(string(body), "written by the integration test") {
		t.Fatalf("GET %s = %d %v", path, st, err)
	}

	a, j, path, m := run("reject")
	if a.State != "rejected" {
		t.Fatalf("rejected approval: state %s", a.State)
	}
	// the model got the rejection as the call's result, then answered
	reqs := h.llm.requests(m)
	if len(reqs) != 2 {
		t.Fatalf("the model was asked %d time(s) for the rejected call, want 2", len(reqs))
	}
	res := strings.Join(reqs[1].toolResults(), "\n")
	if !strings.Contains(res, "files.write_new") || !strings.Contains(res, "rejected") || !strings.Contains(res, "The user rejected this action.") {
		t.Fatalf("the model did not get the rejection back: %q", res)
	}
	if !strings.Contains(j.Answer, "The user rejected this action.") {
		t.Fatalf("the job's answer does not carry the rejection the model saw: %q", j.Answer)
	}
	if h.exists(t, path) {
		t.Fatalf("%s exists after a rejection", path)
	}
}

// (d) a paused agent queues a message and answers it after the resume.
func scenarioPause(t *testing.T, h *harness) {
	h.enrol(t)
	var s statusDoc
	h.expect(t, http.StatusOK, http.MethodPost, "/pause", map[string]any{}, &s)
	if !s.Paused || s.PauseReason != "user" {
		t.Fatalf("status after pause: %+v", s)
	}
	m := marker()
	h.llm.on(m, answer("resumed "+m))
	conv := h.conversation(t)
	sent := h.send(t, conv, "Are you there? "+m)
	if !sent.Agent.Paused {
		t.Fatalf("the send does not report the pause: %+v", sent.Agent)
	}
	// the send woke the workers: for a few seconds (several claim attempts)
	// the job must stay queued and the model unasked
	var j jobDoc
	if !consistently(4*time.Second, func() bool {
		j = h.job(t, sent.Job.ID)
		return j.State == "queued" && len(h.llm.requests(m)) == 0
	}) {
		t.Fatalf("a paused user's job ran: state %s, %d model call(s)", j.State, len(h.llm.requests(m)))
	}
	h.expect(t, http.StatusOK, http.MethodPost, "/resume", map[string]any{}, &s)
	if s.Paused {
		t.Fatalf("status after resume: %+v", s)
	}
	h.waitJob(t, sent.Job.ID, "succeeded")
}

// (e) a renewal replaces the app token in oCIS and deletes the old one.
func scenarioRenew(t *testing.T, h *harness) {
	h.enrol(t)
	before, err := h.ourLabels()
	if err != nil || len(before) != 1 {
		t.Fatalf("tokens before the renewal: %d (%v)", len(before), err)
	}
	var r struct {
		Renewed    bool `json:"renewed"`
		OldCleaned bool `json:"old_cleaned"`
	}
	h.expect(t, http.StatusOK, http.MethodPost, "/token/renew", map[string]any{}, &r)
	if !r.Renewed || !r.OldCleaned {
		t.Fatalf("renew: %+v", r)
	}
	after, err := h.ourLabels()
	if err != nil || len(after) != 1 || after[0] == before[0] {
		t.Fatalf("tokens after the renewal: %d, same label %v (%v)", len(after), len(after) == 1 && after[0] == before[0], err)
	}
	if s := h.status(t); s.Token.Status != "valid" {
		t.Fatalf("status after renewal: %+v", s)
	}
	// the new token works: a job reads through it
	m := marker()
	h.llm.on(m, callThenAnswer("files.list", map[string]string{"path": "/"}, "listed"))
	conv := h.conversation(t)
	h.waitJob(t, h.send(t, conv, "List "+m).Job.ID, "succeeded")
	if res := strings.Join(h.llm.requests(m)[1].toolResults(), ""); !strings.Contains(res, "<data") {
		t.Fatalf("the listing with the renewed token failed: %q", res)
	}
}

// (f) unenrolment deletes the sidecar's tokens in oCIS and stops the agent
// for the user.
func scenarioUnenrol(t *testing.T, h *harness) {
	h.enrol(t)
	conv := h.conversation(t)
	if labels, err := h.ourLabels(); err != nil || len(labels) != 1 {
		t.Fatalf("tokens after enrolment: %d (%v)", len(labels), err)
	}
	h.expect(t, http.StatusNoContent, http.MethodPost, "/unenroll", map[string]any{}, nil)
	labels, err := h.ourLabels()
	if err != nil || len(labels) != 0 {
		t.Fatalf("%d token(s) of the sidecar left in oCIS after unenrolment (%v)", len(labels), err)
	}
	if s := h.status(t); s.Enrolled || s.Token.Status != "none" {
		t.Fatalf("status after unenrolment: %+v", s)
	}
	r := h.callRetry(t, http.MethodPost, "/conversations/"+conv+"/messages", map[string]string{"content": "hello"})
	if r.status != http.StatusConflict || r.errorCode() != "not_enrolled" {
		t.Fatalf("a message after unenrolment = %s", r)
	}
	r = h.callRetry(t, http.MethodPost, "/conversations", map[string]any{})
	if r.status != http.StatusConflict || r.errorCode() != "not_enrolled" {
		t.Fatalf("a conversation after unenrolment = %s", r)
	}
	if r := h.call(t, http.MethodPost, "/unenroll", map[string]any{}, nil); r.status != http.StatusNotFound {
		t.Fatalf("a second unenrolment = %s", r)
	}
}

// (g) the audit chain verifies and the export holds the events of the
// actions above.
func scenarioAudit(t *testing.T, h *harness) {
	h.enrol(t)
	m := marker()
	h.llm.on(m, answer("audited "+m))
	conv := h.conversation(t)
	h.waitJob(t, h.send(t, conv, "Audit me "+m).Job.ID, "succeeded")

	var v struct {
		OK      bool  `json:"ok"`
		Checked int64 `json:"checked"`
	}
	h.expect(t, http.StatusOK, http.MethodGet, "/audit/verify", nil, &v)
	if !v.OK || v.Checked < 1 {
		t.Fatalf("verify: %+v", v)
	}
	r := h.expect(t, http.StatusOK, http.MethodGet, "/audit/export", nil, nil)
	if ct := r.header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("export content type %q", ct)
	}
	kinds := map[string]bool{}
	lines := 0
	sc := bufio.NewScanner(bytes.NewReader(r.body))
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
	var prev string
	for sc.Scan() {
		var e struct {
			Kind     string `json:"kind"`
			PrevHash string `json:"prev_hash"`
			Hash     string `json:"hash"`
		}
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("export line %d: %v", lines+1, err)
		}
		if lines > 0 && e.PrevHash != prev {
			t.Fatalf("export line %d does not link to the one before", lines+1)
		}
		prev = e.Hash
		kinds[e.Kind] = true
		lines++
	}
	if int64(lines) < v.Checked {
		t.Fatalf("export has %d lines, verify checked %d", lines, v.Checked)
	}
	for _, k := range []string{"token_created", "chat_message", "job_finished"} {
		if !kinds[k] {
			t.Fatalf("export has no %s event: %v", k, kinds)
		}
	}
}

// (h) memory: create a note, edit it with If-Match, a stale ETag is 412.
func scenarioMemory(t *testing.T, h *harness) {
	// a slug of [a-z0-9-] that is also an ocis-agent-it-* name
	slug := testFilePrefix + strings.ToLower(strings.Trim(strings.NewReplacer("_", "-").Replace(marker()), "[]")) + ".md"
	name := "notes/" + slug
	h.track("/.ocis-agent/memory/" + name)
	t.Cleanup(func() { h.davDelete(t, "/.ocis-agent/memory/"+name) })

	var created struct {
		Name string `json:"name"`
		ETag string `json:"etag"`
	}
	h.expect(t, http.StatusCreated, http.MethodPost, "/memory", map[string]string{"name": name, "content": "first\n"}, &created)
	if created.Name != name || created.ETag == "" {
		t.Fatalf("create: %+v", created)
	}
	if r := h.callRetry(t, http.MethodPost, "/memory", map[string]string{"name": name, "content": "again\n"}); r.status != http.StatusConflict || r.errorCode() != "already_exists" {
		t.Fatalf("a second create = %s", r)
	}

	var file struct {
		Content string `json:"content"`
		ETag    string `json:"etag"`
	}
	h.expect(t, http.StatusOK, http.MethodGet, "/memory/"+name, nil, &file)
	if file.Content != "first\n" || file.ETag != created.ETag {
		t.Fatalf("read: %q etag changed %v", file.Content, file.ETag != created.ETag)
	}

	put := func(etag, content string) apiResp {
		return h.callRetryHdr(t, http.MethodPut, "/memory/"+name, map[string]string{"content": content}, map[string]string{"If-Match": etag})
	}
	if r := h.call(t, http.MethodPut, "/memory/"+name, map[string]string{"content": "x"}, nil); r.status != http.StatusPreconditionRequired {
		t.Fatalf("PUT without If-Match = %s", r)
	}
	r := put(file.ETag, "second\n")
	if r.status != http.StatusOK {
		t.Fatalf("PUT with the current ETag = %s", r)
	}
	var edited struct {
		ETag string `json:"etag"`
		Diff string `json:"diff"`
	}
	if err := json.Unmarshal(r.body, &edited); err != nil || edited.ETag == "" || edited.ETag == file.ETag || !strings.Contains(edited.Diff, "second") {
		t.Fatalf("edit: %s", r)
	}
	if r := put(file.ETag, "third\n"); r.status != http.StatusPreconditionFailed || r.errorCode() != "precondition_failed" {
		t.Fatalf("PUT with a stale ETag = %s", r)
	}
	st, body, err := h.dav(http.MethodGet, "/.ocis-agent/memory/"+name, nil, nil)
	if err != nil || st != http.StatusOK || string(body) != "second\n" {
		t.Fatalf("the note in oCIS: %d %q %v", st, body, err)
	}
}

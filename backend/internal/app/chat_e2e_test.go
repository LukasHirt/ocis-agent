package app

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/agent"
	"github.com/owncloud/ocis-agent/backend/internal/llm"
	"github.com/owncloud/ocis-agent/backend/internal/llm/llmtest"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// memoryList is the body of GET /memory.
type memoryList struct {
	Files []struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
		ETag string `json:"etag"`
	} `json:"files"`
}

// A real process serves the memory routes (the memory service reaches the
// API), in the api role alone too: with the user's own bearer, without any
// model endpoint.
func TestTheMemoryRoutesWorkInARealProcess(t *testing.T) {
	f := newFixture(t, map[string]string{"AGENT_ROLES": "api", "AGENT_LLM_BASE_URL": "-", "AGENT_LLM_MODEL": "-"})
	a, err := Build(bg, f.cfg, f.st, Options{Log: quietLog()})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	start(t, a)
	c := newClient(t, a, f.cfg.AllowedOrigin)
	c.must(http.StatusCreated, login, http.MethodPost, "/agent-api/v1/enroll", "")
	var list memoryList
	c.must(http.StatusOK, login, http.MethodGet, "/agent-api/v1/memory", "").decode(t, &list)
	c.must(http.StatusCreated, login, http.MethodPost, "/agent-api/v1/memory", `{"name":"profile.md","content":"likes physics\n"}`)
	list = memoryList{}
	c.must(http.StatusOK, login, http.MethodGet, "/agent-api/v1/memory", "").decode(t, &list)
	if len(list.Files) != 1 || list.Files[0].Name != "profile.md" || list.Files[0].ETag == "" {
		t.Fatalf("memory = %+v", list)
	}
	if r := c.must(http.StatusOK, login, http.MethodGet, "/agent-api/v1/memory/profile.md", ""); !strings.Contains(string(r.body), "likes physics") {
		t.Errorf("GET profile.md = %s", r.body)
	}
	if data, _, ok := f.fake.File(drive, "/.ocis-agent/memory/profile.md"); !ok || string(data) != "likes physics\n" {
		t.Errorf("stored = %q, %v", data, ok)
	}
}

// gated plays the script of p, holding the calls whose index (from 0) has a
// channel in hold until it is closed; entered gets the index of every call.
type gated struct {
	p       *llmtest.Provider
	entered chan int

	mu   sync.Mutex
	n    int
	hold map[int]chan struct{}
}

func (g *gated) Complete(ctx context.Context, req llm.CompletionRequest) (llm.CompletionResponse, error) {
	g.mu.Lock()
	i := g.n
	g.n++
	ch := g.hold[i]
	g.mu.Unlock()
	g.entered <- i
	if ch != nil {
		select {
		case <-ch:
		case <-ctx.Done():
			return llm.CompletionResponse{}, ctx.Err()
		}
	}
	return g.p.Complete(ctx, req)
}

type conversationDoc struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type messageDoc struct {
	ID      string `json:"id"`
	Role    string `json:"role"`
	Content string `json:"content"`
	JobID   string `json:"job_id"`
	Job     *struct {
		ID    string `json:"id"`
		State string `json:"state"`
		Error string `json:"error"`
	} `json:"job"`
}

type sentDoc struct {
	Message messageDoc `json:"message"`
	Job     struct {
		ID    string `json:"id"`
		State string `json:"state"`
	} `json:"job"`
	Agent struct {
		Paused     bool `json:"paused"`
		KillSwitch bool `json:"kill_switch"`
	} `json:"agent"`
}

// send posts a chat message, waiting out the 1 s message cooldown (a send
// refused by it does not extend it).
func (c *client) send(who, conv, text string) sentDoc {
	c.t.Helper()
	body, _ := json.Marshal(map[string]string{"content": text})
	deadline := time.Now().Add(10 * time.Second)
	for {
		r := c.do(who, http.MethodPost, "/agent-api/v1/conversations/"+conv+"/messages", string(body))
		if r.code == http.StatusTooManyRequests && strings.Contains(string(r.body), `"rate_limited"`) && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if r.code != http.StatusAccepted {
			c.t.Fatalf("send = %d %s, want 202", r.code, r.body)
		}
		var s sentDoc
		r.decode(c.t, &s)
		return s
	}
}

// messages is the conversation's messages, oldest first.
func (c *client) messages(who, conv string) []messageDoc {
	c.t.Helper()
	var page struct {
		Messages []messageDoc `json:"messages"`
	}
	c.must(http.StatusOK, who, http.MethodGet, "/agent-api/v1/conversations/"+conv+"/messages?limit=100", "").decode(c.t, &page)
	slices.Reverse(page.Messages) // the API lists the newest first
	return page.Messages
}

// answered checks that the last two messages are the user's question and
// the answer of the same job, which succeeded.
func answered(t *testing.T, ms []messageDoc, question, answer, jobID string) {
	t.Helper()
	if len(ms) < 2 {
		t.Fatalf("messages = %+v", ms)
	}
	q, a := ms[len(ms)-2], ms[len(ms)-1]
	if q.Role != "user" || q.Content != question || q.JobID != jobID || q.Job == nil || q.Job.ID != jobID || q.Job.State != "succeeded" {
		t.Errorf("question = %+v (job %+v), want %q of job %s, succeeded", q, q.Job, question, jobID)
	}
	if a.Role != "assistant" || a.Content != answer || a.JobID != jobID || a.Job != nil {
		t.Errorf("answer = %+v, want %q of job %s", a, answer, jobID)
	}
}

// transcript renders the request's messages after the system prompt as
// role:content|...
func transcript(req llm.CompletionRequest) string {
	var parts []string
	for _, m := range req.Messages[1:] {
		parts = append(parts, string(m.Role)+":"+m.Content)
	}
	return strings.Join(parts, "|")
}

// recorder keeps every frame of a stream while passing them on.
type recorder struct {
	mu  sync.Mutex
	all []string
}

func (r *recorder) tee(in <-chan string) <-chan string {
	out := make(chan string, 64)
	go func() {
		defer close(out)
		for fr := range in {
			r.mu.Lock()
			r.all = append(r.all, fr)
			r.mu.Unlock()
			out <- fr
		}
	}()
	return out
}

func (r *recorder) frames() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.all)
}

// changedWith matches a changed frame listing at least the kinds.
func changedWith(kinds ...string) func(string) bool {
	return func(fr string) bool {
		data, ok := strings.CutPrefix(fr, "event: changed\ndata: ")
		if !ok {
			return false
		}
		var d struct {
			Kinds []string `json:"kinds"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &d); err != nil {
			return false
		}
		for _, k := range kinds {
			if !slices.Contains(d.Kinds, k) {
				return false
			}
		}
		return true
	}
}

// drain drops the frames received so far.
func drain(frames <-chan string) {
	for {
		select {
		case <-frames:
		default:
			return
		}
	}
}

// Chat, memory edits and the event stream end to end, over HTTP against the
// api and worker roles of one process with a scripted model: a conversation
// whose answers come from the worker, the conversation as the context of the
// next message, an approval announced by the event stream and decided
// through the API, a user's memory edit that the model must read before it
// can replace the file, a conversation deleted with its queued job, another
// user who sees none of it and a shutdown that ends the open streams at once.
func TestChatMemoryAndEventsEndToEnd(t *testing.T) {
	f := newFixture(t, nil)
	f.fake.AddUser(marieLogin, "radium", marieID, "drive2")
	const (
		q1 = "Remember that my project is relativity."
		a1 = "Noted: your project is relativity."
		q2 = "What is my project?"
		a2 = "Your project is relativity."
		q3 = "Write hi into c.txt."
		a3 = "The file is written."
		q4 = "My project is now quanta; update your notes."
		a4 = "Updated your notes."
		q5 = "This one never runs."
	)
	writeProjects := func(content string) string {
		b, _ := json.Marshal(map[string]string{"name": "projects.md", "content": content, "mode": "replace"})
		return string(b)
	}
	p := llmtest.New(
		// job 1: an auto-run memory write, then the answer
		llmtest.Calls(3, 4, llm.ToolCall{ID: "m", Name: "memory.write", Arguments: writeProjects("Project: relativity\n")}),
		llmtest.Say(a1, 5, 6),
		// job 2: the answer
		llmtest.Say(a2, 1, 1),
		// job 3: a write that needs an approval (call 3 is held), then the answer
		llmtest.Calls(2, 2, llm.ToolCall{ID: "w", Name: "files.write_new", Arguments: `{"path":"/c.txt","content":"hi"}`}),
		llmtest.Say(a3, 1, 1),
		// job 4: a replace after the user's edit is refused, a read, the
		// replace again, the answer
		llmtest.Calls(1, 1, llm.ToolCall{ID: "r1", Name: "memory.write", Arguments: writeProjects("Project: quanta\n")}),
		llmtest.Calls(1, 1, llm.ToolCall{ID: "r2", Name: "memory.read", Arguments: `{"name":"projects.md"}`}),
		llmtest.Calls(1, 1, llm.ToolCall{ID: "r3", Name: "memory.write", Arguments: writeProjects("Project: quanta\n")}),
		llmtest.Say(a4, 1, 1),
	)
	release3 := make(chan struct{})
	g := &gated{p: p, entered: make(chan int, 64), hold: map[int]chan struct{}{3: release3}}
	spy := newClaimSpy(f.st)
	tune := fast()
	tune.eventsPoll = 25 * time.Millisecond
	a, err := Build(bg, f.cfg, spy, Options{Provider: g, Log: quietLog(), tune: tune})
	if err != nil {
		t.Fatal(err)
	}
	r := start(t, a)
	c := newClient(t, a, f.cfg.AllowedOrigin)
	c.must(http.StatusCreated, login, http.MethodPost, "/agent-api/v1/enroll", "")
	c.must(http.StatusCreated, marieLogin, http.MethodPost, "/agent-api/v1/enroll", "")

	// a conversation; the first message is a job the worker runs
	var conv conversationDoc
	c.must(http.StatusCreated, login, http.MethodPost, "/agent-api/v1/conversations", "").decode(t, &conv)
	s1 := c.send(login, conv.ID, q1)
	if s1.Message.Role != "user" || s1.Message.Content != q1 || s1.Message.JobID != s1.Job.ID || s1.Job.State != "queued" ||
		s1.Agent.Paused || s1.Agent.KillSwitch {
		t.Fatalf("sent = %+v", s1)
	}
	if j := f.job(s1.Job.ID); j.TriggerKind != store.TriggerChat || j.TriggerRef != conv.ID || j.Goal != q1 {
		t.Fatalf("job = %+v, want a chat job of the conversation with the text as its goal", j)
	}
	f.waitState(s1.Job.ID, store.JobSucceeded, 10*time.Second)
	answered(t, c.messages(login, conv.ID), q1, a1, s1.Job.ID)
	if got := transcript(p.Requests()[0]); got != "user:"+q1 {
		t.Errorf("first request = %s, want the goal alone", got)
	}
	if len(f.approvals(s1.Job.ID)) != 0 {
		t.Error("the memory write asked for an approval")
	}
	if data, _, ok := f.fake.File(drive, "/.ocis-agent/memory/projects.md"); !ok || string(data) != "Project: relativity\n" {
		t.Fatalf("projects.md = %q, %v", data, ok)
	}

	// the second message gets the conversation before its goal
	s2 := c.send(login, conv.ID, q2)
	f.waitState(s2.Job.ID, store.JobSucceeded, 10*time.Second)
	if got, want := transcript(p.Requests()[2]), "user:"+q1+"|assistant:"+a1+"|user:"+q2; got != want {
		t.Errorf("second job's request = %s\nwant %s", got, want)
	}
	answered(t, c.messages(login, conv.ID), q2, a2, s2.Job.ID)

	// the event streams: einstein's and, to show the scoping, marie's
	res, _ := openEvents(t, a, login)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("events = %d", res.StatusCode)
	}
	var rec recorder
	frames := rec.tee(readFrames(res.Body))
	wantFrame(t, frames, func(s string) bool { return s == "event: ready\ndata: {\"v\":1}\n\n" }, "ready", 5*time.Second)
	mres, _ := openEvents(t, a, marieLogin)
	if mres.StatusCode != http.StatusOK {
		t.Fatalf("marie's events = %d", mres.StatusCode)
	}
	var mrec recorder
	mframes := mrec.tee(readFrames(mres.Body))
	wantFrame(t, mframes, func(s string) bool { return s == "event: ready\ndata: {\"v\":1}\n\n" }, "marie's ready", 5*time.Second)

	// a message that needs an approval: the stream reports the message, its
	// job and its audit event, then (once the held model call returns) the
	// approval
	s3 := c.send(login, conv.ID, q3)
	wantFrame(t, frames, changedWith("jobs", "messages", "activity"), "the sent message", 5*time.Second)
	for entered := false; !entered; {
		select {
		case i := <-g.entered:
			entered = i == 3
		case <-time.After(10 * time.Second):
			t.Fatal("the job's model call did not start")
		}
	}
	close(release3)
	f.waitState(s3.Job.ID, store.JobWaitingApproval, 10*time.Second)
	wantFrame(t, frames, changedWith("jobs", "activity"), "the approval", 5*time.Second)
	var page approvalPage
	c.must(http.StatusOK, login, http.MethodGet, "/agent-api/v1/approvals", "").decode(t, &page)
	if len(page.Approvals) != 1 || page.Approvals[0].JobID != s3.Job.ID || page.Approvals[0].Tool != "files.write_new" ||
		page.Approvals[0].State != "pending" {
		t.Fatalf("approvals = %+v", page.Approvals)
	}
	ap := page.Approvals[0]
	drain(frames)
	c.must(http.StatusOK, login, http.MethodPost, "/agent-api/v1/approvals/"+ap.ID+"/decision",
		`{"decision":"approve","args_hash":"`+ap.ArgsHash+`"}`)
	f.waitState(s3.Job.ID, store.JobSucceeded, 10*time.Second)
	// no message changed between the sent one and the answer
	wantFrame(t, frames, changedWith("messages"), "the answer", 5*time.Second)
	answered(t, c.messages(login, conv.ID), q3, a3, s3.Job.ID)
	if data, _, ok := f.fake.File(drive, "/c.txt"); !ok || string(data) != "hi" {
		t.Errorf("c.txt = %q, %v", data, ok)
	}

	// memory: the user sees the model's file and edits it with its ETag
	var list memoryList
	c.must(http.StatusOK, login, http.MethodGet, "/agent-api/v1/memory", "").decode(t, &list)
	var etag string
	for _, fl := range list.Files {
		if fl.Name == "projects.md" {
			etag = fl.ETag
		}
	}
	if etag == "" {
		t.Fatalf("memory = %+v, want projects.md", list)
	}
	const edited = "Project: relativity (the user's own words)\n"
	putBody := `{"content":"` + strings.TrimSuffix(edited, "\n") + `\n"}`
	if r := c.do(login, http.MethodPut, "/agent-api/v1/memory/projects.md", putBody); r.code != http.StatusPreconditionRequired {
		t.Errorf("PUT without If-Match = %d %s, want 428", r.code, r.body)
	}
	if r := c.do(login, http.MethodPut, "/agent-api/v1/memory/projects.md", putBody, "If-Match", etag); r.code != http.StatusOK {
		t.Fatalf("PUT with the listed ETag = %d %s", r.code, r.body)
	}
	if r := c.do(login, http.MethodPut, "/agent-api/v1/memory/projects.md", putBody, "If-Match", etag); r.code != http.StatusPreconditionFailed {
		t.Errorf("PUT with the old ETag = %d %s, want 412", r.code, r.body)
	}
	if data, _, _ := f.fake.File(drive, "/.ocis-agent/memory/projects.md"); string(data) != edited {
		t.Fatalf("projects.md = %q after the edit", data)
	}

	// the model's replace is refused until it reads the file again
	s4 := c.send(login, conv.ID, q4)
	f.waitState(s4.Job.ID, store.JobSucceeded, 10*time.Second)
	reqs := p.Requests()
	if len(reqs) != 9 {
		t.Fatalf("%d model calls, want 9", len(reqs))
	}
	refused := reqs[6].Messages[len(reqs[6].Messages)-1]
	if refused.Role != llm.RoleTool || !strings.Contains(refused.Content, "memory.read") {
		t.Errorf("the replace without a read got %+v, want a refusal asking for memory.read", refused)
	}
	read := reqs[7].Messages[len(reqs[7].Messages)-1]
	if read.Role != llm.RoleTool || !strings.Contains(read.Content, "the user's own words") {
		t.Errorf("the read got %+v, want the user's edit", read)
	}
	if data, _, _ := f.fake.File(drive, "/.ocis-agent/memory/projects.md"); string(data) != "Project: quanta\n" {
		t.Errorf("projects.md = %q, want the replace after the read", data)
	}
	answered(t, c.messages(login, conv.ID), q4, a4, s4.Job.ID)

	// another user sees none of it
	var convs struct {
		Conversations []conversationDoc `json:"conversations"`
	}
	c.must(http.StatusOK, marieLogin, http.MethodGet, "/agent-api/v1/conversations", "").decode(t, &convs)
	if len(convs.Conversations) != 0 {
		t.Errorf("marie sees %+v", convs.Conversations)
	}
	base := "/agent-api/v1/conversations/" + conv.ID
	c.must(http.StatusNotFound, marieLogin, http.MethodGet, base, "")
	c.must(http.StatusNotFound, marieLogin, http.MethodGet, base+"/messages", "")
	c.must(http.StatusNotFound, marieLogin, http.MethodPut, base, `{"title":"mine"}`)
	c.must(http.StatusNotFound, marieLogin, http.MethodPost, base+"/messages", `{"content":"hello"}`)
	c.must(http.StatusNotFound, marieLogin, http.MethodDelete, base, "")
	c.must(http.StatusNotFound, marieLogin, http.MethodGet, "/agent-api/v1/jobs/"+s1.Job.ID, "")
	c.must(http.StatusNotFound, marieLogin, http.MethodGet, "/agent-api/v1/approvals/"+ap.ID, "")
	c.must(http.StatusNotFound, marieLogin, http.MethodGet, "/agent-api/v1/memory/projects.md", "")
	list = memoryList{}
	c.must(http.StatusOK, marieLogin, http.MethodGet, "/agent-api/v1/memory", "").decode(t, &list)
	if len(list.Files) != 0 {
		t.Errorf("marie's memory = %+v", list.Files)
	}
	if got := c.messages(login, conv.ID); len(got) != 8 {
		t.Errorf("einstein has %d messages after marie's requests, want 8", len(got))
	}

	// deleting the conversation cancels its queued job (staged while the
	// kill switch keeps the worker from claiming it)
	if err := agent.SetKillSwitch(bg, f.st, true); err != nil {
		t.Fatal(err)
	}
	s5 := c.send(login, conv.ID, q5)
	if !s5.Agent.KillSwitch || s5.Job.State != "queued" {
		t.Errorf("sent while killed = %+v", s5)
	}
	c.must(http.StatusNoContent, login, http.MethodDelete, base, "")
	if j := f.job(s5.Job.ID); j.State != store.JobCancelled || j.Runs != 0 {
		t.Fatalf("job of the deleted conversation = %s, %d runs", j.State, j.Runs)
	}
	c.must(http.StatusNotFound, login, http.MethodGet, base, "")
	c.must(http.StatusNotFound, login, http.MethodGet, base+"/messages", "")
	if err := agent.SetKillSwitch(bg, f.st, false); err != nil {
		t.Fatal(err)
	}
	if !spy.emptyClaims(2, 10*time.Second) {
		t.Fatal("the worker did not look for work after the kill switch")
	}
	if j := f.job(s5.Job.ID); j.State != store.JobCancelled || j.Runs != 0 || len(p.Requests()) != 9 {
		t.Errorf("after the kill switch: job %s, %d runs, %d model calls", j.State, j.Runs, len(p.Requests()))
	}
	var audit struct {
		Events []struct {
			Kind string `json:"kind"`
		} `json:"events"`
	}
	c.must(http.StatusOK, login, http.MethodGet, "/agent-api/v1/audit?limit=200", "").decode(t, &audit)
	kinds := map[string]int{}
	for _, e := range audit.Events {
		kinds[e.Kind]++
	}
	if kinds["chat_message"] != 5 || kinds["conversation_deleted"] != 1 || kinds["memory_changed"] < 2 {
		t.Errorf("audit kinds = %v", kinds)
	}

	// a shutdown ends the open streams at once
	began := time.Now()
	r.cancel()
	wantFrame(t, frames, func(s string) bool { return s == "event: bye\ndata: {\"reason\":\"shutdown\"}\n\n" }, "bye", 5*time.Second)
	wantFrame(t, mframes, func(s string) bool { return s == "event: bye\ndata: {\"reason\":\"shutdown\"}\n\n" }, "marie's bye", 5*time.Second)
	if err := r.stop(t, 15*time.Second); err != nil {
		t.Errorf("Run = %v", err)
	}
	if d := time.Since(began); d > 5*time.Second {
		t.Errorf("shutdown with open streams took %v, want well under the 10s forced close", d)
	}

	// the frames name kinds only, and marie's stream saw no change of einstein's
	for _, fr := range rec.frames() {
		for _, secret := range []string{conv.ID, s3.Job.ID, ap.ID, "relativity", userID, login} {
			if strings.Contains(fr, secret) {
				t.Errorf("frame %q carries %q", fr, secret)
			}
		}
	}
	for _, fr := range mrec.frames() {
		if strings.HasPrefix(fr, "event: changed") {
			t.Errorf("marie's stream got %q", fr)
		}
	}
}

// The api role alone accepts chat messages without a model endpoint; they
// queue up, since only a worker runs jobs (an api-only deployment never
// does).
func TestTheAPIRoleAloneQueuesChatMessages(t *testing.T) {
	f := newFixture(t, map[string]string{"AGENT_ROLES": "api", "AGENT_LLM_BASE_URL": "-", "AGENT_LLM_MODEL": "-"})
	a, err := Build(bg, f.cfg, f.st, Options{Log: quietLog()})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	start(t, a)
	c := newClient(t, a, f.cfg.AllowedOrigin)
	c.must(http.StatusCreated, login, http.MethodPost, "/agent-api/v1/enroll", "")
	var conv conversationDoc
	c.must(http.StatusCreated, login, http.MethodPost, "/agent-api/v1/conversations", `{"title":"notes"}`).decode(t, &conv)
	s := c.send(login, conv.ID, "hello")
	if s.Job.State != "queued" || s.Agent.Paused || s.Agent.KillSwitch {
		t.Fatalf("sent = %+v", s)
	}
	ms := c.messages(login, conv.ID)
	if len(ms) != 1 || ms[0].Content != "hello" || ms[0].Job == nil || ms[0].Job.State != "queued" {
		t.Fatalf("messages = %+v", ms)
	}
	if j := f.job(s.Job.ID); j.State != store.JobQueued || j.Runs != 0 {
		t.Errorf("job = %s, %d runs", j.State, j.Runs)
	}
}

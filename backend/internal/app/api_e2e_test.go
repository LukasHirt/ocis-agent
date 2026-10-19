package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/agent"
	"github.com/owncloud/ocis-agent/backend/internal/api"
	"github.com/owncloud/ocis-agent/backend/internal/llm"
	"github.com/owncloud/ocis-agent/backend/internal/llm/llmtest"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient/fakeocis"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

const (
	marieLogin = "marie"
	marieID    = "u2"
)

// client calls the API of a running app over HTTP, as the browser would:
// the user's OIDC bearer and, for state-changing methods, the allowed
// Origin.
type client struct {
	t      *testing.T
	a      *App
	origin string
	hc     *http.Client
}

func newClient(t *testing.T, a *App, origin string) *client {
	hc := &http.Client{Timeout: 10 * time.Second}
	t.Cleanup(hc.CloseIdleConnections)
	return &client{t: t, a: a, origin: origin, hc: hc}
}

type reply struct {
	code int
	body []byte
}

func (r reply) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("body %q: %v", r.body, err)
	}
}

// do sends the request; login "" sends no Authorization header and origin
// "" (for state-changing methods) the client's default.
func (c *client) do(login, method, path, body string, hdr ...string) reply {
	c.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, "http://"+c.a.APIAddr()+path, rd)
	if err != nil {
		c.t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet {
		req.Header.Set("Origin", c.origin)
	}
	if login != "" {
		req.Header.Set("Authorization", "Bearer "+fakeocis.OIDCToken(login))
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatalf("%s %s: read: %v", method, path, err)
	}
	return reply{code: resp.StatusCode, body: b}
}

// must is do with an expected status code.
func (c *client) must(code int, login, method, path, body string) reply {
	c.t.Helper()
	r := c.do(login, method, path, body)
	if r.code != code {
		c.t.Fatalf("%s %s = %d %s, want %d", method, path, r.code, r.body, code)
	}
	return r
}

// countingWaker counts the wakes of the API, forwarding them to the queue.
type countingWaker struct {
	api.Waker
	n atomic.Int64
}

func (w *countingWaker) Wake() {
	w.n.Add(1)
	w.Waker.Wake()
}

// status is the part of GET /status the tests read.
type status struct {
	Enrolled    bool   `json:"enrolled"`
	Paused      bool   `json:"paused"`
	PauseReason string `json:"pause_reason"`
	Budget      struct {
		UsedTokens  int64   `json:"used_tokens"`
		DailyTokens int64   `json:"daily_tokens"`
		ResetsAt    *string `json:"resets_at"`
	} `json:"budget"`
	KillSwitch struct {
		Database    bool `json:"database"`
		Environment bool `json:"environment"`
	} `json:"kill_switch"`
	Models struct {
		Default   string            `json:"default"`
		Available []json.RawMessage `json:"available"`
	} `json:"models"`
	PendingApprovals int `json:"pending_approvals"`
}

type approvalDoc struct {
	ID       string          `json:"id"`
	JobID    string          `json:"job_id"`
	Tool     string          `json:"tool"`
	Args     json.RawMessage `json:"args"`
	ArgsHash string          `json:"args_hash"`
	State    string          `json:"state"`
}

type approvalPage struct {
	Approvals []approvalDoc `json:"approvals"`
}

func compactJSON(t *testing.T, raw []byte) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		t.Fatalf("%q: %v", raw, err)
	}
	return b.String()
}

// The REST API end to end, over HTTP against the api and worker roles of one
// process: enrol, an approval, the decision waking the worker, the job's
// timeline, the audit trail, pause and resume, the kill switch and the
// budget in the status, and the scoping of every lookup.
func TestTheRESTAPIEndToEnd(t *testing.T) {
	f := newFixture(t, nil)
	f.fake.AddUser(marieLogin, "radium", marieID, "drive2")
	p := llmtest.New(
		llmtest.Calls(3, 4, llm.ToolCall{ID: "w", Name: "files.write_new", Arguments: `{"path":"/a.txt","content":"hi"}`}),
		llmtest.Say("written", 5, 6),
		llmtest.Say("after the pause", 1, 1),
	)
	spy := newClaimSpy(f.st)
	wakes := &countingWaker{}
	// a queue poll of an hour: the worker only runs when something wakes it,
	// so a decision that does not reach the real queue leaves the job waiting
	a, err := Build(bg, f.cfg, spy, Options{Provider: p, Log: quietLog(), tune: tuning{
		queuePoll: time.Hour,
		wrapWaker: func(w api.Waker) api.Waker { wakes.Waker = w; return wakes },
	}})
	if err != nil {
		t.Fatal(err)
	}
	if wakes.Waker != api.Waker(a.queue) {
		t.Fatalf("the API wakes %T, not the workers' queue", wakes.Waker)
	}
	start(t, a)
	c := newClient(t, a, f.cfg.AllowedOrigin)

	// no bearer, a foreign Origin
	c.must(http.StatusUnauthorized, "", http.MethodGet, "/agent-api/v1/status", "")
	if r := c.do(login, http.MethodPost, "/agent-api/v1/enroll", "", "Origin", "https://evil.example"); r.code != http.StatusForbidden {
		t.Fatalf("enrol from a foreign origin = %d %s, want 403", r.code, r.body)
	}
	var st status
	c.must(http.StatusOK, login, http.MethodGet, "/agent-api/v1/status", "").decode(t, &st)
	if st.Enrolled {
		t.Fatal("enrolled by a cross-origin request")
	}

	// enrol both users (empty body)
	c.must(http.StatusCreated, login, http.MethodPost, "/agent-api/v1/enroll", "")
	c.must(http.StatusCreated, marieLogin, http.MethodPost, "/agent-api/v1/enroll", "{}")

	// a job (chat comes with the next plan) asks for a write
	j := f.enqueue(a, "write a file")
	f.waitState(j, store.JobWaitingApproval, 10*time.Second)
	stored := f.approvals(j)
	if len(stored) != 1 {
		t.Fatalf("%d approvals", len(stored))
	}
	var page approvalPage
	c.must(http.StatusOK, login, http.MethodGet, "/agent-api/v1/approvals", "").decode(t, &page)
	if len(page.Approvals) != 1 {
		t.Fatalf("GET /approvals = %+v", page)
	}
	ap := page.Approvals[0]
	if ap.ID != stored[0].ID || ap.JobID != j || ap.State != "pending" || ap.Tool != "files.write_new" ||
		ap.ArgsHash != stored[0].ArgsHash || compactJSON(t, ap.Args) != compactJSON(t, stored[0].Args) {
		t.Fatalf("approval = %+v, stored %+v", ap, stored[0])
	}
	if _, _, ok := f.fake.File(drive, "/a.txt"); ok {
		t.Fatal("written before the approval")
	}

	// another user sees nothing of it and cannot decide it
	var other approvalPage
	c.must(http.StatusOK, marieLogin, http.MethodGet, "/agent-api/v1/approvals", "").decode(t, &other)
	if len(other.Approvals) != 0 {
		t.Errorf("marie sees %+v", other.Approvals)
	}
	c.must(http.StatusNotFound, marieLogin, http.MethodGet, "/agent-api/v1/approvals/"+ap.ID, "")
	c.must(http.StatusNotFound, marieLogin, http.MethodGet, "/agent-api/v1/jobs/"+j, "")
	c.must(http.StatusNotFound, marieLogin, http.MethodPost, "/agent-api/v1/approvals/"+ap.ID+"/decision",
		`{"decision":"approve","args_hash":"`+ap.ArgsHash+`"}`)
	c.must(http.StatusNotFound, marieLogin, http.MethodPost, "/agent-api/v1/jobs/"+j+"/cancel", "")
	// a stale hash is refused
	if r := c.do(login, http.MethodPost, "/agent-api/v1/approvals/"+ap.ID+"/decision", `{"decision":"approve","args_hash":"x"}`); r.code != http.StatusConflict ||
		!strings.Contains(string(r.body), "stale_arguments") {
		t.Fatalf("stale hash = %d %s", r.code, r.body)
	}
	if got := f.approvals(j); got[0].State != store.ApprovalPending || f.job(j).State != store.JobWaitingApproval {
		t.Fatalf("after refused decisions: approval %s, job %s", got[0].State, f.job(j).State)
	}

	// the decision wakes the worker (with an hour's poll nothing else would)
	before := wakes.n.Load()
	c.must(http.StatusOK, login, http.MethodPost, "/agent-api/v1/approvals/"+ap.ID+"/decision",
		`{"decision":"approve","args_hash":"`+ap.ArgsHash+`"}`)
	if wakes.n.Load() <= before {
		t.Error("the decision did not wake the queue")
	}
	f.waitState(j, store.JobSucceeded, 10*time.Second)
	data, _, ok := f.fake.File(drive, "/a.txt")
	if !ok || string(data) != "hi" {
		t.Errorf("file = %q, %v", data, ok)
	}
	puts := 0
	for _, rq := range f.fake.Requests() {
		if rq.Method == http.MethodPut && rq.Path == "/remote.php/dav/spaces/"+drive+"/a.txt" {
			puts++
		}
	}
	if puts != 1 {
		t.Errorf("%d PUTs of the file, want 1", puts)
	}

	// the job's timeline and answer
	var job struct {
		ID     string `json:"id"`
		State  string `json:"state"`
		Answer string `json:"answer"`
		Steps  []struct {
			Kind       string `json:"kind"`
			Tool       string `json:"tool"`
			ApprovalID string `json:"approval_id"`
		} `json:"steps"`
	}
	c.must(http.StatusOK, login, http.MethodGet, "/agent-api/v1/jobs/"+j, "").decode(t, &job)
	if job.ID != j || job.State != "succeeded" || job.Answer != "written" {
		t.Errorf("job = %+v", job)
	}
	var toolStep bool
	for _, s := range job.Steps {
		if s.Tool == "files.write_new" && s.ApprovalID == ap.ID {
			toolStep = true
		}
	}
	if len(job.Steps) < 3 || !toolStep {
		t.Errorf("steps = %+v, want the model turns and the approved write", job.Steps)
	}

	// the audit trail
	var audit struct {
		Events []struct {
			Kind  string `json:"kind"`
			JobID string `json:"job_id"`
		} `json:"events"`
	}
	c.must(http.StatusOK, login, http.MethodGet, "/agent-api/v1/audit?limit=200", "").decode(t, &audit)
	kinds := map[string]bool{}
	for _, e := range audit.Events {
		kinds[e.Kind] = true
	}
	for _, k := range []string{"approval_requested", "approval_decided", "action_executed"} {
		if !kinds[k] {
			t.Errorf("no %s in the audit: %+v", k, audit.Events)
		}
	}
	var verify struct {
		OK bool `json:"ok"`
	}
	c.must(http.StatusOK, login, http.MethodGet, "/agent-api/v1/audit/verify", "").decode(t, &verify)
	if !verify.OK {
		t.Error("the audit chain does not verify")
	}

	// pause: nothing of the user is claimed; resume wakes the worker
	c.must(http.StatusOK, login, http.MethodPost, "/agent-api/v1/pause", "").decode(t, &st)
	if !st.Paused || st.PauseReason != store.PauseUser {
		t.Fatalf("after pause: %+v", st)
	}
	for len(spy.empty) > 0 {
		<-spy.empty
	}
	j2 := f.enqueue(a, "after the pause") // wakes the worker, which finds nothing
	select {
	case <-spy.empty:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker did not look for work")
	}
	if got := f.job(j2); got.State != store.JobQueued || got.Runs != 0 {
		t.Fatalf("job of a paused user = %s runs %d", got.State, got.Runs)
	}
	c.must(http.StatusOK, login, http.MethodPost, "/agent-api/v1/resume", "").decode(t, &st)
	if st.Paused || st.PauseReason != "" {
		t.Fatalf("after resume: %+v", st)
	}
	f.waitState(j2, store.JobSucceeded, 10*time.Second)

	// the kill switch flag and the budget in the status
	if err := agent.SetKillSwitch(bg, f.st, true); err != nil {
		t.Fatal(err)
	}
	st = status{}
	// the next UTC midnight by the server's own clock, read around the
	// request (a request across midnight may see either day)
	nextMidnight := func() string {
		y, m, d := a.now().UTC().Date()
		return time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	}
	resetsBefore := nextMidnight()
	c.must(http.StatusOK, login, http.MethodGet, "/agent-api/v1/status", "").decode(t, &st)
	resets := nextMidnight()
	if st.Budget.ResetsAt != nil && *st.Budget.ResetsAt == resetsBefore {
		resets = resetsBefore
	}
	if !st.Enrolled || !st.KillSwitch.Database || st.KillSwitch.Environment {
		t.Errorf("status = %+v, want the database kill switch", st)
	}
	// the expected use: the steps' tokens since the start of the day the
	// status reported (by the stored step times, as SumJobTokens counts)
	dayEnd, _ := time.Parse(time.RFC3339, resets)
	var used int64
	if err := f.st.View(bg, func(tx store.Tx) error {
		for _, id := range []string{j, j2} {
			steps, err := tx.ListSteps(userID, id)
			if err != nil {
				return err
			}
			for _, s := range steps {
				if !s.CreatedAt.Before(dayEnd.AddDate(0, 0, -1)) {
					used += int64(s.TokensIn + s.TokensOut)
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if used == 0 && time.Since(dayEnd.AddDate(0, 0, -1)) > time.Minute {
		t.Fatal("no step tokens recorded")
	}
	if st.Budget.DailyTokens != f.cfg.API.UserDailyTokens || st.Budget.UsedTokens != used ||
		st.Budget.ResetsAt == nil || *st.Budget.ResetsAt != resets {
		t.Errorf("budget = %+v, want cap %d, %d used, resets at %s", st.Budget, f.cfg.API.UserDailyTokens, used, resets)
	}
	if st.Models.Default != model || len(st.Models.Available) != 1 || st.PendingApprovals != 0 {
		t.Errorf("models %+v, pending %d", st.Models, st.PendingApprovals)
	}
	if err := agent.SetKillSwitch(bg, f.st, false); err != nil {
		t.Fatal(err)
	}
}

// The api role alone serves the REST API without a model endpoint: the
// catalog is empty, so no model preference can be chosen, and healthz stays.
func TestTheAPIRoleAloneServesTheRESTAPI(t *testing.T) {
	f := newFixture(t, map[string]string{"AGENT_ROLES": "api", "AGENT_LLM_BASE_URL": "-", "AGENT_LLM_MODEL": "-"})
	a, err := Build(bg, f.cfg, f.st, Options{Log: quietLog()})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	start(t, a)
	c := newClient(t, a, f.cfg.AllowedOrigin)
	if r := c.must(http.StatusOK, "", http.MethodGet, "/agent-api/healthz", ""); !strings.Contains(string(r.body), "ok") {
		t.Errorf("healthz = %s", r.body)
	}
	c.must(http.StatusNotFound, login, http.MethodGet, "/agent/", "")
	c.must(http.StatusUnauthorized, "", http.MethodGet, "/agent-api/v1/status", "")
	c.must(http.StatusCreated, login, http.MethodPost, "/agent-api/v1/enroll", "")
	var st status
	c.must(http.StatusOK, login, http.MethodGet, "/agent-api/v1/status", "").decode(t, &st)
	if !st.Enrolled || st.Models.Default != "" || len(st.Models.Available) != 0 {
		t.Errorf("status = %+v, want enrolled with no models", st)
	}
	if r := c.do(login, http.MethodPut, "/agent-api/v1/settings", `{"models":{"chat":"`+model+`"}}`); r.code != http.StatusBadRequest {
		t.Errorf("choosing a model without a catalog = %d %s, want 400", r.code, r.body)
	}
}

// readFrames reads the event stream frame by frame (a frame ends with an
// empty line) until the body ends.
func readFrames(body io.Reader) <-chan string {
	out := make(chan string, 64)
	go func() {
		defer close(out)
		br := bufio.NewReader(body)
		var frame strings.Builder
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			frame.WriteString(line)
			if line == "\n" {
				out <- frame.String()
				frame.Reset()
			}
		}
	}()
	return out
}

func openEvents(t *testing.T, a *App, who string) (*http.Response, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(bg)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+a.APIAddr()+"/agent-api/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+fakeocis.OIDCToken(who))
	hc := &http.Client{} // no client timeout: the body is a stream
	t.Cleanup(hc.CloseIdleConnections)
	res, err := hc.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = res.Body.Close() })
	return res, cancel
}

func wantFrame(t *testing.T, frames <-chan string, match func(string) bool, what string, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case fr, ok := <-frames:
			if !ok {
				t.Fatalf("the stream ended, want %s", what)
			}
			if match(fr) {
				return
			}
		case <-deadline:
			t.Fatalf("no %s within %v", what, within)
		}
	}
}

// An api-only process streams change notifications from the database with
// the operator's caps, and an open stream ends with bye/shutdown at the
// start of the shutdown, so the API's graceful stop does not wait for its
// 10 s forced close.
func TestEventStreamsInAnAPIOnlyProcessEndAtShutdown(t *testing.T) {
	f := newFixture(t, map[string]string{"AGENT_ROLES": "api", "AGENT_LLM_BASE_URL": "-", "AGENT_LLM_MODEL": "-",
		"AGENT_API_STREAMS_PER_USER": "1"})
	a, err := Build(bg, f.cfg, f.st, Options{Log: quietLog()})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	r := start(t, a)
	c := newClient(t, a, f.cfg.AllowedOrigin)
	c.must(http.StatusCreated, login, http.MethodPost, "/agent-api/v1/enroll", "")
	res, _ := openEvents(t, a, login)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("events = %d", res.StatusCode)
	}
	frames := readFrames(res.Body)
	wantFrame(t, frames, func(s string) bool { return s == "event: ready\ndata: {\"v\":1}\n\n" }, "ready", 5*time.Second)
	if second, _ := openEvents(t, a, login); second.StatusCode != http.StatusTooManyRequests {
		t.Errorf("a second stream = %d, want 429 (AGENT_API_STREAMS_PER_USER=1)", second.StatusCode)
	}
	c.must(http.StatusOK, login, http.MethodPut, "/agent-api/v1/settings", `{"daily_tokens":2000}`)
	wantFrame(t, frames, func(s string) bool {
		return s == "event: changed\ndata: {\"kinds\":[\"activity\",\"settings\"]}\n\n"
	}, "the settings change", 10*time.Second)

	began := time.Now()
	r.cancel()
	wantFrame(t, frames, func(s string) bool { return s == "event: bye\ndata: {\"reason\":\"shutdown\"}\n\n" }, "bye", 5*time.Second)
	if err := r.stop(t, 15*time.Second); err != nil {
		t.Errorf("Run = %v", err)
	}
	if d := time.Since(began); d > 5*time.Second {
		t.Errorf("shutdown with an open stream took %v, want well under the 10s forced close", d)
	}
}

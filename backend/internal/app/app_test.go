package app

import (
	"bytes"
	"net"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/agent"
	"github.com/owncloud/ocis-agent/backend/internal/approvals"
	"github.com/owncloud/ocis-agent/backend/internal/llm"
	"github.com/owncloud/ocis-agent/backend/internal/llm/llmtest"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// Rule 1: the worker role needs a model endpoint; the API role does not.
func TestTheWorkerRoleNeedsAModelEndpoint(t *testing.T) {
	noLLM := map[string]string{"AGENT_LLM_BASE_URL": "-", "AGENT_LLM_MODEL": "-"}
	for _, roles := range []string{"worker", "api,worker"} {
		extra := map[string]string{"AGENT_ROLES": roles}
		for k, v := range noLLM {
			extra[k] = v
		}
		f := newFixture(t, extra)
		_, err := Build(bg, f.cfg, f.st, Options{Log: quietLog()})
		if err == nil || !strings.Contains(err.Error(), "AGENT_LLM_BASE_URL") {
			t.Errorf("roles %s: Build err = %v, want one naming AGENT_LLM_BASE_URL", roles, err)
		}
	}

	// a provider given by the caller replaces the configured endpoint, but
	// the catalog still comes from the configuration
	f := newFixture(t, map[string]string{"AGENT_ROLES": "worker", "AGENT_LLM_BASE_URL": "-", "AGENT_LLM_MODEL": "-"})
	_, err := Build(bg, f.cfg, f.st, Options{Provider: llmtest.New(), Log: quietLog()})
	if err == nil || !strings.Contains(err.Error(), "AGENT_LLM_MODEL") {
		t.Errorf("provider without catalog: err = %v, want one naming AGENT_LLM_MODEL", err)
	}
}

func TestTheAPIRoleStartsWithoutAModelAndWithoutWorkers(t *testing.T) {
	f := newFixture(t, map[string]string{"AGENT_ROLES": "api", "AGENT_LLM_BASE_URL": "-", "AGENT_LLM_MODEL": "-"})
	a, err := Build(bg, f.cfg, f.st, Options{Log: quietLog()})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(a.workers) != 0 || a.scheduler != nil || a.runner != nil {
		t.Fatalf("api only: %d workers, scheduler %v, runner %v", len(a.workers), a.scheduler != nil, a.runner != nil)
	}
	f.enroll(a)
	r := start(t, a)
	if code, body := get(t, "http://"+a.APIAddr()+"/agent-api/healthz"); code != 200 || !strings.Contains(body, "ok") {
		t.Errorf("api healthz = %d %q", code, body)
	}
	if code, _ := get(t, "http://"+a.DebugAddr()+"/readyz"); code != 200 {
		t.Errorf("readyz without the worker role = %d, want 200", code)
	}
	// a job is never claimed: there is no worker
	j := f.enqueue(a, "hello")
	time.Sleep(200 * time.Millisecond)
	if s := f.job(j).State; s != store.JobQueued {
		t.Errorf("job = %s, want queued", s)
	}
	if err := r.stop(t, 15*time.Second); err != nil {
		t.Errorf("Run = %v", err)
	}
}

func TestWorkerIDsAreUnique(t *testing.T) {
	f := newFixture(t, map[string]string{"AGENT_WORKERS": "5"})
	a := f.build(llmtest.New())
	if len(a.workerIDs) != 5 {
		t.Fatalf("%d workers", len(a.workerIDs))
	}
	re := regexp.MustCompile(`^[A-Za-z0-9._-]+-[0-9]+-[0-4]$`)
	seen := map[string]bool{}
	for _, id := range a.workerIDs {
		if seen[id] || !re.MatchString(id) {
			t.Errorf("worker id %q (duplicate %v)", id, seen[id])
		}
		seen[id] = true
	}
}

// Rule 2: a queued chat job is claimed and finished end to end.
func TestAQueuedChatJobIsRunToTheEnd(t *testing.T) {
	f := newFixture(t, nil)
	p := llmtest.New(llmtest.Say("hello from the agent", 3, 4))
	a := f.build(p)
	f.enroll(a)
	j := f.enqueue(a, "say hello")
	r := start(t, a)

	f.waitState(j, store.JobSucceeded, 10*time.Second)
	if got := f.lastAnswer(j); got != "hello from the agent" {
		t.Errorf("answer = %q", got)
	}
	if n := len(p.Requests()); n != 1 {
		t.Errorf("%d model calls", n)
	}
	if err := r.stop(t, 15*time.Second); err != nil {
		t.Errorf("Run = %v", err)
	}
}

// Rule 3: a write waits for the approval, and the decision resumes the job.
func TestAnApprovedWriteResumesTheJobAndWritesOnce(t *testing.T) {
	f := newFixture(t, nil)
	p := llmtest.New(
		llmtest.Calls(1, 1, llm.ToolCall{ID: "w", Name: "files.write_new", Arguments: `{"path":"/a.txt","content":"hi"}`}),
		llmtest.Say("written", 1, 1),
	)
	a := f.build(p)
	f.enroll(a)
	j := f.enqueue(a, "write a file")
	r := start(t, a)

	f.waitState(j, store.JobWaitingApproval, 10*time.Second)
	if _, _, ok := f.fake.File(drive, "/a.txt"); ok {
		t.Fatal("written before the approval")
	}
	as := f.approvals(j)
	if len(as) != 1 || as[0].State != store.ApprovalPending {
		t.Fatalf("approvals = %+v", as)
	}
	if _, err := a.gate.Decide(bg, userID, "oidc-sub", []approvals.DecisionItem{
		{ApprovalID: as[0].ID, Decision: store.ApprovalApproved, ArgsHash: as[0].ArgsHash},
	}); err != nil {
		t.Fatal(err)
	}
	f.waitState(j, store.JobSucceeded, 10*time.Second)
	data, _, ok := f.fake.File(drive, "/a.txt")
	if !ok || string(data) != "hi" {
		t.Errorf("file = %q, %v", data, ok)
	}
	puts := 0
	for _, rq := range f.fake.Requests() {
		if rq.Method == "PUT" && rq.Path == "/remote.php/dav/spaces/"+drive+"/a.txt" {
			puts++
		}
	}
	if puts != 1 {
		t.Errorf("%d PUTs of the file, want 1", puts)
	}
	if got := f.lastAnswer(j); got != "written" {
		t.Errorf("answer = %q", got)
	}
	if err := r.stop(t, 15*time.Second); err != nil {
		t.Errorf("Run = %v", err)
	}
}

// Rule 4a: without a drain, a job blocked in a model call is cancelled and
// requeued with the shutdown delay, not failed, and Run returns.
func TestShutdownWithoutDrainRequeuesTheRunningJob(t *testing.T) {
	f := newFixture(t, nil)
	p := newBlocking()
	a := f.build(p)
	f.enroll(a)
	j := f.enqueue(a, "think hard")
	r := start(t, a)
	select {
	case <-p.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the model was not called")
	}
	stopped := time.Now()
	if err := r.stop(t, 15*time.Second); err != nil {
		t.Errorf("Run = %v", err)
	}
	select {
	case <-p.cancelled:
	default:
		t.Error("the model call was not cancelled")
	}
	got := f.job(j)
	if got.State != store.JobQueued {
		t.Fatalf("job = %s (%q), want queued", got.State, got.Error)
	}
	// agent.DefaultConfig().ShutdownRequeueDelay (2m)
	delay := agent.DefaultConfig().ShutdownRequeueDelay
	if lo := stopped.Add(delay - 5*time.Second); got.NotBefore.Before(lo) || got.NotBefore.After(time.Now().Add(delay+time.Second)) {
		t.Errorf("NotBefore = %v, want about %v after the stop at %v", got.NotBefore, delay, stopped)
	}
}

// Rule 4b: with a drain, Run stops claiming but lets the running job finish
// without cancelling it.
func TestShutdownWithADrainLetsTheRunningJobFinish(t *testing.T) {
	f := newFixture(t, map[string]string{"AGENT_SHUTDOWN_DRAIN": "30s", "AGENT_WORKERS": "2"})
	p := newBlocking()
	a := f.build(p)
	f.enroll(a)
	j := f.enqueue(a, "think hard")
	r := start(t, a)
	select {
	case <-p.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the model was not called")
	}
	r.cancel()
	// the API listener is closed in phase 1, while the job still runs
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", a.APIAddr(), 200*time.Millisecond)
		if err != nil {
			break
		}
		c.Close()
		if time.Now().After(deadline) {
			t.Fatal("the API listener is still open during the drain")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// the second worker stops claiming: a job enqueued now stays queued
	j2 := f.enqueue(a, "later")
	select {
	case err := <-r.done:
		r.done <- err
		t.Fatalf("Run returned during the drain: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	p.release <- llm.CompletionResponse{Message: llm.Message{Role: llm.RoleAssistant, Content: "done"}, Stop: llm.StopEnd}
	select {
	case err := <-r.done:
		if err != nil {
			t.Errorf("Run = %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after the job finished")
	}
	r.done <- nil
	select {
	case <-p.cancelled:
		t.Error("the model call was cancelled during the drain")
	default:
	}
	if got := f.job(j); got.State != store.JobSucceeded || f.lastAnswer(j) != "done" {
		t.Errorf("job = %s %q", got.State, f.lastAnswer(j))
	}
	if got := f.job(j2); got.State != store.JobQueued || got.Runs != 0 {
		t.Errorf("job enqueued during the drain = %s runs %d, want queued and never claimed", got.State, got.Runs)
	}
}

// Rule 5: Run returns when its context ends without any job, leaks no
// goroutine and leaves the store open.
func TestRunReturnsAndLeaksNothing(t *testing.T) {
	f := newFixture(t, map[string]string{"AGENT_WORKERS": "3"})
	before := settledGoroutines(0, 200*time.Millisecond)
	a := f.build(llmtest.New())
	f.enroll(a) // leaves idle connections to oCIS behind
	r := start(t, a)
	if code, _ := get(t, "http://"+a.DebugAddr()+"/readyz"); code != 200 {
		t.Errorf("readyz = %d", code)
	}
	if code, _ := get(t, "http://"+a.APIAddr()+"/agent-api/healthz"); code != 200 {
		t.Errorf("api healthz = %d", code)
	}
	if err := r.stop(t, 15*time.Second); err != nil {
		t.Errorf("Run = %v", err)
	}
	if after := settledGoroutines(before, 5*time.Second); after > before {
		t.Errorf("goroutines: %d before, %d after Run returned", before, after)
	}
	if err := f.st.View(bg, func(tx store.Tx) error { _, err := tx.GetSystem("instance_id"); return err }); err != nil {
		t.Errorf("the store must stay open after Run: %v", err)
	}
	if err := a.Run(bg); err == nil {
		t.Error("a second Run must be refused")
	}
}

func TestAListenerThatCannotBindFailsRun(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	f := newFixture(t, map[string]string{"AGENT_HTTP_ADDR": ln.Addr().String()})
	before := settledGoroutines(0, 200*time.Millisecond)
	a := f.build(llmtest.New())
	done := make(chan error, 1)
	go func() { done <- a.Run(bg) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Run = nil, want the listen error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
	if after := settledGoroutines(before, 5*time.Second); after > before {
		t.Errorf("goroutines: %d before, %d after", before, after)
	}
}

// Rule 6: the probes.
func TestReadinessFollowsTheWorkersAndTheStore(t *testing.T) {
	f := newFixture(t, nil)
	a := f.build(llmtest.New())
	var early int
	a.beforeWorkers = func() {
		early, _ = get(t, "http://"+a.DebugAddr()+"/readyz")
	}
	r := start(t, a)
	if early != 503 {
		t.Errorf("readyz before the workers = %d, want 503", early)
	}
	if code, body := get(t, "http://"+a.DebugAddr()+"/readyz"); code != 200 || !strings.Contains(body, "ready") {
		t.Errorf("readyz = %d %q", code, body)
	}
	for _, p := range []string{"/", "/agent/", "/agent-api/", "/agent-api/healthz", "/metrics", "/healthz/x", "/debug/pprof/"} {
		if code, _ := get(t, "http://"+a.DebugAddr()+p); code != 404 {
			t.Errorf("debug %s = %d, want 404", p, code)
		}
	}
	if err := f.st.Close(); err != nil {
		t.Fatal(err)
	}
	if code, body := get(t, "http://"+a.DebugAddr()+"/readyz"); code != 503 || !strings.Contains(body, `"not ready"`) {
		t.Errorf("readyz with the store closed = %d %q, want 503", code, body)
	}
	if code, _ := get(t, "http://"+a.DebugAddr()+"/healthz"); code != 200 {
		t.Errorf("healthz = %d, want 200 always", code)
	}
	if err := r.stop(t, 30*time.Second); err != nil {
		t.Errorf("Run = %v", err)
	}
}

func TestTheDebugListenerCanBeDisabled(t *testing.T) {
	f := newFixture(t, nil)
	f.cfg.Process.DebugAddr = ""
	a := f.build(llmtest.New())
	r := start(t, a)
	if a.DebugAddr() != "" {
		t.Errorf("DebugAddr = %q", a.DebugAddr())
	}
	if err := r.stop(t, 15*time.Second); err != nil {
		t.Errorf("Run = %v", err)
	}
}

// Rule 7: the kill switch command acts on a running app without a restart.
func TestTheKillSwitchCommandStopsAndResumesARunningApp(t *testing.T) {
	f := newFixture(t, nil)
	p := llmtest.New(llmtest.Say("resumed", 1, 1))
	a := f.build(p)
	f.enroll(a)
	r := start(t, a)

	var out bytes.Buffer
	if err := KillSwitch(bg, f.st, false, "on", &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "kill switch: on (database)\n" {
		t.Errorf("on printed %q", out.String())
	}
	j := f.enqueue(a, "hello")
	time.Sleep(1500 * time.Millisecond)
	if got := f.job(j); got.State != store.JobQueued || len(p.Requests()) != 0 {
		t.Fatalf("with the kill switch on: job %s, %d model calls", got.State, len(p.Requests()))
	}

	out.Reset()
	if err := KillSwitch(bg, f.st, false, "off", &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "kill switch: off (database)\n" {
		t.Errorf("off printed %q", out.String())
	}
	f.waitState(j, store.JobSucceeded, 15*time.Second)
	if err := r.stop(t, 15*time.Second); err != nil {
		t.Errorf("Run = %v", err)
	}
}

func TestTheKillSwitchStatus(t *testing.T) {
	f := newFixture(t, nil)
	for _, c := range []struct {
		db, env bool
		want    string
	}{
		{false, false, "kill switch: off (database)\n"},
		{false, true, "kill switch: off (database) (environment: on)\n"},
		{true, false, "kill switch: on (database)\n"},
		{true, true, "kill switch: on (database) (environment: on)\n"},
	} {
		if err := agent.SetKillSwitch(bg, f.st, c.db); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := KillSwitch(bg, f.st, c.env, "status", &out); err != nil {
			t.Errorf("status: %v", err)
		}
		if out.String() != c.want {
			t.Errorf("db %v env %v: %q, want %q", c.db, c.env, out.String(), c.want)
		}
	}
	var out bytes.Buffer
	for _, bad := range []string{"", "ON", "toggle"} {
		if err := KillSwitch(bg, f.st, false, bad, &out); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
	if out.Len() != 0 {
		t.Errorf("a refused command printed %q", out.String())
	}
}

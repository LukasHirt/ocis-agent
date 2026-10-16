package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/llm"
	"github.com/owncloud/ocis-agent/backend/internal/llm/llmtest"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

func TestACrashAfterTheAssistantMessageResumesWithoutCallingTheModelAgain(t *testing.T) {
	f := newFixture(t)
	f.fake.PutFile(drive, "/docs/x.txt", []byte("x"))
	f.seedJob(crashedTurn(1, list("c0", "/"), list("c1", "/docs")))
	p := llmtest.New(llmtest.Say("done", 1, 1))
	f.provider = p
	f.runOnce()
	if f.job().State != store.JobSucceeded {
		t.Fatalf("job = %+v", f.job())
	}
	reqs := p.Requests()
	if len(reqs) != 1 {
		t.Fatalf("%d model calls, want only the next turn", len(reqs))
	}
	ms := reqs[0].Messages[1:] // after the system prompt
	if len(ms) != 4 || ms[1].Role != llm.RoleAssistant || ms[2].ToolCallID != "c0" || ms[3].ToolCallID != "c1" ||
		!strings.Contains(ms[2].Content, "files.list): done") || !strings.Contains(ms[3].Content, "x.txt") {
		t.Errorf("messages = %+v", ms)
	}
	n := 0
	for _, s := range f.steps() {
		if s.Kind == "tool" && s.Status == "done" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("%d tool steps, want 2", n)
	}
}

func TestAnAutoWriteIsNotRepeatedAfterACrashBeforeTheResultWasSaved(t *testing.T) {
	f := newFixture(t)
	const file = memDir + "notes/n.md"
	p := llmtest.New(
		llmtest.Calls(1, 1, tc("m", "memory.write", `{"name":"notes/n.md","content":"remember this\n"}`)),
		llmtest.Say("ok", 1, 1),
	)
	f.provider = p
	f.enqueue("remember")
	f.faults.failAt("SaveCheckpoint", 2) // 1: the model turn, 2: the write's result
	c := f.claim()
	if err := f.runner().Run(bg, c); !errors.Is(err, errInjected) {
		t.Fatalf("err = %v", err)
	}
	if n := f.modifying(file); n != 1 {
		t.Fatalf("%d writes in the first run", n)
	}
	if un := f.checkpoint().Unresolved(); len(un) != 1 {
		t.Fatalf("the result must not be saved: unresolved = %+v", un)
	}

	f.requeueNow(c)
	f.runOnce()
	if f.job().State != store.JobSucceeded {
		t.Fatalf("job = %+v", f.job())
	}
	if n := f.modifying(file); n != 1 {
		t.Errorf("the memory file was written %d times, want 1", n)
	}
	if data, _, _ := f.fake.File(drive, file); string(data) != "remember this\n" {
		t.Errorf("file = %q", data)
	}
	tm := toolMessages(p.Requests()[1].Messages)
	if len(tm) != 1 || !strings.Contains(tm[0].Content, "memory.write): done") {
		t.Errorf("tool messages = %+v", tm)
	}
	if n := len(f.auditOf("action_executed")); n != 1 {
		t.Errorf("%d action_executed events", n)
	}
	if ch := f.auditOf("memory_changed"); len(ch) != 1 || !strings.Contains(ch[0].Detail, "remember this") {
		t.Errorf("memory_changed = %+v", ch)
	}
}

func TestAnInterruptedExecutionIsReconciledOnTheNextRun(t *testing.T) {
	f := newFixture(t)
	p, a := requestApproval(t, f)
	f.decide(a, store.ApprovalApproved)
	// the PUT lands, its response never arrives, the run gives up
	f.fake.LoseResponse("PUT", "/remote.php/dav/spaces/"+drive+"/a.txt", 1)
	ctx, cancel := context.WithTimeout(bg, time.Second)
	defer cancel()
	if err := f.runner().Run(ctx, f.claim()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if _, _, ok := f.fake.File(drive, "/a.txt"); !ok {
		t.Fatal("the PUT must have landed")
	}
	if st := f.approvals()[0].State; st != store.ApprovalExecuting {
		t.Fatalf("approval = %s, want executing (outcome not known)", st)
	}
	if rec := f.idem(a.IdemKey); rec.State != store.IdemStarted {
		t.Fatalf("idempotency = %s, want started", rec.State)
	}

	c := f.takeover()
	if err := f.runner().Run(bg, c); err != nil {
		t.Fatal(err)
	}
	if f.job().State != store.JobSucceeded {
		t.Fatalf("job = %+v", f.job())
	}
	if n := f.modifying("/a.txt"); n != 1 {
		t.Errorf("the file was written %d times, want 1", n)
	}
	if data, _, _ := f.fake.File(drive, "/a.txt"); string(data) != "hi" {
		t.Errorf("file = %q", data)
	}
	if st := f.approvals()[0].State; st != store.ApprovalExecuted {
		t.Errorf("approval = %s", st)
	}
	tm := toolMessages(p.Requests()[1].Messages)
	if len(tm) != 1 || !strings.Contains(tm[0].Content, "files.write_new): done") {
		t.Errorf("tool messages = %+v", tm)
	}
}

// snapshot is what the new lease holder owns: the job and its timeline.
type snapshot struct {
	job   store.Job
	steps int
	audit int
	ocis  int
}

func (f *fixture) snap() snapshot {
	return snapshot{job: f.job(), steps: len(f.steps()), audit: len(f.audit()), ocis: len(f.fake.Requests())}
}

// assertUntouched checks that the old runner changed nothing the new holder
// owns. extraAudit events (the executor's record of a known outcome) and
// extraOCIS requests are expected (-1: not checked here).
func assertUntouched(t *testing.T, f *fixture, before snapshot, holder store.Claim, extraAudit, extraOCIS int) {
	t.Helper()
	if n := len(f.audit()); n != before.audit+extraAudit {
		t.Errorf("%d audit events, want %d", n, before.audit+extraAudit)
	}
	if n := len(f.fake.Requests()); extraOCIS >= 0 && n != before.ocis+extraOCIS {
		t.Errorf("%d oCIS requests after the takeover, want %d", n-before.ocis, extraOCIS)
	}
	j := f.job()
	if j.State != store.JobRunning || string(j.Checkpoint) != string(before.job.Checkpoint) || j.Error != "" {
		t.Errorf("job changed: %s %q, checkpoint changed %v", j.State, j.Error, string(j.Checkpoint) != string(before.job.Checkpoint))
	}
	if n := len(f.steps()); n != before.steps {
		t.Errorf("%d steps, want %d: the old runner wrote to the timeline", n, before.steps)
	}
	if len(f.auditOf("job_finished")) != 0 {
		t.Error("the old runner ended the job")
	}
	if err := f.st.View(bg, func(tx store.Tx) error { return tx.AssertLease(userID, j.ID, holder.Fence) }); err != nil {
		t.Errorf("the new holder lost its lease: %v", err)
	}
}

func TestALostLeaseStopsTheRunnerWithoutWriting(t *testing.T) {
	f := newFixture(t)
	var holder store.Claim
	var before snapshot
	n := 0
	f.provider = llm.ProviderFunc(func(context.Context, llm.CompletionRequest) (llm.CompletionResponse, error) {
		n++
		if n == 2 { // between the turns, another worker takes the job over
			holder = f.takeover()
			before = f.snap()
		}
		if n == 3 {
			return llm.CompletionResponse{Message: llm.Message{Role: llm.RoleAssistant, Content: "new holder"}, Stop: llm.StopEnd}, nil
		}
		return llm.CompletionResponse{
			Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{list("p", "/")}},
			Stop:    llm.StopToolCalls,
		}, nil
	})
	f.enqueue("g")
	err := f.runner().Run(bg, f.claim())
	if !errors.Is(err, store.ErrFenceLost) {
		t.Fatalf("err = %v, want ErrFenceLost", err)
	}
	assertUntouched(t, f, before, holder, 0, 0)
	if n != 2 {
		t.Errorf("the old runner made %d model calls, want 2", n)
	}

	// the new holder carries on from its own checkpoint
	if err := f.runner().Run(bg, holder); err != nil {
		t.Fatal(err)
	}
	if f.job().State != store.JobSucceeded || f.checkpoint().Answer != "new holder" || f.checkpoint().Turns != 2 {
		t.Errorf("new holder: %s, %+v", f.job().State, f.checkpoint())
	}
}

func TestALostLeaseDuringAnAutoWriteRecordsTheOutcomeButNotTheStep(t *testing.T) {
	f := newFixture(t)
	const file = memDir + "notes/n.md"
	p := llmtest.New(
		llmtest.Calls(1, 1, tc("m", "memory.write", `{"name":"notes/n.md","content":"x"}`)),
		llmtest.Say("ok", 1, 1),
	)
	f.provider = p
	f.enqueue("g")
	var holder store.Claim
	var before snapshot
	f.faults.afterCommit("BeginIdempotent", func() { // between transactions A and B
		holder = f.takeover()
		before = f.snap()
	})
	err := f.runner().Run(bg, f.claim())
	if !errors.Is(err, store.ErrFenceLost) {
		t.Fatalf("err = %v, want ErrFenceLost", err)
	}
	assertUntouched(t, f, before, holder, 1, -1)
	if n := f.modifying(file); n != 1 {
		t.Fatalf("%d writes", n)
	}
	// after the takeover the old runner only finished the write under way
	for _, r := range f.fake.Requests()[before.ocis:] {
		if r.Path != "/remote.php/dav/spaces/"+drive+file {
			t.Errorf("old runner request after the takeover: %s %s", r.Method, r.Path)
		}
	}
	// the known outcome is kept for the new holder
	key := ""
	for _, ev := range f.auditOf("action_executed") {
		if ev.Outcome != "done" {
			t.Errorf("action_executed = %+v", ev)
		}
		key = ev.ArgsHash
	}
	if key == "" {
		t.Fatal("the outcome was not recorded")
	}

	if err := f.runner().Run(bg, holder); err != nil {
		t.Fatal(err)
	}
	if f.job().State != store.JobSucceeded {
		t.Fatalf("job = %+v", f.job())
	}
	if n := f.modifying(file); n != 1 {
		t.Errorf("the memory file was written %d times, want 1", n)
	}
	tm := toolMessages(p.Requests()[1].Messages)
	if len(tm) != 1 || !strings.Contains(tm[0].Content, "memory.write): done") {
		t.Errorf("tool messages = %+v", tm)
	}
}

func TestAnUnauthorizedFromOCISReportsTheCredentialAndRequeues(t *testing.T) {
	f := newFixture(t)
	f.fake.PutFile(drive, "/a.txt", []byte("x"))
	// the stat in Prepare works; the GET in Execute is refused
	f.fake.Fail("GET", "/remote.php/dav/spaces/"+drive+"/a.txt", 401, -1)
	p := llmtest.New(llmtest.Calls(1, 1, tc("r", "files.read_text", `{"path":"/a.txt"}`)))
	f.provider = p
	f.enqueue("g")
	f.runOnce()
	assertRequeued(t, f, DefaultConfig().PausedDelay)
	if n := f.creds.unauthorized.Load(); n != 1 {
		t.Errorf("HandleUnauthorized called %d times", n)
	}
	cp := f.checkpoint()
	if len(toolMessages(cp.Messages)) != 0 {
		t.Errorf("no tool message may be written: %+v", cp.Messages)
	}
	if un := cp.Unresolved(); len(un) != 1 || un[0].ID != "c0" {
		t.Errorf("unresolved = %+v", un)
	}
	for _, s := range f.steps() {
		if s.Kind == "tool" {
			t.Errorf("tool step %+v", s)
		}
	}
}

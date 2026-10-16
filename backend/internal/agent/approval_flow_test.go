package agent

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/approvals"
	"github.com/owncloud/ocis-agent/backend/internal/llm"
	"github.com/owncloud/ocis-agent/backend/internal/llm/llmtest"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/tools"
)

func writeNew(id, path, content string) llm.ToolCall {
	return tc(id, "files.write_new", `{"path":"`+path+`","content":"`+content+`"}`)
}

// requestApproval runs a job whose only call is a write of /a.txt up to its
// approval and returns the pending approval. The provider answers "written"
// on the next turn.
func requestApproval(t *testing.T, f *fixture) (*llmtest.Provider, store.Approval) {
	t.Helper()
	p := llmtest.New(llmtest.Calls(1, 1, writeNew("w", "/a.txt", "hi")), llmtest.Say("written", 1, 1))
	f.provider = p
	f.enqueue("write a file")
	f.runOnce()
	as := f.approvals()
	if len(as) != 1 || as[0].State != store.ApprovalPending {
		t.Fatalf("approvals = %+v", as)
	}
	return p, as[0]
}

func TestAWriteNeedingApprovalReleasesTheJobAndWritesNothing(t *testing.T) {
	f := newFixture(t)
	p, a := requestApproval(t, f)

	if j := f.job(); j.State != store.JobWaitingApproval {
		t.Fatalf("job = %s", j.State)
	}
	canon, err := tools.Canonicalize([]byte(`{"path":"/a.txt","content":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := tools.ArgsHash(canon)
	if a.CallID != "c0" || a.Tool != "files.write_new" || a.Risk != "write" || string(a.Args) != string(canon) ||
		a.ArgsHash != hash || a.IdemKey != tools.IdemKey(f.lastJob, "c0", hash) {
		t.Errorf("approval = %+v, want canonical args %s", a, canon)
	}
	req := f.auditOf("approval_requested")
	if len(req) != 1 || req[0].Detail != "approval="+a.ID || req[0].ArgsHash != hash {
		t.Errorf("approval_requested audit = %+v", req)
	}
	cp := f.checkpoint()
	last := cp.Messages[len(cp.Messages)-1]
	if last.Role != llm.RoleAssistant || len(last.ToolCalls) != 1 || last.ToolCalls[0].ID != "c0" {
		t.Errorf("last message = %+v", last)
	}
	if len(toolMessages(cp.Messages)) != 0 {
		t.Error("the call needing approval must have no result yet")
	}
	if un := cp.Unresolved(); len(un) != 1 || un[0].ID != "c0" {
		t.Errorf("unresolved = %+v", un)
	}
	if _, _, ok := f.fake.File(drive, "/a.txt"); ok || f.modifying("/a.txt") != 0 || f.executes("files.write_new") != 0 {
		t.Error("nothing may be written before the approval")
	}
	if len(p.Requests()) != 1 {
		t.Errorf("%d model calls", len(p.Requests()))
	}
}

func TestApprovingResumesTheJobAndExecutesExactlyOnce(t *testing.T) {
	f := newFixture(t)
	p := llmtest.New(
		llmtest.Calls(1, 1, list("l", "/"), writeNew("w", "/a.txt", "hi")),
		llmtest.Say("written", 1, 1),
	)
	f.provider = p
	f.enqueue("write a file")
	f.runOnce()

	// the read runs, the write waits for its approval
	if j := f.job(); j.State != store.JobWaitingApproval {
		t.Fatalf("job = %s", j.State)
	}
	as := f.approvals()
	if len(as) != 1 || as[0].CallID != "c1" || as[0].State != store.ApprovalPending || as[0].Tool != "files.write_new" {
		t.Fatalf("approvals = %+v", as)
	}
	a := as[0]
	if _, _, ok := f.fake.File(drive, "/a.txt"); ok || f.executes("files.write_new") != 0 {
		t.Fatal("nothing may be written before the approval")
	}
	if un := f.checkpoint().Unresolved(); len(un) != 1 || un[0].ID != "c1" {
		t.Fatalf("unresolved = %+v", un)
	}
	f.decide(a, store.ApprovalApproved)
	if f.job().State != store.JobQueued {
		t.Fatalf("job = %s after approval", f.job().State)
	}

	// first resume: the write lands, then the run dies before the tool
	// message is saved
	f.faults.failAt("SaveCheckpoint", 1)
	c := f.claim()
	if err := f.runner().Run(bg, c); !errors.Is(err, errInjected) {
		t.Fatalf("err = %v, want the injected fault", err)
	}
	if f.approvals()[0].State != store.ApprovalExecuted || f.modifying("/a.txt") != 1 || f.executes("files.write_new") != 1 {
		t.Fatalf("approval = %s, %d writes, %d executes", f.approvals()[0].State, f.modifying("/a.txt"), f.executes("files.write_new"))
	}

	// second resume: the stored outcome is reported, nothing runs again
	f.requeueNow(c)
	f.runOnce()
	if f.job().State != store.JobSucceeded {
		t.Fatalf("job = %+v", f.job())
	}
	if data, _, ok := f.fake.File(drive, "/a.txt"); !ok || string(data) != "hi" {
		t.Fatalf("file = %q %v", data, ok)
	}
	if n := f.executes("files.write_new"); n != 1 {
		t.Errorf("the write was executed %d times, want 1", n)
	}
	if n := f.modifying("/a.txt"); n != 1 {
		t.Errorf("the file was written %d times, want 1", n)
	}
	if n := len(f.auditOf("action_executed")); n != 1 {
		t.Errorf("%d action_executed events, want 1", n)
	}
	if st := f.approvals()[0].State; st != store.ApprovalExecuted {
		t.Errorf("approval = %s", st)
	}
	reqs := p.Requests()
	if len(reqs) != 2 {
		t.Fatalf("%d model calls", len(reqs))
	}
	tm := toolMessages(reqs[1].Messages)
	if len(tm) != 2 || tm[0].ToolCallID != "c0" || tm[1].ToolCallID != "c1" || !strings.Contains(tm[1].Content, "files.write_new): done") {
		t.Errorf("tool messages = %+v", tm)
	}
	if f.checkpoint().Answer != "written" {
		t.Errorf("answer = %q", f.checkpoint().Answer)
	}

	// a stale copy of the first claim (a third resume) runs nothing either
	if err := f.runner().Run(bg, c); !lost(err) {
		t.Errorf("stale claim: err = %v, want a lost lease", err)
	}
	if n, m := f.executes("files.write_new"), f.modifying("/a.txt"); n != 1 || m != 1 {
		t.Errorf("after a stale run: %d executes, %d writes, want 1 each", n, m)
	}
	if f.job().State != store.JobSucceeded || len(p.Requests()) != 2 {
		t.Errorf("a stale run changed the job: %s, %d model calls", f.job().State, len(p.Requests()))
	}
}

func TestRejectingTellsTheModelAndWritesNothing(t *testing.T) {
	f := newFixture(t)
	p, a := requestApproval(t, f)
	f.decide(a, store.ApprovalRejected)
	if d := f.auditOf("approval_decided"); len(d) != 1 || d[0].Outcome != "rejected" || d[0].Cred != "oidc" {
		t.Errorf("approval_decided = %+v", d)
	}
	f.runOnce()
	if f.job().State != store.JobSucceeded {
		t.Fatalf("job = %+v", f.job())
	}
	if _, _, ok := f.fake.File(drive, "/a.txt"); ok || f.modifying("/a.txt") != 0 || f.executes("files.write_new") != 0 ||
		len(f.auditOf("action_executed")) != 0 {
		t.Fatal("a rejected write must not happen")
	}
	tm := toolMessages(p.Requests()[1].Messages)
	if len(tm) != 1 || tm[0].ToolCallID != "c0" || !strings.Contains(tm[0].Content, "files.write_new): rejected") ||
		!strings.Contains(tm[0].Content, ReasonRejectedByUser) {
		t.Errorf("tool messages = %+v", tm)
	}
	var found bool
	for _, s := range f.steps() {
		if s.Kind == "tool" && s.CallID == "c0" && s.Status == "rejected" && s.ApprovalID == a.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("no rejected step: %+v", f.steps())
	}
}

func TestAnExpiredApprovalIsReportedToTheModel(t *testing.T) {
	f := newFixture(t)
	p, _ := requestApproval(t, f)
	f.clk.Advance(168*time.Hour + time.Minute)
	if n, err := f.gate.ExpireAndResume(bg); err != nil || n != 1 {
		t.Fatalf("ExpireAndResume = %d, %v", n, err)
	}
	if f.job().State != store.JobQueued {
		t.Fatalf("job = %s after the expiry", f.job().State)
	}
	f.runOnce()
	if f.job().State != store.JobSucceeded {
		t.Fatalf("job = %+v", f.job())
	}
	if f.modifying("/a.txt") != 0 || f.executes("files.write_new") != 0 || f.approvals()[0].State != store.ApprovalExpired {
		t.Fatalf("an expired approval must not run: %d writes, %s", f.modifying("/a.txt"), f.approvals()[0].State)
	}
	tm := toolMessages(p.Requests()[1].Messages)
	if len(tm) != 1 || tm[0].ToolCallID != "c0" || !strings.Contains(tm[0].Content, "files.write_new): expired") {
		t.Errorf("tool messages = %+v", tm)
	}
}

func TestAnApprovalThatRunsOutAfterTheDecisionIsNotExecuted(t *testing.T) {
	f := newFixture(t)
	p, a := requestApproval(t, f)
	f.decide(a, store.ApprovalApproved)
	f.clk.Advance(168*time.Hour + time.Minute) // approved, but resumed too late
	f.runOnce()
	if f.job().State != store.JobSucceeded {
		t.Fatalf("job = %+v", f.job())
	}
	if f.modifying("/a.txt") != 0 || f.executes("files.write_new") != 0 || f.approvals()[0].State != store.ApprovalExpired {
		t.Fatalf("%d writes, approval %s", f.modifying("/a.txt"), f.approvals()[0].State)
	}
	tm := toolMessages(p.Requests()[1].Messages)
	if len(tm) != 1 || !strings.Contains(tm[0].Content, "files.write_new): expired") {
		t.Errorf("tool messages = %+v", tm)
	}
}

func TestAMixedTurnKeepsOrderAndRequestsOneBatch(t *testing.T) {
	f := newFixture(t)
	p := llmtest.New(
		llmtest.Calls(1, 1,
			list("r", "/"),
			writeNew("w1", "/a.txt", "A"),
			tc("d", "files.nope", `{}`),
			writeNew("w2", "/b.txt", "B"),
		),
		llmtest.Say("done", 1, 1),
	)
	f.provider = p
	f.enqueue("mixed")
	f.runOnce()

	if f.job().State != store.JobWaitingApproval {
		t.Fatalf("job = %s", f.job().State)
	}
	// both approvals exist although the first request released the job: one
	// batch
	as := f.approvals()
	ids := map[string]store.Approval{}
	for _, a := range as {
		ids[a.CallID] = a
	}
	if len(as) != 2 || ids["c1"].State != store.ApprovalPending || ids["c3"].State != store.ApprovalPending {
		t.Fatalf("approvals = %+v", as)
	}
	cp := f.checkpoint()
	tm := toolMessages(cp.Messages)
	if len(tm) != 2 || tm[0].ToolCallID != "c0" || !strings.Contains(tm[0].Content, "files.list): done") ||
		tm[1].ToolCallID != "c2" || !strings.Contains(tm[1].Content, "files.nope): denied") {
		t.Fatalf("tool messages = %+v", tm)
	}
	if un := cp.Unresolved(); len(un) != 2 || un[0].ID != "c1" || un[1].ID != "c3" {
		t.Fatalf("unresolved = %+v", un)
	}

	f.decide(ids["c3"], store.ApprovalApproved)
	if f.job().State != store.JobWaitingApproval {
		t.Fatalf("job = %s with one approval still pending", f.job().State)
	}
	f.decide(ids["c1"], store.ApprovalApproved)
	f.runOnce()
	if f.job().State != store.JobSucceeded {
		t.Fatalf("job = %+v", f.job())
	}
	var puts []string
	dav := "/remote.php/dav/spaces/" + drive
	for _, r := range f.fake.Requests() {
		if r.Method == "PUT" && !strings.HasPrefix(r.Path, dav+"/.ocis-agent/") { // not the fixture's memory setup
			puts = append(puts, r.Path)
		}
	}
	if len(puts) != 2 || puts[0] != dav+"/a.txt" || puts[1] != dav+"/b.txt" {
		t.Errorf("PUTs = %v, want a.txt then b.txt once each", puts)
	}
	if n := f.executes("files.write_new"); n != 2 {
		t.Errorf("%d write executes, want 2", n)
	}
	for _, w := range []string{`{"content":"A","path":"/a.txt"}`, `{"content":"B","path":"/b.txt"}`} {
		if n := f.executes("files.write_new", w); n != 1 {
			t.Errorf("%s executed %d times, want 1", w, n)
		}
	}
	if n := f.executes("files.list"); n != 1 {
		t.Errorf("the read executed %d times, want 1", n)
	}
	if n := f.executes("files.nope"); n != 0 {
		t.Errorf("the denied call executed %d times", n)
	}
	reqs := p.Requests()
	if len(reqs) != 2 {
		t.Fatalf("%d model calls", len(reqs))
	}
	var order []string
	for _, m := range toolMessages(reqs[1].Messages) {
		order = append(order, m.ToolCallID)
	}
	if strings.Join(order, ",") != "c0,c2,c1,c3" {
		t.Errorf("tool message order = %v", order)
	}
}

func TestTheActionLimitDeniesTheBatchWithAFixedReason(t *testing.T) {
	f := newFixture(t)
	var err error
	if f.gate, err = approvals.NewGate(f.st, f.reg, approvals.Config{Now: f.clk.Now, MaxActionsPerJob: 1}); err != nil {
		t.Fatal(err)
	}
	p := llmtest.New(
		llmtest.Calls(1, 1, writeNew("w1", "/a.txt", "A"), writeNew("w2", "/b.txt", "B")),
		llmtest.Say("ok", 1, 1),
	)
	f.provider = p
	f.enqueue("two writes")
	f.runOnce()
	if f.job().State != store.JobSucceeded {
		t.Fatalf("job = %+v", f.job())
	}
	if len(f.approvals()) != 0 || f.modifying("/a.txt")+f.modifying("/b.txt") != 0 {
		t.Fatal("over the limit nothing may be requested or written")
	}
	tm := toolMessages(p.Requests()[1].Messages)
	want := []string{"c0", "c1"}
	if len(tm) != 2 {
		t.Fatalf("tool messages = %+v", tm)
	}
	for i, m := range tm {
		if m.ToolCallID != want[i] || m.Content != "Tool result (files.write_new): denied\n"+actionLimitText {
			t.Errorf("tool message %d = %+v", i, m)
		}
	}
	d := f.auditOf("policy_denied")
	if len(d) != 2 || d[0].Detail != actionLimitText || d[1].Detail != actionLimitText {
		t.Errorf("policy_denied = %+v", d)
	}
}

func TestAPendingApprovalOnResumeNeverExecutes(t *testing.T) {
	f := newFixture(t)
	p, a := requestApproval(t, f)
	f.decide(a, store.ApprovalApproved)
	// inconsistent on purpose: the job is queued, its approval pending
	f.rawSQL(`UPDATE approvals SET state = 'pending', decided_by = '', decided_at = NULL WHERE id = ?`, a.ID)
	c := f.claim()
	if err := f.runner().Run(bg, c); err != nil {
		t.Fatal(err)
	}
	assertRequeued(t, f, requeueDelay(DefaultConfig().RequeueDelay, c.Job.Runs))
	if f.modifying("/a.txt") != 0 || f.approvals()[0].State != store.ApprovalPending {
		t.Fatalf("%d writes, approval %s", f.modifying("/a.txt"), f.approvals()[0].State)
	}
	if n := len(f.auditOf("action_executed")); n != 0 {
		t.Errorf("%d action_executed events", n)
	}
	if f.executes("files.write_new") != 0 {
		t.Error("a pending approval was executed")
	}
	if rec, err := f.idemMaybe(a.IdemKey); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an idempotency record was begun: %+v, %v", rec, err)
	}
	if len(p.Requests()) != 1 {
		t.Errorf("%d model calls", len(p.Requests()))
	}
}

func TestTamperedStoredArgumentsFailTheJobAndAreAudited(t *testing.T) {
	f := newFixture(t)
	p, a := requestApproval(t, f)
	f.decide(a, store.ApprovalApproved)
	f.rawSQL(`UPDATE approvals SET args = ? WHERE id = ?`, `{"content":"evil","path":"/a.txt"}`, a.ID)
	f.runOnce()
	assertFailed(t, f, ReasonInternal)
	if f.modifying("/a.txt") != 0 || f.executes("files.write_new") != 0 {
		t.Fatal("tampered arguments must not run")
	}
	if ev := f.auditOf("tampered"); len(ev) != 1 || ev[0].Tool != "files.write_new" || ev[0].Outcome != "refused" {
		t.Errorf("tampered audit = %+v", ev)
	}
	if st := f.approvals()[0].State; st != store.ApprovalApproved {
		t.Errorf("approval = %s, want untouched", st)
	}
	if len(p.Requests()) != 1 {
		t.Errorf("%d model calls", len(p.Requests()))
	}
}

// expiredSteps counts the expired tool steps of call c0.
func expiredSteps(f *fixture, approvalID string) int {
	n := 0
	for _, s := range f.steps() {
		if s.Kind == "tool" && s.CallID == "c0" && s.Status == "expired" && s.ApprovalID == approvalID {
			n++
		}
	}
	return n
}

func TestAnExpiredApprovalGetsExactlyOneStep(t *testing.T) {
	t.Run("expired while pending", func(t *testing.T) {
		f := newFixture(t)
		_, a := requestApproval(t, f)
		f.clk.Advance(169 * time.Hour)
		if _, err := f.gate.ExpireAndResume(bg); err != nil {
			t.Fatal(err)
		}
		f.runOnce()
		if n := expiredSteps(f, a.ID); n != 1 {
			t.Errorf("%d expired steps, want 1: %+v", n, f.steps())
		}
	})
	t.Run("approved but overdue", func(t *testing.T) {
		f := newFixture(t)
		_, a := requestApproval(t, f)
		f.decide(a, store.ApprovalApproved)
		f.clk.Advance(169 * time.Hour)
		// the executor expires it and records the step; a crash before the
		// tool message is saved must not add a second one
		f.faults.failAt("SaveCheckpoint", 1)
		c := f.claim()
		if err := f.runner().Run(bg, c); !errors.Is(err, errInjected) {
			t.Fatalf("err = %v", err)
		}
		f.requeueNow(c)
		f.runOnce()
		if n := expiredSteps(f, a.ID); n != 1 {
			t.Errorf("%d expired steps, want 1: %+v", n, f.steps())
		}
	})
}

func TestTheCountingRegistryKeepsWhatTheToolsOffer(t *testing.T) {
	f := newFixture(t)
	def, err := tools.NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(def.Names(), ",") != strings.Join(f.reg.Names(), ",") {
		t.Fatalf("names %v != %v", f.reg.Names(), def.Names())
	}
	for _, n := range def.Names() {
		inner, _ := def.Get(n)
		wrapped, _ := f.reg.Get(n)
		_, ri := inner.(tools.Reconciler)
		_, rw := wrapped.(tools.Reconciler)
		if ri != rw || inner.AutoAllowed() != wrapped.AutoAllowed() || inner.Risk(nil) != wrapped.Risk(nil) {
			t.Errorf("%s: reconciler %v/%v, auto %v/%v", n, ri, rw, inner.AutoAllowed(), wrapped.AutoAllowed())
		}
	}
}

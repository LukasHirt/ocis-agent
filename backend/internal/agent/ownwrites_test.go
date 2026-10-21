package agent

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/owncloud/ocis-agent/backend/internal/llm/llmtest"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/tools"
)

// fakeOwnWrites records the calls around approved writes.
type fakeOwnWrites struct {
	mu      sync.Mutex
	before  [][]string
	users   []string
	after   int
	exists  func() bool // reports whether the target exists; called in BeforeWrite
	existed []bool
	during  func() // runs inside BeforeWrite
}

func (o *fakeOwnWrites) BeforeWrite(_ context.Context, env tools.Env, paths []string) func(context.Context) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.before = append(o.before, paths)
	o.users = append(o.users, env.UserID)
	if o.exists != nil {
		o.existed = append(o.existed, o.exists())
	}
	if o.during != nil {
		o.during()
	}
	return func(context.Context) {
		o.mu.Lock()
		defer o.mu.Unlock()
		o.after++
	}
}

func (o *fakeOwnWrites) counts() (int, int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.before), o.after
}

// approvedRun runs a job that asks for a write of /a.txt, decides the
// approval and runs the job again with the observer.
func approvedRun(t *testing.T, f *fixture, o *fakeOwnWrites, d store.ApprovalState, before func()) {
	t.Helper()
	_, a := requestApproval(t, f)
	f.decide(a, d)
	if before != nil {
		before()
	}
	deps := f.deps()
	deps.OwnWrites = o
	r, err := NewRunner(deps)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Run(bg, f.claim()); err != nil {
		t.Fatal(err)
	}
}

func TestAnApprovedWriteIsReportedAroundTheCall(t *testing.T) {
	f := newFixture(t)
	o := &fakeOwnWrites{exists: func() bool { _, _, ok := f.fake.File(drive, "/a.txt"); return ok }}
	approvedRun(t, f, o, store.ApprovalApproved, nil)
	if f.job().State != store.JobSucceeded {
		t.Fatalf("job = %+v", f.job())
	}
	if !slices.Equal(o.before[0], []string{"/a.txt"}) || len(o.before) != 1 || o.users[0] != userID {
		t.Fatalf("BeforeWrite calls = %q for %q", o.before, o.users)
	}
	if o.existed[0] {
		t.Error("BeforeWrite ran after the write")
	}
	if _, after := o.counts(); after != 1 {
		t.Errorf("after called %d times, want 1", after)
	}
}

func TestAFailedOrUnknownWriteIsNotRebaselined(t *testing.T) {
	t.Run("failed", func(t *testing.T) {
		f := newFixture(t)
		o := &fakeOwnWrites{}
		// the target appears before the write: write_new refuses
		approvedRun(t, f, o, store.ApprovalApproved, func() { f.fake.PutFile(drive, "/a.txt", []byte("user")) })
		if st := f.approvals()[0].State; st != store.ApprovalFailed {
			t.Fatalf("approval = %s", st)
		}
		if b, a := o.counts(); b != 1 || a != 0 {
			t.Fatalf("before %d, after %d; want 1, 0", b, a)
		}
	})
	t.Run("unknown", func(t *testing.T) {
		f := newFixture(t)
		o := &fakeOwnWrites{}
		approvedRun(t, f, o, store.ApprovalApproved, func() {
			f.fake.Fail("PUT", "/remote.php/dav/spaces/"+drive+"/a.txt", 500, -1)
		})
		if st := f.approvals()[0].State; st != store.ApprovalUnknown {
			t.Fatalf("approval = %s", st)
		}
		if b, a := o.counts(); b != 1 || a != 0 {
			t.Fatalf("before %d, after %d; want 1, 0", b, a)
		}
	})
}

func TestOnlyFreshApprovedWritesAreReported(t *testing.T) {
	t.Run("rejected", func(t *testing.T) {
		f := newFixture(t)
		o := &fakeOwnWrites{}
		approvedRun(t, f, o, store.ApprovalRejected, nil)
		if b, a := o.counts(); b != 0 || a != 0 {
			t.Fatalf("before %d, after %d for a rejected call", b, a)
		}
	})
	t.Run("reads and memory writes", func(t *testing.T) {
		f := newFixture(t)
		o := &fakeOwnWrites{}
		f.provider = llmtest.New(
			llmtest.Calls(1, 1, list("l", "/"), tc("m", "memory.write", args(map[string]string{"name": "notes/n.md", "content": "x"}))),
			llmtest.Say("done", 1, 1),
		)
		f.enqueue("g")
		deps := f.deps()
		deps.OwnWrites = o
		r, err := NewRunner(deps)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Run(bg, f.claim()); err != nil {
			t.Fatal(err)
		}
		if b, a := o.counts(); b != 0 || a != 0 {
			t.Fatalf("before %d, after %d for a read and a memory write", b, a)
		}
	})
	t.Run("a stored outcome", func(t *testing.T) {
		// the write is done, then the run dies before its tool message is
		// saved: the next run reports the stored outcome without a write
		f := newFixture(t)
		o := &fakeOwnWrites{}
		_, a := requestApproval(t, f)
		f.decide(a, store.ApprovalApproved)
		f.faults.failAt("SaveCheckpoint", 1)
		c := f.claim()
		if err := f.runner().Run(bg, c); err == nil {
			t.Fatal("the injected fault did not stop the run")
		}
		f.requeueNow(c)
		deps := f.deps()
		deps.OwnWrites = o
		r, err := NewRunner(deps)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Run(bg, f.claim()); err != nil {
			t.Fatal(err)
		}
		if f.job().State != store.JobSucceeded {
			t.Fatalf("job = %+v", f.job())
		}
		if b, a := o.counts(); b != 0 || a != 0 {
			t.Fatalf("before %d, after %d for an already executed call", b, a)
		}
	})
	t.Run("a reconciled call", func(t *testing.T) {
		// the PUT lands but its response is lost and the run stops: the
		// next run reconciles the executing approval without a write
		f := newFixture(t)
		o := &fakeOwnWrites{}
		_, a := requestApproval(t, f)
		f.decide(a, store.ApprovalApproved)
		lost := f.fake.LoseResponse("PUT", "/remote.php/dav/spaces/"+drive+"/a.txt", 1)
		ctx, cancel := context.WithCancel(bg)
		defer cancel()
		go func() {
			select {
			case <-lost:
				cancel()
			case <-ctx.Done():
			}
		}()
		if err := f.runner().Run(ctx, f.claim()); err == nil {
			t.Fatal("the run was not interrupted")
		}
		if st := f.approvals()[0].State; st != store.ApprovalExecuting {
			t.Fatalf("approval = %s", st)
		}
		deps := f.deps()
		deps.OwnWrites = o
		r, err := NewRunner(deps)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Run(bg, f.takeover()); err != nil {
			t.Fatal(err)
		}
		if st := f.approvals()[0].State; st != store.ApprovalExecuted {
			t.Fatalf("approval = %s", st)
		}
		if b, a := o.counts(); b != 0 || a != 0 {
			t.Fatalf("before %d, after %d for a reconciled call", b, a)
		}
	})
}

// The checks before an approved write run again after BeforeWrite, which may
// take a while: a pause or kill switch that arrives meanwhile stops the call.
func TestAPauseDuringBeforeWriteStopsTheApprovedWrite(t *testing.T) {
	f := newFixture(t)
	o := &fakeOwnWrites{}
	o.during = func() { f.setPolicy(func(p *store.Policy) { p.Paused, p.PauseReason = true, store.PauseUser }) }
	approvedRun(t, f, o, store.ApprovalApproved, nil)
	if n := f.executes("files.write_new"); n != 0 || f.modifying("/a.txt") != 0 {
		t.Fatalf("%d executes after a pause", n)
	}
	if st := f.approvals()[0].State; st != store.ApprovalApproved {
		t.Fatalf("approval = %s, want approved (resumed later)", st)
	}
	if f.job().State != store.JobQueued {
		t.Fatalf("job = %s, want requeued", f.job().State)
	}
	if _, after := o.counts(); after != 0 {
		t.Errorf("after called %d times", after)
	}
}

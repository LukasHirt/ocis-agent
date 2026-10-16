package agent

import (
	"testing"

	"github.com/owncloud/ocis-agent/backend/internal/llm/llmtest"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// approvedBatch runs a job up to two pending writes of /a.txt and /b.txt,
// approves both and returns them by call id. The next turn answers "done".
func approvedBatch(t *testing.T, f *fixture) map[string]store.Approval {
	t.Helper()
	f.provider = llmtest.New(
		llmtest.Calls(1, 1, writeNew("w1", "/a.txt", "A"), writeNew("w2", "/b.txt", "B")),
		llmtest.Say("done", 1, 1),
	)
	f.enqueue("two writes")
	f.runOnce()
	byCall := map[string]store.Approval{}
	for _, a := range f.approvals() {
		f.decide(a, store.ApprovalApproved)
		byCall[a.CallID] = a
	}
	if len(byCall) != 2 || f.job().State != store.JobQueued {
		t.Fatalf("approvals %+v, job %s", byCall, f.job().State)
	}
	return byCall
}

func (f *fixture) approvalState(id string) store.ApprovalState {
	f.t.Helper()
	for _, a := range f.approvals() {
		if a.ID == id {
			return a.State
		}
	}
	f.t.Fatalf("no approval %s", id)
	return ""
}

func TestTheKillSwitchStopsABatchBetweenTools(t *testing.T) {
	f := newFixture(t)
	as := approvedBatch(t, f)
	f.faults.afterCommit("FinishIdempotent", func() {
		if err := SetKillSwitch(bg, f.st, true); err != nil {
			t.Error(err)
		}
	})
	f.runOnce()
	if j := f.job(); j.State != store.JobCancelled || j.Error != ReasonKillSwitch {
		t.Fatalf("job = %s %q", j.State, j.Error)
	}
	if n := f.executes("files.write_new"); n != 1 {
		t.Fatalf("%d writes executed, want exactly 1", n)
	}
	if s := f.approvalState(as["c0"].ID); s != store.ApprovalExecuted {
		t.Errorf("first approval = %s", s)
	}
	if s := f.approvalState(as["c1"].ID); s != store.ApprovalApproved {
		t.Errorf("second approval = %s, want still approved", s)
	}
	if fin := f.auditOf("job_finished"); len(fin) != 1 || fin[0].Outcome != "cancelled" {
		t.Errorf("job_finished = %+v", fin)
	}
}

func TestAPauseStopsABatchBetweenToolsAndTheRestRunsOnceLater(t *testing.T) {
	f := newFixture(t)
	as := approvedBatch(t, f)
	f.faults.afterCommit("FinishIdempotent", func() {
		if err := f.st.InTx(bg, func(tx store.Tx) error {
			_, err := tx.PausePolicy(userID, store.PauseUser)
			return err
		}); err != nil {
			t.Error(err)
		}
	})
	f.runOnce()
	assertRequeued(t, f, DefaultConfig().PausedDelay)
	if n := f.executes("files.write_new"); n != 1 {
		t.Fatalf("%d writes executed during the pause, want 1", n)
	}
	if s := f.approvalState(as["c1"].ID); s != store.ApprovalApproved {
		t.Fatalf("second approval = %s, want still approved", s)
	}

	if err := f.st.InTx(bg, func(tx store.Tx) error {
		_, err := tx.ClearPause(userID, store.PauseUser)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.clk.Advance(DefaultConfig().PausedDelay)
	f.runOnce()
	if f.job().State != store.JobSucceeded {
		t.Fatalf("job = %+v", f.job())
	}
	for _, w := range []string{`{"content":"A","path":"/a.txt"}`, `{"content":"B","path":"/b.txt"}`} {
		if n := f.executes("files.write_new", w); n != 1 {
			t.Errorf("%s executed %d times, want 1", w, n)
		}
	}
	if s := f.approvalState(as["c1"].ID); s != store.ApprovalExecuted {
		t.Errorf("second approval = %s", s)
	}
}

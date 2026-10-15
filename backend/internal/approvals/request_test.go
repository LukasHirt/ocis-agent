package approvals

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// pending classifies write calls and returns them as Pending entries.
func (f *fixture) pending(c store.Claim, calls ...Call) []Pending {
	f.t.Helper()
	var out []Pending
	for _, cl := range calls {
		d := f.classify(c, cl)
		if d.Kind != KindApprove {
			f.t.Fatalf("%s: decision %+v, want KindApprove", cl.Tool, d)
		}
		out = append(out, Pending{Call: cl, Decision: d})
	}
	return out
}

// requested runs Request for the calls and returns the claim and approvals.
func (f *fixture) requested(calls ...Call) (store.Claim, []store.Approval) {
	f.t.Helper()
	c := f.claim()
	as, err := f.gate.Request(bg, c, f.pending(c, calls...), json.RawMessage(`{"step":1}`))
	if err != nil {
		f.t.Fatal(err)
	}
	return c, as
}

func TestRequestStoresEverythingTheDecisionNeeds(t *testing.T) {
	f := newFixture(t)
	f.fake.PutFile(drive, "/docs/a.md", []byte("old"))
	c := f.claim()
	p := f.pending(c,
		call("c1", "files.overwrite", `{"path":"/docs/a.md","content":"new"}`),
		call("c2", "files.create_folder", `{"path":"/docs/sub"}`),
	)
	as, err := f.gate.Request(bg, c, p, json.RawMessage(`{"step":7}`))
	if err != nil || len(as) != 2 {
		t.Fatalf("approvals = %+v %v", as, err)
	}
	a := as[0]
	if a.State != store.ApprovalPending || a.UserID != userID || a.JobID != c.Job.ID || a.CallID != "c1" || a.Tool != "files.overwrite" || a.Risk != "destructive" {
		t.Fatalf("approval = %+v", a)
	}
	d := p[0].Decision
	if string(a.Args) != string(d.Args) || a.ArgsHash != d.ArgsHash || a.IdemKey != d.IdemKey || a.TargetETag != d.Prepared.TargetETag || a.TargetETag == "" {
		t.Fatalf("approval does not carry the decision: %+v", a)
	}
	if string(a.Preview) != string(d.Prepared.Preview) || a.Rationale != "because" {
		t.Fatalf("preview/rationale: %s %q", a.Preview, a.Rationale)
	}
	if got := a.ExpiresAt.Sub(f.clk.Now()); got != 168*time.Hour {
		t.Fatalf("expires in %s", got)
	}
	if as[1].Risk != "write" || as[1].TargetETag != "" {
		t.Fatalf("second approval = %+v", as[1])
	}
}

func TestRequestReleasesTheJobAuditsAndKeepsTheCheckpoint(t *testing.T) {
	f := newFixture(t)
	c, as := f.requested(call("c1", "files.create_folder", `{"path":"/a"}`))
	if got := f.jobState(c.Job.ID); got != store.JobWaitingApproval {
		t.Fatalf("job state = %s", got)
	}
	var job store.Job
	_ = f.st.View(bg, func(tx store.Tx) error { job, _ = tx.GetJob(userID, c.Job.ID); return nil })
	if string(job.Checkpoint) != `{"step":1}` {
		t.Fatalf("checkpoint = %s", job.Checkpoint)
	}
	recs := f.records()
	if len(recs) != 1 {
		t.Fatalf("audit = %v", f.kinds())
	}
	r := recs[0]
	if r.Kind != "approval_requested" || r.Tool != "files.create_folder" || r.Risk != "write" || r.ArgsHash != as[0].ArgsHash || r.JobID != c.Job.ID || r.Outcome != "pending" || r.Detail != "approval="+as[0].ID || r.Cred != "" {
		t.Fatalf("record = %+v", r)
	}
	// the lease is free again: the same user's next job can be claimed
	err := f.st.InTx(bg, func(tx store.Tx) error {
		if _, err := tx.InsertJob(store.Job{UserID: userID, TriggerKind: "chat", Goal: "next"}); err != nil {
			return err
		}
		_, ok, err := tx.ClaimNextJob("w2", time.Minute)
		if err == nil && !ok {
			t.Error("the lease must be released")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRationaleIsCutAtAUTF8Boundary(t *testing.T) {
	f := newFixture(t)
	f.gate.cfg.RationaleMax = 10
	c := f.claim()
	cl := Call{ID: "c1", Tool: "files.create_folder", Args: json.RawMessage(`{"path":"/a"}`), Rationale: strings.Repeat("ü", 20)}
	as, err := f.gate.Request(bg, c, f.pending(c, cl), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(as[0].Rationale) > 10 || !utf8.ValidString(as[0].Rationale) || as[0].Rationale == "" {
		t.Fatalf("rationale = %q (%d bytes)", as[0].Rationale, len(as[0].Rationale))
	}
}

func TestRequestIsAllOrNothing(t *testing.T) {
	f := newFixture(t)
	c := f.claim()
	p := f.pending(c,
		call("c1", "files.create_folder", `{"path":"/a"}`),
		call("c2", "files.create_folder", `{"path":"/b"}`),
	)
	p[1] = p[0] // the same call twice: the second insert hits the unique idempotency key
	if _, err := f.gate.Request(bg, c, p, nil); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict from the store", err)
	}
	if n := len(f.approvals(c.Job.ID)); n != 0 {
		t.Fatalf("%d approval(s) survived a failed request", n)
	}
	if got := f.jobState(c.Job.ID); got != store.JobRunning {
		t.Fatalf("job state = %s, the job must still be running", got)
	}
	if k := f.kinds(); k != "" {
		t.Fatalf("audit = %s, nothing may be recorded", k)
	}
}

func TestRequestRefusesBadInputWithoutWriting(t *testing.T) {
	f := newFixture(t)
	c := f.claim()
	write := f.pending(c, call("c1", "files.create_folder", `{"path":"/a"}`))
	allow := f.classify(c, call("c2", "probe.read", `{}`))
	other := f.claim2(write[0])
	for name, p := range map[string][]Pending{
		"empty":             nil,
		"an allow":          {{Call: call("c2", "probe.read", `{}`), Decision: allow}},
		"a denial":          {{Call: call("c3", "x", `{}`), Decision: deny("no")}},
		"wrong call id":     {{Call: call("other", "files.create_folder", `{"path":"/a"}`), Decision: write[0].Decision}},
		"another job's key": other,
	} {
		if _, err := f.gate.Request(bg, c, p, nil); !errors.Is(err, ErrBadCall) {
			t.Errorf("%s: err = %v, want ErrBadCall", name, err)
		}
	}
	if n := len(f.approvals(c.Job.ID)); n != 0 || f.jobState(c.Job.ID) != store.JobRunning {
		t.Fatal("bad input must not change anything")
	}
}

// claim2 returns a Pending whose decision was made for a different job id.
func (f *fixture) claim2(p Pending) []Pending {
	d := p.Decision
	d.IdemKey = "01ANOTHERJOB:" + strings.SplitN(d.IdemKey, ":", 2)[1]
	return []Pending{{Call: p.Call, Decision: d}}
}

func TestRequestNeedsTheCurrentLeaseAndARunningJob(t *testing.T) {
	f := newFixture(t)
	c := f.claim()
	p := f.pending(c, call("c1", "files.create_folder", `{"path":"/a"}`))

	stale := c
	stale.Fence--
	if _, err := f.gate.Request(bg, stale, p, nil); !errors.Is(err, store.ErrFenceLost) {
		t.Fatalf("stale fence = %v, want ErrFenceLost", err)
	}

	if err := f.st.InTx(bg, func(tx store.Tx) error { return tx.CancelJob(userID, c.Job.ID) }); err != nil {
		t.Fatal(err)
	}
	if _, err := f.gate.Request(bg, c, p, nil); err == nil {
		t.Fatal("a cancelled job cannot request approvals")
	}
	if n := len(f.approvals(c.Job.ID)); n != 0 {
		t.Fatalf("%d approval(s) for a cancelled job", n)
	}
	if got := f.jobState(c.Job.ID); got != store.JobCancelled {
		t.Fatalf("job state = %s", got)
	}
}

func TestRequestRefusesABatchOverTheActionCapAtomically(t *testing.T) {
	f := newFixture(t)
	f.gate.cfg.MaxActionsPerJob = 3
	c := f.claim()
	p := f.pending(c,
		call("c1", "files.create_folder", `{"path":"/a"}`),
		call("c2", "files.create_folder", `{"path":"/b"}`),
		call("c3", "files.create_folder", `{"path":"/c"}`),
		call("c4", "files.create_folder", `{"path":"/d"}`),
	)
	if _, err := f.gate.Request(bg, c, p, nil); !errors.Is(err, ErrActionLimit) {
		t.Fatalf("err = %v, want ErrActionLimit", err)
	}
	if n := len(f.approvals(c.Job.ID)); n != 0 {
		t.Fatalf("%d approval(s) written", n)
	}
	if got := f.jobState(c.Job.ID); got != store.JobRunning {
		t.Fatalf("job state = %s, want running", got)
	}
	if k := f.kinds(); k != "" {
		t.Fatalf("audit = %s, nothing may be recorded", k)
	}
}

func TestRequestAcceptsABatchAtTheCapAndRefusesOneMore(t *testing.T) {
	f := newFixture(t)
	f.gate.cfg.MaxActionsPerJob = 3
	c := f.claim()
	p := f.pending(c,
		call("c1", "files.create_folder", `{"path":"/a"}`),
		call("c2", "files.create_folder", `{"path":"/b"}`),
		call("c3", "files.create_folder", `{"path":"/c"}`),
		call("c4", "files.create_folder", `{"path":"/d"}`),
	)
	if as, err := f.gate.Request(bg, c, p[:3], nil); err != nil || len(as) != 3 {
		t.Fatalf("batch of 3: %v %v", as, err)
	}
	if _, err := f.gate.Request(bg, c, p[3:], nil); !errors.Is(err, ErrActionLimit) {
		t.Fatalf("err = %v, want ErrActionLimit (3 live + 1 > 3)", err)
	}
	if n := len(f.approvals(c.Job.ID)); n != 3 {
		t.Fatalf("%d approvals, want 3", n)
	}
}

func TestRejectedApprovalsDoNotCountTowardsTheCap(t *testing.T) {
	f := newFixture(t)
	f.gate.cfg.MaxActionsPerJob = 2
	c := f.claim()
	p := f.pending(c,
		call("c1", "files.create_folder", `{"path":"/a"}`),
		call("c2", "files.create_folder", `{"path":"/b"}`),
		call("c3", "files.create_folder", `{"path":"/c"}`),
		call("c4", "files.create_folder", `{"path":"/d"}`),
	)
	as, err := f.gate.Request(bg, c, p[:2], nil)
	if err != nil {
		t.Fatal(err)
	}
	err = f.st.InTx(bg, func(tx store.Tx) error {
		for _, a := range as {
			if _, err := tx.DecideApproval(userID, a.ID, store.ApprovalRejected, userID, a.ArgsHash); err != nil {
				return err
			}
		}
		_, err := tx.ResumeJobIfSettled(userID, c.Job.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// the decisions settled the job back to queued; claim it again
	c2 := f.reclaim()
	if _, err := f.gate.Request(bg, c2, p[2:], nil); err != nil {
		t.Fatalf("rejected approvals must not count: %v", err)
	}
}

// reclaim claims the next queued job (the one resumed after its approvals settled).
func (f *fixture) reclaim() store.Claim {
	f.t.Helper()
	var c store.Claim
	err := f.st.InTx(bg, func(tx store.Tx) error {
		var ok bool
		var err error
		c, ok, err = tx.ClaimNextJob("w1", 2*time.Minute)
		if err == nil && !ok {
			f.t.Fatal("nothing to claim")
		}
		return err
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

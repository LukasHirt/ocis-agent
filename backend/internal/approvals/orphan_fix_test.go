package approvals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/auth"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/tools"
)

// storeOrphan builds, with store primitives only, what a worker crash leaves
// behind for user: an approved probe.write call moved to executing with its
// idempotency record started, and the job failed afterwards (released, so
// the user's next job can be claimed).
func (f *fixture) storeOrphan(user string) store.Approval {
	f.t.Helper()
	var a store.Approval
	err := f.st.InTx(bg, func(tx store.Tx) error {
		if _, err := tx.GetUser(user); errors.Is(err, store.ErrNotFound) {
			if err := tx.PutUser(store.User{ID: user, Name: "name-" + user, PersonalDriveID: "drive-" + user}); err != nil {
				return err
			}
		}
		job, err := tx.InsertJob(store.Job{UserID: user, TriggerKind: "chat", Goal: "g"})
		if err != nil {
			return err
		}
		c, ok, err := tx.ClaimNextJob("w1", 2*time.Minute)
		if err != nil || !ok || c.Job.ID != job.ID {
			return fmt.Errorf("claim: %v %v", ok, err)
		}
		a, err = tx.InsertApproval(store.Approval{
			UserID: user, JobID: job.ID, CallID: "c1", Tool: "probe.write", Risk: store.RiskWrite,
			Args: json.RawMessage(`{"n":1}`), ArgsHash: "sha256:h", Rationale: "because", Preview: json.RawMessage(`{}`),
			IdemKey: job.ID + ":c1:h", ExpiresAt: f.clk.Now().Add(time.Hour),
		}, c.Fence)
		if err != nil {
			return err
		}
		if _, err := tx.ReleaseJob(c, store.JobWaitingApproval, nil, ""); err != nil {
			return err
		}
		if _, err := tx.DecideApproval(user, a.ID, store.ApprovalApproved, user, a.ArgsHash); err != nil {
			return err
		}
		if _, err := tx.ResumeJobIfSettled(user, job.ID); err != nil {
			return err
		}
		if c, ok, err = tx.ClaimNextJob("w1", 2*time.Minute); err != nil || !ok || c.Job.ID != job.ID {
			return fmt.Errorf("reclaim: %v %v", ok, err)
		}
		if _, err := tx.TransitionApproval(user, a.ID, store.ApprovalApproved, store.ApprovalExecuting, store.ApprovalPatch{}); err != nil {
			return err
		}
		if _, err := tx.BeginIdempotent(store.IdemRecord{Key: a.IdemKey, UserID: user, Tool: a.Tool}); err != nil {
			return err
		}
		_, err = tx.ReleaseJob(c, store.JobFailed, nil, "crashed")
		return err
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return a
}

func (f *fixture) approvalFor(user, id string) store.Approval {
	f.t.Helper()
	var a store.Approval
	if err := f.st.View(bg, func(tx store.Tx) error {
		var err error
		a, err = tx.GetApproval(user, id)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return a
}

func (f *fixture) idemFor(user, key string) store.IdemRecord {
	f.t.Helper()
	var r store.IdemRecord
	if err := f.st.View(bg, func(tx store.Tx) error {
		var err error
		r, err = tx.GetIdempotent(user, key)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return r
}

// envBy fails for the users in bad with their error and hands out the
// fixture env for everyone else.
func (f *fixture) envBy(bad map[string]error) EnvFunc {
	return func(_ context.Context, user string) (tools.Env, error) {
		if err, ok := bad[user]; ok {
			return tools.Env{}, err
		}
		return f.env, nil
	}
}

func TestOrphansOfAUserWithoutCredentialsDoNotBlockOthers(t *testing.T) {
	f := newFixture(t)
	p := f.probes["probe.write"]
	p.recon = func(tools.ExecInfo) (tools.Reconciled, error) {
		return tools.Reconciled{State: tools.ReconDone, Result: tools.Result{Summary: "it is there"}}, nil
	}
	var dead []store.Approval
	for i := 0; i < 120; i++ {
		dead = append(dead, f.storeOrphan(userID))
	}
	f.clk.Advance(time.Second)
	healthy := f.storeOrphan("u2")
	env := f.envBy(map[string]error{userID: fmt.Errorf("agent: credentials: %w", auth.ErrCredentialInvalid)})

	first, err := f.ex.ReconcileOrphans(bg, env, f.due(), 100)
	if err != nil || first.Resolved != 100 || len(first.Errors) != 0 {
		t.Fatalf("first pass: resolved %d, %d errors, %v; want 100 settled", first.Resolved, len(first.Errors), err)
	}
	second, err := f.ex.ReconcileOrphans(bg, env, f.due(), 100)
	if err != nil || second.Resolved != 21 || len(second.Errors) != 0 {
		t.Fatalf("second pass: resolved %d, %d errors, %v; want the rest and the healthy user's", second.Resolved, len(second.Errors), err)
	}
	if got := f.approvalFor("u2", healthy.ID).State; got != store.ApprovalExecuted {
		t.Fatalf("healthy user's approval = %s, want executed", got)
	}
	for _, a := range dead {
		if got := f.approvalFor(userID, a.ID).State; got != store.ApprovalUnknown {
			t.Fatalf("approval of the user without credentials = %s, want unknown", got)
		}
	}
	if p.execs.Load() != 0 {
		t.Fatalf("execs = %d, want none", p.execs.Load())
	}
}

func TestAnOrphanOfAUserWithoutCredentialsIsSettledUnknown(t *testing.T) {
	for name, envErr := range map[string]error{
		"no valid credential": fmt.Errorf("agent: credentials: %w", auth.ErrCredentialInvalid),
		"user gone":           fmt.Errorf("agent: load user: %w", store.ErrNotFound),
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			p := f.probes["probe.write"]
			c, a := crashedAndEnded(t, f, p)
			stepsBefore := len(f.steps(c.Job.ID))
			sum, err := f.ex.ReconcileOrphans(bg, f.envBy(map[string]error{userID: envErr}), f.due(), 100)
			if err != nil || sum.Resolved != 1 || len(sum.Errors) != 0 {
				t.Fatalf("summary = %+v %v", sum, err)
			}
			rec := f.idemFor(userID, a.IdemKey)
			if rec.State != store.IdemFailed || !strings.Contains(string(rec.Result), `"outcome":"unknown"`) ||
				!strings.Contains(string(rec.Result), "credentials were unavailable") {
				t.Fatalf("record = %+v / %s", rec, rec.Result)
			}
			if got := f.approvalOf(a.ID); got.State != store.ApprovalUnknown || string(got.Result) != string(rec.Result) {
				t.Fatalf("approval = %s %s", got.State, got.Result)
			}
			recs := f.records()
			last := recs[len(recs)-1]
			if last.Kind != "action_executed" || last.Outcome != "unknown" || last.Tool != "probe.write" ||
				!strings.HasPrefix(last.Detail, "reconciled: ") || !strings.Contains(last.Detail, "credentials were unavailable") {
				t.Fatalf("last audit = %+v", last)
			}
			if got := len(f.steps(c.Job.ID)); got != stepsBefore {
				t.Fatalf("steps %d -> %d: an ended job gets no step", stepsBefore, got)
			}
			if p.execs.Load() != 1 {
				t.Fatalf("execs = %d, want only the original call", p.execs.Load())
			}
		})
	}
}

func TestAStuckOrphanIsSettledUnknownAfterTheGiveUpAge(t *testing.T) {
	transient := errors.New("connection refused")
	for name, tc := range map[string]struct {
		env   func(f *fixture) EnvFunc
		recon func(tools.ExecInfo) (tools.Reconciled, error)
	}{
		"failing env": {env: func(f *fixture) EnvFunc { return f.envBy(map[string]error{userID: transient}) }},
		"401 from the tool": {
			env: func(f *fixture) EnvFunc { return f.envFor() },
			recon: func(tools.ExecInfo) (tools.Reconciled, error) {
				return tools.Reconciled{}, fmt.Errorf("stat: %w", ocisclient.ErrUnauthorized)
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			p := f.probes["probe.write"]
			p.recon = tc.recon
			_, a := crashedAndEnded(t, f, p)
			sum, err := f.ex.ReconcileOrphans(bg, tc.env(f), f.due(), 100)
			if err != nil || sum.Resolved != 0 || len(sum.Errors) != 1 {
				t.Fatalf("young record: summary = %+v %v, want it left open", sum, err)
			}
			f.clk.Advance(23 * time.Hour)
			if sum, _ := f.ex.ReconcileOrphans(bg, tc.env(f), f.due(), 100); sum.Resolved != 0 {
				t.Fatalf("settled before the give-up age: %+v", sum)
			}
			f.clk.Advance(time.Hour)
			sum, err = f.ex.ReconcileOrphans(bg, tc.env(f), f.due(), 100)
			if err != nil || sum.Resolved != 1 || len(sum.Errors) != 0 {
				t.Fatalf("old record: summary = %+v %v, want it settled", sum, err)
			}
			if got := f.approvalOf(a.ID).State; got != store.ApprovalUnknown {
				t.Fatalf("approval = %s, want unknown", got)
			}
			if rec, _ := f.idem(a.IdemKey); rec.State != store.IdemFailed || !strings.Contains(string(rec.Result), `"outcome":"unknown"`) {
				t.Fatalf("record = %+v", rec)
			}
		})
	}
}

func TestTheGiveUpAgeIsConfigurable(t *testing.T) {
	f := newFixture(t)
	ex, err := NewExecutor(f.st, f.reg, Config{Now: f.clk.Now, OrphanGiveUp: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	_, a := crashedAndEnded(t, f, f.probes["probe.write"])
	f.clk.Advance(time.Hour)
	sum, err := ex.ReconcileOrphans(bg, f.envBy(map[string]error{userID: errors.New("timeout")}), f.due(), 100)
	if err != nil || sum.Resolved != 1 || f.approvalOf(a.ID).State != store.ApprovalUnknown {
		t.Fatalf("summary = %+v %v", sum, err)
	}
}

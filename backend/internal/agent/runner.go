package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/approvals"
	"github.com/owncloud/ocis-agent/backend/internal/auth"
	"github.com/owncloud/ocis-agent/backend/internal/llm"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/queue"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/tools"
)

// Deps are the runner's collaborators; every one is required.
type Deps struct {
	Store    store.Store
	Queue    *queue.Queue
	Provider llm.Provider
	Catalog  llm.Catalog
	Registry *tools.Registry
	Gate     *approvals.Gate
	Executor *approvals.Executor
	Env      EnvBuilder
	Creds    Credentials
	Cfg      Config
}

// Runner runs one claimed job at a time: model turns, the gate and the tools.
type Runner struct {
	d     Deps
	cfg   Config
	specs []llm.ToolSpec
}

// NewRunner checks the dependencies and the limits.
func NewRunner(d Deps) (*Runner, error) {
	if d.Store == nil || d.Queue == nil || d.Provider == nil || d.Catalog.Default == "" || d.Registry == nil ||
		d.Gate == nil || d.Executor == nil || d.Creds == nil || !d.Env.complete() {
		return nil, errors.New("agent: every runner dependency is required")
	}
	cfg := d.Cfg.withDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.UserDailyTokens < 1 {
		return nil, errors.New("agent: the operator's daily token cap is required")
	}
	var specs []llm.ToolSpec
	for _, s := range d.Registry.Specs() {
		specs = append(specs, llm.ToolSpec{Name: s.Name, Description: s.Description, Schema: s.Schema})
	}
	return &Runner{d: d, cfg: cfg, specs: specs}, nil
}

// Run runs the claimed job until it is released (succeeded, failed,
// cancelled, queued or waiting_approval) and returns nil. An error means the
// job may still be running and the caller decides: a lost lease or a job
// cancelled meanwhile (store.ErrFenceLost, store.ErrConflict) comes back
// wrapped, a context error untouched; anything else the caller requeues.
func (r *Runner) Run(ctx context.Context, claim store.Claim) error {
	if claim.Job.Runs > r.cfg.MaxRuns {
		return r.fail(ctx, claim, ReasonRunsLimit)
	}
	if _, _, stop, err := r.precheck(ctx, claim, atClaim); stop || err != nil {
		return err
	}
	cp, err := ParseCheckpoint(claim.Job.Checkpoint)
	if err != nil {
		return r.fail(ctx, claim, ReasonInternal)
	}
	if len(cp.Messages) == 0 {
		// a fresh job: a chat job's conversation, then its goal ending the
		// last user message
		cp.Messages = withGoal(r.history(ctx, claim.Job), claim.Job.Goal)
		if isChat(claim.Job) {
			// stored at once, even without history, so a requeued run is
			// never rebuilt and sees the same context
			raw, err := cp.Marshal()
			if err != nil {
				return r.fail(ctx, claim, ReasonTooLarge)
			}
			if err := r.d.Store.InTx(ctx, func(tx store.Tx) error {
				return tx.SaveCheckpoint(claim.Job.UserID, claim.Job.ID, raw, claim.Fence)
			}); err != nil {
				return fmt.Errorf("agent: save checkpoint: %w", err)
			}
		}
	}
	env, err := r.d.Env.Env(ctx, claim.Job.UserID)
	if errors.Is(err, auth.ErrCredentialInvalid) {
		return r.fail(ctx, claim, ReasonCredentials)
	}
	if err != nil {
		return err
	}
	for {
		// calls left from an earlier run (an approval, a crash) come first
		if len(cp.Unresolved()) > 0 {
			if cp.Turns >= r.cfg.MaxSteps {
				// a crash after the turn that reached the limit: its calls
				// never run
				return r.fail(ctx, claim, ReasonStepLimit)
			}
			if stop, err := r.processCalls(ctx, claim, env, &cp); stop || err != nil {
				return err
			}
		}
		if stop, err := r.turn(ctx, claim, env, &cp); stop || err != nil {
			return err
		}
	}
}

type phase int

const (
	atClaim phase = iota
	inRun
)

// precheck runs before every model call: gateCheck, then the daily token
// budget. stop means the job was released.
func (r *Runner) precheck(ctx context.Context, claim store.Claim, ph phase) (store.Policy, ocisclient.Creds, bool, error) {
	pol, creds, stop, err := r.gateCheck(ctx, claim, ph)
	if stop || err != nil {
		return pol, creds, stop, err
	}
	userID := claim.Job.UserID
	var in, out int64
	since := startOfDay(r.cfg.Now())
	if err := r.d.Store.View(ctx, func(tx store.Tx) error {
		var err error
		in, out, err = tx.SumJobTokens(userID, since)
		return err
	}); err != nil {
		return pol, creds, false, fmt.Errorf("agent: read token usage: %w", err)
	}
	// proactive jobs stop at 80% of the budget, so chat keeps the rest
	if store.BudgetReached(in+out, store.DailyBudget(pol.DailyTokens, r.cfg.UserDailyTokens), claim.Job.TriggerKind) {
		return pol, creds, true, r.fail(ctx, claim, ReasonBudget)
	}
	return pol, creds, false, nil
}

// gateCheck runs before every model call and every tool call: the kill
// switch (requeue at claim, cancel in a run), the pause (requeue) and the
// credentials (fail when invalid), all read from the database. It returns the
// current credentials, so a token renewed during the claim is used at once.
// stop means the job was released.
func (r *Runner) gateCheck(ctx context.Context, claim store.Claim, ph phase) (store.Policy, ocisclient.Creds, bool, error) {
	userID := claim.Job.UserID
	none := ocisclient.Creds{}
	on, err := KillSwitchOn(ctx, r.d.Store, r.cfg.KillSwitch)
	if err != nil {
		return store.Policy{}, none, false, err
	}
	if on {
		if ph == atClaim {
			return store.Policy{}, none, true, r.requeue(ctx, claim, r.cfg.PausedDelay)
		}
		return store.Policy{}, none, true, r.end(ctx, claim, ending{state: store.JobCancelled, reason: ReasonKillSwitch})
	}
	var pol store.Policy
	if err := r.d.Store.View(ctx, func(tx store.Tx) error {
		var err error
		pol, err = tx.GetPolicy(userID)
		return err
	}); err != nil {
		return store.Policy{}, none, false, fmt.Errorf("agent: read policy: %w", err)
	}
	if pol.Paused {
		return pol, none, true, r.requeue(ctx, claim, r.cfg.PausedDelay)
	}
	creds, err := r.d.Creds.AppCreds(ctx, userID)
	if err != nil {
		if errors.Is(err, auth.ErrCredentialInvalid) {
			return pol, none, true, r.fail(ctx, claim, ReasonCredentials)
		}
		return pol, none, false, fmt.Errorf("agent: credentials: %w", err)
	}
	return pol, creds, false, nil
}

// toolCheck is gateCheck before a tool call; it refreshes env's credentials.
// An approval stays as it is when the job stops here, so the next claim
// continues it unchanged.
func (r *Runner) toolCheck(ctx context.Context, claim store.Claim, env *tools.Env) (bool, error) {
	_, creds, stop, err := r.gateCheck(ctx, claim, inRun)
	if stop || err != nil {
		return true, err
	}
	env.Creds = creds
	return false, nil
}

func startOfDay(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// isCtxErr reports an error caused by the end of ctx.
func isCtxErr(ctx context.Context, err error) bool {
	return ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))
}

// Package approvals is the deterministic gate between the model and the
// user's data. It classifies each tool call (allow, needs approval, deny),
// requests, decides and expires approvals together with the job's state, and
// executes calls exactly once under the two-transaction protocol. Nothing the
// model or a file says can change a decision: the gate reads the tool's
// declared risk and the call's arguments, never the user's policy flags and
// never any text.
package approvals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/tools"
)

// ErrBadCall reports a caller bug (a malformed call id or a decision that does
// not fit the operation), not something the model did.
var ErrBadCall = errors.New("approvals: invalid call")

var callIDRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// Config tunes the gate and the executor. Zero fields take the defaults.
type Config struct {
	// ApprovalTTL is how long a pending approval stays valid (default 168h).
	ApprovalTTL time.Duration
	// MaxArgsBytes caps the raw size of one call's arguments (default 2 MiB,
	// room for a 1 MiB write).
	MaxArgsBytes int
	// MaxActionsPerJob caps the approvals one job may accumulate (default 50).
	MaxActionsPerJob int
	// RationaleMax caps the stored model explanation, in bytes (default 1000).
	RationaleMax int
	// Now is the clock (default time.Now).
	Now func() time.Time
}

func (c Config) withDefaults() Config {
	if c.ApprovalTTL <= 0 {
		c.ApprovalTTL = 168 * time.Hour
	}
	if c.MaxArgsBytes <= 0 {
		c.MaxArgsBytes = 2 << 20
	}
	if c.MaxActionsPerJob <= 0 {
		c.MaxActionsPerJob = 50
	}
	if c.RationaleMax <= 0 {
		c.RationaleMax = 1000
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// Call is one tool call proposed by the model. ID is the runner's own
// step-derived id (not the provider's): it becomes part of the idempotency
// key, so it must be unique within the job and contain no ':'.
type Call struct {
	ID        string
	Tool      string
	Args      json.RawMessage
	Rationale string // the model's explanation: untrusted, shown as such
}

// Kind is the gate's verdict.
type Kind int

// Verdicts.
const (
	// KindAllow: run it now (a read, or a memory write within its limits).
	KindAllow Kind = iota
	// KindApprove: the user must approve it first.
	KindApprove
	// KindDeny: refuse it; Reason goes back to the model.
	KindDeny
)

func (k Kind) String() string {
	switch k {
	case KindAllow:
		return "allow"
	case KindApprove:
		return "approve"
	case KindDeny:
		return "deny"
	}
	return "unknown"
}

// Decision is the result of classifying a call. For KindAllow and
// KindApprove the other fields are set; for KindDeny only Reason is.
type Decision struct {
	Kind   Kind
	Reason string // model-safe
	Tool   tools.Tool
	Risk   tools.Risk
	// Args are the canonical argument bytes: exactly what is hashed, stored,
	// previewed and later executed.
	Args     json.RawMessage
	ArgsHash string
	IdemKey  string
	Prepared tools.Prepared
}

func deny(reason string) Decision { return Decision{Kind: KindDeny, Reason: reason} }

// Gate classifies calls and drives the approval life cycle.
type Gate struct {
	st  store.Store
	reg *tools.Registry
	cfg Config
}

// NewGate returns a gate over the store and the tool registry.
func NewGate(st store.Store, reg *tools.Registry, cfg Config) (*Gate, error) {
	if st == nil || reg == nil {
		return nil, errors.New("approvals: a store and a tool registry are required")
	}
	return &Gate{st: st, reg: reg, cfg: cfg.withDefaults()}, nil
}

// Classify decides what to do with a call. It validates, canonicalises and
// hashes the arguments, asks the tool to Prepare (which has no side effects),
// and applies the rules: reads are allowed, memory writes (a write-risk tool
// that reports AutoAllowed) are allowed, everything else needs approval. An
// error is returned only for caller bugs (ErrBadCall) and for failures that
// are not the model's fault (a 401 or a network error from Prepare, a store
// error); the runner handles those.
func (g *Gate) Classify(ctx context.Context, claim store.Claim, env tools.Env, call Call) (Decision, error) {
	if !callIDRE.MatchString(call.ID) {
		return Decision{}, fmt.Errorf("%w: the call id must match %s", ErrBadCall, callIDRE)
	}
	if len(call.Args) > g.cfg.MaxArgsBytes {
		return deny("the arguments are too large"), nil
	}
	tool, ok := g.reg.Get(call.Tool)
	if !ok {
		return deny("unknown tool"), nil
	}
	canon, err := tools.Canonicalize(call.Args)
	if err != nil {
		return deny(err.Error()), nil
	}
	hash, err := tools.ArgsHash(canon)
	if err != nil {
		return deny(err.Error()), nil
	}
	prepared, err := tool.Prepare(ctx, env, canon)
	if err != nil {
		if errors.Is(err, tools.ErrInvalidArgs) || errors.Is(err, tools.ErrRefused) {
			return deny(err.Error()), nil
		}
		return Decision{}, err
	}
	risk := tool.Risk(canon)
	d := Decision{
		Tool: tool, Risk: risk, Args: canon, ArgsHash: hash,
		IdemKey: tools.IdemKey(claim.Job.ID, call.ID, hash), Prepared: prepared,
	}
	switch {
	case risk == tools.RiskRead:
		d.Kind = KindAllow
	case risk == tools.RiskWrite && tool.AutoAllowed():
		d.Kind = KindAllow
	default:
		n, err := g.countActions(ctx, claim)
		if err != nil {
			return Decision{}, err
		}
		if n >= g.cfg.MaxActionsPerJob {
			return deny(fmt.Sprintf("this job has reached its limit of %d actions that need approval", g.cfg.MaxActionsPerJob)), nil
		}
		d.Kind = KindApprove
	}
	return d, nil
}

// countActions counts the job's approvals that still matter (rejected and
// expired ones do not).
func (g *Gate) countActions(ctx context.Context, claim store.Claim) (int, error) {
	var list []store.Approval
	err := g.st.View(ctx, func(tx store.Tx) error {
		var err error
		list, err = tx.ListApprovals(claim.Job.UserID, store.ApprovalFilter{JobID: claim.Job.ID, Limit: 500})
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("approvals: count actions: %w", err)
	}
	n := 0
	for _, a := range list {
		if a.State != store.ApprovalRejected && a.State != store.ApprovalExpired {
			n++
		}
	}
	return n, nil
}

// Executor is implemented in a later task (TEMPORARY stub, replaced there).
type Executor struct{}

// NewExecutor is implemented in a later task (TEMPORARY stub, replaced there).
func NewExecutor(st store.Store, reg *tools.Registry, cfg Config) (*Executor, error) {
	return &Executor{}, nil
}

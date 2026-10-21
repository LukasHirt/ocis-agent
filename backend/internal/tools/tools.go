// Package tools holds the agent's tools. A tool validates and previews its
// arguments (Prepare), performs one call (Execute) and, if it has side
// effects, decides after a crash whether the call already happened
// (Reconcile). Tools never decide policy: they report a Risk and the approval
// gate decides. Everything a tool needs comes from an Env built on the
// server, so no model-supplied value can name a drive or a user.
package tools

import (
	"context"
	"encoding/json"

	"github.com/owncloud/ocis-agent/backend/internal/memory"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
)

// Risk is how much damage a tool call can do.
type Risk int

// Risk levels.
const (
	RiskRead Risk = iota
	RiskWrite
	RiskDestructive
	RiskOutward
)

func (r Risk) String() string {
	switch r {
	case RiskRead:
		return "read"
	case RiskWrite:
		return "write"
	case RiskDestructive:
		return "destructive"
	case RiskOutward:
		return "outward"
	}
	return "unknown"
}

// Limits bound what one call may read, write and list.
type Limits struct {
	MaxReadBytes  int64
	MaxWriteBytes int64
	MaxItems      int
}

// DefaultJobReadBytes is how much a whole job may read.
const DefaultJobReadBytes int64 = 20 << 20

// DefaultLimits returns 2 MiB read and 1 MiB write per call and 500 list items.
func DefaultLimits() Limits {
	return Limits{MaxReadBytes: 2 << 20, MaxWriteBytes: 1 << 20, MaxItems: 500}
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.MaxReadBytes <= 0 {
		l.MaxReadBytes = d.MaxReadBytes
	}
	if l.MaxWriteBytes <= 0 {
		l.MaxWriteBytes = d.MaxWriteBytes
	}
	if l.MaxItems <= 0 {
		l.MaxItems = d.MaxItems
	}
	return l
}

// Env is everything a tool call may use. The runner builds it on the server:
// DriveID is the user's personal space (a plain Graph id, never model input).
type Env struct {
	UserID       string
	DriveID      string
	Creds        ocisclient.Creds
	Files        *ocisclient.Client
	Memory       *memory.Service
	Reads        *ReadBudget    // per-job read allowance; nil means no cap
	MemoryWrites *memory.Budget // per-job memory write allowance; nil means no cap
	// WatchFolders are the policy's watch folders when the job started.
	// files.recent_changes lists only these (arguments can only narrow them).
	WatchFolders []string
	Limits       Limits
}

func (e Env) limits() Limits { return e.Limits.withDefaults() }

func (e Env) session() memory.Session {
	return memory.Session{UserID: e.UserID, DriveID: e.DriveID, Creds: e.Creds}
}

// Block is untrusted data a tool returns (file content, a listing, a name).
// The agent loop fences blocks before the model sees them.
type Block struct {
	Source  string `json:"source"`
	Path    string `json:"path,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
	Content string `json:"content"`
}

// Result is what a tool call returns. Summary is built only from numbers and
// constants, never from file names or content; those go into Blocks.
type Result struct {
	Summary string  `json:"summary"`
	Blocks  []Block `json:"blocks,omitempty"`
	Diff    string  `json:"diff,omitempty"`
}

// PreviewDoc is what the approval UI shows for a call, computed from the
// arguments (and, for overwrite and delete, from the current target), never
// from the model's description.
type PreviewDoc struct {
	Action string   `json:"action"`
	Paths  []string `json:"paths,omitempty"`
	Bytes  int64    `json:"bytes,omitempty"`
	SHA256 string   `json:"sha256,omitempty"`
	Detail string   `json:"detail,omitempty"`
}

// Prepared is the outcome of Prepare. TargetETag is the ETag of the existing
// target that an overwrite or delete is bound to; the approval stores it.
type Prepared struct {
	Preview    json.RawMessage
	TargetETag string
}

func (p PreviewDoc) prepared(targetETag string) (Prepared, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return Prepared{}, err
	}
	return Prepared{Preview: b, TargetETag: targetETag}, nil
}

// ExecInfo carries what the executor knows besides the arguments.
type ExecInfo struct {
	IdemKey    string
	TargetETag string // the approved target ETag; empty when the tool has none
}

// ReconState is the verdict of Reconcile.
type ReconState int

// Reconcile verdicts.
const (
	// ReconDone: the call already happened; record it as done.
	ReconDone ReconState = iota
	// ReconRetry: it certainly did not happen and may be executed again.
	ReconRetry
	// ReconFailed: it did not happen and must not be retried (a conflict).
	ReconFailed
	// ReconUnknown: it cannot be told; the user decides.
	ReconUnknown
)

// Reconciled is the outcome of Reconcile. Reason is model- and user-safe text.
type Reconciled struct {
	State  ReconState
	Result Result
	Reason string
}

// Tool is one capability of the agent.
type Tool interface {
	Name() string
	Description() string
	// Schema is the JSON schema of the arguments, shown to the model.
	Schema() json.RawMessage
	Risk(args json.RawMessage) Risk
	// AutoAllowed reports that the gate may run the call without an approval
	// (only memory writes, within their limits).
	AutoAllowed() bool
	// Prepare validates the arguments completely and computes the preview. It
	// has no side effects (it may read from oCIS).
	Prepare(ctx context.Context, env Env, args json.RawMessage) (Prepared, error)
	// Execute performs the call with the stored arguments.
	Execute(ctx context.Context, env Env, args json.RawMessage, info ExecInfo) (Result, error)
}

// Reconciler is implemented by every tool with side effects: after a crash
// between transaction A and B it decides whether the call already happened.
type Reconciler interface {
	Reconcile(ctx context.Context, env Env, args json.RawMessage, info ExecInfo) (Reconciled, error)
}

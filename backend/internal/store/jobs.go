package store

import (
	"encoding/json"
	"time"
)

// JobState is the lifecycle state of a job.
type JobState string

// Job states.
const (
	JobQueued          JobState = "queued"
	JobRunning         JobState = "running"
	JobWaitingApproval JobState = "waiting_approval"
	JobSucceeded       JobState = "succeeded"
	JobFailed          JobState = "failed"
	JobCancelled       JobState = "cancelled"
)

// Valid reports whether s is a known state.
func (s JobState) Valid() bool {
	switch s {
	case JobQueued, JobRunning, JobWaitingApproval, JobSucceeded, JobFailed, JobCancelled:
		return true
	}
	return false
}

// Terminal reports whether no further transitions are possible.
func (s JobState) Terminal() bool {
	return s == JobSucceeded || s == JobFailed || s == JobCancelled
}

// Job is a unit of agent work for one user.
type Job struct {
	ID          string
	UserID      string
	TriggerKind string
	TriggerRef  string
	Goal        string
	State       JobState
	Attempt     int
	Runs        int // times claimed; unlike Attempt it is never refunded, so release-to-queued loops can be capped
	NotBefore   time.Time
	Checkpoint  json.RawMessage
	Error       string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// JobFilter narrows ListJobs and ListJobSummaries.
type JobFilter struct {
	States []JobState
	Limit  int
	// Before, if set, keeps only the jobs strictly after this key in the
	// list order (created_at DESC, id DESC), i.e. older ones: keyset
	// pagination. Use the CreatedAt and ID of the last job of the previous
	// page; the key never changes, so a row that changed state since does
	// not shift the page.
	Before *PageKey
}

// PageKey is a position in a newest-first list: the row's sort time
// (jobs: CreatedAt, approvals: RequestedAt) and its id. Both are required
// (ErrInvalid otherwise).
type PageKey struct {
	At time.Time
	ID string
}

// JobSummary is a job without its checkpoint (which can be up to 1 MiB)
// plus the number of its steps: the projection for job lists.
type JobSummary struct {
	ID          string
	UserID      string
	TriggerKind string
	TriggerRef  string
	Goal        string
	State       JobState
	Attempt     int
	Runs        int
	NotBefore   time.Time
	Error       string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	StepCount   int
}

// JobStatus is the light lookup of a job (GetJobStatus): its state, its
// creation (the list's page key) and its last change, without the
// checkpoint.
type JobStatus struct {
	State     JobState
	CreatedAt time.Time
	UpdatedAt time.Time
}

// JobStep is one entry of a job's timeline.
type JobStep struct {
	JobID      string
	N          int
	Kind       string // "llm" or "tool"
	Tool       string
	CallID     string
	Status     string
	TokensIn   int64
	TokensOut  int64
	Summary    string
	ApprovalID string
	CreatedAt  time.Time
}

// Claim is a worker's hold on a user's lease and the job it runs. Fence is the
// lease generation every write of that worker must carry.
type Claim struct {
	Job       Job
	Worker    string
	Fence     int64
	ExpiresAt time.Time
}

// ReapResult counts what ReapExpired changed.
type ReapResult struct {
	Requeued int
	Failed   int
}

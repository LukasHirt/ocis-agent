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
	NotBefore   time.Time
	Checkpoint  json.RawMessage
	Error       string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// JobFilter narrows ListJobs.
type JobFilter struct {
	States []JobState
	Limit  int
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

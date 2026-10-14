package store

import (
	"encoding/json"
	"time"
)

// IdemState is the state of an idempotency record.
type IdemState string

// Idempotency states.
const (
	IdemStarted IdemState = "started"
	IdemDone    IdemState = "done"
	IdemFailed  IdemState = "failed"
)

// Valid reports whether s is a known state.
func (s IdemState) Valid() bool {
	return s == IdemStarted || s == IdemDone || s == IdemFailed
}

// IdemRecord remembers that a side-effecting call was started for a key, and
// how it ended. A record that stays started after a crash means the outcome
// of the call is unknown.
type IdemRecord struct {
	Key        string
	UserID     string
	Tool       string
	State      IdemState
	StartedAt  time.Time
	FinishedAt *time.Time
	Result     json.RawMessage
}

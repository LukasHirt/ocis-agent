package store

import (
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/audit"
)

// Tx is the set of operations available inside a transaction. Methods return
// ErrNotFound, ErrFenceLost or ErrConflict (wrapped or bare) as documented on
// each area's types.
type Tx interface {
	// Now returns the store clock's current time in UTC.
	Now() time.Time

	// GetSystem returns the value for key, or ErrNotFound.
	GetSystem(key string) (string, error)
	// SetSystem creates or replaces the value for key.
	SetSystem(key, value string) error

	// AppendAudit adds an event to the user's chain, assigning the sequence
	// number, previous hash and hash inside the transaction. A zero TS becomes
	// Now(). UserID and Kind are required.
	AppendAudit(ev audit.Event) (audit.Record, error)
	// ReadAudit returns records with seq > afterSeq in ascending order.
	// limit <= 0 means 100; the maximum is 1000.
	ReadAudit(userID string, afterSeq int64, limit int) ([]audit.Record, error)
	// VerifyAudit recomputes the user's whole chain. Failures wrap
	// audit.ErrChainBroken.
	VerifyAudit(userID string) error
}

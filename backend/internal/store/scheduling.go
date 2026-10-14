package store

import "time"

// TokenDue describes a stored app token that has used half of its lifetime (or
// is past it) and should be renewed.
type TokenDue struct {
	UserID    string
	Label     string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// Cursor is a user's trigger bookkeeping: where the file-event stream was
// last read, when the last proactive sweep ran, and the cached ETags of the
// memory files.
type Cursor struct {
	UserID         string
	SSELastEventID string
	LastSweepAt    *time.Time
	MemoryETags    map[string]string
}

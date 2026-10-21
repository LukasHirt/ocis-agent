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
// last read, when the last proactive sweep ran, the cached ETags of the
// memory files and the state of the watch-folder poll.
type Cursor struct {
	UserID         string
	SSELastEventID string
	LastSweepAt    *time.Time
	MemoryETags    map[string]string
	// WatchETags maps a watch folder to what its last poll saw (the
	// folder's ETag, or a digest of its listing): the baseline the next
	// poll compares with. Never nil when read.
	WatchETags map[string]string
	// WatchDirtySince is when the poll first saw a change that no job has
	// picked up yet (nil: nothing pending); WatchChangedAt is when it saw
	// the latest one.
	WatchDirtySince *time.Time
	WatchChangedAt  *time.Time
}

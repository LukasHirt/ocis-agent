package memory

import (
	"errors"
	"sync"
)

// ErrBudget means the job has used up its memory-write allowance.
var ErrBudget = errors.New("memory: the job's memory write budget is used up")

// Limits bound memory sizes. Zero fields take the defaults.
type Limits struct {
	MaxFileBytes int64 // largest memory file the agent may write
	MaxJobBytes  int64 // bytes of memory writes per job (callers build a Budget from it)
	MaxReadBytes int64 // largest memory file that is read at all
}

// DefaultLimits returns 64 KiB per file, 256 KiB per job and 2 MiB per read.
func DefaultLimits() Limits {
	return Limits{MaxFileBytes: 64 << 10, MaxJobBytes: 256 << 10, MaxReadBytes: 2 << 20}
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = d.MaxFileBytes
	}
	if l.MaxJobBytes <= 0 {
		l.MaxJobBytes = d.MaxJobBytes
	}
	if l.MaxReadBytes <= 0 {
		l.MaxReadBytes = d.MaxReadBytes
	}
	return l
}

// Budget counts the memory bytes one job may still write. A nil *Budget means
// no job cap.
type Budget struct {
	mu          sync.Mutex
	limit, used int64
}

// NewBudget returns a budget of limit bytes.
func NewBudget(limit int64) *Budget { return &Budget{limit: limit} }

// Reserve takes n bytes or fails with ErrBudget without taking anything.
func (b *Budget) Reserve(n int64) error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used+n > b.limit {
		return ErrBudget
	}
	b.used += n
	return nil
}

// Release gives back n reserved bytes (a write that did not happen).
func (b *Budget) Release(n int64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used -= n
	if b.used < 0 {
		b.used = 0
	}
}

// Used returns the reserved bytes.
func (b *Budget) Used() int64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

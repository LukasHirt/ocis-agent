package tools

import (
	"errors"
	"math"
	"sync"
)

// ErrReadBudget means the job has read its allowance.
var ErrReadBudget = errors.New("tools: the job's read budget is used up")

// ReadBudget counts the bytes one job may still read. A nil *ReadBudget means
// no cap.
type ReadBudget struct {
	mu          sync.Mutex
	limit, used int64
}

// NewReadBudget returns a budget of limit bytes.
func NewReadBudget(limit int64) *ReadBudget { return &ReadBudget{limit: limit} }

// Remaining returns how many bytes may still be read.
func (b *ReadBudget) Remaining() int64 {
	if b == nil {
		return math.MaxInt64
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.limit - b.used
}

// Take reserves n bytes or fails with ErrReadBudget, taking nothing.
func (b *ReadBudget) Take(n int64) error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if n < 0 || n > b.limit-b.used {
		return ErrReadBudget
	}
	b.used += n
	return nil
}

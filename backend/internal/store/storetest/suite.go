// Package storetest is the conformance suite every store implementation must
// pass. An implementation's tests call Run with a Factory.
package storetest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// Factory returns a fresh, empty store that reads time from now. The store is
// closed by the factory's own cleanup.
type Factory func(t *testing.T, now func() time.Time) store.Store

// Clock is a controllable clock for tests.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// NewClock returns a clock set to a fixed instant.
func NewClock() *Clock {
	return &Clock{t: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}

// Now returns the clock's current time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance moves the clock forward.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func bg() context.Context { return context.Background() }

func open(t *testing.T, f Factory) (store.Store, *Clock) {
	t.Helper()
	clk := NewClock()
	return f(t, clk.Now), clk
}

// mustTx runs fn in a write transaction and fails the test on error.
func mustTx(t *testing.T, st store.Store, fn func(tx store.Tx) error) {
	t.Helper()
	if err := st.InTx(bg(), fn); err != nil {
		t.Fatalf("InTx: %v", err)
	}
}

type suiteCase struct {
	name string
	run  func(t *testing.T, f Factory)
}

var cases []suiteCase

// register adds a conformance area; each area file calls it from init.
func register(name string, run func(t *testing.T, f Factory)) {
	cases = append(cases, suiteCase{name: name, run: run})
}

// Run executes the whole suite.
func Run(t *testing.T, newStore Factory) {
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) { c.run(t, newStore) })
	}
}

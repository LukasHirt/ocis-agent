// Package store defines the persistence interface for the sidecar's
// operational state. Implementations live in subpackages (sqlite today); the
// behaviour every implementation must show is encoded in the storetest suite.
package store

import (
	"context"
	"errors"
)

var (
	// ErrNotFound means the requested row does not exist.
	ErrNotFound = errors.New("store: not found")
	// ErrFenceLost means a write carried a lease fence that is no longer
	// current: another worker took over, the lease was released, or the job
	// was requeued.
	ErrFenceLost = errors.New("store: lease fence lost")
	// ErrConflict means the write conflicts with the current state of a row
	// (duplicate key, terminal job, ...).
	ErrConflict = errors.New("store: conflict")
)

// Store is the entry point to the operational state.
type Store interface {
	// InTx runs fn in one write transaction. It commits when fn returns nil and
	// rolls back when fn returns an error or panics.
	InTx(ctx context.Context, fn func(tx Tx) error) error
	// View runs fn in a read-only transaction. Any write inside fn fails.
	View(ctx context.Context, fn func(tx Tx) error) error
	// Close releases all connections.
	Close() error
}

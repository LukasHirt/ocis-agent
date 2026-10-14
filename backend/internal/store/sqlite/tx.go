package sqlite

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// querier is the subset of *sql.Tx the implementation needs.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type tx struct {
	ctx context.Context
	q   querier
	now func() time.Time
}

var _ store.Tx = (*tx)(nil)

// Now implements store.Tx.
func (t *tx) Now() time.Time { return t.now().UTC() }

func ms(v time.Time) int64 { return v.UnixMilli() }

func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func isForeignKey(err error) bool {
	return err != nil && strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}

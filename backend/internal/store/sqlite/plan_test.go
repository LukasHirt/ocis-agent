package sqlite

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// The keyset condition must let SQLite seek in the (user_id, time) index: a
// range on the time column, not a scan of all the user's rows from the
// newest one (walking every page would then be quadratic).
func TestKeysetPagesUseAnIndexRange(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "plan.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	key := &store.PageKey{At: time.Unix(1000, 0), ID: "01JX"}
	tail, args, err := jobListQuery("u1", store.JobFilter{Before: key, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	cond, cargs, err := keysetBefore("requested_at", key)
	if err != nil {
		t.Fatal(err)
	}
	for name, q := range map[string]struct {
		sql   string
		args  []any
		index string
	}{
		"jobs":      {`SELECT id` + tail, args, "jobs_user (user_id=? AND created_at<?)"},
		"approvals": {`SELECT id FROM approvals WHERE user_id = ?` + cond + ` ORDER BY requested_at DESC, id DESC LIMIT 10`, append([]any{"u1"}, cargs...), "approvals_user (user_id=? AND requested_at<?)"},
	} {
		rows, err := s.r.Query(`EXPLAIN QUERY PLAN `+q.sql, q.args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		_ = rows.Close()
		if !strings.Contains(strings.Join(plan, "\n"), q.index) {
			t.Errorf("%s plan = %q, want a range on %s", name, plan, q.index)
		}
	}
}

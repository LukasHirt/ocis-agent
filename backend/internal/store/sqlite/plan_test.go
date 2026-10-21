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
	ccond, ccargs, err := keysetBefore("updated_at", key)
	if err != nil {
		t.Fatal(err)
	}
	mcond, mcargs, err := keysetBefore("created_at", key)
	if err != nil {
		t.Fatal(err)
	}
	live, largs, err := countJobsQuery("u1", store.ProactiveTriggers, []store.JobState{store.JobQueued, store.JobRunning, store.JobWaitingApproval}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	daily, dargs, err := countJobsQuery("u1", store.ProactiveTriggers, nil, time.Unix(1000, 0))
	if err != nil {
		t.Fatal(err)
	}
	for name, q := range map[string]struct {
		sql   string
		args  []any
		index string
	}{
		"jobs":                  {`SELECT id` + tail, args, "jobs_user (user_id=? AND created_at<?)"},
		"approvals":             {`SELECT id FROM approvals WHERE user_id = ?` + cond + ` ORDER BY requested_at DESC, id DESC LIMIT 10`, append([]any{"u1"}, cargs...), "approvals_user (user_id=? AND requested_at<?)"},
		"conversations":         {`SELECT id FROM conversations WHERE user_id = ?` + ccond + ` ORDER BY updated_at DESC, id DESC LIMIT 10`, append([]any{"u1"}, ccargs...), "conversations_user (user_id=? AND updated_at<?)"},
		"messages":              {`SELECT id FROM messages WHERE conversation_id = ? AND user_id = ?` + mcond + ` ORDER BY created_at DESC, id DESC LIMIT 10`, append([]any{"c1", "u1"}, mcargs...), "messages_conversation (conversation_id=? AND created_at<?)"},
		"jobs since":            {`SELECT COUNT(*) FROM jobs WHERE user_id = ? AND created_at >= ? AND trigger_kind = ?`, []any{"u1", int64(1), "chat"}, "jobs_user (user_id=? AND created_at>?)"},
		"live proactive jobs":   {live, largs, "jobs_user_state (user_id=? AND state=?"},
		"proactive jobs today":  {daily, dargs, "jobs_user (user_id=? AND created_at>?)"},
		"change marks jobs":     {`SELECT COALESCE(MAX(updated_at), 0) FROM jobs WHERE user_id = ?`, []any{"u1"}, "jobs_user_updated (user_id=?)"},
		"change marks partial":  {partialMarkQuery, []any{"u1"}, "COVERING INDEX job_partials_user (user_id=? AND chat=?)"},
		"change marks messages": {`SELECT COALESCE(MAX(created_at), 0) FROM messages WHERE user_id = ?`, []any{"u1"}, "messages_user (user_id=?)"},
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

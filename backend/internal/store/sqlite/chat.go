package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

const (
	defaultChatLimit = 50
	maxChatLimit     = 200

	conversationColumns = `id, user_id, title, created_at, updated_at`
	messageColumns      = `id, conversation_id, user_id, role, content, job_id, created_at`
)

func chatLimit(n int) int {
	switch {
	case n <= 0:
		return defaultChatLimit
	case n > maxChatLimit:
		return maxChatLimit
	}
	return n
}

func scanConversation(row scanner) (store.Conversation, error) {
	var c store.Conversation
	var created, updated int64
	if err := row.Scan(&c.ID, &c.UserID, &c.Title, &created, &updated); err != nil {
		return store.Conversation{}, err
	}
	c.CreatedAt, c.UpdatedAt = fromMS(created), fromMS(updated)
	return c, nil
}

func scanMessage(row scanner) (store.Message, error) {
	var m store.Message
	var created int64
	if err := row.Scan(&m.ID, &m.ConversationID, &m.UserID, &m.Role, &m.Content, &m.JobID, &created); err != nil {
		return store.Message{}, err
	}
	m.CreatedAt = fromMS(created)
	return m, nil
}

// InsertConversation implements store.Tx.
func (t *tx) InsertConversation(c store.Conversation) (store.Conversation, error) {
	if c.UserID == "" {
		return store.Conversation{}, store.Invalid("a conversation needs a user id")
	}
	if err := store.ValidateTitle(c.Title); err != nil {
		return store.Conversation{}, err
	}
	n, err := t.CountConversations(c.UserID)
	if err != nil {
		return store.Conversation{}, err
	}
	if n >= store.MaxConversationsPerUser {
		return store.Conversation{}, fmt.Errorf("%w: %d conversations", store.ErrLimit, store.MaxConversationsPerUser)
	}
	if c.ID == "" {
		c.ID = store.NewID()
	}
	now := t.now()
	c.CreatedAt, c.UpdatedAt = now, now
	_, err = t.q.ExecContext(t.ctx, `INSERT INTO conversations(`+conversationColumns+`) VALUES(?,?,?,?,?)`,
		c.ID, c.UserID, c.Title, ms(c.CreatedAt), ms(c.UpdatedAt))
	switch {
	case isUnique(err):
		return store.Conversation{}, fmt.Errorf("%w: conversation %s exists", store.ErrConflict, c.ID)
	case isForeignKey(err):
		return store.Conversation{}, fmt.Errorf("%w: user %s", store.ErrNotFound, c.UserID)
	case err != nil:
		return store.Conversation{}, fmt.Errorf("sqlite: insert conversation: %w", err)
	}
	return c, nil
}

// GetConversation implements store.Tx.
func (t *tx) GetConversation(userID, id string) (store.Conversation, error) {
	c, err := scanConversation(t.q.QueryRowContext(t.ctx,
		`SELECT `+conversationColumns+` FROM conversations WHERE id = ? AND user_id = ?`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return store.Conversation{}, store.ErrNotFound
	}
	return c, err
}

// ListConversations implements store.Tx.
func (t *tx) ListConversations(userID string, f store.ConversationFilter) ([]store.Conversation, error) {
	cond, cargs, err := keysetBefore("updated_at", f.Before)
	if err != nil {
		return nil, err
	}
	args := append(append([]any{userID}, cargs...), chatLimit(f.Limit))
	rows, err := t.q.QueryContext(t.ctx, `SELECT `+conversationColumns+` FROM conversations WHERE user_id = ?`+
		cond+` ORDER BY updated_at DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Conversation
	for rows.Next() {
		c, err := scanConversation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// RenameConversation implements store.Tx.
func (t *tx) RenameConversation(userID, id, title string) error {
	if err := store.ValidateTitle(title); err != nil {
		return err
	}
	res, err := t.q.ExecContext(t.ctx, `UPDATE conversations SET title = ?, updated_at = ? WHERE id = ? AND user_id = ?`,
		title, ms(t.now()), id, userID)
	if err != nil {
		return fmt.Errorf("sqlite: rename conversation: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// DeleteConversation implements store.Tx.
func (t *tx) DeleteConversation(userID, id string) ([]string, error) {
	if _, err := t.GetConversation(userID, id); err != nil {
		return nil, err
	}
	rows, err := t.q.QueryContext(t.ctx,
		`SELECT id FROM jobs WHERE user_id = ? AND trigger_kind = 'chat' AND trigger_ref = ?
		   AND state NOT IN `+terminalJobStates+` ORDER BY created_at, id`, userID, id)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var jid string
		if err := rows.Scan(&jid); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, jid)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	// the conversation's messages go with it (ON DELETE CASCADE)
	if _, err := t.q.ExecContext(t.ctx, `DELETE FROM conversations WHERE id = ? AND user_id = ?`, id, userID); err != nil {
		return nil, fmt.Errorf("sqlite: delete conversation: %w", err)
	}
	return ids, nil
}

// InsertMessage implements store.Tx.
func (t *tx) InsertMessage(m store.Message) (store.Message, error) {
	if m.UserID == "" || m.ConversationID == "" {
		return store.Message{}, store.Invalid("a message needs a user id and a conversation id")
	}
	if err := store.ValidateMessage(m.Role, m.Content); err != nil {
		return store.Message{}, err
	}
	if _, err := t.GetConversation(m.UserID, m.ConversationID); err != nil {
		return store.Message{}, err
	}
	if m.JobID != "" {
		if _, err := t.GetJobStatus(m.UserID, m.JobID); err != nil {
			return store.Message{}, err
		}
	}
	if m.ID == "" {
		m.ID = store.NewID()
	}
	m.CreatedAt = t.now()
	_, err := t.q.ExecContext(t.ctx, `INSERT INTO messages(`+messageColumns+`) VALUES(?,?,?,?,?,?,?)`,
		m.ID, m.ConversationID, m.UserID, m.Role, m.Content, m.JobID, ms(m.CreatedAt))
	switch {
	case isUnique(err):
		return store.Message{}, fmt.Errorf("%w: message %s exists", store.ErrConflict, m.ID)
	case err != nil:
		return store.Message{}, fmt.Errorf("sqlite: insert message: %w", err)
	}
	if _, err := t.q.ExecContext(t.ctx, `UPDATE conversations SET updated_at = ? WHERE id = ? AND user_id = ?`,
		ms(m.CreatedAt), m.ConversationID, m.UserID); err != nil {
		return store.Message{}, fmt.Errorf("sqlite: bump conversation: %w", err)
	}
	return m, nil
}

// ListMessages implements store.Tx.
func (t *tx) ListMessages(userID, conversationID string, f store.MessageFilter) ([]store.Message, error) {
	cond, cargs, err := keysetBefore("created_at", f.Before)
	if err != nil {
		return nil, err
	}
	if _, err := t.GetConversation(userID, conversationID); err != nil {
		return nil, err
	}
	args := append(append([]any{conversationID, userID}, cargs...), chatLimit(f.Limit))
	return t.queryMessages(`SELECT `+messageColumns+` FROM messages WHERE conversation_id = ? AND user_id = ?`+
		cond+` ORDER BY created_at DESC, id DESC LIMIT ?`, args...)
}

// GetMessageKey implements store.Tx.
func (t *tx) GetMessageKey(userID, conversationID, id string) (store.PageKey, error) {
	var at int64
	err := t.q.QueryRowContext(t.ctx, `SELECT created_at FROM messages WHERE id = ? AND user_id = ? AND conversation_id = ?`,
		id, userID, conversationID).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return store.PageKey{}, store.ErrNotFound
	}
	if err != nil {
		return store.PageKey{}, fmt.Errorf("sqlite: get message key: %w", err)
	}
	return store.PageKey{At: fromMS(at), ID: id}, nil
}

// ListRecentMessages implements store.Tx.
func (t *tx) ListRecentMessages(userID, conversationID string, n int, excludeJobID string) ([]store.Message, error) {
	if n <= 0 {
		return nil, store.Invalid("n must be at least 1")
	}
	n = min(n, store.MaxRecentMessages)
	if _, err := t.GetConversation(userID, conversationID); err != nil {
		return nil, err
	}
	out, err := t.queryMessages(`SELECT `+messageColumns+` FROM messages
		WHERE conversation_id = ? AND user_id = ? AND role IN ('user','assistant') AND (? = '' OR job_id <> ?)
		ORDER BY created_at DESC, id DESC LIMIT ?`, conversationID, userID, excludeJobID, excludeJobID, n)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func (t *tx) queryMessages(query string, args ...any) ([]store.Message, error) {
	rows, err := t.q.QueryContext(t.ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CountJobsInStates implements store.Tx.
func (t *tx) CountJobsInStates(userID string, states []store.JobState, triggerKind string) (int, error) {
	query := `SELECT COUNT(*) FROM jobs WHERE user_id = ?`
	args := []any{userID}
	if len(states) > 0 {
		marks := make([]string, len(states))
		for i, s := range states {
			if !s.Valid() {
				return 0, store.Invalid("unknown job state")
			}
			marks[i] = "?"
			args = append(args, string(s))
		}
		query += ` AND state IN (` + strings.Join(marks, ",") + `)`
	}
	if triggerKind != "" {
		query += ` AND trigger_kind = ?`
		args = append(args, triggerKind)
	}
	var n int
	err := t.q.QueryRowContext(t.ctx, query, args...).Scan(&n)
	return n, err
}

// CountJobsSince implements store.Tx. It is a range on jobs_user
// (user_id, created_at).
func (t *tx) CountJobsSince(userID, triggerKind string, since time.Time) (int, error) {
	query := `SELECT COUNT(*) FROM jobs WHERE user_id = ? AND created_at >= ?`
	args := []any{userID, ms(since)}
	if triggerKind != "" {
		query += ` AND trigger_kind = ?`
		args = append(args, triggerKind)
	}
	var n int
	err := t.q.QueryRowContext(t.ctx, query, args...).Scan(&n)
	return n, err
}

// CountConversations implements store.Tx.
func (t *tx) CountConversations(userID string) (int, error) {
	var n int
	err := t.q.QueryRowContext(t.ctx, `SELECT COUNT(*) FROM conversations WHERE user_id = ?`, userID).Scan(&n)
	return n, err
}

// partialMarkQuery is the newest partial write of the user's running chat
// jobs (a range on jobs_user_state).
const partialMarkQuery = `SELECT COALESCE(MAX(partial_at), 0) FROM jobs WHERE user_id = ? AND state = 'running' AND trigger_kind = 'chat'`

// ChangeMarks implements store.Tx. Each mark is an indexed MAX; the message
// mark also covers conversation creation and renames through the
// conversation's updated_at and the partial answers of running chat jobs.
func (t *tx) ChangeMarks(userID string) (store.ChangeMarks, error) {
	var m store.ChangeMarks
	var convAt, partialAt int64
	for _, q := range []struct {
		dst *int64
		sql string
	}{
		{&m.JobsAt, `SELECT COALESCE(MAX(updated_at), 0) FROM jobs WHERE user_id = ?`},
		{&m.MessagesAt, `SELECT COALESCE(MAX(created_at), 0) FROM messages WHERE user_id = ?`},
		{&convAt, `SELECT COALESCE(MAX(updated_at), 0) FROM conversations WHERE user_id = ?`},
		{&partialAt, partialMarkQuery},
		{&m.PolicyAt, `SELECT COALESCE(MAX(updated_at), 0) FROM policies WHERE user_id = ?`},
		{&m.AuditSeq, `SELECT COALESCE(MAX(seq), 0) FROM audit WHERE user_id = ?`},
	} {
		if err := t.q.QueryRowContext(t.ctx, q.sql, userID).Scan(q.dst); err != nil {
			return store.ChangeMarks{}, fmt.Errorf("sqlite: change marks: %w", err)
		}
	}
	m.MessagesAt = max(m.MessagesAt, convAt, partialAt)
	return m, nil
}

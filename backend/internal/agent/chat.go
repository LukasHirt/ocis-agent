package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/owncloud/ocis-agent/backend/internal/llm"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// Limits of a chat job's conversation prefix.
const (
	historyMessages        = 20
	maxHistoryMessageBytes = 4 << 10
	maxHistoryBytes        = 24 << 10
)

// truncMarker ends a text cut to a cap (the same marker as the API views).
const truncMarker = "…[truncated]"

// isChat reports a chat job: its TriggerRef names its conversation.
func isChat(j store.Job) bool {
	return j.TriggerKind == store.TriggerChat && j.TriggerRef != ""
}

// capText makes s valid UTF-8 and caps it at limit bytes including
// truncMarker, cut at a rune boundary.
func capText(s string, limit int) string {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= limit {
		return s
	}
	cut := limit - len(truncMarker)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + truncMarker
}

// history returns the conversation before a fresh chat job's goal: the last
// historyMessages user and assistant messages (not the job's own), oldest
// first, each capped at maxHistoryMessageBytes and together at most
// maxHistoryBytes (the oldest go first). User messages newer than the job
// are later instructions, not its context. Assistant messages keep their
// role: they are earlier model output, never part of the system prompt. Any
// failure (a deleted conversation, a store error) means no history: the job
// runs without it.
func (r *Runner) history(ctx context.Context, j store.Job) []llm.Message {
	if !isChat(j) {
		return nil
	}
	var ms []store.Message
	if err := r.d.Store.View(ctx, func(tx store.Tx) error {
		var err error
		ms, err = tx.ListRecentMessages(j.UserID, j.TriggerRef, historyMessages, j.ID)
		return err
	}); err != nil {
		return nil
	}
	var out []llm.Message
	total := 0
	for _, m := range ms {
		var role llm.Role
		switch m.Role {
		case store.RoleUser:
			if m.CreatedAt.After(j.CreatedAt) {
				continue
			}
			role = llm.RoleUser
		case store.RoleAssistant:
			role = llm.RoleAssistant
		default:
			continue
		}
		c := capText(m.Content, maxHistoryMessageBytes)
		if c == "" {
			continue
		}
		out = append(out, llm.Message{Role: role, Content: c})
		total += len(c)
	}
	for len(out) > 0 && total > maxHistoryBytes {
		total -= len(out[0].Content)
		out = out[1:]
	}
	return out
}

// errChatMessage marks a failed insert of a chat job's final message. It
// deliberately does not wrap the store's error, so that a store.ErrConflict
// is not mistaken for a job someone else ended.
var errChatMessage = errors.New("agent: record the chat message")

// recordEnd inserts the conversation message of a chat job that ends in
// state, inside the transaction that releases it: the answer as an
// assistant message for succeeded, the fixed reason as a system message
// otherwise. A deleted conversation gets nothing. The release happens once
// (only a running job under its own fence can be released into a terminal
// state), so the message is written once.
func recordEnd(tx store.Tx, j store.Job, e ending) error {
	if !isChat(j) || !e.state.Terminal() {
		return nil
	}
	m := store.Message{ConversationID: j.TriggerRef, UserID: j.UserID, JobID: j.ID}
	switch {
	case e.state == store.JobSucceeded:
		m.Role, m.Content = store.RoleAssistant, capText(e.answer, store.MaxAnswerMessageBytes)
		if m.Content == "" {
			m.Content = noAnswer
		}
	default:
		m.Role, m.Content = store.RoleSystem, e.reason
		if m.Content == "" {
			m.Content = ReasonInternal
		}
	}
	_, err := tx.InsertMessage(m)
	if errors.Is(err, store.ErrNotFound) {
		return nil // the conversation was deleted meanwhile
	}
	if err != nil {
		return fmt.Errorf("%w: %v", errChatMessage, err)
	}
	return nil
}

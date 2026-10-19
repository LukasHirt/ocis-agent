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

// truncMarker ends a text cut to a cap. It and capText duplicate the API
// views' marker and helper on purpose (agent must not import api); they are
// to be shared in one place later.
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

// history returns the conversation before a fresh chat job's goal, oldest
// first, built from the user and assistant messages of other jobs:
//   - user messages created after the job are later questions, not its
//     context (dropped before the window is cut, so they use no slot);
//   - then the last historyMessages messages, each capped at
//     maxHistoryMessageBytes;
//   - an assistant message stays only together with the user message of the
//     same job (an answer whose question was cut off, newer than the job or
//     without a job is never context), so the prefix never starts with one;
//   - consecutive user messages (a question whose job failed) are merged into
//     one, joined by a blank line;
//   - the oldest messages go first until the merged prefix holds at most
//     maxHistoryBytes.
//
// Assistant messages keep their role: they are earlier model output, never
// part of the system prompt. Any failure (a deleted conversation, a store
// error) means no history: the job runs without it.
func (r *Runner) history(ctx context.Context, j store.Job) []llm.Message {
	if !isChat(j) {
		return nil
	}
	var ms []store.Message
	if err := r.d.Store.View(ctx, func(tx store.Tx) error {
		var err error
		ms, err = tx.ListRecentMessages(j.UserID, j.TriggerRef, store.MaxRecentMessages, j.ID)
		return err
	}); err != nil {
		return nil
	}
	var es []histEntry
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
		es = append(es, histEntry{role: role, content: m.Content, jobID: m.JobID})
	}
	if len(es) > historyMessages {
		es = es[len(es)-historyMessages:]
	}
	for i := range es {
		es[i].content = capText(es[i].content, maxHistoryMessageBytes)
	}
	es = dropOrphans(es)
	for len(es) > 0 && mergedSize(es) > maxHistoryBytes {
		es = dropOrphans(es[1:])
	}
	return merge(es)
}

type histEntry struct {
	role    llm.Role
	content string
	jobID   string
}

// dropOrphans drops empty messages and the assistant messages whose job has
// no user message in es.
func dropOrphans(es []histEntry) []histEntry {
	asked := map[string]bool{}
	for _, e := range es {
		if e.role == llm.RoleUser && e.jobID != "" && e.content != "" {
			asked[e.jobID] = true
		}
	}
	var out []histEntry
	for _, e := range es {
		if e.content == "" || (e.role == llm.RoleAssistant && !asked[e.jobID]) {
			continue
		}
		out = append(out, e)
	}
	return out
}

const userJoin = "\n\n"

// mergedSize is the byte size of merge(es).
func mergedSize(es []histEntry) int {
	n := 0
	for i, e := range es {
		n += len(e.content)
		if i > 0 && e.role == llm.RoleUser && es[i-1].role == llm.RoleUser {
			n += len(userJoin)
		}
	}
	return n
}

// merge turns es into messages, joining consecutive user messages.
func merge(es []histEntry) []llm.Message {
	var out []llm.Message
	for _, e := range es {
		if n := len(out); n > 0 && e.role == llm.RoleUser && out[n-1].Role == llm.RoleUser {
			out[n-1].Content += userJoin + e.content
			continue
		}
		out = append(out, llm.Message{Role: e.role, Content: e.content})
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

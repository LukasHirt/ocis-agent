package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/owncloud/ocis-agent/backend/internal/audit"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// Conversations and chat messages. Everything is scoped by the principal's
// user id: another user's conversation or message is not found, and a
// cursor of another user's (or another conversation's) row is a 400.
//
// A message the user sends becomes a chat job in the same transaction
// (trigger_kind chat, trigger_ref the conversation, Goal the content byte
// for byte): the runner builds the history itself. The text is never
// logged or audited.

// Chat limits of the API.
const (
	maxChatMessageBytes = 8192     // the content of a user's message
	chatMessageMaxBody  = 32 << 10 // POST /conversations/{id}/messages (control characters escape to 6 bytes)
	maxLiveChatJobs     = 5        // queued, running or waiting_approval chat jobs per user
	maxChatJobsPerDay   = 1000     // chat jobs a user may start per UTC day (429 limit_reached)
	maxChatPageLimit    = 100
	titleFromMessage    = 60 // runes of the first message that name an untitled conversation
	// pendingRetryAfter is the Retry-After of too_many_pending: a chat job
	// usually ends within seconds.
	pendingRetryAfter = 5 * time.Second
)

// messageCooldown is the per-user pause between two sent messages.
const messageCooldown = time.Second

// errDailyChatJobs refuses a message over maxChatJobsPerDay.
var errDailyChatJobs = newAPIError(http.StatusTooManyRequests, "limit_reached")

var liveJobStates = []store.JobState{store.JobQueued, store.JobRunning, store.JobWaitingApproval}

func conversationViewOf(c store.Conversation) conversationView {
	return conversationView{ID: c.ID, Title: c.Title, CreatedAt: timeView(c.CreatedAt), UpdatedAt: timeView(c.UpdatedAt)}
}

// messageViewOf caps the content of every role (only assistant and system
// messages can be longer than the cap in the store, but the cap holds for
// all).
func messageViewOf(m store.Message) messageView {
	return messageView{ID: m.ID, Role: m.Role, Content: capText(m.Content, maxAnswerView), JobID: m.JobID, CreatedAt: timeView(m.CreatedAt)}
}

type titleBody struct {
	Title json.RawMessage `json:"title"`
}

// createConversation is POST /agent-api/v1/conversations with an empty
// body, {} or {"title": "..."}: 201 with the conversation. 409 not_enrolled
// for a user who has not enrolled, 409 limit_reached at
// store.MaxConversationsPerUser.
func (s *Server) createConversation(w http.ResponseWriter, r *http.Request) error {
	var body titleBody
	if err := decodeOptionalBody(w, r, &body); err != nil {
		return err
	}
	title := ""
	if len(body.Title) > 0 {
		var err error
		if title, err = jsonString(body.Title); err != nil {
			return err
		}
	}
	p, ok := principal(r.Context())
	if !ok {
		return errNoPrincipal
	}
	var c store.Conversation
	err := s.d.Store.InTx(r.Context(), func(tx store.Tx) error {
		if err := enrolled(tx, p.UserID); err != nil {
			return err
		}
		var err error
		c, err = tx.InsertConversation(store.Conversation{UserID: p.UserID, Title: title})
		return err
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, conversationViewOf(c))
	return nil
}

// enrolled is errNotEnrolled for a user the store does not know.
func enrolled(tx store.Tx, userID string) error {
	_, err := tx.GetUser(userID)
	if errors.Is(err, store.ErrNotFound) {
		return errNotEnrolled
	}
	return err
}

// listConversations is GET /agent-api/v1/conversations?limit=&cursor=
// (most recently updated first). The order is by updated_at, so a
// conversation that gets a message during a walk moves above the cursor:
// the UI reloads from the top on a change.
func (s *Server) listConversations(w http.ResponseWriter, r *http.Request) error {
	p, ok := principal(r.Context())
	if !ok {
		return errNoPrincipal
	}
	q, err := query(r, "limit", "cursor")
	if err != nil {
		return err
	}
	limit, cursor, err := pageParams(q, maxChatPageLimit)
	if err != nil {
		return err
	}
	var entries []json.RawMessage
	var next *string
	err = s.d.Store.View(r.Context(), func(tx store.Tx) error {
		f := store.ConversationFilter{Limit: limit + 1}
		if cursor != "" {
			c, err := tx.GetConversation(p.UserID, cursor)
			if errors.Is(err, store.ErrNotFound) {
				return errBadQuery
			}
			if err != nil {
				return err
			}
			f.Before = &store.PageKey{At: c.UpdatedAt, ID: c.ID}
		}
		list, err := tx.ListConversations(p.UserID, f)
		if err != nil {
			return err
		}
		entries, next, err = encodePage(list, limit, func(c store.Conversation) string { return c.ID },
			func(c store.Conversation) (any, error) { return conversationViewOf(c), nil })
		return err
	})
	if err != nil {
		return err
	}
	writePage(w, "conversations", entries, next)
	return nil
}

// conversationID is the {id} of the path, or errNotFound for an id of the
// wrong shape.
func conversationID(r *http.Request) (string, error) {
	id := r.PathValue("id")
	if !validID(id) {
		return "", errNotFound
	}
	return id, nil
}

// getConversation is GET /agent-api/v1/conversations/{id}.
func (s *Server) getConversation(w http.ResponseWriter, r *http.Request) error {
	p, ok := principal(r.Context())
	if !ok {
		return errNoPrincipal
	}
	id, err := conversationID(r)
	if err != nil {
		return err
	}
	var c store.Conversation
	if err := s.d.Store.View(r.Context(), func(tx store.Tx) error {
		var err error
		c, err = tx.GetConversation(p.UserID, id)
		return err
	}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, conversationViewOf(c))
	return nil
}

// renameConversation is PUT /agent-api/v1/conversations/{id} with
// {"title": "..."} (required; "" clears it): 200 with the conversation.
func (s *Server) renameConversation(w http.ResponseWriter, r *http.Request) error {
	var body titleBody
	if err := decodeBody(w, r, &body); err != nil {
		return err
	}
	title, err := jsonString(body.Title)
	if err != nil {
		return err
	}
	p, ok := principal(r.Context())
	if !ok {
		return errNoPrincipal
	}
	id, err := conversationID(r)
	if err != nil {
		return err
	}
	var c store.Conversation
	err = s.d.Store.InTx(r.Context(), func(tx store.Tx) error {
		if err := tx.RenameConversation(p.UserID, id, title); err != nil {
			return err
		}
		var err error
		c, err = tx.GetConversation(p.UserID, id)
		return err
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, conversationViewOf(c))
	return nil
}

// deleteConversation is DELETE /agent-api/v1/conversations/{id}: in one
// transaction the conversation and its messages go, every chat job of it
// that has not ended is cancelled (a worker running one notices at its next
// write) and conversation_deleted is audited (which also moves the audit
// change mark the event stream watches). 204.
func (s *Server) deleteConversation(w http.ResponseWriter, r *http.Request) error {
	p, ok := principal(r.Context())
	if !ok {
		return errNoPrincipal
	}
	id, err := conversationID(r)
	if err != nil {
		return err
	}
	err = s.d.Store.InTx(r.Context(), func(tx store.Tx) error {
		jobs, err := tx.DeleteConversation(p.UserID, id)
		if err != nil {
			return err
		}
		n := 0
		for _, j := range jobs {
			switch err := tx.CancelJob(p.UserID, j); {
			case err == nil:
				n++
			case errors.Is(err, store.ErrConflict): // ended meanwhile
			default:
				return err
			}
		}
		return s.appendAudit(tx, p.UserID, "conversation_deleted", "deleted", "jobs_cancelled="+strconv.Itoa(n))
	})
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// listMessages is GET /agent-api/v1/conversations/{id}/messages?limit=&cursor=
// (newest first). The cursor is the id of a message of this conversation.
func (s *Server) listMessages(w http.ResponseWriter, r *http.Request) error {
	p, ok := principal(r.Context())
	if !ok {
		return errNoPrincipal
	}
	id, err := conversationID(r)
	if err != nil {
		return err
	}
	q, err := query(r, "limit", "cursor")
	if err != nil {
		return err
	}
	limit, cursor, err := pageParams(q, maxChatPageLimit)
	if err != nil {
		return err
	}
	var entries []json.RawMessage
	var next *string
	err = s.d.Store.View(r.Context(), func(tx store.Tx) error {
		// an unknown or foreign conversation is 404 before any cursor check
		if _, err := tx.GetConversation(p.UserID, id); err != nil {
			return err
		}
		f := store.MessageFilter{Limit: limit + 1}
		if cursor != "" {
			k, err := tx.GetMessageKey(p.UserID, id, cursor)
			if errors.Is(err, store.ErrNotFound) {
				return errBadQuery
			}
			if err != nil {
				return err
			}
			f.Before = &k
		}
		list, err := tx.ListMessages(p.UserID, id, f)
		if err != nil {
			return err
		}
		// the states of the page's answering jobs, in one query
		var ids []string
		for _, m := range list[:min(len(list), limit)] {
			if m.Role == store.RoleUser && m.JobID != "" {
				ids = append(ids, m.JobID)
			}
		}
		jobs, err := tx.JobStatuses(p.UserID, ids)
		if err != nil {
			return err
		}
		entries, next, err = encodePage(list, limit, func(m store.Message) string { return m.ID },
			func(m store.Message) (any, error) {
				v := messageViewOf(m)
				if js, ok := jobs[m.JobID]; ok && m.Role == store.RoleUser {
					v.Job = &messageJobView{ID: m.JobID, State: string(js.State), Error: capText(js.Error, maxReasonView)}
				}
				return v, nil
			})
		return err
	})
	if err != nil {
		return err
	}
	writePage(w, "messages", entries, next)
	return nil
}

type messageBody struct {
	Content json.RawMessage `json:"content"`
}

// decodeTextBody is decodeBody for bodies that carry the user's text: it
// also refuses a body that is not valid UTF-8, which encoding/json would
// silently turn into U+FFFD. A \uD800-style escape of a lone surrogate
// still decodes to U+FFFD (known limit).
func decodeTextBody(w http.ResponseWriter, r *http.Request, dst any) error {
	if r.Body == nil {
		return errBadJSON
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, bodyLimit(r)))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return errTooLarge
		}
		return errBadJSON
	}
	if !utf8.Valid(data) {
		return errBadJSON
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	return decodeBody(w, r, dst)
}

// validMessage checks a user's message: 1..maxChatMessageBytes bytes of
// valid UTF-8 without control characters other than \n, \t and \r.
func validMessage(s string) bool {
	if s == "" || len(s) > maxChatMessageBytes || !utf8.ValidString(s) {
		return false
	}
	return strings.IndexFunc(s, func(r rune) bool {
		return unicode.IsControl(r) && r != '\n' && r != '\t' && r != '\r'
	}) < 0
}

// titleOf names an untitled conversation after its first message: the
// first titleFromMessage runes, control, separator and bidi characters
// replaced by spaces, trimmed.
func titleOf(content string) string {
	var b strings.Builder
	n := 0
	for _, r := range content {
		if n == titleFromMessage {
			break
		}
		if isControlRune(r) {
			r = ' '
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// sendMessage is POST /agent-api/v1/conversations/{id}/messages with
// {"content": "..."}. Checks in this order: the user is enrolled (409
// not_enrolled), the conversation is theirs (404), fewer than
// maxLiveChatJobs live chat jobs (429 too_many_pending), fewer than
// maxChatJobsPerDay chat jobs started this UTC day (429 limit_reached until
// the next UTC midnight) and today's token budget not used up (429
// budget_exhausted until the next UTC midnight). They run in the same
// transaction that inserts the job and the message, so concurrent sends
// never exceed the caps. Then, after the commit, the queue is woken once:
// 202. The per-user cooldown is taken before these checks, so a refused send
// uses it up too (the next try waits a second).
// A paused agent or a kill switch does not refuse the message (the job
// waits; the store does not claim it); the response's agent object says so.
func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request) error {
	var body messageBody
	if err := decodeTextBody(w, r, &body); err != nil {
		return err
	}
	content, err := jsonString(body.Content)
	if err != nil {
		return err
	}
	if !validMessage(content) {
		return errBadJSON
	}
	p, ok := principal(r.Context())
	if !ok {
		return errNoPrincipal
	}
	id, err := conversationID(r)
	if err != nil {
		return err
	}
	if ok, wait := s.messages.take(p.UserID); !ok {
		rateLimited(w, r, wait)
		return nil
	}
	now := s.d.Now()
	dayStart := startOfUTCDay(now)
	var v sentView
	err = s.d.Store.InTx(r.Context(), func(tx store.Tx) error {
		if err := enrolled(tx, p.UserID); err != nil {
			return err
		}
		conv, err := tx.GetConversation(p.UserID, id)
		if err != nil {
			return err
		}
		live, err := tx.CountJobsInStates(p.UserID, liveJobStates, store.TriggerChat)
		if err != nil {
			return err
		}
		if live >= maxLiveChatJobs {
			return &retryAfter{apiError: errTooManyPending, wait: pendingRetryAfter}
		}
		// fast-failing jobs never use up the token budget: cap the jobs too
		today, err := tx.CountJobsSince(p.UserID, store.TriggerChat, dayStart)
		if err != nil {
			return err
		}
		if today >= maxChatJobsPerDay {
			return &retryAfter{apiError: errDailyChatJobs, wait: dayStart.AddDate(0, 0, 1).Sub(now)}
		}
		pol, err := tx.GetPolicy(p.UserID)
		if err != nil {
			return err
		}
		in, out, err := tx.SumJobTokens(p.UserID, dayStart)
		if err != nil {
			return err
		}
		if in+out >= s.dailyBudget(pol.DailyTokens) {
			return &retryAfter{apiError: errBudgetExhausted, wait: dayStart.AddDate(0, 0, 1).Sub(now)}
		}
		kill := s.d.Cfg.KillSwitchEnv
		switch ks, err := tx.GetSystem(store.KillSwitchKey); {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return err
		default:
			kill = kill || ks == store.KillSwitchOnValue
		}
		job, err := tx.InsertJob(store.Job{UserID: p.UserID, TriggerKind: store.TriggerChat, TriggerRef: conv.ID, Goal: content})
		if err != nil {
			return err
		}
		msg, err := tx.InsertMessage(store.Message{
			ConversationID: conv.ID, UserID: p.UserID, Role: store.RoleUser, Content: content, JobID: job.ID,
		})
		if err != nil {
			return err
		}
		if conv.Title == "" {
			if t := titleOf(content); t != "" {
				if err := tx.RenameConversation(p.UserID, conv.ID, t); err != nil {
					return err
				}
			}
		}
		if _, err := tx.AppendAudit(audit.Event{
			UserID: p.UserID, TS: now, JobID: job.ID, Kind: "chat_message", Cred: "oidc", Outcome: "queued",
			Detail: "job=" + job.ID,
		}); err != nil {
			return err
		}
		mv := messageViewOf(msg)
		mv.Job = &messageJobView{ID: job.ID, State: string(job.State), Error: job.Error}
		v = sentView{
			Message: mv,
			Job:     sentJobView{ID: job.ID, State: string(job.State)},
			Agent:   agentView{Paused: pol.Paused, PauseReason: pol.PauseReason, KillSwitch: kill},
		}
		return nil
	})
	if err != nil {
		var ra *retryAfter
		if errors.As(err, &ra) {
			w.Header().Set("Retry-After", strconv.Itoa(retrySeconds(ra.wait)))
		}
		return err
	}
	s.d.Queue.Wake()
	writeJSON(w, http.StatusAccepted, v)
	return nil
}

// retrySeconds is a Retry-After value: whole seconds, rounded up, at least 1.
func retrySeconds(d time.Duration) int {
	return max(1, int(math.Ceil(d.Seconds())))
}

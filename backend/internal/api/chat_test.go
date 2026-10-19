package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/agent"
	"github.com/owncloud/ocis-agent/backend/internal/audit"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

const convPath = "/agent-api/v1/conversations"

// newChatStack is the stack with einstein and marie enrolled.
func newChatStack(t *testing.T) *stack {
	t.Helper()
	s := newStack(t)
	s.enroll(t, "einstein")
	s.enroll(t, "marie")
	return s
}

func strictDecode(t *testing.T, body string, dst any) {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
}

// newConv creates a conversation through the API.
func (s *stack) newConv(t *testing.T, login, body string) conversationView {
	t.Helper()
	rec := s.as(login, http.MethodPost, convPath, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create conversation = %d %s", rec.Code, rec.Body.String())
	}
	var v conversationView
	strictDecode(t, rec.Body.String(), &v)
	return v
}

func msgBody(content string) string {
	b, _ := json.Marshal(map[string]string{"content": content})
	return string(b)
}

// send posts a message and decodes the 202.
func (s *stack) send(t *testing.T, login, conv, content string) sentView {
	t.Helper()
	rec := s.as(login, http.MethodPost, convPath+"/"+conv+"/messages", msgBody(content))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("send = %d %s", rec.Code, rec.Body.String())
	}
	var v sentView
	strictDecode(t, rec.Body.String(), &v)
	return v
}

func (s *stack) getStoredJob(t *testing.T, user, id string) store.Job {
	t.Helper()
	var j store.Job
	if err := s.st.View(bg, func(tx store.Tx) error {
		var err error
		j, err = tx.GetJob(user, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return j
}

func (s *stack) liveChatJobs(t *testing.T, user string) int {
	t.Helper()
	var n int
	if err := s.st.View(bg, func(tx store.Tx) error {
		var err error
		n, err = tx.CountJobsInStates(user, []store.JobState{store.JobQueued, store.JobRunning, store.JobWaitingApproval}, store.TriggerChat)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestConversationCRUD(t *testing.T) {
	s := newChatStack(t)
	c := s.newConv(t, "einstein", "{}")
	if !validID(c.ID) || c.Title != "" || c.CreatedAt == nil || c.UpdatedAt == nil {
		t.Errorf("created = %+v", c)
	}
	named := s.newConv(t, "einstein", `{"title":"Physics"}`)
	if named.Title != "Physics" {
		t.Errorf("title = %q", named.Title)
	}
	// an empty body is the empty object
	if rec := s.as("einstein", http.MethodPost, convPath, ""); rec.Code != http.StatusCreated {
		t.Errorf("empty body = %d %s", rec.Code, rec.Body.String())
	}

	rec := s.as("einstein", http.MethodGet, convPath+"/"+named.ID, "")
	var got conversationView
	strictDecode(t, rec.Body.String(), &got)
	if rec.Code != 200 || got.ID != named.ID || got.Title != "Physics" {
		t.Errorf("get = %d %+v", rec.Code, got)
	}

	s.fake.Advance(time.Second)
	rec = s.as("einstein", http.MethodPut, convPath+"/"+named.ID, `{"title":"Relativity"}`)
	strictDecode(t, rec.Body.String(), &got)
	if rec.Code != 200 || got.Title != "Relativity" || *got.UpdatedAt == *named.UpdatedAt {
		t.Errorf("rename = %d %+v", rec.Code, got)
	}
	for _, body := range []string{`{}`, `{"title":null}`, `{"title":1}`, `{"Title":"x"}`, `{"title":"x","y":1}`} {
		if rec := s.as("einstein", http.MethodPut, convPath+"/"+named.ID, body); rec.Code != 400 {
			t.Errorf("rename %s = %d", body, rec.Code)
		}
	}

	rec = s.as("einstein", http.MethodGet, convPath, "")
	var page struct {
		Conversations []conversationView `json:"conversations"`
		NextCursor    *string            `json:"next_cursor"`
	}
	strictDecode(t, rec.Body.String(), &page)
	if rec.Code != 200 || len(page.Conversations) != 3 || page.Conversations[0].ID != named.ID || page.NextCursor != nil {
		t.Errorf("list = %d %s", rec.Code, rec.Body.String())
	}

	if rec := s.as("einstein", http.MethodDelete, convPath+"/"+named.ID, ""); rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Errorf("delete = %d %q", rec.Code, rec.Body.String())
	}
	if rec := s.as("einstein", http.MethodGet, convPath+"/"+named.ID, ""); rec.Code != 404 {
		t.Errorf("get after delete = %d", rec.Code)
	}
	if rec := s.as("einstein", http.MethodDelete, convPath+"/"+named.ID, ""); rec.Code != 404 {
		t.Errorf("second delete = %d", rec.Code)
	}
}

func TestConversationListPages(t *testing.T) {
	s := newChatStack(t)
	var ids []string
	for i := range 5 {
		ids = append(ids, s.newConv(t, "einstein", fmt.Sprintf(`{"title":"c%d"}`, i)).ID)
		s.fake.Advance(time.Millisecond)
	}
	var seen []string
	cursor := ""
	for range 10 {
		target := convPath + "?limit=2"
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		rec := s.as("einstein", http.MethodGet, target, "")
		var page struct {
			Conversations []conversationView `json:"conversations"`
			NextCursor    *string            `json:"next_cursor"`
		}
		strictDecode(t, rec.Body.String(), &page)
		for _, c := range page.Conversations {
			seen = append(seen, c.ID)
		}
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
	}
	want := []string{ids[4], ids[3], ids[2], ids[1], ids[0]}
	if fmt.Sprint(seen) != fmt.Sprint(want) {
		t.Errorf("walk = %v, want %v", seen, want)
	}
	for _, q := range []string{"?limit=0", "?limit=101", "?cursor=x", "?foo=1", "?limit=2&limit=3"} {
		if rec := s.as("einstein", http.MethodGet, convPath+q, ""); rec.Code != 400 {
			t.Errorf("%s = %d", q, rec.Code)
		}
	}
	if rec := s.as("einstein", http.MethodGet, convPath+"?limit=100", ""); rec.Code != 200 {
		t.Errorf("limit 100 = %d", rec.Code)
	}
}

func TestConversationTitleValidation(t *testing.T) {
	s := newChatStack(t)
	c := s.newConv(t, "einstein", "{}")
	for name, title := range map[string]string{
		"101 runes": strings.Repeat("é", 101),
		"control":   "a\x07b",
		"newline":   "a\nb",
		"bidi":      "a\u202eb",
		"rlm":       "a\u200fb",
		"alm":       "a\u061cb",
		"separator": "a\u2028b",
	} {
		body := msgBodyField("title", title)
		if rec := s.as("einstein", http.MethodPost, convPath, body); rec.Code != 400 {
			t.Errorf("create %s = %d", name, rec.Code)
		}
		if rec := s.as("einstein", http.MethodPut, convPath+"/"+c.ID, body); rec.Code != 400 {
			t.Errorf("rename %s = %d", name, rec.Code)
		}
	}
	if v := s.newConv(t, "einstein", msgBodyField("title", strings.Repeat("é", 100))); v.Title != strings.Repeat("é", 100) {
		t.Errorf("100 runes = %q", v.Title)
	}
}

func msgBodyField(k, v string) string {
	b, _ := json.Marshal(map[string]string{k: v})
	return string(b)
}

func TestConversationCap(t *testing.T) {
	s := newChatStack(t)
	s.mustTx(t, func(tx store.Tx) error {
		for range store.MaxConversationsPerUser {
			if _, err := tx.InsertConversation(store.Conversation{UserID: einsteinID}); err != nil {
				return err
			}
		}
		return nil
	})
	wantError(t, s.as("einstein", http.MethodPost, convPath, "{}"), http.StatusConflict, "limit_reached")
	// another user is not affected
	s.newConv(t, "marie", "{}")
}

func TestConversationsNotEnrolled(t *testing.T) {
	s := newStack(t)
	wantError(t, s.as("einstein", http.MethodPost, convPath, "{}"), http.StatusConflict, "not_enrolled")
	rec := s.as("einstein", http.MethodGet, convPath, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"conversations":[]`) {
		t.Errorf("list = %d %s", rec.Code, rec.Body.String())
	}
}

func TestChatScopedToTheUser(t *testing.T) {
	s := newChatStack(t)
	mine := s.newConv(t, "marie", `{"title":"radium"}`)
	sent := s.send(t, "marie", mine.ID, "hello")
	own := s.newConv(t, "einstein", "{}")
	s.send(t, "einstein", own.ID, "mine")
	base := convPath + "/" + mine.ID
	for _, c := range []struct{ method, target, body string }{
		{http.MethodGet, base, ""},
		{http.MethodPut, base, `{"title":"stolen"}`},
		{http.MethodDelete, base, ""},
		{http.MethodGet, base + "/messages", ""},
		{http.MethodPost, base + "/messages", msgBody("hi")},
	} {
		wantError(t, s.as("einstein", c.method, c.target, c.body), http.StatusNotFound, "not_found")
	}
	// foreign cursors: marie's conversation and message ids
	if rec := s.as("einstein", http.MethodGet, convPath+"?cursor="+mine.ID, ""); rec.Code != 400 {
		t.Errorf("foreign conversation cursor = %d", rec.Code)
	}
	if rec := s.as("einstein", http.MethodGet, convPath+"/"+own.ID+"/messages?cursor="+sent.Message.ID, ""); rec.Code != 400 {
		t.Errorf("foreign message cursor = %d", rec.Code)
	}
	// a message cursor of another conversation of the same user is also refused
	other := s.newConv(t, "einstein", "{}")
	om := s.send(t, "einstein", other.ID, "elsewhere")
	if rec := s.as("einstein", http.MethodGet, convPath+"/"+own.ID+"/messages?cursor="+om.Message.ID, ""); rec.Code != 400 {
		t.Errorf("other conversation's cursor = %d", rec.Code)
	}
	// nothing of marie's changed
	rec := s.as("marie", http.MethodGet, base, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"radium"`) {
		t.Errorf("marie's = %d %s", rec.Code, rec.Body.String())
	}
	rec = s.as("einstein", http.MethodGet, convPath, "")
	if strings.Contains(rec.Body.String(), mine.ID) {
		t.Error("einstein lists marie's conversation")
	}
	if n := s.liveChatJobs(t, marieID); n != 1 {
		t.Errorf("marie's live jobs = %d", n)
	}
	// bad ids are 404
	for _, id := range []string{"x", strings.ToLower(mine.ID)} {
		if rec := s.as("einstein", http.MethodGet, convPath+"/"+id, ""); rec.Code != 404 {
			t.Errorf("%s = %d", id, rec.Code)
		}
	}
}

func TestSendMessage(t *testing.T) {
	s := newChatStack(t)
	c := s.newConv(t, "einstein", "{}")
	content := "  Explain\tthe\r\nphotoelectric effect, please; it is a long question that has more than sixty characters in it"
	before := s.wk.n
	v := s.send(t, "einstein", c.ID, content)
	if s.wk.n != before+1 {
		t.Errorf("wake called %d times", s.wk.n-before)
	}
	if v.Message.Role != store.RoleUser || v.Message.Content != content || v.Message.JobID != v.Job.ID || !validID(v.Message.ID) || v.Message.CreatedAt == nil {
		t.Errorf("message = %+v", v.Message)
	}
	if v.Job.State != string(store.JobQueued) || !validID(v.Job.ID) {
		t.Errorf("job = %+v", v.Job)
	}
	if v.Agent.Paused || v.Agent.PauseReason != "" || v.Agent.KillSwitch {
		t.Errorf("agent = %+v", v.Agent)
	}
	j := s.getStoredJob(t, einsteinID, v.Job.ID)
	if j.Goal != content || j.TriggerKind != store.TriggerChat || j.TriggerRef != c.ID || j.State != store.JobQueued {
		t.Errorf("stored job = %+v", j)
	}
	// the title was filled from the first message: 60 runes, controls as spaces, trimmed
	rec := s.as("einstein", http.MethodGet, convPath+"/"+c.ID, "")
	var got conversationView
	strictDecode(t, rec.Body.String(), &got)
	wantTitle := strings.TrimSpace(strings.NewReplacer("\t", " ", "\r", " ", "\n", " ").Replace(string([]rune(content)[:60])))
	if got.Title != wantTitle {
		t.Errorf("title = %q, want %q", got.Title, wantTitle)
	}
	// a second message never changes the title, the goal is only its own content
	s.fake.Advance(2 * time.Second)
	v2 := s.send(t, "einstein", c.ID, "second")
	if j := s.getStoredJob(t, einsteinID, v2.Job.ID); j.Goal != "second" {
		t.Errorf("second goal = %q", j.Goal)
	}
	rec = s.as("einstein", http.MethodGet, convPath+"/"+c.ID, "")
	strictDecode(t, rec.Body.String(), &got)
	if got.Title != wantTitle {
		t.Errorf("title changed to %q", got.Title)
	}
	// a named conversation keeps its title
	named := s.newConv(t, "einstein", `{"title":"Named"}`)
	s.send(t, "einstein", named.ID, "first")
	rec = s.as("einstein", http.MethodGet, convPath+"/"+named.ID, "")
	strictDecode(t, rec.Body.String(), &got)
	if got.Title != "Named" {
		t.Errorf("named title = %q", got.Title)
	}

	// the stored message row carries the job id
	var msgs []store.Message
	s.mustTx(t, func(tx store.Tx) error {
		var err error
		msgs, err = tx.ListMessages(einsteinID, c.ID, store.MessageFilter{})
		return err
	})
	if len(msgs) != 2 || msgs[1].ID != v.Message.ID || msgs[1].JobID != v.Job.ID || msgs[1].Content != content {
		t.Errorf("stored messages = %+v", msgs)
	}
	// audit: one chat_message per send with the job id only, never the text
	var chats []audit.Record
	for _, r := range s.auditOf(t, einsteinID) {
		if strings.Contains(r.Detail, "photoelectric") || strings.Contains(r.Detail, "second") || strings.Contains(r.Detail, "first") {
			t.Errorf("audit carries text: %+v", r)
		}
		if r.Kind == "chat_message" {
			chats = append(chats, r)
		}
	}
	if len(chats) != 3 || chats[0].Detail != "job="+v.Job.ID || chats[0].JobID != v.Job.ID {
		t.Errorf("chat audit = %+v", chats)
	}
}

func TestSendMessageGoalTitleFillEdgeCases(t *testing.T) {
	s := newChatStack(t)
	c := s.newConv(t, "einstein", "{}")
	content := "a\u202eb" + strings.Repeat("x", 10) // bidi controls in content are allowed but never reach the title
	s.send(t, "einstein", c.ID, content)
	rec := s.as("einstein", http.MethodGet, convPath+"/"+c.ID, "")
	var got conversationView
	strictDecode(t, rec.Body.String(), &got)
	if got.Title != "a b"+strings.Repeat("x", 10) {
		t.Errorf("title = %q", got.Title)
	}
}

func TestSendMessageLimits(t *testing.T) {
	s := newChatStack(t)
	c := s.newConv(t, "einstein", "{}")
	target := convPath + "/" + c.ID + "/messages"
	for name, body := range map[string]string{
		"empty":        msgBody(""),
		"too long":     msgBody(strings.Repeat("a", maxChatMessageBytes+1)),
		"nul":          msgBody("a\x00b"),
		"bell":         msgBody("a\x07b"),
		"del":          msgBody("a\x7fb"),
		"c1":           msgBody("a\u0085b"),
		"invalid utf8": "{\"content\":\"a\xffb\"}",
		"no content":   `{}`,
		"null":         `{"content":null}`,
		"number":       `{"content":1}`,
		"unknown":      `{"content":"a","x":1}`,
		"case":         `{"Content":"a"}`,
		"no body":      ``,
	} {
		if rec := s.as("einstein", http.MethodPost, target, body); rec.Code != 400 {
			t.Errorf("%s = %d %s", name, rec.Code, rec.Body.String())
		}
	}
	// 33 KiB: the route's body limit
	rec := s.as("einstein", http.MethodPost, target, msgBody(strings.Repeat("a", 33<<10)))
	wantError(t, rec, http.StatusRequestEntityTooLarge, "too_large")
	// without a Content-Length the reader stops at the limit
	r := s.req(http.MethodPost, target, msgBody(strings.Repeat("a", 33<<10)))
	r.ContentLength = -1
	wantError(t, s.serve(r), http.StatusRequestEntityTooLarge, "too_large")
	// exactly 8192 bytes (multi-byte runes too) and allowed whitespace pass
	s.send(t, "einstein", c.ID, strings.Repeat("a", maxChatMessageBytes))
	s.send(t, "einstein", c.ID, strings.Repeat("é", maxChatMessageBytes/2))
	s.send(t, "einstein", c.ID, "a\n\tb\r\n")
	if n := s.liveChatJobs(t, einsteinID); n != 3 {
		t.Errorf("live jobs = %d", n)
	}
}

func TestSendMessageNotEnrolled(t *testing.T) {
	s := newStack(t)
	// a conversation id that does not exist: not enrolled is checked first
	wantError(t, s.as("einstein", http.MethodPost, convPath+"/"+store.NewID()+"/messages", msgBody("hi")), http.StatusConflict, "not_enrolled")
}

func TestSendMessageUnknownConversation(t *testing.T) {
	s := newChatStack(t)
	wantError(t, s.as("einstein", http.MethodPost, convPath+"/"+store.NewID()+"/messages", msgBody("hi")), http.StatusNotFound, "not_found")
	if s.wk.n != 0 {
		t.Error("woken for a refused message")
	}
}

func TestSendMessageLiveJobCap(t *testing.T) {
	s := newChatStack(t)
	c := s.newConv(t, "einstein", "{}")
	var jobs []string
	for i := range maxLiveChatJobs {
		jobs = append(jobs, s.send(t, "einstein", c.ID, fmt.Sprint("q", i)).Job.ID)
	}
	wakes := s.wk.n
	auditN := len(s.auditOf(t, einsteinID))
	rec := s.as("einstein", http.MethodPost, convPath+"/"+c.ID+"/messages", msgBody("sixth"))
	wantError(t, rec, http.StatusTooManyRequests, "too_many_pending")
	if ra, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || ra < 1 {
		t.Errorf("Retry-After = %q", rec.Header().Get("Retry-After"))
	}
	if s.wk.n != wakes || len(s.auditOf(t, einsteinID)) != auditN {
		t.Error("a refused message woke the queue or was audited")
	}
	// a non-chat job does not count, another conversation counts too
	s.queuedJob(t, einsteinID, "sweep", "proactive goal")
	other := s.newConv(t, "einstein", "{}")
	wantError(t, s.as("einstein", http.MethodPost, convPath+"/"+other.ID+"/messages", msgBody("x")), http.StatusTooManyRequests, "too_many_pending")
	// another user is not affected
	mc := s.newConv(t, "marie", "{}")
	s.send(t, "marie", mc.ID, "hi")
	// after one job ends the next message works
	s.mustTx(t, func(tx store.Tx) error { return tx.CancelJob(einsteinID, jobs[0]) })
	s.send(t, "einstein", c.ID, "sixth")
	var msgs []store.Message
	s.mustTx(t, func(tx store.Tx) error {
		var err error
		msgs, err = tx.ListMessages(einsteinID, c.ID, store.MessageFilter{})
		return err
	})
	if len(msgs) != maxLiveChatJobs+1 {
		t.Errorf("messages = %d (a refused one was stored?)", len(msgs))
	}
}

func TestSendMessageLiveJobCapUnderConcurrency(t *testing.T) {
	s := newChatStack(t)
	c := s.newConv(t, "einstein", "{}")
	var wg sync.WaitGroup
	codes := make([]int, 20)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = s.as("einstein", http.MethodPost, convPath+"/"+c.ID+"/messages", msgBody(fmt.Sprint("m", i))).Code
		}()
	}
	wg.Wait()
	ok, limited := 0, 0
	for _, code := range codes {
		switch code {
		case http.StatusAccepted:
			ok++
		case http.StatusTooManyRequests:
			limited++
		default:
			t.Errorf("code %d", code)
		}
	}
	if ok != maxLiveChatJobs || limited != 20-maxLiveChatJobs {
		t.Errorf("accepted %d, limited %d", ok, limited)
	}
	if n := s.liveChatJobs(t, einsteinID); n != maxLiveChatJobs {
		t.Errorf("live jobs = %d", n)
	}
}

func TestSendMessageBudgetExhausted(t *testing.T) {
	s := newChatStack(t)
	// 2026-10-14 12:00 UTC at the fake; the user's budget is lowered to 1000
	s.mustTx(t, func(tx store.Tx) error {
		pol, err := tx.GetPolicy(einsteinID)
		if err != nil {
			return err
		}
		pol.DailyTokens = 1000
		return tx.PutPolicy(pol)
	})
	s.appendStep(t, einsteinID, 600, 399)
	c := s.newConv(t, "einstein", "{}")
	s.send(t, "einstein", c.ID, "still fits") // 999 < 1000
	s.appendStepFor(t, einsteinID, 1)
	rec := s.as("einstein", http.MethodPost, convPath+"/"+c.ID+"/messages", msgBody("over"))
	wantError(t, rec, http.StatusTooManyRequests, "budget_exhausted")
	now := s.fake.Now().UTC()
	midnight := startOfUTCDay(now).AddDate(0, 0, 1)
	want := int(midnight.Sub(now).Seconds())
	if ra, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || ra < want || ra > want+1 {
		t.Errorf("Retry-After = %q, want ~%d", rec.Header().Get("Retry-After"), want)
	}
	// marie is not affected
	mc := s.newConv(t, "marie", "{}")
	s.send(t, "marie", mc.ID, "hi")
	// the next UTC day the budget is fresh
	s.fake.Advance(midnight.Sub(now) + time.Second)
	s.send(t, "einstein", c.ID, "next day")
}

// appendStepFor adds a step with in tokens to a running chat job of the
// user (the oldest queued one is claimed) and leaves the job running.
func (s *stack) appendStepFor(t *testing.T, user string, in int64) {
	t.Helper()
	s.mustTx(t, func(tx store.Tx) error {
		c, ok, err := tx.ClaimNextJob("w", time.Hour)
		if err != nil {
			return err
		}
		if !ok || c.Job.UserID != user {
			return fmt.Errorf("claimed %v %+v", ok, c.Job)
		}
		_, err = tx.AppendStep(store.JobStep{JobID: c.Job.ID, Kind: "llm", Status: "ok", TokensIn: in}, c.Fence)
		return err
	})
}

func TestSendMessagePausedOrKilledIsAccepted(t *testing.T) {
	s := newChatStack(t)
	c := s.newConv(t, "einstein", "{}")
	if rec := s.as("einstein", http.MethodPost, "/agent-api/v1/pause", ""); rec.Code != 200 {
		t.Fatalf("pause = %d", rec.Code)
	}
	v := s.send(t, "einstein", c.ID, "while paused")
	if !v.Agent.Paused || v.Agent.PauseReason != store.PauseUser || v.Agent.KillSwitch || v.Job.State != string(store.JobQueued) {
		t.Errorf("paused = %+v", v)
	}
	s.mustTx(t, func(tx store.Tx) error { return tx.SetSystem(store.KillSwitchKey, store.KillSwitchOnValue) })
	s.fake.Advance(2 * time.Second)
	v = s.send(t, "einstein", c.ID, "while killed")
	if !v.Agent.KillSwitch {
		t.Errorf("killed = %+v", v.Agent)
	}
	// the environment's kill switch counts too
	e := newStack(t, func(d *Deps) { d.Cfg.KillSwitchEnv = true })
	e.enroll(t, "einstein")
	ec := e.newConv(t, "einstein", "{}")
	if v := e.send(t, "einstein", ec.ID, "env"); !v.Agent.KillSwitch || v.Agent.Paused {
		t.Errorf("env kill switch = %+v", v.Agent)
	}
	// the raw document has exactly these keys
	rec := s.as("einstein", http.MethodPost, convPath+"/"+c.ID+"/messages", msgBody("keys"))
	m := decode(t, rec)
	agent, _ := m["agent"].(map[string]any)
	job, _ := m["job"].(map[string]any)
	if len(m) != 3 || len(agent) != 3 || len(job) != 2 {
		t.Errorf("document = %s", rec.Body.String())
	}
}

func TestSendMessageCooldown(t *testing.T) {
	s := newChatStack(t)
	s.srv.messages.every = messageCooldown
	c := s.newConv(t, "einstein", "{}")
	s.send(t, "einstein", c.ID, "one")
	rec := s.as("einstein", http.MethodPost, convPath+"/"+c.ID+"/messages", msgBody("two"))
	wantError(t, rec, http.StatusTooManyRequests, "rate_limited")
	if rec.Header().Get("Retry-After") != "1" {
		t.Errorf("Retry-After = %q", rec.Header().Get("Retry-After"))
	}
	// other users and the token actions have their own cooldowns
	mc := s.newConv(t, "marie", "{}")
	s.send(t, "marie", mc.ID, "hi")
	if ok, _ := s.srv.actions.take(einsteinID); !ok {
		t.Error("the message cooldown blocks the token actions")
	}
	s.fake.Advance(time.Second)
	s.send(t, "einstein", c.ID, "two")
	if n := s.liveChatJobs(t, einsteinID); n != 2 {
		t.Errorf("live jobs = %d", n)
	}
}

func TestDeleteConversationCancelsItsJobs(t *testing.T) {
	s := newChatStack(t)
	c := s.newConv(t, "einstein", "{}")
	keep := s.newConv(t, "einstein", "{}")
	var ids []string
	for i := range 3 {
		ids = append(ids, s.send(t, "einstein", c.ID, fmt.Sprint("q", i)).Job.ID)
	}
	other := s.send(t, "einstein", keep.ID, "other conversation").Job.ID
	// one already ended, one is running
	s.mustTx(t, func(tx store.Tx) error { return tx.CancelJob(einsteinID, ids[0]) })
	s.appendStepFor(t, einsteinID, 1) // claims ids[1]
	seqBefore := len(s.auditOf(t, einsteinID))
	if rec := s.as("einstein", http.MethodDelete, convPath+"/"+c.ID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	for _, id := range ids {
		if st := s.getStoredJob(t, einsteinID, id).State; st != store.JobCancelled {
			t.Errorf("job %s = %s", id, st)
		}
	}
	if st := s.getStoredJob(t, einsteinID, other).State; st != store.JobQueued {
		t.Errorf("other conversation's job = %s", st)
	}
	recs := s.auditOf(t, einsteinID)
	if len(recs) != seqBefore+1 {
		t.Fatalf("audit grew by %d", len(recs)-seqBefore)
	}
	last := recs[len(recs)-1]
	if last.Kind != "conversation_deleted" || last.Detail != "jobs_cancelled=2" || last.Cred != "oidc" {
		t.Errorf("audit = %+v", last)
	}
	// the conversation's messages are gone
	if rec := s.as("einstein", http.MethodGet, convPath+"/"+c.ID+"/messages", ""); rec.Code != 404 {
		t.Errorf("messages after delete = %d", rec.Code)
	}
}

func TestListMessages(t *testing.T) {
	s := newChatStack(t)
	c := s.newConv(t, "einstein", "{}")
	var sent []sentView
	for i := range 5 {
		sent = append(sent, s.send(t, "einstein", c.ID, fmt.Sprint("m", i)))
		s.fake.Advance(time.Millisecond)
	}
	big := strings.Repeat("\x01", store.MaxAnswerMessageBytes) // escapes to 6 bytes each
	s.mustTx(t, func(tx store.Tx) error {
		if _, err := tx.InsertMessage(store.Message{UserID: einsteinID, ConversationID: c.ID, Role: store.RoleAssistant, Content: "answer", JobID: sent[4].Job.ID}); err != nil {
			return err
		}
		_, err := tx.InsertMessage(store.Message{UserID: einsteinID, ConversationID: c.ID, Role: store.RoleSystem, Content: big})
		return err
	})
	rec := s.as("einstein", http.MethodGet, convPath+"/"+c.ID+"/messages", "")
	var page struct {
		Messages   []messageView `json:"messages"`
		NextCursor *string       `json:"next_cursor"`
	}
	strictDecode(t, rec.Body.String(), &page)
	if rec.Code != 200 || len(page.Messages) != 7 || page.NextCursor != nil {
		t.Fatalf("list = %d %d %v", rec.Code, len(page.Messages), page.NextCursor)
	}
	if m := page.Messages[0]; m.Role != store.RoleSystem || len(m.Content) > maxAnswerView || m.JobID != "" {
		t.Errorf("newest = %s %d", m.Role, len(m.Content))
	}
	if m := page.Messages[1]; m.Role != store.RoleAssistant || m.Content != "answer" || m.JobID != sent[4].Job.ID {
		t.Errorf("answer = %+v", m)
	}
	if m := page.Messages[6]; m.Content != "m0" || m.ID != sent[0].Message.ID || m.JobID != sent[0].Job.ID {
		t.Errorf("oldest = %+v", m)
	}
	// keyset walk with limit 2
	var walked []string
	cursor := ""
	for range 10 {
		target := convPath + "/" + c.ID + "/messages?limit=2"
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		rec := s.as("einstein", http.MethodGet, target, "")
		var p struct {
			Messages   []messageView `json:"messages"`
			NextCursor *string       `json:"next_cursor"`
		}
		strictDecode(t, rec.Body.String(), &p)
		for _, m := range p.Messages {
			walked = append(walked, m.ID)
		}
		if p.NextCursor == nil {
			break
		}
		cursor = *p.NextCursor
	}
	if len(walked) != 7 || walked[6] != sent[0].Message.ID {
		t.Errorf("walk = %v", walked)
	}
	for _, q := range []string{"?limit=0", "?limit=101", "?cursor=bad", "?x=1"} {
		if rec := s.as("einstein", http.MethodGet, convPath+"/"+c.ID+"/messages"+q, ""); rec.Code != 400 {
			t.Errorf("%s = %d", q, rec.Code)
		}
	}
	if rec := s.as("einstein", http.MethodGet, convPath+"/"+c.ID+"/messages?cursor="+store.NewID(), ""); rec.Code != 400 {
		t.Errorf("unknown cursor = %d", rec.Code)
	}
}

func TestMessageViewCapsUntrustedContent(t *testing.T) {
	long := strings.Repeat("é", maxAnswerView) // 128 KiB
	for _, role := range []string{store.RoleAssistant, store.RoleSystem, store.RoleUser} {
		v := messageViewOf(store.Message{Role: role, Content: long})
		if len(v.Content) > maxAnswerView || !strings.HasSuffix(v.Content, truncMarker) {
			t.Errorf("%s: %d bytes", role, len(v.Content))
		}
	}
}

func TestChatLogsNeverCarryContent(t *testing.T) {
	s := newChatStack(t)
	const marker = "SECRET-CHAT-CONTENT-7f3a"
	c := s.newConv(t, "einstein", msgBodyField("title", "TITLE-"+marker))
	s.send(t, "einstein", c.ID, marker)
	s.as("einstein", http.MethodPost, convPath+"/"+c.ID+"/messages", msgBody(marker+"\x07")) // refused
	s.as("einstein", http.MethodGet, convPath+"/"+c.ID+"/messages", "")
	s.as("einstein", http.MethodPut, convPath+"/"+c.ID, msgBodyField("title", marker+"2"))
	s.as("einstein", http.MethodDelete, convPath+"/"+c.ID, "")
	if strings.Contains(s.logs.String(), marker) {
		t.Errorf("logs carry message content: %s", s.logs.String())
	}
	for _, r := range s.auditOf(t, einsteinID) {
		if strings.Contains(r.Detail, marker) {
			t.Errorf("audit carries content: %+v", r)
		}
	}
}

// claimAndRelease claims the user's oldest runnable job (it must be id)
// with ttl and, unless next is running, releases it to next with msg.
func (s *stack) claimAndRelease(t *testing.T, user, id string, ttl time.Duration, next store.JobState, msg string) {
	t.Helper()
	s.mustTx(t, func(tx store.Tx) error {
		c, ok, err := tx.ClaimNextJob("w", ttl)
		if err != nil {
			return err
		}
		if !ok || c.Job.ID != id || c.Job.UserID != user {
			return fmt.Errorf("claimed %v %+v, want %s", ok, c.Job, id)
		}
		if next == store.JobRunning {
			return nil
		}
		_, err = tx.ReleaseJob(c, next, nil, msg)
		return err
	})
}

func TestMessagesCarryTheStateOfTheirJobs(t *testing.T) {
	s := newChatStack(t)
	c := s.newConv(t, "einstein", "{}")
	send := func(content string) string { return s.send(t, "einstein", c.ID, content).Job.ID }
	succeeded := send("succeeds")
	s.claimAndRelease(t, einsteinID, succeeded, time.Hour, store.JobSucceeded, "")
	failed := send("fails")
	s.claimAndRelease(t, einsteinID, failed, time.Hour, store.JobFailed, agent.ReasonModelBad)
	// the reaper fails a job whose lease ran out at its last attempt: no
	// runner writes a system message for it
	reaped := send("reaped")
	s.claimAndRelease(t, einsteinID, reaped, time.Minute, store.JobRunning, "")
	s.fake.Advance(2 * time.Minute)
	s.mustTx(t, func(tx store.Tx) error {
		r, err := tx.ReapExpired(1, func(int) time.Duration { return time.Minute })
		if err == nil && r.Failed != 1 {
			err = fmt.Errorf("reap = %+v", r)
		}
		return err
	})
	cancelled := send("cancelled")
	s.mustTx(t, func(tx store.Tx) error { return tx.CancelJob(einsteinID, cancelled) })
	waiting := send("waits")
	s.claimAndRelease(t, einsteinID, waiting, time.Hour, store.JobWaitingApproval, "")
	running := send("runs")
	s.claimAndRelease(t, einsteinID, running, time.Hour, store.JobRunning, "")
	queued := send("queued")
	s.mustTx(t, func(tx store.Tx) error {
		_, err := tx.InsertMessage(store.Message{UserID: einsteinID, ConversationID: c.ID, Role: store.RoleAssistant, Content: "answer", JobID: succeeded})
		return err
	})
	want := map[string]messageJobView{
		succeeded: {ID: succeeded, State: "succeeded"},
		failed:    {ID: failed, State: "failed", Error: agent.ReasonModelBad},
		reaped:    {ID: reaped, State: "failed", Error: s.getStoredJob(t, einsteinID, reaped).Error},
		cancelled: {ID: cancelled, State: "cancelled"},
		waiting:   {ID: waiting, State: "waiting_approval"},
		running:   {ID: running, State: "running"},
		queued:    {ID: queued, State: "queued"},
	}
	if want[reaped].Error == "" {
		t.Fatal("the reaper left no error")
	}
	check := func(msgs []messageView) {
		t.Helper()
		for _, m := range msgs {
			switch {
			case m.Role != store.RoleUser:
				if m.Job != nil {
					t.Errorf("%s message carries a job: %+v", m.Role, m.Job)
				}
			case m.Job == nil:
				t.Errorf("user message %q has no job", m.Content)
			case *m.Job != want[m.JobID]:
				t.Errorf("user message %q: job %+v, want %+v", m.Content, *m.Job, want[m.JobID])
			}
		}
	}
	rec := s.as("einstein", http.MethodGet, convPath+"/"+c.ID+"/messages", "")
	var page struct {
		Messages   []messageView `json:"messages"`
		NextCursor *string       `json:"next_cursor"`
	}
	strictDecode(t, rec.Body.String(), &page)
	if len(page.Messages) != 8 {
		t.Fatalf("messages = %d", len(page.Messages))
	}
	check(page.Messages)
	// small pages join their own jobs
	var all []messageView
	cursor := ""
	for range 10 {
		target := convPath + "/" + c.ID + "/messages?limit=3"
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		rec := s.as("einstein", http.MethodGet, target, "")
		var p struct {
			Messages   []messageView `json:"messages"`
			NextCursor *string       `json:"next_cursor"`
		}
		strictDecode(t, rec.Body.String(), &p)
		all = append(all, p.Messages...)
		if p.NextCursor == nil {
			break
		}
		cursor = *p.NextCursor
	}
	if len(all) != 8 {
		t.Fatalf("walk = %d", len(all))
	}
	check(all)
	// the POST response carries the new job too
	v := s.send(t, "einstein", c.ID, "one more")
	if v.Message.Job == nil || *v.Message.Job != (messageJobView{ID: v.Job.ID, State: "queued"}) {
		t.Errorf("sent message job = %+v", v.Message.Job)
	}
	// a user message without a job has none
	s.mustTx(t, func(tx store.Tx) error {
		_, err := tx.InsertMessage(store.Message{UserID: einsteinID, ConversationID: c.ID, Role: store.RoleUser, Content: "jobless"})
		return err
	})
	rec = s.as("einstein", http.MethodGet, convPath+"/"+c.ID+"/messages?limit=1", "")
	m := decode(t, rec)
	first := m["messages"].([]any)[0].(map[string]any)
	if j, ok := first["job"]; !ok || j != nil || first["content"] != "jobless" {
		t.Errorf("jobless = %v", first)
	}
}

// statusSpy counts the store's job status lookups of the message list.
type statusSpy struct {
	store.Store
	mu    sync.Mutex
	calls int
	ids   int
}

type statusSpyTx struct {
	store.Tx
	s *statusSpy
}

func (s *statusSpy) View(ctx context.Context, fn func(tx store.Tx) error) error {
	return s.Store.View(ctx, func(tx store.Tx) error { return fn(statusSpyTx{tx, s}) })
}

func (t statusSpyTx) JobStatuses(user string, ids []string) (map[string]store.JobStatus, error) {
	t.s.mu.Lock()
	t.s.calls++
	t.s.ids = len(ids)
	t.s.mu.Unlock()
	return t.Tx.JobStatuses(user, ids)
}

func (t statusSpyTx) GetJobStatus(user, id string) (store.JobStatus, error) {
	t.s.mu.Lock()
	t.s.calls += 1000 // never used per message
	t.s.mu.Unlock()
	return t.Tx.GetJobStatus(user, id)
}

func TestMessagesJoinTheirJobsInOneQuery(t *testing.T) {
	spy := &statusSpy{}
	s := newStackStore(t, func(st store.Store) store.Store { spy.Store = st; return spy })
	s.enroll(t, "einstein")
	c := s.newConv(t, "einstein", "{}")
	for i := range 4 {
		id := s.send(t, "einstein", c.ID, fmt.Sprint("m", i)).Job.ID
		s.mustTx(t, func(tx store.Tx) error { return tx.CancelJob(einsteinID, id) })
	}
	spy.mu.Lock()
	spy.calls = 0
	spy.mu.Unlock()
	rec := s.as("einstein", http.MethodGet, convPath+"/"+c.ID+"/messages?limit=3", "")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	// limit+1 rows are fetched, only the page's 3 are joined
	if spy.calls != 1 || spy.ids != 3 {
		t.Errorf("calls = %d, ids = %d", spy.calls, spy.ids)
	}
}

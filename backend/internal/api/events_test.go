package api

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/audit"
	"github.com/owncloud/ocis-agent/backend/internal/auth"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient/fakeocis"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

const (
	eventsPath = "/agent-api/v1/events"
	readyFrame = "event: ready\ndata: {\"v\":1}\n\n"
	pingFrame  = ": ping\n\n"
	waitLimit  = 5 * time.Second
)

func changedFrame(kinds ...string) string {
	return "event: changed\ndata: {\"kinds\":[\"" + strings.Join(kinds, `","`) + "\"]}\n\n"
}

func byeFrame(reason string) string {
	return "event: bye\ndata: {\"reason\":\"" + reason + "\"}\n\n"
}

// streamTicks are the timers of one stream, driven by the test.
type streamTicks struct {
	user             string
	poll, ping, life chan time.Time
	polled           chan struct{}
	stopped          atomic.Bool
	panicOnPoll      atomic.Bool
}

// fakeTimers replaces the timers of every stream the server opens; made
// receives them in the order the streams start.
type fakeTimers struct{ made chan *streamTicks }

func installTimers(s *Server) *fakeTimers {
	ft := &fakeTimers{made: make(chan *streamTicks, 256)}
	s.eventTimers = func(user string) eventTimers {
		st := &streamTicks{user: user, poll: make(chan time.Time), ping: make(chan time.Time),
			life: make(chan time.Time), polled: make(chan struct{}, 256)}
		ft.made <- st
		return eventTimers{
			poll: st.poll, ping: st.ping, lifetime: st.life,
			stop: func() { st.stopped.Store(true) },
			afterPoll: func() {
				if st.panicOnPoll.Load() {
					panic("poll-panic")
				}
				st.polled <- struct{}{}
			},
		}
	}
	return ft
}

func (ft *fakeTimers) next(t *testing.T) *streamTicks {
	t.Helper()
	select {
	case st := <-ft.made:
		return st
	case <-time.After(waitLimit):
		t.Fatal("no stream started")
		return nil
	}
}

func (st *streamTicks) send(t *testing.T, ch chan time.Time) {
	t.Helper()
	select {
	case ch <- time.Now():
	case <-time.After(waitLimit):
		t.Fatal("the stream did not take the tick")
	}
}

// pollOnce runs one poll and waits until it is processed (an event it
// produced is written by then).
func (st *streamTicks) pollOnce(t *testing.T) {
	t.Helper()
	st.send(t, st.poll)
	select {
	case <-st.polled:
	case <-time.After(waitLimit):
		t.Fatal("the poll did not finish")
	}
}

// sseClient reads a stream frame by frame (a frame ends with an empty line).
type sseClient struct {
	t      *testing.T
	res    *http.Response
	frames chan string
	cancel context.CancelFunc
	mu     sync.Mutex
	raw    strings.Builder
}

// newEventServer serves the fixture over a real listener; it is closed after
// every stream of the test (cleanups run last-in first-out).
func newEventServer(t *testing.T, h http.Handler) *httptest.Server {
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

// boundedClient waits at most waitLimit for response headers (a stream
// that never flushes fails instead of hanging the test).
func boundedClient(t *testing.T) *http.Client {
	tr := &http.Transport{ResponseHeaderTimeout: waitLimit}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}

// serveBounded serves r on a recorder under a 2 s context, so a request
// that wrongly starts a stream ends (and fails the test) instead of hanging.
func (f *fixture) serveBounded(r *http.Request) *httptest.ResponseRecorder {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	return f.serve(r.WithContext(ctx))
}

func (f *fixture) doBounded(method, target string) *httptest.ResponseRecorder {
	return f.serveBounded(f.req(method, target, ""))
}

func openSSE(t *testing.T, ts *httptest.Server, token, target string) *sseClient {
	t.Helper()
	ctx, cancel := context.WithCancel(bg)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+target, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := boundedClient(t).Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	c := &sseClient{t: t, res: res, frames: make(chan string, 1024), cancel: cancel}
	t.Cleanup(c.close)
	if res.StatusCode != http.StatusOK {
		// a refused stream: the test reads the error body itself
		close(c.frames)
		return c
	}
	go func() {
		defer close(c.frames)
		br := bufio.NewReader(res.Body)
		var frame strings.Builder
		for {
			line, err := br.ReadString('\n')
			c.mu.Lock()
			c.raw.WriteString(line)
			c.mu.Unlock()
			if err != nil {
				return
			}
			frame.WriteString(line)
			if line == "\n" {
				c.frames <- frame.String()
				frame.Reset()
			}
		}
	}()
	return c
}

func (c *sseClient) close() {
	c.cancel()
	_ = c.res.Body.Close()
}

func (c *sseClient) all() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.raw.String()
}

func (c *sseClient) want(frame string) {
	c.t.Helper()
	select {
	case got, ok := <-c.frames:
		if !ok {
			c.t.Fatalf("the stream ended, want %q", frame)
		}
		if got != frame {
			c.t.Fatalf("frame = %q, want %q", got, frame)
		}
	case <-time.After(waitLimit):
		c.t.Fatalf("no frame, want %q", frame)
	}
}

// wantEnd waits until the server closed the stream, with no further frame.
func (c *sseClient) wantEnd() {
	c.t.Helper()
	select {
	case got, ok := <-c.frames:
		if ok {
			c.t.Fatalf("frame %q, want the end of the stream", got)
		}
	case <-time.After(waitLimit):
		c.t.Fatal("the stream is still open")
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// openReady opens a stream, checks the headers and the ready frame and
// returns it with its timers.
func openReady(t *testing.T, ts *httptest.Server, ft *fakeTimers, token string) (*sseClient, *streamTicks) {
	t.Helper()
	c := openSSE(t, ts, token, eventsPath)
	if c.res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(c.res.Body)
		t.Fatalf("status = %d %s", c.res.StatusCode, b)
	}
	c.want(readyFrame)
	return c, ft.next(t)
}

func TestEventsHeaders(t *testing.T) {
	f := newFixture(t)
	ft := installTimers(f.srv)
	ts := newEventServer(t, f.h)
	c, _ := openReady(t, ts, ft, goodToken)
	h := c.res.Header
	for k, v := range map[string]string{
		"Content-Type":      "text/event-stream; charset=utf-8",
		"Cache-Control":     "no-store",
		"X-Accel-Buffering": "no",
	} {
		if got := h.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if h.Get("Content-Length") != "" {
		t.Errorf("Content-Length = %q on a stream", h.Get("Content-Length"))
	}
}

// Every kind maps to its own mark and only marks that moved are reported, in
// a fixed order; nothing is sent when nothing changed (the ping after the
// poll is the next frame).
func TestEventsReportTheKindsThatChanged(t *testing.T) {
	s := newChatStack(t)
	ft := installTimers(s.srv)
	ts := newEventServer(t, s.h)
	c, st := openReady(t, ts, ft, fakeocis.OIDCToken("einstein"))

	quiet := func() {
		t.Helper()
		st.pollOnce(t)
		st.send(t, st.ping)
		c.want(pingFrame)
	}
	quiet()

	// a conversation and a message
	s.fake.Advance(time.Second)
	var conv store.Conversation
	s.mustTx(t, func(tx store.Tx) error {
		var err error
		if conv, err = tx.InsertConversation(store.Conversation{UserID: einsteinID}); err != nil {
			return err
		}
		_, err = tx.InsertMessage(store.Message{UserID: einsteinID, ConversationID: conv.ID, Role: store.RoleUser, Content: "hi"})
		return err
	})
	st.pollOnce(t)
	c.want(changedFrame("messages"))
	quiet() // reported once

	// only a message, in the existing conversation: exactly messages
	s.fake.Advance(time.Second)
	s.mustTx(t, func(tx store.Tx) error {
		_, err := tx.InsertMessage(store.Message{UserID: einsteinID, ConversationID: conv.ID, Role: store.RoleUser, Content: "only"})
		return err
	})
	st.pollOnce(t)
	c.want(changedFrame("messages"))

	// a new job, then a state change of it (no audit: exactly jobs)
	s.fake.Advance(time.Second)
	var job store.Job
	s.mustTx(t, func(tx store.Tx) error {
		var err error
		job, err = tx.InsertJob(store.Job{UserID: einsteinID, TriggerKind: store.TriggerChat, Goal: "g"})
		return err
	})
	st.pollOnce(t)
	c.want(changedFrame("jobs"))
	s.fake.Advance(time.Second)
	s.mustTx(t, func(tx store.Tx) error { return tx.CancelJob(einsteinID, job.ID) })
	st.pollOnce(t)
	c.want(changedFrame("jobs"))

	// an audit event
	seedAudit(t, s.st, einsteinID, 1, nil)
	st.pollOnce(t)
	c.want(changedFrame("activity"))

	// a settings PUT through the API (audited too)
	s.fake.Advance(time.Second)
	if rec := s.as("einstein", http.MethodPut, settingsPath, `{"daily_tokens":2000}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT settings = %d %s", rec.Code, rec.Body.String())
	}
	st.pollOnce(t)
	c.want(changedFrame("activity", "settings"))

	// several kinds at once come in the fixed order
	s.fake.Advance(time.Second)
	s.mustTx(t, func(tx store.Tx) error {
		if _, err := tx.InsertMessage(store.Message{UserID: einsteinID, ConversationID: conv.ID, Role: store.RoleUser, Content: "again"}); err != nil {
			return err
		}
		_, err := tx.InsertJob(store.Job{UserID: einsteinID, TriggerKind: store.TriggerChat, Goal: "g2"})
		return err
	})
	seedAudit(t, s.st, einsteinID, 1, nil)
	st.pollOnce(t)
	c.want(changedFrame("jobs", "messages", "activity"))
	quiet()
}

// A mark that goes DOWN is a change too: deleting the conversation with the
// newest message lowers the message mark.
func TestEventsReportAMarkThatWentDown(t *testing.T) {
	s := newChatStack(t)
	ft := installTimers(s.srv)
	ts := newEventServer(t, s.h)
	var newer store.Conversation
	s.mustTx(t, func(tx store.Tx) error {
		if _, err := tx.InsertConversation(store.Conversation{UserID: einsteinID}); err != nil {
			return err
		}
		return nil
	})
	s.fake.Advance(time.Second)
	s.mustTx(t, func(tx store.Tx) error {
		var err error
		if newer, err = tx.InsertConversation(store.Conversation{UserID: einsteinID}); err != nil {
			return err
		}
		_, err = tx.InsertMessage(store.Message{UserID: einsteinID, ConversationID: newer.ID, Role: store.RoleUser, Content: "x"})
		return err
	})
	c, st := openReady(t, ts, ft, fakeocis.OIDCToken("einstein"))
	s.mustTx(t, func(tx store.Tx) error {
		_, err := tx.DeleteConversation(einsteinID, newer.ID)
		return err
	})
	st.pollOnce(t)
	c.want(changedFrame("messages"))
}

// Another user's changes never reach a stream.
func TestEventsAreScopedToTheUser(t *testing.T) {
	s := newChatStack(t)
	ft := installTimers(s.srv)
	ts := newEventServer(t, s.h)
	ce, ste := openReady(t, ts, ft, fakeocis.OIDCToken("einstein"))
	cm, stm := openReady(t, ts, ft, fakeocis.OIDCToken("marie"))
	if ste.user != einsteinID || stm.user != marieID {
		t.Fatalf("stream users %q %q", ste.user, stm.user)
	}
	s.fake.Advance(time.Second)
	s.mustTx(t, func(tx store.Tx) error {
		c, err := tx.InsertConversation(store.Conversation{UserID: marieID})
		if err != nil {
			return err
		}
		if _, err = tx.InsertMessage(store.Message{UserID: marieID, ConversationID: c.ID, Role: store.RoleUser, Content: "m"}); err != nil {
			return err
		}
		_, err = tx.InsertJob(store.Job{UserID: marieID, TriggerKind: store.TriggerChat, Goal: "g"})
		return err
	})
	seedAudit(t, s.st, marieID, 1, nil)
	if rec := s.as("marie", http.MethodPut, settingsPath, `{"daily_tokens":2000}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT settings = %d", rec.Code)
	}
	ste.pollOnce(t)
	ste.send(t, ste.ping)
	ce.want(pingFrame)
	stm.pollOnce(t)
	cm.want(changedFrame("jobs", "messages", "activity", "settings"))
}

// The payloads carry kinds only: no content, titles, ids or user names.
func TestEventsCarryNoContent(t *testing.T) {
	s := newChatStack(t)
	ft := installTimers(s.srv)
	ts := newEventServer(t, s.h)
	c, st := openReady(t, ts, ft, fakeocis.OIDCToken("einstein"))
	const text, title = "ZQX-distinctive-message-text", "ZQX-distinctive-title"
	s.fake.Advance(time.Second)
	var ids []string
	s.mustTx(t, func(tx store.Tx) error {
		conv, err := tx.InsertConversation(store.Conversation{UserID: einsteinID, Title: title})
		if err != nil {
			return err
		}
		j, err := tx.InsertJob(store.Job{UserID: einsteinID, TriggerKind: store.TriggerChat, Goal: text})
		if err != nil {
			return err
		}
		m, err := tx.InsertMessage(store.Message{UserID: einsteinID, ConversationID: conv.ID, Role: store.RoleUser, Content: text, JobID: j.ID})
		ids = append(ids, conv.ID, j.ID, m.ID)
		return err
	})
	seedAudit(t, s.st, einsteinID, 1, func(_ int, e *audit.Event) { e.Detail = text })
	st.pollOnce(t)
	c.want(changedFrame("jobs", "messages", "activity"))
	st.send(t, st.life)
	c.want(byeFrame("lifetime"))
	c.wantEnd()
	all := c.all()
	for _, bad := range append(ids, text, title, einsteinID, "einstein", "ZQX") {
		if strings.Contains(all, bad) {
			t.Errorf("the stream carries %q:\n%s", bad, all)
		}
	}
}

func TestEventsHeartbeat(t *testing.T) {
	f := newFixture(t)
	ft := installTimers(f.srv)
	ts := newEventServer(t, f.h)
	c, st := openReady(t, ts, ft, goodToken)
	for range 3 {
		st.send(t, st.ping)
		c.want(pingFrame)
	}
}

func TestEventsEndAtTheLifetime(t *testing.T) {
	f := newFixture(t)
	ft := installTimers(f.srv)
	ts := newEventServer(t, f.h)
	c, st := openReady(t, ts, ft, goodToken)
	st.send(t, st.life)
	c.want(byeFrame("lifetime"))
	c.wantEnd()
	waitUntil(t, "the slot to be released", func() bool { return f.srv.streams.open() == 0 })
	if !st.stopped.Load() {
		t.Error("the timers were not stopped")
	}
}

// The lifetime is about one hour (55 to 65 min), polls run every 2 s and
// pings every 25 s; the route's context outlives the longest lifetime.
func TestEventsDefaultTimers(t *testing.T) {
	if eventsPollEvery != 2*time.Second || eventsPingEvery != 25*time.Second || eventsLifetimeMin != 55*time.Minute ||
		eventsLifetimeMax != 65*time.Minute || eventsWriteDeadline != 15*time.Second || eventsRouteTimeout <= eventsLifetimeMax {
		t.Errorf("poll %v ping %v lifetime %v..%v write %v route %v", eventsPollEvery, eventsPingEvery,
			eventsLifetimeMin, eventsLifetimeMax, eventsWriteDeadline, eventsRouteTimeout)
	}
	f := newFixture(t)
	var calls atomic.Int32
	f.srv.lifetimeJitter = func(n int64) int64 { calls.Add(1); return 0 }
	et := f.srv.eventTimers("u")
	defer et.stop()
	if et.poll == nil || et.ping == nil || et.lifetime == nil || et.afterPoll != nil {
		t.Errorf("default timers %+v", et)
	}
	if calls.Load() != 1 {
		t.Errorf("the default timers drew %d jittered lifetimes, want 1", calls.Load())
	}
}

// Each stream draws its lifetime from 55 to 65 min, so streams opened
// together do not all reconnect together.
func TestEventsLifetimeIsJittered(t *testing.T) {
	f := newFixture(t)
	span := int64(eventsLifetimeMax - eventsLifetimeMin)
	var gotN int64
	f.srv.lifetimeJitter = func(n int64) int64 { gotN = n; return 0 }
	if d := f.srv.streamLifetime(); d != 55*time.Minute || gotN != span+1 {
		t.Errorf("lowest lifetime %v (n %d)", d, gotN)
	}
	f.srv.lifetimeJitter = func(n int64) int64 { return n - 1 }
	if d := f.srv.streamLifetime(); d != 65*time.Minute {
		t.Errorf("highest lifetime %v", d)
	}
	f.srv.lifetimeJitter = func(n int64) int64 { return n / 2 }
	if d := f.srv.streamLifetime(); d != 60*time.Minute {
		t.Errorf("middle lifetime %v", d)
	}
	// the real source
	f = newFixture(t)
	seen := map[time.Duration]bool{}
	for range 1000 {
		d := f.srv.streamLifetime()
		if d < 55*time.Minute || d > 65*time.Minute {
			t.Fatalf("lifetime %v out of range", d)
		}
		seen[d] = true
	}
	if len(seen) < 100 {
		t.Errorf("only %d distinct lifetimes in 1000 draws", len(seen))
	}
}

// The real lifetime timer fires after the lifetime it was given.
func TestEventsRealLifetimeTimerFires(t *testing.T) {
	et := realEventTimers(time.Hour, 20*time.Millisecond)
	defer et.stop()
	select {
	case <-et.lifetime:
	case <-time.After(waitLimit):
		t.Fatal("the lifetime timer did not fire")
	}
}

// Deps.EventsPollForTest of 0 or less means the real 2 s poll.
func TestEventsPollIntervalDefault(t *testing.T) {
	for _, c := range []struct {
		in, want time.Duration
	}{{0, 2 * time.Second}, {-time.Second, 2 * time.Second}, {7 * time.Millisecond, 7 * time.Millisecond}} {
		f := newFixture(t, func(d *Deps) { d.EventsPollForTest = c.in })
		if f.srv.eventsPoll != c.want {
			t.Errorf("EventsPollForTest %v: poll %v, want %v", c.in, f.srv.eventsPoll, c.want)
		}
	}
}

// flakyStore fails the next fail read transactions.
type flakyStore struct {
	store.Store
	fail atomic.Int32
}

func (s *flakyStore) View(ctx context.Context, fn func(tx store.Tx) error) error {
	if s.fail.Add(-1) >= 0 {
		return errNotUsed
	}
	s.fail.Store(0)
	return s.Store.View(ctx, fn)
}

func newFlakyFixture(t *testing.T) (*fixture, *flakyStore) {
	fs := &flakyStore{}
	f := newFixture(t, func(d *Deps) { fs.Store = d.Store; d.Store = fs })
	return f, fs
}

// One failed poll is skipped: the stream stays open and reports the next
// change against the marks from before the failure.
func TestEventsSurviveATransientPollFailure(t *testing.T) {
	f, fs := newFlakyFixture(t)
	ft := installTimers(f.srv)
	ts := newEventServer(t, f.h)
	c, st := openReady(t, ts, ft, goodToken)
	fs.fail.Store(1)
	st.pollOnce(t)
	st.send(t, st.ping)
	c.want(pingFrame)
	seedAudit(t, f.st, aliceID.UserID, 1, nil)
	st.pollOnce(t)
	c.want(changedFrame("activity"))
	if f.srv.streams.open() != 1 {
		t.Error("the stream lost its slot")
	}
}

// Three failed polls in a row end the stream with bye/unavailable.
func TestEventsEndAfterThreeFailedPolls(t *testing.T) {
	f, fs := newFlakyFixture(t)
	ft := installTimers(f.srv)
	ts := newEventServer(t, f.h)
	c, st := openReady(t, ts, ft, goodToken)
	fs.fail.Store(3)
	st.pollOnce(t)
	st.pollOnce(t)
	st.send(t, st.poll)
	c.want(byeFrame("unavailable"))
	c.wantEnd()
	waitUntil(t, "the slot to be released", func() bool { return f.srv.streams.open() == 0 })
}

// The failure count resets on a successful poll: 2 failures, a success and
// 2 more failures leave the stream open.
func TestEventsPollFailuresResetOnSuccess(t *testing.T) {
	f, fs := newFlakyFixture(t)
	ft := installTimers(f.srv)
	ts := newEventServer(t, f.h)
	c, st := openReady(t, ts, ft, goodToken)
	fs.fail.Store(2)
	st.pollOnce(t)
	st.pollOnce(t)
	st.pollOnce(t) // succeeds
	fs.fail.Store(2)
	st.pollOnce(t)
	st.pollOnce(t)
	st.send(t, st.ping)
	c.want(pingFrame)
}

// Closing Deps.Stop ends every open stream with bye/shutdown; a stream
// requested after it is refused.
func TestEventsEndWhenTheServerStops(t *testing.T) {
	stop := make(chan struct{})
	f := newFixture(t, func(d *Deps) { d.Stop = stop })
	ft := installTimers(f.srv)
	ts := newEventServer(t, f.h)
	c1, _ := openReady(t, ts, ft, goodToken)
	c2, _ := openReady(t, ts, ft, otherToken)
	close(stop)
	for _, c := range []*sseClient{c1, c2} {
		c.want(byeFrame("shutdown"))
		c.wantEnd()
	}
	waitUntil(t, "the slots to be released", func() bool { return f.srv.streams.open() == 0 })
	wantError(t, f.doBounded(http.MethodGet, eventsPath), http.StatusServiceUnavailable, "unavailable")
}

func TestEventsReleaseTheSlotOnDisconnect(t *testing.T) {
	f := newFixture(t, func(d *Deps) { d.Cfg.StreamsPerUser = 1 })
	ft := installTimers(f.srv)
	ts := newEventServer(t, f.h)
	c, st := openReady(t, ts, ft, goodToken)
	c.close()
	waitUntil(t, "the slot to be released", func() bool { return f.srv.streams.open() == 0 })
	waitUntil(t, "the timers to stop", st.stopped.Load)
	openReady(t, ts, ft, goodToken)
}

func TestEventsReleaseTheSlotOnPanic(t *testing.T) {
	f := newFixture(t, func(d *Deps) { d.Cfg.StreamsPerUser = 1 })
	ft := installTimers(f.srv)
	ts := newEventServer(t, f.h)
	c, st := openReady(t, ts, ft, goodToken)
	st.panicOnPoll.Store(true)
	st.send(t, st.poll)
	c.wantEnd()
	waitUntil(t, "the slot to be released", func() bool { return f.srv.streams.open() == 0 })
	if !st.stopped.Load() {
		t.Error("the timers were not stopped")
	}
	openReady(t, ts, ft, goodToken)
}

func wantStreamRefused(t *testing.T, c *sseClient) {
	t.Helper()
	if c.res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", c.res.StatusCode)
	}
	if ra := c.res.Header.Get("Retry-After"); ra != "5" {
		t.Errorf("Retry-After = %q", ra)
	}
	b, _ := io.ReadAll(c.res.Body)
	if !strings.Contains(string(b), `"code":"rate_limited"`) {
		t.Errorf("body %s", b)
	}
}

func TestEventsPerUserCap(t *testing.T) {
	f := newFixture(t, func(d *Deps) { d.Cfg.StreamsPerUser = 2 })
	ft := installTimers(f.srv)
	ts := newEventServer(t, f.h)
	a1, _ := openReady(t, ts, ft, goodToken)
	openReady(t, ts, ft, goodToken)
	wantStreamRefused(t, openSSE(t, ts, goodToken, eventsPath))
	openReady(t, ts, ft, otherToken) // bob has his own slots
	a1.close()
	waitUntil(t, "the slot to be released", func() bool { return f.srv.streams.open() == 2 })
	openReady(t, ts, ft, goodToken)
}

func TestEventsGlobalCap(t *testing.T) {
	f := newFixture(t, func(d *Deps) { d.Cfg.StreamsPerUser, d.Cfg.StreamsTotal = 3, 2 })
	ft := installTimers(f.srv)
	ts := newEventServer(t, f.h)
	a1, _ := openReady(t, ts, ft, goodToken)
	openReady(t, ts, ft, otherToken)
	wantStreamRefused(t, openSSE(t, ts, goodToken, eventsPath))
	wantStreamRefused(t, openSSE(t, ts, otherToken, eventsPath))
	a1.close()
	waitUntil(t, "the slot to be released", func() bool { return f.srv.streams.open() == 1 })
	openReady(t, ts, ft, otherToken)
}

func TestEventsCapSettings(t *testing.T) {
	f := newFixture(t)
	if f.srv.streams.maxUser != 3 || f.srv.streams.maxTotal != 200 {
		t.Errorf("default caps %d %d", f.srv.streams.maxUser, f.srv.streams.maxTotal)
	}
	f = newFixture(t, func(d *Deps) { d.Cfg.StreamsPerUser, d.Cfg.StreamsTotal = 7, 70 })
	if f.srv.streams.maxUser != 7 || f.srv.streams.maxTotal != 70 {
		t.Errorf("caps %d %d", f.srv.streams.maxUser, f.srv.streams.maxTotal)
	}
	for _, bad := range []func(*Deps){
		func(d *Deps) { d.Cfg.StreamsPerUser = -1 },
		func(d *Deps) { d.Cfg.StreamsTotal = -1 },
	} {
		d := Deps{Store: f.st, Validator: f.v, Auth: f.cr, Gate: fakeDecider{}, Queue: f.wk,
			Cfg: Settings{AllowedOrigin: testOrigin, UserDailyTokens: 1, RatePerSecond: 1, Burst: 1}}
		bad(&d)
		if _, err := NewServer(d); err == nil {
			t.Errorf("NewServer accepted %+v", d.Cfg)
		}
	}
}

// 50 clients of five users connect at once and hold their streams: exactly
// the global cap is admitted, no user over its own cap, and every slot comes
// back when they leave.
func TestEventsSlotAccountingUnderConcurrency(t *testing.T) {
	f := newFixture(t, func(d *Deps) { d.Cfg.StreamsPerUser, d.Cfg.StreamsTotal = 3, 12 })
	installTimers(f.srv)
	users := []string{"t0", "t1", "t2", "t3", "t4"}
	for _, u := range users {
		f.v.tokens["tok-"+u] = auth.Identity{UserID: "u-" + u, Username: u, DriveID: "d-" + u}
	}
	ts := newEventServer(t, f.h)
	var (
		attempted, leave sync.WaitGroup
		mu               sync.Mutex
		admitted         = map[string]int{}
		other            []int
	)
	leave.Add(1)
	attempted.Add(50)
	var done sync.WaitGroup
	hc := boundedClient(t)
	for i := range 50 {
		u := users[i%len(users)]
		done.Go(func() {
			ctx, cancel := context.WithTimeout(bg, 30*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+eventsPath, nil)
			req.Header.Set("Authorization", "Bearer tok-"+u)
			res, err := hc.Do(req)
			if err != nil {
				attempted.Done()
				return
			}
			defer res.Body.Close()
			if res.StatusCode == http.StatusOK {
				buf := make([]byte, len(readyFrame))
				if _, err := io.ReadFull(res.Body, buf); err == nil && string(buf) == readyFrame {
					mu.Lock()
					admitted[u]++
					mu.Unlock()
				}
			} else if res.StatusCode != http.StatusTooManyRequests {
				mu.Lock()
				other = append(other, res.StatusCode)
				mu.Unlock()
			}
			attempted.Done()
			leave.Wait()
		})
	}
	attempted.Wait()
	total := 0
	for u, n := range admitted {
		if n > 3 {
			t.Errorf("%s has %d streams", u, n)
		}
		total += n
	}
	if total != 12 || len(other) != 0 {
		t.Errorf("admitted %d (%v), other statuses %v", total, admitted, other)
	}
	if got := f.srv.streams.open(); got != 12 {
		t.Errorf("open slots = %d", got)
	}
	leave.Done()
	done.Wait()
	waitUntil(t, "every slot to be released", func() bool { return f.srv.streams.open() == 0 })
}

// The slot accounting itself under contention.
func TestStreamSlotsAreExact(t *testing.T) {
	sl := newStreamSlots(3, 10)
	var wg sync.WaitGroup
	var got atomic.Int32
	for i := range 50 {
		wg.Go(func() {
			u := strconv.Itoa(i % 5)
			for range 200 {
				if sl.acquire(u) {
					got.Add(1)
					if sl.open() > 10 || sl.openFor(u) > 3 {
						t.Error("over the cap")
					}
					sl.release(u)
				}
			}
		})
	}
	wg.Wait()
	if sl.open() != 0 || len(sl.perUser) != 0 || got.Load() == 0 {
		t.Errorf("open %d, users %d, acquired %d", sl.open(), len(sl.perUser), got.Load())
	}
}

// viewSpy counts the read transactions and how many are open.
type viewSpy struct {
	store.Store
	views, openNow atomic.Int32
}

func (s *viewSpy) View(ctx context.Context, fn func(tx store.Tx) error) error {
	s.views.Add(1)
	s.openNow.Add(1)
	defer s.openNow.Add(-1)
	return s.Store.View(ctx, fn)
}

// Each poll is one short read transaction; none stays open between polls.
func TestEventsPollWithOneShortViewEach(t *testing.T) {
	spy := &viewSpy{}
	f := newFixture(t, func(d *Deps) { spy.Store = d.Store; d.Store = spy })
	ft := installTimers(f.srv)
	ts := newEventServer(t, f.h)
	c, st := openReady(t, ts, ft, goodToken)
	if n := spy.openNow.Load(); n != 0 {
		t.Fatalf("%d views open after ready", n)
	}
	before := spy.views.Load()
	for i := range 3 {
		st.pollOnce(t)
		if n := spy.openNow.Load(); n != 0 {
			t.Fatalf("poll %d: %d views open between polls", i, n)
		}
		if got := spy.views.Load() - before; got != int32(i+1) {
			t.Fatalf("poll %d: %d views", i, got)
		}
	}
	st.send(t, st.ping)
	c.want(pingFrame)
}

func TestEventsRefuseQueryAndOtherMethods(t *testing.T) {
	f := newFixture(t)
	installTimers(f.srv)
	for _, q := range []string{"?x=1", "?since=5", "?%zz", "?v=1&v=2"} {
		wantError(t, f.doBounded(http.MethodGet, eventsPath+q), http.StatusBadRequest, "invalid_request")
	}
	rec := f.doBounded(http.MethodHead, eventsPath)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodGet {
		t.Errorf("HEAD = %d Allow %q", rec.Code, rec.Header().Get("Allow"))
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		if rec := f.doBounded(m, eventsPath); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s = %d", m, rec.Code)
		}
	}
	if f.srv.streams.open() != 0 {
		t.Errorf("refused requests hold %d slots", f.srv.streams.open())
	}
	r := f.req(http.MethodGet, eventsPath, "")
	r.Header.Del("Authorization")
	wantError(t, f.serveBounded(r), http.StatusUnauthorized, "unauthenticated")
}

// A store failure before the first byte is a normal error response.
func TestEventsStoreFailureBeforeTheStream(t *testing.T) {
	f := newFixture(t, func(d *Deps) { d.Store = failingStore{Store: d.Store, err: errNotUsed} })
	installTimers(f.srv)
	wantError(t, f.doBounded(http.MethodGet, eventsPath), http.StatusInternalServerError, "internal")
	if f.srv.streams.open() != 0 {
		t.Error("the slot was not released")
	}
}

// The stream outlives the pipeline's default deadlines: the read deadline
// set before dispatch is cleared (a read deadline would end the request
// context through the server's background read).
func TestEventsOutliveTheDefaultDeadlines(t *testing.T) {
	old := writeGrace
	writeGrace = 0
	t.Cleanup(func() { writeGrace = old })
	f := newFixture(t)
	f.srv.baseTimeout = 50 * time.Millisecond
	ft := installTimers(f.srv)
	ts := newEventServer(t, f.h)
	c, st := openReady(t, ts, ft, goodToken)
	time.Sleep(300 * time.Millisecond)
	st.send(t, st.ping)
	c.want(pingFrame)
	time.Sleep(300 * time.Millisecond)
	st.pollOnce(t)
	st.send(t, st.ping)
	c.want(pingFrame)
}

// A client that stops reading is cut off at the per-frame write deadline
// (not at the route's or the default deadline) and its slot is freed. The
// server's connection simulates the stall: once stalled, a write blocks
// until the connection's write deadline.
func TestEventsCutOffAStalledClient(t *testing.T) {
	f := newFixture(t)
	f.srv.eventWriteDeadline = 200 * time.Millisecond
	ft := installTimers(f.srv)
	stall := &atomic.Bool{}
	ts := httptest.NewUnstartedServer(f.h)
	sl := &stallListener{Listener: ts.Listener, stall: stall}
	ts.Listener = sl
	ts.Start()
	t.Cleanup(ts.Close)
	t.Cleanup(sl.closeAll) // before ts.Close: a stalled write must not block it
	c, st := openReady(t, ts, ft, goodToken)
	st.send(t, st.ping)
	c.want(pingFrame)
	stall.Store(true)
	began := time.Now()
	st.send(t, st.ping)
	waitUntil(t, "the slot to be released", func() bool { return f.srv.streams.open() == 0 })
	if d := time.Since(began); d > 3*time.Second {
		t.Errorf("cut off after %v", d)
	}
	waitUntil(t, "the timers to stop", st.stopped.Load)
	c.wantEnd()
}

// stallListener hands out connections whose writes block until their write
// deadline once stall is set.
type stallListener struct {
	net.Listener
	stall *atomic.Bool
	mu    sync.Mutex
	conns []*stallConn
}

func (l *stallListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	sc := &stallConn{Conn: c, stall: l.stall, closed: make(chan struct{})}
	l.mu.Lock()
	l.conns = append(l.conns, sc)
	l.mu.Unlock()
	return sc, nil
}

func (l *stallListener) closeAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range l.conns {
		_ = c.Close()
	}
}

type stallConn struct {
	net.Conn
	stall     *atomic.Bool
	mu        sync.Mutex
	deadline  time.Time
	closed    chan struct{}
	closeOnce sync.Once
}

func (c *stallConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	return c.Conn.SetWriteDeadline(t)
}

func (c *stallConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	return c.Conn.SetDeadline(t)
}

func (c *stallConn) Write(b []byte) (int, error) {
	if !c.stall.Load() {
		return c.Conn.Write(b)
	}
	c.mu.Lock()
	dl := c.deadline
	c.mu.Unlock()
	var wait <-chan time.Time // no deadline: blocks until closed
	if !dl.IsZero() {
		wait = time.After(time.Until(dl))
	}
	select {
	case <-wait:
		return 0, os.ErrDeadlineExceeded
	case <-c.closed:
		return 0, net.ErrClosed
	}
}

func (c *stallConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// deadlineWriter is a response writer with connection deadlines: it records
// every deadline the handler and the pipeline set.
type deadlineWriter struct {
	mu     sync.Mutex
	header http.Header
	body   strings.Builder
	reads  []time.Time
	writes []time.Time // with the time they were set
	setAt  []time.Time
}

func (d *deadlineWriter) Header() http.Header { return d.header }
func (d *deadlineWriter) WriteHeader(int)     {}
func (d *deadlineWriter) Write(b []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.body.Write(b)
}
func (d *deadlineWriter) Flush() {}
func (d *deadlineWriter) SetReadDeadline(t time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reads = append(d.reads, t)
	return nil
}
func (d *deadlineWriter) SetWriteDeadline(t time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.writes, d.setAt = append(d.writes, t), append(d.setAt, time.Now())
	return nil
}
func (d *deadlineWriter) bodyHas(s string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return strings.Contains(d.body.String(), s)
}

// The stream route replaces the pipeline's deadlines: the read deadline is
// cleared (the last one set is zero) and every frame renews the write
// deadline to eventWriteDeadline from the moment it is written.
func TestEventsSetTheirOwnDeadlines(t *testing.T) {
	f := newFixture(t)
	f.srv.eventWriteDeadline = 7 * time.Second
	ft := installTimers(f.srv)
	dw := &deadlineWriter{header: http.Header{}}
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.h.ServeHTTP(dw, f.req(http.MethodGet, eventsPath, "").WithContext(ctx))
	}()
	st := ft.next(t)
	waitUntil(t, "ready", func() bool { return dw.bodyHas(readyFrame) })
	dw.mu.Lock()
	beforePing := len(dw.writes)
	dw.mu.Unlock()
	st.send(t, st.ping)
	waitUntil(t, "the ping", func() bool { return dw.bodyHas(pingFrame) })
	cancel()
	<-done
	dw.mu.Lock()
	defer dw.mu.Unlock()
	if n := len(dw.reads); n < 2 || !dw.reads[0].After(time.Now()) || !dw.reads[n-1].IsZero() {
		t.Errorf("read deadlines %v, want the default and then none", dw.reads)
	}
	// pipeline default (serve), the route's, ready, ping
	if len(dw.writes) != 4 || beforePing != 3 {
		t.Fatalf("write deadlines %v (%d before the ping)", dw.writes, beforePing)
	}
	for i := 2; i < 4; i++ {
		if d := dw.writes[i].Sub(dw.setAt[i]); d < 7*time.Second-100*time.Millisecond || d > 7*time.Second {
			t.Errorf("frame %d: write deadline %v ahead, want 7s", i, d)
		}
	}
}

// Streams leave no goroutines behind, however they end.
func TestEventsLeakNoGoroutines(t *testing.T) {
	stop := make(chan struct{})
	f := newFixture(t, func(d *Deps) { d.Stop = stop; d.Cfg.StreamsPerUser = 10 })
	ft := installTimers(f.srv)
	ts := httptest.NewServer(f.h)
	base := runtime.NumGoroutine()
	var clients []*sseClient
	for range 4 {
		c, _ := openReady(t, ts, ft, goodToken)
		clients = append(clients, c)
	}
	clients[0].close()
	close(stop)
	for _, c := range clients[1:] {
		c.want(byeFrame("shutdown"))
		c.wantEnd()
		c.close()
	}
	waitUntil(t, "the slots to be released", func() bool { return f.srv.streams.open() == 0 })
	ts.Client().CloseIdleConnections()
	ts.Close()
	goroutinesSettle(t, base)
}

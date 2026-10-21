package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/tools"
)

const sseEndpoint = "/ocs/v2.php/apps/notifications/api/v1/notifications/sse"

// sseFixture is a watch fixture with the notification streams on, short
// backoffs and app tokens in the store (the stream key follows the label).
func newSSEFixture(t *testing.T) *watchFixture {
	t.Helper()
	w := newWatchFixture(t)
	w.cfg.SSE = SSEConfig{
		Enabled: true, MaxSubscriptions: 10,
		MinBackoff: 5 * time.Millisecond, MaxBackoff: 40 * time.Millisecond, StableAfter: time.Minute,
		PokeGap: time.Second,
	}
	if err := w.st.InTx(bg, func(tx store.Tx) error {
		return tx.PutDEK(store.DEK{ID: "k1", WrappedKey: []byte{1}, Nonce: []byte{1}, CreatedAt: w.clk.Now()})
	}); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *watchFixture) sseUsers(n int, folders ...string) []string {
	w.t.Helper()
	ids := w.watchUsers(n, folders...)
	for _, id := range ids {
		w.setToken(id, "ocis-agent-i-1", store.TokenValid)
	}
	return ids
}

func (w *watchFixture) setToken(user, label string, st store.TokenStatus) {
	w.t.Helper()
	if err := w.st.InTx(bg, func(tx store.Tx) error {
		return tx.PutAppToken(store.AppToken{
			UserID: user, Sealed: store.Sealed{DEKID: "k1", Nonce: []byte{1}, Ciphertext: []byte{1}},
			Label: label, CreatedAt: w.clk.Now(), ExpiresAt: w.clk.Now().Add(72 * time.Hour), Status: st,
		})
	}); err != nil {
		w.t.Fatal(err)
	}
}

func (w *watchFixture) sseScheduler() *Scheduler {
	w.t.Helper()
	s := w.scheduler()
	w.t.Cleanup(s.Close)
	return s
}

func (w *watchFixture) subscribe(s *Scheduler) int {
	w.t.Helper()
	n, _, err := s.sseRun(bg, w.clk.Now())
	if err != nil {
		w.t.Fatalf("sse: %v", err)
	}
	return n
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting: %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (w *watchFixture) streams(user string) int { return w.fake.OpenStreams("name-" + user) }

func TestSSEEventBringsTheNextPollForward(t *testing.T) {
	w := newSSEFixture(t)
	u := w.sseUsers(1)[0]
	s := w.sseScheduler()
	w.poll(s) // baseline; the next poll is 5 minutes away
	if n := w.subscribe(s); n != 1 {
		t.Fatalf("%d subscriptions, want 1", n)
	}
	eventually(t, "the stream opens", func() bool { return w.streams(u) == 1 })

	w.put(u, "/Documents/new.txt")
	w.fake.Publish("name-"+u, "1", "postprocessing-finished", `{"itemid":"x","initiatorid":"`+u+`"}`)
	eventually(t, "the event is received", func() bool { return s.sse.received.Load() == 1 })
	w.clk.Advance(10 * time.Second)
	if n, _ := w.poll(s); n != 1 {
		t.Fatalf("polled %d after the event, want 1", n)
	}
	if c := w.cursor(u); c.WatchDirtySince == nil {
		t.Fatal("the poll brought forward did not see the change")
	}
	// it still goes through the debounce: no job yet
	if jobs := w.fileJobs(u); len(jobs) != 0 {
		t.Fatalf("an event started a job: %+v", jobs)
	}
	// the event is used up: no further early poll
	w.clk.Advance(10 * time.Second)
	if n, _ := w.poll(s); n != 0 {
		t.Fatalf("polled %d without a new event", n)
	}
}

func TestSSEEventsForNothingWatchedAreIgnored(t *testing.T) {
	w := newSSEFixture(t)
	u := w.sseUsers(1)[0]
	s := w.sseScheduler()
	w.poll(s)
	w.subscribe(s)
	eventually(t, "the stream opens", func() bool { return w.streams(u) == 1 })
	for i, name := range []string{"share-created", "message", "space-shared", "Folder-Created", "userlog-notification"} {
		w.fake.Publish("name-"+u, "x", name, `{"secret-marker":"`+name+`"}`)
		eventually(t, "the event is received", func() bool { return s.sse.received.Load() == int64(i+1) })
	}
	w.clk.Advance(10 * time.Second)
	if n, _ := w.poll(s); n != 0 {
		t.Fatalf("polled %d after unrelated events", n)
	}
	for _, name := range []string{"folder-created", "postprocessing-finished", "item-renamed", "item-trashed"} {
		w.fake.Publish("name-"+u, "y", name, `{}`)
		before := s.sse.received.Load()
		eventually(t, "the event is received", func() bool { return s.sse.received.Load() == before+1 })
		w.clk.Advance(time.Minute)
		if n, _ := w.poll(s); n != 1 {
			t.Fatalf("%s: polled %d, want 1", name, n)
		}
	}
	if strings.Contains(w.logs.String(), "secret-marker") {
		t.Error("event data reached the log")
	}
}

func TestSSEPollsBroughtForwardAreSpacedOut(t *testing.T) {
	w := newSSEFixture(t)
	w.cfg.SSE.PokeGap = 30 * time.Second
	u := w.sseUsers(1)[0]
	s := w.sseScheduler()
	w.poll(s)
	w.subscribe(s)
	eventually(t, "the stream opens", func() bool { return w.streams(u) == 1 })
	publish := func() {
		before := s.sse.received.Load()
		w.fake.Publish("name-"+u, "z", "item-trashed", `{}`)
		eventually(t, "the event is received", func() bool { return s.sse.received.Load() == before+1 })
	}
	polls := 0
	for range 9 { // 90 seconds of an event every 10 seconds
		publish()
		w.clk.Advance(10 * time.Second)
		n, _ := w.poll(s)
		polls += n
	}
	if polls != 3 {
		t.Fatalf("%d polls in 90s of events, want 3 (one per 30s)", polls)
	}
}

func TestSSEOnlyEligibleUsersUpToTheCapAreSubscribed(t *testing.T) {
	w := newSSEFixture(t)
	w.cfg.SSE.MaxSubscriptions = 2
	ids := w.sseUsers(4)
	// no valid token: not subscribed
	w.setToken(ids[0], "ocis-agent-i-1", store.TokenInvalid)
	s := w.sseScheduler()
	if n := w.subscribe(s); n != 2 {
		t.Fatalf("%d subscriptions, want the cap of 2", n)
	}
	eventually(t, "two streams", func() bool { return w.streams(ids[1])+w.streams(ids[2]) == 2 })
	time.Sleep(20 * time.Millisecond)
	if w.streams(ids[0]) != 0 || w.streams(ids[3]) != 0 {
		t.Fatal("a stream beyond the cap or without a token")
	}
	// the users beyond the cap are still polled
	w.clk.Advance(time.Hour)
	if n, _ := w.poll(s); n != 4 {
		t.Fatalf("polled %d, want all 4", n)
	}
}

func TestSSEStreamsFollowTheUsersState(t *testing.T) {
	cases := map[string]func(w *watchFixture, u string){
		"paused": func(w *watchFixture, u string) {
			w.enableProactive(u, 0, func(p *store.Policy) { p.Paused, p.PauseReason = true, store.PauseUser })
		},
		"disabled": func(w *watchFixture, u string) {
			w.enableProactive(u, 0, func(p *store.Policy) { p.ProactiveEnabled = false })
		},
		"no watch folders": func(w *watchFixture, u string) {
			w.enableProactive(u, 0, func(p *store.Policy) { p.Watch = nil })
		},
		"unenrolled": func(w *watchFixture, u string) {
			if err := w.st.InTx(bg, func(tx store.Tx) error { return tx.DeleteUser(u) }); err != nil {
				w.t.Fatal(err)
			}
		},
		"token revoked": func(w *watchFixture, u string) {
			if err := w.st.InTx(bg, func(tx store.Tx) error { return tx.WipeAppToken(u) }); err != nil {
				w.t.Fatal(err)
			}
		},
		"kill switch": func(w *watchFixture, u string) {
			if err := w.st.InTx(bg, func(tx store.Tx) error { return tx.SetSystem(store.KillSwitchKey, store.KillSwitchOnValue) }); err != nil {
				w.t.Fatal(err)
			}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			w := newSSEFixture(t)
			ids := w.sseUsers(2)
			s := w.sseScheduler()
			w.subscribe(s)
			eventually(t, "the streams open", func() bool { return w.streams(ids[0]) == 1 && w.streams(ids[1]) == 1 })
			change(w, ids[0])
			w.subscribe(s)
			eventually(t, "the stream closes", func() bool { return w.streams(ids[0]) == 0 })
			if name != "kill switch" && w.streams(ids[1]) != 1 {
				t.Fatal("the other user's stream closed too")
			}
		})
	}
}

func TestSSEKillSwitchFromTheEnvironmentClosesTheStreams(t *testing.T) {
	w := newSSEFixture(t)
	u := w.sseUsers(1)[0]
	w.cfg.Proactive.Limits.KillSwitchEnv = true
	s := w.sseScheduler()
	if n := w.subscribe(s); n != 0 {
		t.Fatalf("%d subscriptions under the kill switch", n)
	}
	time.Sleep(20 * time.Millisecond)
	if w.streams(u) != 0 {
		t.Fatal("a stream under the kill switch")
	}
}

func TestSSEStreamIsRenewedWithTheCredentials(t *testing.T) {
	w := newSSEFixture(t)
	u := w.sseUsers(1)[0]
	s := w.sseScheduler()
	w.subscribe(s)
	eventually(t, "the stream opens", func() bool { return w.streams(u) == 1 })
	connects := s.sse.connects.Load()
	w.subscribe(s)
	time.Sleep(20 * time.Millisecond)
	if got := s.sse.connects.Load(); got != connects {
		t.Fatalf("%d connects without a change, want %d", got, connects)
	}
	w.setToken(u, "ocis-agent-i-2", store.TokenValid) // renewed
	w.subscribe(s)
	eventually(t, "a new stream", func() bool { return s.sse.connects.Load() == connects+1 && w.streams(u) == 1 })
}

func TestSSEReconnectsAfterTheServerClosesTheStream(t *testing.T) {
	w := newSSEFixture(t)
	u := w.sseUsers(1)[0]
	s := w.sseScheduler()
	w.subscribe(s)
	eventually(t, "the stream opens", func() bool { return w.streams(u) == 1 })
	for i := range 3 {
		w.fake.DropStreams()
		eventually(t, "the stream reopens", func() bool { return s.sse.connects.Load() == int64(i+2) && w.streams(u) == 1 })
	}
	// events on the new stream still count
	w.poll(s)
	w.fake.Publish("name-"+u, "1", "folder-created", `{}`)
	eventually(t, "the event is received", func() bool { return s.sse.received.Load() == 1 })
}

// failingSSE answers every stream request with status and records when.
type failingSSE struct {
	mu     sync.Mutex
	status int
	at     []time.Time
	auths  []string
}

func (f *failingSSE) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.at = append(f.at, time.Now())
	f.auths = append(f.auths, r.Header.Get("Authorization"))
	f.mu.Unlock()
	if r.URL.Path != sseEndpoint || r.URL.RawQuery != "" {
		w.WriteHeader(http.StatusTeapot)
		return
	}
	w.WriteHeader(f.status)
}

func (f *failingSSE) attempts() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.at)
}

// withServer makes the users' environments talk to srv.
func (w *watchFixture) withServer(t *testing.T, h http.Handler, opts ...ocisclient.Option) {
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := ocisclient.New(srv.URL, opts...)
	if err != nil {
		t.Fatal(err)
	}
	inner := w.deps.Env
	w.deps.Env = func(ctx context.Context, id string) (tools.Env, error) {
		env, err := inner(ctx, id)
		env.Files = c
		return env, err
	}
}

func TestSSEBacksOffWithJitterAfterFailures(t *testing.T) {
	w := newSSEFixture(t)
	w.cfg.SSE.MinBackoff, w.cfg.SSE.MaxBackoff = 10*time.Millisecond, 80*time.Millisecond
	srv := &failingSSE{status: http.StatusServiceUnavailable}
	w.withServer(t, srv)
	w.sseUsers(1)
	s := w.sseScheduler()
	w.subscribe(s)
	eventually(t, "several attempts", func() bool { return len(srv.attempts()) >= 7 })
	at := srv.attempts()
	for i := 1; i < len(at); i++ {
		want := min(10*time.Millisecond<<(i-1), 80*time.Millisecond)
		if gap := at[i].Sub(at[i-1]); gap < want/2 {
			t.Errorf("attempt %d came %v after the previous one, want at least %v", i, gap, want/2)
		}
	}
	srv.mu.Lock()
	for _, a := range srv.auths {
		if !strings.HasPrefix(a, "Basic ") {
			t.Errorf("authorization %q, want Basic (the app token in a header)", a)
		}
	}
	srv.mu.Unlock()
	if strings.Contains(w.logs.String(), "pw") {
		t.Error("the log mentions the token")
	}
}

func TestSSEUnauthorizedIsReportedAndBacksOffTheMost(t *testing.T) {
	w := newSSEFixture(t)
	w.cfg.SSE.MinBackoff, w.cfg.SSE.MaxBackoff = 5*time.Millisecond, 100*time.Millisecond
	srv := &failingSSE{status: http.StatusUnauthorized}
	w.withServer(t, srv)
	u := w.sseUsers(1)[0]
	s := w.sseScheduler()
	w.subscribe(s)
	eventually(t, "two attempts", func() bool { return len(srv.attempts()) >= 2 })
	at := srv.attempts()
	if gap := at[1].Sub(at[0]); gap < 50*time.Millisecond {
		t.Errorf("retried %v after a 401, want at least half the max backoff", gap)
	}
	if got := w.f.unauthorizedUsers(); len(got) == 0 || got[0] != u {
		t.Fatalf("HandleUnauthorized calls = %v, want %s", got, u)
	}
}

func TestSSECloseStopsEveryStreamAndLeaksNothing(t *testing.T) {
	w := newSSEFixture(t)
	ids := w.sseUsers(3)
	s := w.scheduler()
	before := runtime.NumGoroutine()
	w.subscribe(s)
	eventually(t, "the streams open", func() bool {
		return w.streams(ids[0])+w.streams(ids[1])+w.streams(ids[2]) == 3
	})
	s.Close()
	eventually(t, "the streams close", func() bool {
		return w.streams(ids[0])+w.streams(ids[1])+w.streams(ids[2]) == 0
	})
	s.Close() // idempotent
	// nothing starts after Close
	if n := w.subscribe(s); n != 0 {
		t.Fatalf("%d subscriptions after Close", n)
	}
	eventually(t, "the goroutines end", func() bool { return runtime.NumGoroutine() <= before+2 })
}

func TestSSERunEndsTheStreams(t *testing.T) {
	w := newSSEFixture(t)
	u := w.sseUsers(1)[0]
	w.cfg.Intervals.Tick = 5 * time.Millisecond
	s := w.scheduler()
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	eventually(t, "the stream opens", func() bool { return w.streams(u) == 1 })
	cancel()
	<-done
	if n := s.sse.running(); n != 0 {
		t.Fatalf("%d streams still running after Run returned", n)
	}
	eventually(t, "the stream closes", func() bool { return w.streams(u) == 0 })
}

func TestSSEDisabled(t *testing.T) {
	w := newSSEFixture(t)
	w.cfg.SSE.Enabled = false
	u := w.sseUsers(1)[0]
	s := w.sseScheduler()
	res := s.Tick(bg)
	if slices.Contains(res.Ran, TaskSSE) {
		t.Fatalf("ran %v with the streams off", res.Ran)
	}
	time.Sleep(20 * time.Millisecond)
	if w.streams(u) != 0 {
		t.Fatal("a stream with the streams off")
	}
}

func TestSSETickWiring(t *testing.T) {
	w := newSSEFixture(t)
	u := w.sseUsers(1)[0]
	s := w.sseScheduler()
	res := s.Tick(bg)
	i, j := slices.Index(res.Ran, TaskSSE), slices.Index(res.Ran, TaskReconcile)
	if i < 0 || j < 0 || i > j {
		t.Fatalf("ran %v: the stream task must run among the database tasks", res.Ran)
	}
	if res.Counts[TaskSSE] != 1 {
		t.Fatalf("counts = %v", res.Counts)
	}
	eventually(t, "the stream opens", func() bool { return w.streams(u) == 1 })
}

// The agent's own approved write sends notification events like a user's,
// but they bring forward a poll that sees no change; a user change while a
// review runs is recorded as a skip and starts one follow-up review, whose
// own write starts nothing more.
func TestSSEOwnWritesDoNotLoop(t *testing.T) {
	w := newSSEFixture(t)
	u := w.sseUsers(1)[0]
	s := w.sseScheduler()
	w.poll(s)
	w.subscribe(s)
	eventually(t, "the stream opens", func() bool { return w.streams(u) == 1 })
	event := func() {
		before := s.sse.received.Load()
		w.fake.Publish("name-"+u, "e", "postprocessing-finished", `{"initiatorid":"`+u+`"}`)
		eventually(t, "the event is received", func() bool { return s.sse.received.Load() == before+1 })
	}
	ownWrite := func(p string) {
		w.agentWrite(u, []string{p}, func() { w.put(u, p) })
		event()
	}

	// a burst of the agent's own writes: no job
	for i := range 5 {
		ownWrite("/Documents/own" + string(rune('a'+i)) + ".md")
		w.clk.Advance(time.Minute)
		w.poll(s)
	}
	if jobs := w.fileJobs(u); len(jobs) != 0 {
		t.Fatalf("the agent's own writes started reviews: %+v", jobs)
	}

	// a user change while a review is live waits, audited as a skip
	w.insertJob(u, store.TriggerFileEvent, store.JobQueued)
	w.put(u, "/Documents/user.md")
	event()
	for range 3 {
		w.clk.Advance(time.Minute)
		w.poll(s)
	}
	if got := w.auditOf(u, "proactive_skipped"); len(got) != 1 || !strings.Contains(got[0], "live_job") {
		t.Fatalf("skips = %v, want one live_job", got)
	}
	w.cancelLive(u)
	w.clk.Advance(5 * time.Minute)
	w.poll(s)
	if n := len(w.fileJobs(u)); n != 2 { // the inserted one and the follow-up
		t.Fatalf("%d file jobs, want the follow-up", n)
	}
	// the follow-up's own write starts nothing more
	w.cancelLive(u)
	ownWrite("/Documents/followup.md")
	for range 4 {
		w.clk.Advance(5 * time.Minute)
		w.poll(s)
	}
	if n := len(w.fileJobs(u)); n != 2 {
		t.Fatalf("%d file jobs after the follow-up's own write, want 2", n)
	}
}

// polledUsers returns the users whose folders the fake was asked about.
func (w *watchFixture) polledUsers() map[string]bool {
	out := map[string]bool{}
	for _, r := range w.fake.Requests() {
		if r.Method != "PROPFIND" {
			continue
		}
		if rest, ok := strings.CutPrefix(r.Path, "/remote.php/dav/spaces/drive-"); ok {
			id, _, _ := strings.Cut(rest, "/")
			out[id] = true
		}
	}
	return out
}

// Poked users get at most half of a run's polls, so the rotation (users
// beyond the cap, users whose stream is down) always gets the rest.
func TestSSEPokedPollsLeaveRoomForTheRotation(t *testing.T) {
	w := newSSEFixture(t)
	w.cfg.Watch.PerTick = 4
	ids := w.sseUsers(8)
	s := w.sseScheduler()
	for range 3 {
		w.poll(s) // baselines, four per run, then the rotation wraps
	}
	w.clk.Advance(time.Hour) // everyone is due by the interval
	for _, id := range ids[4:] {
		s.sse.poke(id)
	}
	w.fake.ResetRequests()
	if n, _ := w.poll(s); n != 4 {
		t.Fatalf("polled %d, want PerTick", n)
	}
	got := w.polledUsers()
	poked, rotation := 0, 0
	for id := range got {
		if slices.Contains(ids[4:], id) {
			poked++
		} else {
			rotation++
		}
	}
	if poked != 2 || rotation != 2 {
		t.Fatalf("polled %d poked and %d rotation users, want 2 and 2", poked, rotation)
	}
	// the poked users left over come next
	w.fake.ResetRequests()
	w.clk.Advance(10 * time.Second)
	w.poll(s)
	got = w.polledUsers()
	left := 0
	for _, id := range ids[4:] {
		if got[id] {
			left++
		}
	}
	if left != 2 {
		t.Fatalf("%d of the remaining poked users polled next, want 2 (%v)", left, got)
	}
}

// quietSSE accepts every stream and then sends nothing, like oCIS without
// SSE_KEEPALIVE_INTERVAL.
type quietSSE struct{ failingSSE }

func (q *quietSSE) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q.mu.Lock()
	q.at = append(q.at, time.Now())
	q.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	w.(http.Flusher).Flush()
	<-r.Context().Done()
}

// A stream that only went quiet (the idle timeout) reconnects after a
// modest delay with jitter, not at once, so quiet streams do not churn.
func TestSSEIdleStreamsReconnectAfterAModestDelay(t *testing.T) {
	w := newSSEFixture(t)
	w.cfg.SSE.MinBackoff, w.cfg.SSE.StableAfter, w.cfg.SSE.IdleReconnect = time.Millisecond, time.Millisecond, 80*time.Millisecond
	srv := &quietSSE{}
	w.withServer(t, srv, ocisclient.WithSSEIdleTimeout(20*time.Millisecond))
	w.sseUsers(1)
	s := w.sseScheduler()
	w.subscribe(s)
	eventually(t, "several connections", func() bool { return len(srv.attempts()) >= 4 })
	at := srv.attempts()
	for i := 1; i < len(at); i++ {
		// 20ms idle plus at least half of the 80ms delay
		if gap := at[i].Sub(at[i-1]); gap < 60*time.Millisecond {
			t.Errorf("reconnect %d came %v after the previous connection, want at least 60ms", i, gap)
		}
	}
}

// The notification of the agent's own upload pokes a poll that runs while
// the rebaseline still waits: the folder is left alone and no review starts.
func TestSSEEventPokedPollDuringTheRebaseline(t *testing.T) {
	w := newSSEFixture(t)
	u := w.sseUsers(1)[0]
	s := w.sseScheduler()
	w.poll(s)
	w.subscribe(s)
	eventually(t, "the stream opens", func() bool { return w.streams(u) == 1 })
	w.agentWrite(u, []string{"/Documents/up.md"}, func() {
		w.upload(u, "/Documents/up.md")
		w.fake.Publish("name-"+u, "1", "postprocessing-finished", `{}`)
		eventually(t, "the event is received", func() bool { return s.sse.received.Load() == 1 })
		w.clk.Advance(10 * time.Second)
		if n, _ := w.poll(s); n != 1 {
			t.Fatalf("polled %d, want the poll the event brought forward", n)
		}
	})
	if c := w.cursor(u); c.WatchDirtySince != nil {
		t.Fatalf("the agent's own upload was recorded as a change: %+v", c)
	}
	w.settle(s)
	if jobs := w.fileJobs(u); len(jobs) != 0 {
		t.Fatalf("jobs = %+v", jobs)
	}
}

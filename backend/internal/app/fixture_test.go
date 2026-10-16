package app

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/agent"
	"github.com/owncloud/ocis-agent/backend/internal/auth"
	"github.com/owncloud/ocis-agent/backend/internal/config"
	"github.com/owncloud/ocis-agent/backend/internal/llm"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient/fakeocis"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/store/sqlite"
)

var bg = context.Background()

const (
	userID = "u1"
	login  = "einstein"
	pass   = "relativity"
	drive  = "drive1"
	model  = "test-model"
)

type fixture struct {
	t    *testing.T
	fake *fakeocis.Server
	st   *sqlite.Store
	cfg  config.Config
}

// newFixture starts a fake oCIS with one user and opens a fresh store. The
// configuration is loaded like main does, with a loopback model endpoint
// configured (tests replace the provider through Options), the API and the
// debug listener on free loopback ports, one worker and no shutdown drain.
// extra overrides or adds environment variables; "-" deletes one.
func newFixture(t *testing.T, extra map[string]string) *fixture {
	t.Helper()
	fake := fakeocis.New(t)
	fake.AddUser(login, pass, userID, drive)
	// the fake's clock is frozen; move it to the real time, so that the
	// tokens it hands out are not already due for renewal
	if d := time.Since(fake.Now()); d > 0 {
		fake.Advance(d)
	}
	dir := t.TempDir()
	env := map[string]string{
		"AGENT_OCIS_URL":       fake.URL,
		"AGENT_MASTER_KEY":     base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
		"AGENT_DATA_DIR":       dir,
		"AGENT_HTTP_ADDR":      "127.0.0.1:0",
		"AGENT_LLM_BASE_URL":   "http://127.0.0.1:1/v1",
		"AGENT_LLM_MODEL":      model,
		"AGENT_WORKERS":        "1",
		"AGENT_SHUTDOWN_DRAIN": "0s",
	}
	for k, v := range extra {
		if v == "-" {
			delete(env, k)
		} else {
			env[k] = v
		}
	}
	cfg, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	cfg.Process.DebugAddr = "127.0.0.1:0" // config refuses port 0
	st, err := sqlite.Open(filepath.Join(dir, "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &fixture{t: t, fake: fake, st: st, cfg: cfg}
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fast are the test tunings: a short queue poll and pause delay.
func fast() tuning {
	return tuning{queuePoll: 20 * time.Millisecond, pausedDelay: 50 * time.Millisecond}
}

// build builds the app with the provider (nil: the one from the config).
func (f *fixture) build(p llm.Provider) *App {
	f.t.Helper()
	a, err := Build(bg, f.cfg, f.st, Options{Provider: p, Log: quietLog(), tune: fast()})
	if err != nil {
		f.t.Fatalf("Build: %v", err)
	}
	return a
}

// enroll enrols the fake's user through the app's own auth service, with the
// user's OIDC token, like the enrolment endpoint will.
func (f *fixture) enroll(a *App) {
	f.t.Helper()
	id := auth.Identity{UserID: userID, Username: login, DisplayName: "Einstein", DriveID: drive}
	if err := a.auth.Enroll(bg, id, ocisclient.Secret(fakeocis.OIDCToken(login))); err != nil {
		f.t.Fatalf("Enroll: %v", err)
	}
}

func (f *fixture) enqueue(a *App, goal string) string {
	f.t.Helper()
	j, err := a.queue.Enqueue(bg, store.Job{UserID: userID, TriggerKind: store.TriggerChat, Goal: goal})
	if err != nil {
		f.t.Fatal(err)
	}
	return j.ID
}

func (f *fixture) job(id string) store.Job {
	f.t.Helper()
	var j store.Job
	if err := f.st.View(bg, func(tx store.Tx) error {
		var err error
		j, err = tx.GetJob(userID, id)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return j
}

func (f *fixture) approvals(jobID string) []store.Approval {
	f.t.Helper()
	var out []store.Approval
	if err := f.st.View(bg, func(tx store.Tx) error {
		var err error
		out, err = tx.ListApprovals(userID, store.ApprovalFilter{JobID: jobID, Limit: 100})
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return out
}

// lastAnswer is the content of the last assistant message in the checkpoint.
func (f *fixture) lastAnswer(id string) string {
	f.t.Helper()
	cp, err := agent.ParseCheckpoint(f.job(id).Checkpoint)
	if err != nil {
		f.t.Fatal(err)
	}
	for i := len(cp.Messages) - 1; i >= 0; i-- {
		if m := cp.Messages[i]; m.Role == llm.RoleAssistant {
			return m.Content
		}
	}
	return ""
}

// waitState waits until the job is in state s.
func (f *fixture) waitState(id string, s store.JobState, within time.Duration) store.Job {
	f.t.Helper()
	deadline := time.Now().Add(within)
	for {
		j := f.job(id)
		if j.State == s {
			return j
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("job %s is %s (error %q), want %s", id, j.State, j.Error, s)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// running is an App started by start.
type running struct {
	a      *App
	cancel context.CancelFunc
	done   chan error
}

// start runs the app until stop and waits until every role is started.
func start(t *testing.T, a *App) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(bg)
	r := &running{a: a, cancel: cancel, done: make(chan error, 1)}
	go func() { r.done <- a.Run(ctx) }()
	select {
	case <-a.started:
	case err := <-r.done:
		cancel()
		t.Fatalf("Run returned before starting: %v", err)
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("the app did not start")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(60 * time.Second):
		}
	})
	return r
}

// stop cancels the Run context and waits for Run to return.
func (r *running) stop(t *testing.T, within time.Duration) error {
	t.Helper()
	r.cancel()
	select {
	case err := <-r.done:
		r.done <- err // for the cleanup
		return err
	case <-time.After(within):
		t.Fatalf("Run did not return within %s", within)
		return nil
	}
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	c.CloseIdleConnections()
	return resp.StatusCode, string(b)
}

// settledGoroutines waits until at most n goroutines run and returns the
// count it saw last (the goroutines of finished servers and connections end
// shortly after them).
func settledGoroutines(n int, within time.Duration) int {
	deadline := time.Now().Add(within)
	for {
		g := runtime.NumGoroutine()
		if g <= n || time.Now().After(deadline) {
			return g
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// blocking is a provider whose calls block until release or until their
// context ends; it records whether a call saw its context end.
type blocking struct {
	entered   chan struct{}
	release   chan llm.CompletionResponse
	cancelled chan struct{}
}

func newBlocking() *blocking {
	return &blocking{entered: make(chan struct{}, 10), release: make(chan llm.CompletionResponse, 1), cancelled: make(chan struct{}, 10)}
}

func (b *blocking) Complete(ctx context.Context, _ llm.CompletionRequest) (llm.CompletionResponse, error) {
	b.entered <- struct{}{}
	select {
	case r := <-b.release:
		return r, nil
	case <-ctx.Done():
		b.cancelled <- struct{}{}
		return llm.CompletionResponse{}, ctx.Err()
	}
}

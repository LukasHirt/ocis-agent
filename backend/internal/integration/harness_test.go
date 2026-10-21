//go:build integration

// The end-to-end test of the sidecar against the dev stack (docker compose in
// the repository root): real oCIS, a real OIDC login of the demo user, real
// app tokens. The sidecar runs as a child process with its own temporary data
// directory, port and master key, and a scripted fake model. The scenarios
// skip when the stack does not answer. Run: make integration-test.
//
// What the test may delete in oCIS is decided only by the rules in
// rules_test.go: tokens of this run's sidecar instance (its random label
// prefix, minus every label that existed before it started), files it
// created (ocis-agent-it-* at the drive root and the exact memory paths it
// recorded) and, while they are empty, memory folders that did not exist
// before the run. Folder entries are never purged from the trash.
package integration

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	demoUser     = "einstein"
	demoPassword = "relativity"
)

// The memory paths the sidecar's enrolment creates (memory.Service.Ensure).
var (
	memoryDirs = []string{"/.ocis-agent", "/.ocis-agent/memory", "/.ocis-agent/memory/notes"}
	readmePath = "/.ocis-agent/README.md"
)

var (
	activeMu sync.Mutex
	active   *harness
)

// TestMain runs the cleanup when the run is interrupted (Ctrl-C, SIGTERM):
// the testing package would exit without running t.Cleanup. A run that dies
// without it (the go test timeout, SIGKILL) is cleaned up by the next run
// from the state file (recoverOrphan).
func TestMain(m *testing.M) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		s := <-sig
		activeMu.Lock()
		h := active
		activeMu.Unlock()
		if h != nil {
			fmt.Fprintln(os.Stderr, "integration: interrupted, cleaning up")
			for _, err := range h.teardown(func(f string, a ...any) { fmt.Fprintf(os.Stderr, "integration: "+f+"\n", a...) }) {
				fmt.Fprintln(os.Stderr, "integration: cleanup:", err)
			}
		}
		code := 130
		if s == syscall.SIGTERM {
			code = 143
		}
		os.Exit(code)
	}()
	os.Exit(m.Run())
}

// stackURL returns the dev stack's URL: https://localhost:$OCIS_DEV_PORT, the
// port from the environment, else from the repository's .env, else 9200.
func stackURL() string {
	port := os.Getenv("OCIS_DEV_PORT")
	if port == "" {
		if b, err := os.ReadFile(filepath.Join("..", "..", "..", ".env")); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if v, ok := strings.CutPrefix(strings.TrimSpace(line), "OCIS_DEV_PORT="); ok {
					port = strings.Trim(strings.TrimSpace(v), `"'`)
				}
			}
		}
	}
	if port == "" {
		port = "9200"
	}
	return "https://localhost:" + port
}

// insecureClient trusts the dev stack's self-signed certificate. Test only.
func insecureClient() *http.Client {
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // dev certificate
	return &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// stackUp reports whether oCIS answers at base.
func stackUp(base string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := insecureClient()
	c.Timeout = 5 * time.Second
	var disc struct {
		Issuer string `json:"issuer"`
	}
	if err := getJSON(ctx, c, base+"/.well-known/openid-configuration", &disc); err != nil {
		return err
	}
	if disc.Issuer == "" {
		return errors.New("no OIDC discovery document")
	}
	return nil
}

// harness is the running sidecar plus the clients the scenarios use.
type harness struct {
	base  string // oCIS
	api   string // the sidecar, http://127.0.0.1:port
	debug string // the sidecar's debug listener
	http  *http.Client
	llm   *fakeLLM
	key   string // the master key: only compared with the logs

	procMu  sync.Mutex
	cmd     *exec.Cmd
	exited  chan struct{}
	exitErr error
	logs    *lockedBuffer

	tokMu   sync.Mutex
	tok     oidcToken
	bearers []string // every bearer issued in this run: redacted, checked against the logs

	driveID string
	// preexisting are the labels of the user's app tokens listed before the
	// sidecar started: never deleted
	preexisting map[string]bool

	mu sync.Mutex
	// state is what a later run needs to clean up after this one (written
	// to stateFile at every change)
	state   runState
	created map[string]bool // files the test created (exact paths)
	deleted map[string]bool // files the test deleted (exact paths): their trash entries are purged

	teardownOnce sync.Once
	teardownErrs []error
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// startHarness cleans up after an earlier run that died, records the state
// of the user's space and tokens, starts the fake model and the sidecar and
// logs in. t.Cleanup (or the signal handler) undoes it.
func startHarness(t *testing.T) *harness {
	t.Helper()
	base := stackURL()
	if err := stackUp(base); err != nil {
		t.Skipf("the dev stack does not answer at %s (%v): start it with `make dev-up` (or `docker compose up -d` in the repository root); set OCIS_DEV_PORT for another port", base, err)
	}
	h := &harness{
		base: base, http: insecureClient(), logs: &lockedBuffer{}, exited: make(chan struct{}),
		created: map[string]bool{}, deleted: map[string]bool{},
	}
	h.http.Timeout = 60 * time.Second
	h.state = runState{Started: time.Now().Unix(), TestPID: os.Getpid()}

	if _, err := h.bearer(); err != nil {
		t.Fatalf("OIDC login of %s: %v", demoUser, err)
	}
	drive, err := h.personalDrive()
	if err != nil {
		t.Fatalf("personal drive: %v", err)
	}
	h.driveID = drive

	h.recoverOrphan(t)

	// what exists before the run is never deleted: the memory folders and
	// README the sidecar's enrolment creates are removed afterwards only if
	// they were absent now (folders only while empty)
	for _, d := range memoryDirs {
		if h.exists(t, d) {
			continue
		}
		h.state.MemoryDirs = append(h.state.MemoryDirs, d)
	}
	if !h.exists(t, readmePath) {
		h.track(readmePath)
	}
	list, err := h.listTokens()
	if err != nil {
		t.Fatalf("list the user's app tokens: %v", err)
	}
	h.preexisting = map[string]bool{}
	for _, tk := range list {
		h.preexisting[tk.Label] = true
	}

	activeMu.Lock()
	active = h
	activeMu.Unlock()
	t.Cleanup(func() {
		h.stop(t)
		activeMu.Lock()
		active = nil
		activeMu.Unlock()
	})
	h.saveState(t.Logf)

	llm, err := newFakeLLM()
	if err != nil {
		t.Fatal(err)
	}
	h.llm = llm
	t.Cleanup(llm.Close)

	binDir := h.tempDir(t)
	bin := filepath.Join(binDir, "ocis-agent")
	build := exec.Command("go", "build", "-o", bin, "./cmd/ocis-agent")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the sidecar: %v\n%s", err, out)
	}
	h.mu.Lock()
	h.state.Binary = bin
	h.mu.Unlock()
	h.saveState(t.Logf)

	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		t.Fatal(err)
	}
	h.key = base64.StdEncoding.EncodeToString(keyBytes)
	dataDir := h.tempDir(t)

	// the free ports are found by listening and closing: another process can
	// take one before the sidecar binds it, so retry with new ports
	for attempt := 1; ; attempt++ {
		err := h.startSidecar(bin, dataDir)
		if err == nil {
			err = h.waitHealthy(30 * time.Second)
		}
		if err == nil {
			break
		}
		inUse := strings.Contains(h.logs.String(), "address already in use")
		h.kill()
		if !inUse || attempt == 3 {
			t.Fatalf("the sidecar did not become healthy: %v\n%s", err, h.logTail())
		}
	}
	return h
}

// tempDir makes a directory a later run recognises as ours (ownTempDir) and
// records it for the cleanup.
func (h *harness) tempDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp(os.TempDir(), testFilePrefix+"*")
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.state.Dirs = append(h.state.Dirs, d)
	h.mu.Unlock()
	h.saveState(t.Logf)
	return d
}

func (h *harness) startSidecar(bin, dataDir string) error {
	apiAddr, err := freeAddr()
	if err != nil {
		return err
	}
	debugAddr, err := freeAddr()
	if err != nil {
		return err
	}
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"TMPDIR=" + os.TempDir(),
		"AGENT_OCIS_URL=" + h.base,
		"AGENT_ALLOWED_ORIGIN=" + h.base,
		"AGENT_INSECURE=true",
		"AGENT_MASTER_KEY=" + h.key,
		"AGENT_DATA_DIR=" + dataDir,
		"AGENT_HTTP_ADDR=" + apiAddr,
		"AGENT_DEBUG_ADDR=" + debugAddr,
		"AGENT_ROLES=api,worker",
		"AGENT_LLM_BASE_URL=" + h.llm.BaseURL(),
		"AGENT_LLM_MODEL=fake-model",
		"AGENT_LLM_TIMEOUT=10s",
		"AGENT_WORKERS=2",
		"AGENT_LEASE_TTL=30s",
		"AGENT_SHUTDOWN_DRAIN=5s",
		"AGENT_TOKEN_SWEEP_INTERVAL=1m",
		"AGENT_API_RATE=50",
		"AGENT_API_BURST=200",
		"AGENT_LOG_LEVEL=debug",
	}
	cmd := exec.Command(bin)
	cmd.Env = env
	logs := &lockedBuffer{}
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the sidecar: %w", err)
	}
	exited := make(chan struct{})
	h.procMu.Lock()
	h.cmd, h.exited, h.logs, h.exitErr = cmd, exited, logs, nil
	h.procMu.Unlock()
	h.api, h.debug = "http://"+apiAddr, "http://"+debugAddr
	go func() {
		err := cmd.Wait()
		h.procMu.Lock()
		h.exitErr = err
		h.procMu.Unlock()
		close(exited)
	}()
	h.mu.Lock()
	h.state.SidecarPID = cmd.Process.Pid
	h.mu.Unlock()
	h.saveState(func(string, ...any) {})
	return nil
}

// kill ends the sidecar at once (a failed start).
func (h *harness) kill() {
	h.procMu.Lock()
	cmd, exited := h.cmd, h.exited
	h.procMu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		<-exited
	}
}

func freeAddr() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer ln.Close()
	return ln.Addr().String(), nil
}

func (h *harness) waitHealthy(d time.Duration) error {
	deadline := time.Now().Add(d)
	h.procMu.Lock()
	exited := h.exited
	h.procMu.Unlock()
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			return errors.New("the sidecar exited")
		default:
		}
		resp, err := h.http.Get(h.api + "/agent-api/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				r2, err := h.http.Get(h.debug + "/readyz")
				if err == nil {
					r2.Body.Close()
					if r2.StatusCode == http.StatusOK {
						return nil
					}
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return errors.New("timeout")
}

// stop checks the sidecar's shutdown (SIGTERM: exit 0, "app: stopped", the
// open event stream ends with bye/shutdown, no secret in the log), then runs
// the teardown.
func (h *harness) stop(t *testing.T) {
	t.Helper()
	stream, streamErr := h.openEvents(context.Background())
	h.procMu.Lock()
	cmd, exited := h.cmd, h.exited
	h.procMu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(45 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
			t.Errorf("the sidecar did not stop within 45s of SIGTERM")
		}
		h.procMu.Lock()
		exitErr := h.exitErr
		h.procMu.Unlock()
		if exitErr != nil {
			t.Errorf("the sidecar did not exit cleanly: %v\n%s", exitErr, h.logTail())
		}
		logs := h.logs.String()
		if !strings.Contains(logs, "app: stopped") {
			t.Errorf("the sidecar's log has no clean stop\n%s", h.logTail())
		}
		if streamErr != nil {
			t.Errorf("open an event stream before the shutdown: %v", streamErr)
		} else {
			if !stream.waitFor("bye", `{"reason":"shutdown"}`, 10*time.Second) {
				t.Errorf("the event stream did not end with bye/shutdown: %v", stream.frames())
			}
			stream.close()
		}
		if strings.Contains(logs, h.key) {
			t.Errorf("the sidecar logged the master key")
		}
		for _, b := range h.allBearers() {
			if strings.Contains(logs, b) {
				t.Errorf("the sidecar logged a bearer")
				break
			}
		}
		// the app token values never reach the test, so they cannot be
		// checked here; the sidecar's own tests cover them
	}
	for _, err := range h.teardown(t.Logf) {
		t.Error(err)
	}
}

// teardown stops the sidecar (if still running), deletes this run's app
// tokens and files, purges their trash entries, removes the memory folders
// the run created while they are empty, removes the temporary directories and
// the state file. It runs once; later calls return the first result.
func (h *harness) teardown(logf func(string, ...any)) []error {
	h.teardownOnce.Do(func() {
		h.procMu.Lock()
		cmd, exited := h.cmd, h.exited
		h.procMu.Unlock()
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-exited:
			case <-time.After(30 * time.Second):
				_ = cmd.Process.Kill()
				<-exited
			}
		}
		var errs []error
		if n, err := h.deleteOurTokens(); err != nil {
			errs = append(errs, fmt.Errorf("delete the sidecar's app tokens: %w", err))
		} else if n > 0 {
			logf("deleted %d app token(s) the sidecar left behind", n)
		}
		h.mu.Lock()
		files := sortedKeys(h.created)
		dirs := append([]string(nil), h.state.MemoryDirs...)
		temps := append([]string(nil), h.state.Dirs...)
		h.mu.Unlock()
		for _, p := range files {
			if st, err := h.remove(p); err != nil || (st != http.StatusNoContent && st != http.StatusNotFound) {
				errs = append(errs, fmt.Errorf("delete %s: %d %v", p, st, err))
			}
		}
		errs = append(errs, h.removeEmptyDirs(dirs, logf)...)
		if n, err := h.purgeTrash(); err != nil {
			errs = append(errs, fmt.Errorf("purge the test's files from the trash: %w", err))
		} else if n > 0 {
			logf("purged %d file(s) the test deleted from the trash", n)
		}
		for _, d := range temps {
			if ownTempDir(d) {
				_ = os.RemoveAll(d)
			}
		}
		if len(errs) == 0 {
			_ = os.Remove(stateFile())
		} else {
			logf("the state file %s stays for the next run's cleanup", stateFile())
		}
		h.teardownErrs = errs
	})
	return h.teardownErrs
}

// removeEmptyDirs deletes the given memory folders, deepest first, each only
// if it lists nothing (a folder entry stays in the trash).
func (h *harness) removeEmptyDirs(dirs []string, logf func(string, ...any)) []error {
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	var errs []error
	for _, d := range dirs {
		st, body, err := h.dav("PROPFIND", d, nil, map[string]string{"Depth": "1"})
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("list %s: %w", d, err))
		case st == http.StatusNotFound:
		case st != http.StatusMultiStatus:
			errs = append(errs, fmt.Errorf("list %s: %d", d, st))
		case !folderIsEmpty(string(body)):
			logf("%s is not empty: left in place", d)
		default:
			if st, _, err := h.dav(http.MethodDelete, d, nil, nil); err != nil || (st != http.StatusNoContent && st != http.StatusNotFound) {
				errs = append(errs, fmt.Errorf("delete the empty folder %s: %d %v", d, st, err))
			}
		}
	}
	return errs
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// track records a file the test creates (before it is created, so a crash in
// between still leaves it in the state file).
func (h *harness) track(p string) {
	if !deletablePath(p, map[string]bool{p: true}) {
		panic("integration: the test never creates " + p)
	}
	h.mu.Lock()
	h.created[p] = true
	h.state.Files = sortedKeys(h.created)
	h.mu.Unlock()
	h.saveState(func(string, ...any) {})
}

// remove deletes a file the rules allow (deletablePath) and remembers it for
// the trash purge. Anything else is refused.
func (h *harness) remove(p string) (int, error) {
	h.mu.Lock()
	ok := deletablePath(p, h.created)
	h.mu.Unlock()
	if !ok {
		return 0, fmt.Errorf("refusing to delete %s: not a file this test created", p)
	}
	st, _, err := h.dav(http.MethodDelete, p, nil, nil)
	if err == nil && st == http.StatusNoContent {
		h.mu.Lock()
		h.deleted[p] = true
		h.mu.Unlock()
	}
	return st, err
}

// purgeTrash removes from the trash the files this run deleted (exact
// locations, deleted since the run started); never a folder entry.
func (h *harness) purgeTrash() (int, error) {
	h.mu.Lock()
	files := make(map[string]bool, len(h.deleted))
	for k := range h.deleted {
		files[k] = true
	}
	since := h.state.Started
	h.mu.Unlock()
	if len(files) == 0 {
		return 0, nil
	}
	trash := "/remote.php/dav/spaces/trash-bin/" + url.PathEscape(h.driveID)
	st, body, err := h.ocis("PROPFIND", trash, nil, map[string]string{"Depth": "1"})
	if err != nil {
		return 0, err
	}
	if st != http.StatusMultiStatus {
		return 0, fmt.Errorf("PROPFIND of the trash = %d", st)
	}
	n := 0
	for _, key := range trashToPurge(string(body), files, since) {
		if st, _, err := h.ocis(http.MethodDelete, trash+"/"+url.PathEscape(key), nil, nil); err != nil || st != http.StatusNoContent {
			return n, fmt.Errorf("purge a trash item: %d %v", st, err)
		}
		n++
	}
	return n, nil
}

// exists reports whether p exists in the personal drive; anything but 207 or
// 404 fails the test (an unknown state must never count as "absent").
func (h *harness) exists(t *testing.T, p string) bool {
	t.Helper()
	st, _, err := h.dav("PROPFIND", p, nil, map[string]string{"Depth": "0"})
	if err != nil {
		t.Fatalf("PROPFIND %s: %v", p, err)
	}
	switch st {
	case http.StatusMultiStatus:
		return true
	case http.StatusNotFound:
		return false
	}
	t.Fatalf("PROPFIND %s = %d: cannot tell whether it exists", p, st)
	return false
}

func (h *harness) saveState(logf func(string, ...any)) {
	h.mu.Lock()
	st := h.state
	h.mu.Unlock()
	if err := writeState(stateFile(), st); err != nil {
		logf("write the state file: %v", err)
	}
}

// recoverOrphan cleans up after an earlier run that died without its
// cleanup, from its state file: it stops that run's sidecar if it still runs
// (the same binary path), deletes the app tokens with that run's exact
// instance prefix and the files it recorded, removes the memory folders it
// created while they are empty and its temporary directories. A state file of
// a run that is still active fails the test.
func (h *harness) recoverOrphan(t *testing.T) {
	t.Helper()
	st, ok, err := readState(stateFile())
	if err != nil {
		t.Fatalf("read the state file %s: %v (delete it after checking that no run is active)", stateFile(), err)
	}
	if !ok {
		return
	}
	if st.TestPID != os.Getpid() && processRuns(st.TestPID, ".test") {
		t.Fatalf("another integration run (pid %d) seems active; if not, delete %s", st.TestPID, stateFile())
	}
	t.Logf("cleaning up after an earlier run that did not finish (state %s)", stateFile())
	if st.SidecarPID > 0 && st.Binary != "" && processRuns(st.SidecarPID, st.Binary) {
		if p, err := os.FindProcess(st.SidecarPID); err == nil {
			_ = p.Signal(syscall.SIGTERM)
			if !eventually(15*time.Second, func() bool { return !processRuns(st.SidecarPID, st.Binary) }) {
				_ = p.Kill()
			}
		}
	}
	if validPrefix(st.Prefix) {
		list, err := h.listTokens()
		if err != nil {
			t.Fatalf("list the user's app tokens: %v", err)
		}
		for _, tk := range tokensToDelete(list, st.Prefix, nil) {
			_, _, _ = h.ocis(http.MethodDelete, "/auth-app/tokens?token="+url.QueryEscape(tk.Hash), nil, nil)
		}
		if list, err = h.listTokens(); err != nil || len(tokensToDelete(list, st.Prefix, nil)) > 0 {
			t.Fatalf("the earlier run's app tokens could not be deleted (%v)", err)
		}
	}
	h.mu.Lock()
	for _, p := range st.Files {
		h.created[p] = true // the recorded paths of that run
	}
	h.state.Started = min(h.state.Started, st.Started)
	h.mu.Unlock()
	for _, p := range st.Files {
		if s, err := h.remove(p); err != nil || (s != http.StatusNoContent && s != http.StatusNotFound) {
			t.Fatalf("delete %s of the earlier run: %d %v", p, s, err)
		}
	}
	for _, err := range h.removeEmptyDirs(append([]string(nil), st.MemoryDirs...), t.Logf) {
		t.Fatal(err)
	}
	if _, err := h.purgeTrash(); err != nil {
		t.Fatal(err)
	}
	for _, d := range st.Dirs {
		if ownTempDir(d) {
			_ = os.RemoveAll(d)
		}
	}
	h.mu.Lock()
	h.created, h.deleted = map[string]bool{}, map[string]bool{}
	h.state.Started = time.Now().Unix()
	h.mu.Unlock()
	if err := os.Remove(stateFile()); err != nil {
		t.Fatal(err)
	}
}

// processRuns reports whether pid runs a command line containing want.
func processRuns(pid int, want string) bool {
	if pid <= 0 {
		return false
	}
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && strings.Contains(string(out), want)
}

func (h *harness) logTail() string {
	lines := strings.Split(strings.TrimRight(h.logs.String(), "\n"), "\n")
	if len(lines) > 60 {
		lines = lines[len(lines)-60:]
	}
	out := strings.Join(lines, "\n")
	for _, b := range h.allBearers() {
		out = strings.ReplaceAll(out, b, "[redacted]")
	}
	return strings.ReplaceAll(out, h.key, "[redacted]")
}

func (h *harness) allBearers() []string {
	h.tokMu.Lock()
	defer h.tokMu.Unlock()
	return append([]string(nil), h.bearers...)
}

// bearer returns a valid access token of the demo user, logging in again
// shortly before the current one expires.
func (h *harness) bearer() (string, error) {
	h.tokMu.Lock()
	defer h.tokMu.Unlock()
	if h.tok.value != "" && time.Until(h.tok.expires) > 90*time.Second {
		return h.tok.value, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tok, err := idpLogin{base: h.base, user: demoUser, password: demoPassword, client: h.http}.login(ctx)
	if err != nil {
		return "", err
	}
	h.tok = tok
	h.bearers = append(h.bearers, tok.value)
	return tok.value, nil
}

// --- the sidecar's API ---

type apiResp struct {
	status int
	header http.Header
	body   []byte
}

// errorCode is the error.code of an error response.
func (r apiResp) errorCode() string {
	var doc struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(r.body, &doc)
	return doc.Error.Code
}

func (r apiResp) String() string {
	b := r.body
	if len(b) > 400 {
		b = b[:400]
	}
	return fmt.Sprintf("%d %s", r.status, b)
}

// call sends a request to the sidecar as the browser does: the user's bearer
// and Origin = the oCIS URL. body is JSON-encoded unless it is nil. The
// incident revoke (all of the user's tokens) and an unenrolment that deletes
// the memory are refused: the account is shared with other clients.
func (h *harness) call(t *testing.T, method, path string, body any, hdr map[string]string) apiResp {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			t.Fatal(err)
		}
	}
	if (path == "/token/revoke-all" && method != http.MethodGet) ||
		(path == "/unenroll" && strings.Contains(string(raw), "delete_memory")) {
		t.Fatalf("the integration test must not call %s %s", method, path)
	}
	bearer, err := h.bearer()
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	req, err := http.NewRequest(method, h.api+"/agent-api/v1"+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Origin", h.base)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := h.http.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		t.Fatalf("%s %s: read: %v", method, path, err)
	}
	return apiResp{status: resp.StatusCode, header: resp.Header, body: b}
}

// callRetry is call that waits out a 429 (cooldowns between enrolments,
// renewals and sent messages) up to three times.
func (h *harness) callRetry(t *testing.T, method, path string, body any) apiResp {
	t.Helper()
	for i := 0; ; i++ {
		r := h.call(t, method, path, body, nil)
		if r.status != http.StatusTooManyRequests || i == 3 {
			return r
		}
		wait, _ := strconv.Atoi(r.header.Get("Retry-After"))
		if wait < 1 || wait > 30 {
			wait = 1
		}
		time.Sleep(time.Duration(wait)*time.Second + 100*time.Millisecond)
	}
}

// expect calls and checks the status, decoding a JSON body into dst.
func (h *harness) expect(t *testing.T, want int, method, path string, body any, dst any) apiResp {
	t.Helper()
	r := h.callRetry(t, method, path, body)
	if r.status != want {
		t.Fatalf("%s %s = %s, want %d", method, path, r, want)
	}
	if dst != nil {
		if err := json.Unmarshal(r.body, dst); err != nil {
			t.Fatalf("%s %s: decode: %v", method, path, err)
		}
	}
	return r
}

// --- oCIS, as the user (with the bearer, never through the sidecar) ---

// ocis sends a request to oCIS. 425 (a fresh upload still postprocessed) and
// 423 (locked by that processing) are waited out for about ten seconds.
func (h *harness) ocis(method, path string, body []byte, hdr map[string]string) (int, []byte, error) {
	for attempt := 1; ; attempt++ {
		st, b, err := h.ocisOnce(method, path, body, hdr)
		if err != nil || (st != http.StatusTooEarly && st != http.StatusLocked) || attempt == 12 {
			return st, b, err
		}
		time.Sleep(time.Duration(min(attempt, 5)) * 200 * time.Millisecond)
	}
}

func (h *harness) ocisOnce(method, path string, body []byte, hdr map[string]string) (int, []byte, error) {
	bearer, err := h.bearer()
	if err != nil {
		return 0, nil, err
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, h.base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := h.http.Do(req)
	if err != nil {
		// a *url.Error holds the whole URL, whose query can carry a token hash
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, nil, fmt.Errorf("%s %s: %w", method, req.URL.Path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return resp.StatusCode, b, err
}

// dav sends a WebDAV request to a path of the personal drive.
func (h *harness) dav(method, p string, body []byte, hdr map[string]string) (int, []byte, error) {
	var segs []string
	for _, s := range strings.Split(strings.Trim(p, "/"), "/") {
		segs = append(segs, url.PathEscape(s))
	}
	return h.ocis(method, "/remote.php/dav/spaces/"+url.PathEscape(h.driveID)+"/"+strings.Join(segs, "/"), body, hdr)
}

func (h *harness) personalDrive() (string, error) {
	st, b, err := h.ocis(http.MethodGet, "/graph/v1.0/me/drives", nil, nil)
	if err != nil {
		return "", err
	}
	if st != http.StatusOK {
		return "", fmt.Errorf("GET /me/drives = %d", st)
	}
	var out struct {
		Value []struct {
			ID        string `json:"id"`
			DriveType string `json:"driveType"`
		} `json:"value"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return "", err
	}
	for _, d := range out.Value {
		if d.DriveType == "personal" {
			return d.ID, nil
		}
	}
	return "", errors.New("no personal drive")
}

func (h *harness) listTokens() ([]appToken, error) {
	st, b, err := h.ocis(http.MethodGet, "/auth-app/tokens", nil, nil)
	if err != nil {
		return nil, err
	}
	if st != http.StatusOK {
		return nil, fmt.Errorf("GET /auth-app/tokens = %d", st)
	}
	var list []appToken
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, errors.New("GET /auth-app/tokens: undecodable list")
	}
	return list, nil
}

// ourLabels lists the labels of this run's sidecar tokens in oCIS.
func (h *harness) ourLabels() ([]string, error) {
	prefix := h.labelPrefix()
	if prefix == "" {
		return nil, errors.New("the sidecar's label prefix is not known yet")
	}
	list, err := h.listTokens()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, tk := range tokensToDelete(list, prefix, h.preexisting) {
		out = append(out, tk.Label)
	}
	return out, nil
}

// deleteOurTokens deletes this run's sidecar tokens directly in oCIS (only
// what tokensToDelete selects).
func (h *harness) deleteOurTokens() (int, error) {
	prefix := h.labelPrefix()
	if prefix == "" {
		return 0, nil // never enrolled
	}
	list, err := h.listTokens()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, tk := range tokensToDelete(list, prefix, h.preexisting) {
		// oCIS answers 500 for an unknown hash: confirm through the list
		_, _, _ = h.ocis(http.MethodDelete, "/auth-app/tokens?token="+url.QueryEscape(tk.Hash), nil, nil)
		n++
	}
	left, err := h.ourLabels()
	if err != nil {
		return n, err
	}
	if len(left) > 0 {
		return n, fmt.Errorf("%d token(s) still listed after the delete", len(left))
	}
	return n, nil
}

func (h *harness) labelPrefix() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.state.Prefix
}

// learnPrefix finds this run's sidecar instance prefix after its first
// enrolment: the revoke-all PREVIEW (a read; the revoke itself is never
// called) marks the sidecar's own tokens. The prefix must have the exact
// instance shape, its token must be listed by oCIS and no label that existed
// before the sidecar started may carry it.
func (h *harness) learnPrefix(t *testing.T) {
	t.Helper()
	var pv struct {
		Tokens []struct {
			Label string `json:"label"`
			Ours  bool   `json:"ours"`
		} `json:"tokens"`
	}
	h.expect(t, http.StatusOK, http.MethodGet, "/token/revoke-all/preview", nil, &pv)
	var prefix string
	for _, tk := range pv.Tokens {
		if !tk.Ours {
			continue
		}
		p := tk.Label[:strings.LastIndex(tk.Label, "-")+1]
		if !validPrefix(p) || (prefix != "" && p != prefix) {
			t.Fatalf("the sidecar's own token labels have an unexpected shape")
		}
		prefix = p
	}
	if prefix == "" {
		t.Fatalf("no own token in the revoke-all preview after enrolment")
	}
	if prefixCollides(prefix, h.preexisting) {
		t.Fatalf("a token that existed before the sidecar started carries its label prefix: refusing to manage tokens by it")
	}
	h.mu.Lock()
	h.state.Prefix = prefix
	h.mu.Unlock()
	h.saveState(t.Logf)
}

// --- the event stream ---

type eventStream struct {
	cancel context.CancelFunc
	mu     sync.Mutex
	got    []string // "event data"
	done   chan struct{}
}

// openEvents connects to GET /events and collects frames in the background.
func (h *harness) openEvents(ctx context.Context) (*eventStream, error) {
	bearer, err := h.bearer()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.api+"/agent-api/v1/events", nil)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Origin", h.base)
	c := &http.Client{Transport: h.http.Transport} // no timeout: a stream
	resp, err := c.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("GET /events = %d", resp.StatusCode)
	}
	s := &eventStream{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		var ev, data string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if ev != "" {
					s.mu.Lock()
					s.got = append(s.got, ev+" "+data)
					s.mu.Unlock()
				}
				ev, data = "", ""
			case strings.HasPrefix(line, "event: "):
				ev = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return s, nil
}

func (s *eventStream) frames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.got...)
}

// waitFor waits for a frame of the event whose data contains sub.
func (s *eventStream) waitFor(event, sub string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		for _, f := range s.frames() {
			if ev, data, _ := strings.Cut(f, " "); ev == event && strings.Contains(data, sub) {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-s.done:
			// the stream ended: one last look at what it delivered
			deadline = time.Now()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (s *eventStream) close() {
	s.cancel()
	<-s.done
}

// eventually polls f until it reports true or d passes.
func eventually(d time.Duration, f func() bool) bool {
	deadline := time.Now().Add(d)
	for {
		if f() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// consistently polls f for d and reports false as soon as it does.
func consistently(d time.Duration, f func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !f() {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
	return f()
}

//go:build integration

// The end-to-end test of the sidecar against the dev stack (docker compose in
// the repository root): real oCIS, a real OIDC login of the demo user, real
// app tokens. The sidecar runs as a child process with its own temporary data
// directory, port and master key, and a scripted fake model. The scenarios
// skip when the stack does not answer. Run: make integration-test.
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
	"path"
	"path/filepath"
	"regexp"
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
	labelSpace   = "ocis-agent-"
)

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
	base    string // oCIS
	api     string // the sidecar, http://127.0.0.1:port
	debug   string // the sidecar's debug listener
	http    *http.Client
	llm     *fakeLLM
	cmd     *exec.Cmd
	exited  chan struct{}
	exitErr error
	logs    *lockedBuffer
	key     string // the master key: only compared with the logs

	tokMu sync.Mutex
	tok   oidcToken

	driveID string
	// prefix is this sidecar's label prefix (ocis-agent-<instance>-), known
	// after the first enrolment
	prefixMu sync.Mutex
	prefix   string
	// agentDirExisted: /.ocis-agent was there before the test, so the cleanup
	// removes only what the test created in it
	agentDirExisted bool
	// started and deleted select the trash entries the cleanup purges: the
	// paths the test deleted, deleted since it started
	started   time.Time
	deletedMu sync.Mutex
	deleted   map[string]bool
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

// startHarness starts the fake model and the sidecar, logs in and records the
// state of the user's space. Everything is undone by t.Cleanup.
func startHarness(t *testing.T) *harness {
	t.Helper()
	base := stackURL()
	if err := stackUp(base); err != nil {
		t.Skipf("the dev stack does not answer at %s (%v): start it with `make dev-up` (or `docker compose up -d` in the repository root); set OCIS_DEV_PORT for another port", base, err)
	}
	h := &harness{
		base: base, http: insecureClient(), logs: &lockedBuffer{}, exited: make(chan struct{}),
		started: time.Now(), deleted: map[string]bool{},
	}
	h.http.Timeout = 60 * time.Second

	if _, err := h.bearer(); err != nil {
		t.Fatalf("OIDC login of %s: %v", demoUser, err)
	}
	drive, err := h.personalDrive()
	if err != nil {
		t.Fatalf("personal drive: %v", err)
	}
	h.driveID = drive
	st, _, err := h.dav("PROPFIND", "/.ocis-agent", nil, map[string]string{"Depth": "0"})
	if err != nil {
		t.Fatal(err)
	}
	h.agentDirExisted = st == http.StatusMultiStatus

	llm, err := newFakeLLM()
	if err != nil {
		t.Fatal(err)
	}
	h.llm = llm
	t.Cleanup(llm.Close)

	bin := filepath.Join(t.TempDir(), "ocis-agent")
	build := exec.Command("go", "build", "-o", bin, "./cmd/ocis-agent")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the sidecar: %v\n%s", err, out)
	}

	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		t.Fatal(err)
	}
	h.key = base64.StdEncoding.EncodeToString(keyBytes)
	apiAddr, debugAddr := freeAddr(t), freeAddr(t)
	h.api, h.debug = "http://"+apiAddr, "http://"+debugAddr
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"TMPDIR=" + os.TempDir(),
		"AGENT_OCIS_URL=" + base,
		"AGENT_ALLOWED_ORIGIN=" + base,
		"AGENT_INSECURE=true",
		"AGENT_MASTER_KEY=" + h.key,
		"AGENT_DATA_DIR=" + t.TempDir(),
		"AGENT_HTTP_ADDR=" + apiAddr,
		"AGENT_DEBUG_ADDR=" + debugAddr,
		"AGENT_ROLES=api,worker",
		"AGENT_LLM_BASE_URL=" + llm.BaseURL(),
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
	h.cmd = exec.Command(bin)
	h.cmd.Env = env
	h.cmd.Stdout, h.cmd.Stderr = h.logs, h.logs
	if err := h.cmd.Start(); err != nil {
		t.Fatalf("start the sidecar: %v", err)
	}
	go func() {
		h.exitErr = h.cmd.Wait()
		close(h.exited)
	}()
	t.Cleanup(func() { h.stop(t) })
	if err := h.waitHealthy(30 * time.Second); err != nil {
		t.Fatalf("the sidecar did not become healthy: %v\n%s", err, h.logTail())
	}
	return h
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func (h *harness) waitHealthy(d time.Duration) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		select {
		case <-h.exited:
			return fmt.Errorf("the sidecar exited: %v", h.exitErr)
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

// stop ends the sidecar with SIGTERM and checks that it shut down cleanly
// and never logged a secret; then it removes what the test left in oCIS.
func (h *harness) stop(t *testing.T) {
	t.Helper()
	// an open event stream must end with bye/shutdown
	stream, streamErr := h.openEvents(context.Background())
	if h.cmd.Process != nil {
		_ = h.cmd.Process.Signal(syscall.SIGTERM)
	}
	select {
	case <-h.exited:
	case <-time.After(45 * time.Second):
		_ = h.cmd.Process.Kill()
		<-h.exited
		t.Errorf("the sidecar did not stop within 45s of SIGTERM")
	}
	if h.exitErr != nil {
		t.Errorf("the sidecar did not exit cleanly: %v\n%s", h.exitErr, h.logTail())
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
	h.tokMu.Lock()
	bearer := h.tok.value
	h.tokMu.Unlock()
	if strings.Contains(logs, h.key) || (bearer != "" && strings.Contains(logs, bearer)) {
		t.Errorf("the sidecar logged the master key or the bearer")
	}
	h.cleanupOCIS(t)
}

// cleanupOCIS deletes every app token of this sidecar and what the test
// created in the user's space.
func (h *harness) cleanupOCIS(t *testing.T) {
	t.Helper()
	if n, err := h.deleteOurTokens(); err != nil {
		t.Errorf("delete the sidecar's app tokens: %v", err)
	} else if n > 0 {
		t.Logf("deleted %d app token(s) the sidecar left behind", n)
	}
	if !h.agentDirExisted {
		if st, err := h.remove("/.ocis-agent"); err != nil || (st != http.StatusNoContent && st != http.StatusNotFound) {
			t.Errorf("delete /.ocis-agent: %d %v", st, err)
		}
	}
	if n, err := h.purgeTrash(); err != nil {
		t.Errorf("purge the test's items from the trash: %v", err)
	} else {
		t.Logf("purged %d item(s) the test deleted from the trash", n)
	}
}

// remove deletes a path of the personal drive and remembers it for the
// trash purge.
func (h *harness) remove(p string) (int, error) {
	st, _, err := h.dav(http.MethodDelete, p, nil, nil)
	if err == nil && st == http.StatusNoContent {
		h.deletedMu.Lock()
		h.deleted[strings.Trim(p, "/")] = true
		h.deletedMu.Unlock()
	}
	return st, err
}

var (
	trashEntryRE = regexp.MustCompile(`(?s)<d:response>(.*?)</d:response>`)
	trashHrefRE  = regexp.MustCompile(`<d:href>([^<]*)</d:href>`)
	trashLocRE   = regexp.MustCompile(`<oc:trashbin-original-location>([^<]*)</oc:trashbin-original-location>`)
	trashTSRE    = regexp.MustCompile(`<oc:trashbin-delete-timestamp>([0-9]+)</oc:trashbin-delete-timestamp>`)
)

// purgeTrash removes from the trash what the test deleted (the paths it
// removed, or anything below them, deleted since the test started), so a run
// leaves the user's trash as it found it.
func (h *harness) purgeTrash() (int, error) {
	h.deletedMu.Lock()
	deleted := make(map[string]bool, len(h.deleted))
	for k := range h.deleted {
		deleted[k] = true
	}
	h.deletedMu.Unlock()
	if len(deleted) == 0 {
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
	since := h.started.Add(-5 * time.Second).Unix()
	n := 0
	for _, m := range trashEntryRE.FindAllStringSubmatch(string(body), -1) {
		href, loc, ts := trashHrefRE.FindStringSubmatch(m[1]), trashLocRE.FindStringSubmatch(m[1]), trashTSRE.FindStringSubmatch(m[1])
		if href == nil || loc == nil || ts == nil {
			continue
		}
		when, _ := strconv.ParseInt(ts[1], 10, 64)
		if when < since || !ours(deleted, loc[1]) {
			continue
		}
		key := href[1][strings.LastIndex(strings.TrimRight(href[1], "/"), "/")+1:]
		if st, _, err := h.ocis(http.MethodDelete, trash+"/"+key, nil, nil); err != nil || st != http.StatusNoContent {
			return n, fmt.Errorf("purge a trash item: %d %v", st, err)
		}
		n++
	}
	return n, nil
}

// ours reports whether loc is a deleted path or below one.
func ours(deleted map[string]bool, loc string) bool {
	for p := loc; p != "" && p != "."; p = path.Dir(p) {
		if deleted[p] {
			return true
		}
		if !strings.Contains(p, "/") {
			break
		}
	}
	return false
}

func (h *harness) logTail() string {
	lines := strings.Split(strings.TrimRight(h.logs.String(), "\n"), "\n")
	if len(lines) > 60 {
		lines = lines[len(lines)-60:]
	}
	out := strings.Join(lines, "\n")
	h.tokMu.Lock()
	if h.tok.value != "" {
		out = strings.ReplaceAll(out, h.tok.value, "[redacted]")
	}
	h.tokMu.Unlock()
	return strings.ReplaceAll(out, h.key, "[redacted]")
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
// and Origin = the oCIS URL. body is JSON-encoded unless it is nil.
func (h *harness) call(t *testing.T, method, path string, body any, hdr map[string]string) apiResp {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	bearer, err := h.bearer()
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	req, err := http.NewRequest(method, h.api+"/agent-api/v1"+path, rd)
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

func (h *harness) ocis(method, path string, body []byte, hdr map[string]string) (int, []byte, error) {
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

// appToken is an entry of GET /auth-app/tokens. Hash is the listed token
// field (the hash the delete needs); it is never printed.
type appToken struct {
	Hash    string    `json:"token"`
	Label   string    `json:"label"`
	Created time.Time `json:"created_date"`
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

// ourLabels lists the labels of this sidecar's app tokens in oCIS.
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
	for _, tk := range list {
		if strings.HasPrefix(tk.Label, prefix) {
			out = append(out, tk.Label)
		}
	}
	return out, nil
}

// deleteOurTokens deletes this sidecar's app tokens directly in oCIS.
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
	for _, tk := range list {
		if !strings.HasPrefix(tk.Label, prefix) {
			continue
		}
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
	h.prefixMu.Lock()
	defer h.prefixMu.Unlock()
	return h.prefix
}

// learnPrefix reads this sidecar's label prefix from the revoke-all preview
// (the entries it marks as its own) after an enrolment.
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
		i := strings.LastIndex(tk.Label, "-")
		if !strings.HasPrefix(tk.Label, labelSpace) || i < len(labelSpace) {
			t.Fatalf("an own token label has an unexpected shape")
		}
		p := tk.Label[:i+1]
		if prefix != "" && p != prefix {
			t.Fatalf("own token labels with two prefixes")
		}
		prefix = p
	}
	if prefix == "" {
		t.Fatalf("no own token in the revoke-all preview after enrolment")
	}
	h.prefixMu.Lock()
	h.prefix = prefix
	h.prefixMu.Unlock()
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

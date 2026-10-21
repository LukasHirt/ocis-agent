package api

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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
	auditPath  = "/agent-api/v1/audit"
	verifyPath = "/agent-api/v1/audit/verify"
	exportPath = "/agent-api/v1/audit/export"
)

// seedAudit appends n events for the user through the real store (the real
// hash chain). mod may set fields per event (i counts from 0).
func seedAudit(t *testing.T, st store.Store, user string, n int, mod func(i int, e *audit.Event)) {
	t.Helper()
	base := time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)
	err := st.InTx(bg, func(tx store.Tx) error {
		for i := range n {
			e := audit.Event{UserID: user, TS: base.Add(time.Duration(i) * time.Millisecond), Kind: "tool", Cred: "app", Outcome: "ok"}
			if mod != nil {
				mod(i, &e)
			}
			if _, err := tx.AppendAudit(e); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

type auditList struct {
	Events     []map[string]any `json:"events"`
	NextCursor *string          `json:"next_cursor"`
}

func getAudit(t *testing.T, f *fixture, query string) auditList {
	t.Helper()
	rec := f.do(http.MethodGet, auditPath+query, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET audit%s = %d %s", query, rec.Code, rec.Body.String())
	}
	var l auditList
	if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil {
		t.Fatal(err)
	}
	return l
}

func seqs(l auditList) []int64 {
	var out []int64
	for _, e := range l.Events {
		out = append(out, int64(e["seq"].(float64)))
	}
	return out
}

func TestAuditPaginatesAcrossTheWholeLog(t *testing.T) {
	f := newFixture(t)
	seedAudit(t, f.st, aliceID.UserID, 1200, nil)
	var got []int64
	cursor := ""
	pages := 0
	for {
		l := getAudit(t, f, "?limit=200&cursor="+cursor)
		pages++
		got = append(got, seqs(l)...)
		if l.NextCursor == nil {
			break
		}
		if want := strconv.FormatInt(got[len(got)-1], 10); *l.NextCursor != want {
			t.Fatalf("next_cursor = %s, want the last seq %s", *l.NextCursor, want)
		}
		cursor = *l.NextCursor
	}
	if pages != 6 || len(got) != 1200 {
		t.Fatalf("pages=%d events=%d", pages, len(got))
	}
	for i, s := range got {
		if s != int64(i+1) {
			t.Fatalf("event %d has seq %d", i, s)
		}
	}
	// default limit
	if l := getAudit(t, f, ""); len(l.Events) != 50 || l.NextCursor == nil || *l.NextCursor != "50" {
		t.Fatalf("default page: %d events, cursor %v", len(l.Events), l.NextCursor)
	}
	// exact fit: the last page has no next cursor
	if l := getAudit(t, f, "?limit=200&cursor=1000"); len(l.Events) != 200 || l.NextCursor != nil {
		t.Fatalf("last page: %d events, cursor %v", len(l.Events), l.NextCursor)
	}
}

func TestAuditListNeverShowsChainFields(t *testing.T) {
	f := newFixture(t)
	seedAudit(t, f.st, aliceID.UserID, 3, nil)
	rec := f.do(http.MethodGet, auditPath, "")
	body := rec.Body.String()
	if strings.Contains(body, "prev_hash") || strings.Contains(body, `"hash"`) || strings.Contains(body, audit.Genesis) {
		t.Fatalf("list view leaks chain fields: %s", body)
	}
	l := getAudit(t, f, "")
	want := []string{"seq", "ts", "job_id", "kind", "tool", "risk", "args_hash", "cred", "outcome", "detail"}
	if len(l.Events[0]) != len(want) {
		t.Fatalf("event keys = %v", l.Events[0])
	}
	for _, k := range want {
		if _, ok := l.Events[0][k]; !ok {
			t.Errorf("missing %q", k)
		}
	}
	if ts := l.Events[0]["ts"]; ts != "2026-10-19T08:00:00.000Z" {
		t.Errorf("ts = %v", ts)
	}
}

func TestAuditFilters(t *testing.T) {
	f := newFixture(t)
	jobA, jobB := "01J0000000000000000000000A", "01J0000000000000000000000B"
	seedAudit(t, f.st, aliceID.UserID, 1200, func(i int, e *audit.Event) {
		switch i % 4 {
		case 0:
			e.JobID, e.Tool, e.Risk = jobA, "files.delete", "destructive"
		case 1:
			e.JobID, e.Tool, e.Risk = jobB, "files.read", "read"
		case 2:
			e.JobID, e.Tool, e.Risk = jobA, "memory.write", "write"
		}
	})
	check := func(query string, wantN int, pred func(m map[string]any) bool) {
		t.Helper()
		var all []map[string]any
		cursor := ""
		for range 100 {
			l := getAudit(t, f, "?limit=200&"+query+"&cursor="+cursor)
			all = append(all, l.Events...)
			if l.NextCursor == nil {
				break
			}
			cursor = *l.NextCursor
		}
		if len(all) != wantN {
			t.Errorf("%s: %d events, want %d", query, len(all), wantN)
		}
		for _, e := range all {
			if !pred(e) {
				t.Errorf("%s: unexpected %v", query, e)
				break
			}
		}
	}
	check("job_id="+jobA, 600, func(m map[string]any) bool { return m["job_id"] == jobA })
	check("tool=files.delete", 300, func(m map[string]any) bool { return m["tool"] == "files.delete" })
	check("risk=read", 300, func(m map[string]any) bool { return m["risk"] == "read" })
	check("job_id="+jobA+"&risk=write", 300, func(m map[string]any) bool { return m["job_id"] == jobA && m["risk"] == "write" })
	check("tool=files.read&risk=write", 0, func(map[string]any) bool { return false })

	// a small limit returns exactly limit matches and a cursor at the last one
	l := getAudit(t, f, "?limit=3&tool=files.delete")
	if got := seqs(l); len(got) != 3 || got[0] != 1 || got[1] != 5 || got[2] != 9 || l.NextCursor == nil || *l.NextCursor != "9" {
		t.Fatalf("limit 3: %v cursor %v", got, l.NextCursor)
	}
	// exactly limit matches left: no next page
	if l := getAudit(t, f, "?limit=5&tool=files.delete&cursor=1177"); len(l.Events) != 5 || l.NextCursor != nil {
		t.Fatalf("tail: %d events, cursor %v", len(l.Events), l.NextCursor)
	}
}

func TestAuditFilterScanIsBounded(t *testing.T) {
	f := newFixture(t)
	// the only match sits behind 6000 non-matching records
	seedAudit(t, f.st, aliceID.UserID, 6001, func(i int, e *audit.Event) {
		if i == 6000 {
			e.Tool = "files.move"
		}
	})
	l := getAudit(t, f, "?tool=files.move")
	if len(l.Events) != 0 || l.NextCursor == nil || *l.NextCursor != "5000" {
		t.Fatalf("first scan: %d events, cursor %v (want empty page, cursor 5000)", len(l.Events), l.NextCursor)
	}
	l = getAudit(t, f, "?tool=files.move&cursor=5000")
	if got := seqs(l); len(got) != 1 || got[0] != 6001 || l.NextCursor != nil {
		t.Fatalf("second scan: %v cursor %v", got, l.NextCursor)
	}
}

func TestAuditRejectsMalformedQueries(t *testing.T) {
	f := newFixture(t)
	seedAudit(t, f.st, aliceID.UserID, 2, nil)
	for _, q := range []string{
		"?cursor=abc", "?cursor=-1", "?cursor=0", "?cursor=1.5", "?cursor=%201", "?cursor=1234567890123456789",
		"?cursor=1&cursor=2",
		"?limit=0", "?limit=201", "?limit=-1", "?limit=x", "?limit=1&limit=2",
		"?job_id=nope", "?job_id=01J0000000000000000000000a", "?job_id=" + strings.Repeat("A", 27),
		"?tool=Files.Read", "?tool=a%20b", "?tool=" + strings.Repeat("a", 300), "?tool=a/../b", "?tool=x&tool=y",
		"?risk=nuclear", "?risk=READ", "?risk=read&risk=write",
	} {
		wantError(t, f.do(http.MethodGet, auditPath+q, ""), http.StatusBadRequest, "invalid_request")
	}
	for _, q := range []string{"?from=0", "?from=x", "?from=-3", "?from=1&from=2", "?from=1234567890123456789"} {
		wantError(t, f.do(http.MethodGet, exportPath+q, ""), http.StatusBadRequest, "invalid_request")
	}
}

func TestAuditDetailIsCapped(t *testing.T) {
	f := newFixture(t)
	long := strings.Repeat("é", 3000) // 6000 bytes
	seedAudit(t, f.st, aliceID.UserID, 2, func(i int, e *audit.Event) {
		if i == 0 {
			e.Detail = long
		} else {
			e.Detail = "short"
		}
	})
	l := getAudit(t, f, "")
	d := l.Events[0]["detail"].(string)
	if len(d) > maxAuditDetailView || !strings.HasSuffix(d, truncMarker) || !strings.HasPrefix(d, "éé") {
		t.Fatalf("detail: %d bytes, suffix %q", len(d), d[len(d)-20:])
	}
	if l.Events[1]["detail"] != "short" {
		t.Errorf("short detail changed: %v", l.Events[1]["detail"])
	}
	// the export is the evidence and is not capped
	rec := f.do(http.MethodGet, exportPath, "")
	var first map[string]any
	if err := json.Unmarshal([]byte(strings.SplitN(rec.Body.String(), "\n", 2)[0]), &first); err != nil || first["detail"] != long {
		t.Fatalf("export detail was altered: %v", err)
	}
}

func TestAuditIsScopedToThePrincipal(t *testing.T) {
	f := newFixture(t)
	seedAudit(t, f.st, aliceID.UserID, 5, func(i int, e *audit.Event) { e.Detail = "alice-" + strconv.Itoa(i) })
	seedAudit(t, f.st, bobID.UserID, 7, func(i int, e *audit.Event) { e.Detail = "bob-secret-" + strconv.Itoa(i); e.Tool = "files.read" })

	for _, target := range []string{auditPath, auditPath + "?tool=files.read", exportPath} {
		rec := f.do(http.MethodGet, target, "")
		if strings.Contains(rec.Body.String(), "bob") || strings.Contains(rec.Body.String(), bobID.UserID) {
			t.Errorf("%s shows bob's events to alice: %s", target, rec.Body.String())
		}
	}
	if n := len(getAudit(t, f, "").Events); n != 5 {
		t.Errorf("alice sees %d events, want 5", n)
	}
	// a user id in the query or a header changes nothing
	r := f.req(http.MethodGet, auditPath+"?user_id="+bobID.UserID, "")
	r.Header.Set("X-User-Id", bobID.UserID)
	if rec := f.serve(r); strings.Contains(rec.Body.String(), "bob") {
		t.Errorf("a named user changed the scope: %s", rec.Body.String())
	}
	f.bearer = otherToken
	l := getAudit(t, f, "?limit=200")
	if len(l.Events) != 7 {
		t.Fatalf("bob sees %d events", len(l.Events))
	}
	for _, e := range l.Events {
		if strings.HasPrefix(e["detail"].(string), "alice") {
			t.Errorf("bob sees %v", e)
		}
	}
	if rec := f.do(http.MethodGet, exportPath, ""); strings.Contains(rec.Body.String(), "alice-") {
		t.Errorf("bob's export has alice's events")
	}
}

// tamperTx wraps a transaction to fail or block its audit calls.
type tamperTx struct {
	store.Tx
	s *tamperStore
}

func (t tamperTx) ReadAudit(userID string, after int64, limit int) ([]audit.Record, error) {
	n := t.s.reads.Add(1)
	if t.s.readHook != nil {
		if err := t.s.readHook(int(n)); err != nil {
			return nil, err
		}
	}
	recs, err := t.Tx.ReadAudit(userID, after, limit)
	if t.s.afterRead != nil {
		t.s.afterRead(int(n))
	}
	return recs, err
}

func (t tamperTx) VerifyAudit(userID string) (int64, error) {
	if t.s.verifyHook != nil {
		return t.s.verifyChecked, t.s.verifyHook(userID)
	}
	return t.Tx.VerifyAudit(userID)
}

// tamperStore: the audit table's triggers forbid tampering with the real
// rows, so broken chains, slow verifications and read failures are injected
// around the real store.
type tamperStore struct {
	store.Store
	reads      atomic.Int32
	readHook   func(n int) error
	afterRead  func(n int)
	verifyHook func(user string) error
	// verifyChecked is the count a hooked verification reports
	verifyChecked int64
	mu            sync.Mutex
	deadline      time.Time
	ignoreCtx     bool // the store does not notice a cancelled context
}

func (s *tamperStore) lastDeadline() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Until(s.deadline)
}

func (s *tamperStore) View(ctx context.Context, fn func(store.Tx) error) error {
	if dl, ok := ctx.Deadline(); ok {
		s.mu.Lock()
		s.deadline = dl
		s.mu.Unlock()
	}
	if s.ignoreCtx {
		ctx = context.WithoutCancel(ctx)
	}
	return s.Store.View(ctx, func(tx store.Tx) error { return fn(tamperTx{Tx: tx, s: s}) })
}

func newTamperFixture(t *testing.T) (*fixture, *tamperStore) {
	t.Helper()
	ts := &tamperStore{}
	f := newFixture(t, func(d *Deps) {
		ts.Store = d.Store
		d.Store = ts
	})
	return f, ts
}

func TestAuditVerify(t *testing.T) {
	f, ts := newTamperFixture(t)
	seedAudit(t, f.st, aliceID.UserID, 30, nil)
	rec := f.do(http.MethodGet, verifyPath, "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"ok":true,"limitation":"removal of the newest records cannot be detected","checked":30}` {
		t.Fatalf("ok: %d %s", rec.Code, rec.Body.String())
	}
	// a real, untouched chain of another user does not matter; a broken one does
	ts.verifyChecked = 6
	ts.verifyHook = func(string) error {
		return fmt.Errorf("sqlite: %w", &audit.ChainError{UserID: "u-alice", Seq: 7, Reason: "SECRET-detail"})
	}
	rec = f.do(http.MethodGet, verifyPath, "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) !=
		`{"ok":false,"message":"the audit chain is broken","limitation":"removal of the newest records cannot be detected","checked":6,"broken_at":7}` {
		t.Fatalf("broken: %d %s", rec.Code, rec.Body.String())
	}
	// a broken chain without a position still reports the break
	ts.verifyChecked = 0
	ts.verifyHook = func(string) error { return fmt.Errorf("%w: SECRET", audit.ErrChainBroken) }
	rec = f.do(http.MethodGet, verifyPath, "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) !=
		`{"ok":false,"message":"the audit chain is broken","limitation":"removal of the newest records cannot be detected","checked":0}` {
		t.Fatalf("broken without position: %d %s", rec.Code, rec.Body.String())
	}
	// any other failure goes through the error mapper
	ts.verifyHook = func(string) error { return errors.New("SECRET disk failure") }
	b := wantError(t, f.do(http.MethodGet, verifyPath, ""), http.StatusInternalServerError, "internal")
	if strings.Contains(b.Error.Message, "SECRET") {
		t.Error("error text leaked")
	}
}

type verifyResult struct {
	OK         bool   `json:"ok"`
	Message    string `json:"message"`
	Limitation string `json:"limitation"`
	Checked    *int64 `json:"checked"`
	BrokenAt   *int64 `json:"broken_at"`
}

func getVerify(t *testing.T, f *fixture) (verifyResult, string) {
	t.Helper()
	rec := f.do(http.MethodGet, verifyPath, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("verify = %d %s", rec.Code, rec.Body.String())
	}
	var v verifyResult
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v, rec.Body.String()
}

// The counts come from the real chain in the database: an empty log, an
// intact one and one with a row rewritten behind the triggers' back.
func TestAuditVerifyReportsCountsOfTheRealChain(t *testing.T) {
	f := newFixture(t)
	if v, body := getVerify(t, f); !v.OK || v.Checked == nil || *v.Checked != 0 || v.BrokenAt != nil {
		t.Fatalf("empty log: %s", body)
	}
	seedAudit(t, f.st, aliceID.UserID, 10, nil)
	seedAudit(t, f.st, bobID.UserID, 4, nil)
	if v, body := getVerify(t, f); !v.OK || v.Checked == nil || *v.Checked != 10 || v.BrokenAt != nil {
		t.Fatalf("intact chain: %s", body)
	}
	raw, err := sql.Open("sqlite", "file:"+f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	for _, q := range []string{
		`DROP TRIGGER audit_no_update`,
		`UPDATE audit SET detail = 'TAMPERED-detail' WHERE user_id = '` + aliceID.UserID + `' AND seq = 4`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	v, body := getVerify(t, f)
	if v.OK || v.Message != "the audit chain is broken" || v.Checked == nil || *v.Checked != 3 || v.BrokenAt == nil || *v.BrokenAt != 4 {
		t.Fatalf("tampered chain: %s", body)
	}
	if strings.Contains(body, "TAMPERED") || strings.Contains(body, aliceID.UserID) {
		t.Errorf("event data in the response: %s", body)
	}
	// bob's chain is untouched
	f.bearer = otherToken
	if v, body := getVerify(t, f); !v.OK || v.Checked == nil || *v.Checked != 4 {
		t.Fatalf("bob: %s", body)
	}
}

func TestAuditVerifyDetectsTheRealChainOnlyForTheCaller(t *testing.T) {
	f, ts := newTamperFixture(t)
	seedAudit(t, f.st, aliceID.UserID, 10, nil)
	seedAudit(t, f.st, bobID.UserID, 10, nil)
	var asked []string
	var mu sync.Mutex
	ts.verifyHook = func(u string) error {
		mu.Lock()
		asked = append(asked, u)
		mu.Unlock()
		return nil
	}
	// a parameter naming another user is refused outright...
	wantError(t, f.do(http.MethodGet, verifyPath+"?user_id="+bobID.UserID, ""), 400, "invalid_request")
	if len(asked) != 0 {
		t.Fatalf("verified %v", asked)
	}
	// ...and the caller's own chain is the only one verified
	r := f.req(http.MethodGet, verifyPath, "")
	r.Header.Set("X-User-Id", bobID.UserID)
	f.serve(r)
	if len(asked) != 1 || asked[0] != aliceID.UserID {
		t.Fatalf("verified %v", asked)
	}
}

func TestAuditVerifyAllowsOneRunPerUser(t *testing.T) {
	f, ts := newTamperFixture(t)
	started, release := make(chan struct{}, 4), make(chan struct{})
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	defer free()
	// a missing limit would block the second request forever: a watchdog turns that into a failure
	defer time.AfterFunc(5*time.Second, free).Stop()
	ts.verifyHook = func(u string) error {
		if u == aliceID.UserID {
			started <- struct{}{}
			<-release
		}
		return nil
	}
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- f.do(http.MethodGet, verifyPath, "") }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first verification did not start")
	}
	wantError(t, f.do(http.MethodGet, verifyPath, ""), http.StatusTooManyRequests, "rate_limited")
	// another user is not affected
	f.bearer = otherToken
	if rec := f.do(http.MethodGet, verifyPath, ""); rec.Code != http.StatusOK {
		t.Errorf("bob's verification = %d", rec.Code)
	}
	free()
	if rec := <-first; rec.Code != http.StatusOK {
		t.Errorf("first = %d", rec.Code)
	}
	// the slot is free again (also after a failure)
	f.bearer = goodToken
	ts.verifyHook = func(string) error { return errors.New("boom") }
	f.do(http.MethodGet, verifyPath, "")
	ts.verifyHook = nil
	if rec := f.do(http.MethodGet, verifyPath, ""); rec.Code != http.StatusOK {
		t.Errorf("after a failed run = %d %s", rec.Code, rec.Body.String())
	}
}

func parseExport(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestAuditExportEqualsTheChain(t *testing.T) {
	f := newFixture(t)
	seedAudit(t, f.st, aliceID.UserID, 1100, func(i int, e *audit.Event) {
		e.JobID, e.Tool, e.Risk, e.ArgsHash = "01J0000000000000000000000A", "files.read", "read", "h"+strconv.Itoa(i)
		e.Detail = "path /a/é\n\"quoted\" " + strconv.Itoa(i)
	})
	seedAudit(t, f.st, bobID.UserID, 5, nil)
	rec := f.do(http.MethodGet, exportPath, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("export = %d", rec.Code)
	}
	sum := sha256.Sum256([]byte(aliceID.UserID))
	if got, want := rec.Header().Get("Content-Disposition"), `attachment; filename="audit-`+hex.EncodeToString(sum[:4])+`.jsonl"`; got != want {
		t.Errorf("Content-Disposition = %q, want %q", got, want)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("Content-Type = %q", ct)
	}
	lines := parseExport(t, rec.Body.String())
	if len(lines) != 1100 {
		t.Fatalf("%d lines", len(lines))
	}
	v := audit.NewVerifier()
	for i, m := range lines {
		ts, err := time.Parse(time.RFC3339Nano, m["ts"].(string))
		if err != nil {
			t.Fatal(err)
		}
		r := audit.Record{
			Event: audit.Event{
				UserID: aliceID.UserID, TS: ts, JobID: m["job_id"].(string), Kind: m["kind"].(string), Tool: m["tool"].(string),
				Risk: m["risk"].(string), ArgsHash: m["args_hash"].(string), Cred: m["cred"].(string),
				Outcome: m["outcome"].(string), Detail: m["detail"].(string),
			},
			Seq: int64(m["seq"].(float64)), PrevHash: m["prev_hash"].(string), Hash: m["hash"].(string),
		}
		if r.Seq != int64(i+1) {
			t.Fatalf("line %d has seq %d", i, r.Seq)
		}
		if err := v.Add(r); err != nil {
			t.Fatalf("the exported chain does not verify at line %d: %v", i, err)
		}
	}
	if lines[0]["prev_hash"] != audit.Genesis {
		t.Error("the first record does not start at genesis")
	}
	// resume
	part := parseExport(t, f.do(http.MethodGet, exportPath+"?from=1050", "").Body.String())
	if len(part) != 51 || int64(part[0]["seq"].(float64)) != 1050 || part[0]["prev_hash"] != lines[1048]["hash"] {
		t.Errorf("from=1050: %d lines", len(part))
	}
	// an empty log is an empty 200
	f.bearer = "" // the default bearer (alice): from=5000 is past her last record
	if rec := f.do(http.MethodGet, exportPath+"?from=5000", ""); rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("empty export = %d %q", rec.Code, rec.Body.String())
	}
}

func TestAuditExportHasNoCredentials(t *testing.T) {
	s := newStack(t)
	s.enroll(t, "einstein")
	s.enroll(t, "marie")
	bearer := fakeocis.OIDCToken("einstein")
	rec := s.as("einstein", http.MethodGet, exportPath, "")
	if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
		t.Fatalf("export = %d %q", rec.Code, rec.Body.String())
	}
	list := s.as("einstein", http.MethodGet, auditPath, "").Body.String()
	for _, body := range []string{rec.Body.String(), list, s.as("einstein", http.MethodGet, verifyPath, "").Body.String()} {
		if strings.Contains(body, bearer) || strings.Contains(strings.ToLower(body), "bearer") {
			t.Fatalf("a bearer is in %q", body)
		}
		for _, ti := range s.fake.Tokens() {
			if ti.Value != "" && strings.Contains(body, ti.Value) {
				t.Fatalf("an app token is in the audit output")
			}
		}
	}
	if strings.Contains(rec.Body.String(), "marie") {
		t.Error("marie's events in einstein's export")
	}
}

func goroutinesSettle(t *testing.T, base int) {
	t.Helper()
	for range 100 {
		if runtime.NumGoroutine() <= base+2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	buf := make([]byte, 1<<16)
	t.Fatalf("goroutines leaked: %d > %d\n%s", runtime.NumGoroutine(), base, buf[:runtime.Stack(buf, true)])
}

func TestAuditExportStopsWhenTheClientGoesAway(t *testing.T) {
	f, ts := newTamperFixture(t)
	seedAudit(t, f.st, aliceID.UserID, 3000, nil)
	base := runtime.NumGoroutine()
	// the export itself must notice the departure, not only the store
	ts.ignoreCtx = true
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	ts.afterRead = func(n int) {
		if n == 2 { // the client leaves while the second page is being served
			cancel()
		}
	}
	r := f.req(http.MethodGet, exportPath, "").WithContext(ctx)
	rec := httptest.NewRecorder()
	func() {
		defer func() {
			if v := recover(); v != http.ErrAbortHandler {
				t.Errorf("recovered %v, want http.ErrAbortHandler", v)
			}
		}()
		f.h.ServeHTTP(rec, r)
	}()
	if got := ts.reads.Load(); got != 2 {
		t.Errorf("the export read %d pages after the client left, want 2", got)
	}
	if n := len(parseExport(t, rec.Body.String())); n != 1000 {
		t.Errorf("%d lines written, want 1000 (pages 1 and 2)", n)
	}
	goroutinesSettle(t, base)
}

// failWriter fails the write after okWrites.
type failWriter struct {
	*httptest.ResponseRecorder
	okWrites int
	n        int
}

func (w *failWriter) Write(b []byte) (int, error) {
	w.n++
	if w.n > w.okWrites {
		return 0, errors.New("broken pipe")
	}
	return w.ResponseRecorder.Write(b)
}

func TestAuditExportAbortsOnAWriteError(t *testing.T) {
	f := newFixture(t)
	seedAudit(t, f.st, aliceID.UserID, 1200, nil)
	w := &failWriter{ResponseRecorder: httptest.NewRecorder(), okWrites: 1}
	defer func() {
		if v := recover(); v != http.ErrAbortHandler {
			t.Errorf("recovered %v, want http.ErrAbortHandler", v)
		}
	}()
	f.h.ServeHTTP(w, f.req(http.MethodGet, exportPath, ""))
	t.Error("the handler returned normally after a write error")
}

func rawGet(t *testing.T, addr, path string) (*http.Response, []byte, error) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_, _ = conn.Write([]byte("GET " + path + " HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer " + goodToken + "\r\n\r\n"))
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(res.Body)
	return res, body, err
}

func TestAuditExportFailureMidStreamIsNotACleanResponse(t *testing.T) {
	f, ts := newTamperFixture(t)
	seedAudit(t, f.st, aliceID.UserID, 1200, nil)
	ts.readHook = func(n int) error {
		if n == 2 {
			return errors.New("SECRET disk failure")
		}
		return nil
	}
	srv := httptest.NewServer(f.h)
	defer srv.Close()
	res, body, err := rawGet(t, srv.Listener.Addr().String(), exportPath)
	if err == nil {
		t.Fatalf("the truncated export ended cleanly: %d, %d bytes", res.StatusCode, len(body))
	}
	if res.StatusCode != http.StatusOK || strings.Contains(string(body), "SECRET") {
		t.Errorf("status %d body %q", res.StatusCode, body)
	}
	if strings.Contains(f.logs.String(), "SECRET") {
		t.Errorf("logs: %s", f.logs.String())
	}

	// a failure before the first byte is a normal error response
	ts.reads.Store(0)
	ts.readHook = func(int) error { return errors.New("SECRET disk failure") }
	res, body, err = rawGet(t, srv.Listener.Addr().String(), exportPath)
	if err != nil || res.StatusCode != http.StatusInternalServerError || !strings.Contains(string(body), `"internal"`) || strings.Contains(string(body), "SECRET") {
		t.Errorf("first-page failure: %d %q %v", res.StatusCode, body, err)
	}
}

func TestAuditExportHasItsOwnDeadline(t *testing.T) {
	f, ts := newTamperFixture(t)
	seedAudit(t, f.st, aliceID.UserID, 1, nil)
	f.do(http.MethodGet, exportPath, "")
	if d := ts.lastDeadline(); d < 4*time.Minute || d > 5*time.Minute {
		t.Errorf("export deadline in %v, want about 5 minutes", d)
	}
	f.do(http.MethodGet, auditPath, "")
	if d := ts.lastDeadline(); d > time.Minute {
		t.Errorf("list deadline in %v, want the default", d)
	}
}

// Verify holds a database reader for long: one per user and at most two in
// the process (the reader pool has four connections). Exports read one short
// page at a time: one per user and up to eight in the process, so an export
// never blocks a verification. A refused request is 429 with Retry-After.
func TestAuditVerifyAndExportAreCappedPerUserAndInTheProcess(t *testing.T) {
	f, ts := newTamperFixture(t)
	carol := auth.Identity{UserID: "u-carol", Username: "carol", DisplayName: "Carol", DriveID: "d-carol"}
	f.v.tokens["carol-token"] = carol
	for _, u := range []string{aliceID.UserID, bobID.UserID, carol.UserID} {
		seedAudit(t, f.st, u, 3, nil)
	}
	started, release := make(chan string, 8), make(chan struct{})
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	defer free()
	defer time.AfterFunc(10*time.Second, free).Stop()
	blocking := atomic.Bool{}
	blocking.Store(true)
	ts.verifyHook = func(u string) error {
		if blocking.Load() {
			started <- "verify"
			<-release
		}
		return nil
	}
	ts.readHook = func(int) error {
		if blocking.Load() {
			started <- "export"
			<-release
		}
		return nil
	}
	as := func(tok, path string) *httptest.ResponseRecorder {
		r := f.req(http.MethodGet, path, "")
		r.Header.Set("Authorization", "Bearer "+tok)
		return f.serve(r)
	}
	wait := func(what string) {
		t.Helper()
		select {
		case got := <-started:
			if got != what {
				t.Fatalf("started %s, want %s", got, what)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s did not start", what)
		}
	}
	busy := func(rec *httptest.ResponseRecorder) {
		t.Helper()
		wantError(t, rec, http.StatusTooManyRequests, "rate_limited")
		if rec.Header().Get("Retry-After") == "" {
			t.Error("no Retry-After")
		}
	}
	done := make(chan *httptest.ResponseRecorder, 3)
	go func() { done <- as(goodToken, exportPath) }()
	wait("export")
	busy(as(goodToken, exportPath)) // alice's second export
	go func() { done <- as(goodToken, verifyPath) }()
	wait("verify")                  // an export does not take a verification slot
	busy(as(goodToken, verifyPath)) // alice's second verification
	go func() { done <- as(otherToken, verifyPath) }()
	wait("verify")
	busy(as("carol-token", verifyPath)) // the process cap of verifications: two are running
	blocking.Store(false)
	free()
	for range 3 {
		if rec := <-done; rec.Code != http.StatusOK {
			t.Errorf("blocked request = %d", rec.Code)
		}
	}
	if rec := as("carol-token", verifyPath); rec.Code != http.StatusOK {
		t.Errorf("carol after the others finished = %d", rec.Code)
	}
}

func TestHeavyLimiter(t *testing.T) {
	var l heavyLimiter
	if !l.acquire("a", heavyVerify) || l.acquire("a", heavyVerify) {
		t.Fatal("per user and kind")
	}
	if !l.acquire("a", heavyExport) {
		t.Fatal("other kind of the same user")
	}
	if !l.acquire("b", heavyVerify) || l.acquire("c", heavyVerify) {
		t.Fatal("verify cap 2")
	}
	for i := range maxExports - 1 {
		if !l.acquire(fmt.Sprint("e", i), heavyExport) {
			t.Fatalf("export %d refused below the cap", i)
		}
	}
	if l.acquire("x", heavyExport) {
		t.Fatal("export cap")
	}
	l.release("a", heavyVerify)
	if !l.acquire("c", heavyVerify) {
		t.Fatal("slot not freed")
	}
}

// A client that stops reading an export is cut off at the per-page write
// deadline (not after the route's five minutes), and its slot is freed.
func TestAStalledExportClientIsCutOff(t *testing.T) {
	f := newFixture(t)
	f.srv.exportPageDeadline = 200 * time.Millisecond
	seedAudit(t, f.st, aliceID.UserID, 12000, func(i int, e *audit.Event) { e.Detail = strings.Repeat("d", 500) })
	ts := httptest.NewServer(f.h)
	defer ts.Close()
	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(4 << 10)
	}
	_, _ = io.WriteString(conn, "GET "+exportPath+" HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer "+goodToken+"\r\n\r\n")
	// read nothing: the server's writes block once the buffers are full
	waitFor := func(cond func() bool) bool {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				return true
			}
			time.Sleep(20 * time.Millisecond)
		}
		return false
	}
	if !waitFor(func() bool { return f.srv.heavy.running() == 1 }) {
		t.Fatal("the export did not start")
	}
	// meanwhile another user's verification runs
	r := f.req(http.MethodGet, verifyPath, "")
	r.Header.Set("Authorization", "Bearer "+otherToken)
	if rec := f.serve(r); rec.Code != http.StatusOK {
		t.Errorf("bob's verification during a stalled export = %d", rec.Code)
	}
	began := time.Now()
	if !waitFor(func() bool { return f.srv.heavy.running() == 0 }) {
		t.Fatal("the stalled export still holds its slot")
	}
	if d := time.Since(began); d > 5*time.Second {
		t.Errorf("cut off after %v", d)
	}
	r = f.req(http.MethodGet, exportPath+"?from=11990", "")
	if rec := f.serve(r); rec.Code != http.StatusOK {
		t.Errorf("a new export after the cut = %d", rec.Code)
	}
}

// HEAD on verify and export does no work: 405.
func TestAuditHeavyRoutesRefuseHEAD(t *testing.T) {
	f, ts := newTamperFixture(t)
	seedAudit(t, f.st, aliceID.UserID, 3, nil)
	called := atomic.Int32{}
	ts.verifyHook = func(string) error { called.Add(1); return nil }
	ts.readHook = func(int) error { called.Add(1); return nil }
	for _, path := range []string{verifyPath, exportPath} {
		rec := f.do(http.MethodHead, path, "")
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET" {
			t.Errorf("HEAD %s = %d, Allow %q", path, rec.Code, rec.Header().Get("Allow"))
		}
	}
	if called.Load() != 0 {
		t.Error("HEAD ran a verification or an export")
	}
}

func TestAuditVerifyPastItsDeadlineIsUnavailable(t *testing.T) {
	f, ts := newTamperFixture(t)
	ts.verifyHook = func(string) error { return fmt.Errorf("sqlite: verify: %w", context.DeadlineExceeded) }
	wantError(t, f.do(http.MethodGet, verifyPath, ""), http.StatusServiceUnavailable, "unavailable")
}

type flushFailWriter struct {
	*httptest.ResponseRecorder
}

func (w flushFailWriter) FlushError() error { return errors.New("broken pipe") }

func TestAuditExportAbortsOnAFlushError(t *testing.T) {
	f := newFixture(t)
	seedAudit(t, f.st, aliceID.UserID, 10, nil)
	defer func() {
		if v := recover(); v != http.ErrAbortHandler {
			t.Errorf("recovered %v, want http.ErrAbortHandler", v)
		}
	}()
	f.h.ServeHTTP(flushFailWriter{httptest.NewRecorder()}, f.req(http.MethodGet, exportPath, ""))
	t.Error("the handler returned normally after a flush error")
}

func TestAuditExportIgnoresHeadersNamingAnotherUser(t *testing.T) {
	f := newFixture(t)
	seedAudit(t, f.st, aliceID.UserID, 3, nil)
	seedAudit(t, f.st, bobID.UserID, 5, nil)
	r := f.req(http.MethodGet, exportPath, "")
	r.Header.Set("X-User-Id", bobID.UserID)
	r.Header.Set("X-User", bobID.UserID)
	if lines := parseExport(t, f.serve(r).Body.String()); len(lines) != 3 {
		t.Errorf("export has %d lines, want alice's 3", len(lines))
	}
	wantError(t, f.do(http.MethodGet, exportPath+"?user_id="+bobID.UserID, ""), 400, "invalid_request")
}

func TestSeqCursorsAreCanonical(t *testing.T) {
	for _, bad := range []string{"0", "01", "007", "-1", "+1", "1e3", " 1", "1 "} {
		if _, ok := parseSeq(bad); ok {
			t.Errorf("parseSeq(%q) accepted", bad)
		}
	}
	for in, want := range map[string]int64{"1": 1, "10": 10, "999999999999999999": 999999999999999999} {
		if n, ok := parseSeq(in); !ok || n != want {
			t.Errorf("parseSeq(%q) = %d %v", in, n, ok)
		}
	}
}

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/owncloud/ocis-agent/backend/internal/memory"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient/fakeocis"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

const (
	memBase    = "/agent-api/v1/memory"
	einsteinMD = "/.ocis-agent/memory/profile.md"
)

// memStack is an enrolled stack (the memory folders exist).
func memStack(t *testing.T) *stack {
	t.Helper()
	s := newStack(t)
	s.enroll(t, "einstein")
	return s
}

func (s *stack) putMem(login, name, ifMatch, body string) *httptest.ResponseRecorder {
	r := s.req(http.MethodPut, memBase+"/"+name, body)
	r.Header.Set("Authorization", "Bearer "+fakeocis.OIDCToken(login))
	if ifMatch != "" {
		r.Header.Set("If-Match", ifMatch)
	}
	return s.serve(r)
}

func (s *stack) memETag(t *testing.T, drive, p string) string {
	t.Helper()
	_, e, ok := s.fake.File(drive, p)
	if !ok {
		t.Fatalf("%s missing", p)
	}
	return e
}

func TestMemoryListAndGet(t *testing.T) {
	s := memStack(t)
	s.fake.PutFile(einsteinDrive, einsteinMD, []byte("# Me\n"))
	s.fake.PutFile(einsteinDrive, "/.ocis-agent/memory/notes/a.md", []byte("n\n"))

	rec := s.do(http.MethodGet, memBase, "")
	if rec.Code != 200 {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}
	var l struct {
		Files []struct {
			Name     string `json:"name"`
			Size     int64  `json:"size"`
			ETag     string `json:"etag"`
			Modified string `json:"modified"`
		} `json:"files"`
	}
	mustDecode(t, rec, &l)
	if len(l.Files) != 2 || l.Files[0].Name != "notes/a.md" || l.Files[1].Name != "profile.md" ||
		l.Files[1].Size != 5 || l.Files[1].ETag != s.memETag(t, einsteinDrive, einsteinMD) || l.Files[1].Modified == "" {
		t.Fatalf("list = %+v", l)
	}

	rec = s.do(http.MethodGet, memBase+"/profile.md", "")
	var g struct{ Name, Content, ETag string }
	mustDecode(t, rec, &g)
	if rec.Code != 200 || g.Name != "profile.md" || g.Content != "# Me\n" || g.ETag != s.memETag(t, einsteinDrive, einsteinMD) || rec.Header().Get("ETag") != g.ETag {
		t.Fatalf("get = %d %+v hdr %q", rec.Code, g, rec.Header().Get("ETag"))
	}
	// notes with a slash, plain or escaped
	for _, p := range []string{"notes/a.md", "notes%2Fa.md"} {
		rec = s.do(http.MethodGet, memBase+"/"+p, "")
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"content":"n\n"`) {
			t.Errorf("get %s = %d %s", p, rec.Code, rec.Body)
		}
	}
	wantError(t, s.do(http.MethodGet, memBase+"/projects.md", ""), 404, "not_found")
}

func TestMemoryListWithoutTheFolderIsEmpty(t *testing.T) {
	s := newStack(t) // not enrolled: no /.ocis-agent
	rec := s.do(http.MethodGet, memBase, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"files":[]`) {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}
}

func TestMemoryGetLeavesTheETagCacheAlone(t *testing.T) {
	s := memStack(t)
	s.fake.PutFile(einsteinDrive, einsteinMD, []byte("# Me\n"))
	for _, p := range []string{"", "/profile.md", "/projects.md"} {
		s.do(http.MethodGet, memBase+p, "")
	}
	var etags map[string]string
	if err := s.st.View(bg, func(tx store.Tx) error {
		c, err := tx.GetCursor(einsteinID)
		etags = c.MemoryETags
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(etags) != 0 {
		t.Fatalf("the API cached ETags: %v", etags)
	}
	// and the user's edit does not either
	et := s.memETag(t, einsteinDrive, einsteinMD)
	if rec := s.putMem("einstein", "profile.md", et, `{"content":"x\n"}`); rec.Code != 200 {
		t.Fatalf("put = %d %s", rec.Code, rec.Body)
	}
	if err := s.st.View(bg, func(tx store.Tx) error {
		c, err := tx.GetCursor(einsteinID)
		etags = c.MemoryETags
		return err
	}); err != nil || len(etags) != 0 {
		t.Fatalf("etags after PUT = %v, %v", etags, err)
	}
}

func TestMemoryPutReplacesAndAudits(t *testing.T) {
	s := memStack(t)
	s.fake.PutFile(einsteinDrive, einsteinMD, []byte("# Me\n"))
	et := s.memETag(t, einsteinDrive, einsteinMD)
	rec := s.putMem("einstein", "profile.md", et, `{"content":"# Me\nlikes tea\n"}`)
	var out struct{ Name, ETag, Diff string }
	mustDecode(t, rec, &out)
	now := s.memETag(t, einsteinDrive, einsteinMD)
	if rec.Code != 200 || out.Name != "profile.md" || out.ETag != now || now == et || out.Diff != "+likes tea\n" {
		t.Fatalf("put = %d %+v", rec.Code, out)
	}
	if b, _, _ := s.fake.File(einsteinDrive, einsteinMD); string(b) != "# Me\nlikes tea\n" {
		t.Fatalf("file = %q", b)
	}
	ev := s.auditKind(t, einsteinID, "memory_edited")
	if len(ev) != 1 || ev[0].Cred != "oidc" || ev[0].Detail != "name=profile.md; diff=+likes tea\n" {
		t.Fatalf("audit = %+v", ev)
	}
}

func TestMemoryPutNeedsIfMatch(t *testing.T) {
	s := memStack(t)
	s.fake.PutFile(einsteinDrive, einsteinMD, []byte("v1\n"))
	s.fake.ResetRequests()
	wantError(t, s.putMem("einstein", "profile.md", "", `{"content":"x"}`), 428, "precondition_required")
	for _, bad := range []string{"*", `"1", "2"`} {
		wantError(t, s.putMem("einstein", "profile.md", bad, `{"content":"x"}`), 400, "invalid_request")
	}
	if n := len(s.fake.Requests()); n != 0 {
		t.Fatalf("%d oCIS requests", n)
	}
	if b, _, _ := s.fake.File(einsteinDrive, einsteinMD); string(b) != "v1\n" {
		t.Fatal("file changed")
	}
	if len(s.auditKind(t, einsteinID, "memory_edited")) != 0 {
		t.Fatal("audited a refused edit")
	}
}

func TestMemoryPutWithAStaleETagIs412(t *testing.T) {
	s := memStack(t)
	s.fake.PutFile(einsteinDrive, einsteinMD, []byte("v1\n"))
	old := s.memETag(t, einsteinDrive, einsteinMD)
	s.fake.PutFile(einsteinDrive, einsteinMD, []byte("v2\n"))
	wantError(t, s.putMem("einstein", "profile.md", old, `{"content":"mine"}`), 412, "precondition_failed")
	if b, _, _ := s.fake.File(einsteinDrive, einsteinMD); string(b) != "v2\n" {
		t.Fatalf("file = %q", b)
	}
	if len(s.auditKind(t, einsteinID, "memory_edited")) != 0 {
		t.Fatal("audited a failed edit")
	}
	// a missing file is 404
	wantError(t, s.putMem("einstein", "projects.md", `"1"`, `{"content":"x"}`), 404, "not_found")
}

func TestMemoryPostCreates(t *testing.T) {
	s := memStack(t)
	rec := s.do(http.MethodPost, memBase, `{"name":"notes/ideas.md","content":"one\n"}`)
	var out struct{ Name, ETag, Diff string }
	mustDecode(t, rec, &out)
	_, et, ok := s.fake.File(einsteinDrive, "/.ocis-agent/memory/notes/ideas.md")
	if rec.Code != 201 || !ok || out.Name != "notes/ideas.md" || out.ETag != et || out.Diff != "+one\n" {
		t.Fatalf("post = %d %+v", rec.Code, out)
	}
	ev := s.auditKind(t, einsteinID, "memory_edited")
	if len(ev) != 1 || ev[0].Detail != "name=notes/ideas.md; diff=+one\n" || ev[0].Outcome != "created" {
		t.Fatalf("audit = %+v", ev)
	}
	wantError(t, s.do(http.MethodPost, memBase, `{"name":"notes/ideas.md","content":"two\n"}`), 409, "already_exists")
	if b, _, _ := s.fake.File(einsteinDrive, "/.ocis-agent/memory/notes/ideas.md"); string(b) != "one\n" {
		t.Fatalf("file = %q", b)
	}
	if len(s.auditKind(t, einsteinID, "memory_edited")) != 1 {
		t.Fatal("audited the refused create")
	}
	// strict body: unknown and case-variant fields
	wantError(t, s.do(http.MethodPost, memBase, `{"name":"notes/x.md","content":"c","extra":1}`), 400, "invalid_request")
	wantError(t, s.do(http.MethodPost, memBase, `{"Name":"notes/x.md","content":"c"}`), 400, "invalid_request")
}

func TestMemoryInvalidNamesNeverReachOCIS(t *testing.T) {
	s := memStack(t)
	long := "notes/" + strings.Repeat("a", 65) + ".md"
	names := []string{"notes%2F..%2Fx.md", "a%2Fb.md", "a/b", "%2e%2e", "Profile.md", "NOTES/a.md", "notes/A.md",
		"README.md", ".ocis-agent%2Fmemory%2Fprofile.md", long, "profile.md%00", "profile.MD", "notes/a.txt", "*", "%2A", "profile.md%2F"}
	s.fake.ResetRequests()
	for _, n := range names {
		for _, m := range []string{http.MethodGet, http.MethodPut} {
			r := s.req(m, memBase+"/"+n, `{"content":"x"}`)
			if m == http.MethodGet {
				r = s.req(m, memBase+"/"+n, "")
			}
			r.Header.Set("If-Match", `"1"`)
			rec := s.serve(r)
			if rec.Code != 400 && rec.Code != 404 {
				t.Errorf("%s %s = %d %s", m, n, rec.Code, rec.Body)
			}
		}
		rec := s.do(http.MethodPost, memBase, `{"name":`+jsonStr(t, mustUnescape(t, n))+`,"content":"x"}`)
		if rec.Code != 400 {
			t.Errorf("POST %q = %d %s", n, rec.Code, rec.Body)
		}
	}
	// names must be 400 (not just unmatched) where the route matched
	wantError(t, s.do(http.MethodGet, memBase+"/Profile.md", ""), 400, "invalid_request")
	wantError(t, s.do(http.MethodGet, memBase+"/a%2Fb.md", ""), 400, "invalid_request")
	wantError(t, s.do(http.MethodGet, memBase+"/"+long, ""), 400, "invalid_request")
	if reqs := s.fake.Requests(); len(reqs) != 0 {
		t.Fatalf("oCIS saw %d requests for invalid names, first: %s %s", len(reqs), reqs[0].Method, reqs[0].Path)
	}
}

func TestMemoryContentLimits(t *testing.T) {
	s := memStack(t)
	s.fake.PutFile(einsteinDrive, einsteinMD, []byte("v1\n"))
	et := s.memETag(t, einsteinDrive, einsteinMD)
	s.fake.ResetRequests()
	big := strings.Repeat("a", 64<<10+1)
	wantError(t, s.putMem("einstein", "profile.md", et, `{"content":"`+big+`"}`), 413, "too_large")
	if n := len(s.fake.Requests()); n != 0 {
		t.Fatalf("%d oCIS requests for an oversized file", n)
	}
	wantError(t, s.putMem("einstein", "profile.md", et, `{"content":"a\u0000b"}`), 400, "invalid_request")
	wantError(t, s.do(http.MethodPost, memBase, `{"name":"notes/n.md","content":"`+big+`"}`), 413, "too_large")
	// a body over the route limit (200 KiB)
	wantError(t, s.putMem("einstein", "profile.md", et, `{"content":"`+strings.Repeat("a", 201<<10)+`"}`), 413, "too_large")
	// content over the limit but a body within it: the content check refuses it
	wantError(t, s.putMem("einstein", "profile.md", et, `{"content":"`+strings.Repeat(`\n`, 64<<10+1)+`"}`), 413, "too_large")
	// invalid UTF-8 and lone surrogates are refused by the body decoder
	for _, body := range []string{"{\"content\":\"a\xffb\"}", `{"content":"a\ud800b"}`} {
		r2 := httptest.NewRequest(http.MethodPut, memBase+"/profile.md", strings.NewReader(body))
		r2.Header.Set("Authorization", "Bearer "+fakeocis.OIDCToken("einstein"))
		r2.Header.Set("Origin", testOrigin)
		r2.Header.Set("Content-Type", "application/json")
		r2.Header.Set("If-Match", et)
		wantError(t, s.serve(r2), 400, "invalid_request")
	}
	// a valid 64 KiB file of newlines (a ~128 KiB body) passes
	if rec := s.putMem("einstein", "profile.md", s.memETag(t, einsteinDrive, einsteinMD), `{"content":"`+strings.Repeat(`\n`, 64<<10)+`"}`); rec.Code != 200 {
		t.Errorf("64 KiB of newlines = %d %.200s", rec.Code, rec.Body)
	}
	// documented limit: a valid file dense with control characters (6x
	// escaped, ~384 KiB) is over the 200 KiB body limit
	wantError(t, s.putMem("einstein", "profile.md", s.memETag(t, einsteinDrive, einsteinMD), `{"content":"`+strings.Repeat(`\u0001`, 64<<10)+`"}`), 413, "too_large")
	et = s.memETag(t, einsteinDrive, einsteinMD)
	// exactly 64 KiB works
	if rec := s.putMem("einstein", "profile.md", et, `{"content":"`+strings.Repeat("a", 64<<10)+`"}`); rec.Code != 200 {
		t.Errorf("64 KiB = %d %s", rec.Code, rec.Body)
	}
}

func TestMemoryAuditCapsTheDiffAndNeverHoldsTheContent(t *testing.T) {
	s := memStack(t)
	s.fake.PutFile(einsteinDrive, einsteinMD, []byte("old\n"))
	et := s.memETag(t, einsteinDrive, einsteinMD)
	content := strings.Repeat("é line of text that is long\n", 2000) // ~58 KB
	rec := s.putMem("einstein", "profile.md", et, `{"content":`+jsonStr(t, content)+`}`)
	var out struct{ Diff string }
	mustDecode(t, rec, &out)
	if rec.Code != 200 || len(out.Diff) > 16<<10 || len(out.Diff) < 16<<10-4 || !strings.HasPrefix(out.Diff, "-old\n+é line") {
		t.Fatalf("put = %d, diff %d bytes", rec.Code, len(out.Diff))
	}
	ev := s.auditKind(t, einsteinID, "memory_edited")
	if len(ev) != 1 {
		t.Fatalf("audit = %d", len(ev))
	}
	d := ev[0].Detail
	const prefix = "name=profile.md; diff="
	if !strings.HasPrefix(d, prefix) || len(d)-len(prefix) > 1500 || len(d)-len(prefix) < 1496 {
		t.Fatalf("detail is %d bytes (%q...)", len(d), d[:40])
	}
	if strings.Count(d, "é line") > 60 {
		t.Fatal("the audit holds more than the capped diff")
	}
}

func TestMemoryAuditFailureStillAnswers200(t *testing.T) {
	var fail bool
	s := newStackStore(t, func(st store.Store) store.Store { return &failingTx{Store: st, fail: &fail} })
	s.enroll(t, "einstein")
	s.fake.PutFile(einsteinDrive, einsteinMD, []byte("v1\n"))
	et := s.memETag(t, einsteinDrive, einsteinMD)
	fail = true
	rec := s.putMem("einstein", "profile.md", et, `{"content":"v2\n"}`)
	if rec.Code != 200 {
		t.Fatalf("put = %d %s", rec.Code, rec.Body)
	}
	if b, _, _ := s.fake.File(einsteinDrive, einsteinMD); string(b) != "v2\n" {
		t.Fatalf("file = %q", b)
	}
	if !strings.Contains(s.logs.String(), `"category":"memory_audit"`) {
		t.Fatalf("no category log: %s", s.logs.String())
	}
	if strings.Contains(s.logs.String(), "SECRET-db") {
		t.Fatal("log holds the store error text")
	}
}

type failingTx struct {
	store.Store
	fail *bool
}

func (s *failingTx) InTx(ctx context.Context, fn func(tx store.Tx) error) error {
	if *s.fail {
		return errors.New("SECRET-db failure")
	}
	return s.Store.InTx(ctx, fn)
}

func TestMemoryUsersSeeOnlyTheirOwnSpace(t *testing.T) {
	s := memStack(t)
	s.enroll(t, "marie")
	s.fake.PutFile(einsteinDrive, einsteinMD, []byte("einstein secret\n"))
	s.fake.PutFile(marieDrive, einsteinMD, []byte("marie secret\n"))
	s.fake.PutFile(marieDrive, "/.ocis-agent/memory/notes/radium.md", []byte("r\n"))

	var l struct{ Files []struct{ Name string } }
	mustDecode(t, s.as("einstein", http.MethodGet, memBase, ""), &l)
	if len(l.Files) != 1 || l.Files[0].Name != "profile.md" {
		t.Fatalf("einstein lists %+v", l)
	}
	if rec := s.as("einstein", http.MethodGet, memBase+"/profile.md", ""); !strings.Contains(rec.Body.String(), "einstein secret") || strings.Contains(rec.Body.String(), "marie") {
		t.Fatalf("einstein reads %s", rec.Body)
	}
	if rec := s.as("marie", http.MethodGet, memBase+"/profile.md", ""); !strings.Contains(rec.Body.String(), "marie secret") || strings.Contains(rec.Body.String(), "einstein") {
		t.Fatalf("marie reads %s", rec.Body)
	}
	wantError(t, s.as("einstein", http.MethodGet, memBase+"/notes/radium.md", ""), 404, "not_found")
	// einstein's PUT with marie's ETag changes nothing of marie's
	mEt := s.memETag(t, marieDrive, einsteinMD)
	rec := s.putMem("einstein", "profile.md", mEt, `{"content":"pwned\n"}`)
	if rec.Code != 412 {
		t.Fatalf("cross put = %d %s", rec.Code, rec.Body)
	}
	if b, _, _ := s.fake.File(marieDrive, einsteinMD); string(b) != "marie secret\n" {
		t.Fatalf("marie's file = %q", b)
	}
	// the audit lands on the editor
	s.putMem("marie", "profile.md", mEt, `{"content":"m2\n"}`)
	if len(s.auditKind(t, marieID, "memory_edited")) != 1 || len(s.auditKind(t, einsteinID, "memory_edited")) != 0 {
		t.Fatal("audit rows on the wrong user")
	}
}

func TestMemoryUsesOnlyTheUsersBearerAndNeverLogsIt(t *testing.T) {
	s := memStack(t)
	s.fake.PutFile(einsteinDrive, einsteinMD, []byte("v1\n"))
	et := s.memETag(t, einsteinDrive, einsteinMD)
	s.fake.ResetRequests()
	s.do(http.MethodGet, memBase, "")
	s.do(http.MethodGet, memBase+"/profile.md", "")
	rec := s.putMem("einstein", "profile.md", et, `{"content":"v2\n"}`)
	s.do(http.MethodPost, memBase, `{"name":"notes/b.md","content":"b\n"}`)
	reqs := s.fake.Requests()
	if len(reqs) == 0 {
		t.Fatal("no requests")
	}
	for _, rq := range reqs {
		if rq.Header.Get("Authorization") != "Bearer "+fakeocis.OIDCToken("einstein") {
			t.Errorf("%s %s used credentials %q", rq.Method, rq.Path, rq.Header.Get("Authorization"))
		}
	}
	tok := fakeocis.OIDCToken("einstein")
	if strings.Contains(s.logs.String(), tok) || strings.Contains(rec.Body.String(), tok) {
		t.Fatal("the bearer leaked")
	}
}

func TestMemoryOCISFailuresAreMapped(t *testing.T) {
	s := memStack(t)
	s.fake.PutFile(einsteinDrive, einsteinMD, []byte("v1\n"))
	et := s.memETag(t, einsteinDrive, einsteinMD)

	s.fake.Fail("GET", "/remote.php/dav/spaces/"+einsteinDrive, 401, 1)
	wantError(t, s.do(http.MethodGet, memBase+"/profile.md", ""), 502, "upstream_rejected")
	s.fake.Fail("PROPFIND", "/remote.php/dav/spaces/"+einsteinDrive, 403, 1)
	wantError(t, s.do(http.MethodGet, memBase, ""), 502, "upstream_rejected")
	s.fake.Fail("GET", "/remote.php/dav/spaces/"+einsteinDrive, 503, 1)
	wantError(t, s.do(http.MethodGet, memBase+"/profile.md", ""), 502, "upstream_unavailable")
	s.fake.Fail("PUT", "/remote.php/dav/spaces/"+einsteinDrive, 401, 1)
	wantError(t, s.putMem("einstein", "profile.md", et, `{"content":"x"}`), 502, "upstream_rejected")
	if len(s.auditKind(t, einsteinID, "memory_edited")) != 0 {
		t.Fatal("audited a failed write")
	}
}

func TestMemoryRoutesNeedAuthAndTheRightMethods(t *testing.T) {
	s := memStack(t)
	r := httptest.NewRequest(http.MethodGet, memBase, nil)
	wantError(t, s.serve(r), 401, "unauthenticated")
	wantError(t, s.do(http.MethodDelete, memBase+"/profile.md", ""), 405, "method_not_allowed")
	wantError(t, s.do(http.MethodPut, memBase, `{}`), 405, "method_not_allowed")
	// state-changing methods need the allowed Origin
	r = s.req(http.MethodPost, memBase, `{"name":"notes/o.md","content":"c"}`)
	r.Header.Set("Origin", "https://evil.example")
	wantError(t, s.serve(r), 403, "origin_rejected")
}

func mustDecode(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("status %d, body %.200q: %v", rec.Code, rec.Body.String(), err)
	}
}

func mustUnescape(t *testing.T, s string) string {
	t.Helper()
	u, err := url.PathUnescape(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func jsonStr(t *testing.T, v string) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestMemoryGetOfABigFileIsTruncatedAndNotEditable(t *testing.T) {
	s := memStack(t)
	big := strings.Repeat("é", 40<<10) // 80 KiB, over the 64 KiB edit limit
	s.fake.PutFile(einsteinDrive, einsteinMD, []byte(big))
	var g struct {
		Content   string
		Truncated bool
		Editable  bool
	}
	rec := s.do(http.MethodGet, memBase+"/profile.md", "")
	mustDecode(t, rec, &g)
	if rec.Code != 200 || !g.Truncated || g.Editable || len(g.Content) != 64<<10 || !utf8.ValidString(g.Content) {
		t.Fatalf("get = %d truncated=%v editable=%v len=%d", rec.Code, g.Truncated, g.Editable, len(g.Content))
	}
	s.fake.PutFile(einsteinDrive, einsteinMD, []byte("small\n"))
	g.Content = ""
	mustDecode(t, s.do(http.MethodGet, memBase+"/profile.md", ""), &g)
	if g.Truncated || !g.Editable || g.Content != "small\n" {
		t.Fatalf("small file = %+v", g)
	}
}

func TestMemoryFilesOverTheReadLimitAre422(t *testing.T) {
	s := memStack(t)
	s.fake.PutFile(einsteinDrive, einsteinMD, []byte(strings.Repeat("a", 2<<20+1)))
	et := s.memETag(t, einsteinDrive, einsteinMD)
	wantError(t, s.do(http.MethodGet, memBase+"/profile.md", ""), 422, "file_too_large")
	wantError(t, s.putMem("einstein", "profile.md", et, `{"content":"x"}`), 422, "file_too_large")
}

// stubMemory records calls; the API must not reach it for invalid names.
type stubMemory struct {
	calls   int
	replace func(ctx context.Context) error
}

func (m *stubMemory) List(context.Context, memory.Session) ([]memory.Info, error) {
	m.calls++
	return nil, nil
}
func (m *stubMemory) Peek(context.Context, memory.Session, string) (memory.File, error) {
	m.calls++
	return memory.File{}, nil
}
func (m *stubMemory) Replace(ctx context.Context, _ memory.Session, name, content, _ string) (memory.Change, error) {
	m.calls++
	if m.replace != nil {
		if err := m.replace(ctx); err != nil {
			return memory.Change{}, err
		}
	}
	return memory.Change{Name: name, After: content, Diff: "+" + content, ETag: `"9"`}, nil
}
func (m *stubMemory) Create(context.Context, memory.Session, string, string) (memory.Change, error) {
	m.calls++
	return memory.Change{}, nil
}
func (m *stubMemory) Limits() memory.Limits { return memory.DefaultLimits() }

func TestMemoryInvalidNamesNeverReachTheService(t *testing.T) {
	stub := &stubMemory{}
	f := newFixture(t, func(d *Deps) { d.Memory = stub })
	names := []string{"Profile.md", "a%2Fb.md", "notes%2F..%2Fx.md", "notes/" + strings.Repeat("a", 65) + ".md", "%2A", "README.md", "notes/A.md"}
	for _, n := range names {
		for _, m := range []string{http.MethodGet, http.MethodPut} {
			body := ""
			if m == http.MethodPut {
				body = `{"content":"x"}`
			}
			r := f.req(m, memBase+"/"+n, body)
			r.Header.Set("If-Match", `"1"`)
			if rec := f.serve(r); rec.Code != 400 && rec.Code != 404 {
				t.Errorf("%s %s = %d", m, n, rec.Code)
			}
		}
		wantError(t, f.do(http.MethodPost, memBase, `{"name":`+jsonStr(t, mustUnescape(t, n))+`,"content":"x"}`), 400, "invalid_request")
	}
	// the single-segment routes answer 400 itself
	wantError(t, f.do(http.MethodGet, memBase+"/Profile.md", ""), 400, "invalid_request")
	wantError(t, f.do(http.MethodGet, memBase+"/a%2Fb.md", ""), 400, "invalid_request")
	if stub.calls != 0 {
		t.Fatalf("the service was called %d times for invalid names", stub.calls)
	}
	// a valid name does reach it
	f.do(http.MethodGet, memBase+"/profile.md", "")
	if stub.calls != 1 {
		t.Fatalf("calls = %d", stub.calls)
	}
}

func TestMemoryAuditSurvivesTheClientLeaving(t *testing.T) {
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	stub := &stubMemory{replace: func(context.Context) error { cancel(); return nil }}
	f := newFixture(t, func(d *Deps) { d.Memory = stub })
	r := f.req(http.MethodPut, memBase+"/profile.md", `{"content":"x\n"}`).WithContext(ctx)
	r.Header.Set("If-Match", `"1"`)
	rec := f.serve(r)
	if rec.Code != 200 {
		t.Fatalf("put = %d %s", rec.Code, rec.Body)
	}
	var n int
	if err := f.st.View(bg, func(tx store.Tx) error {
		recs, err := tx.ReadAudit(aliceID.UserID, 0, 100)
		for _, a := range recs {
			if a.Kind == "memory_edited" {
				n++
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("memory_edited rows = %d, the audit was dropped with the request context", n)
	}
}

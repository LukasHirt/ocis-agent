package ocisclient

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/ocisclient/fakeocis"
)

const (
	testDrive = "6cb9$4c51"
	testPass  = "relativity"
)

type fixture struct {
	c  *Client
	s  *fakeocis.Server
	cr Creds
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	s := fakeocis.New(t)
	s.AddUser("einstein", testPass, "4c51", testDrive)
	c, err := New(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{c: c, s: s, cr: AppToken("einstein", testPass)}
}

var bg = context.Background()

func TestStatAndList(t *testing.T) {
	f := newFixture(t)
	etag := f.s.PutFile(testDrive, "/docs/b.md", []byte("hello"))
	f.s.PutFile(testDrive, "/docs/a.md", []byte("x"))
	f.s.PutFile(testDrive, "/docs/sub/c.md", []byte("y"))

	e, err := f.c.Stat(bg, f.cr, testDrive, "/docs/b.md")
	if err != nil {
		t.Fatal(err)
	}
	if e.Path != "/docs/b.md" || e.Dir || e.Size != 5 || e.ETag != etag || e.ModTime.IsZero() {
		t.Fatalf("stat = %+v (etag want %q)", e, etag)
	}
	d, err := f.c.Stat(bg, f.cr, testDrive, "docs")
	if err != nil || !d.Dir || d.Path != "/docs" {
		t.Fatalf("stat dir = %+v %v", d, err)
	}
	root, err := f.c.Stat(bg, f.cr, testDrive, "/")
	if err != nil || !root.Dir || root.Path != "/" {
		t.Fatalf("stat root = %+v %v", root, err)
	}
	if _, err := f.c.Stat(bg, f.cr, testDrive, "/nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stat missing = %v", err)
	}

	list, err := f.c.List(bg, f.cr, testDrive, "/docs")
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, e := range list {
		paths = append(paths, e.Path)
	}
	if strings.Join(paths, ",") != "/docs/a.md,/docs/b.md,/docs/sub" {
		t.Fatalf("list = %v", paths)
	}
	if !list[2].Dir {
		t.Fatal("sub must be a directory")
	}
	rootList, err := f.c.List(bg, f.cr, testDrive, "/")
	if err != nil || len(rootList) != 1 || rootList[0].Path != "/docs" {
		t.Fatalf("root list = %+v %v", rootList, err)
	}
	if _, err := f.c.List(bg, f.cr, testDrive, "/docs/a.md"); !errors.Is(err, ErrNotDir) {
		t.Fatalf("list a file = %v, want ErrNotDir", err)
	}
	if _, err := f.c.List(bg, f.cr, testDrive, "/nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("list missing = %v", err)
	}
}

func TestPathsWithSpecialCharactersRoundTrip(t *testing.T) {
	f := newFixture(t)
	names := []string{"a b.md", "100%.md", "q?x#y.md", "plus+sign.md", "ünï cødé.md", "a&b.md", "semi;colon,comma.md", "[brackets].md"}
	for _, n := range names {
		if _, err := f.c.Put(bg, f.cr, testDrive, "/dir with space/"+n, []byte("body "+n), ""); err == nil {
			t.Fatalf("put into a missing parent must fail for %q", n)
		}
	}
	if err := f.c.Mkcol(bg, f.cr, testDrive, "/dir with space"); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if _, err := f.c.Put(bg, f.cr, testDrive, "/dir with space/"+n, []byte("body "+n), ""); err != nil {
			t.Fatalf("put %q: %v", n, err)
		}
		if data, _, ok := f.s.File(testDrive, "/dir with space/"+n); !ok || string(data) != "body "+n {
			t.Fatalf("server did not receive %q intact (ok=%v data=%q)", n, ok, data)
		}
		data, _, err := f.c.Get(bg, f.cr, testDrive, "/dir with space/"+n, 1024)
		if err != nil || string(data) != "body "+n {
			t.Fatalf("get %q = %q %v", n, data, err)
		}
		e, err := f.c.Stat(bg, f.cr, testDrive, "/dir with space/"+n)
		if err != nil || e.Path != "/dir with space/"+n {
			t.Fatalf("stat %q = %+v %v", n, e, err)
		}
	}
	list, err := f.c.List(bg, f.cr, testDrive, "/dir with space")
	if err != nil || len(list) != len(names) {
		t.Fatalf("list = %d entries, %v (want %d)", len(list), err, len(names))
	}
	got := map[string]bool{}
	for _, e := range list {
		got[e.Path] = true
	}
	for _, n := range names {
		if !got["/dir with space/"+n] {
			t.Errorf("listing lost %q: %v", n, got)
		}
	}
}

func TestInvalidPathsNeverReachTheServer(t *testing.T) {
	f := newFixture(t)
	for _, p := range []string{"", "a/../b", "/..", "../x", ".", "a/./b", "a\x00b", "a\nb", "a\x7fb"} {
		if _, err := f.c.Stat(bg, f.cr, testDrive, p); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("Stat(%q) = %v, want ErrInvalidPath", p, err)
		}
		if err := f.c.Mkcol(bg, f.cr, testDrive, p); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("Mkcol(%q) = %v, want ErrInvalidPath", p, err)
		}
	}
	if _, err := f.c.Stat(bg, f.cr, "", "/a"); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("empty drive = %v", err)
	}
	if _, err := f.c.Stat(bg, f.cr, "a/b", "/a"); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("drive with a slash = %v", err)
	}
	if err := f.c.Delete(bg, f.cr, testDrive, "/"); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("deleting the root must be refused client-side, got %v", err)
	}
	if _, err := f.c.Put(bg, f.cr, testDrive, "/", []byte("x"), ""); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("putting the root must be refused client-side, got %v", err)
	}
	if n := len(f.s.Requests()); n != 0 {
		t.Fatalf("%d request(s) were sent for invalid paths", n)
	}
	// "a.md" and "/a.md" are the same file
	f.s.PutFile(testDrive, "/a.md", []byte("x"))
	if _, err := f.c.Stat(bg, f.cr, testDrive, "a.md"); err != nil {
		t.Fatal(err)
	}
}

func TestGet(t *testing.T) {
	f := newFixture(t)
	etag := f.s.PutFile(testDrive, "/a.md", []byte("0123456789"))
	data, got, err := f.c.Get(bg, f.cr, testDrive, "/a.md", 10)
	if err != nil || string(data) != "0123456789" || got != etag {
		t.Fatalf("get = %q %q %v", data, got, err)
	}
	if _, _, err := f.c.Get(bg, f.cr, testDrive, "/a.md", 9); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over the cap = %v, want ErrTooLarge", err)
	}
	if _, _, err := f.c.Get(bg, f.cr, testDrive, "/missing.md", 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
}

func TestGetDoesNotBufferAnUnboundedBody(t *testing.T) {
	// the server streams far more than the cap and never sets Content-Length
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"1"`)
		w.(http.Flusher).Flush()
		chunk := []byte(strings.Repeat("a", 64<<10))
		for i := 0; i < 1024; i++ { // 64 MiB
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Get(bg, AppToken("u", "t"), testDrive, "/big.bin", 1<<20); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

func TestPut(t *testing.T) {
	f := newFixture(t)
	e1, err := f.c.Put(bg, f.cr, testDrive, "/a.md", []byte("one"), "")
	if err != nil || e1 == "" {
		t.Fatalf("create = %q %v", e1, err)
	}
	if _, etag, _ := f.s.File(testDrive, "/a.md"); etag != e1 {
		t.Fatalf("returned etag %q, server has %q", e1, etag)
	}
	e2, err := f.c.Put(bg, f.cr, testDrive, "/a.md", []byte("two"), e1)
	if err != nil || e2 == e1 {
		t.Fatalf("conditional overwrite = %q %v", e2, err)
	}
	// a stale If-Match is a precondition failure and leaves the file alone
	if _, err := f.c.Put(bg, f.cr, testDrive, "/a.md", []byte("three"), e1); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("stale If-Match = %v, want ErrPrecondition", err)
	}
	if data, _, _ := f.s.File(testDrive, "/a.md"); string(data) != "two" {
		t.Fatalf("content = %q", data)
	}
	// If-Match against a file that is gone is a precondition failure too
	if _, err := f.c.Put(bg, f.cr, testDrive, "/gone.md", []byte("x"), e2); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("If-Match on a missing file = %v", err)
	}
	// no If-Match and a missing parent: the target is not found
	if _, err := f.c.Put(bg, f.cr, testDrive, "/nodir/a.md", []byte("x"), ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing parent = %v, want ErrNotFound", err)
	}
	// nil data creates an empty file
	if _, err := f.c.Put(bg, f.cr, testDrive, "/empty.md", nil, ""); err != nil {
		t.Fatal(err)
	}
	if data, _, ok := f.s.File(testDrive, "/empty.md"); !ok || len(data) != 0 {
		t.Fatalf("empty file = %q %v", data, ok)
	}
}

func TestPutFailsClosedWhenNoETagHeader(t *testing.T) {
	var propfinds int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PROPFIND" {
			propfinds++
		}
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusCreated) // no ETag header
		}
	}))
	t.Cleanup(srv.Close)
	c, _ := New(srv.URL)
	etag, err := c.Put(bg, AppToken("u", "t"), "dr", "/a.md", []byte("x"), "")
	if err == nil || etag != "" || propfinds != 0 {
		t.Fatalf("etag = %q err = %v propfinds = %d", etag, err, propfinds)
	}
}

func TestMkcolAndDelete(t *testing.T) {
	f := newFixture(t)
	if err := f.c.Mkcol(bg, f.cr, testDrive, "/d"); err != nil {
		t.Fatal(err)
	}
	if err := f.c.Mkcol(bg, f.cr, testDrive, "/d"); !errors.Is(err, ErrExists) {
		t.Fatalf("mkcol existing = %v, want ErrExists", err)
	}
	if err := f.c.Mkcol(bg, f.cr, testDrive, "/x/y"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("mkcol without parent = %v, want ErrNotFound", err)
	}
	if err := f.c.Delete(bg, f.cr, testDrive, "/d"); err != nil {
		t.Fatal(err)
	}
	if err := f.c.Delete(bg, f.cr, testDrive, "/d"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice = %v, want ErrNotFound", err)
	}
}

func TestMoveAndCopyNeverOverwrite(t *testing.T) {
	f := newFixture(t)
	f.s.PutFile(testDrive, "/a.md", []byte("a"))
	f.s.PutFile(testDrive, "/b.md", []byte("b"))
	for name, op := range map[string]func(ctx context.Context, cr Creds, drive, from, to string) error{
		"move": f.c.Move, "copy": f.c.Copy,
	} {
		if err := op(bg, f.cr, testDrive, "/a.md", "/b.md"); !errors.Is(err, ErrExists) {
			t.Fatalf("%s onto an existing file = %v, want ErrExists", name, err)
		}
		if err := op(bg, f.cr, testDrive, "/missing.md", "/c.md"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s of a missing source = %v", name, err)
		}
		if err := op(bg, f.cr, testDrive, "/a.md", "/nodir/c.md"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s into a missing folder = %v", name, err)
		}
	}
	if data, _, _ := f.s.File(testDrive, "/b.md"); string(data) != "b" {
		t.Fatal("the destination was overwritten")
	}
	if err := f.c.Copy(bg, f.cr, testDrive, "/a.md", "/with space.md"); err != nil {
		t.Fatal(err)
	}
	if err := f.c.Move(bg, f.cr, testDrive, "/a.md", "/moved.md"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := f.s.File(testDrive, "/a.md"); ok {
		t.Fatal("move left the source behind")
	}
	if data, _, ok := f.s.File(testDrive, "/with space.md"); !ok || string(data) != "a" {
		t.Fatal("copy did not create the destination")
	}
}

func TestWebDAVFailureModes(t *testing.T) {
	f := newFixture(t)
	f.s.PutFile(testDrive, "/a.md", []byte("a"))

	bad := mustClient(t, f.s.URL)
	if _, err := bad.Stat(bg, AppToken("einstein", "wrong"), testDrive, "/a.md"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("bad credentials = %v, want ErrUnauthorized", err)
	}

	f.s.Fail("PROPFIND", "/remote.php/", 503, 1)
	_, err := f.c.Stat(bg, f.cr, testDrive, "/a.md")
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 503 || se.Method != "PROPFIND" {
		t.Fatalf("injected 503 = %v", err)
	}
	if _, err := f.c.Stat(bg, f.cr, testDrive, "/a.md"); err != nil {
		t.Fatalf("the next call must succeed: %v", err)
	}

	f.s.Hang("GET", "/remote.php/")
	ctx, cancel := context.WithTimeout(bg, 150*time.Millisecond)
	defer cancel()
	if _, _, err := f.c.Get(ctx, f.cr, testDrive, "/a.md", 10); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hung server = %v, want a deadline error", err)
	}
}

// mustClient builds a client for a URL and fails the test on error.
func mustClient(t *testing.T, url string) *Client {
	t.Helper()
	c, err := New(url)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestParsesARealOCISMultistatus(t *testing.T) {
	// captured from oCIS 9.0.0-rc.1: dollar sign unescaped in the href, a 404
	// propstat for the content length of a collection, quoted etag
	const real = `<d:multistatus xmlns:s="http://sabredav.org/ns" xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns"><d:response><d:href>/remote.php/dav/spaces/6cb9$4c51/</d:href><d:propstat><d:prop><d:getetag>"0b957eec332b1b7af5a3e39265a7d164"</d:getetag><d:getlastmodified>Wed, 14 Oct 2026 15:00:12 GMT</d:getlastmodified><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat><d:propstat><d:prop><d:getcontentlength></d:getcontentlength></d:prop><d:status>HTTP/1.1 404 Not Found</d:status></d:propstat></d:response></d:multistatus>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write([]byte(real))
	}))
	t.Cleanup(srv.Close)
	e, err := mustClient(t, srv.URL).Stat(bg, AppToken("u", "t"), "6cb9$4c51", "/")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 10, 14, 15, 0, 12, 0, time.UTC)
	if e.Path != "/" || !e.Dir || e.ETag != `"0b957eec332b1b7af5a3e39265a7d164"` || !e.ModTime.Equal(want) || e.Size != 0 {
		t.Fatalf("entry = %+v", e)
	}
}

func TestMultistatusErrors(t *testing.T) {
	serve := func(body string) *Client {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		return mustClient(t, srv.URL)
	}
	cr := AppToken("u", "t")
	if _, err := serve("not xml").Stat(bg, cr, "dr", "/a.md"); err == nil {
		t.Error("malformed XML must fail")
	}
	foreign := `<d:multistatus xmlns:d="DAV:"><d:response><d:href>/somewhere/else</d:href><d:propstat><d:prop><d:getetag>"1"</d:getetag></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`
	if _, err := serve(foreign).Stat(bg, cr, "dr", "/a.md"); err == nil {
		t.Error("an href outside the space must fail")
	}
	if _, err := serve(`<d:multistatus xmlns:d="DAV:"></d:multistatus>`).Stat(bg, cr, "dr", "/a.md"); err == nil {
		t.Error("an empty multistatus must fail")
	}
}

func TestPropfindBombIsCapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "5242880")
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write([]byte(strings.Repeat("x", 5<<20)))
	}))
	t.Cleanup(srv.Close)
	if _, err := mustClient(t, srv.URL).List(bg, AppToken("u", "t"), "dr", "/"); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

func TestInvalidDriveNeverReachesTheServer(t *testing.T) {
	f := newFixture(t)
	for _, d := range []string{"..", ".", "a\x00b", "a\nb", "a\x7fb", "a?b", "a#b"} {
		if _, err := f.c.Stat(bg, f.cr, d, "/a"); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("Stat drive %q = %v, want ErrInvalidPath", d, err)
		}
	}
	if n := len(f.s.Requests()); n != 0 {
		t.Fatalf("%d request(s) were sent for invalid drives", n)
	}
}

func TestListRejectsHostileHrefs(t *testing.T) {
	const self = `<d:response><d:href>/remote.php/dav/spaces/dr/docs/</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop></d:propstat></d:response>`
	for _, href := range []string{
		"/remote.php/dav/spaces/dr/%2e%2e/x",
		"/remote.php/dav/spaces/dr/docs//y",
		"/remote.php/dav/spaces/dr/docs/../docs/x",
		"/remote.php/dav/spaces/dr/docs%2Fx",
		"/remote.php/dav/spaces/dr/docs/a%00b",
		"https://evil.example/remote.php/dav/spaces/dr/docs/x",
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = w.Write([]byte(`<d:multistatus xmlns:d="DAV:">` + self +
				`<d:response><d:href>` + href + `</d:href><d:propstat><d:prop><d:getetag>"1"</d:getetag></d:prop></d:propstat></d:response></d:multistatus>`))
		}))
		list, err := mustClient(t, srv.URL).List(bg, AppToken("u", "t"), "dr", "/docs")
		srv.Close()
		if err == nil {
			for _, e := range list {
				t.Errorf("href %q produced entry %+v", href, e)
			}
		}
	}
}

func TestMoveCopyRootRefused(t *testing.T) {
	f := newFixture(t)
	f.s.PutFile(testDrive, "/a.md", []byte("a"))
	n := len(f.s.Requests())
	for name, op := range map[string]func(ctx context.Context, cr Creds, drive, from, to string) error{"move": f.c.Move, "copy": f.c.Copy} {
		if err := op(bg, f.cr, testDrive, "/", "/x"); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("%s of root = %v", name, err)
		}
		if err := op(bg, f.cr, testDrive, "/a.md", "/"); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("%s onto root = %v", name, err)
		}
	}
	if len(f.s.Requests()) != n {
		t.Fatal("requests were sent")
	}
}

func TestPut412WithoutIfMatchIsPrecondition(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPreconditionFailed)
	}))
	t.Cleanup(srv.Close)
	if _, err := mustClient(t, srv.URL).Put(bg, AppToken("u", "t"), "dr", "/a.md", []byte("x"), ""); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("err = %v", err)
	}
}

func TestPutRejectsIfMatchWithNewline(t *testing.T) {
	f := newFixture(t)
	for _, v := range []string{"\"a\"\r\nX: y", "\"a\"\n"} {
		if _, err := f.c.Put(bg, f.cr, testDrive, "/a.md", []byte("x"), v); err == nil {
			t.Errorf("If-Match %q accepted", v)
		}
	}
	if len(f.s.Requests()) != 0 {
		t.Fatal("request sent")
	}
}

func TestGetFailsClosedWhenNoETagHeader(t *testing.T) {
	var propfinds int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PROPFIND" {
			propfinds++
		}
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte("hi"))
		}
	}))
	t.Cleanup(srv.Close)
	data, etag, err := mustClient(t, srv.URL).Get(bg, AppToken("u", "t"), "dr", "/a.md", 10)
	if err == nil || data != nil || etag != "" || propfinds != 0 {
		t.Fatalf("%q %q %v %d", data, etag, err, propfinds)
	}
}

func TestGetWaitsOutPostprocessing(t *testing.T) {
	f := newFixture(t)
	f.c.tooEarlyWaits = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	f.s.SetPostprocessing(3)
	etag, err := f.c.Put(bg, f.cr, testDrive, "/a.md", []byte("fresh"), "")
	if err != nil {
		t.Fatal(err)
	}
	data, got, err := f.c.Get(bg, f.cr, testDrive, "/a.md", 10)
	if err != nil || string(data) != "fresh" || got != etag {
		t.Fatalf("get right after an upload = %q %q %v", data, got, err)
	}

	f.s.SetPostprocessing(4) // one more 425 than the client waits out
	if _, err := f.c.Put(bg, f.cr, testDrive, "/b.md", []byte("slow"), ""); err != nil {
		t.Fatal(err)
	}
	var se *StatusError
	if _, _, err := f.c.Get(bg, f.cr, testDrive, "/b.md", 10); !errors.Is(err, ErrTooEarly) || !errors.As(err, &se) || se.Status != http.StatusTooEarly {
		t.Fatalf("get of a file still postprocessing = %v, want ErrTooEarly", err)
	}

	f.s.SetPostprocessing(100)
	if _, err := f.c.Put(bg, f.cr, testDrive, "/c.md", []byte("x"), ""); err != nil {
		t.Fatal(err)
	}
	f.c.tooEarlyWaits = []time.Duration{time.Hour}
	ctx, cancel := context.WithTimeout(bg, 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, _, err := f.c.Get(ctx, f.cr, testDrive, "/c.md", 10); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Fatalf("the wait does not end with the context: %v after %v", err, time.Since(start))
	}
}

// The 425 bodies are drained, so the retries reuse the connection instead of
// opening a new TLS connection each time.
func TestTooEarlyRetriesReuseTheConnection(t *testing.T) {
	var gets, conns int
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets++
		if gets <= 3 {
			w.WriteHeader(http.StatusTooEarly)
			_, _ = w.Write([]byte(strings.Repeat("x", 8<<10)))
			return
		}
		w.Header().Set("ETag", `"1"`)
		_, _ = w.Write([]byte("ok"))
	}))
	srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			conns++
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	c := mustClient(t, srv.URL)
	c.tooEarlyWaits = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	if data, _, err := c.Get(bg, AppToken("u", "t"), "dr", "/a.md", 10); err != nil || string(data) != "ok" {
		t.Fatalf("get = %q %v", data, err)
	}
	if conns != 1 {
		t.Fatalf("%d connections for %d requests, want 1", conns, gets)
	}
}

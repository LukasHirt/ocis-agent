package ocisclient

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func walkPaths(r WalkResult) string {
	var p []string
	for _, e := range r.Entries {
		p = append(p, e.Path)
	}
	return strings.Join(p, ",")
}

func TestWalkListsLevelByLevelWithinTheDepth(t *testing.T) {
	f := newFixture(t)
	f.s.PutFile(testDrive, "/w/a.md", []byte("a"))
	f.s.PutFile(testDrive, "/w/s1/b.md", []byte("bb"))
	f.s.PutFile(testDrive, "/w/s1/s2/c.md", []byte("c"))
	f.s.PutFile(testDrive, "/w/s1/s2/s3/d.md", []byte("d"))
	f.s.PutFile(testDrive, "/other/x.md", []byte("x"))

	r, err := f.c.Walk(bg, f.cr, testDrive, "/w", WalkLimits{MaxDepth: 2, MaxEntries: 100, MaxRequests: 100}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := walkPaths(r); got != "/w/a.md,/w/s1,/w/s1/b.md,/w/s1/s2" {
		t.Fatalf("depth 2 = %s", got)
	}
	if r.Truncated || r.Skipped != 0 || r.Requests != 2 {
		t.Fatalf("result = %+v", r)
	}
	for _, e := range r.Entries {
		if e.ModTime.IsZero() || e.ETag == "" {
			t.Fatalf("entry without metadata: %+v", e)
		}
	}
	r, err = f.c.Walk(bg, f.cr, testDrive, "/w", WalkLimits{MaxDepth: 4, MaxEntries: 100, MaxRequests: 100}, nil)
	if err != nil || !strings.HasSuffix(walkPaths(r), "/w/s1/s2/s3/d.md") || r.Requests != 4 {
		t.Fatalf("depth 4 = %s %+v %v", walkPaths(r), r, err)
	}
}

func TestWalkStopsAtItsCaps(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 5; i++ {
		f.s.PutFile(testDrive, fmt.Sprintf("/w/d%d/f.md", i), []byte("x"))
	}
	r, err := f.c.Walk(bg, f.cr, testDrive, "/w", WalkLimits{MaxDepth: 4, MaxEntries: 7, MaxRequests: 100}, nil)
	if err != nil || len(r.Entries) != 7 || !r.Truncated {
		t.Fatalf("entry cap: %d entries %+v %v", len(r.Entries), r, err)
	}
	f.s.ResetRequests()
	r, err = f.c.Walk(bg, f.cr, testDrive, "/w", WalkLimits{MaxDepth: 4, MaxEntries: 100, MaxRequests: 3}, nil)
	if err != nil || !r.Truncated || r.Requests != 3 || len(f.s.Requests()) != 3 {
		t.Fatalf("request cap: %+v (%d requests) %v", r, len(f.s.Requests()), err)
	}
}

func TestWalkSkipsExcludedPathsAndUnreadableSubfolders(t *testing.T) {
	f := newFixture(t)
	f.s.PutFile(testDrive, "/w/keep/a.md", []byte("a"))
	f.s.PutFile(testDrive, "/w/secret/b.md", []byte("b"))
	f.s.PutFile(testDrive, "/w/locked/c.md", []byte("c"))
	f.s.Fail("PROPFIND", "/remote.php/dav/spaces/"+testDrive+"/w/locked", http.StatusForbidden, 1)
	skip := func(p string) bool { return strings.HasPrefix(p, "/w/secret") }
	r, err := f.c.Walk(bg, f.cr, testDrive, "/w", WalkLimits{MaxDepth: 4, MaxEntries: 100, MaxRequests: 100}, skip)
	if err != nil {
		t.Fatal(err)
	}
	if got := walkPaths(r); got != "/w/keep,/w/locked,/w/keep/a.md" {
		t.Fatalf("entries = %s", got)
	}
	if r.Skipped != 1 {
		t.Fatalf("skipped = %d, want 1 (the forbidden folder)", r.Skipped)
	}
}

func TestWalkFailsOnTheRootAndOnCredentialErrors(t *testing.T) {
	f := newFixture(t)
	f.s.PutFile(testDrive, "/w/sub/a.md", []byte("a"))
	f.s.PutFile(testDrive, "/file.md", []byte("a"))
	lim := WalkLimits{MaxDepth: 4, MaxEntries: 100, MaxRequests: 100}
	if _, err := f.c.Walk(bg, f.cr, testDrive, "/nope", lim, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing root = %v", err)
	}
	if _, err := f.c.Walk(bg, f.cr, testDrive, "/file.md", lim, nil); !errors.Is(err, ErrNotDir) {
		t.Fatalf("file root = %v", err)
	}
	f.s.Fail("PROPFIND", "/remote.php/dav/spaces/"+testDrive+"/w/sub", http.StatusUnauthorized, 1)
	if _, err := f.c.Walk(bg, f.cr, testDrive, "/w", lim, nil); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("a 401 in a subfolder must abort the walk, got %v", err)
	}
	if _, err := f.c.Walk(bg, f.cr, testDrive, "/w", WalkLimits{}, nil); err == nil {
		t.Fatal("zero limits must be refused")
	}
}

func TestMalformedPropfindMatchesErrMalformed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write([]byte(`<d:multistatus xmlns:d="DAV:"><d:response><d:href>/remote.php/dav/spaces/dr/docs/a%0Ab</d:href></d:response></d:multistatus>`))
	}))
	defer srv.Close()
	if _, err := mustClient(t, srv.URL).List(bg, AppToken("u", "t"), "dr", "/docs"); !errors.Is(err, ErrMalformed) {
		t.Fatalf("err = %v, want ErrMalformed", err)
	}
}

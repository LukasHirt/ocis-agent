package fakeocis

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	drv   = "6cb9$4c51"
	dav   = "/remote.php/dav/spaces/" + drv
	pass  = "relativity"
	other = "9999$other"
)

type cr struct {
	user, secret string
	bearer       bool
}

var einstein = cr{user: "einstein", secret: pass}

func newFake(t *testing.T) *Server {
	t.Helper()
	s := New(t)
	s.AddUser("einstein", pass, "4c51", drv)
	s.AddUser("marie", "radioactivity", "7a7a", other)
	return s
}

func call(t *testing.T, s *Server, method, p string, c cr, hdr map[string]string, body string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequest(method, s.URL+p, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if c.bearer {
		req.Header.Set("Authorization", "Bearer "+c.secret)
	} else if c.user != "" {
		req.SetBasicAuth(c.user, c.secret)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b)
}

func status(t *testing.T, s *Server, method, p string, c cr, hdr map[string]string, body string) int {
	t.Helper()
	st, _, _ := call(t, s, method, p, c, hdr, body)
	return st
}

func TestAuthentication(t *testing.T) {
	s := newFake(t)
	if got := status(t, s, "GET", "/graph/v1.0/me", cr{}, nil, ""); got != 401 {
		t.Fatalf("no credentials = %d", got)
	}
	if got := status(t, s, "GET", "/graph/v1.0/me", cr{user: "einstein", secret: "wrong"}, nil, ""); got != 401 {
		t.Fatalf("wrong password = %d", got)
	}
	if got := status(t, s, "GET", "/graph/v1.0/me", einstein, nil, ""); got != 200 {
		t.Fatalf("password = %d", got)
	}
	if got := status(t, s, "GET", "/graph/v1.0/me", cr{secret: OIDCToken("einstein"), bearer: true}, nil, ""); got != 200 {
		t.Fatalf("bearer = %d", got)
	}
	if got := status(t, s, "GET", "/graph/v1.0/me", cr{secret: "nonsense", bearer: true}, nil, ""); got != 401 {
		t.Fatalf("bad bearer = %d", got)
	}
	// einstein cannot touch marie's space
	if got := status(t, s, "PROPFIND", "/remote.php/dav/spaces/"+other+"/", einstein, nil, ""); got != 404 {
		t.Fatalf("foreign drive = %d", got)
	}
}

func TestGraph(t *testing.T) {
	s := newFake(t)
	_, _, body := call(t, s, "GET", "/graph/v1.0/me", einstein, nil, "")
	var me struct {
		ID, DisplayName, OnPremisesSamAccountName string
	}
	if err := json.Unmarshal([]byte(body), &me); err != nil || me.ID != "4c51" || me.OnPremisesSamAccountName != "einstein" {
		t.Fatalf("me = %s (%v)", body, err)
	}
	_, _, body = call(t, s, "GET", "/graph/v1.0/me/drives", einstein, nil, "")
	var drives struct {
		Value []struct{ ID, DriveType string }
	}
	if err := json.Unmarshal([]byte(body), &drives); err != nil {
		t.Fatal(err)
	}
	var personal int
	for _, d := range drives.Value {
		if d.DriveType == "personal" && d.ID == drv {
			personal++
		}
	}
	if personal != 1 || len(drives.Value) < 2 {
		t.Fatalf("drives = %s", body)
	}
}

func TestPutQuirks(t *testing.T) {
	s := newFake(t)
	st, hdr, _ := call(t, s, "PUT", dav+"/a.md", einstein, nil, "one")
	if st != 201 || hdr.Get("ETag") == "" {
		t.Fatalf("create = %d etag %q", st, hdr.Get("ETag"))
	}
	e1 := hdr.Get("ETag")

	// If-None-Match is ignored: the file is overwritten
	st, hdr, _ = call(t, s, "PUT", dav+"/a.md", einstein, map[string]string{"If-None-Match": "*"}, "two")
	if st != 204 || hdr.Get("ETag") == e1 {
		t.Fatalf("overwrite despite If-None-Match = %d etag %q (was %q)", st, hdr.Get("ETag"), e1)
	}
	e2 := hdr.Get("ETag")

	// a wrong If-Match gives 409, and the content is untouched
	if st := status(t, s, "PUT", dav+"/a.md", einstein, map[string]string{"If-Match": e1}, "three"); st != 409 {
		t.Fatalf("stale If-Match = %d, want 409", st)
	}
	if data, _, _ := s.File(drv, "/a.md"); string(data) != "two" {
		t.Fatalf("content = %q", data)
	}
	// the right If-Match succeeds
	if st := status(t, s, "PUT", dav+"/a.md", einstein, map[string]string{"If-Match": e2}, "three"); st != 204 {
		t.Fatalf("matching If-Match = %d", st)
	}
	// If-Match on a file that does not exist is also 409
	if st := status(t, s, "PUT", dav+"/missing.md", einstein, map[string]string{"If-Match": e2}, "x"); st != 409 {
		t.Fatalf("If-Match on a missing file = %d", st)
	}
	// missing parent
	if st := status(t, s, "PUT", dav+"/nodir/a.md", einstein, nil, "x"); st != 409 {
		t.Fatalf("missing parent = %d", st)
	}
}

func TestMkcolDeleteMoveCopy(t *testing.T) {
	s := newFake(t)
	if st := status(t, s, "MKCOL", dav+"/d", einstein, nil, ""); st != 201 {
		t.Fatalf("mkcol = %d", st)
	}
	if st := status(t, s, "MKCOL", dav+"/d", einstein, nil, ""); st != 405 {
		t.Fatalf("mkcol existing = %d", st)
	}
	if st := status(t, s, "MKCOL", dav+"/x/y", einstein, nil, ""); st != 409 {
		t.Fatalf("mkcol without parent = %d", st)
	}

	s.PutFile(drv, "/d/a.md", []byte("a"))
	s.PutFile(drv, "/b.md", []byte("b"))

	// Overwrite: F refuses to replace an existing destination
	for _, m := range []string{"MOVE", "COPY"} {
		st := status(t, s, m, dav+"/b.md", einstein, map[string]string{"Destination": s.URL + dav + "/d/a.md", "Overwrite": "F"}, "")
		if st != 412 {
			t.Fatalf("%s onto an existing file with Overwrite: F = %d", m, st)
		}
	}
	if st := status(t, s, "COPY", dav+"/b.md", einstein, map[string]string{"Destination": s.URL + dav + "/d/c.md", "Overwrite": "F"}, ""); st != 201 {
		t.Fatalf("copy = %d", st)
	}
	if data, _, ok := s.File(drv, "/d/c.md"); !ok || string(data) != "b" {
		t.Fatal("copy did not create the file")
	}
	if st := status(t, s, "MOVE", dav+"/d", einstein, map[string]string{"Destination": s.URL + dav + "/e", "Overwrite": "F"}, ""); st != 201 {
		t.Fatalf("move collection = %d", st)
	}
	if _, _, ok := s.File(drv, "/d/a.md"); ok {
		t.Fatal("source still exists after MOVE")
	}
	if _, _, ok := s.File(drv, "/e/a.md"); !ok {
		t.Fatal("MOVE did not carry the children")
	}
	// missing destination parent
	if st := status(t, s, "COPY", dav+"/b.md", einstein, map[string]string{"Destination": s.URL + dav + "/nope/b.md"}, ""); st != 409 {
		t.Fatalf("copy to a missing parent = %d", st)
	}

	// DELETE ignores If-Match
	if st := status(t, s, "DELETE", dav+"/b.md", einstein, map[string]string{"If-Match": `"stale"`}, ""); st != 204 {
		t.Fatalf("delete with a stale If-Match = %d, want 204 (ignored)", st)
	}
	if st := status(t, s, "DELETE", dav+"/b.md", einstein, nil, ""); st != 404 {
		t.Fatalf("delete twice = %d", st)
	}
}

func TestPropfindAndETagPropagation(t *testing.T) {
	s := newFake(t)
	s.PutFile(drv, "/dir/a b&c#é.md", []byte("hello"))

	_, _, root0 := call(t, s, "PROPFIND", dav+"/", einstein, map[string]string{"Depth": "0"}, "")
	_, _, dir1 := call(t, s, "PROPFIND", dav+"/dir", einstein, map[string]string{"Depth": "1"}, "")
	if !strings.Contains(dir1, "<d:href>"+dav+"/dir/</d:href>") {
		t.Fatalf("collection href must end in /: %s", dir1)
	}
	wantChild := dav + "/dir/a%20b&amp;c%23%C3%A9.md"
	if !strings.Contains(dir1, "<d:href>"+wantChild+"</d:href>") {
		t.Fatalf("child href not escaped as expected (%s): %s", wantChild, dir1)
	}
	if !strings.Contains(dir1, "<d:getcontentlength>5</d:getcontentlength>") || !strings.Contains(dir1, "<d:collection/>") {
		t.Fatalf("missing length or resourcetype: %s", dir1)
	}
	_, _, depth0 := call(t, s, "PROPFIND", dav+"/dir", einstein, map[string]string{"Depth": "0"}, "")
	if strings.Contains(depth0, "a%20b") {
		t.Fatalf("Depth 0 must not list children: %s", depth0)
	}
	if st := status(t, s, "PROPFIND", dav+"/nothing", einstein, nil, ""); st != 404 {
		t.Fatalf("propfind missing = %d", st)
	}

	s.PutFile(drv, "/dir/other.md", []byte("x"))
	_, _, root1 := call(t, s, "PROPFIND", dav+"/", einstein, map[string]string{"Depth": "0"}, "")
	if etagOf(t, root0) == etagOf(t, root1) {
		t.Fatal("a change below must bump the etag of every ancestor")
	}
}

func etagOf(t *testing.T, multistatus string) string {
	t.Helper()
	const open = "<d:getetag>"
	i := strings.Index(multistatus, open)
	j := strings.Index(multistatus, "</d:getetag>")
	if i < 0 || j < 0 {
		t.Fatalf("no etag in %s", multistatus)
	}
	return multistatus[i+len(open) : j]
}

func TestGetReturnsContentAndETag(t *testing.T) {
	s := newFake(t)
	etag := s.PutFile(drv, "/a.md", []byte("body"))
	st, hdr, body := call(t, s, "GET", dav+"/a.md", einstein, nil, "")
	if st != 200 || body != "body" || hdr.Get("ETag") != etag {
		t.Fatalf("GET = %d %q etag %q (want %q)", st, body, hdr.Get("ETag"), etag)
	}
	if st := status(t, s, "GET", dav+"/missing.md", einstein, nil, ""); st != 404 {
		t.Fatalf("GET missing = %d", st)
	}
}

func createToken(t *testing.T, s *Server, c cr, label string) map[string]string {
	t.Helper()
	st, _, body := call(t, s, "POST", "/auth-app/tokens?expiry=72h&label="+label, c, nil, "")
	if st != 200 {
		t.Fatalf("create token = %d", st)
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(body), &out); err != nil || out["token"] == "" || out["label"] != label {
		t.Fatalf("create body = %s (%v)", body, err)
	}
	return out
}

func TestAuthAppQuirks(t *testing.T) {
	s := newFake(t)
	t1 := createToken(t, s, cr{secret: OIDCToken("einstein"), bearer: true}, "first")
	appTok := cr{user: "einstein", secret: t1["token"]}
	if st := status(t, s, "GET", "/graph/v1.0/me", appTok, nil, ""); st != 200 {
		t.Fatalf("app token on Graph = %d", st)
	}
	// an app token can mint another app token
	t2 := createToken(t, s, appTok, "second")

	_, _, body := call(t, s, "GET", "/auth-app/tokens", appTok, nil, "")
	var list []map[string]string
	if err := json.Unmarshal([]byte(body), &list); err != nil || len(list) != 2 {
		t.Fatalf("list = %s (%v)", body, err)
	}
	var firstHash string
	for _, e := range list {
		if e["token"] == t1["token"] || e["token"] == t2["token"] {
			t.Fatal("the list must return hashes, not plaintext tokens")
		}
		if e["label"] == "first" {
			firstHash = e["token"]
		}
	}

	// plaintext delete = 500, hash delete = 200 and the token then gets 401
	if st := status(t, s, "DELETE", "/auth-app/tokens?token="+t1["token"], appTok, nil, ""); st != 500 {
		t.Fatalf("delete by plaintext = %d, want 500", st)
	}
	if st := status(t, s, "DELETE", "/auth-app/tokens?token="+firstHash, cr{user: "einstein", secret: t2["token"]}, nil, ""); st != 200 {
		t.Fatalf("delete by hash = %d", st)
	}
	if st := status(t, s, "GET", "/graph/v1.0/me", appTok, nil, ""); st != 401 {
		t.Fatalf("deleted token = %d, want 401", st)
	}
	// independent: the other token still works
	if st := status(t, s, "GET", "/graph/v1.0/me", cr{user: "einstein", secret: t2["token"]}, nil, ""); st != 200 {
		t.Fatalf("remaining token = %d", st)
	}
	if st := status(t, s, "DELETE", "/auth-app/tokens?token=unknown", einstein, nil, ""); st != 500 {
		t.Fatalf("delete unknown = %d, want 500", st)
	}
	if st := status(t, s, "DELETE", "/auth-app/tokens?token="+firstHash, cr{user: "einstein", secret: t2["token"]}, nil, ""); st != 500 {
		t.Fatalf("delete the same hash twice = %d, want 500", st)
	}
	live := s.Tokens()
	if len(live) != 1 || live[0].Label != "second" || live[0].User != "einstein" {
		t.Fatalf("Tokens() = %+v", live)
	}
}

func TestTokensExpire(t *testing.T) {
	s := newFake(t)
	tok := createToken(t, s, einstein, "short")
	if st := status(t, s, "POST", "/auth-app/tokens?expiry=notaduration&label=x", einstein, nil, ""); st != 400 {
		t.Fatalf("bad expiry = %d", st)
	}
	c := cr{user: "einstein", secret: tok["token"]}
	s.Advance(71 * time.Hour)
	if st := status(t, s, "GET", "/graph/v1.0/me", c, nil, ""); st != 200 {
		t.Fatalf("before expiry = %d", st)
	}
	s.Advance(2 * time.Hour)
	if st := status(t, s, "GET", "/graph/v1.0/me", c, nil, ""); st != 401 {
		t.Fatalf("after expiry = %d", st)
	}
}

func TestFaultInjection(t *testing.T) {
	s := newFake(t)
	s.Fail("PUT", "/remote.php/dav/spaces/", 503, 2)
	for i := 0; i < 2; i++ {
		if st := status(t, s, "PUT", dav+"/a.md", einstein, nil, "x"); st != 503 {
			t.Fatalf("attempt %d = %d, want the injected 503", i, st)
		}
	}
	if st := status(t, s, "PUT", dav+"/a.md", einstein, nil, "x"); st != 201 {
		t.Fatalf("after the faults are used up = %d", st)
	}
	if st := status(t, s, "GET", dav+"/a.md", einstein, nil, ""); st != 200 {
		t.Fatalf("other methods are unaffected = %d", st)
	}

	s.Fail("", "/graph/", 401, -1)
	for i := 0; i < 3; i++ {
		if st := status(t, s, "GET", "/graph/v1.0/me", einstein, nil, ""); st != 401 {
			t.Fatalf("forever fault = %d", st)
		}
	}

	s.Hang("GET", "/auth-app/")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", s.URL+"/auth-app/tokens", nil)
	req.SetBasicAuth("einstein", pass)
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("a hung request must not be answered")
	}
}

// lostPut sends a PUT whose response the fake withholds and cancels it once
// the fake signals that the request took effect.
func lostPut(t *testing.T, s *Server, p, body string, lost <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-lost:
		case <-time.After(5 * time.Second): // a missing signal fails below
		}
		cancel()
	}()
	req, _ := http.NewRequestWithContext(ctx, "PUT", s.URL+p, strings.NewReader(body))
	req.SetBasicAuth("einstein", pass)
	start := time.Now()
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("a lost response must not reach the client")
	}
	if time.Since(start) > 4*time.Second {
		t.Fatal("the fake did not signal the lost response")
	}
}

func TestALostResponseStillAppliesTheRequest(t *testing.T) {
	s := newFake(t)
	lostPut(t, s, dav+"/a.md", "x", s.LoseResponse("PUT", dav+"/a.md", 1))
	if data, _, ok := s.File(drv, "/a.md"); !ok || string(data) != "x" {
		t.Fatalf("the PUT must land: %q %v", data, ok)
	}
	if st := status(t, s, "PUT", dav+"/a.md", einstein, nil, "y"); st != 204 {
		t.Fatalf("after the fault is used up = %d", st)
	}
	// a request that fails (missing parent) loses its response too and
	// changes nothing
	lostPut(t, s, dav+"/missing/b.md", "x", s.LoseResponse("PUT", dav+"/missing/b.md", 1))
	if _, _, ok := s.File(drv, "/missing/b.md"); ok {
		t.Fatal("a failed PUT must not create the file")
	}
	if st := status(t, s, "PROPFIND", dav+"/missing", einstein, map[string]string{"Depth": "0"}, ""); st != 404 {
		t.Fatalf("a failed PUT must not create the parent: PROPFIND = %d", st)
	}
	if data, _, _ := s.File(drv, "/a.md"); string(data) != "y" {
		t.Fatalf("other files must be unchanged: %q", data)
	}
}

func TestRequestsAreRecordedWithoutQueries(t *testing.T) {
	s := newFake(t)
	call(t, s, "DELETE", "/auth-app/tokens?token=abc", einstein, map[string]string{"Last-Event-ID": "7"}, "")
	reqs := s.Requests()
	if len(reqs) != 1 || reqs[0].Method != "DELETE" || reqs[0].Path != "/auth-app/tokens" || reqs[0].Header.Get("Last-Event-ID") != "7" {
		t.Fatalf("requests = %+v", reqs)
	}
	s.ResetRequests()
	if len(s.Requests()) != 0 {
		t.Fatal("ResetRequests did not clear")
	}
}

func openSSE(t *testing.T, s *Server, c cr) (*http.Response, *bufio.Reader) {
	t.Helper()
	req, _ := http.NewRequest("GET", s.URL+"/ocs/v2.php/apps/notifications/api/v1/notifications/sse", nil)
	req.SetBasicAuth(c.user, c.secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp, bufio.NewReader(resp.Body)
}

func readLine(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	ch := make(chan string, 1)
	go func() { l, _ := r.ReadString('\n'); ch <- l }()
	select {
	case l := <-ch:
		return strings.TrimRight(l, "\r\n")
	case <-time.After(3 * time.Second):
		t.Fatal("timed out reading the stream")
		return ""
	}
}

func TestSSEDeliversOnlyToTheUser(t *testing.T) {
	s := newFake(t)
	resp, r := openSSE(t, s, einstein)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if l := readLine(t, r); !strings.HasPrefix(l, ":") {
		t.Fatalf("first line = %q, want a comment", l)
	}
	readLine(t, r) // blank line
	if n := s.OpenStreams("einstein"); n != 1 {
		t.Fatalf("OpenStreams = %d, want 1", n)
	}

	s.Publish("marie", "1", "folder-created", `{"x":1}`)
	s.Publish("einstein", "2", "item-trashed", `{"id":"f"}`)
	var got []string
	for i := 0; i < 3; i++ {
		got = append(got, readLine(t, r))
	}
	want := []string{"id: 2", "event: item-trashed", `data: {"id":"f"}`}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("lines = %v, want %v", got, want)
		}
	}
}

func TestSSEHeartbeatAndDrop(t *testing.T) {
	s := newFake(t)
	s.SetHeartbeat(10 * time.Millisecond)
	_, r := openSSE(t, s, einstein)
	readLine(t, r)
	readLine(t, r)
	if l := readLine(t, r); l != ": heartbeat" {
		t.Fatalf("line = %q, want a heartbeat", l)
	}
	s.DropStreams()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("the stream was not closed")
		default:
		}
		if _, err := r.ReadString('\n'); err != nil {
			return
		}
	}
}

func TestStalledUploadDoesNotBlockOtherRequests(t *testing.T) {
	s := newFake(t)
	pr, pw := io.Pipe()
	req, _ := http.NewRequest("PUT", s.URL+dav+"/slow.md", pr)
	req.SetBasicAuth("einstein", pass)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	pw.Write([]byte("partial")) // the handler is now waiting for the rest of the body
	time.Sleep(100 * time.Millisecond)

	fin := make(chan int, 1)
	go func() { fin <- status(t, s, "GET", "/graph/v1.0/me", einstein, nil, "") }()
	select {
	case st := <-fin:
		if st != 200 {
			t.Fatalf("me = %d", st)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a stalled upload blocks other requests")
	}
	pw.Close()
	<-done
}

func TestOversizedPutIs413(t *testing.T) {
	s := newFake(t)
	s.SetMaxPut(4)
	if st := status(t, s, "PUT", dav+"/big.md", einstein, nil, "12345"); st != 413 {
		t.Fatalf("oversized = %d, want 413", st)
	}
	if _, _, ok := s.File(drv, "/big.md"); ok {
		t.Fatal("oversized body was stored")
	}
	if st := status(t, s, "PUT", dav+"/ok.md", einstein, nil, "1234"); st != 201 {
		t.Fatalf("at the limit = %d", st)
	}
}

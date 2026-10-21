// Package fakeocis is an in-memory stand-in for the parts of oCIS the agent
// uses: Graph, WebDAV (with ETags), auth-app and the SSE notification stream.
// It reproduces the oCIS behaviour measured on the dev stack, including the
// quirks (see the cases in server_test.go), and offers fault injection. It is
// test support: production code must not import it.
package fakeocis

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	davPrefix     = "/remote.php/dav/spaces/"
	ssePath       = "/ocs/v2.php/apps/notifications/api/v1/notifications/sse"
	defaultMaxPut = 16 << 20
)

// Request is one recorded request. Path is the decoded URL path without the
// query string.
type Request struct {
	Method string
	Path   string
	Header http.Header
}

// TokenInfo describes a live app token.
type TokenInfo struct{ Label, Value, User string }

type node struct {
	dir  bool
	data []byte
	etag string
	mod  time.Time
	// tooEarly is how many more GETs answer 425 (the upload is still being
	// postprocessed)
	tooEarly int
	// repropagate: the end of postprocessing moves the ETags once more
	repropagate bool
}

type account struct{ username, password, id, driveID string }

type token struct {
	value, label, user string
	created, expires   time.Time
	deleted            bool
}

type fault struct {
	method, prefix string
	status         int
	times          int
	hang           bool
	lose           bool          // handle the request, then withhold the response
	lost           chan struct{} // signalled when a lost response's request took effect
}

type sseEvent struct{ id, name, data string }

type sub struct {
	user string
	ch   chan sseEvent
	drop chan struct{}
}

// Server is the fake. All methods are safe for concurrent use.
type Server struct {
	URL string
	srv *httptest.Server

	mu        sync.Mutex
	accounts  map[string]*account
	tokens    []*token
	drives    map[string]map[string]*node
	seq       int
	clock     time.Time
	faults    []*fault
	reqs      []Request
	subs      []*sub
	heartbeat time.Duration
	closing   chan struct{}
	maxPut    int64
	postproc  int // GETs answered 425 after each upload
	// reprop: the end of an upload's postprocessing moves the ETag of the
	// file's folders a second time, repropDelay after the 425 window ends
	reprop      bool
	repropDelay time.Duration
}

// New starts a fake and stops it when the test ends.
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{
		accounts: map[string]*account{},
		drives:   map[string]map[string]*node{},
		clock:    time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC),
		closing:  make(chan struct{}),
		maxPut:   defaultMaxPut,
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	s.URL = s.srv.URL
	t.Cleanup(func() {
		close(s.closing) // lets hung and streaming handlers return
		s.srv.Close()
	})
	return s
}

// AddUser registers a user with a password and an empty personal drive.
func (s *Server) AddUser(username, password, userID, driveID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts[username] = &account{username: username, password: password, id: userID, driveID: driveID}
	s.drives[driveID] = map[string]*node{"/": {dir: true, etag: s.nextETag(), mod: s.clock}}
}

// OIDCToken returns the Bearer value the fake accepts for username. It is a
// token68 (like a real JWT), so it also passes the API's bearer parser.
func OIDCToken(username string) string { return "oidc." + username }

// Advance moves the fake clock forward (token expiry, modification times).
func (s *Server) Advance(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clock = s.clock.Add(d)
}

// Now returns the fake clock. Tests give it to the services under test so that
// token expiry and the services agree on the time.
func (s *Server) Now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clock
}

// Fail answers matching requests with status instead of handling them, times
// times (a negative number means forever). An empty method matches any.
func (s *Server) Fail(method, pathPrefix string, status, times int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = append(s.faults, &fault{method: method, prefix: pathPrefix, status: status, times: times})
}

// Hang makes matching requests never answer until the client gives up.
func (s *Server) Hang(method, pathPrefix string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = append(s.faults, &fault{method: method, prefix: pathPrefix, times: -1, hang: true})
}

// LoseResponse makes matching requests take effect as usual but never
// answers them (the response is lost, like a connection that drops after the
// server committed the change), times times (a negative number means
// forever). The client sees a timeout or its own cancellation. The returned
// channel gets a value (non-blocking, buffered 1) each time a matching
// request has been handled, so a test can cancel the client right then. Do
// not use it on streaming endpoints (SSE): their handler never finishes.
func (s *Server) LoseResponse(method, pathPrefix string, times int) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	lost := make(chan struct{}, 1)
	s.faults = append(s.faults, &fault{method: method, prefix: pathPrefix, times: times, lose: true, lost: lost})
	return lost
}

// Requests returns a copy of the recorded requests.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.reqs...)
}

// ResetRequests clears the recorded requests.
func (s *Server) ResetRequests() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = nil
}

// File returns the content and etag of a file.
func (s *Server) File(driveID, p string) ([]byte, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.drives[driveID][path.Clean("/"+p)]
	if !ok || n.dir {
		return nil, "", false
	}
	return append([]byte(nil), n.data...), n.etag, true
}

// PutFile writes a file out of band (like a user editing it in the web UI),
// creating missing parents, and returns the new etag.
func (s *Server) PutFile(driveID, p string, data []byte) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	nodes := s.drives[driveID]
	p = path.Clean("/" + p)
	for dir := path.Dir(p); dir != "/"; dir = path.Dir(dir) {
		if _, ok := nodes[dir]; !ok {
			nodes[dir] = &node{dir: true}
			s.bump(nodes, dir)
		}
	}
	nodes[p] = &node{data: append([]byte(nil), data...)}
	s.bump(nodes, p)
	return nodes[p].etag
}

// Tokens lists the live app tokens.
func (s *Server) Tokens() []TokenInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []TokenInfo
	for _, t := range s.tokens {
		if !t.deleted && s.clock.Before(t.expires) {
			out = append(out, TokenInfo{Label: t.label, Value: t.value, User: t.user})
		}
	}
	return out
}

// Publish sends an SSE event to the user's open streams.
func (s *Server) Publish(user, id, name, data string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, su := range s.subs {
		if su.user == user {
			select {
			case su.ch <- sseEvent{id, name, data}:
			default:
			}
		}
	}
}

// OpenStreams returns how many SSE streams the user has open.
func (s *Server) OpenStreams(user string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, su := range s.subs {
		if su.user == user {
			n++
		}
	}
	return n
}

// SetHeartbeat makes new SSE streams send a comment every d (0 = never).
func (s *Server) SetHeartbeat(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.heartbeat = d
}

// SetMaxPut lowers the largest accepted PUT body (default 16 MiB); larger
// bodies are answered with 413.
func (s *Server) SetMaxPut(n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maxPut = n
}

// SetPostprocessing makes the next n GETs of a file answer 425 Too Early
// after every upload (PUT), as oCIS does while it postprocesses a fresh
// upload; PROPFIND is not affected. The default is 0. PutFile is not an
// upload and is never postprocessed.
func (s *Server) SetPostprocessing(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.postproc = n
}

// SetPostprocessingPropagation makes the end of every later upload's
// postprocessing give the uploaded file's folder (and its ancestors) a new
// ETag once more, delay after the 425 window of SetPostprocessing ends (at
// once after the upload without one), as oCIS does when postprocessing
// finishes (measured on the dev stack: a second move of the parent ETag about
// 30 ms after the PUT returned). Off by default.
func (s *Server) SetPostprocessingPropagation(on bool, delay time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reprop, s.repropDelay = on, delay
}

// repropagate schedules the second propagation for the upload at p. Callers
// hold s.mu.
func (s *Server) repropagate(drive, p string) {
	time.AfterFunc(s.repropDelay, func() {
		select {
		case <-s.closing:
			return
		default:
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if nodes := s.drives[drive]; nodes != nil {
			s.bump(nodes, path.Dir(p))
		}
	})
}

// DropStreams closes every open SSE stream from the server side.
func (s *Server) DropStreams() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, su := range s.subs {
		close(su.drop)
	}
	s.subs = nil
}

func (s *Server) nextETag() string {
	s.seq++
	return fmt.Sprintf(`"%d"`, s.seq)
}

// bump gives p and every ancestor a new etag and modification time. Callers
// hold s.mu.
func (s *Server) bump(nodes map[string]*node, p string) {
	for {
		if n, ok := nodes[p]; ok {
			n.etag = s.nextETag()
			n.mod = s.clock
		}
		if p == "/" {
			return
		}
		p = path.Dir(p)
	}
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.reqs = append(s.reqs, Request{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone()})
	f := s.matchFault(r)
	s.mu.Unlock()
	if f != nil && f.lose {
		s.handle(httptest.NewRecorder(), r)
		select {
		case f.lost <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-s.closing:
		}
		return
	}
	if f != nil {
		if f.hang {
			select {
			case <-r.Context().Done():
			case <-s.closing:
			}
			return
		}
		w.WriteHeader(f.status)
		return
	}
	s.handle(w, r)
}

// handle serves a request that no fault intercepted.
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	acct := s.authenticate(r)
	if acct == nil {
		w.Header().Set("WWW-Authenticate", `Basic realm="fakeocis"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch p := r.URL.Path; {
	case p == "/graph/v1.0/me":
		s.graphMe(w, acct)
	case p == "/graph/v1.0/me/drives":
		s.graphDrives(w, acct)
	case p == "/auth-app/tokens":
		s.authApp(w, r, acct)
	case p == ssePath:
		s.sse(w, r, acct)
	case strings.HasPrefix(p, davPrefix):
		s.dav(w, r, acct)
	default:
		http.NotFound(w, r)
	}
}

// matchFault returns the first matching fault and consumes one use. Callers
// hold s.mu.
func (s *Server) matchFault(r *http.Request) *fault {
	for _, f := range s.faults {
		if f.times == 0 || (f.method != "" && f.method != r.Method) || !strings.HasPrefix(r.URL.Path, f.prefix) {
			continue
		}
		if f.times > 0 {
			f.times--
		}
		c := *f
		return &c
	}
	return nil
}

func (s *Server) authenticate(r *http.Request) *account {
	s.mu.Lock()
	defer s.mu.Unlock()
	if user, secret, ok := r.BasicAuth(); ok {
		a := s.accounts[user]
		if a == nil {
			return nil
		}
		if secret == a.password {
			return a
		}
		for _, t := range s.tokens {
			if !t.deleted && t.user == user && t.value == secret && s.clock.Before(t.expires) {
				return a
			}
		}
		return nil
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer oidc.") {
		return s.accounts[strings.TrimPrefix(h, "Bearer oidc.")]
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) graphMe(w http.ResponseWriter, a *account) {
	writeJSON(w, http.StatusOK, map[string]any{
		"id":                       a.id,
		"displayName":              strings.ToUpper(a.username[:1]) + a.username[1:],
		"onPremisesSamAccountName": a.username,
		"userType":                 "Member",
	})
}

func (s *Server) graphDrives(w http.ResponseWriter, a *account) {
	writeJSON(w, http.StatusOK, map[string]any{"value": []map[string]string{
		{"id": "virtual$virtual", "driveType": "virtual", "name": "Shares"},
		{"id": "project$space", "driveType": "project", "name": "Project"},
		{"id": a.driveID, "driveType": "personal", "name": a.username},
	}})
}

func hashOf(t *token) string { return "$2a$12$hash-of-" + t.value }

func tokenJSON(value string, t *token) map[string]string {
	return map[string]string{
		"token":           value,
		"label":           t.label,
		"created_date":    t.created.Format(time.RFC3339),
		"expiration_date": t.expires.Format(time.RFC3339),
	}
}

func (s *Server) authApp(w http.ResponseWriter, r *http.Request, a *account) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.Method {
	case http.MethodPost:
		exp := 72 * time.Hour
		if v := r.URL.Query().Get("expiry"); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			exp = d
		}
		s.seq++
		t := &token{
			value: fmt.Sprintf("apptoken-%d", s.seq), label: r.URL.Query().Get("label"), user: a.username,
			created: s.clock, expires: s.clock.Add(exp),
		}
		s.tokens = append(s.tokens, t)
		writeJSON(w, http.StatusOK, tokenJSON(t.value, t))
	case http.MethodGet:
		out := []map[string]string{}
		for _, t := range s.tokens {
			if !t.deleted && t.user == a.username && s.clock.Before(t.expires) {
				out = append(out, tokenJSON(hashOf(t), t))
			}
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodDelete:
		q := r.URL.Query().Get("token")
		for _, t := range s.tokens {
			if !t.deleted && t.user == a.username && hashOf(t) == q {
				t.deleted = true
				w.WriteHeader(http.StatusOK)
				return
			}
		}
		// measured on oCIS: an unknown, already deleted or plaintext value all answer 500
		w.WriteHeader(http.StatusInternalServerError)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) dav(w http.ResponseWriter, r *http.Request, a *account) {
	drive, p, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, davPrefix), "/")
	p = path.Clean("/" + p)
	// Read the upload before taking the lock: a slow uploader must not block
	// every other request.
	var body []byte
	if r.Method == http.MethodPut {
		s.mu.Lock()
		limit := s.maxPut
		s.mu.Unlock()
		var err error
		body, err = io.ReadAll(io.LimitReader(r.Body, limit+1))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if int64(len(body)) > limit {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	nodes := s.drives[drive]
	if nodes == nil || a.driveID != drive {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch r.Method {
	case "PROPFIND":
		s.propfind(w, r, drive, nodes, p)
	case http.MethodGet, http.MethodHead:
		n, ok := nodes[p]
		switch {
		case !ok:
			w.WriteHeader(http.StatusNotFound)
		case n.dir:
			w.WriteHeader(http.StatusMethodNotAllowed)
		case n.tooEarly > 0:
			n.tooEarly--
			if n.tooEarly == 0 && n.repropagate {
				n.repropagate = false
				s.repropagate(drive, p)
			}
			w.WriteHeader(http.StatusTooEarly)
		default:
			w.Header().Set("ETag", n.etag)
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(n.data)
		}
	case http.MethodPut:
		s.put(w, r, drive, nodes, p, body)
	case "MKCOL":
		if _, ok := nodes[p]; ok {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if pn, ok := nodes[path.Dir(p)]; !ok || !pn.dir {
			w.WriteHeader(http.StatusConflict)
			return
		}
		nodes[p] = &node{dir: true}
		s.bump(nodes, p)
		w.WriteHeader(http.StatusCreated)
	case http.MethodDelete:
		// If-Match is ignored, exactly like oCIS
		if _, ok := nodes[p]; !ok || p == "/" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		remove(nodes, p)
		s.bump(nodes, path.Dir(p))
		w.WriteHeader(http.StatusNoContent)
	case "MOVE", "COPY":
		s.moveCopy(w, r, drive, nodes, p)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func remove(nodes map[string]*node, p string) {
	for k := range nodes {
		if k == p || strings.HasPrefix(k, p+"/") {
			delete(nodes, k)
		}
	}
}

func (s *Server) put(w http.ResponseWriter, r *http.Request, drive string, nodes map[string]*node, p string, data []byte) {
	if pn, ok := nodes[path.Dir(p)]; !ok || !pn.dir || p == "/" {
		w.WriteHeader(http.StatusConflict)
		return
	}
	existing, exists := nodes[p]
	if exists && existing.dir {
		w.WriteHeader(http.StatusConflict)
		return
	}
	// If-None-Match is ignored; a failing If-Match answers 409, not 412
	if im := r.Header.Get("If-Match"); im != "" && (!exists || existing.etag != im) {
		w.WriteHeader(http.StatusConflict)
		return
	}
	nodes[p] = &node{data: data, tooEarly: s.postproc, repropagate: s.reprop && s.postproc > 0}
	s.bump(nodes, p)
	if s.reprop && s.postproc == 0 {
		s.repropagate(drive, p)
	}
	w.Header().Set("ETag", nodes[p].etag)
	if exists {
		w.WriteHeader(http.StatusNoContent)
	} else {
		w.WriteHeader(http.StatusCreated)
	}
}

func (s *Server) moveCopy(w http.ResponseWriter, r *http.Request, drive string, nodes map[string]*node, p string) {
	du, err := url.Parse(r.Header.Get("Destination"))
	prefix := davPrefix + drive
	if err != nil || !strings.HasPrefix(du.Path, prefix+"/") {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	dst := path.Clean(strings.TrimPrefix(du.Path, prefix))
	if _, ok := nodes[p]; !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if p == "/" || dst == p || strings.HasPrefix(dst, p+"/") {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if pn, ok := nodes[path.Dir(dst)]; !ok || !pn.dir {
		w.WriteHeader(http.StatusConflict)
		return
	}
	_, exists := nodes[dst]
	if exists {
		if r.Header.Get("Overwrite") == "F" {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		remove(nodes, dst)
	}
	var keys []string
	for k := range nodes {
		if k == p || strings.HasPrefix(k, p+"/") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		src := nodes[k]
		nodes[dst+strings.TrimPrefix(k, p)] = &node{dir: src.dir, data: append([]byte(nil), src.data...), etag: s.nextETag(), mod: s.clock}
	}
	if r.Method == "MOVE" {
		remove(nodes, p)
		s.bump(nodes, path.Dir(p))
	}
	s.bump(nodes, dst)
	if exists {
		w.WriteHeader(http.StatusNoContent)
	} else {
		w.WriteHeader(http.StatusCreated)
	}
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func (s *Server) href(drive, p string, dir bool) string {
	var segs []string
	for _, seg := range strings.Split(strings.Trim(p, "/"), "/") {
		if seg != "" {
			segs = append(segs, url.PathEscape(seg))
		}
	}
	h := davPrefix + url.PathEscape(drive)
	if len(segs) > 0 {
		h += "/" + strings.Join(segs, "/")
	}
	if dir {
		h += "/"
	}
	return h
}

func (s *Server) propfind(w http.ResponseWriter, r *http.Request, drive string, nodes map[string]*node, p string) {
	n, ok := nodes[p]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	list := []string{p}
	if r.Header.Get("Depth") != "0" && n.dir {
		var children []string
		for k := range nodes {
			if k != "/" && path.Dir(k) == p {
				children = append(children, k)
			}
		}
		sort.Strings(children)
		list = append(list, children...)
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?><d:multistatus xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns">`)
	for _, k := range list {
		n := nodes[k]
		fmt.Fprintf(&b, `<d:response><d:href>%s</d:href><d:propstat><d:prop><d:getetag>%s</d:getetag><d:getlastmodified>%s</d:getlastmodified>`,
			xmlEscape(s.href(drive, k, n.dir)), n.etag, n.mod.UTC().Format(http.TimeFormat))
		if n.dir {
			b.WriteString(`<d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat>`)
			b.WriteString(`<d:propstat><d:prop><d:getcontentlength></d:getcontentlength></d:prop><d:status>HTTP/1.1 404 Not Found</d:status></d:propstat>`)
		} else {
			fmt.Fprintf(&b, `<d:getcontentlength>%d</d:getcontentlength><d:resourcetype></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat>`, len(n.data))
		}
		b.WriteString(`</d:response>`)
	}
	b.WriteString(`</d:multistatus>`)
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	_, _ = io.WriteString(w, b.String())
}

func (s *Server) sse(w http.ResponseWriter, r *http.Request, a *account) {
	fl, ok := w.(http.Flusher)
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	su := &sub{user: a.username, ch: make(chan sseEvent, 16), drop: make(chan struct{})}
	s.mu.Lock()
	s.subs = append(s.subs, su)
	hb := s.heartbeat
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for i, o := range s.subs {
			if o == su {
				s.subs = append(s.subs[:i], s.subs[i+1:]...)
				break
			}
		}
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, ": connected\n\n")
	fl.Flush()

	var tick <-chan time.Time
	if hb > 0 {
		tk := time.NewTicker(hb)
		defer tk.Stop()
		tick = tk.C
	}
	for {
		select {
		case ev := <-su.ch:
			fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", ev.id, ev.name, ev.data)
			fl.Flush()
		case <-tick:
			_, _ = io.WriteString(w, ": heartbeat\n\n")
			fl.Flush()
		case <-su.drop:
			return
		case <-r.Context().Done():
			return
		case <-s.closing:
			return
		}
	}
}

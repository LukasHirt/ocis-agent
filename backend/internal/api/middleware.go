package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"math"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/auth"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// The request pipeline, in this order (the tests pin it):
//
//  1. recover: a panic is a 500 internal and a log line with category
//     "panic", never the panic value; if the response had already started,
//     the connection is aborted (re-panic with http.ErrAbortHandler) so the
//     client never sees a clean, truncated response
//  2. request id: 16 random hex digits, X-Request-Id, in every log line and
//     error body; a client-supplied id is ignored
//  3. security headers on every response
//  4. pre-auth gate per client IP (RemoteAddr, IPv6 by /64; X-Forwarded-For
//     is never trusted): a token the validator has neither cached nor known
//     reserves a token of the IP's failure bucket before validation; while
//     the bucket is empty only cached and known tokens get through (see
//     ratelimit.go)
//  5. for state-changing methods: Origin must equal the allowed origin
//     exactly and Sec-Fetch-Site, if sent, must be same-origin; checked
//     before auth, so a cross-site request never reaches token validation
//  6. Authorization: Bearer <token>, validated by oCIS (TokenValidator); a
//     missing, malformed or rejected token stays charged to the IP, as does a
//     request whose client hung up; success, oCIS failures and the route's
//     own deadline refund
//  7. per-user rate limit
//  8. Seen(userID), at most once a minute per user (seen.go), also for a
//     user who has not enrolled; a failure is logged and ignored, and
//     retried by the next request
//  9. body rules (requireJSON) and the body limit
//  10. the handler, under the route's deadline
//
// Steps 1-3 wrap everything, including unmatched paths (JSON 404/405) and
// GET /agent-api/healthz, which skips 4-9; they get the default read and
// write deadlines (30s, set before dispatch). The route deadline (default 30s)
// starts at step 4: a context deadline plus read and write deadlines on the
// connection through http.ResponseController. A stream route (withStream)
// clears the read deadline instead and renews the write deadline per frame.

// writeGrace is how much longer the connection's write deadline is than the
// route's context deadline, so a handler that stops at its deadline can
// still write its error response.
var writeGrace = 5 * time.Second // a variable for tests

// maxBearerLen caps the bearer token.
const maxBearerLen = 8 << 10

type ctxKey int

const (
	infoKey ctxKey = iota
	principalKey
	bearerKey
)

// reqInfo is what the request log line reports. It holds only ids and fixed
// categories.
type reqInfo struct {
	maxBody  int64 // the route's body limit
	id       string
	route    string
	user     string
	category string
}

func infoFrom(ctx context.Context) *reqInfo {
	ri, _ := ctx.Value(infoKey).(*reqInfo)
	return ri
}

// principal returns the authenticated caller.
func principal(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey).(Principal)
	return p, ok
}

// bearerFrom returns the caller's validated bearer token, which only the
// endpoints that need the user's own credentials (enrol, unenrol, revoke)
// pass on.
func bearerFrom(ctx context.Context) (ocisclient.Secret, bool) {
	b, ok := ctx.Value(bearerKey).(ocisclient.Secret)
	return b, ok
}

// statusWriter records the status for the log line. It implements Unwrap,
// so http.ResponseController reaches the connection through it, and
// FlushError/Flush, so both the controller and w.(http.Flusher) work.
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// FlushError flushes and returns the error; http.ResponseController.Flush
// prefers this method, so its callers see a failed flush.
func (w *statusWriter) FlushError() error {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

// Flush lets handlers use w.(http.Flusher) too; it cannot report errors.
func (w *statusWriter) Flush() { _ = w.FlushError() }

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // never fails (crypto/rand)
	return hex.EncodeToString(b[:])
}

func setSecurityHeaders(h http.Header) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
}

// logMethod is the method for the log line: a client can send any token.
func logMethod(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodOptions:
		return m
	}
	return "other"
}

// serve is steps 1-3 and the request log line.
func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ri := &reqInfo{id: newRequestID(), route: "unmatched"}
	sw := &statusWriter{ResponseWriter: w}
	setSecurityHeaders(sw.Header())
	sw.Header().Set("X-Request-Id", ri.id)
	r = r.WithContext(context.WithValue(r.Context(), infoKey, ri))
	// every request gets read and write deadlines, also one that never
	// reaches a route (the server has no ReadTimeout or WriteTimeout): a body
	// that never ends must not hold the connection; a route replaces them
	setDeadlines(sw, s.baseTimeout)
	defer func() {
		v := recover()
		abort := false
		if v != nil {
			if v == http.ErrAbortHandler {
				abort = true
				ri.category = "aborted"
			} else {
				s.d.Log.Error("api: panic", "category", "panic", "request_id", ri.id, "route", ri.route)
				if sw.wrote {
					abort = true
					ri.category = "panic"
				} else {
					writeCode(sw, r, http.StatusInternalServerError, "internal")
				}
			}
		}
		status := sw.status
		if !sw.wrote && !abort {
			status = http.StatusOK
		} // an aborted request that wrote nothing has no status (0)
		s.d.Log.Info("api: request", "request_id", ri.id, "method", logMethod(r.Method), "route", ri.route,
			"status", status, "duration_ms", time.Since(start).Milliseconds(), "user", ri.user, "category", ri.category)
		if abort {
			// net/http closes the connection without logging a stack
			panic(http.ErrAbortHandler)
		}
	}()
	s.dispatch(sw, r)
}

// dispatch routes the request. Only clean paths under /agent-api/ reach the
// mux (so it never redirects and /agent/... is never routed); anything
// unmatched is a JSON 404, a wrong method a JSON 405 with Allow.
func (s *Server) dispatch(w http.ResponseWriter, r *http.Request) {
	p, ep := r.URL.Path, r.URL.EscapedPath()
	if !strings.HasPrefix(p, "/agent-api/") || !strings.HasPrefix(ep, "/agent-api/") || !cleanURLPath(ep) {
		writeCode(w, r, http.StatusNotFound, "not_found")
		return
	}
	h, pattern := s.mux.Handler(r)
	if pattern == "" {
		cw := &captureWriter{header: http.Header{}}
		h.ServeHTTP(cw, r)
		if cw.status == http.StatusMethodNotAllowed {
			w.Header().Set("Allow", cw.header.Get("Allow"))
			writeCode(w, r, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		writeCode(w, r, http.StatusNotFound, "not_found")
		return
	}
	s.mux.ServeHTTP(w, r)
}

func cleanURLPath(p string) bool {
	c := path.Clean(p)
	if strings.HasSuffix(p, "/") && c != "/" {
		c += "/"
	}
	return c == p
}

// setDeadlines sets the connection's read deadline to now+d and its write
// deadline to now+d+writeGrace. They are ErrNotSupported where the writer has
// no connection (httptest recorders); a route's context deadline still
// applies there.
func setDeadlines(w http.ResponseWriter, d time.Duration) {
	ctl := http.NewResponseController(w)
	_ = ctl.SetReadDeadline(time.Now().Add(d))
	_ = ctl.SetWriteDeadline(time.Now().Add(d + writeGrace))
}

// streamDeadlines clears the connection's read deadline (a stream route has
// no body; the request headers are already read) and sets the write deadline
// of a default route, which the stream handler renews for every frame.
func streamDeadlines(w http.ResponseWriter) {
	ctl := http.NewResponseController(w)
	_ = ctl.SetReadDeadline(time.Time{})
	_ = ctl.SetWriteDeadline(time.Now().Add(defaultTimeout + writeGrace))
}

// captureWriter takes the mux's own 404/405 response, so it can be replaced
// by the JSON one.
type captureWriter struct {
	header http.Header
	status int
}

func (c *captureWriter) Header() http.Header         { return c.header }
func (c *captureWriter) Write(b []byte) (int, error) { return len(b), nil }
func (c *captureWriter) WriteHeader(code int)        { c.status = code }

// chain is steps 4-10 for one route.
func (s *Server) chain(pattern string, rc routeConfig, h handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ri := infoFrom(r.Context())
		ri.route, ri.maxBody = pattern, rc.maxBody

		parent := r.Context() // ends when the client goes away
		ctx, cancel := context.WithTimeout(parent, rc.timeout)
		defer cancel()
		if rc.stream {
			streamDeadlines(w)
		} else {
			setDeadlines(w, rc.timeout)
		}
		r = r.WithContext(ctx)

		// 4: parsing and the Cached/Known lookups make no network call. A
		// token that is neither cached nor known RESERVES a token of the
		// IP's failure bucket before it may reach oCIS, so concurrency
		// cannot multiply the validator calls; the reservation is refunded
		// when the failure was not the client's (see ratelimit.go).
		ipKey := clientKey(r)
		tok, tokOK := parseBearer(r.Header.Values("Authorization"))
		var cached *auth.Identity
		known := false
		if tokOK {
			if id, hit := s.d.Validator.Cached(tok); hit {
				cached = &id
			} else {
				known = s.d.Validator.Known(tok)
			}
		}
		reserved := false
		if cached == nil && !known {
			ok, wait := s.authFail.allow(ipKey)
			if !ok {
				rateLimited(w, r, wait)
				return
			}
			reserved = true
		}
		refund := func() {
			if reserved {
				s.authFail.refund(ipKey)
			}
		}
		// 5
		if !safeMethod(r.Method) && !s.originOK(r) {
			refund() // refused without any oCIS call
			writeCode(w, r, http.StatusForbidden, "origin_rejected")
			return
		}
		// 6: a missing or malformed bearer keeps its reservation
		if !tokOK {
			writeCode(w, r, http.StatusUnauthorized, "unauthenticated")
			return
		}
		var id auth.Identity
		if cached != nil {
			id = *cached
		} else {
			var err error
			if id, err = s.d.Validator.Validate(ctx, tok); err != nil {
				st, code := validatorError(err)
				switch {
				case st == http.StatusUnauthorized:
					if !reserved {
						s.authFail.charge(ipKey) // a known token that oCIS now rejects
					}
				case parent.Err() != nil:
					// the client hung up: the reservation stays charged, or
					// hanging up right away would make validation free (the
					// route's own deadline, a slow oCIS, is refunded below)
				default:
					refund() // an oCIS outage, our deadline or a 422 is not the client's failure
				}
				writeCode(w, r, st, code)
				return
			}
			refund()
		}
		if id.UserID == "" {
			writeCode(w, r, http.StatusBadGateway, "upstream_unavailable")
			return
		}
		p := Principal{UserID: id.UserID, Username: id.Username, DisplayName: id.DisplayName, DriveID: id.DriveID, Subject: id.UserID}
		ri.user = p.UserID
		ctx = context.WithValue(context.WithValue(ctx, principalKey, p), bearerKey, tok)
		r = r.WithContext(ctx)
		// 7
		if ok, wait := s.userLm.allow(p.UserID); !ok {
			rateLimited(w, r, wait)
			return
		}
		// 8: a user who has not enrolled has no row (ErrNotFound): nothing
		// to record, throttled like a success (enrolment records the visit)
		if s.seen.due(p.UserID) {
			switch err := s.d.Auth.Seen(ctx, p.UserID); {
			case err == nil, errors.Is(err, store.ErrNotFound):
				s.seen.done(p.UserID)
			default:
				s.d.Log.Warn("api: recording the visit failed", "category", "seen", "request_id", ri.id, "user", p.UserID)
			}
		}
		// 9
		if err := requireJSON(r); err != nil {
			s.fail(w, r, err)
			return
		}
		if hasBody(r.Method) && r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, rc.maxBody)
		}
		// 10
		if err := h(w, r); err != nil {
			s.fail(w, r, err)
		}
	})
}

// fail writes the mapped error response (if nothing was written yet). Only
// the code is logged (in the request line), never the error's text.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, code, msg := mapError(err)
	if sw, ok := w.(*statusWriter); ok && sw.wrote {
		if ri := infoFrom(r.Context()); ri != nil {
			ri.category = code
		}
		return
	}
	writeError(w, r, status, code, msg)
}

func validatorError(err error) (int, string) {
	switch {
	case errors.Is(err, auth.ErrUnauthenticated):
		return http.StatusUnauthorized, "unauthenticated"
	case errors.Is(err, auth.ErrNoLoginName):
		return http.StatusUnprocessableEntity, "no_login_name"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return http.StatusServiceUnavailable, "unavailable"
	}
	// network, 5xx: oCIS could not tell whether the token is valid
	return http.StatusBadGateway, "upstream_unavailable"
}

func rateLimited(w http.ResponseWriter, r *http.Request, wait time.Duration) {
	secs := int(math.Ceil(wait.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeCode(w, r, http.StatusTooManyRequests, "rate_limited")
}

func safeMethod(m string) bool { return m == http.MethodGet || m == http.MethodHead }

// originOK is step 5: exactly one Origin, byte-equal to the allowed origin
// (config normalises it to lower case without a default port, which is how
// browsers send it), and no Sec-Fetch-Site other than same-origin.
func (s *Server) originOK(r *http.Request) bool {
	o := r.Header.Values("Origin")
	if len(o) != 1 || o[0] != s.d.Cfg.AllowedOrigin {
		return false
	}
	switch sfs := r.Header.Values("Sec-Fetch-Site"); len(sfs) {
	case 0:
		return true
	case 1:
		return sfs[0] == "same-origin"
	}
	return false
}

// parseBearer accepts exactly one Authorization header "Bearer <token>"
// (scheme case-insensitive, one space, a token68 of at most 8 KiB). Anything
// else is refused without calling the validator; in particular Basic auth
// (the agent's app token) is never an authenticator here.
func parseBearer(vals []string) (ocisclient.Secret, bool) {
	if len(vals) != 1 {
		return "", false
	}
	scheme, tok, ok := strings.Cut(vals[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || tok == "" || len(tok) > maxBearerLen || !token68(tok) {
		return "", false
	}
	return ocisclient.Secret(tok), true
}

// token68 reports whether t is a token68 (RFC 9110): ALPHA / DIGIT /
// "-._~+/" followed by optional "=" padding.
func token68(t string) bool {
	pad := false
	for i := 0; i < len(t); i++ {
		c := t[i]
		switch {
		case c == '=':
			pad = true
		case pad:
			return false
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9',
			c == '-', c == '.', c == '_', c == '~', c == '+', c == '/':
		default:
			return false
		}
	}
	return t[0] != '='
}

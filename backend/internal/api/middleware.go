package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"math"
	"net"
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
//     "panic", never the panic value
//  2. request id: 16 random hex digits, X-Request-Id, in every log line and
//     error body; a client-supplied id is ignored
//  3. security headers on every response
//  4. pre-auth rate limit per client IP (RemoteAddr; X-Forwarded-For is
//     never trusted)
//  5. for state-changing methods: Origin must equal the allowed origin and
//     Sec-Fetch-Site, if sent, must be same-origin or same-site; checked
//     before auth, so a cross-site request never reaches token validation
//  6. Authorization: Bearer <token>, validated by oCIS (TokenValidator)
//  7. Seen(userID) once per request; a failure is logged and ignored
//  8. per-user rate limit
//  9. body rules (requireJSON) and the body limit
//  10. the handler, under the route's deadline
//
// Steps 1-3 wrap everything, including unmatched paths (JSON 404/405) and
// GET /agent-api/healthz, which skips 4-9. The route deadline (default 30s)
// starts at step 4: a context deadline plus read and write deadlines on the
// connection through http.ResponseController.

// writeGrace is how much longer the connection's write deadline is than the
// route's context deadline, so a handler that stops at its deadline can
// still write its error response.
const writeGrace = 5 * time.Second

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
// so http.ResponseController reaches the connection through it (handlers
// must use the controller rather than type assertions such as http.Flusher).
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
	defer func() {
		if v := recover(); v != nil {
			if v == http.ErrAbortHandler {
				panic(v)
			}
			s.d.Log.Error("api: panic", "category", "panic", "request_id", ri.id, "route", ri.route)
			if !sw.wrote {
				writeCode(sw, r, http.StatusInternalServerError, "internal")
			}
		}
		status := sw.status
		if !sw.wrote {
			status = http.StatusOK
		}
		s.d.Log.Info("api: request", "request_id", ri.id, "method", logMethod(r.Method), "route", ri.route,
			"status", status, "duration_ms", time.Since(start).Milliseconds(), "user", ri.user, "category", ri.category)
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
		ri.route = pattern

		ctx, cancel := context.WithTimeout(r.Context(), rc.timeout)
		defer cancel()
		// ErrNotSupported where the writer has no connection (httptest
		// recorders); the context deadline still applies
		ctl := http.NewResponseController(w)
		_ = ctl.SetReadDeadline(time.Now().Add(rc.timeout))
		_ = ctl.SetWriteDeadline(time.Now().Add(rc.timeout + writeGrace))
		r = r.WithContext(ctx)

		// 4
		if ok, wait := s.ipLim.allow(clientIP(r)); !ok {
			rateLimited(w, r, wait)
			return
		}
		// 5
		if !safeMethod(r.Method) && !s.originOK(r) {
			writeCode(w, r, http.StatusForbidden, "origin_rejected")
			return
		}
		// 6
		tok, ok := parseBearer(r.Header.Values("Authorization"))
		if !ok {
			writeCode(w, r, http.StatusUnauthorized, "unauthenticated")
			return
		}
		id, err := s.d.Validator.Validate(ctx, tok)
		if err != nil {
			st, code := validatorError(err)
			writeCode(w, r, st, code)
			return
		}
		if id.UserID == "" {
			writeCode(w, r, http.StatusBadGateway, "upstream_unavailable")
			return
		}
		p := Principal{UserID: id.UserID, Username: id.Username, DisplayName: id.DisplayName, DriveID: id.DriveID, Subject: id.UserID}
		ri.user = p.UserID
		ctx = context.WithValue(context.WithValue(ctx, principalKey, p), bearerKey, tok)
		r = r.WithContext(ctx)
		// 7: a user who has not enrolled has no row yet (ErrNotFound)
		if err := s.d.Auth.Seen(ctx, p.UserID); err != nil && !errors.Is(err, store.ErrNotFound) {
			s.d.Log.Warn("api: recording the visit failed", "category", "seen", "request_id", ri.id, "user", p.UserID)
		}
		// 8
		if ok, wait := s.userLm.allow(p.UserID); !ok {
			rateLimited(w, r, wait)
			return
		}
		// 9
		if err := requireJSON(r); err != nil {
			s.fail(w, r, err)
			return
		}
		if hasBody(r.Method) && r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
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

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func safeMethod(m string) bool { return m == http.MethodGet || m == http.MethodHead }

// originOK is step 5: exactly one Origin, equal to the allowed origin, and
// no Sec-Fetch-Site other than same-origin or same-site.
func (s *Server) originOK(r *http.Request) bool {
	o := r.Header.Values("Origin")
	if len(o) != 1 || o[0] != s.d.Cfg.AllowedOrigin {
		return false
	}
	switch sfs := r.Header.Values("Sec-Fetch-Site"); len(sfs) {
	case 0:
		return true
	case 1:
		return sfs[0] == "same-origin" || sfs[0] == "same-site"
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

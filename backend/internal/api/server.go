// Package api is the sidecar's REST API (NewServer). All routes live under
// /agent-api/: GET /agent-api/healthz and the authenticated /agent-api/v1/
// routes. It never serves /agent/, the extension's UI routes.
package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/approvals"
	"github.com/owncloud/ocis-agent/backend/internal/auth"
	"github.com/owncloud/ocis-agent/backend/internal/llm"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/queue"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// TokenValidator checks a bearer token with oCIS (auth.Validator). Cached
// (the identity of a recently accepted token) and Known (oCIS accepted the
// token within the last hours) are lookups without any network call.
type TokenValidator interface {
	Validate(ctx context.Context, token ocisclient.Secret) (auth.Identity, error)
	Cached(token ocisclient.Secret) (auth.Identity, bool)
	Known(token ocisclient.Secret) bool
}

// Credentials is the part of auth.Service the API uses.
type Credentials interface {
	Enroll(ctx context.Context, id auth.Identity, bearer ocisclient.Secret) error
	Unenroll(ctx context.Context, userID string, bearer ocisclient.Secret, deleteMemory bool) error
	Revoke(ctx context.Context, userID string, bearer ocisclient.Secret) (int, error)
	RevokeAll(ctx context.Context, userID string, bearer ocisclient.Secret, confirm bool) (int, error)
	PreviewRevokeAll(ctx context.Context, bearer ocisclient.Secret) ([]auth.TokenSummary, error)
	Renew(ctx context.Context, userID string) (auth.Renewed, error)
	Seen(ctx context.Context, userID string) error
	HandleUnauthorized(ctx context.Context, userID string) error
}

// Decider records the user's approval decisions (approvals.Gate).
type Decider interface {
	Decide(ctx context.Context, userID, decidedBy string, items []approvals.DecisionItem) ([]store.Approval, error)
}

// Waker wakes the workers after a change that makes a job claimable
// (queue.Queue).
type Waker interface{ Wake() }

var (
	_ TokenValidator = (*auth.Validator)(nil)
	_ Credentials    = (*auth.Service)(nil)
	_ Decider        = (*approvals.Gate)(nil)
	_ Waker          = (*queue.Queue)(nil)
)

// Settings are the operator's settings the API applies.
type Settings struct {
	AllowedOrigin   string  // the oCIS Web origin (scheme://host[:port]); state-changing requests must come from it
	KillSwitchEnv   bool    // AGENT_KILL_SWITCH
	UserDailyTokens int64   // operator cap and default of a user's daily token budget
	RatePerSecond   float64 // per-user request rate
	Burst           int     // per-user burst
}

// Deps are the server's collaborators.
type Deps struct {
	Store     store.Store
	Validator TokenValidator
	Auth      Credentials
	Gate      Decider
	Queue     Waker
	Catalog   llm.Catalog // may be empty when no LLM is configured
	Cfg       Settings
	Log       *slog.Logger     // default: discard
	Now       func() time.Time // default: time.Now; the rate limiter's clock
}

// Principal is the authenticated caller, taken only from the identity the
// validator returned for the bearer, never from headers, the query or the
// body.
type Principal struct {
	UserID      string `json:"user_id"`
	Username    string `json:"username"` // login name
	DisplayName string `json:"display_name"`
	DriveID     string `json:"drive_id"`
	// Subject is recorded as decidedBy of approval decisions. auth.Identity
	// has no separate OIDC subject (the validator asks Graph /me, which
	// returns the user id), so Subject == UserID.
	Subject string `json:"subject"`
}

// defaultTimeout is the per-request deadline of a route.
const defaultTimeout = 30 * time.Second

// Server is the sidecar's REST API.
type Server struct {
	d         Deps
	mux       *http.ServeMux
	authFail  *limiter // failed authentications per client IP (/64 for IPv6)
	userLm    *limiter
	seen      *seenThrottle
	verifying verifyLimiter // one audit verification per user
	actions   *cooldown     // enrol, renew, revoke, revoke-all per user
	// baseTimeout is the read and write deadline of every request before a
	// route sets its own (unmatched paths, 405, healthz); a field for tests
	baseTimeout time.Duration
}

// NewServer checks the dependencies and registers the routes.
func NewServer(d Deps) (*Server, error) {
	switch {
	case d.Store == nil:
		return nil, errors.New("api: a store is required")
	case d.Validator == nil:
		return nil, errors.New("api: a token validator is required")
	case d.Auth == nil:
		return nil, errors.New("api: the credentials service is required")
	case d.Gate == nil:
		return nil, errors.New("api: the approval gate is required")
	case d.Queue == nil:
		return nil, errors.New("api: the queue is required")
	case !validOrigin(d.Cfg.AllowedOrigin):
		return nil, errors.New("api: the allowed origin must be scheme://host[:port]")
	case !(d.Cfg.RatePerSecond > 0), d.Cfg.Burst < 1:
		return nil, errors.New("api: the rate limit must be positive")
	case d.Cfg.UserDailyTokens < 1:
		return nil, errors.New("api: the daily token cap must be positive")
	}
	if d.Log == nil {
		d.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	s := &Server{
		d:        d,
		mux:      http.NewServeMux(),
		authFail: newLimiter(d.Cfg.RatePerSecond*ipRateFactor, d.Cfg.Burst*ipBurstFactor, maxLimiterKeys, d.Now),
		userLm:   newLimiter(d.Cfg.RatePerSecond, d.Cfg.Burst, maxLimiterKeys, d.Now),
		seen:     newSeenThrottle(seenEvery, maxLimiterKeys, d.Now),
		actions:  newCooldown(actionCooldown, maxLimiterKeys, d.Now),

		baseTimeout: defaultTimeout,
	}
	// healthz skips steps 4-9 of the pipeline (no rate limit, Origin or auth)
	s.mux.HandleFunc("GET /agent-api/healthz", healthz)
	s.routes()
	return s, nil
}

// routes registers the API's routes.
func (s *Server) routes() {
	s.handle("GET /agent-api/v1/status", s.getStatus)
	s.handle("POST /agent-api/v1/enroll", s.enroll)
	s.handle("POST /agent-api/v1/unenroll", s.unenroll)
	s.handle("DELETE /agent-api/v1/token", s.revoke)
	s.handle("GET /agent-api/v1/token/revoke-all/preview", s.previewRevokeAll)
	s.handle("POST /agent-api/v1/token/revoke-all", s.revokeAll)
	s.handle("POST /agent-api/v1/token/renew", s.renew)
	s.handle("POST /agent-api/v1/pause", s.pause)
	s.handle("POST /agent-api/v1/resume", s.resume)
	s.handle("GET /agent-api/v1/settings", s.getSettings)
	s.handle("PUT /agent-api/v1/settings", s.putSettings)
	s.handle("GET /agent-api/v1/jobs", s.listJobs)
	s.handle("GET /agent-api/v1/jobs/{id}", s.getJob)
	s.handle("POST /agent-api/v1/jobs/{id}/cancel", s.cancelJob)
	s.handle("GET /agent-api/v1/approvals", s.listApprovals)
	s.handle("GET /agent-api/v1/approvals/{id}", s.getApproval)
	s.handle("POST /agent-api/v1/approvals/{id}/decision", s.decideApproval)
	s.handle("POST /agent-api/v1/approvals/bulk-decision", s.bulkDecision)
	s.handle("GET /agent-api/v1/audit", s.listAudit)
	s.handle("GET /agent-api/v1/audit/verify", s.verifyAudit)
	s.handle("GET /agent-api/v1/audit/export", s.exportAudit, withTimeout(auditExportDeadline))
}

func validOrigin(o string) bool {
	u, err := url.Parse(o)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" &&
		u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == "" && !strings.HasSuffix(o, "/") &&
		u.Scheme+"://"+u.Host == o
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
}

// Handler returns the API's HTTP handler: steps 1-3 of the pipeline
// (recover, request id, security headers) around the router.
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serve) }

// handlerFunc is a route handler. A returned error is mapped with mapError
// (it must be returned before anything was written).
type handlerFunc func(w http.ResponseWriter, r *http.Request) error

type routeConfig struct {
	timeout time.Duration
	maxBody int64
}

type routeOption func(*routeConfig)

// withTimeout replaces the route's default 30s deadline.
func withTimeout(d time.Duration) routeOption { return func(c *routeConfig) { c.timeout = d } }

// withMaxBody replaces the route's default body limit (defaultMaxBody).
func withMaxBody(n int64) routeOption { return func(c *routeConfig) { c.maxBody = n } }

// handle registers an authenticated route: steps 4-10 of the pipeline wrap
// h. The pattern must be "METHOD /agent-api/v1/..." without a trailing
// slash (no subtree patterns, so the mux never redirects); a wrong pattern is
// a programming error and panics.
func (s *Server) handle(pattern string, h handlerFunc, opts ...routeOption) {
	method, path, ok := strings.Cut(pattern, " ")
	if !ok || method == "" || strings.ToUpper(method) != method || !strings.HasPrefix(path, "/agent-api/v1/") ||
		strings.HasSuffix(path, "/") || strings.Contains(path, "{$}") {
		panic("api: invalid route pattern " + pattern)
	}
	rc := routeConfig{timeout: defaultTimeout, maxBody: defaultMaxBody}
	for _, o := range opts {
		o(&rc)
	}
	s.mux.Handle(pattern, s.chain(pattern, rc, h))
}

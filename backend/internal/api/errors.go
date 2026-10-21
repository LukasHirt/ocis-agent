package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/approvals"
	"github.com/owncloud/ocis-agent/backend/internal/auth"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// errorMessages are the fixed, client-facing texts per error code. A
// response never carries the text of an underlying error: store, oCIS and
// provider errors can hold paths, SQL or other data that is not the
// client's.
var errorMessages = map[string]string{
	"invalid_request":        "The request is invalid.",
	"unauthenticated":        "Authentication is required.",
	"origin_rejected":        "Cross-origin requests are not allowed.",
	"not_found":              "Not found.",
	"method_not_allowed":     "Method not allowed.",
	"conflict":               "The request conflicts with the current state.",
	"already_exists":         "That file already exists.",
	"precondition_required":  "The If-Match header with the file's ETag is required.",
	"precondition_failed":    "The file changed since you loaded it; reload it.",
	"file_too_large":         "The file is too large to open.",
	"not_text":               "The file is not UTF-8 text.",
	"job_ended":              "The job has already ended.",
	"stale_arguments":        "The action is not the one you were shown; reload it.",
	"already_decided":        "This approval has already been decided.",
	"not_acknowledgeable":    "There is no uncertain outcome to acknowledge for this action.",
	"limit_reached":          "A limit has been reached.",
	"not_enrolled":           "You are not enrolled; enrol first.",
	"budget_exhausted":       "Today's token budget is used up; it resets at midnight UTC.",
	"too_many_pending":       "Too many messages are still waiting for an answer; try again later.",
	"credentials_invalid":    "The agent has no valid credentials for you; enrol again.",
	"expired":                "This has expired.",
	"too_large":              "The request body is too large.",
	"unsupported_media_type": "The request body must be application/json.",
	"no_login_name":          "Your account has no login name, which the agent needs.",
	"rate_limited":           "Too many requests; try again later.",
	"upstream_rejected":      "ownCloud refused the request.",
	"upstream_unavailable":   "ownCloud could not be reached.",
	"unavailable":            "The service is temporarily unavailable.",
	"internal":               "Internal error.",
}

// apiError is an error that already knows its response. Handlers return it
// for failures they classify themselves; the message is always the fixed
// text of the code.
type apiError struct {
	status int
	code   string
}

func (e *apiError) Error() string { return "api: " + e.code }

func newAPIError(status int, code string) *apiError { return &apiError{status: status, code: code} }

// Errors of the request pipeline.
var (
	errTooLarge  = newAPIError(http.StatusRequestEntityTooLarge, "too_large")
	errBadJSON   = newAPIError(http.StatusBadRequest, "invalid_request")
	errMediaType = newAPIError(http.StatusUnsupportedMediaType, "unsupported_media_type")
	errNotFound  = newAPIError(http.StatusNotFound, "not_found")
)

// Errors of memory edits.
var (
	errPreconditionRequired = newAPIError(http.StatusPreconditionRequired, "precondition_required")
	errPreconditionFailed   = newAPIError(http.StatusPreconditionFailed, "precondition_failed")
	errAlreadyExists        = newAPIError(http.StatusConflict, "already_exists")
	errNotText              = newAPIError(http.StatusUnprocessableEntity, "not_text")
	errFileTooLarge         = newAPIError(http.StatusUnprocessableEntity, "file_too_large")
)

// Errors of chat.
var (
	errNotEnrolled     = newAPIError(http.StatusConflict, "not_enrolled")
	errTooManyPending  = newAPIError(http.StatusTooManyRequests, "too_many_pending")
	errBudgetExhausted = newAPIError(http.StatusTooManyRequests, "budget_exhausted")
)

// retryAfter is an error whose response carries Retry-After (whole seconds,
// at least 1). The handler sets the header (retryAfterHeader) before
// returning it to the mapper.
type retryAfter struct {
	*apiError
	wait time.Duration
}

func (e *retryAfter) Unwrap() error { return e.apiError }

// Errors of approval decisions.
var (
	// errStaleArguments: the arguments hash the client echoed is not the
	// stored one (the client shows an outdated approval).
	errStaleArguments = newAPIError(http.StatusConflict, "stale_arguments")
	// errAlreadyDecided: the approval is no longer pending.
	errAlreadyDecided = newAPIError(http.StatusConflict, "already_decided")
	errExpired        = newAPIError(http.StatusGone, "expired")
	// errNotAcknowledgeable: the approval has no uncertain outcome (it is
	// certain, or not settled yet).
	errNotAcknowledgeable = newAPIError(http.StatusConflict, "not_acknowledgeable")
)

var ocisSentinels = []error{
	ocisclient.ErrNotFound, ocisclient.ErrPrecondition, ocisclient.ErrExists, ocisclient.ErrNotDir,
	ocisclient.ErrTooLarge, ocisclient.ErrInvalidPath, ocisclient.ErrNoCredentials,
	ocisclient.ErrNoPersonalDrive, ocisclient.ErrStreamClosed, ocisclient.ErrIdleTimeout,
}

// mapError maps an error to its response. The order matters: specific
// sentinels before the generic ones, and a context error before net.Error
// (context.DeadlineExceeded implements net.Error).
//
// ocisclient.ErrUnauthorized and ErrForbidden come from calls made with the
// user's bearer, which the pipeline validated a moment ago: they are 502
// upstream_rejected, never 401, so the client does not loop into a logout.
func mapError(err error) (status int, code, message string) {
	status, code = classify(err)
	return status, code, errorMessages[code]
}

func classify(err error) (int, string) {
	var ae *apiError
	var mbe *http.MaxBytesError
	var se *ocisclient.StatusError
	var ne net.Error
	switch {
	case errors.As(err, &ae):
		return ae.status, ae.code
	case errors.As(err, &mbe):
		return http.StatusRequestEntityTooLarge, "too_large"
	case errors.Is(err, approvals.ErrJobEnded):
		return http.StatusConflict, "job_ended"
	case errors.Is(err, approvals.ErrActionLimit), errors.Is(err, store.ErrLimit):
		return http.StatusConflict, "limit_reached"
	case errors.Is(err, auth.ErrUnauthenticated):
		return http.StatusUnauthorized, "unauthenticated"
	case errors.Is(err, auth.ErrNoLoginName):
		return http.StatusUnprocessableEntity, "no_login_name"
	case errors.Is(err, auth.ErrCredentialInvalid):
		return http.StatusConflict, "credentials_invalid"
	case errors.Is(err, auth.ErrNotConfirmed):
		return http.StatusBadRequest, "invalid_request"
	case errors.Is(err, auth.ErrRevokeIncomplete):
		// oCIS still listed a token after its delete: nothing local changed, retry
		return http.StatusBadGateway, "upstream_unavailable"
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, store.ErrInvalid):
		return http.StatusBadRequest, "invalid_request"
	case errors.Is(err, store.ErrConflict):
		return http.StatusConflict, "conflict"
	case errors.Is(err, store.ErrExpired):
		return http.StatusGone, "expired"
	case errors.Is(err, ocisclient.ErrUnauthorized), errors.Is(err, ocisclient.ErrForbidden):
		return http.StatusBadGateway, "upstream_rejected"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return http.StatusServiceUnavailable, "unavailable"
	case errors.As(err, &se), errors.As(err, &ne), isOCISSentinel(err):
		return http.StatusBadGateway, "upstream_unavailable"
	}
	return http.StatusInternalServerError, "internal"
}

func isOCISSentinel(err error) bool {
	for _, s := range ocisSentinels {
		if errors.Is(err, s) {
			return true
		}
	}
	return false
}

type errorDoc struct {
	Error errorFields `json:"error"`
}

type errorFields struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

// writeError writes the JSON error document. Callers pass the fixed message
// of the code (errorMessages). A 401 carries WWW-Authenticate: Bearer.
func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	ri := infoFrom(r.Context())
	if ri != nil {
		ri.category = code
	}
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	doc := errorDoc{Error: errorFields{Code: code, Message: message}}
	if ri != nil {
		doc.Error.RequestID = ri.id
	}
	writeJSON(w, status, doc)
}

// writeCode writes the error response of a code with its fixed message.
func writeCode(w http.ResponseWriter, r *http.Request, status int, code string) {
	writeError(w, r, status, code, errorMessages[code])
}

// writeJSON writes v as JSON. A value that cannot be encoded is a 500 with a
// static body (nothing was written yet).
func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		status = http.StatusInternalServerError
		b = []byte(`{"error":{"code":"internal","message":"` + errorMessages["internal"] + `"}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}

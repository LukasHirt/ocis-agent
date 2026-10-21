package ocisclient

import (
	"errors"
	"fmt"
	"net/url"
)

// Sentinels. Test for them with errors.Is; the concrete error is usually a
// *StatusError that also carries the method, path and status.
var (
	ErrUnauthorized = errors.New("ocisclient: unauthorized")
	ErrForbidden    = errors.New("ocisclient: forbidden")
	ErrNotFound     = errors.New("ocisclient: not found")
	ErrPrecondition = errors.New("ocisclient: precondition failed")
	// ErrTooEarly: oCIS still postprocesses a fresh upload (425 to a GET)
	// after the client waited a few seconds for it.
	ErrTooEarly        = errors.New("ocisclient: file still being processed")
	ErrExists          = errors.New("ocisclient: already exists")
	ErrNotDir          = errors.New("ocisclient: not a directory")
	ErrTooLarge        = errors.New("ocisclient: response too large")
	ErrInvalidPath     = errors.New("ocisclient: invalid path")
	ErrNoCredentials   = errors.New("ocisclient: missing credentials")
	ErrNoPersonalDrive = errors.New("ocisclient: no personal drive")
	ErrStreamClosed    = errors.New("ocisclient: event stream closed by the server")
	ErrIdleTimeout     = errors.New("ocisclient: event stream idle timeout")
)

// StatusError is an unexpected HTTP status. Path is the URL path only: query
// strings can carry token hashes and are never recorded. The response body is
// never recorded either.
type StatusError struct {
	Method string
	Path   string
	Status int
	kind   error
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("ocisclient: %s %s: status %d", e.Method, e.Path, e.Status)
}

// Is makes errors.Is(err, ErrNotFound) and friends work.
func (e *StatusError) Is(target error) bool { return e.kind != nil && target == e.kind }

// statusError builds a StatusError. kind overrides the default mapping
// (401, 403 and 404 map to their sentinels); callers pass it where a status
// means something specific for the method, for example 412 on MOVE.
func statusError(method, path string, status int, kind error) *StatusError {
	if kind == nil {
		switch status {
		case 401:
			kind = ErrUnauthorized
		case 403:
			kind = ErrForbidden
		case 404:
			kind = ErrNotFound
		}
	}
	return &StatusError{Method: method, Path: path, Status: status, kind: kind}
}

// transportError wraps a failure of the HTTP round trip. net/http errors embed
// the full URL including the query string, so only the inner error is kept.
func transportError(method, path string, err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return fmt.Errorf("ocisclient: %s %s: %w", method, path, err)
}

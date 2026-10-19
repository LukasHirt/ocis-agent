package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/owncloud/ocis-agent/backend/internal/approvals"
	"github.com/owncloud/ocis-agent/backend/internal/auth"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

type fakeNetErr struct{}

func (fakeNetErr) Error() string   { return "i/o timeout SECRET" }
func (fakeNetErr) Timeout() bool   { return true }
func (fakeNetErr) Temporary() bool { return true }

var _ net.Error = fakeNetErr{}

func TestErrorMappingTable(t *testing.T) {
	w := func(err error) error { return fmt.Errorf("layer: SECRET: %w", err) }
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{store.ErrNotFound, 404, "not_found"},
		{store.ErrInvalid, 400, "invalid_request"},
		{store.ErrConflict, 409, "conflict"},
		{store.ErrExpired, 410, "expired"},
		{approvals.ErrJobEnded, 409, "job_ended"},
		{approvals.ErrActionLimit, 409, "limit_reached"},
		{auth.ErrUnauthenticated, 401, "unauthenticated"},
		{auth.ErrNoLoginName, 422, "no_login_name"},
		{auth.ErrCredentialInvalid, 409, "credentials_invalid"},
		{auth.ErrNotConfirmed, 400, "invalid_request"},
		{auth.ErrRevokeIncomplete, 502, "upstream_unavailable"},
		{ocisclient.ErrUnauthorized, 502, "upstream_rejected"},
		{ocisclient.ErrForbidden, 502, "upstream_rejected"},
		{ocisclient.ErrNotFound, 502, "upstream_unavailable"},
		{ocisclient.ErrPrecondition, 502, "upstream_unavailable"},
		{ocisclient.ErrTooLarge, 502, "upstream_unavailable"},
		{ocisclient.ErrNoPersonalDrive, 502, "upstream_unavailable"},
		{&ocisclient.StatusError{Method: "GET", Path: "/x", Status: 503}, 502, "upstream_unavailable"},
		{fakeNetErr{}, 502, "upstream_unavailable"},
		{context.DeadlineExceeded, 503, "unavailable"},
		{context.Canceled, 503, "unavailable"},
		{&http.MaxBytesError{Limit: 1}, 413, "too_large"},
		{errTooLarge, 413, "too_large"},
		{errBadJSON, 400, "invalid_request"},
		{errMediaType, 415, "unsupported_media_type"},
		{errStaleArguments, 409, "stale_arguments"},
		{errAlreadyDecided, 409, "already_decided"},
		{&apiError{status: 418, code: "rate_limited"}, 418, "rate_limited"},
		{errors.New("SECRET"), 500, "internal"},
		{store.ErrFenceLost, 500, "internal"},
	}
	for _, c := range cases {
		for _, err := range []error{c.err, w(c.err)} {
			st, code, msg := mapError(err)
			if st != c.status || code != c.code {
				t.Errorf("%v: got %d %s, want %d %s", err, st, code, c.status, c.code)
			}
			if msg == "" || msg != errorMessages[code] || strings.Contains(msg, "SECRET") {
				t.Errorf("%v: message %q is not the fixed text", err, msg)
			}
		}
	}
	// a deadline wins over the net.Error interface it also implements
	if st, _, _ := mapError(fmt.Errorf("ocisclient: GET /x: %w", context.DeadlineExceeded)); st != 503 {
		t.Errorf("deadline = %d", st)
	}
}

func TestEveryCodeHasAFixedMessage(t *testing.T) {
	for code, msg := range errorMessages {
		if msg == "" || strings.ContainsAny(msg, "%{}") {
			t.Errorf("%s: %q", code, msg)
		}
	}
}

func TestWriteError(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	writeError(rec, r, 401, "unauthenticated", errorMessages["unauthenticated"])
	if rec.Code != 401 || rec.Header().Get("WWW-Authenticate") != "Bearer" || rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("%d %v", rec.Code, rec.Header())
	}
	var b errBody
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil || b.Error.Code != "unauthenticated" {
		t.Errorf("%s %v", rec.Body.String(), err)
	}
	rec = httptest.NewRecorder()
	writeError(rec, r, 404, "not_found", errorMessages["not_found"])
	if rec.Header().Get("WWW-Authenticate") != "" {
		t.Error("WWW-Authenticate on a 404")
	}
}

func TestWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, 201, map[string]int{"a": 1})
	if rec.Code != 201 || rec.Body.String() != `{"a":1}`+"\n" || rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("%d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}
	rec = httptest.NewRecorder()
	writeJSON(rec, 200, map[string]any{"f": func() {}}) // cannot be encoded
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), `"internal"`) {
		t.Errorf("unencodable: %d %s", rec.Code, rec.Body.String())
	}
}

func TestDecodeBodyStructure(t *testing.T) {
	deep := strings.Repeat(`[`, 40) + strings.Repeat(`]`, 40)
	var many strings.Builder
	many.WriteString("{")
	for i := range 300 {
		if i > 0 {
			many.WriteString(",")
		}
		fmt.Fprintf(&many, `"k%d":1`, i)
	}
	many.WriteString("}")
	for name, body := range map[string]string{
		"deep":          `{"a":` + deep + `}`,
		"many keys":     many.String(),
		"nested dup":    `{"a":{"b":1,"b":2}}`,
		"nested fold":   `{"a":[{"k":1,"K":2}]}`,
		"empty":         "",
		"string":        `"x"`,
		"two objects":   `{} {}`,
		"bad utf8 junk": "{}\x00",
	} {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		var dst map[string]any
		if err := decodeBody(httptest.NewRecorder(), r, &dst); !errors.Is(err, errBadJSON) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"a":{"b":[1,2]}}`))
	var dst map[string]any
	if err := decodeBody(httptest.NewRecorder(), r, &dst); err != nil || dst["a"] == nil {
		t.Errorf("valid: %v %v", dst, err)
	}
}

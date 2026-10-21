package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/owncloud/ocis-agent/backend/internal/approvals"
	"github.com/owncloud/ocis-agent/backend/internal/auth"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// The status column of the error-code table in docs/api.md lists exactly the
// statuses each code is answered with.
func TestAPIDocErrorStatuses(t *testing.T) {
	want := map[string][]int{}
	add := func(code string, status int) {
		if !slices.Contains(want[code], status) {
			want[code] = append(want[code], status)
		}
	}
	// every apiError the handlers and the pipeline return
	for _, e := range []*apiError{
		errTooLarge, errBadJSON, errMediaType, errNotFound, errPreconditionRequired, errPreconditionFailed,
		errAlreadyExists, errNotText, errFileTooLarge, errNotEnrolled, errTooManyPending, errBudgetExhausted,
		errStaleArguments, errAlreadyDecided, errExpired, errNotAcknowledgeable, errDailyChatJobs, errStopping,
		errBadQuery,
	} {
		add(e.code, e.status)
	}
	for _, e := range proactiveRefusals {
		add(e.code, e.status)
	}
	// the mapped sentinels
	for _, err := range []error{
		approvals.ErrJobEnded, approvals.ErrActionLimit, store.ErrLimit, store.ErrNotFound, store.ErrInvalid,
		store.ErrConflict, store.ErrExpired, auth.ErrUnauthenticated, auth.ErrNoLoginName, auth.ErrCredentialInvalid,
		auth.ErrNotConfirmed, auth.ErrRevokeIncomplete, ocisclient.ErrUnauthorized, ocisclient.ErrNotFound,
		ocisclient.ErrTooEarly, context.DeadlineExceeded, &http.MaxBytesError{}, store.ErrFenceLost,
	} {
		st, code, _ := mapError(err)
		add(code, st)
	}
	for _, err := range []error{auth.ErrUnauthenticated, auth.ErrNoLoginName, context.DeadlineExceeded, ocisclient.ErrNotFound} {
		st, code := validatorError(err)
		add(code, st)
	}
	// written directly by the pipeline (writeCode, rateLimited)
	add("origin_rejected", http.StatusForbidden)
	add("method_not_allowed", http.StatusMethodNotAllowed)
	add("rate_limited", http.StatusTooManyRequests)
	add("upstream_unavailable", http.StatusBadGateway)
	add("internal", http.StatusInternalServerError)

	b, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "api.md"))
	if err != nil {
		t.Fatal(err)
	}
	rows := regexp.MustCompile("(?m)^\\| `([a-z_]+)` \\| ([0-9, ]+) \\|").FindAllStringSubmatch(string(b), -1)
	if len(rows) == 0 {
		t.Fatal("no error rows in docs/api.md")
	}
	documented := map[string][]int{}
	for _, m := range rows {
		for _, f := range strings.Split(m[2], ",") {
			n, err := strconv.Atoi(strings.TrimSpace(f))
			if err != nil {
				t.Fatalf("row %s: status %q", m[1], f)
			}
			documented[m[1]] = append(documented[m[1]], n)
		}
	}
	for code := range errorMessages {
		if _, ok := want[code]; !ok {
			t.Errorf("code %s is answered by nothing in this test's list", code)
		}
	}
	for code, sts := range want {
		got := slices.Sorted(slices.Values(documented[code]))
		slices.Sort(sts)
		if !slices.Equal(got, sts) {
			t.Errorf("%s: documented statuses %v, the code answers %v", code, got, sts)
		}
	}
}

package api

import (
	"net/http"
	"testing"
)

// Query strings are parsed strictly: a malformed one or a parameter the
// route does not know is a 400, never silently dropped.
func TestListQueriesAreStrict(t *testing.T) {
	s := newStack(t)
	s.enroll(t, "einstein")
	for _, target := range []string{
		"/agent-api/v1/jobs?state=running;x",
		"/agent-api/v1/jobs?bogus=1",
		"/agent-api/v1/jobs?State=running",
		"/agent-api/v1/jobs?limit=%zz",
		"/agent-api/v1/approvals?state=all;x=1",
		"/agent-api/v1/approvals?user_id=" + marieID,
		"/agent-api/v1/audit?cursor=1;",
		"/agent-api/v1/audit?from=1",
		"/agent-api/v1/audit/export?cursor=1",
		"/agent-api/v1/audit/export?from=1&user_id=" + marieID,
		"/agent-api/v1/audit/verify?x=1",
	} {
		rec := s.as("einstein", http.MethodGet, target, "")
		t.Run(target, func(t *testing.T) { wantError(t, rec, 400, "invalid_request") })
	}
	for _, target := range []string{
		"/agent-api/v1/jobs?state=running&limit=5",
		"/agent-api/v1/approvals?state=all&limit=5",
		"/agent-api/v1/audit?limit=5&tool=files.read&risk=read",
		"/agent-api/v1/audit/export?from=1",
		"/agent-api/v1/audit/verify",
		"/agent-api/v1/jobs?",
	} {
		if rec := s.as("einstein", http.MethodGet, target, ""); rec.Code != 200 {
			t.Errorf("%s: %d %s", target, rec.Code, rec.Body.String())
		}
	}
}

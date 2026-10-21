package api

import (
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// Routes lists every registered pattern exactly once, sorted, and each one
// is what the mux matches for a request of that shape.
func TestRoutesListsTheRegisteredPatterns(t *testing.T) {
	f := newFixture(t)
	routes := f.srv.Routes()
	for _, want := range []string{
		"GET /agent-api/healthz",
		"GET /agent-api/v1/status",
		"POST /agent-api/v1/approvals/{id}/acknowledge",
		"GET /agent-api/v1/events",
	} {
		if !slices.Contains(routes, want) {
			t.Errorf("Routes() lacks %q", want)
		}
	}
	if !slices.IsSorted(routes) || len(slices.Compact(slices.Clone(routes))) != len(routes) {
		t.Fatalf("Routes() is not sorted and unique: %v", routes)
	}
	for _, pat := range routes {
		method, path, _ := strings.Cut(pat, " ")
		target := strings.NewReplacer("{id}", "01J00000000000000000000000", "{name}", "profile.md", "{slug}", "a.md").Replace(path)
		_, got := f.srv.mux.Handler(httptest.NewRequest(method, target, nil))
		if got != pat {
			t.Errorf("%s %s is matched by %q, want %q", method, target, got, pat)
		}
	}
	// a copy: the caller cannot change the server's list
	routes[0] = "changed"
	if f.srv.Routes()[0] == "changed" {
		t.Fatal("Routes() returned the server's own slice")
	}
}

// ErrorCodes is the fixed message of every code the API answers with.
func TestErrorCodesAreTheFixedMessages(t *testing.T) {
	codes := ErrorCodes()
	if len(codes) != len(errorMessages) {
		t.Fatalf("got %d codes, want %d", len(codes), len(errorMessages))
	}
	for code, msg := range errorMessages {
		if codes[code] != msg {
			t.Errorf("%s: %q, want %q", code, codes[code], msg)
		}
	}
	codes["internal"] = "changed"
	if errorMessages["internal"] == "changed" {
		t.Fatal("ErrorCodes() returned the server's own map")
	}
}

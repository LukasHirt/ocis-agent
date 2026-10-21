// Package doccheck_test keeps docs/api.md and docs/deployment.md in step with
// the code: every registered route and error code must be documented in the
// API reference (and nothing else), and every AGENT_* variable the
// configuration reads must be in the deployment guide's table (and nothing
// else).
package doccheck_test

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/owncloud/ocis-agent/backend/internal/api"
	"github.com/owncloud/ocis-agent/backend/internal/approvals"
	"github.com/owncloud/ocis-agent/backend/internal/auth"
	"github.com/owncloud/ocis-agent/backend/internal/config"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/store/sqlite"
)

// readDoc reads a file of docs/ at the repository root (three levels above
// this package).
func readDoc(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", name))
	if err != nil {
		t.Fatalf("read docs/%s: %v", name, err)
	}
	return string(b)
}

// section returns the part of doc from the heading line "## title" to the
// next "## " heading.
func section(t *testing.T, doc, title string) string {
	t.Helper()
	start := strings.Index(doc, "\n## "+title+"\n")
	if start < 0 {
		t.Fatalf("no section %q", title)
	}
	rest := doc[start+len(title)+5:]
	if end := strings.Index(rest, "\n## "); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

func diff(t *testing.T, what string, code, doc []string) {
	t.Helper()
	for _, c := range code {
		if !slices.Contains(doc, c) {
			t.Errorf("%s %q is not documented", what, c)
		}
	}
	for _, d := range doc {
		if !slices.Contains(code, d) {
			t.Errorf("documented %s %q does not exist in the code", what, d)
		}
	}
}

// Each route has a heading "### METHOD /agent-api/..." in docs/api.md, and
// each such heading is a registered route.
func TestAPIDocListsEveryRoute(t *testing.T) {
	doc := readDoc(t, "api.md")
	var documented []string
	for _, m := range regexp.MustCompile("(?m)^### `?([A-Z]+ /agent-api/[^`\\s]*)`?\\s*$").FindAllStringSubmatch(doc, -1) {
		documented = append(documented, m[1])
	}
	if len(documented) == 0 {
		t.Fatal("no route headings in docs/api.md")
	}
	diff(t, "route", newServer(t).Routes(), documented)
}

// The error code table of docs/api.md has one row per code with its fixed
// message, and no other codes.
func TestAPIDocListsEveryErrorCode(t *testing.T) {
	table := section(t, readDoc(t, "api.md"), "Error codes")
	rowRE := regexp.MustCompile("(?m)^\\| `([a-z_]+)` \\|(.*)$")
	codes := api.ErrorCodes()
	var documented []string
	for _, m := range rowRE.FindAllStringSubmatch(table, -1) {
		documented = append(documented, m[1])
		if msg, ok := codes[m[1]]; ok && !strings.Contains(m[2], msg) {
			t.Errorf("the row of %s lacks its message %q", m[1], msg)
		}
	}
	var names []string
	for c := range codes {
		names = append(names, c)
	}
	diff(t, "error code", names, documented)
}

// Every AGENT_* variable config.Load (and config.DatabasePath) reads has a
// row in the configuration table of docs/deployment.md, and every row is a
// variable the configuration reads.
func TestDeploymentDocListsEveryVariable(t *testing.T) {
	read := map[string]bool{}
	valid := map[string]string{
		"AGENT_OCIS_URL":   "https://ocis.example.com",
		"AGENT_MASTER_KEY": base64.StdEncoding.EncodeToString(make([]byte, 32)),
	}
	getenv := func(name string) string {
		read[name] = true
		return valid[name]
	}
	if _, err := config.Load(getenv); err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	config.DatabasePath(getenv)
	var names []string
	for n := range read {
		if strings.HasPrefix(n, "AGENT_") {
			names = append(names, n)
		}
	}
	if len(names) < 20 {
		t.Fatalf("only %d variables recorded: %v", len(names), names)
	}

	table := section(t, readDoc(t, "deployment.md"), "Configuration")
	var documented []string
	for _, m := range regexp.MustCompile("(?m)^\\| `(AGENT_[A-Z0-9_]+)` \\|").FindAllStringSubmatch(table, -1) {
		documented = append(documented, m[1])
	}
	diff(t, "variable", names, documented)
}

// newServer builds an API server over a fresh store; the other
// collaborators are never called (only the route list is read).
func newServer(t *testing.T) *api.Server {
	t.Helper()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s, err := api.NewServer(api.Deps{
		Store: st, Validator: stub{}, Auth: stub{}, Gate: stub{}, Queue: stub{},
		Cfg: api.Settings{AllowedOrigin: "https://ocis.example.com", UserDailyTokens: 1000, RatePerSecond: 1, Burst: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// stub satisfies the server's collaborator interfaces; no method is called.
type stub struct{}

func (stub) Validate(context.Context, ocisclient.Secret) (auth.Identity, error) {
	return auth.Identity{}, auth.ErrUnauthenticated
}
func (stub) Cached(ocisclient.Secret) (auth.Identity, bool)                  { return auth.Identity{}, false }
func (stub) Known(ocisclient.Secret) bool                                    { return false }
func (stub) Enroll(context.Context, auth.Identity, ocisclient.Secret) error  { return nil }
func (stub) Unenroll(context.Context, string, ocisclient.Secret, bool) error { return nil }
func (stub) Revoke(context.Context, string, ocisclient.Secret) (int, error)  { return 0, nil }
func (stub) Renew(context.Context, string) (auth.Renewed, error)             { return auth.Renewed{}, nil }
func (stub) Seen(context.Context, string) error                              { return nil }
func (stub) Wake()                                                           {}
func (stub) RevokeAll(context.Context, string, ocisclient.Secret, bool) (int, error) {
	return 0, nil
}
func (stub) PreviewRevokeAll(context.Context, ocisclient.Secret) ([]auth.TokenSummary, error) {
	return nil, nil
}
func (stub) Decide(context.Context, string, string, []approvals.DecisionItem) ([]store.Approval, error) {
	return nil, nil
}
func (stub) Acknowledge(context.Context, string, string, string) (store.Approval, error) {
	return store.Approval{}, nil
}

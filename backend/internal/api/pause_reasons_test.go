package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// Every store.Pause* reason has an explicit decision here: does POST /pause
// keep it (automatic pauses that only re-enrolment or the sweep clear) or
// replace it with the user's own pause? A new reason in the store fails this
// test until it is decided.
func TestEveryPauseReasonHasAPauseDecision(t *testing.T) {
	decided := map[string]bool{ // reason -> keepsPause
		store.PauseUser:              false,
		store.PauseDormant:           false,
		store.PauseTokenAnomaly:      true,
		store.PauseCredentialInvalid: true,
		store.PauseCredentialRevoked: true,
		store.PauseCredentialExpired: true,
	}
	fset := token.NewFileSet()
	names, err := filepath.Glob("../store/*.go")
	if err != nil || len(names) == 0 {
		t.Fatalf("store sources: %v", err)
	}
	found := 0
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			vs, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "Pause") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				reason, _ := strconv.Unquote(lit.Value)
				found++
				keep, ok := decided[reason]
				if !ok {
					t.Errorf("store.%s (%q) has no decision: add it here and to keepsPause", name.Name, reason)
					continue
				}
				if keepsPause(reason) != keep {
					t.Errorf("keepsPause(%q) = %v, want %v", reason, !keep, keep)
				}
				if !store.ValidPauseReason(reason) {
					t.Errorf("%q is not a valid pause reason", reason)
				}
			}
			return true
		})
	}
	if found != len(decided) {
		t.Errorf("found %d pause reasons in the store, decided %d", found, len(decided))
	}
}

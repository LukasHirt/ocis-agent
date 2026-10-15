package approvals

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/tools"
)

func TestConfigDefaults(t *testing.T) {
	c := Config{}.withDefaults()
	if c.ApprovalTTL != 168*time.Hour || c.MaxArgsBytes != 2<<20 || c.MaxActionsPerJob != 50 || c.RationaleMax != 1000 || c.RetrySettle != 2*time.Minute || c.Now == nil {
		t.Fatalf("defaults = %+v", c)
	}
	if _, err := NewGate(nil, nil, Config{}); err == nil {
		t.Fatal("a gate needs a store and a registry")
	}
}

func TestClassificationOfTheDefaultTools(t *testing.T) {
	f := newFixture(t)
	f.fake.PutFile(drive, "/docs/a.md", []byte("x"))
	f.fake.PutFile(drive, "/.ocis-agent/memory/profile.md", []byte("p"))
	c := f.claim()
	cases := []struct {
		tool, args string
		want       Kind
	}{
		{"files.list_spaces", `{}`, KindAllow},
		{"files.list", `{"path":"/docs"}`, KindAllow},
		{"files.stat", `{"path":"/docs/a.md"}`, KindAllow},
		{"files.read_text", `{"path":"/docs/a.md"}`, KindAllow},
		{"memory.list", `{}`, KindAllow},
		{"memory.read", `{"name":"profile.md"}`, KindAllow},
		{"memory.write", `{"name":"notes/x.md","content":"hi"}`, KindAllow},
		{"files.create_folder", `{"path":"/docs/new"}`, KindApprove},
		{"files.write_new", `{"path":"/docs/new.md","content":"x"}`, KindApprove},
		{"files.move", `{"from":"/docs/a.md","to":"/docs/b.md"}`, KindApprove},
		{"files.copy", `{"from":"/docs/a.md","to":"/docs/c.md"}`, KindApprove},
		{"files.overwrite", `{"path":"/docs/a.md","content":"y"}`, KindApprove},
		{"files.delete", `{"path":"/docs/a.md"}`, KindApprove},
	}
	for i, tc := range cases {
		d := f.classify(c, call(fmt.Sprintf("c%d", i), tc.tool, tc.args))
		if d.Kind != tc.want {
			t.Errorf("%s %s: kind %v (%s), want %v", tc.tool, tc.args, d.Kind, d.Reason, tc.want)
		}
	}
}

func TestDenials(t *testing.T) {
	f := newFixture(t)
	f.fake.PutFile(drive, "/.ocis-agent/memory/profile.md", []byte("secret"))
	c := f.claim()
	for name, cl := range map[string]Call{
		"unknown tool":      call("c1", "shell.exec", `{}`),
		"invalid arguments": call("c2", "files.list", `{"path":"relative"}`),
		"unknown field":     call("c3", "files.list", `{"path":"/","drive":"x"}`),
		"not an object":     call("c4", "files.list", `[]`),
		"duplicate keys":    call("c5", "files.list", `{"path":"/a","PATH":"/b"}`),
		"reserved path":     call("c6", "files.delete", `{"path":"/.ocis-agent/memory/profile.md"}`),
		"missing target":    call("c7", "files.delete", `{"path":"/nope"}`),
	} {
		d, err := f.gate.Classify(bg, c, f.env, cl)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if d.Kind != KindDeny || d.Reason == "" {
			t.Errorf("%s: decision %+v, want a denial with a reason", name, d)
		}
	}
	if data, _, _ := f.fake.File(drive, "/.ocis-agent/memory/profile.md"); string(data) != "secret" {
		t.Fatal("a denial must not touch anything")
	}
}

func TestOversizedArgumentsAreDeniedBeforeParsing(t *testing.T) {
	f := newFixture(t)
	f.gate.cfg.MaxArgsBytes = 100
	c := f.claim()
	// malformed on purpose: only a size check that runs before Canonicalize
	// can produce this reason
	big := `{` + strings.Repeat("x", 200)
	d := f.classify(c, call("c1", "files.write_new", big))
	if d.Kind != KindDeny || d.Reason != "the arguments are too large" {
		t.Fatalf("decision = %+v", d)
	}
	// a 1 MiB write fits the default limit
	f.gate.cfg.MaxArgsBytes = 2 << 20
	ok := `{"path":"/big.md","content":"` + strings.Repeat("x", 1<<20) + `"}`
	if d := f.classify(c, call("c2", "files.write_new", ok)); d.Kind != KindApprove {
		t.Fatalf("a maximal write must still be classifiable: %+v", d)
	}
}

func TestBadCallIDIsACallerBug(t *testing.T) {
	f := newFixture(t)
	c := f.claim()
	// the id check runs before the size check and the tool lookup
	f.gate.cfg.MaxArgsBytes = 10
	if _, err := f.gate.Classify(bg, c, f.env, call("a:b", "no.such.tool", strings.Repeat("x", 100))); !errors.Is(err, ErrBadCall) {
		t.Fatalf("err = %v, want ErrBadCall before any other check", err)
	}
	f.gate.cfg.MaxArgsBytes = 2 << 20
	for _, id := range []string{"", "a:b", "a b", strings.Repeat("x", 65), "ü", "a/b"} {
		if _, err := f.gate.Classify(bg, c, f.env, call(id, "files.list_spaces", `{}`)); !errors.Is(err, ErrBadCall) {
			t.Errorf("id %q: err = %v, want ErrBadCall", id, err)
		}
	}
}

func TestDecisionCarriesTheCanonicalArgumentsAndTheDerivedKeys(t *testing.T) {
	f := newFixture(t)
	f.fake.PutFile(drive, "/docs/a.md", []byte("x"))
	c := f.claim()
	d := f.classify(c, call("c1", "files.overwrite", `{ "content" : "new", "path":"/docs/a.md" }`))
	if d.Kind != KindApprove || d.Risk != tools.RiskDestructive {
		t.Fatalf("decision = %+v", d)
	}
	if string(d.Args) != `{"content":"new","path":"/docs/a.md"}` {
		t.Fatalf("args = %s, want the canonical form", d.Args)
	}
	want, err := tools.ArgsHash(json.RawMessage(`{"path":"/docs/a.md","content":"new"}`))
	if err != nil || d.ArgsHash != want {
		t.Fatalf("hash = %q, want %q (%v)", d.ArgsHash, want, err)
	}
	if d.IdemKey != tools.IdemKey(c.Job.ID, "c1", want) {
		t.Fatalf("idem key = %q", d.IdemKey)
	}
	if d.Prepared.TargetETag == "" || len(d.Prepared.Preview) == 0 {
		t.Fatalf("the decision must carry the preview and the target ETag: %+v", d.Prepared)
	}
}

func TestAutoApproveWriteIsNeverRead(t *testing.T) {
	f := newFixture(t)
	if err := f.st.InTx(bg, func(tx store.Tx) error {
		p, err := tx.GetPolicy(userID)
		if err != nil {
			return err
		}
		p.AutoApproveWrite = true
		return tx.PutPolicy(p)
	}); err != nil {
		t.Fatal(err)
	}
	c := f.claim()
	if d := f.classify(c, call("c1", "files.create_folder", `{"path":"/docs"}`)); d.Kind != KindApprove {
		t.Fatalf("auto_approve_write must not change anything: %+v", d)
	}
}

func TestAutoAllowedIsOnlyHonouredForWriteRisk(t *testing.T) {
	f := newFixture(t)
	c := f.claim()
	if d := f.classify(c, call("c1", "probe.auto", `{}`)); d.Kind != KindAllow {
		t.Fatalf("an auto-allowed write must be allowed: %+v", d)
	}
	f.probes["probe.destroy"].auto = true
	if d := f.classify(c, call("c2", "probe.destroy", `{}`)); d.Kind != KindApprove {
		t.Fatalf("a destructive tool must need an approval even if it claims AutoAllowed: %+v", d)
	}
	if d := f.classify(c, call("c3", "probe.write", `{}`)); d.Kind != KindApprove {
		t.Fatalf("a plain write needs an approval: %+v", d)
	}
}

func TestPrepareErrorsThatAreNotRefusalsAreReturned(t *testing.T) {
	f := newFixture(t)
	f.probes["probe.write"].prep = func() (tools.Prepared, error) {
		return tools.Prepared{}, fmt.Errorf("tools: stat: %w", ocisclient.ErrUnauthorized)
	}
	c := f.claim()
	_, err := f.gate.Classify(bg, c, f.env, call("c1", "probe.write", `{}`))
	if !errors.Is(err, ocisclient.ErrUnauthorized) {
		t.Fatalf("err = %v, want the 401 passed through for the runner", err)
	}
}

func TestPerJobActionCap(t *testing.T) {
	f := newFixture(t)
	f.gate.cfg.MaxActionsPerJob = 2
	c := f.claim()
	if d := f.classify(c, call("first", "probe.write", `{}`)); d.Kind != KindApprove {
		t.Fatalf("below the cap: %+v", d)
	}
	for i := 0; i < 3; i++ {
		if err := f.st.InTx(bg, func(tx store.Tx) error {
			_, err := tx.InsertApproval(store.Approval{
				UserID: userID, JobID: c.Job.ID, CallID: fmt.Sprintf("a%d", i), Tool: "probe.write", Risk: "write",
				Args: json.RawMessage(`{}`), ArgsHash: "sha256:x", IdemKey: fmt.Sprintf("k%d", i),
				ExpiresAt: f.clk.Now().Add(time.Hour),
			}, c.Fence)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	// 3 live approvals >= cap 2: the next approval-needing call is denied
	d := f.classify(c, call("z1", "probe.write", `{"n":99}`))
	if d.Kind != KindDeny || !strings.Contains(d.Reason, "limit") {
		t.Fatalf("decision = %+v", d)
	}
	// reads and memory writes are not affected
	if d := f.classify(c, call("z2", "probe.read", `{}`)); d.Kind != KindAllow {
		t.Fatalf("a read must still be allowed: %+v", d)
	}
	if d := f.classify(c, call("z3", "probe.auto", `{}`)); d.Kind != KindAllow {
		t.Fatalf("an auto-allowed write must still be allowed: %+v", d)
	}
}

func TestClassifyNeverWritesToOCIS(t *testing.T) {
	f := newFixture(t)
	f.fake.PutFile(drive, "/docs/a.md", []byte("x"))
	c := f.claim()
	f.fake.ResetRequests()
	for _, cl := range []Call{
		call("c1", "files.overwrite", `{"path":"/docs/a.md","content":"y"}`),
		call("c2", "files.delete", `{"path":"/docs/a.md"}`),
		call("c3", "files.move", `{"from":"/docs/a.md","to":"/docs/b.md"}`),
		call("c4", "files.write_new", `{"path":"/docs/n.md","content":"y"}`),
		call("c5", "memory.write", `{"name":"profile.md","content":"hi"}`),
	} {
		f.classify(c, cl)
	}
	for _, r := range f.fake.Requests() {
		switch r.Method {
		case "PUT", "MKCOL", "MOVE", "COPY", "DELETE":
			t.Fatalf("Classify sent %s %s", r.Method, r.Path)
		}
	}
}

func TestToolSeesOnlyTheCanonicalArguments(t *testing.T) {
	f := newFixture(t)
	c := f.claim()
	for i, raw := range []string{` { "b" : 2,  "a" : 1 } `, `{"a":1,"b":2}`, "\n{\"b\":2,\t\"a\":1}\n"} {
		d := f.classify(c, call(fmt.Sprintf("c%d", i), "probe.write", raw))
		if string(d.Args) != `{"a":1,"b":2}` {
			t.Fatalf("args = %s", d.Args)
		}
	}
	prep, risk := f.probes["probe.write"].seen()
	if len(prep) != 3 || len(risk) != 3 {
		t.Fatalf("prepare saw %d calls, risk %d", len(prep), len(risk))
	}
	for i := range prep {
		if string(prep[i]) != `{"a":1,"b":2}` || string(risk[i]) != `{"a":1,"b":2}` {
			t.Fatalf("call %d: prepare %s, risk %s, want the canonical bytes", i, prep[i], risk[i])
		}
	}
}

func TestMaxActionsPerJobAbove500IsRejected(t *testing.T) {
	f := newFixture(t)
	_, err := NewGate(f.st, f.reg, Config{MaxActionsPerJob: 501})
	if err == nil || err.Error() != "approvals: MaxActionsPerJob must be at most 500" {
		t.Fatalf("err = %v", err)
	}
	if _, err := NewGate(f.st, f.reg, Config{MaxActionsPerJob: 500}); err != nil {
		t.Fatal(err)
	}
}

// seedApprovals inserts n pending approvals for the claimed job and returns them.
func (f *fixture) seedApprovals(c store.Claim, prefix string, n int) []store.Approval {
	f.t.Helper()
	var out []store.Approval
	if err := f.st.InTx(bg, func(tx store.Tx) error {
		for i := 0; i < n; i++ {
			a, err := tx.InsertApproval(store.Approval{
				UserID: userID, JobID: c.Job.ID, CallID: fmt.Sprintf("%s%d", prefix, i), Tool: "probe.write", Risk: "write",
				Args: json.RawMessage(`{}`), ArgsHash: "sha256:x", IdemKey: fmt.Sprintf("%s-k%d", prefix, i),
				ExpiresAt: f.clk.Now().Add(time.Hour),
			}, c.Fence)
			if err != nil {
				return err
			}
			out = append(out, a)
		}
		return nil
	}); err != nil {
		f.t.Fatal(err)
	}
	return out
}

func (f *fixture) reject(a store.Approval) {
	f.t.Helper()
	if err := f.st.InTx(bg, func(tx store.Tx) error {
		_, err := tx.DecideApproval(userID, a.ID, store.ApprovalRejected, userID, a.ArgsHash)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
}

func TestActionCapBoundary(t *testing.T) {
	f := newFixture(t)
	f.gate.cfg.MaxActionsPerJob = 2
	c := f.claim()
	f.seedApprovals(c, "a", 1)
	if d := f.classify(c, call("c1", "probe.write", `{}`)); d.Kind != KindApprove {
		t.Fatalf("1 live approval, cap 2: %+v", d)
	}
	f.seedApprovals(c, "b", 1)
	if d := f.classify(c, call("c2", "probe.write", `{}`)); d.Kind != KindDeny {
		t.Fatalf("2 live approvals, cap 2: %+v", d)
	}
}

func TestRejectedAndExpiredApprovalsDoNotCount(t *testing.T) {
	f := newFixture(t)
	f.gate.cfg.MaxActionsPerJob = 2
	c := f.claim()
	live := f.seedApprovals(c, "live", 1)
	f.reject(f.seedApprovals(c, "rej", 1)[0])
	// the last one expires: advance past its expiry, but keep the live one
	// alive by deciding it first
	exp := f.seedApprovals(c, "exp", 1)[0]
	if err := f.st.InTx(bg, func(tx store.Tx) error {
		_, err := tx.DecideApproval(userID, live[0].ID, store.ApprovalApproved, userID, live[0].ArgsHash)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.clk.Advance(2 * time.Hour)
	if err := f.st.InTx(bg, func(tx store.Tx) error {
		refs, err := tx.ExpireApprovals()
		if err == nil && len(refs) != 1 {
			t.Errorf("expired %d approvals, want 1 (%s)", len(refs), exp.ID)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if d := f.classify(c, call("c1", "probe.write", `{}`)); d.Kind != KindApprove {
		t.Fatalf("1 live + 1 rejected + 1 expired, cap 2: %+v", d)
	}
}

func TestLiveApprovalsAreCountedBeyondTheListWindow(t *testing.T) {
	f := newFixture(t)
	f.gate.cfg.MaxActionsPerJob = 5
	c := f.claim()
	// 4 live approvals, then 505 newer rejected ones: the live rows fall out
	// of a newest-first window of 500 unless rejected rows are filtered by the query
	f.seedApprovals(c, "live", 4)
	if d := f.classify(c, call("c0", "probe.write", `{}`)); d.Kind != KindApprove {
		t.Fatalf("4 live approvals, cap 5: %+v", d)
	}
	rejected := f.seedApprovals(c, "rej", 505)
	if err := f.st.InTx(bg, func(tx store.Tx) error {
		for _, a := range rejected {
			if _, err := tx.DecideApproval(userID, a.ID, store.ApprovalRejected, userID, a.ArgsHash); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if d := f.classify(c, call("c1", "probe.write", `{}`)); d.Kind != KindApprove {
		t.Fatalf("4 live approvals behind 505 rejected ones, cap 5: %+v", d)
	}
	f.seedApprovals(c, "more", 1)
	if d := f.classify(c, call("c2", "probe.write", `{}`)); d.Kind != KindDeny {
		t.Fatalf("5 live approvals behind 505 rejected ones, cap 5: %+v", d)
	}
}

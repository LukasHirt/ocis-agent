package tools

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/owncloud/ocis-agent/backend/internal/memory"
)

func memFixture(t *testing.T) fixture {
	t.Helper()
	f := newFixture(t)
	if err := f.env.Memory.Ensure(bg, f.env.session()); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestMemoryWriteIsAutoAllowedAndTheOthersAreNot(t *testing.T) {
	if !NewMemoryWrite().AutoAllowed() || NewMemoryRead().AutoAllowed() || NewMemoryList().AutoAllowed() {
		t.Fatal("only memory.write may run without an approval")
	}
	if NewMemoryWrite().Risk(nil) != RiskWrite || NewMemoryRead().Risk(nil) != RiskRead || NewMemoryList().Risk(nil) != RiskRead {
		t.Fatal("unexpected risks")
	}
}

func TestMemoryWriteCreateThenReadThenReplace(t *testing.T) {
	f := memFixture(t)
	res, err := f.run(t, NewMemoryWrite(), `{"name":"profile.md","content":"# Me\n"}`)
	if err != nil || res.Summary != "wrote the memory file (5 bytes)" || res.Diff != "+# Me\n" {
		t.Fatalf("create: %+v %v", res, err)
	}
	// after the service cached the ETag of the write, a replace works
	res, err = f.run(t, NewMemoryWrite(), `{"name":"profile.md","content":"# Me\nlikes tea\n","mode":"replace"}`)
	if err != nil || res.Diff != "+likes tea\n" {
		t.Fatalf("replace: %+v %v", res, err)
	}
	rd, err := f.run(t, NewMemoryRead(), `{"name":"profile.md"}`)
	if err != nil || len(rd.Blocks) != 1 || rd.Blocks[0].Source != "memory" || rd.Blocks[0].Content != "# Me\nlikes tea\n" || rd.Blocks[0].SHA256 != sha256Hex([]byte(rd.Blocks[0].Content)) {
		t.Fatalf("read: %+v %v", rd, err)
	}
	if strings.Contains(rd.Summary, "likes tea") {
		t.Fatal("memory content must not reach the summary")
	}
}

func TestMemoryReplaceNeedsAReadAndExplainsWhat(t *testing.T) {
	f := memFixture(t)
	f.put("/.ocis-agent/memory/profile.md", "written by the user\n")
	_, err := f.run(t, NewMemoryWrite(), `{"name":"profile.md","content":"overwrite"}`)
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "memory.read") || !strings.Contains(err.Error(), "append") {
		t.Fatalf("err = %v, want a refusal that points at memory.read and append", err)
	}
	if data, _, _ := f.fake.File(testDrive, "/.ocis-agent/memory/profile.md"); string(data) != "written by the user\n" {
		t.Fatal("nothing may be overwritten blind")
	}
	// after reading, the replace goes through; a user edit in between is a conflict
	if _, err := f.run(t, NewMemoryRead(), `{"name":"profile.md"}`); err != nil {
		t.Fatal(err)
	}
	f.put("/.ocis-agent/memory/profile.md", "edited meanwhile\n")
	_, err = f.run(t, NewMemoryWrite(), `{"name":"profile.md","content":"mine"}`)
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("err = %v", err)
	}
	if data, _, _ := f.fake.File(testDrive, "/.ocis-agent/memory/profile.md"); string(data) != "edited meanwhile\n" {
		t.Fatal("the user's edit must survive")
	}
}

func TestMemoryAppendKeepsTheUsersEdit(t *testing.T) {
	f := memFixture(t)
	f.put("/.ocis-agent/memory/projects.md", "user line\n")
	res, err := f.run(t, NewMemoryWrite(), `{"name":"projects.md","content":"## Agent note\nhello","mode":"append"}`)
	if err != nil || res.Summary != "appended to the memory file" {
		t.Fatalf("res = %+v %v", res, err)
	}
	if data, _, _ := f.fake.File(testDrive, "/.ocis-agent/memory/projects.md"); string(data) != "user line\n\n## Agent note\nhello\n" {
		t.Fatalf("content = %q", data)
	}
	if !strings.Contains(res.Diff, "+## Agent note") {
		t.Fatalf("diff = %q", res.Diff)
	}
}

func TestMemoryToolsValidateNamesModesAndSizes(t *testing.T) {
	f := memFixture(t)
	for _, args := range []string{
		`{"name":"../x.md","content":"x"}`, `{"name":"other.md","content":"x"}`, `{"name":"notes/A.md","content":"x"}`,
		`{"name":"profile.md","content":"x","mode":"delete"}`, `{"name":"profile.md","content":"x","path":"/a"}`,
		`{"name":"profile.md","content":"a\u0000b"}`, `{"name":"profile.md","content":"   ","mode":"append"}`, `{"name":"profile.md"}`,
	} {
		if _, err := NewMemoryWrite().Prepare(bg, f.env, json.RawMessage(args)); !errors.Is(err, ErrInvalidArgs) {
			t.Errorf("write %s: err = %v, want ErrInvalidArgs", args, err)
		}
	}
	for _, args := range []string{`{"name":"../x.md"}`, `{"name":"x"}`, `{}`} {
		if _, err := NewMemoryRead().Prepare(bg, f.env, json.RawMessage(args)); !errors.Is(err, ErrInvalidArgs) {
			t.Errorf("read %s: err = %v", args, err)
		}
	}
	if _, err := NewMemoryList().Prepare(bg, f.env, json.RawMessage(`{"x":1}`)); !errors.Is(err, ErrInvalidArgs) {
		t.Errorf("list with an argument: %v", err)
	}
	if _, err := f.run(t, NewMemoryRead(), `{"name":"projects.md"}`); !errors.Is(err, ErrRefused) {
		t.Errorf("reading a missing file = %v, want ErrRefused", err)
	}
}

func TestMemoryWriteBudgetAndSizeLimitsAreRefusals(t *testing.T) {
	f := memFixture(t)
	f.env.MemoryWrites = memory.NewBudget(10)
	if _, err := f.run(t, NewMemoryWrite(), `{"name":"profile.md","content":"12345678"}`); err != nil {
		t.Fatal(err)
	}
	_, err := f.run(t, NewMemoryWrite(), `{"name":"projects.md","content":"12345678"}`)
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("err = %v, want a budget refusal", err)
	}
	if _, _, ok := f.fake.File(testDrive, "/.ocis-agent/memory/projects.md"); ok {
		t.Fatal("a refused write must not reach oCIS")
	}
}

func TestMemoryListAndReadUseBlocksAndTheReadBudget(t *testing.T) {
	f := memFixture(t)
	f.put("/.ocis-agent/memory/profile.md", "p\n")
	f.put("/.ocis-agent/memory/notes/trip.md", "t\n")
	res, err := f.run(t, NewMemoryList(), `{}`)
	if err != nil || res.Summary != "2 memory file(s)" || len(res.Blocks) != 1 || res.Blocks[0].Source != "memory" {
		t.Fatalf("list: %+v %v", res, err)
	}
	if !strings.Contains(res.Blocks[0].Content, "notes/trip.md") || strings.Contains(res.Summary, "trip") {
		t.Fatalf("content = %q summary = %q", res.Blocks[0].Content, res.Summary)
	}
	f.env.Reads = NewReadBudget(1)
	if _, err := f.run(t, NewMemoryRead(), `{"name":"profile.md"}`); !errors.Is(err, ErrRefused) {
		t.Fatalf("read over the job budget = %v, want ErrRefused", err)
	}
}

func TestMemoryToolsOnlyTouchTheMemoryFolder(t *testing.T) {
	f := memFixture(t)
	f.put("/docs/other.md", "x")
	f.fake.ResetRequests()
	for _, c := range []struct {
		tool Tool
		args string
	}{
		{NewMemoryWrite(), `{"name":"notes/a.md","content":"x"}`},
		{NewMemoryRead(), `{"name":"notes/a.md"}`},
		{NewMemoryList(), `{}`},
	} {
		if _, err := f.run(t, c.tool, c.args); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range f.fake.Requests() {
		if strings.Contains(r.Path, "/remote.php/") && !strings.Contains(r.Path, "/.ocis-agent") {
			t.Fatalf("a memory tool touched %s %s", r.Method, r.Path)
		}
	}
}

func TestMemoryWriteReconcile(t *testing.T) {
	f := memFixture(t)
	f.put("/.ocis-agent/memory/profile.md", "wanted\n")
	tool := NewMemoryWrite().(Reconciler)
	rec := func(args string) Reconciled {
		r, err := tool.Reconcile(bg, f.env, json.RawMessage(args), ExecInfo{})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := rec(`{"name":"profile.md","content":"wanted\n"}`); r.State != ReconDone {
		t.Fatalf("already applied: %+v", r)
	}
	if r := rec(`{"name":"profile.md","content":"other\n"}`); r.State != ReconRetry {
		t.Fatalf("not applied: %+v", r)
	}
	if r := rec(`{"name":"projects.md","content":"x"}`); r.State != ReconRetry {
		t.Fatalf("absent: %+v", r)
	}
	if r := rec(`{"name":"profile.md","content":"x","mode":"append"}`); r.State != ReconUnknown {
		t.Fatalf("append: %+v, want unknown (it may or may not have been appended)", r)
	}
}

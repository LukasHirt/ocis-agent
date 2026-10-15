package tools

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/owncloud/ocis-agent/backend/internal/ocisclient/fakeocis"
)

func writeRequests(fake *fakeocis.Server) []string {
	var m []string
	for _, r := range fake.Requests() {
		switch r.Method {
		case "PUT", "MKCOL", "MOVE", "COPY", "DELETE":
			m = append(m, r.Method+" "+r.Path)
		}
	}
	return m
}

func reconcile(t *testing.T, f fixture, tool Tool, args string) Reconciled {
	t.Helper()
	r, ok := tool.(Reconciler)
	if !ok {
		t.Fatalf("%s must implement Reconciler", tool.Name())
	}
	got, err := r.Reconcile(bg, f.env, json.RawMessage(args), ExecInfo{IdemKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestWriteToolsRefuseReservedAndRootPathsBeforeAnyWrite(t *testing.T) {
	f := newFixture(t)
	f.put("/a/file.md", "x")
	f.put("/.ocis-agent/memory/profile.md", "secret")
	cases := []struct {
		tool Tool
		args string
	}{
		{NewCreateFolder(), `{"path":"/.ocis-agent/x"}`},
		{NewCreateFolder(), `{"path":"/.OCIS-Agent/x"}`},
		{NewCreateFolder(), `{"path":"/"}`},
		{NewCreateFolder(), `{"path":"//.ocis-agent/x"}`},
		{NewCreateFolder(), `{"path":"/.ocis-agent/"}`},
		{NewWriteNew(), `{"path":"/.OCIS-agent/memory","content":"x"}`},
		{NewWriteNew(), `{"path":"/.ocis-agent/memory/new.md","content":"x"}`},
		{NewWriteNew(), `{"path":"/.OCIS-AGENT","content":"x"}`},
		{NewWriteNew(), `{"path":"/","content":"x"}`},
		{NewMove(), `{"from":"/a/file.md","to":"/.ocis-agent/file.md"}`},
		{NewMove(), `{"from":"/.ocis-agent/memory/profile.md","to":"/stolen.md"}`},
		{NewMove(), `{"from":"/.OCIS-agent","to":"/stolen"}`},
		{NewCopy(), `{"from":"/a/file.md","to":"/.ocis-agent/file.md"}`},
		{NewCopy(), `{"from":"/.ocis-agent/memory/profile.md","to":"/copy.md"}`},
	}
	f.fake.ResetRequests()
	for _, c := range cases {
		if _, err := c.tool.Prepare(bg, f.env, json.RawMessage(c.args)); !errors.Is(err, ErrRefused) {
			t.Errorf("%s %s: Prepare = %v, want ErrRefused", c.tool.Name(), c.args, err)
		}
		// Execute re-checks on its own: a stored call must never write there
		if _, err := c.tool.Execute(bg, f.env, json.RawMessage(c.args), ExecInfo{}); !errors.Is(err, ErrRefused) {
			t.Errorf("%s %s: Execute = %v, want ErrRefused", c.tool.Name(), c.args, err)
		}
	}
	if w := writeRequests(f.fake); len(w) != 0 {
		t.Fatalf("write requests were sent for refused calls: %v", w)
	}
	if data, _, _ := f.fake.File(testDrive, "/.ocis-agent/memory/profile.md"); string(data) != "secret" {
		t.Fatal("the reserved file must be untouched")
	}
}

func TestWriteToolsRejectBadPaths(t *testing.T) {
	f := newFixture(t)
	for _, c := range []struct {
		tool Tool
		args string
	}{
		{NewCreateFolder(), `{"path":"/a/../.ocis-agent/x"}`},
		{NewCreateFolder(), `{"path":"relative"}`},
		{NewWriteNew(), `{"path":"/a/./b","content":"x"}`},
		{NewWriteNew(), `{"path":"/a\n","content":"x"}`},
		{NewMove(), `{"from":"/a/..","to":"/b"}`},
		{NewCopy(), `{"from":"/a","to":"b"}`},
		{NewMove(), `{"from":"/a","to":"/b","drive":"x"}`},
	} {
		if _, err := c.tool.Prepare(bg, f.env, json.RawMessage(c.args)); !errors.Is(err, ErrInvalidArgs) {
			t.Errorf("%s %s: err = %v, want ErrInvalidArgs", c.tool.Name(), c.args, err)
		}
	}
}

func TestCreateFolder(t *testing.T) {
	f := newFixture(t)
	f.put("/docs/a.md", "x")
	p, err := NewCreateFolder().Prepare(bg, f.env, json.RawMessage(`{"path":"/docs/new"}`))
	if err != nil {
		t.Fatal(err)
	}
	var doc PreviewDoc
	if err := json.Unmarshal(p.Preview, &doc); err != nil || doc.Action != "create folder" || len(doc.Paths) != 1 || doc.Paths[0] != "/docs/new" || p.TargetETag != "" {
		t.Fatalf("preview = %s (%v)", p.Preview, err)
	}
	res, err := f.run(t, NewCreateFolder(), `{"path":"/docs/new"}`)
	if err != nil || res.Summary != "created the folder" {
		t.Fatalf("res = %+v %v", res, err)
	}
	if st, err := f.env.Files.Stat(bg, f.env.Creds, testDrive, "/docs/new"); err != nil || !st.Dir {
		t.Fatalf("the folder must exist: %+v %v", st, err)
	}
	if _, err := NewCreateFolder().Prepare(bg, f.env, json.RawMessage(`{"path":"/docs/new"}`)); !errors.Is(err, ErrRefused) {
		t.Fatalf("an existing target must be refused, got %v", err)
	}
	if _, err := NewCreateFolder().Prepare(bg, f.env, json.RawMessage(`{"path":"/nope/deeper"}`)); !errors.Is(err, ErrRefused) {
		t.Fatalf("a missing parent must be refused, got %v", err)
	}
	// created between Prepare and Execute: idempotent, not an error
	res, err = NewCreateFolder().Execute(bg, f.env, json.RawMessage(`{"path":"/docs/new"}`), ExecInfo{})
	if err != nil || !strings.Contains(res.Summary, "already exists") {
		t.Fatalf("res = %+v %v", res, err)
	}
	// a FILE at the target is a refusal
	if _, err := NewCreateFolder().Execute(bg, f.env, json.RawMessage(`{"path":"/docs/a.md"}`), ExecInfo{}); !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v", err)
	}
}

func TestCreateFolderReconcile(t *testing.T) {
	f := newFixture(t)
	f.put("/docs/a.md", "x")
	f.fake.PutFile(testDrive, "/docs/made/keep", []byte("k"))
	tool := NewCreateFolder()
	if r := reconcile(t, f, tool, `{"path":"/docs/made"}`); r.State != ReconDone {
		t.Fatalf("existing folder: %+v", r)
	}
	if r := reconcile(t, f, tool, `{"path":"/docs/absent"}`); r.State != ReconRetry {
		t.Fatalf("absent: %+v", r)
	}
	if r := reconcile(t, f, tool, `{"path":"/docs/a.md"}`); r.State != ReconFailed || r.Reason == "" {
		t.Fatalf("a file there: %+v", r)
	}
}

func TestWriteNew(t *testing.T) {
	f := newFixture(t)
	f.put("/docs/exists.md", "old")
	res, err := f.run(t, NewWriteNew(), `{"path":"/docs/new.md","content":"hello"}`)
	if err != nil || res.Summary != "created the file (5 bytes)" {
		t.Fatalf("res = %+v %v", res, err)
	}
	if data, _, ok := f.fake.File(testDrive, "/docs/new.md"); !ok || string(data) != "hello" {
		t.Fatalf("file = %q %v", data, ok)
	}
	p, err := NewWriteNew().Prepare(bg, f.env, json.RawMessage(`{"path":"/docs/p.md","content":"abc"}`))
	var doc PreviewDoc
	if err != nil || json.Unmarshal(p.Preview, &doc) != nil || doc.Action != "create file" || doc.Bytes != 3 || doc.SHA256 != sha256Hex([]byte("abc")) {
		t.Fatalf("preview = %s (%v)", p.Preview, err)
	}
	for name, args := range map[string]string{
		"exists":         `{"path":"/docs/exists.md","content":"x"}`,
		"missing parent": `{"path":"/nope/x.md","content":"x"}`,
		"parent is file": `{"path":"/docs/exists.md/x.md","content":"x"}`,
	} {
		if _, err := NewWriteNew().Prepare(bg, f.env, json.RawMessage(args)); !errors.Is(err, ErrRefused) {
			t.Errorf("%s: err = %v, want ErrRefused", name, err)
		}
	}
	f.env.Limits = Limits{MaxWriteBytes: 4}
	if _, err := NewWriteNew().Prepare(bg, f.env, json.RawMessage(`{"path":"/docs/big.md","content":"12345"}`)); !errors.Is(err, ErrInvalidArgs) {
		t.Fatalf("over the write limit: %v", err)
	}
	if _, err := NewWriteNew().Prepare(bg, f.env, json.RawMessage(`{"path":"/docs/nul.md","content":"a\u0000b"}`)); !errors.Is(err, ErrInvalidArgs) {
		t.Fatalf("NUL: %v", err)
	}
}

func TestWriteNewNeverOverwritesAFileThatAppearedMeanwhile(t *testing.T) {
	f := newFixture(t)
	args := `{"path":"/docs/race.md","content":"mine"}`
	f.put("/docs/seed.md", "x")
	if _, err := NewWriteNew().Prepare(bg, f.env, json.RawMessage(args)); err != nil {
		t.Fatal(err)
	}
	f.put("/docs/race.md", "theirs") // appears after Prepare
	if _, err := NewWriteNew().Execute(bg, f.env, json.RawMessage(args), ExecInfo{}); !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if data, _, _ := f.fake.File(testDrive, "/docs/race.md"); string(data) != "theirs" {
		t.Fatalf("content = %q: the other file must survive", data)
	}
}

func TestWriteNewReconcile(t *testing.T) {
	f := newFixture(t)
	f.put("/docs/same.md", "hello")
	f.put("/docs/other.md", "different")
	tool := NewWriteNew()
	if r := reconcile(t, f, tool, `{"path":"/docs/same.md","content":"hello"}`); r.State != ReconDone {
		t.Fatalf("same content: %+v", r)
	}
	if r := reconcile(t, f, tool, `{"path":"/docs/absent.md","content":"hello"}`); r.State != ReconRetry {
		t.Fatalf("absent: %+v", r)
	}
	if r := reconcile(t, f, tool, `{"path":"/docs/other.md","content":"hello"}`); r.State != ReconFailed || r.Reason == "" {
		t.Fatalf("different content: %+v", r)
	}
	if r := reconcile(t, f, tool, `{"path":"/docs","content":"hello"}`); r.State != ReconFailed {
		t.Fatalf("a folder there: %+v", r)
	}
}

func TestMoveAndCopy(t *testing.T) {
	f := newFixture(t)
	f.put("/src/a.md", "aaa")
	f.put("/dest/keep.md", "k")
	f.fake.ResetRequests()
	if res, err := f.run(t, NewMove(), `{"from":"/src/a.md","to":"/dest/b.md"}`); err != nil || res.Summary != "moved the item" {
		t.Fatalf("move: %+v %v", res, err)
	}
	if _, _, ok := f.fake.File(testDrive, "/src/a.md"); ok {
		t.Fatal("the source must be gone")
	}
	if data, _, ok := f.fake.File(testDrive, "/dest/b.md"); !ok || string(data) != "aaa" {
		t.Fatal("the destination must exist")
	}
	if res, err := f.run(t, NewCopy(), `{"from":"/dest/b.md","to":"/dest/c.md"}`); err != nil || res.Summary != "copied the item" {
		t.Fatalf("copy: %+v %v", res, err)
	}
	if _, _, ok := f.fake.File(testDrive, "/dest/b.md"); !ok {
		t.Fatal("a copy must keep the source")
	}
	for _, r := range f.fake.Requests() {
		if (r.Method == "MOVE" || r.Method == "COPY") && r.Header.Get("Overwrite") != "F" {
			t.Fatalf("%s was sent without Overwrite: F", r.Method)
		}
	}
	// folders move with their children
	if _, err := f.run(t, NewMove(), `{"from":"/dest","to":"/moved"}`); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := f.fake.File(testDrive, "/moved/c.md"); !ok {
		t.Fatal("children must follow the folder")
	}
}

func TestMoveAndCopyPrepareRefusals(t *testing.T) {
	f := newFixture(t)
	f.put("/a/x.md", "x")
	f.put("/b/y.md", "y")
	for _, tool := range []Tool{NewMove(), NewCopy()} {
		for name, args := range map[string]string{
			"source missing":      `{"from":"/a/nope.md","to":"/b/z.md"}`,
			"destination exists":  `{"from":"/a/x.md","to":"/b/y.md"}`,
			"parent missing":      `{"from":"/a/x.md","to":"/nodir/z.md"}`,
			"same path":           `{"from":"/a/x.md","to":"/a/x.md"}`,
			"into itself":         `{"from":"/a","to":"/a/inside"}`,
			"destination at root": `{"from":"/a/x.md","to":"/"}`,
		} {
			if _, err := tool.Prepare(bg, f.env, json.RawMessage(args)); !errors.Is(err, ErrRefused) {
				t.Errorf("%s %s: err = %v, want ErrRefused", tool.Name(), name, err)
			}
		}
	}
	if _, err := NewMove().Prepare(bg, f.env, json.RawMessage(`{"from":"/a","to":"/ab"}`)); err != nil {
		t.Fatalf("a sibling whose name starts with the source name is fine: %v", err)
	}
}

func TestMoveAndCopyNeverOverwriteADestinationThatAppearedMeanwhile(t *testing.T) {
	for _, tool := range []Tool{NewMove(), NewCopy()} {
		f := newFixture(t)
		f.put("/a/x.md", "mine")
		args := `{"from":"/a/x.md","to":"/a/y.md"}`
		if _, err := tool.Prepare(bg, f.env, json.RawMessage(args)); err != nil {
			t.Fatal(err)
		}
		f.put("/a/y.md", "theirs")
		if _, err := tool.Execute(bg, f.env, json.RawMessage(args), ExecInfo{}); !errors.Is(err, ErrRefused) {
			t.Fatalf("%s: err = %v, want ErrRefused", tool.Name(), err)
		}
		if data, _, _ := f.fake.File(testDrive, "/a/y.md"); string(data) != "theirs" {
			t.Fatalf("%s overwrote the destination: %q", tool.Name(), data)
		}
		if _, _, ok := f.fake.File(testDrive, "/a/x.md"); !ok {
			t.Fatalf("%s lost the source", tool.Name())
		}
	}
}

func TestMoveReconcile(t *testing.T) {
	f := newFixture(t)
	f.put("/s/both1.md", "1")
	f.put("/d/both1.md", "1")
	f.put("/s/only-src.md", "2")
	f.put("/d/only-dst.md", "3")
	tool := NewMove()
	for name, c := range map[string]struct {
		args string
		want ReconState
	}{
		"moved":   {`{"from":"/s/gone.md","to":"/d/only-dst.md"}`, ReconDone},
		"not yet": {`{"from":"/s/only-src.md","to":"/d/absent.md"}`, ReconRetry},
		"both":    {`{"from":"/s/both1.md","to":"/d/both1.md"}`, ReconFailed},
		"neither": {`{"from":"/s/gone.md","to":"/d/absent.md"}`, ReconFailed},
	} {
		if r := reconcile(t, f, tool, c.args); r.State != c.want {
			t.Errorf("%s: %+v, want state %d", name, r, c.want)
		}
	}
}

func TestCopyReconcile(t *testing.T) {
	f := newFixture(t)
	f.put("/s/a.md", "abc")
	f.put("/d/a.md", "abc")
	f.put("/d/short.md", "x")
	f.put("/s/dir/k", "k")
	f.put("/d/dir/k", "k")
	tool := NewCopy()
	for name, c := range map[string]struct {
		args string
		want ReconState
	}{
		"same size":   {`{"from":"/s/a.md","to":"/d/a.md"}`, ReconDone},
		"absent":      {`{"from":"/s/a.md","to":"/d/absent.md"}`, ReconRetry},
		"other size":  {`{"from":"/s/a.md","to":"/d/short.md"}`, ReconFailed},
		"folder done": {`{"from":"/s/dir","to":"/d/dir"}`, ReconDone},
		"type clash":  {`{"from":"/s/a.md","to":"/d/dir"}`, ReconFailed},
	} {
		if r := reconcile(t, f, tool, c.args); r.State != c.want {
			t.Errorf("%s: %+v, want state %d", name, r, c.want)
		}
	}
}

func TestWriteToolsKeepTheOutcomeMessagesFreeOfNames(t *testing.T) {
	f := newFixture(t)
	name := "/docs/" + injection
	f.put("/docs/seed.md", "x")
	res, err := f.run(t, NewWriteNew(), `{"path":"`+name+`","content":"c"}`)
	if err != nil || strings.Contains(res.Summary, "IGNORE") {
		t.Fatalf("res = %+v %v", res, err)
	}
	// a refusal quotes only the path the model itself supplied
	_, err = f.run(t, NewWriteNew(), `{"path":"`+name+`","content":"c"}`)
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v", err)
	}
	// the refusal repeats the path the model gave, nothing it was not given
	if strings.Contains(err.Error(), "c\"") || strings.Contains(err.Error(), "content") || strings.Contains(strings.ReplaceAll(err.Error(), name, ""), "IGNORE") {
		t.Fatalf("the refusal leaks more than the supplied path: %q", err)
	}
}

func TestMoveAndCopyExecuteRefusesSamePathAndIntoItselfWithoutSendingAnything(t *testing.T) {
	f := newFixture(t)
	f.put("/a/x.md", "x")
	f.fake.ResetRequests()
	for _, tool := range []Tool{NewMove(), NewCopy()} {
		for name, args := range map[string]string{
			"same path":   `{"from":"/a/x.md","to":"/a/x.md"}`,
			"into itself": `{"from":"/a","to":"/a/inside"}`,
		} {
			if _, err := tool.Execute(bg, f.env, json.RawMessage(args), ExecInfo{}); !errors.Is(err, ErrRefused) {
				t.Errorf("%s %s: err = %v, want ErrRefused", tool.Name(), name, err)
			}
		}
	}
	if w := writeRequests(f.fake); len(w) != 0 {
		t.Fatalf("write requests: %v", w)
	}
}

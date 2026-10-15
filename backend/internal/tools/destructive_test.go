package tools

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func prepared(t *testing.T, f fixture, tool Tool, args string) Prepared {
	t.Helper()
	p, err := tool.Prepare(bg, f.env, json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOverwriteBindsToTheApprovedETagAndSendsIfMatch(t *testing.T) {
	f := newFixture(t)
	etag := f.put("/docs/a.md", "old content")
	args := `{"path":"/docs/a.md","content":"new"}`
	p := prepared(t, f, NewOverwrite(), args)
	if p.TargetETag != etag {
		t.Fatalf("TargetETag = %q, want the current %q", p.TargetETag, etag)
	}
	var doc PreviewDoc
	if err := json.Unmarshal(p.Preview, &doc); err != nil || doc.Action != "overwrite file" || doc.Bytes != 3 || doc.SHA256 != sha256Hex([]byte("new")) || !strings.Contains(doc.Detail, "11 bytes") {
		t.Fatalf("preview = %s (%v)", p.Preview, err)
	}
	f.fake.ResetRequests()
	res, err := NewOverwrite().Execute(bg, f.env, json.RawMessage(args), ExecInfo{TargetETag: p.TargetETag})
	if err != nil || res.Summary != "overwrote the file (3 bytes)" {
		t.Fatalf("res = %+v %v", res, err)
	}
	if data, _, _ := f.fake.File(testDrive, "/docs/a.md"); string(data) != "new" {
		t.Fatalf("content = %q", data)
	}
	var ifMatch string
	for _, r := range f.fake.Requests() {
		if r.Method == "PUT" {
			ifMatch = r.Header.Get("If-Match")
		}
	}
	if ifMatch != etag {
		t.Fatalf("PUT If-Match = %q, want the approved ETag %q", ifMatch, etag)
	}
}

func TestOverwriteDoesNotGoThroughWhenTheFileChangedAfterTheApproval(t *testing.T) {
	f := newFixture(t)
	f.put("/docs/a.md", "old")
	args := `{"path":"/docs/a.md","content":"agent version"}`
	p := prepared(t, f, NewOverwrite(), args)
	f.put("/docs/a.md", "the user's edit") // after the approval
	_, err := NewOverwrite().Execute(bg, f.env, json.RawMessage(args), ExecInfo{TargetETag: p.TargetETag})
	if !errors.Is(err, ErrTargetChanged) {
		t.Fatalf("err = %v, want ErrTargetChanged", err)
	}
	if data, _, _ := f.fake.File(testDrive, "/docs/a.md"); string(data) != "the user's edit" {
		t.Fatalf("content = %q: the user's edit must survive", data)
	}
}

func TestOverwriteAndDeleteNeedTheApprovedETag(t *testing.T) {
	f := newFixture(t)
	f.put("/a.md", "x")
	f.fake.ResetRequests()
	if _, err := NewOverwrite().Execute(bg, f.env, json.RawMessage(`{"path":"/a.md","content":"y"}`), ExecInfo{}); err == nil || errors.Is(err, ErrRefused) {
		t.Fatalf("overwrite without a target ETag = %v, want a plain error", err)
	}
	if _, err := NewDelete().Execute(bg, f.env, json.RawMessage(`{"path":"/a.md"}`), ExecInfo{}); err == nil || errors.Is(err, ErrRefused) {
		t.Fatalf("delete without a target ETag = %v, want a plain error", err)
	}
	if w := writeRequests(f.fake); len(w) != 0 {
		t.Fatalf("write requests were sent: %v", w)
	}
	if data, _, _ := f.fake.File(testDrive, "/a.md"); string(data) != "x" {
		t.Fatal("the file must be untouched")
	}
}

func TestOverwritePrepareRefusals(t *testing.T) {
	f := newFixture(t)
	f.put("/docs/a.md", "x")
	f.put("/.ocis-agent/memory/profile.md", "secret")
	for name, args := range map[string]string{
		"missing":  `{"path":"/docs/nope.md","content":"x"}`,
		"a folder": `{"path":"/docs","content":"x"}`,
		"reserved": `{"path":"/.ocis-agent/memory/profile.md","content":"x"}`,
		"case":     `{"path":"/.OCIS-AGENT/memory/profile.md","content":"x"}`,
		"root":     `{"path":"/","content":"x"}`,
	} {
		if _, err := NewOverwrite().Prepare(bg, f.env, json.RawMessage(args)); !errors.Is(err, ErrRefused) {
			t.Errorf("%s: err = %v, want ErrRefused", name, err)
		}
	}
	f.env.Limits = Limits{MaxWriteBytes: 2}
	if _, err := NewOverwrite().Prepare(bg, f.env, json.RawMessage(`{"path":"/docs/a.md","content":"123"}`)); !errors.Is(err, ErrInvalidArgs) {
		t.Fatalf("over the write limit: %v", err)
	}
	if data, _, _ := f.fake.File(testDrive, "/.ocis-agent/memory/profile.md"); string(data) != "secret" {
		t.Fatal("the reserved file must be untouched")
	}
}

func TestOverwriteReconcile(t *testing.T) {
	f := newFixture(t)
	etag := f.put("/docs/a.md", "old")
	tool := NewOverwrite()
	rec := func(args, target string) Reconciled {
		r, err := tool.(Reconciler).Reconcile(bg, f.env, json.RawMessage(args), ExecInfo{TargetETag: target})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	// not applied yet: the ETag still equals the approved one
	if r := rec(`{"path":"/docs/a.md","content":"new"}`, etag); r.State != ReconRetry {
		t.Fatalf("untouched: %+v", r)
	}
	// applied before the crash: the content is what we wanted
	f.put("/docs/a.md", "new")
	if r := rec(`{"path":"/docs/a.md","content":"new"}`, etag); r.State != ReconDone {
		t.Fatalf("applied: %+v", r)
	}
	// changed by someone else
	f.put("/docs/a.md", "theirs")
	if r := rec(`{"path":"/docs/a.md","content":"new"}`, etag); r.State != ReconFailed || r.Reason == "" {
		t.Fatalf("changed: %+v", r)
	}
	// gone
	if r := rec(`{"path":"/docs/nope.md","content":"new"}`, etag); r.State != ReconFailed {
		t.Fatalf("missing: %+v", r)
	}
}

func TestDeleteChecksTheETagItselfAndGoesToTheTrash(t *testing.T) {
	f := newFixture(t)
	etag := f.put("/docs/a.md", "bye")
	args := `{"path":"/docs/a.md"}`
	p := prepared(t, f, NewDelete(), args)
	if p.TargetETag != etag {
		t.Fatalf("TargetETag = %q, want %q", p.TargetETag, etag)
	}
	var doc PreviewDoc
	if err := json.Unmarshal(p.Preview, &doc); err != nil || doc.Action != "delete file" || !strings.Contains(doc.Detail, "trash") {
		t.Fatalf("preview = %s (%v)", p.Preview, err)
	}
	f.fake.ResetRequests()
	res, err := NewDelete().Execute(bg, f.env, json.RawMessage(args), ExecInfo{TargetETag: p.TargetETag})
	if err != nil || res.Summary != "deleted the item (moved to the trash)" {
		t.Fatalf("res = %+v %v", res, err)
	}
	if _, _, ok := f.fake.File(testDrive, "/docs/a.md"); ok {
		t.Fatal("the file must be deleted")
	}
	// the stat comes right before the DELETE
	m := requestMethods(f.fake)
	if len(m) < 2 || m[len(m)-2] != "PROPFIND" || m[len(m)-1] != "DELETE" {
		t.Fatalf("request order = %v, want a PROPFIND immediately before the DELETE", m)
	}
}

func TestDeleteRefusesToDeleteWhatChangedAfterTheApproval(t *testing.T) {
	f := newFixture(t)
	f.put("/docs/a.md", "v1")
	args := `{"path":"/docs/a.md"}`
	p := prepared(t, f, NewDelete(), args)
	f.put("/docs/a.md", "v2 edited by the user")
	f.fake.ResetRequests()
	_, err := NewDelete().Execute(bg, f.env, json.RawMessage(args), ExecInfo{TargetETag: p.TargetETag})
	if !errors.Is(err, ErrTargetChanged) {
		t.Fatalf("err = %v, want ErrTargetChanged", err)
	}
	if _, _, ok := f.fake.File(testDrive, "/docs/a.md"); !ok {
		t.Fatal("the changed file must not be deleted")
	}
	for _, m := range requestMethods(f.fake) {
		if m == "DELETE" {
			t.Fatal("no DELETE may be sent when the ETag differs")
		}
	}
}

func TestDeleteOfAFolderAndOfSomethingAlreadyGone(t *testing.T) {
	f := newFixture(t)
	f.put("/docs/sub/x.md", "x")
	p := prepared(t, f, NewDelete(), `{"path":"/docs/sub"}`)
	var doc PreviewDoc
	if err := json.Unmarshal(p.Preview, &doc); err != nil || doc.Action != "delete folder" || !strings.Contains(doc.Detail, "everything in it") {
		t.Fatalf("folder preview = %s (%v)", p.Preview, err)
	}
	if _, err := NewDelete().Execute(bg, f.env, json.RawMessage(`{"path":"/docs/sub"}`), ExecInfo{TargetETag: p.TargetETag}); err != nil {
		t.Fatal(err)
	}
	res, err := NewDelete().Execute(bg, f.env, json.RawMessage(`{"path":"/docs/sub"}`), ExecInfo{TargetETag: p.TargetETag})
	if err != nil || res.Summary != "the item was already gone" {
		t.Fatalf("already gone: %+v %v", res, err)
	}
}

func TestDeleteRefusals(t *testing.T) {
	f := newFixture(t)
	f.put("/.ocis-agent/memory/profile.md", "secret")
	for name, args := range map[string]string{
		"missing":  `{"path":"/nope"}`,
		"reserved": `{"path":"/.ocis-agent/memory/profile.md"}`,
		"folder":   `{"path":"/.ocis-agent"}`,
		"case":     `{"path":"/.Ocis-Agent"}`,
		"root":     `{"path":"/"}`,
	} {
		if _, err := NewDelete().Prepare(bg, f.env, json.RawMessage(args)); !errors.Is(err, ErrRefused) {
			t.Errorf("%s Prepare: %v, want ErrRefused", name, err)
		}
		if name != "missing" {
			f.fake.ResetRequests()
			if _, err := NewDelete().Execute(bg, f.env, json.RawMessage(args), ExecInfo{TargetETag: "\"x\""}); !errors.Is(err, ErrRefused) {
				t.Errorf("%s Execute: %v, want ErrRefused", name, err)
			}
			if w := writeRequests(f.fake); len(w) != 0 {
				t.Errorf("%s: write requests %v", name, w)
			}
		}
	}
	if _, _, ok := f.fake.File(testDrive, "/.ocis-agent/memory/profile.md"); !ok {
		t.Fatal("the reserved file must survive")
	}
}

func TestDeleteReconcile(t *testing.T) {
	f := newFixture(t)
	etag := f.put("/docs/a.md", "x")
	tool := NewDelete().(Reconciler)
	rec := func(args, target string) Reconciled {
		r, err := tool.Reconcile(bg, f.env, json.RawMessage(args), ExecInfo{TargetETag: target})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := rec(`{"path":"/docs/a.md"}`, etag); r.State != ReconRetry {
		t.Fatalf("still there with the approved ETag: %+v", r)
	}
	if r := rec(`{"path":"/docs/gone.md"}`, etag); r.State != ReconDone {
		t.Fatalf("gone: %+v", r)
	}
	f.put("/docs/a.md", "edited")
	if r := rec(`{"path":"/docs/a.md"}`, etag); r.State != ReconFailed || r.Reason == "" {
		t.Fatalf("changed: %+v", r)
	}
}

func TestDeleteOfAFolderIsRefusedWhenAChildChangedAfterTheApproval(t *testing.T) {
	f := newFixture(t)
	f.put("/docs/sub/x.md", "v1")
	args := `{"path":"/docs/sub"}`
	p := prepared(t, f, NewDelete(), args)
	f.put("/docs/sub/x.md", "v2 edited by the user") // a child changes the folder ETag
	f.fake.ResetRequests()
	_, err := NewDelete().Execute(bg, f.env, json.RawMessage(args), ExecInfo{TargetETag: p.TargetETag})
	if !errors.Is(err, ErrTargetChanged) {
		t.Fatalf("err = %v, want ErrTargetChanged", err)
	}
	if data, _, ok := f.fake.File(testDrive, "/docs/sub/x.md"); !ok || string(data) != "v2 edited by the user" {
		t.Fatal("the folder and its child must survive")
	}
	for _, m := range requestMethods(f.fake) {
		if m == "DELETE" {
			t.Fatal("no DELETE may be sent")
		}
	}
}

package tools

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestTargetPathsNameWhatEveryEffectfulFileToolChanges(t *testing.T) {
	f := newFixture(t)
	f.put("/a/file.md", "x")
	f.put("/b/keep.md", "x")
	calls := map[string]string{
		"files.create_folder": `{"path":"/a/new"}`,
		"files.write_new":     `{"path":"/a/new.md","content":"x"}`,
		"files.overwrite":     `{"path":"/a/file.md","content":"y"}`,
		"files.delete":        `{"path":"/a/file.md"}`,
		"files.move":          `{"from":"/a/file.md","to":"/b/moved.md"}`,
		"files.copy":          `{"from":"/a/file.md","to":"/b/copied.md"}`,
	}
	reg, err := NewDefault()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range reg.Names() {
		tool, _ := reg.Get(name)
		args, ok := calls[name]
		if tool.Risk(json.RawMessage(args)) == RiskRead || tool.AutoAllowed() {
			continue
		}
		if !ok {
			t.Errorf("%s: no test call: add one so its target paths are checked", name)
			continue
		}
		p, err := tool.Prepare(bg, f.env, json.RawMessage(args))
		if err != nil {
			t.Fatalf("%s: prepare: %v", name, err)
		}
		var doc PreviewDoc
		if err := json.Unmarshal(p.Preview, &doc); err != nil {
			t.Fatal(err)
		}
		if got := TargetPaths(json.RawMessage(args)); !slices.Equal(got, doc.Paths) {
			t.Errorf("%s: TargetPaths = %q, the preview names %q", name, got, doc.Paths)
		}
	}
}

func TestTargetPathsCleansAndDropsInvalidPaths(t *testing.T) {
	cases := map[string][]string{
		`{"path":"/a//b/"}`:         {"/a/b"},
		`{"from":"/x","to":"/y"}`:   {"/x", "/y"},
		`{"path":"relative"}`:       nil,
		`{"path":"/a/../b"}`:        nil,
		`{"path":"/a\u202eb"}`:      nil,
		`{"from":"/x","to":7}`:      nil,
		`[]`:                        nil,
		`not json`:                  nil,
		`{"content":"/not/a/path"}`: nil,
		`{"path":"/same","from":"/same","to":"/other"}`: {"/same", "/other"},
	}
	for args, want := range cases {
		if got := TargetPaths(json.RawMessage(args)); !slices.Equal(got, want) {
			t.Errorf("TargetPaths(%s) = %q, want %q", args, got, want)
		}
	}
}

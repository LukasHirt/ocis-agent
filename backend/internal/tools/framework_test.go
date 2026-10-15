package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

var bg = context.Background()

func TestRiskStrings(t *testing.T) {
	for r, want := range map[Risk]string{RiskRead: "read", RiskWrite: "write", RiskDestructive: "destructive", RiskOutward: "outward"} {
		if r.String() != want {
			t.Errorf("%d = %q, want %q", r, r.String(), want)
		}
	}
	if Risk(99).String() != "unknown" {
		t.Error("an out-of-range risk must be unknown")
	}
}

func TestDefaultLimitsAndZeroValueEnv(t *testing.T) {
	d := DefaultLimits()
	if d.MaxReadBytes != 2<<20 || d.MaxWriteBytes != 1<<20 || d.MaxItems != 500 || DefaultJobReadBytes != 20<<20 {
		t.Fatalf("defaults = %+v", d)
	}
	got := (Env{Limits: Limits{MaxItems: 3}}).limits()
	if got.MaxItems != 3 || got.MaxReadBytes != d.MaxReadBytes || got.MaxWriteBytes != d.MaxWriteBytes {
		t.Fatalf("limits() = %+v", got)
	}
}

func TestCanonicalizeIsStableAcrossKeyOrderAndWhitespace(t *testing.T) {
	a, err := Canonicalize(json.RawMessage(`{"b": 1, "a": {"y": [1, 2, {"q":true,"p":null}], "x": "s"}}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Canonicalize(json.RawMessage("{\n \"a\":{\"x\":\"s\",\"y\":[1,2,{\"p\":null,\"q\":true}]},\n\"b\":1}"))
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("canonical forms differ:\n%s\n%s", a, b)
	}
	if string(a) != `{"a":{"x":"s","y":[1,2,{"p":null,"q":true}]},"b":1}` {
		t.Fatalf("canonical = %s", a)
	}
}

func TestCanonicalizeKeepsNumberTextAndRejectsGarbage(t *testing.T) {
	c, err := Canonicalize(json.RawMessage(`{"n":12345678901234567890,"f":1.50}`))
	if err != nil || string(c) != `{"f":1.50,"n":12345678901234567890}` {
		t.Fatalf("canonical = %s %v", c, err)
	}
	for _, bad := range []string{``, `{`, `{"a":1} x`, `{"a":1}}`, `nope`, `{"a":1}{"b":2}`} {
		if _, err := Canonicalize(json.RawMessage(bad)); !errors.Is(err, ErrInvalidArgs) {
			t.Errorf("Canonicalize(%q) = %v, want ErrInvalidArgs", bad, err)
		}
	}
}

func TestArgsHashBindsTheArguments(t *testing.T) {
	h1, err := ArgsHash(json.RawMessage(`{"path":"/a","content":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	h2, _ := ArgsHash(json.RawMessage(`{"content":"x","path":"/a"}`))
	h3, _ := ArgsHash(json.RawMessage(`{"path":"/a","content":"y"}`))
	h4, _ := ArgsHash(json.RawMessage(`{"path":"/a ","content":"x"}`))
	if h1 != h2 {
		t.Fatal("key order must not change the hash")
	}
	if h1 == h3 || h1 == h4 {
		t.Fatal("any change of a value must change the hash")
	}
	if !strings.HasPrefix(h1, "sha256:") || len(h1) != len("sha256:")+64 {
		t.Fatalf("hash = %q", h1)
	}
	if _, err := ArgsHash(json.RawMessage(`{`)); !errors.Is(err, ErrInvalidArgs) {
		t.Fatalf("err = %v", err)
	}
}

func TestIdemKeyIsDerivedFromServerSideValues(t *testing.T) {
	h, _ := ArgsHash(json.RawMessage(`{"path":"/a"}`))
	k := IdemKey("job1", "call7", h)
	if k != "job1:call7:"+strings.TrimPrefix(h, "sha256:") {
		t.Fatalf("key = %q", k)
	}
	if IdemKey("job1", "call8", h) == k || IdemKey("job2", "call7", h) == k {
		t.Fatal("job and call ids must be part of the key")
	}
}

type sample struct {
	Path string `json:"path"`
	N    int    `json:"n"`
}

func TestParseIsStrict(t *testing.T) {
	got, err := parse[sample](json.RawMessage(` {"path":"/a","n":2} `))
	if err != nil || got != (sample{Path: "/a", N: 2}) {
		t.Fatalf("parse = %+v %v", got, err)
	}
	for name, raw := range map[string]string{
		"unknown field":  `{"path":"/a","drive":"other"}`,
		"not an object":  `["/a"]`,
		"null":           `null`,
		"string":         `"/a"`,
		"empty":          ``,
		"trailing":       `{"path":"/a"} {"path":"/b"}`,
		"trailing brace": `{"path":"/a"}}`,
		"wrong type":     `{"path":5}`,
	} {
		if _, err := parse[sample](json.RawMessage(raw)); !errors.Is(err, ErrInvalidArgs) {
			t.Errorf("%s: err = %v, want ErrInvalidArgs", name, err)
		}
	}
}

func TestCleanPath(t *testing.T) {
	good := map[string]string{"/": "/", "/a": "/a", "/a/": "/a", "/a//b": "/a/b", "/a b/ünï.md": "/a b/ünï.md", "/.hidden": "/.hidden"}
	for in, want := range good {
		if got, err := cleanPath(in); err != nil || got != want {
			t.Errorf("cleanPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := []string{"", "a", "a/b", "./a", "../a", "/..", "/a/..", "/a/../b", "/a/./b", "/.", "/a\x00b", "/a\nb", "/a\x7fb", "/\xff", "\\a", strings.Repeat("/a", 3000)}
	for _, in := range bad {
		if _, err := cleanPath(in); !errors.Is(err, ErrInvalidArgs) {
			t.Errorf("cleanPath(%q) = %v, want ErrInvalidArgs", in, err)
		}
	}
}

func TestErrorKindsAndMessages(t *testing.T) {
	e := invalid("bad %s", "thing")
	if !errors.Is(e, ErrInvalidArgs) || errors.Is(e, ErrRefused) || !strings.Contains(e.Error(), "bad thing") {
		t.Fatalf("invalid = %v", e)
	}
	r := refuse("no %d", 5)
	if !errors.Is(r, ErrRefused) || r.Error() != "no 5" {
		t.Fatalf("refuse = %v", r)
	}
	c := changed("moved")
	if !errors.Is(c, ErrTargetChanged) || c.Error() != "moved" {
		t.Fatalf("changed = %v", c)
	}
}

func TestPreviewDocPrepared(t *testing.T) {
	p, err := PreviewDoc{Action: "create file", Paths: []string{"/a"}, Bytes: 3, SHA256: "ab"}.prepared("\"e1\"")
	if err != nil {
		t.Fatal(err)
	}
	if p.TargetETag != "\"e1\"" {
		t.Fatalf("etag = %q", p.TargetETag)
	}
	var back PreviewDoc
	if err := json.Unmarshal(p.Preview, &back); err != nil || back.Action != "create file" || back.Paths[0] != "/a" || back.Bytes != 3 {
		t.Fatalf("preview = %s %v", p.Preview, err)
	}
}

type fakeTool struct {
	name string
	risk Risk
}

func (f fakeTool) Name() string              { return f.name }
func (f fakeTool) Description() string       { return "d-" + f.name }
func (f fakeTool) Schema() json.RawMessage   { return json.RawMessage(`{"type":"object"}`) }
func (f fakeTool) Risk(json.RawMessage) Risk { return f.risk }
func (f fakeTool) AutoAllowed() bool         { return f.name == "z.auto" }
func (fakeTool) Prepare(context.Context, Env, json.RawMessage) (Prepared, error) {
	return Prepared{}, nil
}
func (fakeTool) Execute(context.Context, Env, json.RawMessage, ExecInfo) (Result, error) {
	return Result{}, nil
}

func TestRegistry(t *testing.T) {
	r, err := NewRegistry(fakeTool{name: "b.two", risk: RiskWrite}, fakeTool{name: "a.one", risk: RiskRead}, fakeTool{name: "z.auto", risk: RiskWrite})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.Names(), ","); got != "a.one,b.two,z.auto" {
		t.Fatalf("names = %s", got)
	}
	if _, ok := r.Get("a.one"); !ok {
		t.Fatal("a.one must exist")
	}
	if _, ok := r.Get("nope"); ok {
		t.Fatal("unknown tool must not exist")
	}
	specs := r.Specs()
	if len(specs) != 3 || specs[0].Name != "a.one" || specs[0].Risk != "read" || specs[2].AutoAllowed != true || specs[1].AutoAllowed {
		t.Fatalf("specs = %+v", specs)
	}
	if _, err := NewRegistry(fakeTool{name: "dup"}, fakeTool{name: "dup"}); err == nil {
		t.Error("duplicate names must fail")
	}
	if _, err := NewRegistry(fakeTool{name: ""}); err == nil {
		t.Error("an empty name must fail")
	}
}

func TestReadBudget(t *testing.T) {
	b := NewReadBudget(10)
	if b.Remaining() != 10 {
		t.Fatalf("remaining = %d", b.Remaining())
	}
	if err := b.Take(6); err != nil {
		t.Fatal(err)
	}
	if err := b.Take(5); !errors.Is(err, ErrReadBudget) {
		t.Fatalf("err = %v", err)
	}
	if b.Remaining() != 4 {
		t.Fatalf("a refused take must not count: remaining = %d", b.Remaining())
	}
	var nilBudget *ReadBudget
	if err := nilBudget.Take(1 << 40); err != nil || nilBudget.Remaining() < 1<<40 {
		t.Fatal("a nil budget means no cap")
	}
	c := NewReadBudget(1000)
	var wg sync.WaitGroup
	granted := make(chan struct{}, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c.Take(100) == nil {
				granted <- struct{}{}
			}
		}()
	}
	wg.Wait()
	if len(granted) != 10 {
		t.Fatalf("granted %d takes of 100 from 1000, want exactly 10", len(granted))
	}
}

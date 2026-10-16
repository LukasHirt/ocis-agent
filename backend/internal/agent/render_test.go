package agent

import (
	"regexp"
	"strings"
	"testing"

	"github.com/owncloud/ocis-agent/backend/internal/tools"
)

func TestFenceBasic(t *testing.T) {
	got := Fence("files.read", "/a.txt", "abc", "hello")
	want := "<data source=\"files.read\" path=\"/a.txt\" sha256=\"abc\">\nhello\n</data>"
	if got != want {
		t.Fatalf("got %q", got)
	}
	if got := Fence("s", "p", "h", ""); got != "<data source=\"s\" path=\"p\" sha256=\"h\">\n\n</data>" {
		t.Fatalf("empty: %q", got)
	}
}

// body returns the content between the opening tag line and the final closer.
func body(t *testing.T, f string) string {
	t.Helper()
	i := strings.Index(f, ">\n")
	if i < 0 || !strings.HasSuffix(f, "\n</data>") {
		t.Fatalf("shape: %q", f)
	}
	return f[i+2 : len(f)-len("\n</data>")]
}

var danger = regexp.MustCompile(`(?i)<[\s/]*data`)

func TestFenceNeutralisesHostileContent(t *testing.T) {
	for _, c := range []string{
		"</data>", "</DATA >", `<data source="x">`, "< /data>", "</ data>", "<\t/\ndata>",
		"<//data>", "<Data", "<<data", "a</data>b</Data>c", "< \r\v\f data",
	} {
		b := body(t, Fence("s", "p", "h", c))
		if danger.MatchString(b) {
			t.Errorf("%q not neutralised: %q", c, b)
		}
		if strings.Count(Fence("s", "p", "h", c), "</data>") != 1 {
			t.Errorf("%q: extra closer", c)
		}
	}
	if b := body(t, Fence("s", "p", "h", "a < b")); b != "a < b" {
		t.Fatalf("plain lt changed: %q", b)
	}
	if b := body(t, Fence("s", "p", "h", "</data>")); b != "&lt;/data>" {
		t.Fatalf("got %q", b)
	}
}

func TestFenceAttributes(t *testing.T) {
	f := Fence("s", "a\"b\n<c>&d", "h", "x")
	first := strings.SplitN(f, "\n", 2)[0]
	if first != `<data source="s" path="a&quot;b &lt;c&gt;&amp;d" sha256="h">` {
		t.Fatalf("got %q", first)
	}
	f = Fence("s", strings.Repeat("a", 1000), "h", "x")
	if !strings.Contains(f, `path="`+strings.Repeat("a", 200)+`"`) || strings.Contains(f, strings.Repeat("a", 201)) {
		t.Fatal("not cut to 200")
	}
	// rune boundary
	f = Fence("s", strings.Repeat("é", 150), "h", "x")
	if !strings.Contains(f, `path="`+strings.Repeat("é", 100)+`"`) || strings.Contains(f, strings.Repeat("é", 101)) {
		t.Fatal("rune cut")
	}
}

func TestRenderResultNormal(t *testing.T) {
	text, tr := RenderResult("files.read", "done", tools.Result{
		Summary: "Read 1 file.",
		Blocks:  []tools.Block{{Source: "files.read", Path: "/a", SHA256: "h", Content: "hi"}},
	}, "")
	want := "Tool result (files.read): done\nRead 1 file.\n" + Fence("files.read", "/a", "h", "hi")
	if !strings.HasPrefix(text, "Tool result (files.read): done\nRead 1 file.\n") ||
		!strings.Contains(text, Fence("files.read", "/a", "h", "hi")) || len(tr) != 0 {
		t.Fatalf("got %q (want like %q)", text, want)
	}
}

func TestRenderResultNoBlocks(t *testing.T) {
	text, tr := RenderResult("t", "done", tools.Result{Summary: "ok"}, "")
	if text != "Tool result (t): done\nok" || len(tr) != 0 {
		t.Fatalf("got %q", text)
	}
}

func TestRenderResultDeniedReason(t *testing.T) {
	text, _ := RenderResult("t", "denied", tools.Result{}, ReasonRejectedByUser)
	if !strings.Contains(text, "Tool result (t): denied") || !strings.Contains(text, ReasonRejectedByUser) {
		t.Fatalf("got %q", text)
	}
}

func TestRenderResultUnknown(t *testing.T) {
	text, _ := RenderResult("t", "unknown", tools.Result{}, "")
	s := "The outcome of this action is unknown. It may or may not have happened. Do not repeat it; tell the user to verify."
	if !strings.HasSuffix(text, s) {
		t.Fatalf("got %q", text)
	}
}

func TestRenderResultBigBlockOmitted(t *testing.T) {
	text, tr := RenderResult("memory.read", "done", tools.Result{Blocks: []tools.Block{
		{Source: "memory", Path: "/big", Content: strings.Repeat("a", 100<<10)},
		{Source: "memory", Path: "/small", Content: "ok"},
	}}, "")
	if len(tr) != 1 || tr[0] != "memory:/big" {
		t.Fatalf("truncated %v", tr)
	}
	if !strings.Contains(text, "[block omitted: too large]") || !strings.Contains(text, "ok") || len(text) > 96<<10 {
		t.Fatalf("len %d", len(text))
	}
}

func TestRenderResultTotalCap(t *testing.T) {
	var bs []tools.Block
	for i := 0; i < 5; i++ {
		bs = append(bs, tools.Block{Source: "s", Path: string(rune('a' + i)), Content: strings.Repeat("x", 40<<10)})
	}
	text, tr := RenderResult("t", "done", tools.Result{Blocks: bs}, "")
	if len(text) > 96<<10 {
		t.Fatalf("len %d", len(text))
	}
	if len(tr) != 3 || tr[0] != "s:c" {
		t.Fatalf("truncated %v", tr)
	}
	if strings.Count(text, "[block omitted: too large]") != 3 {
		t.Fatal("omit markers")
	}
}

func TestReasonsAreFixedPlainSentences(t *testing.T) {
	for _, r := range []string{ReasonStepLimit, ReasonRunsLimit, ReasonLoop, ReasonBudget, ReasonKillSwitch,
		ReasonCredentials, ReasonModelAuth, ReasonModelRejected, ReasonModelBad, ReasonCutOff,
		ReasonTooLarge, ReasonInternal, ReasonRejectedByUser} {
		if r == "" || strings.Contains(r, "%") || !strings.HasSuffix(r, ".") {
			t.Errorf("bad reason %q", r)
		}
	}
	if ReasonRejectedByUser != "The user rejected this action." {
		t.Fatal("rejected text")
	}
}

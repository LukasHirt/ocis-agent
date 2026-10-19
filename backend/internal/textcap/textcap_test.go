package textcap

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCutKeepsRunesWhole(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"abc", 5, "abc"},
		{"abc", 3, "abc"},
		{"abc", 2, "ab"},
		{"aé", 2, "a"}, // é is two bytes
		{"日本", 4, "日"},
		{"日本", 2, ""},
		{"abc", 0, ""},
		{"abc", -1, ""},
	} {
		if got := Cut(tc.in, tc.n); got != tc.want {
			t.Errorf("Cut(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

func TestCapEndsInTheMarkerAndNeverExceedsTheLimit(t *testing.T) {
	if Marker != "…[truncated]" {
		t.Fatalf("Marker = %q", Marker)
	}
	if got := Cap("short", 100); got != "short" {
		t.Errorf("short = %q", got)
	}
	long := "x" + strings.Repeat("é", 100)
	for limit := len(Marker); limit < len(long); limit++ {
		got := Cap(long, limit)
		if len(got) > limit || !strings.HasSuffix(got, Marker) || !utf8.ValidString(got) {
			t.Fatalf("Cap(%d) = %q (%d bytes)", limit, got, len(got))
		}
		if len(got) < limit-1 {
			t.Fatalf("Cap(%d) cut too much: %d bytes", limit, len(got))
		}
	}
	// a limit below the marker's size: a plain cut
	if got := Cap(long, 3); got != "xé" {
		t.Errorf("tiny limit = %q", got)
	}
	if got := Cap(long, 0); got != "" {
		t.Errorf("zero limit = %q", got)
	}
}

func TestCapRepairsInvalidUTF8(t *testing.T) {
	if got := Cap("a\xffb", 100); got != "a�b" {
		t.Errorf("repaired = %q", got)
	}
	got := Cap(strings.Repeat("\xffa", 100), 50) // each invalid run becomes one U+FFFD
	if !utf8.ValidString(got) || len(got) > 50 || !strings.HasSuffix(got, Marker) {
		t.Errorf("capped invalid = %q", got)
	}
}

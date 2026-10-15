package memory

import (
	"fmt"
	"strings"
	"testing"
)

func TestDiff(t *testing.T) {
	cases := []struct{ name, before, after, want string }{
		{"equal", "same\n", "same\n", ""},
		{"empty to empty", "", "", ""},
		{"change one line", "a\nb\n", "a\nc\n", "-b\n+c\n"},
		{"remove a line", "a\nb\nc\n", "a\nc\n", "-b\n"},
		{"add a line", "a\nc\n", "a\nb\nc\n", "+b\n"},
		{"from empty", "", "x\ny\n", "+x\n+y\n"},
		{"to empty", "x\n", "", "-x\n"},
		{"unchanged lines in between are not listed", "1\n2\n3\n4\n5\n", "1\nX\n3\n4\nY\n", "-2\n+X\n-5\n+Y\n"},
		{"only the trailing newline", "a", "a\n", "~ trailing newline changed\n"},
		{"no trailing newline at all", "a\nb", "a\nc", "-b\n+c\n"},
	}
	for _, tc := range cases {
		if got := Diff(tc.before, tc.after); got != tc.want {
			t.Errorf("%s: Diff(%q, %q) = %q, want %q", tc.name, tc.before, tc.after, got, tc.want)
		}
	}
}

func TestDiffOfHugeUnrelatedFilesStaysBounded(t *testing.T) {
	var a, b strings.Builder
	for i := 0; i < 1100; i++ {
		fmt.Fprintf(&a, "old %d\n", i)
		fmt.Fprintf(&b, "new %d\n", i)
	}
	got := Diff(a.String(), b.String())
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 2200 || !strings.HasPrefix(lines[0], "-old 0") || !strings.HasPrefix(lines[1100], "+new 0") {
		t.Fatalf("got %d lines, first %q", len(lines), lines[0])
	}
}

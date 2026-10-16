package memory

import (
	"errors"
	"strings"
	"testing"
)

func TestResolveName(t *testing.T) {
	good := map[string]string{
		"profile.md":     "/.ocis-agent/memory/profile.md",
		"projects.md":    "/.ocis-agent/memory/projects.md",
		"notes/trip.md":  "/.ocis-agent/memory/notes/trip.md",
		"notes/a1-b2.md": "/.ocis-agent/memory/notes/a1-b2.md",
		"notes/" + strings.Repeat("a", 64) + ".md": "/.ocis-agent/memory/notes/" + strings.Repeat("a", 64) + ".md",
	}
	for name, want := range good {
		got, err := ResolveName(name)
		if err != nil || got != want {
			t.Errorf("ResolveName(%q) = %q, %v; want %q", name, got, err, want)
		}
	}
	bad := []string{
		"", "profile", "PROFILE.md", "other.md", "/profile.md", "profile.md/", "../profile.md", "notes", "notes/", "notes/.md",
		"notes/a/b.md", "notes/Up.md", "notes/-x.md", "notes/x.txt", "notes/x y.md", "notes/ä.md", "notes/../profile.md",
		"notes/" + strings.Repeat("a", 65) + ".md", "profile.md\x00", "notes/a\n.md", ".ocis-agent/memory/profile.md",
	}
	for _, name := range bad {
		if _, err := ResolveName(name); !errors.Is(err, ErrInvalidName) {
			t.Errorf("ResolveName(%q) = %v, want ErrInvalidName", name, err)
		}
	}
}

func TestIsReserved(t *testing.T) {
	reserved := []string{
		"/.ocis-agent", "/.ocis-agent/", "/.ocis-agent/memory/profile.md", ".ocis-agent/x", "/.OCIS-Agent/x",
		"/docs/../.ocis-agent/x", "//.ocis-agent//x", "/.ocis-agent/../.ocis-agent/y",
	}
	for _, p := range reserved {
		if !IsReserved(p) {
			t.Errorf("IsReserved(%q) = false, want true", p)
		}
	}
	free := []string{"", "/", "/.ocis-agent2/x", "/docs/.ocis-agent/x", "/ocis-agent/x", "/docs/a.md", "/.ocis-agentx"}
	for _, p := range free {
		if IsReserved(p) {
			t.Errorf("IsReserved(%q) = true, want false", p)
		}
	}
}

func TestIsReservedIgnoresCase(t *testing.T) {
	for _, p := range []string{
		"/.OCIS-AGENT", "/.OCIS-AGENT/", "/.Ocis-Agent/Memory/Profile.md", "/.ocis-AGENT/memory/notes/X.md",
		"/DOCS/../.OCIS-agent/x", "//.oCiS-aGeNt//README.md", ".OCIS-AGENT",
	} {
		if !IsReserved(p) {
			t.Errorf("IsReserved(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"/.OCIS-AGENT2/x", "/Docs/.OCIS-AGENT/x", "/.OCIS-AGENTX"} {
		if IsReserved(p) {
			t.Errorf("IsReserved(%q) = true, want false", p)
		}
	}
}

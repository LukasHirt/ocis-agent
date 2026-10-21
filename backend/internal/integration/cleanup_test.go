package integration

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The rules of what the integration test may delete in oCIS. They run in the
// normal unit tests (no build tag), so a change that widens them fails CI.

func TestValidPrefix(t *testing.T) {
	for p, want := range map[string]bool{
		"ocis-agent-0123456789ab-":  true,
		"ocis-agent-":               false, // everything of every instance
		"ocis-agent-0123456789ab":   false, // would match ...abX
		"ocis-agent-0123456789AB-":  false,
		"ocis-agent-0123456789a-":   false,
		"ocis-agent-0123456789abc-": false,
		"":                          false,
		"ocis-agent-spike-":         false,
	} {
		if got := validPrefix(p); got != want {
			t.Errorf("validPrefix(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestTokensToDelete(t *testing.T) {
	const ours = "ocis-agent-0123456789ab-"
	list := []appToken{
		{Hash: "h1", Label: ours + "1111111111111111"},
		{Hash: "h2", Label: "ocis-agent-a5331749c6ec-97961b2241d9a7b9"}, // another instance (the user's sidecar)
		{Hash: "h3", Label: "my laptop"},
		{Hash: "h4", Label: ours + "2222222222222222"},
		{Hash: "h5", Label: "ocis-agent-0123456789ab"}, // no dash: not ours
		{Hash: "h6", Label: ours + "3333333333333333"}, // listed before our sidecar started
	}
	pre := map[string]bool{ours + "3333333333333333": true}
	got := tokensToDelete(list, ours, pre)
	if len(got) != 2 || got[0].Hash != "h1" || got[1].Hash != "h4" {
		t.Fatalf("tokensToDelete = %+v", got)
	}
	for _, bad := range []string{"", "ocis-agent-", "ocis-agent-0123456789ab", "x"} {
		if got := tokensToDelete(list, bad, nil); len(got) != 0 {
			t.Fatalf("prefix %q selects %d token(s)", bad, len(got))
		}
	}
}

func TestPrefixCollides(t *testing.T) {
	const ours = "ocis-agent-0123456789ab-"
	if prefixCollides(ours, map[string]bool{"ocis-agent-a5331749c6ec-97961b2241d9a7b9": true, "laptop": true}) {
		t.Fatal("a foreign prefix collides")
	}
	if !prefixCollides(ours, map[string]bool{ours + "1": true}) {
		t.Fatal("a pre-existing label with our prefix does not collide")
	}
}

func TestDeletablePath(t *testing.T) {
	created := map[string]bool{
		"/.ocis-agent/memory/notes/ocis-agent-it-abc.md": true,
		"/.ocis-agent/README.md":                         true,
		// recorded, but never a file the test creates: refused anyway
		"/.ocis-agent/memory/profile.md": true,
		"/.ocis-agent":                   true,
		"/Documents/report.docx":         true,
	}
	for p, want := range map[string]bool{
		"/ocis-agent-it-abc.txt":                         true,
		"ocis-agent-it-abc.txt":                          false, // not absolute
		"/ocis-agent-it-":                                false,
		"/docs/ocis-agent-it-abc.txt":                    false, // not at the root
		"/ocis-agent-it-abc/../x":                        false,
		"/ocis-agent-it-a/b":                             false,
		"/.ocis-agent/memory/notes/ocis-agent-it-abc.md": true,
		"/.ocis-agent/memory/notes/ocis-agent-it-zzz.md": false, // not recorded
		"/.ocis-agent/README.md":                         true,
		"/.ocis-agent/memory/notes/other.md":             false,
		"/.ocis-agent":                                   false,
		"/.ocis-agent/memory/profile.md":                 false,
		"/Documents/report.docx":                         false,
		"/":                                              false,
		"/ocis-agent-spike-x":                            false,
	} {
		if got := deletablePath(p, created); got != want {
			t.Errorf("deletablePath(%q) = %v, want %v", p, got, want)
		}
	}
	if deletablePath("/.ocis-agent/README.md", nil) {
		t.Error("the README is deletable without being recorded")
	}
}

func TestTrashToPurge(t *testing.T) {
	entry := func(key, loc, ts string, dir bool) string {
		rt := "<d:resourcetype></d:resourcetype>"
		if dir {
			rt = "<d:resourcetype><d:collection/></d:resourcetype>"
		}
		return `<d:response><d:href>/remote.php/dav/spaces/trash-bin/d/` + key + `</d:href><d:propstat><d:prop>` +
			`<oc:trashbin-original-location>` + loc + `</oc:trashbin-original-location>` +
			`<oc:trashbin-delete-timestamp>` + ts + `</oc:trashbin-delete-timestamp>` + rt + `</d:prop></d:propstat></d:response>`
	}
	body := `<?xml version="1.0"?><d:multistatus>` +
		`<d:response><d:href>/remote.php/dav/spaces/trash-bin/d/</d:href></d:response>` +
		entry("k1", "ocis-agent-it-a.txt", "2000", false) + // ours
		entry("k2", "ocis-agent-it-a.txt", "999", false) + // same name, deleted before the run
		entry("k3", ".ocis-agent", "2000", true) + // a folder: never purged
		entry("k4", ".ocis-agent/memory/notes/ocis-agent-it-b.md", "2000", false) + // ours
		entry("k5", ".ocis-agent/memory/profile.md", "2000", false) + // the user's
		entry("k6", "ocis-agent-it-c.txt", "2000", false) + // not deleted by this run
		`</d:multistatus>`
	files := map[string]bool{"/ocis-agent-it-a.txt": true, "/.ocis-agent/memory/notes/ocis-agent-it-b.md": true, "/.ocis-agent": true}
	got := trashToPurge(body, files, 1000)
	if !reflect.DeepEqual(got, []string{"k1", "k4"}) {
		t.Fatalf("trashToPurge = %v", got)
	}
}

func TestFolderIsEmpty(t *testing.T) {
	self := `<d:response><d:href>/remote.php/dav/spaces/d/.ocis-agent/</d:href></d:response>`
	child := `<d:response><d:href>/remote.php/dav/spaces/d/.ocis-agent/README.md</d:href></d:response>`
	if !folderIsEmpty(`<d:multistatus>` + self + `</d:multistatus>`) {
		t.Fatal("a folder with no entries is not empty")
	}
	if folderIsEmpty(`<d:multistatus>` + self + child + `</d:multistatus>`) {
		t.Fatal("a folder with a file is empty")
	}
	if folderIsEmpty(`garbage`) {
		t.Fatal("an unparsable answer counts as empty")
	}
}

func TestStateFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st := runState{TestPID: 1, SidecarPID: 2, Binary: "/tmp/x/ocis-agent", Prefix: "ocis-agent-0123456789ab-",
		Files: []string{"/ocis-agent-it-a.txt"}, Dirs: []string{"/tmp/ocis-agent-it-1"}}
	p := filepath.Join(dir, "state.json")
	if err := writeState(p, st); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode: %v %v", fi, err)
	}
	got, ok, err := readState(p)
	if err != nil || !ok || !reflect.DeepEqual(got, st) {
		t.Fatalf("readState = %+v %v %v", got, ok, err)
	}
	if _, ok, err := readState(filepath.Join(dir, "missing.json")); ok || err != nil {
		t.Fatalf("a missing state = %v %v", ok, err)
	}
}

func TestOwnTempDir(t *testing.T) {
	root := os.TempDir()
	if !ownTempDir(filepath.Join(root, "ocis-agent-it-123")) {
		t.Fatal("our temp dir is refused")
	}
	for _, p := range []string{root, filepath.Join(root, "other"), "/", filepath.Join(root, "ocis-agent-it-1", ".."), "/home/ocis-agent-it-1"} {
		if ownTempDir(p) {
			t.Fatalf("ownTempDir(%q) = true", p)
		}
	}
	if !strings.HasPrefix(stateFile(), root) {
		t.Fatalf("state file %s is not in the temp dir", stateFile())
	}
}

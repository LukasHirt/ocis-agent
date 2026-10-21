//go:build unix

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
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
		"/ocis-agent-it-abc.txt":                         true,
		"/ocis-agent-it-":                                true, // recorded, but no name after the prefix
		"/ocis-agent-it-abc/../x":                        true,
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

func TestCheckStateDir(t *testing.T) {
	uid := os.Getuid()
	good := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(good, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := checkStateDir(good, uid); err != nil {
		t.Fatalf("a private dir is refused: %v", err)
	}
	if err := checkStateDir(good, uid+1); err == nil {
		t.Fatal("a dir of another owner is accepted")
	}
	open := filepath.Join(t.TempDir(), "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkStateDir(open, uid); err == nil {
		t.Fatal("a 0755 dir is accepted")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	if err := checkStateDir(link, uid); err == nil {
		t.Fatal("a symlink to a private dir is accepted")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := checkStateDir(file, uid); err == nil {
		t.Fatal("a file is accepted")
	}
	if err := checkStateDir(filepath.Join(t.TempDir(), "missing"), uid); err == nil {
		t.Fatal("a missing dir is accepted")
	}
}

func TestRecordedBinary(t *testing.T) {
	own := filepath.Join(os.TempDir(), testFilePrefix+"42")
	for _, c := range []struct {
		st   runState
		want bool
	}{
		{runState{Binary: filepath.Join(own, "ocis-agent"), Dirs: []string{own}}, true},
		{runState{Binary: "/", Dirs: []string{own}}, false},
		{runState{Binary: "", Dirs: []string{own}}, false},
		{runState{Binary: filepath.Join(own, "ocis-agent")}, false},                                      // dir not recorded
		{runState{Binary: filepath.Join(own, "other"), Dirs: []string{own}}, false},                      // not the built binary
		{runState{Binary: "/usr/bin/ocis-agent", Dirs: []string{"/usr/bin"}}, false},                     // not our temp dir
		{runState{Binary: filepath.Join(own, "..", "x", "ocis-agent"), Dirs: []string{own}}, false},      // not clean
		{runState{Binary: filepath.Join(own, "sub", "ocis-agent"), Dirs: []string{own + "/sub"}}, false}, // not directly in the temp dir
	} {
		if _, got := recordedBinary(c.st); got != c.want {
			t.Errorf("recordedBinary(%+v) = %v, want %v", c.st, got, c.want)
		}
	}
}

// startSleep runs an unrelated process with an absolute argv[0].
func startSleep(t *testing.T) (*exec.Cmd, string) {
	t.Helper()
	bin, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep binary")
	}
	if bin, err = filepath.Abs(bin); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd, bin
}

func TestRunsExactly(t *testing.T) {
	cmd, bin := startSleep(t)
	pid := cmd.Process.Pid
	if !runsExactly(pid, bin) {
		t.Fatalf("the sleep process is not recognised by its exact argv[0] %s", bin)
	}
	for _, want := range []string{"/", "", "sleep", bin + "x", filepath.Dir(bin)} {
		if runsExactly(pid, want) {
			t.Errorf("runsExactly(sleep, %q) = true", want)
		}
	}
	if runsExactly(0, bin) || runsExactly(-1, bin) {
		t.Error("an invalid pid matches")
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if runsExactly(pid, bin) {
		t.Error("an ended process still matches")
	}
}

func TestRunLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	release, err := acquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireLock(dir); err == nil || !strings.Contains(err.Error(), "another integration run") {
		t.Fatalf("a second lock while the first is held = %v", err)
	}
	release()
	release() // idempotent
	release2, err := acquireLock(dir)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	release2()

	// a stale lock: its pid runs something else than the recorded binary
	cmd, bin := startSleep(t)
	lock := filepath.Join(dir, "lock")
	plant := func(pid int, argv0 string) {
		t.Helper()
		if err := os.WriteFile(lock, []byte(`{"pid":`+strconv.Itoa(pid)+`,"argv0":"`+argv0+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	plant(cmd.Process.Pid, "/some/integration.test")
	release3, err := acquireLock(dir)
	if err != nil {
		t.Fatalf("a lock of a pid that runs another binary is not taken over: %v", err)
	}
	release3()
	// a lock of a live process that runs exactly the recorded binary is held
	plant(cmd.Process.Pid, bin)
	if _, err := acquireLock(dir); err == nil {
		t.Fatal("a live lock is taken over")
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if !eventuallyTrue(5*time.Second, func() bool { return !runsExactly(cmd.Process.Pid, bin) }) {
		t.Fatal("sleep did not end")
	}
	release4, err := acquireLock(dir)
	if err != nil {
		t.Fatalf("the lock of an ended process is not taken over: %v", err)
	}
	release4()
	// a damaged lock is not taken over silently
	if err := os.WriteFile(lock, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireLock(dir); err == nil {
		t.Fatal("a damaged lock is taken over")
	}
}

func eventuallyTrue(d time.Duration, f func() bool) bool {
	deadline := time.Now().Add(d)
	for !f() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
	return true
}

func TestReadmeDeletable(t *testing.T) {
	if !readmeDeletable(`"e1"`, `"e1"`) {
		t.Fatal("the README the test created is not deletable")
	}
	for _, c := range [][2]string{{"", ""}, {"", `"e1"`}, {`"e1"`, `"e2"`}, {`"e1"`, ""}} {
		if readmeDeletable(c[0], c[1]) {
			t.Errorf("readmeDeletable(%q, %q) = true", c[0], c[1])
		}
	}
}

func TestPropfindInfo(t *testing.T) {
	file := `<d:multistatus><d:response><d:href>/x</d:href><d:propstat><d:prop><d:getetag>"abc"</d:getetag><d:resourcetype></d:resourcetype></d:prop></d:propstat></d:response></d:multistatus>`
	dir := `<d:multistatus><d:response><d:href>/x/</d:href><d:propstat><d:prop><d:getetag>"d"</d:getetag><d:resourcetype><d:collection/></d:resourcetype></d:prop></d:propstat></d:response></d:multistatus>`
	if etag, isDir, ok := propfindInfo(file); !ok || isDir || etag != `"abc"` {
		t.Fatalf("file: %q %v %v", etag, isDir, ok)
	}
	if _, isDir, ok := propfindInfo(dir); !ok || !isDir {
		t.Fatalf("dir: %v %v", isDir, ok)
	}
	if _, _, ok := propfindInfo("garbage"); ok {
		t.Fatal("garbage parses")
	}
	two := `<d:multistatus><d:response></d:response><d:response></d:response></d:multistatus>`
	if _, _, ok := propfindInfo(two); ok {
		t.Fatal("two entries parse as one")
	}
}

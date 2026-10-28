//go:build unix

package integration

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// What the integration test may delete in oCIS, as pure functions (tested in
// cleanup_test.go without the dev stack). The harness deletes nothing
// that these functions do not select.

// testFilePrefix starts the name of every file the test creates at the drive
// root.
const testFilePrefix = "ocis-agent-it-"

// appToken is an entry of GET /auth-app/tokens. Hash is the listed token
// field (the hash the delete needs); it is never printed.
type appToken struct {
	Hash    string    `json:"token"`
	Label   string    `json:"label"`
	Created time.Time `json:"created_date"`
}

// instancePrefixRE is exactly the label prefix of one sidecar instance:
// "ocis-agent-" + its 12 random hex characters + "-".
var instancePrefixRE = regexp.MustCompile(`^ocis-agent-[0-9a-f]{12}-$`)

func validPrefix(p string) bool { return instancePrefixRE.MatchString(p) }

// tokensToDelete selects the tokens of ONE sidecar instance (prefix, which
// must be a valid instance prefix) that were not already listed before that
// sidecar started (preexisting, exact labels). Nothing else is ever selected.
func tokensToDelete(list []appToken, prefix string, preexisting map[string]bool) []appToken {
	if !validPrefix(prefix) {
		return nil
	}
	var out []appToken
	for _, tk := range list {
		if strings.HasPrefix(tk.Label, prefix) && !preexisting[tk.Label] {
			out = append(out, tk)
		}
	}
	return out
}

// prefixCollides reports whether a label that existed before our sidecar
// started carries its prefix (then the prefix is not provably ours).
func prefixCollides(prefix string, preexisting map[string]bool) bool {
	for l := range preexisting {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

// deletablePath reports whether the test may delete the file p: p must be
// recorded as created by the test AND have one of the shapes the test ever
// creates: a file at the drive root named ocis-agent-it-*, the README of the
// agent folder, or a note named ocis-agent-it-*.md. A recorded path of any
// other shape (a damaged state file) is refused. The rule is about paths
// only: the caller (harness.remove) also refuses anything oCIS reports as a
// folder, so a folder is never deleted through it.
func deletablePath(p string, created map[string]bool) bool {
	if !strings.HasPrefix(p, "/") || path.Clean(p) != p || !created[p] {
		return false
	}
	if rootTestFile(p) {
		return true
	}
	if p == "/.ocis-agent/README.md" {
		return true
	}
	note, ok := strings.CutPrefix(p, "/.ocis-agent/memory/notes/")
	return ok && !strings.Contains(note, "/") && strings.HasPrefix(note, testFilePrefix) &&
		strings.HasSuffix(note, ".md") && len(note) > len(testFilePrefix)+len(".md")
}

// rootTestFile: /ocis-agent-it-<something>, directly at the drive root.
func rootTestFile(p string) bool {
	name := strings.TrimPrefix(p, "/")
	return !strings.Contains(name, "/") && strings.HasPrefix(name, testFilePrefix) && len(name) > len(testFilePrefix)
}

var (
	davResponseRE = regexp.MustCompile(`(?s)<d:response>(.*?)</d:response>`)
	davHrefRE     = regexp.MustCompile(`<d:href>([^<]*)</d:href>`)
	trashLocRE    = regexp.MustCompile(`<oc:trashbin-original-location>([^<]*)</oc:trashbin-original-location>`)
	trashTSRE     = regexp.MustCompile(`<oc:trashbin-delete-timestamp>([0-9]+)</oc:trashbin-delete-timestamp>`)
)

// trashToPurge returns the keys of the trash entries (a PROPFIND Depth 1 of
// the trash) that are FILES the test deleted (exact original location, files
// keyed with a leading slash) and that were deleted at or after since (Unix
// seconds). Folder entries are never selected.
func trashToPurge(body string, files map[string]bool, since int64) []string {
	var keys []string
	for _, m := range davResponseRE.FindAllStringSubmatch(body, -1) {
		href, loc, ts := davHrefRE.FindStringSubmatch(m[1]), trashLocRE.FindStringSubmatch(m[1]), trashTSRE.FindStringSubmatch(m[1])
		if href == nil || loc == nil || ts == nil || strings.Contains(m[1], "<d:collection") {
			continue
		}
		when, err := strconv.ParseInt(ts[1], 10, 64)
		if err != nil || when < since || !files["/"+loc[1]] {
			continue
		}
		h := strings.TrimRight(href[1], "/")
		keys = append(keys, h[strings.LastIndex(h, "/")+1:])
	}
	return keys
}

// folderIsEmpty reports whether a PROPFIND Depth 1 answer lists the folder
// itself and nothing else.
func folderIsEmpty(body string) bool {
	return strings.Contains(body, "<d:multistatus") && len(davResponseRE.FindAllString(body, -1)) == 1
}

// runState is what a run records for the next one, so a run that died
// without its cleanup (Ctrl-C during a hang, the go test timeout, SIGKILL)
// is cleaned up later: its sidecar process, its token prefix, the files it
// created and its temporary directories.
type runState struct {
	Started    int64    `json:"started"` // Unix seconds; trash entries from before are never purged
	TestPID    int      `json:"test_pid"`
	SidecarPID int      `json:"sidecar_pid"`
	Binary     string   `json:"binary"`
	Prefix     string   `json:"prefix"`
	Files      []string `json:"files"`
	Dirs       []string `json:"dirs"`
	// MemoryDirs are the memory folders that did not exist before the run
	// (removed only while empty)
	MemoryDirs []string `json:"memory_dirs"`
	// ReadmeETag is the ETag of the README the run's enrolment created (empty:
	// the README is not the run's); it is deleted only while it still has it
	ReadmeETag string `json:"readme_etag"`
}

// stateFile is where the run state lives: one per user temp dir.
func stateFile() string { return filepath.Join(os.TempDir(), "ocis-agent-it", "state.json") }

// writeState writes the state file into its (already checked) directory.
func writeState(p string, st runState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func readState(p string) (runState, bool, error) {
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return runState{}, false, nil
	}
	if err != nil {
		return runState{}, false, err
	}
	var st runState
	if err := json.Unmarshal(b, &st); err != nil {
		return runState{}, false, err
	}
	return st, true, nil
}

// ownTempDir reports whether p is a directory the harness made with
// os.MkdirTemp(os.TempDir(), "ocis-agent-it-*"), so a later run may remove it.
func ownTempDir(p string) bool {
	root := filepath.Clean(os.TempDir())
	return filepath.Clean(p) == p && filepath.Dir(p) == root && strings.HasPrefix(filepath.Base(p), testFilePrefix)
}

// ensureStateDir creates the state directory if needed and checks it
// (checkStateDir): a directory planted by someone else is never used.
func ensureStateDir() (string, error) {
	dir := filepath.Dir(stateFile())
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", err
	}
	return dir, checkStateDir(dir, os.Getuid())
}

// checkStateDir accepts only a real directory (not a symlink) with mode 0700
// owned by uid.
func checkStateDir(dir string, uid int) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("%s is a symlink", dir)
	case !fi.IsDir():
		return fmt.Errorf("%s is not a directory", dir)
	case fi.Mode().Perm() != 0o700:
		return fmt.Errorf("%s has mode %o, want 700", dir, fi.Mode().Perm())
	case !ok || int(st.Uid) != uid:
		return fmt.Errorf("%s is not owned by uid %d", dir, uid)
	}
	return nil
}

// recordedBinary returns the sidecar binary a state file names, only if it is
// exactly what a harness builds: <one of its recorded own temp dirs>/ocis-agent.
func recordedBinary(st runState) (string, bool) {
	b := st.Binary
	if b == "" || filepath.Clean(b) != b || filepath.Base(b) != "ocis-agent" {
		return "", false
	}
	d := filepath.Dir(b)
	if !ownTempDir(d) {
		return "", false
	}
	for _, r := range st.Dirs {
		if r == d {
			return b, true
		}
	}
	return "", false
}

// argv0 is the first argument a live process was started with ("" if it is
// unknown: no such process, or a command line it cannot tell apart).
func argv0(pid int) string {
	if pid <= 0 {
		return ""
	}
	// An empty read is not an answer: right after exec the kernel can briefly
	// show no arguments, so fall through to ps.
	if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline"); err == nil && len(b) > 0 {
		first, _, _ := strings.Cut(string(b), "\x00")
		return first
	}
	out, err := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// runsExactly reports whether pid is alive and was started as exactly bin
// (argv[0]; a path with spaces never matches, which fails safe).
func runsExactly(pid int, bin string) bool {
	return bin != "" && argv0(pid) == bin
}

// lockInfo is the content of the run lock.
type lockInfo struct {
	PID   int    `json:"pid"`
	Argv0 string `json:"argv0"`
}

// acquireLock takes the run lock in dir (O_EXCL): one run at a time. A lock
// whose process no longer runs exactly the recorded test binary is stale and
// taken over; a damaged lock file is an error. release removes the lock if it
// is still ours; it may be called more than once.
func acquireLock(dir string) (release func(), err error) {
	lock := filepath.Join(dir, "lock")
	me := lockInfo{PID: os.Getpid(), Argv0: os.Args[0]}
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			b, _ := json.Marshal(me)
			_, werr := f.Write(b)
			if cerr := f.Close(); werr == nil {
				werr = cerr
			}
			if werr != nil {
				_ = os.Remove(lock)
				return nil, werr
			}
			var once sync.Once
			return func() {
				once.Do(func() {
					if b, err := os.ReadFile(lock); err == nil {
						var l lockInfo
						if json.Unmarshal(b, &l) == nil && l == me {
							_ = os.Remove(lock)
						}
					}
				})
			}, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		b, err := os.ReadFile(lock)
		if err != nil {
			return nil, err
		}
		var l lockInfo
		if json.Unmarshal(b, &l) != nil || l.PID <= 0 || l.Argv0 == "" {
			return nil, fmt.Errorf("the lock file %s is damaged: delete it after checking that no integration run is active", lock)
		}
		if runsExactly(l.PID, l.Argv0) {
			return nil, fmt.Errorf("another integration run (pid %d) holds %s", l.PID, lock)
		}
		if err := os.Remove(lock); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("could not take the lock %s", lock)
}

// readmeDeletable: the README is the run's only while it still has the ETag
// it had right after the run's enrolment created it.
func readmeDeletable(recorded, current string) bool { return recorded != "" && recorded == current }

var (
	davETagRE = regexp.MustCompile(`<d:getetag>([^<]*)</d:getetag>`)
)

// propfindInfo reads a PROPFIND Depth 0 answer of exactly one entry: its ETag
// and whether it is a folder.
func propfindInfo(body string) (etag string, isDir, ok bool) {
	rs := davResponseRE.FindAllStringSubmatch(body, -1)
	if !strings.Contains(body, "<d:multistatus") || len(rs) != 1 {
		return "", false, false
	}
	if m := davETagRE.FindStringSubmatch(rs[0][1]); m != nil {
		etag = m[1]
	}
	return etag, strings.Contains(rs[0][1], "<d:collection"), true
}

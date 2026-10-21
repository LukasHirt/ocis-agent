package integration

import (
	"encoding/json"
	"errors"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
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

// deletablePath reports whether the test may delete the file p: a file at the
// drive root named ocis-agent-it-*, or a memory file the test recorded as
// created that has one of the two shapes the test ever creates (the README
// of the agent folder, a note named ocis-agent-it-*.md). A recorded path of
// any other shape (a damaged state file) is refused. Folders are never
// deleted through this rule.
func deletablePath(p string, created map[string]bool) bool {
	if !strings.HasPrefix(p, "/") || path.Clean(p) != p {
		return false
	}
	if rootTestFile(p) {
		return true
	}
	if !created[p] {
		return false
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
}

// stateFile is where the run state lives: one per user temp dir.
func stateFile() string { return filepath.Join(os.TempDir(), "ocis-agent-it", "state.json") }

func writeState(p string, st runState) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
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

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/memory"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
)

const (
	schemaRecent = `{"type":"object","properties":{` +
		`"paths":{"type":"array","items":{"type":"string"},"maxItems":20,"description":"Optional: watch folders (or folders inside them) to check; default all watch folders"},` +
		`"since":{"type":"string","description":"Optional RFC 3339 time, at most 30 days ago; default 24 hours ago"},` +
		`"limit":{"type":"integer","minimum":1,"maximum":100,"description":"Optional: most entries to return, newest first; default 50"}` +
		`},"additionalProperties":false}`

	recentMaxPaths     = 20
	recentDefaultSince = 24 * time.Hour
	recentMaxSince     = 30 * 24 * time.Hour
	recentDefaultLimit = 50
	recentMaxLimit     = 100
	recentMaxDepth     = 4    // levels below a watch folder
	recentMaxScan      = 5000 // entries scanned per call, all folders together
	recentMaxRequests  = 200  // PROPFIND requests per call
	recentTimeout      = 2 * time.Minute
	// maxRecentBlockBytes keeps the block below the runner's per-block limit,
	// so a page of long names is cut rather than dropped whole.
	maxRecentBlockBytes = 48 << 10
)

type recentArgs struct {
	Paths []string `json:"paths"`
	Since *string  `json:"since"`
	Limit *int     `json:"limit"`
}

type recentCall struct {
	folders []string
	since   time.Time
	limit   int
}

type recentChanges struct {
	base
	now      func() time.Time
	maxScan  int
	maxDepth int
	timeout  time.Duration // the whole call; the walk stops with what it found
}

// NewRecentChanges returns files.recent_changes: what changed lately in the
// user's watch folders. The folders come from the policy (Env.WatchFolders);
// the arguments can only narrow them.
func NewRecentChanges() Tool { return newRecentChanges(time.Now) }

func newRecentChanges(now func() time.Time) recentChanges {
	return recentChanges{
		base: base{
			name: "files.recent_changes",
			desc: "List what changed recently in the user's watch folders (up to 4 levels deep), newest first. " +
				"Each line: kind (f file, d folder), size, modification time, ETag, path, tab-separated. " +
				"A folder's time and ETag change whenever anything inside it changes.",
			schema: schemaRecent, risk: RiskRead,
		},
		now: now, maxScan: recentMaxScan, maxDepth: recentMaxDepth, timeout: recentTimeout,
	}
}

// watchFolders returns the env's watch folders that are valid paths outside
// the agent's own folder. The policy API stores only such paths; this is the
// second line.
func watchFolders(env Env) []string {
	var out []string
	for _, w := range env.WatchFolders {
		c, err := cleanPath(w)
		if err != nil || memory.IsReserved(c) {
			continue
		}
		out = append(out, c)
	}
	return out
}

func inside(p, folder string) bool {
	return folder == "/" || p == folder || strings.HasPrefix(p, folder+"/")
}

// parse validates the arguments against the env's watch folders. It sends
// nothing to oCIS. Refusals never name a watch folder.
func (t recentChanges) parse(env Env, raw json.RawMessage) (recentCall, error) {
	a, err := parse[recentArgs](raw)
	if err != nil {
		return recentCall{}, err
	}
	now := t.now().UTC()
	c := recentCall{since: now.Add(-recentDefaultSince), limit: recentDefaultLimit}
	if a.Limit != nil {
		if *a.Limit < 1 || *a.Limit > recentMaxLimit {
			return recentCall{}, invalid("limit must be between 1 and %d", recentMaxLimit)
		}
		c.limit = *a.Limit
	}
	if a.Since != nil {
		s, err := time.Parse(time.RFC3339, *a.Since)
		if err != nil {
			return recentCall{}, invalid("since must be an RFC 3339 time such as 2026-01-02T15:04:05Z")
		}
		switch {
		case s.After(now):
			return recentCall{}, invalid("since must not be in the future")
		case s.Before(now.Add(-recentMaxSince)):
			return recentCall{}, invalid("since must be at most 30 days ago")
		}
		c.since = s.UTC()
	}
	if len(a.Paths) > recentMaxPaths {
		return recentCall{}, invalid("at most %d paths", recentMaxPaths)
	}
	var wanted []string
	for _, p := range a.Paths {
		cp, err := cleanPath(p)
		if err != nil {
			return recentCall{}, err
		}
		wanted = append(wanted, cp)
	}
	watch := watchFolders(env)
	if len(watch) == 0 {
		return recentCall{}, refuse("no watch folders are configured; the user can add them in the agent's settings")
	}
	if len(wanted) == 0 {
		wanted = watch
	}
	for _, p := range wanted {
		if memory.IsReserved(p) {
			return recentCall{}, refuse("paths under /.ocis-agent/ are reserved for the agent's memory; use the memory tools")
		}
		ok := false
		for _, w := range watch {
			if inside(p, w) {
				ok = true
				break
			}
		}
		if !ok {
			return recentCall{}, refuse("a requested path is not inside a watch folder")
		}
	}
	// walk each subtree once: drop a folder inside another requested one
	for i, p := range wanted {
		covered := false
		for j, q := range wanted {
			if j != i && inside(p, q) && (p != q || j < i) {
				covered = true
				break
			}
		}
		if !covered {
			c.folders = append(c.folders, p)
		}
	}
	return c, nil
}

func (t recentChanges) Prepare(_ context.Context, env Env, raw json.RawMessage) (Prepared, error) {
	_, err := t.parse(env, raw)
	return Prepared{}, err
}

func (t recentChanges) Execute(ctx context.Context, env Env, raw json.RawMessage, _ ExecInfo) (Result, error) {
	c, err := t.parse(env, raw)
	if err != nil {
		return Result{}, err
	}
	if env.Reads.Remaining() <= 0 {
		return Result{}, refuse("the job's read budget is used up")
	}
	parent := ctx
	ctx, cancel := context.WithTimeout(parent, t.timeout)
	defer cancel()
	seen := map[string]bool{}
	var changed []ocisclient.Entry
	unreadable, dropped, scanned, requests := 0, 0, 0, 0
	cut, timedOut, notChecked := false, false, 0
	for i, folder := range c.folders {
		if timedOut || scanned >= t.maxScan || requests >= recentMaxRequests {
			notChecked = len(c.folders) - i
			break
		}
		lim := ocisclient.WalkLimits{MaxDepth: t.maxDepth, MaxEntries: t.maxScan - scanned, MaxRequests: recentMaxRequests - requests}
		w, err := env.Files.Walk(ctx, env.Creds, env.DriveID, folder, lim, memory.IsReserved)
		requests += w.Requests
		switch {
		case errors.Is(err, ocisclient.ErrNotFound), errors.Is(err, ocisclient.ErrNotDir),
			errors.Is(err, ocisclient.ErrForbidden), errors.Is(err, ocisclient.ErrMalformed),
			errors.Is(err, ocisclient.ErrTooLarge):
			unreadable++
			continue
		case err != nil && ctx.Err() != nil && parent.Err() == nil:
			timedOut = true // the call's own deadline: keep what the walk found
		case err != nil:
			return Result{}, fmt.Errorf("tools: recent changes: %w", err)
		}
		scanned += len(w.Entries)
		unreadable += w.Skipped
		dropped += w.Dropped
		cut = cut || w.Truncated
		for _, e := range w.Entries {
			if seen[e.Path] || e.ModTime.Before(c.since) {
				continue
			}
			seen[e.Path] = true
			changed = append(changed, e)
		}
	}
	sort.Slice(changed, func(i, j int) bool {
		if !changed[i].ModTime.Equal(changed[j].ModTime) {
			return changed[i].ModTime.After(changed[j].ModTime)
		}
		return changed[i].Path < changed[j].Path
	})
	var lines []string
	size := 0
	for _, e := range changed {
		if len(lines) >= c.limit {
			break
		}
		l := fmtEntry(e)
		if size+len(l)+1 > maxRecentBlockBytes {
			break
		}
		lines = append(lines, l)
		size += len(l) + 1
	}
	content := strings.Join(lines, "\n")
	if err := env.Reads.Take(int64(len(content))); err != nil {
		return Result{}, refuse("the job's read budget is used up")
	}
	summary := fmt.Sprintf("%d entries changed since %s", len(changed), c.since.Format(time.RFC3339))
	if len(lines) < len(changed) {
		summary += fmt.Sprintf("; showing the newest %d", len(lines))
	}
	switch {
	case timedOut:
		summary += "; the scan stopped at its time limit"
	case cut || notChecked > 0:
		summary += "; the scan stopped at its limit"
	}
	if notChecked > 0 {
		summary += fmt.Sprintf("; %d folder(s) not checked", notChecked)
	}
	if unreadable > 0 {
		summary += fmt.Sprintf("; %d folder(s) could not be read", unreadable)
	}
	if dropped > 0 {
		summary += fmt.Sprintf("; %d entries skipped as unreadable names", dropped)
	}
	return Result{Summary: summary, Blocks: []Block{{Source: "changes", Content: content}}}, nil
}

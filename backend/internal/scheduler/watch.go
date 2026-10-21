package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/auth"
	"github.com/owncloud/ocis-agent/backend/internal/memory"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/tools"
)

// WatchConfig holds the settings of the watch-folder poll. Zero values take
// the defaults; negative values are refused.
type WatchConfig struct {
	PollInterval time.Duration // how often a user's watch folders are checked (default 5m)
	Debounce     time.Duration // how long they must stay unchanged before a job (default 1m)
	MaxWait      time.Duration // the longest a change waits for its job while changes keep coming (default 10m; >= Debounce)
	PerTick      int           // users polled per run (default 50)
	Parallel     int           // users polled at the same time (default 4)
}

func (c WatchConfig) withDefaults() (WatchConfig, error) {
	if c.PollInterval < 0 || c.Debounce < 0 || c.MaxWait < 0 || c.PerTick < 0 || c.Parallel < 0 {
		return c, errors.New("scheduler: negative watch config value")
	}
	if c.PollInterval == 0 {
		c.PollInterval = 5 * time.Minute
	}
	if c.Debounce == 0 {
		c.Debounce = time.Minute
	}
	if c.MaxWait == 0 {
		c.MaxWait = 10 * time.Minute
	}
	if c.PerTick == 0 {
		c.PerTick = 50
	}
	if c.Parallel == 0 {
		c.Parallel = 4
	}
	if c.Debounce > c.MaxWait {
		return c, errors.New("scheduler: watch debounce above the max wait")
	}
	return c, nil
}

// Watch-folder poll limits for a folder that holds the agent's own folder
// (the root): its listing is digested instead of its ETag.
const (
	maxWatchPages       = 10
	maxRootListEntries  = 5000
	listDigestPrefix    = "list:"
	watchFolderSkipped  = 0
	watchFolderSeen     = 1
	watchFolderVanished = 2
)

// watchState is the in-memory part of the poll: the rotation cursor, when
// each user is due again, and the users the current pass has seen. A
// restart loses it and polls everyone once more (the baselines and pending
// changes live in the cursors table).
type watchState struct {
	after string
	next  map[string]time.Time
	seen  map[string]bool
}

func newWatchState() watchState {
	return watchState{next: map[string]time.Time{}, seen: map[string]bool{}}
}

// folderResult is what one poll learned about one watch folder.
type folderResult struct {
	state int    // watchFolderSkipped, watchFolderSeen or watchFolderVanished
	value string // the ETag or listing digest (watchFolderSeen)
}

// watchPoll is one user's poll.
type watchPoll struct {
	user    string
	folders map[string]folderResult
	noCred  bool  // no usable credentials: nothing was polled
	envErr  error // building the environment failed: nothing was polled
	unauth  bool  // oCIS answered 401: the poll is discarded
	errs    []error
}

// watchRun polls the watch folders of the users that are due: enrolled,
// proactive work on, not paused, with watch folders (ListWatchUsers, in
// user id order from the rotation cursor), each at most once per
// PollInterval or, while a change is pending, once the debounce can have
// passed. At most PerTick users per run, Parallel at a time, with the app
// credentials; nothing while the kill switch is on. A folder is checked
// with a depth-0 PROPFIND: oCIS moves a folder's ETag whenever anything
// below it changes. The root, which holds the agent's own folder (memory
// writes move its ETag), is listed one level deep instead and digested
// without the agent's folder. Each user's result is applied in one
// transaction (applyWatch). It returns the number of users polled and the
// per-folder errors (a folder that fails keeps its baseline; a 401 is
// reported to Auth.HandleUnauthorized and discards the user's poll).
func (s *Scheduler) watchRun(ctx context.Context, now time.Time) (int, int, error) {
	var kill bool
	if err := s.d.Store.View(ctx, func(tx store.Tx) error {
		var err error
		kill, err = store.KillSwitchOn(tx)
		return err
	}); err != nil {
		return 0, 0, fmt.Errorf("scheduler: kill switch: %w", err)
	}
	if kill || s.c.Proactive.Limits.KillSwitchEnv {
		return 0, 0, nil
	}
	due, err := s.watchDue(ctx, now)
	if err != nil {
		return 0, 0, err
	}
	polls := make([]watchPoll, len(due))
	sem := make(chan struct{}, s.c.Watch.Parallel)
	var wg sync.WaitGroup
	for i, id := range due {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			polls[i] = s.pollUser(ctx, id)
		}()
	}
	wg.Wait()

	polled, started := 0, 0
	var errs []error
	defer func() {
		if started > 0 {
			s.d.Queue.Wake()
		}
		s.itemLog(ctx, TaskWatch, errs)
	}()
	st := &s.watch
	for _, p := range polls {
		errs = append(errs, p.errs...)
		st.next[p.user] = now.Add(s.c.Watch.PollInterval)
		switch {
		case p.noCred:
			continue
		case p.envErr != nil:
			errs = append(errs, p.envErr)
			continue
		}
		polled++
		if p.unauth {
			continue
		}
		next, ok, err := s.applyWatch(ctx, p, now)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !next.IsZero() {
			st.next[p.user] = next
		}
		if ok {
			started++
		}
	}
	return polled, len(errs), nil
}

// watchDue picks the users to poll in this run from the rotation cursor.
func (s *Scheduler) watchDue(ctx context.Context, now time.Time) ([]string, error) {
	st := &s.watch
	var due []string
	for page := 0; page < maxWatchPages; page++ {
		var ids []string
		if err := s.d.Store.View(ctx, func(tx store.Tx) error {
			var err error
			ids, err = tx.ListWatchUsers(st.after, s.c.Batch)
			return err
		}); err != nil {
			return nil, fmt.Errorf("scheduler: list watch users: %w", err)
		}
		for _, id := range ids {
			st.seen[id] = true
			if next, ok := st.next[id]; ok && now.Before(next) {
				st.after = id
				continue
			}
			due = append(due, id)
			st.after = id
			if len(due) >= s.c.Watch.PerTick {
				return due, nil
			}
		}
		if len(ids) < s.c.Batch {
			s.wrapWatch()
			return due, nil
		}
	}
	return due, nil
}

// wrapWatch ends a pass: the cursor restarts at the first user and users the
// pass did not see (no longer watching) are forgotten.
func (s *Scheduler) wrapWatch() {
	st := &s.watch
	for id := range st.next {
		if !st.seen[id] {
			delete(st.next, id)
		}
	}
	st.after = ""
	st.seen = map[string]bool{}
}

// pollUser checks every watch folder of the user. It talks to oCIS only.
func (s *Scheduler) pollUser(ctx context.Context, userID string) watchPoll {
	p := watchPoll{user: userID, folders: map[string]folderResult{}}
	env, err := s.d.Env(ctx, userID)
	switch {
	case errors.Is(err, auth.ErrCredentialInvalid):
		p.noCred = true
		return p
	case err != nil:
		p.envErr = err
		return p
	}
	for _, f := range env.WatchFolders {
		if ctx.Err() != nil {
			p.errs = append(p.errs, ctx.Err())
			return p
		}
		if _, done := p.folders[f]; done || memory.IsReserved(f) {
			continue
		}
		r, err := checkFolder(ctx, env, f)
		p.folders[f] = r
		if err == nil {
			continue
		}
		p.errs = append(p.errs, err)
		if errors.Is(err, ocisclient.ErrUnauthorized) {
			p.unauth = true
			if herr := s.d.Auth.HandleUnauthorized(ctx, userID); herr != nil {
				p.errs = append(p.errs, herr)
			}
			return p
		}
	}
	return p
}

// checkFolder returns what the folder looks like now. A folder that is gone
// (or is no longer a folder) is watchFolderVanished with a nil error; any
// other failure is watchFolderSkipped with the error.
func checkFolder(ctx context.Context, env tools.Env, folder string) (folderResult, error) {
	if memory.IsReserved(path.Join(folder, path.Base(memory.Root))) {
		return listDigest(ctx, env, folder)
	}
	e, err := env.Files.Stat(ctx, env.Creds, env.DriveID, folder)
	switch {
	case errors.Is(err, ocisclient.ErrNotFound), errors.Is(err, ocisclient.ErrNotDir):
		return folderResult{state: watchFolderVanished}, nil
	case err != nil:
		return folderResult{}, fmt.Errorf("scheduler: stat watch folder: %w", err)
	case !e.Dir:
		return folderResult{state: watchFolderVanished}, nil
	case e.ETag == "":
		return folderResult{}, fmt.Errorf("scheduler: stat watch folder: no etag: %w", ocisclient.ErrMalformed)
	}
	return folderResult{state: watchFolderSeen, value: e.ETag}, nil
}

// listDigest digests the folder's children (path, kind and ETag) without
// the agent's own folder: a change below a child moves the child's ETag.
func listDigest(ctx context.Context, env tools.Env, folder string) (folderResult, error) {
	res, err := env.Files.Walk(ctx, env.Creds, env.DriveID, folder,
		ocisclient.WalkLimits{MaxDepth: 1, MaxEntries: maxRootListEntries, MaxRequests: 1}, memory.IsReserved)
	switch {
	case errors.Is(err, ocisclient.ErrNotFound), errors.Is(err, ocisclient.ErrNotDir):
		return folderResult{state: watchFolderVanished}, nil
	case err != nil:
		return folderResult{}, fmt.Errorf("scheduler: list watch folder: %w", err)
	}
	h := sha256.New()
	for _, e := range res.Entries {
		typ := "f"
		if e.Dir {
			typ = "d"
		}
		fmt.Fprintf(h, "%d:%s%s %d:%s\n", len(e.Path), e.Path, typ, len(e.ETag), e.ETag)
	}
	fmt.Fprintf(h, "dropped %s truncated %t\n", strconv.Itoa(res.Dropped), res.Truncated)
	return folderResult{state: watchFolderSeen, value: listDigestPrefix + hex.EncodeToString(h.Sum(nil))}, nil
}

// applyWatch records one user's poll in one transaction: folders seen for
// the first time get a baseline, a moved ETag marks a pending change (first
// and latest time), a vanished folder loses its baseline, a failed one
// keeps it, and folders no longer on the policy's list are forgotten. A
// pending change whose folders stayed unchanged for Debounce (or that waits
// for MaxWait) starts a file_event job if store.CheckProactive allows it;
// otherwise it stays pending and the refusal goes to
// store.AuditProactiveSkip, which decides whether it is recorded.
// It returns when the user is due again (zero: the normal interval) and
// whether a job started.
func (s *Scheduler) applyWatch(ctx context.Context, p watchPoll, now time.Time) (time.Time, bool, error) {
	var next time.Time
	var started bool
	err := s.d.Store.InTx(ctx, func(tx store.Tx) error {
		next, started = time.Time{}, false
		pol, err := tx.GetPolicy(p.user)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		c, err := tx.GetCursor(p.user)
		if err != nil {
			return err
		}
		changed := false
		for f, r := range p.folders {
			if !slices.Contains(pol.Watch, f) {
				continue
			}
			switch r.state {
			case watchFolderVanished:
				delete(c.WatchETags, f)
			case watchFolderSeen:
				if old, had := c.WatchETags[f]; had && old != r.value {
					changed = true
				}
				c.WatchETags[f] = r.value
			}
		}
		for f := range c.WatchETags {
			if !slices.Contains(pol.Watch, f) {
				delete(c.WatchETags, f)
			}
		}
		if changed {
			at := now
			if c.WatchDirtySince == nil {
				c.WatchDirtySince = &at
			}
			c.WatchChangedAt = &at
		}
		if c.WatchDirtySince != nil {
			if c.WatchChangedAt == nil {
				c.WatchChangedAt = c.WatchDirtySince
			}
			quietAt := c.WatchChangedAt.Add(s.c.Watch.Debounce)
			deadline := c.WatchDirtySince.Add(s.c.Watch.MaxWait)
			if now.Before(quietAt) && now.Before(deadline) {
				next = quietAt
				if deadline.Before(next) {
					next = deadline
				}
			} else {
				reason, err := store.CheckProactive(tx, p.user, now, s.c.Proactive.Limits, true)
				if err != nil {
					return err
				}
				switch reason {
				case "":
					if _, err := store.StartProactive(tx, p.user, store.TriggerFileEvent, now, ""); err != nil {
						return err
					}
					c.WatchDirtySince, c.WatchChangedAt = nil, nil
					started = true
				case store.SkipNotEnrolled:
					return nil
				case store.SkipDisabled:
					c.WatchDirtySince, c.WatchChangedAt = nil, nil
				case store.SkipKillSwitch:
				default:
					if _, err := store.AuditProactiveSkip(tx, p.user, store.TriggerFileEvent, reason, now); err != nil {
						return err
					}
				}
			}
		}
		return tx.PutCursor(c)
	})
	if err != nil {
		return time.Time{}, false, err
	}
	return next, started, nil
}

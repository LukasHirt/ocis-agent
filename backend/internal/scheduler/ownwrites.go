package scheduler

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/memory"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/tools"
)

// ownWriteTimeout bounds each check around an approved write;
// ownWriteEndedBudget bounds the whole hook once the job's context ended (it
// must not use up the shutdown margin).
const (
	ownWriteTimeout     = 15 * time.Second
	ownWriteEndedBudget = 3 * time.Second
)

// OwnWritesConfig holds the waits after an approved write. Zero values take
// the defaults.
type OwnWritesConfig struct {
	// Settle is how long a folder must keep its new value before it is
	// recorded (default 1s): oCIS moves the parent's ETag a second time when
	// an upload's postprocessing ends, tens of milliseconds after the PUT
	// returned on the dev stack, longer with virus scanning.
	Settle time.Duration
	// MaxWait bounds the wait for postprocessing and a settled folder
	// (default 10s); past it the rebaseline is skipped.
	MaxWait time.Duration
	// Poll is the pause between two postprocessing checks (default 200ms).
	Poll time.Duration
}

func (c OwnWritesConfig) withDefaults() OwnWritesConfig {
	if c.Settle <= 0 {
		c.Settle = time.Second
	}
	if c.MaxWait <= 0 {
		c.MaxWait = 10 * time.Second
	}
	if c.Poll <= 0 {
		c.Poll = 200 * time.Millisecond
	}
	return c
}

// OwnWrites keeps the agent's own approved writes from looking like user
// changes to the watch-folder poll: without it, every approved write into
// a watched folder would start a follow-up file_event review. Around the
// write it checks the watch folders that contain the call's paths, with the
// same check the poll uses (checkFolder). After the write it waits until
// oCIS has postprocessed the files among the targets and each folder has
// kept its value for Settle, then moves the user's stored baseline of each
// folder to that value only if it still equals the value seen before the
// write (a compare-and-swap). A user change made before the write therefore
// still fires; one landing inside the write window (up to MaxWait after the
// write) is taken for the agent's, an accepted residual risk like the ETag
// race of the write tools. It is best effort: any failed check, a file still
// processed at MaxWait or a folder that does not settle skips the folder, and
// the poll then sees the write as a change.
//
// While a write and its rebaseline are under way, the folders are marked as
// being written (in memory: there is one worker process per database) and
// the watch poll leaves them alone (Writing), so a poll that lands in the
// window, for example one brought forward by the write's own notification
// event, cannot record the write as a change. A mark ends with the hook or,
// if the hook never runs, after markTTL.
type OwnWrites struct {
	st      store.Store
	report  func(ctx context.Context, userID string) error
	cfg     OwnWritesConfig
	markTTL time.Duration

	mu    sync.Mutex
	marks map[string]map[string]int64 // user, folder: write id
	ends  map[int64]time.Time         // write id: when its mark expires
	seq   int64
}

// NewOwnWrites returns the rebaseline over the store. report, if not nil, is
// told about a 401 for the user's app credentials (Auth.HandleUnauthorized).
func NewOwnWrites(st store.Store, report func(ctx context.Context, userID string) error, cfg OwnWritesConfig) (*OwnWrites, error) {
	if st == nil {
		return nil, errors.New("scheduler: own writes need a store")
	}
	cfg = cfg.withDefaults()
	return &OwnWrites{
		st: st, report: report, cfg: cfg, markTTL: cfg.MaxWait + 3*ownWriteTimeout,
		marks: map[string]map[string]int64{}, ends: map[int64]time.Time{},
	}, nil
}

// Writing reports whether the agent is writing into the user's watch folder
// (between BeforeWrite and the end of its hook, at most markTTL).
func (o *OwnWrites) Writing(userID, folder string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	id, ok := o.marks[userID][folder]
	if !ok {
		return false
	}
	if time.Now().After(o.ends[id]) {
		o.clearLocked(userID, id)
		return false
	}
	return true
}

func (o *OwnWrites) mark(userID string, folders []string) int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seq++
	if o.marks[userID] == nil {
		o.marks[userID] = map[string]int64{}
	}
	for _, f := range folders {
		o.marks[userID][f] = o.seq
	}
	o.ends[o.seq] = time.Now().Add(o.markTTL)
	return o.seq
}

func (o *OwnWrites) unmark(userID string, id int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.clearLocked(userID, id)
}

// clearLocked drops the marks of write id (a later write of the same folder
// keeps its own).
func (o *OwnWrites) clearLocked(userID string, id int64) {
	for f, w := range o.marks[userID] {
		if w == id {
			delete(o.marks[userID], f)
		}
	}
	if len(o.marks[userID]) == 0 {
		delete(o.marks, userID)
	}
	delete(o.ends, id)
}

// BeforeWrite checks the watch folders that contain any of paths (the call's
// targets: source and destination of a move or copy) and have a stored
// baseline, for a user with proactive work on. It returns the function to
// call after the call, with done true only if the write was done, or nil
// when there is nothing to do; the function must be called (it ends the
// marks). It talks to oCIS with env's credentials and never changes anything
// itself. The returned function finishes even when its context has ended (the
// write is done), but then it does not wait and takes at most a few seconds.
func (o *OwnWrites) BeforeWrite(ctx context.Context, env tools.Env, paths []string) func(ctx context.Context, done bool) {
	folders, err := o.folders(ctx, env.UserID, paths)
	if err != nil || len(folders) == 0 {
		return nil
	}
	id := o.mark(env.UserID, folders)
	pre := check(ctx, env, folders)
	if len(pre) == 0 {
		o.unmark(env.UserID, id)
		return nil
	}
	var targets []string
	for _, p := range paths {
		if slices.ContainsFunc(folders, func(f string) bool { return within(f, p) }) {
			targets = append(targets, p)
		}
	}
	return func(job context.Context, done bool) {
		defer o.unmark(env.UserID, id)
		if !done {
			return
		}
		budget := o.cfg.MaxWait + 2*ownWriteTimeout
		if job.Err() != nil {
			budget = ownWriteEndedBudget
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(job), budget)
		defer cancel()
		post, unauthorized := o.settled(ctx, job, env, pre, targets)
		if unauthorized && o.report != nil {
			_ = o.report(ctx, env.UserID)
		}
		moved := map[string]string{}
		for f, v := range post {
			if p, ok := pre[f]; ok && p != v {
				moved[f] = v
			}
		}
		if len(moved) == 0 {
			return
		}
		_ = o.st.InTx(ctx, func(tx store.Tx) error {
			c, err := tx.GetCursor(env.UserID)
			if err != nil {
				return err
			}
			changed := false
			for f, v := range moved {
				if cur, had := c.WatchETags[f]; had && cur == pre[f] {
					c.WatchETags[f] = v
					changed = true
				}
			}
			if !changed {
				return nil
			}
			return tx.PutCursor(c)
		})
	}
}

// settled waits until no file among targets is postprocessed any more and
// then returns the folders of pre whose value stayed the same for Settle.
// Once job has ended it waits no more: a file still processed then skips
// everything, and the folders are checked twice in a row.
func (o *OwnWrites) settled(ctx, job context.Context, env tools.Env, pre map[string]string, targets []string) (map[string]string, bool) {
	deadline := time.Now().Add(o.cfg.MaxWait)
	unauthorized := false
	failed := func(err error) bool {
		unauthorized = unauthorized || errors.Is(err, ocisclient.ErrUnauthorized)
		return err != nil
	}
	for _, p := range targets {
		e, err := env.Files.Stat(ctx, env.Creds, env.DriveID, p)
		if errors.Is(err, ocisclient.ErrNotFound) || (err == nil && e.Dir) {
			continue // deleted, moved away or a folder: nothing to process
		}
		if failed(err) {
			return nil, unauthorized
		}
		for {
			busy, err := env.Files.Processing(ctx, env.Creds, env.DriveID, p)
			if failed(err) {
				return nil, unauthorized
			}
			if !busy {
				break
			}
			if job.Err() != nil || !time.Now().Before(deadline) || !sleep(ctx, o.cfg.Poll) {
				return nil, unauthorized
			}
		}
	}
	out := map[string]string{}
	last, unauth := checkAll(ctx, env, slices.Sorted(maps.Keys(pre)))
	unauthorized = unauthorized || unauth
	for len(last) > 0 {
		if job.Err() == nil && !sleep(ctx, o.cfg.Settle) {
			return out, unauthorized
		}
		now, unauth := checkAll(ctx, env, slices.Sorted(maps.Keys(last)))
		unauthorized = unauthorized || unauth
		next := map[string]string{}
		for f, v := range now {
			if last[f] == v {
				out[f] = v
			} else {
				next[f] = v
			}
		}
		last = next
		if len(last) > 0 && (job.Err() != nil || !time.Now().Before(deadline)) {
			return out, unauthorized // the rest did not settle
		}
	}
	return out, unauthorized
}

// folders returns the watch folders with a baseline that contain a path.
func (o *OwnWrites) folders(ctx context.Context, userID string, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	var out []string
	err := o.st.View(ctx, func(tx store.Tx) error {
		pol, err := tx.GetPolicy(userID)
		if err != nil {
			return err
		}
		if !pol.ProactiveEnabled {
			return nil
		}
		c, err := tx.GetCursor(userID)
		if err != nil {
			return err
		}
		for _, f := range pol.Watch {
			if _, had := c.WatchETags[f]; !had || memory.IsReserved(f) || slices.Contains(out, f) {
				continue
			}
			if slices.ContainsFunc(paths, func(p string) bool { return within(f, p) }) {
				out = append(out, f)
			}
		}
		return nil
	})
	return out, err
}

// within reports whether p is strictly inside folder f (the folder itself
// is not: a write to it moves or removes the watch folder).
func within(f, p string) bool {
	if f == "/" {
		return p != "/" && strings.HasPrefix(p, "/")
	}
	return strings.HasPrefix(p, f+"/")
}

// check returns the current value of each folder that was seen (a vanished
// or failing folder is left out).
func check(ctx context.Context, env tools.Env, folders []string) map[string]string {
	out, _ := checkAll(ctx, env, folders)
	return out
}

// checkAll is check that also reports whether oCIS answered 401.
func checkAll(ctx context.Context, env tools.Env, folders []string) (map[string]string, bool) {
	ctx, cancel := context.WithTimeout(ctx, ownWriteTimeout)
	defer cancel()
	out := map[string]string{}
	unauthorized := false
	for _, f := range folders {
		r, err := checkFolder(ctx, env, f)
		unauthorized = unauthorized || errors.Is(err, ocisclient.ErrUnauthorized)
		if err != nil || r.state != watchFolderSeen || ctx.Err() != nil {
			continue
		}
		out[f] = r.value
	}
	return out, unauthorized
}

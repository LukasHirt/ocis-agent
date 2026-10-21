package scheduler

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/memory"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/tools"
)

// ownWriteTimeout bounds each check around an approved write.
const ownWriteTimeout = 15 * time.Second

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
type OwnWrites struct {
	st  store.Store
	cfg OwnWritesConfig
}

// NewOwnWrites returns the rebaseline over the store.
func NewOwnWrites(st store.Store, cfg OwnWritesConfig) (*OwnWrites, error) {
	if st == nil {
		return nil, errors.New("scheduler: own writes need a store")
	}
	return &OwnWrites{st: st, cfg: cfg.withDefaults()}, nil
}

// BeforeWrite checks the watch folders that contain any of paths (the call's
// targets: source and destination of a move or copy) and have a stored
// baseline, for a user with proactive work on. It returns the function to
// call once the write is done, or nil when there is nothing to do. It talks
// to oCIS with env's credentials and never changes anything itself. The
// returned function finishes even when its context has ended (the write is
// done), but then it does not wait.
func (o *OwnWrites) BeforeWrite(ctx context.Context, env tools.Env, paths []string) func(context.Context) {
	folders, err := o.folders(ctx, env.UserID, paths)
	if err != nil || len(folders) == 0 {
		return nil
	}
	pre := check(ctx, env, folders)
	if len(pre) == 0 {
		return nil
	}
	var targets []string
	for _, p := range paths {
		if slices.ContainsFunc(folders, func(f string) bool { return within(f, p) }) {
			targets = append(targets, p)
		}
	}
	return func(job context.Context) {
		budget := o.cfg.MaxWait + 2*ownWriteTimeout
		if job.Err() != nil {
			budget = ownWriteTimeout
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(job), budget)
		defer cancel()
		post := o.settled(ctx, job, env, pre, targets)
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
func (o *OwnWrites) settled(ctx, job context.Context, env tools.Env, pre map[string]string, targets []string) map[string]string {
	deadline := time.Now().Add(o.cfg.MaxWait)
	pause := func(d time.Duration) bool {
		if job.Err() != nil {
			return true
		}
		return sleep(ctx, d)
	}
	for _, p := range targets {
		e, err := env.Files.Stat(ctx, env.Creds, env.DriveID, p)
		if errors.Is(err, ocisclient.ErrNotFound) || (err == nil && e.Dir) {
			continue // deleted, moved away or a folder: nothing to process
		}
		if err != nil {
			return nil
		}
		for {
			busy, err := env.Files.Processing(ctx, env.Creds, env.DriveID, p)
			if err != nil {
				return nil
			}
			if !busy {
				break
			}
			if job.Err() != nil || !time.Now().Before(deadline) || !sleep(ctx, o.cfg.Poll) {
				return nil
			}
		}
	}
	out := map[string]string{}
	last := check(ctx, env, slices.Sorted(maps.Keys(pre)))
	for len(last) > 0 {
		if !pause(o.cfg.Settle) {
			return out
		}
		now := check(ctx, env, slices.Sorted(maps.Keys(last)))
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
			return out // the rest did not settle
		}
	}
	return out
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
	ctx, cancel := context.WithTimeout(ctx, ownWriteTimeout)
	defer cancel()
	out := map[string]string{}
	for _, f := range folders {
		r, err := checkFolder(ctx, env, f)
		if err != nil || r.state != watchFolderSeen || ctx.Err() != nil {
			continue
		}
		out[f] = r.value
	}
	return out
}

package scheduler

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/memory"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/tools"
)

// ownWriteTimeout bounds each of the two checks around an approved write.
const ownWriteTimeout = 15 * time.Second

// OwnWrites keeps the agent's own approved writes from looking like user
// changes to the watch-folder poll: without it, every approved write into
// a watched folder would start a follow-up file_event review. Around the
// write it checks the watch folders that contain the call's paths, with the
// same check the poll uses (checkFolder), and afterwards moves the user's
// stored baseline of each folder to the new value only if it still equals
// the value seen before the write (a compare-and-swap). A user change made
// before the write therefore still fires; one landing inside the write
// window is taken for the agent's (an accepted residual risk, like the
// ETag race of the write tools). It is best effort: any failed check skips
// the folder, and the poll then sees the write as a change.
type OwnWrites struct {
	st store.Store
}

// NewOwnWrites returns the rebaseline over the store.
func NewOwnWrites(st store.Store) (*OwnWrites, error) {
	if st == nil {
		return nil, errors.New("scheduler: own writes need a store")
	}
	return &OwnWrites{st: st}, nil
}

// BeforeWrite checks the watch folders that contain any of paths (the call's
// targets: source and destination of a move or copy) and have a stored
// baseline, for a user with proactive work on. It returns the function to
// call once the write is done, or nil when there is nothing to do. It talks
// to oCIS with env's credentials and never changes anything itself.
func (o *OwnWrites) BeforeWrite(ctx context.Context, env tools.Env, paths []string) func(context.Context) {
	folders, err := o.folders(ctx, env.UserID, paths)
	if err != nil || len(folders) == 0 {
		return nil
	}
	pre := check(ctx, env, folders)
	if len(pre) == 0 {
		return nil
	}
	return func(ctx context.Context) {
		post := check(ctx, env, slices.Sorted(maps.Keys(pre)))
		moved := map[string]string{}
		for f, v := range post {
			if p, ok := pre[f]; ok && p != v {
				moved[f] = v
			}
		}
		if len(moved) == 0 {
			return
		}
		tctx, cancel := context.WithTimeout(ctx, ownWriteTimeout)
		defer cancel()
		_ = o.st.InTx(tctx, func(tx store.Tx) error {
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

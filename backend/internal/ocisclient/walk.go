package ocisclient

import (
	"context"
	"errors"
)

// WalkLimits bound a Walk. All three must be positive.
type WalkLimits struct {
	MaxDepth    int // levels below the root: 1 lists only the root's children
	MaxEntries  int // entries collected in total
	MaxRequests int // PROPFIND requests sent in total
}

// WalkResult is what a Walk found.
type WalkResult struct {
	Entries []Entry // level by level, each folder's children sorted by path
	// Truncated: a cap stopped the walk while entries or folders were left
	// unlisted.
	Truncated bool
	Skipped   int // sub-folders that could not be listed and were left out
	// Dropped counts entries left out because oCIS sent an unusable name for
	// them (a control character, for example); a dropped folder's content is
	// not listed either.
	Dropped  int
	Requests int // PROPFIND requests sent
}

// Walk lists the folder root and its sub-folders breadth first with depth-1
// PROPFINDs (oCIS does not serve Depth: infinity), within lim. The root itself
// is not an entry. skip, if not nil, leaves out an entry (and, for a folder,
// everything below it). Unlike List, a listing with an unusable entry name
// drops only that entry (counted in Dropped), so one hostile name cannot hide
// its siblings. An error listing the root is returned as is, with no entries.
// A sub-folder that vanished, is not a folder any more, is forbidden, is too
// large to list or answers with a malformed listing is counted in Skipped;
// any other error (a 401, a server or transport error, the context) ends the
// walk with that error and the entries found so far.
func (c *Client) Walk(ctx context.Context, cr Creds, drive, root string, lim WalkLimits, skip func(path string) bool) (WalkResult, error) {
	var res WalkResult
	if lim.MaxDepth <= 0 || lim.MaxEntries <= 0 || lim.MaxRequests <= 0 {
		return res, errors.New("ocisclient: walk limits must be positive")
	}
	type folder struct {
		path  string
		depth int // depth of the folder's children
	}
	queue := []folder{{path: root, depth: 1}}
	for len(queue) > 0 {
		if res.Requests >= lim.MaxRequests || len(res.Entries) >= lim.MaxEntries {
			res.Truncated = true
			break
		}
		f := queue[0]
		queue = queue[1:]
		res.Requests++
		children, dropped, err := c.list(ctx, cr, drive, f.path, true)
		if err != nil {
			if res.Requests == 1 { // the root itself
				return WalkResult{Requests: res.Requests}, err
			}
			if errors.Is(err, ErrNotFound) || errors.Is(err, ErrNotDir) || errors.Is(err, ErrForbidden) ||
				errors.Is(err, ErrTooLarge) || errors.Is(err, ErrMalformed) {
				res.Skipped++
				continue
			}
			return res, err
		}
		res.Dropped += dropped
		for _, e := range children {
			if skip != nil && skip(e.Path) {
				continue
			}
			if len(res.Entries) >= lim.MaxEntries {
				res.Truncated = true
				return res, nil
			}
			res.Entries = append(res.Entries, e)
			if e.Dir && f.depth < lim.MaxDepth {
				queue = append(queue, folder{path: e.Path, depth: f.depth + 1})
			}
		}
	}
	return res, nil
}

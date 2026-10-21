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
	Entries   []Entry // level by level, each folder's children sorted by path
	Truncated bool    // a cap stopped the walk before it was complete
	Skipped   int     // sub-folders that could not be listed and were left out
	Requests  int     // PROPFIND requests sent
}

// Walk lists the folder root and its sub-folders breadth first with depth-1
// PROPFINDs (oCIS does not serve Depth: infinity), within lim. The root itself
// is not an entry. skip, if not nil, leaves out an entry (and, for a folder,
// everything below it). An error listing the root is returned as is. A
// sub-folder that vanished, is not a folder any more, is forbidden or answers
// with a malformed listing is counted in Skipped; any other error (a 401, a
// server or transport error, the context) ends the walk with that error.
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
		if res.Requests >= lim.MaxRequests {
			res.Truncated = true
			break
		}
		f := queue[0]
		queue = queue[1:]
		res.Requests++
		children, err := c.List(ctx, cr, drive, f.path)
		if err != nil {
			if res.Requests == 1 { // the root itself
				return WalkResult{Requests: res.Requests}, err
			}
			if errors.Is(err, ErrNotFound) || errors.Is(err, ErrNotDir) || errors.Is(err, ErrForbidden) || errors.Is(err, ErrMalformed) {
				res.Skipped++
				continue
			}
			return res, err
		}
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

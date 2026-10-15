// Package memory keeps the agent's Markdown memory in the user's personal
// space under /.ocis-agent/memory/, through the ocisclient package. Memory is
// untrusted data: other people with write access to the space, or the model
// itself in an earlier job, may have written it. Nothing in it is policy.
package memory

import (
	"errors"
	"path"
	"regexp"
	"strings"
)

// Root is the agent's private folder in the personal space.
const Root = "/.ocis-agent"

const (
	memoryDir  = Root + "/memory"
	notesDir   = memoryDir + "/notes"
	readmePath = Root + "/README.md"
)

// ErrInvalidName is returned for a memory file name outside the allowed set.
var ErrInvalidName = errors.New("memory: invalid file name")

var slugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// ResolveName maps a memory file name to its path in the personal space. The
// allowed names are profile.md, projects.md and notes/<slug>.md; everything
// else (other folders, other extensions, path tricks) is refused.
func ResolveName(name string) (string, error) {
	switch name {
	case "profile.md", "projects.md":
		return memoryDir + "/" + name, nil
	}
	if rest, ok := strings.CutPrefix(name, "notes/"); ok {
		if slug, ok := strings.CutSuffix(rest, ".md"); ok && slugRE.MatchString(slug) {
			return notesDir + "/" + slug + ".md", nil
		}
	}
	return "", ErrInvalidName
}

// IsReserved reports whether p is the agent's private folder or inside it.
// The path is cleaned first and compared without regard to case, so "..",
// doubled slashes and a different spelling of the folder name do not escape
// the check. Every file tool other than memory.* must refuse to write where
// this returns true.
func IsReserved(p string) bool {
	c := strings.ToLower(path.Clean("/" + p))
	root := strings.ToLower(Root)
	return c == root || strings.HasPrefix(c, root+"/")
}

package tools

import (
	"encoding/json"
	"slices"
)

// TargetPaths returns the cleaned paths an effectful call's arguments name
// (its "path", "from" and "to"), in that order and without duplicates, as
// the call's preview lists them. Arguments that do not parse, and paths that
// cleanPath refuses, give nothing: the call itself would refuse them.
func TargetPaths(args json.RawMessage) []string {
	var a struct {
		Path, From, To string
	}
	if json.Unmarshal(args, &a) != nil {
		return nil
	}
	var out []string
	for _, p := range []string{a.Path, a.From, a.To} {
		if p == "" {
			continue
		}
		c, err := cleanPath(p)
		if err != nil {
			continue
		}
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out
}

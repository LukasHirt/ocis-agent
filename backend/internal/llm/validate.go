package llm

import (
	"encoding/json"
	"fmt"
	"regexp"
)

const (
	maxTools   = 64
	maxNameLen = 62
)

// nameRE is the tool-name grammar: dot-separated segments of lower-case
// letters and digits with single underscores inside a segment. Because no
// name contains "__" or an underscore next to a dot, mapping "." to "__" on
// the wire is reversible.
var nameRE = regexp.MustCompile(`^[a-z][a-z0-9]*(?:_[a-z0-9]+)*(?:\.[a-z][a-z0-9]*(?:_[a-z0-9]+)*)*$`)

func validName(n string) bool { return len(n) <= maxNameLen && nameRE.MatchString(n) }

var modelRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,127}$`)

// ValidModelID reports whether id is a plausible provider model id: ASCII
// letters, digits and . _ : / @ + - only, at most 128 bytes.
func ValidModelID(id string) bool { return modelRE.MatchString(id) }

func invalid(msg string) error { return fmt.Errorf("%w: %s", ErrInvalid, msg) }

// Validate checks the request before anything is sent. Messages never echo
// content or arguments.
func (r CompletionRequest) Validate() error {
	if r.Model != "" && !ValidModelID(r.Model) {
		return invalid("bad model id")
	}
	if len(r.Messages) == 0 {
		return invalid("no messages")
	}
	if r.MaxTokens < 0 {
		return invalid("negative max tokens")
	}
	if len(r.Tools) > maxTools {
		return invalid("too many tools")
	}
	seen := make(map[string]bool, len(r.Tools))
	for _, t := range r.Tools {
		if !validName(t.Name) {
			return invalid("bad tool name")
		}
		if seen[t.Name] {
			return invalid("duplicate tool name")
		}
		seen[t.Name] = true
		if len(t.Schema) > 0 {
			var m map[string]json.RawMessage
			if json.Unmarshal(t.Schema, &m) != nil || m == nil {
				return invalid("tool schema must be a JSON object")
			}
		}
	}
	for i, m := range r.Messages {
		switch m.Role {
		case RoleSystem, RoleUser:
			if len(m.ToolCalls) > 0 || m.ToolCallID != "" {
				return invalid(fmt.Sprintf("message %d: unexpected tool fields", i))
			}
		case RoleAssistant:
			if m.ToolCallID != "" {
				return invalid(fmt.Sprintf("message %d: unexpected tool call id", i))
			}
			for _, c := range m.ToolCalls {
				if c.ID == "" || !validName(c.Name) {
					return invalid(fmt.Sprintf("message %d: bad tool call", i))
				}
			}
		case RoleTool:
			if m.ToolCallID == "" || len(m.ToolCalls) > 0 {
				return invalid(fmt.Sprintf("message %d: bad tool result", i))
			}
		default:
			return invalid(fmt.Sprintf("message %d: unknown role", i))
		}
	}
	return nil
}

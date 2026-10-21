package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/owncloud/ocis-agent/backend/internal/llm"
)

const (
	// MaxCheckpointBytes caps a marshalled checkpoint.
	MaxCheckpointBytes = 1 << 20
	// MaxSeenKeys caps the distinct loop-detector keys.
	MaxSeenKeys = 256
)

// Checkpoint is the resumable state of a job.
type Checkpoint struct {
	V        int            `json:"v"`
	Messages []llm.Message  `json:"messages"`
	NextCall int            `json:"next_call"`
	Turns    int            `json:"turns"`
	Seen     map[string]int `json:"seen"`
	// Counted holds the call ids of the current turn already noted in Seen,
	// so a call classified again after a failed run is not counted twice.
	// A new turn starts with an empty set.
	Counted map[string]bool `json:"counted,omitempty"`
	Answer  string          `json:"answer,omitempty"`
}

// ParseCheckpoint decodes a stored checkpoint. Empty input, "null" and "{}"
// give a fresh checkpoint.
func ParseCheckpoint(raw json.RawMessage) (Checkpoint, error) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "null" || string(t) == "{}" {
		return Checkpoint{V: 1, Seen: map[string]int{}}, nil
	}
	var cp Checkpoint
	if err := json.Unmarshal(t, &cp); err != nil {
		return Checkpoint{}, fmt.Errorf("agent: parse checkpoint: %w", err)
	}
	var keys map[string]json.RawMessage
	_ = json.Unmarshal(t, &keys) // already known to be an object
	if _, hasV := keys["v"]; !hasV && cp.V == 0 && len(cp.Messages) == 0 && cp.NextCall == 0 &&
		cp.Turns == 0 && len(cp.Seen) == 0 && len(cp.Counted) == 0 && cp.Answer == "" {
		return Checkpoint{V: 1, Seen: map[string]int{}}, nil
	}
	if cp.V != 1 {
		return Checkpoint{}, fmt.Errorf("agent: unknown checkpoint version %d", cp.V)
	}
	if cp.Seen == nil {
		cp.Seen = map[string]int{}
	}
	return cp, nil
}

// Marshal encodes the checkpoint, refusing one over MaxCheckpointBytes.
func (c Checkpoint) Marshal() (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // do not inflate <, > and & six-fold
	if err := enc.Encode(c); err != nil {
		return nil, fmt.Errorf("agent: marshal checkpoint: %w", err)
	}
	b := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	if len(b) > MaxCheckpointBytes {
		return nil, errors.New("agent: checkpoint too large")
	}
	return b, nil
}

// NewCallID returns the next call id and advances the counter.
func (c *Checkpoint) NewCallID() string {
	id := "c" + strconv.Itoa(c.NextCall)
	c.NextCall++
	return id
}

// Unresolved returns the tool calls of the last assistant message that have no
// tool message answering them, in call order.
func (c Checkpoint) Unresolved() []llm.ToolCall {
	if len(c.Messages) == 0 {
		return nil
	}
	// Skip the tool answers at the end; the message before them must be the
	// assistant message that made the calls.
	last := len(c.Messages) - 1
	for last >= 0 && c.Messages[last].Role == llm.RoleTool {
		last--
	}
	if last >= 0 && c.Messages[last].Role != llm.RoleAssistant {
		return nil
	}
	if last < 0 || len(c.Messages[last].ToolCalls) == 0 {
		return nil
	}
	answered := map[string]bool{}
	for _, m := range c.Messages[last+1:] {
		if m.Role == llm.RoleTool {
			answered[m.ToolCallID] = true
		}
	}
	var out []llm.ToolCall
	for _, tc := range c.Messages[last].ToolCalls {
		if !answered[tc.ID] {
			out = append(out, tc)
		}
	}
	return out
}

// Note counts an occurrence of key for loop detection and returns the count.
// A new key beyond MaxSeenKeys is not stored and returns MaxSeenKeys.
func (c *Checkpoint) Note(key string) int {
	if c.Seen == nil {
		c.Seen = map[string]int{}
	}
	n, ok := c.Seen[key]
	if !ok && len(c.Seen) >= MaxSeenKeys {
		return MaxSeenKeys
	}
	n++
	c.Seen[key] = n
	return n
}

// NoteCall counts key for the call callID once: a call id already counted
// returns the current count of key without counting again (MaxSeenKeys if
// key was not stored because Seen was full). At most MaxSeenKeys call ids
// are remembered.
func (c *Checkpoint) NoteCall(callID, key string) int {
	if c.Counted[callID] {
		if n, ok := c.Seen[key]; ok {
			return n
		}
		return MaxSeenKeys // Seen was full when the call was counted
	}
	if c.Counted == nil {
		c.Counted = map[string]bool{}
	}
	if len(c.Counted) < MaxSeenKeys {
		c.Counted[callID] = true
	}
	return c.Note(key)
}

// Fixed, model-safe and operator-safe refusal texts.
const (
	ReasonStepLimit      = "The agent stopped because this task needed more steps than allowed."
	ReasonRunsLimit      = "The agent stopped because this task was restarted too many times."
	ReasonLoop           = "The agent stopped because it kept repeating the same action."
	ReasonBudget         = "The agent stopped because the usage budget is used up."
	ReasonKillSwitch     = "The agent is switched off by the operator."
	ReasonCredentials    = "The agent could not access your files with its stored credentials."
	ReasonModelAuth      = "The language model provider refused the configured credentials."
	ReasonModelRejected  = "The language model provider rejected the request."
	ReasonModelBad       = "The language model returned an unusable answer."
	ReasonCutOff         = "The language model answer was cut off before it was complete."
	ReasonTooLarge       = "The task grew too large to continue."
	ReasonInternal       = "The agent stopped because of an internal error."
	ReasonRejectedByUser = "The user rejected this action."
	ReasonProactiveOff   = "The agent stopped because proactive work was switched off."
)

package llm

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

type wireRequest struct {
	Model     string        `json:"model"`
	Messages  []wireMessage `json:"messages"`
	Tools     []wireTool    `json:"tools,omitempty"`
	MaxTokens int           `json:"max_tokens,omitempty"`
	// ReasoningEffort is omitted unless the operator configured it.
	ReasoningEffort string             `json:"reasoning_effort,omitempty"`
	Stream          bool               `json:"stream,omitempty"`
	StreamOptions   *wireStreamOptions `json:"stream_options,omitempty"`
}

type wireStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireMessage struct {
	Role       string     `json:"role"`
	Content    *string    `json:"content"`
	ToolCalls  []wireCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type wireCall struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Function wireFunc `json:"function"`
}

type wireFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireTool struct {
	Type     string     `json:"type"`
	Function wireToolFn `json:"function"`
}

type wireToolFn struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type wireResponse struct {
	Choices []struct {
		Message struct {
			Content   *string    `json:"content"`
			ToolCalls []wireCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`
}

// Our tool names use dots, the wire format does not allow them. Validate
// guarantees that no name contains "__" or an underscore next to a dot, so
// the mapping is reversible.
func encodeName(n string) string { return strings.ReplaceAll(n, ".", "__") }
func decodeName(n string) string { return strings.ReplaceAll(n, "__", ".") }

const emptySchema = `{"type":"object","properties":{}}`

func (o *OpenAI) encode(r CompletionRequest) wireRequest {
	model := o.model
	if r.Model != "" {
		model = r.Model
	}
	w := wireRequest{Model: model, MaxTokens: r.MaxTokens, ReasoningEffort: o.effort}
	for _, m := range r.Messages {
		wm := wireMessage{Role: string(m.Role), ToolCallID: m.ToolCallID}
		if !(m.Role == RoleAssistant && len(m.ToolCalls) > 0 && m.Content == "") {
			c := m.Content
			wm.Content = &c
		}
		for _, c := range m.ToolCalls {
			args := c.Arguments
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			wm.ToolCalls = append(wm.ToolCalls, wireCall{ID: c.ID, Type: "function", Function: wireFunc{Name: encodeName(c.Name), Arguments: args}})
		}
		w.Messages = append(w.Messages, wm)
	}
	for _, t := range r.Tools {
		params := t.Schema
		if len(params) == 0 {
			params = json.RawMessage(emptySchema)
		}
		w.Tools = append(w.Tools, wireTool{Type: "function", Function: wireToolFn{Name: encodeName(t.Name), Description: t.Description, Parameters: params}})
	}
	return w
}

// UnknownTool is the name given to a model-supplied tool call whose name is not
// a valid, reversible tool name. It satisfies the name grammar but is not a real
// tool, so the approval gate denies it.
const UnknownTool = "llm.invalid_tool_name"

var callIDRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// safeName decodes a wire name, or returns UnknownTool when the name is not
// one we could have sent.
func safeName(w string) string {
	n := decodeName(w)
	if validName(n) && encodeName(n) == w {
		return n
	}
	return UnknownTool
}

func malformed(what string) error { return fmt.Errorf("%w: %s", ErrMalformed, what) }

func (o *OpenAI) decode(raw []byte) (CompletionResponse, error) {
	var w wireResponse
	if err := json.Unmarshal(raw, &w); err != nil {
		return CompletionResponse{}, malformed("not valid JSON")
	}
	if len(w.Choices) == 0 {
		return CompletionResponse{}, malformed("no choices")
	}
	ch := w.Choices[0] // only the first choice is used
	msg, stop, err := o.decodeChoice(ch.Message.Content, ch.Message.ToolCalls, ch.FinishReason)
	if err != nil {
		return CompletionResponse{}, err
	}
	return CompletionResponse{
		Message: msg,
		Stop:    stop,
		Usage:   Usage{In: clampUsage(w.Usage.PromptTokens), Out: clampUsage(w.Usage.CompletionTokens)},
	}, nil
}

// decodeChoice checks the model's message and maps the finish reason; the
// streaming and the plain path share it.
func (o *OpenAI) decodeChoice(content *string, calls []wireCall, finish string) (Message, StopReason, error) {
	if len(calls) > o.maxCalls {
		return Message{}, "", malformed("too many tool calls")
	}
	msg := Message{Role: RoleAssistant}
	if content != nil {
		msg.Content = *content
	}
	seen := make(map[string]bool, len(calls))
	for i, c := range calls {
		if c.Type != "" && c.Type != "function" {
			return Message{}, "", malformed("unsupported tool call type")
		}
		id := c.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", i) // per response; the runner derives its own ids
		} else if !callIDRE.MatchString(id) {
			return Message{}, "", malformed("bad tool call id")
		}
		if seen[id] {
			return Message{}, "", malformed("bad tool call id")
		}
		seen[id] = true
		if c.Function.Name == "" {
			return Message{}, "", malformed("tool call without a name")
		}
		args := c.Function.Arguments
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{ID: id, Name: safeName(c.Function.Name), Arguments: args})
	}

	var stop StopReason
	switch finish {
	case "length":
		stop = StopLength
	case "content_filter":
		return Message{}, "", fmt.Errorf("%w: content filtered", ErrRejected)
	case "", "stop", "tool_calls", "function_call":
		stop = StopEnd
		if len(msg.ToolCalls) > 0 {
			stop = StopToolCalls
		}
		if finish == "tool_calls" && len(msg.ToolCalls) == 0 {
			return Message{}, "", malformed("tool_calls without calls")
		}
	default:
		return Message{}, "", malformed("unknown finish reason")
	}
	return msg, stop, nil
}

const maxUsage = 100_000_000

func clampUsage(n int64) int { return int(min(max(n, 0), maxUsage)) }

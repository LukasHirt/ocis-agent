package llm

import (
	"encoding/json"
	"fmt"
	"strings"
)

type wireRequest struct {
	Model     string        `json:"model"`
	Messages  []wireMessage `json:"messages"`
	Tools     []wireTool    `json:"tools,omitempty"`
	MaxTokens int           `json:"max_tokens,omitempty"`
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
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
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
	w := wireRequest{Model: model, MaxTokens: r.MaxTokens}
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

func malformed(what string) error { return fmt.Errorf("%w: %s", ErrMalformed, what) }

func (o *OpenAI) decode(raw []byte) (CompletionResponse, error) {
	var w wireResponse
	if err := json.Unmarshal(raw, &w); err != nil {
		return CompletionResponse{}, malformed("not valid JSON")
	}
	if len(w.Choices) == 0 {
		return CompletionResponse{}, malformed("no choices")
	}
	ch := w.Choices[0]
	if len(ch.Message.ToolCalls) > o.maxCalls {
		return CompletionResponse{}, malformed("too many tool calls")
	}
	msg := Message{Role: RoleAssistant}
	if ch.Message.Content != nil {
		msg.Content = *ch.Message.Content
	}
	seen := make(map[string]bool, len(ch.Message.ToolCalls))
	for i, c := range ch.Message.ToolCalls {
		if c.Type != "" && c.Type != "function" {
			return CompletionResponse{}, malformed("unsupported tool call type")
		}
		id := c.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", i)
		}
		if len(id) > maxCallIDLen || seen[id] {
			return CompletionResponse{}, malformed("bad tool call id")
		}
		seen[id] = true
		if c.Function.Name == "" {
			return CompletionResponse{}, malformed("tool call without a name")
		}
		args := c.Function.Arguments
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{ID: id, Name: decodeName(c.Function.Name), Arguments: args})
	}

	var stop StopReason
	switch ch.FinishReason {
	case "length":
		stop = StopLength
	case "content_filter":
		return CompletionResponse{}, fmt.Errorf("%w: content filtered", ErrRejected)
	case "", "stop", "tool_calls", "function_call":
		stop = StopEnd
		if len(msg.ToolCalls) > 0 {
			stop = StopToolCalls
		}
		if ch.FinishReason == "tool_calls" && len(msg.ToolCalls) == 0 {
			return CompletionResponse{}, malformed("tool_calls without calls")
		}
	default:
		return CompletionResponse{}, malformed("unknown finish reason")
	}

	return CompletionResponse{
		Message: msg,
		Stop:    stop,
		Usage:   Usage{In: max(w.Usage.PromptTokens, 0), Out: max(w.Usage.CompletionTokens, 0)},
	}, nil
}

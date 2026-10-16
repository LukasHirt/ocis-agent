package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func okReq() CompletionRequest {
	return CompletionRequest{
		Messages: []Message{
			{Role: RoleSystem, Content: "s"},
			{Role: RoleUser, Content: "hi"},
		},
		Tools: []ToolSpec{{Name: "files.list_spaces", Description: "d", Schema: json.RawMessage(`{"type":"object"}`)}},
	}
}

func TestValidateAcceptsAWellFormedRequest(t *testing.T) {
	r := okReq()
	r.Messages = append(r.Messages,
		Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "files.list_spaces", Arguments: "{}"}}},
		Message{Role: RoleTool, ToolCallID: "c1", Content: "[]"},
	)
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidModelID(t *testing.T) {
	for _, id := range []string{"gpt-4o", "meta-llama/Llama-3.1:8b", "claude-fable-5-1", "o3", "m@v1+x_y.z", strings.Repeat("m", 128)} {
		if !ValidModelID(id) {
			t.Errorf("ValidModelID(%q) = false", id)
		}
	}
	for _, id := range []string{"", "-x", ".x", "a b", "a,b", "a=b", "a\nb", "ä", strings.Repeat("m", 129)} {
		if ValidModelID(id) {
			t.Errorf("ValidModelID(%q) = true", id)
		}
	}
	r := okReq()
	r.Model = "meta-llama/Llama-3.1:8b"
	if err := r.Validate(); err != nil {
		t.Errorf("a valid model override: %v", err)
	}
}

func TestValidateAcceptsAnEmptySchemaAndNoTools(t *testing.T) {
	r := okReq()
	r.Tools[0].Schema = nil
	if err := r.Validate(); err != nil {
		t.Fatalf("empty schema: %v", err)
	}
	r.Tools = nil
	if err := r.Validate(); err != nil {
		t.Fatalf("no tools: %v", err)
	}
}

func TestValidateRefusesMalformedRequests(t *testing.T) {
	many := func(r *CompletionRequest) {
		r.Tools = nil
		for i := 0; i < 65; i++ {
			r.Tools = append(r.Tools, ToolSpec{Name: fmt.Sprintf("t%d", i)})
		}
	}
	name := func(n string) func(*CompletionRequest) {
		return func(r *CompletionRequest) { r.Tools[0].Name = n }
	}
	cases := map[string]func(*CompletionRequest){
		"no messages":           func(r *CompletionRequest) { r.Messages = nil },
		"negative max tokens":   func(r *CompletionRequest) { r.MaxTokens = -1 },
		"model with a space":    func(r *CompletionRequest) { r.Model = "gpt x" },
		"model starting a dash": func(r *CompletionRequest) { r.Model = "-gpt" },
		"model too long":        func(r *CompletionRequest) { r.Model = strings.Repeat("m", 129) },
		"too many tools":        many,
		"upper case name":       name("Files.list"),
		"double underscore":     name("a__b"),
		"underscore before dot": name("a_.b"),
		"dot before underscore": name("a._b"),
		"leading digit":         name("1a"),
		"trailing dot":          name("a."),
		"empty name":            name(""),
		"hyphen":                name("a-b"),
		"name too long":         name(strings.Repeat("a", 63)),
		"duplicate tool": func(r *CompletionRequest) {
			r.Tools = append(r.Tools, r.Tools[0])
		},
		"schema is an array": func(r *CompletionRequest) { r.Tools[0].Schema = json.RawMessage(`[1]`) },
		"schema is null":     func(r *CompletionRequest) { r.Tools[0].Schema = json.RawMessage(`null`) },
		"schema is garbage":  func(r *CompletionRequest) { r.Tools[0].Schema = json.RawMessage(`{`) },
		"unknown role":       func(r *CompletionRequest) { r.Messages[1].Role = "function" },
		"tool id on user":    func(r *CompletionRequest) { r.Messages[1].ToolCallID = "x" },
		"calls on user": func(r *CompletionRequest) {
			r.Messages[1].ToolCalls = []ToolCall{{ID: "c", Name: "a.b"}}
		},
		"assistant call without id": func(r *CompletionRequest) {
			r.Messages = append(r.Messages, Message{Role: RoleAssistant, ToolCalls: []ToolCall{{Name: "a.b"}}})
		},
		"assistant call bad name": func(r *CompletionRequest) {
			r.Messages = append(r.Messages, Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c", Name: "A"}}})
		},
		"assistant with tool id": func(r *CompletionRequest) {
			r.Messages = append(r.Messages, Message{Role: RoleAssistant, ToolCallID: "c"})
		},
		"tool message without id": func(r *CompletionRequest) {
			r.Messages = append(r.Messages, Message{Role: RoleTool, Content: "x"})
		},
		"tool message with calls": func(r *CompletionRequest) {
			r.Messages = append(r.Messages, Message{Role: RoleTool, ToolCallID: "c", ToolCalls: []ToolCall{{ID: "c", Name: "a.b"}}})
		},
	}
	for label, mutate := range cases {
		t.Run(label, func(t *testing.T) {
			r := okReq()
			mutate(&r)
			err := r.Validate()
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Validate = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestValidateErrorsDoNotEchoContent(t *testing.T) {
	r := okReq()
	r.Messages = append(r.Messages, Message{Role: RoleAssistant, Content: "SECRET-CONTENT", ToolCalls: []ToolCall{{ID: "c", Name: "BAD-NAME-SECRET"}}})
	err := r.Validate()
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, leak := range []string{"SECRET-CONTENT", "BAD-NAME-SECRET"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error %q leaks %q", err, leak)
		}
	}
}

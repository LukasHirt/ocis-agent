package llmtest

import (
	"context"
	"testing"

	"github.com/owncloud/ocis-agent/backend/internal/llm"
)

func TestProviderRecordsDeepCopiesOfToolCallsAndTools(t *testing.T) {
	p := New(Say("x", 1, 1))
	calls := []llm.ToolCall{{ID: "c1", Name: "files.list", Arguments: "{}"}}
	tools := []llm.ToolSpec{{Name: "files.list"}}
	r := llm.CompletionRequest{
		Messages: []llm.Message{{Role: llm.RoleAssistant, ToolCalls: calls}, {Role: llm.RoleTool, ToolCallID: "c1", Content: "[]"}},
		Tools:    tools,
	}
	if _, err := p.Complete(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	calls[0].Name = "changed.name"
	tools[0].Name = "changed.tool"
	got := p.Requests()[0]
	if got.Messages[0].ToolCalls[0].Name != "files.list" || got.Tools[0].Name != "files.list" {
		t.Errorf("recorded request changed: %+v", got)
	}
}

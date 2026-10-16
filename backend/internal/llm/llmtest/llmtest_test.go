package llmtest

import (
	"context"
	"errors"
	"testing"

	"github.com/owncloud/ocis-agent/backend/internal/llm"
)

func req(text string) llm.CompletionRequest {
	return llm.CompletionRequest{Messages: []llm.Message{{Role: llm.RoleUser, Content: text}}}
}

func TestProviderPlaysItsScriptInOrderAndRecordsRequests(t *testing.T) {
	p := New(
		Say("one", 10, 2),
		Calls(5, 1, llm.ToolCall{ID: "c1", Name: "files.list", Arguments: "{}"}),
		Fail(llm.ErrRateLimited),
	)
	var _ llm.Provider = p

	r1, err := p.Complete(context.Background(), req("a"))
	if err != nil || r1.Message.Content != "one" || r1.Usage != (llm.Usage{In: 10, Out: 2}) || r1.Stop != llm.StopEnd || r1.Message.Role != llm.RoleAssistant {
		t.Fatalf("first = %+v, %v", r1, err)
	}
	r2, err := p.Complete(context.Background(), req("b"))
	if err != nil || r2.Stop != llm.StopToolCalls || len(r2.Message.ToolCalls) != 1 || r2.Message.ToolCalls[0].ID != "c1" {
		t.Fatalf("second = %+v, %v", r2, err)
	}
	if _, err := p.Complete(context.Background(), req("c")); !errors.Is(err, llm.ErrRateLimited) {
		t.Fatalf("third err = %v", err)
	}
	if p.Remaining() != 0 {
		t.Errorf("Remaining = %d", p.Remaining())
	}
	if _, err := p.Complete(context.Background(), req("d")); err == nil {
		t.Error("an exhausted script must fail")
	}
	got := p.Requests()
	if len(got) != 4 || got[0].Messages[0].Content != "a" || got[3].Messages[0].Content != "d" {
		t.Fatalf("Requests = %+v", got)
	}
}

func TestProviderRecordsACopyOfTheMessages(t *testing.T) {
	p := New(Say("x", 1, 1))
	msgs := []llm.Message{{Role: llm.RoleUser, Content: "before"}}
	if _, err := p.Complete(context.Background(), llm.CompletionRequest{Messages: msgs}); err != nil {
		t.Fatal(err)
	}
	msgs[0].Content = "after"
	if got := p.Requests()[0].Messages[0].Content; got != "before" {
		t.Errorf("recorded request changed to %q", got)
	}
}

func TestProviderEnforcesTheRequestContract(t *testing.T) {
	p := New(Say("x", 1, 1))
	_, err := p.Complete(context.Background(), llm.CompletionRequest{})
	if !errors.Is(err, llm.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if p.Remaining() != 1 {
		t.Error("an invalid request must not consume a step")
	}
}

func TestProviderHonoursACancelledContext(t *testing.T) {
	p := New(Say("x", 1, 1))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Complete(ctx, req("a")); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if p.Remaining() != 1 {
		t.Error("a cancelled call must not consume a step")
	}
}

func TestProviderFuncAdapter(t *testing.T) {
	var p llm.Provider = llm.ProviderFunc(func(context.Context, llm.CompletionRequest) (llm.CompletionResponse, error) {
		return llm.CompletionResponse{Stop: llm.StopEnd}, nil
	})
	r, err := p.Complete(context.Background(), req("a"))
	if err != nil || r.Stop != llm.StopEnd {
		t.Fatalf("got %+v, %v", r, err)
	}
}

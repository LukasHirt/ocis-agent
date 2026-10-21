package agent

import (
	"context"
	"sync"
	"testing"

	"github.com/owncloud/ocis-agent/backend/internal/llm"
	"github.com/owncloud/ocis-agent/backend/internal/llm/llmtest"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// A proactive job that runs after the user switched proactive work off is
// cancelled with a fixed reason, at the claim and before every later step;
// a chat job is not affected.
func TestAProactiveJobStopsOnceProactiveWorkIsOff(t *testing.T) {
	for _, kind := range store.ProactiveTriggers {
		t.Run(kind+" at the claim", func(t *testing.T) {
			f := newFixture(t)
			p := llmtest.New(llmtest.Say("done", 1, 1))
			f.provider = p
			goal, _ := store.ProactiveGoal(kind)
			f.enqueueKind(kind, goal)
			f.runOnce()
			if j := f.job(); j.State != store.JobCancelled || j.Error != ReasonProactiveOff {
				t.Fatalf("job = %s %q", j.State, j.Error)
			}
			if len(p.Requests()) != 0 {
				t.Error("a model call for a job of switched-off proactive work")
			}
		})
	}
	t.Run("in a run", func(t *testing.T) {
		f := newFixture(t)
		f.setPolicy(func(p *store.Policy) { p.ProactiveEnabled = true })
		p := llmtest.New(llmtest.Calls(1, 1, list("l", "/")), llmtest.Say("done", 1, 1))
		f.provider = p
		goal, _ := store.ProactiveGoal(store.TriggerSchedule)
		f.enqueueKind(store.TriggerSchedule, goal)
		// switched off while the first model call runs
		f.provider = providerThen(p, func() { f.setPolicy(func(p *store.Policy) { p.ProactiveEnabled = false }) })
		f.runOnce()
		if j := f.job(); j.State != store.JobCancelled || j.Error != ReasonProactiveOff {
			t.Fatalf("job = %s %q", j.State, j.Error)
		}
		if n := f.calls.get("execute", "files.list"); n != 0 {
			t.Errorf("%d tool calls after proactive work was switched off", n)
		}
	})
	t.Run("chat", func(t *testing.T) {
		f := newFixture(t)
		f.provider = llmtest.New(llmtest.Say("done", 1, 1))
		f.enqueue("hello")
		f.runOnce()
		if j := f.job(); j.State != store.JobSucceeded {
			t.Fatalf("chat job = %s %q", j.State, j.Error)
		}
	})
}

// providerThen answers like p and runs fn after the first answer.
func providerThen(p llm.Provider, fn func()) llm.Provider {
	var once sync.Once
	return llm.ProviderFunc(func(ctx context.Context, req llm.CompletionRequest) (llm.CompletionResponse, error) {
		resp, err := p.Complete(ctx, req)
		once.Do(fn)
		return resp, err
	})
}

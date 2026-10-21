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

// A proactive job gets exactly chat's gate: every write, move, copy and
// delete it proposes waits for the user's approval and nothing changes.
func TestProactiveJobsOnlyProposeWrites(t *testing.T) {
	for _, kind := range store.ProactiveTriggers {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			f.fake.PutFile(drive, "/old.txt", []byte("old"))
			f.setPolicy(func(p *store.Policy) { p.ProactiveEnabled = true })
			f.provider = llmtest.New(llmtest.Calls(1, 1,
				writeNew("w", "/new.txt", "x"),
				tc("m", "files.create_folder", `{"path":"/Folder"}`),
				tc("o", "files.overwrite", `{"path":"/old.txt","content":"y"}`),
				tc("v", "files.move", `{"from":"/old.txt","to":"/moved.txt"}`),
				tc("c", "files.copy", `{"from":"/old.txt","to":"/copy.txt"}`),
				tc("d", "files.delete", `{"path":"/old.txt"}`),
			))
			goal, _ := store.ProactiveGoal(kind)
			f.enqueueKind(kind, goal)
			f.runOnce()
			if j := f.job(); j.State != store.JobWaitingApproval {
				t.Fatalf("job = %s %q, want waiting for approval", j.State, j.Error)
			}
			as := f.approvals()
			if len(as) != 6 {
				t.Fatalf("%d approvals, want 6", len(as))
			}
			for _, a := range as {
				if a.State != store.ApprovalPending {
					t.Errorf("%s: %s", a.Tool, a.State)
				}
			}
			for _, p := range []string{"/new.txt", "/Folder", "/old.txt", "/moved.txt", "/copy.txt"} {
				if n := f.modifying(p); n != 0 {
					t.Errorf("%s modified %d times", p, n)
				}
			}
		})
	}
}

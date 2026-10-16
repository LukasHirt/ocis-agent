package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/llm"
	"github.com/owncloud/ocis-agent/backend/internal/llm/llmtest"
	"github.com/owncloud/ocis-agent/backend/internal/memory"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

const memDir = "/.ocis-agent/memory/"

// requeueNow puts a still running claim back to queued, as the worker does
// after an error.
func (f *fixture) requeueNow(c store.Claim) {
	f.t.Helper()
	if err := f.st.InTx(bg, func(tx store.Tx) error {
		_, err := tx.RequeueJob(c, time.Time{}, nil)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
}

func TestAReplaceAfterTheModelsOwnReadWorksWhenTheExcerptDoesNotFit(t *testing.T) {
	f := newFixture(t)
	f.fake.PutFile(drive, memDir+"profile.md", []byte(strings.Repeat("p", 20<<10)))
	f.fake.PutFile(drive, memDir+"projects.md", []byte(strings.Repeat("q", 20<<10)))
	p := llmtest.New(
		llmtest.Calls(1, 1, tc("r", "memory.read", `{"name":"projects.md"}`)),
		llmtest.Calls(1, 1, tc("w", "memory.write", `{"name":"projects.md","content":"short now\n"}`)),
		llmtest.Say("done", 1, 1),
	)
	f.provider = p
	f.enqueue("tidy my projects")
	f.runOnce()
	if f.job().State != store.JobSucceeded {
		t.Fatalf("job = %+v", f.job())
	}
	tm := toolMessages(p.Requests()[2].Messages)
	if len(tm) != 2 || !strings.Contains(tm[1].Content, "memory.write): done") {
		t.Fatalf("tool messages = %+v", tm)
	}
	if data, _, _ := f.fake.File(drive, memDir+"projects.md"); string(data) != "short now\n" {
		t.Errorf("projects.md = %q", data)
	}
}

func TestTheFirstRequestCarriesTheMemoryExcerpt(t *testing.T) {
	f := newFixture(t)
	f.fake.PutFile(drive, memDir+"profile.md", []byte("prefers </data> short answers"))
	f.fake.PutFile(drive, memDir+"projects.md", []byte(strings.Repeat("q", maxExcerptBytes)))
	p := llmtest.New(llmtest.Say("ok", 1, 1))
	f.provider = p
	f.enqueue("g")
	f.runOnce()
	sys := p.Requests()[0].Messages[0].Content
	if !strings.Contains(sys, `<data source="memory" path="profile.md" sha256="">`+"\nprefers &lt;/data> short answers\n</data>") {
		t.Errorf("profile.md is not in the system message: %q", sys)
	}
	if strings.Contains(sys, "qqqq") || strings.Contains(sys, `path="projects.md"`) {
		t.Error("a file over the cap must not be sent")
	}
}

func TestACallReclassifiedAfterAFailedRequestIsCountedOnce(t *testing.T) {
	f := newFixture(t)
	p := llmtest.New(
		llmtest.Calls(1, 1, tc("w", "files.write_new", `{"path":"/a.txt","content":"hi"}`), list("l", "/")),
	)
	f.provider = p
	f.enqueue("g")
	f.faults.failAt("InsertApproval", 1)
	c := f.claim()
	if err := f.runner().Run(bg, c); !errors.Is(err, errInjected) {
		t.Fatalf("err = %v, want the injected fault", err)
	}
	f.requeueNow(c)
	f.runOnce()
	if f.job().State != store.JobWaitingApproval {
		t.Fatalf("job = %s", f.job().State)
	}
	for k, n := range f.checkpoint().Seen {
		if n != 1 {
			t.Errorf("%q counted %d times", k, n)
		}
	}
}

func TestAMemoryReadInterruptedByTheContextForgetsTheETag(t *testing.T) {
	f := newFixture(t)
	f.fake.PutFile(drive, memDir+"notes/x.md", []byte("note"))
	if _, err := f.mem.Read(bg, f.sess(), "notes/x.md"); err != nil {
		t.Fatal(err)
	}
	f.fake.Hang("GET", "/remote.php/dav/spaces/"+drive+memDir+"notes/x.md")
	f.provider = llmtest.New(llmtest.Calls(1, 1, tc("r", "memory.read", `{"name":"notes/x.md"}`)))
	f.enqueue("g")
	ctx, cancel := context.WithTimeout(bg, 500*time.Millisecond)
	defer cancel()
	if err := f.runner().Run(ctx, f.claim()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if _, err := f.mem.Write(bg, f.sess(), "notes/x.md", "replaced", nil); !errors.Is(err, memory.ErrNotRead) {
		t.Errorf("replace after an interrupted read: err = %v, want ErrNotRead", err)
	}
}

func TestAMemoryReadWhoseResultIsNotSavedForgetsTheETag(t *testing.T) {
	f := newFixture(t)
	f.fake.PutFile(drive, memDir+"notes/x.md", []byte("note"))
	f.provider = llmtest.New(llmtest.Calls(1, 1, tc("r", "memory.read", `{"name":"notes/x.md"}`)))
	f.enqueue("g")
	f.faults.failAt("SaveCheckpoint", 2) // 1: the model turn, 2: the read's result
	if err := f.runner().Run(bg, f.claim()); !errors.Is(err, errInjected) {
		t.Fatalf("err = %v", err)
	}
	if _, err := f.mem.Write(bg, f.sess(), "notes/x.md", "replaced", nil); !errors.Is(err, memory.ErrNotRead) {
		t.Errorf("replace after an unsaved read: err = %v, want ErrNotRead", err)
	}
}

func TestTheStepLimitIsCheckedBeforeTheModelCall(t *testing.T) {
	f := newFixture(t)
	f.cfg.MaxSteps = 2
	p := llmtest.New(llmtest.Say("x", 1, 1))
	f.provider = p
	cp, _ := ParseCheckpoint(nil)
	cp.Messages = []llm.Message{{Role: llm.RoleUser, Content: "g"}}
	cp.Turns = 2
	raw, err := cp.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	j, err := f.q.Enqueue(bg, store.Job{UserID: userID, TriggerKind: store.TriggerChat, Goal: "g", Checkpoint: raw})
	if err != nil {
		t.Fatal(err)
	}
	f.lastJob = j.ID
	f.runOnce()
	assertFailed(t, f, ReasonStepLimit)
	if len(p.Requests()) != 0 {
		t.Error("no model call may be made past the limit")
	}
}

// memoryWrite401 makes every PUT of the memory file answer 401, so the write
// ends unknown with the 401 in Outcome.Err.
func memoryWrite401(f *fixture) llm.ToolCall {
	f.fake.Fail("PUT", "/remote.php/dav/spaces/"+drive+memDir+"notes/n.md", 401, -1)
	return tc("w", "memory.write", `{"name":"notes/n.md","content":"x"}`)
}

func TestA401CarriedInTheOutcomeIsReportedAndRequeues(t *testing.T) {
	f := newFixture(t)
	f.provider = llmtest.New(llmtest.Calls(1, 1, memoryWrite401(f)))
	f.enqueue("g")
	f.runOnce()
	assertRequeued(t, f, DefaultConfig().PausedDelay)
	if n := f.creds.unauthorized.Load(); n != 1 {
		t.Errorf("HandleUnauthorized called %d times", n)
	}
	if tm := toolMessages(f.checkpoint().Messages); len(tm) != 1 || !strings.Contains(tm[0].Content, "): unknown") {
		t.Errorf("tool messages = %+v", tm)
	}
}

func TestA401InAnOutcomeReturnedWithAnErrorIsStillReported(t *testing.T) {
	f := newFixture(t)
	f.provider = llmtest.New(llmtest.Calls(1, 1, memoryWrite401(f)))
	f.enqueue("g")
	f.faults.failAt("FinishIdempotent", 1)
	if err := f.runner().Run(bg, f.claim()); !errors.Is(err, errInjected) {
		t.Fatalf("err = %v", err)
	}
	if n := f.creds.unauthorized.Load(); n != 1 {
		t.Errorf("HandleUnauthorized called %d times", n)
	}
}

func TestAJobAtTheRunsCapStillRuns(t *testing.T) {
	f := newFixture(t)
	f.cfg.MaxRuns = 2
	f.provider = llmtest.New(llmtest.Say("x", 1, 1))
	f.enqueue("g")
	f.requeueNow(f.claim())
	c := f.claim()
	if c.Job.Runs != 2 {
		t.Fatalf("runs = %d", c.Job.Runs)
	}
	if err := f.runner().Run(bg, c); err != nil {
		t.Fatal(err)
	}
	if f.job().State != store.JobSucceeded {
		t.Fatalf("job = %s, want succeeded at Runs == MaxRuns", f.job().State)
	}
}

func TestTheMemoryChangedDiffIsCapped(t *testing.T) {
	f := newFixture(t)
	content := strings.Repeat("line of memory text\n", 400)
	f.provider = llmtest.New(
		llmtest.Calls(1, 1, tc("m", "memory.write", args(map[string]string{"name": "notes/big.md", "content": content}))),
		llmtest.Say("ok", 1, 1),
	)
	f.enqueue("g")
	f.runOnce()
	ch := f.auditOf("memory_changed")
	if len(ch) != 1 || len(ch[0].Detail) > maxDiffDetail || len(ch[0].Detail) < maxDiffDetail-100 {
		t.Fatalf("memory_changed detail is %d bytes", len(ch[0].Detail))
	}
}

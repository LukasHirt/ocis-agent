package app

import (
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/approvals"
	"github.com/owncloud/ocis-agent/backend/internal/llm"
	"github.com/owncloud/ocis-agent/backend/internal/llm/llmtest"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// watchDocuments turns proactive work on for the user with /Documents as
// the watch folder and a scheduled sweep far away.
func (f *fixture) watchDocuments() {
	f.t.Helper()
	f.fake.PutFile(drive, "/Documents/seed.txt", []byte("seed"))
	if err := f.st.InTx(bg, func(tx store.Tx) error {
		p, err := tx.GetPolicy(userID)
		if err != nil {
			return err
		}
		p.ProactiveEnabled, p.SweepInterval, p.Watch = true, 24*time.Hour, []string{"/Documents"}
		if err := tx.PutPolicy(p); err != nil {
			return err
		}
		c, err := tx.GetCursor(userID)
		if err != nil {
			return err
		}
		now := time.Now()
		c.LastSweepAt = &now
		return tx.PutCursor(c)
	}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) cursor() store.Cursor {
	f.t.Helper()
	var c store.Cursor
	if err := f.st.View(bg, func(tx store.Tx) error {
		var err error
		c, err = tx.GetCursor(userID)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return c
}

func (f *fixture) folderETag(p string) string {
	f.t.Helper()
	c, err := ocisclient.New(f.fake.URL)
	if err != nil {
		f.t.Fatal(err)
	}
	e, err := c.Stat(bg, ocisclient.AppToken(login, pass), drive, p)
	if err != nil {
		f.t.Fatal(err)
	}
	return e.ETag
}

// An approved write into a watch folder moves the stored baseline with it,
// so the watch poll does not take the agent's own work for a user change.
func TestAnApprovedWriteIntoAWatchFolderIsNotAFileChange(t *testing.T) {
	f := newFixture(t, nil)
	p := llmtest.New(
		llmtest.Calls(1, 1, llm.ToolCall{ID: "w", Name: "files.write_new", Arguments: `{"path":"/Documents/a.txt","content":"hi"}`}),
		llmtest.Say("written", 1, 1),
	)
	a := f.build(p)
	f.enroll(a)
	f.watchDocuments()
	j := f.enqueue(a, "write a file")
	r := start(t, a)

	f.waitState(j, store.JobWaitingApproval, 10*time.Second)
	// the scheduler's first watch poll records the baseline
	deadline := time.Now().Add(10 * time.Second)
	for f.cursor().WatchETags["/Documents"] == "" {
		if time.Now().After(deadline) {
			t.Fatal("no baseline")
		}
		time.Sleep(10 * time.Millisecond)
	}
	before := f.cursor().WatchETags["/Documents"]
	as := f.approvals(j)
	if _, err := a.gate.Decide(bg, userID, "oidc-sub", []approvals.DecisionItem{
		{ApprovalID: as[0].ID, Decision: store.ApprovalApproved, ArgsHash: as[0].ArgsHash},
	}); err != nil {
		t.Fatal(err)
	}
	a.queue.Wake()
	f.waitState(j, store.JobSucceeded, 10*time.Second)
	if err := r.stop(t, 15*time.Second); err != nil {
		t.Errorf("Run = %v", err)
	}
	now := f.folderETag("/Documents")
	c := f.cursor()
	if now == before || c.WatchETags["/Documents"] != now || c.WatchDirtySince != nil {
		t.Fatalf("baseline %q before, %q after, folder now %q, dirty %v", before, c.WatchETags["/Documents"], now, c.WatchDirtySince)
	}
}

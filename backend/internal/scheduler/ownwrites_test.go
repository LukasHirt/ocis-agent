package scheduler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/tools"
)

// ownWrites returns the fixture's one OwnWrites (built on first use with
// ownCfg), which its schedulers share like the app's.
func (w *watchFixture) ownWrites() *OwnWrites {
	w.t.Helper()
	if w.own == nil {
		o, err := NewOwnWrites(w.st, w.f.HandleUnauthorized, w.ownCfg)
		if err != nil {
			w.t.Fatal(err)
		}
		w.own = o
	}
	return w.own
}

// scheduler builds a scheduler that knows the fixture's OwnWrites.
func (w *watchFixture) scheduler() *Scheduler {
	w.t.Helper()
	w.deps.OwnWrites = w.ownWrites()
	return w.fixture.scheduler()
}

func (w *watchFixture) userEnv(u string) tools.Env {
	w.t.Helper()
	env, err := w.env(bg, u)
	if err != nil {
		w.t.Fatal(err)
	}
	return env
}

// agentWrite runs write between BeforeWrite and the function it returns,
// like the runner around an approved call that is done.
func (w *watchFixture) agentWrite(u string, paths []string, write func()) {
	w.t.Helper()
	after := w.ownWrites().BeforeWrite(bg, w.userEnv(u), paths)
	write()
	if after != nil {
		after(bg, true)
	}
}

// settle polls past the debounce twice so a pending change starts its job.
func (w *watchFixture) settle(s *Scheduler) {
	for range 3 {
		w.clk.Advance(5 * time.Minute)
		w.poll(s)
	}
}

func TestOwnWriteIsNotAFileChange(t *testing.T) {
	w := newWatchFixture(t)
	u := w.watchUsers(1)[0]
	s := w.scheduler()
	w.poll(s)

	w.agentWrite(u, []string{"/Documents/report.md"}, func() { w.put(u, "/Documents/report.md") })
	if c := w.cursor(u); c.WatchETags["/Documents"] == "" || c.WatchDirtySince != nil {
		t.Fatalf("cursor after the write = %+v", c)
	}
	w.settle(s)
	if jobs := w.fileJobs(u); len(jobs) != 0 {
		t.Fatalf("the agent's own write started a review: %+v", jobs)
	}
	// a user change afterwards still fires
	w.put(u, "/Documents/user.md")
	w.settle(s)
	if n := len(w.fileJobs(u)); n != 1 {
		t.Fatalf("%d file jobs after a user change, want 1", n)
	}
}

func TestOwnWriteKeepsAUserChangeMadeBeforeIt(t *testing.T) {
	w := newWatchFixture(t)
	u := w.watchUsers(1)[0]
	s := w.scheduler()
	w.poll(s)
	base := w.cursor(u).WatchETags["/Documents"]

	w.put(u, "/Documents/user.md") // not polled yet
	w.agentWrite(u, []string{"/Documents/report.md"}, func() { w.put(u, "/Documents/report.md") })
	if got := w.cursor(u).WatchETags["/Documents"]; got != base {
		t.Fatalf("the baseline moved from %q to %q: the user's change would be lost", base, got)
	}
	w.settle(s)
	if n := len(w.fileJobs(u)); n != 1 {
		t.Fatalf("%d file jobs, want 1 for the user's change", n)
	}
}

func TestOwnWriteMoveRebaselinesSourceAndDestination(t *testing.T) {
	w := newWatchFixture(t)
	u := w.watchUsers(1, "/Documents", "/Archive")[0]
	s := w.scheduler()
	w.poll(s)
	w.agentWrite(u, []string{"/Documents/seed.txt", "/Archive/moved.txt"}, func() {
		if err := w.client.Move(bg, w.userEnv(u).Creds, "drive-"+u, "/Documents/seed.txt", "/Archive/moved.txt"); err != nil {
			t.Fatal(err)
		}
	})
	w.settle(s)
	if jobs := w.fileJobs(u); len(jobs) != 0 {
		t.Fatalf("the agent's own move started a review: %+v", jobs)
	}
}

func TestOwnWriteOutsideTheWatchFoldersDoesNothing(t *testing.T) {
	w := newWatchFixture(t)
	u := w.watchUsers(1)[0]
	w.poll(w.scheduler())
	w.fake.ResetRequests()
	for _, p := range [][]string{{"/Other/a.md"}, {"/Documents2/a.md"}, {"/"}, nil} {
		if after := w.ownWrites().BeforeWrite(bg, w.userEnv(u), p); after != nil {
			t.Errorf("BeforeWrite(%q) returned a function", p)
		}
	}
	if n := w.propfinds(); n != 0 {
		t.Errorf("%d PROPFINDs for writes outside the watch folders", n)
	}
}

func TestOwnWriteNeedsAStoredBaselineAndProactiveWork(t *testing.T) {
	w := newWatchFixture(t)
	u := w.watchUsers(1)[0]
	// no baseline yet
	if after := w.ownWrites().BeforeWrite(bg, w.userEnv(u), []string{"/Documents/a.md"}); after != nil {
		t.Error("a function without a baseline")
	}
	w.poll(w.scheduler())
	w.enableProactive(u, 0, func(p *store.Policy) { p.ProactiveEnabled = false })
	w.fake.ResetRequests()
	if after := w.ownWrites().BeforeWrite(bg, w.userEnv(u), []string{"/Documents/a.md"}); after != nil {
		t.Error("a function while proactive work is off")
	}
	if n := w.propfinds(); n != 0 {
		t.Errorf("%d PROPFINDs", n)
	}
}

func TestOwnWriteIntoTheWatchedRootUsesTheListingDigest(t *testing.T) {
	w := newWatchFixture(t)
	u := w.watchUsers(1, "/")[0]
	w.put(u, "/top.txt")
	s := w.scheduler()
	w.poll(s)
	base := w.cursor(u).WatchETags["/"]
	w.agentWrite(u, []string{"/Documents/deep/new.md"}, func() { w.put(u, "/Documents/deep/new.md") })
	got := w.cursor(u).WatchETags["/"]
	if got == base || got[:len(listDigestPrefix)] != listDigestPrefix {
		t.Fatalf("root baseline %q -> %q, want a new listing digest", base, got)
	}
	// the agent's memory write meanwhile changes nothing either
	w.put(u, "/.ocis-agent/memory/profile.md")
	w.settle(s)
	if jobs := w.fileJobs(u); len(jobs) != 0 {
		t.Fatalf("the agent's own write into the root started a review: %+v", jobs)
	}
}

func TestOwnWriteWithAPollInsideTheWriteWindow(t *testing.T) {
	t.Run("before the write", func(t *testing.T) {
		w := newWatchFixture(t)
		u := w.watchUsers(1)[0]
		s := w.scheduler()
		w.poll(s)
		w.agentWrite(u, []string{"/Documents/a.md"}, func() {
			w.clk.Advance(5 * time.Minute)
			w.poll(s) // sees the folder as it was
			w.put(u, "/Documents/a.md")
		})
		w.settle(s)
		if jobs := w.fileJobs(u); len(jobs) != 0 {
			t.Fatalf("jobs = %+v", jobs)
		}
	})
	t.Run("after the write", func(t *testing.T) {
		// the poll sees the write before the rebaseline: the folder is
		// being written by the agent, so the poll leaves it alone
		w := newWatchFixture(t)
		u := w.watchUsers(1)[0]
		s := w.scheduler()
		w.poll(s)
		w.agentWrite(u, []string{"/Documents/a.md"}, func() {
			w.put(u, "/Documents/a.md")
			w.clk.Advance(5 * time.Minute)
			w.poll(s)
		})
		c := w.cursor(u)
		if got, want := c.WatchETags["/Documents"], w.folderETag(u, "/Documents"); c.WatchDirtySince != nil || got != want {
			t.Fatalf("cursor = %+v, want the rebaselined %q and no change", c, want)
		}
		w.settle(s)
		if jobs := w.fileJobs(u); len(jobs) != 0 {
			t.Fatalf("jobs = %+v", jobs)
		}
	})
	t.Run("a user change before the write", func(t *testing.T) {
		w := newWatchFixture(t)
		u := w.watchUsers(1)[0]
		s := w.scheduler()
		w.poll(s)
		w.put(u, "/Documents/user.md")
		w.agentWrite(u, []string{"/Documents/a.md"}, func() {
			w.put(u, "/Documents/a.md")
			w.clk.Advance(5 * time.Minute)
			w.poll(s)
		})
		w.settle(s)
		if n := len(w.fileJobs(u)); n != 1 {
			t.Fatalf("%d file jobs, want 1 for the user's change", n)
		}
	})
}

func TestOwnWriteLeavesAMovedBaselineAlone(t *testing.T) {
	w := newWatchFixture(t)
	u := w.watchUsers(1)[0]
	w.poll(w.scheduler())
	after := w.ownWrites().BeforeWrite(bg, w.userEnv(u), []string{"/Documents/a.md"})
	if after == nil {
		t.Fatal("no function")
	}
	w.put(u, "/Documents/a.md")
	// another writer moved the stored baseline meanwhile
	if err := w.st.InTx(bg, func(tx store.Tx) error {
		c, err := tx.GetCursor(u)
		if err != nil {
			return err
		}
		c.WatchETags["/Documents"] = `"other"`
		return tx.PutCursor(c)
	}); err != nil {
		t.Fatal(err)
	}
	after(bg, true)
	if got := w.cursor(u).WatchETags["/Documents"]; got != `"other"` {
		t.Fatalf("baseline = %q, want it untouched", got)
	}
}

func TestOwnWriteSkipsOnAStatError(t *testing.T) {
	for _, when := range []string{"before", "after"} {
		t.Run(when, func(t *testing.T) {
			w := newWatchFixture(t)
			u := w.watchUsers(1)[0]
			s := w.scheduler()
			w.poll(s)
			base := w.cursor(u).WatchETags["/Documents"]
			fail := func() {
				w.fake.Fail("PROPFIND", "/remote.php/dav/spaces/drive-"+u+"/Documents", http.StatusBadGateway, 1)
			}
			if when == "before" {
				fail()
			}
			after := w.ownWrites().BeforeWrite(bg, w.userEnv(u), []string{"/Documents/a.md"})
			w.put(u, "/Documents/a.md")
			if when == "after" {
				fail()
			}
			if after != nil {
				after(bg, true)
			}
			if got := w.cursor(u).WatchETags["/Documents"]; got != base {
				t.Fatalf("baseline moved to %q despite the failed stat", got)
			}
			// today's behaviour: the poll sees the write as a change
			w.settle(s)
			if n := len(w.fileJobs(u)); n != 1 {
				t.Fatalf("%d file jobs, want 1", n)
			}
		})
	}
}

func TestOwnWriteFinishesUnderACancelledContext(t *testing.T) {
	w := newWatchFixture(t)
	w.ownCfg.Settle = time.Hour // a cancelled job must not wait for it
	u := w.watchUsers(1)[0]
	w.poll(w.scheduler())
	after := w.ownWrites().BeforeWrite(bg, w.userEnv(u), []string{"/Documents/a.md"})
	w.put(u, "/Documents/a.md")
	ctx, cancel := context.WithCancel(bg)
	cancel()
	start := time.Now()
	after(ctx, true)
	if time.Since(start) > 5*time.Second {
		t.Fatal("the rebaseline waited under a cancelled context")
	}
	if got, want := w.cursor(u).WatchETags["/Documents"], w.folderETag(u, "/Documents"); got != want {
		t.Fatalf("baseline %q, want the folder's new ETag %q", got, want)
	}
}

func (w *watchFixture) folderETag(u, p string) string {
	w.t.Helper()
	e, err := w.client.Stat(bg, w.userEnv(u).Creds, "drive-"+u, p)
	if err != nil {
		w.t.Fatal(err)
	}
	return e.ETag
}

// upload writes p with a real PUT, which oCIS postprocesses.
func (w *watchFixture) upload(u, p string) {
	w.t.Helper()
	if _, err := w.client.Put(bg, w.userEnv(u).Creds, "drive-"+u, p, []byte("upload"), ""); err != nil {
		w.t.Fatal(err)
	}
}

// oCIS moves the folder's ETag a second time when the upload's
// postprocessing finishes: the rebaseline waits for that.
func TestOwnWriteWaitsForPostprocessing(t *testing.T) {
	for name, set := range map[string]func(w *watchFixture){
		"425 window": func(w *watchFixture) {
			w.fake.SetPostprocessing(3)
			w.fake.SetPostprocessingPropagation(true, 20*time.Millisecond)
		},
		"late propagation": func(w *watchFixture) {
			w.fake.SetPostprocessingPropagation(true, 30*time.Millisecond)
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWatchFixture(t)
			w.ownCfg = OwnWritesConfig{Settle: 100 * time.Millisecond, MaxWait: 5 * time.Second, Poll: 5 * time.Millisecond}
			u := w.watchUsers(1)[0]
			s := w.scheduler()
			w.poll(s)
			set(w)
			w.agentWrite(u, []string{"/Documents/up.md"}, func() { w.upload(u, "/Documents/up.md") })
			// postprocessing ends at the latest now (the fake ends its 425
			// window by GETs)
			for range 5 {
				_, _, _ = w.client.Get(bg, w.userEnv(u).Creds, "drive-"+u, "/Documents/up.md", 1<<10)
			}
			time.Sleep(100 * time.Millisecond)
			if got, want := w.cursor(u).WatchETags["/Documents"], w.folderETag(u, "/Documents"); got != want {
				t.Fatalf("baseline %q, the folder is now %q", got, want)
			}
			w.settle(s)
			if jobs := w.fileJobs(u); len(jobs) != 0 {
				t.Fatalf("the agent's own upload started a review: %+v", jobs)
			}
		})
	}
}

func TestOwnWriteGivesUpOnEndlessPostprocessing(t *testing.T) {
	w := newWatchFixture(t)
	w.ownCfg = OwnWritesConfig{Settle: 5 * time.Millisecond, MaxWait: 100 * time.Millisecond, Poll: 5 * time.Millisecond}
	u := w.watchUsers(1)[0]
	s := w.scheduler()
	w.poll(s)
	base := w.cursor(u).WatchETags["/Documents"]
	w.fake.SetPostprocessing(1 << 20)
	start := time.Now()
	w.agentWrite(u, []string{"/Documents/up.md"}, func() { w.upload(u, "/Documents/up.md") })
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("waited %v", d)
	}
	if got := w.cursor(u).WatchETags["/Documents"]; got != base {
		t.Fatalf("baseline moved to %q while the upload was still processed", got)
	}
	w.settle(s)
	if n := len(w.fileJobs(u)); n != 1 {
		t.Fatalf("%d file jobs, want today's follow-up", n)
	}
}

func TestOwnWriteWaitsForAFolderToSettle(t *testing.T) {
	w := newWatchFixture(t)
	w.ownCfg = OwnWritesConfig{Settle: 50 * time.Millisecond, MaxWait: 5 * time.Second, Poll: 5 * time.Millisecond}
	u := w.watchUsers(1)[0]
	s := w.scheduler()
	w.poll(s)
	// something keeps moving the folder for a while after the write
	done := make(chan struct{})
	w.agentWrite(u, []string{"/Documents/a.md"}, func() {
		w.put(u, "/Documents/a.md")
		go func() {
			defer close(done)
			for i := range 3 {
				time.Sleep(10 * time.Millisecond)
				w.put(u, "/Documents/a.md"+string(rune('0'+i)))
			}
		}()
	})
	<-done
	if got, want := w.cursor(u).WatchETags["/Documents"], w.folderETag(u, "/Documents"); got != want {
		t.Fatalf("baseline %q, the folder is now %q", got, want)
	}
}

// A poll that checked the folder before the agent's write but is recorded
// after the rebaseline must not undo it.
func TestOwnWriteSurvivesAPollRecordedAfterIt(t *testing.T) {
	w := newWatchFixture(t)
	u := w.watchUsers(1)[0]
	s := w.scheduler()
	w.poll(s)
	w.clk.Advance(5 * time.Minute)
	once := false
	s.beforeApply = func(string) {
		if once {
			return
		}
		once = true
		// the poll has checked the folder; now the agent writes
		w.agentWrite(u, []string{"/Documents/a.md"}, func() { w.put(u, "/Documents/a.md") })
	}
	w.poll(s)
	c := w.cursor(u)
	if got, want := c.WatchETags["/Documents"], w.folderETag(u, "/Documents"); got != want || c.WatchDirtySince != nil {
		t.Fatalf("baseline %q (dirty %v), want the rebaselined %q", got, c.WatchDirtySince, want)
	}
	w.settle(s)
	if jobs := w.fileJobs(u); len(jobs) != 0 {
		t.Fatalf("jobs = %+v", jobs)
	}
	// a user change is still seen by the next poll
	w.put(u, "/Documents/user.md")
	w.settle(s)
	if n := len(w.fileJobs(u)); n != 1 {
		t.Fatalf("%d file jobs after a user change, want 1", n)
	}
}

// A write that did not happen clears the mark at once: the poll sees the
// folder again (here a user change).
func TestOwnWriteThatFailedClearsTheMark(t *testing.T) {
	w := newWatchFixture(t)
	u := w.watchUsers(1)[0]
	s := w.scheduler()
	w.poll(s)
	after := w.ownWrites().BeforeWrite(bg, w.userEnv(u), []string{"/Documents/a.md"})
	w.put(u, "/Documents/user.md")
	w.clk.Advance(5 * time.Minute)
	w.poll(s)
	if c := w.cursor(u); c.WatchDirtySince != nil {
		t.Fatal("a folder being written was recorded")
	}
	after(bg, false)
	w.settle(s)
	if n := len(w.fileJobs(u)); n != 1 {
		t.Fatalf("%d file jobs, want 1", n)
	}
}

// A mark that is never cleared (a lost hook) expires.
func TestOwnWriteMarksExpire(t *testing.T) {
	w := newWatchFixture(t)
	u := w.watchUsers(1)[0]
	s := w.scheduler()
	w.poll(s)
	w.ownWrites().markTTL = 50 * time.Millisecond
	if after := w.ownWrites().BeforeWrite(bg, w.userEnv(u), []string{"/Documents/a.md"}); after == nil {
		t.Fatal("no function")
	}
	w.put(u, "/Documents/user.md")
	time.Sleep(100 * time.Millisecond)
	w.settle(s)
	if n := len(w.fileJobs(u)); n != 1 {
		t.Fatalf("%d file jobs, want 1 once the mark expired", n)
	}
}

// Once the job's context has ended, the hook takes at most a few seconds,
// even against an oCIS that does not answer.
func TestOwnWriteIsShortAfterTheJobEnded(t *testing.T) {
	w := newWatchFixture(t)
	u := w.watchUsers(1)[0]
	w.poll(w.scheduler())
	after := w.ownWrites().BeforeWrite(bg, w.userEnv(u), []string{"/Documents/a.md"})
	w.put(u, "/Documents/a.md")
	w.fake.Hang("PROPFIND", "/remote.php/dav/spaces/drive-"+u)
	ctx, cancel := context.WithCancel(bg)
	cancel()
	start := time.Now()
	after(ctx, true)
	if d := time.Since(start); d > 4*time.Second {
		t.Fatalf("the hook took %v after the job ended", d)
	}
}

func TestOwnWriteReportsA401(t *testing.T) {
	w := newWatchFixture(t)
	u := w.watchUsers(1)[0]
	w.poll(w.scheduler())
	after := w.ownWrites().BeforeWrite(bg, w.userEnv(u), []string{"/Documents/a.md"})
	w.upload(u, "/Documents/a.md")
	w.fake.Fail("GET", "/remote.php/dav/spaces/drive-"+u+"/Documents/a.md", http.StatusUnauthorized, -1)
	after(bg, true)
	if got := w.f.unauthorizedUsers(); len(got) != 1 || got[0] != u {
		t.Fatalf("HandleUnauthorized calls = %v, want one for %s", got, u)
	}
}

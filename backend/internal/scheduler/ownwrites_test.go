package scheduler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/tools"
)

func (w *watchFixture) ownWrites() *OwnWrites {
	w.t.Helper()
	o, err := NewOwnWrites(w.st)
	if err != nil {
		w.t.Fatal(err)
	}
	return o
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
		after(bg)
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
		// the poll saw the write first: the change is recorded and the
		// rebaseline leaves it (an accepted follow-up review)
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
		if c.WatchDirtySince == nil {
			t.Fatalf("the rebaseline cleared a recorded change: %+v", c)
		}
		w.settle(s)
		if n := len(w.fileJobs(u)); n != 1 {
			t.Fatalf("%d file jobs, want 1", n)
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
	after(bg)
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
				after(bg)
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

func TestOwnWriteStopsWithTheContext(t *testing.T) {
	w := newWatchFixture(t)
	u := w.watchUsers(1)[0]
	w.poll(w.scheduler())
	base := w.cursor(u).WatchETags["/Documents"]
	after := w.ownWrites().BeforeWrite(bg, w.userEnv(u), []string{"/Documents/a.md"})
	w.put(u, "/Documents/a.md")
	ctx, cancel := context.WithCancel(bg)
	cancel()
	after(ctx)
	if got := w.cursor(u).WatchETags["/Documents"]; got != base {
		t.Fatalf("baseline moved to %q under a cancelled context", got)
	}
}

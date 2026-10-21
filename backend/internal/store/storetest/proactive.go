package storetest

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/audit"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

func init() { register("Proactive", runProactive) }

func setProactive(t *testing.T, st store.Store, user string, mod func(p *store.Policy)) {
	t.Helper()
	mustTx(t, st, func(tx store.Tx) error {
		p, err := tx.GetPolicy(user)
		if err != nil {
			return err
		}
		mod(&p)
		return tx.PutPolicy(p)
	})
}

func setLastSweep(t *testing.T, st store.Store, user string, at time.Time) {
	t.Helper()
	mustTx(t, st, func(tx store.Tx) error {
		c, err := tx.GetCursor(user)
		if err != nil {
			return err
		}
		c.LastSweepAt = &at
		return tx.PutCursor(c)
	})
}

func proactiveDue(t *testing.T, st store.Store, now time.Time, after string, limit int) []string {
	t.Helper()
	var ids []string
	if err := st.View(bg(), func(tx store.Tx) error {
		var err error
		ids, err = tx.ListProactiveDue(now, after, limit)
		return err
	}); err != nil {
		t.Fatalf("ListProactiveDue: %v", err)
	}
	return ids
}

func checkProactive(t *testing.T, st store.Store, user string, now time.Time, l store.ProactiveLimits, quiet bool) string {
	t.Helper()
	var reason string
	if err := st.View(bg(), func(tx store.Tx) error {
		var err error
		reason, err = store.CheckProactive(tx, user, now, l, quiet)
		return err
	}); err != nil {
		t.Fatalf("CheckProactive: %v", err)
	}
	return reason
}

func runProactive(t *testing.T, f Factory) {
	t.Run("due users: enabled, never swept or the interval passed, in id order", func(t *testing.T) {
		st, clk := open(t, f)
		for _, id := range []string{"a", "b", "c", "d", "e", "f"} {
			mustUser(t, st, id)
		}
		now := clk.Now()
		on := func(p *store.Policy) { p.ProactiveEnabled, p.SweepInterval = true, time.Hour }
		// a: enabled, never swept; b: swept exactly one interval ago;
		// c: swept a millisecond short of it; d: disabled; e: paused (still
		// listed: the caller records why it skips); f: enabled, swept long ago
		setProactive(t, st, "a", on)
		setProactive(t, st, "b", on)
		setLastSweep(t, st, "b", now.Add(-time.Hour))
		setProactive(t, st, "c", on)
		setLastSweep(t, st, "c", now.Add(-time.Hour+time.Millisecond))
		setLastSweep(t, st, "d", now.Add(-48*time.Hour))
		setProactive(t, st, "e", on)
		pause(t, st, "e", store.PauseUser)
		setProactive(t, st, "f", on)
		setLastSweep(t, st, "f", now.Add(-48*time.Hour))

		if got := proactiveDue(t, st, now, "", 0); !slices.Equal(got, []string{"a", "b", "e", "f"}) {
			t.Errorf("due = %v, want [a b e f]", got)
		}
		if got := proactiveDue(t, st, now, "b", 2); !slices.Equal(got, []string{"e", "f"}) {
			t.Errorf("after b, limit 2 = %v", got)
		}
		if got := proactiveDue(t, st, now, "f", 0); len(got) != 0 {
			t.Errorf("after the last = %v", got)
		}
		if got := proactiveDue(t, st, now.Add(time.Millisecond), "", 0); !slices.Contains(got, "c") {
			t.Errorf("c is due a millisecond later: %v", got)
		}
		mustTx(t, st, func(tx store.Tx) error { return tx.DeleteUser("a") })
		if got := proactiveDue(t, st, now, "", 1); !slices.Equal(got, []string{"b"}) {
			t.Errorf("after deleting a: %v", got)
		}
	})

	t.Run("CountJobsOfKinds counts kinds, states and creation in one", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u")
		mustUser(t, st, "other")
		start := clk.Now()
		mustTx(t, st, func(tx store.Tx) error {
			for _, j := range []store.Job{
				{UserID: "u", TriggerKind: store.TriggerSchedule, Goal: "g"},
				{UserID: "u", TriggerKind: store.TriggerManual, Goal: "g"},
				{UserID: "u", TriggerKind: store.TriggerChat, Goal: "g"},
				{UserID: "other", TriggerKind: store.TriggerSchedule, Goal: "g"},
			} {
				if _, err := tx.InsertJob(j); err != nil {
					return err
				}
			}
			return nil
		})
		clk.Advance(time.Hour)
		mustTx(t, st, func(tx store.Tx) error {
			j, err := tx.InsertJob(store.Job{UserID: "u", TriggerKind: store.TriggerFileEvent, Goal: "g"})
			if err != nil {
				return err
			}
			return tx.CancelJob("u", j.ID)
		})
		count := func(kinds []string, states []store.JobState, since time.Time) int {
			t.Helper()
			var n int
			if err := st.View(bg(), func(tx store.Tx) error {
				var err error
				n, err = tx.CountJobsOfKinds("u", kinds, states, since)
				return err
			}); err != nil {
				t.Fatalf("CountJobsOfKinds: %v", err)
			}
			return n
		}
		live := []store.JobState{store.JobQueued, store.JobRunning, store.JobWaitingApproval}
		if got := count(store.ProactiveTriggers, live, time.Time{}); got != 2 {
			t.Errorf("live proactive = %d, want 2", got)
		}
		if got := count(store.ProactiveTriggers, nil, time.Time{}); got != 3 {
			t.Errorf("all proactive = %d, want 3", got)
		}
		if got := count(store.ProactiveTriggers, nil, start.Add(time.Hour)); got != 1 {
			t.Errorf("proactive since the hour = %d, want 1", got)
		}
		if got := count(store.ProactiveTriggers, nil, start); got != 3 {
			t.Errorf("since is inclusive: %d", got)
		}
		if got := count([]string{store.TriggerChat}, live, time.Time{}); got != 1 {
			t.Errorf("chat = %d", got)
		}
		if got := count(nil, nil, time.Time{}); got != 0 {
			t.Errorf("no kinds = %d, want 0", got)
		}
		err := st.View(bg(), func(tx store.Tx) error {
			_, err := tx.CountJobsOfKinds("u", store.ProactiveTriggers, []store.JobState{"bogus"}, time.Time{})
			return err
		})
		if !errors.Is(err, store.ErrInvalid) {
			t.Errorf("unknown state: %v", err)
		}
	})

	t.Run("AuditProactiveSkip records a reason when it changes and then once a day", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u")
		skip := func(trigger, reason string) bool {
			t.Helper()
			var wrote bool
			mustTx(t, st, func(tx store.Tx) error {
				var err error
				wrote, err = store.AuditProactiveSkip(tx, "u", trigger, reason, clk.Now())
				return err
			})
			return wrote
		}
		// never: reasons that are audited elsewhere or expected
		for _, r := range []string{store.SkipNotEnrolled, store.SkipDisabled, store.SkipKillSwitch, store.SkipPaused, store.SkipQuietHours} {
			if skip(store.TriggerSchedule, r) {
				t.Errorf("%s was audited", r)
			}
		}
		if !skip(store.TriggerSchedule, store.SkipLiveJob) {
			t.Fatal("the first live_job skip was not audited")
		}
		clk.Advance(23 * time.Hour)
		if skip(store.TriggerSchedule, store.SkipLiveJob) || skip(store.TriggerFileEvent, store.SkipLiveJob) {
			t.Error("the same reason was audited again within a day")
		}
		// a paused or quiet skip in between is no change
		skip(store.TriggerSchedule, store.SkipPaused)
		if skip(store.TriggerSchedule, store.SkipLiveJob) {
			t.Error("a pause in between counted as a change")
		}
		clk.Advance(time.Hour)
		if !skip(store.TriggerSchedule, store.SkipLiveJob) {
			t.Error("not audited again after 24h")
		}
		if !skip(store.TriggerSchedule, store.SkipBudget) || !skip(store.TriggerSchedule, store.SkipLiveJob) {
			t.Error("a change of reason was not audited")
		}
		// a started job ends the run of skips: the next one is a change
		mustTx(t, st, func(tx store.Tx) error {
			_, err := store.StartProactive(tx, "u", store.TriggerManual, clk.Now(), "oidc")
			return err
		})
		clk.Advance(time.Minute)
		if !skip(store.TriggerSchedule, store.SkipLiveJob) {
			t.Error("a skip after a start was not audited")
		}
		var details []string
		for _, r := range readAll(t, st, "u") {
			if r.Kind == audit.KindProactiveSkipped {
				details = append(details, r.Outcome+" "+r.Detail)
			}
		}
		want := []string{
			"skipped trigger=schedule reason=live_job", "skipped trigger=schedule reason=live_job",
			"skipped trigger=schedule reason=budget", "skipped trigger=schedule reason=live_job",
			"skipped trigger=schedule reason=live_job",
		}
		if !slices.Equal(details, want) {
			t.Errorf("skips = %v, want %v", details, want)
		}
		// a rolled-back skip leaves nothing: the next one is audited
		mustUser(t, st, "v")
		boom := errors.New("boom")
		if err := st.InTx(bg(), func(tx store.Tx) error {
			if _, err := store.AuditProactiveSkip(tx, "v", store.TriggerSchedule, store.SkipDailyCap, clk.Now()); err != nil {
				return err
			}
			return boom
		}); !errors.Is(err, boom) {
			t.Fatal(err)
		}
		mustTx(t, st, func(tx store.Tx) error {
			wrote, err := store.AuditProactiveSkip(tx, "v", store.TriggerSchedule, store.SkipDailyCap, clk.Now())
			if err == nil && !wrote {
				t.Error("the skip after a rollback was not audited")
			}
			return err
		})
	})

	t.Run("CheckProactive reasons", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u")
		l := store.ProactiveLimits{DailyJobs: 2, DailyTokens: 1000}
		now := clk.Now()
		check := func(quiet bool) string { return checkProactive(t, st, "u", now, l, quiet) }

		if got := checkProactive(t, st, "ghost", now, l, true); got != store.SkipNotEnrolled {
			t.Errorf("unknown user: %q", got)
		}
		if got := check(true); got != store.SkipDisabled {
			t.Errorf("disabled: %q", got)
		}
		setProactive(t, st, "u", func(p *store.Policy) { p.ProactiveEnabled = true })
		if got := check(true); got != "" {
			t.Errorf("eligible: %q", got)
		}

		setKillSwitch(t, st, store.KillSwitchOnValue)
		if got := check(true); got != store.SkipKillSwitch {
			t.Errorf("kill switch: %q", got)
		}
		setKillSwitch(t, st, "0")
		if got := checkProactive(t, st, "u", now, store.ProactiveLimits{DailyJobs: 2, DailyTokens: 1000, KillSwitchEnv: true}, true); got != store.SkipKillSwitch {
			t.Errorf("kill switch from the environment: %q", got)
		}

		for _, reason := range []string{store.PauseUser, store.PauseDormant, store.PauseTokenAnomaly, store.PauseCredentialInvalid} {
			setProactive(t, st, "u", func(p *store.Policy) { p.Paused, p.PauseReason = true, reason })
			if got := check(true); got != store.SkipPaused {
				t.Errorf("paused %s: %q", reason, got)
			}
		}
		setProactive(t, st, "u", func(p *store.Policy) { p.Paused, p.PauseReason = false, "" })

		// quiet hours apply only when asked for (the manual run ignores them)
		q := now.UTC().Format("15:04") + "-" + now.UTC().Add(time.Hour).Format("15:04")
		setProactive(t, st, "u", func(p *store.Policy) { p.QuietHours = q })
		if got := check(true); got != store.SkipQuietHours {
			t.Errorf("quiet hours %s at %s: %q", q, now.Format("15:04"), got)
		}
		if got := check(false); got != "" {
			t.Errorf("quiet hours ignored: %q", got)
		}
		setProactive(t, st, "u", func(p *store.Policy) { p.QuietHours = "" })

		// one live proactive job of any proactive kind; chat jobs do not count
		var job store.Job
		mustTx(t, st, func(tx store.Tx) error {
			if _, err := tx.InsertJob(store.Job{UserID: "u", TriggerKind: store.TriggerChat, Goal: "hi"}); err != nil {
				return err
			}
			var err error
			job, err = tx.InsertJob(store.Job{UserID: "u", TriggerKind: store.TriggerFileEvent, Goal: "g"})
			return err
		})
		if got := check(true); got != store.SkipLiveJob {
			t.Errorf("live file_event job: %q", got)
		}
		mustTx(t, st, func(tx store.Tx) error { return tx.CancelJob("u", job.ID) })
		if got := check(true); got != "" {
			t.Errorf("after the cancel: %q", got)
		}

		// the daily cap counts proactive jobs of today in any state
		mustTx(t, st, func(tx store.Tx) error {
			j, err := tx.InsertJob(store.Job{UserID: "u", TriggerKind: store.TriggerManual, Goal: "g"})
			if err != nil {
				return err
			}
			return tx.CancelJob("u", j.ID)
		})
		if got := check(true); got != store.SkipDailyCap {
			t.Errorf("two proactive jobs today with a cap of 2: %q", got)
		}
		tomorrow := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
		if got := checkProactive(t, st, "u", tomorrow, l, true); got != "" {
			t.Errorf("the cap resets at UTC midnight: %q", got)
		}
	})

	t.Run("CheckProactive skips at 80 percent of the daily budget", func(t *testing.T) {
		for _, c := range []struct {
			used int64
			want string
		}{{0, ""}, {799, ""}, {790, ""}, {800, store.SkipBudget}, {810, store.SkipBudget}, {5000, store.SkipBudget}} {
			st, clk := open(t, f)
			mustUser(t, st, "u")
			setProactive(t, st, "u", func(p *store.Policy) { p.ProactiveEnabled = true })
			if c.used > 0 {
				mustTx(t, st, func(tx store.Tx) error {
					if _, err := tx.InsertJob(store.Job{UserID: "u", TriggerKind: store.TriggerChat, Goal: "g"}); err != nil {
						return err
					}
					cl, ok, err := tx.ClaimNextJob("w", time.Minute)
					if err != nil || !ok {
						return errors.New("claim failed")
					}
					if _, err := tx.AppendStep(store.JobStep{JobID: cl.Job.ID, Kind: "llm", Status: "ok", TokensIn: c.used / 2, TokensOut: c.used - c.used/2}, cl.Fence); err != nil {
						return err
					}
					_, err = tx.ReleaseJob(cl, store.JobSucceeded, nil, "")
					return err
				})
			}
			// stored 0: the operator's cap of 1000 applies
			got := checkProactive(t, st, "u", clk.Now(), store.ProactiveLimits{DailyJobs: 5, DailyTokens: 1000}, true)
			if got != c.want {
				t.Errorf("used %d of 1000: %q, want %q", c.used, got, c.want)
			}
		}
	})

	t.Run("StartProactive inserts the job, moves the sweep mark and audits in one transaction", func(t *testing.T) {
		st, clk := open(t, f)
		mustUser(t, st, "u")
		mustTx(t, st, func(tx store.Tx) error {
			c, err := tx.GetCursor("u")
			if err != nil {
				return err
			}
			c.SSELastEventID, c.MemoryETags = "ev-1", map[string]string{"profile.md": `"e1"`}
			return tx.PutCursor(c)
		})
		now := clk.Now()
		var job store.Job
		mustTx(t, st, func(tx store.Tx) error {
			var err error
			job, err = store.StartProactive(tx, "u", store.TriggerSchedule, now, "")
			return err
		})
		goal, _ := store.ProactiveGoal(store.TriggerSchedule)
		if job.TriggerKind != store.TriggerSchedule || job.TriggerRef != "" || job.Goal != goal || job.State != store.JobQueued {
			t.Errorf("job = %+v", job)
		}
		var cur store.Cursor
		mustTx(t, st, func(tx store.Tx) error {
			var err error
			cur, err = tx.GetCursor("u")
			return err
		})
		if cur.LastSweepAt == nil || !cur.LastSweepAt.Equal(now) || cur.SSELastEventID != "ev-1" || cur.MemoryETags["profile.md"] != `"e1"` {
			t.Errorf("cursor = %+v (the other fields must be kept)", cur)
		}
		recs := readAll(t, st, "u")
		if len(recs) != 1 || recs[0].Kind != audit.KindProactiveStarted || recs[0].JobID != job.ID ||
			recs[0].Detail != "trigger=schedule" {
			t.Errorf("audit = %+v", recs)
		}

		// a failing transaction leaves neither job nor mark
		later := now.Add(time.Hour)
		boom := errors.New("boom")
		err := st.InTx(bg(), func(tx store.Tx) error {
			if _, err := store.StartProactive(tx, "u", store.TriggerManual, later, "oidc"); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatal(err)
		}
		mustTx(t, st, func(tx store.Tx) error {
			var err error
			cur, err = tx.GetCursor("u")
			return err
		})
		if !cur.LastSweepAt.Equal(now) {
			t.Errorf("a rolled-back start moved the mark: %v", cur.LastSweepAt)
		}

		// a file event does not move the sweep mark; other kinds are refused
		mustTx(t, st, func(tx store.Tx) error {
			_, err := store.StartProactive(tx, "u", store.TriggerFileEvent, later, "")
			return err
		})
		mustTx(t, st, func(tx store.Tx) error {
			var err error
			cur, err = tx.GetCursor("u")
			return err
		})
		if !cur.LastSweepAt.Equal(now) {
			t.Errorf("a file event moved the sweep mark: %v", cur.LastSweepAt)
		}
		err = st.InTx(bg(), func(tx store.Tx) error {
			_, err := store.StartProactive(tx, "u", store.TriggerChat, later, "")
			return err
		})
		if !errors.Is(err, store.ErrInvalid) {
			t.Errorf("chat kind: %v, want ErrInvalid", err)
		}
	})
}

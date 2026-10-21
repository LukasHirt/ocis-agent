package store

import (
	"errors"
	"strings"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/audit"
)

// Trigger kinds of proactive jobs. Their goals are the fixed templates of
// ProactiveGoal; ModelTaskFor maps all of them to ModelTaskProactive.
const (
	TriggerSchedule  = "schedule"   // the periodic sweep
	TriggerFileEvent = "file_event" // a change in a watch folder
	TriggerManual    = "manual"     // the user asked for a review now
)

const (
	goalReview = "Review my recent file changes and open tasks and propose helpful next steps. Only propose actions; do not make changes without approval."
	goalFiles  = "Files changed in my watched folders. Review what changed and propose helpful next steps. Only propose actions; do not make changes without approval."
)

// ProactiveGoal returns the goal of a proactive job of this trigger kind. The
// Goal reaches the model as the user's own instruction, so it is always one of
// these fixed texts and never embeds a path, a file name, memory or model
// output: the agent finds the changes itself with files.recent_changes, whose
// results are fenced as untrusted data. ok is false for any other kind.
func ProactiveGoal(kind string) (goal string, ok bool) {
	switch kind {
	case TriggerSchedule, TriggerManual:
		return goalReview, true
	case TriggerFileEvent:
		return goalFiles, true
	}
	return "", false
}

// IsProactiveGoal reports whether goal is exactly one of the fixed templates.
func IsProactiveGoal(goal string) bool { return goal == goalReview || goal == goalFiles }

// ProactiveTriggers are the trigger kinds of proactive jobs.
var ProactiveTriggers = []string{TriggerSchedule, TriggerFileEvent, TriggerManual}

// IsProactiveTrigger reports whether kind is one of ProactiveTriggers.
func IsProactiveTrigger(kind string) bool {
	_, ok := ProactiveGoal(kind)
	return ok
}

// Reasons why proactive work is not started (CheckProactive). They are the
// fixed detail of a proactive_skipped audit event.
const (
	SkipNotEnrolled = "not_enrolled"
	SkipDisabled    = "disabled"
	SkipKillSwitch  = "killswitch"
	SkipPaused      = "paused"
	SkipQuietHours  = "quiet_hours"
	SkipBudget      = "budget"
	SkipLiveJob     = "live_job"
	SkipDailyCap    = "daily_cap"
)

// ProactiveLimits are the operator's limits on proactive work.
type ProactiveLimits struct {
	DailyJobs     int   // proactive jobs per user and UTC day (AGENT_PROACTIVE_DAILY_JOBS); <= 0 allows none
	DailyTokens   int64 // the operator's daily token cap (AGENT_USER_DAILY_TOKENS), see DailyBudget
	KillSwitchEnv bool  // AGENT_KILL_SWITCH
}

// liveJobStates are the states of a job that has not ended.
var liveJobStates = []JobState{JobQueued, JobRunning, JobWaitingApproval}

// CheckProactive decides from the database alone whether a proactive job may
// be started for the user at now. It returns "" or the first reason that
// refuses it, in this order: SkipNotEnrolled, SkipDisabled, SkipKillSwitch,
// SkipPaused (any pause reason), SkipQuietHours (only with quiet; UTC),
// SkipLiveJob (a proactive job of any kind is queued, running or waiting for
// approval), SkipDailyCap (l.DailyJobs proactive jobs created since the
// start of the UTC day, in any state) and SkipBudget (the tokens used since
// the start of the UTC day reached 80% of DailyBudget, so interactive chat
// keeps the rest). Run it in the same
// transaction as StartProactive, so concurrent starts never exceed the caps.
func CheckProactive(tx Tx, userID string, now time.Time, l ProactiveLimits, quiet bool) (string, error) {
	if _, err := tx.GetUser(userID); errors.Is(err, ErrNotFound) {
		return SkipNotEnrolled, nil
	} else if err != nil {
		return "", err
	}
	pol, err := tx.GetPolicy(userID)
	if errors.Is(err, ErrNotFound) {
		return SkipNotEnrolled, nil
	} else if err != nil {
		return "", err
	}
	if !pol.ProactiveEnabled {
		return SkipDisabled, nil
	}
	kill, err := KillSwitchOn(tx)
	if err != nil {
		return "", err
	}
	if kill || l.KillSwitchEnv {
		return SkipKillSwitch, nil
	}
	if pol.Paused {
		return SkipPaused, nil
	}
	if quiet && InQuietHours(pol.QuietHours, now) {
		return SkipQuietHours, nil
	}
	live, err := tx.CountJobsOfKinds(userID, ProactiveTriggers, liveJobStates, time.Time{})
	if err != nil {
		return "", err
	}
	if live > 0 {
		return SkipLiveJob, nil
	}
	day := StartOfUTCDay(now)
	total, err := tx.CountJobsOfKinds(userID, ProactiveTriggers, nil, day)
	if err != nil {
		return "", err
	}
	if total >= l.DailyJobs {
		return SkipDailyCap, nil
	}
	if budget := DailyBudget(pol.DailyTokens, l.DailyTokens); budget > 0 {
		in, out, err := tx.SumJobTokens(userID, day)
		if err != nil {
			return "", err
		}
		if BudgetReached(in+out, budget, TriggerSchedule) {
			return SkipBudget, nil
		}
	}
	return "", nil
}

// StartProactive inserts a queued proactive job of kind for the user (the
// fixed goal of ProactiveGoal, no trigger ref), records it as the user's
// last sweep (schedule and manual only: a file event does not postpone the
// sweep) and appends the proactive_started audit event, all in tx. cred is
// the audit's credential ("oidc" for the user's own request, "" for the
// scheduler). ErrInvalid for a kind that is not proactive. Check with
// CheckProactive in the same transaction first.
func StartProactive(tx Tx, userID, kind string, now time.Time, cred string) (Job, error) {
	goal, ok := ProactiveGoal(kind)
	if !ok {
		return Job{}, Invalid("not a proactive trigger kind")
	}
	job, err := tx.InsertJob(Job{UserID: userID, TriggerKind: kind, Goal: goal})
	if err != nil {
		return Job{}, err
	}
	if kind != TriggerFileEvent {
		c, err := tx.GetCursor(userID)
		if err != nil {
			return Job{}, err
		}
		at := now
		c.LastSweepAt = &at
		if err := tx.PutCursor(c); err != nil {
			return Job{}, err
		}
	}
	if _, err := tx.AppendAudit(audit.Event{
		UserID: userID, TS: now, JobID: job.ID, Kind: audit.KindProactiveStarted, Cred: cred, Outcome: "queued",
		Detail: "trigger=" + kind,
	}); err != nil {
		return Job{}, err
	}
	return job, nil
}

// Skip auditing (AuditProactiveSkip).
const (
	// SkipAuditRepeat is how long the same skip reason stays unaudited.
	SkipAuditRepeat = 24 * time.Hour
	// skipAuditLookback is how many of the user's newest audit records are
	// searched for the last proactive event; one further back counts as no
	// event, so at worst a skip is audited once per that many other events.
	skipAuditLookback = 200
)

// auditedSkip reports whether a skip reason is ever audited: only the caps,
// which say why proactive work did not happen. A pause is audited when it
// happens, quiet hours are the user's own setting, and the others mean
// there is nothing to report.
// SkipAudited reports whether AuditProactiveSkip can record a skip for
// reason (live_job, daily_cap and budget); for the others it never writes.
func SkipAudited(reason string) bool { return auditedSkip(reason) }

func auditedSkip(reason string) bool {
	return reason == SkipLiveJob || reason == SkipDailyCap || reason == SkipBudget
}

// AuditProactiveSkip appends a proactive_skipped event (outcome skipped,
// detail "trigger=<trigger> reason=<reason>") when it is worth recording,
// and reports whether it did: only for SkipLiveJob, SkipDailyCap and
// SkipBudget, and only if the user's last proactive event (skipped or
// started, within the newest records of the chain) is not a skip for the
// same reason less than SkipAuditRepeat ago. The record is the audit chain
// itself, so the rule holds across restarts, a rolled-back skip leaves no
// trace, and a started job (proactive_started) ends a run of skips. Call it
// in the transaction that decided the skip.
func AuditProactiveSkip(tx Tx, userID, trigger, reason string, now time.Time) (bool, error) {
	if !auditedSkip(reason) {
		return false, nil
	}
	recs, err := tx.ReadAuditBefore(userID, 0, skipAuditLookback)
	if err != nil {
		return false, err
	}
	for _, r := range recs {
		if r.Kind == audit.KindProactiveStarted {
			break
		}
		if r.Kind == audit.KindProactiveSkipped {
			if strings.HasSuffix(r.Detail, " reason="+reason) && now.Sub(r.TS) < SkipAuditRepeat {
				return false, nil
			}
			break
		}
	}
	if _, err := tx.AppendAudit(audit.Event{
		UserID: userID, TS: now, Kind: audit.KindProactiveSkipped, Outcome: "skipped",
		Detail: "trigger=" + trigger + " reason=" + reason,
	}); err != nil {
		return false, err
	}
	return true, nil
}

// BudgetReached reports whether used tokens exhaust the daily budget for a
// job of triggerKind: a chat job may use all of it, every other (proactive)
// job stops at 80%, so interactive chat keeps the rest. budget <= 0 is
// unlimited. The runner's precheck and CheckProactive both use it.
func BudgetReached(used, budget int64, triggerKind string) bool {
	if budget <= 0 {
		return false
	}
	if ModelTaskFor(triggerKind) == ModelTaskProactive {
		// budget - budget/5 is 80% of the budget rounded up, without overflow
		return used >= budget-budget/5
	}
	return used >= budget
}

// StartOfUTCDay is the midnight (UTC) that starts t's UTC day: daily caps
// and budgets reset there.
func StartOfUTCDay(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// ValidQuietHours accepts "" (no quiet hours) or HH:MM-HH:MM in 24-hour time
// with two different ends.
func ValidQuietHours(q string) bool {
	_, _, ok := parseQuietHours(q)
	return ok || q == ""
}

// InQuietHours reports whether t (read in UTC) falls into the quiet hours q:
// from the start (inclusive) to the end (exclusive), wrapping midnight when
// the end is earlier than the start. "" is never quiet; a malformed value is
// always quiet, so a corrupt row cannot let proactive work through.
func InQuietHours(q string, t time.Time) bool {
	if q == "" {
		return false
	}
	start, end, ok := parseQuietHours(q)
	if !ok {
		return true
	}
	u := t.UTC()
	m := u.Hour()*60 + u.Minute()
	if start < end {
		return m >= start && m < end
	}
	return m >= start || m < end
}

// QuietHoursEnd returns when the quiet hours q that contain t (read in UTC)
// end, or the zero time if t is not in them (also for "" and a malformed
// value).
func QuietHoursEnd(q string, t time.Time) time.Time {
	start, end, ok := parseQuietHours(q)
	if !ok || !InQuietHours(q, t) {
		return time.Time{}
	}
	u := t.UTC()
	day := StartOfUTCDay(u)
	at := day.Add(time.Duration(end) * time.Minute)
	if m := u.Hour()*60 + u.Minute(); start > end && m >= start {
		at = at.AddDate(0, 0, 1)
	}
	return at
}

func parseQuietHours(q string) (start, end int, ok bool) {
	if len(q) != 11 || q[5] != '-' {
		return 0, 0, false
	}
	a, ok1 := clockMinutes(q[:5])
	b, ok2 := clockMinutes(q[6:])
	if !ok1 || !ok2 || a == b {
		return 0, 0, false
	}
	return a, b, true
}

func clockMinutes(s string) (int, bool) {
	if len(s) != 5 || s[2] != ':' {
		return 0, false
	}
	for _, i := range []int{0, 1, 3, 4} {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	h, m := int(s[0]-'0')*10+int(s[1]-'0'), int(s[3]-'0')*10+int(s[4]-'0')
	if h > 23 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

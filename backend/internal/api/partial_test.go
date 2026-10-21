package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/ocisclient/fakeocis"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// hostilePartial is model text the views must return exactly as stored:
// markup, a bidi override and terminal escapes are not interpreted.
const hostilePartial = "<script>alert(1)</script> ‮evil‬ \x1b[31mred\x1b[0m &amp; \"q\" \\ \t\n"

func (s *stack) setPartial(t *testing.T, c store.Claim, state store.PartialState, text string) {
	t.Helper()
	s.mustTx(t, func(tx store.Tx) error { return tx.SetJobPartial(c.Job.UserID, c.Job.ID, c.Fence, state, text) })
}

// claimRunning claims the queued job id, like a worker.
func (s *stack) claimRunning(t *testing.T, user, id string) store.Claim {
	t.Helper()
	var c store.Claim
	s.mustTx(t, func(tx store.Tx) error {
		var ok bool
		var err error
		c, ok, err = tx.ClaimNextJob("w", time.Hour)
		if err == nil && (!ok || c.Job.ID != id || c.Job.UserID != user) {
			err = fmt.Errorf("claimed %v %+v, want %s", ok, c.Job, id)
		}
		return err
	})
	return c
}

// messageJobOf returns the job object of the user message answered by
// jobID in the conversation's first page.
func (s *stack) messageJobOf(t *testing.T, login, conv, jobID string) map[string]any {
	t.Helper()
	rec := s.as(login, http.MethodGet, convPath+"/"+conv+"/messages", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("messages = %d %s", rec.Code, rec.Body.String())
	}
	for _, m := range decode(t, rec)["messages"].([]any) {
		m := m.(map[string]any)
		if m["job_id"] == jobID && m["role"] == store.RoleUser {
			return m["job"].(map[string]any)
		}
	}
	t.Fatalf("no user message of job %s", jobID)
	return nil
}

func TestRunningJobShowsItsPartial(t *testing.T) {
	s := newWorkStack(t)
	c := s.newConv(t, "einstein", "{}")
	id := s.send(t, "einstein", c.ID, "question").Job.ID
	claim := s.claimRunning(t, einsteinID, id)

	check := func(state, text string) {
		t.Helper()
		views := map[string]map[string]any{
			"list":    s.listJobs(t, "einstein", "").Jobs[0],
			"detail":  decode(t, s.as("einstein", http.MethodGet, jobsPath+"/"+id, "")),
			"message": s.messageJobOf(t, "einstein", c.ID, id),
		}
		for name, v := range views {
			if v["id"] != id || v["state"] != "running" {
				t.Fatalf("%s: %v", name, v)
			}
			if v["partial"] != text || v["partial_state"] != state {
				got, _ := v["partial"].(string)
				t.Errorf("%s: partial %q (%v), want %q (%s)", name, got[:min(len(got), 80)], v["partial_state"], text[:min(len(text), 80)], state)
			}
		}
	}
	check("", "") // claimed, nothing streamed yet
	s.setPartial(t, claim, store.PartialThinking, "")
	check("thinking", "")
	s.setPartial(t, claim, store.PartialWriting, hostilePartial)
	check("writing", hostilePartial)
	// the stored maximum is returned whole
	full := strings.Repeat("ä", store.MaxPartialBytes/2)
	s.setPartial(t, claim, store.PartialWriting, full)
	check("writing", full)

	// another user's job and conversation stay 404
	if rec := s.as("marie", http.MethodGet, jobsPath+"/"+id, ""); rec.Code != http.StatusNotFound {
		t.Errorf("marie's detail = %d", rec.Code)
	}
	if rec := s.as("marie", http.MethodGet, convPath+"/"+c.ID+"/messages", ""); rec.Code != http.StatusNotFound {
		t.Errorf("marie's messages = %d", rec.Code)
	}
	for _, j := range s.listJobs(t, "marie", "").Jobs {
		if j["partial"] != "" {
			t.Errorf("marie sees a partial: %v", j)
		}
	}

	// the cancel response shows the job ended: no partial
	rec := s.as("einstein", http.MethodPost, jobsPath+"/"+id+"/cancel", "")
	if m := decode(t, rec); rec.Code != http.StatusOK || m["partial"] != "" || m["partial_state"] != "" {
		t.Errorf("cancel = %d %v", rec.Code, m)
	}
}

// staleWrap hands every job out with a partial, whatever its state, like a
// row whose partial a bug failed to clear.
type staleWrap struct{ store.Store }

type staleTx struct{ store.Tx }

var stalePartial = store.JobPartial{Partial: "stale text", PartialState: store.PartialWriting, PartialAt: time.Unix(1, 0)}

func (t staleTx) GetJob(user, id string) (store.Job, error) {
	j, err := t.Tx.GetJob(user, id)
	j.JobPartial = stalePartial
	return j, err
}

func (t staleTx) ListJobSummaries(user string, f store.JobFilter) ([]store.JobSummary, error) {
	list, err := t.Tx.ListJobSummaries(user, f)
	for i := range list {
		list[i].JobPartial = stalePartial
	}
	return list, err
}

func (t staleTx) JobStatuses(user string, ids []string) (map[string]store.JobStatus, error) {
	m, err := t.Tx.JobStatuses(user, ids)
	for k, v := range m {
		v.JobPartial = stalePartial
		m[k] = v
	}
	return m, err
}

func (s staleWrap) View(ctx context.Context, fn func(store.Tx) error) error {
	return s.Store.View(ctx, func(tx store.Tx) error { return fn(staleTx{tx}) })
}

func TestOnlyARunningJobShowsAPartial(t *testing.T) {
	s := newStackStore(t, func(st store.Store) store.Store { return staleWrap{st} })
	s.enroll(t, "einstein")
	c := s.newConv(t, "einstein", "{}")
	send := func(content string) string { return s.send(t, "einstein", c.ID, content).Job.ID }
	byState := map[string]string{}
	byState["succeeded"] = send("s")
	s.claimAndRelease(t, einsteinID, byState["succeeded"], time.Hour, store.JobSucceeded, "")
	byState["failed"] = send("f")
	s.claimAndRelease(t, einsteinID, byState["failed"], time.Hour, store.JobFailed, "x")
	byState["cancelled"] = send("c")
	s.mustTx(t, func(tx store.Tx) error { return tx.CancelJob(einsteinID, byState["cancelled"]) })
	byState["waiting_approval"] = send("w")
	s.claimAndRelease(t, einsteinID, byState["waiting_approval"], time.Hour, store.JobWaitingApproval, "")
	byState["running"] = send("r")
	s.claimAndRelease(t, einsteinID, byState["running"], time.Hour, store.JobRunning, "")
	byState["queued"] = send("q")

	list := map[string]map[string]any{}
	for _, j := range s.listJobs(t, "einstein", "").Jobs {
		list[j["id"].(string)] = j
	}
	for state, id := range byState {
		wantText, wantState := "", ""
		if state == "running" {
			wantText, wantState = stalePartial.Partial, string(stalePartial.PartialState)
		}
		for name, v := range map[string]map[string]any{
			"list":    list[id],
			"detail":  decode(t, s.as("einstein", http.MethodGet, jobsPath+"/"+id, "")),
			"message": s.messageJobOf(t, "einstein", c.ID, id),
		} {
			if v["state"] != state || v["partial"] != wantText || v["partial_state"] != wantState {
				t.Errorf("%s of a %s job: state %v, partial %q (%v)", name, state, v["state"], v["partial"], v["partial_state"])
			}
		}
	}
}

func TestPartialViewIsCappedAndChecked(t *testing.T) {
	long := store.JobPartial{Partial: strings.Repeat("a", 70<<10), PartialState: store.PartialWriting}
	v := jobViewOf(store.JobSummary{State: store.JobRunning, JobPartial: long})
	if len(v.Partial) > maxAnswerView || !strings.HasSuffix(v.Partial, truncMarker) || v.PartialState != "writing" {
		t.Errorf("capped partial: len %d, state %q", len(v.Partial), v.PartialState)
	}
	mv := messageJobViewOf("J", store.JobStatus{State: store.JobRunning, JobPartial: long})
	if mv.Partial != v.Partial || mv.PartialState != "writing" {
		t.Errorf("message view differs: len %d", len(mv.Partial))
	}
	// an unknown state is not passed on, and text without a state is not shown
	odd := jobViewOf(store.JobSummary{State: store.JobRunning, JobPartial: store.JobPartial{Partial: "x", PartialState: "evil"}})
	if odd.Partial != "" || odd.PartialState != "" {
		t.Errorf("unknown state: %+v", odd)
	}
	none := jobViewOf(store.JobSummary{State: store.JobRunning, JobPartial: store.JobPartial{Partial: "x"}})
	if none.Partial != "" || none.PartialState != "" {
		t.Errorf("no state: %+v", none)
	}
}

// A chat job's partial write moves the messages mark only; another job's
// moves no mark.
func TestEventsReportPartialWrites(t *testing.T) {
	s := newWorkStack(t)
	ft := installTimers(s.srv)
	ts := newEventServer(t, s.h)
	conv := s.newConv(t, "einstein", "{}")
	id := s.send(t, "einstein", conv.ID, "question").Job.ID
	claim := s.claimRunning(t, einsteinID, id)
	c, st := openReady(t, ts, ft, fakeocis.OIDCToken("einstein"))
	quiet := func() {
		t.Helper()
		st.pollOnce(t)
		st.send(t, st.ping)
		c.want(pingFrame)
	}
	quiet()
	for _, text := range []string{"Hel", "Hello"} {
		s.fake.Advance(time.Second)
		s.setPartial(t, claim, store.PartialWriting, text)
		st.pollOnce(t)
		c.want(changedFrame("messages"))
	}
	quiet()

	// the job ends: its partial no longer counts for the messages mark
	s.fake.Advance(time.Second)
	s.finishJob(t, claim, store.JobSucceeded, nil, "")
	st.pollOnce(t)
	c.want(changedFrame("jobs", "messages"))

	// a proactive job's partial moves no mark
	s.fake.Advance(time.Second)
	p := s.startJob(t, einsteinID, store.TriggerManual, "review")
	st.pollOnce(t)
	c.want(changedFrame("jobs"))
	s.fake.Advance(time.Second)
	s.setPartial(t, p, store.PartialWriting, "draft")
	quiet()
}

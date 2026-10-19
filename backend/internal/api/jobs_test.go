package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/agent"
	"github.com/owncloud/ocis-agent/backend/internal/approvals"
	"github.com/owncloud/ocis-agent/backend/internal/llm"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/tools"
)

const jobsPath = "/agent-api/v1/jobs"

// newWorkStack is the stack with the real approvals.Gate (its clock is the
// fake's) and both users enrolled.
func newWorkStack(t *testing.T) *stack {
	t.Helper()
	s := newStack(t, func(d *Deps) {
		reg, err := tools.NewRegistry(tools.All()...)
		if err != nil {
			t.Fatal(err)
		}
		g, err := approvals.NewGate(d.Store, reg, approvals.Config{Now: d.Now})
		if err != nil {
			t.Fatal(err)
		}
		d.Gate = g
	})
	s.enroll(t, "einstein")
	s.enroll(t, "marie")
	return s
}

// queuedJob inserts a job that no claim picks up (not before next year).
func (s *stack) queuedJob(t *testing.T, user, trigger, goal string) store.Job {
	t.Helper()
	var j store.Job
	s.mustTx(t, func(tx store.Tx) error {
		var err error
		j, err = tx.InsertJob(store.Job{UserID: user, TriggerKind: trigger, Goal: goal, NotBefore: s.fake.Now().AddDate(1, 0, 0)})
		return err
	})
	return j
}

// startJob inserts a job and claims it, like a worker. The user must not
// hold a lease (no other running job).
func (s *stack) startJob(t *testing.T, user, trigger, goal string) store.Claim {
	t.Helper()
	var c store.Claim
	s.mustTx(t, func(tx store.Tx) error {
		j, err := tx.InsertJob(store.Job{UserID: user, TriggerKind: trigger, Goal: goal})
		if err != nil {
			return err
		}
		var ok bool
		c, ok, err = tx.ClaimNextJob("w1", time.Hour)
		if err != nil {
			return err
		}
		if !ok || c.Job.ID != j.ID {
			return fmt.Errorf("claimed %v %q, want %q", ok, c.Job.ID, j.ID)
		}
		return nil
	})
	return c
}

func (s *stack) addStep(t *testing.T, c store.Claim, st store.JobStep) {
	t.Helper()
	st.JobID = c.Job.ID
	s.mustTx(t, func(tx store.Tx) error { _, err := tx.AppendStep(st, c.Fence); return err })
}

// finishJob releases the claim into a terminal state with the checkpoint.
func (s *stack) finishJob(t *testing.T, c store.Claim, state store.JobState, cp json.RawMessage, errMsg string) {
	t.Helper()
	s.mustTx(t, func(tx store.Tx) error {
		ok, err := tx.ReleaseJob(c, state, cp, errMsg)
		if err == nil && !ok {
			err = fmt.Errorf("release not applied")
		}
		return err
	})
}

func hashOf(args string) string {
	sum := sha256.Sum256([]byte(args))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// requestApprovals inserts one pending approval per args document and puts
// the job to sleep, in one transaction (as Gate.Request does).
func (s *stack) requestApprovals(t *testing.T, c store.Claim, args ...string) []store.Approval {
	t.Helper()
	var out []store.Approval
	s.mustTx(t, func(tx store.Tx) error {
		for i, a := range args {
			call := fmt.Sprintf("c%d", i)
			ap, err := tx.InsertApproval(store.Approval{
				UserID: c.Job.UserID, JobID: c.Job.ID, CallID: call, Tool: "files.write", Risk: store.RiskWrite,
				Args: json.RawMessage(a), ArgsHash: hashOf(a), TargetETag: `"etag-1"`, Rationale: "the model says so",
				Preview: json.RawMessage(`{"action":"write","paths":["/a.txt"]}`), IdemKey: c.Job.ID + ":" + call + ":" + hashOf(a),
				ExpiresAt: s.fake.Now().Add(time.Hour),
			}, c.Fence)
			if err != nil {
				return err
			}
			out = append(out, ap)
		}
		ok, err := tx.ReleaseJob(c, store.JobWaitingApproval, nil, "")
		if err == nil && !ok {
			err = fmt.Errorf("release not applied")
		}
		return err
	})
	return out
}

func (s *stack) job(t *testing.T, user, id string) store.Job {
	t.Helper()
	var j store.Job
	if err := s.st.View(bg, func(tx store.Tx) error {
		var err error
		j, err = tx.GetJob(user, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return j
}

func checkpointWith(t *testing.T, answer string) json.RawMessage {
	t.Helper()
	cp := agent.Checkpoint{V: 1, Messages: []llm.Message{{Role: llm.RoleUser, Content: "goal"}}, Seen: map[string]int{}, Answer: answer}
	b, err := cp.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type jobPage struct {
	Jobs       []map[string]any `json:"jobs"`
	NextCursor *string          `json:"next_cursor"`
}

func jobsOf(t *testing.T, body string) jobPage {
	t.Helper()
	var p jobPage
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("%s: %v", body, err)
	}
	return p
}

func (s *stack) listJobs(t *testing.T, login, query string) jobPage {
	t.Helper()
	rec := s.as(login, http.MethodGet, jobsPath+query, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list %s = %d %s", query, rec.Code, rec.Body.String())
	}
	return jobsOf(t, rec.Body.String())
}

func ids(list []map[string]any) []string {
	var out []string
	for _, m := range list {
		out = append(out, m["id"].(string))
	}
	return out
}

var jobKeys = []string{"id", "trigger_kind", "goal", "state", "attempt", "runs", "created_at", "updated_at", "error", "steps_count"}

func wantKeys(t *testing.T, m map[string]any, keys ...string) {
	t.Helper()
	if len(m) != len(keys) {
		t.Errorf("keys = %v, want %v", mapKeys(m), keys)
	}
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			t.Errorf("missing key %q in %v", k, mapKeys(m))
		}
	}
}

func mapKeys(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestJobListShapeAndScoping(t *testing.T) {
	s := newWorkStack(t)
	a1 := s.queuedJob(t, einsteinID, "chat", "first")
	b1 := s.queuedJob(t, marieID, "chat", "marie's")
	a2 := s.queuedJob(t, einsteinID, "file_change", "second")

	p := s.listJobs(t, "einstein", "")
	if got := ids(p.Jobs); strings.Join(got, ",") != a2.ID+","+a1.ID {
		t.Fatalf("ids = %v, want newest first %s,%s", got, a2.ID, a1.ID)
	}
	if p.NextCursor != nil {
		t.Errorf("next_cursor = %v", *p.NextCursor)
	}
	wantKeys(t, p.Jobs[0], jobKeys...)
	j := p.Jobs[0]
	if j["trigger_kind"] != "file_change" || j["goal"] != "second" || j["state"] != "queued" || j["steps_count"] != float64(0) ||
		j["error"] != "" || j["created_at"] == nil {
		t.Errorf("view = %v", j)
	}
	for _, id := range ids(s.listJobs(t, "marie", "").Jobs) {
		if id != b1.ID {
			t.Errorf("marie sees %s", id)
		}
	}
	// another user's job is not found, through every route
	for _, rec := range []interface{ Result() *http.Response }{
		s.as("einstein", http.MethodGet, jobsPath+"/"+b1.ID, ""),
		s.as("einstein", http.MethodPost, jobsPath+"/"+b1.ID+"/cancel", ""),
	} {
		if rec.Result().StatusCode != http.StatusNotFound {
			t.Errorf("other user's job = %d", rec.Result().StatusCode)
		}
	}
	if st := s.job(t, marieID, b1.ID).State; st != store.JobQueued {
		t.Errorf("marie's job = %s", st)
	}
}

func TestJobListPagination(t *testing.T) {
	s := newWorkStack(t)
	var want []string // newest first
	for i := range 230 {
		j := s.queuedJob(t, einsteinID, "chat", fmt.Sprintf("goal %d", i))
		want = append([]string{j.ID}, want...)
	}
	s.queuedJob(t, marieID, "chat", "not einstein's")
	var got []string
	cursor := ""
	pages := 0
	for {
		q := "?limit=100"
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		p := s.listJobs(t, "einstein", q)
		pages++
		got = append(got, ids(p.Jobs)...)
		if p.NextCursor == nil {
			break
		}
		if *p.NextCursor != got[len(got)-1] {
			t.Fatalf("next_cursor %s is not the last id", *p.NextCursor)
		}
		cursor = *p.NextCursor
		if pages > 5 {
			t.Fatal("pagination does not end")
		}
	}
	if pages != 3 || strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("pages = %d, got %d ids (want %d) in order: %v", pages, len(got), len(want), strings.Join(got, ",") == strings.Join(want, ","))
	}
	if p := s.listJobs(t, "einstein", ""); len(p.Jobs) != 50 || p.NextCursor == nil {
		t.Errorf("default page = %d %v", len(p.Jobs), p.NextCursor)
	}
	if p := s.listJobs(t, "einstein", "?limit=1"); len(p.Jobs) != 1 || *p.NextCursor != want[0] {
		t.Errorf("limit=1 = %v", ids(p.Jobs))
	}
	// a well-formed cursor that is not one of the caller's jobs: an empty page
	other := s.queuedJob(t, marieID, "chat", "x")
	if p := s.listJobs(t, "einstein", "?cursor="+other.ID); len(p.Jobs) != 0 || p.NextCursor != nil {
		t.Errorf("foreign cursor = %v", ids(p.Jobs))
	}
	for _, q := range []string{
		"?cursor=nope", "?cursor=" + strings.ToLower(want[0]), "?cursor=" + want[0] + "x", "?cursor=8" + want[0][1:],
		"?cursor=" + want[0][:25] + "U", "?limit=0", "?limit=101", "?limit=-1", "?limit=+5", "?limit=x", "?limit=1&limit=2",
		"?state=done", "?state=QUEUED", "?state=queued&state=failed", "?cursor=" + want[0] + "&cursor=" + want[1],
	} {
		wantError(t, s.as("einstein", http.MethodGet, jobsPath+q, ""), http.StatusBadRequest, "invalid_request")
	}
}

func TestJobListStateFilter(t *testing.T) {
	s := newWorkStack(t)
	c := s.startJob(t, einsteinID, "chat", "done")
	s.finishJob(t, c, store.JobSucceeded, nil, "")
	c = s.startJob(t, einsteinID, "chat", "broken")
	s.finishJob(t, c, store.JobFailed, nil, agent.ReasonInternal)
	q := s.queuedJob(t, einsteinID, "chat", "waiting")
	for state, want := range map[string]string{"succeeded": "done", "failed": "broken", "queued": "waiting"} {
		p := s.listJobs(t, "einstein", "?state="+state)
		if len(p.Jobs) != 1 || p.Jobs[0]["goal"] != want {
			t.Errorf("state %s = %v", state, p.Jobs)
		}
	}
	if p := s.listJobs(t, "einstein", "?state=failed"); p.Jobs[0]["error"] != agent.ReasonInternal {
		t.Errorf("error = %v", p.Jobs[0]["error"])
	}
	if p := s.listJobs(t, "einstein", "?state="); len(p.Jobs) != 3 || p.Jobs[0]["id"] != q.ID {
		t.Errorf("all = %v", ids(p.Jobs))
	}
	if p := s.listJobs(t, "einstein", "?state=running"); len(p.Jobs) != 0 {
		t.Errorf("running = %v", ids(p.Jobs))
	}
}

func TestJobDetailWithStepsAndAnswer(t *testing.T) {
	s := newWorkStack(t)
	c := s.startJob(t, einsteinID, "chat", "summarise")
	s.addStep(t, c, store.JobStep{Kind: "llm", Status: "ok", TokensIn: 10, TokensOut: 5})
	s.addStep(t, c, store.JobStep{Kind: "tool", Tool: "files.read", CallID: "c0", Status: "done", Summary: "read /a.txt"})
	s.finishJob(t, c, store.JobSucceeded, checkpointWith(t, "Here is the summary."), "")

	rec := s.as("einstein", http.MethodGet, jobsPath+"/"+c.Job.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	wantKeys(t, m, append(jobKeys, "steps", "answer")...)
	if m["answer"] != "Here is the summary." || m["state"] != "succeeded" || m["steps_count"] != float64(2) {
		t.Errorf("detail = %v", m)
	}
	steps := m["steps"].([]any)
	if len(steps) != 2 {
		t.Fatalf("steps = %v", steps)
	}
	st := steps[1].(map[string]any)
	wantKeys(t, st, "n", "kind", "tool", "call_id", "status", "tokens_in", "tokens_out", "summary", "approval_id", "created_at")
	if st["n"] != float64(2) || st["tool"] != "files.read" || st["call_id"] != "c0" || st["summary"] != "read /a.txt" {
		t.Errorf("step = %v", st)
	}
	if l := steps[0].(map[string]any); l["tokens_in"] != float64(10) || l["tokens_out"] != float64(5) || l["kind"] != "llm" {
		t.Errorf("llm step = %v", l)
	}
	// a list entry has no steps or answer
	wantKeys(t, s.listJobs(t, "einstein", "").Jobs[0], jobKeys...)
}

func TestJobDetailEmptyStepsAndGarbageCheckpoint(t *testing.T) {
	s := newWorkStack(t)
	q := s.queuedJob(t, einsteinID, "chat", "fresh")
	m := decode(t, s.as("einstein", http.MethodGet, jobsPath+"/"+q.ID, ""))
	if steps, ok := m["steps"].([]any); !ok || len(steps) != 0 || m["answer"] != "" {
		t.Errorf("fresh job = %v", m)
	}
	c := s.startJob(t, einsteinID, "chat", "garbage")
	s.finishJob(t, c, store.JobSucceeded, json.RawMessage(`{"v":7,"answer":"SECRET-CHECKPOINT-TEXT"}`), "")
	rec := s.as("einstein", http.MethodGet, jobsPath+"/"+c.Job.ID, "")
	if rec.Code != http.StatusOK || decode(t, rec)["answer"] != "" || strings.Contains(rec.Body.String(), "SECRET-CHECKPOINT") {
		t.Errorf("garbage checkpoint = %d %s", rec.Code, rec.Body.String())
	}
}

func TestJobUntrustedFieldsAreCapped(t *testing.T) {
	s := newWorkStack(t)
	goal := strings.Repeat("g", 5000)
	c := s.startJob(t, einsteinID, "file_change", goal)
	s.addStep(t, c, store.JobStep{Kind: "tool", Tool: "files.read", Status: "done", Summary: strings.Repeat("ü", 1000)})
	s.finishJob(t, c, store.JobSucceeded, checkpointWith(t, strings.Repeat("a", 70<<10)), "")
	m := decode(t, s.as("einstein", http.MethodGet, jobsPath+"/"+c.Job.ID, ""))
	check := func(name, v string, limit int, prefix string) {
		t.Helper()
		if len(v) > limit || !strings.HasSuffix(v, truncMarker) || !strings.HasPrefix(v, prefix) {
			t.Errorf("%s: len %d, suffix %q", name, len(v), v[max(0, len(v)-20):])
		}
	}
	check("goal", m["goal"].(string), 4<<10, "ggg")
	check("summary", m["steps"].([]any)[0].(map[string]any)["summary"].(string), 1<<10, "üü")
	check("answer", m["answer"].(string), 64<<10, "aaa")
	check("list goal", s.listJobs(t, "einstein", "").Jobs[0]["goal"].(string), 4<<10, "ggg")
	// a value at the cap is unchanged
	if got := capText(strings.Repeat("x", 1024), 1024); got != strings.Repeat("x", 1024) {
		t.Errorf("at cap: %d", len(got))
	}
	if got := capText("", 10); got != "" {
		t.Errorf("empty: %q", got)
	}
}

func TestJobCancel(t *testing.T) {
	s := newWorkStack(t)
	q := s.queuedJob(t, einsteinID, "chat", "stop me")
	rec := s.as("einstein", http.MethodPost, jobsPath+"/"+q.ID+"/cancel", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel = %d %s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	wantKeys(t, m, jobKeys...)
	if m["state"] != "cancelled" || m["id"] != q.ID {
		t.Errorf("view = %v", m)
	}
	if st := s.job(t, einsteinID, q.ID).State; st != store.JobCancelled {
		t.Errorf("state = %s", st)
	}
	ev := s.auditKind(t, einsteinID, "job_cancelled")
	if len(ev) != 1 || ev[0].JobID != q.ID || ev[0].Cred != "oidc" || ev[0].Outcome != "cancelled" {
		t.Errorf("audit = %+v", ev)
	}
	// already terminal
	wantError(t, s.as("einstein", http.MethodPost, jobsPath+"/"+q.ID+"/cancel", "{}"), http.StatusConflict, "conflict")
	if n := len(s.auditKind(t, einsteinID, "job_cancelled")); n != 1 {
		t.Errorf("audit after conflict = %d", n)
	}
	// {} body works, other bodies are refused
	q2 := s.queuedJob(t, einsteinID, "chat", "again")
	for _, body := range []string{`{"x":1}`, `[]`, `null`, `{"reason":"x"}`} {
		wantError(t, s.as("einstein", http.MethodPost, jobsPath+"/"+q2.ID+"/cancel", body), http.StatusBadRequest, "invalid_request")
	}
	if rec := s.as("einstein", http.MethodPost, jobsPath+"/"+q2.ID+"/cancel", "{}"); rec.Code != http.StatusOK {
		t.Errorf("{} = %d", rec.Code)
	}
	// unknown and malformed ids
	wantError(t, s.as("einstein", http.MethodPost, jobsPath+"/"+store.NewID()+"/cancel", ""), http.StatusNotFound, "not_found")
	wantError(t, s.as("einstein", http.MethodPost, jobsPath+"/x/cancel", ""), http.StatusNotFound, "not_found")
}

func TestJobCancelLeavesApprovalsAlone(t *testing.T) {
	s := newWorkStack(t)
	c := s.startJob(t, einsteinID, "chat", "w")
	aps := s.requestApprovals(t, c, `{"path":"/a.txt"}`)
	if rec := s.as("einstein", http.MethodPost, jobsPath+"/"+c.Job.ID+"/cancel", ""); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if a := s.approval(t, einsteinID, aps[0].ID); a.State != store.ApprovalPending {
		t.Errorf("approval = %s", a.State)
	}
}

func TestJobIDShape(t *testing.T) {
	s := newWorkStack(t)
	q := s.queuedJob(t, einsteinID, "chat", "x")
	if rec := s.as("einstein", http.MethodGet, jobsPath+"/"+q.ID, ""); rec.Code != http.StatusOK {
		t.Fatalf("valid = %d", rec.Code)
	}
	for _, id := range []string{"x", strings.ToLower(q.ID), q.ID + "0", q.ID[:25], "8" + q.ID[1:], q.ID[:25] + "I", q.ID[:25] + "%20"} {
		wantError(t, s.as("einstein", http.MethodGet, jobsPath+"/"+id, ""), http.StatusNotFound, "not_found")
	}
	if !validID(q.ID) || validID("") || validID(strings.Repeat("0", 26)+"0") {
		t.Error("validID")
	}
	for _, c := range "ILOU" {
		if validID(q.ID[:25] + string(c)) {
			t.Errorf("%c accepted", c)
		}
	}
}

// unreachableStore fails every transaction: a request that reaches it is a
// 500, one refused before the store a 404.
type unreachableStore struct{ store.Store }

func (unreachableStore) View(context.Context, func(store.Tx) error) error {
	return errors.New("store reached")
}

func (unreachableStore) InTx(context.Context, func(store.Tx) error) error {
	return errors.New("store reached")
}

func TestMalformedIDsNeverReachTheStore(t *testing.T) {
	f := newFixture(t, func(d *Deps) { d.Store = unreachableStore{d.Store} })
	good := store.NewID()
	routes := []struct{ method, path, body string }{
		{http.MethodGet, jobsPath + "/%s", ""},
		{http.MethodPost, jobsPath + "/%s/cancel", ""},
		{http.MethodGet, approvalsPath + "/%s", ""},
		{http.MethodPost, approvalsPath + "/%s/decision", decisionDoc("approve", "sha256:x")},
	}
	for _, rt := range routes {
		wantError(t, f.do(rt.method, fmt.Sprintf(rt.path, "not-an-id"), rt.body), http.StatusNotFound, "not_found")
		wantError(t, f.do(rt.method, fmt.Sprintf(rt.path, strings.ToLower(good)), rt.body), http.StatusNotFound, "not_found")
		// a well-formed id does reach the store
		wantError(t, f.do(rt.method, fmt.Sprintf(rt.path, good), rt.body), http.StatusInternalServerError, "internal")
	}
	for _, q := range []string{jobsPath + "?cursor=bad", approvalsPath + "?job_id=bad", approvalsPath + "?cursor=bad"} {
		wantError(t, f.do(http.MethodGet, q, ""), http.StatusBadRequest, "invalid_request")
	}
	if strings.Contains(f.logs.String(), "store reached") {
		t.Error("an error text reached the logs")
	}
}

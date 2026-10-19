package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/approvals"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

const approvalsPath = "/agent-api/v1/approvals"

func (s *stack) approval(t *testing.T, user, id string) store.Approval {
	t.Helper()
	var a store.Approval
	if err := s.st.View(bg, func(tx store.Tx) error {
		var err error
		a, err = tx.GetApproval(user, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return a
}

// decideInStore records a decision directly (seeding, not the API).
func (s *stack) decideInStore(t *testing.T, a store.Approval, d store.ApprovalState) {
	t.Helper()
	s.mustTx(t, func(tx store.Tx) error {
		_, err := tx.DecideApproval(a.UserID, a.ID, d, a.UserID, a.ArgsHash)
		return err
	})
}

// settle moves an approved approval through executing to its final state
// with the stored result, like the executor.
func (s *stack) settle(t *testing.T, a store.Approval, to store.ApprovalState, result string) {
	t.Helper()
	s.mustTx(t, func(tx store.Tx) error {
		if _, err := tx.TransitionApproval(a.UserID, a.ID, store.ApprovalApproved, store.ApprovalExecuting, store.ApprovalPatch{}); err != nil {
			return err
		}
		_, err := tx.TransitionApproval(a.UserID, a.ID, store.ApprovalExecuting, to, store.ApprovalPatch{Result: json.RawMessage(result)})
		return err
	})
}

func (s *stack) cancelInStore(t *testing.T, user, jobID string) {
	t.Helper()
	s.mustTx(t, func(tx store.Tx) error { return tx.CancelJob(user, jobID) })
}

type approvalPage struct {
	Approvals  []map[string]any `json:"approvals"`
	NextCursor *string          `json:"next_cursor"`
}

func (s *stack) listApprovals(t *testing.T, login, query string) approvalPage {
	t.Helper()
	rec := s.as(login, http.MethodGet, approvalsPath+query, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list %s = %d %s", query, rec.Code, rec.Body.String())
	}
	var p approvalPage
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func (s *stack) getApproval(t *testing.T, login, id string) map[string]any {
	t.Helper()
	rec := s.as(login, http.MethodGet, approvalsPath+"/"+id, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get %s = %d %s", id, rec.Code, rec.Body.String())
	}
	return decode(t, rec)
}

func decisionDoc(decision, hash string) string {
	return fmt.Sprintf(`{"decision":%q,"args_hash":%q}`, decision, hash)
}

func (s *stack) decide(login, id, body string) *httptest.ResponseRecorder {
	return s.as(login, http.MethodPost, approvalsPath+"/"+id+"/decision", body)
}

var approvalKeys = []string{
	"id", "job_id", "call_id", "tool", "risk", "args", "args_hash", "target_etag", "rationale", "preview", "state",
	"requested_at", "expires_at", "decided_by", "decided_at", "executed_at", "outcome", "job_ended", "needs_verification",
}

const secretArgs = `{"content":"SECRET-ARGS-CONTENT","path":"/notes/SECRET-ARGS-PATH.txt"}`

func TestApprovalListShapeAndScoping(t *testing.T) {
	s := newWorkStack(t)
	c := s.startJob(t, einsteinID, "chat", "w")
	aps := s.requestApprovals(t, c, secretArgs, `{"path":"/b.txt"}`)
	cm := s.startJob(t, marieID, "chat", "m")
	mine := s.requestApprovals(t, cm, `{"path":"/marie.txt"}`)

	p := s.listApprovals(t, "einstein", "")
	if got := ids(p.Approvals); strings.Join(got, ",") != aps[1].ID+","+aps[0].ID || p.NextCursor != nil {
		t.Fatalf("ids = %v, want newest first", got)
	}
	m := p.Approvals[1]
	wantKeys(t, m, approvalKeys...)
	args, _ := json.Marshal(m["args"])
	if string(args) != secretArgs || m["args_hash"] != hashOf(secretArgs) || m["state"] != "pending" || m["job_id"] != c.Job.ID ||
		m["call_id"] != "c0" || m["tool"] != "files.write" || m["risk"] != "write" || m["target_etag"] != `"etag-1"` ||
		m["rationale"] != "the model says so" || m["outcome"] != nil || m["job_ended"] != false ||
		m["needs_verification"] != false || m["decided_by"] != "" || m["decided_at"] != nil || m["executed_at"] != nil ||
		m["requested_at"] == nil || m["expires_at"] == nil {
		t.Errorf("view = %v", m)
	}
	if prev, _ := json.Marshal(m["preview"]); string(prev) != `{"action":"write","paths":["/a.txt"]}` {
		t.Errorf("preview = %s", prev)
	}
	if got := s.getApproval(t, "einstein", aps[0].ID); got["id"] != aps[0].ID {
		t.Errorf("get = %v", got)
	}
	// marie's approval does not exist for einstein
	wantError(t, s.as("einstein", http.MethodGet, approvalsPath+"/"+mine[0].ID, ""), http.StatusNotFound, "not_found")
	wantError(t, s.decide("einstein", mine[0].ID, decisionDoc("approve", mine[0].ArgsHash)), http.StatusNotFound, "not_found")
	if a := s.approval(t, marieID, mine[0].ID); a.State != store.ApprovalPending {
		t.Errorf("marie's approval = %s", a.State)
	}
	if p := s.listApprovals(t, "einstein", "?job_id="+cm.Job.ID+"&state=all"); len(p.Approvals) != 0 {
		t.Errorf("job filter leaks %v", ids(p.Approvals))
	}
	for _, id := range ids(s.listApprovals(t, "marie", "?state=all").Approvals) {
		if id != mine[0].ID {
			t.Errorf("marie sees %s", id)
		}
	}
	if p := s.listApprovals(t, "einstein", "?job_id="+c.Job.ID); len(p.Approvals) != 2 {
		t.Errorf("job filter = %v", ids(p.Approvals))
	}
	for _, q := range []string{"?state=nope", "?state=PENDING", "?job_id=x", "?cursor=x", "?limit=0", "?limit=101", "?state=all&state=pending", "?job_id=" + c.Job.ID + "&job_id=" + c.Job.ID} {
		wantError(t, s.as("einstein", http.MethodGet, approvalsPath+q, ""), http.StatusBadRequest, "invalid_request")
	}
	for _, id := range []string{"x", strings.ToLower(aps[0].ID), aps[0].ID + "0"} {
		wantError(t, s.as("einstein", http.MethodGet, approvalsPath+"/"+id, ""), http.StatusNotFound, "not_found")
		wantError(t, s.decide("einstein", id, decisionDoc("approve", aps[0].ArgsHash)), http.StatusNotFound, "not_found")
	}
}

func TestApprovalListPaginationAndStates(t *testing.T) {
	s := newWorkStack(t)
	c := s.startJob(t, einsteinID, "chat", "w")
	var args []string
	for i := range 120 {
		args = append(args, fmt.Sprintf(`{"path":"/f%d.txt"}`, i))
	}
	aps := s.requestApprovals(t, c, args...)
	s.decideInStore(t, aps[0], store.ApprovalRejected)
	var got []string
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("pagination does not end")
		}
		q := "?limit=50"
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		p := s.listApprovals(t, "einstein", q)
		got = append(got, ids(p.Approvals)...)
		if p.NextCursor == nil {
			break
		}
		cursor = *p.NextCursor
	}
	if len(got) != 119 || got[0] != aps[119].ID || got[118] != aps[1].ID {
		t.Errorf("pending pages: %d ids", len(got))
	}
	if p := s.listApprovals(t, "einstein", "?state=all&limit=100"); len(p.Approvals) != 100 || p.NextCursor == nil {
		t.Errorf("all = %d", len(p.Approvals))
	}
	if p := s.listApprovals(t, "einstein", "?state=rejected"); len(p.Approvals) != 1 || p.Approvals[0]["id"] != aps[0].ID {
		t.Errorf("rejected = %v", ids(p.Approvals))
	}
}

func TestApprovalViewsPerState(t *testing.T) {
	s := newWorkStack(t)
	// job2: pending approval settled by the scheduler after the job ended
	c2 := s.startJob(t, einsteinID, "chat", "ended")
	g2s := s.requestApprovals(t, c2, `{"path":"/g.txt"}`, `{"path":"/g2.txt"}`)
	g, g2 := g2s[0], g2s[1]
	s.decideInStore(t, g2, store.ApprovalApproved) // approved, never run
	s.cancelInStore(t, einsteinID, c2.Job.ID)
	s.mustTx(t, func(tx store.Tx) error { _, err := tx.ExpireEndedJobApprovals(100); return err })

	// job1: one approval per state
	c1 := s.startJob(t, einsteinID, "chat", "states")
	aps := s.requestApprovals(t, c1, `{"n":"a"}`, `{"n":"b"}`, `{"n":"c"}`, `{"n":"d"}`, `{"n":"e"}`, `{"n":"f"}`)
	a, b, cc, d, e, f := aps[0], aps[1], aps[2], aps[3], aps[4], aps[5]
	for _, x := range []store.Approval{a, b, cc, f} {
		s.decideInStore(t, x, store.ApprovalApproved)
	}
	s.decideInStore(t, d, store.ApprovalRejected)
	s.settle(t, b, store.ApprovalExecuted, `{"outcome":"done","result":{"summary":"wrote /b.txt","blocks":[{"kind":"file","text":"SECRET-BLOCK"}],"diff":"+b"}}`)
	s.settle(t, cc, store.ApprovalFailed, `{"outcome":"failed","reason":"the target changed"}`)
	s.settle(t, f, store.ApprovalUnknown, `{"outcome":"unknown","reason":"the outcome of the action is unknown; please verify it"}`)
	s.fake.Advance(2 * time.Hour) // e expires
	s.mustTx(t, func(tx store.Tx) error {
		refs, err := tx.ExpireApprovals()
		for _, r := range refs {
			if _, err := tx.ResumeJobIfSettled(r.UserID, r.JobID); err != nil {
				return err
			}
		}
		return err
	})
	s.cancelInStore(t, einsteinID, c1.Job.ID) // after the expiry: e keeps the expiry reason

	// job3: pending approval of an ended job, not settled yet
	c3 := s.startJob(t, einsteinID, "chat", "ended, unsettled")
	k := s.requestApprovals(t, c3, `{"n":"k"}`)[0]
	s.cancelInStore(t, einsteinID, c3.Job.ID)

	type want struct {
		state     string
		outcome   map[string]any // nil: null
		ended     bool
		verify    bool
		decidedBy string
	}
	o := func(state, summary, reason, diff string) map[string]any {
		return map[string]any{"state": state, "summary": summary, "reason": reason, "diff": diff}
	}
	cases := map[string]want{
		a.ID:  {"approved", nil, false, false, einsteinID},
		b.ID:  {"executed", o("done", "wrote /b.txt", "", "+b"), false, false, einsteinID},
		cc.ID: {"failed", o("failed", "", "the target changed", ""), false, false, einsteinID},
		f.ID:  {"unknown", o("unknown", "", "the outcome of the action is unknown; please verify it", ""), false, true, einsteinID},
		d.ID:  {"rejected", o("rejected", "", "Rejected by the user.", ""), false, false, einsteinID},
		e.ID:  {"expired", o("expired", "", "The approval expired before it was decided.", ""), false, false, ""},
		g.ID:  {"expired", o("expired", "", "The job ended before this was decided.", ""), false, false, ""},
		g2.ID: {"expired", o("expired", "", "The job ended before this was executed.", ""), false, false, einsteinID},
		k.ID:  {"pending", nil, true, false, ""},
	}
	for id, w := range cases {
		m := s.getApproval(t, "einstein", id)
		wantKeys(t, m, approvalKeys...)
		if m["state"] != w.state || m["job_ended"] != w.ended || m["needs_verification"] != w.verify || m["decided_by"] != w.decidedBy {
			t.Errorf("%s: %v", w.state, m)
		}
		got, _ := json.Marshal(m["outcome"])
		exp, _ := json.Marshal(w.outcome)
		if string(got) != string(exp) {
			t.Errorf("%s outcome = %s, want %s", w.state, got, exp)
		}
		if strings.Contains(fmt.Sprint(m), "SECRET-BLOCK") {
			t.Errorf("%s: blocks are shown", w.state)
		}
	}
	if m := s.getApproval(t, "einstein", b.ID); m["executed_at"] == nil || m["decided_at"] == nil {
		t.Errorf("times = %v", m)
	}
	// the list carries the same flags
	p := s.listApprovals(t, "einstein", "")
	if len(p.Approvals) != 1 || p.Approvals[0]["id"] != k.ID || p.Approvals[0]["job_ended"] != true {
		t.Errorf("pending list = %v", p.Approvals)
	}
}

func TestApprovalUnreadableResult(t *testing.T) {
	s := newWorkStack(t)
	c := s.startJob(t, einsteinID, "chat", "w")
	a := s.requestApprovals(t, c, `{"n":1}`)[0]
	s.decideInStore(t, a, store.ApprovalApproved)
	s.settle(t, a, store.ApprovalExecuted, `["SECRET-STRANGE"]`)
	m := s.getApproval(t, "einstein", a.ID)
	out := m["outcome"].(map[string]any)
	if out["state"] != "unknown" || out["reason"] != reasonUnreadable || m["needs_verification"] != true || strings.Contains(fmt.Sprint(m), "SECRET-STRANGE") {
		t.Errorf("outcome = %v", out)
	}
}

func TestApprovalUntrustedFieldsAreCapped(t *testing.T) {
	s := newWorkStack(t)
	c := s.startJob(t, einsteinID, "chat", "w")
	a := s.requestApprovalsWith(t, c, func(a *store.Approval) { a.Rationale = strings.Repeat("ß", 3000) }, `{"n":1}`)[0]
	s.decideInStore(t, a, store.ApprovalApproved)
	res, _ := json.Marshal(map[string]any{
		"outcome": "done",
		"result":  map[string]string{"summary": strings.Repeat("s", 5000), "diff": strings.Repeat("d", 20000)},
		"reason":  strings.Repeat("r", 3000),
	})
	s.settle(t, a, store.ApprovalExecuted, string(res))
	m := s.getApproval(t, "einstein", a.ID)
	if r := m["rationale"].(string); len(r) > 4<<10 || !strings.HasSuffix(r, truncMarker) || !strings.HasPrefix(r, "ßß") {
		t.Errorf("rationale: len %d", len(r))
	}
	out := m["outcome"].(map[string]any)
	for k, limit := range map[string]int{"summary": 4 << 10, "diff": 16 << 10, "reason": 1 << 10} {
		v := out[k].(string)
		if len(v) > limit || !strings.HasSuffix(v, truncMarker) || len(v) < limit-len(truncMarker)-4 {
			t.Errorf("%s: len %d", k, len(v))
		}
	}
}

func TestDecisionApprove(t *testing.T) {
	s := newWorkStack(t)
	c := s.startJob(t, einsteinID, "chat", "w")
	a := s.requestApprovals(t, c, secretArgs)[0]
	w0 := s.wakes()
	rec := s.decide("einstein", a.ID, decisionDoc("approve", a.ArgsHash))
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	wantKeys(t, m, approvalKeys...)
	if m["state"] != "approved" || m["decided_by"] != einsteinID || m["decided_at"] == nil || m["outcome"] != nil {
		t.Errorf("view = %v", m)
	}
	if got := s.approval(t, einsteinID, a.ID); got.State != store.ApprovalApproved || got.DecidedBy != einsteinID {
		t.Errorf("stored = %s %q", got.State, got.DecidedBy)
	}
	if st := s.job(t, einsteinID, c.Job.ID).State; st != store.JobQueued {
		t.Errorf("job = %s", st)
	}
	if s.wakes() != w0+1 {
		t.Errorf("wakes = %d, want %d", s.wakes(), w0+1)
	}
	ev := s.auditKind(t, einsteinID, "approval_decided")
	if len(ev) != 1 || ev[0].Cred != "oidc" || ev[0].Outcome != "approved" || ev[0].Detail != "approval="+a.ID || ev[0].ArgsHash != a.ArgsHash {
		t.Errorf("audit = %+v", ev)
	}
	// the second decision on it
	wantError(t, s.decide("einstein", a.ID, decisionDoc("reject", a.ArgsHash)), http.StatusConflict, "already_decided")
	if s.wakes() != w0+1 || len(s.auditKind(t, einsteinID, "approval_decided")) != 1 {
		t.Error("the double decision changed something")
	}
}

func TestDecisionReject(t *testing.T) {
	s := newWorkStack(t)
	c := s.startJob(t, einsteinID, "chat", "w")
	aps := s.requestApprovals(t, c, `{"n":1}`, `{"n":2}`)
	rec := s.decide("einstein", aps[0].ID, decisionDoc("reject", aps[0].ArgsHash))
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	if m["state"] != "rejected" || m["outcome"].(map[string]any)["reason"] != "Rejected by the user." {
		t.Errorf("view = %v", m)
	}
	// one still pending: the job keeps waiting
	if st := s.job(t, einsteinID, c.Job.ID).State; st != store.JobWaitingApproval {
		t.Errorf("job = %s", st)
	}
	if rec := s.decide("einstein", aps[1].ID, decisionDoc("reject", aps[1].ArgsHash)); rec.Code != http.StatusOK {
		t.Fatalf("%d", rec.Code)
	}
	if st := s.job(t, einsteinID, c.Job.ID).State; st != store.JobQueued {
		t.Errorf("job = %s", st)
	}
}

func TestDecisionStaleHashChangesNothing(t *testing.T) {
	s := newWorkStack(t)
	c := s.startJob(t, einsteinID, "chat", "w")
	a := s.requestApprovals(t, c, `{"n":1}`)[0]
	w0, n0 := s.wakes(), len(s.auditOf(t, einsteinID))
	for _, h := range []string{hashOf(`{"n":2}`), "sha256:", strings.ToUpper(a.ArgsHash), a.ArgsHash + " "} {
		wantError(t, s.decide("einstein", a.ID, decisionDoc("approve", h)), http.StatusConflict, "stale_arguments")
	}
	if got := s.approval(t, einsteinID, a.ID); got.State != store.ApprovalPending || got.DecidedBy != "" {
		t.Errorf("approval = %s", got.State)
	}
	if st := s.job(t, einsteinID, c.Job.ID).State; st != store.JobWaitingApproval {
		t.Errorf("job = %s", st)
	}
	if s.wakes() != w0 || len(s.auditOf(t, einsteinID)) != n0 {
		t.Errorf("wakes %d->%d, audit %d->%d", w0, s.wakes(), n0, len(s.auditOf(t, einsteinID)))
	}
}

func TestDecisionExpired(t *testing.T) {
	s := newWorkStack(t)
	c := s.startJob(t, einsteinID, "chat", "w")
	aps := s.requestApprovals(t, c, `{"n":1}`)
	s.fake.Advance(2 * time.Hour)
	w0 := s.wakes()
	// past its deadline, the scheduler has not run yet
	wantError(t, s.decide("einstein", aps[0].ID, decisionDoc("approve", aps[0].ArgsHash)), http.StatusGone, "expired")
	if got := s.approval(t, einsteinID, aps[0].ID); got.State != store.ApprovalPending {
		t.Errorf("approval = %s", got.State)
	}
	// after the scheduler's expiry
	s.mustTx(t, func(tx store.Tx) error { _, err := tx.ExpireApprovals(); return err })
	wantError(t, s.decide("einstein", aps[0].ID, decisionDoc("approve", aps[0].ArgsHash)), http.StatusGone, "expired")
	if s.wakes() != w0 {
		t.Error("woke after a refused decision")
	}
}

func TestDecisionOnAnEndedJob(t *testing.T) {
	s := newWorkStack(t)
	c := s.startJob(t, einsteinID, "chat", "w")
	a := s.requestApprovals(t, c, `{"n":1}`)[0]
	s.cancelInStore(t, einsteinID, c.Job.ID)
	w0 := s.wakes()
	wantError(t, s.decide("einstein", a.ID, decisionDoc("approve", a.ArgsHash)), http.StatusConflict, "job_ended")
	if got := s.approval(t, einsteinID, a.ID); got.State != store.ApprovalPending || s.wakes() != w0 {
		t.Errorf("approval = %s", got.State)
	}
}

func TestDecisionBodyIsStrict(t *testing.T) {
	s := newWorkStack(t)
	c := s.startJob(t, einsteinID, "chat", "w")
	a := s.requestApprovals(t, c, `{"n":1}`)[0]
	h := a.ArgsHash
	for _, body := range []string{
		"", "{}", "[]", "null",
		decisionDoc("maybe", h), decisionDoc("approved", h), decisionDoc("Approve", h), decisionDoc("", h),
		`{"decision":"approve"}`, `{"args_hash":"` + h + `"}`,
		`{"decision":"approve","args_hash":"` + h + `","x":1}`,
		`{"Decision":"approve","args_hash":"` + h + `"}`,
		`{"decision":"approve","Args_Hash":"` + h + `"}`,
		`{"decision":"approve","decision":"approve","args_hash":"` + h + `"}`,
		`{"decision":null,"args_hash":"` + h + `"}`,
		`{"decision":"approve","args_hash":null}`,
		`{"decision":"approve","args_hash":""}`,
		`{"decision":1,"args_hash":"` + h + `"}`,
		`{"decision":"approve","args_hash":"` + strings.Repeat("a", 300) + `"}`,
	} {
		wantError(t, s.decide("einstein", a.ID, body), http.StatusBadRequest, "invalid_request")
	}
	if got := s.approval(t, einsteinID, a.ID); got.State != store.ApprovalPending {
		t.Errorf("approval = %s", got.State)
	}
}

func bulkBody(decision string, aps ...store.Approval) string {
	var items []string
	for _, a := range aps {
		items = append(items, fmt.Sprintf(`{"id":%q,"args_hash":%q}`, a.ID, a.ArgsHash))
	}
	return fmt.Sprintf(`{"decision":%q,"items":[%s]}`, decision, strings.Join(items, ","))
}

const bulkPath = approvalsPath + "/bulk-decision"

func TestBulkDecision(t *testing.T) {
	s := newWorkStack(t)
	c := s.startJob(t, einsteinID, "chat", "w")
	aps := s.requestApprovals(t, c, `{"n":1}`, `{"n":2}`, `{"n":3}`)
	w0 := s.wakes()
	rec := s.as("einstein", http.MethodPost, bulkPath, bulkBody("approve", aps...))
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Approvals []map[string]any `json:"approvals"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Approvals) != 3 {
		t.Fatalf("%s %v", rec.Body.String(), err)
	}
	for i, m := range body.Approvals {
		wantKeys(t, m, approvalKeys...)
		if m["id"] != aps[i].ID || m["state"] != "approved" || m["decided_by"] != einsteinID {
			t.Errorf("item %d = %v", i, m)
		}
	}
	if st := s.job(t, einsteinID, c.Job.ID).State; st != store.JobQueued {
		t.Errorf("job = %s", st)
	}
	if s.wakes() != w0+1 {
		t.Errorf("wakes = %d, want one", s.wakes()-w0)
	}
	if n := len(s.auditKind(t, einsteinID, "approval_decided")); n != 3 {
		t.Errorf("audit = %d", n)
	}
	wantError(t, s.as("einstein", http.MethodPost, bulkPath, bulkBody("reject", aps[0])), http.StatusConflict, "already_decided")
}

func TestBulkDecisionRefusals(t *testing.T) {
	s := newWorkStack(t)
	c1 := s.startJob(t, einsteinID, "chat", "one")
	var args []string
	for i := range 101 {
		args = append(args, fmt.Sprintf(`{"n":%d}`, i))
	}
	one := s.requestApprovals(t, c1, args...)
	c2 := s.startJob(t, einsteinID, "chat", "two")
	two := s.requestApprovals(t, c2, `{"n":"two"}`)
	cm := s.startJob(t, marieID, "chat", "marie")
	marie := s.requestApprovals(t, cm, `{"n":"marie"}`)
	decided := s.requestApprovalsDecided(t, einsteinID)
	c4 := s.startJob(t, einsteinID, "chat", "half decided")
	half := s.requestApprovals(t, c4, `{"n":"h1"}`, `{"n":"h2"}`)
	s.decideInStore(t, half[0], store.ApprovalRejected)
	halfStale := half[1]
	halfStale.ArgsHash = hashOf("other")
	stale := one[1]
	stale.ArgsHash = hashOf("other")

	w0, n0 := s.wakes(), len(s.auditOf(t, einsteinID))
	cases := []struct {
		name, body string
		status     int
		code       string
	}{
		{"mixed jobs", bulkBody("approve", one[0], two[0]), 400, "invalid_request"},
		{"duplicate ids", bulkBody("approve", one[0], one[0]), 400, "invalid_request"},
		{"101 items", bulkBody("approve", one...), 400, "invalid_request"},
		{"no items", `{"decision":"approve","items":[]}`, 400, "invalid_request"},
		{"null items", `{"decision":"approve","items":null}`, 400, "invalid_request"},
		{"missing items", `{"decision":"approve"}`, 400, "invalid_request"},
		{"maybe", bulkBody("maybe", one[0]), 400, "invalid_request"},
		{"missing decision", `{"items":[{"id":"` + one[0].ID + `","args_hash":"` + one[0].ArgsHash + `"}]}`, 400, "invalid_request"},
		{"extra field", `{"decision":"approve","items":[],"x":1}`, 400, "invalid_request"},
		{"extra item field", `{"decision":"approve","items":[{"id":"` + one[0].ID + `","args_hash":"` + one[0].ArgsHash + `","x":1}]}`, 400, "invalid_request"},
		{"case item field", `{"decision":"approve","items":[{"ID":"` + one[0].ID + `","args_hash":"` + one[0].ArgsHash + `"}]}`, 400, "invalid_request"},
		{"missing hash", `{"decision":"approve","items":[{"id":"` + one[0].ID + `"}]}`, 400, "invalid_request"},
		{"malformed id", `{"decision":"approve","items":[{"id":"x","args_hash":"h"}]}`, 400, "invalid_request"},
		{"empty", "", 400, "invalid_request"},
		{"one stale hash", bulkBody("approve", one[0], stale, one[2]), 409, "stale_arguments"},
		{"another user's", bulkBody("approve", marie[0]), 404, "not_found"},
		{"unknown id", bulkBody("approve", store.Approval{ID: store.NewID(), ArgsHash: "h"}), 404, "not_found"},
		{"one not pending", bulkBody("approve", decided), 409, "already_decided"},
		{"stale before not pending", bulkBody("approve", half[0], halfStale), 409, "stale_arguments"},
	}
	for _, c := range cases {
		rec := s.as("einstein", http.MethodPost, bulkPath, c.body)
		if rec.Code != c.status {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body.String())
			continue
		}
		wantError(t, rec, c.status, c.code)
	}
	for _, a := range append(append(one, two...), marie...) {
		if got := s.approval(t, a.UserID, a.ID); got.State != store.ApprovalPending {
			t.Errorf("%s changed to %s", a.ID, got.State)
		}
	}
	if s.wakes() != w0 || len(s.auditOf(t, einsteinID)) != n0 {
		t.Errorf("wakes %d->%d, audit %d->%d", w0, s.wakes(), n0, len(s.auditOf(t, einsteinID)))
	}
	// 100 items of one job are fine
	if rec := s.as("einstein", http.MethodPost, bulkPath, bulkBody("reject", one[:100]...)); rec.Code != http.StatusOK {
		t.Errorf("100 items = %d %s", rec.Code, rec.Body.String())
	}
}

// requestApprovalsDecided makes a job with one approval that was already
// rejected.
func (s *stack) requestApprovalsDecided(t *testing.T, user string) store.Approval {
	t.Helper()
	c := s.startJob(t, user, "chat", "decided")
	a := s.requestApprovals(t, c, `{"n":"decided"}`)[0]
	s.decideInStore(t, a, store.ApprovalRejected)
	return a
}

func TestBulkDecisionOnAnEndedOrExpiredJobIsAtomic(t *testing.T) {
	s := newWorkStack(t)
	c := s.startJob(t, einsteinID, "chat", "w")
	aps := s.requestApprovals(t, c, `{"n":1}`, `{"n":2}`)
	s.cancelInStore(t, einsteinID, c.Job.ID)
	wantError(t, s.as("einstein", http.MethodPost, bulkPath, bulkBody("approve", aps...)), http.StatusConflict, "job_ended")
	c2 := s.startJob(t, einsteinID, "chat", "w2")
	aps2 := s.requestApprovals(t, c2, `{"n":3}`)
	s.fake.Advance(2 * time.Hour)
	wantError(t, s.as("einstein", http.MethodPost, bulkPath, bulkBody("approve", aps2...)), http.StatusGone, "expired")
	for _, a := range append(aps, aps2...) {
		if got := s.approval(t, einsteinID, a.ID); got.State != store.ApprovalPending {
			t.Errorf("%s = %s", a.ID, got.State)
		}
	}
}

func TestApprovalArgsNeverReachTheLogs(t *testing.T) {
	s := newWorkStack(t)
	c := s.startJob(t, einsteinID, "chat", "w")
	aps := s.requestApprovals(t, c, secretArgs, `{"content":"SECRET-ARGS-TWO"}`)
	s.listApprovals(t, "einstein", "?state=all")
	s.getApproval(t, "einstein", aps[0].ID)
	s.decide("einstein", aps[0].ID, decisionDoc("approve", hashOf("stale")))
	s.decide("einstein", aps[0].ID, `{"decision":"SECRET-ARGS-BODY","args_hash":"x"}`)
	s.decide("einstein", aps[0].ID, decisionDoc("approve", aps[0].ArgsHash))
	s.as("einstein", http.MethodPost, bulkPath, bulkBody("approve", aps[1]))
	s.as("einstein", http.MethodGet, jobsPath+"/"+c.Job.ID, "")
	logs := s.logs.String()
	if logs == "" {
		t.Fatal("no log output captured")
	}
	for _, secret := range []string{"SECRET-ARGS", aps[0].ArgsHash, "the model says so"} {
		if strings.Contains(logs, secret) {
			t.Errorf("logs contain %q", secret)
		}
	}
}

func TestBulkDecisionOnASettledExpiredApproval(t *testing.T) {
	s := newWorkStack(t)
	c := s.startJob(t, einsteinID, "chat", "w")
	aps := s.requestApprovals(t, c, `{"n":1}`)
	s.fake.Advance(2 * time.Hour)
	s.mustTx(t, func(tx store.Tx) error { _, err := tx.ExpireApprovals(); return err })
	wantError(t, s.as("einstein", http.MethodPost, bulkPath, bulkBody("approve", aps...)), http.StatusGone, "expired")
}

// conflictDecider answers like Gate.Decide when the approval stopped being
// pending after the handler's pre-check (a concurrent decision).
type conflictDecider struct{}

func (conflictDecider) Decide(context.Context, string, string, []approvals.DecisionItem) ([]store.Approval, error) {
	return nil, fmt.Errorf("approvals: %w", store.ErrConflict)
}

func TestDecideMapsALateConflictToAlreadyDecided(t *testing.T) {
	f := newFixture(t, func(d *Deps) { d.Gate = conflictDecider{} })
	r := f.req(http.MethodPost, bulkPath, "")
	err := f.srv.decide(r, Principal{UserID: "u", Subject: "u"}, []approvals.DecisionItem{{ApprovalID: store.NewID()}})
	if st, code, _ := mapError(err); st != http.StatusConflict || code != "already_decided" {
		t.Errorf("late conflict = %d %s", st, code)
	}
}

func TestApprovalCursorSurvivesADecision(t *testing.T) {
	s := newWorkStack(t)
	c := s.startJob(t, einsteinID, "chat", "w")
	aps := s.requestApprovals(t, c, `{"n":1}`, `{"n":2}`, `{"n":3}`, `{"n":4}`, `{"n":5}`)
	want := []string{aps[4].ID, aps[3].ID, aps[2].ID, aps[1].ID, aps[0].ID}
	p1 := s.listApprovals(t, "einstein", "?limit=2")
	if strings.Join(ids(p1.Approvals), ",") != strings.Join(want[:2], ",") || *p1.NextCursor != want[1] {
		t.Fatalf("page 1 = %v", ids(p1.Approvals))
	}
	// deciding the last item of page 1 must not hide the remaining ones
	if rec := s.decide("einstein", aps[3].ID, decisionDoc("reject", aps[3].ArgsHash)); rec.Code != http.StatusOK {
		t.Fatalf("decide = %d", rec.Code)
	}
	p2 := s.listApprovals(t, "einstein", "?limit=2&cursor="+*p1.NextCursor)
	if strings.Join(ids(p2.Approvals), ",") != strings.Join(want[2:4], ",") || p2.NextCursor == nil {
		t.Fatalf("page 2 = %v, want %v", ids(p2.Approvals), want[2:4])
	}
	p3 := s.listApprovals(t, "einstein", "?limit=2&cursor="+*p2.NextCursor)
	if strings.Join(ids(p3.Approvals), ",") != want[4] || p3.NextCursor != nil {
		t.Fatalf("page 3 = %v", ids(p3.Approvals))
	}
	// a cursor that is not one of the caller's approvals is invalid
	cm := s.startJob(t, marieID, "chat", "m")
	theirs := s.requestApprovals(t, cm, `{"n":"m"}`)[0]
	for _, cur := range []string{theirs.ID, store.NewID(), c.Job.ID} {
		wantError(t, s.as("einstein", http.MethodGet, approvalsPath+"?cursor="+cur, ""), http.StatusBadRequest, "invalid_request")
	}
}

// bigArgs is a valid arguments document of about n bytes.
func bigArgs(tag string, n int) string {
	return `{"content":"` + tag + strings.Repeat("x", n) + `"}`
}

func TestApprovalPagesStayWithinTheByteBudget(t *testing.T) {
	s := newWorkStack(t)
	c := s.startJob(t, einsteinID, "chat", "w")
	var args []string
	for i := range 5 {
		args = append(args, bigArgs(fmt.Sprint(i), 3<<20))
	}
	aps := s.requestApprovals(t, c, args...)
	var got []string
	var sizes []int
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 6 {
			t.Fatal("pagination does not end")
		}
		q := "?limit=100"
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		rec := s.as("einstein", http.MethodGet, approvalsPath+q, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%d", rec.Code)
		}
		if rec.Body.Len() > maxListBytes+64<<10 {
			t.Errorf("page of %d bytes", rec.Body.Len())
		}
		var p approvalPage
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		got = append(got, ids(p.Approvals)...)
		sizes = append(sizes, len(p.Approvals))
		if p.NextCursor == nil {
			break
		}
		cursor = *p.NextCursor
	}
	if fmt.Sprint(sizes) != "[2 2 1]" || len(got) != 5 || got[0] != aps[4].ID || got[4] != aps[0].ID {
		t.Errorf("pages %v, ids %d", sizes, len(got))
	}
}

func TestAnOversizedApprovalIsAloneInItsPage(t *testing.T) {
	s := newWorkStack(t)
	c := s.startJob(t, einsteinID, "chat", "w")
	aps := s.requestApprovals(t, c, `{"n":1}`, bigArgs("big", maxListBytes+1), `{"n":3}`)
	var sizes []int
	var got []string
	cursor := ""
	for pages := 0; pages < 5; pages++ {
		q := ""
		if cursor != "" {
			q = "?cursor=" + cursor
		}
		p := s.listApprovals(t, "einstein", q)
		sizes = append(sizes, len(p.Approvals))
		got = append(got, ids(p.Approvals)...)
		if p.NextCursor == nil {
			break
		}
		cursor = *p.NextCursor
	}
	if fmt.Sprint(sizes) != "[1 1 1]" || strings.Join(got, ",") != aps[2].ID+","+aps[1].ID+","+aps[0].ID {
		t.Errorf("pages %v", sizes)
	}
	// its args are never cut
	if m := s.getApproval(t, "einstein", aps[1].ID); len(fmt.Sprint(m["args"])) < maxListBytes {
		t.Error("args were truncated")
	}
}

func TestApprovedThenSettledReasonIgnoresTiming(t *testing.T) {
	s := newWorkStack(t)
	c := s.startJob(t, einsteinID, "chat", "w")
	a := s.requestApprovals(t, c, `{"n":1}`)[0]
	s.decideInStore(t, a, store.ApprovalApproved) // no worker runs it
	s.fake.Advance(2 * time.Hour)                 // the deadline passes
	s.cancelInStore(t, einsteinID, c.Job.ID)      // the job ends after the deadline
	s.mustTx(t, func(tx store.Tx) error { _, err := tx.ExpireEndedJobApprovals(100); return err })
	out := s.getApproval(t, "einstein", a.ID)["outcome"].(map[string]any)
	if out["state"] != "expired" || out["reason"] != "The job ended before this was executed." {
		t.Errorf("outcome = %v", out)
	}
}

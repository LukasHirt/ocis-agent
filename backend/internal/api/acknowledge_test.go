package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

func (s *stack) acknowledge(login, id, body string) *httptest.ResponseRecorder {
	return s.as(login, http.MethodPost, approvalsPath+"/"+id+"/acknowledge", body)
}

// settledApprovals requests approvals for one job, approves them all and
// settles each in the given state with the given stored result (approved
// ones stay approved), keyed "state:result".
func (s *stack) settledApprovals(t *testing.T, user string, results map[store.ApprovalState][]string) map[string]store.Approval {
	t.Helper()
	c := s.startJob(t, user, "chat", "settle")
	var args []string
	var states []store.ApprovalState
	var stored []string
	for st, rs := range results {
		for _, r := range rs {
			args = append(args, fmt.Sprintf(`{"n":%d}`, len(args)))
			states = append(states, st)
			stored = append(stored, r)
		}
	}
	aps := s.requestApprovals(t, c, args...)
	out := map[string]store.Approval{}
	for i, a := range aps {
		s.decideInStore(t, a, store.ApprovalApproved)
		if states[i] != store.ApprovalApproved {
			s.settle(t, a, states[i], stored[i])
		}
		out[string(states[i])+":"+stored[i]] = a
	}
	return out
}

func TestAcknowledgeAnUnknownOutcome(t *testing.T) {
	s := newWorkStack(t)
	aps := s.settledApprovals(t, einsteinID, map[store.ApprovalState][]string{
		store.ApprovalUnknown: {`{"outcome":"unknown","reason":"the outcome of the action is unknown; please verify it"}`},
	})
	var a store.Approval
	for _, v := range aps {
		a = v
	}
	w0 := s.wakes()
	if m := s.getApproval(t, "einstein", a.ID); m["needs_verification"] != true || m["acknowledged_at"] != nil || m["acknowledged_by"] != "" {
		t.Fatalf("before = %v", m)
	}
	rec := s.acknowledge("einstein", a.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	wantKeys(t, m, approvalKeys...)
	if m["state"] != "unknown" || m["needs_verification"] != false || m["acknowledged_by"] != einsteinID || m["acknowledged_at"] == nil {
		t.Errorf("view = %v", m)
	}
	if out := m["outcome"].(map[string]any); out["state"] != "unknown" {
		t.Errorf("outcome = %v, the truth stays", out)
	}
	if got := s.approval(t, einsteinID, a.ID); got.State != store.ApprovalUnknown || got.AcknowledgedBy != einsteinID || got.AcknowledgedAt == nil {
		t.Errorf("stored = %+v", got)
	}
	ev := s.auditKind(t, einsteinID, "approval_acknowledged")
	if len(ev) != 1 || ev[0].Detail != "approval="+a.ID || ev[0].Tool != a.Tool || ev[0].Cred != "oidc" || ev[0].ArgsHash != "" {
		t.Errorf("audit = %+v", ev)
	}
	// idempotent: the same view, no second event, {} is fine too
	again := s.acknowledge("einstein", a.ID, "{}")
	if again.Code != http.StatusOK || again.Body.String() != rec.Body.String() {
		t.Errorf("again = %d %s, want %s", again.Code, again.Body.String(), rec.Body.String())
	}
	if len(s.auditKind(t, einsteinID, "approval_acknowledged")) != 1 {
		t.Error("the second acknowledgement was audited")
	}
	if s.wakes() != w0 {
		t.Errorf("wakes = %d, an acknowledgement wakes nothing", s.wakes()-w0)
	}
	if got := s.getApproval(t, "einstein", a.ID); got["needs_verification"] != false || got["acknowledged_by"] != einsteinID {
		t.Errorf("get = %v", got)
	}
}

func TestAcknowledgeAnUnreadableResult(t *testing.T) {
	s := newWorkStack(t)
	aps := s.settledApprovals(t, einsteinID, map[store.ApprovalState][]string{store.ApprovalExecuted: {`["SECRET-STRANGE"]`}})
	a := aps[`executed:["SECRET-STRANGE"]`]
	rec := s.acknowledge("einstein", a.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if m := decode(t, rec); m["state"] != "executed" || m["needs_verification"] != false || strings.Contains(rec.Body.String(), "SECRET-STRANGE") {
		t.Errorf("view = %v", m)
	}
}

func TestAcknowledgeRefusals(t *testing.T) {
	s := newWorkStack(t)
	aps := s.settledApprovals(t, einsteinID, map[store.ApprovalState][]string{
		store.ApprovalApproved: {""},
		store.ApprovalExecuted: {`{"outcome":"done","result":{"summary":"ok"}}`},
		store.ApprovalFailed:   {`{"outcome":"failed","reason":"no"}`},
		store.ApprovalUnknown:  {""},
	})
	c := s.startJob(t, einsteinID, "chat", "pending")
	pending := s.requestApprovals(t, c, `{"n":"p"}`)[0]
	cm := s.startJob(t, marieID, "chat", "marie")
	marie := s.requestApprovals(t, cm, `{"n":"m"}`)[0]
	s.decideInStore(t, marie, store.ApprovalApproved)
	s.settle(t, marie, store.ApprovalUnknown, "")

	for _, a := range []store.Approval{pending, aps["approved:"], aps[`executed:{"outcome":"done","result":{"summary":"ok"}}`], aps[`failed:{"outcome":"failed","reason":"no"}`]} {
		wantError(t, s.acknowledge("einstein", a.ID, ""), http.StatusConflict, "not_acknowledgeable")
		if got := s.approval(t, einsteinID, a.ID); got.AcknowledgedAt != nil {
			t.Errorf("%s acknowledged", got.State)
		}
	}
	// another user's approval does not exist
	wantError(t, s.acknowledge("einstein", marie.ID, ""), http.StatusNotFound, "not_found")
	if got := s.approval(t, marieID, marie.ID); got.AcknowledgedAt != nil {
		t.Error("marie's approval was acknowledged")
	}
	wantError(t, s.acknowledge("einstein", store.NewID(), ""), http.StatusNotFound, "not_found")
	wantError(t, s.acknowledge("einstein", "not-an-id", ""), http.StatusNotFound, "not_found")
	unknown := aps["unknown:"]
	// strict body
	for _, body := range []string{`{"x":1}`, `[]`, `null`, `{"decision":"approve"}`} {
		wantError(t, s.acknowledge("einstein", unknown.ID, body), http.StatusBadRequest, "invalid_request")
	}
	// the Origin rule of every state-changing request
	r := s.req(http.MethodPost, approvalsPath+"/"+unknown.ID+"/acknowledge", "")
	r.Header.Set("Origin", "https://evil.example.com")
	wantError(t, s.serve(r), http.StatusForbidden, "origin_rejected")
	if got := s.approval(t, einsteinID, unknown.ID); got.AcknowledgedAt != nil {
		t.Error("a refused request acknowledged")
	}
	if len(s.auditKind(t, einsteinID, "approval_acknowledged")) != 0 {
		t.Error("a refusal was audited")
	}
}

func TestAcknowledgeAfterAReconcile(t *testing.T) {
	s := newWorkStack(t)
	c := s.startJob(t, einsteinID, "chat", "w")
	aps := s.requestApprovals(t, c, `{"n":1}`, `{"n":2}`)
	for _, a := range aps {
		s.decideInStore(t, a, store.ApprovalApproved)
		s.mustTx(t, func(tx store.Tx) error {
			_, err := tx.TransitionApproval(a.UserID, a.ID, store.ApprovalApproved, store.ApprovalExecuting, store.ApprovalPatch{})
			return err
		})
	}
	// still executing: not settled, nothing to acknowledge
	wantError(t, s.acknowledge("einstein", aps[0].ID, ""), http.StatusConflict, "not_acknowledgeable")
	// a reconcile settles one as done: nothing to verify
	s.mustTx(t, func(tx store.Tx) error {
		_, err := tx.TransitionApproval(einsteinID, aps[0].ID, store.ApprovalExecuting, store.ApprovalExecuted,
			store.ApprovalPatch{Result: []byte(`{"outcome":"done"}`)})
		return err
	})
	wantError(t, s.acknowledge("einstein", aps[0].ID, ""), http.StatusConflict, "not_acknowledgeable")
	// the other as unknown: now it can be acknowledged
	s.mustTx(t, func(tx store.Tx) error {
		_, err := tx.TransitionApproval(einsteinID, aps[1].ID, store.ApprovalExecuting, store.ApprovalUnknown,
			store.ApprovalPatch{Result: []byte(`{"outcome":"unknown"}`)})
		return err
	})
	if rec := s.acknowledge("einstein", aps[1].ID, ""); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestApprovalListVerificationFilter(t *testing.T) {
	s := newWorkStack(t)
	aps := s.settledApprovals(t, einsteinID, map[store.ApprovalState][]string{
		store.ApprovalExecuted: {`{"outcome":"done"}`, `["strange"]`},
		store.ApprovalFailed:   {`{"outcome":"failed"}`},
		store.ApprovalUnknown:  {"", `{"outcome":"unknown"}`},
	})
	cm := s.startJob(t, marieID, "chat", "marie")
	marie := s.requestApprovals(t, cm, `{"n":"m"}`)[0]
	s.decideInStore(t, marie, store.ApprovalApproved)
	s.settle(t, marie, store.ApprovalUnknown, "")

	want := map[string]bool{aps[`executed:["strange"]`].ID: true, aps["unknown:"].ID: true, aps[`unknown:{"outcome":"unknown"}`].ID: true}
	got := s.listApprovals(t, "einstein", "?needs_verification=true")
	if len(got.Approvals) != len(want) || got.NextCursor != nil {
		t.Fatalf("list = %v", ids(got.Approvals))
	}
	for _, m := range got.Approvals {
		if !want[m["id"].(string)] || m["needs_verification"] != true {
			t.Errorf("listed %v", m)
		}
	}
	// paging
	p1 := s.listApprovals(t, "einstein", "?needs_verification=true&limit=2")
	if len(p1.Approvals) != 2 || p1.NextCursor == nil {
		t.Fatalf("page 1 = %v", ids(p1.Approvals))
	}
	p2 := s.listApprovals(t, "einstein", "?needs_verification=true&limit=2&cursor="+*p1.NextCursor)
	if len(p2.Approvals) != 1 || p2.NextCursor != nil || want[p2.Approvals[0]["id"].(string)] != true {
		t.Fatalf("page 2 = %v", ids(p2.Approvals))
	}
	// combined with a state
	if p := s.listApprovals(t, "einstein", "?needs_verification=true&state=executed"); len(p.Approvals) != 1 || p.Approvals[0]["id"] != aps[`executed:["strange"]`].ID {
		t.Errorf("executed = %v", ids(p.Approvals))
	}
	if p := s.listApprovals(t, "einstein", "?needs_verification=true&state=pending"); len(p.Approvals) != 0 {
		t.Errorf("pending = %v", ids(p.Approvals))
	}
	// an acknowledged one leaves the list
	if rec := s.acknowledge("einstein", aps["unknown:"].ID, ""); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if p := s.listApprovals(t, "einstein", "?needs_verification=true"); len(p.Approvals) != 2 {
		t.Errorf("after the acknowledgement = %v", ids(p.Approvals))
	}
	// without the filter the default is unchanged (pending only)
	if p := s.listApprovals(t, "einstein", ""); len(p.Approvals) != 0 {
		t.Errorf("default = %v", ids(p.Approvals))
	}
	if p := s.listApprovals(t, "marie", "?needs_verification=true"); len(p.Approvals) != 1 || p.Approvals[0]["id"] != marie.ID {
		t.Errorf("marie = %v", ids(p.Approvals))
	}
	for _, q := range []string{"false", "1", "", "TRUE", "true&needs_verification=true"} {
		wantError(t, s.as("einstein", http.MethodGet, approvalsPath+"?needs_verification="+q, ""), http.StatusBadRequest, "invalid_request")
	}
}

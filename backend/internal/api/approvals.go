package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/owncloud/ocis-agent/backend/internal/approvals"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// Approvals: list, detail and the user's decisions. Every lookup is scoped
// by the principal's user id. Decisions go through approvals.Gate.Decide
// (one transaction with the audit events and the job's resume) with the
// principal's subject as decidedBy. Nothing here logs arguments, previews,
// rationales or results.

// Fixed outcome reasons of approvals the executor never recorded a result
// for.
const (
	reasonRejected      = "Rejected by the user."
	reasonExpired       = "The approval expired before it was decided."
	reasonJobEnded      = "The job ended before this was decided."
	reasonJobEndedUnrun = "The job ended before this was executed."
	reasonUnreadable    = "The stored outcome is unreadable; please verify the action."
	reasonNoOutcome     = "No outcome was recorded; please verify the action."
)

// maxBulkItems caps one bulk decision.
const maxBulkItems = 100

// maxHashLen bounds an echoed arguments hash ("sha256:" and 64 hex digits).
const maxHashLen = 128

// storedOutcome mirrors the executor's record in Approval.Result
// ({"outcome","result":{"summary","blocks","diff"},"reason"}). Blocks are
// file contents for the model and are never shown here.
type storedOutcome struct {
	Outcome string `json:"outcome"`
	Result  *struct {
		Summary string `json:"summary"`
		Diff    string `json:"diff"`
	} `json:"result"`
	Reason string `json:"reason"`
}

// approvalViewer builds views inside one read transaction, looking up each
// approval's job once, with the light GetJobStatus (never the checkpoint).
type approvalViewer struct {
	tx   store.Tx
	user string
	jobs map[string]*store.JobStatus // nil: the job does not exist
}

func newApprovalViewer(tx store.Tx, user string) *approvalViewer {
	return &approvalViewer{tx: tx, user: user, jobs: map[string]*store.JobStatus{}}
}

func (v *approvalViewer) job(id string) (*store.JobStatus, error) {
	if j, ok := v.jobs[id]; ok {
		return j, nil
	}
	j, err := v.tx.GetJobStatus(v.user, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		v.jobs[id] = nil
		return nil, nil
	case err != nil:
		return nil, err
	}
	v.jobs[id] = &j
	return &j, nil
}

func (v *approvalViewer) view(a store.Approval) (approvalView, error) {
	j, err := v.job(a.JobID)
	if err != nil {
		return approvalView{}, err
	}
	ended := j != nil && j.State.Terminal()
	out := approvalView{
		ID: a.ID, JobID: a.JobID, CallID: a.CallID, Tool: a.Tool, Risk: a.Risk, Args: a.Args, ArgsHash: a.ArgsHash,
		TargetETag: a.TargetETag, Rationale: capText(a.Rationale, maxRationaleView), Preview: a.Preview,
		State: string(a.State), RequestedAt: timeView(a.RequestedAt), ExpiresAt: timeView(a.ExpiresAt),
		DecidedBy: a.DecidedBy, DecidedAt: timePtrView(a.DecidedAt), ExecutedAt: timePtrView(a.ExecutedAt),
		JobEnded: a.State == store.ApprovalPending && ended,
	}
	if len(out.Args) == 0 {
		out.Args = json.RawMessage("null")
	}
	if len(out.Preview) == 0 {
		out.Preview = json.RawMessage("{}")
	}
	out.Outcome = outcomeOf(a, j)
	out.NeedsVerification = a.State == store.ApprovalUnknown || (out.Outcome != nil && out.Outcome.State == "unknown")
	return out, nil
}

// outcomeOf derives the outcome: from the executor's record when there is
// one, else a fixed reason for the settled states; nil while the approval
// is pending, approved or executing.
func outcomeOf(a store.Approval, j *store.JobStatus) *outcomeView {
	if len(a.Result) > 0 {
		var so storedOutcome
		if err := json.Unmarshal(a.Result, &so); err != nil {
			return &outcomeView{State: "unknown", Reason: reasonUnreadable}
		}
		o := &outcomeView{State: so.Outcome, Reason: capText(so.Reason, maxReasonView)}
		switch so.Outcome {
		case "done", "failed", "unknown", "expired":
		default:
			return &outcomeView{State: "unknown", Reason: reasonUnreadable}
		}
		if so.Result != nil {
			o.Summary = capText(so.Result.Summary, maxOutcomeView)
			o.Diff = capText(so.Result.Diff, maxDiffView)
		}
		return o
	}
	switch a.State {
	case store.ApprovalRejected:
		return &outcomeView{State: "rejected", Reason: reasonRejected}
	case store.ApprovalExpired:
		// Expired without a record: by its deadline (ExpireApprovals) or
		// because its job ended first (the scheduler's SettleEnded). A job
		// that ended before the deadline is the second case.
		// An approved one without a record was settled because its job
		// ended before a worker ran it (deadline or not: the executor's own
		// expiry writes a record).
		if a.DecidedAt != nil {
			return &outcomeView{State: "expired", Reason: reasonJobEndedUnrun}
		}
		if j != nil && j.State.Terminal() && j.UpdatedAt.Before(a.ExpiresAt) {
			return &outcomeView{State: "expired", Reason: reasonJobEnded}
		}
		return &outcomeView{State: "expired", Reason: reasonExpired}
	case store.ApprovalExecuted:
		return &outcomeView{State: "done", Reason: reasonNoOutcome}
	case store.ApprovalFailed:
		return &outcomeView{State: "failed", Reason: reasonNoOutcome}
	case store.ApprovalUnknown:
		return &outcomeView{State: "unknown", Reason: reasonNoOutcome}
	}
	return nil
}

// listApprovals is GET /agent-api/v1/approvals?state=&job_id=&limit=&cursor=
// (state default pending, "all" for every state; newest first).
func (s *Server) listApprovals(w http.ResponseWriter, r *http.Request) error {
	p, ok := principal(r.Context())
	if !ok {
		return errNoPrincipal
	}
	q, err := query(r, "state", "job_id", "limit", "cursor")
	if err != nil {
		return err
	}
	state, err := queryValue(q, "state")
	if err != nil {
		return err
	}
	limit, cursor, err := pageParams(q, maxApprovPageLimit)
	if err != nil {
		return err
	}
	// approvals carry args and preview (up to 2 MiB each): the store loads
	// only the rows within the response's byte budget (plus the one that
	// reaches it)
	f := store.ApprovalFilter{Limit: limit + 1, MaxBytes: maxListBytes}
	switch {
	case state == "":
		f.States = []store.ApprovalState{store.ApprovalPending}
	case state == "all":
	case store.ApprovalState(state).Valid():
		f.States = []store.ApprovalState{store.ApprovalState(state)}
	default:
		return errBadQuery
	}
	if f.JobID, err = queryValue(q, "job_id"); err != nil {
		return err
	}
	if f.JobID != "" && !validID(f.JobID) {
		return errBadQuery
	}
	out := approvalPageView{}
	err = s.d.Store.View(r.Context(), func(tx store.Tx) error {
		if cursor != "" {
			a, err := tx.GetApproval(p.UserID, cursor)
			if errors.Is(err, store.ErrNotFound) {
				return errBadQuery
			}
			if err != nil {
				return err
			}
			f.Before = &store.PageKey{At: a.RequestedAt, ID: a.ID}
		}
		list, err := tx.ListApprovals(p.UserID, f)
		if err != nil {
			return err
		}
		vw := newApprovalViewer(tx, p.UserID)
		out.Approvals, out.NextCursor, err = encodePage(list, limit, func(a store.Approval) string { return a.ID },
			func(a store.Approval) (any, error) { return vw.view(a) })
		if err == nil && out.NextCursor == nil && len(list) > 0 && len(list) <= limit && approvalBytes(list) >= maxListBytes {
			// the store's byte budget may have ended the fetch early: more
			// can follow (the next page may then be empty, with a null cursor)
			last := list[len(list)-1].ID
			out.NextCursor = &last
		}
		return err
	})
	if err != nil {
		return err
	}
	writePage(w, "approvals", out.Approvals, out.NextCursor)
	return nil
}

// approvalBytes is what ApprovalFilter.MaxBytes counts of the rows.
func approvalBytes(list []store.Approval) int64 {
	var n int64
	for _, a := range list {
		n += int64(len(a.Args) + len(a.Preview))
	}
	return n
}

// getApproval is GET /agent-api/v1/approvals/{id}.
func (s *Server) getApproval(w http.ResponseWriter, r *http.Request) error {
	p, ok := principal(r.Context())
	if !ok {
		return errNoPrincipal
	}
	id := r.PathValue("id")
	if !validID(id) {
		return errNotFound
	}
	views, err := s.approvalViews(r, p.UserID, []string{id})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, views[0])
	return nil
}

// approvalViews reads the approvals with these ids in one transaction.
func (s *Server) approvalViews(r *http.Request, user string, ids []string) ([]approvalView, error) {
	out := make([]approvalView, 0, len(ids))
	err := s.d.Store.View(r.Context(), func(tx store.Tx) error {
		out = out[:0]
		vw := newApprovalViewer(tx, user)
		for _, id := range ids {
			a, err := tx.GetApproval(user, id)
			if err != nil {
				return err
			}
			v, err := vw.view(a)
			if err != nil {
				return err
			}
			out = append(out, v)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

type decisionBody struct {
	Decision json.RawMessage `json:"decision"`
	ArgsHash json.RawMessage `json:"args_hash"`
}

type bulkDecisionBody struct {
	Decision json.RawMessage `json:"decision"`
	Items    []bulkItem      `json:"items"`
}

type bulkItem struct {
	ID       json.RawMessage `json:"id"`
	ArgsHash json.RawMessage `json:"args_hash"`
}

// parseDecision maps "approve" and "reject" (exactly) to the store states.
func parseDecision(raw json.RawMessage) (store.ApprovalState, error) {
	if len(raw) == 0 {
		return "", errBadJSON
	}
	d, err := jsonString(raw)
	if err != nil {
		return "", err
	}
	switch d {
	case "approve":
		return store.ApprovalApproved, nil
	case "reject":
		return store.ApprovalRejected, nil
	}
	return "", errBadJSON
}

// parseHash reads the echoed arguments hash: a required, non-empty string.
// Whether it is the stored one is checked against the approval.
func parseHash(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", errBadJSON
	}
	h, err := jsonString(raw)
	if err != nil {
		return "", err
	}
	if h == "" || len(h) > maxHashLen {
		return "", errBadJSON
	}
	return h, nil
}

// precheck refuses decisions before Gate.Decide, without writing anything:
// a hash that is not the stored one is stale_arguments (the store would
// report it as ErrInvalid, like other invalid input), an expired approval is
// expired, any other settled one already_decided. A pending approval of an
// ended job is left to Decide (job_ended).
func precheck(a store.Approval, hash string) error {
	if hash != a.ArgsHash {
		return errStaleArguments
	}
	return precheckState(a)
}

// precheckState is the state part of precheck.
func precheckState(a store.Approval) error {
	switch {
	case a.State == store.ApprovalExpired:
		return errExpired
	case a.State != store.ApprovalPending:
		return errAlreadyDecided
	}
	return nil
}

// decide runs Gate.Decide and maps its conflict (no longer pending) to
// already_decided; every other error goes to the mapper (ErrExpired 410,
// ErrJobEnded 409, ErrNotFound 404, ErrInvalid 400).
func (s *Server) decide(r *http.Request, p Principal, items []approvals.DecisionItem) error {
	if _, err := s.d.Gate.Decide(r.Context(), p.UserID, p.Subject, items); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return errAlreadyDecided
		}
		return err
	}
	return nil
}

// decideApproval is POST /agent-api/v1/approvals/{id}/decision with
// {"decision":"approve"|"reject","args_hash":"..."}.
func (s *Server) decideApproval(w http.ResponseWriter, r *http.Request) error {
	p, ok := principal(r.Context())
	if !ok {
		return errNoPrincipal
	}
	id := r.PathValue("id")
	if !validID(id) {
		return errNotFound
	}
	var body decisionBody
	if err := decodeBody(w, r, &body); err != nil {
		return err
	}
	decision, err := parseDecision(body.Decision)
	if err != nil {
		return err
	}
	hash, err := parseHash(body.ArgsHash)
	if err != nil {
		return err
	}
	if err := s.d.Store.View(r.Context(), func(tx store.Tx) error {
		a, err := tx.GetApproval(p.UserID, id)
		if err != nil {
			return err
		}
		return precheck(a, hash)
	}); err != nil {
		return err
	}
	if err := s.decide(r, p, []approvals.DecisionItem{{ApprovalID: id, Decision: decision, ArgsHash: hash}}); err != nil {
		return err
	}
	s.d.Queue.Wake() // the job may be claimable again
	views, err := s.approvalViews(r, p.UserID, []string{id})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, views[0])
	return nil
}

// bulkDecision is POST /agent-api/v1/approvals/bulk-decision with
// {"decision":...,"items":[{"id":...,"args_hash":...}]}: 1..100 distinct
// pending approvals of ONE job, decided together by one Gate.Decide (all or
// nothing) and one wake.
func (s *Server) bulkDecision(w http.ResponseWriter, r *http.Request) error {
	p, ok := principal(r.Context())
	if !ok {
		return errNoPrincipal
	}
	var body bulkDecisionBody
	if err := decodeBody(w, r, &body); err != nil {
		return err
	}
	decision, err := parseDecision(body.Decision)
	if err != nil {
		return err
	}
	if len(body.Items) < 1 || len(body.Items) > maxBulkItems {
		return errBadJSON
	}
	items := make([]approvals.DecisionItem, 0, len(body.Items))
	ids := make([]string, 0, len(body.Items))
	seen := map[string]bool{}
	for _, it := range body.Items {
		if len(it.ID) == 0 {
			return errBadJSON
		}
		id, err := jsonString(it.ID)
		if err != nil {
			return err
		}
		if !validID(id) || seen[id] {
			return errBadJSON
		}
		seen[id] = true
		hash, err := parseHash(it.ArgsHash)
		if err != nil {
			return err
		}
		items = append(items, approvals.DecisionItem{ApprovalID: id, Decision: decision, ArgsHash: hash})
		ids = append(ids, id)
	}
	if err := s.d.Store.View(r.Context(), func(tx store.Tx) error {
		list := make([]store.Approval, 0, len(items))
		for _, it := range items {
			a, err := tx.GetApproval(p.UserID, it.ApprovalID)
			if err != nil {
				return err
			}
			list = append(list, a)
		}
		for _, a := range list {
			if a.JobID != list[0].JobID {
				return errBadJSON
			}
		}
		// every hash before any state, so one stale item is always reported
		for i, a := range list {
			if items[i].ArgsHash != a.ArgsHash {
				return errStaleArguments
			}
		}
		for _, a := range list {
			if err := precheckState(a); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := s.decide(r, p, items); err != nil {
		return err
	}
	s.d.Queue.Wake()
	views, err := s.approvalViews(r, p.UserID, ids)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, approvalsView{Approvals: views})
	return nil
}

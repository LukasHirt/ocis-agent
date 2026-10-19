package api

import (
	"net/http"
	"net/url"
	"unicode/utf8"

	"github.com/owncloud/ocis-agent/backend/internal/agent"
	"github.com/owncloud/ocis-agent/backend/internal/audit"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// Jobs: list, detail and cancel. Every lookup is scoped by the principal's
// user id; another user's job is simply not found.

// Caps of untrusted text in views. A longer value is cut at a rune boundary
// and ends in truncMarker; the result never exceeds the cap.
const (
	maxGoalView      = 4 << 10
	maxSummaryView   = 1 << 10
	maxAnswerView    = 64 << 10
	maxOutcomeView   = 4 << 10
	maxDiffView      = 16 << 10
	maxReasonView    = 1 << 10
	maxRationaleView = 4 << 10
)

const truncMarker = "…[truncated]"

// capText caps s at limit bytes including the marker.
func capText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit - len(truncMarker)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + truncMarker
}

// Pagination. A page is cut from ONE bounded fetch of the newest pageWindow
// rows (the store's ListJobs/ListApprovals maximum): the cursor is the id of
// the last item of the previous page, and the page holds the items after it.
// Known bound: only the newest pageWindow rows (of the filter) are reachable;
// next_cursor is null at the end of the window even if older rows exist. A
// well-formed cursor that is not in the window (another user's id, or a row
// that fell out of it) gives an empty page.
const (
	pageWindow       = 500
	defaultPageLimit = 50
	maxPageLimit     = 100
)

var errBadQuery = newAPIError(http.StatusBadRequest, "invalid_request")

// queryValue returns the single value of key ("" if absent); a repeated
// parameter is refused.
func queryValue(q url.Values, key string) (string, error) {
	v := q[key]
	switch len(v) {
	case 0:
		return "", nil
	case 1:
		return v[0], nil
	}
	return "", errBadQuery
}

// pageParams reads limit (1..maxPageLimit, decimal digits only, default
// defaultPageLimit) and cursor (an id, see validID).
func pageParams(q url.Values) (limit int, cursor string, err error) {
	ls, err := queryValue(q, "limit")
	if err != nil {
		return 0, "", err
	}
	limit = defaultPageLimit
	if ls != "" {
		if len(ls) > 3 {
			return 0, "", errBadQuery
		}
		limit = 0
		for _, c := range ls {
			if c < '0' || c > '9' {
				return 0, "", errBadQuery
			}
			limit = limit*10 + int(c-'0')
		}
		if limit < 1 || limit > maxPageLimit {
			return 0, "", errBadQuery
		}
	}
	if cursor, err = queryValue(q, "cursor"); err != nil {
		return 0, "", err
	}
	if cursor != "" && !validID(cursor) {
		return 0, "", errBadQuery
	}
	return limit, cursor, nil
}

// page returns up to limit items after the one with the cursor id (from the
// start without a cursor) and the next cursor (nil at the end).
func page[T any](items []T, id func(T) string, cursor string, limit int) ([]T, *string) {
	start := 0
	if cursor != "" {
		start = -1
		for i, it := range items {
			if id(it) == cursor {
				start = i + 1
				break
			}
		}
		if start < 0 {
			return nil, nil
		}
	}
	end := min(start+limit, len(items))
	out := items[start:end]
	if end < len(items) && len(out) > 0 {
		next := id(out[len(out)-1])
		return out, &next
	}
	return out, nil
}

// crockford is the ULID alphabet (upper case, without I, L, O and U).
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// validID reports whether s has the shape of a store id (store.NewID: a
// ULID in canonical upper case). Path and query ids of any other shape are
// refused before they reach the store.
func validID(s string) bool {
	if len(s) != 26 || s[0] > '7' {
		return false
	}
	for i := range len(s) {
		ok := false
		for j := range len(crockford) {
			if s[i] == crockford[j] {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

func jobViewOf(j store.Job, steps int) jobView {
	return jobView{
		ID: j.ID, TriggerKind: j.TriggerKind, Goal: capText(j.Goal, maxGoalView), State: string(j.State),
		Attempt: j.Attempt, Runs: j.Runs, CreatedAt: timeView(j.CreatedAt), UpdatedAt: timeView(j.UpdatedAt),
		Error: j.Error, StepsCount: steps,
	}
}

// listJobs is GET /agent-api/v1/jobs?state=&limit=&cursor= (newest first).
func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) error {
	p, ok := principal(r.Context())
	if !ok {
		return errNoPrincipal
	}
	q := r.URL.Query()
	state, err := queryValue(q, "state")
	if err != nil {
		return err
	}
	f := store.JobFilter{Limit: pageWindow}
	if state != "" {
		if !store.JobState(state).Valid() {
			return errBadQuery
		}
		f.States = []store.JobState{store.JobState(state)}
	}
	limit, cursor, err := pageParams(q)
	if err != nil {
		return err
	}
	out := jobPageView{Jobs: []jobView{}}
	err = s.d.Store.View(r.Context(), func(tx store.Tx) error {
		list, err := tx.ListJobs(p.UserID, f)
		if err != nil {
			return err
		}
		items, next := page(list, func(j store.Job) string { return j.ID }, cursor, limit)
		out.NextCursor = next
		for _, j := range items {
			steps, err := tx.ListSteps(p.UserID, j.ID)
			if err != nil {
				return err
			}
			out.Jobs = append(out.Jobs, jobViewOf(j, len(steps)))
		}
		return nil
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// getJob is GET /agent-api/v1/jobs/{id}.
func (s *Server) getJob(w http.ResponseWriter, r *http.Request) error {
	p, ok := principal(r.Context())
	if !ok {
		return errNoPrincipal
	}
	id := r.PathValue("id")
	if !validID(id) {
		return errNotFound
	}
	var v jobDetailView
	err := s.d.Store.View(r.Context(), func(tx store.Tx) error {
		j, err := tx.GetJob(p.UserID, id)
		if err != nil {
			return err
		}
		steps, err := tx.ListSteps(p.UserID, id)
		if err != nil {
			return err
		}
		v = jobDetailView{jobView: jobViewOf(j, len(steps)), Steps: make([]stepView, 0, len(steps))}
		for _, st := range steps {
			v.Steps = append(v.Steps, stepView{
				N: st.N, Kind: st.Kind, Tool: st.Tool, CallID: st.CallID, Status: st.Status,
				TokensIn: st.TokensIn, TokensOut: st.TokensOut, Summary: capText(st.Summary, maxSummaryView),
				ApprovalID: st.ApprovalID, CreatedAt: timeView(st.CreatedAt),
			})
		}
		// a checkpoint that does not parse has no answer to show
		if cp, err := agent.ParseCheckpoint(j.Checkpoint); err == nil {
			v.Answer = capText(cp.Answer, maxAnswerView)
		}
		return nil
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, v)
	return nil
}

// cancelJob is POST /agent-api/v1/jobs/{id}/cancel (empty body or {}): the
// job is cancelled and audited in one transaction; 409 conflict if it has
// already ended. Its approvals are left to the scheduler, which settles the
// approvals of ended jobs; a running worker notices at its next write.
func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) error {
	var body struct{}
	if err := decodeOptionalBody(w, r, &body); err != nil {
		return err
	}
	p, ok := principal(r.Context())
	if !ok {
		return errNoPrincipal
	}
	id := r.PathValue("id")
	if !validID(id) {
		return errNotFound
	}
	var v jobView
	err := s.d.Store.InTx(r.Context(), func(tx store.Tx) error {
		if err := tx.CancelJob(p.UserID, id); err != nil {
			return err
		}
		if _, err := tx.AppendAudit(audit.Event{
			UserID: p.UserID, TS: s.d.Now(), JobID: id, Kind: "job_cancelled", Cred: "oidc", Outcome: "cancelled",
		}); err != nil {
			return err
		}
		j, err := tx.GetJob(p.UserID, id)
		if err != nil {
			return err
		}
		steps, err := tx.ListSteps(p.UserID, id)
		if err != nil {
			return err
		}
		v = jobViewOf(j, len(steps))
		return nil
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, v)
	return nil
}

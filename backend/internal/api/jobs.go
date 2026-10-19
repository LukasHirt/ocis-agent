package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/owncloud/ocis-agent/backend/internal/agent"
	"github.com/owncloud/ocis-agent/backend/internal/audit"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/textcap"
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

const truncMarker = textcap.Marker

// capText caps s at limit bytes including the marker (textcap.Cap: invalid
// UTF-8 is repaired first).
func capText(s string, limit int) string { return textcap.Cap(s, limit) }

// Pagination is keyset pagination in the store's newest-first order: the
// cursor is the id of the last item of the previous page, resolved with the
// caller's own GetJob/GetApproval (an unknown or another user's id is 400)
// into the store's sort key (created_at or requested_at, id). The next page
// holds the rows strictly older than that key, so a cursor row that changed
// state meanwhile does not hide or repeat rows. Each request fetches
// limit+1 rows (the extra row says whether there is a next page).
//
// Byte budget: a page's encoded entries stop at maxListBytes. The entry
// that would exceed it starts the next page (next_cursor is the last
// included item), so nothing is skipped and an entry is never cut; a page
// always has at least one entry, so a single entry larger than the budget
// is returned alone in its page.
const (
	defaultPageLimit   = 50
	maxJobPageLimit    = 200
	maxApprovPageLimit = 100 // approvals carry their arguments (up to 2 MiB each)
)

// maxListBytes is the byte budget of a list page's entries.
const maxListBytes = 8 << 20

var errBadQuery = newAPIError(http.StatusBadRequest, "invalid_request")

// query parses the request's query string strictly: a malformed one (which
// r.URL.Query would drop silently) or a parameter not in allowed is
// errBadQuery.
func query(r *http.Request, allowed ...string) (url.Values, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, errBadQuery
	}
	for k := range q {
		if !slices.Contains(allowed, k) {
			return nil, errBadQuery
		}
	}
	return q, nil
}

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

// pageParams reads limit (1..maxLimit, decimal digits only, default
// defaultPageLimit) and cursor (an id, see validID).
func pageParams(q url.Values, maxLimit int) (limit int, cursor string, err error) {
	if limit, err = limitParam(q, maxLimit); err != nil {
		return 0, "", err
	}
	if cursor, err = queryValue(q, "cursor"); err != nil {
		return 0, "", err
	}
	if cursor != "" && !validID(cursor) {
		return 0, "", errBadQuery
	}
	return limit, cursor, nil
}

// limitParam reads limit (1..maxLimit, decimal digits only, default
// defaultPageLimit).
func limitParam(q url.Values, maxLimit int) (limit int, err error) {
	ls, err := queryValue(q, "limit")
	if err != nil {
		return 0, err
	}
	limit = defaultPageLimit
	if ls != "" {
		if len(ls) > 3 {
			return 0, errBadQuery
		}
		limit = 0
		for _, c := range ls {
			if c < '0' || c > '9' {
				return 0, errBadQuery
			}
			limit = limit*10 + int(c-'0')
		}
		if limit < 1 || limit > maxLimit {
			return 0, errBadQuery
		}
	}
	return limit, nil
}

// encodePage encodes up to limit of rows (fetched with limit+1) within the
// byte budget and returns the entries and the next cursor (nil on the last
// page).
func encodePage[T any](rows []T, limit int, id func(T) string, encode func(T) (any, error)) ([]json.RawMessage, *string, error) {
	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	out := make([]json.RawMessage, 0, len(rows))
	total, last := 0, ""
	for _, r := range rows {
		v, err := encode(r)
		if err != nil {
			return nil, nil, err
		}
		b, err := json.Marshal(v)
		if err != nil {
			return nil, nil, err
		}
		if len(out) > 0 && total+len(b) > maxListBytes {
			more = true
			break
		}
		out = append(out, b)
		total += len(b)
		last = id(r)
	}
	if !more {
		return out, nil, nil
	}
	return out, &last, nil
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
		if strings.IndexByte(crockford, s[i]) < 0 {
			return false
		}
	}
	return true
}

func summaryOf(j store.Job, steps int) store.JobSummary {
	return store.JobSummary{
		ID: j.ID, UserID: j.UserID, TriggerKind: j.TriggerKind, TriggerRef: j.TriggerRef, Goal: j.Goal, State: j.State,
		Attempt: j.Attempt, Runs: j.Runs, NotBefore: j.NotBefore, Error: j.Error, CreatedAt: j.CreatedAt, UpdatedAt: j.UpdatedAt,
		StepCount: steps,
	}
}

func jobViewOf(j store.JobSummary) jobView {
	return jobView{
		ID: j.ID, TriggerKind: j.TriggerKind, Goal: capText(j.Goal, maxGoalView), State: string(j.State),
		Attempt: j.Attempt, Runs: j.Runs, CreatedAt: timeView(j.CreatedAt), UpdatedAt: timeView(j.UpdatedAt),
		Error: j.Error, StepsCount: j.StepCount,
	}
}

// listJobs is GET /agent-api/v1/jobs?state=&limit=&cursor= (newest first;
// summaries only: no checkpoint is loaded).
func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) error {
	p, ok := principal(r.Context())
	if !ok {
		return errNoPrincipal
	}
	q, err := query(r, "state", "limit", "cursor")
	if err != nil {
		return err
	}
	state, err := queryValue(q, "state")
	if err != nil {
		return err
	}
	limit, cursor, err := pageParams(q, maxJobPageLimit)
	if err != nil {
		return err
	}
	f := store.JobFilter{Limit: limit + 1}
	if state != "" {
		if !store.JobState(state).Valid() {
			return errBadQuery
		}
		f.States = []store.JobState{store.JobState(state)}
	}
	out := jobPageView{}
	err = s.d.Store.View(r.Context(), func(tx store.Tx) error {
		if cursor != "" {
			j, err := tx.GetJobStatus(p.UserID, cursor) // not GetJob: no checkpoint
			if errors.Is(err, store.ErrNotFound) {
				return errBadQuery
			}
			if err != nil {
				return err
			}
			f.Before = &store.PageKey{At: j.CreatedAt, ID: cursor}
		}
		list, err := tx.ListJobSummaries(p.UserID, f)
		if err != nil {
			return err
		}
		out.Jobs, out.NextCursor, err = encodePage(list, limit, func(j store.JobSummary) string { return j.ID },
			func(j store.JobSummary) (any, error) { return jobViewOf(j), nil })
		return err
	})
	if err != nil {
		return err
	}
	writePage(w, "jobs", out.Jobs, out.NextCursor)
	return nil
}

// writePage writes {"<field>":[entries...],"next_cursor":...} from the
// already encoded entries, without encoding the (up to 8 MiB) page again
// as writeJSON would. field is a fixed name; next is an id (no escaping
// needed, see validID).
func writePage(w http.ResponseWriter, field string, entries []json.RawMessage, next *string) {
	size := len(field) + 32
	for _, e := range entries {
		size += len(e) + 1
	}
	buf := make([]byte, 0, size+40)
	buf = append(buf, `{"`...)
	buf = append(buf, field...)
	buf = append(buf, `":[`...)
	for i, e := range entries {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, e...)
	}
	buf = append(buf, `],"next_cursor":`...)
	if next == nil {
		buf = append(buf, "null"...)
	} else {
		buf = append(buf, '"')
		buf = append(buf, *next...)
		buf = append(buf, '"')
	}
	buf = append(buf, "}\n"...)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf)
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
		v = jobDetailView{jobView: jobViewOf(summaryOf(j, len(steps))), Steps: make([]stepView, 0, len(steps))}
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
		v = jobViewOf(summaryOf(j, len(steps)))
		return nil
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, v)
	return nil
}

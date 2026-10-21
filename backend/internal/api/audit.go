package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/audit"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/tools"
)

// Audit endpoints. Every read is scoped by the principal's user id.

const (
	maxAuditDetailView  = 2 << 10 // detail in the list view
	maxAuditPageLimit   = 200
	auditFetchPage      = 500  // records read per store call (list scan and export)
	auditScanBudget     = 5000 // records a filtered list request scans at most
	auditExportDeadline = 5 * time.Minute
	auditLimitation     = "removal of the newest records cannot be detected"
	auditBrokenMessage  = "the audit chain is broken"
)

var (
	toolNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}(\.[a-z][a-z0-9_]{0,63}){0,3}$`)
	riskNames  = map[string]bool{}
)

func init() {
	for _, r := range []tools.Risk{tools.RiskRead, tools.RiskWrite, tools.RiskDestructive, tools.RiskOutward} {
		riskNames[r.String()] = true
	}
}

// auditTime formats a record time in UTC with millisecond precision, which
// is what the chain hashes.
func auditTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// parseSeq parses a positive decimal sequence number in canonical form (at
// most 18 digits, no leading zero), so one record has exactly one cursor.
func parseSeq(s string) (int64, bool) {
	if s == "" || len(s) > 18 || s[0] == '0' {
		return 0, false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil && n > 0
}

// A verification holds a database reader for up to its 30 s deadline and the
// reader pool is small (four connections), so at most maxVerifies run in the
// process. An export reads one short page per transaction but streams for
// minutes, so it has its own, larger process cap (maxExports) and never takes
// a verification's slot. Each user runs at most one of each. A refused
// request is 429 with Retry-After heavyRetryAfter.
const (
	heavyVerify     = "verify"
	heavyExport     = "export"
	maxVerifies     = 2
	maxExports      = 8
	heavyRetryAfter = 5 * time.Second
	// exportPageDeadline is the write deadline of each export page,
	// renewed per page: a client that stops reading is cut off after it
	exportPageDeadline = 30 * time.Second
)

type heavyLimiter struct {
	mu    sync.Mutex
	busy  map[string]struct{} // user + "\x00" + kind
	kinds map[string]int      // running per kind
}

func (l *heavyLimiter) acquire(user, kind string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	limit := maxExports
	if kind == heavyVerify {
		limit = maxVerifies
	}
	k := user + "\x00" + kind
	if _, dup := l.busy[k]; dup || l.kinds[kind] >= limit {
		return false
	}
	if l.busy == nil {
		l.busy, l.kinds = map[string]struct{}{}, map[string]int{}
	}
	l.busy[k] = struct{}{}
	l.kinds[kind]++
	return true
}

func (l *heavyLimiter) release(user, kind string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := user + "\x00" + kind
	if _, ok := l.busy[k]; ok {
		delete(l.busy, k)
		l.kinds[kind]--
	}
}

func (l *heavyLimiter) running() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.busy)
}

// refuseHEAD answers HEAD on a GET route that would do heavy work for a
// body nobody reads: 405 with Allow: GET.
func refuseHEAD(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodHead {
		return false
	}
	w.Header().Set("Allow", http.MethodGet)
	writeCode(w, r, http.StatusMethodNotAllowed, "method_not_allowed")
	return true
}

// descCursor prefixes the cursors of an order=desc listing, so a cursor
// works only in the order that issued it.
const descCursor = "d"

// listAudit is GET /agent-api/v1/audit?cursor=&limit=&order=&job_id=&tool=&risk=&kind=
// (no chain fields). order is asc (default, ascending by seq) or desc
// (newest first); kind is one of the kinds the agent records (audit.IsKind).
//
// cursor is the seq of the last record the previous page covered: the next
// page holds records with a greater seq (asc) or a smaller one (desc). A desc
// cursor carries the prefix "d", so a cursor is refused (400) in the other
// order, and must name a record of the caller's own log (400 otherwise). An
// asc cursor may be any seq: one past the end gives an empty last page.
// Without filters that is the last returned event. With filters the handler
// reads pages of 500 and stops after limit+1 matches (the extra one only
// proves there is more) or after 5000 scanned records. In the first case
// next_cursor is the seq of the last returned match; in the second it is the
// seq of the last SCANNED record, so a page may then hold fewer than limit
// events (even none) and still have a next_cursor: keep following it until
// it is null. next_cursor is null only when the end of the user's log was
// reached.
func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) error {
	p, ok := principal(r.Context())
	if !ok {
		return errNoPrincipal
	}
	q, err := query(r, "cursor", "limit", "order", "job_id", "tool", "risk", "kind")
	if err != nil {
		return err
	}
	limit, err := limitParam(q, maxAuditPageLimit)
	if err != nil {
		return err
	}
	order, err := queryValue(q, "order")
	if err != nil {
		return err
	}
	var desc bool
	switch order {
	case "", "asc":
	case "desc":
		desc = true
	default:
		return errBadQuery
	}
	var pos int64 // the cursor's seq; 0: from the start of the order
	cs, err := queryValue(q, "cursor")
	if err != nil {
		return err
	}
	if cs != "" {
		if desc {
			var found bool
			if cs, found = strings.CutPrefix(cs, descCursor); !found {
				return errBadQuery
			}
		}
		if pos, ok = parseSeq(cs); !ok {
			return errBadQuery
		}
	}
	jobID, err := queryValue(q, "job_id")
	if err != nil {
		return err
	}
	if jobID != "" && !validID(jobID) {
		return errBadQuery
	}
	tool, err := queryValue(q, "tool")
	if err != nil {
		return err
	}
	if tool != "" && !toolNameRE.MatchString(tool) {
		return errBadQuery
	}
	risk, err := queryValue(q, "risk")
	if err != nil {
		return err
	}
	if risk != "" && !riskNames[risk] {
		return errBadQuery
	}
	kind, err := queryValue(q, "kind")
	if err != nil {
		return err
	}
	if kind != "" && !audit.IsKind(kind) {
		return errBadQuery
	}
	filtered := jobID != "" || tool != "" || risk != "" || kind != ""

	out := auditPageView{Events: []auditEventView{}}
	var next int64 // 0: no next page
	err = s.d.Store.View(r.Context(), func(tx store.Tx) error {
		if desc && pos > 0 {
			// a desc cursor names a record of the caller's own log (asc
			// cursors are not checked: existing clients probe with seqs
			// past the end, which only yields an empty last page)
			recs, err := tx.ReadAudit(p.UserID, pos-1, 1)
			if err != nil {
				return err
			}
			if len(recs) == 0 || recs[0].Seq != pos {
				return errBadQuery
			}
		}
		read := tx.ReadAudit
		if desc {
			read = tx.ReadAuditBefore
		}
		scanned := 0
		for {
			n := auditFetchPage
			if !filtered {
				n = limit + 1
			} else if rest := auditScanBudget - scanned; rest < n {
				n = rest
			}
			recs, err := read(p.UserID, pos, n)
			if err != nil {
				return err
			}
			for _, rec := range recs {
				scanned++
				pos = rec.Seq
				if (jobID != "" && rec.JobID != jobID) || (tool != "" && rec.Tool != tool) ||
					(risk != "" && rec.Risk != risk) || (kind != "" && rec.Kind != kind) {
					continue
				}
				if len(out.Events) == limit {
					return nil // one more match: there is a next page
				}
				out.Events = append(out.Events, auditEventView{
					Seq: rec.Seq, TS: auditTime(rec.TS), JobID: rec.JobID, Kind: rec.Kind, Tool: rec.Tool, Risk: rec.Risk,
					ArgsHash: rec.ArgsHash, Cred: rec.Cred, Outcome: rec.Outcome, Detail: capText(rec.Detail, maxAuditDetailView),
				})
				next = rec.Seq
			}
			if len(recs) < n {
				next = 0 // the end of the log
				return nil
			}
			if scanned >= auditScanBudget {
				next = pos // the scan budget is spent: continue after the last scanned record
				return nil
			}
		}
	})
	if err != nil {
		return err
	}
	if next > 0 {
		c := strconv.FormatInt(next, 10)
		if desc {
			c = descCursor + c
		}
		out.NextCursor = &c
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// verifyAudit is GET /agent-api/v1/audit/verify (heavyLimiter: one per user,
// maxVerifies in the process; 429 rate_limited with Retry-After otherwise); it runs
// under the request deadline. HEAD is 405. The result counts the events that
// verified and, for a broken chain, names the seq of the first failing one;
// the message is fixed text, never event data.
func (s *Server) verifyAudit(w http.ResponseWriter, r *http.Request) error {
	p, ok := principal(r.Context())
	if !ok {
		return errNoPrincipal
	}
	if refuseHEAD(w, r) {
		return nil
	}
	if _, err := query(r); err != nil {
		return err
	}
	if !s.heavy.acquire(p.UserID, heavyVerify) {
		rateLimited(w, r, heavyRetryAfter)
		return nil
	}
	defer s.heavy.release(p.UserID, heavyVerify)
	var checked int64
	err := s.d.Store.View(r.Context(), func(tx store.Tx) error {
		var err error
		checked, err = tx.VerifyAudit(p.UserID)
		return err
	})
	var ce *audit.ChainError
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, auditVerifyView{OK: true, Limitation: auditLimitation, Checked: checked})
	case errors.Is(err, audit.ErrChainBroken):
		v := auditVerifyView{OK: false, Message: auditBrokenMessage, Limitation: auditLimitation, Checked: checked}
		if errors.As(err, &ce) {
			v.BrokenAt = &ce.Seq
		}
		writeJSON(w, http.StatusOK, v)
	default:
		return err
	}
	return nil
}

// exportAudit is GET /agent-api/v1/audit/export?from=<seq> (heavyLimiter:
// one per user, maxExports in the process; every page must be written
// within exportPageDeadline; HEAD is 405): the user's whole
// chain as application/x-ndjson, one record per line with prev_hash and hash,
// read in pages of 500 (one short read transaction each, so a slow client
// never holds the database). from=<seq> starts at that seq (default 1).
//
// A failure before the first byte is a normal error response. Once the
// response has started the status can no longer change, so any later failure
// (read error, write or flush error, the 5 minute deadline, a client that went
// away) aborts the connection with http.ErrAbortHandler: the client sees a
// broken transfer, never a clean-looking short file.
func (s *Server) exportAudit(w http.ResponseWriter, r *http.Request) error {
	p, ok := principal(r.Context())
	if !ok {
		return errNoPrincipal
	}
	if refuseHEAD(w, r) {
		return nil
	}
	var after int64
	q, err := query(r, "from")
	if err != nil {
		return err
	}
	fs, err := queryValue(q, "from")
	if err != nil {
		return err
	}
	if fs != "" {
		from, ok := parseSeq(fs)
		if !ok {
			return errBadQuery
		}
		after = from - 1
	}
	if !s.heavy.acquire(p.UserID, heavyExport) {
		rateLimited(w, r, heavyRetryAfter)
		return nil
	}
	defer s.heavy.release(p.UserID, heavyExport)
	ctx := r.Context()
	page := func() ([]audit.Record, error) {
		var recs []audit.Record
		err := s.d.Store.View(ctx, func(tx store.Tx) error {
			var err error
			recs, err = tx.ReadAudit(p.UserID, after, auditFetchPage)
			return err
		})
		return recs, err
	}
	recs, err := page()
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(p.UserID))
	h := w.Header()
	h.Set("Content-Type", "application/x-ndjson")
	h.Set("Content-Disposition", `attachment; filename="audit-`+hex.EncodeToString(sum[:4])+`.jsonl"`)
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	for {
		var buf []byte
		for _, rec := range recs {
			line, err := json.Marshal(auditExportView{
				Seq: rec.Seq, TS: auditTime(rec.TS), JobID: rec.JobID, Kind: rec.Kind, Tool: rec.Tool, Risk: rec.Risk,
				ArgsHash: rec.ArgsHash, Cred: rec.Cred, Outcome: rec.Outcome, Detail: rec.Detail,
				PrevHash: rec.PrevHash, Hash: rec.Hash,
			})
			if err != nil {
				panic(http.ErrAbortHandler)
			}
			buf = append(append(buf, line...), '\n')
		}
		// per-page write deadline, renewed for every page (ErrNotSupported
		// without a connection): a client that stops reading is cut off
		_ = rc.SetWriteDeadline(time.Now().Add(s.exportPageDeadline))
		if len(buf) > 0 {
			if _, err := w.Write(buf); err != nil {
				panic(http.ErrAbortHandler)
			}
		}
		if err := rc.Flush(); err != nil {
			panic(http.ErrAbortHandler)
		}
		if len(recs) < auditFetchPage {
			return nil
		}
		after = recs[len(recs)-1].Seq
		if ctx.Err() != nil { // the client went away or the deadline passed
			panic(http.ErrAbortHandler)
		}
		if recs, err = page(); err != nil {
			panic(http.ErrAbortHandler)
		}
	}
}

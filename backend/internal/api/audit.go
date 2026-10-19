package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
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

// parseSeq parses a positive decimal sequence number (at most 18 digits).
func parseSeq(s string) (int64, bool) {
	if s == "" || len(s) > 18 {
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

// verifyLimiter allows one running verification per user.
type verifyLimiter struct {
	mu      sync.Mutex
	running map[string]struct{}
}

func (l *verifyLimiter) acquire(user string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, busy := l.running[user]; busy {
		return false
	}
	if l.running == nil {
		l.running = map[string]struct{}{}
	}
	l.running[user] = struct{}{}
	return true
}

func (l *verifyLimiter) release(user string) {
	l.mu.Lock()
	delete(l.running, user)
	l.mu.Unlock()
}

// listAudit is GET /agent-api/v1/audit?cursor=&limit=&job_id=&tool=&risk=
// (ascending by seq; no chain fields).
//
// cursor is the seq of the last record the previous page covered: the next
// page holds records with a greater seq. Without filters that is the last
// returned event. With filters the handler reads pages of 500 and stops after
// limit+1 matches (the extra one only proves there is more) or after 5000
// scanned records. In the first case next_cursor is the seq of the last
// returned match; in the second it is the seq of the last SCANNED record, so
// a page may then hold fewer than limit events (even none) and still have a
// next_cursor: keep following it until it is null. next_cursor is null only
// when the end of the user's log was reached.
func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) error {
	p, ok := principal(r.Context())
	if !ok {
		return errNoPrincipal
	}
	q, err := query(r, "cursor", "limit", "job_id", "tool", "risk")
	if err != nil {
		return err
	}
	limit, err := limitParam(q, maxAuditPageLimit)
	if err != nil {
		return err
	}
	var after int64
	cs, err := queryValue(q, "cursor")
	if err != nil {
		return err
	}
	if cs != "" {
		if after, ok = parseSeq(cs); !ok {
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
	filtered := jobID != "" || tool != "" || risk != ""

	out := auditPageView{Events: []auditEventView{}}
	var next int64 // 0: no next page
	err = s.d.Store.View(r.Context(), func(tx store.Tx) error {
		scanned := 0
		for {
			n := auditFetchPage
			if !filtered {
				n = limit + 1
			} else if rest := auditScanBudget - scanned; rest < n {
				n = rest
			}
			recs, err := tx.ReadAudit(p.UserID, after, n)
			if err != nil {
				return err
			}
			for _, rec := range recs {
				scanned++
				after = rec.Seq
				if (jobID != "" && rec.JobID != jobID) || (tool != "" && rec.Tool != tool) || (risk != "" && rec.Risk != risk) {
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
				next = after // the scan budget is spent: continue after the last scanned record
				return nil
			}
		}
	})
	if err != nil {
		return err
	}
	if next > 0 {
		c := strconv.FormatInt(next, 10)
		out.NextCursor = &c
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// verifyAudit is GET /agent-api/v1/audit/verify. One verification per user
// runs at a time (429 rate_limited otherwise); it runs under the request
// deadline.
func (s *Server) verifyAudit(w http.ResponseWriter, r *http.Request) error {
	p, ok := principal(r.Context())
	if !ok {
		return errNoPrincipal
	}
	if _, err := query(r); err != nil {
		return err
	}
	if !s.verifying.acquire(p.UserID) {
		writeCode(w, r, http.StatusTooManyRequests, "rate_limited")
		return nil
	}
	defer s.verifying.release(p.UserID)
	err := s.d.Store.View(r.Context(), func(tx store.Tx) error { return tx.VerifyAudit(p.UserID) })
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, auditVerifyView{OK: true, Limitation: auditLimitation})
	case errors.Is(err, audit.ErrChainBroken):
		writeJSON(w, http.StatusOK, auditVerifyView{OK: false, Message: auditBrokenMessage, Limitation: auditLimitation})
	default:
		return err
	}
	return nil
}

// exportAudit is GET /agent-api/v1/audit/export?from=<seq>: the user's whole
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

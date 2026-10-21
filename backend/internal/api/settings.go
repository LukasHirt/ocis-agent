package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/owncloud/ocis-agent/backend/internal/audit"
	"github.com/owncloud/ocis-agent/backend/internal/llm"
	"github.com/owncloud/ocis-agent/backend/internal/memory"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// Pause, resume and the user's settings. Everything is scoped by the
// principal; policy is changed only here and in auth/approvals, never from
// text the model or a file produced.

// Limits of the editable settings.
const (
	maxWatchEntries = 20
	maxWatchPathLen = 1024
	minSweep        = 5 * time.Minute
	maxSweep        = 7 * 24 * time.Hour
	minDailyTokens  = 1000
	settingsMaxBody = 128 << 10 // PUT /settings' body limit
)

// decodeOptionalBody is decodeBody for action endpoints whose body may be
// left out: an empty body is the empty object.
func decodeOptionalBody(w http.ResponseWriter, r *http.Request, dst any) error {
	if r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, bodyLimit(r)))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return errTooLarge
		}
		return errBadJSON
	}
	if len(data) == 0 {
		return nil
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	return decodeBody(w, r, dst)
}

// pause is POST /agent-api/v1/pause (empty body or {}): the user's own
// pause. A stronger pause already in place stays. 200 with the status.
func (s *Server) pause(w http.ResponseWriter, r *http.Request) error {
	var body struct{}
	if err := decodeOptionalBody(w, r, &body); err != nil {
		return err
	}
	p, ok := principal(r.Context())
	if !ok {
		return errNoPrincipal
	}
	err := s.d.Store.InTx(r.Context(), func(tx store.Tx) error {
		pol, err := tx.GetPolicy(p.UserID)
		if err != nil {
			return err
		}
		// A credential or anomaly pause is only re-raised when oCIS answers
		// 401 or the sweep finds a token, so the user's pause must not
		// replace it (a later resume would clear it for good). Keep it and
		// record that the request was seen.
		if pol.Paused && keepsPause(pol.PauseReason) {
			return s.appendAudit(tx, p.UserID, audit.KindAgentPaused, "kept_pause", "reason="+pol.PauseReason)
		}
		changed, err := tx.PausePolicy(p.UserID, store.PauseUser)
		if err != nil || !changed {
			return err
		}
		return s.appendAudit(tx, p.UserID, audit.KindAgentPaused, "paused", "")
	})
	if err != nil {
		return err
	}
	return s.writeStatus(w, r, p)
}

// keepsPause reports the automatic pauses that POST /pause must leave alone.
func keepsPause(reason string) bool {
	switch reason {
	case store.PauseCredentialInvalid, store.PauseCredentialRevoked, store.PauseCredentialExpired, store.PauseTokenAnomaly:
		return true
	}
	return false
}

type resumeBody struct {
	Reason json.RawMessage `json:"reason"`
}

// resume is POST /agent-api/v1/resume with an empty body or
// {"reason": "user" | "token_anomaly"} (default user). Credential problems
// are fixed by enrolling again and dormancy by visiting, so those reasons
// are refused. Resuming a pause with another reason is a no-op.
func (s *Server) resume(w http.ResponseWriter, r *http.Request) error {
	var body resumeBody
	if err := decodeOptionalBody(w, r, &body); err != nil {
		return err
	}
	reason := store.PauseUser
	if len(body.Reason) > 0 {
		var err error
		if reason, err = jsonString(body.Reason); err != nil {
			return err
		}
	}
	if reason != store.PauseUser && reason != store.PauseTokenAnomaly {
		return errBadJSON
	}
	p, ok := principal(r.Context())
	if !ok {
		return errNoPrincipal
	}
	err := s.d.Store.InTx(r.Context(), func(tx store.Tx) error {
		cleared, err := tx.ClearPause(p.UserID, reason)
		if err != nil || !cleared {
			return err
		}
		return s.appendAudit(tx, p.UserID, audit.KindAgentResumed, "resumed", "reason="+reason)
	})
	if err != nil {
		return err
	}
	s.d.Queue.Wake() // queued jobs of the user are claimable again
	return s.writeStatus(w, r, p)
}

func (s *Server) writeStatus(w http.ResponseWriter, r *http.Request, p Principal) error {
	v, err := s.status(r.Context(), p)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, v)
	return nil
}

func (s *Server) appendAudit(tx store.Tx, userID, kind, outcome, detail string) error {
	_, err := tx.AppendAudit(audit.Event{
		UserID: userID, TS: s.d.Now(), Kind: kind, Cred: "oidc", Outcome: outcome, Detail: detail,
	})
	return err
}

// getSettings is GET /agent-api/v1/settings.
func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) error {
	p, ok := principal(r.Context())
	if !ok {
		return errNoPrincipal
	}
	var pol store.Policy
	if err := s.d.Store.View(r.Context(), func(tx store.Tx) error {
		var err error
		pol, err = tx.GetPolicy(p.UserID)
		return err
	}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, s.settingsView(pol))
	return nil
}

func (s *Server) settingsView(pol store.Policy) settingsView {
	mv := s.modelsView(pol).Preferences
	watch := pol.Watch
	if watch == nil {
		watch = []string{}
	}
	return settingsView{
		ProactiveEnabled: pol.ProactiveEnabled,
		Watch:            watch,
		SweepInterval:    pol.SweepInterval.String(),
		QuietHours:       pol.QuietHours,
		DailyTokens:      s.dailyBudget(pol.DailyTokens),
		DailyTokensMax:   s.d.Cfg.UserDailyTokens,
		DormancyDays:     pol.DormancyDays,
		Models:           settingsModels{Chat: mv.Chat, Proactive: mv.Proactive},
	}
}

// settingsBody holds every editable field raw, so an absent field is told
// from a null or invalid one. Fields that are not listed here are unknown
// fields and refused by decodeBody.
type settingsBody struct {
	ProactiveEnabled json.RawMessage `json:"proactive_enabled"`
	Watch            json.RawMessage `json:"watch"`
	SweepInterval    json.RawMessage `json:"sweep_interval"`
	QuietHours       json.RawMessage `json:"quiet_hours"`
	DailyTokens      json.RawMessage `json:"daily_tokens"`
	Models           json.RawMessage `json:"models"`
}

// settingsPatch is a validated settingsBody; nil means "not mentioned".
type settingsPatch struct {
	proactive *bool
	watch     []string
	setWatch  bool
	sweep     *time.Duration
	quiet     *string
	tokens    *int64
	models    map[string]*string // task -> id; nil value clears
}

// putSettings is PUT /agent-api/v1/settings: the mentioned fields replace
// the stored ones, the others (and everything not editable here) stay.
func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) error {
	var body settingsBody
	if err := decodeBody(w, r, &body); err != nil {
		return err
	}
	patch, err := s.parseSettings(body)
	if err != nil {
		return err
	}
	p, ok := principal(r.Context())
	if !ok {
		return errNoPrincipal
	}
	var out store.Policy
	err = s.d.Store.InTx(r.Context(), func(tx store.Tx) error {
		pol, err := tx.GetPolicy(p.UserID)
		if err != nil {
			return err
		}
		changed := patch.apply(&pol)
		out = pol
		if len(changed) == 0 {
			return nil
		}
		if err := tx.PutPolicy(pol); err != nil {
			return err
		}
		if pol.ProactiveEnabled && slices.Contains(changed, "proactive_enabled") {
			// switched on: the first scheduled sweep comes one interval
			// from now, not at once, and the watch poll starts from fresh
			// baselines (a change from before never fires a job)
			c, err := tx.GetCursor(p.UserID)
			if err != nil {
				return err
			}
			at := s.d.Now()
			c.LastSweepAt = &at
			c.WatchETags, c.WatchDirtySince, c.WatchChangedAt = nil, nil, nil
			if err := tx.PutCursor(c); err != nil {
				return err
			}
		}
		if !pol.ProactiveEnabled && slices.Contains(changed, "proactive_enabled") {
			// switched off: its live jobs (queued, requeued, running or
			// waiting for approval) stop, so no proposal comes after "off"
			if err := s.cancelProactiveJobs(tx, p.UserID); err != nil {
				return err
			}
		}
		if !pol.ProactiveEnabled || !slices.Contains(changed, "proactive_enabled") {
			if slices.Contains(changed, "watch") {
				// a pending change may be of a folder no longer watched;
				// the baselines stay (a removed folder's is pruned by the
				// next poll)
				c, err := tx.GetCursor(p.UserID)
				if err != nil {
					return err
				}
				if c.WatchDirtySince != nil || c.WatchChangedAt != nil {
					c.WatchDirtySince, c.WatchChangedAt = nil, nil
					if err := tx.PutCursor(c); err != nil {
						return err
					}
				}
			}
		}
		// names only: values can be paths and are the user's data
		return s.appendAudit(tx, p.UserID, audit.KindPolicyChanged, "changed", strings.Join(changed, ","))
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, s.settingsView(out))
	return nil
}

// cancelProactiveJobs cancels the user's live proactive jobs and audits each
// like a cancel. A running one stops at its worker's next write (and the
// runner cancels one that is claimed again while proactive work is off).
func (s *Server) cancelProactiveJobs(tx store.Tx, userID string) error {
	jobs, err := tx.ListJobs(userID, store.JobFilter{
		States: []store.JobState{store.JobQueued, store.JobRunning, store.JobWaitingApproval}, Limit: 1000,
	})
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if !store.IsProactiveTrigger(j.TriggerKind) {
			continue
		}
		switch err := tx.CancelJob(userID, j.ID); {
		case errors.Is(err, store.ErrConflict): // ended meanwhile
			continue
		case err != nil:
			return err
		}
		if _, err := tx.AppendAudit(audit.Event{
			UserID: userID, TS: s.d.Now(), JobID: j.ID, Kind: audit.KindJobCancelled, Cred: "oidc",
			Outcome: "cancelled", Detail: "proactive_disabled",
		}); err != nil {
			return err
		}
	}
	return nil
}

// apply sets the mentioned fields and returns the sorted names of those
// that changed.
func (sp settingsPatch) apply(pol *store.Policy) []string {
	var changed []string
	if sp.proactive != nil && *sp.proactive != pol.ProactiveEnabled {
		pol.ProactiveEnabled = *sp.proactive
		changed = append(changed, "proactive_enabled")
	}
	if sp.setWatch && !slices.Equal(sp.watch, pol.Watch) {
		pol.Watch = sp.watch
		changed = append(changed, "watch")
	}
	if sp.sweep != nil && *sp.sweep != pol.SweepInterval {
		pol.SweepInterval = *sp.sweep
		changed = append(changed, "sweep_interval")
	}
	if sp.quiet != nil && *sp.quiet != pol.QuietHours {
		pol.QuietHours = *sp.quiet
		changed = append(changed, "quiet_hours")
	}
	if sp.tokens != nil && *sp.tokens != pol.DailyTokens {
		pol.DailyTokens = *sp.tokens
		changed = append(changed, "daily_tokens")
	}
	if len(sp.models) > 0 {
		next := maps.Clone(pol.Models)
		if next == nil {
			next = map[string]string{}
		}
		for task, id := range sp.models {
			if id == nil {
				delete(next, task)
			} else {
				next[task] = *id
			}
		}
		if !maps.Equal(next, pol.Models) {
			pol.Models = next
			changed = append(changed, "models")
		}
	}
	slices.Sort(changed)
	return changed
}

func (s *Server) parseSettings(b settingsBody) (settingsPatch, error) {
	var sp settingsPatch
	n := 0
	if len(b.ProactiveEnabled) > 0 {
		n++
		v, _, err := strictBool(b.ProactiveEnabled)
		if err != nil {
			return sp, err
		}
		sp.proactive = &v
	}
	if len(b.Watch) > 0 {
		n++
		w, err := parseWatch(b.Watch)
		if err != nil {
			return sp, err
		}
		sp.watch, sp.setWatch = w, true
	}
	if len(b.SweepInterval) > 0 {
		n++
		str, err := jsonString(b.SweepInterval)
		if err != nil {
			return sp, err
		}
		d, err := time.ParseDuration(str)
		if err != nil || d < minSweep || d > maxSweep || d%time.Second != 0 {
			return sp, errBadJSON
		}
		sp.sweep = &d
	}
	if len(b.QuietHours) > 0 {
		n++
		str, err := jsonString(b.QuietHours)
		if err != nil || !store.ValidQuietHours(str) {
			return sp, errBadJSON
		}
		sp.quiet = &str
	}
	if string(b.DailyTokens) == "null" {
		// back to following the operator's cap (stored as 0, see
		// store.DailyBudget); never a way to store 0 as a number
		n++
		zero := int64(0)
		sp.tokens = &zero
	} else if len(b.DailyTokens) > 0 {
		n++
		v, ok := parseInteger(b.DailyTokens)
		if !ok || v < minDailyTokens || v > s.d.Cfg.UserDailyTokens {
			return sp, errBadJSON
		}
		sp.tokens = &v
	}
	if len(b.Models) > 0 {
		n++
		m, err := s.parseModels(b.Models)
		if err != nil {
			return sp, err
		}
		if len(m) == 0 {
			return sp, errBadJSON // an empty object would change nothing
		}
		sp.models = m
	}
	if n == 0 {
		return sp, errBadJSON
	}
	return sp, nil
}

// jsonString reads a JSON string; null (which encoding/json would accept as
// the empty string), numbers and the rest are errBadJSON.
func jsonString(raw json.RawMessage) (string, error) {
	var s string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
		return "", errBadJSON
	}
	return s, nil
}

// parseInteger accepts only a plain JSON integer (no fraction or exponent).
func parseInteger(raw json.RawMessage) (int64, bool) {
	s := string(raw)
	if s == "" || s == "-" || strings.TrimLeft(s, "-0123456789") != "" || strings.LastIndex(s, "-") > 0 {
		return 0, false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	return v, err == nil
}

// parseModels reads {"chat": id|null, "proactive": id|null}. The keys are
// checked here, exactly (decodeBody's case check does not look into maps
// and the store would refuse an unknown task only as a 400 of its own).
func (s *Server) parseModels(raw json.RawMessage) (map[string]*string, error) {
	var in map[string]json.RawMessage
	if err := json.Unmarshal(raw, &in); err != nil || in == nil {
		return nil, errBadJSON
	}
	out := make(map[string]*string, len(in))
	for task, v := range in {
		if task != store.ModelTaskChat && task != store.ModelTaskProactive {
			return nil, errBadJSON
		}
		if string(v) == "null" {
			out[task] = nil
			continue
		}
		id, err := jsonString(v)
		if err != nil || !llm.ValidModelID(id) || !s.d.Catalog.Has(id) {
			return nil, errBadJSON
		}
		out[task] = &id
	}
	return out, nil
}

// parseWatch validates and cleans the watch list: absolute, no . or ..
// segments, no control characters, not under the agent's own directory,
// duplicates dropped. The same rules as the tools' path cleaning.
func parseWatch(raw json.RawMessage) ([]string, error) {
	var in []string
	if err := json.Unmarshal(raw, &in); err != nil || in == nil || len(in) > maxWatchEntries {
		return nil, errBadJSON
	}
	out := make([]string, 0, len(in))
	for _, p := range in {
		c, ok := cleanWatchPath(p)
		if !ok {
			return nil, errBadJSON
		}
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out, nil
}

func cleanWatchPath(p string) (string, bool) {
	if p == "" || len(p) > maxWatchPathLen || p[0] != '/' || !utf8.ValidString(p) {
		return "", false
	}
	for _, r := range p {
		if isControlRune(r) {
			return "", false
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return "", false
		}
	}
	c := path.Clean(p)
	if memory.IsReserved(c) {
		return "", false
	}
	return c, true
}

// isControlRune mirrors tools.isControl: C0 and C1 controls, line and
// paragraph separators and bidirectional controls (ZWJ/ZWNJ stay allowed).
func isControlRune(r rune) bool {
	switch {
	case r < 0x20, r >= 0x7f && r <= 0x9f:
		return true
	case r == 0x2028, r == 0x2029, r == 0x200e, r == 0x200f, r == 0x061c:
		return true
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		return true
	}
	return false
}

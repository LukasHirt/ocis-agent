package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/audit"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// labelGrace is how long an open label whose token does not exist remotely is
// kept open. Client requests time out after 30s, but a create that the client
// gave up on may still land at oCIS later; closing the label too early would
// let that token live unseen.
const labelGrace = 5 * time.Minute

// Renewed is the outcome of a successful renewal.
type Renewed struct {
	Label      string // label of the new token
	OldCleaned bool   // every superseded token is gone and no young absent label is still open
}

// Renew replaces the user's app token with a fresh one, using the agent's own
// app token (never the OIDC token): record a new label, create the token,
// verify it, swap it in one transaction, then delete what it replaced. The old
// token is only deleted after the new one is verified and stored, so a
// failure at any step leaves a working token. A 401 means the credential is
// dead (HandleUnauthorized semantics, ErrCredentialInvalid). A failed delete
// after the swap is not an error: Sweep retries it.
func (s *Service) Renew(ctx context.Context, userID string) (Renewed, error) {
	unlock := s.lockUser(userID)
	defer unlock()

	u, _, cur, err := s.loadToken(ctx, userID)
	if err != nil {
		return Renewed{}, err
	}

	// Record the label BEFORE the request: whatever happens to the request, a
	// token carrying this label is one we created.
	label := s.newLabel()
	if err := s.d.Store.InTx(ctx, func(tx store.Tx) error { return tx.AddTokenLabel(userID, label) }); err != nil {
		return Renewed{}, fmt.Errorf("auth: renew: record label: %w", err)
	}

	tok, err := s.d.Client.CreateToken(ctx, cur, s.d.TokenTTL, label)
	if err != nil {
		if errors.Is(err, ocisclient.ErrUnauthorized) {
			// refused outright: nothing was created under this label
			s.markLabelDeleted(ctx, userID, label)
			if herr := s.markUnauthorized(ctx, userID); herr != nil {
				return Renewed{}, herr
			}
			return Renewed{}, fmt.Errorf("%w: %w", ErrCredentialInvalid, err)
		}
		// unknown outcome (a timeout may hide a created token): the label stays
		// open and Sweep decides
		return Renewed{}, fmt.Errorf("auth: renew: create the token: %w", err)
	}
	next := ocisclient.AppToken(u.Name, tok.Value)

	discard := func() {
		cctx, cancel := cleanupContext(ctx)
		defer cancel()
		if _, gone := s.deleteByLabel(cctx, cur, label); gone {
			s.markLabelDeleted(ctx, userID, label)
		}
	}

	if me, err := s.d.Client.Me(ctx, next); err != nil || me.ID != userID {
		discard()
		if err == nil {
			err = errors.New("the new token belongs to a different user")
		}
		return Renewed{}, fmt.Errorf("auth: renew: verify the new token: %w", err)
	}

	sealed, err := s.d.Secrets.Seal(ctx, []byte(tok.Value.Reveal()), s.aad(userID))
	if err != nil {
		discard()
		return Renewed{}, fmt.Errorf("auth: renew: seal the token: %w", err)
	}
	// The label is already recorded: this transaction must not add it again.
	err = s.d.Store.InTx(ctx, func(tx store.Tx) error {
		if err := tx.PutAppToken(store.AppToken{
			UserID: userID, Sealed: sealed, Label: label,
			CreatedAt: tok.CreatedAt, ExpiresAt: tok.ExpiresAt, Status: store.TokenValid,
		}); err != nil {
			return err
		}
		return s.audit(tx, userID, audit.KindTokenRenewed, "app", "ok", label)
	})
	if err != nil {
		discard()
		return Renewed{}, fmt.Errorf("auth: renew: store the token: %w", err)
	}

	res := Renewed{Label: label}
	_, _, res.OldCleaned = s.cleanupOrphans(ctx, userID, next, label)
	return res, nil
}

// cleanupOrphans deletes every recorded, not yet deleted label other than
// current, then confirms by listing. A label with a remote token is deleted
// at once (audit token_deleted). A label without one is closed
// (token_label_closed) only once it is older than labelGrace; younger ones
// stay open because an in-flight create may still land. It returns how many
// tokens it deleted, how many absent labels it closed, and whether no open
// label is left (apart from current).
func (s *Service) cleanupOrphans(ctx context.Context, userID string, cr ocisclient.Creds, current string) (deleted, closed int, clean bool) {
	var labels []store.TokenLabel
	if err := s.d.Store.View(ctx, func(tx store.Tx) error {
		var err error
		labels, err = tx.ListTokenLabels(userID)
		return err
	}); err != nil {
		return 0, 0, false
	}
	open := map[string]store.TokenLabel{}
	for _, l := range labels {
		if l.DeletedAt == nil && l.Label != current {
			open[l.Label] = l
		}
	}
	if len(open) == 0 {
		return 0, 0, true
	}

	cctx, cancel := cleanupContext(ctx)
	defer cancel()
	clean = true
	list, err := s.d.Client.ListTokens(cctx, cr)
	if err != nil {
		return 0, 0, false
	}
	existed := map[string]bool{}
	for _, ti := range list {
		if _, ok := open[ti.Label]; ok {
			existed[ti.Label] = true
			if err := s.d.Client.DeleteToken(cctx, cr, ti.Hash); err != nil {
				clean = false
			}
		}
	}
	list, err = s.d.Client.ListTokens(cctx, cr)
	if err != nil {
		return 0, 0, false
	}
	still := map[string]bool{}
	for _, ti := range list {
		still[ti.Label] = true
	}
	for label, l := range open {
		if still[label] {
			clean = false
			continue
		}
		kind := audit.KindTokenDeleted
		if !existed[label] {
			if s.now().Sub(l.CreatedAt) < labelGrace {
				clean = false
				continue
			}
			kind = audit.KindTokenLabelClosed
		}
		err := s.d.Store.InTx(cctx, func(tx store.Tx) error {
			if err := tx.MarkTokenLabelDeleted(userID, label); err != nil {
				return err
			}
			return s.audit(tx, userID, kind, "app", "ok", label)
		})
		if err != nil {
			clean = false
			continue
		}
		if kind == audit.KindTokenDeleted {
			deleted++
		} else {
			closed++
		}
	}
	return deleted, closed, clean
}

// SweepResult is the outcome of Sweep.
type SweepResult struct {
	Unknown int  // tokens with our prefix that are unrecorded, survive under a closed label, or duplicate the current label
	Deleted int  // orphan tokens deleted and confirmed gone
	Closed  int  // absent labels closed (no token existed under them)
	Clean   bool // no orphan is left open
}

// Sweep checks the user's tokens with the agent's own credentials. A token
// with our label prefix is an anomaly when (1) its label was never recorded,
// (2) its label was recorded and closed (our tokens never survive under a
// closed label), or (3) it is a second or later token under the current
// label. Then the agent pauses (token_anomaly) and the audit log gets the
// COUNT, never a label, because the label may have been chosen by whoever
// created the token. Orphaned open labels (a crash between create and swap,
// a failed delete, a superseded enrolment) are deleted; an open label with no
// token is closed only after labelGrace. Tokens the sidecar cannot attribute
// are never deleted automatically.
func (s *Service) Sweep(ctx context.Context, userID string) (SweepResult, error) {
	unlock := s.lockUser(userID)
	defer unlock()

	_, rec, cr, err := s.loadToken(ctx, userID)
	if err != nil {
		return SweepResult{}, err
	}
	list, err := s.d.Client.ListTokens(ctx, cr)
	if err != nil {
		if errors.Is(err, ocisclient.ErrUnauthorized) {
			if herr := s.markUnauthorized(ctx, userID); herr != nil {
				return SweepResult{}, herr
			}
			return SweepResult{}, fmt.Errorf("%w: %w", ErrCredentialInvalid, err)
		}
		return SweepResult{}, fmt.Errorf("auth: sweep: list tokens: %w", err)
	}

	var labels []store.TokenLabel
	if err := s.d.Store.View(ctx, func(tx store.Tx) error {
		var err error
		labels, err = tx.ListTokenLabels(userID)
		return err
	}); err != nil {
		return SweepResult{}, fmt.Errorf("auth: sweep: list labels: %w", err)
	}
	recorded := map[string]bool{} // label -> closed
	for _, l := range labels {
		recorded[l.Label] = l.DeletedAt != nil
	}
	// A token with our prefix is unknown when its label was never recorded, was
	// recorded and closed (our tokens never survive under a closed label), or
	// is a second or later token under the current label.
	var res SweepResult
	seenCurrent := false
	for _, ti := range list {
		if !strings.HasPrefix(ti.Label, s.LabelPrefix()) {
			continue
		}
		closed, ok := recorded[ti.Label]
		switch {
		case !ok || closed:
			res.Unknown++
		case ti.Label == rec.Label:
			if seenCurrent {
				res.Unknown++
			}
			seenCurrent = true
		}
	}
	if res.Unknown > 0 {
		err := s.d.Store.InTx(ctx, func(tx store.Tx) error {
			changed, err := tx.PausePolicy(userID, store.PauseTokenAnomaly)
			if err != nil || !changed {
				return err
			}
			return s.audit(tx, userID, audit.KindTokenAnomaly, "app", "paused", fmt.Sprintf("unknown=%d", res.Unknown))
		})
		if err != nil {
			return res, fmt.Errorf("auth: sweep: pause: %w", err)
		}
	}

	res.Deleted, res.Closed, res.Clean = s.cleanupOrphans(ctx, userID, cr, rec.Label)
	return res, nil
}

// RenewOutcome is the result of renewing one user's token.
type RenewOutcome struct {
	UserID string
	Result Renewed
	Err    error
}

// DueForRenewal returns the ids of the users whose valid token has used half
// of its lifetime (up to limit), the users RenewDue would renew. A caller that
// renews them one by one with Renew can stop early (the scheduler stops at an
// oCIS outage).
func (s *Service) DueForRenewal(ctx context.Context, limit int) ([]string, error) {
	var due []store.TokenDue
	if err := s.d.Store.View(ctx, func(tx store.Tx) error {
		var err error
		due, err = tx.ListTokensDueForRenewal(s.now(), limit)
		return err
	}); err != nil {
		return nil, fmt.Errorf("auth: list due tokens: %w", err)
	}
	ids := make([]string, len(due))
	for i, d := range due {
		ids[i] = d.UserID
	}
	return ids, nil
}

// RenewDue renews every valid token that has used half of its lifetime (up to
// limit). A token whose renewal fails permanently (401) is marked invalid and
// leaves the due list; transient failures are returned in the outcomes and
// retried on the next call.
func (s *Service) RenewDue(ctx context.Context, limit int) ([]RenewOutcome, error) {
	ids, err := s.DueForRenewal(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]RenewOutcome, 0, len(ids))
	for _, id := range ids {
		res, err := s.Renew(ctx, id)
		out = append(out, RenewOutcome{UserID: id, Result: res, Err: err})
	}
	return out, nil
}

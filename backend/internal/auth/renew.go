package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// Renewed is the outcome of a successful renewal.
type Renewed struct {
	Label      string // label of the new token
	OldCleaned bool   // every superseded token is gone from oCIS
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
		if s.deleteByLabel(cctx, cur, label) {
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
		return s.audit(tx, userID, "token_renewed", "app", "ok", label)
	})
	if err != nil {
		discard()
		return Renewed{}, fmt.Errorf("auth: renew: store the token: %w", err)
	}

	res := Renewed{Label: label}
	_, res.OldCleaned = s.cleanupOrphans(ctx, userID, next, label)
	return res, nil
}

// cleanupOrphans deletes every recorded, not yet deleted label other than
// current, then confirms by listing. Labels that no longer exist remotely are
// marked deleted. It returns how many labels it closed and whether none is
// left open (apart from current).
func (s *Service) cleanupOrphans(ctx context.Context, userID string, cr ocisclient.Creds, current string) (closed int, clean bool) {
	var labels []store.TokenLabel
	if err := s.d.Store.View(ctx, func(tx store.Tx) error {
		var err error
		labels, err = tx.ListTokenLabels(userID)
		return err
	}); err != nil {
		return 0, false
	}
	open := map[string]bool{}
	for _, l := range labels {
		if l.DeletedAt == nil && l.Label != current {
			open[l.Label] = true
		}
	}
	if len(open) == 0 {
		return 0, true
	}

	cctx, cancel := cleanupContext(ctx)
	defer cancel()
	clean = true
	list, err := s.d.Client.ListTokens(cctx, cr)
	if err != nil {
		return 0, false
	}
	for _, ti := range list {
		if open[ti.Label] {
			if err := s.d.Client.DeleteToken(cctx, cr, ti.Hash); err != nil {
				clean = false
			}
		}
	}
	list, err = s.d.Client.ListTokens(cctx, cr)
	if err != nil {
		return 0, false
	}
	still := map[string]bool{}
	for _, ti := range list {
		still[ti.Label] = true
	}
	for label := range open {
		if still[label] {
			clean = false
			continue
		}
		err := s.d.Store.InTx(cctx, func(tx store.Tx) error {
			if err := tx.MarkTokenLabelDeleted(userID, label); err != nil {
				return err
			}
			return s.audit(tx, userID, "token_deleted", "app", "ok", label)
		})
		if err != nil {
			clean = false
			continue
		}
		closed++
	}
	return closed, clean
}

// SweepResult is the outcome of Sweep.
type SweepResult struct {
	Unknown int  // tokens with our prefix that we never recorded
	Deleted int  // orphaned labels closed
	Clean   bool // no orphan is left open
}

// Sweep checks the user's tokens with the agent's own credentials. A token
// with our label prefix that the sidecar never recorded is an anomaly: the
// agent pauses (token_anomaly) and the audit log gets the COUNT, never the
// label, because the label was chosen by whoever created the token. Orphaned
// labels (a crash between create and swap, a failed delete, a superseded
// enrolment) are deleted. Tokens the sidecar cannot attribute are never
// deleted automatically.
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
	recorded := map[string]bool{}
	for _, l := range labels {
		recorded[l.Label] = true
	}
	var res SweepResult
	for _, ti := range list {
		if strings.HasPrefix(ti.Label, s.LabelPrefix()) && !recorded[ti.Label] {
			res.Unknown++
		}
	}
	if res.Unknown > 0 {
		err := s.d.Store.InTx(ctx, func(tx store.Tx) error {
			changed, err := tx.PausePolicy(userID, store.PauseTokenAnomaly)
			if err != nil || !changed {
				return err
			}
			return s.audit(tx, userID, "token_anomaly", "app", "paused", fmt.Sprintf("unknown=%d", res.Unknown))
		})
		if err != nil {
			return res, fmt.Errorf("auth: sweep: pause: %w", err)
		}
	}

	res.Deleted, res.Clean = s.cleanupOrphans(ctx, userID, cr, rec.Label)
	return res, nil
}

// RenewOutcome is the result of renewing one user's token.
type RenewOutcome struct {
	UserID string
	Result Renewed
	Err    error
}

// RenewDue renews every valid token that has used half of its lifetime (up to
// limit). A token whose renewal fails permanently (401) is marked invalid and
// leaves the due list; transient failures are returned in the outcomes and
// retried on the next call.
func (s *Service) RenewDue(ctx context.Context, limit int) ([]RenewOutcome, error) {
	var due []store.TokenDue
	if err := s.d.Store.View(ctx, func(tx store.Tx) error {
		var err error
		due, err = tx.ListTokensDueForRenewal(s.now(), limit)
		return err
	}); err != nil {
		return nil, fmt.Errorf("auth: list due tokens: %w", err)
	}
	out := make([]RenewOutcome, 0, len(due))
	for _, d := range due {
		res, err := s.Renew(ctx, d.UserID)
		out = append(out, RenewOutcome{UserID: d.UserID, Result: res, Err: err})
	}
	return out, nil
}

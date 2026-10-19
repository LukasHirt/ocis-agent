package auth

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/memory"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// TokenSummary describes one of the user's app tokens for a confirmation
// dialog. It never carries a hash or a value.
type TokenSummary struct {
	Label     string
	CreatedAt time.Time
	ExpiresAt time.Time
	Ours      bool // the label carries this sidecar's prefix
}

// PreviewRevokeAll lists all of the caller's app tokens, oldest first.
func (s *Service) PreviewRevokeAll(ctx context.Context, bearer ocisclient.Secret) ([]TokenSummary, error) {
	list, err := s.d.Client.ListTokens(ctx, ocisclient.Bearer(bearer))
	if err != nil {
		return nil, fmt.Errorf("auth: list tokens: %w", err)
	}
	out := make([]TokenSummary, 0, len(list))
	for _, ti := range list {
		out = append(out, TokenSummary{
			Label: ti.Label, CreatedAt: ti.CreatedAt, ExpiresAt: ti.ExpiresAt,
			Ours: strings.HasPrefix(ti.Label, s.LabelPrefix()),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// Revoke deletes every token whose label carries this instance's prefix (a
// renewal chain can leave several), wipes the stored token and pauses the
// agent with credential_revoked. It needs the user's OIDC bearer. It returns
// the number of tokens it deleted.
func (s *Service) Revoke(ctx context.Context, userID string, bearer ocisclient.Secret) (int, error) {
	unlock := s.lockUser(userID)
	defer unlock()
	prefix := s.LabelPrefix()
	return s.revokeLocked(ctx, userID, bearer, "token_deleted",
		func(label string) bool { return strings.HasPrefix(label, prefix) })
}

// RevokeAll is the incident revoke: it deletes ALL of the user's app tokens,
// including the user's own app passwords for other clients, because a stolen
// token's holder can give its children any label. It does nothing at all
// (no request) unless confirm is true.
func (s *Service) RevokeAll(ctx context.Context, userID string, bearer ocisclient.Secret, confirm bool) (int, error) {
	if !confirm {
		return 0, ErrNotConfirmed
	}
	unlock := s.lockUser(userID)
	defer unlock()
	return s.revokeLocked(ctx, userID, bearer, "token_revoked_all", func(string) bool { return true })
}

// revokeLocked deletes the matching tokens with the OIDC bearer, confirms by
// listing again, and only then changes local state. The caller holds the user
// lock.
func (s *Service) revokeLocked(ctx context.Context, userID string, bearer ocisclient.Secret, kind string, match func(label string) bool) (int, error) {
	if bearer == "" {
		return 0, ocisclient.ErrNoCredentials
	}
	oidc := ocisclient.Bearer(bearer)
	// The caller derives userID from the bearer, but the pairing is checked
	// here too: a wrong pair must delete nothing.
	me, err := s.d.Client.Me(ctx, oidc)
	if err != nil {
		return 0, fmt.Errorf("auth: revoke: verify the bearer: %w", err)
	}
	if me.ID != userID {
		return 0, errors.New("auth: the bearer token does not belong to this user")
	}
	list, err := s.d.Client.ListTokens(ctx, oidc)
	if err != nil {
		return 0, fmt.Errorf("auth: revoke: list tokens: %w", err)
	}
	deleted := 0
	for _, ti := range list {
		if !match(ti.Label) {
			continue
		}
		if err := s.d.Client.DeleteToken(ctx, oidc, ti.Hash); err != nil {
			return deleted, fmt.Errorf("auth: revoke: delete a token: %w", err)
		}
		deleted++
	}
	list, err = s.d.Client.ListTokens(ctx, oidc)
	if err != nil {
		return deleted, fmt.Errorf("auth: revoke: confirm: %w", err)
	}
	for _, ti := range list {
		if match(ti.Label) {
			return deleted, ErrRevokeIncomplete
		}
	}

	// The remote deletes are done: a cancelled request must not skip the
	// local update and audit.
	cctx, cancel := cleanupContext(ctx)
	defer cancel()
	detail := fmt.Sprintf("tokens=%d", deleted)
	err = s.d.Store.InTx(cctx, func(tx store.Tx) error {
		if _, err := tx.GetUser(userID); errors.Is(err, store.ErrNotFound) {
			// never enrolled here: no local state to change; an action that
			// deleted tokens is still audited, an empty one leaves no trace
			if deleted == 0 {
				return nil
			}
			return s.audit(tx, userID, kind, "oidc", "ok", detail)
		} else if err != nil {
			return err
		}
		labels, err := tx.ListTokenLabels(userID)
		if err != nil {
			return err
		}
		for _, l := range labels {
			if l.DeletedAt == nil {
				if err := tx.MarkTokenLabelDeleted(userID, l.Label); err != nil {
					return err
				}
			}
		}
		if err := tx.WipeAppToken(userID); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		changed, err := tx.PausePolicy(userID, store.PauseCredentialRevoked)
		if err != nil {
			return err
		}
		outcome := "paused"
		if !changed {
			outcome = "kept_pause" // a stronger pause was kept
		}
		return s.audit(tx, userID, kind, "oidc", outcome, detail)
	})
	if err != nil {
		return deleted, fmt.Errorf("auth: revoke: update local state: %w", err)
	}
	return deleted, nil
}

// Unenroll revokes the agent's tokens, optionally deletes the memory folder
// (with the user's bearer), then audits `unenrolled` and deletes the user's
// rows (policy, token, labels, jobs). Audit records are kept. A failure
// before the final transaction leaves the user in place so the call can be
// repeated.
func (s *Service) Unenroll(ctx context.Context, userID string, bearer ocisclient.Secret, deleteMemory bool) error {
	unlock := s.lockUser(userID)
	defer unlock()

	var u store.User
	if err := s.d.Store.View(ctx, func(tx store.Tx) error {
		var err error
		u, err = tx.GetUser(userID)
		return err
	}); err != nil {
		return fmt.Errorf("auth: unenroll: %w", err)
	}
	prefix := s.LabelPrefix()
	if _, err := s.revokeLocked(ctx, userID, bearer, "token_deleted",
		func(label string) bool { return strings.HasPrefix(label, prefix) }); err != nil {
		return err
	}
	if deleteMemory {
		sess := memory.Session{UserID: userID, DriveID: u.PersonalDriveID, Creds: ocisclient.Bearer(bearer)}
		if err := s.d.Memory.Purge(ctx, sess); err != nil {
			return fmt.Errorf("auth: unenroll: %w", err)
		}
	}
	if err := s.d.Store.InTx(ctx, func(tx store.Tx) error {
		if err := s.audit(tx, userID, "unenrolled", "oidc", "ok", ""); err != nil {
			return err
		}
		return tx.DeleteUser(userID)
	}); err != nil {
		return fmt.Errorf("auth: unenroll: %w", err)
	}
	return nil
}

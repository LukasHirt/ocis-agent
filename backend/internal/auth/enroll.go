package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/memory"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// cleanupContext returns a context for cleanup work that survives the
// cancellation of the request that failed.
func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
}

// deleteByLabel deletes the token with exactly this label (by its listed
// hash) and confirms by listing again. oCIS answers 500 for a hash it no
// longer knows, so the second listing is the only reliable proof. found says
// a token with the label was listed at the first listing; gone says no token
// with the label remains after the confirming listing (false on any error).
func (s *Service) deleteByLabel(ctx context.Context, cr ocisclient.Creds, label string) (found, gone bool) {
	list, err := s.d.Client.ListTokens(ctx, cr)
	if err != nil {
		return false, false
	}
	for _, ti := range list {
		if ti.Label == label {
			found = true
			if err := s.d.Client.DeleteToken(ctx, cr, ti.Hash); err != nil {
				return found, false
			}
		}
	}
	list, err = s.d.Client.ListTokens(ctx, cr)
	if err != nil {
		return found, false
	}
	for _, ti := range list {
		if ti.Label == label {
			return found, false
		}
	}
	return found, true
}

// markLabelDeleted records that the token with this label is gone. Best
// effort: a failure leaves the label open, and the next Sweep retries.
func (s *Service) markLabelDeleted(ctx context.Context, userID, label string) {
	cctx, cancel := cleanupContext(ctx)
	defer cancel()
	_ = s.d.Store.InTx(cctx, func(tx store.Tx) error {
		err := tx.MarkTokenLabelDeleted(userID, label)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	})
}

// Enroll creates the agent's app token for the user and prepares their memory
// folder. id must come from a validated bearer token; bearer is that token
// (enrolment is an action of the user, so it never uses an app token).
func (s *Service) Enroll(ctx context.Context, id Identity, bearer ocisclient.Secret) error {
	if id.UserID == "" || id.Username == "" || id.DriveID == "" || bearer == "" {
		return errors.New("auth: enrolment needs a user id, a login name, a personal drive and a bearer token")
	}
	unlock := s.lockUser(id.UserID)
	defer unlock()

	oidc := ocisclient.Bearer(bearer)
	label := s.newLabel()

	// 1. Record the user and the label BEFORE asking oCIS for a token, so that
	// whatever happens next, every token we create has a recorded label.
	var first bool
	err := s.d.Store.InTx(ctx, func(tx store.Tx) error {
		_, err := tx.GetUser(id.UserID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			first = true
		case err != nil:
			return err
		}
		if err := tx.PutUser(store.User{ID: id.UserID, Name: id.Username, PersonalDriveID: id.DriveID}); err != nil {
			return err
		}
		if first && s.d.DormancyDays > 0 {
			p, err := tx.GetPolicy(id.UserID)
			if err != nil {
				return err
			}
			p.DormancyDays = s.d.DormancyDays
			if err := tx.PutPolicy(p); err != nil {
				return err
			}
		}
		if err := tx.TouchUser(id.UserID, s.now()); err != nil {
			return err
		}
		return tx.AddTokenLabel(id.UserID, label)
	})
	if err != nil {
		return fmt.Errorf("auth: record enrolment: %w", err)
	}

	// abandon undoes step 1 after a failure. refused means oCIS plainly
	// refused the create (nothing exists). uncertain means the create failed in
	// a way that does not prove no token was made (timeout, 5xx, lost
	// response): the request may still land at oCIS after we looked. Then the
	// label is closed only if a token was actually found and deleted; if
	// nothing was listed, the label stays open (and so does the user row) so
	// that Sweep's grace path deletes a token that lands later. Otherwise
	// (the token is known to exist) it is deleted and the label closed once
	// confirmed gone.
	abandon := func(refused, uncertain bool) {
		cctx, cancel := cleanupContext(ctx)
		defer cancel()
		if !refused {
			found, gone := s.deleteByLabel(cctx, oidc, label)
			if !gone || (uncertain && !found) {
				return // keep the label open: a later Sweep deletes the token
			}
		}
		s.markLabelDeleted(ctx, id.UserID, label)
		if first {
			// When cleanup cannot be confirmed (deleteByLabel false above, or a
			// non-NotFound store error here) the label stays open and the user
			// row stays, so a later Sweep deletes the token and attributes it.
			// A later Enroll then sees first=false and does not apply
			// Deps.DormancyDays (accepted trade-off).
			_ = s.d.Store.InTx(cctx, func(tx store.Tx) error {
				_, err := tx.GetAppToken(id.UserID)
				switch {
				case err == nil:
					return nil // a concurrent path stored a token: keep the user
				case !errors.Is(err, store.ErrNotFound):
					return err
				}
				return tx.DeleteUser(id.UserID)
			})
		}
	}

	// 2. Create the token with the user's own credential.
	tok, err := s.d.Client.CreateToken(ctx, oidc, s.d.TokenTTL, label)
	if err != nil {
		// An error does not prove that no token was made (lost response,
		// timeout, 5xx after creation): ask oCIS, unless it plainly refused.
		refused := errors.Is(err, ocisclient.ErrUnauthorized) || errors.Is(err, ocisclient.ErrForbidden)
		abandon(refused, !refused)
		return fmt.Errorf("auth: create the app token: %w", err)
	}
	app := ocisclient.AppToken(id.Username, tok.Value)

	// 3. The token must work and belong to this user.
	me, err := s.d.Client.Me(ctx, app)
	if err != nil {
		abandon(false, false)
		return fmt.Errorf("auth: verify the new token: %w", err)
	}
	if me.ID != id.UserID {
		abandon(false, false)
		return errors.New("auth: the new token belongs to a different user")
	}

	// 4. The memory folder, with the new credential.
	if err := s.d.Memory.Ensure(ctx, memory.Session{UserID: id.UserID, DriveID: id.DriveID, Creds: app}); err != nil {
		abandon(false, false)
		return fmt.Errorf("auth: prepare the memory folder: %w", err)
	}

	// 5. Seal outside the transaction, then commit everything at once.
	sealed, err := s.d.Secrets.Seal(ctx, []byte(tok.Value.Reveal()), s.aad(id.UserID))
	if err != nil {
		abandon(false, false)
		return fmt.Errorf("auth: seal the token: %w", err)
	}
	err = s.d.Store.InTx(ctx, func(tx store.Tx) error {
		if err := tx.PutAppToken(store.AppToken{
			UserID: id.UserID, Sealed: sealed, Label: label,
			CreatedAt: tok.CreatedAt, ExpiresAt: tok.ExpiresAt, Status: store.TokenValid,
		}); err != nil {
			return err
		}
		if _, err := tx.ClearPause(id.UserID, store.PauseCredentialInvalid, store.PauseCredentialRevoked, store.PauseCredentialExpired); err != nil {
			return err
		}
		return s.audit(tx, id.UserID, "token_created", "oidc", "ok", label)
	})
	if err != nil {
		abandon(false, false)
		return fmt.Errorf("auth: store the token: %w", err)
	}
	return nil
}

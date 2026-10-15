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
// longer knows, so the second listing is the only reliable proof. It reports
// whether no token with the label remains.
func (s *Service) deleteByLabel(ctx context.Context, cr ocisclient.Creds, label string) bool {
	list, err := s.d.Client.ListTokens(ctx, cr)
	if err != nil {
		return false
	}
	for _, ti := range list {
		if ti.Label == label {
			if err := s.d.Client.DeleteToken(ctx, cr, ti.Hash); err != nil {
				return false
			}
		}
	}
	list, err = s.d.Client.ListTokens(ctx, cr)
	if err != nil {
		return false
	}
	for _, ti := range list {
		if ti.Label == label {
			return false
		}
	}
	return true
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

	// abandon undoes step 1 after a failure. created says whether the token
	// may exist on oCIS and must be deleted first.
	abandon := func(created bool) {
		cctx, cancel := cleanupContext(ctx)
		defer cancel()
		if created && !s.deleteByLabel(cctx, oidc, label) {
			return // keep the label open: a later Sweep deletes the token
		}
		s.markLabelDeleted(ctx, id.UserID, label)
		if first {
			_ = s.d.Store.InTx(cctx, func(tx store.Tx) error {
				if _, err := tx.GetAppToken(id.UserID); err == nil {
					return nil // a concurrent path stored a token: keep the user
				}
				return tx.DeleteUser(id.UserID)
			})
		}
	}

	// 2. Create the token with the user's own credential.
	tok, err := s.d.Client.CreateToken(ctx, oidc, s.d.TokenTTL, label)
	if err != nil {
		abandon(false)
		return fmt.Errorf("auth: create the app token: %w", err)
	}
	app := ocisclient.AppToken(id.Username, tok.Value)

	// 3. The token must work and belong to this user.
	me, err := s.d.Client.Me(ctx, app)
	if err != nil {
		abandon(true)
		return fmt.Errorf("auth: verify the new token: %w", err)
	}
	if me.ID != id.UserID {
		abandon(true)
		return errors.New("auth: the new token belongs to a different user")
	}

	// 4. The memory folder, with the new credential.
	if err := s.d.Memory.Ensure(ctx, memory.Session{UserID: id.UserID, DriveID: id.DriveID, Creds: app}); err != nil {
		abandon(true)
		return fmt.Errorf("auth: prepare the memory folder: %w", err)
	}

	// 5. Seal outside the transaction, then commit everything at once.
	sealed, err := s.d.Secrets.Seal(ctx, []byte(tok.Value.Reveal()), s.aad(id.UserID))
	if err != nil {
		abandon(true)
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
		abandon(true)
		return fmt.Errorf("auth: store the token: %w", err)
	}
	return nil
}

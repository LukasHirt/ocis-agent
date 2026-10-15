package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/audit"
	"github.com/owncloud/ocis-agent/backend/internal/memory"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/secrets"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

var (
	// ErrCredentialInvalid: the user has no usable app token (none stored,
	// wiped, or marked invalid). Re-enrolment is needed.
	ErrCredentialInvalid = errors.New("auth: the agent has no valid credential for this user")
	// ErrNotConfirmed: an action that needs explicit confirmation was called
	// without it. Nothing was sent to oCIS.
	ErrNotConfirmed = errors.New("auth: the action needs explicit confirmation")
	// ErrRevokeIncomplete: some tokens were still listed after the delete
	// requests. Nothing local was changed; retry.
	ErrRevokeIncomplete = errors.New("auth: some tokens could not be deleted")
)

const (
	defaultTokenTTL = 72 * time.Hour
	instanceKey     = "instance_id"
	labelNamespace  = "ocis-agent-"
)

// Deps are the collaborators of a Service.
type Deps struct {
	Store   store.Store
	Secrets *secrets.Service
	Client  *ocisclient.Client
	Memory  *memory.Service
	// TokenTTL is the lifetime requested for new app tokens (default 72h).
	TokenTTL time.Duration
	// DormancyDays is the dormancy pause threshold given to NEW users; zero or
	// less keeps the store default.
	DormancyDays int
	// Now is the clock (default time.Now).
	Now func() time.Time
}

// Service runs the lifecycle of the agent's credentials.
type Service struct {
	d        Deps
	instance string
	locks    userLocks
}

// New returns a Service, creating the persistent instance id on first use.
func New(ctx context.Context, d Deps) (*Service, error) {
	switch {
	case d.Store == nil:
		return nil, errors.New("auth: a store is required")
	case d.Secrets == nil:
		return nil, errors.New("auth: a secrets service is required")
	case d.Client == nil:
		return nil, errors.New("auth: an oCIS client is required")
	case d.Memory == nil:
		return nil, errors.New("auth: a memory service is required")
	}
	if d.TokenTTL <= 0 {
		d.TokenTTL = defaultTokenTTL
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	id, err := instanceID(ctx, d.Store)
	if err != nil {
		return nil, err
	}
	return &Service{d: d, instance: id, locks: userLocks{m: map[string]*userLock{}}}, nil
}

// instanceID returns the persisted instance id, creating it once.
func instanceID(ctx context.Context, st store.Store) (string, error) {
	var id string
	err := st.InTx(ctx, func(tx store.Tx) error {
		v, err := tx.GetSystem(instanceKey)
		if err == nil {
			id = v
			return nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		id, err = randomHex(6)
		if err != nil {
			return err
		}
		return tx.SetSystem(instanceKey, id)
	})
	if err != nil {
		return "", fmt.Errorf("auth: instance id: %w", err)
	}
	return id, nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// InstanceID returns this sidecar's persistent instance id.
func (s *Service) InstanceID() string { return s.instance }

// LabelPrefix is the prefix of every token label this sidecar creates.
func (s *Service) LabelPrefix() string { return labelNamespace + s.instance + "-" }

// newLabel returns a fresh unique label. crypto/rand failing is not
// recoverable, so it panics rather than returning a predictable label.
func (s *Service) newLabel() string {
	n, err := randomHex(8)
	if err != nil {
		panic("auth: crypto/rand failed: " + err.Error())
	}
	return s.LabelPrefix() + n
}

func (s *Service) now() time.Time { return s.d.Now() }

// aad is the associated data that binds a sealed token to its user.
func (s *Service) aad(userID string) []byte { return []byte("app_token:" + userID) }

// audit appends an event in tx. Details must never contain tokens, hashes or
// labels we did not create.
func (s *Service) audit(tx store.Tx, userID, kind, cred, outcome, detail string) error {
	_, err := tx.AppendAudit(audit.Event{
		UserID: userID, TS: s.now(), Kind: kind, Cred: cred, Outcome: outcome, Detail: detail,
	})
	return err
}

// userLocks serialises operations per user inside this process.
type userLocks struct {
	mu sync.Mutex
	m  map[string]*userLock
}

type userLock struct {
	mu   sync.Mutex
	refs int
}

func (s *Service) lockUser(userID string) func() {
	l := &s.locks
	l.mu.Lock()
	e := l.m[userID]
	if e == nil {
		e = &userLock{}
		l.m[userID] = e
	}
	e.refs++
	l.mu.Unlock()
	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		l.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(l.m, userID)
		}
		l.mu.Unlock()
	}
}

// loadToken reads the user, the stored token and the opened credentials.
func (s *Service) loadToken(ctx context.Context, userID string) (store.User, store.AppToken, ocisclient.Creds, error) {
	var (
		u   store.User
		rec store.AppToken
	)
	err := s.d.Store.View(ctx, func(tx store.Tx) error {
		var err error
		if u, err = tx.GetUser(userID); err != nil {
			return err
		}
		rec, err = tx.GetAppToken(userID)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return store.User{}, store.AppToken{}, ocisclient.Creds{}, ErrCredentialInvalid
	}
	if err != nil {
		return store.User{}, store.AppToken{}, ocisclient.Creds{}, fmt.Errorf("auth: load token: %w", err)
	}
	if rec.Status != store.TokenValid || len(rec.Sealed.Ciphertext) == 0 {
		return u, rec, ocisclient.Creds{}, ErrCredentialInvalid
	}
	plain, err := s.d.Secrets.Open(ctx, rec.Sealed, s.aad(userID))
	if err != nil {
		return u, rec, ocisclient.Creds{}, fmt.Errorf("auth: open token: %w", err)
	}
	return u, rec, ocisclient.AppToken(u.Name, ocisclient.Secret(plain)), nil
}

// AppCreds returns the user's decrypted app credentials. It makes no oCIS
// call. ErrCredentialInvalid means re-enrolment is needed.
func (s *Service) AppCreds(ctx context.Context, userID string) (ocisclient.Creds, error) {
	_, _, cr, err := s.loadToken(ctx, userID)
	return cr, err
}

// HandleUnauthorized is called when oCIS answered 401 for the user's app
// credentials. The report is only a hint: under the user lock it re-checks the
// STORED credentials, so a stale report about a token that a renewal has since
// replaced is harmless. Only a definitive 401/403 for the stored credentials
// marks the token invalid and pauses the agent with credential_expired (the
// expiry has passed) or credential_invalid; a stronger pause (the user's own,
// or token_anomaly) is kept. Other errors are returned and nothing is marked.
func (s *Service) HandleUnauthorized(ctx context.Context, userID string) error {
	unlock := s.lockUser(userID)
	defer unlock()
	_, _, cr, err := s.loadToken(ctx, userID)
	if errors.Is(err, ErrCredentialInvalid) {
		return nil // nothing stored, or already handled
	}
	if err != nil {
		return err
	}
	_, err = s.d.Client.Me(ctx, cr)
	switch {
	case err == nil:
		return nil // stale report: the stored credentials work
	case errors.Is(err, ocisclient.ErrUnauthorized), errors.Is(err, ocisclient.ErrForbidden):
		return s.markUnauthorized(ctx, userID)
	default:
		return fmt.Errorf("auth: re-check credentials: %w", err)
	}
}

// markUnauthorized marks the stored token invalid unconditionally. Callers hold
// the user lock and have just seen a definitive 401 for the CURRENT credentials.
func (s *Service) markUnauthorized(ctx context.Context, userID string) error {
	return s.d.Store.InTx(ctx, func(tx store.Tx) error {
		rec, err := tx.GetAppToken(userID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if rec.Status != store.TokenValid {
			return nil // already handled
		}
		if err := tx.SetAppTokenStatus(userID, store.TokenInvalid); err != nil {
			return err
		}
		reason, kind := store.PauseCredentialInvalid, "token_invalid"
		if !s.now().Before(rec.ExpiresAt) {
			reason, kind = store.PauseCredentialExpired, "token_expired"
		}
		changed, err := tx.PausePolicy(userID, reason)
		if err != nil {
			return err
		}
		outcome := "paused"
		if !changed {
			outcome = "kept_pause" // a stronger pause was kept
		}
		return s.audit(tx, userID, kind, "app", outcome, "")
	})
}

// Seen records a visit of the user: last_seen_at moves forward and a dormant
// pause is cleared (audit agent_resumed). No other pause reason is touched.
func (s *Service) Seen(ctx context.Context, userID string) error {
	return s.d.Store.InTx(ctx, func(tx store.Tx) error {
		if err := tx.TouchUser(userID, s.now()); err != nil {
			return err
		}
		cleared, err := tx.ClearPause(userID, store.PauseDormant)
		if err != nil {
			return err
		}
		if cleared {
			return s.audit(tx, userID, "agent_resumed", "oidc", "resumed", "dormant")
		}
		return nil
	})
}

// PauseDormant pauses up to limit users who have not been seen for their
// dormancy_days and returns how many it paused. Tokens are not touched:
// renewal continues while dormant.
func (s *Service) PauseDormant(ctx context.Context, limit int) (int, error) {
	var ids []string
	if err := s.d.Store.View(ctx, func(tx store.Tx) error {
		var err error
		ids, err = tx.ListDormantUsers(s.now(), limit)
		return err
	}); err != nil {
		return 0, fmt.Errorf("auth: list dormant users: %w", err)
	}
	paused := 0
	for _, id := range ids {
		changed := false
		err := s.d.Store.InTx(ctx, func(tx store.Tx) error {
			var err error
			changed, err = tx.PausePolicy(id, store.PauseDormant)
			if err != nil || !changed {
				return err
			}
			return s.audit(tx, id, "agent_dormant", "app", "paused", "")
		})
		if errors.Is(err, store.ErrNotFound) {
			continue // the user's policy disappeared meanwhile
		}
		if err != nil {
			return paused, fmt.Errorf("auth: pause dormant user: %w", err)
		}
		if changed {
			paused++
		}
	}
	return paused, nil
}

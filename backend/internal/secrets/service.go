// Package secrets seals and opens small secrets (app tokens, connector
// credentials) with envelope encryption: each secret is encrypted with a
// data-encryption key (DEK), and DEKs are stored wrapped by a master key that
// lives only in the process environment.
package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// ErrDecrypt is returned for every failure to open a value or unwrap a key.
// It deliberately carries no detail about why.
var ErrDecrypt = errors.New("secrets: cannot decrypt")

const keyLen = 32

// Service seals and opens secrets.
type Service struct {
	st     store.Store
	master cipher.AEAD

	mu    sync.Mutex
	cache map[string]cipher.AEAD // unwrapped DEKs by id
}

// New returns a Service. masterKey must be exactly 32 bytes.
func New(st store.Store, masterKey []byte) (*Service, error) {
	if len(masterKey) != keyLen {
		return nil, fmt.Errorf("secrets: the master key must be %d bytes", keyLen)
	}
	master, err := newGCM(masterKey)
	if err != nil {
		return nil, err
	}
	return &Service{st: st, master: master, cache: map[string]cipher.AEAD{}}, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal encrypts plaintext with the active DEK (creating the first one on
// first use). aad binds the ciphertext to its context.
func (s *Service) Seal(ctx context.Context, plaintext, aad []byte) (store.Sealed, error) {
	dek, err := s.activeDEK(ctx)
	if err != nil {
		return store.Sealed{}, err
	}
	aead, err := s.aeadFor(dek)
	if err != nil {
		return store.Sealed{}, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return store.Sealed{}, fmt.Errorf("secrets: random nonce: %w", err)
	}
	return store.Sealed{DEKID: dek.ID, Nonce: nonce, Ciphertext: aead.Seal(nil, nonce, plaintext, aad)}, nil
}

// Open decrypts a sealed value. Key and data failures (unknown key, wrong
// master key, wrong aad, corrupt data) return ErrDecrypt; store failures are
// returned unchanged.
func (s *Service) Open(ctx context.Context, sealed store.Sealed, aad []byte) ([]byte, error) {
	var dek store.DEK
	err := s.st.View(ctx, func(tx store.Tx) error {
		var err error
		dek, err = tx.GetDEK(sealed.DEKID)
		return err
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrDecrypt
		}
		return nil, err
	}
	aead, err := s.aeadFor(dek)
	if err != nil {
		return nil, err
	}
	if len(sealed.Nonce) != aead.NonceSize() {
		return nil, ErrDecrypt
	}
	plaintext, err := aead.Open(nil, sealed.Nonce, sealed.Ciphertext, aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return plaintext, nil
}

// Rotate retires the active DEK and creates a new one. Values sealed with the
// old key stay readable.
func (s *Service) Rotate(ctx context.Context) error {
	return s.st.InTx(ctx, func(tx store.Tx) error {
		old, err := tx.ActiveDEK()
		switch {
		case err == nil:
			if err := tx.RetireDEK(old.ID); err != nil {
				return err
			}
		case errors.Is(err, store.ErrNotFound):
		default:
			return err
		}
		dek, err := s.newWrappedDEK(tx.Now())
		if err != nil {
			return err
		}
		return tx.PutDEK(dek)
	})
}

func (s *Service) activeDEK(ctx context.Context) (store.DEK, error) {
	var dek store.DEK
	err := s.st.View(ctx, func(tx store.Tx) error {
		var err error
		dek, err = tx.ActiveDEK()
		return err
	})
	if err == nil || !errors.Is(err, store.ErrNotFound) {
		return dek, err
	}
	// First use: create the DEK, re-checking inside the write transaction so
	// concurrent first users agree on a single key.
	err = s.st.InTx(ctx, func(tx store.Tx) error {
		existing, err := tx.ActiveDEK()
		if err == nil {
			dek = existing
			return nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		created, err := s.newWrappedDEK(tx.Now())
		if err != nil {
			return err
		}
		if err := tx.PutDEK(created); err != nil {
			return err
		}
		dek = created
		return nil
	})
	return dek, err
}

func (s *Service) newWrappedDEK(now time.Time) (store.DEK, error) {
	key := make([]byte, keyLen)
	if _, err := rand.Read(key); err != nil {
		return store.DEK{}, fmt.Errorf("secrets: random key: %w", err)
	}
	defer clear(key)
	id := store.NewID()
	nonce := make([]byte, s.master.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return store.DEK{}, fmt.Errorf("secrets: random nonce: %w", err)
	}
	return store.DEK{
		ID:         id,
		WrappedKey: s.master.Seal(nil, nonce, key, []byte("dek:"+id)),
		Nonce:      nonce,
		CreatedAt:  now,
	}, nil
}

func (s *Service) aeadFor(dek store.DEK) (cipher.AEAD, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.cache[dek.ID]; ok {
		return a, nil
	}
	if len(dek.Nonce) != s.master.NonceSize() {
		return nil, ErrDecrypt
	}
	key, err := s.master.Open(nil, dek.Nonce, dek.WrappedKey, []byte("dek:"+dek.ID))
	if err != nil {
		return nil, ErrDecrypt
	}
	defer clear(key)
	if len(key) != keyLen {
		return nil, ErrDecrypt
	}
	a, err := newGCM(key)
	if err != nil {
		return nil, ErrDecrypt
	}
	s.cache[dek.ID] = a
	return a, nil
}

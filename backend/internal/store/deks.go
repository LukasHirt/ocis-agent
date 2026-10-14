package store

import "time"

// DEK is a data-encryption key as stored: wrapped by the master key, never in
// plaintext.
type DEK struct {
	ID         string
	WrappedKey []byte
	Nonce      []byte
	CreatedAt  time.Time
	// RetiredAt is set once the key is no longer used to seal new data. A
	// retired key can still open what it sealed.
	RetiredAt *time.Time
}

// Sealed is an encrypted value together with what is needed to open it
// (given the master key and the same additional authenticated data).
type Sealed struct {
	DEKID      string
	Nonce      []byte
	Ciphertext []byte
}

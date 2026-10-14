// Package audit defines the audit record and the per-user hash chain that
// makes tampering with the log detectable.
package audit

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"strings"
	"time"
)

// Genesis is the prev_hash of the first record in every user's chain.
var Genesis = strings.Repeat("0", 64)

// ErrChainBroken is wrapped by every verification failure.
var ErrChainBroken = errors.New("audit: chain broken")

// Event is what callers append. The store assigns Seq, PrevHash and Hash.
type Event struct {
	UserID   string
	TS       time.Time
	JobID    string
	Kind     string
	Tool     string
	Risk     string
	ArgsHash string
	Cred     string
	Outcome  string
	Detail   string
}

// Record is a stored event with its place in the chain.
type Record struct {
	Event
	Seq      int64
	PrevHash string
	Hash     string
}

// Hash returns the hex SHA-256 over a canonical, length-prefixed encoding of
// the previous hash, the position and all event fields. Length prefixes make
// the field boundaries unambiguous. The timestamp is hashed at millisecond
// precision, which is also what the store keeps.
func Hash(prev string, seq int64, e Event) string {
	h := sha256.New()
	writeString(h, "ocis-agent/audit/v1")
	writeString(h, prev)
	writeString(h, e.UserID)
	writeInt(h, seq)
	writeInt(h, e.TS.UnixMilli())
	for _, s := range []string{e.JobID, e.Kind, e.Tool, e.Risk, e.ArgsHash, e.Cred, e.Outcome, e.Detail} {
		writeString(h, s)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func writeString(h hash.Hash, s string) {
	writeInt(h, int64(len(s)))
	h.Write([]byte(s)) // hash.Hash.Write never returns an error
}

func writeInt(h hash.Hash, n int64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(n))
	h.Write(b[:])
}

// Verifier checks a user's records in order. It is incremental so a long log
// can be verified in pages.
type Verifier struct {
	nextSeq int64
	prev    string
}

// NewVerifier starts at the beginning of a chain.
func NewVerifier() *Verifier {
	return &Verifier{nextSeq: 1, prev: Genesis}
}

// NewVerifierFrom starts at a checkpoint: the next expected sequence number
// and the hash of the record before it.
func NewVerifierFrom(nextSeq int64, prev string) *Verifier {
	return &Verifier{nextSeq: nextSeq, prev: prev}
}

// Add checks the next record. It fails if the sequence has a gap, the record
// does not point at its predecessor, or its own hash does not match its
// content.
func (v *Verifier) Add(r Record) error {
	if r.Seq != v.nextSeq {
		return fmt.Errorf("%w: user %s: expected seq %d, got %d", ErrChainBroken, r.UserID, v.nextSeq, r.Seq)
	}
	if r.PrevHash != v.prev {
		return fmt.Errorf("%w: user %s seq %d: prev_hash does not match the previous record", ErrChainBroken, r.UserID, r.Seq)
	}
	if want := Hash(r.PrevHash, r.Seq, r.Event); r.Hash != want {
		return fmt.Errorf("%w: user %s seq %d: hash does not match the record content", ErrChainBroken, r.UserID, r.Seq)
	}
	v.nextSeq++
	v.prev = r.Hash
	return nil
}

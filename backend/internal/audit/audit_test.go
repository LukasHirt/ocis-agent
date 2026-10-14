package audit

import (
	"errors"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func event(i int) Event {
	return Event{
		UserID:   "u1",
		TS:       t0.Add(time.Duration(i) * time.Second),
		JobID:    "job",
		Kind:     "tool_call",
		Tool:     "files.list",
		Risk:     "read",
		ArgsHash: "sha256:abc",
		Cred:     "app",
		Outcome:  "ok",
		Detail:   "detail",
	}
}

func chain(n int) []Record {
	recs := make([]Record, 0, n)
	prev := Genesis
	for i := 1; i <= n; i++ {
		e := event(i)
		r := Record{Event: e, Seq: int64(i), PrevHash: prev}
		r.Hash = Hash(prev, r.Seq, e)
		recs = append(recs, r)
		prev = r.Hash
	}
	return recs
}

func verify(recs []Record) error {
	v := NewVerifier()
	for _, r := range recs {
		if err := v.Add(r); err != nil {
			return err
		}
	}
	return nil
}

func TestHashIsDeterministicAndHex(t *testing.T) {
	a := Hash(Genesis, 1, event(1))
	b := Hash(Genesis, 1, event(1))
	if a != b {
		t.Fatal("hash is not deterministic")
	}
	if len(a) != 64 {
		t.Fatalf("hash length = %d, want 64 hex chars", len(a))
	}
}

func TestEveryFieldChangesTheHash(t *testing.T) {
	base := event(1)
	want := Hash(Genesis, 1, base)
	mutations := map[string]func(Event) Event{
		"user":    func(e Event) Event { e.UserID = "u2"; return e },
		"ts":      func(e Event) Event { e.TS = e.TS.Add(time.Millisecond); return e },
		"job":     func(e Event) Event { e.JobID = "job2"; return e },
		"kind":    func(e Event) Event { e.Kind = "other"; return e },
		"tool":    func(e Event) Event { e.Tool = "files.read_text"; return e },
		"risk":    func(e Event) Event { e.Risk = "write"; return e },
		"args":    func(e Event) Event { e.ArgsHash = "sha256:def"; return e },
		"cred":    func(e Event) Event { e.Cred = "oidc"; return e },
		"outcome": func(e Event) Event { e.Outcome = "denied"; return e },
		"detail":  func(e Event) Event { e.Detail = "other"; return e },
	}
	for name, mut := range mutations {
		if got := Hash(Genesis, 1, mut(base)); got == want {
			t.Errorf("changing %s did not change the hash", name)
		}
	}
	if Hash(Genesis, 2, base) == want {
		t.Error("changing seq did not change the hash")
	}
	if Hash("1"+Genesis[1:], 1, base) == want {
		t.Error("changing prev did not change the hash")
	}
}

func TestFieldBoundariesAreUnambiguous(t *testing.T) {
	a := event(1)
	a.Tool, a.Risk = "a", "bc"
	b := event(1)
	b.Tool, b.Risk = "ab", "c"
	if Hash(Genesis, 1, a) == Hash(Genesis, 1, b) {
		t.Fatal("moving a character between adjacent fields must change the hash")
	}
}

func TestVerifierAcceptsAValidChain(t *testing.T) {
	if err := verify(chain(5)); err != nil {
		t.Fatalf("valid chain rejected: %v", err)
	}
}

func TestVerifierDetectsTampering(t *testing.T) {
	recs := chain(4)
	recs[1].Detail = "tampered"
	err := verify(recs)
	if !errors.Is(err, ErrChainBroken) {
		t.Fatalf("err = %v, want ErrChainBroken", err)
	}
}

func TestVerifierDetectsADeletedMiddleRecord(t *testing.T) {
	recs := chain(4)
	recs = append(recs[:1], recs[2:]...)
	if err := verify(recs); !errors.Is(err, ErrChainBroken) {
		t.Fatalf("err = %v, want ErrChainBroken", err)
	}
}

func TestVerifierDetectsADeletedFirstRecord(t *testing.T) {
	recs := chain(3)[1:]
	if err := verify(recs); !errors.Is(err, ErrChainBroken) {
		t.Fatalf("err = %v, want ErrChainBroken", err)
	}
}

func TestVerifierDetectsReordering(t *testing.T) {
	recs := chain(3)
	recs[1], recs[2] = recs[2], recs[1]
	if err := verify(recs); !errors.Is(err, ErrChainBroken) {
		t.Fatalf("err = %v, want ErrChainBroken", err)
	}
}

func TestVerifierDetectsARewrittenRecordWithRecomputedHash(t *testing.T) {
	recs := chain(3)
	recs[1].Detail = "tampered"
	recs[1].Hash = Hash(recs[1].PrevHash, recs[1].Seq, recs[1].Event)
	// the attacker fixed record 2's own hash but record 3 still points at the old one
	if err := verify(recs); !errors.Is(err, ErrChainBroken) {
		t.Fatalf("err = %v, want ErrChainBroken", err)
	}
}

func TestVerifierFromACheckpoint(t *testing.T) {
	recs := chain(5)
	v := NewVerifierFrom(3, recs[1].Hash)
	for _, r := range recs[2:] {
		if err := v.Add(r); err != nil {
			t.Fatalf("checkpointed chain rejected: %v", err)
		}
	}
}

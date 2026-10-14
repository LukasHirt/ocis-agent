package storetest

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/audit"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

func init() { register("Invalid", runInvalid) }

func runInvalid(t *testing.T, f Factory) {
	st, _ := open(t, f)
	backoff := func(int) time.Duration { return time.Minute }
	cases := []struct {
		name string
		call func(tx store.Tx) error
	}{
		{"empty user", func(tx store.Tx) error { return tx.PutUser(store.User{}) }},
		{"invalid policy", func(tx store.Tx) error { return tx.PutPolicy(store.Policy{UserID: "u", Paused: true}) }},
		{"empty app token", func(tx store.Tx) error { return tx.PutAppToken(store.AppToken{}) }},
		{"unknown token status", func(tx store.Tx) error { return tx.SetAppTokenStatus("u", "weird") }},
		{"empty token label", func(tx store.Tx) error { return tx.AddTokenLabel("", "") }},
		{"empty DEK", func(tx store.Tx) error { return tx.PutDEK(store.DEK{}) }},
		{"empty audit event", func(tx store.Tx) error { _, err := tx.AppendAudit(audit.Event{}); return err }},
		{"empty job", func(tx store.Tx) error { _, err := tx.InsertJob(store.Job{}); return err }},
		{"broken checkpoint on insert", func(tx store.Tx) error {
			_, err := tx.InsertJob(store.Job{UserID: "u", TriggerKind: "chat", Goal: "g", Checkpoint: json.RawMessage("{")})
			return err
		}},
		{"broken checkpoint on save", func(tx store.Tx) error {
			return tx.SaveCheckpoint("u", "j", json.RawMessage("{"), 1)
		}},
		{"unknown step kind", func(tx store.Tx) error {
			_, err := tx.AppendStep(store.JobStep{JobID: "j", Kind: "weird", Status: "ok"}, 1)
			return err
		}},
		{"step without a status", func(tx store.Tx) error {
			_, err := tx.AppendStep(store.JobStep{JobID: "j", Kind: "tool"}, 1)
			return err
		}},
		{"claim without a worker", func(tx store.Tx) error { _, _, err := tx.ClaimNextJob("", time.Minute); return err }},
		{"claim with a tiny ttl", func(tx store.Tx) error { _, _, err := tx.ClaimNextJob("w", time.Microsecond); return err }},
		{"renew with a tiny ttl", func(tx store.Tx) error { _, err := tx.RenewLease(store.Claim{}, 0); return err }},
		{"release into running", func(tx store.Tx) error {
			_, err := tx.ReleaseJob(store.Claim{}, store.JobRunning, nil, "")
			return err
		}},
		{"release with a broken checkpoint", func(tx store.Tx) error {
			_, err := tx.ReleaseJob(store.Claim{}, store.JobQueued, json.RawMessage("{"), "")
			return err
		}},
		{"reap without a backoff", func(tx store.Tx) error { _, err := tx.ReapExpired(1, nil); return err }},
		{"reap with zero attempts", func(tx store.Tx) error { _, err := tx.ReapExpired(0, backoff); return err }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := st.InTx(bg(), c.call)
			if !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

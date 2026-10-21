package store

import (
	"encoding/json"
	"testing"
	"time"
)

func TestApprovalOutcomeUncertainAndNeedsVerification(t *testing.T) {
	cases := []struct {
		state  ApprovalState
		result string
		want   bool
	}{
		{ApprovalPending, "", false},
		{ApprovalApproved, "", false},
		{ApprovalExecuting, "", false},
		{ApprovalRejected, "", false},
		{ApprovalExpired, "", false},
		{ApprovalExecuted, "", false},
		{ApprovalFailed, "", false},
		{ApprovalUnknown, "", true},
		{ApprovalUnknown, `{"outcome":"done"}`, true},
		{ApprovalExecuted, `{"outcome":"done","result":{"summary":"ok","diff":"+a"}}`, false},
		{ApprovalFailed, `{"outcome":"failed","reason":"no"}`, false},
		{ApprovalExpired, `{"outcome":"expired"}`, false},
		{ApprovalFailed, `{"outcome":"unknown"}`, true},
		{ApprovalExecuted, `["strange"]`, true},
		{ApprovalExecuted, `{"outcome":"maybe"}`, true},
		{ApprovalExecuted, `{"outcome":5}`, true},
		{ApprovalExecuted, `{"outcome":"done","result":{"summary":5}}`, true},
		{ApprovalExecuted, `{"outcome":"done","result":"x"}`, true},
		{ApprovalFailed, `{broken`, true},
	}
	for _, c := range cases {
		a := Approval{State: c.state}
		if c.result != "" {
			a.Result = json.RawMessage(c.result)
		}
		if got := a.OutcomeUncertain(); got != c.want {
			t.Errorf("%s %s: OutcomeUncertain = %v, want %v", c.state, c.result, got, c.want)
		}
		if got := a.NeedsVerification(); got != c.want {
			t.Errorf("%s %s: NeedsVerification = %v, want %v", c.state, c.result, got, c.want)
		}
		now := time.Now()
		a.AcknowledgedAt = &now
		if a.NeedsVerification() || a.OutcomeUncertain() != c.want {
			t.Errorf("%s %s: an acknowledged approval needs no verification and keeps its outcome", c.state, c.result)
		}
	}
}

func TestDecodeOutcome(t *testing.T) {
	o, err := DecodeOutcome(json.RawMessage(`{"outcome":"done","result":{"summary":"s","diff":"d","blocks":[{"source":"file","path":"/p","sha256":"h","content":"c"}]},"reason":"r"}`))
	if err != nil || o.Outcome != "done" || o.Reason != "r" || o.Result == nil || o.Result.Summary != "s" || o.Result.Diff != "d" ||
		len(o.Result.Blocks) != 1 || o.Result.Blocks[0] != (OutcomeBlock{Source: "file", Path: "/p", SHA256: "h", Content: "c"}) || !o.Certain() {
		t.Fatalf("decoded = %+v %v", o, err)
	}
	for _, raw := range []string{`["x"]`, `{"outcome":5}`, `{"outcome":"done","result":{"blocks":5}}`, `{"outcome":"done","result":{"blocks":[{"content":1}]}}`, `{x`} {
		if _, err := DecodeOutcome(json.RawMessage(raw)); err == nil {
			t.Errorf("%s decoded", raw)
		}
	}
	for out, certain := range map[string]bool{"done": true, "failed": true, "expired": true, "unknown": false, "": false, "maybe": false} {
		if (RecordedOutcome{Outcome: out}).Certain() != certain {
			t.Errorf("%q: Certain != %v", out, certain)
		}
	}
	a := Approval{State: ApprovalExecuted, Result: json.RawMessage(`{"outcome":"done","result":{"blocks":5}}`)}
	if !a.OutcomeUncertain() {
		t.Error("a result with unreadable blocks is uncertain")
	}
}

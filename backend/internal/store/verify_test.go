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

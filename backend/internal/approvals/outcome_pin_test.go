package approvals

import (
	"encoding/json"
	"testing"

	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/store/storetest"
	"github.com/owncloud/ocis-agent/backend/internal/tools"
)

// TestStoredOutcomesAgreeWithTheStore: the executor reads a stored result as
// unknown exactly when the store calls it uncertain (needs_verification).
func TestStoredOutcomesAgreeWithTheStore(t *testing.T) {
	for _, raw := range storetest.StoredResults {
		a := store.Approval{State: store.ApprovalExecuted, Result: json.RawMessage(raw)}
		if got, want := decodeOutcome(a.Result).State == OutcomeUnknown, a.OutcomeUncertain(); got != want {
			t.Errorf("%s: executor unknown = %v, store uncertain = %v", raw, got, want)
		}
	}
	// what the executor writes reads back as written
	for _, o := range []Outcome{
		{State: OutcomeDone, Result: tools.Result{Summary: "s", Diff: "d", Blocks: []tools.Block{{Source: "file", Path: "/p", Content: "c"}}}},
		{State: OutcomeFailed, Reason: "r"}, {State: OutcomeExpired, Reason: "e"}, {State: OutcomeUnknown, Reason: "u"},
	} {
		back := decodeOutcome(encode(o))
		if back.State != o.State || back.Reason != o.Reason || back.Result.Summary != o.Result.Summary ||
			back.Result.Diff != o.Result.Diff || len(back.Result.Blocks) != len(o.Result.Blocks) {
			t.Errorf("%+v read back as %+v", o, back)
		}
		a := store.Approval{State: store.ApprovalExecuted, Result: encode(o)}
		if a.OutcomeUncertain() != (o.State == OutcomeUnknown) {
			t.Errorf("%+v: store uncertain = %v", o, a.OutcomeUncertain())
		}
	}
}

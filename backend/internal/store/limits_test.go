package store

import "testing"

// history() in the API passes up to MaxRecentMessages ids to JobStatuses;
// raising one limit above the other would make every such call fail and
// silently drop all chat history.
func TestRecentMessagesFitInOneJobStatusesCall(t *testing.T) {
	if MaxRecentMessages > MaxJobStatusIDs {
		t.Fatalf("MaxRecentMessages (%d) > MaxJobStatusIDs (%d)", MaxRecentMessages, MaxJobStatusIDs)
	}
}

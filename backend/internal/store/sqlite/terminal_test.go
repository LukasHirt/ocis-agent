package sqlite

import (
	"strings"
	"testing"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// TestTerminalJobStatesComeFromTheStore: the SQL list of ended job states
// is built from store.JobState.Terminal, the one source of which states end
// a job (the migrations keep their own literals).
func TestTerminalJobStatesComeFromTheStore(t *testing.T) {
	var want []string
	for _, s := range []store.JobState{store.JobQueued, store.JobRunning, store.JobWaitingApproval,
		store.JobSucceeded, store.JobFailed, store.JobCancelled} {
		if s.Terminal() {
			want = append(want, "'"+string(s)+"'")
		}
	}
	if got := "(" + strings.Join(want, ", ") + ")"; terminalJobStates != got {
		t.Fatalf("terminalJobStates = %s, want %s", terminalJobStates, got)
	}
}

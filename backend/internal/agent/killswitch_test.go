package agent

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/store/sqlite"
)

func newStore(t *testing.T) store.Store {
	t.Helper()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestKillSwitch(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	on, err := KillSwitchOn(ctx, st, false)
	if err != nil || on {
		t.Fatalf("absent key: %v %v", on, err)
	}
	on, err = KillSwitchOn(ctx, st, true)
	if err != nil || !on {
		t.Fatalf("env flag: %v %v", on, err)
	}
	if err := SetKillSwitch(ctx, st, true); err != nil {
		t.Fatal(err)
	}
	on, err = KillSwitchOn(ctx, st, false)
	if err != nil || !on {
		t.Fatalf("db flag: %v %v", on, err)
	}
	on, _ = KillSwitchOn(ctx, st, true)
	if !on {
		t.Fatal("both on")
	}
	if err := SetKillSwitch(ctx, st, false); err != nil {
		t.Fatal(err)
	}
	on, err = KillSwitchOn(ctx, st, false)
	if err != nil || on {
		t.Fatalf("flipped back: %v %v", on, err)
	}
	on, _ = KillSwitchOn(ctx, st, true)
	if !on {
		t.Fatal("env still wins")
	}
}

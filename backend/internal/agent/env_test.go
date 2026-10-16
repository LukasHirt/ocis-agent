package agent

import (
	"errors"
	"testing"

	"github.com/owncloud/ocis-agent/backend/internal/auth"
	"github.com/owncloud/ocis-agent/backend/internal/memory"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/tools"
)

func TestEnvComesFromTheServerWithFreshBudgets(t *testing.T) {
	f := newFixture(t)
	b := f.deps().Env
	env, err := b.Env(bg, userID)
	if err != nil {
		t.Fatal(err)
	}
	if env.UserID != userID || env.DriveID != drive || env.Files != f.client || env.Memory != f.mem {
		t.Fatalf("env = %+v", env)
	}
	if env.Creds.Kind() != "app" {
		t.Errorf("creds kind = %q", env.Creds.Kind())
	}
	if env.Reads == nil || env.Reads.Remaining() != tools.DefaultJobReadBytes {
		t.Errorf("read budget = %v", env.Reads)
	}
	if env.MemoryWrites == nil || env.MemoryWrites.Used() != 0 {
		t.Error("a fresh memory budget is required")
	}
	if err := env.MemoryWrites.Reserve(memory.DefaultLimits().MaxJobBytes); err != nil {
		t.Errorf("the memory budget must hold the job limit: %v", err)
	}
	other, err := b.EnvFunc()(bg, userID)
	if err != nil {
		t.Fatal(err)
	}
	if other.Reads == env.Reads || other.MemoryWrites == env.MemoryWrites {
		t.Error("every env must get its own budgets")
	}
}

func TestEnvFailsWithoutCredentialsOrUser(t *testing.T) {
	f := newFixture(t)
	b := f.deps().Env
	if _, err := b.Env(bg, "nobody"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown user: err = %v", err)
	}
	f.creds.setErr(auth.ErrCredentialInvalid)
	if _, err := b.Env(bg, userID); !errors.Is(err, auth.ErrCredentialInvalid) {
		t.Errorf("err = %v, want ErrCredentialInvalid", err)
	}
}

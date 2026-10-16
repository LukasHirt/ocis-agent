package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/owncloud/ocis-agent/backend/internal/approvals"
	"github.com/owncloud/ocis-agent/backend/internal/auth"
	"github.com/owncloud/ocis-agent/backend/internal/memory"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/tools"
)

// Credentials hands out the agent's app credentials for a user and takes
// reports of a 401 from oCIS. *auth.Service satisfies it.
type Credentials interface {
	AppCreds(ctx context.Context, userID string) (ocisclient.Creds, error)
	HandleUnauthorized(ctx context.Context, userID string) error
}

var _ Credentials = (*auth.Service)(nil)

// EnvBuilder builds the tool environment of a user on the server: the drive
// comes from the database, never from model input.
type EnvBuilder struct {
	Creds  Credentials
	Store  store.Store
	Files  *ocisclient.Client
	Memory *memory.Service
}

func (b EnvBuilder) complete() bool {
	return b.Creds != nil && b.Store != nil && b.Files != nil && b.Memory != nil
}

// Env returns the user's environment with fresh per-job budgets. An error
// from the credentials (auth.ErrCredentialInvalid) is returned wrapped.
func (b EnvBuilder) Env(ctx context.Context, userID string) (tools.Env, error) {
	var u store.User
	if err := b.Store.View(ctx, func(tx store.Tx) error {
		var err error
		u, err = tx.GetUser(userID)
		return err
	}); err != nil {
		return tools.Env{}, fmt.Errorf("agent: load user: %w", err)
	}
	if u.PersonalDriveID == "" || u.Name == "" {
		return tools.Env{}, errors.New("agent: the user has no personal drive or login name")
	}
	creds, err := b.Creds.AppCreds(ctx, userID)
	if err != nil {
		return tools.Env{}, fmt.Errorf("agent: credentials: %w", err)
	}
	return tools.Env{
		UserID: userID, DriveID: u.PersonalDriveID, Creds: creds, Files: b.Files, Memory: b.Memory,
		Reads:        tools.NewReadBudget(tools.DefaultJobReadBytes),
		MemoryWrites: memory.NewBudget(memory.DefaultLimits().MaxJobBytes),
	}, nil
}

// EnvFunc adapts Env for the executor's orphan reconciliation.
func (b EnvBuilder) EnvFunc() approvals.EnvFunc { return b.Env }

func session(env tools.Env) memory.Session {
	return memory.Session{UserID: env.UserID, DriveID: env.DriveID, Creds: env.Creds}
}

package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

const killSwitchKey = "killswitch"

// KillSwitchOn reports whether the kill switch is on: the environment flag or
// the "killswitch" system key equal to "1". An absent key is off.
func KillSwitchOn(ctx context.Context, st store.Store, envFlag bool) (bool, error) {
	if envFlag {
		return true, nil
	}
	var v string
	err := st.View(ctx, func(tx store.Tx) error {
		var err error
		v, err = tx.GetSystem(killSwitchKey)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("agent: read kill switch: %w", err)
	}
	return v == "1", nil
}

// SetKillSwitch persists the kill switch state.
func SetKillSwitch(ctx context.Context, st store.Store, on bool) error {
	v := "0"
	if on {
		v = "1"
	}
	if err := st.InTx(ctx, func(tx store.Tx) error { return tx.SetSystem(killSwitchKey, v) }); err != nil {
		return fmt.Errorf("agent: write kill switch: %w", err)
	}
	return nil
}

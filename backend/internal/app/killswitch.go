package app

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/owncloud/ocis-agent/backend/internal/agent"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// KillSwitch runs the killswitch command: "on" and "off" set the database
// flag, "status" reads it. Running workers pick a change up without a
// restart. It prints one line, the database state and, when envFlag
// (AGENT_KILL_SWITCH) is true, that the environment holds the switch on
// regardless. It needs only the store: no master key is used.
func KillSwitch(ctx context.Context, st store.Store, envFlag bool, cmd string, w io.Writer) error {
	switch cmd {
	case "on", "off":
		if err := agent.SetKillSwitch(ctx, st, cmd == "on"); err != nil {
			return err
		}
	case "status":
	default:
		return errors.New(`killswitch takes one argument: on, off or status`)
	}
	// the database flag alone: the environment is reported separately
	on, err := agent.KillSwitchOn(ctx, st, false)
	if err != nil {
		return err
	}
	line := "kill switch: " + onOff(on) + " (database)"
	if envFlag {
		line += " (environment: on)"
	}
	_, err = fmt.Fprintln(w, line)
	return err
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/owncloud/ocis-agent/backend/internal/secrets"
	"github.com/owncloud/ocis-agent/backend/internal/store/sqlite"
)

var (
	keyA = base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	keyB = base64.StdEncoding.EncodeToString([]byte("fedcba9876543210fedcba9876543210"))
)

// setup points the environment at a temporary data directory and captures
// the command's output.
func setup(t *testing.T) (dir string, out, errOut *bytes.Buffer) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "data")
	t.Setenv("AGENT_OCIS_URL", "https://cloud.example.com")
	t.Setenv("AGENT_MASTER_KEY", keyA)
	t.Setenv("AGENT_DATA_DIR", dir)
	for _, k := range []string{"AGENT_SQLITE_PATH", "AGENT_ROLES", "AGENT_LOG_LEVEL", "AGENT_KILL_SWITCH", "AGENT_LLM_BASE_URL", "AGENT_LLM_MODEL", "AGENT_LLM_API_KEY", "AGENT_LLM_API_KEY_FILE", "AGENT_LLM_MODELS", "AGENT_LLM_PROVIDER", "AGENT_LLM_ALLOWED_HOSTS"} {
		t.Setenv(k, "")
	}
	out, errOut = &bytes.Buffer{}, &bytes.Buffer{}
	oldOut, oldErr := stdout, stderr
	stdout, stderr = out, errOut
	t.Cleanup(func() { stdout, stderr = oldOut, oldErr })
	return dir, out, errOut
}

// Rule 9: migrate still works and unknown commands are refused.
func TestMigrate(t *testing.T) {
	dir, out, _ := setup(t)
	if err := run([]string{"migrate"}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "agent.db")); err != nil {
		t.Errorf("no database: %v", err)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("data dir mode = %v, %v", fi.Mode().Perm(), err)
	}
	if out.Len() != 0 {
		t.Errorf("migrate printed %q", out.String())
	}
	if err := run([]string{"migrate", "now"}); err == nil {
		t.Error("migrate with an argument: no error")
	}
}

func TestUnknownCommands(t *testing.T) {
	setup(t)
	for _, args := range [][]string{{"serve"}, {"help"}, {"--roles=api"}, {"killswitch"}, {"killswitch", "maybe"}, {"killswitch", "on", "now"}} {
		err := run(args)
		if err == nil {
			t.Errorf("%q: no error", args)
			continue
		}
		if args[0] != "killswitch" && !strings.Contains(err.Error(), "known commands") {
			t.Errorf("%q: %v", args, err)
		}
	}
}

// Rule 8: an unknown role is refused with a clear error.
func TestAnUnknownRoleIsRefused(t *testing.T) {
	setup(t)
	t.Setenv("AGENT_ROLES", "api,scheduler")
	err := run(nil)
	if err == nil || !strings.Contains(err.Error(), "AGENT_ROLES") {
		t.Errorf("err = %v, want one naming AGENT_ROLES", err)
	}
}

func TestRunWithTheWorkerRoleNeedsAModel(t *testing.T) {
	setup(t)
	err := run(nil)
	if err == nil || !strings.Contains(err.Error(), "AGENT_LLM_BASE_URL") {
		t.Errorf("err = %v, want one naming AGENT_LLM_BASE_URL", err)
	}
}

// Rule 7 (command side): the kill switch needs the database but not a master
// key that opens it, and never prints a secret.
func TestKillSwitchCommand(t *testing.T) {
	dir, out, errOut := setup(t)
	// a database whose data-encryption key is wrapped by key A
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(filepath.Join(dir, "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	sec, err := secrets.New(st, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sec.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_MASTER_KEY", keyB)
	t.Setenv("AGENT_ROLES", "api")

	// the server refuses the wrong key
	if err := run(nil); err == nil || !strings.Contains(err.Error(), "AGENT_MASTER_KEY") {
		t.Fatalf("run with the wrong key: %v", err)
	}
	for _, c := range []struct {
		args []string
		env  string
		want string
	}{
		{[]string{"killswitch", "status"}, "", "kill switch: off (database)\n"},
		{[]string{"killswitch", "on"}, "", "kill switch: on (database)\n"},
		{[]string{"killswitch", "status"}, "", "kill switch: on (database)\n"},
		{[]string{"killswitch", "off"}, "true", "kill switch: off (database) (environment: on)\n"},
		{[]string{"killswitch", "status"}, "true", "kill switch: off (database) (environment: on)\n"},
		{[]string{"killswitch", "status"}, "false", "kill switch: off (database)\n"},
	} {
		t.Setenv("AGENT_KILL_SWITCH", c.env)
		out.Reset()
		if err := run(c.args); err != nil {
			t.Errorf("%q: %v", c.args, err)
			continue
		}
		if out.String() != c.want {
			t.Errorf("%q (env %q): printed %q, want %q", c.args, c.env, out.String(), c.want)
		}
	}
	all := out.String() + errOut.String()
	for _, secret := range []string{keyA, keyB} {
		if strings.Contains(all, secret) {
			t.Error("the output contains a master key")
		}
	}
}

func TestLogsAreJSONAtTheConfiguredLevel(t *testing.T) {
	_, _, errOut := setup(t)
	t.Setenv("AGENT_LOG_LEVEL", "debug")
	if err := run([]string{"migrate"}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(errOut.String()), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatal("no log line")
	}
	for _, l := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil || m["level"] == nil || m["msg"] == nil {
			t.Errorf("not a JSON log line: %q", l)
		}
	}
	if strings.Contains(errOut.String(), keyA) {
		t.Error("the log contains the master key")
	}

	errOut.Reset()
	t.Setenv("AGENT_LOG_LEVEL", "error")
	if err := run([]string{"migrate"}); err != nil {
		t.Fatal(err)
	}
	if errOut.Len() != 0 {
		t.Errorf("info logged at level error: %q", errOut.String())
	}
}

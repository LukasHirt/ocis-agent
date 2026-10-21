package config

import "testing"

func TestSQLitePathDefaultsToTheDataDir(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"AGENT_OCIS_URL":   "https://cloud.example.com",
		"AGENT_MASTER_KEY": key32(),
		"AGENT_DATA_DIR":   "/srv/agent",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SQLitePath != "/srv/agent/agent.db" {
		t.Fatalf("SQLitePath = %q", cfg.SQLitePath)
	}
}

func TestSQLitePathDefaultWithTheDefaultDataDir(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"AGENT_OCIS_URL":   "https://cloud.example.com",
		"AGENT_MASTER_KEY": key32(),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SQLitePath != "/var/lib/ocis-agent/agent.db" {
		t.Fatalf("SQLitePath = %q", cfg.SQLitePath)
	}
}

func TestSQLitePathCanBeOverridden(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"AGENT_OCIS_URL":    "https://cloud.example.com",
		"AGENT_MASTER_KEY":  key32(),
		"AGENT_SQLITE_PATH": "/data/custom.db",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SQLitePath != "/data/custom.db" {
		t.Fatalf("SQLitePath = %q", cfg.SQLitePath)
	}
}

// DatabasePath needs neither the oCIS URL nor the master key (the backup
// command reads only the data location) and agrees with Load.
func TestDatabasePathNeedsOnlyTheDataLocation(t *testing.T) {
	for _, c := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{}, "/var/lib/ocis-agent/agent.db"},
		{map[string]string{"AGENT_DATA_DIR": "/srv/agent"}, "/srv/agent/agent.db"},
		{map[string]string{"AGENT_DATA_DIR": "/srv/agent", "AGENT_SQLITE_PATH": "/data/custom.db"}, "/data/custom.db"},
	} {
		if got := DatabasePath(env(c.env)); got != c.want {
			t.Errorf("DatabasePath(%v) = %q, want %q", c.env, got, c.want)
		}
		full := map[string]string{"AGENT_OCIS_URL": "https://cloud.example.com", "AGENT_MASTER_KEY": key32()}
		for k, v := range c.env {
			full[k] = v
		}
		cfg, err := Load(env(full))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.SQLitePath != c.want {
			t.Errorf("Load SQLitePath = %q, want %q", cfg.SQLitePath, c.want)
		}
	}
}

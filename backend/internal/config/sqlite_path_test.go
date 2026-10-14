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

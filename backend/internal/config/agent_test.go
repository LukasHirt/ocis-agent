package config

import (
	"strings"
	"testing"
	"time"
)

func TestAgentDefaults(t *testing.T) {
	cfg, err := Load(llmEnv(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := AgentConfig{Workers: 4, MaxSteps: 30, MaxAttempts: 3, MaxRuns: 20, MaxTokens: 4096, LeaseTTL: 2 * time.Minute}
	if cfg.Agent != want {
		t.Errorf("Agent = %+v, want %+v", cfg.Agent, want)
	}
}

func TestAgentOverrides(t *testing.T) {
	cfg, err := Load(llmEnv(map[string]string{
		"AGENT_WORKERS":        " 64 ",
		"AGENT_MAX_STEPS":      "200",
		"AGENT_MAX_ATTEMPTS":   "1",
		"AGENT_MAX_RUNS":       "1000",
		"AGENT_LLM_MAX_TOKENS": "64",
		"AGENT_LEASE_TTL":      " 1s ",
		"AGENT_KILL_SWITCH":    " true ",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := AgentConfig{Workers: 64, MaxSteps: 200, MaxAttempts: 1, MaxRuns: 1000, MaxTokens: 64, LeaseTTL: time.Second, KillSwitch: true}
	if cfg.Agent != want {
		t.Errorf("Agent = %+v, want %+v", cfg.Agent, want)
	}
	cfg, err = Load(llmEnv(map[string]string{"AGENT_LEASE_TTL": "1h", "AGENT_LLM_MAX_TOKENS": "100000", "AGENT_WORKERS": "1"}))
	if err != nil || cfg.Agent.LeaseTTL != time.Hour || cfg.Agent.MaxTokens != 100000 || cfg.Agent.Workers != 1 {
		t.Errorf("bounds: %+v, %v", cfg.Agent, err)
	}
}

func TestAgentInvalidValues(t *testing.T) {
	bad := map[string][]string{
		"AGENT_WORKERS":        {"x", "0", "-1", "65", "1.5"},
		"AGENT_MAX_STEPS":      {"x", "0", "-1", "201"},
		"AGENT_MAX_ATTEMPTS":   {"x", "0", "-1", "21"},
		"AGENT_MAX_RUNS":       {"x", "0", "-1", "1001"},
		"AGENT_LLM_MAX_TOKENS": {"x", "0", "63", "-1", "100001"},
		"AGENT_LEASE_TTL":      {"x", "500ms", "0s", "-1m", "61m", "5"},
		"AGENT_KILL_SWITCH":    {"maybe", "2"},
	}
	for name, vals := range bad {
		for _, v := range vals {
			_, err := Load(llmEnv(map[string]string{name: v, "AGENT_WORKERS_OTHER": "secret-value"}))
			if err == nil {
				t.Errorf("%s=%q: no error", name, v)
				continue
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("%s=%q: error %q does not name the variable", name, v, err)
			}
			if v != "" && strings.Contains(err.Error(), "secret-value") {
				t.Errorf("%s=%q: error echoes another value", name, v)
			}
		}
	}
}

func TestAgentEmptyMeansDefault(t *testing.T) {
	cfg, err := Load(llmEnv(map[string]string{"AGENT_WORKERS": "  ", "AGENT_KILL_SWITCH": ""}))
	if err != nil || cfg.Agent.Workers != 4 || cfg.Agent.KillSwitch {
		t.Errorf("%+v, %v", cfg.Agent, err)
	}
}

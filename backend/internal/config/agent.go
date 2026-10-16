package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// AgentConfig holds the settings of the agent loop.
type AgentConfig struct {
	Workers     int           // concurrent job workers
	MaxSteps    int           // model/tool steps per job
	MaxAttempts int           // crash attempts per job
	MaxRuns     int           // claims per job, the cap that stops requeue loops
	MaxTokens   int           // output limit per model call
	LeaseTTL    time.Duration // job lease duration
	KillSwitch  bool          // stop all agent work
}

func loadAgent(getenv func(string) string) (AgentConfig, error) {
	var a AgentConfig
	var err error
	ints := []struct {
		name     string
		def      int
		min, max int
		dst      *int
	}{
		{"AGENT_WORKERS", 4, 1, 64, &a.Workers},
		{"AGENT_MAX_STEPS", 30, 1, 200, &a.MaxSteps},
		{"AGENT_MAX_ATTEMPTS", 3, 1, 20, &a.MaxAttempts},
		{"AGENT_MAX_RUNS", 20, 1, 1000, &a.MaxRuns},
		{"AGENT_LLM_MAX_TOKENS", 4096, 64, 100000, &a.MaxTokens},
	}
	for _, f := range ints {
		*f.dst = f.def
		raw := strings.TrimSpace(getenv(f.name))
		if raw == "" {
			continue
		}
		n, perr := strconv.Atoi(raw)
		if perr != nil || n < f.min || n > f.max {
			return AgentConfig{}, fmt.Errorf("%s must be an integer from %d to %d", f.name, f.min, f.max)
		}
		*f.dst = n
	}

	a.LeaseTTL = 2 * time.Minute
	if raw := strings.TrimSpace(getenv("AGENT_LEASE_TTL")); raw != "" {
		if a.LeaseTTL, err = time.ParseDuration(raw); err != nil || a.LeaseTTL < time.Second || a.LeaseTTL > time.Hour {
			return AgentConfig{}, fmt.Errorf("AGENT_LEASE_TTL must be a duration from 1s to 1h")
		}
	}
	if raw := strings.TrimSpace(getenv("AGENT_KILL_SWITCH")); raw != "" {
		if a.KillSwitch, err = strconv.ParseBool(raw); err != nil {
			return AgentConfig{}, fmt.Errorf("AGENT_KILL_SWITCH must be a boolean")
		}
	}
	return a, nil
}

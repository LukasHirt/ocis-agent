package config

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// ProcessConfig holds the process-level settings: which roles run, the debug
// listener, the token and approval lifetimes and the scheduler cadence.
type ProcessConfig struct {
	Roles         []string      // subset of "api" and "worker", deduplicated, in the given order
	DebugAddr     string        // debug listener address; "" when disabled
	ApprovalTTL   time.Duration // how long an approval stays pending
	TokenTTL      time.Duration // lifetime of an app token
	DormancyDays  int           // days unseen before the dormant pause; 0 disables it
	ShutdownDrain time.Duration // how long shutdown waits for work in flight
	SweepInterval time.Duration // how often the token sweep visits each user
	LogLevel      string        // debug, info, warn or error

	WatchPollInterval time.Duration // how often each user's watch folders are checked
	WatchDebounce     time.Duration // how long the watch folders must stay unchanged before a file-change job
	WatchPollsPerTick int           // users whose watch folders one scheduler run checks
}

// Has reports whether the process runs the given role.
func (p ProcessConfig) Has(role string) bool {
	for _, r := range p.Roles {
		if r == role {
			return true
		}
	}
	return false
}

func loadProcess(getenv func(string) string) (ProcessConfig, error) {
	var p ProcessConfig
	get := func(name string) string { return strings.TrimSpace(getenv(name)) }

	p.Roles = []string{"api", "worker"}
	if raw := get("AGENT_ROLES"); raw != "" {
		var roles []string
		seen := map[string]bool{}
		for _, r := range strings.Split(raw, ",") {
			r = strings.TrimSpace(r)
			if r == "" {
				continue
			}
			if r != "api" && r != "worker" {
				return ProcessConfig{}, fmt.Errorf("AGENT_ROLES must be a comma-separated list of api and worker")
			}
			if !seen[r] {
				seen[r] = true
				roles = append(roles, r)
			}
		}
		if len(roles) == 0 {
			return ProcessConfig{}, fmt.Errorf("AGENT_ROLES must name at least one of api and worker")
		}
		p.Roles = roles
	}

	p.DebugAddr = ":9301"
	if raw := get("AGENT_DEBUG_ADDR"); raw == "off" {
		p.DebugAddr = ""
	} else if raw != "" {
		_, port, err := net.SplitHostPort(raw)
		n, perr := strconv.Atoi(port)
		if err != nil || perr != nil || n < 1 || n > 65535 {
			return ProcessConfig{}, fmt.Errorf("AGENT_DEBUG_ADDR must be host:port with a port from 1 to 65535, or off")
		}
		p.DebugAddr = raw
	}

	durations := []struct {
		name     string
		def      time.Duration
		min, max time.Duration
		bounds   string
		dst      *time.Duration
	}{
		{"AGENT_APPROVAL_TTL", 168 * time.Hour, time.Minute, 8760 * time.Hour, "1m to 8760h", &p.ApprovalTTL},
		{"AGENT_TOKEN_TTL", 72 * time.Hour, time.Hour, 720 * time.Hour, "1h to 720h", &p.TokenTTL},
		{"AGENT_SHUTDOWN_DRAIN", 30 * time.Second, 0, 10 * time.Minute, "0s to 10m", &p.ShutdownDrain},
		{"AGENT_TOKEN_SWEEP_INTERVAL", 15 * time.Minute, time.Minute, 24 * time.Hour, "1m to 24h", &p.SweepInterval},
		{"AGENT_WATCH_POLL_INTERVAL", 5 * time.Minute, time.Minute, 24 * time.Hour, "1m to 24h", &p.WatchPollInterval},
		{"AGENT_WATCH_DEBOUNCE", time.Minute, 10 * time.Second, 10 * time.Minute, "10s to 10m", &p.WatchDebounce},
	}
	for _, f := range durations {
		*f.dst = f.def
		raw := get(f.name)
		if raw == "" {
			continue
		}
		d, err := time.ParseDuration(raw)
		if err != nil || d < f.min || d > f.max {
			return ProcessConfig{}, fmt.Errorf("%s must be a duration from %s", f.name, f.bounds)
		}
		*f.dst = d
	}

	p.DormancyDays = 30
	if raw := get("AGENT_DORMANCY_DAYS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || n > 3650 {
			return ProcessConfig{}, fmt.Errorf("AGENT_DORMANCY_DAYS must be an integer from 0 to 3650")
		}
		p.DormancyDays = n
	}

	p.WatchPollsPerTick = 50
	if raw := get("AGENT_WATCH_MAX_POLLS_PER_TICK"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 1000 {
			return ProcessConfig{}, fmt.Errorf("AGENT_WATCH_MAX_POLLS_PER_TICK must be an integer from 1 to 1000")
		}
		p.WatchPollsPerTick = n
	}

	p.LogLevel = "info"
	if raw := get("AGENT_LOG_LEVEL"); raw != "" {
		switch raw {
		case "debug", "info", "warn", "error":
			p.LogLevel = raw
		default:
			return ProcessConfig{}, fmt.Errorf("AGENT_LOG_LEVEL must be one of debug, info, warn, error")
		}
	}
	return p, nil
}

package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestProcessDefaults(t *testing.T) {
	cfg, err := Load(llmEnv(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := ProcessConfig{
		Roles:         []string{"api", "worker"},
		DebugAddr:     ":9301",
		ApprovalTTL:   168 * time.Hour,
		TokenTTL:      72 * time.Hour,
		DormancyDays:  30,
		ShutdownDrain: 30 * time.Second,
		SweepInterval: 15 * time.Minute,
		LogLevel:      "info",
	}
	if !reflect.DeepEqual(cfg.Process, want) {
		t.Errorf("Process = %+v, want %+v", cfg.Process, want)
	}
	if !cfg.Process.Has("api") || !cfg.Process.Has("worker") || cfg.Process.Has("other") {
		t.Errorf("Has: %+v", cfg.Process)
	}
}

func TestProcessOverrides(t *testing.T) {
	cfg, err := Load(llmEnv(map[string]string{
		"AGENT_ROLES":                " worker , api ,worker",
		"AGENT_DEBUG_ADDR":           " 127.0.0.1:6060 ",
		"AGENT_APPROVAL_TTL":         " 1m ",
		"AGENT_TOKEN_TTL":            "720h",
		"AGENT_DORMANCY_DAYS":        " 3650 ",
		"AGENT_SHUTDOWN_DRAIN":       "10m",
		"AGENT_TOKEN_SWEEP_INTERVAL": "24h",
		"AGENT_LOG_LEVEL":            " debug ",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := ProcessConfig{
		Roles:         []string{"worker", "api"},
		DebugAddr:     "127.0.0.1:6060",
		ApprovalTTL:   time.Minute,
		TokenTTL:      720 * time.Hour,
		DormancyDays:  3650,
		ShutdownDrain: 10 * time.Minute,
		SweepInterval: 24 * time.Hour,
		LogLevel:      "debug",
	}
	if !reflect.DeepEqual(cfg.Process, want) {
		t.Errorf("Process = %+v, want %+v", cfg.Process, want)
	}

	cfg, err = Load(llmEnv(map[string]string{
		"AGENT_APPROVAL_TTL": "8760h", "AGENT_TOKEN_TTL": "1h", "AGENT_DORMANCY_DAYS": "0",
		"AGENT_SHUTDOWN_DRAIN": "0s", "AGENT_TOKEN_SWEEP_INTERVAL": "1m",
	}))
	if err != nil {
		t.Fatalf("Load lower bounds: %v", err)
	}
	p := cfg.Process
	if p.ApprovalTTL != 8760*time.Hour || p.TokenTTL != time.Hour || p.DormancyDays != 0 || p.ShutdownDrain != 0 || p.SweepInterval != time.Minute {
		t.Errorf("bounds: %+v", p)
	}
}

func TestProcessRoles(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
	}{
		{"worker", []string{"worker"}},
		{"api", []string{"api"}},
		{"api,worker", []string{"api", "worker"}},
		{"worker,api", []string{"worker", "api"}},
		{"api,api,api", []string{"api"}},
		{" api , , worker ", []string{"api", "worker"}},
		{"   ", []string{"api", "worker"}},
	} {
		cfg, err := Load(llmEnv(map[string]string{"AGENT_ROLES": c.in}))
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if !reflect.DeepEqual(cfg.Process.Roles, c.want) {
			t.Errorf("%q: Roles = %v, want %v", c.in, cfg.Process.Roles, c.want)
		}
	}
	cfg, err := Load(llmEnv(map[string]string{"AGENT_ROLES": "worker"}))
	if err != nil || cfg.Process.Has("api") || !cfg.Process.Has("worker") {
		t.Errorf("worker only: %+v, %v", cfg.Process, err)
	}
	for _, in := range []string{",", " , ,", "scheduler", "api,bogus", "API", "api;worker", "api worker"} {
		if _, err := Load(llmEnv(map[string]string{"AGENT_ROLES": in})); err == nil {
			t.Errorf("%q: no error", in)
		}
	}
}

func TestProcessDebugAddr(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"off", ""},
		{" off ", ""},
		{":9301", ":9301"},
		{":1", ":1"},
		{":65535", ":65535"},
		{"localhost:6060", "localhost:6060"},
		{"[::1]:6060", "[::1]:6060"},
	} {
		cfg, err := Load(llmEnv(map[string]string{"AGENT_DEBUG_ADDR": c.in}))
		if err != nil || cfg.Process.DebugAddr != c.want {
			t.Errorf("%q: %q, %v; want %q", c.in, cfg.Process.DebugAddr, err, c.want)
		}
	}
	for _, in := range []string{"9301", "localhost", ":0", ":65536", ":-1", ":http", "host:port:1", "OFF"} {
		if _, err := Load(llmEnv(map[string]string{"AGENT_DEBUG_ADDR": in})); err == nil {
			t.Errorf("%q: no error", in)
		}
	}
}

func TestProcessLogLevel(t *testing.T) {
	for _, l := range []string{"debug", "info", "warn", "error"} {
		cfg, err := Load(llmEnv(map[string]string{"AGENT_LOG_LEVEL": l}))
		if err != nil || cfg.Process.LogLevel != l {
			t.Errorf("%q: %q, %v", l, cfg.Process.LogLevel, err)
		}
	}
	cfg, err := Load(llmEnv(map[string]string{"AGENT_LOG_LEVEL": "  "}))
	if err != nil || cfg.Process.LogLevel != "info" {
		t.Errorf("empty: %q, %v", cfg.Process.LogLevel, err)
	}
	for _, l := range []string{"DEBUG", "warning", "trace", "0"} {
		if _, err := Load(llmEnv(map[string]string{"AGENT_LOG_LEVEL": l})); err == nil {
			t.Errorf("%q: no error", l)
		}
	}
}

func TestProcessInvalidValues(t *testing.T) {
	bad := map[string][]string{
		"AGENT_ROLES":                {"scheduler", ",", ",,"},
		"AGENT_DEBUG_ADDR":           {"9301", ":0", ":65536"},
		"AGENT_APPROVAL_TTL":         {"x", "59s", "0s", "-1h", "8761h", "5"},
		"AGENT_TOKEN_TTL":            {"x", "59m", "0s", "-1h", "721h", "5"},
		"AGENT_DORMANCY_DAYS":        {"x", "-1", "3651", "1.5", "30d"},
		"AGENT_SHUTDOWN_DRAIN":       {"x", "-1s", "10m1s", "601s", "5"},
		"AGENT_TOKEN_SWEEP_INTERVAL": {"x", "59s", "0s", "-1m", "24h1s", "1441m", "5"},
		"AGENT_LOG_LEVEL":            {"trace", "INFO"},
	}
	for name, vals := range bad {
		for _, v := range vals {
			_, err := Load(llmEnv(map[string]string{name: v}))
			if err == nil {
				t.Errorf("%s=%q: no error", name, v)
				continue
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("%s=%q: error %q does not name the variable", name, v, err)
			}
		}
	}
}

func TestProcessErrorsNeverEchoTheValue(t *testing.T) {
	// Distinctive values that cannot appear in a message by accident.
	for _, c := range []struct{ name, val string }{
		{"AGENT_ROLES", "zq7-bogus-role"},
		{"AGENT_ROLES", "api,zq7-bogus-role"},
		{"AGENT_DEBUG_ADDR", "zq7-host-9"},
		{"AGENT_DEBUG_ADDR", ":7777777"},
		{"AGENT_APPROVAL_TTL", "zq7-dur"},
		{"AGENT_APPROVAL_TTL", "7777777h"},
		{"AGENT_TOKEN_TTL", "zq7-dur"},
		{"AGENT_TOKEN_TTL", "7777777h"},
		{"AGENT_DORMANCY_DAYS", "zq7-days"},
		{"AGENT_DORMANCY_DAYS", "7777777"},
		{"AGENT_SHUTDOWN_DRAIN", "zq7-dur"},
		{"AGENT_SHUTDOWN_DRAIN", "7777777h"},
		{"AGENT_TOKEN_SWEEP_INTERVAL", "zq7-dur"},
		{"AGENT_TOKEN_SWEEP_INTERVAL", "7777777h"},
		{"AGENT_LOG_LEVEL", "zq7-level"},
	} {
		_, err := Load(llmEnv(map[string]string{c.name: c.val}))
		if err == nil {
			t.Errorf("%s=%q: no error", c.name, c.val)
			continue
		}
		if !strings.Contains(err.Error(), c.name) {
			t.Errorf("%s=%q: error %q does not name the variable", c.name, c.val, err)
		}
		for _, frag := range []string{"zq7", "7777777"} {
			if strings.Contains(err.Error(), frag) {
				t.Errorf("%s=%q: error %q echoes the bad value", c.name, c.val, err)
			}
		}
	}
}

func TestProcessDurationEdges(t *testing.T) {
	for _, c := range []struct {
		name     string
		min, max time.Duration
		get      func(ProcessConfig) time.Duration
	}{
		{"AGENT_APPROVAL_TTL", time.Minute, 8760 * time.Hour, func(p ProcessConfig) time.Duration { return p.ApprovalTTL }},
		{"AGENT_TOKEN_TTL", time.Hour, 720 * time.Hour, func(p ProcessConfig) time.Duration { return p.TokenTTL }},
		{"AGENT_SHUTDOWN_DRAIN", 0, 10 * time.Minute, func(p ProcessConfig) time.Duration { return p.ShutdownDrain }},
		{"AGENT_TOKEN_SWEEP_INTERVAL", time.Minute, 24 * time.Hour, func(p ProcessConfig) time.Duration { return p.SweepInterval }},
	} {
		for _, ok := range []time.Duration{c.min, c.max} {
			cfg, err := Load(llmEnv(map[string]string{c.name: ok.String()}))
			if err != nil || c.get(cfg.Process) != ok {
				t.Errorf("%s=%s: %v, %v", c.name, ok, c.get(cfg.Process), err)
			}
		}
		for _, bad := range []time.Duration{c.min - time.Second, c.max + time.Second} {
			if _, err := Load(llmEnv(map[string]string{c.name: bad.String()})); err == nil {
				t.Errorf("%s=%s: no error", c.name, bad)
			}
		}
	}
}

func TestProcessDormancyDaysEdges(t *testing.T) {
	for in, want := range map[string]int{"0": 0, "1": 1, "3649": 3649, "3650": 3650} {
		cfg, err := Load(llmEnv(map[string]string{"AGENT_DORMANCY_DAYS": in}))
		if err != nil || cfg.Process.DormancyDays != want {
			t.Errorf("%s: %d, %v", in, cfg.Process.DormancyDays, err)
		}
	}
	for _, in := range []string{"-1", "3651"} {
		if _, err := Load(llmEnv(map[string]string{"AGENT_DORMANCY_DAYS": in})); err == nil {
			t.Errorf("%s: no error", in)
		}
	}
}

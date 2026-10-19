package config

import (
	"strings"
	"testing"
)

func TestAPIDefaults(t *testing.T) {
	cfg, err := Load(llmEnv(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := APIConfig{Rate: 20, Burst: 40, UserDailyTokens: 200000, StreamsPerUser: 3, StreamsTotal: 200}
	if cfg.API != want {
		t.Errorf("API = %+v, want %+v", cfg.API, want)
	}
}

func TestAPIEmptyMeansDefault(t *testing.T) {
	cfg, err := Load(llmEnv(map[string]string{"AGENT_API_RATE": " ", "AGENT_API_BURST": "", "AGENT_USER_DAILY_TOKENS": "\t",
		"AGENT_API_STREAMS_PER_USER": " ", "AGENT_API_STREAMS_TOTAL": ""}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := (APIConfig{Rate: 20, Burst: 40, UserDailyTokens: 200000, StreamsPerUser: 3, StreamsTotal: 200}); cfg.API != want {
		t.Errorf("API = %+v, want %+v", cfg.API, want)
	}
}

func TestAPIBoundaries(t *testing.T) {
	for _, c := range []struct {
		name, v string
		ok      bool
		check   func(APIConfig) bool
	}{
		{"AGENT_API_RATE", "0.5", true, func(a APIConfig) bool { return a.Rate == 0.5 }},
		{"AGENT_API_RATE", " 1000 ", true, func(a APIConfig) bool { return a.Rate == 1000 }},
		{"AGENT_API_RATE", "0.49", false, nil},
		{"AGENT_API_RATE", "1000.01", false, nil},
		{"AGENT_API_BURST", "1", true, func(a APIConfig) bool { return a.Burst == 1 }},
		{"AGENT_API_BURST", "5000", true, func(a APIConfig) bool { return a.Burst == 5000 }},
		{"AGENT_API_BURST", "0", false, nil},
		{"AGENT_API_BURST", "5001", false, nil},
		{"AGENT_USER_DAILY_TOKENS", "1000", true, func(a APIConfig) bool { return a.UserDailyTokens == 1000 }},
		{"AGENT_USER_DAILY_TOKENS", "1000000000", true, func(a APIConfig) bool { return a.UserDailyTokens == 1000000000 }},
		{"AGENT_USER_DAILY_TOKENS", "999", false, nil},
		{"AGENT_USER_DAILY_TOKENS", "1000000001", false, nil},
		{"AGENT_API_STREAMS_PER_USER", "1", true, func(a APIConfig) bool { return a.StreamsPerUser == 1 }},
		{"AGENT_API_STREAMS_PER_USER", " 10 ", true, func(a APIConfig) bool { return a.StreamsPerUser == 10 }},
		{"AGENT_API_STREAMS_PER_USER", "0", false, nil},
		{"AGENT_API_STREAMS_PER_USER", "11", false, nil},
		{"AGENT_API_STREAMS_TOTAL", "1", true, func(a APIConfig) bool { return a.StreamsTotal == 1 }},
		{"AGENT_API_STREAMS_TOTAL", "5000", true, func(a APIConfig) bool { return a.StreamsTotal == 5000 }},
		{"AGENT_API_STREAMS_TOTAL", "0", false, nil},
		{"AGENT_API_STREAMS_TOTAL", "5001", false, nil},
	} {
		cfg, err := Load(llmEnv(map[string]string{c.name: c.v}))
		if (err == nil) != c.ok {
			t.Errorf("%s=%q: err = %v, want ok=%v", c.name, c.v, err, c.ok)
			continue
		}
		if c.ok && !c.check(cfg.API) {
			t.Errorf("%s=%q: API = %+v", c.name, c.v, cfg.API)
		}
	}
}

func TestAPIInvalidValues(t *testing.T) {
	bad := map[string][]string{
		"AGENT_API_RATE":             {"x", "0", "-1", "NaN", "Inf", "+Inf", "1e9", "0x10"},
		"AGENT_API_BURST":            {"x", "-1", "1.5", "1e3"},
		"AGENT_USER_DAILY_TOKENS":    {"x", "-1000", "1000.5", "99999999999999999999"},
		"AGENT_API_STREAMS_PER_USER": {"x", "-1", "1.5", "1e1", "+3", "0x3"},
		"AGENT_API_STREAMS_TOTAL":    {"x", "-1", "1.5", "1e3", "+3", "0x10"},
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

func TestAPIErrorsNeverEchoTheBadValue(t *testing.T) {
	const bad = "zzBADVALUEzz"
	for _, name := range []string{"AGENT_API_RATE", "AGENT_API_BURST", "AGENT_USER_DAILY_TOKENS",
		"AGENT_API_STREAMS_PER_USER", "AGENT_API_STREAMS_TOTAL"} {
		_, err := Load(llmEnv(map[string]string{name: bad}))
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		if strings.Contains(err.Error(), bad) {
			t.Errorf("%s: error %q echoes the value", name, err)
		}
	}
	// a numeric value out of range is not echoed either
	_, err := Load(llmEnv(map[string]string{"AGENT_USER_DAILY_TOKENS": "987654321987"}))
	if err == nil || strings.Contains(err.Error(), "987654321987") {
		t.Errorf("out of range: %v", err)
	}
	for _, name := range []string{"AGENT_API_STREAMS_PER_USER", "AGENT_API_STREAMS_TOTAL"} {
		_, err := Load(llmEnv(map[string]string{name: "987654"}))
		if err == nil || strings.Contains(err.Error(), "987654") {
			t.Errorf("%s out of range: %v", name, err)
		}
	}
}

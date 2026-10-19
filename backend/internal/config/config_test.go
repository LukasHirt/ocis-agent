package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

func key32() string {
	return base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
}

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"AGENT_OCIS_URL":   "https://cloud.example.com",
		"AGENT_MASTER_KEY": key32(),
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OCISURL != "https://cloud.example.com" {
		t.Errorf("OCISURL = %q", cfg.OCISURL)
	}
	if cfg.AllowedOrigin != "https://cloud.example.com" {
		t.Errorf("AllowedOrigin = %q, want OCISURL", cfg.AllowedOrigin)
	}
	if cfg.HTTPAddr != ":9300" {
		t.Errorf("HTTPAddr = %q", cfg.HTTPAddr)
	}
	if cfg.DataDir != "/var/lib/ocis-agent" {
		t.Errorf("DataDir = %q", cfg.DataDir)
	}
	if cfg.Insecure {
		t.Error("Insecure should default to false")
	}
	if len(cfg.MasterKey) != 32 {
		t.Errorf("MasterKey length = %d", len(cfg.MasterKey))
	}
}

func TestLoadNormalisesTrailingSlash(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"AGENT_OCIS_URL":   "https://cloud.example.com:9200/",
		"AGENT_MASTER_KEY": key32(),
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OCISURL != "https://cloud.example.com:9200" {
		t.Errorf("OCISURL = %q", cfg.OCISURL)
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"AGENT_OCIS_URL":       "https://cloud.example.com",
		"AGENT_ALLOWED_ORIGIN": "https://other.example.com",
		"AGENT_HTTP_ADDR":      "127.0.0.1:9999",
		"AGENT_DATA_DIR":       "/tmp/agent",
		"AGENT_INSECURE":       "true",
		"AGENT_MASTER_KEY":     key32(),
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AllowedOrigin != "https://other.example.com" || cfg.HTTPAddr != "127.0.0.1:9999" ||
		cfg.DataDir != "/tmp/agent" || !cfg.Insecure {
		t.Errorf("overrides not applied: %+v", cfg)
	}
}

func TestLoadErrors(t *testing.T) {
	good := map[string]string{
		"AGENT_OCIS_URL":   "https://cloud.example.com",
		"AGENT_MASTER_KEY": key32(),
	}
	with := func(k, v string) map[string]string {
		m := map[string]string{}
		for kk, vv := range good {
			m[kk] = vv
		}
		if v == "" {
			delete(m, k)
		} else {
			m[k] = v
		}
		return m
	}

	tests := []struct {
		name    string
		env     map[string]string
		wantVar string
	}{
		{"missing ocis url", with("AGENT_OCIS_URL", ""), "AGENT_OCIS_URL"},
		{"ocis url without scheme", with("AGENT_OCIS_URL", "cloud.example.com"), "AGENT_OCIS_URL"},
		{"ocis url with path", with("AGENT_OCIS_URL", "https://cloud.example.com/ocis"), "AGENT_OCIS_URL"},
		{"ocis url with query", with("AGENT_OCIS_URL", "https://cloud.example.com/?a=b"), "AGENT_OCIS_URL"},
		{"ocis url with userinfo", with("AGENT_OCIS_URL", "https://u:p@cloud.example.com"), "AGENT_OCIS_URL"},
		{"ftp scheme", with("AGENT_OCIS_URL", "ftp://cloud.example.com"), "AGENT_OCIS_URL"},
		{"allowed origin with path", with("AGENT_ALLOWED_ORIGIN", "https://x.example.com/a"), "AGENT_ALLOWED_ORIGIN"},
		{"missing master key", with("AGENT_MASTER_KEY", ""), "AGENT_MASTER_KEY"},
		{"master key not base64", with("AGENT_MASTER_KEY", "not base64 !!!"), "AGENT_MASTER_KEY"},
		{"master key hex", with("AGENT_MASTER_KEY", strings.Repeat("ab", 32)), "AGENT_MASTER_KEY"},
		{"master key 31 bytes", with("AGENT_MASTER_KEY", base64.StdEncoding.EncodeToString(make([]byte, 31))), "AGENT_MASTER_KEY"},
		{"master key 33 bytes", with("AGENT_MASTER_KEY", base64.StdEncoding.EncodeToString(make([]byte, 33))), "AGENT_MASTER_KEY"},
		{"bad insecure flag", with("AGENT_INSECURE", "maybe"), "AGENT_INSECURE"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(env(tc.env))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.wantVar) {
				t.Errorf("error %q does not name %s", err, tc.wantVar)
			}
			if mk := tc.env["AGENT_MASTER_KEY"]; mk != "" && strings.Contains(err.Error(), mk) {
				t.Errorf("error leaks the master key value: %q", err)
			}
		})
	}
}

func TestLoadNormalisesOrigins(t *testing.T) {
	for in, want := range map[string]string{
		"HTTPS://Cloud.Example.COM":      "https://cloud.example.com",
		"https://cloud.example.com:443/": "https://cloud.example.com",
		"http://Cloud.Example.com:80":    "http://cloud.example.com",
		"https://cloud.example.com:8443": "https://cloud.example.com:8443",
		"http://cloud.example.com:443":   "http://cloud.example.com:443",
		"https://cloud.example.com:80":   "https://cloud.example.com:80",
		"https://[2001:DB8::1]:443":      "https://[2001:db8::1]",
		"https://[2001:db8::1]:9200":     "https://[2001:db8::1]:9200",
		"https://localhost:9200/":        "https://localhost:9200",
	} {
		cfg, err := Load(env(map[string]string{
			"AGENT_OCIS_URL":       in,
			"AGENT_ALLOWED_ORIGIN": in,
			"AGENT_MASTER_KEY":     key32(),
		}))
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if cfg.OCISURL != want || cfg.AllowedOrigin != want {
			t.Errorf("%s: OCISURL %q, AllowedOrigin %q, want %q", in, cfg.OCISURL, cfg.AllowedOrigin, want)
		}
	}
}

func TestLoadRejectsAnEmptyPort(t *testing.T) {
	for _, name := range []string{"AGENT_OCIS_URL", "AGENT_ALLOWED_ORIGIN"} {
		m := map[string]string{"AGENT_OCIS_URL": "https://cloud.example.com", "AGENT_MASTER_KEY": key32()}
		m[name] = "https://zzhostzz:"
		_, err := Load(env(m))
		if err == nil || !strings.Contains(err.Error(), name) || strings.Contains(err.Error(), "zzhostzz") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

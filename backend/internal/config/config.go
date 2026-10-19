// Package config loads the sidecar configuration from environment variables.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
)

const masterKeyLen = 32

// Config is the validated sidecar configuration.
type Config struct {
	OCISURL       string
	AllowedOrigin string
	HTTPAddr      string
	DataDir       string
	SQLitePath    string
	Insecure      bool
	MasterKey     []byte
	LLM           LLMConfig
	Agent         AgentConfig
	Process       ProcessConfig
	API           APIConfig
}

// Load reads and validates the configuration. getenv is os.Getenv in
// production and a map lookup in tests. Errors name the variable but never
// include the master key.
func Load(getenv func(string) string) (Config, error) {
	var cfg Config

	raw := getenv("AGENT_OCIS_URL")
	if raw == "" {
		return Config{}, errors.New("AGENT_OCIS_URL is required")
	}
	ocisURL, err := parseOrigin("AGENT_OCIS_URL", raw)
	if err != nil {
		return Config{}, err
	}
	cfg.OCISURL = ocisURL

	cfg.AllowedOrigin = cfg.OCISURL
	if raw := getenv("AGENT_ALLOWED_ORIGIN"); raw != "" {
		origin, err := parseOrigin("AGENT_ALLOWED_ORIGIN", raw)
		if err != nil {
			return Config{}, err
		}
		cfg.AllowedOrigin = origin
	}

	cfg.HTTPAddr = valueOr(getenv("AGENT_HTTP_ADDR"), ":9300")
	cfg.DataDir = valueOr(getenv("AGENT_DATA_DIR"), "/var/lib/ocis-agent")
	cfg.SQLitePath = valueOr(getenv("AGENT_SQLITE_PATH"), filepath.Join(cfg.DataDir, "agent.db"))

	if raw := getenv("AGENT_INSECURE"); raw != "" {
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, errors.New("AGENT_INSECURE must be a boolean")
		}
		cfg.Insecure = b
	}

	rawKey := getenv("AGENT_MASTER_KEY")
	if rawKey == "" {
		return Config{}, errors.New("AGENT_MASTER_KEY is required (base64, 32 bytes)")
	}
	key, err := base64.StdEncoding.DecodeString(rawKey)
	if err != nil {
		return Config{}, errors.New("AGENT_MASTER_KEY must be standard base64")
	}
	if len(key) != masterKeyLen {
		return Config{}, fmt.Errorf("AGENT_MASTER_KEY must decode to %d bytes, got %d", masterKeyLen, len(key))
	}
	cfg.MasterKey = key

	if cfg.LLM, err = loadLLM(getenv); err != nil {
		return Config{}, err
	}

	if cfg.Agent, err = loadAgent(getenv); err != nil {
		return Config{}, err
	}

	if cfg.Process, err = loadProcess(getenv); err != nil {
		return Config{}, err
	}

	if cfg.API, err = loadAPI(getenv); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// parseOrigin accepts only scheme://host[:port] (an optional trailing slash is
// dropped) and returns it without the slash, the host in lower case and
// without the scheme's default port.
func parseOrigin(name, raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%s is not a valid URL", name)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("%s must start with http:// or https://", name)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%s must include a host", name)
	}
	if u.User != nil {
		return "", fmt.Errorf("%s must not contain credentials", name)
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%s must be scheme://host[:port] without a path, query or fragment", name)
	}
	// normalise to what browsers send in Origin: a lower-case host and no
	// default port
	host := strings.ToLower(u.Host)
	if (u.Scheme == "https" && u.Port() == "443") || (u.Scheme == "http" && u.Port() == "80") {
		host = strings.TrimSuffix(host, ":"+u.Port())
	}
	return u.Scheme + "://" + host, nil
}

func valueOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

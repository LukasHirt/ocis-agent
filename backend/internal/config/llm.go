package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/llm"
)

// readFile is replaced in tests.
var readFile = os.ReadFile

// LLMConfig is the model endpoint. It is optional until the worker role needs
// it: with no AGENT_LLM_* variable set it is disabled.
type LLMConfig struct {
	Provider     string
	BaseURL      string
	APIKey       llm.Secret
	Catalog      llm.Catalog // the default model and the models users may choose
	AllowedHosts []string    // empty: the host of BaseURL
	AllowHTTP    bool        // true only for an http:// loopback endpoint
	// Timeout bounds one model call (AGENT_LLM_TIMEOUT, default 30m with
	// streaming, else 120s).
	Timeout time.Duration
	// Stream requests streamed answers (AGENT_LLM_STREAM, default true).
	Stream bool
	// PlainTimeout bounds a call that is not streamed (streaming off or
	// refused, or a JSON answer to a stream request): Timeout if
	// AGENT_LLM_TIMEOUT is set, else 120s.
	PlainTimeout time.Duration
	// IdleTimeout ends a streamed call that receives nothing for this long
	// (AGENT_LLM_IDLE_TIMEOUT, default 60s).
	IdleTimeout time.Duration
	// ReasoningEffort is sent as reasoning_effort when not empty
	// (AGENT_LLM_REASONING_EFFORT).
	ReasoningEffort string
}

// Enabled reports whether a model endpoint is configured.
func (l LLMConfig) Enabled() bool { return l.BaseURL != "" }

func loadLLM(getenv func(string) string) (LLMConfig, error) {
	baseRaw := strings.TrimSpace(getenv("AGENT_LLM_BASE_URL"))
	provider := strings.TrimSpace(getenv("AGENT_LLM_PROVIDER"))
	keyRaw := strings.TrimSpace(getenv("AGENT_LLM_API_KEY"))
	keyFile := getenv("AGENT_LLM_API_KEY_FILE")
	model := strings.TrimSpace(getenv("AGENT_LLM_MODEL"))
	modelsRaw := getenv("AGENT_LLM_MODELS")
	hostsRaw := getenv("AGENT_LLM_ALLOWED_HOSTS")
	timeoutRaw := strings.TrimSpace(getenv("AGENT_LLM_TIMEOUT"))
	effort := strings.TrimSpace(getenv("AGENT_LLM_REASONING_EFFORT"))
	streamRaw := strings.TrimSpace(getenv("AGENT_LLM_STREAM"))
	idleRaw := strings.TrimSpace(getenv("AGENT_LLM_IDLE_TIMEOUT"))

	if baseRaw == "" {
		if provider != "" || keyRaw != "" || keyFile != "" || model != "" || modelsRaw != "" || hostsRaw != "" || timeoutRaw != "" || effort != "" ||
			streamRaw != "" || idleRaw != "" {
			return LLMConfig{}, errors.New("AGENT_LLM_BASE_URL is required when other AGENT_LLM_ variables are set")
		}
		return LLMConfig{}, nil
	}

	var l LLMConfig
	l.Provider = valueOr(provider, "openai")
	if l.Provider != "openai" {
		return LLMConfig{}, errors.New(`AGENT_LLM_PROVIDER must be "openai"`)
	}

	u, err := url.Parse(baseRaw)
	if err != nil || u.Host == "" || u.Hostname() == "" || strings.Contains(u.Host, "%") || (u.Scheme != "http" && u.Scheme != "https") {
		return LLMConfig{}, errors.New("AGENT_LLM_BASE_URL must be an http(s) URL with a host")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return LLMConfig{}, errors.New("AGENT_LLM_BASE_URL must not contain credentials, a query or a fragment")
	}
	loopback := llm.IsLoopbackHost(u.Host)
	if u.Scheme == "http" {
		if !loopback {
			return LLMConfig{}, errors.New("AGENT_LLM_BASE_URL must use https unless the host is loopback")
		}
		l.AllowHTTP = true
	}
	l.BaseURL = u.Scheme + "://" + u.Host + strings.TrimRight(u.EscapedPath(), "/")

	if model == "" {
		return LLMConfig{}, errors.New("AGENT_LLM_MODEL is required")
	}
	if !llm.ValidModelID(model) {
		return LLMConfig{}, errors.New("AGENT_LLM_MODEL is not a valid model id")
	}
	var extra []llm.ModelInfo
	for _, entry := range strings.Split(modelsRaw, ",") {
		if entry = strings.TrimSpace(entry); entry == "" {
			continue
		}
		id, label, _ := strings.Cut(entry, "=")
		extra = append(extra, llm.ModelInfo{ID: strings.TrimSpace(id), Label: strings.TrimSpace(label)})
	}
	if l.Catalog, err = llm.NewCatalog(model, extra); err != nil {
		return LLMConfig{}, errors.New("AGENT_LLM_MODELS is invalid: ids, labels, duplicates or size")
	}

	if keyRaw != "" && keyFile != "" {
		return LLMConfig{}, errors.New("AGENT_LLM_API_KEY and AGENT_LLM_API_KEY_FILE are mutually exclusive")
	}
	key := keyRaw
	if keyFile != "" {
		b, err := readFile(keyFile)
		if err != nil {
			return LLMConfig{}, fmt.Errorf("AGENT_LLM_API_KEY_FILE: %w", err)
		}
		key = strings.TrimSpace(string(b))
		if key == "" {
			return LLMConfig{}, errors.New("AGENT_LLM_API_KEY_FILE is empty")
		}
	}
	if key != "" && !llm.ValidAPIKey(key) {
		return LLMConfig{}, errors.New("AGENT_LLM_API_KEY (or AGENT_LLM_API_KEY_FILE) must be printable ASCII without spaces or line breaks")
	}
	if key == "" && !loopback {
		return LLMConfig{}, errors.New("AGENT_LLM_API_KEY (or AGENT_LLM_API_KEY_FILE) is required")
	}
	l.APIKey = llm.Secret(key)

	for _, h := range strings.Split(hostsRaw, ",") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			l.AllowedHosts = append(l.AllowedHosts, h)
		}
	}
	if !llm.HostAllowed(u.Host, l.AllowedHosts) {
		return LLMConfig{}, errors.New("AGENT_LLM_ALLOWED_HOSTS must contain the host of AGENT_LLM_BASE_URL")
	}

	if effort != "" && !llm.ValidReasoningEffort(effort) {
		return LLMConfig{}, errors.New("AGENT_LLM_REASONING_EFFORT must be none, low, medium or high")
	}
	l.ReasoningEffort = effort

	l.Stream = true
	if streamRaw != "" {
		if l.Stream, err = strconv.ParseBool(streamRaw); err != nil {
			return LLMConfig{}, errors.New("AGENT_LLM_STREAM must be a boolean")
		}
	}
	l.IdleTimeout = 60 * time.Second
	if idleRaw != "" {
		if l.IdleTimeout, err = time.ParseDuration(idleRaw); err != nil || l.IdleTimeout < 5*time.Second || l.IdleTimeout > 10*time.Minute {
			return LLMConfig{}, errors.New("AGENT_LLM_IDLE_TIMEOUT must be a duration from 5s to 10m")
		}
	}
	// with streaming the idle timeout is the stall guard, so the total
	// bound defaults to the maximum
	l.Timeout = 120 * time.Second
	if l.Stream {
		l.Timeout = 30 * time.Minute
	}
	l.PlainTimeout = 120 * time.Second
	if timeoutRaw != "" {
		if l.Timeout, err = time.ParseDuration(timeoutRaw); err != nil || l.Timeout < 10*time.Second || l.Timeout > 30*time.Minute {
			return LLMConfig{}, errors.New("AGENT_LLM_TIMEOUT must be a duration from 10s to 30m")
		}
		l.PlainTimeout = l.Timeout
	}
	if l.Stream && l.IdleTimeout >= l.Timeout {
		return LLMConfig{}, errors.New("AGENT_LLM_IDLE_TIMEOUT must be shorter than AGENT_LLM_TIMEOUT")
	}
	return l, nil
}

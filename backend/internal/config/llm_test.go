package config

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/llm"
)

func llmEnv(extra map[string]string) func(string) string {
	m := map[string]string{"AGENT_OCIS_URL": "https://cloud.example.com", "AGENT_MASTER_KEY": key32()}
	for k, v := range extra {
		m[k] = v
	}
	return env(m)
}

func TestLLMIsDisabledByDefault(t *testing.T) {
	cfg, err := Load(llmEnv(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLM.Enabled() {
		t.Errorf("LLM = %+v, want disabled", cfg.LLM)
	}
}

func TestLLMLoadsFromTheEnvironment(t *testing.T) {
	cfg, err := Load(llmEnv(map[string]string{
		"AGENT_LLM_BASE_URL":      "https://api.example.com/v1/",
		"AGENT_LLM_API_KEY":       " sk-1 ",
		"AGENT_LLM_MODEL":         "gpt-x",
		"AGENT_LLM_MODELS":        "gpt-x-mini=Fast, big , gpt-x=Balanced",
		"AGENT_LLM_ALLOWED_HOSTS": " API.example.com , other.example.com,, ",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	l := cfg.LLM
	if !l.Enabled() || l.Provider != "openai" || l.BaseURL != "https://api.example.com/v1" || l.Catalog.Default != "gpt-x" ||
		l.APIKey.Reveal() != "sk-1" || l.AllowHTTP {
		t.Errorf("LLM = %+v", l)
	}
	if want := []string{"api.example.com", "other.example.com"}; !reflect.DeepEqual(l.AllowedHosts, want) {
		t.Errorf("AllowedHosts = %v, want %v", l.AllowedHosts, want)
	}
	wantModels := []llm.ModelInfo{{ID: "gpt-x", Label: "Balanced"}, {ID: "gpt-x-mini", Label: "Fast"}, {ID: "big", Label: "big"}}
	if !reflect.DeepEqual(l.Catalog.Models, wantModels) {
		t.Errorf("Catalog.Models = %+v, want %+v", l.Catalog.Models, wantModels)
	}
}

func TestLLMKeyFromAFile(t *testing.T) {
	old := readFile
	defer func() { readFile = old }()
	readFile = func(name string) ([]byte, error) {
		if name != "/run/secrets/llm" {
			t.Errorf("read %q", name)
		}
		return []byte("sk-file\n"), nil
	}
	cfg, err := Load(llmEnv(map[string]string{
		"AGENT_LLM_BASE_URL":     "https://api.example.com",
		"AGENT_LLM_MODEL":        "m",
		"AGENT_LLM_API_KEY_FILE": "/run/secrets/llm",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLM.APIKey.Reveal() != "sk-file" {
		t.Errorf("key = %q", cfg.LLM.APIKey.Reveal())
	}
}

func TestLLMLoopbackNeedsNoKeyAndMayUseHTTP(t *testing.T) {
	cfg, err := Load(llmEnv(map[string]string{
		"AGENT_LLM_BASE_URL": "http://localhost:11434/v1",
		"AGENT_LLM_MODEL":    "llama",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.LLM.AllowHTTP || cfg.LLM.APIKey != "" || cfg.LLM.BaseURL != "http://localhost:11434/v1" {
		t.Errorf("LLM = %+v", cfg.LLM)
	}
}

func TestLLMRefusals(t *testing.T) {
	old := readFile
	defer func() { readFile = old }()
	readFile = func(name string) ([]byte, error) {
		switch name {
		case "/empty":
			return []byte(" \n"), nil
		case "/badkey":
			return []byte("sk\nX"), nil
		case "/missing":
			return nil, errors.New("open /missing: no such file")
		}
		return []byte("sk-file"), nil
	}
	ok := map[string]string{"AGENT_LLM_BASE_URL": "https://api.example.com", "AGENT_LLM_MODEL": "m", "AGENT_LLM_API_KEY": "sk-secret"}
	with := func(over map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range ok {
			m[k] = v
		}
		for k, v := range over {
			if v == "-" {
				delete(m, k)
			} else {
				m[k] = v
			}
		}
		return m
	}
	cases := []struct {
		label string
		env   map[string]string
		want  string
	}{
		{"model without base url", map[string]string{"AGENT_LLM_MODEL": "m"}, "AGENT_LLM_BASE_URL"},
		{"key without base url", map[string]string{"AGENT_LLM_API_KEY": "sk-secret"}, "AGENT_LLM_BASE_URL"},
		{"unknown provider", with(map[string]string{"AGENT_LLM_PROVIDER": "anthropic"}), "AGENT_LLM_PROVIDER"},
		{"no model", with(map[string]string{"AGENT_LLM_MODEL": "-"}), "AGENT_LLM_MODEL"},
		{"bad default model id", with(map[string]string{"AGENT_LLM_MODEL": "bad id"}), "AGENT_LLM_MODEL"},
		{"bad model id in the list", with(map[string]string{"AGENT_LLM_MODELS": "good,bad id=Label"}), "AGENT_LLM_MODELS"},
		{"duplicate model in the list", with(map[string]string{"AGENT_LLM_MODELS": "a,a"}), "AGENT_LLM_MODELS"},
		{"bad label in the list", with(map[string]string{"AGENT_LLM_MODELS": "a=x\ty"}), "AGENT_LLM_MODELS"},
		{"models without base url", map[string]string{"AGENT_LLM_MODELS": "a"}, "AGENT_LLM_BASE_URL"},
		{"no key on a remote host", with(map[string]string{"AGENT_LLM_API_KEY": "-"}), "AGENT_LLM_API_KEY"},
		{"key and file", with(map[string]string{"AGENT_LLM_API_KEY_FILE": "/k"}), "AGENT_LLM_API_KEY_FILE"},
		{"unreadable key file", with(map[string]string{"AGENT_LLM_API_KEY": "-", "AGENT_LLM_API_KEY_FILE": "/missing"}), "AGENT_LLM_API_KEY_FILE"},
		{"empty key file", with(map[string]string{"AGENT_LLM_API_KEY": "-", "AGENT_LLM_API_KEY_FILE": "/empty"}), "AGENT_LLM_API_KEY_FILE"},
		{"http on a remote host", with(map[string]string{"AGENT_LLM_BASE_URL": "http://api.example.com"}), "https"},
		{"credentials in the url", with(map[string]string{"AGENT_LLM_BASE_URL": "https://user:pw-secret@api.example.com"}), "AGENT_LLM_BASE_URL"},
		{"query in the url", with(map[string]string{"AGENT_LLM_BASE_URL": "https://api.example.com/v1?key=url-secret"}), "AGENT_LLM_BASE_URL"},
		{"fragment in the url", with(map[string]string{"AGENT_LLM_BASE_URL": "https://api.example.com/v1#x"}), "AGENT_LLM_BASE_URL"},
		{"allow list without the host", with(map[string]string{"AGENT_LLM_ALLOWED_HOSTS": "other.example.com"}), "AGENT_LLM_ALLOWED_HOSTS"},
		{"key with newline", with(map[string]string{"AGENT_LLM_API_KEY": "sk\nX"}), "AGENT_LLM_API_KEY"},
		{"key file with newline", with(map[string]string{"AGENT_LLM_API_KEY": "-", "AGENT_LLM_API_KEY_FILE": "/badkey"}), "AGENT_LLM_API_KEY_FILE"},
		{"empty hostname", with(map[string]string{"AGENT_LLM_BASE_URL": "https://:443/x"}), "AGENT_LLM_BASE_URL"},
		{"empty host", with(map[string]string{"AGENT_LLM_BASE_URL": "https:///x"}), "AGENT_LLM_BASE_URL"},
		{"key file alone", map[string]string{"AGENT_LLM_API_KEY_FILE": "/k"}, "AGENT_LLM_BASE_URL"},
		{"allowed hosts alone", map[string]string{"AGENT_LLM_ALLOWED_HOSTS": "a.example.com"}, "AGENT_LLM_BASE_URL"},
		{"not a url", with(map[string]string{"AGENT_LLM_BASE_URL": "api.example.com"}), "AGENT_LLM_BASE_URL"},
		{"ipv6 zone id", with(map[string]string{"AGENT_LLM_BASE_URL": "https://[fe80::1%25en0]:443/v1"}), "AGENT_LLM_BASE_URL"},
		{"timeout not a duration", with(map[string]string{"AGENT_LLM_TIMEOUT": "ten minutes"}), "AGENT_LLM_TIMEOUT"},
		{"timeout too short", with(map[string]string{"AGENT_LLM_TIMEOUT": "9s"}), "AGENT_LLM_TIMEOUT"},
		{"timeout too long", with(map[string]string{"AGENT_LLM_TIMEOUT": "31m"}), "AGENT_LLM_TIMEOUT"},
		{"timeout without base url", map[string]string{"AGENT_LLM_TIMEOUT": "5m"}, "AGENT_LLM_BASE_URL"},
		{"unknown reasoning effort", with(map[string]string{"AGENT_LLM_REASONING_EFFORT": "max"}), "AGENT_LLM_REASONING_EFFORT"},
		{"upper-case reasoning effort", with(map[string]string{"AGENT_LLM_REASONING_EFFORT": "LOW"}), "AGENT_LLM_REASONING_EFFORT"},
		{"reasoning effort without base url", map[string]string{"AGENT_LLM_REASONING_EFFORT": "low"}, "AGENT_LLM_BASE_URL"},
		{"stream not a boolean", with(map[string]string{"AGENT_LLM_STREAM": "yes"}), "AGENT_LLM_STREAM"},
		{"stream without base url", map[string]string{"AGENT_LLM_STREAM": "false"}, "AGENT_LLM_BASE_URL"},
		{"idle timeout not a duration", with(map[string]string{"AGENT_LLM_IDLE_TIMEOUT": "a minute"}), "AGENT_LLM_IDLE_TIMEOUT"},
		{"idle timeout too short", with(map[string]string{"AGENT_LLM_IDLE_TIMEOUT": "4s"}), "AGENT_LLM_IDLE_TIMEOUT"},
		{"idle timeout too long", with(map[string]string{"AGENT_LLM_IDLE_TIMEOUT": "11m"}), "AGENT_LLM_IDLE_TIMEOUT"},
		{"idle timeout negative", with(map[string]string{"AGENT_LLM_IDLE_TIMEOUT": "-1m"}), "AGENT_LLM_IDLE_TIMEOUT"},
		{"idle timeout without base url", map[string]string{"AGENT_LLM_IDLE_TIMEOUT": "1m"}, "AGENT_LLM_BASE_URL"},
		{"idle timeout longer than the timeout", with(map[string]string{"AGENT_LLM_IDLE_TIMEOUT": "5m", "AGENT_LLM_TIMEOUT": "2m"}), "AGENT_LLM_IDLE_TIMEOUT"},
		{"idle timeout equal to the timeout", with(map[string]string{"AGENT_LLM_IDLE_TIMEOUT": "2m", "AGENT_LLM_TIMEOUT": "2m"}), "AGENT_LLM_TIMEOUT"},
		{"default idle timeout over a short timeout", with(map[string]string{"AGENT_LLM_TIMEOUT": "30s"}), "AGENT_LLM_IDLE_TIMEOUT"},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			_, err := Load(llmEnv(c.env))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
			for _, leak := range []string{"sk-secret", "sk-file", "pw-secret", "url-secret"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("error %q leaks %q", err, leak)
				}
			}
		})
	}
}

func TestLLMPathIsKeptEscapedAndAcceptedByTheClient(t *testing.T) {
	cfg, err := Load(llmEnv(map[string]string{
		"AGENT_LLM_BASE_URL": "https://api.example.com/v1%3Fx/",
		"AGENT_LLM_API_KEY":  "sk-1",
		"AGENT_LLM_MODEL":    "m",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLM.BaseURL != "https://api.example.com/v1%3Fx" {
		t.Errorf("BaseURL = %q", cfg.LLM.BaseURL)
	}
	if _, err := llm.NewOpenAI(llm.OpenAIConfig{BaseURL: cfg.LLM.BaseURL, APIKey: cfg.LLM.APIKey, Model: "m", AllowedHosts: cfg.LLM.AllowedHosts}); err != nil {
		t.Errorf("NewOpenAI refuses what loadLLM accepted: %v", err)
	}
}

func TestLLMProviderIsTrimmed(t *testing.T) {
	cfg, err := Load(llmEnv(map[string]string{
		"AGENT_LLM_BASE_URL": "https://api.example.com", "AGENT_LLM_API_KEY": "sk-1", "AGENT_LLM_MODEL": "m", "AGENT_LLM_PROVIDER": " openai ",
	}))
	if err != nil || cfg.LLM.Provider != "openai" {
		t.Errorf("provider = %q, err = %v", cfg.LLM.Provider, err)
	}
}

func TestLLMUnreadableKeyFileDoesNotLeakTheKey(t *testing.T) {
	old := readFile
	defer func() { readFile = old }()
	readFile = func(name string) ([]byte, error) { return nil, errors.New("open " + name + ": permission denied") }
	_, err := Load(llmEnv(map[string]string{
		"AGENT_LLM_BASE_URL": "https://api.example.com", "AGENT_LLM_MODEL": "m", "AGENT_LLM_API_KEY_FILE": "/run/secrets/real-key-path",
	}))
	if err == nil || strings.Contains(err.Error(), "sk-real-key-value") {
		t.Fatalf("err = %v", err)
	}
	_, err = Load(llmEnv(map[string]string{
		"AGENT_LLM_BASE_URL": "https://api.example.com", "AGENT_LLM_MODEL": "m", "AGENT_LLM_API_KEY": "sk-real-key-value", "AGENT_LLM_ALLOWED_HOSTS": "other.example.com",
	}))
	if err == nil || strings.Contains(err.Error(), "sk-real-key-value") {
		t.Fatalf("err = %v", err)
	}
}

func TestLLMTimeoutAndReasoningEffort(t *testing.T) {
	base := map[string]string{"AGENT_LLM_BASE_URL": "http://localhost:11434/v1", "AGENT_LLM_MODEL": "qwen3"}
	cfg, err := Load(llmEnv(base))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLM.Timeout != 30*time.Minute || cfg.LLM.ReasoningEffort != "" {
		t.Errorf("defaults: Timeout = %v, ReasoningEffort = %q, want 30m (streaming) and empty", cfg.LLM.Timeout, cfg.LLM.ReasoningEffort)
	}
	for _, c := range []struct {
		timeout, effort string
		want            time.Duration
	}{{"10s", "none", 10 * time.Second}, {" 15m ", "low", 15 * time.Minute}, {"30m", " medium ", 30 * time.Minute}, {"2m", "high", 2 * time.Minute}} {
		env := map[string]string{"AGENT_LLM_TIMEOUT": c.timeout, "AGENT_LLM_REASONING_EFFORT": c.effort, "AGENT_LLM_IDLE_TIMEOUT": "5s"}
		for k, v := range base {
			env[k] = v
		}
		cfg, err := Load(llmEnv(env))
		if err != nil {
			t.Fatalf("Load(%q, %q): %v", c.timeout, c.effort, err)
		}
		if cfg.LLM.Timeout != c.want || cfg.LLM.ReasoningEffort != strings.TrimSpace(c.effort) {
			t.Errorf("Load(%q, %q): Timeout = %v, ReasoningEffort = %q", c.timeout, c.effort, cfg.LLM.Timeout, cfg.LLM.ReasoningEffort)
		}
	}
}

func TestLLMStreamingDefaultsAndOverrides(t *testing.T) {
	base := map[string]string{"AGENT_LLM_BASE_URL": "http://localhost:11434/v1", "AGENT_LLM_MODEL": "qwen3"}
	cfg, err := Load(llmEnv(base))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.LLM.Stream || cfg.LLM.IdleTimeout != 60*time.Second || cfg.LLM.Timeout != 30*time.Minute {
		t.Errorf("defaults: stream %v, idle %v, timeout %v", cfg.LLM.Stream, cfg.LLM.IdleTimeout, cfg.LLM.Timeout)
	}
	for _, c := range []struct {
		stream, idle string
		wantStream   bool
		wantIdle     time.Duration
	}{
		{" false ", "5s", false, 5 * time.Second},
		{"true", " 10m ", true, 10 * time.Minute},
		{"0", "90s", false, 90 * time.Second},
	} {
		m := map[string]string{"AGENT_LLM_STREAM": c.stream, "AGENT_LLM_IDLE_TIMEOUT": c.idle}
		for k, v := range base {
			m[k] = v
		}
		cfg, err := Load(llmEnv(m))
		if err != nil {
			t.Fatalf("%v: %v", c, err)
		}
		if cfg.LLM.Stream != c.wantStream || cfg.LLM.IdleTimeout != c.wantIdle {
			t.Errorf("%v: stream %v, idle %v", c, cfg.LLM.Stream, cfg.LLM.IdleTimeout)
		}
	}
}

func TestLLMTimeoutDefaultDependsOnStreaming(t *testing.T) {
	base := map[string]string{"AGENT_LLM_BASE_URL": "http://localhost:11434/v1", "AGENT_LLM_MODEL": "qwen3"}
	with := func(extra map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	cfg, err := Load(llmEnv(with(map[string]string{"AGENT_LLM_STREAM": "false"})))
	if err != nil || cfg.LLM.Timeout != 120*time.Second {
		t.Errorf("without streaming: timeout %v, err %v, want 120s", cfg.LLM.Timeout, err)
	}
	// without streaming the idle timeout is not used, so it need not be shorter
	cfg, err = Load(llmEnv(with(map[string]string{"AGENT_LLM_STREAM": "false", "AGENT_LLM_TIMEOUT": "10s", "AGENT_LLM_IDLE_TIMEOUT": "10m"})))
	if err != nil || cfg.LLM.Timeout != 10*time.Second {
		t.Errorf("without streaming: timeout %v, err %v", cfg.LLM.Timeout, err)
	}
	cfg, err = Load(llmEnv(with(map[string]string{"AGENT_LLM_TIMEOUT": "61s"})))
	if err != nil || cfg.LLM.Timeout != 61*time.Second || cfg.LLM.IdleTimeout != time.Minute {
		t.Errorf("streaming: timeout %v, idle %v, err %v", cfg.LLM.Timeout, cfg.LLM.IdleTimeout, err)
	}
}

// A call that is not streamed (streaming off or refused, or a JSON answer to
// a stream request) is bounded by 120s unless AGENT_LLM_TIMEOUT is set
// explicitly, never by the 30m streaming default.
func TestLLMPlainTimeout(t *testing.T) {
	base := map[string]string{"AGENT_LLM_BASE_URL": "http://localhost:11434/v1", "AGENT_LLM_MODEL": "qwen3"}
	for _, c := range []struct {
		extra map[string]string
		want  time.Duration
	}{
		{nil, 120 * time.Second},
		{map[string]string{"AGENT_LLM_STREAM": "false"}, 120 * time.Second},
		{map[string]string{"AGENT_LLM_TIMEOUT": "20m"}, 20 * time.Minute},
		{map[string]string{"AGENT_LLM_TIMEOUT": "30m"}, 30 * time.Minute},
		{map[string]string{"AGENT_LLM_STREAM": "false", "AGENT_LLM_TIMEOUT": "15s"}, 15 * time.Second},
	} {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		for k, v := range c.extra {
			env[k] = v
		}
		cfg, err := Load(llmEnv(env))
		if err != nil || cfg.LLM.PlainTimeout != c.want {
			t.Errorf("%v: plain timeout %v, err %v, want %v", c.extra, cfg.LLM.PlainTimeout, err, c.want)
		}
	}
}

package config

import (
	"errors"
	"reflect"
	"strings"
	"testing"

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

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidateChecksCallIDsAndAnswers(t *testing.T) {
	call := func(id string) Message {
		return Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: id, Name: "files.list", Arguments: "{}"}}}
	}
	answer := func(id string) Message { return Message{Role: RoleTool, ToolCallID: id, Content: "[]"} }
	cases := map[string][]Message{
		"newline id":     {call("a\nb"), answer("a")},
		"non-ascii id":   {call("caf\u00e9")},
		"long id":        {call(strings.Repeat("x", 129))},
		"duplicate id":   {call("c1"), answer("c1"), call("c1")},
		"unknown result": {call("c1"), answer("zzz")},
		"double answer":  {call("c1"), answer("c1"), answer("c1")},
		"answer before":  {answer("c1"), call("c1")},
		"bad result id":  {call("c1"), answer("c1\n")},
	}
	for label, msgs := range cases {
		t.Run(label, func(t *testing.T) {
			r := okReq()
			r.Messages = append(r.Messages, msgs...)
			if err := r.Validate(); !errors.Is(err, ErrInvalid) {
				t.Errorf("Validate = %v, want ErrInvalid", err)
			}
		})
	}
	r := okReq()
	r.Messages = append(r.Messages, call("c1"), call("c2"), answer("c2"))
	if err := r.Validate(); err != nil {
		t.Errorf("unanswered calls must be allowed: %v", err)
	}
}

func TestADecodedUnknownToolCallCanBeEchoedBack(t *testing.T) {
	var body string
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := new(strings.Builder)
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		x, _ := json.Marshal(m)
		b.Write(x)
		body = b.String()
		n++
		if n == 1 {
			reply(w, `{"choices":[{"message":{"tool_calls":[{"id":"c1","type":"function","function":{"name":"Bad Name","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		reply(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	o := newClient(t, srv.URL, nil)
	r1, err := o.Complete(context.Background(), simpleReq())
	if err != nil {
		t.Fatal(err)
	}
	req := simpleReq()
	req.Messages = append(req.Messages, r1.Message, Message{Role: RoleTool, ToolCallID: "c1", Content: "denied"})
	if _, err := o.Complete(context.Background(), req); err != nil {
		t.Fatalf("echo: %v", err)
	}
	if !strings.Contains(body, `"llm__invalid_tool_name"`) {
		t.Errorf("wire body %s lacks llm__invalid_tool_name", body)
	}
}

func TestNewOpenAIFinalHardening(t *testing.T) {
	base := OpenAIConfig{BaseURL: "https://api.example.com", APIKey: "k", Model: "m"}
	with := func(f func(*OpenAIConfig)) OpenAIConfig { c := base; f(&c); return c }
	bad := map[string]OpenAIConfig{
		"key with newline": with(func(c *OpenAIConfig) { c.APIKey = "sk\nX" }),
		"key with space":   with(func(c *OpenAIConfig) { c.APIKey = "sk X" }),
		"key non-ascii":    with(func(c *OpenAIConfig) { c.APIKey = "sk-\u00e9" }),
		"empty hostname":   with(func(c *OpenAIConfig) { c.BaseURL = "https://:443/x" }),
		"model with nl":    with(func(c *OpenAIConfig) { c.Model = "bad id\n" }),
	}
	for label, cfg := range bad {
		t.Run(label, func(t *testing.T) {
			_, err := NewOpenAI(cfg)
			if err == nil {
				t.Fatal("expected an error")
			}
			if strings.Contains(err.Error(), "sk") && strings.Contains(err.Error(), "X") {
				t.Errorf("error %q echoes the key", err)
			}
		})
	}
	o, err := NewOpenAI(with(func(c *OpenAIConfig) { c.BaseURL = "https://api.example.com/v1%3Fx" }))
	if err != nil {
		t.Fatal(err)
	}
	if o.endpoint != "https://api.example.com/v1%3Fx/chat/completions" {
		t.Errorf("endpoint = %q", o.endpoint)
	}
}

func TestHostAllowed(t *testing.T) {
	cases := []struct {
		host    string
		allowed []string
		want    bool
	}{
		{"api.example.com", nil, true},
		{"API.example.com:8443", []string{"api.example.com"}, true},
		{"api.example.com:8443", []string{"api.example.com:8443"}, true},
		{"api.example.com:8443", []string{"api.example.com:9"}, false},
		{"[::1]:8443", []string{"[::1]"}, true},
		{"[::1]:8443", []string{"[::1]:8443"}, true},
		{"api.example.com", []string{"other.example.com"}, false},
	}
	for _, c := range cases {
		if got := HostAllowed(c.host, c.allowed); got != c.want {
			t.Errorf("HostAllowed(%q, %v) = %v, want %v", c.host, c.allowed, got, c.want)
		}
	}
}

func TestValidAPIKey(t *testing.T) {
	for _, k := range []string{"sk-abc_123.XYZ"} {
		if !ValidAPIKey(k) {
			t.Errorf("ValidAPIKey(%q) = false", k)
		}
	}
	for _, k := range []string{"", "a b", "a\nb", "a\x00b", "\u00e9", "a\x7fb"} {
		if ValidAPIKey(k) {
			t.Errorf("ValidAPIKey(%q) = true", k)
		}
	}
}

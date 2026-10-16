package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func reply(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

func newClient(t *testing.T, url string, mutate func(*OpenAIConfig)) *OpenAI {
	t.Helper()
	cfg := OpenAIConfig{BaseURL: url, APIKey: "sk-test", Model: "m1", AllowHTTP: true}
	if mutate != nil {
		mutate(&cfg)
	}
	o, err := NewOpenAI(cfg)
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
	}
	return o
}

func simpleReq() CompletionRequest {
	return CompletionRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}}
}

// at walks a decoded JSON value by object keys and array indexes.
func at(t *testing.T, v any, path ...any) any {
	t.Helper()
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("path %v: not an object at %q", path, k)
			}
			v = m[k]
		case int:
			s, ok := v.([]any)
			if !ok || k >= len(s) {
				t.Fatalf("path %v: no index %d", path, k)
			}
			v = s[k]
		}
	}
	return v
}

func TestCompleteSendsAnOpenAICompatibleRequest(t *testing.T) {
	var gotPath, gotAuth, gotType string
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotType = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&got)
		reply(w, `{"choices":[{"message":{"content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":3}}`)
	}))
	defer srv.Close()

	o := newClient(t, srv.URL+"/v1/", nil)
	resp, err := o.Complete(context.Background(), CompletionRequest{
		Messages: []Message{
			{Role: RoleSystem, Content: "sys"},
			{Role: RoleUser, Content: "list"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1", Name: "files.list", Arguments: `{"path":"/"}`}, {ID: "c2", Name: "memory.read"}}},
			{Role: RoleTool, ToolCallID: "c1", Content: "[]"},
		},
		Tools:     []ToolSpec{{Name: "files.list", Description: "List", Schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)}, {Name: "memory.read"}},
		MaxTokens: 256,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotPath != "/v1/chat/completions" || gotAuth != "Bearer sk-test" || gotType != "application/json" {
		t.Errorf("path/auth/type = %q %q %q", gotPath, gotAuth, gotType)
	}
	if at(t, got, "model") != "m1" || at(t, got, "max_tokens") != float64(256) {
		t.Errorf("model/max_tokens wrong: %v", got)
	}
	if at(t, got, "messages", 2, "content") != nil {
		t.Error("an assistant message with calls and no text must send content null")
	}
	if at(t, got, "messages", 2, "tool_calls", 0, "type") != "function" ||
		at(t, got, "messages", 2, "tool_calls", 0, "function", "name") != "files__list" ||
		at(t, got, "messages", 2, "tool_calls", 0, "function", "arguments") != `{"path":"/"}` {
		t.Errorf("assistant tool call wrongly encoded: %v", at(t, got, "messages", 2))
	}
	if at(t, got, "messages", 2, "tool_calls", 1, "function", "arguments") != "{}" {
		t.Error("empty arguments must be sent as {}")
	}
	if at(t, got, "messages", 3, "tool_call_id") != "c1" || at(t, got, "messages", 3, "content") != "[]" {
		t.Errorf("tool result wrongly encoded: %v", at(t, got, "messages", 3))
	}
	if at(t, got, "tools", 0, "type") != "function" || at(t, got, "tools", 0, "function", "name") != "files__list" ||
		at(t, got, "tools", 0, "function", "description") != "List" ||
		at(t, got, "tools", 0, "function", "parameters", "type") != "object" {
		t.Errorf("tool wrongly encoded: %v", at(t, got, "tools", 0))
	}
	if at(t, got, "tools", 1, "function", "parameters", "type") != "object" {
		t.Error("a tool without a schema must send an empty object schema")
	}
	if resp.Message.Role != RoleAssistant || resp.Message.Content != "hello" || resp.Usage != (Usage{In: 11, Out: 3}) || resp.Stop != StopEnd {
		t.Errorf("response = %+v", resp)
	}
}

func TestCompleteHonoursAPerRequestModel(t *testing.T) {
	var models []any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		_ = json.NewDecoder(r.Body).Decode(&got)
		models = append(models, got["model"])
		reply(w, `{"choices":[{"message":{"content":"x"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	o := newClient(t, srv.URL, nil)
	r := simpleReq()
	if _, err := o.Complete(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	r.Model = "other-model"
	if _, err := o.Complete(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0] != "m1" || models[1] != "other-model" {
		t.Errorf("models sent = %v, want [m1 other-model]", models)
	}
}

func TestCompleteOmitsAuthorizationAndMaxTokensWhenUnset(t *testing.T) {
	var auth string
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		reply(w, `{"choices":[{"message":{"content":"x"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	o := newClient(t, srv.URL, func(c *OpenAIConfig) { c.APIKey = "" })
	if _, err := o.Complete(context.Background(), simpleReq()); err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		t.Errorf("Authorization = %q", auth)
	}
	if _, ok := got["max_tokens"]; ok {
		t.Error("max_tokens must be omitted when 0")
	}
	if _, ok := got["tools"]; ok {
		t.Error("tools must be omitted when there are none")
	}
}

func TestCompleteDecodesToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply(w, `{"choices":[{"message":{"content":null,"tool_calls":[
			{"id":"a","type":"function","function":{"name":"files__list_spaces","arguments":"{\"path\":\"/\"}"}},
			{"id":"","function":{"name":"memory__read","arguments":""}}]},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":-5,"completion_tokens":7}}`)
	}))
	defer srv.Close()
	resp, err := newClient(t, srv.URL, nil).Complete(context.Background(), simpleReq())
	if err != nil {
		t.Fatal(err)
	}
	if resp.Stop != StopToolCalls || len(resp.Message.ToolCalls) != 2 {
		t.Fatalf("resp = %+v", resp)
	}
	c0, c1 := resp.Message.ToolCalls[0], resp.Message.ToolCalls[1]
	if c0.ID != "a" || c0.Name != "files.list_spaces" || c0.Arguments != `{"path":"/"}` {
		t.Errorf("call 0 = %+v", c0)
	}
	if c1.ID != "call_1" || c1.Name != "memory.read" || c1.Arguments != "{}" {
		t.Errorf("call 1 = %+v", c1)
	}
	if resp.Usage != (Usage{In: 0, Out: 7}) {
		t.Errorf("usage = %+v, want negative clamped to 0", resp.Usage)
	}
}

func TestCompleteFinishReasons(t *testing.T) {
	cases := []struct {
		body string
		want StopReason
		err  error
	}{
		{`{"choices":[{"message":{"content":"x"},"finish_reason":"length"}]}`, StopLength, nil},
		{`{"choices":[{"message":{"content":"x"},"finish_reason":""}]}`, StopEnd, nil},
		{`{"choices":[{"message":{"content":"x"}}]}`, StopEnd, nil},
		{`{"choices":[{"message":{"content":"x"},"finish_reason":"function_call"}]}`, StopEnd, nil},
		{`{"choices":[{"message":{"content":""},"finish_reason":"content_filter"}]}`, "", ErrRejected},
		{`{"choices":[{"message":{"content":"x"},"finish_reason":"weird"}]}`, "", ErrMalformed},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reply(w, c.body) }))
		resp, err := newClient(t, srv.URL, nil).Complete(context.Background(), simpleReq())
		srv.Close()
		if c.err != nil {
			if !errors.Is(err, c.err) {
				t.Errorf("%s: err = %v, want %v", c.body, err, c.err)
			}
			continue
		}
		if err != nil || resp.Stop != c.want {
			t.Errorf("%s: got %+v, %v; want stop %q", c.body, resp, err, c.want)
		}
	}
}

func TestCompleteRefusesMalformedResponses(t *testing.T) {
	call := func(id, typ, name string) string {
		return fmt.Sprintf(`{"id":%q,"type":%q,"function":{"name":%q,"arguments":"{}"}}`, id, typ, name)
	}
	long := strings.Repeat("x", 129)
	cases := map[string]string{
		"not json":            `nope`,
		"no choices":          `{"choices":[]}`,
		"tool_calls no calls": `{"choices":[{"message":{"content":""},"finish_reason":"tool_calls"}]}`,
		"duplicate ids":       `{"choices":[{"message":{"tool_calls":[` + call("x", "function", "a__b") + `,` + call("x", "function", "a__b") + `]},"finish_reason":"tool_calls"}]}`,
		"nameless call":       `{"choices":[{"message":{"tool_calls":[` + call("x", "function", "") + `]},"finish_reason":"tool_calls"}]}`,
		"bad call type":       `{"choices":[{"message":{"tool_calls":[` + call("x", "custom", "a__b") + `]},"finish_reason":"tool_calls"}]}`,
		"id too long":         `{"choices":[{"message":{"tool_calls":[` + call(long, "function", "a__b") + `]},"finish_reason":"tool_calls"}]}`,
		"too many calls":      `{"choices":[{"message":{"tool_calls":[` + call("1", "function", "a__b") + `,` + call("2", "function", "a__b") + `]},"finish_reason":"tool_calls"}]}`,
	}
	for label, body := range cases {
		t.Run(label, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reply(w, body) }))
			defer srv.Close()
			o := newClient(t, srv.URL, func(c *OpenAIConfig) { c.MaxToolCalls = 1 })
			if label == "duplicate ids" {
				o = newClient(t, srv.URL, nil)
			}
			_, err := o.Complete(context.Background(), simpleReq())
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("err = %v, want ErrMalformed", err)
			}
		})
	}
}

func TestCompleteMapsStatusCodesAndLeaksNothing(t *testing.T) {
	cases := map[int]error{
		401: ErrAuth, 403: ErrAuth, 429: ErrRateLimited, 408: ErrUnavailable, 500: ErrUnavailable,
		503: ErrUnavailable, 400: ErrRejected, 404: ErrRejected, 302: ErrRejected, 204: ErrRejected,
	}
	for code, want := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte("secret-body sk-test"))
		}))
		_, err := newClient(t, srv.URL, nil).Complete(context.Background(), simpleReq())
		srv.Close()
		if !errors.Is(err, want) {
			t.Errorf("status %d: err = %v, want %v", code, err, want)
			continue
		}
		for _, leak := range []string{"secret-body", "sk-test", srv.URL} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("status %d: error %q leaks %q", code, err, leak)
			}
		}
	}
}

func TestCompleteNeverFollowsRedirects(t *testing.T) {
	hit := false
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	_, err := newClient(t, srv.URL, nil).Complete(context.Background(), simpleReq())
	if !errors.Is(err, ErrRejected) || hit {
		t.Fatalf("err = %v, redirect target hit = %v", err, hit)
	}
}

func TestCompleteCapsTheResponseSize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply(w, `{"choices":[],"pad":"`+strings.Repeat("x", 1000)+`"}`)
	}))
	defer srv.Close()
	o := newClient(t, srv.URL, func(c *OpenAIConfig) { c.MaxResponseBytes = 100 })
	if _, err := o.Complete(context.Background(), simpleReq()); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

func TestCompleteReportsACancelledContextAsSuch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The server only notices a closed connection after the body is read.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	_, err := newClient(t, srv.URL, nil).Complete(ctx, simpleReq())
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want context.Canceled and not ErrUnavailable", err)
	}
}

func TestCompleteReportsATransportFailureAsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	_, err := newClient(t, url, nil).Complete(context.Background(), simpleReq())
	if !errors.Is(err, ErrUnavailable) || !Retryable(err) {
		t.Fatalf("err = %v, want a retryable ErrUnavailable", err)
	}
	if strings.Contains(err.Error(), url) {
		t.Errorf("error %q leaks the URL", err)
	}
}

func TestCompleteRefusesAnInvalidRequestWithoutSendingIt(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer srv.Close()
	r := simpleReq()
	r.Tools = []ToolSpec{{Name: "Bad__Name"}}
	if _, err := newClient(t, srv.URL, nil).Complete(context.Background(), r); !errors.Is(err, ErrInvalid) || hit {
		t.Fatalf("err = %v, server hit = %v", err, hit)
	}
}

func TestNewOpenAIValidatesItsConfiguration(t *testing.T) {
	good := OpenAIConfig{BaseURL: "https://api.example.com/v1", APIKey: "k", Model: "m"}
	with := func(mutate func(*OpenAIConfig)) OpenAIConfig {
		c := good
		mutate(&c)
		return c
	}
	bad := map[string]OpenAIConfig{
		"not a url":          with(func(c *OpenAIConfig) { c.BaseURL = "::" }),
		"no host":            with(func(c *OpenAIConfig) { c.BaseURL = "https://" }),
		"ftp":                with(func(c *OpenAIConfig) { c.BaseURL = "ftp://api.example.com" }),
		"http without allow": with(func(c *OpenAIConfig) { c.BaseURL = "http://api.example.com" }),
		"credentials":        with(func(c *OpenAIConfig) { c.BaseURL = "https://u:p@api.example.com" }),
		"query":              with(func(c *OpenAIConfig) { c.BaseURL = "https://api.example.com/v1?x=1" }),
		"fragment":           with(func(c *OpenAIConfig) { c.BaseURL = "https://api.example.com/v1#x" }),
		"no model":           with(func(c *OpenAIConfig) { c.Model = "" }),
		"host not allowed":   with(func(c *OpenAIConfig) { c.AllowedHosts = []string{"other.example.com"} }),
		"no key remote":      with(func(c *OpenAIConfig) { c.APIKey = "" }),
	}
	for label, cfg := range bad {
		t.Run(label, func(t *testing.T) {
			if _, err := NewOpenAI(cfg); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
	okCases := map[string]OpenAIConfig{
		"plain": good,
		"allow list with port": with(func(c *OpenAIConfig) {
			c.BaseURL = "https://api.example.com:8443"
			c.AllowedHosts = []string{"API.example.com:8443"}
		}),
		"allow list host only": with(func(c *OpenAIConfig) {
			c.BaseURL = "https://api.example.com:8443"
			c.AllowedHosts = []string{"api.example.com"}
		}),
		"loopback http no key":  {BaseURL: "http://localhost:11434/v1", Model: "m", AllowHTTP: true},
		"loopback ip no key":    {BaseURL: "http://127.0.0.1:8080", Model: "m", AllowHTTP: true},
		"loopback https no key": {BaseURL: "https://[::1]:8443", Model: "m"},
	}
	for label, cfg := range okCases {
		t.Run(label, func(t *testing.T) {
			if _, err := NewOpenAI(cfg); err != nil {
				t.Fatalf("NewOpenAI: %v", err)
			}
		})
	}
}

func TestIsLoopbackHost(t *testing.T) {
	for _, h := range []string{"localhost", "LOCALHOST:11434", "127.0.0.1", "127.0.0.1:80", "[::1]:80", "::1"} {
		if !IsLoopbackHost(h) {
			t.Errorf("IsLoopbackHost(%q) = false", h)
		}
	}
	for _, h := range []string{"", "example.com", "example.com:80", "10.0.0.1", "localhost.example.com", "0.0.0.0"} {
		if IsLoopbackHost(h) {
			t.Errorf("IsLoopbackHost(%q) = true", h)
		}
	}
}

func TestTheKeyDoesNotAppearWhenTheClientOrConfigIsPrinted(t *testing.T) {
	cfg := OpenAIConfig{BaseURL: "https://api.example.com", APIKey: "sk-very-secret", Model: "m"}
	o, err := NewOpenAI(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{fmt.Sprintf("%v", o), fmt.Sprintf("%+v", o), fmt.Sprintf("%#v", o), fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg)} {
		if strings.Contains(out, "sk-very-secret") {
			t.Errorf("printed output %q leaks the key", out)
		}
	}
}

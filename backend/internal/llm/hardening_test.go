package llm

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestValidateBoundsTheWireNameLength(t *testing.T) {
	r := func(name string) CompletionRequest {
		x := simpleReq()
		x.Tools = []ToolSpec{{Name: name}}
		return x
	}
	dotted := strings.Repeat("a.", 30) + "a" // 61 bytes, 91 on the wire
	if err := r(dotted).Validate(); !errors.Is(err, ErrInvalid) {
		t.Errorf("61-byte dotted name: err = %v, want ErrInvalid", err)
	}
	boundary := strings.Repeat("a", 31) + "." + strings.Repeat("a", 31) // 63 bytes, 64 on the wire? 63+1
	if err := r(boundary).Validate(); err != nil {
		t.Errorf("64-byte wire name refused: %v", err)
	}
	if err := r(boundary + "a").Validate(); !errors.Is(err, ErrInvalid) {
		t.Errorf("65-byte wire name accepted")
	}
	if err := r(strings.Repeat("a", 62)).Validate(); err != nil {
		t.Errorf("62-byte name without dots refused: %v", err)
	}
}

func TestDecodeDistrustsToolNamesAndIDs(t *testing.T) {
	mk := func(id, name string) string {
		return fmt.Sprintf(`{"choices":[{"message":{"tool_calls":[{"id":%q,"type":"function","function":{"name":%q,"arguments":"{\"k\":1}"}}]},"finish_reason":"tool_calls"}]}`, id, name)
	}
	run := func(body string) (CompletionResponse, error) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reply(w, body) }))
		defer srv.Close()
		return newClient(t, srv.URL, nil).Complete(context.Background(), simpleReq())
	}
	for _, id := range []string{"a\x00b", "a\nb", "caf\u00e9", "a b"} {
		if _, err := run(mk(id, "files__list")); !errors.Is(err, ErrMalformed) {
			t.Errorf("id %q: err = %v, want ErrMalformed", id, err)
		}
	}
	for _, name := range []string{"files___list", "a____b", "Files__List", "rm -rf", "files\u202elist", strings.Repeat("a", 5000)} {
		resp, err := run(mk("c1", name))
		if err != nil {
			t.Errorf("name %.20q: %v", name, err)
			continue
		}
		c := resp.Message.ToolCalls[0]
		if c.Name != UnknownTool || c.Arguments != `{"k":1}` {
			t.Errorf("name %.20q: call = %+v", name, c)
		}
	}
	resp, err := run(mk("c.1:x-y_z", "files__list_spaces"))
	if err != nil || resp.Message.ToolCalls[0].Name != "files.list_spaces" {
		t.Errorf("good name: %+v %v", resp, err)
	}
	if !validName(UnknownTool) {
		t.Error("UnknownTool must satisfy the grammar")
	}
}

func TestNewOpenAIGivesACustomClientATimeout(t *testing.T) {
	o := newClient(t, "http://127.0.0.1:1", func(c *OpenAIConfig) { c.HTTPClient = &http.Client{} })
	if o.client.Timeout == 0 {
		t.Error("a client without a timeout must get the default")
	}
	o = newClient(t, "http://127.0.0.1:1", func(c *OpenAIConfig) { c.HTTPClient = &http.Client{Timeout: time.Second} })
	if o.client.Timeout != time.Second {
		t.Errorf("timeout = %v, want the configured 1s", o.client.Timeout)
	}
}

func TestNewOpenAIHardening(t *testing.T) {
	if _, err := NewOpenAI(OpenAIConfig{BaseURL: "http://evil.example.com", APIKey: "k", Model: "m", AllowHTTP: true}); err == nil {
		t.Error("http to a non-loopback host must be refused even with AllowHTTP")
	}
	if _, err := NewOpenAI(OpenAIConfig{BaseURL: "https://api.example.com/v1?", APIKey: "k", Model: "m"}); err == nil {
		t.Error("a bare ? must be refused")
	}
	for _, allow := range []string{"[::1]", "[::1]:8443", "::1"} {
		if _, err := NewOpenAI(OpenAIConfig{BaseURL: "https://[::1]:8443", Model: "m", AllowedHosts: []string{allow}}); err != nil {
			t.Errorf("allow entry %q: %v", allow, err)
		}
	}
	if _, err := NewOpenAI(OpenAIConfig{BaseURL: "https://API.Example.COM/v1", APIKey: "k", Model: "m", AllowedHosts: []string{"api.example.com"}}); err != nil {
		t.Errorf("upper-case host: %v", err)
	}
	if u, _ := url.Parse("https://x"); u == nil {
		t.Fatal()
	}
}

func TestCompleteClampsUsageAndHugeLimits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply(w, `{"choices":[{"message":{"content":"x"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9000000000000000000,"completion_tokens":5}}`)
	}))
	defer srv.Close()
	o := newClient(t, srv.URL, func(c *OpenAIConfig) { c.MaxResponseBytes = math.MaxInt64 })
	resp, err := o.Complete(context.Background(), simpleReq())
	if err != nil || resp.Usage != (Usage{In: 100_000_000, Out: 5}) {
		t.Fatalf("resp = %+v, err = %v", resp, err)
	}
}

func TestCompleteReportsACancelledContextWhileReadingTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write([]byte(`{"choices"`))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	_, err := newClient(t, srv.URL, nil).Complete(ctx, simpleReq())
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want context.Canceled and not ErrUnavailable", err)
	}
}

func TestSecretIsRedactedThroughAPointer(t *testing.T) {
	s := Secret("sk-ptr")
	if out := fmt.Sprintf("%v %+v %s", &s, &s, &s); strings.Contains(out, "sk-ptr") {
		t.Errorf("leaked: %q", out)
	}
}

//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// fakeLLM is an OpenAI-compatible /chat/completions endpoint that plays
// scripted turns. A script is chosen by a marker string that the test puts
// into its chat message (the job's goal), so scenarios that run against the
// same sidecar never see each other's turns. Every request is recorded.
type fakeLLM struct {
	srv     *http.Server
	ln      net.Listener
	mu      sync.Mutex
	scripts map[string]script
	reqs    []llmRequest
	errs    []string
}

// script returns the next turn for a request.
type script func(req llmRequest) llmTurn

type llmRequest struct {
	Model    string       `json:"model"`
	Messages []llmMessage `json:"messages"`
	Tools    []struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

type llmMessage struct {
	Role       string    `json:"role"`
	Content    *string   `json:"content"`
	ToolCalls  []llmCall `json:"tool_calls,omitempty"`
	ToolCallID string    `json:"tool_call_id,omitempty"`
}

func (m llmMessage) text() string {
	if m.Content == nil {
		return ""
	}
	return *m.Content
}

type llmCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// llmTurn is one scripted answer: text, or one tool call (the tool name in
// our dotted form; the wire uses "__").
type llmTurn struct {
	Text string
	Tool string
	Args any
}

func newFakeLLM() (*fakeLLM, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	f := &fakeLLM{ln: ln, scripts: map[string]script{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", f.complete)
	f.srv = &http.Server{Handler: mux}
	go func() { _ = f.srv.Serve(ln) }()
	return f, nil
}

// BaseURL is the AGENT_LLM_BASE_URL of the fake.
func (f *fakeLLM) BaseURL() string { return "http://" + f.ln.Addr().String() + "/v1" }

func (f *fakeLLM) Close() { _ = f.srv.Close() }

// on registers the script of the conversations whose messages contain marker.
func (f *fakeLLM) on(marker string, s script) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts[marker] = s
}

// requests returns the recorded requests that carry marker.
func (f *fakeLLM) requests(marker string) []llmRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []llmRequest
	for _, r := range f.reqs {
		if r.mentions(marker) {
			out = append(out, r)
		}
	}
	return out
}

// errors are requests the fake could not answer (no script, bad JSON).
func (f *fakeLLM) errors() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.errs...)
}

// mentions reports whether the LAST user message (the job's goal) carries
// marker: a chat job's transcript starts with the earlier messages of its
// conversation, which carry the markers of earlier jobs.
func (r llmRequest) mentions(marker string) bool {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		if r.Messages[i].Role == "user" {
			return strings.Contains(r.Messages[i].text(), marker)
		}
	}
	return false
}

// toolResults are the contents of the tool messages after the last user
// message (the results of this job's calls so far).
func (r llmRequest) toolResults() []string {
	var out []string
	for _, m := range r.Messages {
		switch m.Role {
		case "user":
			out = nil
		case "tool":
			out = append(out, m.text())
		}
	}
	return out
}

func (r llmRequest) system() string {
	for _, m := range r.Messages {
		if m.Role == "system" {
			return m.text()
		}
	}
	return ""
}

func (r llmRequest) toolNames() []string {
	var out []string
	for _, t := range r.Tools {
		out = append(out, strings.ReplaceAll(t.Function.Name, "__", "."))
	}
	return out
}

func (f *fakeLLM) complete(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	var req llmRequest
	if err == nil {
		err = json.Unmarshal(raw, &req)
	}
	if err != nil {
		f.fail(w, "undecodable request")
		return
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	var s script
	for marker, sc := range f.scripts {
		if req.mentions(marker) {
			s = sc
			break
		}
	}
	f.mu.Unlock()
	if s == nil {
		f.fail(w, "no script for the request")
		return
	}
	turn := s(req)
	type call struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	msg := struct {
		Role      string  `json:"role"`
		Content   *string `json:"content"`
		ToolCalls []call  `json:"tool_calls,omitempty"`
	}{Role: "assistant"}
	finish := "stop"
	if turn.Tool != "" {
		args, err := json.Marshal(turn.Args)
		if err != nil {
			f.fail(w, "unencodable arguments")
			return
		}
		var c call
		c.ID, c.Type = "call_1", "function"
		c.Function.Name, c.Function.Arguments = strings.ReplaceAll(turn.Tool, ".", "__"), string(args)
		msg.ToolCalls, finish = []call{c}, "tool_calls"
	} else {
		text := turn.Text
		msg.Content = &text
	}
	resp := map[string]any{
		"id": "fake", "object": "chat.completion", "model": req.Model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
		"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (f *fakeLLM) fail(w http.ResponseWriter, why string) {
	f.mu.Lock()
	f.errs = append(f.errs, why)
	f.mu.Unlock()
	// 400 is not retryable for the sidecar: the job fails at once
	http.Error(w, `{"error":{"message":"fake: `+why+`"}}`, http.StatusBadRequest)
}

// answer is a script that replies with text.
func answer(text string) script { return func(llmRequest) llmTurn { return llmTurn{Text: text} } }

// callThenAnswer calls tool once; after its result it replies with the
// text plus the result (so the test can see what the model was given).
func callThenAnswer(tool string, args any, text string) script {
	return func(req llmRequest) llmTurn {
		if res := req.toolResults(); len(res) > 0 {
			return llmTurn{Text: text + "\n" + strings.Join(res, "\n")}
		}
		return llmTurn{Tool: tool, Args: args}
	}
}

// TestFakeLLM checks the fake's wire format: tool calls with "__" names,
// text answers, scripts selected by marker and recorded requests.
func TestFakeLLM(t *testing.T) {
	f, err := newFakeLLM()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.on("[m1]", callThenAnswer("files.list", map[string]string{"path": "/"}, "done"))

	post := func(body string) map[string]any {
		t.Helper()
		resp, err := http.Post(f.BaseURL()+"/chat/completions", "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d: %v", resp.StatusCode, out)
		}
		return out
	}
	first := post(`{"model":"m","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi [m1]"}],
		"tools":[{"type":"function","function":{"name":"files__list","parameters":{}}}]}`)
	ch := first["choices"].([]any)[0].(map[string]any)
	calls := ch["message"].(map[string]any)["tool_calls"].([]any)
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if ch["finish_reason"] != "tool_calls" || fn["name"] != "files__list" || fn["arguments"] != `{"path":"/"}` {
		t.Fatalf("first turn: %v", ch)
	}
	second := post(`{"model":"m","messages":[{"role":"user","content":"hi [m1]"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"files__list","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"RESULT"}]}`)
	msg := second["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if c, _ := msg["content"].(string); !strings.Contains(c, "done") || !strings.Contains(c, "RESULT") {
		t.Fatalf("second turn: %v", msg)
	}
	// an earlier job's marker in the conversation prefix does not select its script
	f.on("[m2]", answer("two"))
	third := post(`{"model":"m","messages":[{"role":"user","content":"hi [m1]"},{"role":"assistant","content":"done"},
		{"role":"user","content":"next [m2]"}]}`)
	if c, _ := third["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"].(string); c != "two" {
		t.Fatalf("third turn picked another script: %q", c)
	}
	reqs := f.requests("[m1]")
	if len(reqs) != 2 || reqs[0].system() != "sys" || fmt.Sprint(reqs[0].toolNames()) != "[files.list]" {
		t.Fatalf("recorded: %+v", reqs)
	}
	resp, err := http.Post(f.BaseURL()+"/chat/completions", "application/json",
		bytes.NewBufferString(`{"messages":[{"role":"user","content":"unknown"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || len(f.errors()) != 1 {
		t.Fatalf("an unscripted request: %d, errors %v", resp.StatusCode, f.errors())
	}
}

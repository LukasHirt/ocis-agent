package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// sse writes each part and flushes, waiting gap between parts.
func sse(gap time.Duration, parts ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		for _, p := range parts {
			if gap > 0 {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(gap):
				}
			}
			if _, err := io.WriteString(w, p); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	}
}

func ev(json string) string { return "data: " + json + "\n\n" }

const done = "data: [DONE]\n\n"

func streamClient(t *testing.T, url string, mutate func(*OpenAIConfig)) *OpenAI {
	t.Helper()
	return newClient(t, url, func(c *OpenAIConfig) {
		c.Stream = true
		if mutate != nil {
			mutate(c)
		}
	})
}

type deltaLog struct {
	mu sync.Mutex
	d  []Delta
}

func (l *deltaLog) add(d Delta) { l.mu.Lock(); l.d = append(l.d, d); l.mu.Unlock() }
func (l *deltaLog) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var b strings.Builder
	for _, d := range l.d {
		b.WriteString(d.Text)
	}
	return b.String()
}
func (l *deltaLog) thinking() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, d := range l.d {
		if d.Thinking {
			n++
		}
	}
	return n
}

func noLeak(t *testing.T, err error, url string) {
	t.Helper()
	if err == nil {
		return
	}
	for _, leak := range []string{url, "127.0.0.1", "sk-test", "http"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error %q leaks %q", err, leak)
		}
	}
}

var _ Streamer = (*OpenAI)(nil)

func TestStreamSendsTheStreamFlagsAndAggregatesAnOpenAIStream(t *testing.T) {
	var got map[string]any
	var accept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept = r.Header.Get("Accept")
		_ = json.NewDecoder(r.Body).Decode(&got)
		sse(0,
			ev(`{"id":"x","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}],"usage":null}`),
			ev(`{"id":"x","choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}],"usage":null}`),
			ev(`{"id":"x","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}],"usage":null}`),
			ev(`{"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":null}`),
			ev(`{"id":"x","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}}`),
			done,
		)(w, r)
	}))
	defer srv.Close()
	var log deltaLog
	resp, err := streamClient(t, srv.URL, nil).CompleteStream(context.Background(), simpleReq(), log.add)
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
	if at(t, got, "stream") != true || at(t, got, "stream_options", "include_usage") != true || accept != "text/event-stream" {
		t.Errorf("request = %v, Accept = %q", got, accept)
	}
	if resp.Message.Content != "Hello" || resp.Message.Role != RoleAssistant || resp.Stop != StopEnd ||
		resp.Usage != (Usage{In: 11, Out: 3}) || resp.UsageEstimated {
		t.Errorf("resp = %+v", resp)
	}
	if log.text() != "Hello" || log.thinking() != 0 {
		t.Errorf("deltas = %+v", log.d)
	}
}

func TestCompleteStreamsTooWhenStreamingIsOn(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		sse(0, ev(`{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`), done)(w, r)
	}))
	defer srv.Close()
	resp, err := streamClient(t, srv.URL, nil).Complete(context.Background(), simpleReq())
	if err != nil || resp.Message.Content != "ok" || at(t, got, "stream") != true {
		t.Fatalf("resp = %+v, err = %v, request = %v", resp, err, got)
	}
}

func TestStreamingOffSendsAPlainRequestAndNoDeltas(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		reply(w, `{"choices":[{"message":{"content":"plain"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()
	var log deltaLog
	resp, err := newClient(t, srv.URL, nil).CompleteStream(context.Background(), simpleReq(), log.add)
	if err != nil || resp.Message.Content != "plain" {
		t.Fatalf("resp = %+v, err = %v", resp, err)
	}
	if _, ok := got["stream"]; ok {
		t.Errorf("stream sent while streaming is off: %v", got)
	}
	if _, ok := got["stream_options"]; ok {
		t.Errorf("stream_options sent while streaming is off: %v", got)
	}
	if len(log.d) != 0 {
		t.Errorf("deltas = %+v", log.d)
	}
}

func TestStreamSurvivesChunksSplitMidEventAndMidRune(t *testing.T) {
	raw := ev(`{"choices":[{"index":0,"delta":{"content":"Grüße "},"finish_reason":null}]}`) +
		ev(`{"choices":[{"index":0,"delta":{"content":"🌍"},"finish_reason":"stop"}]}`) +
		ev(`{"choices":[],"usage":{"prompt_tokens":4,"completion_tokens":2}}`) + done
	var parts []string
	for i := 0; i < len(raw); i += 3 {
		parts = append(parts, raw[i:min(i+3, len(raw))])
	}
	srv := httptest.NewServer(sse(0, parts...))
	defer srv.Close()
	var log deltaLog
	resp, err := streamClient(t, srv.URL, nil).CompleteStream(context.Background(), simpleReq(), log.add)
	if err != nil || resp.Message.Content != "Grüße 🌍" || log.text() != "Grüße 🌍" || resp.Usage != (Usage{In: 4, Out: 2}) {
		t.Fatalf("resp = %+v, deltas = %q, err = %v", resp, log.text(), err)
	}
}

func TestStreamAccumulatesToolCallFragmentsInterleavedWithContent(t *testing.T) {
	srv := httptest.NewServer(sse(0,
		ev(`{"choices":[{"index":0,"delta":{"content":"Let me look."},"finish_reason":null}]}`),
		ev(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"files__read","arguments":""}}]},"finish_reason":null}]}`),
		ev(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"pa"}}]},"finish_reason":null}]}`),
		ev(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"type":"function","function":{"name":"memory__read","arguments":"{"}}]},"finish_reason":null}]}`),
		ev(`{"choices":[{"index":0,"delta":{"content":" Still"},"finish_reason":null}]}`),
		ev(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"/a\"}"}}]},"finish_reason":null}]}`),
		ev(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"}"}}]},"finish_reason":null}]}`),
		ev(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
		ev(`{"choices":[],"usage":{"prompt_tokens":20,"completion_tokens":9}}`),
		done,
	))
	defer srv.Close()
	var log deltaLog
	resp, err := streamClient(t, srv.URL, nil).CompleteStream(context.Background(), simpleReq(), log.add)
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
	want := []ToolCall{{ID: "call_a", Name: "files.read", Arguments: `{"path":"/a"}`}, {ID: "call_1", Name: "memory.read", Arguments: "{}"}}
	if resp.Stop != StopToolCalls || resp.Message.Content != "Let me look. Still" || fmt.Sprint(resp.Message.ToolCalls) != fmt.Sprint(want) {
		t.Errorf("resp = %+v", resp)
	}
	if log.text() != "Let me look. Still" {
		t.Errorf("deltas = %q", log.text())
	}
}

func TestStreamCutOffWithToolCallsIsMarkedLength(t *testing.T) {
	srv := httptest.NewServer(sse(0,
		ev(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c","type":"function","function":{"name":"files__write","arguments":"{\"pa"}}]},"finish_reason":null}]}`),
		ev(`{"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`),
		done,
	))
	defer srv.Close()
	resp, err := streamClient(t, srv.URL, nil).CompleteStream(context.Background(), simpleReq(), nil)
	if err != nil || resp.Stop != StopLength || len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("resp = %+v, err = %v", resp, err)
	}
}

// Ollama: hidden reasoning in "reasoning", whole tool calls in one chunk,
// usage in a chunk without choices.
func TestStreamOfAThinkingModelHidesTheReasoning(t *testing.T) {
	for _, field := range []string{"reasoning", "reasoning_content"} {
		t.Run(field, func(t *testing.T) {
			srv := httptest.NewServer(sse(0,
				ev(`{"id":"chatcmpl-1","object":"chat.completion.chunk","model":"qwen3","system_fingerprint":"fp_ollama","choices":[{"index":0,"delta":{"role":"assistant","content":"","`+field+`":"SECRET-THOUGHT one"},"finish_reason":null}]}`),
				ev(`{"choices":[{"index":0,"delta":{"role":"assistant","content":"","`+field+`":" two"},"finish_reason":null}]}`),
				ev(`{"choices":[{"index":0,"delta":{"role":"assistant","content":"Answer"},"finish_reason":null}]}`),
				ev(`{"choices":[{"index":0,"delta":{"role":"assistant","content":"","tool_calls":[{"id":"call_x1","index":0,"type":"function","function":{"name":"files__list","arguments":"{\"path\":\"/\"}"}}]},"finish_reason":null}]}`),
				ev(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":"tool_calls"}]}`),
				ev(`{"choices":[],"usage":{"prompt_tokens":30,"completion_tokens":12,"total_tokens":42}}`),
				done,
			))
			defer srv.Close()
			var log deltaLog
			resp, err := streamClient(t, srv.URL, nil).CompleteStream(context.Background(), simpleReq(), log.add)
			if err != nil {
				t.Fatalf("CompleteStream: %v", err)
			}
			if resp.Message.Content != "Answer" || resp.Stop != StopToolCalls || len(resp.Message.ToolCalls) != 1 ||
				resp.Message.ToolCalls[0].Name != "files.list" || resp.Usage != (Usage{In: 30, Out: 12}) {
				t.Errorf("resp = %+v", resp)
			}
			if log.thinking() != 1 || log.text() != "Answer" {
				t.Errorf("deltas = %+v", log.d)
			}
			for _, d := range log.d {
				if strings.Contains(d.Text, "SECRET") {
					t.Errorf("reasoning leaked into a delta: %+v", d)
				}
			}
			if strings.Contains(fmt.Sprintf("%+v", resp), "SECRET") {
				t.Errorf("reasoning leaked into the response: %+v", resp)
			}
		})
	}
}

func TestReasoningDeltasKeepTheCallAliveButSilenceEndsIt(t *testing.T) {
	var parts []string
	for i := 0; i < 10; i++ {
		parts = append(parts, ev(`{"choices":[{"index":0,"delta":{"reasoning":"hmm"},"finish_reason":null}]}`))
	}
	parts = append(parts, ev(`{"choices":[{"index":0,"delta":{"content":"done"},"finish_reason":"stop"}]}`), done)
	alive := httptest.NewServer(sse(60*time.Millisecond, parts...)) // 660ms in all
	defer alive.Close()
	resp, err := streamClient(t, alive.URL, func(c *OpenAIConfig) { c.IdleTimeout = 250 * time.Millisecond }).
		CompleteStream(context.Background(), simpleReq(), nil)
	if err != nil || resp.Message.Content != "done" {
		t.Fatalf("a stream with activity failed: %+v, %v", resp, err)
	}

	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse(0, ev(`{"choices":[{"index":0,"delta":{"content":"half"},"finish_reason":null}]}`))(w, r)
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer silent.Close()
	var log deltaLog
	start := time.Now()
	resp, err = streamClient(t, silent.URL, func(c *OpenAIConfig) { c.IdleTimeout = 200 * time.Millisecond }).
		CompleteStream(context.Background(), simpleReq(), log.add)
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the idle timeout was not applied: %v", took)
	}
	if !errors.Is(err, ErrUnavailable) || !Retryable(err) || resp.Message.Content != "" {
		t.Fatalf("resp = %+v, err = %v, want a retryable ErrUnavailable and no answer", resp, err)
	}
	if log.text() != "half" {
		t.Errorf("deltas = %q", log.text())
	}
	noLeak(t, err, silent.URL)
}

func TestTheIdleTimeoutCoversTheWaitForHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer srv.Close()
	start := time.Now()
	_, err := streamClient(t, srv.URL, func(c *OpenAIConfig) { c.IdleTimeout = 150 * time.Millisecond }).
		CompleteStream(context.Background(), simpleReq(), nil)
	if took := time.Since(start); took > 3*time.Second || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("took %v, err = %v", took, err)
	}
}

func TestTheTotalTimeoutEndsAStreamThatKeepsTalking(t *testing.T) {
	var parts []string
	for i := 0; i < 200; i++ {
		parts = append(parts, ": keep-alive\n\n")
	}
	srv := httptest.NewServer(sse(30*time.Millisecond, parts...))
	defer srv.Close()
	start := time.Now()
	_, err := streamClient(t, srv.URL, func(c *OpenAIConfig) {
		c.Timeout = 300 * time.Millisecond
		c.IdleTimeout = time.Second
	}).CompleteStream(context.Background(), simpleReq(), nil)
	if took := time.Since(start); took > 3*time.Second || !errors.Is(err, ErrUnavailable) || !Retryable(err) {
		t.Fatalf("took %v, err = %v", took, err)
	}
	noLeak(t, err, srv.URL)
}

func TestAStreamThatEndsEarlyIsARetryableFailure(t *testing.T) {
	for label, parts := range map[string][]string{
		"eof after content":    {ev(`{"choices":[{"index":0,"delta":{"content":"half an"},"finish_reason":null}]}`)},
		"cut mid event":        {ev(`{"choices":[{"index":0,"delta":{"content":"half"},"finish_reason":null}]}`), `data: {"choices":[{"index":0,"delta":{},"finish_re`},
		"empty stream":         {},
		"only a usage chunk":   {ev(`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)},
		"an error in a stream": {ev(`{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}`), ev(`{"error":{"message":"overloaded http://internal.example"}}`), done},
	} {
		t.Run(label, func(t *testing.T) {
			srv := httptest.NewServer(sse(0, parts...))
			defer srv.Close()
			resp, err := streamClient(t, srv.URL, nil).CompleteStream(context.Background(), simpleReq(), nil)
			if !errors.Is(err, ErrUnavailable) || resp.Message.Content != "" {
				t.Fatalf("resp = %+v, err = %v, want ErrUnavailable and no answer", resp, err)
			}
			noLeak(t, err, srv.URL)
			if strings.Contains(err.Error(), "overloaded") {
				t.Errorf("error %q echoes the provider's text", err)
			}
		})
	}
}

func TestStreamEndsWithDoneOrAFinishReason(t *testing.T) {
	for label, parts := range map[string][]string{
		"done without a finish reason": {ev(`{"choices":[{"index":0,"delta":{"content":"a"},"finish_reason":null}]}`), done},
		"finish reason then eof":       {ev(`{"choices":[{"index":0,"delta":{"content":"a"},"finish_reason":"stop"}]}`)},
		"done without a blank line":    {ev(`{"choices":[{"index":0,"delta":{"content":"a"},"finish_reason":null}]}`), "data: [DONE]"},
	} {
		t.Run(label, func(t *testing.T) {
			srv := httptest.NewServer(sse(0, parts...))
			defer srv.Close()
			resp, err := streamClient(t, srv.URL, nil).CompleteStream(context.Background(), simpleReq(), nil)
			if err != nil || resp.Message.Content != "a" || resp.Stop != StopEnd {
				t.Fatalf("resp = %+v, err = %v", resp, err)
			}
		})
	}
}

func TestStreamIgnoresCommentsEventNamesAndHandlesCRLFAndMultiLineData(t *testing.T) {
	srv := httptest.NewServer(sse(0,
		": OPENROUTER PROCESSING\r\n\r\n",
		"event: message\r\nid: 1\r\nretry: 100\r\ndata: {\"choices\":[{\"index\":0,\r\ndata: \"delta\":{\"content\":\"A\"},\"finish_reason\":null}]}\r\n\r\n",
		"data:{\"choices\":[{\"index\":0,\"delta\":{\"content\":\"B\"},\"finish_reason\":\"stop\"}]}\n\n",
		"nonsense-field\n\n",
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\r\n\r\n",
		"data: [DONE]\r\n\r\n",
	))
	defer srv.Close()
	resp, err := streamClient(t, srv.URL, nil).CompleteStream(context.Background(), simpleReq(), nil)
	if err != nil || resp.Message.Content != "AB" || resp.Usage != (Usage{In: 3, Out: 2}) {
		t.Fatalf("resp = %+v, err = %v", resp, err)
	}
}

func TestAJSONAnswerToAStreamRequestIsParsedAsACompletion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply(w, `{"choices":[{"message":{"content":"whole"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`)
	}))
	defer srv.Close()
	resp, err := streamClient(t, srv.URL, nil).CompleteStream(context.Background(), simpleReq(), nil)
	if err != nil || resp.Message.Content != "whole" || resp.Usage != (Usage{In: 5, Out: 1}) {
		t.Fatalf("resp = %+v, err = %v", resp, err)
	}
}

type recorded struct {
	stream, opts bool
}

// fallbackServer answers a streaming request with refuse (when not 0) and
// any other with a JSON completion; it records what each request asked for.
func fallbackServer(t *testing.T, refuse func(recorded) int) (*httptest.Server, func() []recorded) {
	t.Helper()
	var mu sync.Mutex
	var reqs []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, opts := body["stream_options"]
		rec := recorded{stream: body["stream"] == true, opts: opts}
		mu.Lock()
		reqs = append(reqs, rec)
		mu.Unlock()
		if code := refuse(rec); code != 0 {
			w.WriteHeader(code)
			_, _ = io.WriteString(w, `{"error":"no streaming here"}`)
			return
		}
		if rec.stream {
			sse(0, ev(`{"choices":[{"index":0,"delta":{"content":"s"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`), done)(w, r)
			return
		}
		reply(w, `{"choices":[{"message":{"content":"j"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []recorded {
		mu.Lock()
		defer mu.Unlock()
		return append([]recorded(nil), reqs...)
	}
}

func TestAnEndpointWithoutStreamingFallsBackOnceAndIsRemembered(t *testing.T) {
	for _, code := range []int{http.StatusBadRequest, http.StatusNotFound, http.StatusNotImplemented} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			srv, reqs := fallbackServer(t, func(r recorded) int {
				if r.stream {
					return code
				}
				return 0
			})
			o := streamClient(t, srv.URL, nil)
			for i := 0; i < 2; i++ {
				resp, err := o.CompleteStream(context.Background(), simpleReq(), nil)
				if err != nil || resp.Message.Content != "j" {
					t.Fatalf("call %d: resp = %+v, err = %v", i, resp, err)
				}
			}
			want := []recorded{{true, true}, {false, false}, {false, false}}
			if code == http.StatusBadRequest {
				want = []recorded{{true, true}, {true, false}, {false, false}, {false, false}}
			}
			if fmt.Sprint(reqs()) != fmt.Sprint(want) {
				t.Errorf("requests = %v, want %v", reqs(), want)
			}
		})
	}
}

func TestStreamOptionsRefusedWithA400AreDroppedAndRemembered(t *testing.T) {
	srv, reqs := fallbackServer(t, func(r recorded) int {
		if r.opts {
			return http.StatusBadRequest
		}
		return 0
	})
	o := streamClient(t, srv.URL, nil)
	for i := 0; i < 2; i++ {
		resp, err := o.CompleteStream(context.Background(), simpleReq(), nil)
		if err != nil || resp.Message.Content != "s" {
			t.Fatalf("call %d: resp = %+v, err = %v", i, resp, err)
		}
	}
	if want := []recorded{{true, true}, {true, false}, {true, false}}; fmt.Sprint(reqs()) != fmt.Sprint(want) {
		t.Errorf("requests = %v, want %v", reqs(), want)
	}
}

func TestARequestRejectedEverywhereIsRejectedAndNothingIsRemembered(t *testing.T) {
	srv, reqs := fallbackServer(t, func(recorded) int { return http.StatusBadRequest })
	o := streamClient(t, srv.URL, nil)
	for i := 0; i < 2; i++ {
		_, err := o.CompleteStream(context.Background(), simpleReq(), nil)
		if !errors.Is(err, ErrRejected) || Retryable(err) {
			t.Fatalf("call %d: err = %v, want ErrRejected", i, err)
		}
		noLeak(t, err, srv.URL)
	}
	if got := reqs(); len(got) != 6 || !got[3].stream || !got[3].opts {
		t.Errorf("requests = %v, want three per call, streaming again on the second", got)
	}
}

func TestOtherStatusCodesOfAStreamRequestAreNotFallenBackOn(t *testing.T) {
	for code, want := range map[int]error{401: ErrAuth, 429: ErrRateLimited, 500: ErrUnavailable, 503: ErrUnavailable, 422: ErrRejected} {
		srv, reqs := fallbackServer(t, func(recorded) int { return code })
		_, err := streamClient(t, srv.URL, nil).CompleteStream(context.Background(), simpleReq(), nil)
		if !errors.Is(err, want) || len(reqs()) != 1 {
			t.Errorf("status %d: err = %v, requests = %v", code, err, reqs())
		}
	}
}

func TestMissingStreamUsageIsEstimatedNeverZero(t *testing.T) {
	for label, usage := range map[string]string{
		"absent": "",
		"null":   `,"usage":null`,
		"zero":   `,"usage":{"prompt_tokens":0,"completion_tokens":0}`,
	} {
		t.Run(label, func(t *testing.T) {
			var size int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				size = len(b)
				sse(0,
					ev(`{"choices":[{"index":0,"delta":{"reasoning":"abc"},"finish_reason":null}]}`),
					ev(`{"choices":[{"index":0,"delta":{"content":"abcdefghi"},"finish_reason":"stop"}]`+usage+`}`),
					done,
				)(w, r)
			}))
			defer srv.Close()
			resp, err := streamClient(t, srv.URL, nil).CompleteStream(context.Background(), simpleReq(), nil)
			if err != nil {
				t.Fatalf("CompleteStream: %v", err)
			}
			// 12 bytes of output (reasoning included) are 3 tokens
			if !resp.UsageEstimated || resp.Usage.Out != 3 || resp.Usage.In != (size+3)/4 || resp.Usage.In == 0 {
				t.Errorf("usage = %+v estimated = %v, request size %d", resp.Usage, resp.UsageEstimated, size)
			}
		})
	}
	srv := httptest.NewServer(sse(0, ev(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`), done))
	defer srv.Close()
	resp, err := streamClient(t, srv.URL, nil).CompleteStream(context.Background(), simpleReq(), nil)
	if err != nil || resp.Usage.Out < 1 || resp.Usage.In < 1 || !resp.UsageEstimated {
		t.Errorf("empty answer: resp = %+v, err = %v", resp, err)
	}
}

func TestStreamSizesAreCapped(t *testing.T) {
	huge := httptest.NewServer(sse(0, "data: "+strings.Repeat("a", 8192)))
	defer huge.Close()
	_, err := streamClient(t, huge.URL, func(c *OpenAIConfig) { c.MaxResponseBytes = 1024 }).CompleteStream(context.Background(), simpleReq(), nil)
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("huge line: err = %v, want ErrTooLarge", err)
	}

	var parts []string
	for i := 0; i < 100; i++ {
		parts = append(parts, ev(`{"choices":[{"index":0,"delta":{"content":"0123456789"},"finish_reason":null}]}`))
	}
	many := httptest.NewServer(sse(0, append(parts, done)...))
	defer many.Close()
	_, err = streamClient(t, many.URL, func(c *OpenAIConfig) { c.MaxResponseBytes = 2048 }).CompleteStream(context.Background(), simpleReq(), nil)
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("many events: err = %v, want ErrTooLarge", err)
	}
}

func TestStreamRefusesMalformedToolCalls(t *testing.T) {
	call := func(s string) string {
		return ev(`{"choices":[{"index":0,"delta":{"tool_calls":[` + s + `]},"finish_reason":null}]}`)
	}
	finish := ev(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
	var tooMany []string
	for i := 0; i < 17; i++ {
		tooMany = append(tooMany, call(fmt.Sprintf(`{"index":%d,"id":"c%d","function":{"name":"files__list","arguments":"{}"}}`, i, i)))
	}
	for label, parts := range map[string][]string{
		"index out of range": {call(`{"index":5,"id":"c","function":{"name":"files__list"}}`), finish, done},
		"negative index":     {call(`{"index":-1,"id":"c","function":{"name":"files__list"}}`), finish, done},
		"changing id":        {call(`{"index":0,"id":"a","function":{"name":"files__list"}}`), call(`{"index":0,"id":"b","function":{"arguments":"{}"}}`), finish, done},
		"bad id":             {call(`{"index":0,"id":"a b","function":{"name":"files__list"}}`), finish, done},
		"no name":            {call(`{"index":0,"id":"a","function":{"arguments":"{}"}}`), finish, done},
		"other type":         {call(`{"index":0,"id":"a","type":"code","function":{"name":"files__list"}}`), finish, done},
		"too many":           append(tooMany, finish, done),
		"bad chunk":          {"data: {not json\n\n", done},
		"unknown finish":     {ev(`{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"weird"}]}`), done},
	} {
		t.Run(label, func(t *testing.T) {
			srv := httptest.NewServer(sse(0, parts...))
			defer srv.Close()
			_, err := streamClient(t, srv.URL, nil).CompleteStream(context.Background(), simpleReq(), nil)
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("err = %v, want ErrMalformed", err)
			}
		})
	}
	srv := httptest.NewServer(sse(0, ev(`{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"content_filter"}]}`), done))
	defer srv.Close()
	if _, err := streamClient(t, srv.URL, nil).CompleteStream(context.Background(), simpleReq(), nil); !errors.Is(err, ErrRejected) {
		t.Errorf("content filter: err = %v", err)
	}
}

func TestStreamToolCallsWithoutIndexes(t *testing.T) {
	srv := httptest.NewServer(sse(0,
		ev(`{"choices":[{"index":0,"delta":{"tool_calls":[{"id":"a","function":{"name":"files__list","arguments":"{\"path\""}}]},"finish_reason":null}]}`),
		ev(`{"choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":":\"/\"}"}}]},"finish_reason":null}]}`),
		ev(`{"choices":[{"index":0,"delta":{"tool_calls":[{"id":"b","function":{"name":"memory__read","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`),
		done,
	))
	defer srv.Close()
	resp, err := streamClient(t, srv.URL, nil).CompleteStream(context.Background(), simpleReq(), nil)
	want := []ToolCall{{ID: "a", Name: "files.list", Arguments: `{"path":"/"}`}, {ID: "b", Name: "memory.read", Arguments: "{}"}}
	if err != nil || fmt.Sprint(resp.Message.ToolCalls) != fmt.Sprint(want) {
		t.Fatalf("resp = %+v, err = %v", resp, err)
	}
}

func TestStreamUsesOnlyTheFirstChoice(t *testing.T) {
	srv := httptest.NewServer(sse(0,
		ev(`{"choices":[{"index":1,"delta":{"content":"other"},"finish_reason":null},{"index":0,"delta":{"content":"mine"},"finish_reason":null}]}`),
		ev(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
		done,
	))
	defer srv.Close()
	var log deltaLog
	resp, err := streamClient(t, srv.URL, nil).CompleteStream(context.Background(), simpleReq(), log.add)
	if err != nil || resp.Message.Content != "mine" || log.text() != "mine" {
		t.Fatalf("resp = %+v, deltas %q, err = %v", resp, log.text(), err)
	}
}

func TestCancellingMidStreamIsAContextError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse(0, ev(`{"choices":[{"index":0,"delta":{"content":"a"},"finish_reason":null}]}`))(w, r)
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := streamClient(t, srv.URL, nil).CompleteStream(ctx, simpleReq(), func(Delta) { cancel() })
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want context.Canceled and not ErrUnavailable", err)
	}
}

func TestStreamsLeaveNoGoroutinesBehind(t *testing.T) {
	ok := httptest.NewServer(sse(0, ev(`{"choices":[{"index":0,"delta":{"content":"a"},"finish_reason":"stop"}]}`), done))
	stall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse(0, ev(`{"choices":[{"index":0,"delta":{"content":"a"},"finish_reason":null}]}`))(w, r)
		<-r.Context().Done()
	}))
	transport := &http.Transport{}
	before := runtime.NumGoroutine()
	for i := 0; i < 5; i++ {
		for _, url := range []string{ok.URL, stall.URL} {
			o := streamClient(t, url, func(c *OpenAIConfig) {
				c.HTTPClient = &http.Client{Transport: transport}
				c.IdleTimeout = 50 * time.Millisecond
			})
			_, _ = o.CompleteStream(context.Background(), simpleReq(), nil)
			ctx, cancel := context.WithCancel(context.Background())
			_, _ = o.CompleteStream(ctx, simpleReq(), func(Delta) { cancel() })
			cancel()
		}
	}
	transport.CloseIdleConnections()
	ok.Close()
	stall.Close()
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		buf := make([]byte, 1<<16)
		t.Fatalf("goroutines: %d before, %d after\n%s", before, n, buf[:runtime.Stack(buf, true)])
	}
}

func TestNewOpenAIDefaultsTheIdleTimeout(t *testing.T) {
	if o := newClient(t, "http://127.0.0.1:1", nil); o.idle != 60*time.Second {
		t.Errorf("idle = %v, want 60s", o.idle)
	}
	if o := newClient(t, "http://127.0.0.1:1", func(c *OpenAIConfig) { c.IdleTimeout = 7 * time.Second }); o.idle != 7*time.Second {
		t.Errorf("idle = %v, want 7s", o.idle)
	}
}

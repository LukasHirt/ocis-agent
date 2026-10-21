package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	defaultMaxResponse  = 4 << 20
	defaultMaxToolCalls = 16
	maxResponseCap      = 64 << 20
	defaultTimeout      = 120 * time.Second
	defaultStreamTimout = 30 * time.Minute
	defaultIdleTimeout  = 60 * time.Second
	// the wire of a stream carries an envelope per token: it may be this
	// many times the kept size, up to maxWireCap
	wireFactor = 16
	maxWireCap = 256 << 20
	// once the model finished, the rest of a stream (usage, [DONE]) must
	// follow within this time
	finishGrace = 3 * time.Second
	// what was learnt about an endpoint is forgotten after this time
	learntTTL  = time.Hour
	learntKeep = 256
)

// OpenAIConfig configures an OpenAI-compatible chat-completions client.
type OpenAIConfig struct {
	BaseURL      string // e.g. https://api.openai.com/v1
	APIKey       Secret // may be empty only for a loopback host
	Model        string
	AllowedHosts []string // egress guard; empty means the base URL's host
	AllowHTTP    bool     // permit http:// (loopback endpoints and tests)
	HTTPClient   *http.Client
	// MaxResponseBytes caps a plain response, and of a stream what is kept
	// (text, reasoning, tool names and arguments); the stream itself may be
	// 16 times as large. Default 4 MiB.
	MaxResponseBytes int64
	MaxToolCalls     int // default 16
	// Timeout bounds one whole call, reading the response included; 0 means
	// the HTTPClient's timeout, or 30m with Stream and 120s without. A slow
	// local model may need more without streaming.
	Timeout time.Duration
	// Stream requests a streamed response (stream: true); see
	// CompleteStream. Off, every call waits for the whole answer.
	Stream bool
	// IdleTimeout ends a streamed call that receives no byte for this long,
	// the wait for the response headers included; 0 means 60s.
	IdleTimeout time.Duration
	// ReasoningEffort is sent as reasoning_effort when not empty; see
	// ValidReasoningEffort.
	ReasoningEffort string
}

// ValidReasoningEffort reports whether e is a reasoning_effort value the
// client may send: none, low, medium or high.
func ValidReasoningEffort(e string) bool {
	switch e {
	case "none", "low", "medium", "high":
		return true
	}
	return false
}

// OpenAI is a Provider for OpenAI-compatible chat-completions endpoints. It
// uses max_tokens, which compatible servers accept.
type OpenAI struct {
	endpoint    string
	model       string
	key         *Secret // behind a pointer so printing the client cannot show it
	client      *http.Client
	maxResponse int64
	maxCalls    int
	effort      string
	maxWire     int64
	stream      bool
	idle        time.Duration
	finishGrace time.Duration
	now         func() time.Time
	// learnt from the endpoint per model, forgotten after learntTTL
	mu     sync.Mutex
	learnt map[learntKey]time.Time
}

type learntKey struct {
	model string
	what  refusal
}

type refusal uint8

const (
	refusesStream        refusal = iota + 1 // stream: true
	refusesStreamOptions                    // stream_options
)

// NewOpenAI validates the configuration and returns the client.
func NewOpenAI(cfg OpenAIConfig) (*OpenAI, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return nil, errors.New("llm: base URL is not a valid URL")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !cfg.AllowHTTP || !IsLoopbackHost(u.Host) {
			return nil, errors.New("llm: base URL must use https (http only for a loopback host)")
		}
	default:
		return nil, errors.New("llm: base URL must start with https://")
	}
	if u.User != nil || u.ForceQuery || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("llm: base URL must not contain credentials, a query or a fragment")
	}
	if !ValidModelID(cfg.Model) {
		return nil, errors.New("llm: the model id is missing or invalid")
	}
	if cfg.APIKey != "" && !ValidAPIKey(string(cfg.APIKey)) {
		return nil, errors.New("llm: the API key contains characters outside printable ASCII")
	}
	if !HostAllowed(u.Host, cfg.AllowedHosts) {
		return nil, errors.New("llm: base URL host is not on the allow-list")
	}
	if cfg.APIKey == "" && !IsLoopbackHost(u.Host) {
		return nil, errors.New("llm: an API key is required for a non-loopback endpoint")
	}

	if cfg.ReasoningEffort != "" && !ValidReasoningEffort(cfg.ReasoningEffort) {
		return nil, errors.New("llm: the reasoning effort must be none, low, medium or high")
	}

	client := &http.Client{}
	if cfg.HTTPClient != nil {
		c := *cfg.HTTPClient
		client = &c
	}
	if cfg.Timeout > 0 {
		client.Timeout = cfg.Timeout
	}
	if client.Timeout <= 0 {
		client.Timeout = defaultTimeout
		if cfg.Stream {
			client.Timeout = defaultStreamTimout
		}
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	o := &OpenAI{
		endpoint:    strings.TrimRight(u.String(), "/") + "/chat/completions",
		model:       cfg.Model,
		client:      client,
		maxResponse: cfg.MaxResponseBytes,
		maxCalls:    cfg.MaxToolCalls,
		effort:      cfg.ReasoningEffort,
		stream:      cfg.Stream,
		idle:        cfg.IdleTimeout,
		finishGrace: finishGrace,
		now:         time.Now,
		learnt:      map[learntKey]time.Time{},
	}
	if o.idle <= 0 {
		o.idle = defaultIdleTimeout
	}
	if o.maxResponse <= 0 {
		o.maxResponse = defaultMaxResponse
	}
	o.maxResponse = min(o.maxResponse, maxResponseCap)
	o.maxWire = min(o.maxResponse*wireFactor, maxWireCap)
	if o.maxCalls <= 0 {
		o.maxCalls = defaultMaxToolCalls
	}
	if cfg.APIKey != "" {
		k := cfg.APIKey
		o.key = &k
	}
	return o, nil
}

// HostAllowed reports whether host (with or without a port) is on the
// allow-list. An empty list allows everything. Entries match the host with or
// without its port, case-insensitively, with or without IPv6 brackets.
func HostAllowed(host string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	norm := func(s string) string {
		return strings.NewReplacer("[", "", "]", "").Replace(strings.ToLower(strings.TrimSpace(s)))
	}
	hostname := (&url.URL{Host: host}).Hostname()
	for _, a := range allowed {
		a = norm(a)
		if a != "" && (a == norm(host) || a == norm(hostname)) {
			return true
		}
	}
	return false
}

// ValidAPIKey reports whether k is non-empty printable ASCII without spaces,
// which is what an Authorization header can carry.
func ValidAPIKey(k string) bool {
	if k == "" {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x21 || k[i] > 0x7e {
			return false
		}
	}
	return true
}

// IsLoopbackHost reports whether host (with or without a port) is localhost or
// a loopback IP address.
func IsLoopbackHost(host string) bool {
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	h = strings.Trim(strings.ToLower(h), "[]")
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func statusError(code int) error {
	switch {
	case code == http.StatusOK:
		return nil
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return fmt.Errorf("%w: status %d", ErrAuth, code)
	case code == http.StatusTooManyRequests:
		return fmt.Errorf("%w: status %d", ErrRateLimited, code)
	case code == http.StatusRequestTimeout || code >= 500:
		return fmt.Errorf("%w: status %d", ErrUnavailable, code)
	default:
		return fmt.Errorf("%w: status %d", ErrRejected, code)
	}
}

// Complete implements Provider. With streaming on it streams too, so the
// idle timeout applies, and reports no deltas.
func (o *OpenAI) Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error) {
	return o.CompleteStream(ctx, req, nil)
}

// CompleteStream implements Streamer. An endpoint that answers a streaming
// request with 400 or 422 gets it once more without stream_options, and one
// that answers 400, 404 or 501 then once without streaming; what worked is
// remembered per model for an hour. Deltas of a call that fails are not the
// answer. A stream that fails after it started returns, with the error, a
// response whose only content is the estimated Usage of what it received
// (UsageEstimated set), so that a caller can charge the tokens spent.
func (o *OpenAI) CompleteStream(ctx context.Context, req CompletionRequest, onDelta func(Delta)) (CompletionResponse, error) {
	if err := req.Validate(); err != nil {
		return CompletionResponse{}, err
	}
	model := o.model
	if req.Model != "" {
		model = req.Model
	}
	if !o.stream || o.refuses(model, refusesStream) {
		resp, _, err := o.post(ctx, req, false, false, nil)
		return resp, err
	}
	opts := !o.refuses(model, refusesStreamOptions)
	resp, status, err := o.post(ctx, req, true, opts, onDelta)
	if opts && (status == http.StatusBadRequest || status == http.StatusUnprocessableEntity) {
		resp, status, err = o.post(ctx, req, true, false, onDelta)
		if status == http.StatusOK {
			o.remember(model, refusesStreamOptions)
		}
	}
	if !streamRefused(status) {
		return resp, err
	}
	resp, status, err = o.post(ctx, req, false, false, nil)
	if status == http.StatusOK {
		o.remember(model, refusesStream)
	}
	return resp, err
}

func (o *OpenAI) refuses(model string, what refusal) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	k := learntKey{model, what}
	until, ok := o.learnt[k]
	if ok && o.now().Before(until) {
		return true
	}
	delete(o.learnt, k)
	return false
}

func (o *OpenAI) remember(model string, what refusal) {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := o.now()
	if len(o.learnt) >= learntKeep {
		for k, until := range o.learnt {
			if !now.Before(until) {
				delete(o.learnt, k)
			}
		}
		if len(o.learnt) >= learntKeep {
			clear(o.learnt)
		}
	}
	o.learnt[learntKey{model, what}] = now.Add(learntTTL)
}

func streamRefused(status int) bool {
	return status == http.StatusBadRequest || status == http.StatusNotFound || status == http.StatusNotImplemented
}

// post makes one request and returns the HTTP status (0 when none arrived).
func (o *OpenAI) post(ctx context.Context, req CompletionRequest, stream, opts bool, onDelta func(Delta)) (CompletionResponse, int, error) {
	w := o.encode(req)
	if stream {
		w.Stream = true
		if opts {
			w.StreamOptions = &wireStreamOptions{IncludeUsage: true}
		}
	}
	body, err := json.Marshal(w)
	if err != nil {
		return CompletionResponse{}, 0, fmt.Errorf("%w: encoding failed", ErrInvalid)
	}
	rctx := ctx
	var idle *time.Timer
	if stream {
		var cancel context.CancelCauseFunc
		rctx, cancel = context.WithCancelCause(ctx)
		defer cancel(nil)
		idle = time.AfterFunc(o.idle, func() { cancel(errIdle) })
		defer idle.Stop()
	}
	hr, err := http.NewRequestWithContext(rctx, http.MethodPost, o.endpoint, bytes.NewReader(body))
	if err != nil {
		return CompletionResponse{}, 0, fmt.Errorf("%w: building the request failed", ErrInvalid)
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Accept", "application/json")
	if stream {
		hr.Header.Set("Accept", "text/event-stream")
	}
	if o.key != nil {
		hr.Header.Set("Authorization", "Bearer "+o.key.Reveal())
	}

	resp, err := o.client.Do(hr)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return CompletionResponse{}, 0, fmt.Errorf("llm: %w", cerr)
		}
		return CompletionResponse{}, 0, fmt.Errorf("%w: transport error", ErrUnavailable)
	}
	defer resp.Body.Close()
	if err := statusError(resp.StatusCode); err != nil {
		return CompletionResponse{}, resp.StatusCode, err
	}
	var rd io.Reader = resp.Body
	if stream {
		idle.Reset(o.idle)
		act := &activity{r: resp.Body, t: idle, d: o.idle}
		rd = act
		if isEventStream(resp.Header.Get("Content-Type")) {
			out, err := o.readStream(ctx, act, len(body), onDelta)
			return out, resp.StatusCode, err
		}
	}
	raw, err := io.ReadAll(io.LimitReader(rd, o.maxResponse+1))
	if err != nil {
		return CompletionResponse{}, resp.StatusCode, readError(ctx)
	}
	if int64(len(raw)) > o.maxResponse {
		return CompletionResponse{}, resp.StatusCode, ErrTooLarge
	}
	out, err := o.decode(raw)
	return out, resp.StatusCode, err
}

// readError is the error of a failed read: the caller's context error, or
// an outage (a timeout, the idle timeout, a lost connection).
func readError(ctx context.Context) error {
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("llm: %w", cerr)
	}
	return fmt.Errorf("%w: reading the response failed", ErrUnavailable)
}

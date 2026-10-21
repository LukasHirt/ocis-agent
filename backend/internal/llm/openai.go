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
	"time"
)

const (
	defaultMaxResponse  = 4 << 20
	defaultMaxToolCalls = 16
	maxResponseCap      = 64 << 20
	defaultTimeout      = 120 * time.Second
)

// OpenAIConfig configures an OpenAI-compatible chat-completions client.
type OpenAIConfig struct {
	BaseURL          string // e.g. https://api.openai.com/v1
	APIKey           Secret // may be empty only for a loopback host
	Model            string
	AllowedHosts     []string // egress guard; empty means the base URL's host
	AllowHTTP        bool     // permit http:// (loopback endpoints and tests)
	HTTPClient       *http.Client
	MaxResponseBytes int64 // default 4 MiB
	MaxToolCalls     int   // default 16
	// Timeout bounds one whole call, reading the response included; 0 means
	// the HTTPClient's timeout, or 120s. A slow local model may need more.
	Timeout time.Duration
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
}

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

	client := &http.Client{Timeout: defaultTimeout}
	if cfg.HTTPClient != nil {
		c := *cfg.HTTPClient
		client = &c
	}
	if cfg.Timeout > 0 {
		client.Timeout = cfg.Timeout
	}
	if client.Timeout <= 0 {
		client.Timeout = defaultTimeout
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	o := &OpenAI{
		endpoint:    strings.TrimRight(u.String(), "/") + "/chat/completions",
		model:       cfg.Model,
		client:      client,
		maxResponse: cfg.MaxResponseBytes,
		maxCalls:    cfg.MaxToolCalls,
		effort:      cfg.ReasoningEffort,
	}
	if o.maxResponse <= 0 {
		o.maxResponse = defaultMaxResponse
	}
	o.maxResponse = min(o.maxResponse, maxResponseCap)
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

// Complete implements Provider.
func (o *OpenAI) Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error) {
	if err := req.Validate(); err != nil {
		return CompletionResponse{}, err
	}
	body, err := json.Marshal(o.encode(req))
	if err != nil {
		return CompletionResponse{}, fmt.Errorf("%w: encoding failed", ErrInvalid)
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, o.endpoint, bytes.NewReader(body))
	if err != nil {
		return CompletionResponse{}, fmt.Errorf("%w: building the request failed", ErrInvalid)
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Accept", "application/json")
	if o.key != nil {
		hr.Header.Set("Authorization", "Bearer "+o.key.Reveal())
	}

	resp, err := o.client.Do(hr)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return CompletionResponse{}, fmt.Errorf("llm: %w", cerr)
		}
		return CompletionResponse{}, fmt.Errorf("%w: transport error", ErrUnavailable)
	}
	defer resp.Body.Close()
	if err := statusError(resp.StatusCode); err != nil {
		return CompletionResponse{}, err
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, o.maxResponse+1))
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return CompletionResponse{}, fmt.Errorf("llm: %w", cerr)
		}
		return CompletionResponse{}, fmt.Errorf("%w: reading the response failed", ErrUnavailable)
	}
	if int64(len(raw)) > o.maxResponse {
		return CompletionResponse{}, ErrTooLarge
	}
	return o.decode(raw)
}

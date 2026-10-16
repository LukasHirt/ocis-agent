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
	maxCallIDLen        = 128
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
}

// NewOpenAI validates the configuration and returns the client.
func NewOpenAI(cfg OpenAIConfig) (*OpenAI, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Host == "" {
		return nil, errors.New("llm: base URL is not a valid URL")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !cfg.AllowHTTP {
			return nil, errors.New("llm: base URL must use https")
		}
	default:
		return nil, errors.New("llm: base URL must start with https://")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("llm: base URL must not contain credentials, a query or a fragment")
	}
	if cfg.Model == "" {
		return nil, errors.New("llm: a model is required")
	}
	if !hostAllowed(u, cfg.AllowedHosts) {
		return nil, errors.New("llm: base URL host is not on the allow-list")
	}
	if cfg.APIKey == "" && !IsLoopbackHost(u.Host) {
		return nil, errors.New("llm: an API key is required for a non-loopback endpoint")
	}

	client := &http.Client{Timeout: defaultTimeout}
	if cfg.HTTPClient != nil {
		c := *cfg.HTTPClient
		client = &c
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	o := &OpenAI{
		endpoint:    strings.TrimRight(u.String(), "/") + "/chat/completions",
		model:       cfg.Model,
		client:      client,
		maxResponse: cfg.MaxResponseBytes,
		maxCalls:    cfg.MaxToolCalls,
	}
	if o.maxResponse <= 0 {
		o.maxResponse = defaultMaxResponse
	}
	if o.maxCalls <= 0 {
		o.maxCalls = defaultMaxToolCalls
	}
	if cfg.APIKey != "" {
		k := cfg.APIKey
		o.key = &k
	}
	return o, nil
}

func hostAllowed(u *url.URL, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, a := range allowed {
		a = strings.ToLower(strings.TrimSpace(a))
		if a != "" && (a == strings.ToLower(u.Host) || a == strings.ToLower(u.Hostname())) {
			return true
		}
	}
	return false
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

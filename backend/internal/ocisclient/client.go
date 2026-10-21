// Package ocisclient is a typed client for the public oCIS APIs the agent uses:
// Graph, WebDAV, auth-app and the SSE notification stream. It is stateless:
// every call takes the credentials to use, so one Client serves every user.
package ocisclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	requestTimeout = 30 * time.Second
	// maxBody caps buffered JSON and PROPFIND responses.
	maxBody = 4 << 20
)

// Client talks to one oCIS instance.
type Client struct {
	base    string
	hc      *http.Client
	sseIdle time.Duration
	// tooEarlyWaits are the pauses before each retry of a GET that oCIS
	// answered 425 (a fresh upload still in postprocessing); a field for tests
	tooEarlyWaits []time.Duration
}

// defaultTooEarlyWaits wait out about three seconds of postprocessing.
var defaultTooEarlyWaits = []time.Duration{
	100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond, 1600 * time.Millisecond,
}

// Option configures New.
type Option func(*Client)

// WithHTTPClient replaces the default HTTP client (see NewHTTPClient). A nil
// client is ignored. The client is copied and the copy never follows redirects,
// so credentials cannot be forwarded; the caller's client is not modified.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc == nil {
			return
		}
		h := *hc
		h.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		c.hc = &h
	}
}

// WithSSEIdleTimeout sets how long StreamEvents waits without receiving any
// bytes (heartbeats count) before it gives up. The default is two minutes.
func WithSSEIdleTimeout(d time.Duration) Option { return func(c *Client) { c.sseIdle = d } }

// New returns a client for the oCIS origin baseURL, for example
// https://ocis.example.org. The URL must be an origin: scheme and host only.
func New(baseURL string, opts ...Option) (*Client, error) {
	u, err := url.Parse(baseURL)
	// the error text never echoes the input: it may contain credentials
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("ocisclient: base URL must be an http(s) origin without credentials, path or query")
	}
	c := &Client{base: u.Scheme + "://" + u.Host, hc: NewHTTPClient(false), sseIdle: 2 * time.Minute, tooEarlyWaits: defaultTooEarlyWaits}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// NewHTTPClient returns the default HTTP client: no redirects are followed
// (a redirect must never carry credentials elsewhere) and response headers
// must arrive within 30 seconds. insecure skips TLS verification and exists
// only for the self-signed dev stack.
func NewHTTPClient(insecure bool) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = requestTimeout
	if insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // dev stack only
	}
	return &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

type response struct {
	status int
	header http.Header
	body   []byte
}

// newRequest builds a request for the escaped URL path escPath.
func (c *Client) newRequest(ctx context.Context, cr Creds, method, escPath string, query url.Values, hdr map[string]string, body []byte) (*http.Request, error) {
	if !strings.HasPrefix(escPath, "/") || strings.HasPrefix(escPath, "//") || strings.ContainsAny(escPath, "?#") {
		return nil, ErrInvalidPath
	}
	u, err := url.Parse(c.base + escPath)
	if err != nil {
		return nil, ErrInvalidPath
	}
	if b, err := url.Parse(c.base); err != nil || u.Scheme != b.Scheme || u.Host != b.Host || u.User != nil {
		return nil, ErrInvalidPath
	}
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return nil, transportError(method, u.Path, err)
	}
	req.Header.Set("User-Agent", "ocis-agent")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	cr.apply(req)
	return req, nil
}

// do sends one request and buffers at most limit bytes of a 2xx body. Bodies
// of non-2xx responses are dropped: they are never used and may hold details
// that must not travel into errors or logs. The caller interprets the status.
func (c *Client) do(ctx context.Context, cr Creds, method, escPath string, query url.Values, hdr map[string]string, body []byte, limit int64) (response, error) {
	if !cr.valid() {
		return response{}, ErrNoCredentials
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := c.newRequest(ctx, cr, method, escPath, query, hdr, body)
	if err != nil {
		return response{}, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return response{}, transportError(method, req.URL.Path, err)
	}
	defer resp.Body.Close()
	out := response{status: resp.StatusCode, header: resp.Header}
	if resp.StatusCode/100 != 2 {
		return out, nil
	}
	if resp.ContentLength > limit {
		return response{}, ErrTooLarge
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return response{}, transportError(method, req.URL.Path, err)
	}
	if int64(len(b)) > limit {
		return response{}, ErrTooLarge
	}
	out.body = b
	return out, nil
}

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

type cred struct{ user, secret string }

type response struct {
	status int
	header http.Header
	body   []byte
}

type client struct {
	base string
	http *http.Client
}

func newClient(base string, insecure bool) *client {
	tr := &http.Transport{
		// dev stack uses a self-signed certificate; never used outside this spike
		TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure}, //nolint:gosec
	}
	// no client-level timeout: SSE streams are bounded by their context instead
	return &client{base: base, http: &http.Client{Transport: tr}}
}

func (c *client) do(ctx context.Context, cr cred, method, path string, hdr map[string]string, body io.Reader) (response, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return response{}, err
	}
	req.SetBasicAuth(cr.user, cr.secret)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return response{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return response{}, err
	}
	return response{status: resp.StatusCode, header: resp.Header, body: b}, nil
}

type appToken struct {
	Token          string    `json:"token"`
	ExpirationDate time.Time `json:"expiration_date"`
	CreatedDate    time.Time `json:"created_date"`
	Label          string    `json:"label"`
}

// createToken calls POST /auth-app/tokens. The returned *appToken is nil when
// the server did not answer 2xx; the raw response is always returned.
func (c *client) createToken(ctx context.Context, cr cred, label string) (response, *appToken, error) {
	q := url.Values{"expiry": {"1h"}, "label": {label}}
	resp, err := c.do(ctx, cr, http.MethodPost, "/auth-app/tokens?"+q.Encode(), nil, nil)
	if err != nil {
		return response{}, nil, err
	}
	if resp.status/100 != 2 {
		return resp, nil, nil
	}
	var t appToken
	if err := json.Unmarshal(resp.body, &t); err != nil || t.Token == "" {
		return resp, nil, fmt.Errorf("2xx from /auth-app/tokens but no token in body: %v", err)
	}
	return resp, &t, nil
}

func (c *client) deleteToken(ctx context.Context, cr cred, token string) (response, error) {
	return c.do(ctx, cr, http.MethodDelete, "/auth-app/tokens?token="+url.QueryEscape(token), nil, nil)
}

func (c *client) me(ctx context.Context, cr cred) (response, error) {
	return c.do(ctx, cr, http.MethodGet, "/graph/v1.0/me", nil, nil)
}

func (c *client) personalDriveID(ctx context.Context, cr cred) (string, error) {
	resp, err := c.do(ctx, cr, http.MethodGet, "/graph/v1.0/me/drives", nil, nil)
	if err != nil {
		return "", err
	}
	if resp.status != http.StatusOK {
		return "", fmt.Errorf("GET /me/drives = %d", resp.status)
	}
	var out struct {
		Value []struct {
			ID        string `json:"id"`
			DriveType string `json:"driveType"`
		} `json:"value"`
	}
	if err := json.Unmarshal(resp.body, &out); err != nil {
		return "", err
	}
	for _, d := range out.Value {
		if d.DriveType == "personal" {
			return d.ID, nil
		}
	}
	return "", errors.New("no personal drive found")
}

// davPath returns the WebDAV path of name inside the drive.
func davPath(driveID, name string) string {
	return "/remote.php/dav/spaces/" + url.PathEscape(driveID) + "/" + name
}

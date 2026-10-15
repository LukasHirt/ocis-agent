package ocisclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"
)

const tokensPath = "/auth-app/tokens"

// Token is a freshly created app token. Value is the only copy of the secret.
type Token struct {
	Value     Secret
	Label     string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// TokenInfo is a listed app token. Hash is what DELETE needs; the plaintext
// value is never listed.
type TokenInfo struct {
	Hash      Secret
	Label     string
	CreatedAt time.Time
	ExpiresAt time.Time
}

type tokenWire struct {
	Token   string    `json:"token"`
	Label   string    `json:"label"`
	Created time.Time `json:"created_date"`
	Expires time.Time `json:"expiration_date"`
}

// CreateToken creates an app token for the calling user. cr may be the user's
// OIDC token or an app token (that is how a token renews itself).
func (c *Client) CreateToken(ctx context.Context, cr Creds, expiry time.Duration, label string) (Token, error) {
	if expiry <= 0 || label == "" {
		return Token{}, errors.New("ocisclient: a token needs a positive expiry and a label")
	}
	q := url.Values{"expiry": {expiry.String()}, "label": {label}}
	resp, err := c.do(ctx, cr, http.MethodPost, tokensPath, q, nil, nil, maxBody)
	if err != nil {
		return Token{}, err
	}
	if resp.status != http.StatusOK && resp.status != http.StatusCreated {
		return Token{}, statusError(http.MethodPost, tokensPath, resp.status, nil)
	}
	var w tokenWire
	if err := json.Unmarshal(resp.body, &w); err != nil || w.Token == "" {
		return Token{}, errors.New("ocisclient: POST " + tokensPath + ": no token in the response")
	}
	return Token{Value: Secret(w.Token), Label: w.Label, CreatedAt: w.Created, ExpiresAt: w.Expires}, nil
}

// ListTokens lists the calling user's app tokens.
func (c *Client) ListTokens(ctx context.Context, cr Creds) ([]TokenInfo, error) {
	resp, err := c.do(ctx, cr, http.MethodGet, tokensPath, nil, nil, nil, maxBody)
	if err != nil {
		return nil, err
	}
	if resp.status != http.StatusOK {
		return nil, statusError(http.MethodGet, tokensPath, resp.status, nil)
	}
	var ws []tokenWire
	if err := json.Unmarshal(resp.body, &ws); err != nil {
		return nil, errors.New("ocisclient: GET " + tokensPath + ": malformed response")
	}
	out := make([]TokenInfo, 0, len(ws))
	for _, w := range ws {
		out = append(out, TokenInfo{Hash: Secret(w.Token), Label: w.Label, CreatedAt: w.Created, ExpiresAt: w.Expires})
	}
	return out, nil
}

// DeleteToken deletes the token with the given listed hash (from ListTokens).
// oCIS answers 500 to the plaintext token. The hash travels in the query
// string, which no error or log line ever includes.
func (c *Client) DeleteToken(ctx context.Context, cr Creds, hash Secret) error {
	if hash == "" {
		return errors.New("ocisclient: a token hash is required")
	}
	resp, err := c.do(ctx, cr, http.MethodDelete, tokensPath, url.Values{"token": {hash.Reveal()}}, nil, nil, maxBody)
	if err != nil {
		return err
	}
	if resp.status == http.StatusOK || resp.status == http.StatusNoContent {
		return nil
	}
	return statusError(http.MethodDelete, tokensPath, resp.status, nil)
}

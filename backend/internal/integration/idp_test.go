//go:build integration

package integration

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The oCIS built-in IdP (LibreGraph Connect, "lico") has no password grant.
// Its web login is a small JSON API behind the sign-in page:
//
//  1. POST /signin/v1/identifier/_/logon with {"params":[user, password, "1"],
//     "hello":{...}} ("1" is the username-and-password mode). It needs the
//     header Kopano-Konnect-XSRF: 1 and an Origin or Referer of the same host,
//     and answers 200 with a logon cookie (204 for wrong credentials).
//  2. GET the authorization endpoint (from the discovery document) with that
//     cookie: a PKCE authorization-code request of the "web" client (the
//     client oCIS Web uses, from /config.json), redirect URI
//     <oCIS URL>/oidc-callback.html (the one oCIS registers for it). The
//     trusted web client needs no consent, so the answer is a 302 to the
//     redirect URI with the code.
//  3. POST the token endpoint with the code and the PKCE verifier (public
//     client, no secret).
//
// The result is the same access token the browser gets; the sidecar validates
// it with oCIS like any other bearer.

// oidcToken is an access token and when it stops being usable. The value is
// never printed: String hides it.
type oidcToken struct {
	value   string
	expires time.Time
}

func (t oidcToken) String() string { return "[redacted]" }

type idpLogin struct {
	base     string // oCIS URL, no trailing slash
	user     string
	password string
	client   *http.Client // TLS settings only; login makes its own cookie jar
}

// login runs the flow above and returns the access token.
func (l idpLogin) login(ctx context.Context) (oidcToken, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return oidcToken{}, err
	}
	c := &http.Client{
		Transport: l.client.Transport,
		Jar:       jar,
		Timeout:   30 * time.Second,
		// the authorize step's 302 carries the code: read it, never follow it
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	var disc struct {
		Authorization string `json:"authorization_endpoint"`
		Token         string `json:"token_endpoint"`
	}
	if err := getJSON(ctx, c, l.base+"/.well-known/openid-configuration", &disc); err != nil {
		return oidcToken{}, fmt.Errorf("idp: discovery: %w", err)
	}
	var webCfg struct {
		OIDC struct {
			ClientID string `json:"client_id"`
			Scope    string `json:"scope"`
		} `json:"openIdConnect"`
	}
	if err := getJSON(ctx, c, l.base+"/config.json", &webCfg); err != nil {
		return oidcToken{}, fmt.Errorf("idp: web config: %w", err)
	}
	clientID, scope := webCfg.OIDC.ClientID, webCfg.OIDC.Scope
	if clientID == "" {
		clientID = "web"
	}
	if scope == "" {
		scope = "openid profile email"
	}
	redirect := l.base + "/oidc-callback.html"
	if disc.Authorization == "" || disc.Token == "" {
		return oidcToken{}, errors.New("idp: discovery without authorization or token endpoint")
	}

	// 1. logon
	hello := map[string]string{"scope": scope, "client_id": clientID, "redirect_uri": redirect, "flow": "oidc"}
	body, _ := json.Marshal(map[string]any{
		"state": randomString(12), "params": []string{l.user, l.password, "1"}, "hello": hello,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.base+"/signin/v1/identifier/_/logon", strings.NewReader(string(body)))
	if err != nil {
		return oidcToken{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Kopano-Konnect-XSRF", "1")
	req.Header.Set("Origin", l.base)
	req.Header.Set("Referer", l.base+"/signin/v1/identifier")
	resp, err := c.Do(req)
	if err != nil {
		return oidcToken{}, fmt.Errorf("idp: logon: %w", err)
	}
	var logon struct {
		Success bool `json:"success"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&logon)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || err != nil || !logon.Success {
		return oidcToken{}, fmt.Errorf("idp: logon refused (status %d)", resp.StatusCode)
	}

	// 2. authorize
	verifier := randomString(48)
	sum := sha256.Sum256([]byte(verifier))
	state := randomString(16)
	q := url.Values{
		"client_id": {clientID}, "response_type": {"code"}, "scope": {scope}, "redirect_uri": {redirect},
		"state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, disc.Authorization+"?"+q.Encode(), nil)
	if err != nil {
		return oidcToken{}, err
	}
	resp, err = c.Do(req)
	if err != nil {
		return oidcToken{}, fmt.Errorf("idp: authorize: %w", err)
	}
	resp.Body.Close()
	loc, err := resp.Location()
	if resp.StatusCode != http.StatusFound || err != nil {
		return oidcToken{}, fmt.Errorf("idp: authorize answered %d, not a redirect", resp.StatusCode)
	}
	if !strings.HasPrefix(loc.String(), redirect) {
		return oidcToken{}, errors.New("idp: authorize redirected somewhere else than the redirect URI")
	}
	if e := loc.Query().Get("error"); e != "" {
		return oidcToken{}, fmt.Errorf("idp: authorize error %q", e)
	}
	code := loc.Query().Get("code")
	if code == "" || loc.Query().Get("state") != state {
		return oidcToken{}, errors.New("idp: authorize redirect without code or with another state")
	}

	// 3. token
	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect},
		"client_id": {clientID}, "code_verifier": {verifier},
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodPost, disc.Token, strings.NewReader(form.Encode()))
	if err != nil {
		return oidcToken{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err = c.Do(req)
	if err != nil {
		return oidcToken{}, fmt.Errorf("idp: token: %w", err)
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok)
	if resp.StatusCode != http.StatusOK || err != nil || tok.AccessToken == "" {
		return oidcToken{}, fmt.Errorf("idp: token exchange answered %d", resp.StatusCode)
	}
	if tok.ExpiresIn <= 0 {
		tok.ExpiresIn = 300
	}
	return oidcToken{value: tok.AccessToken, expires: time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)}, nil
}

func getJSON(ctx context.Context, c *http.Client, u string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(dst)
}

// randomString returns n URL-safe random characters.
func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)[:n]
}

// TestIDPLoginAgainstFake checks the login flow against a fake IdP that
// enforces what lico enforces (XSRF header, same-origin, cookie, PKCE).
func TestIDPLoginAgainstFake(t *testing.T) {
	var challenge, base string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"authorization_endpoint":%q,"token_endpoint":%q}`, base+"/authorize", base+"/token")
	})
	mux.HandleFunc("GET /config.json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"openIdConnect":{"client_id":"web","scope":"openid profile email"}}`)
	})
	mux.HandleFunc("POST /signin/v1/identifier/_/logon", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Params []string          `json:"params"`
			Hello  map[string]string `json:"hello"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if r.Header.Get("Kopano-Konnect-XSRF") != "1" || r.Header.Get("Origin") != base ||
			len(in.Params) != 3 || in.Params[0] != "einstein" || in.Params[1] != "relativity" || in.Params[2] != "1" ||
			in.Hello["client_id"] != "web" || in.Hello["redirect_uri"] != base+"/oidc-callback.html" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "__Secure-KKT", Value: "ok", Path: "/"})
		fmt.Fprint(w, `{"success":true}`)
	})
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("__Secure-KKT"); err != nil || c.Value != "ok" {
			http.Error(w, "no session", http.StatusUnauthorized)
			return
		}
		q := r.URL.Query()
		if q.Get("code_challenge_method") != "S256" || q.Get("client_id") != "web" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		challenge = q.Get("code_challenge")
		http.Redirect(w, r, q.Get("redirect_uri")+"?code=the-code&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if r.PostForm.Get("code") != "the-code" || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge ||
			r.PostForm.Get("grant_type") != "authorization_code" {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, `{"access_token":"tok-123","expires_in":300}`)
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	base = srv.URL

	ctx := context.Background()
	tok, err := idpLogin{base: base, user: "einstein", password: "relativity", client: srv.Client()}.login(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if tok.value != "tok-123" || time.Until(tok.expires) < 4*time.Minute {
		t.Fatalf("unexpected token (expires %v)", tok.expires)
	}
	if s := fmt.Sprint(tok); strings.Contains(s, "tok-123") {
		t.Fatal("the token prints in clear")
	}
	if _, err := (idpLogin{base: base, user: "einstein", password: "wrong", client: srv.Client()}).login(ctx); err == nil {
		t.Fatal("a wrong password logged in")
	}
}

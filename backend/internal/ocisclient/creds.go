package ocisclient

import (
	"log/slog"
	"net/http"
)

const redacted = "[redacted]"

// Secret is a credential value: an app token, a token hash or an OIDC token.
// It never prints itself, so a secret that reaches a log line, an error or a
// JSON body shows as [redacted]. Call Reveal where the raw value is needed.
//
// Caution: fmt cannot call String on an unexported struct field, so a bare
// Secret held in an unexported field prints in clear when the struct is
// printed. Hold credentials as Creds (which keeps the value behind a pointer)
// and never print structs with unexported Secret fields.
type Secret string

func (Secret) String() string               { return redacted }
func (Secret) GoString() string             { return redacted }
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }
func (Secret) LogValue() slog.Value         { return slog.StringValue(redacted) }

// Reveal returns the raw value.
func (s Secret) Reveal() string { return string(s) }

type credKind int

const (
	credNone credKind = iota
	credApp
	credBearer
)

// Creds authenticate one request: an app token (HTTP Basic with the login
// name) or the user's OIDC token (Bearer). The zero value has no credentials.
type Creds struct {
	kind   credKind
	user   string
	secret *Secret // behind a pointer so a nested print shows an address, not the value
}

// AppToken returns credentials for an app token. user is the login name
// (Graph /me onPremisesSamAccountName), not the user id.
func AppToken(user string, token Secret) Creds {
	return Creds{kind: credApp, user: user, secret: &token}
}

// Bearer returns credentials for the user's OIDC access token.
func Bearer(token Secret) Creds { return Creds{kind: credBearer, secret: &token} }

// Kind names the credential for the audit log: "app", "oidc" or "".
func (c Creds) Kind() string {
	switch c.kind {
	case credApp:
		return "app"
	case credBearer:
		return "oidc"
	}
	return ""
}

func (c Creds) valid() bool {
	switch c.kind {
	case credApp:
		return c.user != "" && c.secret != nil && *c.secret != ""
	case credBearer:
		return c.secret != nil && *c.secret != ""
	}
	return false
}

func (c Creds) apply(r *http.Request) {
	switch c.kind {
	case credApp:
		r.SetBasicAuth(c.user, c.secret.Reveal())
	case credBearer:
		r.Header.Set("Authorization", "Bearer "+c.secret.Reveal())
	}
}

// String never includes the secret.
func (c Creds) String() string {
	switch c.kind {
	case credApp:
		return "ocisclient.Creds{app " + c.user + " " + redacted + "}"
	case credBearer:
		return "ocisclient.Creds{oidc " + redacted + "}"
	}
	return "ocisclient.Creds{none}"
}

func (c Creds) GoString() string     { return c.String() }
func (c Creds) LogValue() slog.Value { return slog.StringValue(c.String()) }

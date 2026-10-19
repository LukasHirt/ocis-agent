package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/auth"
	"github.com/owncloud/ocis-agent/backend/internal/llm"
	"github.com/owncloud/ocis-agent/backend/internal/memory"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient/fakeocis"
	"github.com/owncloud/ocis-agent/backend/internal/secrets"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/store/sqlite"
)

var bg = context.Background()

const (
	einsteinID    = "u-einstein"
	einsteinDrive = "d-einstein"
	marieID       = "u-marie"
	marieDrive    = "d-marie"
)

// stack is the API over the real components: sqlite, secrets, the oCIS
// client against fakeocis, the real auth.Validator and auth.Service. Every
// clock is the fake's clock, so token expiry, step times and the budget day
// agree.
type stack struct {
	*fixture
	fake   *fakeocis.Server
	client *ocisclient.Client
	svc    *auth.Service
	val    *auth.Validator
}

var testCatalog = llm.Catalog{
	Default: "model-a",
	Models:  []llm.ModelInfo{{ID: "model-a", Label: "Model A"}, {ID: "model-b", Label: "Model B"}},
}

func newStack(t *testing.T, mod ...func(*Deps)) *stack {
	t.Helper()
	fake := fakeocis.New(t)
	fake.AddUser("einstein", "relativity", einsteinID, einsteinDrive)
	fake.AddUser("marie", "radium", marieID, marieDrive)
	client, err := ocisclient.New(fake.URL)
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "stack.db"), sqlite.WithClock(fake.Now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sec, err := secrets.New(st, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	mem := memory.New(client, st, memory.Limits{})
	svc, err := auth.New(bg, auth.Deps{Store: st, Secrets: sec, Client: client, Memory: mem, Now: fake.Now})
	if err != nil {
		t.Fatal(err)
	}
	val := auth.NewValidator(client)
	s := &stack{fake: fake, client: client, svc: svc, val: val}
	all := append([]func(*Deps){func(d *Deps) {
		d.Store, d.Validator, d.Auth, d.Now, d.Catalog = st, val, svc, fake.Now, testCatalog
	}}, mod...)
	s.fixture = newFixture(t, all...)
	s.st = st
	s.bearer = fakeocis.OIDCToken("einstein")
	return s
}

// as returns a request of the given user (their OIDC bearer).
func (s *stack) as(login, method, target, body string) *httptest.ResponseRecorder {
	r := s.req(method, target, body)
	r.Header.Set("Authorization", "Bearer "+fakeocis.OIDCToken(login))
	return s.serve(r)
}

// enroll enrols the user through the API.
func (s *stack) enroll(t *testing.T, login string) {
	t.Helper()
	rec := s.as(login, http.MethodPost, "/agent-api/v1/enroll", "{}")
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("enroll %s = %d %s", login, rec.Code, rec.Body.String())
	}
}

// liveTokens returns the labels of the user's live tokens at the fake.
func (s *stack) liveTokens(login string) []string {
	var out []string
	for _, ti := range s.fake.Tokens() {
		if ti.User == login {
			out = append(out, ti.Label)
		}
	}
	return out
}

// ours counts the live tokens of login carrying this sidecar's prefix.
func (s *stack) ours(login string) int {
	n := 0
	for _, l := range s.liveTokens(login) {
		if strings.HasPrefix(l, s.svc.LabelPrefix()) {
			n++
		}
	}
	return n
}

// appPassword creates one of the user's own app passwords (not ours).
func (s *stack) appPassword(t *testing.T, login, label string) {
	t.Helper()
	cr := ocisclient.Bearer(ocisclient.Secret(fakeocis.OIDCToken(login)))
	if _, err := s.client.CreateToken(bg, cr, 24*time.Hour, label); err != nil {
		t.Fatal(err)
	}
}

func (s *stack) mustTx(t *testing.T, fn func(tx store.Tx) error) {
	t.Helper()
	if err := s.st.InTx(bg, fn); err != nil {
		t.Fatal(err)
	}
}

func (s *stack) user(t *testing.T, id string) (store.User, bool) {
	t.Helper()
	var u store.User
	var found bool
	err := s.st.View(bg, func(tx store.Tx) error {
		var err error
		u, err = tx.GetUser(id)
		if err == nil {
			found = true
			return nil
		}
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return u, found
}

func (s *stack) appToken(t *testing.T, id string) store.AppToken {
	t.Helper()
	var tok store.AppToken
	if err := s.st.View(bg, func(tx store.Tx) error {
		var err error
		tok, err = tx.GetAppToken(id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return tok
}

// decode decodes a JSON response into a generic map (for exact key checks).
func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return m
}

func statusDoc(t *testing.T, rec *httptest.ResponseRecorder) statusView {
	t.Helper()
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	var v statusView
	dec := json.NewDecoder(strings.NewReader(rec.Body.String()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("status body %q: %v", rec.Body.String(), err)
	}
	return v
}

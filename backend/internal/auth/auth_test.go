package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/audit"
	"github.com/owncloud/ocis-agent/backend/internal/memory"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient/fakeocis"
	"github.com/owncloud/ocis-agent/backend/internal/secrets"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/store/sqlite"
)

const (
	drive  = "6cb9$4c51"
	userID = "4c51"
)

type env struct {
	svc    *Service
	fake   *fakeocis.Server
	st     store.Store
	sec    *secrets.Service
	client *ocisclient.Client
	mem    *memory.Service
	ident  Identity
	bearer ocisclient.Secret
}

func (e env) now() time.Time { return e.fake.Now() }

// newEnv builds a service over the fake oCIS and a temporary SQLite store
// whose clock is the fake's clock, so token expiry, the store and the service
// agree on the time.
func newEnv(t *testing.T) env {
	t.Helper()
	fake := fakeocis.New(t)
	fake.AddUser("einstein", "relativity", userID, drive)
	client, err := ocisclient.New(fake.URL)
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(t.TempDir()+"/agent.db", sqlite.WithClock(fake.Now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sec, err := secrets.New(st, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	mem := memory.New(client, st, memory.Limits{})
	svc, err := New(bg, Deps{Store: st, Secrets: sec, Client: client, Memory: mem, Now: fake.Now})
	if err != nil {
		t.Fatal(err)
	}
	return env{
		svc: svc, fake: fake, st: st, sec: sec, client: client, mem: mem,
		ident:  Identity{UserID: userID, Username: "einstein", DisplayName: "Einstein", DriveID: drive},
		bearer: ocisclient.Secret(fakeocis.OIDCToken("einstein")),
	}
}

// seed stores a working token for username without using Enroll: user and
// label rows, a token created through the OIDC credential, sealed into the
// store. It returns the token's label.
func (e env) seed(t *testing.T, id Identity, ttl time.Duration) string {
	t.Helper()
	label := e.svc.newLabel()
	tok, err := e.client.CreateToken(bg, ocisclient.Bearer(ocisclient.Secret(fakeocis.OIDCToken(id.Username))), ttl, label)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := e.sec.Seal(bg, []byte(tok.Value.Reveal()), e.svc.aad(id.UserID))
	if err != nil {
		t.Fatal(err)
	}
	err = e.st.InTx(bg, func(tx store.Tx) error {
		if err := tx.PutUser(store.User{ID: id.UserID, Name: id.Username, PersonalDriveID: id.DriveID}); err != nil {
			return err
		}
		if err := tx.AddTokenLabel(id.UserID, label); err != nil {
			return err
		}
		return tx.PutAppToken(store.AppToken{
			UserID: id.UserID, Sealed: sealed, Label: label,
			CreatedAt: tok.CreatedAt, ExpiresAt: tok.ExpiresAt, Status: store.TokenValid,
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	return label
}

func (e env) policy(t *testing.T, id string) store.Policy {
	t.Helper()
	var p store.Policy
	if err := e.st.View(bg, func(tx store.Tx) error {
		var err error
		p, err = tx.GetPolicy(id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return p
}

func (e env) token(t *testing.T, id string) store.AppToken {
	t.Helper()
	var a store.AppToken
	if err := e.st.View(bg, func(tx store.Tx) error {
		var err error
		a, err = tx.GetAppToken(id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return a
}

func (e env) labels(t *testing.T, id string) []store.TokenLabel {
	t.Helper()
	var l []store.TokenLabel
	if err := e.st.View(bg, func(tx store.Tx) error {
		var err error
		l, err = tx.ListTokenLabels(id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return l
}

// events returns the audit records of the user.
func (e env) events(t *testing.T, id string) []audit.Record {
	t.Helper()
	var r []audit.Record
	if err := e.st.View(bg, func(tx store.Tx) error {
		var err error
		r, err = tx.ReadAudit(id, 0, 1000)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return r
}

func kinds(rs []audit.Record) string {
	var k []string
	for _, r := range rs {
		k = append(k, r.Kind)
	}
	return strings.Join(k, ",")
}

// noLeaks fails if any audit record of the user mentions a secret.
func (e env) noLeaks(t *testing.T, id string, secrets ...string) {
	t.Helper()
	for _, r := range e.events(t, id) {
		for _, s := range secrets {
			if s != "" && strings.Contains(r.Detail+r.Outcome+r.Kind+r.Cred, s) {
				t.Fatalf("audit record %q leaks a secret: %+v", r.Kind, r)
			}
		}
	}
}

// deleteRemote deletes the token with the label on the fake oCIS (as the user's
// OIDC credential), so the stored copy of it answers 401.
func (e env) deleteRemote(t *testing.T, label string) {
	t.Helper()
	cr := ocisclient.Bearer(e.bearer)
	toks, err := e.client.ListTokens(bg, cr)
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range toks {
		if tk.Label == label {
			if err := e.client.DeleteToken(bg, cr, tk.Hash); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatalf("no remote token with label %q", label)
}

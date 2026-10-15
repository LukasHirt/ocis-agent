package ocisclient

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/ocisclient/fakeocis"
)

func oidc(user string) Creds { return Bearer(Secret(fakeocis.OIDCToken(user))) }

func TestCreateTokenAndUseIt(t *testing.T) {
	f := newFixture(t)
	tok, err := f.c.CreateToken(bg, oidc("einstein"), 72*time.Hour, "ocis-agent-i1-n1")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Value == "" || tok.Label != "ocis-agent-i1-n1" {
		t.Fatalf("token = %+v", tok)
	}
	if got := tok.ExpiresAt.Sub(tok.CreatedAt); got != 72*time.Hour {
		t.Fatalf("lifetime = %s", got)
	}
	if out := fmt.Sprintf("%v %+v %#v", tok, tok, tok); containsStr(out, tok.Value.Reveal()) {
		t.Fatalf("the token value leaks through formatting: %s", out)
	}
	// the token authenticates as the user
	me, err := f.c.Me(bg, AppToken("einstein", tok.Value))
	if err != nil || me.ID != "4c51" {
		t.Fatalf("me with the new token = %+v %v", me, err)
	}
	// an app token can mint its successor (the self-renewal path)
	next, err := f.c.CreateToken(bg, AppToken("einstein", tok.Value), 72*time.Hour, "ocis-agent-i1-n2")
	if err != nil || next.Value == tok.Value {
		t.Fatalf("renewal = %+v %v", next, err)
	}
}

func containsStr(s, sub string) bool { return sub != "" && strings.Contains(s, sub) }

func TestCreateTokenValidatesBeforeSending(t *testing.T) {
	f := newFixture(t)
	if _, err := f.c.CreateToken(bg, oidc("einstein"), 0, "l"); err == nil {
		t.Error("zero expiry must fail")
	}
	if _, err := f.c.CreateToken(bg, oidc("einstein"), time.Hour, ""); err == nil {
		t.Error("empty label must fail")
	}
	if n := len(f.s.Requests()); n != 0 {
		t.Fatalf("%d request(s) sent for invalid input", n)
	}
	if _, err := f.c.CreateToken(bg, Bearer("nonsense"), time.Hour, "l"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("bad credentials = %v", err)
	}
}

func TestListTokensReturnsHashesNotValues(t *testing.T) {
	f := newFixture(t)
	a, _ := f.c.CreateToken(bg, oidc("einstein"), time.Hour, "first")
	b, _ := f.c.CreateToken(bg, oidc("einstein"), 2*time.Hour, "second")
	list, err := f.c.ListTokens(bg, AppToken("einstein", a.Value))
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %+v %v", list, err)
	}
	for _, ti := range list {
		if ti.Hash == "" || ti.Hash.Reveal() == a.Value.Reveal() || ti.Hash.Reveal() == b.Value.Reveal() {
			t.Fatalf("the list must carry a hash, got %+v", ti)
		}
		if ti.Label == "" || ti.CreatedAt.IsZero() || ti.ExpiresAt.IsZero() {
			t.Fatalf("incomplete info %+v", ti)
		}
		if out := fmt.Sprintf("%v %+v", ti, ti); containsStr(out, ti.Hash.Reveal()) {
			t.Fatalf("the hash leaks through formatting: %s", out)
		}
	}
}

func TestDeleteTokenNeedsTheListedHash(t *testing.T) {
	f := newFixture(t)
	a, _ := f.c.CreateToken(bg, oidc("einstein"), time.Hour, "victim")
	keep, _ := f.c.CreateToken(bg, oidc("einstein"), time.Hour, "keep")
	cr := AppToken("einstein", keep.Value)

	// the plaintext is refused by oCIS (500) and the error must not echo it
	err := f.c.DeleteToken(bg, cr, a.Value)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 500 {
		t.Fatalf("delete by plaintext = %v, want a 500 StatusError", err)
	}
	if containsStr(err.Error(), a.Value.Reveal()) {
		t.Fatalf("the error leaks the token: %v", err)
	}

	list, err := f.c.ListTokens(bg, cr)
	if err != nil {
		t.Fatal(err)
	}
	var hash Secret
	for _, ti := range list {
		if ti.Label == "victim" {
			hash = ti.Hash
		}
	}
	if err := f.c.DeleteToken(bg, cr, hash); err != nil {
		t.Fatalf("delete by hash: %v", err)
	}
	if _, err := f.c.Me(bg, AppToken("einstein", a.Value)); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("the deleted token must be rejected, got %v", err)
	}
	if _, err := f.c.Me(bg, cr); err != nil {
		t.Fatalf("other tokens stay valid: %v", err)
	}
	if err := f.c.DeleteToken(bg, cr, hash); !errors.As(err, &se) || se.Status != 500 || errors.Is(err, ErrNotFound) || containsStr(err.Error(), hash.Reveal()) {
		t.Fatalf("delete twice = %v, want a 500 StatusError", err)
	}
	if err := f.c.DeleteToken(bg, cr, ""); err == nil {
		t.Fatal("an empty hash must be refused")
	}
}

func TestAuthAppErrorsNeverContainTheHash(t *testing.T) {
	f := newFixture(t)
	f.s.Fail("DELETE", "/auth-app/", 503, 1)
	err := f.c.DeleteToken(bg, oidc("einstein"), "hash-SECRET-HASH")
	if err == nil || containsStr(err.Error(), "SECRET-HASH") {
		t.Fatalf("err = %v", err)
	}
}

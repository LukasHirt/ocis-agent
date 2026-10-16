package memory

import (
	"errors"
	"testing"
)

func TestPeekReturnsTheFileWithoutCachingItsETag(t *testing.T) {
	e := ready(t, Limits{})
	e.fake.PutFile("drive1", profile, []byte("user wrote this\n"))
	f, err := e.svc.Peek(bg, e.sess, "profile.md")
	if err != nil || f.Name != "profile.md" || f.Content != "user wrote this\n" || f.ETag == "" {
		t.Fatalf("peek = %+v, %v", f, err)
	}
	if got := e.cachedETag(t, "profile.md"); got != "" {
		t.Fatalf("Peek cached %q", got)
	}
	if _, err := e.svc.Write(bg, e.sess, "profile.md", "overwrite\n", nil); !errors.Is(err, ErrNotRead) {
		t.Fatalf("replace after a Peek = %v, want ErrNotRead", err)
	}
}

func TestPeekKeepsAnETagCachedByRead(t *testing.T) {
	e := ready(t, Limits{})
	e.fake.PutFile("drive1", profile, []byte("v1\n"))
	if _, err := e.svc.Read(bg, e.sess, "profile.md"); err != nil {
		t.Fatal(err)
	}
	before := e.cachedETag(t, "profile.md")
	if _, err := e.svc.Peek(bg, e.sess, "profile.md"); err != nil {
		t.Fatal(err)
	}
	if got := e.cachedETag(t, "profile.md"); got != before || got == "" {
		t.Fatalf("cache = %q, want %q", got, before)
	}
	if _, err := e.svc.Write(bg, e.sess, "profile.md", "v2\n", nil); err != nil {
		t.Fatalf("replace after Read then Peek: %v", err)
	}
}

func TestPeekOfADeletedFileKeepsTheCacheAndIsNotFound(t *testing.T) {
	e := ready(t, Limits{})
	if _, err := e.svc.Write(bg, e.sess, "profile.md", "v1\n", nil); err != nil {
		t.Fatal(err)
	}
	cached := e.cachedETag(t, "profile.md")
	if err := e.svc.files.Delete(bg, e.sess.Creds, "drive1", profile); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Peek(bg, e.sess, "profile.md"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("peek = %v, want ErrNotFound", err)
	}
	if got := e.cachedETag(t, "profile.md"); got != cached {
		t.Fatalf("Peek touched the cache: %q, want %q", got, cached)
	}
}

func TestPeekHasTheErrorsOfRead(t *testing.T) {
	e := ready(t, Limits{MaxReadBytes: 8})
	for _, name := range []string{"", "../x.md", "other.md"} {
		if _, err := e.svc.Peek(bg, e.sess, name); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Peek(%q) = %v", name, err)
		}
	}
	e.fake.PutFile("drive1", profile, []byte{0xff, 0xfe, 'a'})
	if _, err := e.svc.Peek(bg, e.sess, "profile.md"); !errors.Is(err, ErrNotText) {
		t.Errorf("invalid UTF-8 = %v", err)
	}
	e.fake.PutFile("drive1", profile, []byte("a\x00b"))
	if _, err := e.svc.Peek(bg, e.sess, "profile.md"); !errors.Is(err, ErrNotText) {
		t.Errorf("NUL = %v", err)
	}
	e.fake.PutFile("drive1", profile, []byte("123456789"))
	if _, err := e.svc.Peek(bg, e.sess, "profile.md"); !errors.Is(err, ErrTooLarge) {
		t.Errorf("too large = %v", err)
	}
	if _, err := e.svc.Peek(bg, e.sess, "projects.md"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing = %v", err)
	}
}

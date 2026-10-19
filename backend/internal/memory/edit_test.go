package memory

import (
	"errors"
	"strings"
	"testing"
)

func TestReplaceWritesWithTheExplicitETag(t *testing.T) {
	e := ready(t, Limits{})
	e.fake.PutFile("drive1", profile, []byte("# Me\n"))
	_, etag, _ := e.fake.File("drive1", profile)
	ch, err := e.svc.Replace(bg, e.sess, "profile.md", "# Me\nlikes tea\n", etag)
	if err != nil {
		t.Fatal(err)
	}
	_, now, _ := e.fake.File("drive1", profile)
	if ch.Created || ch.Before != "# Me\n" || ch.After != "# Me\nlikes tea\n" || ch.Diff != "+likes tea\n" || ch.ETag != now || now == etag {
		t.Fatalf("change = %+v (etag %q -> %q)", ch, etag, now)
	}
	if e.content(t, profile) != "# Me\nlikes tea\n" {
		t.Fatal("content not written")
	}
}

func TestReplaceWithAStaleETagIsAConflictAndLeavesTheFile(t *testing.T) {
	e := ready(t, Limits{})
	e.fake.PutFile("drive1", profile, []byte("v1\n"))
	_, old, _ := e.fake.File("drive1", profile)
	e.fake.PutFile("drive1", profile, []byte("v2\n"))
	if _, err := e.svc.Replace(bg, e.sess, "profile.md", "mine\n", old); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if e.content(t, profile) != "v2\n" {
		t.Fatal("file changed")
	}
}

func TestReplaceValidatesBeforeAnyRequest(t *testing.T) {
	e := ready(t, Limits{})
	e.fake.ResetRequests()
	big := strings.Repeat("a", 64<<10+1)
	cases := []struct {
		name, content, etag string
		want                error
	}{
		{"../x.md", "x", `"1"`, ErrInvalidName},
		{"profile.md", big, `"1"`, ErrTooLarge},
		{"profile.md", "a\xffb", `"1"`, ErrNotText},
		{"profile.md", "a\x00b", `"1"`, ErrNotText},
		{"profile.md", "x", "", ErrInvalid},
		{"profile.md", "x", "*", ErrInvalid},
	}
	for _, c := range cases {
		if _, err := e.svc.Replace(bg, e.sess, c.name, c.content, c.etag); !errors.Is(err, c.want) {
			t.Errorf("Replace(%q, etag %q) = %v, want %v", c.name, c.etag, err, c.want)
		}
	}
	if n := len(e.fake.Requests()); n != 0 {
		t.Fatalf("%d oCIS requests for invalid input", n)
	}
}

func TestReplaceOfAMissingFileIsNotFound(t *testing.T) {
	e := ready(t, Limits{})
	if _, err := e.svc.Replace(bg, e.sess, "projects.md", "x\n", `"1"`); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if _, _, ok := e.fake.File("drive1", "/.ocis-agent/memory/projects.md"); ok {
		t.Fatal("Replace created a file")
	}
}

func TestCreateMakesFoldersAndRefusesAnExistingFile(t *testing.T) {
	e := newEnv(t, Limits{}) // nothing ensured
	ch, err := e.svc.Create(bg, e.sess, "notes/ideas.md", "one\n")
	if err != nil {
		t.Fatal(err)
	}
	_, etag, ok := e.fake.File("drive1", "/.ocis-agent/memory/notes/ideas.md")
	if !ok || !ch.Created || ch.Before != "" || ch.After != "one\n" || ch.Diff != "+one\n" || ch.ETag != etag {
		t.Fatalf("change = %+v ok=%v etag=%q", ch, ok, etag)
	}
	if _, err := e.svc.Create(bg, e.sess, "notes/ideas.md", "two\n"); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("second Create = %v, want ErrAlreadyExists", err)
	}
	if got := e.content(t, "/.ocis-agent/memory/notes/ideas.md"); got != "one\n" {
		t.Fatalf("content = %q", got)
	}
}

func TestCreateValidatesBeforeAnyRequest(t *testing.T) {
	e := newEnv(t, Limits{})
	for name, c := range map[string]struct {
		name, content string
		want          error
	}{
		"name": {"Profile.md", "x", ErrInvalidName},
		"big":  {"profile.md", strings.Repeat("a", 64<<10+1), ErrTooLarge},
		"utf8": {"profile.md", "\xff", ErrNotText},
		"nul":  {"profile.md", "\x00", ErrNotText},
	} {
		if _, err := e.svc.Create(bg, e.sess, c.name, c.content); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	if n := len(e.fake.Requests()); n != 0 {
		t.Fatalf("%d oCIS requests", n)
	}
}

func TestEditsNeverTouchTheETagCacheAndTheModelMustReadAgain(t *testing.T) {
	e := ready(t, Limits{})
	if _, err := e.svc.Write(bg, e.sess, "profile.md", "v1\n", nil); err != nil { // model write caches the ETag
		t.Fatal(err)
	}
	cached := e.cachedETag(t, "profile.md")
	// the user edits through Replace using the current ETag
	_, cur, _ := e.fake.File("drive1", profile)
	if _, err := e.svc.Replace(bg, e.sess, "profile.md", "user\n", cur); err != nil {
		t.Fatal(err)
	}
	if got := e.cachedETag(t, "profile.md"); got != cached {
		t.Fatalf("Replace changed the cache: %q -> %q", cached, got)
	}
	// the model's replace is refused until it reads again
	if _, err := e.svc.Write(bg, e.sess, "profile.md", "model\n", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("model write = %v, want ErrConflict", err)
	}
	if _, err := e.svc.Read(bg, e.sess, "profile.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Write(bg, e.sess, "profile.md", "model\n", nil); err != nil {
		t.Fatalf("after read: %v", err)
	}
	// Create does not cache either
	if _, err := e.svc.Create(bg, e.sess, "notes/n.md", "x\n"); err != nil {
		t.Fatal(err)
	}
	if got := e.cachedETag(t, "notes/n.md"); got != "" {
		t.Fatalf("Create cached %q", got)
	}
	if _, err := e.svc.Write(bg, e.sess, "notes/n.md", "y\n", nil); !errors.Is(err, ErrNotRead) {
		t.Fatalf("model write of a user-created file = %v, want ErrNotRead", err)
	}
}

func TestReplaceSendsIfMatchOnThePut(t *testing.T) {
	e := ready(t, Limits{})
	e.fake.PutFile("drive1", profile, []byte("v1\n"))
	_, etag, _ := e.fake.File("drive1", profile)
	e.fake.ResetRequests()
	if _, err := e.svc.Replace(bg, e.sess, "profile.md", "v2\n", etag); err != nil {
		t.Fatal(err)
	}
	puts := 0
	for _, r := range e.fake.Requests() {
		if r.Method == "PUT" {
			puts++
			if got := r.Header.Get("If-Match"); got != etag {
				t.Errorf("PUT If-Match = %q, want %q", got, etag)
			}
		}
	}
	if puts != 1 {
		t.Fatalf("%d PUTs", puts)
	}
}

func TestReplaceOfAFileOverTheReadLimitIsErrFileTooLarge(t *testing.T) {
	e := ready(t, Limits{MaxReadBytes: 100})
	e.fake.PutFile("drive1", profile, []byte(strings.Repeat("a", 200)))
	_, etag, _ := e.fake.File("drive1", profile)
	if _, err := e.svc.Replace(bg, e.sess, "profile.md", "x", etag); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("err = %v", err)
	}
}

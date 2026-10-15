package memory

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient/fakeocis"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/store/sqlite"
)

var bg = context.Background()

type env struct {
	svc  *Service
	fake *fakeocis.Server
	st   store.Store
	sess Session
}

func addUser(t *testing.T, st store.Store, id, drive string) {
	t.Helper()
	err := st.InTx(bg, func(tx store.Tx) error {
		return tx.PutUser(store.User{ID: id, Name: id, PersonalDriveID: drive})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func newEnv(t *testing.T, lim Limits) env {
	t.Helper()
	fake := fakeocis.New(t)
	fake.AddUser("einstein", "pw", "u1", "drive1")
	client, err := ocisclient.New(fake.URL)
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	addUser(t, st, "u1", "drive1")
	return env{
		svc:  New(client, st, lim),
		fake: fake,
		st:   st,
		sess: Session{UserID: "u1", DriveID: "drive1", Creds: ocisclient.AppToken("einstein", "pw")},
	}
}

func ready(t *testing.T, lim Limits) env {
	t.Helper()
	e := newEnv(t, lim)
	if err := e.svc.Ensure(bg, e.sess); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e env) content(t *testing.T, p string) string {
	t.Helper()
	data, _, ok := e.fake.File("drive1", p)
	if !ok {
		t.Fatalf("%s does not exist", p)
	}
	return string(data)
}

const profile = "/.ocis-agent/memory/profile.md"

func TestEnsureCreatesTheFolderAndIsIdempotent(t *testing.T) {
	e := newEnv(t, Limits{})
	if err := e.svc.Ensure(bg, e.sess); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.content(t, "/.ocis-agent/README.md"), "memory/") {
		t.Fatal("README must explain the folder")
	}
	// the folders exist: a note can be created right away
	if _, err := e.svc.Write(bg, e.sess, "notes/a.md", "x\n", nil); err != nil {
		t.Fatalf("write after Ensure: %v", err)
	}
	// a README the user edited is left alone
	e.fake.PutFile("drive1", "/.ocis-agent/README.md", []byte("mine"))
	if err := e.svc.Ensure(bg, e.sess); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if got := e.content(t, "/.ocis-agent/README.md"); got != "mine" {
		t.Fatalf("README = %q, Ensure must not overwrite it", got)
	}
}

func TestWriteCreatesTheFileAndCachesTheETag(t *testing.T) {
	e := ready(t, Limits{})
	ch, err := e.svc.Write(bg, e.sess, "profile.md", "# Me\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, serverETag, _ := e.fake.File("drive1", profile)
	if !ch.Created || ch.Before != "" || ch.After != "# Me\n" || ch.Diff != "+# Me\n" || ch.ETag != serverETag || ch.Name != "profile.md" {
		t.Fatalf("change = %+v (server etag %q)", ch, serverETag)
	}
	// the cache lets the next write go through without a read
	ch2, err := e.svc.Write(bg, e.sess, "profile.md", "# Me\nlikes tea\n", nil)
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if ch2.Created || ch2.Before != "# Me\n" || ch2.Diff != "+likes tea\n" {
		t.Fatalf("change 2 = %+v", ch2)
	}
	var cached string
	if err := e.st.View(bg, func(tx store.Tx) error {
		c, err := tx.GetCursor("u1")
		cached = c.MemoryETags["profile.md"]
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, etag, _ := e.fake.File("drive1", profile); cached != etag || cached != ch2.ETag {
		t.Fatalf("cached %q, server %q, change %q", cached, etag, ch2.ETag)
	}
}

func TestWritingAnExistingFileNeedsARead(t *testing.T) {
	e := ready(t, Limits{})
	e.fake.PutFile("drive1", profile, []byte("user wrote this\n"))
	if _, err := e.svc.Write(bg, e.sess, "profile.md", "overwrite\n", nil); !errors.Is(err, ErrNotRead) {
		t.Fatalf("err = %v, want ErrNotRead", err)
	}
	if got := e.content(t, profile); got != "user wrote this\n" {
		t.Fatalf("content = %q: nothing may be overwritten blind", got)
	}
	f, err := e.svc.Read(bg, e.sess, "profile.md")
	if err != nil || f.Content != "user wrote this\n" || f.Name != "profile.md" || f.ETag == "" {
		t.Fatalf("read = %+v %v", f, err)
	}
	ch, err := e.svc.Write(bg, e.sess, "profile.md", "merged\n", nil)
	if err != nil || ch.Before != "user wrote this\n" {
		t.Fatalf("write after read = %+v %v", ch, err)
	}
}

func TestAnEditBetweenReadAndWriteIsAConflict(t *testing.T) {
	e := ready(t, Limits{})
	if _, err := e.svc.Write(bg, e.sess, "profile.md", "v1\n", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Read(bg, e.sess, "profile.md"); err != nil {
		t.Fatal(err)
	}
	e.fake.PutFile("drive1", profile, []byte("v1\nuser edit\n")) // the user edits in the web UI
	if _, err := e.svc.Write(bg, e.sess, "profile.md", "agent version\n", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if got := e.content(t, profile); got != "v1\nuser edit\n" {
		t.Fatalf("content = %q: the user's edit must survive", got)
	}
	// still a conflict until the file is read again
	if _, err := e.svc.Write(bg, e.sess, "profile.md", "again\n", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("second attempt = %v", err)
	}
	if _, err := e.svc.Read(bg, e.sess, "profile.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Write(bg, e.sess, "profile.md", "v1\nuser edit\nagent line\n", nil); err != nil {
		t.Fatalf("write after re-read: %v", err)
	}
}

func TestAnEditInTheLastMomentIsAConflictToo(t *testing.T) {
	e := ready(t, Limits{})
	if _, err := e.svc.Write(bg, e.sess, "profile.md", "v1\n", nil); err != nil {
		t.Fatal(err)
	}
	// oCIS rejects the conditional upload although the pre-check passed
	e.fake.Fail("PUT", "/remote.php/", 409, 1)
	if _, err := e.svc.Write(bg, e.sess, "profile.md", "v2\n", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

func TestWritingAfterTheUserDeletedTheFileIsAConflict(t *testing.T) {
	e := ready(t, Limits{})
	if _, err := e.svc.Write(bg, e.sess, "profile.md", "v1\n", nil); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.files.Delete(bg, e.sess.Creds, "drive1", profile); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Write(bg, e.sess, "profile.md", "v2\n", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

func TestAppendKeepsTheUsersEdit(t *testing.T) {
	e := ready(t, Limits{})
	if _, err := e.svc.Write(bg, e.sess, "profile.md", "base\n", nil); err != nil {
		t.Fatal(err)
	}
	e.fake.PutFile("drive1", profile, []byte("base\nuser line\n"))
	ch, err := e.svc.Append(bg, e.sess, "profile.md", "## New\nnote  \n\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "base\nuser line\n\n## New\nnote\n"
	if got := e.content(t, profile); got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
	if ch.Created || ch.Diff != "+\n+## New\n+note\n" || ch.After != want {
		t.Fatalf("change = %+v", ch)
	}
	// the new ETag is cached, so a Write straight after works
	if _, err := e.svc.Write(bg, e.sess, "profile.md", want+"more\n", nil); err != nil {
		t.Fatalf("write after append: %v", err)
	}
}

func TestAppendCreatesAMissingFileAndFixesAMissingNewline(t *testing.T) {
	e := ready(t, Limits{})
	ch, err := e.svc.Append(bg, e.sess, "notes/trip.md", "## Plan\nx", nil)
	if err != nil || !ch.Created {
		t.Fatalf("append to a new file = %+v %v", ch, err)
	}
	if got := e.content(t, "/.ocis-agent/memory/notes/trip.md"); got != "## Plan\nx\n" {
		t.Fatalf("content = %q", got)
	}
	e.fake.PutFile("drive1", "/.ocis-agent/memory/notes/raw.md", []byte("no newline at the end"))
	if _, err := e.svc.Append(bg, e.sess, "notes/raw.md", "added", nil); err != nil {
		t.Fatal(err)
	}
	if got := e.content(t, "/.ocis-agent/memory/notes/raw.md"); got != "no newline at the end\n\nadded\n" {
		t.Fatalf("content = %q", got)
	}
}

func TestAppendRetriesWhenTheFileChangesUnderIt(t *testing.T) {
	e := ready(t, Limits{})
	e.fake.PutFile("drive1", profile, []byte("base\n"))
	e.fake.Fail("PUT", "/remote.php/", 409, 2)
	b := NewBudget(1000)
	if _, err := e.svc.Append(bg, e.sess, "profile.md", "line", b); err != nil {
		t.Fatalf("append with two lost races: %v", err)
	}
	if got := e.content(t, profile); got != "base\n\nline\n" {
		t.Fatalf("content = %q: the section must be added exactly once", got)
	}
	if b.Used() != int64(len("line\n")) {
		t.Fatalf("budget used = %d", b.Used())
	}
}

func TestAppendGivesUpAfterThreeLostRacesAndReleasesTheBudget(t *testing.T) {
	e := ready(t, Limits{})
	e.fake.PutFile("drive1", profile, []byte("base\n"))
	e.fake.Fail("PUT", "/remote.php/", 409, 3)
	b := NewBudget(1000)
	if _, err := e.svc.Append(bg, e.sess, "profile.md", "line", b); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if got := e.content(t, profile); got != "base\n" || b.Used() != 0 {
		t.Fatalf("content %q, budget used %d: a failed append changes nothing", got, b.Used())
	}
}

func TestSizeLimits(t *testing.T) {
	e := ready(t, Limits{MaxFileBytes: 10})
	if _, err := e.svc.Write(bg, e.sess, "profile.md", strings.Repeat("a", 11), nil); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("11 bytes = %v, want ErrTooLarge", err)
	}
	if _, err := e.svc.Write(bg, e.sess, "profile.md", strings.Repeat("a", 10), nil); err != nil {
		t.Fatalf("exactly the limit: %v", err)
	}
	if _, err := e.svc.Append(bg, e.sess, "profile.md", "b", nil); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("append past the limit = %v, want ErrTooLarge", err)
	}
	if got := e.content(t, profile); got != strings.Repeat("a", 10) {
		t.Fatalf("content = %q", got)
	}
}

func TestJobBudget(t *testing.T) {
	e := ready(t, Limits{})
	b := NewBudget(20)
	if _, err := e.svc.Write(bg, e.sess, "profile.md", strings.Repeat("a", 12), b); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Write(bg, e.sess, "projects.md", strings.Repeat("b", 12), b); !errors.Is(err, ErrBudget) {
		t.Fatalf("second write = %v, want ErrBudget", err)
	}
	if _, _, ok := e.fake.File("drive1", "/.ocis-agent/memory/projects.md"); ok {
		t.Fatal("a refused write must not reach oCIS")
	}
	if b.Used() != 12 {
		t.Fatalf("used = %d, a refused write consumes nothing", b.Used())
	}
	// a write that fails after the reservation gives it back
	e.fake.Fail("PUT", "/remote.php/", 503, 1)
	if _, err := e.svc.Write(bg, e.sess, "projects.md", "small", b); err == nil {
		t.Fatal("expected the injected failure")
	}
	if b.Used() != 12 {
		t.Fatalf("used = %d after a failed upload, want 12", b.Used())
	}
}

func TestInvalidNamesNeverReachOCIS(t *testing.T) {
	e := newEnv(t, Limits{})
	for _, name := range []string{"", "../x.md", "profile", "PROFILE.md", "notes/", "notes/a/b.md", "notes/Up.md", "/profile.md", "other.md"} {
		if _, err := e.svc.Read(bg, e.sess, name); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Read(%q) = %v", name, err)
		}
		if _, err := e.svc.Write(bg, e.sess, name, "x", nil); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Write(%q) = %v", name, err)
		}
		if _, err := e.svc.Append(bg, e.sess, name, "x", nil); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Append(%q) = %v", name, err)
		}
	}
	if n := len(e.fake.Requests()); n != 0 {
		t.Fatalf("%d request(s) for invalid names", n)
	}
}

func TestOnlyTextIsRead(t *testing.T) {
	e := ready(t, Limits{MaxReadBytes: 8})
	e.fake.PutFile("drive1", profile, []byte{0xff, 0xfe, 'a'})
	if _, err := e.svc.Read(bg, e.sess, "profile.md"); !errors.Is(err, ErrNotText) {
		t.Fatalf("invalid UTF-8 = %v, want ErrNotText", err)
	}
	e.fake.PutFile("drive1", profile, []byte("a\x00b"))
	if _, err := e.svc.Read(bg, e.sess, "profile.md"); !errors.Is(err, ErrNotText) {
		t.Fatalf("NUL byte = %v, want ErrNotText", err)
	}
	e.fake.PutFile("drive1", profile, []byte("123456789"))
	if _, err := e.svc.Read(bg, e.sess, "profile.md"); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over the read limit = %v, want ErrTooLarge", err)
	}
	if _, err := e.svc.Read(bg, e.sess, "projects.md"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing = %v, want ErrNotFound", err)
	}
}

func TestOnlyTextIsWritten(t *testing.T) {
	e := ready(t, Limits{})
	for _, bad := range []string{"a\x00b", "\xff\xfe"} {
		if _, err := e.svc.Write(bg, e.sess, "profile.md", bad, nil); !errors.Is(err, ErrNotText) {
			t.Errorf("Write(%q) = %v", bad, err)
		}
		if _, err := e.svc.Append(bg, e.sess, "profile.md", bad, nil); !errors.Is(err, ErrNotText) {
			t.Errorf("Append(%q) = %v", bad, err)
		}
	}
	if _, err := e.svc.Append(bg, e.sess, "profile.md", " \n\t\n", nil); !errors.Is(err, ErrEmpty) {
		t.Errorf("an empty section = %v, want ErrEmpty", err)
	}
}

func TestListReturnsOnlyValidMemoryFiles(t *testing.T) {
	e := newEnv(t, Limits{})
	if got, err := e.svc.List(bg, e.sess); err != nil || len(got) != 0 {
		t.Fatalf("list before Ensure = %+v %v", got, err)
	}
	if err := e.svc.Ensure(bg, e.sess); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"profile.md", "notes/b.md", "notes/a.md"} {
		if _, err := e.svc.Write(bg, e.sess, n, "x\n", nil); err != nil {
			t.Fatal(err)
		}
	}
	e.fake.PutFile("drive1", "/.ocis-agent/memory/junk.txt", []byte("j"))
	e.fake.PutFile("drive1", "/.ocis-agent/memory/notes/Bad Name.md", []byte("j"))
	e.fake.PutFile("drive1", "/.ocis-agent/memory/notes/sub/deep.md", []byte("j"))
	got, err := e.svc.List(bg, e.sess)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, i := range got {
		names = append(names, i.Name)
		if i.Size != 2 || i.ETag == "" || i.ModTime.IsZero() {
			t.Errorf("incomplete info %+v", i)
		}
	}
	if strings.Join(names, ",") != "notes/a.md,notes/b.md,profile.md" {
		t.Fatalf("names = %v", names)
	}
}

func TestETagCacheIsPerUserAndKeepsOtherCursorFields(t *testing.T) {
	e := ready(t, Limits{})
	if err := e.st.InTx(bg, func(tx store.Tx) error {
		c, err := tx.GetCursor("u1")
		if err != nil {
			return err
		}
		c.SSELastEventID = "9"
		return tx.PutCursor(c)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Write(bg, e.sess, "profile.md", "u1 data\n", nil); err != nil {
		t.Fatal(err)
	}
	var c store.Cursor
	if err := e.st.View(bg, func(tx store.Tx) error {
		var err error
		c, err = tx.GetCursor("u1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if c.SSELastEventID != "9" || c.MemoryETags["profile.md"] == "" {
		t.Fatalf("cursor = %+v: the other fields must survive", c)
	}

	// a second user does not inherit u1's cached ETags
	e.fake.AddUser("marie", "pw2", "u2", "drive2")
	addUser(t, e.st, "u2", "drive2")
	s2 := Session{UserID: "u2", DriveID: "drive2", Creds: ocisclient.AppToken("marie", "pw2")}
	if err := e.svc.Ensure(bg, s2); err != nil {
		t.Fatal(err)
	}
	e.fake.PutFile("drive2", profile, []byte("marie's file\n"))
	if _, err := e.svc.Write(bg, s2, "profile.md", "blind overwrite\n", nil); !errors.Is(err, ErrNotRead) {
		t.Fatalf("err = %v, want ErrNotRead: u2 has no cached ETag of its own", err)
	}
}

func TestCredentialFailuresPassThrough(t *testing.T) {
	e := newEnv(t, Limits{})
	bad := e.sess
	bad.Creds = ocisclient.AppToken("einstein", "wrong")
	if err := e.svc.Ensure(bg, bad); !errors.Is(err, ocisclient.ErrUnauthorized) {
		t.Fatalf("Ensure = %v", err)
	}
	if _, err := e.svc.Read(bg, bad, "profile.md"); !errors.Is(err, ocisclient.ErrUnauthorized) {
		t.Fatalf("Read = %v", err)
	}
	if _, err := e.svc.List(bg, bad); !errors.Is(err, ocisclient.ErrUnauthorized) {
		t.Fatalf("List = %v", err)
	}
}

package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// Errors. ErrNotFound and ErrTooLarge are the ocisclient sentinels, so
// errors.Is works with either name.
var (
	ErrNotFound = ocisclient.ErrNotFound
	ErrTooLarge = ocisclient.ErrTooLarge
	// ErrNotRead: the file exists but this user has no cached ETag for it.
	// Read it first, so a write cannot overwrite content nobody has seen.
	ErrNotRead = errors.New("memory: read the file before writing it")
	// ErrConflict: the file changed (or was deleted) since it was read. Read it
	// again, or use Append.
	ErrConflict = errors.New("memory: the file changed since it was read")
	ErrNotText  = errors.New("memory: not a UTF-8 text file")
	ErrEmpty    = errors.New("memory: nothing to append")
)

const appendAttempts = 3

const readmeText = "# ocis-agent\n\n" +
	"This folder belongs to your agent. `memory/` holds Markdown notes the agent keeps about you and your projects.\n" +
	"You can read and edit them. The agent treats them as untrusted notes, never as instructions or settings.\n" +
	"Jobs, approvals and the audit log are stored by the agent service, not here.\n"

// Session identifies the user and the credentials to use for one operation.
// DriveID must be the personal drive of UserID: the ETag cache is keyed by
// user and file name only.
type Session struct {
	UserID  string
	DriveID string
	Creds   ocisclient.Creds
}

// File is a memory file as read.
type File struct {
	Name    string
	Content string
	ETag    string
}

// Info describes a memory file in a listing.
type Info struct {
	Name    string
	Size    int64
	ETag    string
	ModTime time.Time
}

// Change describes a completed write, for the activity feed. Diff is the line
// diff from Before to After.
type Change struct {
	Name    string
	Created bool
	Before  string
	After   string
	Diff    string
	ETag    string
}

// Service reads and writes memory files.
type Service struct {
	files *ocisclient.Client
	st    store.Store
	lim   Limits
}

// New returns a memory service. Zero fields of lim take the defaults.
func New(files *ocisclient.Client, st store.Store, lim Limits) *Service {
	return &Service{files: files, st: st, lim: lim.withDefaults()}
}

func validText(b []byte) bool { return utf8.Valid(b) && !bytes.ContainsRune(b, 0) }

// Ensure creates /.ocis-agent/, memory/ and memory/notes/ and a README.md if
// they are missing. A README the user changed is left alone.
func (s *Service) Ensure(ctx context.Context, sess Session) error {
	for _, dir := range []string{Root, memoryDir, notesDir} {
		err := s.files.Mkcol(ctx, sess.Creds, sess.DriveID, dir)
		if err != nil && !errors.Is(err, ocisclient.ErrExists) {
			return fmt.Errorf("memory: create %s: %w", dir, err)
		}
	}
	_, err := s.files.Stat(ctx, sess.Creds, sess.DriveID, readmePath)
	switch {
	case err == nil:
		return nil
	case !errors.Is(err, ocisclient.ErrNotFound):
		return fmt.Errorf("memory: check README: %w", err)
	}
	if _, err := s.files.Put(ctx, sess.Creds, sess.DriveID, readmePath, []byte(readmeText), ""); err != nil {
		return fmt.Errorf("memory: create README: %w", err)
	}
	return nil
}

// List returns the memory files that exist, sorted by name. Files with names
// ResolveName would refuse are not listed.
func (s *Service) List(ctx context.Context, sess Session) ([]Info, error) {
	var out []Info
	add := func(dir, prefix string) error {
		entries, err := s.files.List(ctx, sess.Creds, sess.DriveID, dir)
		if errors.Is(err, ocisclient.ErrNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("memory: list %s: %w", dir, err)
		}
		for _, e := range entries {
			if e.Dir {
				continue
			}
			name := prefix + e.Path[strings.LastIndex(e.Path, "/")+1:]
			if _, err := ResolveName(name); err != nil {
				continue
			}
			out = append(out, Info{Name: name, Size: e.Size, ETag: e.ETag, ModTime: e.ModTime})
		}
		return nil
	}
	if err := add(memoryDir, ""); err != nil {
		return nil, err
	}
	if err := add(notesDir, "notes/"); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Read returns a memory file and remembers its ETag for later writes. The
// content is untrusted data: callers fence it in prompts and never act on it
// as policy.
func (s *Service) Read(ctx context.Context, sess Session, name string) (File, error) {
	p, err := ResolveName(name)
	if err != nil {
		return File{}, err
	}
	data, etag, err := s.files.Get(ctx, sess.Creds, sess.DriveID, p, s.lim.MaxReadBytes)
	if errors.Is(err, ocisclient.ErrNotFound) {
		// the file is gone: forget its ETag so Write can recreate it
		if cerr := s.dropETag(ctx, sess.UserID, name); cerr != nil {
			return File{}, fmt.Errorf("memory: read %s: %w (and %v)", name, err, cerr)
		}
		return File{}, fmt.Errorf("memory: read %s: %w", name, err)
	}
	if err != nil {
		return File{}, fmt.Errorf("memory: read %s: %w", name, err)
	}
	if !validText(data) {
		return File{}, ErrNotText
	}
	if err := s.cacheETag(ctx, sess.UserID, name, etag); err != nil {
		return File{}, err
	}
	return File{Name: name, Content: string(data), ETag: etag}, nil
}

// Write replaces (or creates) a memory file. An existing file must have been
// read first (ErrNotRead) and must not have changed since (ErrConflict). b is
// the job's write budget; nil means no job cap.
//
// Creating a file stats first and then uploads without a precondition: oCIS
// ignores If-None-Match, so a file created by someone else in that instant is
// overwritten (an accepted residual race).
func (s *Service) Write(ctx context.Context, sess Session, name, content string, b *Budget) (Change, error) {
	p, err := ResolveName(name)
	if err != nil {
		return Change{}, err
	}
	if err := s.checkContent(content); err != nil {
		return Change{}, err
	}
	cached, err := s.cachedETag(ctx, sess.UserID, name)
	if err != nil {
		return Change{}, err
	}

	var before, ifMatch string
	created := false
	if cached == "" {
		switch _, err := s.files.Stat(ctx, sess.Creds, sess.DriveID, p); {
		case err == nil:
			return Change{}, ErrNotRead
		case errors.Is(err, ocisclient.ErrNotFound):
			created = true
		default:
			return Change{}, fmt.Errorf("memory: check %s: %w", name, err)
		}
	} else {
		// An existing file larger than the read limit surfaces ErrTooLarge
		// from the existing content here.
		data, etag, err := s.files.Get(ctx, sess.Creds, sess.DriveID, p, s.lim.MaxReadBytes)
		switch {
		case errors.Is(err, ocisclient.ErrNotFound):
			// deleted since it was read: drop the stale ETag so the next
			// Write takes the create path
			if cerr := s.dropETag(ctx, sess.UserID, name); cerr != nil {
				return Change{}, fmt.Errorf("%w (and %v)", ErrConflict, cerr)
			}
			return Change{}, ErrConflict
		case err != nil:
			return Change{}, fmt.Errorf("memory: read %s: %w", name, err)
		case etag != cached:
			return Change{}, ErrConflict
		}
		before, ifMatch = string(data), cached
	}

	n := int64(len(content))
	if err := b.Reserve(n); err != nil {
		return Change{}, err
	}
	etag, err := s.files.Put(ctx, sess.Creds, sess.DriveID, p, []byte(content), ifMatch)
	if err != nil {
		b.Release(n)
		if errors.Is(err, ocisclient.ErrPrecondition) {
			return Change{}, ErrConflict
		}
		return Change{}, fmt.Errorf("memory: write %s: %w", name, err)
	}
	ch := Change{Name: name, Created: created, Before: before, After: content, Diff: Diff(before, content), ETag: etag}
	return ch, s.cacheETag(ctx, sess.UserID, name, etag)
}

// Append adds a section to the end of a memory file (creating it if needed),
// separated by a blank line. It always works on the current content, so it is
// the safe way to add to a file the user may be editing: if the file changes
// during the call, it reads again and retries, up to three times (ErrConflict).
// A missing file (or one the user deleted) is created without a precondition,
// so a file created at the same instant is overwritten (accepted race), and a
// deleted file is silently recreated.
func (s *Service) Append(ctx context.Context, sess Session, name, section string, b *Budget) (Change, error) {
	p, err := ResolveName(name)
	if err != nil {
		return Change{}, err
	}
	if !validText([]byte(section)) {
		return Change{}, ErrNotText
	}
	section = strings.TrimRight(section, " \t\r\n")
	if strings.TrimSpace(section) == "" {
		return Change{}, ErrEmpty
	}
	add := section + "\n"
	n := int64(len(add))
	if err := b.Reserve(n); err != nil {
		return Change{}, err
	}
	done := false
	defer func() {
		if !done {
			b.Release(n)
		}
	}()

	for attempt := 0; attempt < appendAttempts; attempt++ {
		var before, ifMatch string
		created := false
		data, etag, err := s.files.Get(ctx, sess.Creds, sess.DriveID, p, s.lim.MaxReadBytes)
		switch {
		case errors.Is(err, ocisclient.ErrNotFound):
			created = true
		case err != nil:
			return Change{}, fmt.Errorf("memory: read %s: %w", name, err)
		case !validText(data):
			return Change{}, ErrNotText
		default:
			before, ifMatch = string(data), etag
		}
		after := joinSection(before, add)
		if int64(len(after)) > s.lim.MaxFileBytes {
			return Change{}, ErrTooLarge
		}
		newETag, err := s.files.Put(ctx, sess.Creds, sess.DriveID, p, []byte(after), ifMatch)
		if errors.Is(err, ocisclient.ErrPrecondition) {
			continue // changed since we read it: read again
		}
		if err != nil {
			return Change{}, fmt.Errorf("memory: write %s: %w", name, err)
		}
		done = true
		ch := Change{Name: name, Created: created, Before: before, After: after, Diff: Diff(before, after), ETag: newETag}
		return ch, s.cacheETag(ctx, sess.UserID, name, newETag)
	}
	return Change{}, ErrConflict
}

func joinSection(before, add string) string {
	if before == "" {
		return add
	}
	if !strings.HasSuffix(before, "\n") {
		before += "\n"
	}
	return before + "\n" + add
}

func (s *Service) checkContent(content string) error {
	if int64(len(content)) > s.lim.MaxFileBytes {
		return ErrTooLarge
	}
	if !validText([]byte(content)) {
		return ErrNotText
	}
	return nil
}

func (s *Service) cachedETag(ctx context.Context, userID, name string) (string, error) {
	var etag string
	err := s.st.View(ctx, func(tx store.Tx) error {
		c, err := tx.GetCursor(userID)
		if err != nil {
			return err
		}
		etag = c.MemoryETags[name]
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("memory: load cached ETag: %w", err)
	}
	return etag, nil
}

// cacheETag records the ETag of name. It reads and writes the cursor in one
// transaction, because PutCursor replaces the whole row.
func (s *Service) cacheETag(ctx context.Context, userID, name, etag string) error {
	err := s.st.InTx(ctx, func(tx store.Tx) error {
		c, err := tx.GetCursor(userID)
		if err != nil {
			return err
		}
		m := make(map[string]string, len(c.MemoryETags)+1)
		for k, v := range c.MemoryETags {
			m[k] = v
		}
		m[name] = etag
		c.MemoryETags = m
		return tx.PutCursor(c)
	})
	if err != nil {
		return fmt.Errorf("memory: cache ETag of %s: %w", name, err)
	}
	return nil
}

// dropETag forgets the cached ETag of name. A no-op if there is none.
func (s *Service) dropETag(ctx context.Context, userID, name string) error {
	err := s.st.InTx(ctx, func(tx store.Tx) error {
		c, err := tx.GetCursor(userID)
		if err != nil {
			return err
		}
		if _, ok := c.MemoryETags[name]; !ok {
			return nil
		}
		m := make(map[string]string, len(c.MemoryETags))
		for k, v := range c.MemoryETags {
			if k != name {
				m[k] = v
			}
		}
		c.MemoryETags = m
		return tx.PutCursor(c)
	})
	if err != nil {
		return fmt.Errorf("memory: drop cached ETag of %s: %w", name, err)
	}
	return nil
}

package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
)

// ErrInvalid is returned for a request the caller must correct (for example
// an empty or wildcard If-Match).
var ErrInvalid = errors.New("memory: invalid request")

// ErrFileTooLarge: the existing file is larger than the read limit, so Replace
// cannot show what it overwrites. It also matches ErrTooLarge; check it
// first to tell it from content that is too large.
var ErrFileTooLarge = errors.New("memory: the existing file is too large to read")

// ErrAlreadyExists: Create found the file.
var ErrAlreadyExists = errors.New("memory: the file already exists")

// Replace overwrites an existing memory file with content, on behalf of the
// user (the UI): the write carries If-Match: ifMatch, so it only succeeds
// while the file still has the version the user saw (ErrConflict otherwise).
// A missing file is ErrNotFound, an empty or "*" ifMatch is ErrInvalid.
//
// Replace never reads, stores or drops the per-user ETag cache: after a user
// edit the model's later Write sees an ETag mismatch (ErrConflict) and must
// Read again, so it cannot overwrite the user's edit unseen. Name and content
// are validated before any request.
func (s *Service) Replace(ctx context.Context, sess Session, name, content, ifMatch string) (Change, error) {
	p, err := ResolveName(name)
	if err != nil {
		return Change{}, err
	}
	if err := s.checkContent(content); err != nil {
		return Change{}, err
	}
	if strings.TrimSpace(ifMatch) == "" || strings.TrimSpace(ifMatch) == "*" {
		return Change{}, ErrInvalid
	}
	data, etag, err := s.files.Get(ctx, sess.Creds, sess.DriveID, p, s.lim.MaxReadBytes)
	if errors.Is(err, ocisclient.ErrTooLarge) {
		return Change{}, fmt.Errorf("memory: read %s: %w: %w", name, ErrFileTooLarge, err)
	}
	if err != nil {
		return Change{}, fmt.Errorf("memory: read %s: %w", name, err)
	}
	if etag != ifMatch {
		return Change{}, ErrConflict
	}
	before := ""
	if validText(data) {
		before = string(data)
	}
	newETag, err := s.files.Put(ctx, sess.Creds, sess.DriveID, p, []byte(content), ifMatch)
	if err != nil {
		if errors.Is(err, ocisclient.ErrPrecondition) {
			return Change{}, ErrConflict
		}
		return Change{}, fmt.Errorf("memory: write %s: %w", name, err)
	}
	return Change{Name: name, Before: before, After: content, Diff: Diff(before, content), ETag: newETag}, nil
}

// Create makes a new memory file on behalf of the user (the UI), creating the
// folders if needed. An existing file is ErrAlreadyExists. Like Replace it
// leaves the ETag cache alone, so the model must Read the file before it
// replaces it. The existence check is a stat before the upload (oCIS ignores
// If-None-Match): a file created by someone else in that instant is
// overwritten (accepted residual race, as in Write).
func (s *Service) Create(ctx context.Context, sess Session, name, content string) (Change, error) {
	p, err := ResolveName(name)
	if err != nil {
		return Change{}, err
	}
	if err := s.checkContent(content); err != nil {
		return Change{}, err
	}
	switch _, err := s.files.Stat(ctx, sess.Creds, sess.DriveID, p); {
	case err == nil:
		return Change{}, ErrAlreadyExists
	case !errors.Is(err, ocisclient.ErrNotFound):
		return Change{}, fmt.Errorf("memory: check %s: %w", name, err)
	}
	if err := s.Ensure(ctx, sess); err != nil {
		return Change{}, err
	}
	etag, err := s.files.Put(ctx, sess.Creds, sess.DriveID, p, []byte(content), "")
	if err != nil {
		return Change{}, fmt.Errorf("memory: write %s: %w", name, err)
	}
	return Change{Name: name, Created: true, After: content, Diff: Diff("", content), ETag: etag}, nil
}

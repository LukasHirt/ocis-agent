package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/memory"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/textcap"
)

// The memory routes show and edit the user's files under /.ocis-agent/memory/
// with the user's own bearer (never the app token), in the personal drive of
// the validated identity: no name, header or body field can address another
// drive. Content is untrusted data; render it as plain text. The API only
// lists, peeks and edits with an explicit ETag (memory.Service.Peek, Replace,
// Create); it never reads with the caching Read, never writes with Write and
// never calls Forget, so a user looking at or editing a file neither unlocks
// nor revokes the model's right to replace it.

const (
	// memoryMaxBody: a valid 64 KiB file of newlines, quotes or backslashes
	// escapes to 2x (~130 KiB). A file dense with control characters escapes
	// to \u00XX (6x) and may get 413 although it is valid: the body is read
	// and parsed generically (~24x its size in heap per request), so the
	// limit stays small. The content limit (64 KiB) is checked by the
	// service after the body is decoded.
	memoryMaxBody = 200 << 10
	// maxDiffResponse caps the diff in the response of an edit.
	maxDiffResponse = 16 << 10
	// maxDiffAudit caps the diff bytes in the audit detail; the audit log
	// holds names and a short diff, never the file.
	maxDiffAudit = 1500
	maxETagLen   = 256
	auditTimeout = 5 * time.Second
)

type memoryFileView struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	ETag     string `json:"etag"`
	Modified string `json:"modified"`
}

type memoryListView struct {
	Files []memoryFileView `json:"files"`
}

// memoryFileDoc is the body of GET /memory/{name}. Content is untrusted.
//
// A file larger than the edit limit (MaxFileBytes) is shown cut at that
// size on a rune boundary with truncated=true and editable=false: saving the
// cut text would destroy the rest.
type memoryFileDoc struct {
	Name      string `json:"name"`
	Content   string `json:"content"`
	ETag      string `json:"etag"`
	Truncated bool   `json:"truncated"`
	Editable  bool   `json:"editable"`
}

type memoryEditView struct {
	Name string `json:"name"`
	ETag string `json:"etag"`
	Diff string `json:"diff"` // untrusted, capped at 16 KiB
}

type memoryPutBody struct {
	Content *string `json:"content"`
}

type memoryCreateBody struct {
	Name    string  `json:"name"`
	Content *string `json:"content"`
}

// memoryCall returns the memory service and the session of a request: the
// drive comes from the validated identity, the credentials are the request's
// own bearer, used only for this call.
func (s *Server) memoryCall(r *http.Request) (MemoryService, Principal, memory.Session, error) {
	if s.d.Memory == nil {
		return nil, Principal{}, memory.Session{}, newAPIError(http.StatusServiceUnavailable, "unavailable")
	}
	p, bearer, err := caller(r)
	if err != nil {
		return nil, Principal{}, memory.Session{}, err
	}
	return s.d.Memory, p, memory.Session{UserID: p.UserID, DriveID: p.DriveID, Creds: ocisclient.Bearer(bearer)}, nil
}

// memoryPathName is the file name of a route: {name} (also an escaped
// notes%2Fa.md) or notes/{slug}.
func memoryPathName(r *http.Request) string {
	if slug := r.PathValue("slug"); slug != "" {
		return "notes/" + slug
	}
	return r.PathValue("name")
}

// memoryName validates a name from the URL or the body before any call to
// oCIS: wildcards and escaped separators arrive unescaped in PathValue.
func memoryName(name string) error {
	if _, err := memory.ResolveName(name); err != nil {
		return errBadJSON
	}
	return nil
}

// memoryError maps an error of the memory service. write is true for edits,
// where a too large file is the client's content (413); for a read it is a
// file we cannot show (the generic upstream mapping).
func memoryError(err error, write bool) error {
	switch {
	case errors.Is(err, memory.ErrInvalidName), errors.Is(err, memory.ErrInvalid):
		return errBadJSON
	case errors.Is(err, memory.ErrNotText):
		if write {
			return errBadJSON
		}
		return errNotText
	case errors.Is(err, memory.ErrConflict):
		return errPreconditionFailed
	case errors.Is(err, memory.ErrAlreadyExists):
		return errAlreadyExists
	case errors.Is(err, memory.ErrNotFound):
		return errNotFound
	case errors.Is(err, memory.ErrFileTooLarge):
		return errFileTooLarge
	case write && errors.Is(err, memory.ErrTooLarge):
		return errTooLarge
	case !write && errors.Is(err, memory.ErrTooLarge):
		return errFileTooLarge
	}
	return err
}

// listMemory is GET /agent-api/v1/memory: the memory files, sorted by name;
// an empty list when the folder does not exist.
func (s *Server) listMemory(w http.ResponseWriter, r *http.Request) error {
	svc, _, sess, err := s.memoryCall(r)
	if err != nil {
		return err
	}
	infos, err := svc.List(r.Context(), sess)
	if err != nil {
		return memoryError(err, false)
	}
	v := memoryListView{Files: make([]memoryFileView, 0, len(infos))}
	for _, i := range infos {
		v.Files = append(v.Files, memoryFileView{
			Name: i.Name, Size: i.Size, ETag: i.ETag, Modified: i.ModTime.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, v)
	return nil
}

// getMemory is GET /agent-api/v1/memory/{name}: the content (untrusted) and
// its ETag, also as the ETag header. It uses Peek, which caches nothing.
func (s *Server) getMemory(w http.ResponseWriter, r *http.Request) error {
	name := memoryPathName(r)
	if err := memoryName(name); err != nil {
		return err
	}
	svc, _, sess, err := s.memoryCall(r)
	if err != nil {
		return err
	}
	f, err := svc.Peek(r.Context(), sess, name)
	if err != nil {
		return memoryError(err, false)
	}
	w.Header().Set("ETag", f.ETag)
	doc := memoryFileDoc{Name: f.Name, Content: f.Content, ETag: f.ETag, Editable: true}
	if max := svc.Limits().MaxFileBytes; int64(len(f.Content)) > max {
		doc.Content, doc.Truncated, doc.Editable = textcap.Cut(f.Content, int(max)), true, false
	}
	writeJSON(w, http.StatusOK, doc)
	return nil
}

// ifMatch returns the request's If-Match: required (428), exactly one ETag
// (no list, no "*": a wildcard would accept any version).
func ifMatch(r *http.Request) (string, error) {
	vals := r.Header.Values("If-Match")
	if len(vals) == 0 {
		return "", errPreconditionRequired
	}
	v := strings.TrimSpace(vals[0])
	if len(vals) != 1 || v == "" || v == "*" || strings.Contains(v, ",") || len(v) > maxETagLen {
		return "", errBadJSON
	}
	return v, nil
}

// putMemory is PUT /agent-api/v1/memory/{name} with {"content": "..."} and
// If-Match: the ETag the client loaded. 412 when the file changed since, 404
// when it does not exist.
func (s *Server) putMemory(w http.ResponseWriter, r *http.Request) error {
	name := memoryPathName(r)
	if err := memoryName(name); err != nil {
		return err
	}
	match, err := ifMatch(r)
	if err != nil {
		return err
	}
	var body memoryPutBody
	if err := decodeBody(w, r, &body); err != nil {
		return err
	}
	if body.Content == nil {
		return errBadJSON
	}
	svc, p, sess, err := s.memoryCall(r)
	if err != nil {
		return err
	}
	ch, err := svc.Replace(r.Context(), sess, name, *body.Content, match)
	if err != nil {
		return memoryError(err, true)
	}
	s.auditMemoryEdit(r, p, ch, "replaced")
	writeJSON(w, http.StatusOK, editView(ch))
	return nil
}

// createMemory is POST /agent-api/v1/memory with {"name", "content"}: 201, or
// 409 already_exists.
func (s *Server) createMemory(w http.ResponseWriter, r *http.Request) error {
	var body memoryCreateBody
	if err := decodeBody(w, r, &body); err != nil {
		return err
	}
	if body.Content == nil {
		return errBadJSON
	}
	if err := memoryName(body.Name); err != nil {
		return err
	}
	svc, p, sess, err := s.memoryCall(r)
	if err != nil {
		return err
	}
	ch, err := svc.Create(r.Context(), sess, body.Name, *body.Content)
	if err != nil {
		return memoryError(err, true)
	}
	s.auditMemoryEdit(r, p, ch, "created")
	writeJSON(w, http.StatusCreated, editView(ch))
	return nil
}

func editView(ch memory.Change) memoryEditView {
	return memoryEditView{Name: ch.Name, ETag: ch.ETag, Diff: textcap.Cut(ch.Diff, maxDiffResponse)}
}

// auditMemoryEdit records memory_edited after the write to oCIS succeeded. It
// is best effort in its own transaction, on a context that outlives the
// request (with a short timeout): the file is already changed, so a failing
// append is logged by category and the response stays 200/201 (an error
// response would make the client retry an edit that happened). Known limit:
// a PUT whose outcome is uncertain (timeout or lost response from oCIS)
// returns an error and is not audited, although the file may have changed.
// The detail holds the name and the first 1500 bytes of the diff, never the
// file.
func (s *Server) auditMemoryEdit(r *http.Request, p Principal, ch memory.Change, outcome string) {
	detail := "name=" + ch.Name + "; diff=" + textcap.Cut(ch.Diff, maxDiffAudit)
	// the write happened: a client that disconnected or a route deadline that
	// ended must not drop the record
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), auditTimeout)
	defer cancel()
	err := s.d.Store.InTx(ctx, func(tx store.Tx) error {
		return s.appendAudit(tx, p.UserID, "memory_edited", outcome, detail)
	})
	if err != nil {
		var id string
		if ri := infoFrom(r.Context()); ri != nil {
			id = ri.id
		}
		s.d.Log.Warn("api: recording a memory edit failed", "category", "memory_audit", "request_id", id, "user", p.UserID)
	}
}

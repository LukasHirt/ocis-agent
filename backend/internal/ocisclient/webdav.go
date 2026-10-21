package ocisclient

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

const davPrefix = "/remote.php/dav/spaces/"

const propfindBody = `<?xml version="1.0" encoding="utf-8"?><d:propfind xmlns:d="DAV:"><d:prop><d:getetag/><d:getcontentlength/><d:getlastmodified/><d:resourcetype/></d:prop></d:propfind>`

// Entry is a file or folder in a space.
type Entry struct {
	Path    string // absolute inside the space; "/" is the root; no trailing slash
	Dir     bool
	Size    int64
	ETag    string // as the server sends it, quotes included: ready for If-Match
	ModTime time.Time
}

// target is a validated WebDAV address.
type target struct {
	clean string // normalised path inside the space
	esc   string // escaped URL path
	log   string // what errors show: the decoded URL path
}

// davTarget validates drive and p and builds the URL path. Segments "." and
// ".." and control characters are rejected rather than resolved, so a path
// cannot climb out of what the caller meant.
func davTarget(drive, p string) (target, error) {
	if drive == "" || drive == "." || drive == ".." || strings.ContainsAny(drive, "/?#") {
		return target{}, ErrInvalidPath
	}
	for _, r := range drive {
		if r < 0x20 || r == 0x7f {
			return target{}, ErrInvalidPath
		}
	}
	if p == "" {
		return target{}, ErrInvalidPath
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return target{}, ErrInvalidPath
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return target{}, ErrInvalidPath
		}
	}
	clean := path.Clean("/" + p)
	esc := davPrefix + url.PathEscape(drive)
	if clean != "/" {
		for _, seg := range strings.Split(strings.TrimPrefix(clean, "/"), "/") {
			esc += "/" + url.PathEscape(seg)
		}
	}
	return target{clean: clean, esc: esc, log: davPrefix + drive + strings.TrimSuffix(clean, "/")}, nil
}

type multistatus struct {
	Responses []struct {
		Href      string `xml:"href"`
		Propstats []struct {
			Prop struct {
				ETag     string `xml:"getetag"`
				Length   string `xml:"getcontentlength"`
				Modified string `xml:"getlastmodified"`
				Type     struct {
					Collection *struct{} `xml:"collection"`
				} `xml:"resourcetype"`
			} `xml:"prop"`
		} `xml:"propstat"`
	} `xml:"response"`
}

// hrefToPath turns a PROPFIND href into a path inside the space.
// Hrefs that are not canonical, hold control characters or an encoded slash,
// or point at another host are rejected.
func (c *Client) hrefToPath(href, drive string) (string, error) {
	bad := fmt.Errorf("ocisclient: PROPFIND: href: %w", ErrMalformed)
	u, err := url.Parse(href)
	if err != nil {
		return "", bad
	}
	if u.Host != "" {
		base, err := url.Parse(c.base)
		if err != nil || !strings.EqualFold(u.Host, base.Host) {
			return "", bad
		}
	}
	if strings.Contains(strings.ToLower(u.EscapedPath()), "%2f") {
		return "", bad
	}
	prefix := davPrefix + drive
	if u.Path != prefix && !strings.HasPrefix(u.Path, prefix+"/") {
		return "", fmt.Errorf("ocisclient: PROPFIND: href outside the requested space: %w", ErrMalformed)
	}
	rel := strings.TrimRight(strings.TrimPrefix(u.Path, prefix), "/")
	if rel == "" {
		return "/", nil
	}
	for _, r := range rel {
		if r < 0x20 || r == 0x7f {
			return "", bad
		}
	}
	if rel != path.Clean(rel) || !strings.HasPrefix(rel, "/") {
		return "", bad
	}
	return rel, nil
}

// propfind sends a PROPFIND. In lenient mode a response entry whose href is
// unusable (a control character in the name, outside the space, not
// canonical) is left out and counted in dropped instead of failing the whole
// listing; strict mode fails with ErrMalformed.
func (c *Client) propfind(ctx context.Context, cr Creds, drive, p, depth string, lenient bool) (_ []Entry, dropped int, _ target, _ error) {
	tg, err := davTarget(drive, p)
	if err != nil {
		return nil, 0, target{}, err
	}
	resp, err := c.do(ctx, cr, "PROPFIND", tg.esc, nil,
		map[string]string{"Depth": depth, "Content-Type": "application/xml; charset=utf-8"}, []byte(propfindBody), maxBody)
	if err != nil {
		return nil, 0, tg, err
	}
	if resp.status != http.StatusMultiStatus {
		return nil, 0, tg, statusError("PROPFIND", tg.log, resp.status, nil)
	}
	var ms multistatus
	if err := xml.Unmarshal(resp.body, &ms); err != nil {
		return nil, 0, tg, fmt.Errorf("ocisclient: PROPFIND: %w", ErrMalformed)
	}
	var out []Entry
	for _, r := range ms.Responses {
		ep, err := c.hrefToPath(r.Href, drive)
		if err != nil && lenient {
			dropped++
			continue
		}
		if err != nil {
			return nil, 0, tg, err
		}
		e := Entry{Path: ep}
		for _, ps := range r.Propstats {
			if ps.Prop.ETag != "" && e.ETag == "" {
				e.ETag = ps.Prop.ETag
			}
			if ps.Prop.Type.Collection != nil {
				e.Dir = true
			}
			if n, err := strconv.ParseInt(strings.TrimSpace(ps.Prop.Length), 10, 64); err == nil && n >= 0 {
				e.Size = n
			}
			if t, err := http.ParseTime(strings.TrimSpace(ps.Prop.Modified)); err == nil {
				e.ModTime = t.UTC()
			}
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		return nil, 0, tg, fmt.Errorf("ocisclient: PROPFIND: empty response: %w", ErrMalformed)
	}
	return out, dropped, tg, nil
}

// Stat returns the entry at p (PROPFIND, depth 0).
func (c *Client) Stat(ctx context.Context, cr Creds, drive, p string) (Entry, error) {
	entries, _, tg, err := c.propfind(ctx, cr, drive, p, "0", false)
	if err != nil {
		return Entry{}, err
	}
	for _, e := range entries {
		if e.Path == tg.clean {
			return e, nil
		}
	}
	return Entry{}, fmt.Errorf("ocisclient: PROPFIND: the response does not describe the requested path: %w", ErrMalformed)
}

// List returns the children of the folder at p, sorted by path. Any unusable
// entry fails the listing with ErrMalformed.
func (c *Client) List(ctx context.Context, cr Creds, drive, p string) ([]Entry, error) {
	children, _, err := c.list(ctx, cr, drive, p, false)
	return children, err
}

func (c *Client) list(ctx context.Context, cr Creds, drive, p string, lenient bool) ([]Entry, int, error) {
	entries, dropped, tg, err := c.propfind(ctx, cr, drive, p, "1", lenient)
	if err != nil {
		return nil, 0, err
	}
	var self *Entry
	var children []Entry
	for i, e := range entries {
		switch {
		case e.Path == tg.clean:
			self = &entries[i]
		case path.Dir(e.Path) == tg.clean:
			children = append(children, e)
		}
	}
	if self == nil {
		return nil, 0, fmt.Errorf("ocisclient: PROPFIND: the response does not describe the requested path: %w", ErrMalformed)
	}
	if !self.Dir {
		return nil, 0, ErrNotDir
	}
	sort.Slice(children, func(i, j int) bool { return children[i].Path < children[j].Path })
	return children, dropped, nil
}

func etagFrom(h http.Header) string {
	if e := h.Get("ETag"); e != "" {
		return e
	}
	return h.Get("OC-ETag")
}

// Get downloads a file of at most maxBytes and returns its content and ETag.
// A larger file fails with ErrTooLarge without being read to the end. It fails
// closed when the response carries no ETag (a Stat could return a newer one
// than the content). While oCIS postprocesses a fresh upload it answers 425;
// Get retries for about three seconds and then fails with ErrTooEarly.
func (c *Client) Get(ctx context.Context, cr Creds, drive, p string, maxBytes int64) ([]byte, string, error) {
	tg, err := davTarget(drive, p)
	if err != nil {
		return nil, "", err
	}
	if maxBytes < 0 {
		return nil, "", errors.New("ocisclient: negative size limit")
	}
	resp, err := c.do(ctx, cr, http.MethodGet, tg.esc, nil, nil, nil, maxBytes)
	// oCIS answers 425 while it postprocesses a fresh upload (a GET right
	// after a PUT, also our own): wait it out for a few seconds
	for i := 0; err == nil && resp.status == http.StatusTooEarly && i < len(c.tooEarlyWaits); i++ {
		t := time.NewTimer(c.tooEarlyWaits[i])
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, "", fmt.Errorf("ocisclient: GET %s: %w", tg.log, ctx.Err())
		case <-t.C:
		}
		resp, err = c.do(ctx, cr, http.MethodGet, tg.esc, nil, nil, nil, maxBytes)
	}
	if err != nil {
		return nil, "", err
	}
	if resp.status == http.StatusTooEarly {
		return nil, "", statusError(http.MethodGet, tg.log, resp.status, ErrTooEarly)
	}
	if resp.status != http.StatusOK {
		return nil, "", statusError(http.MethodGet, tg.log, resp.status, nil)
	}
	etag := etagFrom(resp.header)
	if etag == "" {
		return nil, "", fmt.Errorf("ocisclient: GET %s: no ETag in the response", tg.log)
	}
	return resp.body, etag, nil
}

// Put uploads data to p and returns the new ETag. With a non-empty ifMatch the
// upload only replaces the file if it still has that ETag; otherwise it fails
// with ErrPrecondition (oCIS answers 409 for this, 412 is accepted too). It
// fails closed when the response carries no ETag. With
// an empty ifMatch it overwrites unconditionally: oCIS ignores If-None-Match,
// so callers that must not overwrite stat first and accept the small race.
func (c *Client) Put(ctx context.Context, cr Creds, drive, p string, data []byte, ifMatch string) (string, error) {
	tg, err := davTarget(drive, p)
	if err != nil {
		return "", err
	}
	if tg.clean == "/" {
		return "", ErrInvalidPath
	}
	if data == nil {
		data = []byte{}
	}
	if strings.ContainsAny(ifMatch, "\r\n") {
		return "", errors.New("ocisclient: invalid If-Match value")
	}
	hdr := map[string]string{"Content-Type": "application/octet-stream"}
	if ifMatch != "" {
		hdr["If-Match"] = ifMatch
	}
	resp, err := c.do(ctx, cr, http.MethodPut, tg.esc, nil, hdr, data, maxBody)
	if err != nil {
		return "", err
	}
	switch resp.status {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent:
	case http.StatusConflict, http.StatusPreconditionFailed:
		kind := ErrNotFound // no If-Match: a 409 means a missing parent folder
		if ifMatch != "" || resp.status == http.StatusPreconditionFailed {
			kind = ErrPrecondition
		}
		return "", statusError(http.MethodPut, tg.log, resp.status, kind)
	default:
		return "", statusError(http.MethodPut, tg.log, resp.status, nil)
	}
	etag := etagFrom(resp.header)
	if etag == "" {
		return "", fmt.Errorf("ocisclient: PUT %s: no ETag in the response", tg.log)
	}
	return etag, nil
}

// Mkcol creates a folder. ErrExists if p exists, ErrNotFound if the parent is
// missing.
func (c *Client) Mkcol(ctx context.Context, cr Creds, drive, p string) error {
	tg, err := davTarget(drive, p)
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, cr, "MKCOL", tg.esc, nil, nil, nil, maxBody)
	if err != nil {
		return err
	}
	switch resp.status {
	case http.StatusCreated, http.StatusOK:
		return nil
	case http.StatusMethodNotAllowed:
		return statusError("MKCOL", tg.log, resp.status, ErrExists)
	case http.StatusConflict:
		return statusError("MKCOL", tg.log, resp.status, ErrNotFound)
	}
	return statusError("MKCOL", tg.log, resp.status, nil)
}

// Delete removes a file or folder (oCIS moves it to the trash). oCIS ignores
// If-Match on DELETE, so a caller that must delete one specific version
// compares the ETag itself first. The space root cannot be deleted.
func (c *Client) Delete(ctx context.Context, cr Creds, drive, p string) error {
	tg, err := davTarget(drive, p)
	if err != nil {
		return err
	}
	if tg.clean == "/" {
		return ErrInvalidPath
	}
	resp, err := c.do(ctx, cr, http.MethodDelete, tg.esc, nil, nil, nil, maxBody)
	if err != nil {
		return err
	}
	if resp.status == http.StatusNoContent || resp.status == http.StatusOK {
		return nil
	}
	return statusError(http.MethodDelete, tg.log, resp.status, nil)
}

// Move renames or moves from to to within the space. It never overwrites:
// ErrExists if to exists.
func (c *Client) Move(ctx context.Context, cr Creds, drive, from, to string) error {
	return c.moveCopy(ctx, cr, "MOVE", drive, from, to)
}

// Copy copies from to to within the space. It never overwrites: ErrExists if
// to exists.
func (c *Client) Copy(ctx context.Context, cr Creds, drive, from, to string) error {
	return c.moveCopy(ctx, cr, "COPY", drive, from, to)
}

func (c *Client) moveCopy(ctx context.Context, cr Creds, method, drive, from, to string) error {
	src, err := davTarget(drive, from)
	if err != nil {
		return err
	}
	dst, err := davTarget(drive, to)
	if err != nil {
		return err
	}
	if src.clean == "/" || dst.clean == "/" {
		return ErrInvalidPath
	}
	hdr := map[string]string{"Destination": c.base + dst.esc, "Overwrite": "F"}
	resp, err := c.do(ctx, cr, method, src.esc, nil, hdr, nil, maxBody)
	if err != nil {
		return err
	}
	switch resp.status {
	case http.StatusCreated, http.StatusNoContent, http.StatusOK:
		return nil
	case http.StatusPreconditionFailed:
		return statusError(method, src.log, resp.status, ErrExists)
	case http.StatusConflict:
		return statusError(method, src.log, resp.status, ErrNotFound)
	}
	return statusError(method, src.log, resp.status, nil)
}

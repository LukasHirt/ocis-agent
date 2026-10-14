package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const ssePath = "/ocs/v2.php/apps/notifications/api/v1/notifications/sse"

func status(r response, err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	return fmt.Sprintf("%d", r.status)
}

// probeQ3 asks: can an app token create (renew) an app token?
func probeQ3(ctx context.Context, c *client, app cred, orig *appToken, out io.Writer) {
	fmt.Fprintln(out, "## Q3: app-token self-renewal")
	resp, renewed, err := c.createToken(ctx, app, "ocis-agent-spike-renewed")
	switch {
	case err != nil:
		fmt.Fprintf(out, "- POST /auth-app/tokens authenticated with an app token: error: %v\n", err)
	case renewed == nil:
		fmt.Fprintf(out, "- POST /auth-app/tokens authenticated with an app token: **%d** body=`%s`\n",
			resp.status, redact(string(resp.body), app.secret))
	default:
		fmt.Fprintf(out, "- POST /auth-app/tokens authenticated with an app token: **%d, a new token was issued**\n", resp.status)
		fmt.Fprintf(out, "  - original expires %s, renewed expires %s (label %q)\n",
			orig.ExpirationDate.Format(time.RFC3339), renewed.ExpirationDate.Format(time.RFC3339), renewed.Label)
		meResp, meErr := c.me(ctx, cred{app.user, renewed.Token})
		fmt.Fprintf(out, "  - GET /graph/v1.0/me with the renewed token: %s\n", status(meResp, meErr))
		delResp, delErr := c.deleteByLabel(ctx, cred{app.user, renewed.Token}, "ocis-agent-spike")
		fmt.Fprintf(out, "  - DELETE of the ORIGINAL token authenticated with the renewed one: %s\n", status(delResp, delErr))
		cleanResp, cleanErr := c.deleteByLabel(ctx, cred{app.user, renewed.Token}, "ocis-agent-spike-renewed")
		fmt.Fprintf(out, "  - cleanup DELETE of the renewed token (self): %s\n", status(cleanResp, cleanErr))
	}
	listResp, listErr := c.do(ctx, app, http.MethodGet, "/auth-app/tokens", nil, nil)
	fmt.Fprintf(out, "- GET /auth-app/tokens authenticated with an app token: %s\n", status(listResp, listErr))
	fmt.Fprintln(out)
}

type fileOpResult struct{ step, status string }

// exerciseFiles creates a folder, uploads a file, moves it, then deletes it, to
// provoke file-change events. It returns the status of each step.
func exerciseFiles(ctx context.Context, c *client, cr cred, driveID, dir string) []fileOpResult {
	var res []fileOpResult
	add := func(step string, r response, err error) { res = append(res, fileOpResult{step, status(r, err)}) }

	r, err := c.do(ctx, cr, "MKCOL", davPath(driveID, dir), nil, nil)
	add("MKCOL "+dir, r, err)
	r, err = c.do(ctx, cr, http.MethodPut, davPath(driveID, dir+"/a.txt"), nil, strings.NewReader("hello"))
	add("PUT a.txt", r, err)
	r, err = c.do(ctx, cr, "MOVE", davPath(driveID, dir+"/a.txt"),
		map[string]string{"Destination": c.base + davPath(driveID, dir+"/b.txt"), "Overwrite": "F"}, nil)
	add("MOVE a.txt -> b.txt", r, err)
	r, err = c.do(ctx, cr, http.MethodDelete, davPath(driveID, dir+"/b.txt"), nil, nil)
	add("DELETE b.txt", r, err)
	r, err = c.do(ctx, cr, http.MethodDelete, davPath(driveID, dir), nil, nil)
	add("DELETE "+dir, r, err)
	return res
}

// probeSSE opens the SSE stream with cr, triggers file operations with the
// same credential after a short delay, and prints every event it receives.
func probeSSE(ctx context.Context, c *client, label string, cr cred, driveID string, window time.Duration, out io.Writer) {
	fmt.Fprintf(out, "### SSE with %s\n", label)

	sctx, cancel := context.WithTimeout(ctx, window)
	defer cancel()
	req, err := http.NewRequestWithContext(sctx, http.MethodGet, c.base+ssePath, nil)
	if err != nil {
		fmt.Fprintf(out, "- request error: %v\n\n", err)
		return
	}
	req.SetBasicAuth(cr.user, cr.secret)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		fmt.Fprintf(out, "- connect error: %v\n\n", err)
		return
	}
	defer resp.Body.Close()
	fmt.Fprintf(out, "- GET %s: **%d**, Content-Type `%s`\n", ssePath, resp.StatusCode, resp.Header.Get("Content-Type"))
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		fmt.Fprintf(out, "- body: `%s`\n\n", redact(string(b), cr.secret))
		return
	}

	opsCh := make(chan []fileOpResult, 1)
	go func() {
		select {
		case <-time.After(2 * time.Second): // let the stream settle first
		case <-sctx.Done():
			opsCh <- nil
			return
		}
		opsCh <- exerciseFiles(ctx, c, cr, driveID, fmt.Sprintf("ocis-agent-spike-sse-%d", time.Now().UnixNano()))
	}()

	var events []sseEvent
	_ = readSSE(resp.Body, func(e sseEvent) { events = append(events, e) }) // ends when sctx expires
	ops := <-opsCh

	for _, o := range ops {
		fmt.Fprintf(out, "- file op %s: %s\n", o.step, o.status)
	}
	fmt.Fprintf(out, "- events received in %s: **%d**\n", window, len(events))
	for _, e := range events {
		fmt.Fprintf(out, "  - `event=%s` data=`%s`\n", e.Name, redact(e.Data, cr.secret))
	}
	fmt.Fprintln(out)
}

// probeConditional checks the WebDAV preconditions the agent's write tools rely on
// (If-None-Match, If-Match, Overwrite: F).
func probeConditional(ctx context.Context, c *client, cr cred, driveID string, out io.Writer) {
	fmt.Fprintln(out, "## WebDAV conditional requests")
	fmt.Fprintln(out, "| Step | Expected | Actual |")
	fmt.Fprintln(out, "|---|---|---|")
	row := func(step, expected string, r response, err error) {
		fmt.Fprintf(out, "| %s | %s | %s |\n", step, expected, status(r, err))
	}

	dir := fmt.Sprintf("ocis-agent-spike-cond-%d", time.Now().UnixNano())
	f1, f2 := dir+"/f1.txt", dir+"/f2.txt"
	put := func(name, body string, hdr map[string]string) (response, error) {
		return c.do(ctx, cr, http.MethodPut, davPath(driveID, name), hdr, strings.NewReader(body))
	}
	etag := func(name string) string {
		r, err := c.do(ctx, cr, "PROPFIND", davPath(driveID, name), map[string]string{"Depth": "0"},
			strings.NewReader(`<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:getetag/></d:prop></d:propfind>`))
		if err != nil || r.status/100 != 2 {
			return ""
		}
		e, _ := parseETag(r.body)
		return e
	}
	defer func() { _, _ = c.do(ctx, cr, http.MethodDelete, davPath(driveID, dir), nil, nil) }()

	r, err := c.do(ctx, cr, "MKCOL", davPath(driveID, dir), nil, nil)
	row("MKCOL test folder", "201", r, err)

	r, err = put(f1, "v1", map[string]string{"If-None-Match": "*"})
	row("PUT new file, If-None-Match: *", "201", r, err)
	r, err = put(f1, "again", map[string]string{"If-None-Match": "*"})
	row("PUT same path again, If-None-Match: *", "412", r, err)

	e1 := etag(f1)
	r, err = put(f1, "v2", map[string]string{"If-Match": `"bogus"`})
	row("PUT overwrite, If-Match: wrong etag", "412", r, err)
	r, err = put(f1, "v2", map[string]string{"If-Match": e1})
	row("PUT overwrite, If-Match: current etag", "204", r, err)
	e2 := etag(f1)
	fmt.Fprintf(out, "| etag changed after overwrite | different | %v |\n", e1 != "" && e2 != "" && e1 != e2)

	r, err = put(f2, "x", nil)
	row("PUT second file", "201", r, err)
	r, err = c.do(ctx, cr, "MOVE", davPath(driveID, f2),
		map[string]string{"Destination": c.base + davPath(driveID, f1), "Overwrite": "F"}, nil)
	row("MOVE onto existing, Overwrite: F", "412", r, err)
	r, err = c.do(ctx, cr, "COPY", davPath(driveID, f2),
		map[string]string{"Destination": c.base + davPath(driveID, f1), "Overwrite": "F"}, nil)
	row("COPY onto existing, Overwrite: F", "412", r, err)

	r, err = c.do(ctx, cr, http.MethodDelete, davPath(driveID, f1), map[string]string{"If-Match": `"bogus"`}, nil)
	row("DELETE, If-Match: wrong etag", "412 (unknown: record the truth)", r, err)
	r, err = c.do(ctx, cr, http.MethodDelete, davPath(driveID, f1), map[string]string{"If-Match": e2}, nil)
	row("DELETE, If-Match: current etag", "204", r, err)
	fmt.Fprintln(out)
}

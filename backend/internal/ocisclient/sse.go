package ocisclient

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

const ssePath = "/ocs/v2.php/apps/notifications/api/v1/notifications/sse"

// Event is one server-sent event. Data carries ids and an etag but no path.
type Event struct {
	ID, Name, Data string
}

// readSSE parses a text/event-stream and calls emit for each complete event.
// Comment lines (": ...") are heartbeats and produce nothing. An event without
// a name is called "message". An event that is not terminated by a blank line
// when the stream ends is dropped, as the SSE specification requires. If emit
// fails, reading stops and the error is returned.
func readSSE(r io.Reader, emit func(Event) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var (
		ev      Event
		data    []string
		started bool
	)
	flush := func() error {
		defer func() { ev, data, started = Event{}, nil, false }()
		if !started {
			return nil
		}
		ev.Data = strings.Join(data, "\n")
		if ev.Name == "" {
			ev.Name = "message"
		}
		return emit(ev)
	}

	for sc.Scan() {
		line := sc.Text() // ScanLines strips \n and \r\n
		switch {
		case line == "":
			if err := flush(); err != nil {
				return err
			}
		case strings.HasPrefix(line, ":"):
			// heartbeat / comment
		default:
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "event":
				ev.Name, started = value, true
			case "data":
				data, started = append(data, value), true
			case "id":
				ev.ID, started = value, true
			}
		}
	}
	return sc.Err()
}

// idleReader cancels the request when nothing has been read for d.
type idleReader struct {
	r     io.Reader
	d     time.Duration
	timer *time.Timer
	fired atomic.Bool
}

func newIdleReader(r io.Reader, d time.Duration, onIdle func()) *idleReader {
	ir := &idleReader{r: r, d: d}
	ir.timer = time.AfterFunc(d, func() {
		ir.fired.Store(true)
		onIdle()
	})
	return ir
}

func (i *idleReader) Read(p []byte) (int, error) {
	n, err := i.r.Read(p)
	if n > 0 {
		i.timer.Reset(i.d)
	}
	return n, err
}

// StreamEvents connects to the notification stream and calls handle for each
// event until the stream ends. lastEventID, when set, is sent as Last-Event-ID
// so a server that supports it can resume. The HTTP client must not have an
// overall Timeout (the default client has none). Returns: nil when ctx ends,
// ErrStreamClosed when the server closes the stream, ErrIdleTimeout when
// nothing (not even a heartbeat) arrives within the idle timeout, the
// handler's error, or a *StatusError for a non-200 answer.
func (c *Client) StreamEvents(ctx context.Context, cr Creds, lastEventID string, handle func(Event) error) error {
	if !cr.valid() {
		return ErrNoCredentials
	}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	hdr := map[string]string{"Accept": "text/event-stream", "Cache-Control": "no-cache"}
	if lastEventID != "" {
		hdr["Last-Event-ID"] = lastEventID
	}
	req, err := c.newRequest(sctx, cr, http.MethodGet, ssePath, nil, hdr, nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return transportError(http.MethodGet, ssePath, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return statusError(http.MethodGet, ssePath, resp.StatusCode, nil)
	}

	idle := newIdleReader(resp.Body, c.sseIdle, cancel)
	defer idle.timer.Stop()
	var herr error
	err = readSSE(idle, func(e Event) error {
		if herr = handle(e); herr != nil {
			return herr
		}
		return nil
	})
	idle.timer.Stop()
	switch {
	case herr != nil:
		return herr
	case ctx.Err() != nil:
		return nil
	case idle.fired.Load():
		return ErrIdleTimeout
	case err != nil:
		return transportError(http.MethodGet, ssePath, err)
	}
	return ErrStreamClosed
}

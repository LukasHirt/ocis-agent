package api

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A request that never reaches a route (an unmatched path, a wrong method,
// healthz) still gets read and write deadlines: a chunked body that never
// ends must not hold the connection open.
func TestRequestsOutsideRoutesHaveDeadlines(t *testing.T) {
	f := newFixture(t)
	f.srv.baseTimeout = 200 * time.Millisecond
	ts := httptest.NewServer(f.h)
	defer ts.Close()
	for _, start := range []string{
		"POST /agent-api/v1/nope HTTP/1.1",
		"POST /agent-api/healthz HTTP/1.1",
		"GET /agent-api/healthz HTTP/1.1",
		"POST /agent/x HTTP/1.1",
	} {
		conn, err := net.Dial("tcp", ts.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }() // before ts.Close, which waits for handlers
		// headers and one chunk, then nothing: the body never ends
		began := time.Now()
		_, _ = io.WriteString(conn, start+"\r\nHost: x\r\nTransfer-Encoding: chunked\r\nContent-Type: application/json\r\n\r\n1\r\n{\r\n")
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		// the server answers and closes the connection at its read deadline
		_, err = io.Copy(io.Discard, conn)
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() || time.Since(began) > 3*time.Second {
			t.Errorf("%s: connection still open after the deadline (%v after %v)", start, err, time.Since(began))
		}
	}
}

// Routes keep their own deadline (the export's is longer than the default).
func TestRouteDeadlineOverridesTheDefault(t *testing.T) {
	f := newFixture(t)
	f.srv.baseTimeout = 50 * time.Millisecond
	ts := httptest.NewServer(f.h)
	defer ts.Close()
	f.srv.handle("GET /agent-api/v1/test/longer", func(w http.ResponseWriter, r *http.Request) error {
		time.Sleep(300 * time.Millisecond)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return nil
	}, withTimeout(5*time.Second))
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/agent-api/v1/test/longer", nil)
	req.Header.Set("Authorization", "Bearer "+goodToken)
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if err != nil || res.StatusCode != 200 || !strings.Contains(string(body), "ok") {
		t.Errorf("%d %q %v", res.StatusCode, body, err)
	}
}

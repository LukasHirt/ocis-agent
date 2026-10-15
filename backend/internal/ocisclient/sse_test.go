package ocisclient

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func parse(t *testing.T, stream string) []Event {
	t.Helper()
	var got []Event
	err := readSSE(strings.NewReader(stream), func(e Event) error { got = append(got, e); return nil })
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestReadSSEParsesTheWireFormat(t *testing.T) {
	got := parse(t, ": connected\n\n"+
		"id: 1\nevent: folder-created\ndata: {\"id\":\"a\"}\n\n"+
		": heartbeat\n\n"+
		"id: 2\r\nevent: item-renamed\r\ndata: line1\r\ndata: line2\r\n\r\n"+
		"data: no name\n\n"+
		"event: unterminated\ndata: dropped")
	want := []Event{
		{ID: "1", Name: "folder-created", Data: `{"id":"a"}`},
		{ID: "2", Name: "item-renamed", Data: "line1\nline2"},
		{Name: "message", Data: "no name"},
	}
	if len(got) != len(want) {
		t.Fatalf("events = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestReadSSEStopsWhenTheHandlerFails(t *testing.T) {
	boom := errors.New("boom")
	n := 0
	err := readSSE(strings.NewReader("data: 1\n\ndata: 2\n\ndata: 3\n\n"), func(Event) error { n++; return boom })
	if !errors.Is(err, boom) || n != 1 {
		t.Fatalf("err = %v after %d events", err, n)
	}
}

type stream struct {
	cancel context.CancelFunc
	events chan Event
	done   chan error
}

func startStream(t *testing.T, f fixture, c *Client, last string) *stream {
	t.Helper()
	ctx, cancel := context.WithCancel(bg)
	st := &stream{cancel: cancel, events: make(chan Event, 16), done: make(chan error, 1)}
	go func() {
		st.done <- c.StreamEvents(ctx, f.cr, last, func(e Event) error { st.events <- e; return nil })
	}()
	t.Cleanup(cancel)
	deadline := time.Now().Add(3 * time.Second)
	for f.s.OpenStreams("einstein") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the stream never opened")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return st
}

func waitDone(t *testing.T, st *stream) error {
	t.Helper()
	select {
	case err := <-st.done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("StreamEvents did not return")
		return nil
	}
}

func TestStreamEventsDeliversEventsAndSendsLastEventID(t *testing.T) {
	f := newFixture(t)
	st := startStream(t, f, f.c, "41")
	f.s.Publish("einstein", "42", "item-trashed", `{"id":"f"}`)
	select {
	case e := <-st.events:
		if e != (Event{ID: "42", Name: "item-trashed", Data: `{"id":"f"}`}) {
			t.Fatalf("event = %+v", e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no event")
	}
	var sent string
	for _, r := range f.s.Requests() {
		if r.Path == "/ocs/v2.php/apps/notifications/api/v1/notifications/sse" {
			sent = r.Header.Get("Last-Event-ID")
		}
	}
	if sent != "41" {
		t.Fatalf("Last-Event-ID = %q, want 41", sent)
	}
	st.cancel()
	if err := waitDone(t, st); err != nil {
		t.Fatalf("a caller-cancelled stream returns nil, got %v", err)
	}
}

func TestStreamEventsReportsAServerClose(t *testing.T) {
	f := newFixture(t)
	st := startStream(t, f, f.c, "")
	f.s.DropStreams()
	if err := waitDone(t, st); !errors.Is(err, ErrStreamClosed) {
		t.Fatalf("err = %v, want ErrStreamClosed", err)
	}
}

func TestStreamEventsGivesUpOnASilentConnection(t *testing.T) {
	f := newFixture(t)
	c, err := New(f.s.URL, WithSSEIdleTimeout(150*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	st := startStream(t, f, c, "")
	if err := waitDone(t, st); !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("err = %v, want ErrIdleTimeout", err)
	}
}

func TestHeartbeatsKeepTheStreamAlive(t *testing.T) {
	f := newFixture(t)
	f.s.SetHeartbeat(20 * time.Millisecond)
	c, err := New(f.s.URL, WithSSEIdleTimeout(150*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	st := startStream(t, f, c, "")
	select {
	case err := <-st.done:
		t.Fatalf("the stream ended despite heartbeats: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	st.cancel()
	if err := waitDone(t, st); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestStreamEventsStopsWithTheHandlersError(t *testing.T) {
	f := newFixture(t)
	boom := errors.New("handler failed")
	done := make(chan error, 1)
	go func() {
		done <- f.c.StreamEvents(bg, f.cr, "", func(Event) error { return boom })
	}()
	deadline := time.Now().Add(3 * time.Second)
	for f.s.OpenStreams("einstein") == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	f.s.Publish("einstein", "1", "x", "{}")
	select {
	case err := <-done:
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the handler's error", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("StreamEvents did not return")
	}
}

func TestStreamEventsRejectsBadCredentials(t *testing.T) {
	f := newFixture(t)
	err := f.c.StreamEvents(bg, AppToken("einstein", "wrong"), "", func(Event) error { return nil })
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if err := f.c.StreamEvents(bg, Creds{}, "", func(Event) error { return nil }); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("no credentials = %v", err)
	}
}

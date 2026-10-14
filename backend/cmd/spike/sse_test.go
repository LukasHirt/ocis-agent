package main

import (
	"strings"
	"testing"
)

func collect(t *testing.T, input string) []sseEvent {
	t.Helper()
	var got []sseEvent
	if err := readSSE(strings.NewReader(input), func(e sseEvent) { got = append(got, e) }); err != nil {
		t.Fatalf("readSSE: %v", err)
	}
	return got
}

func TestReadSSEBasicEvent(t *testing.T) {
	got := collect(t, "event: postprocessing-finished\ndata: {\"a\":1}\n\n")
	if len(got) != 1 || got[0].Name != "postprocessing-finished" || got[0].Data != `{"a":1}` {
		t.Fatalf("got %+v", got)
	}
}

func TestReadSSEMultiLineDataAndID(t *testing.T) {
	got := collect(t, "id: 7\nevent: x\ndata: line1\ndata: line2\n\n")
	if len(got) != 1 || got[0].ID != "7" || got[0].Data != "line1\nline2" {
		t.Fatalf("got %+v", got)
	}
}

func TestReadSSEIgnoresCommentHeartbeats(t *testing.T) {
	got := collect(t, ": keep-alive\n\n: another\n\nevent: a\ndata: b\n\n")
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("heartbeats must not produce events, got %+v", got)
	}
}

func TestReadSSEHandlesCRLF(t *testing.T) {
	got := collect(t, "event: a\r\ndata: b\r\n\r\n")
	if len(got) != 1 || got[0].Name != "a" || got[0].Data != "b" {
		t.Fatalf("got %+v", got)
	}
}

func TestReadSSEDropsEventCutMidStream(t *testing.T) {
	got := collect(t, "event: complete\ndata: 1\n\nevent: partial\ndata: 2")
	if len(got) != 1 || got[0].Name != "complete" {
		t.Fatalf("the unterminated event must be dropped, got %+v", got)
	}
}

func TestReadSSEFieldWithoutColonOrSpace(t *testing.T) {
	got := collect(t, "event:nospace\ndata\n\n")
	if len(got) != 1 || got[0].Name != "nospace" || got[0].Data != "" {
		t.Fatalf("got %+v", got)
	}
}

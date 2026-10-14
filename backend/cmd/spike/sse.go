package main

import (
	"bufio"
	"io"
	"strings"
)

type sseEvent struct {
	ID, Name, Data string
}

// readSSE parses a text/event-stream and calls emit for each complete event.
// Comment lines (": ...") are heartbeats and produce nothing. An event that is
// not terminated by a blank line when the stream ends is dropped, as the SSE
// specification requires.
func readSSE(r io.Reader, emit func(sseEvent)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var (
		ev      sseEvent
		data    []string
		started bool
	)
	flush := func() {
		if started {
			ev.Data = strings.Join(data, "\n")
			emit(ev)
		}
		ev, data, started = sseEvent{}, nil, false
	}

	for sc.Scan() {
		line := sc.Text() // ScanLines strips \n and \r\n
		switch {
		case line == "":
			flush()
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

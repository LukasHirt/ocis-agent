package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"strings"
	"time"
)

// errIdle is the cancel cause of the idle timer; it never leaves the package
// (a stall is reported as ErrUnavailable).
var errIdle = errors.New("llm: idle timeout")

// activity restarts the idle timer whenever bytes arrive.
type activity struct {
	r io.Reader
	t *time.Timer
	d time.Duration
}

func (a *activity) Read(p []byte) (int, error) {
	n, err := a.r.Read(p)
	if n > 0 {
		a.t.Reset(a.d)
	}
	return n, err
}

func isEventStream(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && strings.EqualFold(mt, "text/event-stream")
}

type wireChunk struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content          *string         `json:"content"`
			Reasoning        *string         `json:"reasoning"`
			ReasoningContent *string         `json:"reasoning_content"`
			ToolCalls        []wireCallDelta `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`
	Error json.RawMessage `json:"error"`
}

type wireCallDelta struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type partialCall struct {
	id, typ, name string
	args          strings.Builder
}

// streamState accumulates the first choice of a chat-completions stream.
type streamState struct {
	o         *OpenAI
	onDelta   func(Delta)
	content   strings.Builder
	reasoning int // bytes of hidden reasoning, only counted
	thinking  bool
	kept      int64 // content, reasoning, tool names and arguments
	calls     []*partialCall
	byIndex   map[int]*partialCall // the latest call of each wire index
	finish    *string
	usage     *Usage
	done      bool // [DONE] seen
}

// countingReader counts the bytes read so far.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

var errLineTooLong = errors.New("llm: line too long")

// readStream parses a server-sent-events body (data lines, comments, CRLF,
// multi-line data; event names, ids and retry hints are ignored). What is
// kept, one line and one event are capped at the response size limit, the
// whole stream at maxWire. A stream that ends without a finish reason or
// [DONE] is an outage, never a half answer. After the finish reason the rest
// must arrive within the finish grace. On an error the response carries the
// estimated usage of what was received.
func (o *OpenAI) readStream(ctx context.Context, body *activity, reqSize int, onDelta func(Delta)) (CompletionResponse, error) {
	st := &streamState{o: o, onDelta: onDelta, byIndex: map[int]*partialCall{}}
	if err := st.read(ctx, body); err != nil {
		u, _ := st.usageFor(reqSize)
		return CompletionResponse{Usage: u, UsageEstimated: true}, err
	}
	resp, err := st.result(reqSize)
	if err != nil {
		u, _ := st.usageFor(reqSize)
		return CompletionResponse{Usage: u, UsageEstimated: true}, err
	}
	return resp, nil
}

func (st *streamState) read(ctx context.Context, body *activity) error {
	o := st.o
	cr := &countingReader{r: io.LimitReader(body, o.maxWire+1)}
	br := bufio.NewReaderSize(cr, 32<<10)
	var data []byte
	hasData, graced := false, false
	for !st.done && !(st.finish != nil && st.usage != nil) {
		if st.finish != nil && !graced {
			graced = true
			if body.d > o.finishGrace {
				body.d = o.finishGrace // same goroutine as Read
				body.t.Reset(o.finishGrace)
			}
		}
		line, rerr := readLine(br, o.maxResponse)
		if cr.n > o.maxWire || rerr == errLineTooLong {
			return ErrTooLarge
		}
		if rerr != nil && rerr != io.EOF {
			if cerr := ctx.Err(); cerr != nil {
				return fmt.Errorf("llm: %w", cerr)
			}
			if st.finish != nil {
				break // finished; only the usage chunk is missing
			}
			return readError(ctx)
		}
		if rerr == io.EOF {
			// an event without its blank line: only [DONE] is trusted
			if hasData && string(bytes.TrimSpace(data)) == "[DONE]" {
				st.done = true
			} else if line := bytes.TrimSpace(line); !hasData && bytes.HasPrefix(line, []byte("data:")) && string(bytes.TrimSpace(line[5:])) == "[DONE]" {
				st.done = true
			}
			break
		}
		switch {
		case len(line) == 0:
			if hasData {
				if err := st.event(data); err != nil {
					return err
				}
			}
			data, hasData = data[:0], false
		case line[0] == ':': // comment or keep-alive
		default:
			field, value, _ := bytes.Cut(line, []byte(":"))
			if string(field) != "data" {
				continue // event, id, retry and unknown fields
			}
			value = bytes.TrimPrefix(value, []byte(" "))
			if hasData {
				data = append(data, '\n')
			}
			data = append(data, value...)
			hasData = true
			if int64(len(data)) > o.maxResponse {
				return ErrTooLarge
			}
		}
	}
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("llm: %w", cerr)
	}
	if !st.done && st.finish == nil {
		return fmt.Errorf("%w: the stream ended early", ErrUnavailable)
	}
	return nil
}

// readLine returns the next line without its LF or CRLF, or errLineTooLong
// once it exceeds limit. At the end of the body it returns what is left with
// io.EOF.
func readLine(br *bufio.Reader, limit int64) ([]byte, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		line = append(line, chunk...)
		if int64(len(line)) > limit+2 { // the line ending does not count
			return nil, errLineTooLong
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return line, err
		}
		line = line[:len(line)-1]
		return bytes.TrimSuffix(line, []byte("\r")), nil
	}
}

func (st *streamState) emit(d Delta) {
	if st.onDelta != nil {
		st.onDelta(d)
	}
}

// keep counts n more bytes against the response size limit.
func (st *streamState) keep(n int) error {
	st.kept += int64(n)
	if st.kept > st.o.maxResponse {
		return ErrTooLarge
	}
	return nil
}

// event handles the data of one event.
func (st *streamState) event(data []byte) error {
	if string(bytes.TrimSpace(data)) == "[DONE]" {
		st.done = true
		return nil
	}
	var c wireChunk
	if err := json.Unmarshal(data, &c); err != nil {
		return malformed("bad stream chunk")
	}
	if len(c.Error) > 0 && string(c.Error) != "null" {
		return fmt.Errorf("%w: error in the stream", ErrUnavailable)
	}
	if c.Usage != nil {
		st.usage = &Usage{In: clampUsage(c.Usage.PromptTokens), Out: clampUsage(c.Usage.CompletionTokens)}
	}
	for _, ch := range c.Choices {
		if ch.Index != 0 {
			continue // only the first choice is used
		}
		d := ch.Delta
		emitted := false // a delta of this chunk reached onDelta
		for _, r := range []*string{d.Reasoning, d.ReasoningContent} {
			if r != nil && *r != "" {
				st.reasoning += len(*r)
				if err := st.keep(len(*r)); err != nil {
					return err
				}
				if !st.thinking {
					st.thinking = true
					st.emit(Delta{Thinking: true})
					emitted = true
				}
			}
		}
		if d.Content != nil && *d.Content != "" {
			if err := st.keep(len(*d.Content)); err != nil {
				return err
			}
			st.content.WriteString(*d.Content)
			st.emit(Delta{Text: *d.Content})
			emitted = true
		}
		for _, tc := range d.ToolCalls {
			if err := st.callDelta(tc); err != nil {
				return err
			}
		}
		reasoned := d.Reasoning != nil && *d.Reasoning != "" || d.ReasoningContent != nil && *d.ReasoningContent != ""
		if !emitted && (reasoned || len(d.ToolCalls) > 0) {
			// reasoning or tool-call fragments only: a heartbeat, so the
			// caller can store text it held back (the stream is alive)
			st.emit(Delta{})
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			f := *ch.FinishReason
			st.finish = &f
		}
	}
	return nil
}

// callDelta adds a tool-call fragment. Fragments are matched by index; a
// known index with a new id starts a new call once the current one has a
// name (servers that number the calls of each chunk from 0), and a server
// that sends no index starts a new call with every new id.
func (st *streamState) callDelta(d wireCallDelta) error {
	var c *partialCall
	newCall := func() error {
		if len(st.calls) >= st.o.maxCalls {
			return malformed("too many tool calls")
		}
		c = &partialCall{}
		st.calls = append(st.calls, c)
		return nil
	}
	if d.Index != nil {
		if *d.Index < 0 {
			return malformed("bad tool call index")
		}
		c = st.byIndex[*d.Index]
		if c == nil || (d.ID != "" && c.id != "" && d.ID != c.id && c.name != "") {
			if err := newCall(); err != nil {
				return err
			}
			st.byIndex[*d.Index] = c
		}
	} else if len(st.calls) == 0 || (d.ID != "" && d.ID != st.calls[len(st.calls)-1].id) {
		if err := newCall(); err != nil {
			return err
		}
	} else {
		c = st.calls[len(st.calls)-1]
	}
	if d.ID != "" {
		if c.id != "" && c.id != d.ID {
			return malformed("bad tool call id")
		}
		c.id = d.ID
	}
	if d.Type != "" {
		if c.typ != "" && c.typ != d.Type {
			return malformed("unsupported tool call type")
		}
		c.typ = d.Type
	}
	if err := st.keep(len(d.Function.Name) + len(d.Function.Arguments)); err != nil {
		return err
	}
	// name fragments are concatenated, like the arguments; a server that
	// repeats the whole name yields an unknown tool, which the gate denies
	c.name += d.Function.Name
	c.args.WriteString(d.Function.Arguments)
	return nil
}

// usageFor is the streamed usage with every missing (zero) field estimated
// roughly at three bytes a token, never zero: the request for the prompt,
// what was received (hidden reasoning included) for the output.
func (st *streamState) usageFor(reqSize int) (Usage, bool) {
	var u Usage
	if st.usage != nil {
		u = *st.usage
	}
	estimated := false
	if u.In <= 0 {
		u.In = clampUsage(int64(max(1, (reqSize+2)/3)))
		estimated = true
	}
	if u.Out <= 0 {
		u.Out = clampUsage(max(1, (st.kept+2)/3))
		estimated = true
	}
	return u, estimated
}

func (st *streamState) result(reqSize int) (CompletionResponse, error) {
	content := st.content.String()
	calls := make([]wireCall, len(st.calls))
	for i, c := range st.calls {
		calls[i] = wireCall{ID: c.id, Type: c.typ, Function: wireFunc{Name: c.name, Arguments: c.args.String()}}
	}
	finish := ""
	if st.finish != nil {
		finish = *st.finish
	}
	msg, stop, err := st.o.decodeChoice(&content, calls, finish)
	if err != nil {
		return CompletionResponse{}, err
	}
	resp := CompletionResponse{Message: msg, Stop: stop}
	resp.Usage, resp.UsageEstimated = st.usageFor(reqSize)
	return resp, nil
}

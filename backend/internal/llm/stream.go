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
	calls     []*partialCall
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

// readStream parses a server-sent-events body (data lines, comments, CRLF,
// multi-line data; event names, ids and retry hints are ignored). The whole
// stream is capped at the response size limit. A stream that ends without a
// finish reason or [DONE] is an outage, never a half answer.
func (o *OpenAI) readStream(ctx context.Context, body io.Reader, reqSize int, onDelta func(Delta)) (CompletionResponse, error) {
	cr := &countingReader{r: io.LimitReader(body, o.maxResponse+1)}
	br := bufio.NewReaderSize(cr, 32<<10)
	st := &streamState{o: o, onDelta: onDelta}
	var data []byte
	hasData := false
	for !st.done && !(st.finish != nil && st.usage != nil) {
		line, rerr := readLine(br)
		if cr.n > o.maxResponse {
			return CompletionResponse{}, ErrTooLarge
		}
		if rerr != nil && rerr != io.EOF {
			if cerr := ctx.Err(); cerr != nil {
				return CompletionResponse{}, fmt.Errorf("llm: %w", cerr)
			}
			if st.finish != nil {
				break // finished; only the usage chunk is missing
			}
			return CompletionResponse{}, readError(ctx)
		}
		if rerr == io.EOF {
			// an event without its blank line: only [DONE] is trusted
			if hasData && string(bytes.TrimSpace(data)) == "[DONE]" {
				st.done = true
			} else if line := bytes.TrimSpace(line); bytes.HasPrefix(line, []byte("data:")) && string(bytes.TrimSpace(line[5:])) == "[DONE]" && !hasData {
				st.done = true
			}
			break
		}
		switch {
		case len(line) == 0:
			if hasData {
				if err := st.event(data); err != nil {
					return CompletionResponse{}, err
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
		}
	}
	if cerr := ctx.Err(); cerr != nil {
		return CompletionResponse{}, fmt.Errorf("llm: %w", cerr)
	}
	if !st.done && st.finish == nil {
		return CompletionResponse{}, fmt.Errorf("%w: the stream ended early", ErrUnavailable)
	}
	return st.result(reqSize)
}

// readLine returns the next line without its LF or CRLF. At the end of the
// body it returns what is left with io.EOF.
func readLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		line = append(line, chunk...)
		if err == bufio.ErrBufferFull {
			continue // the size cap bounds the line through the counting reader
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
		for _, r := range []*string{d.Reasoning, d.ReasoningContent} {
			if r != nil && *r != "" {
				st.reasoning += len(*r)
				if !st.thinking {
					st.thinking = true
					st.emit(Delta{Thinking: true})
				}
			}
		}
		if d.Content != nil && *d.Content != "" {
			st.content.WriteString(*d.Content)
			st.emit(Delta{Text: *d.Content})
		}
		for _, tc := range d.ToolCalls {
			if err := st.callDelta(tc); err != nil {
				return err
			}
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			f := *ch.FinishReason
			st.finish = &f
		}
	}
	return nil
}

// callDelta adds a tool-call fragment. Fragments are matched by index; a
// server that sends none starts a new call with every new id.
func (st *streamState) callDelta(d wireCallDelta) error {
	var i int
	switch {
	case d.Index != nil:
		i = *d.Index
	case len(st.calls) == 0 || (d.ID != "" && d.ID != st.calls[len(st.calls)-1].id):
		i = len(st.calls)
	default:
		i = len(st.calls) - 1
	}
	if i < 0 || i > len(st.calls) {
		return malformed("bad tool call index")
	}
	if i == len(st.calls) {
		if i >= st.o.maxCalls {
			return malformed("too many tool calls")
		}
		st.calls = append(st.calls, &partialCall{})
	}
	c := st.calls[i]
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
	// some servers repeat the whole name in every fragment
	if n := d.Function.Name; n != "" && n != c.name {
		c.name += n
	}
	c.args.WriteString(d.Function.Arguments)
	return nil
}

func (st *streamState) result(reqSize int) (CompletionResponse, error) {
	content := st.content.String()
	out := len(content) + st.reasoning
	calls := make([]wireCall, len(st.calls))
	for i, c := range st.calls {
		calls[i] = wireCall{ID: c.id, Type: c.typ, Function: wireFunc{Name: c.name, Arguments: c.args.String()}}
		out += len(c.name) + c.args.Len()
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
	if st.usage != nil && st.usage.In+st.usage.Out > 0 {
		resp.Usage = *st.usage
	} else {
		// no usage streamed: about four bytes a token, never zero
		resp.Usage = Usage{In: clampUsage(int64(max(1, (reqSize+3)/4))), Out: clampUsage(int64(max(1, (out+3)/4)))}
		resp.UsageEstimated = true
	}
	return resp, nil
}

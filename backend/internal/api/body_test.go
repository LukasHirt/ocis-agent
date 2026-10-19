package api

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

// heapDelta returns the bytes allocated while fn runs.
func heapDelta(fn func()) uint64 {
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	fn()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

func TestBodyLimitIs64KiBByDefault(t *testing.T) {
	f := newFixture(t)
	ok := `{"value":"` + strings.Repeat("x", 60<<10) + `"}`
	if rec := f.do(http.MethodPost, "/agent-api/v1/test/echo", ok); rec.Code != 200 {
		t.Fatalf("60 KiB: %d", rec.Code)
	}
	big := `{"value":"` + strings.Repeat("x", 64<<10) + `"}`
	wantError(t, f.do(http.MethodPost, "/agent-api/v1/test/echo", big), 413, "too_large")
	r := f.req(http.MethodPost, "/agent-api/v1/test/echo", "")
	r.Body = io.NopCloser(strings.NewReader(big))
	r.ContentLength = -1
	r.Header.Set("Content-Type", "application/json")
	wantError(t, f.serve(r), 413, "too_large")
}

func TestARouteCanRaiseItsBodyLimit(t *testing.T) {
	f := newFixture(t)
	f.srv.handle("POST /agent-api/v1/test/bigger", func(w http.ResponseWriter, r *http.Request) error {
		var b echoBody
		if err := decodeBody(w, r, &b); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]int{"len": len(b.Value)})
		return nil
	}, withMaxBody(256<<10))
	body := `{"value":"` + strings.Repeat("x", 200<<10) + `"}`
	if rec := f.do(http.MethodPost, "/agent-api/v1/test/bigger", body); rec.Code != 200 {
		t.Errorf("200 KiB on a 256 KiB route: %d", rec.Code)
	}
	wantError(t, f.do(http.MethodPost, "/agent-api/v1/test/echo", body), 413, "too_large")
}

// A large body is refused before it is read or parsed: a 1 MiB array costs
// (almost) nothing, with or without a Content-Length.
func TestOversizedBodiesAreRefusedWithoutAllocating(t *testing.T) {
	f := newFixture(t)
	huge := `{"watch":[` + strings.Repeat("0,", 512<<10) + `0]}`
	withLength := f.req(http.MethodPost, "/agent-api/v1/test/echo", huge)
	chunked := f.req(http.MethodPost, "/agent-api/v1/test/echo", "")
	chunked.Body = io.NopCloser(strings.NewReader(huge))
	chunked.ContentLength = -1
	chunked.Header.Set("Content-Type", "application/json")
	for name, c := range map[string]struct {
		r   func() int
		max uint64
	}{
		"content-length": {func() int { return f.serve(withLength).Code }, 64 << 10}, // about 15 KiB: nothing is read
		"chunked":        {func() int { return f.serve(chunked).Code }, 512 << 10},   // about 150 KiB: the limit is read
	} {
		var code int
		d := heapDelta(func() { code = c.r() })
		if code != 413 {
			t.Errorf("%s: %d", name, code)
		}
		if d > c.max {
			t.Errorf("%s: %d bytes allocated, want at most %d", name, d, c.max)
		}
	}
}

// A body within the size limit but with very many values is refused before
// the generic parses (each value would be boxed three times).
func TestBodiesWithTooManyValuesAreRefusedCheaply(t *testing.T) {
	f := newFixture(t)
	many := `{"value":"v","list":[` + strings.Repeat("0,", 30000) + `0]}`
	var rec int
	d := heapDelta(func() { rec = f.do(http.MethodPost, "/agent-api/v1/test/echo", many).Code })
	if rec != 400 {
		t.Errorf("status %d", rec)
	}
	if d > 1<<20 {
		t.Errorf("%d bytes allocated for a refused body", d)
	}
	// strings with commas and brackets are not counted as values
	str := `{"value":"` + strings.Repeat("[,{", 10000) + `"}`
	if c := f.do(http.MethodPost, "/agent-api/v1/test/echo", str).Code; c != 200 {
		t.Errorf("a long string with separators: %d", c)
	}
}

func TestCountValues(t *testing.T) {
	for in, want := range map[string]int{
		`{}`:                      1,
		`{"a":1}`:                 2,
		`{"a":[1,2,3]}`:           5,
		`{"a":"x,[{}]","b":null}`: 3,
		`{"a":"\"],"}`:            2,
		`[[],[],{}]`:              4,
	} {
		if got := countValues([]byte(in)); got != want {
			t.Errorf("countValues(%s) = %d, want %d", in, got, want)
		}
	}
}

// The largest legitimate bodies fit: settings with 20 watch paths of about
// 1 KiB (~21 KiB) and a bulk decision of 100 items (~12 KiB).
func TestTheLargestLegitimateBodiesFit(t *testing.T) {
	s := newStack(t)
	s.enroll(t, "einstein")
	var watch []string
	for i := range maxWatchEntries {
		watch = append(watch, fmt.Sprintf(`"/p%02d/%s"`, i, strings.Repeat("a", maxWatchPathLen-6)))
	}
	settings := `{"proactive_enabled":true,"watch":[` + strings.Join(watch, ",") + `],"sweep_interval":"6h0m0s","quiet_hours":"22:00-07:00","daily_tokens":100000,"models":{"chat":"model-a","proactive":"model-b"}}`
	if len(settings) < 20<<10 {
		t.Fatalf("settings body is only %d bytes", len(settings))
	}
	if rec := s.as("einstein", http.MethodPut, "/agent-api/v1/settings", settings); rec.Code != 200 {
		t.Errorf("settings of %d bytes: %d %s", len(settings), rec.Code, rec.Body.String())
	}
	var items []string
	for i := range maxBulkItems {
		items = append(items, fmt.Sprintf(`{"id":"01JABCDEFGHJKMNPQRSTV%05d","args_hash":"sha256:%064x"}`, i, i))
	}
	bulk := `{"decision":"approve","items":[` + strings.Join(items, ",") + `]}`
	r := s.req(http.MethodPost, "/", bulk)
	var b bulkDecisionBody
	if err := decodeBody(nil, r, &b); err != nil || len(b.Items) != maxBulkItems {
		t.Errorf("bulk of %d bytes: %v (%d items)", len(bulk), err, len(b.Items))
	}
}

func TestBodiesWithInvalidTextAreRefused(t *testing.T) {
	f := newFixture(t)
	post := func(body string) *httptest.ResponseRecorder {
		return f.do(http.MethodPost, "/agent-api/v1/test/echo", body)
	}
	bad := map[string]string{
		"raw 0xff":             "{\"value\":\"a\xffb\"}",
		"truncated sequence":   "{\"value\":\"a\xe2\x82\"}",
		"lone high":            `{"value":"a\ud800b"}`,
		"lone high upper":      `{"value":"a\uDBFFb"}`,
		"lone low":             `{"value":"a\udc00b"}`,
		"high then text":       `{"value":"\ud83dx"}`,
		"high then other esc":  `{"value":"\ud83dA"}`,
		"high at end":          `{"value":"x\ud83d"}`,
		"low then high":        `{"value":"\ude00\ud83d"}`,
		"escaped then lone":    `{"value":"\\\ud800"}`,
		"raw 0xff in key":      "{\"va\xfflue\":\"a\"}",
		"lone high in a key":   `{"va\ud800":"a"}`,
		"high then escaped bs": `{"value":"\ud83d\\u0041"}`,
	}
	for name, body := range bad {
		wantError(t, post(body), http.StatusBadRequest, "invalid_request")
		_ = name
	}
	good := map[string]string{
		"valid pair":        `{"value":"\ud83d\ude00"}`,
		"valid pair upper":  `{"value":"\uD83D\uDE00!"}`,
		"raw emoji":         `{"value":"😀"}`,
		"escaped backslash": `{"value":"\\ud800"}`,
		"escaped bs twice":  `{"value":"\\\\ud800 \\ud800"}`,
		"bmp escape":        `{"value":"\u00e9\uffff"}`,
	}
	for name, body := range good {
		if rec := post(body); rec.Code != http.StatusOK {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	// the decoded value of the escaped-backslash case is the literal text
	rec := post(`{"value":"\\ud800"}`)
	if !strings.Contains(rec.Body.String(), `\\ud800`) {
		t.Errorf("body = %s", rec.Body.String())
	}
}

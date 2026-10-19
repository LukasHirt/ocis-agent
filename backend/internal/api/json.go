package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/owncloud/ocis-agent/backend/internal/tools"
)

// defaultMaxBody caps a request body unless the route sets its own limit
// (withMaxBody). The largest bodies of this API are a settings document with
// 20 watch paths of 1 KiB (~21 KiB) and a bulk decision of 100 items
// (~12 KiB). decodeBody parses a body three times generically, so the
// memory per request is a multiple of this limit.
const defaultMaxBody = 64 << 10

// maxBodyValues caps the JSON values of a body; it is checked by a byte
// scan before any parse (the largest legitimate bodies have a few hundred).
const maxBodyValues = 2048

// bodyLimit is the body limit of the request's route.
func bodyLimit(r *http.Request) int64 {
	if ri := infoFrom(r.Context()); ri != nil && ri.maxBody > 0 {
		return ri.maxBody
	}
	return defaultMaxBody
}

// hasBody reports whether the method carries a request body in this API.
func hasBody(method string) bool {
	return method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch
}

// requireJSON is step 9 of the pipeline. POST, PUT and PATCH with a
// non-empty body need Content-Type application/json (only a utf-8 charset
// parameter is allowed) and get the body limit; an empty one needs no
// content type (decodeBody still refuses it where a document is required).
// Other methods must not carry a body at all.
func requireJSON(r *http.Request) error {
	if !hasBody(r.Method) {
		if r.ContentLength != 0 {
			return errBadJSON
		}
		return nil
	}
	if r.ContentLength == 0 {
		return nil
	}
	if r.ContentLength > bodyLimit(r) {
		return errTooLarge
	}
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return errMediaType
	}
	for k, v := range params {
		if k != "charset" || !strings.EqualFold(v, "utf-8") {
			return errMediaType
		}
	}
	return nil
}

// decodeBody strictly decodes the request body into dst: exactly one JSON
// object, no unknown fields, no duplicate or case-variant keys at any depth
// (encoding/json would let the last duplicate win and matches field names
// case-insensitively), nesting of at most 32 levels and at most 256 keys per
// object (tools.Canonicalize enforces the structure), at most maxBodyValues
// values, nothing after it and at most the route's body limit. Errors are errBadJSON or errTooLarge; they never carry
// the decoder's text, which can echo parts of the body.
//
// Keys must match the struct fields' JSON names exactly (checkKeyCase):
// encoding/json would accept "Daily_Tokens" for "daily_tokens".
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) error {
	if r.Body == nil {
		return errBadJSON
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, bodyLimit(r)))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return errTooLarge
		}
		return errBadJSON
	}
	if !utf8.Valid(data) || hasLoneSurrogate(data) {
		return errBadJSON
	}
	if countValues(data) > maxBodyValues {
		return errBadJSON
	}
	if _, err := tools.Canonicalize(data); err != nil {
		return errBadJSON
	}
	if err := checkKeyCase(data, reflect.TypeOf(dst)); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errBadJSON
	}
	if _, err := dec.Token(); err != io.EOF {
		return errBadJSON
	}
	return nil
}

// checkKeyCase refuses any object key that matches a field of the
// destination struct only case-insensitively (encoding/json matches field
// names case-insensitively), at any depth of nested structs, pointers,
// slices, arrays and map VALUES. Keys that match no field at all are left
// to DisallowUnknownFields. Map KEYS are data, which the handler validates
// itself; so are interfaces. A type with its own UnmarshalJSON (on the type
// or its pointer, json.RawMessage included) decodes itself and is skipped.
func checkKeyCase(data []byte, t reflect.Type) error {
	var v any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if dec.Decode(&v) != nil {
		return errBadJSON
	}
	if !keyCaseOK(v, t) {
		return errBadJSON
	}
	return nil
}

var unmarshalerType = reflect.TypeFor[json.Unmarshaler]()

func keyCaseOK(v any, t reflect.Type) bool {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Implements(unmarshalerType) || reflect.PointerTo(t).Implements(unmarshalerType) {
		return true
	}
	switch t.Kind() {
	case reflect.Map:
		obj, ok := v.(map[string]any)
		if !ok {
			return true
		}
		for _, val := range obj {
			if !keyCaseOK(val, t.Elem()) {
				return false
			}
		}
	case reflect.Struct:
		obj, ok := v.(map[string]any)
		if !ok {
			return true // a type mismatch is the decoder's error
		}
		fields := jsonFields(t)
		for k, val := range obj {
			if ft, ok := fields[k]; ok {
				if !keyCaseOK(val, ft) {
					return false
				}
				continue
			}
			for name := range fields {
				if strings.EqualFold(name, k) {
					return false
				}
			}
		}
	case reflect.Slice, reflect.Array:
		arr, ok := v.([]any)
		if !ok {
			return true
		}
		for _, e := range arr {
			if !keyCaseOK(e, t.Elem()) {
				return false
			}
		}
	}
	return true
}

// jsonFields maps the JSON names of t's fields (as encoding/json sees them,
// including the promoted fields of embedded structs without a tag) to their
// types.
func jsonFields(t reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			et := f.Type
			if et.Kind() == reflect.Pointer {
				et = et.Elem()
			}
			if et.Kind() == reflect.Struct {
				for k, v := range jsonFields(et) {
					if _, dup := out[k]; !dup {
						out[k] = v
					}
				}
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = f.Type
	}
	return out
}

// countValues counts the JSON values in b (object keys are not values) with
// a byte scan that allocates nothing beyond its nesting stack. It does not
// validate: malformed input gets some count and the parse refuses it.
func countValues(b []byte) int {
	n := 0
	expect := true // a value may start here
	inStr, esc := false, false
	var stack []byte
	for _, c := range b {
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case ' ', '\t', '\n', '\r':
		case '"':
			if expect {
				n++
				expect = false
			}
			inStr = true
		case '{':
			if expect {
				n++
			}
			stack = append(stack, c)
			expect = false // a key comes next
		case '[':
			if expect {
				n++
			}
			stack = append(stack, c)
			expect = true
		case ':':
			expect = true
		case ',':
			expect = len(stack) > 0 && stack[len(stack)-1] == '['
		case ']', '}':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			expect = false
		default:
			if expect {
				n++
				expect = false
			}
		}
	}
	return n
}

// hasLoneSurrogate reports whether a string of b holds a \uD800-\uDFFF escape
// that is not part of a valid high+low pair (encoding/json would silently
// turn it into U+FFFD). Backslash escapes are consumed in pairs, so an
// escaped backslash followed by "ud800" is plain text. Together with
// utf8.Valid on the raw body, every string of a body is valid Unicode.
func hasLoneSurrogate(b []byte) bool {
	inStr := false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if !inStr {
			inStr = c == '"'
			continue
		}
		switch c {
		case '"':
			inStr = false
		case '\\':
			if i+1 >= len(b) {
				return false // the parse refuses it
			}
			i++
			if b[i] != 'u' {
				continue
			}
			u, ok := hex4(b, i+1)
			if !ok {
				return false // the parse refuses it
			}
			i += 4
			switch {
			case u >= 0xDC00 && u <= 0xDFFF:
				return true
			case u >= 0xD800 && u <= 0xDBFF:
				if i+6 >= len(b) || b[i+1] != '\\' || b[i+2] != 'u' {
					return true
				}
				lo, ok := hex4(b, i+3)
				if !ok || lo < 0xDC00 || lo > 0xDFFF {
					return true
				}
				i += 6
			}
		}
	}
	return false
}

// hex4 parses the four hex digits at b[i:].
func hex4(b []byte, i int) (rune, bool) {
	if i+4 > len(b) {
		return 0, false
	}
	var v rune
	for _, c := range b[i : i+4] {
		switch {
		case c >= '0' && c <= '9':
			v = v<<4 | rune(c-'0')
		case c >= 'a' && c <= 'f':
			v = v<<4 | rune(c-'a'+10)
		case c >= 'A' && c <= 'F':
			v = v<<4 | rune(c-'A'+10)
		default:
			return 0, false
		}
	}
	return v, true
}

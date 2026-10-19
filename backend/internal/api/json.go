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

	"github.com/owncloud/ocis-agent/backend/internal/tools"
)

// maxBodyBytes caps every request body. The API takes small JSON documents
// only.
const maxBodyBytes = 1 << 20

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
	if r.ContentLength > maxBodyBytes {
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
// object (tools.Canonicalize enforces the structure), nothing after it and at
// most maxBodyBytes. Errors are errBadJSON or errTooLarge; they never carry
// the decoder's text, which can echo parts of the body.
//
// Keys must match the struct fields' JSON names exactly (checkKeyCase):
// encoding/json would accept "Daily_Tokens" for "daily_tokens".
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) error {
	if r.Body == nil {
		return errBadJSON
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return errTooLarge
		}
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
// slices and arrays. Keys that match no field at all are left to
// DisallowUnknownFields. Map-typed fields (and json.RawMessage, interfaces)
// are not checked: their keys are data, which the handler validates itself.
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

func keyCaseOK(v any, t reflect.Type) bool {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil {
		return true
	}
	switch t.Kind() {
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

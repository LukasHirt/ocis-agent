package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
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
// Known limit: a single key that differs from the field name only in case
// (for example "Value" for "value") is still accepted by encoding/json; it
// cannot be confused with another key, since duplicates under case folding
// are refused.
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

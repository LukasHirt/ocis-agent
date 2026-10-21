package tools

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxPathLen = 4096
	maxDepth   = 32  // nesting levels of arrays and objects
	maxKeys    = 256 // keys of one object
)

// parse decodes the arguments strictly: a JSON object, no unknown fields (so
// no drive or user can be smuggled in), nothing after it.
func parse[T any](raw json.RawMessage) (T, error) {
	var v T
	trimmed := bytes.Trim(raw, " \t\n\r")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return v, invalid("the arguments must be a JSON object")
	}
	if err := checkKeys(trimmed); err != nil {
		return v, err
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, invalid("%s", err.Error())
	}
	if _, err := dec.Token(); err != io.EOF {
		return v, invalid("unexpected data after the arguments")
	}
	return v, nil
}

// Canonicalize returns the arguments as canonical JSON: object keys sorted,
// no insignificant whitespace, numbers kept as written. The approval binds
// the hash of this form, so the same arguments always hash the same.
func Canonicalize(raw json.RawMessage) ([]byte, error) {
	trimmed := bytes.Trim(raw, " \t\n\r")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, invalid("the arguments must be a JSON object")
	}
	if err := checkKeys(trimmed); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, invalid("%s", err.Error())
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, invalid("unexpected data after the arguments")
	}
	return json.Marshal(v)
}

// checkKeys rejects any object, at any depth, with a duplicate key or two keys
// that are equal under case folding. encoding/json matches struct fields
// case-insensitively and lets the last duplicate win, so such a document could
// execute differently from what its hash approved. Messages never echo keys.
func checkKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := walkValue(dec, 1); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return invalid("unexpected data after the arguments")
	}
	return nil
}

// foldKey maps a key to a form that is equal for exactly the keys that are
// equal under strings.EqualFold: every rune becomes the smallest member of its
// Unicode simple-folding orbit.
func foldKey(k string) string {
	var b strings.Builder
	for _, r := range k {
		m := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if f < m {
				m = f
			}
		}
		b.WriteRune(m)
	}
	return b.String()
}

// walkValue checks one value; depth is the nesting level of the value if it
// is a container (the top-level object is level 1).
func walkValue(dec *json.Decoder, depth int) error {
	tok, err := dec.Token()
	if err != nil {
		return invalid("malformed JSON")
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	if depth > maxDepth {
		return invalid("the arguments are nested too deeply")
	}
	switch d {
	case '[':
		for dec.More() {
			if err := walkValue(dec, depth+1); err != nil {
				return err
			}
		}
	case '{':
		seen := make(map[string]struct{})
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return invalid("malformed JSON")
			}
			k, _ := kt.(string)
			if len(seen) >= maxKeys {
				return invalid("an object has too many keys")
			}
			f := foldKey(k)
			if _, dup := seen[f]; dup {
				return invalid("an object has a duplicate key")
			}
			seen[f] = struct{}{}
			if err := walkValue(dec, depth+1); err != nil {
				return err
			}
		}
	}
	if _, err := dec.Token(); err != nil {
		return invalid("malformed JSON")
	}
	return nil
}

// ArgsHash is "sha256:" and the hex SHA-256 of the canonical arguments. It
// must be computed in exactly this one place.
func ArgsHash(raw json.RawMessage) (string, error) {
	c, err := Canonicalize(raw)
	if err != nil {
		return "", err
	}
	return "sha256:" + sha256Hex(c), nil
}

// IdemKey is the idempotency key of one call: job id, call id and the hex of
// the arguments hash. All three are server-side values.
func IdemKey(jobID, callID, argsHash string) string {
	return jobID + ":" + callID + ":" + strings.TrimPrefix(argsHash, "sha256:")
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// cleanPath validates a path argument: absolute, valid UTF-8, no control
// characters, no "." or ".." segments (rejected, not resolved). It returns the
// path without a trailing slash or doubled slashes.
func cleanPath(p string) (string, error) {
	switch {
	case p == "":
		return "", invalid("a path is required")
	case len(p) > maxPathLen:
		return "", invalid("the path is too long")
	case p[0] != '/':
		return "", invalid("the path must be absolute (start with /)")
	case !utf8.ValidString(p):
		return "", invalid("the path is not valid UTF-8")
	}
	for _, r := range p {
		if isControl(r) {
			return "", invalid("the path contains control or direction-override characters")
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return "", invalid("the path must not contain . or .. segments")
		}
	}
	return path.Clean(p), nil
}

// isControl reports C0 and C1 control characters, line and paragraph
// separators and bidirectional controls. ZWJ and ZWNJ stay allowed (emoji).
func isControl(r rune) bool {
	switch {
	case r < 0x20, r >= 0x7f && r <= 0x9f:
		return true
	case r == 0x2028, r == 0x2029, r == 0x200e, r == 0x200f, r == 0x061c:
		return true
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		return true
	}
	return false
}

// visibleName replaces every character that can break, hide or reorder a line
// (the isControl set: C0 and C1 controls such as NEL, line and paragraph
// separators, bidirectional controls; and invisible ones: soft hyphen,
// combining grapheme joiner, zero-width space, word joiner and invisible
// operators, BOM, tag characters, Hangul fillers) with its visible escape
// \u{XXXX}, as the extension's visibleText does. ZWJ and ZWNJ stay (emoji).
func visibleName(s string) string {
	if !strings.ContainsFunc(s, hiddenRune) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if hiddenRune(r) {
			fmt.Fprintf(&b, "\\u{%04X}", r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func hiddenRune(r rune) bool {
	switch {
	case isControl(r):
		return true
	case r == 0x00ad, r == 0x034f, r == 0x115f, r == 0x1160, r == 0x200b, r == 0x3164, r == 0xfeff:
		return true
	case r >= 0x2060 && r <= 0x2064, r >= 0xe0000 && r <= 0xe007f:
		return true
	}
	return false
}

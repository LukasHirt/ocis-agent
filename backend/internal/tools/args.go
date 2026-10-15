package tools

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"path"
	"strings"
	"unicode/utf8"
)

const maxPathLen = 4096

// parse decodes the arguments strictly: a JSON object, no unknown fields (so
// no drive or user can be smuggled in), nothing after it.
func parse[T any](raw json.RawMessage) (T, error) {
	var v T
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return v, invalid("the arguments must be a JSON object")
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
		if r < 0x20 || r == 0x7f {
			return "", invalid("the path contains control characters")
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return "", invalid("the path must not contain . or .. segments")
		}
	}
	return path.Clean(p), nil
}

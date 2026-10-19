// Package textcap cuts untrusted text to byte caps at rune boundaries. The
// API views and the agent runner share it, so every capped text ends in the
// same marker.
package textcap

import (
	"strings"
	"unicode/utf8"
)

// Marker ends a text that Cap shortened.
const Marker = "…[truncated]"

// Cut returns s cut to at most n bytes at a rune boundary, without a marker.
func Cut(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// Cap makes s valid UTF-8 (invalid bytes become U+FFFD) and caps it at limit
// bytes including Marker; the result never exceeds limit. A limit smaller
// than the marker gives a plain Cut.
func Cap(s string, limit int) string {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= limit {
		return s
	}
	if limit < len(Marker) {
		return Cut(s, limit)
	}
	return Cut(s, limit-len(Marker)) + Marker
}

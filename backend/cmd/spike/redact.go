package main

import "strings"

const maxLogLen = 300

// redact makes a response body safe to print: every secret is replaced,
// whitespace is collapsed, and the result is truncated.
func redact(s string, secrets ...string) string {
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "<redacted>")
		}
	}
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxLogLen {
		s = s[:maxLogLen] + "..."
	}
	return s
}

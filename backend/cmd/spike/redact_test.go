package main

import (
	"strings"
	"testing"
)

func TestRedactReplacesEverySecretOccurrence(t *testing.T) {
	got := redact(`{"token":"s3cr3t","again":"s3cr3t"}`, "s3cr3t")
	if strings.Contains(got, "s3cr3t") {
		t.Fatalf("secret leaked: %s", got)
	}
	if strings.Count(got, "<redacted>") != 2 {
		t.Fatalf("got %s", got)
	}
}

func TestRedactIgnoresEmptySecret(t *testing.T) {
	got := redact("hello", "")
	if got != "hello" {
		t.Fatalf("an empty secret must not replace anything, got %q", got)
	}
}

func TestRedactTruncates(t *testing.T) {
	got := redact(strings.Repeat("a", 1000))
	if len(got) > 320 {
		t.Fatalf("not truncated: %d bytes", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("truncation must be visible: %q", got[len(got)-10:])
	}
}

func TestRedactCollapsesWhitespace(t *testing.T) {
	got := redact("a\n  b\t\tc")
	if got != "a b c" {
		t.Fatalf("got %q", got)
	}
}

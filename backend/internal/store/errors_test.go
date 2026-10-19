package store

import (
	"errors"
	"fmt"
	"testing"
)

func TestInvalidMatchesErrInvalidAndKeepsTheMessage(t *testing.T) {
	err := Invalid("unknown pause reason %q", "because")
	if !errors.Is(err, ErrInvalid) {
		t.Fatal("Invalid(...) does not match ErrInvalid")
	}
	if got, want := err.Error(), `store: unknown pause reason "because"`; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

func TestInvalidSurvivesWrapping(t *testing.T) {
	err := fmt.Errorf("put policy: %w", Invalid("bad"))
	if !errors.Is(err, ErrInvalid) {
		t.Fatal("a wrapped Invalid error no longer matches ErrInvalid")
	}
}

func TestSentinelsAreDistinct(t *testing.T) {
	all := []error{ErrNotFound, ErrFenceLost, ErrConflict, ErrInvalid, ErrExpired, ErrLimit}
	for i, a := range all {
		for j, b := range all {
			if i != j && errors.Is(a, b) {
				t.Errorf("%v matches %v", a, b)
			}
		}
	}
	if errors.Is(Invalid("x"), ErrConflict) || errors.Is(Invalid("x"), ErrNotFound) {
		t.Error("an Invalid error must not match the other sentinels")
	}
}

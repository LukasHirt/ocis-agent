package store

import (
	"errors"
	"fmt"
)

var (
	// ErrInvalid is matched by every error that reports invalid caller input
	// (a missing field, an unknown state, malformed JSON). An API layer maps
	// it to a client error.
	ErrInvalid = errors.New("store: invalid input")
	// ErrExpired is returned when an approval is decided after it expired.
	ErrExpired = errors.New("store: expired")
)

type invalidError struct{ msg string }

func (e *invalidError) Error() string { return "store: " + e.msg }

// Is makes errors.Is(err, ErrInvalid) true for every Invalid error.
func (e *invalidError) Is(target error) bool { return target == ErrInvalid }

// Invalid returns an error that matches ErrInvalid. Its text is "store: "
// followed by the formatted message.
func Invalid(format string, args ...any) error {
	return &invalidError{msg: fmt.Sprintf(format, args...)}
}

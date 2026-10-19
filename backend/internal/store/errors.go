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
	// ErrLimit is returned when a per-user cap is reached (for example 200
	// conversations). An API layer maps it to limit_reached.
	ErrLimit = errors.New("store: limit reached")
)

type invalidError struct{ msg string }

func (e *invalidError) Error() string { return "store: " + e.msg }

// Is makes errors.Is(err, ErrInvalid) true for every Invalid error.
func (e *invalidError) Is(target error) bool { return target == ErrInvalid }

// Invalid returns an error that matches ErrInvalid. Its text is "store: "
// followed by the formatted message. The format is printf-style: write %% for
// a literal percent sign; %w is not supported. For dynamic text use
// Invalid("%s", text) rather than passing the text as the format.
func Invalid(format string, args ...any) error {
	return &invalidError{msg: fmt.Sprintf(format, args...)}
}

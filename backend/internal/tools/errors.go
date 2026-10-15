package tools

import (
	"errors"
	"fmt"
)

// Sentinels. Their messages, and the messages of errors that match them, are
// safe to show to the model and the user.
var (
	// ErrInvalidArgs: the arguments are malformed or out of range.
	ErrInvalidArgs = errors.New("tools: invalid arguments")
	// ErrRefused: a policy or state refusal (the target exists, a reserved
	// path, a read budget used up). The model may read the reason.
	ErrRefused = errors.New("tools: refused")
	// ErrTargetChanged: an overwrite or delete target no longer matches what
	// the user approved. Nothing was changed.
	ErrTargetChanged = errors.New("tools: the target changed since it was approved")
)

type reasonError struct {
	kind error
	msg  string
}

func (e *reasonError) Error() string        { return e.msg }
func (e *reasonError) Is(target error) bool { return target == e.kind }

func invalid(format string, a ...any) error {
	return &reasonError{kind: ErrInvalidArgs, msg: "invalid arguments: " + fmt.Sprintf(format, a...)}
}

func refuse(format string, a ...any) error {
	return &reasonError{kind: ErrRefused, msg: fmt.Sprintf(format, a...)}
}

func changed(format string, a ...any) error {
	return &reasonError{kind: ErrTargetChanged, msg: fmt.Sprintf(format, a...)}
}

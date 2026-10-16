package llm

import (
	"errors"
	"fmt"
	"testing"
)

func TestRetryable(t *testing.T) {
	yes := []error{ErrRateLimited, ErrUnavailable, fmt.Errorf("%w: status 503", ErrUnavailable)}
	no := []error{ErrAuth, ErrRejected, ErrMalformed, ErrTooLarge, ErrInvalid, errors.New("other"), nil}
	for _, err := range yes {
		if !Retryable(err) {
			t.Errorf("Retryable(%v) = false", err)
		}
	}
	for _, err := range no {
		if Retryable(err) {
			t.Errorf("Retryable(%v) = true", err)
		}
	}
}

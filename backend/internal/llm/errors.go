package llm

import "errors"

// Errors never contain response bodies, URLs, keys or model output; wrap
// these sentinels with a status code or a constant only.
var (
	ErrInvalid     = errors.New("llm: invalid request")
	ErrAuth        = errors.New("llm: credentials rejected")
	ErrRateLimited = errors.New("llm: rate limited")
	ErrUnavailable = errors.New("llm: provider unavailable")
	ErrRejected    = errors.New("llm: request rejected")
	ErrMalformed   = errors.New("llm: malformed response")
	ErrTooLarge    = errors.New("llm: response too large")
)

// Retryable reports whether trying the same request again later may work.
func Retryable(err error) bool {
	return errors.Is(err, ErrRateLimited) || errors.Is(err, ErrUnavailable)
}

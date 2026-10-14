package store

import "github.com/oklog/ulid/v2"

// NewID returns a new ULID string. IDs created within the same millisecond
// still sort in creation order.
func NewID() string {
	return ulid.Make().String()
}

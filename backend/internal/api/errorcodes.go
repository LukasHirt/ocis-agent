package api

import "maps"

// ErrorCodes returns every error code of the API with its fixed message (a
// copy): the API reference is checked against it.
func ErrorCodes() map[string]string { return maps.Clone(errorMessages) }

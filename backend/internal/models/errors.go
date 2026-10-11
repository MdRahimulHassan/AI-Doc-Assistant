package models

import "errors"

// Sentinel errors: higher layers compare against these with errors.Is
// and never need to know anything about Postgres.
var (
	ErrNotFound   = errors.New("not found")
	ErrEmailTaken = errors.New("email already registered")
)
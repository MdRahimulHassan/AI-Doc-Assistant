package models

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

type User struct {
	ID           uuid.UUID `json:"id"`
	Email        string    `json:"email"`
	PasswordHash string    `json:"-"` // never serialized, even if someone returns a User directly
	CreatedAt    time.Time `json:"created_at"`
}

// NormalizeEmail is the single definition of "the same email".
// Every code path that stores or looks up an email goes through it.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}
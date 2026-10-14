package service

import "errors"

var (
	// ErrInvalidCredentials is returned for BOTH "no such user" and
	// "wrong password", so callers can't tell which emails exist.
	ErrInvalidCredentials = errors.New("invalid email or password")

	// ErrInvalidToken covers malformed, tampered, expired, and wrong-algorithm
	// tokens. The middleware doesn't need to know which.
	ErrInvalidToken = errors.New("invalid or expired token")
)

var (
	ErrEmptyFile           = errors.New("uploaded file is empty")
	ErrUnsupportedFileType = errors.New("unsupported file type")
	ErrBusy                = errors.New("ingestion is busy")
)

// ValidationError says which input field was wrong and why. The handler
// will turn it into response.ErrorWithDetails, so the React form can
// highlight the right input.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string { return e.Field + ": " + e.Message }
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/MdRahimulHassan/AI-Doc-Assistant/backend/internal/models"
)

const (
	DefaultBcryptCost = 12 // roughly a quarter second per hash on a typical laptop
	minPasswordChars  = 8
	maxPasswordBytes  = 72 // bcrypt silently ignores everything after this
	maxEmailLen       = 254
)

// UserRepository is what the service needs, nothing more.
type UserRepository interface {
	Create(ctx context.Context, email, passwordHash string) (*models.User, error)
	GetByEmail(ctx context.Context, email string) (*models.User, error)
	GetByID(ctx context.Context, id uuid.UUID) (*models.User, error)
}

type AuthService struct {
	users      UserRepository
	tokens     *TokenManager
	bcryptCost int
	dummyHash  []byte // compared against when the email doesn't exist
}

type LoginResult struct {
	User      *models.User
	Token     string
	ExpiresAt time.Time
}

func NewAuthService(users UserRepository, tokens *TokenManager, bcryptCost int) (*AuthService, error) {
	if bcryptCost < bcrypt.MinCost || bcryptCost > bcrypt.MaxCost {
		return nil, fmt.Errorf("bcrypt cost must be between %d and %d", bcrypt.MinCost, bcrypt.MaxCost)
	}

	// Made at the SAME cost as real hashes, so an unknown-email login
	// takes about as long as a wrong-password login.
	dummy, err := bcrypt.GenerateFromPassword([]byte("timing-equalizer"), bcryptCost)
	if err != nil {
		return nil, fmt.Errorf("create dummy hash: %w", err)
	}

	return &AuthService{users: users, tokens: tokens, bcryptCost: bcryptCost, dummyHash: dummy}, nil
}

// Register validates, hashes, stores. It returns the user WITHOUT the hash.
func (s *AuthService) Register(ctx context.Context, email, password string) (*models.User, error) {
	email = models.NormalizeEmail(email)

	if err := validateEmail(email); err != nil {
		return nil, err
	}
	if err := validatePassword(password); err != nil {
		return nil, err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), s.bcryptCost)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	user, err := s.users.Create(ctx, email, string(hash))
	if err != nil {
		// models.ErrEmailTaken stays detectable with errors.Is.
		return nil, fmt.Errorf("register: %w", err)
	}

	user.PasswordHash = "" // defense in depth, on top of json:"-"
	return user, nil
}

// Login checks the credentials and issues a token.
func (s *AuthService) Login(ctx context.Context, email, password string) (*LoginResult, error) {
	email = models.NormalizeEmail(email)

	// bcrypt truncates at 72 bytes, so without this check a 72-byte password
	// plus any extra characters would still be accepted.
	if len(password) > maxPasswordBytes {
		return nil, ErrInvalidCredentials
	}

	user, err := s.users.GetByEmail(ctx, email)
	hash := s.dummyHash
	switch {
	case err == nil:
		hash = []byte(user.PasswordHash)
	case errors.Is(err, models.ErrNotFound):
		user = nil // keep the dummy hash; the comparison below still runs
	default:
		return nil, fmt.Errorf("login: look up user: %w", err)
	}

	// Runs even when the user doesn't exist (timing equalization).
	cmpErr := bcrypt.CompareHashAndPassword(hash, []byte(password))
	if cmpErr != nil && !errors.Is(cmpErr, bcrypt.ErrMismatchedHashAndPassword) {
		slog.Error("unexpected bcrypt error", "error", cmpErr) // e.g. corrupt stored hash
	}
	if user == nil || cmpErr != nil {
		return nil, ErrInvalidCredentials
	}

	token, expiresAt, err := s.tokens.Generate(user.ID)
	if err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}

	user.PasswordHash = ""
	return &LoginResult{User: user, Token: token, ExpiresAt: expiresAt}, nil
}

// GetUser is what GET /api/me will call, with the ID taken from the token.
func (s *AuthService) GetUser(ctx context.Context, id uuid.UUID) (*models.User, error) {
	user, err := s.users.GetByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err) // ErrNotFound stays detectable
	}
	user.PasswordHash = ""
	return user, nil
}

func validateEmail(email string) error {
	bad := &ValidationError{Field: "email", Message: "a valid email address is required"}

	if email == "" || len(email) > maxEmailLen {
		return bad
	}
	// ParseAddress also accepts "Name <a@b.com>", so require that the parsed
	// address is exactly what was typed.
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email {
		return bad
	}
	if !strings.Contains(email[strings.LastIndex(email, "@")+1:], ".") {
		return bad // "user@localhost" is valid for the parser but not for signups
	}
	return nil
}

func validatePassword(password string) error {
	// Passwords are deliberately NOT trimmed: spaces are legitimate characters.
	if utf8.RuneCountInString(password) < minPasswordChars {
		return &ValidationError{Field: "password",
			Message: fmt.Sprintf("password must be at least %d characters", minPasswordChars)}
	}
	if len(password) > maxPasswordBytes { // bytes, not characters
		return &ValidationError{Field: "password",
			Message: fmt.Sprintf("password must be at most %d bytes", maxPasswordBytes)}
	}
	return nil
}
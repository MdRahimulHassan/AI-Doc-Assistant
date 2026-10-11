package service

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const minSecretLen = 32

// TokenManager is the ONE place JWTs are created and validated.
type TokenManager struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time // replaceable in tests, so expiry needs no sleeping
}

func NewTokenManager(secret string, ttl time.Duration) (*TokenManager, error) {
	// Config already checks this. Repeating it here means the type is
	// safe even if someone constructs it from somewhere else.
	if len(secret) < minSecretLen {
		return nil, fmt.Errorf("jwt secret must be at least %d characters", minSecretLen)
	}
	if ttl <= 0 {
		return nil, errors.New("jwt ttl must be positive")
	}
	return &TokenManager{secret: []byte(secret), ttl: ttl, now: time.Now}, nil
}

// Generate signs a token containing only sub, iat and exp.
// No email, no roles: the token proves identity, nothing more.
func (m *TokenManager) Generate(userID uuid.UUID) (string, time.Time, error) {
	now := m.now()
	expiresAt := now.Add(m.ttl)

	claims := jwt.RegisteredClaims{
		Subject:   userID.String(),
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(expiresAt),
	}

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(m.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign token: %w", err)
	}
	return signed, expiresAt, nil
}

// Parse validates the token and returns the user ID inside it.
// Every failure wraps ErrInvalidToken, so callers use errors.Is.
func (m *TokenManager) Parse(tokenString string) (uuid.UUID, error) {
	claims := &jwt.RegisteredClaims{}

	_, err := jwt.ParseWithClaims(tokenString, claims,
		func(t *jwt.Token) (any, error) {
			// The token's own header says which algorithm to use. Never
			// trust that blindly: this blocks "alg: none" and
			// algorithm-confusion attacks.
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
			}
			return m.secret, nil
		},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), // HS256 only
		jwt.WithTimeFunc(m.now),
	)
	if err != nil {
		// %w keeps errors.Is working; %v keeps the detail for server logs.
		return uuid.Nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}

	// A token without an expiry would be valid forever.
	if claims.ExpiresAt == nil {
		return uuid.Nil, fmt.Errorf("%w: missing exp claim", ErrInvalidToken)
	}

	id, err := uuid.Parse(claims.Subject)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: bad subject", ErrInvalidToken)
	}
	return id, nil
}
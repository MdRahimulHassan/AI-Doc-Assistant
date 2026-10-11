package service

import (
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

func TestTokenManager_Parse(t *testing.T) {
	id := uuid.New()
	tm, err := NewTokenManager(testSecret, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	validToken, _, err := tm.Generate(id)
	if err != nil {
		t.Fatal(err)
	}

	claims := jwt.RegisteredClaims{
		Subject:   id.String(),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}
	noneToken, _ := jwt.NewWithClaims(jwt.SigningMethodNone, claims).
		SignedString(jwt.UnsafeAllowNoneSignatureType)
	hs512Token, _ := jwt.NewWithClaims(jwt.SigningMethodHS512, claims).
		SignedString([]byte(testSecret))
	wrongSecretToken, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).
		SignedString([]byte("a-completely-different-secret-value!!"))

	tests := []struct {
		name    string
		token   string
		wantErr bool
	}{
		{"valid", validToken, false},
		{"alg none", noneToken, true},
		{"different HMAC algorithm (HS512)", hs512Token, true},
		{"signed with another secret", wrongSecretToken, true},
		{"garbage", "not.a.jwt", true},
		{"empty", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tm.Parse(tc.token)
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidToken) {
					t.Errorf("err = %v, want ErrInvalidToken", err)
				}
				return
			}
			if err != nil || got != id {
				t.Errorf("got (%v, %v), want (%v, nil)", got, err, id)
			}
		})
	}
}

func TestTokenManager_Expired(t *testing.T) {
	tm, _ := NewTokenManager(testSecret, time.Minute)
	token, _, err := tm.Generate(uuid.New())
	if err != nil {
		t.Fatal(err)
	}

	// Move the clock forward instead of sleeping.
	tm.now = func() time.Time { return time.Now().Add(2 * time.Minute) }

	if _, err := tm.Parse(token); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("err = %v, want ErrInvalidToken for an expired token", err)
	}
}
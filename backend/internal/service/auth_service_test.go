package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const testSecret = "0123456789abcdef0123456789abcdef" // 32 chars

func newTestService(t *testing.T) (*AuthService, *fakeUserRepo, *TokenManager) {
	t.Helper()
	repo := newFakeUserRepo()
	tm, err := NewTokenManager(testSecret, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewAuthService(repo, tm, bcrypt.MinCost) // MinCost keeps tests fast
	if err != nil {
		t.Fatal(err)
	}
	return svc, repo, tm
}

func TestRegisterThenLogin_ReturnsParseableToken(t *testing.T) {
	svc, repo, tm := newTestService(t)
	ctx := context.Background()
	const password = "correct-horse-battery"

	user, err := svc.Register(ctx, "  Alice@Example.COM ", password)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if user.PasswordHash != "" {
		t.Error("Register must not return the password hash")
	}
	if user.Email != "alice@example.com" {
		t.Errorf("email = %q, want normalized", user.Email)
	}

	// What was STORED must be a bcrypt hash, never the plaintext.
	stored := repo.byEmail["alice@example.com"]
	if stored.PasswordHash == password || !strings.HasPrefix(stored.PasswordHash, "$2") {
		t.Errorf("stored value does not look like a bcrypt hash: %q", stored.PasswordHash)
	}

	res, err := svc.Login(ctx, "ALICE@example.com", password)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if res.User.PasswordHash != "" {
		t.Error("Login must not return the password hash")
	}

	gotID, err := tm.Parse(res.Token)
	if err != nil {
		t.Fatalf("token is not parseable: %v", err)
	}
	if gotID != user.ID {
		t.Errorf("token subject = %v, want %v", gotID, user.ID)
	}
}
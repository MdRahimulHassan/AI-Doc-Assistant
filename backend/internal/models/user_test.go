package models

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestUser_JSONHidesPasswordHash(t *testing.T) {
	u := User{ID: uuid.New(), Email: "a@x.com", PasswordHash: "super-secret-hash"}

	b, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)

	if strings.Contains(out, "super-secret-hash") || strings.Contains(strings.ToLower(out), "password") {
		t.Errorf("password hash leaked into JSON: %s", out)
	}
}

func TestNormalizeEmail(t *testing.T) {
	tests := []struct{ in, want string }{
		{"a@x.com", "a@x.com"},
		{"  A@X.com  ", "a@x.com"},
		{"MiXeD@Example.COM", "mixed@example.com"},
	}
	for _, tc := range tests {
		if got := NormalizeEmail(tc.in); got != tc.want {
			t.Errorf("NormalizeEmail(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
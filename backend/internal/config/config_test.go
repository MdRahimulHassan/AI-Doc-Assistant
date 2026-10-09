package config

import "testing"

func setValidEnv(t *testing.T) {
	t.Helper()
	t.Setenv("JWT_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("DB_USER", "u")
	t.Setenv("DB_PASSWORD", "p")
	t.Setenv("DB_NAME", "n")
}

func TestLoad_Valid(t *testing.T) {
	setValidEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != "8080" {
		t.Errorf("default port = %q, want 8080", cfg.Port)
	}
}

func TestLoad_SecretValidation(t *testing.T) {
	tests := []struct {
		name   string
		secret string
	}{
		{"missing", ""},
		{"too short", "short"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv("JWT_SECRET", tc.secret)
			if _, err := Load(); err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}
}
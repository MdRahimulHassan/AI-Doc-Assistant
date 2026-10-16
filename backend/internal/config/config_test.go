package config

import (
	"testing"
	"path/filepath"
)


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

func TestLoad_UploadDefaults(t *testing.T) {
	setValidEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MaxUploadBytes != 10<<20 {
		t.Errorf("MaxUploadBytes = %d, want %d", cfg.MaxUploadBytes, 10<<20)
	}
	if cfg.IngestWorkers != 2 {
		t.Errorf("IngestWorkers = %d, want 2", cfg.IngestWorkers)
	}
	if !filepath.IsAbs(cfg.UploadDir) {
		t.Errorf("UploadDir = %q, want an absolute path", cfg.UploadDir)
	}
	if cfg.IngestQueueSize != 100 {
		t.Errorf("IngestQueueSize = %d, want 100", cfg.IngestQueueSize)
	}
}

func TestLoad_UploadCustomValues(t *testing.T) {
	setValidEnv(t)
	t.Setenv("MAX_UPLOAD_MB", "25")
	t.Setenv("INGEST_WORKERS", "4")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MaxUploadBytes != 25<<20 || cfg.IngestWorkers != 4 {
		t.Errorf("got %d bytes / %d workers, want %d / 4", cfg.MaxUploadBytes, cfg.IngestWorkers, 25<<20)
	}
}

func TestLoad_InvalidNumbers(t *testing.T) {
	tests := []struct {
		name, key, value string
	}{
		{"zero workers", "INGEST_WORKERS", "0"},
		{"negative workers", "INGEST_WORKERS", "-1"},
		{"too many workers", "INGEST_WORKERS", "999"},
		{"workers not a number", "INGEST_WORKERS", "two"},
		{"zero upload size", "MAX_UPLOAD_MB", "0"},
		{"negative upload size", "MAX_UPLOAD_MB", "-5"},
		{"absurd upload size", "MAX_UPLOAD_MB", "100000"},
		{"upload size not a number", "MAX_UPLOAD_MB", "ten"},
		{"zero queue size", "INGEST_QUEUE_SIZE", "0"},
		{"absurd queue size", "INGEST_QUEUE_SIZE", "100000"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv(tc.key, tc.value)

			if _, err := Load(); err == nil {
				t.Fatalf("expected an error for %s=%s, got nil", tc.key, tc.value)
			}
		})
	}
}
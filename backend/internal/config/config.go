package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"
	"path/filepath"
	"strconv"

	"github.com/joho/godotenv"
)

const minJWTSecretLen = 32

// Config holds all runtime settings. It is created once in main
// and passed explicitly to whatever needs it.
type Config struct {
	Port        string
	DatabaseURL string
	JWTSecret   string
	JWTLifetime time.Duration
	CORSOrigins []string

	// Documents (Week 2)
	UploadDir      string // absolute path
	MaxUploadBytes int64
	IngestWorkers  int
	IngestQueueSize int
}

// Load reads configuration from the environment (and a .env file if present).
// It collects ALL problems and returns them together, so you can fix
// everything in one go instead of one error per restart.
func Load() (*Config, error) {
	// Local development convenience. Errors are ignored on purpose:
	// in Docker/production there is no .env file and that's fine.
	// Called separately because Load stops at the first missing file.
	_ = godotenv.Load(".env")
	_ = godotenv.Load("../.env")

	var errs []error
	cfg := &Config{}

	// --- Server ---
	cfg.Port = getEnv("SERVER_PORT", "8080")

	// --- CORS ---
	cfg.CORSOrigins = splitCSV(getEnv("CORS_ORIGINS", "http://localhost:5173"))

	if len(cfg.CORSOrigins) == 0 {
		errs = append(errs, errors.New("CORS_ORIGINS must contain at least one origin"))
	}
	for _, o := range cfg.CORSOrigins {
		if o == "*" {
			errs = append(errs, errors.New(`CORS_ORIGINS must not contain "*": list explicit origins`))
		}
	}

	// --- JWT secret (required, minimum length) ---
	cfg.JWTSecret = os.Getenv("JWT_SECRET")
	switch {
	case cfg.JWTSecret == "":
		errs = append(errs, errors.New("JWT_SECRET is required"))
	case len(cfg.JWTSecret) < minJWTSecretLen:
		errs = append(errs, fmt.Errorf(
			"JWT_SECRET must be at least %d characters (got %d)",
			minJWTSecretLen, len(cfg.JWTSecret)))
	}

	// --- JWT lifetime ---
	ttlStr := getEnv("JWT_TTL", "24h")
	ttl, err := time.ParseDuration(ttlStr)
	if err != nil || ttl <= 0 {
		errs = append(errs, fmt.Errorf(
			"JWT_TTL %q is invalid: use a positive duration such as 15m or 24h", ttlStr))
	}
	cfg.JWTLifetime = ttl

	// --- Database URL ---
	// Either provide DATABASE_URL directly, or it is built from the DB_* parts.
	if dbURL := os.Getenv("DATABASE_URL"); dbURL != "" {
		cfg.DatabaseURL = dbURL
	} else {
		user := required("DB_USER", &errs)
		pass := required("DB_PASSWORD", &errs)
		name := required("DB_NAME", &errs)
		host := getEnv("DB_HOST", "localhost")
		port := getEnv("DB_PORT", "5432")

		if user != "" && pass != "" && name != "" {
			u := url.URL{
				Scheme:   "postgres",
				User:     url.UserPassword(user, pass), // escapes special characters
				Host:     net.JoinHostPort(host, port),
				Path:     "/" + name,
				RawQuery: "sslmode=disable",
			}
			cfg.DatabaseURL = u.String()
		}
	}

		// --- Uploads and ingestion ---
	uploadDir := getEnv("UPLOAD_DIR", "./storage/uploads")
	absDir, err := filepath.Abs(uploadDir)
	if err != nil {
		errs = append(errs, fmt.Errorf("UPLOAD_DIR %q is invalid: %w", uploadDir, err))
	}
	cfg.UploadDir = absDir

	maxMB := getInt("MAX_UPLOAD_MB", 10, 1, 100, &errs)
	cfg.MaxUploadBytes = int64(maxMB) << 20 // MB -> bytes

	cfg.IngestWorkers = getInt("INGEST_WORKERS", 2, 1, 32, &errs)
	cfg.IngestQueueSize = getInt("INGEST_QUEUE_SIZE", 100, 1, 10000, &errs)

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return cfg, nil
}

// getEnv returns the variable's value, or fallback if unset/empty.
func getEnv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// required returns the variable's value and records an error if it's missing.
func required(key string, errs *[]error) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		*errs = append(*errs, fmt.Errorf("%s is required", key))
	}
	return v
}

// getInt reads an integer variable. Unset means the default. A value that
// isn't a whole number, or falls outside [lo, hi], is recorded as an error.
func getInt(key string, def, lo, hi int, errs *[]error) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s must be a whole number, got %q", key, raw))
		return def
	}
	if n < lo || n > hi {
		*errs = append(*errs, fmt.Errorf("%s must be between %d and %d, got %d", key, lo, hi, n))
		return def
	}
	return n
}

// splitCSV turns "a, b,,c" into ["a", "b", "c"].
func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

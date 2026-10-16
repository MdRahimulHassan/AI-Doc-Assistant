package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/joho/godotenv"
)

const minJWTSecretLen = 32

// promptOverheadTokens is the room reserved for the system prompt, the
// question and any conversation history. It is used to check that ONE question
// can fit in the per-minute token budget at all.
const promptOverheadTokens = 500

// A model id such as "openai/gpt-oss-120b". The strict character set keeps
// anything surprising out of the JSON body and the logs.
var modelIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,99}$`)

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

	// AI features (Week 3)
	GroqAPIKey              string // optional: empty means the AI features are disabled
	GroqModel               string
	GroqSummaryModel        string
	GroqTPMBudget           int // tokens per minute this app may spend
	AskTopK                 int
	AskContextTokens        int
	AskMaxOutputTokens      int
	AskMaxQuestionChars     int
	LLMRatePerMinute        int
	LLMMaxConcurrentPerUser int
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

	// --- AI features (Groq) ---
	// The key is OPTIONAL: without it the server still starts and the AI
	// endpoints answer 503. An error about the key must never repeat the key.
	cfg.GroqAPIKey = strings.TrimSpace(os.Getenv("GROQ_API_KEY"))
	if cfg.GroqAPIKey != "" {
		if err := checkAPIKey(cfg.GroqAPIKey); err != nil {
			errs = append(errs, fmt.Errorf("GROQ_API_KEY is malformed: %w", err))
		}
	}

	cfg.GroqModel = getModel("GROQ_MODEL", "openai/gpt-oss-120b", &errs)
	cfg.GroqSummaryModel = getModel("GROQ_SUMMARY_MODEL", cfg.GroqModel, &errs) // defaults to GROQ_MODEL
	cfg.GroqTPMBudget = getInt("GROQ_TPM_BUDGET", 6000, 500, 5_000_000, &errs)
	cfg.AskTopK = getInt("ASK_TOP_K", 4, 1, 20, &errs)
	cfg.AskContextTokens = getInt("ASK_CONTEXT_TOKENS", 3000, 500, 100_000, &errs)
	cfg.AskMaxOutputTokens = getInt("ASK_MAX_OUTPUT_TOKENS", 1500, 100, 32_000, &errs)
	cfg.AskMaxQuestionChars = getInt("ASK_MAX_QUESTION_CHARS", 1000, 50, 10_000, &errs)
	cfg.LLMRatePerMinute = getInt("LLM_RATE_PER_MINUTE", 6, 1, 600, &errs)
	cfg.LLMMaxConcurrentPerUser = getInt("LLM_MAX_CONCURRENT_PER_USER", 2, 1, 20, &errs)

	// If one question can never fit in the per-minute budget, the pacer in
	// front of Groq would wait forever. Catch that at startup instead.
	if need := cfg.AskContextTokens + cfg.AskMaxOutputTokens + promptOverheadTokens; need > cfg.GroqTPMBudget {
		errs = append(errs, fmt.Errorf(
			"one question needs about %d tokens (ASK_CONTEXT_TOKENS %d + ASK_MAX_OUTPUT_TOKENS %d + %d for the prompt) "+
				"but GROQ_TPM_BUDGET is only %d: lower ASK_CONTEXT_TOKENS or raise GROQ_TPM_BUDGET",
			need, cfg.AskContextTokens, cfg.AskMaxOutputTokens, promptOverheadTokens, cfg.GroqTPMBudget))
	}

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

// LLMEnabled reports whether the AI features can be used.
func (c Config) LLMEnabled() bool { return c.GroqAPIKey != "" }

// String and GoString make printing a Config safe: %v, %+v, %s and %#v all
// give this summary, which contains no secret (no JWT secret, database
// password or API key). A Config must never be logged any other way.
func (c Config) String() string {
	llm := "disabled"
	if c.LLMEnabled() {
		llm = "enabled (" + c.GroqModel + ")"
	}
	return fmt.Sprintf("Config{port=%s upload_dir=%s max_upload=%dB ingest_workers=%d queue=%d cors=%v jwt_ttl=%s llm=%s}",
		c.Port, c.UploadDir, c.MaxUploadBytes, c.IngestWorkers, c.IngestQueueSize, c.CORSOrigins, c.JWTLifetime, llm)
}

func (c Config) GoString() string { return c.String() }

// getModel reads a model id and rejects anything that is not a plain id.
func getModel(key, def string, errs *[]error) string {
	v := getEnv(key, def)
	if !modelIDPattern.MatchString(v) {
		*errs = append(*errs, fmt.Errorf("%s %q is not a valid model id (letters, digits and . _ : / - only)", key, v))
		return def
	}
	return v
}

// checkAPIKey catches the usual copy-paste mistakes WITHOUT echoing the key:
// quotes, spaces, line breaks, non-ASCII. The key's format itself is not
// checked, because providers change it.
func checkAPIKey(key string) error {
	if len(key) < 20 {
		return errors.New("it is too short to be a real key")
	}
	bad := func(r rune) bool {
		return r > unicode.MaxASCII || unicode.IsSpace(r) || unicode.IsControl(r) || r == '"' || r == '\'' || r == '`'
	}
	if strings.IndexFunc(key, bad) >= 0 {
		return errors.New("it contains spaces, quotes or other unexpected characters (remove any quotes around the value)")
	}
	return nil
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

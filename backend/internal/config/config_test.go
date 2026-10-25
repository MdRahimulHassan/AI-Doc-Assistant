package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func setValidEnv(t *testing.T) {
	t.Helper()
	t.Setenv("JWT_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("DB_USER", "u")
	t.Setenv("DB_PASSWORD", "p")
	t.Setenv("DB_NAME", "n")
	clearLLMEnv(t)
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

// clearLLMEnv blanks every AI setting, so values left in the developer's shell
// (for example from curl experiments) cannot change test results.
func clearLLMEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"GROQ_API_KEY", "GROQ_MODEL", "GROQ_SUMMARY_MODEL", "GROQ_TPM_BUDGET",
		"ASK_TOP_K", "ASK_CONTEXT_TOKENS", "ASK_MAX_OUTPUT_TOKENS", "ASK_MAX_QUESTION_CHARS",
		"LLM_RATE_PER_MINUTE", "LLM_MAX_CONCURRENT_PER_USER","SUMMARY_GROUP_TOKENS", 
		"SUMMARY_MAX_OUTPUT_TOKENS", "SUMMARY_MAX_CHUNKS", "SUMMARY_WORKERS",
	} {
		t.Setenv(k, "")
	}
}

func TestLoad_LLMDefaults(t *testing.T) {
	setValidEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.LLMEnabled() {
		t.Error("without GROQ_API_KEY the AI features must be disabled, and that must not be an error")
	}
	if cfg.GroqModel != "openai/gpt-oss-120b" || cfg.GroqSummaryModel != cfg.GroqModel {
		t.Errorf("models = %q and %q", cfg.GroqModel, cfg.GroqSummaryModel)
	}
	got := []int{cfg.GroqTPMBudget, cfg.AskTopK, cfg.AskContextTokens, cfg.AskMaxOutputTokens,
		cfg.AskMaxQuestionChars, cfg.LLMRatePerMinute, cfg.LLMMaxConcurrentPerUser}
	want := []int{6000, 4, 3000, 1500, 1000, 6, 2}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("default #%d = %d, want %d (all: %v)", i, got[i], want[i], got)
		}
	}
}

func TestLoad_GroqAPIKey(t *testing.T) {
	good := "gsk_" + strings.Repeat("a", 40)

	tests := []struct {
		name    string
		key     string
		enabled bool
		wantErr bool
	}{
		{"missing", "", false, false},
		{"valid", good, true, false},
		{"surrounding spaces are trimmed", "  " + good + "  ", true, false},
		{"wrapped in double quotes", `"` + good + `"`, false, true},
		{"wrapped in single quotes", "'" + good + "'", false, true},
		{"contains a space", "gsk_ " + strings.Repeat("a", 40), false, true},
		{"too short", "gsk_abc", false, true},
		{"non-ASCII", "gsk_" + strings.Repeat("é", 40), false, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv("GROQ_API_KEY", tc.key)

			cfg, err := Load()
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				if secret := strings.Trim(tc.key, ` "'`); secret != "" && strings.Contains(err.Error(), secret) {
					t.Errorf("the error message repeats the key: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.LLMEnabled() != tc.enabled {
				t.Errorf("LLMEnabled = %v, want %v", cfg.LLMEnabled(), tc.enabled)
			}
		})
	}
}

func TestLoad_LLMInvalidValues(t *testing.T) {
	tests := []struct{ name, key, value string }{
		{"model with a space", "GROQ_MODEL", "gpt oss"},
		{"model with a quote", "GROQ_MODEL", `gpt"oss`},
		{"summary model that looks like a path", "GROQ_SUMMARY_MODEL", "../etc"},
		{"zero top-k", "ASK_TOP_K", "0"},
		{"huge top-k", "ASK_TOP_K", "999"},
		{"budget not a number", "GROQ_TPM_BUDGET", "lots"},
		{"tiny budget", "GROQ_TPM_BUDGET", "10"},
		{"zero question length", "ASK_MAX_QUESTION_CHARS", "0"},
		{"zero rate", "LLM_RATE_PER_MINUTE", "0"},
		{"negative concurrency", "LLM_MAX_CONCURRENT_PER_USER", "-1"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv(tc.key, tc.value)

			if _, err := Load(); err == nil {
				t.Fatalf("expected an error for %s=%s", tc.key, tc.value)
			}
		})
	}
}

func TestLoad_BudgetMustFitOneQuestion(t *testing.T) {
	setValidEnv(t)
	t.Setenv("ASK_CONTEXT_TOKENS", "5500") // 5500 + 1500 + 500 is more than the default budget of 6000

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "GROQ_TPM_BUDGET") {
		t.Fatalf("err = %v, want an error that names GROQ_TPM_BUDGET", err)
	}

	t.Setenv("GROQ_TPM_BUDGET", "10000") // the same settings are fine with a bigger budget
	if _, err := Load(); err != nil {
		t.Errorf("unexpected error with a larger budget: %v", err)
	}
}

func TestLoad_SummaryModel(t *testing.T) {
	setValidEnv(t)
	t.Setenv("GROQ_MODEL", "openai/gpt-oss-20b")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GroqSummaryModel != "openai/gpt-oss-20b" {
		t.Errorf("summary model = %q, want it to default to GROQ_MODEL", cfg.GroqSummaryModel)
	}

	t.Setenv("GROQ_SUMMARY_MODEL", "openai/gpt-oss-120b")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GroqSummaryModel != "openai/gpt-oss-120b" || cfg.GroqModel != "openai/gpt-oss-20b" {
		t.Errorf("models = %q and %q", cfg.GroqModel, cfg.GroqSummaryModel)
	}
}

func TestConfig_PrintingNeverLeaksSecrets(t *testing.T) {
	setValidEnv(t)
	key := "gsk_" + strings.Repeat("k", 40)
	t.Setenv("GROQ_API_KEY", key)
	t.Setenv("DB_PASSWORD", "s3cr3t-db-password")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}

	out := fmt.Sprintf("%v | %+v | %#v | %s | %v", cfg, cfg, cfg, *cfg, *cfg)
	for _, secret := range []string{key, "s3cr3t-db-password", "0123456789abcdef0123456789abcdef"} {
		if strings.Contains(out, secret) {
			t.Errorf("printing a Config leaked %q: %s", secret, out)
		}
	}
}

func TestLoad_SummaryDefaults(t *testing.T) {
	setValidEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := []int{cfg.SummaryGroupTokens, cfg.SummaryMaxOutputTokens, cfg.SummaryMaxChunks, cfg.SummaryWorkers}
	want := []int{2500, 800, 120, 1}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("default #%d = %d, want %d (all: %v)", i, got[i], want[i], got)
		}
	}
}

func TestLoad_SummaryInvalidValues(t *testing.T) {
	tests := []struct{ name, key, value string }{
		{"zero group size", "SUMMARY_GROUP_TOKENS", "0"},
		{"tiny group size", "SUMMARY_GROUP_TOKENS", "100"},
		{"zero output limit", "SUMMARY_MAX_OUTPUT_TOKENS", "0"},
		{"zero chunk cap", "SUMMARY_MAX_CHUNKS", "0"},
		{"absurd chunk cap", "SUMMARY_MAX_CHUNKS", "100000"},
		{"zero workers", "SUMMARY_WORKERS", "0"},
		{"too many workers", "SUMMARY_WORKERS", "99"},
		{"not a number", "SUMMARY_WORKERS", "two"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv(tc.key, tc.value)
			if _, err := Load(); err == nil {
				t.Fatalf("expected an error for %s=%s", tc.key, tc.value)
			}
		})
	}
}

func TestLoad_SummaryGroupMustHoldTwoSummaries(t *testing.T) {
	setValidEnv(t)
	t.Setenv("SUMMARY_GROUP_TOKENS", "1000") // 2 * (800 + 12) = 1624 is needed

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "SUMMARY_GROUP_TOKENS") {
		t.Fatalf("err = %v, want an error that names SUMMARY_GROUP_TOKENS", err)
	}
}

func TestLoad_SummaryCallMustFitTheBudget(t *testing.T) {
	setValidEnv(t)
	t.Setenv("GROQ_TPM_BUDGET", "10000")
	t.Setenv("SUMMARY_GROUP_TOKENS", "9000") // 9000 + 350 + 1600 is more than 10000

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "GROQ_TPM_BUDGET") {
		t.Fatalf("err = %v, want an error that names GROQ_TPM_BUDGET", err)
	}
}
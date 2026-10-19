package llm

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"2", 2 * time.Second},
		{" 7 ", 7 * time.Second},
		{"0.5", 500 * time.Millisecond},
		{"0", 0},
		{"-3", 0},
		{"abc", 0},
		{"NaN", 0},
		{"Inf", 24 * time.Hour},
		{"1e9", 24 * time.Hour},
		{now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0},
	}
	for _, tc := range tests {
		if got := parseRetryAfter(tc.in, now); got != tc.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestBackoffCeiling(t *testing.T) {
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second}
	for i, w := range want {
		if got := backoffCeiling(500*time.Millisecond, 8*time.Second, i+1); got != w {
			t.Errorf("attempt %d: ceiling %v, want %v", i+1, got, w)
		}
	}
}

func TestFullJitterStaysInRange(t *testing.T) {
	for i := 0; i < 2000; i++ {
		if d := fullJitter(time.Second); d < 0 || d > time.Second {
			t.Fatalf("jitter %v is outside [0, 1s]", d)
		}
	}
	if d := fullJitter(0); d != 0 {
		t.Errorf("jitter(0) = %v, want 0", d)
	}
}

func TestSleepContext(t *testing.T) {
	if err := sleepContext(context.Background(), 0); err != nil {
		t.Errorf("zero sleep: %v", err)
	}
	if err := sleepContext(context.Background(), 5*time.Millisecond); err != nil {
		t.Errorf("short sleep: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel)
	start := time.Now()
	if err := sleepContext(ctx, time.Minute); err != context.Canceled {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("a cancelled sleep must return promptly")
	}
}
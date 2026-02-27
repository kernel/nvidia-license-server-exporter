package main

import (
	"os"
	"testing"
	"time"
)

func TestBoolFromEnv(t *testing.T) {
	key := "TEST_BOOL_FROM_ENV"
	_ = os.Unsetenv(key)
	if got := boolFromEnv(key, true); !got {
		t.Fatalf("expected fallback true when env missing")
	}

	t.Setenv(key, "false")
	if got := boolFromEnv(key, true); got {
		t.Fatalf("expected parsed false")
	}
}

func TestDurationFromEnv(t *testing.T) {
	key := "TEST_DURATION_FROM_ENV"
	_ = os.Unsetenv(key)

	if got := durationFromEnv(key, 7*time.Second); got != 7*time.Second {
		t.Fatalf("expected fallback duration, got %s", got)
	}

	t.Setenv(key, "90s")
	if got := durationFromEnv(key, 7*time.Second); got != 90*time.Second {
		t.Fatalf("expected parsed duration 90s, got %s", got)
	}
}

func TestFirstNonEmpty(t *testing.T) {
	got := firstNonEmpty("", "  ", "value", "x")
	if got != "value" {
		t.Fatalf("expected first non-empty value, got %q", got)
	}
}

func TestDefaultListenAddress(t *testing.T) {
	t.Run("uses LISTEN_ADDRESS when set", func(t *testing.T) {
		t.Setenv("LISTEN_ADDRESS", ":7777")
		t.Setenv("PORT", "9999")
		if got := defaultListenAddress(); got != ":7777" {
			t.Fatalf("expected LISTEN_ADDRESS to win, got %q", got)
		}
	})

	t.Run("uses PORT when LISTEN_ADDRESS missing", func(t *testing.T) {
		t.Setenv("LISTEN_ADDRESS", "")
		t.Setenv("PORT", "8080")
		if got := defaultListenAddress(); got != ":8080" {
			t.Fatalf("expected :8080, got %q", got)
		}
	})

	t.Run("accepts PORT with leading colon", func(t *testing.T) {
		t.Setenv("LISTEN_ADDRESS", "")
		t.Setenv("PORT", ":9090")
		if got := defaultListenAddress(); got != ":9090" {
			t.Fatalf("expected :9090, got %q", got)
		}
	})

	t.Run("falls back to default", func(t *testing.T) {
		t.Setenv("LISTEN_ADDRESS", "")
		t.Setenv("PORT", "")
		if got := defaultListenAddress(); got != ":9844" {
			t.Fatalf("expected default :9844, got %q", got)
		}
	})
}

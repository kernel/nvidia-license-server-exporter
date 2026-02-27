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

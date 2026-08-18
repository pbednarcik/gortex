package lsp

import (
	"testing"
	"time"
)

func TestIdleTimeoutFromEnv(t *testing.T) {
	const fallback = 10 * time.Minute

	t.Setenv(IdleTTLEnv, "")
	if got := IdleTimeoutFromEnv(fallback); got != fallback {
		t.Fatalf("unset: got %v, want fallback %v", got, fallback)
	}
	t.Setenv(IdleTTLEnv, "45m")
	if got := IdleTimeoutFromEnv(fallback); got != 45*time.Minute {
		t.Fatalf("45m: got %v", got)
	}
	// Zero (and negative, clamped to zero) disables the reaper — the
	// router's idleTimeout <= 0 no-op.
	t.Setenv(IdleTTLEnv, "0")
	if got := IdleTimeoutFromEnv(fallback); got != 0 {
		t.Fatalf("0: got %v, want 0", got)
	}
	t.Setenv(IdleTTLEnv, "-5m")
	if got := IdleTimeoutFromEnv(fallback); got != 0 {
		t.Fatalf("-5m: got %v, want 0", got)
	}
	t.Setenv(IdleTTLEnv, "not-a-duration")
	if got := IdleTimeoutFromEnv(fallback); got != fallback {
		t.Fatalf("unparseable: got %v, want fallback %v", got, fallback)
	}
}

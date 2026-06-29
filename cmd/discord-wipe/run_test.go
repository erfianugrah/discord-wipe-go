package main

import (
	"testing"
	"time"
)

// TestRunRetentionNotClobberedByPurgeDefault is the regression test for the
// v1.0.0 "deletes everything" bug.
//
// `run` and `purge` both bind the package-global retentionDays. Go runs init()
// in filename order, so search.go (purge, default 0) registers AFTER run.go
// (default 14) and clobbers the shared variable to 0. pflag does not reset an
// unset flag during Parse, so the run loop computed cutoff = now - 0 = now and
// deleted EVERY message regardless of the 14-day retention window. The fix is
// the resolveFloat guard in cmdRun.Run, which re-derives the value from env /
// the command's own default instead of trusting the clobbered global.
func TestRunRetentionNotClobberedByPurgeDefault(t *testing.T) {
	t.Setenv("RETENTION_DAYS", "14")
	// Simulate purge's init() having clobbered the shared global to 0.
	retentionDays = 0
	got := resolveFloat(cmdRun, "retention-days", "RETENTION_DAYS", 14)
	if got != 14 {
		t.Fatalf("run retention resolved to %v, want 14 (regression: purge's default 0 leaked into run)", got)
	}
}

func TestResolveFloatFallsBackToEnvThenDefault(t *testing.T) {
	// Empty env => default.
	t.Setenv("DELETE_DELAY", "")
	if got := resolveFloat(cmdRun, "delete-delay", "DELETE_DELAY", 1.0); got != 1.0 {
		t.Fatalf("empty env: got %v, want default 1.0", got)
	}
	// Set env => env wins (flag not changed).
	t.Setenv("DELETE_DELAY", "2.5")
	if got := resolveFloat(cmdRun, "delete-delay", "DELETE_DELAY", 1.0); got != 2.5 {
		t.Fatalf("env set: got %v, want 2.5", got)
	}
}

// TestPurgeRetentionIgnoresEnv documents that purge's retention is intentionally
// env-independent: a stray RETENTION_DAYS must not narrow a purge to a 14-day
// window. With an empty envKey, resolveFloat returns the supplied default (0).
func TestPurgeRetentionIgnoresEnv(t *testing.T) {
	t.Setenv("RETENTION_DAYS", "14")
	if got := resolveFloat(cmdPurge, "retention-days", "", 0); got != 0 {
		t.Fatalf("purge retention resolved to %v, want 0 (must ignore RETENTION_DAYS env)", got)
	}
}

func TestRetentionCutoff(t *testing.T) {
	now := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	if d := now.Sub(retentionCutoff(now, 14)); d != 14*24*time.Hour {
		t.Fatalf("14d cutoff delta = %s, want 336h", d)
	}
	if d := now.Sub(retentionCutoff(now, 0)); d != 0 {
		t.Fatalf("0d cutoff delta = %s, want 0 (delete-everything)", d)
	}
	if d := now.Sub(retentionCutoff(now, 7)); d != 7*24*time.Hour {
		t.Fatalf("7d cutoff delta = %s, want 168h", d)
	}
}

package ai

import (
	"os"
	"testing"
)

// The runaway-loop guard's budget must be configurable per-deployment. The
// original hard-coded cap of 60 was hit by a legitimately autonomous turn on
// 2026-10-14 (60 productive iterations: screenshots, git history rewrite, PR
// prep — still mid-task when the guard halted it "without reason").
func TestMaxLoopIterationsFromEnv(t *testing.T) {
	old := os.Getenv("MAX_LOOP_ITERATIONS")
	defer os.Setenv("MAX_LOOP_ITERATIONS", old)

	cases := []struct {
		name string
		env  string
		want int
	}{
		{"unset uses default", "", defaultMaxLoopIterations},
		{"explicit override", "300", 300},
		{"non-numeric falls back", "abc", defaultMaxLoopIterations},
		{"zero falls back", "0", defaultMaxLoopIterations},
		{"negative falls back", "-5", defaultMaxLoopIterations},
		{"whitespace-ish malformed falls back", " 10", defaultMaxLoopIterations},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env == "" {
				os.Unsetenv("MAX_LOOP_ITERATIONS")
			} else {
				os.Setenv("MAX_LOOP_ITERATIONS", tc.env)
			}
			if got := maxLoopIterationsFromEnv(); got != tc.want {
				t.Fatalf("maxLoopIterationsFromEnv() = %d, want %d (env=%q)", got, tc.want, tc.env)
			}
		})
	}
}

// The zero-progress stall guard's limit must be configurable per-deployment
// (mirrors MAX_LOOP_ITERATIONS), and must always fall back to a sane default
// on malformed input so a bad env var can never disable the guard entirely
// (a value of 0 or negative would make the loop halt instantly — the guard
// must stay protective AND conservative).
func TestProgressStallLimitFromEnv(t *testing.T) {
	old := os.Getenv("LOOP_PROGRESS_STALL_LIMIT")
	defer os.Setenv("LOOP_PROGRESS_STALL_LIMIT", old)

	cases := []struct {
		name string
		env  string
		want int
	}{
		{"unset uses default", "", defaultProgressStallLimit},
		{"explicit override", "10", 10},
		{"non-numeric falls back", "abc", defaultProgressStallLimit},
		{"zero falls back", "0", defaultProgressStallLimit},
		{"negative falls back", "-5", defaultProgressStallLimit},
		{"whitespace-ish malformed falls back", " 10", defaultProgressStallLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env == "" {
				os.Unsetenv("LOOP_PROGRESS_STALL_LIMIT")
			} else {
				os.Setenv("LOOP_PROGRESS_STALL_LIMIT", tc.env)
			}
			if got := progressStallLimitFromEnv(); got != tc.want {
				t.Fatalf("progressStallLimitFromEnv() = %d, want %d (env=%q)", got, tc.want, tc.env)
			}
		})
	}
}
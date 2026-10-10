package sandbox

import (
	"testing"
	"time"
)

// ONE DEADLINE: the instant a parked run's question stops keeping its place is
// exactly the instant the reaper reclaims its box. A person told "your answer
// keeps its place until 18:40" and a reaper that reclaims at 18:30 would be
// two answers to one question; [PendingRun.HeldUntil] is both.
func TestHeldUntilIsTheReapersDeadline(t *testing.T) {
	t.Parallel()
	paused := time.Date(2031, 4, 16, 10, 0, 0, 0, time.UTC)
	parked := paused.Add(3 * time.Second)
	base := PendingRun{TurnID: "t", SandboxID: "box-1", Status: StatusAwaiting,
		PauseTTLSeconds: 1800, PausedAt: paused, UpdatedAt: parked}
	cases := []struct {
		name string
		edit func(*PendingRun)
		want time.Time
	}{
		{"a stamped pause is counted from its stamp", func(*PendingRun) {}, paused.Add(30 * time.Minute)},
		{"an unstamped park is counted from the park", func(r *PendingRun) { r.PausedAt = time.Time{} },
			parked.Add(30 * time.Minute)},
		{"an answered run's box waits on its resume too", func(r *PendingRun) { r.Status = StatusAnswered },
			paused.Add(30 * time.Minute)},
		{"a fractional TTL is not rounded", func(r *PendingRun) { r.PauseTTLSeconds = 1.5 },
			paused.Add(1500 * time.Millisecond)},
		{"a re-seeded run holds no box", func(r *PendingRun) {
			r.Status, r.SandboxID, r.PausedAt = StatusReseed, "", time.Time{}
		}, time.Time{}},
		{"a zero TTL holds nothing", func(r *PendingRun) { r.PauseTTLSeconds = 0 }, time.Time{}},
		{"a run the engine is driving has no deadline", func(r *PendingRun) { r.Status = StatusRunning }, time.Time{}},
		{"no box, no deadline", func(r *PendingRun) { r.SandboxID = "" }, time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := base
			tc.edit(&run)
			until, held := run.HeldUntil()
			if held != !tc.want.IsZero() || !until.Equal(tc.want) {
				t.Fatalf("HeldUntil = %v, %v; want %v", until, held, tc.want)
			}
			if !held {
				if pauseExpired(run, paused.Add(1000*time.Hour)) {
					t.Error("the reaper reclaims a box that keeps no place")
				}
				return
			}
			if pauseExpired(run, until.Add(-time.Nanosecond)) {
				t.Error("the reaper reclaims a box before the instant it is said to be held until")
			}
			if !pauseExpired(run, until) {
				t.Error("the reaper leaves a box past the instant it is said to be held until")
			}
		})
	}
}

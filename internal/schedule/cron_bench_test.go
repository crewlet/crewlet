package schedule_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/schedule"
)

// BenchmarkNextUnreachable measures the WORST case the linear scan can be
// asked for: an expression that never matches, so the walk runs the whole
// horizon before reporting not-found.
//
// It exists to keep the cost of [schedule.Horizon] an observed number rather
// than an argument. Every reachable expression terminates at its next fire —
// a daily cron costs at most a day of minutes — so this is the only shape
// that pays the full walk, and it is reached only by an impossible date.
func BenchmarkNextUnreachable(b *testing.B) {
	e, err := schedule.Parse("0 0 30 2 *") // February 30th
	if err != nil {
		b.Fatal(err)
	}
	loc, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		b.Fatal(err)
	}
	start := time.Date(2026, time.June, 8, 0, 0, 0, 0, time.UTC)
	b.ResetTimer()
	for range b.N {
		if _, ok := e.Next(start, loc); ok {
			b.Fatal("expected not found")
		}
	}
}

// BenchmarkNextDaily is the shape an operator actually runs: one fire a day,
// found within a day of minutes.
func BenchmarkNextDaily(b *testing.B) {
	e, err := schedule.Parse("30 9 * * 1-5")
	if err != nil {
		b.Fatal(err)
	}
	loc, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		b.Fatal(err)
	}
	start := time.Date(2026, time.June, 8, 9, 31, 0, 0, time.UTC)
	b.ResetTimer()
	for range b.N {
		if _, ok := e.Next(start, loc); !ok {
			b.Fatal("expected a fire")
		}
	}
}

package builtin_test

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE WORK TOOLS CUT THEIR DAY ON THE COMPANY'S CLOCK, AS IT IS AT THE CALL
// (ADR-0018).
//
// 23:30 on 23 September in Los Angeles is 06:30 on the 24th in UTC, so a tool
// that cut "today" on UTC answered about the wrong date for the last seven
// hours of every Los Angeles day. And the clock is READ AT THE CALL rather
// than captured when the tools were registered: the operator's surface is
// built once at startup, so a captured zone answered every assistant on the
// clock the company booted with — which, before that surface was given one at
// all, was UTC.
func TestTheWorkToolsCutTheirDayOnTheCompanysClockAtTheCall(t *testing.T) {
	t.Parallel()
	losAngeles, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	var clock atomic.Pointer[time.Location]
	clock.Store(losAngeles)

	now := time.Date(2026, time.September, 24, 6, 30, 0, 0, time.UTC)
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{
		Reader: trk, Writer: trk.as,
		Now:  func() time.Time { return now },
		Zone: clock.Load,
	})

	if res := callWork(t, reg, tracker.ListWorkItemsTool, map[string]any{"due": "overdue"}); res.Failed {
		t.Fatalf("list_work_items: %s", res.Output)
	}
	// Midnight on the 23rd in Los Angeles (PDT, UTC-7), which is where
	// `overdue` begins and what every row's overdue mark is cut on.
	if want := time.Date(2026, time.September, 23, 7, 0, 0, 0, time.UTC); !trk.query.DayStart.Equal(want) {
		t.Errorf("list_work_items cut today at %v, want the company's midnight %v",
			trk.query.DayStart, want)
	}
	callWork(t, reg, tracker.MyWorkTool, nil)
	if trk.myWorkZone != losAngeles {
		t.Errorf("my_work cut its day on %v, want the company's clock %v",
			trk.myWorkZone, losAngeles)
	}

	// AN APPLY MOVES THE CLOCK, and the same registered tools follow it.
	clock.Store(berlin)
	if res := callWork(t, reg, tracker.ListWorkItemsTool, map[string]any{"due": "overdue"}); res.Failed {
		t.Fatalf("list_work_items: %s", res.Output)
	}
	// Midnight on the 24th in Berlin (CEST, UTC+2).
	if want := time.Date(2026, time.September, 23, 22, 0, 0, 0, time.UTC); !trk.query.DayStart.Equal(want) {
		t.Errorf("after the clock moved, list_work_items cut today at %v, want %v",
			trk.query.DayStart, want)
	}
	callWork(t, reg, tracker.MyWorkTool, nil)
	if trk.myWorkZone != berlin {
		t.Errorf("after the clock moved, my_work cut its day on %v, want %v",
			trk.myWorkZone, berlin)
	}
}

// NO CLOCK IS UTC, never a crash: a surface wired without one is the default
// company, whose clock is UTC.
func TestAWorkSurfaceWithNoClockCutsOnUTC(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 24, 6, 30, 0, 0, time.UTC)
	for name, zone := range map[string]func() *time.Location{
		"no seam":              nil,
		"a seam answering nil": func() *time.Location { return nil },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			trk := newFakeTracker()
			reg := workRegistry(t, builtin.WorkDeps{
				Reader: trk, Writer: trk.as,
				Now:  func() time.Time { return now },
				Zone: zone,
			})
			callWork(t, reg, tracker.MyWorkTool, nil)
			if trk.myWorkZone != time.UTC {
				t.Errorf("my_work cut its day on %v, want UTC", trk.myWorkZone)
			}
		})
	}
}

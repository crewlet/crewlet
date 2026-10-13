package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// raised is the kinds a reading raises.
func raised(reading statelog.Reading) map[statelog.Kind]string {
	out := map[statelog.Kind]string{}
	for _, a := range statelog.Evaluate(reading) {
		out[a.Kind] = a.Detail
	}
	return out
}

// THE DEFERRAL ALARM AGES THE OLDEST HELD RECORD, measured by the series that
// knows when it arrived.
//
// The reading used to set the age to EXACTLY the grace whenever any record was
// deferred, and the rule fires strictly above the grace — so `deferred_old`
// could never fire, and its detail named thirty minutes for a record held thirty
// seconds or a day. A node past the grace has already taken its copy of that log
// out of service, and the alarm is what tells an operator why.
func TestTheDeferralAlarmAgesTheOldestHeldRecord(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		held  time.Duration
		fires bool
	}{
		{held: 31 * time.Minute, fires: true},
		{held: 29 * time.Minute, fires: false},
	} {
		running := &runningLog{domain: tracker.Domain{}}
		running.progress.observe(now.Add(-tc.held), 10, false, true)
		r := &retention{state: &stateLog{logs: newLogSet([]*runningLog{running})},
			fleet: coordmem.NewFleet(), nodeID: "node-a"}
		healths := map[string]domainHealth{
			trackerName: {health: statelog.Health{Deferred: 1}},
		}
		reading := r.reading(t.Context(), now, coord.BackupPoint{At: now}, true, healths, nil)
		if reading.DeferredAge != tc.held {
			t.Fatalf("a record held %s is read as %s old", tc.held, reading.DeferredAge)
		}
		detail, fired := raised(reading)[statelog.KindDeferredOld]
		if fired != tc.fires {
			t.Fatalf("a record held %s: deferred_old fired = %v, want %v", tc.held, fired, tc.fires)
		}
		if fired && !strings.Contains(detail, "31m") {
			t.Fatalf("deferred_old says %q, want the age it measured", detail)
		}
	}
}

// THE FLOOR ALARM AGES THE FLOOR'S UNREADABILITY, from this node's own reads of
// the register.
//
// It was set from a health whose floor was missing, and a health whose floor
// could not be read is a health that failed — which the reading skips — so
// the field was never set at all; and where it was, it was set to EXACTLY the
// threshold the rule fires strictly above. `floor_unknown` could not fire, on a
// node every one of whose reads was refusing `floor_unknown`.
func TestTheFloorAlarmAgesTheUnreadFloor(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	s := &stateLog{}
	r := &retention{state: s, fleet: coordmem.NewFleet(), nodeID: "node-a"}
	read := func(at time.Time) statelog.Reading {
		return r.reading(t.Context(), at, coord.BackupPoint{At: at}, true, nil, nil)
	}
	unreachable := errors.New("the coordination store is unreachable")

	s.noteFloors(publishedFloors{at: now})
	if got := read(now).FloorUnknownFor; got != 0 {
		t.Fatalf("a floor read a moment ago is read as unreadable for %s", got)
	}
	s.noteFloors(publishedFloors{err: unreachable, at: now})
	s.noteFloors(publishedFloors{err: unreachable, at: now.Add(30 * time.Second)})
	if got := read(now.Add(30 * time.Second)).FloorUnknownFor; got != 30*time.Second {
		t.Fatalf("a floor unreadable for 30s is read as %s — measured from a later "+
			"failure rather than the first", got)
	}
	later := now.Add(2 * time.Minute)
	reading := read(later)
	if reading.FloorUnknownFor != 2*time.Minute {
		t.Fatalf("a floor unreadable for 2m is read as %s", reading.FloorUnknownFor)
	}
	if _, fired := raised(reading)[statelog.KindFloorUnknown]; !fired {
		t.Fatal("floor_unknown did not fire on a floor unreadable past the bound")
	}

	s.noteFloors(publishedFloors{at: later})
	if got := read(later).FloorUnknownFor; got != 0 {
		t.Fatalf("a floor read again is still read as unreadable for %s", got)
	}
	if _, fired := raised(read(later))[statelog.KindFloorUnknown]; fired {
		t.Fatal("floor_unknown still fires once the floor is read")
	}
}

// AND EVERY READ OF THE REGISTER IS WHAT IT IS AGED BY: the node's own reads —
// the verdict's, every reader's health, the report's — note their outcome, so
// the age is the node's experience of the register rather than a probe of its
// own.
func TestEveryFloorReadIsNoted(t *testing.T) {
	t.Parallel()
	fleet := &unreadFloors{fleetRegister: coordmem.NewFleet(), failing: true}
	s := &stateLog{fleet: fleet}
	if read := s.readFloors(t.Context()); read.err == nil {
		t.Fatal("the premise: the register does not answer")
	}
	if got := s.floorsUnreadFor(time.Now().Add(time.Minute)); got < time.Minute {
		t.Fatalf("a failed read of the register was not noted: unreadable for %s", got)
	}
	fleet.failing = false
	if read := s.readFloors(t.Context()); read.err != nil {
		t.Fatalf("the register answers again: %v", read.err)
	}
	if got := s.floorsUnreadFor(time.Now()); got != 0 {
		t.Fatalf("a read that answered left the floor unreadable for %s", got)
	}
}

// unreadFloors is a fleet whose floor register stops answering on demand.
type unreadFloors struct {
	fleetRegister
	failing bool
}

func (u *unreadFloors) Floors(ctx context.Context) ([]coord.TrimFloor, error) {
	if u.failing {
		return nil, errors.New("the coordination store is unreachable")
	}
	return u.fleetRegister.Floors(ctx)
}

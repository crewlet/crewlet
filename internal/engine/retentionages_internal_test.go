package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/pages"
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

	s.noteFloors(floorRead(trackerName, nil, now))
	if got := read(now).FloorUnknown; len(got) != 0 {
		t.Fatalf("a floor read a moment ago is read as unreadable: %v", got)
	}
	s.noteFloors(floorRead(trackerName, unreachable, now))
	s.noteFloors(floorRead(trackerName, unreachable, now.Add(30*time.Second)))
	if got := read(now.Add(30 * time.Second)).FloorUnknown[trackerName]; got != 30*time.Second {
		t.Fatalf("a floor unreadable for 30s is read as %s — measured from a later "+
			"failure rather than the first", got)
	}
	later := now.Add(2 * time.Minute)
	reading := read(later)
	if got := reading.FloorUnknown[trackerName]; got != 2*time.Minute {
		t.Fatalf("a floor unreadable for 2m is read as %s", got)
	}
	if detail, fired := raised(reading)[statelog.KindFloorUnknown]; !fired ||
		!strings.Contains(detail, trackerName) {
		t.Fatalf("floor_unknown on a floor unreadable past the bound: fired %v, %q — "+
			"want it fired naming the tracker", fired, detail)
	}

	s.noteFloors(floorRead(trackerName, nil, later))
	if got := read(later).FloorUnknown; len(got) != 0 {
		t.Fatalf("a floor read again is still read as unreadable: %v", got)
	}
	if _, fired := raised(read(later))[statelog.KindFloorUnknown]; fired {
		t.Fatal("floor_unknown still fires once the floor is read")
	}
}

// floorRead is one read of domain's floor that landed at at, answered — no
// row — or failed with err.
func floorRead(domain string, err error, at time.Time) publishedFloors {
	return publishedFloors{domains: map[string]domainFloor{domain: {err: err}}, at: at}
}

// AND EVERY READ OF THE REGISTER IS WHAT IT IS AGED BY: the node's own reads —
// the verdict's, every reader's health, the report's — note their outcome, so
// the age is the node's experience of the register rather than a probe of its
// own.
func TestEveryFloorReadIsNoted(t *testing.T) {
	t.Parallel()
	fleet := &unreadFloors{fleetRegister: coordmem.NewFleet(), failing: true}
	s := &stateLog{fleet: fleet}
	if read := s.readFloors(t.Context()); read.unread() == nil {
		t.Fatal("the premise: the register does not answer")
	}
	if got := s.floorsUnread(time.Now().Add(time.Minute)); len(got) == 0 || got[trackerName] < time.Minute {
		t.Fatalf("a failed read of the register was not noted: unreadable %v", got)
	}
	fleet.failing = false
	if read := s.readFloors(t.Context()); read.unread() != nil {
		t.Fatalf("the register answers again: %v", read.unread())
	}
	if unread := s.floorsUnread(time.Now()); len(unread) != 0 {
		t.Fatalf("a read that answered left floors unreadable: %v", unread)
	}
}

// unreadFloors is a fleet whose floor register stops answering on demand.
type unreadFloors struct {
	fleetRegister
	failing bool
}

func (u *unreadFloors) Floor(ctx context.Context, domain string) (coord.TrimFloor, bool, error) {
	if u.failing {
		return coord.TrimFloor{}, false, errors.New("the coordination store is unreachable")
	}
	return u.fleetRegister.Floor(ctx, domain)
}

// ONE DOMAIN'S FLOOR ANSWERING SAYS NOTHING ABOUT ANOTHER'S, so it does not
// stop the `floor_unknown` alarm aging the one that does not answer.
//
// A write's fence reads its own domain's key alone, several times a second on
// a node writing that domain. Noted as the whole register's answer, each fence
// read that answered cleared the alarm's clock — so a node whose knowledge
// base floor no longer read refused every read of the knowledge base
// `floor_unknown` once its memo aged, while the alarm of that name stayed
// silent because the tracker's fence kept answering.
func TestOneDomainsFloorAnsweringDoesNotClearAnothersAlarm(t *testing.T) {
	t.Parallel()
	fleet := &oneFloorUnread{fleetRegister: coordmem.NewFleet(), domain: pages.Domain{}.Name()}
	s := &stateLog{fleet: fleet}
	r := &retention{state: s, fleet: fleet, nodeID: "node-a"}

	s.readFloors(t.Context())
	if _, err := s.trimFloor(t.Context(), trackerName, 0); err != nil {
		t.Fatalf("the premise: the tracker's fence reads its own floor: %v", err)
	}
	later := time.Now().Add(statelog.FloorCacheStale + time.Minute)
	reading := r.reading(t.Context(), later, coord.BackupPoint{At: later}, true, nil, nil)
	if got := reading.FloorUnknown[pages.Domain{}.Name()]; got <= statelog.FloorCacheStale {
		t.Fatalf("the knowledge base's floor has not read for %s, and the alarm "+
			"reads it as unreadable for %s — the tracker's fence answering cleared "+
			"a clock it says nothing about", statelog.FloorCacheStale+time.Minute, got)
	}
	detail, fired := raised(reading)[statelog.KindFloorUnknown]
	if !fired {
		t.Fatal("floor_unknown did not fire on a domain whose floor has not read past the bound")
	}
	if !strings.Contains(detail, pages.Domain{}.Name()) || strings.Contains(detail, trackerName) {
		t.Fatalf("the alarm says %q, want it to name the knowledge base's floor and "+
			"not the tracker's, which answers", detail)
	}
}

// A DOMAIN UNREAD FOR A MOMENT IS NOT NAMED BESIDE ONE UNREAD PAST THE BOUND,
// and each domain the alarm names carries its own age.
//
// Two domains' floors begin failing a minute and a half apart. The reading
// once carried the longest age and every failing domain: past the bound for
// the knowledge base alone, the alarm said the tracker's floor too had been
// unreadable for the knowledge base's two and a half minutes, and its remedy
// that the tracker's reads were refused — while the tracker was still served
// from its memo, its floor having gone unread for a minute.
//
// Mutation: fill the reading with the longest age beside every failing domain,
// or have the alarm name every failing domain, and this goes red.
func TestAFloorUnreadForAMomentIsNotNamedBesideOnePastTheBound(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	s := &stateLog{}
	r := &retention{state: s, fleet: coordmem.NewFleet(), nodeID: "node-a"}
	unreachable := errors.New("the row does not decode")
	pagesName := pages.Domain{}.Name()

	s.noteFloors(floorRead(pagesName, unreachable, now))
	s.noteFloors(floorRead(trackerName, unreachable, now.Add(90*time.Second)))
	at := now.Add(150 * time.Second)
	reading := r.reading(t.Context(), at, coord.BackupPoint{At: at}, true, nil, nil)
	if got := reading.FloorUnknown; got[pagesName] != 150*time.Second ||
		got[trackerName] != time.Minute {
		t.Fatalf("the reading ages the floors %v, want the knowledge base's 2m 30s "+
			"and the tracker's 1m, each from its own first failure", got)
	}
	detail, fired := raised(reading)[statelog.KindFloorUnknown]
	if !fired {
		t.Fatal("floor_unknown is silent on a floor unread past the bound")
	}
	if want := "the trim floor of " + pagesName + " has been unreadable for 2m 30s"; detail != want {
		t.Fatalf("the alarm says %q, want %q — naming only the domain whose reads "+
			"are refused, with its own age; the tracker's floor has gone unread for "+
			"%s, inside the %s its memo is served for", detail, want, time.Minute,
			statelog.FloorCacheStale)
	}

	// AND ONCE THE TRACKER'S FLOOR IS PAST THE BOUND TOO, it is named with its
	// own age, never the knowledge base's.
	later := now.Add(200 * time.Second)
	reading = r.reading(t.Context(), later, coord.BackupPoint{At: later}, true, nil, nil)
	want := "the trim floor of " + pagesName + " has been unreadable for 3m 20s, and " +
		trackerName + "'s for 1m 50s"
	if detail := raised(reading)[statelog.KindFloorUnknown]; detail != want {
		t.Fatalf("the alarm says %q, want %q", detail, want)
	}
}

// oneFloorUnread is a fleet whose one domain's floor does not read — a row
// the store refuses as it refuses one that does not decode, in the class the
// register's contract gives that refusal ([coord.ErrFloorUnreadable]) — while
// every other domain's answers.
type oneFloorUnread struct {
	fleetRegister
	domain string
}

func (u *oneFloorUnread) Floor(ctx context.Context, domain string) (coord.TrimFloor, bool, error) {
	if domain == u.domain {
		return coord.TrimFloor{}, false, fmt.Errorf("%w: the trim floor at floor.%s "+
			"does not decode", coord.ErrFloorUnreadable, domain)
	}
	return u.fleetRegister.Floor(ctx, domain)
}

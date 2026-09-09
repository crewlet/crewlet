package statelog_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE FLOOR IS THREE-VALUED, and the third value takes the SAME branch as the
// bad one rather than the optimistic one.
//
// A boolean here hid the answer that matters. A floor that could not be read
// is not a floor that is satisfied, and guessing keeps a node serving over a
// hole it cannot see — which is the one failure a replicated log has no way to
// notice later.
func TestAnUnreadableFloorDoesNotServe(t *testing.T) {
	t.Parallel()
	now := time.Now()
	for state, serves := range map[statelog.FloorState]bool{
		statelog.FloorOK:      true,
		statelog.FloorBelow:   false,
		statelog.FloorUnknown: false,
	} {
		floor := statelog.Floor{State: state, ReadAt: now}
		if got := floor.Serves(now); got != serves {
			t.Errorf("%s.Serves() = %v, want %v", state, got, serves)
		}
		if state.String() == "" || strings.HasPrefix(state.String(), "FloorState(") {
			t.Errorf("%v has no name an operator could read", int(state))
		}
	}

	// AND A FLOOR NOBODY HAS READ RECENTLY IS UNKNOWN, whatever it last
	// said. A cached "at or above the floor" from four heartbeats ago is
	// not a floor that is satisfied — it is a coordination path that has
	// stopped answering, and only the instant tells them apart.
	stale := statelog.Floor{State: statelog.FloorOK, ReadAt: now.Add(-statelog.FloorCacheStale - time.Second)}
	if stale.Effective(now) != statelog.FloorUnknown {
		t.Fatalf("a floor last read %s ago still reports %s — a state whose age "+
			"nobody carries ages silently into an assertion",
			statelog.FloorCacheStale+time.Second, stale.Effective(now))
	}
	never := statelog.Floor{State: statelog.FloorOK}
	if never.Effective(now) != statelog.FloorUnknown {
		t.Fatal("a floor this node has never read reports itself satisfied")
	}
}

// HOLDING RECORDS THIS BUILD CANNOT DECODE DOES NOT SHED SEATS, UNTIL IT DOES.
//
// Folding a deferral into "stalled" takes every un-upgraded node out of
// service the moment one upgraded writer publishes — which is exactly the
// outage the retain rule exists to prevent, arriving through the applier
// instead of the codec. Past the grace the honest reading changes: "this node
// cannot run this company's records" is worth moving work for, and a node that
// fails every call about a growing set of objects while keeping its seats is
// the same outage in a slower form.
func TestADeferralShedsSeatsOnlyPastTheGrace(t *testing.T) {
	t.Parallel()
	now := time.Now()
	base := statelog.Health{
		Position: statelog.Position{Stream: "S", Generation: 1, Seq: 10},
		CaughtUp: true,
		Floor:    statelog.Floor{State: statelog.FloorOK, ReadAt: now},
	}

	if !base.Healthy(now, statelog.DeferredSince{}) {
		t.Fatal("a caught-up node with nothing deferred is not healthy")
	}

	held := base
	held.Deferred = 3
	fresh := statelog.DeferredSince{Since: now.Add(-time.Minute), Held: true}
	if !held.Healthy(now, fresh) {
		t.Fatal("a node holding records for a minute lost its seats — every " +
			"rolling upgrade this design describes is two heartbeats, and " +
			"shedding for one takes the whole fleet out at once")
	}

	old := statelog.DeferredSince{Since: now.Add(-statelog.DeferralGrace - time.Second), Held: true}
	if held.Healthy(now, old) {
		t.Fatalf("a node holding records it cannot decode for longer than %s is "+
			"still admitting seats — at that point it fails every call about a "+
			"growing set of objects", statelog.DeferralGrace)
	}
}

// ESTABLISHED IS A TWO-SIDED INEQUALITY, and each side fails for its own
// reason.
//
// Below the floor, records this node never applied have been trimmed. Above
// the stream's end, a consumer created there waits for a sequence that never
// arrives, reports nothing pending, and looks perfectly caught up while
// applying nothing — for ever.
func TestEstablishedRefusesEachSideForItsOwnReason(t *testing.T) {
	t.Parallel()
	at := func(seq uint64) statelog.Position {
		return statelog.Position{Stream: "S", Generation: 1, Seq: seq}
	}
	ptr := func(v uint64) *uint64 { return &v }

	for name, tc := range map[string]struct {
		health statelog.Health
		strict bool
		ok     bool
		want   statelog.ReadRefusal
	}{
		"at the floor and caught up": {
			health: statelog.Health{
				Position: at(100), TrimFloor: ptr(50), FirstSeq: ptr(50),
				Lag: ptr(0), CaughtUp: true,
			},
			strict: true, ok: true,
		},
		"below the floor": {
			health: statelog.Health{
				Position: at(10), TrimFloor: ptr(500), FirstSeq: ptr(500), Lag: ptr(0),
			},
			want: statelog.RefuseBelowFloor,
		},
		"a floor nobody could read": {
			health: statelog.Health{Position: at(100), Lag: ptr(0), CaughtUp: true},
			want:   statelog.RefuseFloorUnknown,
		},
		"a stream whose end could not be read": {
			health: statelog.Health{
				Position: at(100), TrimFloor: ptr(50), FirstSeq: ptr(50), CaughtUp: true,
			},
			want: statelog.RefuseBrokerUnreachable,
		},
		"a strict domain that has never drained": {
			health: statelog.Health{
				Position: at(100), TrimFloor: ptr(50), FirstSeq: ptr(50), Lag: ptr(3),
			},
			strict: true, want: statelog.RefuseBehind,
		},
		// THE STREAM'S OWN FIRST SEQUENCE MAY ONLY RAISE THE FLOOR. It
		// arrives from a possibly-non-authoritative member, and a stale
		// one is LOWER than the truth — so a maximum that trusts it
		// under-fires and a node below the real floor keeps serving.
		"a stale first sequence cannot lower the published floor": {
			health: statelog.Health{
				Position: at(10), TrimFloor: ptr(500), FirstSeq: ptr(1), Lag: ptr(0),
				CaughtUp: true,
			},
			strict: true, want: statelog.RefuseBelowFloor,
		},
		"and a first sequence above it does raise it": {
			health: statelog.Health{
				Position: at(100), TrimFloor: ptr(50), FirstSeq: ptr(900), Lag: ptr(0),
				CaughtUp: true,
			},
			strict: true, want: statelog.RefuseBelowFloor,
		},
	} {
		t.Run(name, func(t *testing.T) {
			ok, code := tc.health.Established(tc.strict)
			if ok != tc.ok {
				t.Fatalf("Established = (%v, %q), want ok=%v", ok, code, tc.ok)
			}
			if code != tc.want {
				t.Fatalf("code = %q, want %q", code, tc.want)
			}
		})
	}
}

// HEALTH IS THIRTEEN FIELDS, DECLARED ONCE.
//
// The count is asserted because the failure is a copy: written out per reader
// it becomes three lists that disagree, and the fields most likely to be
// dropped are the NILABLE ones — which are precisely the ones carrying "this
// is unknown" rather than "this is zero". A zero-because-unknown lag serves a
// bounded stale read with no bound at all.
func TestHealthCarriesEveryFieldItsContractsCite(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeFor[statelog.Health]()
	if got := typ.NumField(); got != 13 {
		t.Fatalf("Health has %d fields, want 13 — this struct is cited from the "+
			"framework's contracts, the readiness gate, the operator surface and "+
			"the register's heartbeat, and a field added here without a reason "+
			"is a field one of them will not know about", got)
	}
	// AND THE THREE NILABLE ONES STAY NILABLE.
	for _, name := range []string{"Lag", "FirstSeq", "TrimFloor"} {
		f, ok := typ.FieldByName(name)
		if !ok {
			t.Fatalf("Health has no %s", name)
		}
		if f.Type.Kind() != reflect.Pointer {
			t.Errorf("Health.%s is %s — a zero-because-unknown value here answers "+
				"a question nobody asked, and the caller cannot tell", name, f.Type)
		}
	}
}

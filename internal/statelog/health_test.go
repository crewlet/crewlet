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
		Drained:  true,
		Floor:    statelog.Floor{State: statelog.FloorOK, ReadAt: now},
	}

	if !base.Healthy(now, statelog.DeferredSince{}) {
		t.Fatal("a drained node with nothing deferred is not healthy")
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
				Lag: ptr(0), LastSeq: ptr(100), Drained: true,
			},
			strict: true, ok: true,
		},
		"below the floor": {
			health: statelog.Health{
				Position: at(10), TrimFloor: ptr(500), FirstSeq: ptr(500), Lag: ptr(0),
				LastSeq: ptr(900),
			},
			want: statelog.RefuseBelowFloor,
		},
		"a floor nobody could read": {
			health: statelog.Health{Position: at(100), Lag: ptr(0), LastSeq: ptr(100), Drained: true},
			want:   statelog.RefuseFloorUnknown,
		},
		"a stream whose end could not be read": {
			health: statelog.Health{
				Position: at(100), TrimFloor: ptr(50), FirstSeq: ptr(50), Drained: true,
			},
			want: statelog.RefuseBrokerUnreachable,
		},
		// A LAG WITHOUT AN END IS THE SAME REFUSAL: the lag is derived
		// from the end and clamped, so it cannot stand in for it.
		"a lag reported with no end behind it": {
			health: statelog.Health{
				Position: at(100), TrimFloor: ptr(50), FirstSeq: ptr(50), Lag: ptr(0),
				Drained: true,
			},
			want: statelog.RefuseBrokerUnreachable,
		},
		// ABOVE THE END IS A REFUSAL, NOT A CLAMP. The lag reads zero
		// here — which is exactly why the end is its own field.
		"a checkpoint past the log's end": {
			health: statelog.Health{
				Position: at(100), TrimFloor: ptr(50), FirstSeq: ptr(50), Lag: ptr(0),
				LastSeq: ptr(60), Drained: true,
			},
			strict: true, want: statelog.RefuseWrongStream,
		},
		"a strict domain that is behind right now": {
			health: statelog.Health{
				Position: at(100), TrimFloor: ptr(50), FirstSeq: ptr(50), Lag: ptr(3),
				LastSeq: ptr(103),
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
				LastSeq: ptr(900), Drained: true,
			},
			strict: true, want: statelog.RefuseBelowFloor,
		},
		"and a first sequence above it does raise it": {
			health: statelog.Health{
				Position: at(100), TrimFloor: ptr(50), FirstSeq: ptr(900), Lag: ptr(0),
				LastSeq: ptr(900), Drained: true,
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

// HEALTH IS FIFTEEN FIELDS, DECLARED ONCE.
//
// The count is asserted because the failure is a copy: written out per reader
// it becomes three lists that disagree, and the fields most likely to be
// dropped are the NILABLE ones — which are precisely the ones carrying "this
// is unknown" rather than "this is zero". A zero-because-unknown lag serves a
// bounded stale read with no bound at all.
func TestHealthCarriesEveryFieldItsContractsCite(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeFor[statelog.Health]()
	if got := typ.NumField(); got != 15 {
		t.Fatalf("Health has %d fields, want 15 — this struct is cited from the "+
			"framework's contracts, the readiness gate, the operator surface and "+
			"the register's heartbeat, and a field added here without a reason "+
			"is a field one of them will not know about", got)
	}
	// AND THE FOUR NILABLE ONES STAY NILABLE.
	for _, name := range []string{"Lag", "FirstSeq", "TrimFloor", "LastSeq"} {
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

// EVERY FIELD Refusal AND Healthy READ MUST HAVE A PRODUCER, and nothing said
// so until this test.
//
// Four of [statelog.Health]'s fourteen fields — Err, Stalled, Evicted and
// Floor — were read by both decision functions and assigned by nothing. The
// consequences were silent in exactly the way a zero value is: every arm of
// [Health.Refusal] was unreachable, so an evicted node, a node below the trim
// floor and a node whose applier had STOPPED all went on serving reads as
// though current; and [Health.Healthy] could never go false, so the shed the
// `deferred_old` alarm promises an operator never happened.
//
// A STRUCTURAL TEST rather than a behavioural one, because the defect is
// structural: each arm has a behavioural test above that passes a Health
// built BY HAND, and a hand-built value proves the function and says nothing
// about whether the engine fills the field. This asserts the decision
// functions actually turn on each field, so a field that stopped being read
// is caught here and a field that stopped being WRITTEN is caught by
// TestEveryHealthInputIsPopulated in internal/engine.
func TestEveryFieldTheDecisionsReadCanChangeTheAnswer(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	serving := func() statelog.Health {
		return statelog.Health{
			Floor: statelog.Floor{State: statelog.FloorOK, ReadAt: now},
		}
	}

	if code := serving().Refusal(now); code != "" {
		t.Fatalf("the control refuses with %q, so every case below is vacuous", code)
	}
	if !serving().Healthy(now, statelog.DeferredSince{}) {
		t.Fatal("the control is unhealthy, so every case below is vacuous")
	}

	for _, tc := range []struct {
		field string
		mutil func(*statelog.Health)
		want  statelog.ReadRefusal
	}{
		{"Evicted", func(h *statelog.Health) { h.Evicted = true }, statelog.RefuseEvicted},
		{"Err", func(h *statelog.Health) { h.Err = "halted at 41" }, statelog.RefuseStalled},
		{"Stalled", func(h *statelog.Health) { h.Stalled = true }, statelog.RefuseStalled},
		{"Floor(below)", func(h *statelog.Health) { h.Floor.State = statelog.FloorBelow },
			statelog.RefuseBelowFloor},
		{"Floor(unread)", func(h *statelog.Health) { h.Floor = statelog.Floor{} },
			statelog.RefuseFloorUnknown},
		// THE END, which the lag cannot stand in for: it is clamped at
		// zero, so a checkpoint past the end reads as caught up.
		{"LastSeq(behind the checkpoint)", func(h *statelog.Health) {
			end := uint64(40)
			h.Position.Seq, h.LastSeq = 41, &end
		}, statelog.RefuseWrongStream},
		// AND THE REBUILT STREAM THE SEQUENCES CANNOT SHOW. It comes
		// back at generation 0 counting from 1, so once it has
		// published past this node's checkpoint the term above goes
		// quiet while the node applies a different history.
		{"StreamRecreated", func(h *statelog.Health) { h.StreamRecreated = true },
			statelog.RefuseWrongStream},
	} {
		h := serving()
		tc.mutil(&h)
		if got := h.Refusal(now); got != tc.want {
			t.Errorf("%s set: Refusal = %q, want %q — this field cannot change "+
				"the answer, so nothing needs to produce it", tc.field, got, tc.want)
		}
		if h.Healthy(now, statelog.DeferredSince{}) {
			t.Errorf("%s set: still Healthy, so a node in this state keeps its seats",
				tc.field)
		}
	}

	// The deferral shed is the one condition that needs a SERIES, so it is
	// the one whose input is a second argument rather than a field.
	held := serving()
	held.Deferred = 1
	if !held.Healthy(now, statelog.DeferredSince{Since: now.Add(-time.Minute), Held: true}) {
		t.Error("a deferral inside the grace shed the seats, which would move a " +
			"company's work on every rolling upgrade")
	}
	if held.Healthy(now, statelog.DeferredSince{
		Since: now.Add(-statelog.DeferralGrace - time.Second), Held: true}) {
		t.Error("a deferral past the grace kept the seats, which is what the " +
			"deferred_old alarm already tells an operator has stopped")
	}
}

// BEING BEHIND DOES NOT SHED SEATS; HAVING STOPPED DOES.
//
// The two facts were one bool, and it was assigned `lag == 0` on every
// heartbeat — so a node holding a record it had not applied YET read as a node
// whose copy was WRONG. The measured cost was a single node releasing all
// seven of its seats on each burst of tracker writes and reclaiming them about
// five seconds later, six times in eight minutes. The distinction this pins is
// the one [statelog.Health.Healthy]'s whole contract rests on: a copy that is
// behind catches up, and a copy that has stopped moving does not.
func TestALaggingNodeKeepsItsSeatsAndAStalledOneDoesNot(t *testing.T) {
	t.Parallel()
	now := time.Now()
	ptr := func(v uint64) *uint64 { return &v }
	behind := func() statelog.Health {
		return statelog.Health{
			Position:  statelog.Position{Stream: "S", Generation: 1, Seq: 100},
			Drained:   true,
			Floor:     statelog.Floor{State: statelog.FloorOK, ReadAt: now},
			Lag:       ptr(7),
			LastSeq:   ptr(107),
			FirstSeq:  ptr(1),
			TrimFloor: ptr(1),
		}
	}

	if !behind().Healthy(now, statelog.DeferredSince{}) {
		t.Fatal("a node seven records behind gave up its seats — with a real " +
			"model that is every turn on the node interrupted by somebody " +
			"filing a work item, and the records catch up on their own")
	}
	if code := behind().Refusal(now); code != "" {
		t.Errorf("a node seven records behind refuses reads with %q; ordinary "+
			"lag is answered by the caller's own staleness bound, not by a "+
			"refusal", code)
	}

	// A HUGE LAG IS STILL A LAG. Nothing here is a threshold on the
	// distance: a node that is applying is a node that is catching up,
	// however far it has to come.
	far := behind()
	far.Lag, far.LastSeq = ptr(1_000_000), ptr(1_000_100)
	if !far.Healthy(now, statelog.DeferredSince{}) {
		t.Error("a node a million records behind gave up its seats, so a fleet " +
			"whose peer published a backlog moves the company's work rather " +
			"than waiting out the replay")
	}

	// AND A NODE THAT HAS NEVER DRAINED IS THE SAME KIND OF BEHIND. It is
	// what a node looks like between its boot and its first catch-up, and
	// the remedy is the admission gate below, never a shed.
	fresh := behind()
	fresh.Drained = false
	if !fresh.Healthy(now, statelog.DeferredSince{}) {
		t.Error("a node that has not finished hydrating reports its copy WRONG, " +
			"so the sweep logs `seats_shed_unserviceable` at WARN every pass " +
			"for a node whose only fault is that it is still catching up")
	}

	// THE STALL IS THE ONE THAT DOES SHED: this node owes progress and has
	// made none for the grace, so its rows are frozen rather than moving.
	stalled := behind()
	stalled.Stalled = true
	if stalled.Healthy(now, statelog.DeferredSince{}) {
		t.Errorf("a node frozen for %s kept its seats — every expectation it "+
			"forms is stale and every write burns its round budget on a "+
			"conflict", statelog.StallGrace)
	}
	if code := stalled.Refusal(now); code != statelog.RefuseStalled {
		t.Errorf("a stalled node refuses with %q, want %q", code, statelog.RefuseStalled)
	}

	// ADMISSION IS THE OTHER GATE AND IT READS THE INSTANT. A seat about to
	// attach would act on rows that are behind, so `strict` refuses on the
	// LAG whatever the drain latch says.
	if ok, code := behind().Established(true); ok || code != statelog.RefuseBehind {
		t.Errorf("Established(strict) = (%v, %q) for a drained node seven "+
			"records behind, want a %q refusal — a seat attaching here "+
			"answers \"there is no such item\" about work it was just handed",
			ok, code, statelog.RefuseBehind)
	}
	// And a node that is level is admitted, drain latch or not: being level
	// IS having drained, this instant.
	level := behind()
	level.Lag, level.LastSeq, level.Drained = ptr(0), ptr(100), false
	if ok, code := level.Established(true); !ok {
		t.Errorf("Established(strict) = (%v, %q) for a node level with the log, "+
			"want it admitted", ok, code)
	}
}

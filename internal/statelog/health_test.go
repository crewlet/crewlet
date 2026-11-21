package statelog_test

import (
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// AN UNREADABLE FLOOR TAKES THE SAME BRANCH AS THE BAD ONE rather than the
// optimistic one, in both decisions that read it.
//
// Guessing keeps a node serving over a hole it cannot see, which a replicated
// log never notices later. A node replaying up to the floor serves no read
// either, but its copy stays in service: behind, not wrong. So both decisions,
// [statelog.Health.Refusal] and [statelog.Health.Healthy], are asserted, not
// one "does it serve" predicate, which would take a replaying copy out.
func TestAnUnreadableFloorTakesTheBadBranch(t *testing.T) {
	t.Parallel()
	now := time.Now()
	for state, want := range map[statelog.FloorState]struct {
		refusal statelog.ReadRefusal
		healthy bool
	}{
		statelog.FloorOK:        {refusal: "", healthy: true},
		statelog.FloorReplaying: {refusal: statelog.RefuseBehind, healthy: true},
		statelog.FloorBelow:     {refusal: statelog.RefuseBelowFloor, healthy: false},
		statelog.FloorUnknown:   {refusal: statelog.RefuseFloorUnknown, healthy: false},
	} {
		h := statelog.Health{Floor: statelog.Floor{State: state, ReadAt: now}}
		if got := h.Refusal(now); got != want.refusal {
			t.Errorf("a %s floor refuses reads with %q, want %q", state, got, want.refusal)
		}
		if got := h.Healthy(now, statelog.DeferredSince{}); got != want.healthy {
			t.Errorf("a %s floor keeps the copy in service: %v, want %v", state, got, want.healthy)
		}
		if state.String() == "" || strings.HasPrefix(state.String(), "FloorState(") {
			t.Errorf("%v has no name an operator could read", int(state))
		}
	}

	// AND A FLOOR NOBODY HAS READ RECENTLY IS UNKNOWN, whatever it last
	// said: an old cached answer means the coordination path has stopped
	// answering, and only the instant tells.
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

// A CHECKPOINT ONE BELOW THE LOG'S FIRST RECORD HAS MISSED NOTHING, and one
// further down has missed exactly one record.
//
// Every reader of the floor shares this boundary (readiness gate, health read,
// join, heartbeat, backup, both write fences): a step either way refuses a
// node holding all it needs or serves over a hole. The zero cases are what JetStream reports:
// a never-written stream says first = 0, a purged-empty one first = last+1.
func TestACheckpointOneBelowTheFirstRecordHasMissedNothing(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		checkpoint, first uint64
		replayable        bool
	}{
		"a never-written stream and a node that applied nothing":  {0, 0, true},
		"a published floor of 0, which licensed removing nothing": {7, 0, true},
		"nothing applied and the whole log still ahead":           {0, 1, true},
		"nothing applied and record 1 already gone":               {0, 2, false},
		"one applied and the next record is the first":            {1, 2, true},
		"one applied and record 2 already gone":                   {1, 3, false},
		"exactly one below the first record":                      {5, 6, true},
		"well above the first record":                             {100, 50, true},
		"exactly at the first record":                             {6, 6, true},
		// A PURGE THAT EMPTIED THE STREAM leaves first = last+1: applied
		// through last misses nothing, a record short lost it.
		"purged empty after 5, applied through 5": {5, 5 + 1, true},
		"purged empty after 5, applied through 4": {4, 5 + 1, false},
		// THE TOP OF THE RANGE, where `first > checkpoint+1` wraps to 0
		// and would leave behind a node that applied everything.
		"a checkpoint at the top of the range":           {math.MaxUint64, 1, true},
		"the top of the range against itself":            {math.MaxUint64, math.MaxUint64, true},
		"one below the top of the range":                 {math.MaxUint64 - 1, math.MaxUint64, true},
		"two below the top of the range":                 {math.MaxUint64 - 2, math.MaxUint64, false},
		"nothing applied against a first at the top":     {0, math.MaxUint64, false},
		"the last valid sequence, purged empty after it": {statelog.MaxSeq, statelog.MaxSeq + 1, true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := statelog.Replayable(tc.checkpoint, tc.first); got != tc.replayable {
				t.Fatalf("Replayable(checkpoint %d, first %d) = %v, want %v",
					tc.checkpoint, tc.first, got, tc.replayable)
			}
		})
	}
}

// HOLDING RECORDS THIS BUILD CANNOT DECODE DOES NOT TAKE A COPY OUT OF
// SERVICE, UNTIL IT DOES.
//
// Folding a deferral into "stalled" takes every un-upgraded copy out the
// moment one upgraded writer publishes: the outage the retain rule prevents
// (package doc, "A record this build cannot decode is RETAINED, with one
// exception"). Past the grace, a copy failing every call about a growing set
// of objects is that outage, slower, so its readers go elsewhere.
func TestADeferralTakesACopyOutOfServiceOnlyPastTheGrace(t *testing.T) {
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
		t.Fatal("a node holding records for a minute took its copy out of " +
			"service — every rolling upgrade this design describes is two " +
			"heartbeats, and taking copies out for one takes every " +
			"un-upgraded copy out at once")
	}

	old := statelog.DeferredSince{Since: now.Add(-statelog.DeferralGrace - time.Second), Held: true}
	if held.Healthy(now, old) {
		t.Fatalf("a node holding records it cannot decode for longer than %s is "+
			"still serving its copy — at that point it fails every call about a "+
			"growing set of objects", statelog.DeferralGrace)
	}
}

// A WRONG COPY NAMES ITS FAULT, and a sound one names none: every state that
// makes [statelog.Health.Healthy] false has the reason its out-of-service line
// carries, and being behind (replaying, or deferring inside the grace) is no
// fault. Healthy is the reason's emptiness, so the two cannot disagree.
func TestAWrongCopyNamesItsFault(t *testing.T) {
	t.Parallel()
	now := time.Now()
	end := uint64(10)
	sound := statelog.Health{
		Position: statelog.Position{Stream: "S", Generation: 1, Seq: 10},
		LastSeq:  &end, Drained: true,
		Floor: statelog.Floor{State: statelog.FloorOK, ReadAt: now},
	}
	ahead := uint64(9)
	old := statelog.DeferredSince{Since: now.Add(-statelog.DeferralGrace - time.Second), Held: true}
	fresh := statelog.DeferredSince{Since: now.Add(-time.Minute), Held: true}
	for name, tc := range map[string]struct {
		change func(*statelog.Health)
		since  statelog.DeferredSince
		want   statelog.ReadRefusal
	}{
		"sound":                {change: func(*statelog.Health) {}},
		"replaying":            {change: func(h *statelog.Health) { h.Floor.State = statelog.FloorReplaying }},
		"deferred, in grace":   {change: func(h *statelog.Health) { h.Deferred = 1 }, since: fresh},
		"evicted":              {change: func(h *statelog.Health) { h.Evicted = true }, want: statelog.RefuseEvicted},
		"floor unknown":        {change: func(h *statelog.Health) { h.Floor.ReadAt = time.Time{} }, want: statelog.RefuseFloorUnknown},
		"below the log":        {change: func(h *statelog.Health) { h.Floor.State = statelog.FloorBelow }, want: statelog.RefuseBelowFloor},
		"ahead of the log":     {change: func(h *statelog.Health) { h.LastSeq = &ahead }, want: statelog.RefuseWrongStream},
		"a recreated stream":   {change: func(h *statelog.Health) { h.StreamRecreated = true }, want: statelog.RefuseWrongStream},
		"a passed generation":  {change: func(h *statelog.Health) { h.GenerationPassed = true }, want: statelog.RefuseWrongStream},
		"a diverged log":       {change: func(h *statelog.Health) { h.LogDiverged = true }, want: statelog.RefuseWrongStream},
		"a stopped applier":    {change: func(h *statelog.Health) { h.Err = "halted at 7" }, want: statelog.RefuseStalled},
		"a stalled prefix":     {change: func(h *statelog.Health) { h.Stalled = true }, want: statelog.RefuseStalled},
		"deferred, past grace": {change: func(h *statelog.Health) { h.Deferred = 1 }, since: old, want: statelog.RefuseDeferred},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := sound
			tc.change(&h)
			if got := h.Fault(now, tc.since); got != tc.want {
				t.Fatalf("Fault = %q, want %q", got, tc.want)
			}
			if got := h.Healthy(now, tc.since); got != (tc.want == "") {
				t.Fatalf("Healthy = %v with fault %q", got, tc.want)
			}
		})
	}
}

// TAKING A COPY OUT OF SERVICE AND THE ALARM READ ONE AGE, and it can reach
// the grace.
//
// `deferred_old` fires past [statelog.DeferralGrace] to say the copy goes out
// of service, which [statelog.Health.Healthy] going false does. Both take the
// age from [statelog.DeferredSince.Age], so the two instants agree; the cases
// hold the arithmetic to the table's rule at the grace and past it.
func TestTakingACopyOutOfServiceAndTheAlarmReadOneDeferralAge(t *testing.T) {
	t.Parallel()
	now := time.Date(2031, 4, 2, 3, 14, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		since statelog.DeferredSince
		want  time.Duration
	}{
		"nothing held":             {statelog.DeferredSince{}, 0},
		"held and never dated":     {statelog.DeferredSince{Held: true}, 0},
		"dated and no longer held": {statelog.DeferredSince{Since: now.Add(-time.Hour)}, 0},
		"held for exactly the grace": {statelog.DeferredSince{
			Since: now.Add(-statelog.DeferralGrace), Held: true}, statelog.DeferralGrace},
		"held since a minute past the grace": {statelog.DeferredSince{
			Since: now.Add(-statelog.DeferralGrace - time.Minute), Held: true},
			statelog.DeferralGrace + time.Minute},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.since.Age(now); got != tc.want {
				t.Fatalf("age = %s, want %s", got, tc.want)
			}
			_, fired := find(statelog.Evaluate(statelog.Reading{
				DeferredAge: tc.since.Age(now)}),
				statelog.KindDeferredOld)
			held := statelog.Health{
				Position: statelog.Position{Stream: "S", Generation: 1, Seq: 10},
				Drained:  true, Deferred: 1,
				Floor: statelog.Floor{State: statelog.FloorOK, ReadAt: now},
			}
			if out := !held.Healthy(now, tc.since); out != fired {
				t.Errorf("the copy's health says out of service = %v and the "+
					"alarm says %v about one deferral — the alarm tells an "+
					"operator when the copy stops serving, so the two must "+
					"agree", out, fired)
			}
			if fired != (tc.want > statelog.DeferralGrace) {
				t.Errorf("deferred_old fired = %v for an age of %s", fired, tc.want)
			}
		})
	}
}

// A SERVING COPY HOLDS A WHOLE STATE AND IS CLOSE TO THE LOG, WHATEVER THE
// INSTANT SAYS: a drained, established copy serves at a lag of one (a busy
// log's record in flight) and at the snapshot slack, and is behind one past it
// or before it ever drained. Established's refusals on the log's own terms
// are its refusals too.
func TestAServingCopyIsDrainedAndWithinTheSlack(t *testing.T) {
	t.Parallel()
	ptr := func(v uint64) *uint64 { return &v }
	copyAt := func(lag uint64, drained bool) statelog.Health {
		end := uint64(10_000)
		return statelog.Health{
			Position:  statelog.Position{Stream: "S", Generation: 1, Seq: end - lag},
			TrimFloor: ptr(50), FirstSeq: ptr(50), Lag: ptr(lag), LastSeq: ptr(end),
			Drained: drained,
		}
	}
	for name, tc := range map[string]struct {
		health statelog.Health
		ok     bool
		want   statelog.ReadRefusal
	}{
		"caught up":            {health: copyAt(0, true), ok: true},
		"one record in flight": {health: copyAt(1, true), ok: true},
		"at the slack":         {health: copyAt(statelog.SnapshotLagSlack, true), ok: true},
		"past the slack":       {health: copyAt(statelog.SnapshotLagSlack+1, true), want: statelog.RefuseBehind},
		"never drained":        {health: copyAt(0, false), want: statelog.RefuseBehind},
		"the end unread":       {health: func() statelog.Health { h := copyAt(0, true); h.Lag, h.LastSeq = nil, nil; return h }(), want: statelog.RefuseBrokerUnreachable},
		"a floor nobody read":  {health: func() statelog.Health { h := copyAt(0, true); h.TrimFloor = nil; return h }(), want: statelog.RefuseFloorUnknown},
		"a stream rebuilt":     {health: func() statelog.Health { h := copyAt(0, true); h.StreamRecreated = true; return h }(), want: statelog.RefuseWrongStream},
		"below the log's start": {health: func() statelog.Health {
			h := copyAt(0, true)
			h.FirstSeq, h.TrimFloor = ptr(10_000), ptr(10_000)
			h.Position.Seq = 10
			return h
		}(), want: statelog.RefuseBelowFloor},
	} {
		ok, refusal := tc.health.Serving()
		if ok != tc.ok || refusal != tc.want {
			t.Errorf("%s: Serving = (%v, %q), want (%v, %q)", name, ok, refusal, tc.ok, tc.want)
		}
	}
	// THE PREMISE: admission refuses the copy with one record in flight,
	// and serving does not.
	if ok, _ := copyAt(1, true).Established(true); ok {
		t.Error("the premise: seat admission admits a copy with a record in flight")
	}
}

// A COPY ANSWERS A REQUEST WHEN IT IS SERVING OR LEVEL THIS INSTANT: a drained
// copy with a record in flight answers, and so does one at lag zero not yet
// latched drained (else a seat admitted at lag zero is refused meanwhile). One
// far behind, or never drained and behind, does not, and Established's
// refusals on the log's own terms are its refusals too.
func TestACopyAnswersWhenServingOrLevel(t *testing.T) {
	t.Parallel()
	ptr := func(v uint64) *uint64 { return &v }
	copyAt := func(lag uint64, drained bool) statelog.Health {
		end := uint64(10_000)
		return statelog.Health{
			Position:  statelog.Position{Stream: "S", Generation: 1, Seq: end - lag},
			TrimFloor: ptr(50), FirstSeq: ptr(50), Lag: ptr(lag), LastSeq: ptr(end),
			Drained: drained,
		}
	}
	for name, tc := range map[string]struct {
		health statelog.Health
		ok     bool
		want   statelog.ReadRefusal
	}{
		"drained, a record in flight":    {health: copyAt(1, true), ok: true},
		"level before the drained latch": {health: copyAt(0, false), ok: true},
		"never drained and behind":       {health: copyAt(3, false), want: statelog.RefuseBehind},
		"past the slack":                 {health: copyAt(statelog.SnapshotLagSlack+1, true), want: statelog.RefuseBehind},
		"a stream rebuilt":               {health: func() statelog.Health { h := copyAt(0, false); h.StreamRecreated = true; return h }(), want: statelog.RefuseWrongStream},
		"a floor nobody read":            {health: func() statelog.Health { h := copyAt(0, false); h.TrimFloor = nil; return h }(), want: statelog.RefuseFloorUnknown},
	} {
		ok, refusal := tc.health.Answers()
		if ok != tc.ok || refusal != tc.want {
			t.Errorf("%s: Answers = (%v, %q), want (%v, %q)", name, ok, refusal, tc.ok, tc.want)
		}
	}
}

// ESTABLISHED IS A TWO-SIDED INEQUALITY, and each side fails for its own
// reason.
//
// Below the log, records this node never applied are trimmed. Below the
// published floor but above the log, they are still there and the node is
// replaying them, so it is `behind`, which clears on its own. Above the
// stream's end, a consumer waits for a sequence that never comes and looks
// caught up while applying nothing, for ever.
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
		"below the log": {
			health: statelog.Health{
				Position: at(10), TrimFloor: ptr(500), FirstSeq: ptr(500), Lag: ptr(0),
				LastSeq: ptr(900),
			},
			want: statelog.RefuseBelowFloor,
		},
		// THE FLOOR IS PUBLISHED BEFORE THE PURGE IT LICENSES, so a failed
		// purge leaves it above the log's first record; what this node
		// lacks is still there and it is replaying it, on either kind of
		// readiness.
		"below the published floor and above the log": {
			health: statelog.Health{
				Position: at(10), TrimFloor: ptr(500), FirstSeq: ptr(1), Lag: ptr(890),
				LastSeq: ptr(900),
			},
			want: statelog.RefuseBehind,
		},
		"below the published floor and above the log, strictly": {
			health: statelog.Health{
				Position: at(10), TrimFloor: ptr(500), FirstSeq: ptr(1), Lag: ptr(890),
				LastSeq: ptr(900),
			},
			strict: true, want: statelog.RefuseBehind,
		},
		// AND WITH NO FIRST SEQUENCE THERE IS NO SAYING WHICH: the
		// unanswering broker is the reason.
		"below the published floor with the log's bounds unread": {
			health: statelog.Health{
				Position: at(10), TrimFloor: ptr(500), Lag: ptr(0), LastSeq: ptr(900),
			},
			want: statelog.RefuseBrokerUnreachable,
		},
		// THE BOUNDARY ITSELF, as a purge leaves it: nothing below 50,
		// last record 49, so a node at 49 has applied all that was removed.
		"one below the floor has missed nothing": {
			health: statelog.Health{
				Position: at(49), TrimFloor: ptr(50), FirstSeq: ptr(50),
				Lag: ptr(0), LastSeq: ptr(49), Drained: true,
			},
			strict: true, ok: true,
		},
		"two below the floor has missed a record": {
			health: statelog.Health{
				Position: at(48), TrimFloor: ptr(50), FirstSeq: ptr(50),
				Lag: ptr(1), LastSeq: ptr(49),
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
		// A GENERATION A PEER RE-ANCHORED PAST is not this node's history
		// however caught up its numbers read.
		"a generation a peer re-anchored past": {
			health: statelog.Health{
				Position: at(100), TrimFloor: ptr(50), FirstSeq: ptr(50), Lag: ptr(0),
				LastSeq: ptr(100), Drained: true, GenerationPassed: true,
			},
			want: statelog.RefuseWrongStream,
		},
		// A LOG THAT DIVERGED FROM THE ROWS is not this node's history
		// either, and its end has caught up with the checkpoint.
		"a log that diverged from the rows": {
			health: statelog.Health{
				Position: at(100), TrimFloor: ptr(50), FirstSeq: ptr(50), Lag: ptr(0),
				LastSeq: ptr(100), Drained: true, LogDiverged: true,
			},
			want: statelog.RefuseWrongStream,
		},
		"a log that diverged from rows below the floor": {
			health: statelog.Health{
				Position: at(10), TrimFloor: ptr(50), FirstSeq: ptr(5), Lag: ptr(90),
				LastSeq: ptr(100), LogDiverged: true,
			},
			want: statelog.RefuseWrongStream,
		},
		"a strict domain that is behind right now": {
			health: statelog.Health{
				Position: at(100), TrimFloor: ptr(50), FirstSeq: ptr(50), Lag: ptr(3),
				LastSeq: ptr(103),
			},
			strict: true, want: statelog.RefuseBehind,
		},
		// THE STREAM'S OWN FIRST SEQUENCE MAY ONLY RAISE THE FLOOR: from a
		// possibly-non-authoritative member, a stale one is LOWER than the
		// truth, and trusting it keeps a node below the real floor serving.
		// Below the floor it picks the refusal, and never clears one.
		"a stale first sequence cannot lower the published floor": {
			health: statelog.Health{
				Position: at(10), TrimFloor: ptr(500), FirstSeq: ptr(1), Lag: ptr(0),
				LastSeq: ptr(900), Drained: true,
			},
			strict: true, want: statelog.RefuseBehind,
		},
		"and a first sequence above it does raise it": {
			health: statelog.Health{
				Position: at(100), TrimFloor: ptr(50), FirstSeq: ptr(900), Lag: ptr(0),
				LastSeq: ptr(900), Drained: true,
			},
			strict: true, want: statelog.RefuseBelowFloor,
		},
		// A REBUILT STREAM UNDER A STALE FLOOR IS NOT A REPLAY: a rebuilt
		// stream keeps the old floor (only a reanchor touches it), so a node
		// below it replays nothing that clears, and the retryable `behind`
		// would send a caller back for ever.
		"a rebuilt stream below a stale floor": {
			health: statelog.Health{
				Position: at(5), TrimFloor: ptr(10), FirstSeq: ptr(1), Lag: ptr(15),
				LastSeq: ptr(20), StreamRecreated: true,
			},
			want: statelog.RefuseWrongStream,
		},
		"a rebuilt stream below a stale floor, strictly": {
			health: statelog.Health{
				Position: at(5), TrimFloor: ptr(10), FirstSeq: ptr(1), Lag: ptr(15),
				LastSeq: ptr(20), StreamRecreated: true,
			},
			strict: true, want: statelog.RefuseWrongStream,
		},
		// AND AN EMPTY ONE, whose end is below the checkpoint: the floor
		// split must not mask the end either.
		"an empty rebuilt stream below a stale floor": {
			health: statelog.Health{
				Position: at(5), TrimFloor: ptr(10), FirstSeq: ptr(0), Lag: ptr(0),
				LastSeq: ptr(0),
			},
			want: statelog.RefuseWrongStream,
		},
		"an empty rebuilt stream below a stale floor, strictly": {
			health: statelog.Health{
				Position: at(5), TrimFloor: ptr(10), FirstSeq: ptr(0), Lag: ptr(0),
				LastSeq: ptr(0),
			},
			strict: true, want: statelog.RefuseWrongStream,
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

// HEALTH IS SEVENTEEN FIELDS, DECLARED ONCE.
//
// Copied per reader, it becomes lists that disagree, and the fields most
// likely dropped are the NILABLE ones, which carry "unknown" rather than
// "zero": a zero-because-unknown lag serves a bounded stale read unbounded.
func TestHealthCarriesEveryFieldItsContractsCite(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeFor[statelog.Health]()
	if got := typ.NumField(); got != 17 {
		t.Fatalf("Health has %d fields, want 17 — this struct is cited from the "+
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

// EVERY FIELD Refusal AND Healthy READ MUST HAVE A PRODUCER.
//
// A field read and never assigned fails silently as its zero value: an
// evicted, trimmed-past or stopped node serves as though current. The
// behavioural tests above build Health BY HAND, which says nothing about the
// engine filling it, so this asserts each field turns the decisions; a field
// no longer WRITTEN is caught by TestEveryHealthInputIsPopulated in
// internal/engine.
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
		// AND THE REBUILT STREAM THE SEQUENCES CANNOT SHOW: back at
		// generation 0 from 1, once past this node's checkpoint the term
		// above goes quiet on a different history.
		{"StreamRecreated", func(h *statelog.Health) { h.StreamRecreated = true },
			statelog.RefuseWrongStream},
		// AND A GENERATION A PEER RE-ANCHORED PAST, invisible to both above
		// on a broker restored from an older copy: the log continues from
		// that peer's rows, not these.
		{"GenerationPassed", func(h *statelog.Health) { h.GenerationPassed = true },
			statelog.RefuseWrongStream},
		// AND A LOG THAT DIVERGED FROM THE ROWS: a restored broker written
		// past the checkpoint, where every other term reads healthy.
		{"LogDiverged", func(h *statelog.Health) { h.LogDiverged = true },
			statelog.RefuseWrongStream},
	} {
		h := serving()
		tc.mutil(&h)
		if got := h.Refusal(now); got != tc.want {
			t.Errorf("%s set: Refusal = %q, want %q — this field cannot change "+
				"the answer, so nothing needs to produce it", tc.field, got, tc.want)
		}
		if h.Healthy(now, statelog.DeferredSince{}) {
			t.Errorf("%s set: still Healthy, so a copy in this state keeps serving",
				tc.field)
		}
	}

	// REPLAYING UP TO THE FLOOR IS A REFUSAL AND NOT A FAULT: the records
	// are on the log, so a read is told to come back and the copy stays in
	// service. WITH RECORDS PENDING, the only way the engine produces it (a
	// published floor is at most one past the log's end); a level case
	// would certify a state no node reports.
	replaying := serving()
	replaying.Floor.State = statelog.FloorReplaying
	pending := uint64(5)
	replaying.Lag = &pending
	if got := replaying.Refusal(now); got != statelog.RefuseBehind {
		t.Errorf("a node replaying up to the floor refuses %q, want %q — the "+
			"state clears on its own, and a code no caller retries sends them "+
			"away from a node that is only catching up", got, statelog.RefuseBehind)
	}
	if !replaying.Healthy(now, statelog.DeferredSince{}) {
		t.Error("a node replaying up to the floor was judged wrong, which takes " +
			"out of service a copy that is behind rather than wrong")
	}
	// AND IT IS THE LAST REFUSAL, because it clears on its own only while
	// the applier moves: a stalled node replaying nothing is stalled.
	replaying.Stalled = true
	if got := replaying.Refusal(now); got != statelog.RefuseStalled {
		t.Errorf("a stalled node below the floor refuses %q, want %q — it is "+
			"not replaying anything", got, statelog.RefuseStalled)
	}

	// The deferral fault needs a SERIES, so its input is an argument, not a
	// field.
	held := serving()
	held.Deferred = 1
	if !held.Healthy(now, statelog.DeferredSince{Since: now.Add(-time.Minute), Held: true}) {
		t.Error("a deferral inside the grace took the copy out of service, which " +
			"would take every un-upgraded copy out on every rolling upgrade")
	}
	if held.Healthy(now, statelog.DeferredSince{
		Since: now.Add(-statelog.DeferralGrace - time.Second), Held: true}) {
		t.Error("a deferral past the grace kept the copy in service, which is " +
			"what the deferred_old alarm already tells an operator has stopped")
	}
}

// BEING BEHIND DOES NOT TAKE A COPY OUT OF SERVICE; HAVING STOPPED DOES.
//
// [statelog.Health.Healthy]'s contract: a copy that is behind catches up, and
// one that has stopped does not. Reading a record not applied YET as a WRONG
// copy makes a node drop all its seats on every burst of writes.
func TestALaggingCopyKeepsServingAndAStalledOneDoesNot(t *testing.T) {
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
		t.Fatal("a node seven records behind was judged wrong — its copy would " +
			"stop serving on every burst of tracker writes, and a single node's " +
			"seats would be refused every call, while the records catch up on " +
			"their own")
	}
	if code := behind().Refusal(now); code != "" {
		t.Errorf("a node seven records behind refuses reads with %q; ordinary "+
			"lag is answered by the caller's own staleness bound, not by a "+
			"refusal", code)
	}

	// A HUGE LAG IS STILL A LAG: no threshold on distance; a node applying
	// is catching up.
	far := behind()
	far.Lag, far.LastSeq = ptr(1_000_000), ptr(1_000_100)
	if !far.Healthy(now, statelog.DeferredSince{}) {
		t.Error("a node a million records behind was judged wrong, so a fleet " +
			"whose peer published a backlog takes every copy out of service " +
			"rather than waiting out the replay")
	}

	// AND A NODE THAT HAS NEVER DRAINED (booted, not yet caught up) IS THE
	// SAME KIND OF BEHIND: the admission gate below handles it, not a fault.
	fresh := behind()
	fresh.Drained = false
	if !fresh.Healthy(now, statelog.DeferredSince{}) {
		t.Error("a node that has not finished hydrating reports its copy WRONG, " +
			"so it stops serving a copy whose only fault is that it is " +
			"still catching up")
	}

	// THE STALL IS THE FAULT: no progress owed was made for the grace.
	stalled := behind()
	stalled.Stalled = true
	if stalled.Healthy(now, statelog.DeferredSince{}) {
		t.Errorf("a node frozen for %s kept its copy in service — every expectation it "+
			"forms is stale and every write burns its round budget on a "+
			"conflict", statelog.StallGrace)
	}
	if code := stalled.Refusal(now); code != statelog.RefuseStalled {
		t.Errorf("a stalled node refuses with %q, want %q", code, statelog.RefuseStalled)
	}

	// ADMISSION IS THE OTHER GATE AND IT READS THE INSTANT: a seat about to
	// attach would act on stale rows, so `strict` refuses on the LAG,
	// whatever the drain latch says.
	if ok, code := behind().Established(true); ok || code != statelog.RefuseBehind {
		t.Errorf("Established(strict) = (%v, %q) for a drained node seven "+
			"records behind, want a %q refusal — a seat attaching here "+
			"answers \"there is no such item\" about work it was just handed",
			ok, code, statelog.RefuseBehind)
	}
	// A level node is admitted, drain latch or not: level IS drained, now.
	level := behind()
	level.Lag, level.LastSeq, level.Drained = ptr(0), ptr(100), false
	if ok, code := level.Established(true); !ok {
		t.Errorf("Established(strict) = (%v, %q) for a node level with the log, "+
			"want it admitted", ok, code)
	}
}

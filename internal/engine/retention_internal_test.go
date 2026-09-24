package engine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// probe answers a sequence's stored instant from a table, and reports which
// sequences the search actually asked about.
type probe struct {
	stored map[uint64]time.Time
	gone   map[uint64]bool
	fail   error
	asked  []uint64
}

func (p *probe) at(seq uint64) (time.Time, bool, error) {
	p.asked = append(p.asked, seq)
	if p.fail != nil {
		return time.Time{}, false, p.fail
	}
	if p.gone[seq] {
		return time.Time{}, false, nil
	}
	stored, held := p.stored[seq]
	return stored, held, nil
}

// TestTheAgeFloorNeverLetsTheTrimPassARecordInsideTheWindow is the invariant
// the age term exists for: `min_age` is a FLOOR on trimming, so every record
// younger than it must be at or above the answer.
//
// The inversion this protects against is reading `min_age` as a ceiling on
// retention, which trims exactly the records it was set to keep.
func TestTheAgeFloorNeverLetsTheTrimPassARecordInsideTheWindow(t *testing.T) {
	base := time.Date(2031, 4, 2, 3, 0, 0, 0, time.UTC)
	cutoff := base.Add(50 * time.Minute)

	stored := map[uint64]time.Time{}
	for seq := uint64(1); seq <= 100; seq++ {
		stored[seq] = base.Add(time.Duration(seq) * time.Minute)
	}
	p := &probe{stored: stored}

	got, err := ageFloorOf(1, 100, 100, cutoff, p.at)
	if err != nil {
		t.Fatalf("ageFloorOf: %v", err)
	}
	// Record 50 sits EXACTLY on the cutoff, so its age is exactly
	// `min_age` and it is kept: the floor is a floor, and a record whose
	// age has only just reached it is inside the window rather than past
	// it. Records 1..49 are strictly older and may go.
	if got != 50 {
		t.Fatalf("age floor = %d, want 50 — the trim would %s", got,
			map[bool]string{true: "delete a record inside min_age",
				false: "keep records it is allowed to remove"}[got > 50])
	}
	for seq := got; seq <= 100; seq++ {
		if stored[seq].Before(cutoff) {
			t.Fatalf("record %d is older than the cutoff and is at or above the "+
				"floor %d — the floor must be the FIRST record to keep", seq, got)
		}
	}
	// A BINARY SEARCH, not a walk: a hundred-record log must not cost a
	// hundred probes, or a year-five log costs a round trip per record
	// every quarter of an hour.
	if len(p.asked) > 10 {
		t.Fatalf("the search probed %d sequences over a span of 100 — it is "+
			"walking rather than bisecting", len(p.asked))
	}
}

// TestTheAgeFloorReadsATrimmedHoleAsOldRatherThanAsMissing protects the one
// case a naive search gets wrong: the log the age term runs over has already
// been purged below its own floor, so the range it bisects has holes in it.
//
// A hole read as "not older" stops the search early and returns a floor BELOW
// records the term should have permitted — the trim then never advances past
// the hole, which is a log that grows for ever with no term reporting a block.
func TestTheAgeFloorReadsATrimmedHoleAsOldRatherThanAsMissing(t *testing.T) {
	base := time.Date(2031, 4, 2, 3, 0, 0, 0, time.UTC)
	cutoff := base.Add(50 * time.Minute)

	stored := map[uint64]time.Time{}
	gone := map[uint64]bool{}
	for seq := uint64(1); seq <= 100; seq++ {
		if seq >= 20 && seq <= 40 {
			gone[seq] = true
			continue
		}
		stored[seq] = base.Add(time.Duration(seq) * time.Minute)
	}
	got, err := ageFloorOf(1, 100, 79, cutoff, (&probe{stored: stored, gone: gone}).at)
	if err != nil {
		t.Fatalf("ageFloorOf: %v", err)
	}
	if got != 50 {
		t.Fatalf("age floor = %d over a log with a purged hole, want 50: a hole "+
			"read as young stops the search inside it and the trim never "+
			"advances past it", got)
	}
}

// TestAnEmptyLogsAgeTermPermitsEverythingRatherThanBlocking is the boundary
// that would otherwise deadlock a fresh company: a log with no records has no
// record older than the floor, and a term that answered zero would block the
// trim on a log that has nothing to trim.
func TestAnEmptyLogsAgeTermPermitsEverythingRatherThanBlocking(t *testing.T) {
	got, err := ageFloorOf(0, 0, 0, time.Now(), func(uint64) (time.Time, bool, error) {
		t.Fatal("an empty log was probed")
		return time.Time{}, false, nil
	})
	if err != nil {
		t.Fatalf("ageFloorOf: %v", err)
	}
	if got != 1 {
		t.Fatalf("an empty log's age floor is %d, want 1 (its head plus one)", got)
	}
}

// TestAnUnreadableProbeRefusesRatherThanGuessingAFloor is the third value: a
// broker that could not answer is not a log whose records are all young, and a
// floor guessed from a failed probe is one the trim deletes against.
func TestAnUnreadableProbeRefusesRatherThanGuessingAFloor(t *testing.T) {
	want := errors.New("broker unreachable")
	if _, err := ageFloorOf(1, 100, 100, time.Now(),
		(&probe{fail: want}).at); !errors.Is(err, want) {
		t.Fatalf("err = %v, want the probe's own failure — a guessed floor here "+
			"deletes records against a log nobody could read", err)
	}
}

// floorFleet is a retention loop over the memory coordination twin.
func floorFleet(t *testing.T) *retention {
	t.Helper()
	return &retention{fleet: coordmem.NewFleet(), nodeID: "node-a"}
}

// TestABlockedTrimsClockSurvivesTheDutyMovingBetweenNodes is why
// `blocked_since` is published rather than held in memory.
//
// The trim is a fleet singleton on a lease. A value the holder kept would
// reset on every handover, so the twenty-four-hour backup condition — the one
// that turns "this company never backs up and therefore never trims" into
// something that reaches a person — would never be reached on a fleet whose
// lease flapped even once a day.
func TestABlockedTrimsClockSurvivesTheDutyMovingBetweenNodes(t *testing.T) {
	ctx := context.Background()
	r := floorFleet(t)
	blocked := statelog.TrimDecision{
		BlockedBy: statelog.TermBackupFloor,
		Terms:     []statelog.Term{{Name: statelog.TermBackupFloor}},
	}
	first := time.Date(2031, 4, 2, 3, 0, 0, 0, time.UTC)

	if err := r.publish(ctx, "tracker", 1, blocked, fleetInputs{
		at: first, previous: map[string]coord.TrimFloor{},
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	published, err := r.fleet.Floors(ctx)
	if err != nil || len(published) != 1 {
		t.Fatalf("floors = %v (err %v)", published, err)
	}
	if !published[0].BlockedSince.Equal(first) {
		t.Fatalf("blocked_since = %s, want %s", published[0].BlockedSince, first)
	}

	// THE DUTY MOVES: a different node, three hours later, reading the
	// floor its predecessor published.
	later := first.Add(3 * time.Hour)
	peer := &retention{fleet: r.fleet, nodeID: "node-b"}
	if err := peer.publish(ctx, "tracker", 1, blocked, fleetInputs{
		at: later, previous: map[string]coord.TrimFloor{"tracker": published[0]},
	}); err != nil {
		t.Fatalf("publish from the new holder: %v", err)
	}
	published, err = peer.fleet.Floors(ctx)
	if err != nil || len(published) != 1 {
		t.Fatalf("floors = %v (err %v)", published, err)
	}
	if !published[0].BlockedSince.Equal(first) {
		t.Fatalf("blocked_since = %s after the duty moved, want the original %s "+
			"— a clock that restarts on a handover never reaches the "+
			"twenty-four-hour condition", published[0].BlockedSince, first)
	}
	if published[0].By != "node-b" {
		t.Fatalf("the floor names %q as its writer, want node-b", published[0].By)
	}
}

// TestATrimThatAdvancesClearsItsBlockedClock is the other half: a stale
// instant left on an advancing domain ages into an alarm on a healthy fleet,
// which is the alarm nobody believes the second time.
func TestATrimThatAdvancesClearsItsBlockedClock(t *testing.T) {
	ctx := context.Background()
	r := floorFleet(t)
	first := time.Date(2031, 4, 2, 3, 0, 0, 0, time.UTC)
	was := coord.TrimFloor{
		Domain: "tracker", BlockedBy: string(statelog.TermBackupFloor),
		BlockedSince: first,
	}
	advancing := statelog.TrimDecision{
		To:    900,
		Terms: []statelog.Term{{Name: statelog.TermBackupFloor, Seq: 900, Known: true}},
	}
	if err := r.publish(ctx, "tracker", 1, advancing, fleetInputs{
		at: first.Add(time.Hour), previous: map[string]coord.TrimFloor{"tracker": was},
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	published, _ := r.fleet.Floors(ctx)
	if len(published) != 1 || published[0].Blocked() {
		t.Fatalf("an advancing trim published %+v", published)
	}
	if !published[0].BlockedSince.IsZero() {
		t.Fatalf("blocked_since = %s on an advancing trim — a stale instant here "+
			"ages into an alarm on a healthy fleet", published[0].BlockedSince)
	}
}

// TestATrimThatAdvancedAndBlockedAgainStartsANewClock is the boundary between
// the two above. The previous tick was NOT blocked, so this is a new block: a
// clock carried over from an older one would report a fleet as blocked for a
// day when it has been blocked for a minute.
//
// There is deliberately no case for "blocked further along than last time":
// [statelog.Trim] reports Blocked exactly when it permits removing up to zero,
// so a blocked tick's TrimTo is always 0 and that state does not exist.
func TestATrimThatAdvancedAndBlockedAgainStartsANewClock(t *testing.T) {
	ctx := context.Background()
	r := floorFleet(t)
	first := time.Date(2031, 4, 2, 3, 0, 0, 0, time.UTC)
	// The previous tick ADVANCED: no blocking term, no instant.
	was := coord.TrimFloor{Domain: "tracker", TrimTo: 100}
	now := first.Add(6 * time.Hour)

	if err := r.publish(ctx, "tracker", 1, statelog.TrimDecision{
		BlockedBy: statelog.TermApplied,
		Terms:     []statelog.Term{{Name: statelog.TermApplied}},
	}, fleetInputs{at: now, previous: map[string]coord.TrimFloor{"tracker": was}}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	published, _ := r.fleet.Floors(ctx)
	if len(published) != 1 || !published[0].BlockedSince.Equal(now) {
		t.Fatalf("blocked_since = %v after a trim that had been advancing "+
			"blocked, want %s", published, now)
	}
}

// TestABlockedTrimsClockSurvivesTheTermChangingUnderIt: a fleet whose blocking
// term rotates while the trim goes nowhere has been blocked throughout, and
// restarting the clock on each rotation hides the longest outages behind the
// noisiest ones.
func TestABlockedTrimsClockSurvivesTheTermChangingUnderIt(t *testing.T) {
	ctx := context.Background()
	r := floorFleet(t)
	first := time.Date(2031, 4, 2, 3, 0, 0, 0, time.UTC)
	was := coord.TrimFloor{
		Domain: "tracker", BlockedBy: string(statelog.TermBackupFloor),
		BlockedSince: first,
	}
	if err := r.publish(ctx, "tracker", 1, statelog.TrimDecision{
		BlockedBy: statelog.TermApplied,
		Terms:     []statelog.Term{{Name: statelog.TermApplied}},
	}, fleetInputs{
		at: first.Add(9 * time.Hour), previous: map[string]coord.TrimFloor{"tracker": was},
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	published, _ := r.fleet.Floors(ctx)
	if len(published) != 1 || !published[0].BlockedSince.Equal(first) {
		t.Fatalf("blocked_since = %v after the blocking term changed, want the "+
			"original %s", published, first)
	}
	if published[0].BlockedBy != string(statelog.TermApplied) {
		t.Fatalf("the floor names %q as its blocking term, want the current one",
			published[0].BlockedBy)
	}
}

// TestTheBackupTermFollowsTheOperatorsOwnPolicy is the difference between the
// two `backup_floor` values, and it is a difference in WHICH rows count and in
// nothing else.
//
// Under `operator` the engine's own copies do not satisfy the term, because
// the policy is "trim only what has left the host" and the engine cannot see
// that a copy has. A build that counted them anyway would trim against a
// backup the policy does not recognise.
func TestTheBackupTermFollowsTheOperatorsOwnPolicy(t *testing.T) {
	at := time.Date(2031, 4, 2, 3, 0, 0, 0, time.UTC)
	points := []coord.BackupPoint{
		{
			Owner: "node-a", At: at, Verified: true,
			Streams: map[string]coord.Position{
				"CREWLET_TRACKER_LOG": {Stream: "CREWLET_TRACKER_LOG", Seq: 900},
			},
		},
		{
			Owner: coord.OperatorBackupOwner, At: at.Add(-time.Hour), Verified: true,
			Streams: map[string]coord.Position{
				"CREWLET_TRACKER_LOG": {Stream: "CREWLET_TRACKER_LOG", Seq: 400},
			},
		},
	}
	engineFloor := &retention{cfg: config.TrackerRetention{
		BackupFloor: config.BackupFloorEngine,
	}}
	seq, _, _, have := engineFloor.backupTerm(points, "CREWLET_TRACKER_LOG")
	if !have || seq != 900 {
		t.Fatalf("under `engine` the term is (%d, %v), want the newest copy at 900",
			seq, have)
	}
	operatorFloor := &retention{cfg: config.TrackerRetention{
		BackupFloor: config.BackupFloorOperator,
	}}
	seq, _, _, have = operatorFloor.backupTerm(points, "CREWLET_TRACKER_LOG")
	if !have || seq != 400 {
		t.Fatalf("under `operator` the term is (%d, %v), want the acknowledgement "+
			"at 400 — counting the engine's own copy trims against a backup "+
			"the policy does not recognise", seq, have)
	}
}

// TestAnUnverifiedBackupSatisfiesNoPolicy is the safety half of the same term:
// a file that exists and was never opened is not a backup, and deleting the
// log's only copy of a record against one is what the whole term prevents.
func TestAnUnverifiedBackupSatisfiesNoPolicy(t *testing.T) {
	r := &retention{}
	_, _, _, have := r.backupTerm([]coord.BackupPoint{{
		Owner: "node-a", At: time.Now(), Verified: false,
		Streams: map[string]coord.Position{"S": {Stream: "S", Seq: 900}},
	}}, "S")
	if have {
		t.Fatal("an unverified backup satisfied the backup term — the trim would " +
			"delete the log against a copy nobody opened")
	}
}

// TestABackupThatDoesNotCoverALogIsNotABackupAtZero is the three-valued edge:
// a fleet whose backup covers the tracker and not the vectors must BLOCK the
// vector log's trim, not permit it up to sequence zero.
func TestABackupThatDoesNotCoverALogIsNotABackupAtZero(t *testing.T) {
	r := &retention{}
	seq, _, _, have := r.backupTerm([]coord.BackupPoint{{
		Owner: "node-a", At: time.Now(), Verified: true,
		Streams: map[string]coord.Position{
			"CREWLET_TRACKER_LOG": {Stream: "CREWLET_TRACKER_LOG", Seq: 900},
		},
	}}, "CREWLET_TRACKER_VECTORS")
	if have {
		t.Fatalf("a backup covering another log answered (%d, true) for the "+
			"vectors — an absent term must block rather than permit", seq)
	}
}

// THE SEARCH ALARMS' INPUTS ARE ACTUALLY FED.
//
// `search_scoped`, `search_degraded` and `search_slow` are each a property of
// the answers this node gave, and the alarm table can only read them off a
// [statelog.Reading]. An input nothing records leaves its alarm permanently
// silent while its row and its condition are both right — which is
// indistinguishable from a node with nothing wrong.
func TestTheSearchAlarmsReadWhatTheAnswersActuallyCovered(t *testing.T) {
	t.Parallel()
	recorder, err := metrics.New()
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	e := &Engine{metrics: recorder}

	// Four answers: one over part of the corpus, one without its semantic
	// half, two complete.
	e.reportSearch(search.Answer{BucketsAnswered: 64}, 10*time.Millisecond)
	e.reportSearch(search.Answer{BucketsAnswered: 64}, 12*time.Millisecond)
	e.reportSearch(search.Answer{
		BucketsAnswered: 32, BucketsMissing: 32, Absent: []string{"node-b"},
	}, 15*time.Millisecond)
	e.reportSearch(search.Answer{BucketsAnswered: 64, SemanticSkipped: true},
		11*time.Millisecond)

	r := &retention{metrics: recorder}
	var reading statelog.Reading
	r.observed(&reading)

	if got := reading.SearchScopedFraction; got != 0.25 {
		t.Errorf("one of four answers was over part of the corpus and the "+
			"reading says %.3f — a fraction nobody counts is an alarm that "+
			"never fires", got)
	}
	if got := reading.SearchDegradedFraction; got != 0.25 {
		t.Errorf("one of four answers ran without its semantic half and the "+
			"reading says %.3f", got)
	}
	if reading.SearchP95 <= 0 {
		t.Error("four measured searches left the p95 at zero, so `search_slow` " +
			"has no input and cannot fire")
	}

	// AND THE ALARM TABLE ACTUALLY FIRES ON THEM, which is the half a
	// reading alone does not prove: a field filled in and never read is
	// the same silence with an extra step.
	fired := map[statelog.Kind]bool{}
	for _, alarm := range statelog.Evaluate(reading) {
		fired[alarm.Kind] = true
	}
	for _, kind := range []statelog.Kind{
		statelog.KindSearchScoped, statelog.KindSearchDegraded,
	} {
		if !fired[kind] {
			t.Errorf("%s did not fire on a reading that says it should", kind)
		}
	}
}

// A LEXICAL-ONLY COMPANY IS NOT A DEGRADED ONE.
//
// A company that has configured no embeddings provider runs every search
// without a semantic half by request. Counting those as degraded would fire
// `search_degraded` at 100% for the life of the deployment, which is an alarm
// nobody reads and therefore an alarm that no longer reports the real thing.
func TestASearchWithNoSemanticHalfAskedForIsNotDegraded(t *testing.T) {
	t.Parallel()
	recorder, err := metrics.New()
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	e := &Engine{metrics: recorder}
	for range 5 {
		e.reportSearch(search.Answer{BucketsAnswered: 64}, time.Millisecond)
	}
	var reading statelog.Reading
	(&retention{metrics: recorder}).observed(&reading)
	if reading.SearchDegradedFraction != 0 {
		t.Fatalf("a company running lexical search by choice reports %.2f of "+
			"its answers degraded", reading.SearchDegradedFraction)
	}
}

// THE SEARCH ROSTER IS WHO IS ALIVE, NOT WHO HELD THE LOG BACK.
//
// The two registers a fleet keeps differ in exactly the way that matters here:
// a position row is held by every node the trim has to wait for, INCLUDING one
// that has been gone for hours, while a presence lease expires. A dead node on
// the roster is handed a bucket range nobody scans, so every search on this
// node reports a partial answer for as long as that row survives — and an
// operator chasing a phantom missing slice is worse off than one with no
// report at all.
func TestTheSearchRosterIsWhoIsAliveRatherThanWhoHeldTheLogBack(t *testing.T) {
	t.Parallel()
	backend := coordmem.New()
	e := &Engine{backends: &Backends{Coord: backend}}

	for _, id := range []string{"node-b", "node-a"} {
		if _, err := backend.TryAcquire(t.Context(), coord.NodeResource(id),
			coord.AcquireOptions{
				Owner: id + ":1", TTL: time.Minute,
				Meta: map[string]any{"roles": []string{"seats"}},
			}); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	// AND ONE THAT IS GONE. Its lease has expired, so it is not a
	// participant — where the positions register would still name it.
	if _, err := backend.TryAcquire(t.Context(), coord.NodeResource("node-dead"),
		coord.AcquireOptions{
			Owner: "node-dead:1", TTL: time.Nanosecond,
			Meta: map[string]any{"roles": []string{"seats"}},
		}); err != nil {
		t.Fatalf("register the dead node: %v", err)
	}

	roster, err := e.searchRoster(t.Context())
	if err != nil {
		t.Fatalf("roster: %v", err)
	}
	slices.Sort(roster)
	if want := []string{"node-a", "node-b"}; !slices.Equal(roster, want) {
		t.Fatalf("the roster is %v, want %v — a node on it that cannot answer "+
			"costs every search a bucket range nobody scans", roster, want)
	}

	// AND THE DIVISION OVER IT COVERS THE CORPUS EXACTLY ONCE, which is
	// what makes the roster the input rather than a display value.
	covered := 0
	for _, a := range search.Divide(roster) {
		covered += a.Shards.Width()
	}
	if covered != search.SearchShards {
		t.Fatalf("the roster divides into %d buckets, not %d", covered,
			search.SearchShards)
	}
}

// AN ALARM THAT CANNOT GO OUT IS AN ALARM AN OPERATOR LEARNS TO IGNORE.
//
// Every reading here is a THRESHOLD, and a threshold against a counter that
// only grows latches. `search_degraded` fires on a fraction being above zero,
// so computed from [metrics.Recorder.Read]'s cumulative totals one degraded
// search after boot would light it for the life of the process; `search_slow`
// and `barrier_slow` take a p95, and a p95 over every observation since boot
// dilutes a bad hour and never forgets it. [metrics.Window] is what bounds
// each reading to the last day.
//
// `crewlet retention status` derives its exit code from these, so an alarm
// that cannot clear is a cron that fires for ever.
//
// THE ASSERTION IS THAT TIME PASSING CLEARS IT. That is the property the
// window buys and the one no snapshot test can express, which is why the
// clock is injected rather than waited out.
func TestAnAlarmClearsOnceItsHourRollsOutOfTheWindow(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	recorder, err := metrics.New()
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	recorder = recorder.WithClock(func() time.Time { return at })
	e := &Engine{metrics: recorder}
	r := &retention{metrics: recorder}

	// One degraded search, and three good ones, all in one hour.
	e.reportSearch(search.Answer{BucketsAnswered: 64, SemanticSkipped: true},
		11*time.Millisecond)
	for range 3 {
		e.reportSearch(search.Answer{BucketsAnswered: 64}, 10*time.Millisecond)
	}

	var lit statelog.Reading
	r.observed(&lit)
	if lit.SearchDegradedFraction <= 0 {
		t.Fatal("a degraded search left the fraction at zero, so this case " +
			"proves nothing about clearing")
	}
	if !firedKind(statelog.Evaluate(lit), statelog.KindSearchDegraded) {
		t.Fatal("search_degraded did not fire on a degraded search")
	}

	// A DAY LATER WITH NOTHING SINCE. The hour that held it has rolled out.
	at = at.Add(metrics.Buckets*time.Hour + time.Hour)
	var cleared statelog.Reading
	r.observed(&cleared)
	if cleared.SearchDegradedFraction != 0 {
		t.Errorf("SearchDegradedFraction = %.3f a day after the last degraded "+
			"search, so the alarm is reading a cumulative counter and can "+
			"never go out", cleared.SearchDegradedFraction)
	}
	if firedKind(statelog.Evaluate(cleared), statelog.KindSearchDegraded) {
		t.Error("search_degraded is still lit a day after the search that lit it")
	}
	if cleared.SearchP95 != 0 {
		t.Errorf("SearchP95 = %v a day after the last search, so `search_slow` "+
			"is reading a distribution with no window", cleared.SearchP95)
	}
}

// THE ALARM TABLE IS EVALUATED ON EVERY PASS, AND THE TRIM ON THE PASSES A TRIM
// INTERVAL APART.
//
// An alarm is only as prompt as the look that raises it: evaluated only on the
// passes that trim, an operator would hear of a condition up to a trim interval
// after it began. The trim is a burst of purges behind a duty claim, sized in
// quarter hours; taken on every alarm pass it would ask for the lease every
// fifteen seconds for nothing the lease allows.
//
// Mutation: evaluate only on the passes that trim and the second and third
// observe nothing; trim on every pass, or stamp a pass only when the claim is
// won, and a node holding no duty asks for it on every pass.
func TestTheAlarmTableIsEvaluatedOnEveryPassAndTheTrimOnItsOwnInterval(t *testing.T) {
	t.Parallel()
	evaluations, claims := 0, 0
	r := &retention{
		fleet:  coordmem.NewFleet(),
		state:  &stateLog{},
		nodeID: "node-a",
		// THE TRACKER'S CLOCK IS READ ONCE PER OBSERVATION, which is what
		// counts the evaluations.
		alarms: statelog.NewTracker(nil, func() time.Time {
			evaluations++
			return time.Now().UTC()
		}),
		pooled: map[string]poolCounters{},
		claim: func(context.Context) (bool, error) {
			claims++
			return false, nil
		},
	}
	for range 3 {
		r.tick(t.Context())
	}
	if evaluations != 3 {
		t.Errorf("three passes evaluated the table %d time(s), want 3", evaluations)
	}
	if claims != 1 {
		t.Errorf("three passes inside one trim interval asked for the duty %d "+
			"time(s), want 1 — on the first", claims)
	}

	// ONE TRIM INTERVAL AFTER THE PASS THAT TOOK IT, the next pass trims.
	r.trimmedAt = r.trimmedAt.Add(-RetentionInterval)
	r.tick(t.Context())
	if claims != 2 {
		t.Errorf("a pass a trim interval after the last one asked for the duty %d "+
			"time(s) in all, want 2", claims)
	}
}

// THE REPORT CARRIES A RUN OF REFUSED READS TO THE ALARM.
//
// `read_refusals` is judged on [statelog.Reading.RefusalsSince], and the one
// thing that fills it is [series] asking the domain's own reader how long its
// reads have been refused. A reading assembled without that question evaluates
// a table in which reads never refuse, on a node refusing every one — so the
// case refuses reads through the reader a running domain carries, and hands
// that domain to the function the report hands it to.
//
// Mutation: drop the reader from [series] and the run reaches no reading.
func TestTheReportCarriesARunOfRefusedReadsToTheAlarm(t *testing.T) {
	t.Parallel()
	began := time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)
	clock := began
	reader, err := statelog.NewReader(statelog.ReaderDeps{
		Domain: tracker.Domain{},
		DB:     untouchedStore{t},
		Waiter: stillWaiter{},
		// A STALLED APPLIER, which a read refuses on this node's own
		// health before it reads or appends anything.
		Health: func() statelog.Health {
			return statelog.Health{Stalled: true,
				Floor: statelog.Floor{State: statelog.FloorOK, ReadAt: clock}}
		},
		Now: func() time.Time { return clock },
	})
	if err != nil {
		t.Fatalf("build the reader: %v", err)
	}
	refuse := func() {
		t.Helper()
		_, err := reader.Read(t.Context(), statelog.Query{Level: statelog.ReadLinearizable},
			func(*sql.Tx) error { return nil })
		var refused *statelog.Refused
		if !errors.As(err, &refused) || refused.Code != statelog.RefuseStalled {
			t.Fatalf("a read on a stalled applier answered %v, want a %s refusal",
				err, statelog.RefuseStalled)
		}
	}
	running := &runningDomain{domain: tracker.Domain{}, reader: reader}

	// ONE REFUSAL IS NOT A RUN WORTH AN ALARM.
	refuse()
	var first statelog.Reading
	series(running, clock, &first)
	if firedKind(statelog.Evaluate(first), statelog.KindReadRefusals) {
		t.Errorf("one refused read raised %s (refusing for %v)",
			statelog.KindReadRefusals, first.RefusalsSince)
	}

	// A RUN LONGER THAN THE ALARM'S THRESHOLD IS.
	clock = began.Add(coord.ReconcileInterval + time.Second)
	refuse()
	var reading statelog.Reading
	series(running, clock, &reading)
	if reading.RefusalsSince != coord.ReconcileInterval+time.Second {
		t.Errorf("reads refused from %v to %v reached the reading as a run of %v, "+
			"want %v", began, clock, reading.RefusalsSince,
			coord.ReconcileInterval+time.Second)
	}
	if !firedKind(statelog.Evaluate(reading), statelog.KindReadRefusals) {
		t.Errorf("reads refused for %v did not raise %s", reading.RefusalsSince,
			statelog.KindReadRefusals)
	}
}

// AN UNREADABLE EVICTION TABLE IS WRITTEN DOWN ONCE A TRIM INTERVAL, and at once
// again after a read of it has answered.
//
// Every report reads the table, and one is assembled on every alarm pass as
// well as on every operator request — so a table that stays unreadable,
// written down on every read, repeats one WARN on every node four times a
// minute. Once a [RetentionInterval] says it as often as the trim that the
// missing tombstones hold back runs. A read that answers clears the stamp, so a
// failure after it is news and is written at once.
//
// Mutation: drop the stamp check and the second read writes a second line;
// drop the clear on a read that answers and the failure after it writes
// nothing.
func TestAnUnreadableEvictionTableIsWrittenDownOnceATrimInterval(t *testing.T) {
	t.Parallel()
	healthy := freshStore(t)
	broken := freshStore(t)
	if _, err := broken.Replicated().SQL().ExecContext(t.Context(),
		`DROP TABLE tracker_evictions`); err != nil {
		t.Fatalf("drop the eviction table: %v", err)
	}
	var out bytes.Buffer
	r := &retention{logger: slog.New(slog.NewJSONHandler(&out, nil))}
	running := &runningDomain{domain: tracker.Domain{}}
	read := func(db *store.DB, want int, when string) {
		t.Helper()
		r.db = db
		if got := r.tombstones(t.Context(), running, 1); len(got) != 0 {
			t.Fatalf("%s: a table holding no evictions answered %v", when, got)
		}
		if got := countLogged(t, &out, slog.LevelWarn,
			"retention_evictions_unreadable"); got != want {
			t.Errorf("%s: %d line(s) written in all, want %d", when, got, want)
		}
	}

	read(broken, 1, "the first read that fails")
	read(broken, 1, "a second read inside the interval")
	r.evictionsWarnedAt = r.evictionsWarnedAt.Add(-RetentionInterval)
	read(broken, 2, "a read one interval after the last line")
	read(healthy, 2, "a read that answers")
	read(broken, 3, "the first failure after a read that answered")
}

// EACH GATED LOG'S TOMBSTONES COME FROM ITS OWN APPLIER'S ROWS.
//
// An eviction is one record on every log whose applier installs the gate, and
// each applier keeps its own table — so a log stops counting an evicted node
// once ITS record has applied, whatever the other log's table says. Read from
// the tracker's table alone, a node evicted from the fleet pinned the knowledge
// base's floor for ever. A log whose applier installs no gate has no say.
//
// Mutation: read `tracker_evictions` for every domain and the pages log
// answers no tombstone for the node evicted on it.
func TestEachGatedLogsTombstonesComeFromItsOwnRows(t *testing.T) {
	t.Parallel()
	db := freshStore(t)
	at := time.Date(2031, 4, 2, 3, 0, 0, 0, time.UTC)
	if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		for _, stmt := range []string{
			`INSERT INTO pages_evictions (node_id, at, by, from_position, version)
			 VALUES ('node-pages', ?, 'ops', 7, 7)`,
			`INSERT INTO pages_evictions
			 (node_id, at, by, from_position, readmitted_position, version)
			 VALUES ('node-back', ?, 'ops', 7, 9, 9)`,
			`INSERT INTO tracker_evictions (node_id, log_stream, from_position, at, by)
			 VALUES ('node-tracker', 'CREWLET_TRACKER_LOG', 5, ?, 'ops')`,
		} {
			if _, err := tx.ExecContext(t.Context(), stmt, store.EncodeTime(at)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the evictions: %v", err)
	}
	r := &retention{db: db, logger: slog.New(slog.DiscardHandler)}
	nodes := func(d statelog.Domain) []string {
		var out []string
		for _, tomb := range r.tombstones(t.Context(), &runningDomain{domain: d}, 1) {
			out = append(out, tomb.NodeID)
		}
		return out
	}
	if got := nodes(pages.Domain{}); !slices.Equal(got, []string{"node-pages"}) {
		t.Errorf("the pages log's tombstones are %v, want the node evicted on it "+
			"and not the one readmitted there", got)
	}
	if got := nodes(tracker.Domain{}); !slices.Equal(got, []string{"node-tracker"}) {
		t.Errorf("the tracker log's tombstones are %v, want node-tracker", got)
	}
	if got := nodes(search.Domain{}); len(got) != 0 {
		t.Errorf("the vector log, whose applier installs no gate, answered %v", got)
	}
}

// AN EVICTION LANDS ON EVERY GATED LOG, AND ONLY FOR A NODE THAT IS SILENT.
//
// Each applier drops an evicted node's records by its own domain's table, so a
// gesture that recorded the eviction on one log left the node's writes to the
// other applying on every peer. And the permission the whole design rests on —
// an eviction only once the node has stopped reaching coordination — is
// checked by the gesture itself rather than stated beside it.
//
// Mutation: skip the pages log and its table never names the node; drop the
// presence check and a node holding a live lease is evicted unforced.
func TestAnEvictionLandsOnEveryGatedLog(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
	b.Store.Path = t.TempDir() + "/crewlet.db"
	b.Stream.StoreDir = t.TempDir() + "/stream"
	cfg, err := config.ParseCompany([]byte(nativeCleanupCompany))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	back, err := OpenBackends(t.Context(), &b, cfg)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	e, err := New(t.Context(), Options{Bootstrap: &b, Company: cfg, Backends: back})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })

	// A NODE STILL REACHING COORDINATION IS REFUSED UNFORCED.
	if _, err := back.Coord.TryAcquire(t.Context(), coord.NodeResource("node-live"),
		coord.AcquireOptions{Owner: "node-live", TTL: time.Minute}); err != nil {
		t.Fatalf("take node-live's presence: %v", err)
	}
	_, err = e.EvictNode(t.Context(), GateRequest{
		Node: "node-live", OpID: "op-live", By: "ops",
	})
	var refused *statelog.EvictionRefusal
	if !errors.As(err, &refused) {
		t.Fatalf("evicting a node that holds a live presence lease = %v, want "+
			"the permission refused", err)
	}

	results, err := e.EvictNode(t.Context(), GateRequest{
		Node: "node-gone", OpID: "op-gone", By: "ops",
	})
	if err != nil {
		t.Fatalf("EvictNode: %v", err)
	}
	var domains []string
	for _, res := range results {
		domains = append(domains, res.Domain)
	}
	if !slices.Equal(domains, []string{tracker.Domain{}.Name(), pages.Domain{}.Name()}) {
		t.Fatalf("the eviction reached %v, want every log whose applier installs "+
			"the gate", domains)
	}
	evicted := func() (inTracker, inPages bool) {
		trackerRows, err := tracker.Evictions(t.Context(), back.Store.Replicated(),
			tracker.Domain{}.Stream().Name)
		if err != nil {
			t.Fatalf("read the tracker's evictions: %v", err)
		}
		pagesRows, err := pages.Evictions(t.Context(), back.Store.Replicated())
		if err != nil {
			t.Fatalf("read the pages log's evictions: %v", err)
		}
		gone := func(node string, back bool, by string) bool {
			return node == "node-gone" && !back && by == "ops"
		}
		return slices.ContainsFunc(trackerRows, func(r tracker.EvictionRow) bool {
				return gone(r.NodeID, r.IsBack, r.By)
			}), slices.ContainsFunc(pagesRows, func(r pages.EvictionRow) bool {
				return gone(r.NodeID, r.IsBack, r.By)
			})
	}
	waitUntil(t, 20*time.Second, "both logs to apply the eviction", func() bool {
		inTracker, inPages := evicted()
		return inTracker && inPages
	})

	if _, err := e.ReadmitNode(t.Context(), GateRequest{
		Node: "node-gone", OpID: "op-back", By: "ops",
	}); err != nil {
		t.Fatalf("ReadmitNode: %v", err)
	}
	waitUntil(t, 20*time.Second, "both logs to apply the readmission", func() bool {
		inTracker, inPages := evicted()
		return !inTracker && !inPages
	})
}

// freshStore is a node's two estates in a directory of their own.
func freshStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), t.TempDir()+"/index.db", store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// countLogged is how many lines a JSON handler wrote with one message at one
// level.
func countLogged(t *testing.T, out *bytes.Buffer, level slog.Level, msg string) int {
	t.Helper()
	count := 0
	for _, line := range bytes.Split(out.Bytes(), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var entry struct {
			Level string `json:"level"`
			Msg   string `json:"msg"`
		}
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("decode the log line %q: %v", line, err)
		}
		if entry.Level == level.String() && entry.Msg == msg {
			count++
		}
	}
	return count
}

// EACH LOG'S TRIM READS ITS OWN WAKE FEED.
//
// The feed term holds a log's trim at what that log's wake feed has
// acknowledged, and each domain's feed is a consumer group on its own log,
// under the name its translator declares. Asked for another domain's group, a
// log answers that no such consumer exists, which the term reads as a feed that
// has seen nothing — so the trim of that log would stay at zero for as long as
// the node runs.
//
// Mutation: read the tracker's group on every log and the knowledge base's
// term stays at zero after its feed has acknowledged two records.
func TestEachLogsTrimReadsItsOwnWakeFeed(t *testing.T) {
	t.Parallel()
	q, err := jetstream.Open(t.Context(), jetstream.Config{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open a broker: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
	r := &retention{}
	for _, tc := range []struct {
		domain statelog.Domain
		// group is the one the engine runs this domain's feed under.
		group string
	}{
		{tracker.Domain{}, tracker.NewTranslator().Source().Group},
		{pages.Domain{}, pages.NewTranslator(nil).Source().Group},
	} {
		spec := tc.domain.Stream()
		if err := q.EnsureDomainStream(t.Context(), jetstream.DomainStream{
			Name: spec.Name, Subjects: spec.Subjects, MaxBytes: 16 << 20,
			MaxPerSubject: spec.MaxPerSubject, Duplicates: spec.Duplicates,
		}); err != nil {
			t.Fatalf("provision %s: %v", spec.Name, err)
		}
		domainLog, err := q.DomainLog(t.Context(), spec.Name)
		if err != nil {
			t.Fatalf("open %s: %v", spec.Name, err)
		}
		for i := range 3 {
			if _, _, err := domainLog.Append(t.Context(),
				fmt.Sprintf("%s.probe.%d", spec.SubjectPrefix, i),
				fmt.Sprintf("op-%s-%d", tc.domain.Name(), i), nil,
				[]byte("record")); err != nil {
				t.Fatalf("append to %s: %v", spec.Name, err)
			}
		}
		// THE FEED ACKNOWLEDGES TWO of the three.
		feed, err := domainLog.Group(t.Context(), tc.group)
		if err != nil {
			t.Fatalf("open %s's feed: %v", tc.domain.Name(), err)
		}
		for range 2 {
			delivery, err := feed.Next(t.Context())
			if err != nil || delivery == nil {
				t.Fatalf("read %s's feed: %v", tc.domain.Name(), err)
			}
			if err := delivery.Ack(); err != nil {
				t.Fatalf("acknowledge on %s's feed: %v", tc.domain.Name(), err)
			}
		}
		running := &runningDomain{domain: tc.domain, log: domainLog}
		waitUntil(t, 10*time.Second, tc.domain.Name()+"'s trim to read its feed at 2",
			func() bool {
				seq, has, readable := r.feedTerm(t.Context(), running)
				return seq == 2 && has && readable
			})
		_ = feed.Stop()
	}
}

// untouchedStore is a database a read must never reach.
type untouchedStore struct{ t *testing.T }

func (s untouchedStore) Read(context.Context, func(*sql.Tx) error) error {
	s.t.Error("a read the node's own health refuses reached the database")
	return errors.New("this case's database is not there to be read")
}

// stillWaiter is an applier that has committed nothing and never will.
type stillWaiter struct{}

func (stillWaiter) Committed() statelog.Position { return statelog.Position{} }

func (stillWaiter) WaitCommitted(context.Context, statelog.Position) error {
	return errors.New("this case's applier does not move")
}

func (stillWaiter) WaitApplied(context.Context, statelog.ScopeSet, statelog.Position) error {
	return errors.New("this case's applier does not move")
}

// firedKind reports whether an evaluation raised one alarm.
func firedKind(alarms []statelog.Alarm, want statelog.Kind) bool {
	for _, a := range alarms {
		if a.Kind == want {
			return true
		}
	}
	return false
}

package engine

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
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
// The inversion this protects against is the one three readers of this design
// got backwards — reading `min_age` as a ceiling on retention, which trims
// exactly the records it was set to keep.
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
// [statelog.Reading]. Nothing recorded them for the life of the alarm table:
// the rows existed, the conditions were right, and all three were permanently
// silent — which is indistinguishable from a node with nothing wrong.
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

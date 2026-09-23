package engine

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
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

	if err := r.publish(ctx, "tracker", 1, blocked, 0, fleetInputs{
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
	if err := peer.publish(ctx, "tracker", 1, blocked, 0, fleetInputs{
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
	if err := r.publish(ctx, "tracker", 1, advancing, 0, fleetInputs{
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
	}, 0, fleetInputs{at: now, previous: map[string]coord.TrimFloor{"tracker": was}}); err != nil {
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
	}, 0, fleetInputs{
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

// TestTheFloorNeverMovesDownWithinAGeneration is what makes the published
// floor an upper bound on what is gone rather than a report of one tick.
//
// A tick's conclusion falls on every blocked tick and whenever the lowest
// counted node is below the log, and records an earlier tick licensed removing
// are gone all the same — so a floor that followed it down tells exactly that
// node it holds everything that may be missing, which is the one node the
// write fence exists to refuse.
func TestTheFloorNeverMovesDownWithinAGeneration(t *testing.T) {
	t.Parallel()
	at := func(gen uint32, floor uint64) coord.TrimFloor {
		return coord.TrimFloor{Domain: "tracker", Generation: gen, Floor: floor}
	}
	for name, tc := range map[string]struct {
		previous  coord.TrimFloor
		had       bool
		to, first uint64
		want      uint64
		refused   bool
	}{
		"the first tick there has ever been": {to: 900, first: 1, want: 900},
		"a blocked tick keeps what an earlier one licensed": {
			previous: at(1, 900), had: true, to: 0, first: 10, want: 900},
		"a conclusion the counted minimum dragged down": {
			previous: at(1, 900), had: true, to: 300, first: 10, want: 900},
		"an advancing tick raises it": {
			previous: at(1, 900), had: true, to: 1_200, first: 900, want: 1_200},
		"the stream has lost more than any tick licensed": {
			previous: at(1, 900), had: true, to: 0, first: 1_500, want: 1_500},
		"a floor from a previous generation is not carried": {
			previous: at(0, 900), had: true, to: 50, first: 40, want: 50},
		"a floor from a generation ahead refuses": {
			previous: at(2, 900), had: true, to: 50, first: 40, refused: true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := nextFloor(tc.previous, tc.had, 1, tc.to, tc.first)
			if (err != nil) != tc.refused {
				t.Fatalf("nextFloor = (%d, %v), want refused=%v", got, err, tc.refused)
			}
			if !tc.refused && got != tc.want {
				t.Fatalf("nextFloor(previous %d, to %d, first %d) = %d, want %d",
					tc.previous.Floor, tc.to, tc.first, got, tc.want)
			}
		})
	}
}

// TestABlockedTickPublishesTheFloorAnEarlierTickLicensed is the same
// property through the register, where a reader actually meets it: after an
// advance and a block, the conclusion reads zero and the floor reads what the
// advance licensed.
func TestABlockedTickPublishesTheFloorAnEarlierTickLicensed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := floorFleet(t)
	now := time.Date(2031, 4, 2, 3, 0, 0, 0, time.UTC)
	if err := r.publish(ctx, "tracker", 1, statelog.TrimDecision{
		To:    900,
		Terms: []statelog.Term{{Name: statelog.TermApplied, Seq: 900, Known: true}},
	}, 1, fleetInputs{at: now, previous: map[string]coord.TrimFloor{}}); err != nil {
		t.Fatalf("publish the advance: %v", err)
	}
	advanced, err := r.fleet.Floors(ctx)
	if err != nil || len(advanced) != 1 {
		t.Fatalf("floors = %v (err %v)", advanced, err)
	}
	if err := r.publish(ctx, "tracker", 1, statelog.TrimDecision{
		BlockedBy: statelog.TermApplied,
		Terms:     []statelog.Term{{Name: statelog.TermApplied, Known: true}},
	}, 1, fleetInputs{
		at: now.Add(RetentionInterval), previous: map[string]coord.TrimFloor{"tracker": advanced[0]},
	}); err != nil {
		t.Fatalf("publish the block: %v", err)
	}
	floors, err := r.fleet.Floors(ctx)
	if err != nil || len(floors) != 1 {
		t.Fatalf("floors = %v (err %v)", floors, err)
	}
	if floors[0].TrimTo != 0 || floors[0].Floor != 900 {
		t.Fatalf("a blocked tick after an advance to 900 published trim_to %d and "+
			"floor %d, want 0 and 900 — records below 900 may be gone, and a floor "+
			"that says zero clears every node to publish over them",
			floors[0].TrimTo, floors[0].Floor)
	}
	if got, err := floorFor(floors, "tracker", 1); err != nil || got != 900 {
		t.Fatalf("the fence reads a floor of %d (err %v) after the block, want 900",
			got, err)
	}
}

// fleetRegister is [coord.Fleet] under a name that does not collide with the
// interface's own Fleet method, so a wrapper can embed it.
type fleetRegister = coord.Fleet

// orderFleet is a register that notes what the log still held at the instant
// a floor reached it, and can refuse the write.
type orderFleet struct {
	fleetRegister
	stream *jetstream.DomainLog
	refuse error

	// firstAt is the log's first surviving sequence when PutFloor ran.
	firstAt uint64
	puts    int
}

func (f *orderFleet) PutFloor(ctx context.Context, row coord.TrimFloor) error {
	first, _, err := f.stream.Bounds(ctx)
	if err != nil {
		return err
	}
	f.firstAt, f.puts = first, f.puts+1
	if f.refuse != nil {
		return f.refuse
	}
	return f.fleetRegister.PutFloor(ctx, row)
}

// trimmedTracker is one node on a real embedded broker with its own trim
// stopped, and three records on the tracker's log — so every purge and every
// floor a case sees is the case's own, and a purge has something to remove.
func trimmedTracker(t *testing.T) (*Engine, *Backends, *runningDomain) {
	t.Helper()
	b := config.DefaultBootstrap()
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
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
	waitUntil(t, 20*time.Second, "the node to admit seats", e.NativeHydrated)
	// THE NODE'S OWN TRIM IS THE OTHER WRITER of both the floor and the
	// log's first sequence; stopped, it waits out an in-flight tick.
	e.stopRetention()
	running := e.native.log.Domain(tracker.Domain{}.Name())
	if running == nil {
		t.Fatal("the tracker domain is not running")
	}
	for range 3 {
		barrierOn(t, running)
	}
	return e, back, running
}

// barrierOn puts one record on a domain's log that no gate drops and no
// build refuses to decode, and answers its sequence.
//
// IN THE DOMAIN'S OWN ENCODING, through the engine's own [barrierEncoder]: a
// tracker-shaped barrier on the knowledge base's log is a record that log's
// envelope decoder need not accept, and a case would be measuring that
// instead.
func barrierOn(t *testing.T, running *runningDomain) uint64 {
	t.Helper()
	encode := barrierEncoder(running.domain)
	if encode == nil {
		t.Fatalf("%s has no barrier encoding to put a record on its log with",
			running.domain.Name())
	}
	body, err := encode(statelog.Envelope{
		V: statelog.BarrierVersion, Kind: statelog.BarrierKind,
		Subject: statelog.Subject{Kind: statelog.BarrierKind},
		Gen:     running.runner.Committed().Generation,
		Scope:   statelog.ScopeSet{Paths: []string{statelog.BarrierScope}},
	})
	if err != nil {
		t.Fatalf("encode a barrier: %v", err)
	}
	seq, _, err := running.log.Append(t.Context(),
		running.domain.Stream().SubjectPrefix+"."+statelog.BarrierKind, "", nil, body)
	if err != nil {
		t.Fatalf("append a barrier: %v", err)
	}
	return seq
}

// TestTheFloorIsPublishedBeforeThePurgeItLicenses is the order the floor
// theorem assumes, on a real stream.
//
// The write fence clears an expectation of zero against the published floor,
// so every record the log has lost must already sit below it. Purged first,
// the floor trails the purge by however long its write takes to succeed, and a
// node the purge just left below the log is cleared against the old number —
// publishing at zero over a record it never applied.
func TestTheFloorIsPublishedBeforeThePurgeItLicenses(t *testing.T) {
	t.Parallel()
	_, back, running := trimmedTracker(t)
	first, last, err := running.log.Bounds(t.Context())
	if err != nil {
		t.Fatalf("bounds: %v", err)
	}
	if last < first+2 {
		t.Fatalf("the log holds %d..%d, want at least three records to purge", first, last)
	}
	fleet := &orderFleet{fleetRegister: back.Fleet, stream: running.log}
	r := &retention{fleet: fleet, nodeID: "node-a"}
	name := running.domain.Name()
	to := last
	if err := r.apply(t.Context(), running.log, name, running.runner.Committed().Generation,
		statelog.TrimDecision{
			To:    to,
			Terms: []statelog.Term{{Name: statelog.TermApplied, Seq: to, Known: true}},
		}, first, fleetInputs{at: time.Now().UTC(), previous: map[string]coord.TrimFloor{}}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if fleet.puts != 1 {
		t.Fatalf("the tick published %d floor(s), want 1", fleet.puts)
	}
	if fleet.firstAt >= to {
		t.Fatalf("the floor reached the register when the log already started at %d "+
			"— the purge below %d had run first, so a write fence reading the "+
			"register in between cleared nodes the purge had left below the log",
			fleet.firstAt, to)
	}
	floors, err := back.Fleet.Floors(t.Context())
	if err != nil {
		t.Fatalf("floors: %v", err)
	}
	published, err := floorFor(floors, name, running.runner.Committed().Generation)
	if err != nil || published < to {
		t.Fatalf("the published floor is %d (err %v), want at least the %d it licensed",
			published, err, to)
	}
	if now, _, err := running.log.Bounds(t.Context()); err != nil || now != to {
		t.Fatalf("after the tick the log starts at %d (err %v), want %d — the "+
			"purge the floor licensed did not run", now, err, to)
	}
}

// TestAFloorThatCouldNotBePublishedPurgesNothing is the failure half of the
// same order. Purge-then-publish did not leave a window one tick wide: a purge
// whose publish then FAILED left the register naming the old floor for as long
// as the register refused writes, and every write fence in the fleet cleared
// against it.
func TestAFloorThatCouldNotBePublishedPurgesNothing(t *testing.T) {
	t.Parallel()
	_, back, running := trimmedTracker(t)
	first, last, err := running.log.Bounds(t.Context())
	if err != nil {
		t.Fatalf("bounds: %v", err)
	}
	fleet := &orderFleet{fleetRegister: back.Fleet, stream: running.log,
		refuse: errors.New("the register refuses writes")}
	r := &retention{fleet: fleet, nodeID: "node-a"}
	err = r.apply(t.Context(), running.log, running.domain.Name(),
		running.runner.Committed().Generation, statelog.TrimDecision{
			To:    last,
			Terms: []statelog.Term{{Name: statelog.TermApplied, Seq: last, Known: true}},
		}, first, fleetInputs{at: time.Now().UTC(), previous: map[string]coord.TrimFloor{}})
	if err == nil {
		t.Fatal("a tick whose floor could not be published reported success")
	}
	if now, _, err := running.log.Bounds(t.Context()); err != nil || now != first {
		t.Fatalf("the log starts at %d (err %v) after a tick whose floor was never "+
			"published, want the %d it held — the purge ran on a licence nobody "+
			"can read", now, err, first)
	}
}

// TestEachLogsTrimWaitsOnItsOwnWakeFeed is the feed term read against every
// registered log's own consumer, on a real stream.
//
// The trim used to name the TRACKER's group on every log with a feed. On the
// knowledge base's log that consumer never exists, so the lookup answered
// "never opened", the term permitted nothing, and that log was blocked on
// `feed_ack_floor` for the life of the deployment — while its own feed
// acknowledged every record it was handed. So each log's own feed
// acknowledges a record here, every other term is satisfied, and the tick
// has to trim up to that record; a log with no feed must report the term
// absent rather than borrow somebody else's.
func TestEachLogsTrimWaitsOnItsOwnWakeFeed(t *testing.T) {
	t.Parallel()
	e, back, _ := trimmedTracker(t)
	// MIN_AGE AT A NANOSECOND, so the age term permits everything already
	// written and the tick is decided by the other five. Set on the loop
	// rather than through validated config: the 24-hour floor exists for
	// an operator, and this case is about a different term.
	r := &retention{fleet: back.Fleet, state: e.native.log, nodeID: "node-a",
		cfg: config.TrackerRetention{MinAgeRaw: "1ns"}}
	for _, name := range e.native.log.order {
		running := e.native.log.Domain(name)
		t.Run(name, func(t *testing.T) {
			group := running.domain.FeedGroup()
			if group == "" {
				if _, has, _ := r.feedTerm(t.Context(), running); has {
					t.Fatalf("%s declares no wake feed and its trim waits on one "+
						"anyway — a consumer nobody opens on this log", name)
				}
				return
			}
			at := barrierOn(t, running)
			var acked uint64
			waitUntil(t, 20*time.Second, name+"'s own feed to acknowledge the record",
				func() bool {
					floor, exists, err := running.log.GroupAckFloor(t.Context(), group)
					acked = floor
					return err == nil && exists && floor >= at
				})
			waitUntil(t, 20*time.Second, name+"'s applier to commit the record",
				func() bool { return running.runner.Committed().Seq >= at })

			seq, has, readable := r.feedTerm(t.Context(), running)
			if !has || !readable || seq < acked {
				t.Fatalf("%s's feed term is (%d, has %v, readable %v), want its own "+
					"feed %q's acknowledgement at %d — the term is reading a "+
					"consumer the wakes on this log never advance",
					name, seq, has, readable, group, acked)
			}

			committed := running.runner.Committed()
			stream := running.domain.Stream().Name
			now := time.Now().UTC()
			floors, err := back.Fleet.Floors(t.Context())
			if err != nil {
				t.Fatalf("floors: %v", err)
			}
			previous := map[string]coord.TrimFloor{}
			for _, f := range floors {
				previous[f.Domain] = f
			}
			// EVERY OTHER TERM SATISFIED UP TO THE RECORD: this node
			// counted at its own committed position, a verified backup
			// covering the record, no holds, and a solo fleet's snapshot
			// term — so a tick that still refuses is refusing on the feed.
			shared := fleetInputs{
				at: now, readable: true, previous: previous,
				positions: []coord.NodePositions{{
					NodeID: "node-a", At: now,
					Domains: map[string]coord.DomainPosition{name: {
						Generation: committed.Generation, Seq: committed.Seq,
					}},
				}},
				backups: []coord.BackupPoint{{
					Owner: "node-a", At: now, Verified: true,
					Streams: map[string]coord.Position{stream: {
						Stream: stream, Generation: committed.Generation, Seq: at,
					}},
				}},
			}
			if err := r.domain(t.Context(), name, shared); err != nil {
				t.Fatalf("the tick on %s: %v", name, err)
			}
			floors, err = back.Fleet.Floors(t.Context())
			if err != nil {
				t.Fatalf("floors: %v", err)
			}
			var row coord.TrimFloor
			for _, f := range floors {
				if f.Domain == name {
					row = f
				}
			}
			if row.Blocked() {
				t.Fatalf("%s's trim is blocked by %s with its own feed acknowledged "+
					"through %d and every other term past it", name, row.BlockedBy, acked)
			}
			if row.TrimTo != at {
				t.Fatalf("%s's tick licensed trimming to %d, want %d — the record "+
					"the backup covers", name, row.TrimTo, at)
			}
			if first, _, err := running.log.Bounds(t.Context()); err != nil || first != at {
				t.Fatalf("after the tick %s's log starts at %d (err %v), want %d",
					name, first, err, at)
			}
		})
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

// AN ALARM THAT CANNOT GO OUT IS AN ALARM AN OPERATOR LEARNS TO IGNORE.
//
// Every reading here is a THRESHOLD, and a threshold against a counter that
// only grows latches. `search_degraded` fires on a fraction being above zero,
// so computed from [metrics.Recorder.Read]'s cumulative totals one degraded
// search after boot lights it for the life of the process; `search_slow` and
// `barrier_slow` take a maximum, so one slow observation ever is permanent.
// [metrics.Window] exists for precisely this and had no caller at all.
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
			"is reading a maximum with no window", cleared.SearchP95)
	}
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

// A COMPANY WITH NO BACKUP HANDS IN AN ABSENCE, NOT A FABRICATED AGE.
//
// The alarm has to fire — `backup_floor` refuses until a backup exists, so a
// company that never takes one never trims — and the form this replaces bought
// that firing by filling the reading with `backup_max_age + 1h`. The threshold
// was then reached by a number nobody measured, and the alarm told a company
// four seconds old that its newest verified backup was twenty-five hours old,
// one line above the trim term reporting that no backup had been recorded at
// all: one state, described two ways, and the louder of them invented a
// backup.
func TestANodeWithNoBackupReportsTheAbsenceRatherThanAnAge(t *testing.T) {
	t.Parallel()
	r := &retention{state: &stateLog{}, fleet: coordmem.NewFleet(), nodeID: "node-a"}
	now := time.Date(2031, 4, 2, 3, 0, 0, 0, time.UTC)

	none := r.reading(t.Context(), now, coord.BackupPoint{}, false)
	if none.BackupAge != nil {
		t.Errorf("a fleet with no backup reported an age of %v", *none.BackupAge)
	}
	if none.BackupMaxAge != config.DefaultBackupMaxAge {
		t.Fatalf("BackupMaxAge = %v, want the default %v — with no policy the "+
			"alarm is silent and this case proves nothing",
			none.BackupMaxAge, config.DefaultBackupMaxAge)
	}
	raised := statelog.Evaluate(none)
	if !firedKind(raised, statelog.KindBackupAge) {
		t.Fatalf("a fleet with no backup raised %v — the trim will not advance "+
			"until one exists", raised)
	}
	for _, a := range raised {
		if a.Kind == statelog.KindBackupAge && strings.Contains(a.Detail, " old") {
			t.Errorf("detail = %q: it names an age for a backup that does not "+
				"exist", a.Detail)
		}
	}

	// AND A REAL BACKUP IS STILL MEASURED against the same clock, so the
	// nil above is the absence rather than a field nothing fills.
	taken := r.reading(t.Context(), now,
		coord.BackupPoint{At: now.Add(-2 * time.Hour)}, true)
	if taken.BackupAge == nil || *taken.BackupAge != 2*time.Hour {
		t.Fatalf("a two-hour-old backup reported %v", taken.BackupAge)
	}
	if got := statelog.Evaluate(taken); len(got) != 0 {
		t.Errorf("a node inside its backup policy raised %v", got)
	}
}

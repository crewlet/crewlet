package usage_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/schedule/sqlledger"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/usage"
)

// clock is a publisher's clock a case moves.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// said counts a publisher's lines of one message.
func (r *refusalLines) said(msg string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Count(r.buf.String(), "msg="+msg)
}

// newClockedPublisher is node-a's publisher over st — its own store unless a
// case wraps it — publishing into own's replicated estate, on a clock the case
// moves, with its lines kept.
func newClockedPublisher(t *testing.T, own *store.DB, st usage.Store, c *clock) (
	*usage.Publisher, *loopback, *refusalLines) {

	t.Helper()
	log := &loopback{t: t, into: own, node: "node-a"}
	p, lines := publisherOn(t, own, st, c, log)
	return p, log, lines
}

// publisherOn is [newClockedPublisher] over a log the case already holds — the
// same log a previous process of the node published into, for a case about
// what a restart does.
func publisherOn(t *testing.T, own *store.DB, st usage.Store, c *clock, log *loopback) (
	*usage.Publisher, *refusalLines) {

	t.Helper()
	return publisherWith(t, own, st, c, log, publisherOptions{})
}

// publisherOptions is what a case changes about node-a's publisher beyond its
// store, clock and log: the company's clock, which an apply can move, whether
// its usage applier has caught up with the log, and the organisation that names
// a seat it holds. Zero is Santiago, caught up, and no organisation.
type publisherOptions struct {
	zone   func() *time.Location
	caught func() bool
	handle func(agentID string) (string, bool)
}

// publisherWith is [publisherOn] with the options a case sets.
func publisherWith(t *testing.T, own *store.DB, st usage.Store, c *clock, log *loopback,
	o publisherOptions) (*usage.Publisher, *refusalLines) {

	t.Helper()
	if st == nil {
		st = own
	}
	if o.zone == nil {
		o.zone = func() *time.Location { return santiago }
	}
	if o.caught == nil {
		o.caught = caught
	}
	lines := &refusalLines{}
	p, err := usage.NewPublisher(usage.PublisherDeps{
		Store: st, Log: log, Estate: own.Replicated(), NodeID: "node-a", Caught: o.caught,
		Generations: unseen,
		Handle:      o.handle,
		Zone:        o.zone,
		Now:         c.now,
		FireHistory: fireHistory,
		Logger:      slog.New(slog.NewTextHandler(lines, nil)),
	})
	if err != nil {
		t.Fatalf("build the publisher: %v", err)
	}
	return p, lines
}

// custodyPhase is one phase record of a stateless node's seat, as a custody
// batch carries it.
func custodyPhase(t *testing.T, id string, at time.Time, agent string, input int) store.EventRecord {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"agent_id": agent, "role": "Dev", "turn_id": "t-" + id, "phase": "execute",
		"model": "m-1", "provider_key": "primary", "input_tokens": input,
		"output_tokens": 1, "total_tokens": input + 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return store.EventRecord{ID: id, Type: "agent_phase_completed", Source: "Dev",
		Category: "agent", Time: at, Actor: "Dev", Payload: body,
		Tags: map[string]string{"agent_id": agent, "agent_role": "Dev", "turn_id": "t-" + id}}
}

// custodyReview is one review phase record of a stateless node's seat, of the
// turn named, deciding what `decision` says, as a custody batch carries it.
func custodyReview(t *testing.T, id string, at time.Time, agent, turn, decision string,
	input int) store.EventRecord {

	t.Helper()
	body, err := json.Marshal(map[string]any{
		"agent_id": agent, "role": "Dev", "turn_id": turn, "phase": "review",
		"decision": decision, "model": "m-1", "provider_key": "primary",
		"input_tokens": input, "output_tokens": 1, "total_tokens": input + 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return store.EventRecord{ID: id, Type: "agent_phase_completed", Source: "Dev",
		Category: "agent", Time: at, Actor: "Dev", Payload: body,
		Tags: map[string]string{"agent_id": agent, "agent_role": "Dev", "turn_id": turn}}
}

// keep writes a custody batch into own's log and settles it as kept, the way a
// keeper that wins its claim does.
func keep(ctx context.Context, t *testing.T, own *store.DB, id string, recs ...store.EventRecord) {
	t.Helper()
	if err := own.Events().WriteCustody(ctx, store.CustodyBatch{ID: id,
		Origin: "stateless-1", Records: recs}, time.Now()); err != nil {
		t.Fatalf("write batch %s: %v", id, err)
	}
	if err := own.Events().SettleCustody(ctx, id, true); err != nil {
		t.Fatalf("settle batch %s: %v", id, err)
	}
}

// spendOn is node-a's input tokens per seat on one company day, as the
// replicated history holds it.
func spendOn(t *testing.T, own *store.DB, day time.Time) map[string]int64 {
	t.Helper()
	label := day.In(santiago).Format(time.DateOnly)
	rows, err := usage.Spend(t.Context(), own.Replicated(), usage.SpendQuery{From: label, To: label})
	if err != nil {
		t.Fatalf("read %s: %v", label, err)
	}
	out := map[string]int64{}
	for _, r := range rows {
		if r.Node == "node-a" {
			out[r.AgentID] += r.Input
		}
	}
	return out
}

// marked is whether the store still holds a re-derive mark.
func marked(t *testing.T, own *store.DB) bool {
	t.Helper()
	_, ok, err := own.UsageRederive(t.Context())
	if err != nil {
		t.Fatalf("read the mark: %v", err)
	}
	return ok
}

// daysBack is the local hour of the day n days before the case's noon.
func daysBack(noon time.Time, n, hour int) time.Time {
	d := noon.AddDate(0, 0, -n)
	return time.Date(d.Year(), d.Month(), d.Day(), hour, 0, 0, 0, santiago)
}

// A SETTLE MARK REPUBLISHES THE DAY IT CHANGED, however old, and is then
// cleared.
//
// A stateless node's batch from three days back is settled as kept: its rows
// count now on a day a tick fingerprints neither as today nor as yesterday.
// Without the mark that day keeps the value it was published at for the 181
// days the history holds it — a stateless node's spend missing from every
// window that reaches it.
func TestASettleMarkRepublishesTheDayItChanged(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	noon := time.Date(2026, 9, 23, 12, 0, 0, 0, santiago)
	c := &clock{t: noon}
	ev := &events{t: t, db: own}
	// A FRESH STORE OWES NOTHING OLDER — no watermark — so the old day
	// reaches the fleet by the mark or not at all.
	ev.phase(noon.Add(-time.Hour), "seat-1", "t-today", "execute", "", 5, 1)
	ev.phase(daysBack(noon, 3, 10), "seat-1", "t-old", "execute", "", 100, 1)

	p, _, _ := newClockedPublisher(t, own, nil, c)
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := spendOn(t, own, daysBack(noon, 3, 10)); len(got) != 0 {
		t.Fatalf("the control: three days back was published at boot (%v), so this "+
			"case could not tell the mark's work from the boot's", got)
	}

	keep(t.Context(), t, own, "batch-old", custodyPhase(t, "b1", daysBack(noon, 3, 11), "seat-2", 1000))
	if !marked(t, own) {
		t.Fatal("the premise: a kept settle left no mark")
	}
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := spendOn(t, own, daysBack(noon, 3, 10)); got["seat-1"] != 100 || got["seat-2"] != 1000 {
		t.Fatalf("after a kept settle three days back the day holds %v, want this node's "+
			"own 100 and the batch's 1000 — a day older than yesterday was never "+
			"derived again", got)
	}
	if marked(t, own) {
		t.Error("the mark outlived the pass that published every day it named")
	}
}

// A DAY PAST THE EVENT LOG'S FLOOR IS NEVER DERIVED AGAIN, whatever the mark
// names — and a day past the dispatch ledger's is derived without its
// schedules.
//
// The floor is what makes deriving again safe: a day whose rows the sweep has
// started on derives SHORT, and its record would replace the whole day every
// node holds. So the mark's day forty days back keeps its value, the tick says
// so once, and the days inside the floor are still derived. The same holds for
// the fires of a day the ledger has swept: a schedule-day ten days back is
// never published from what is left of it.
func TestADayPastTheFloorIsNeverDerivedAgain(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	noon := time.Date(2026, 9, 23, 12, 0, 0, 0, santiago)
	c := &clock{t: noon}
	(&events{t: t, db: own}).phase(noon.Add(-time.Hour), "seat-1", "t-today", "execute", "", 5, 1)
	tenBack := daysBack(noon, 10, 9)
	if _, err := sqlledger.New(own.SQL()).Claim(t.Context(), schedule.Run{
		FireKey: schedule.FireKey{Scope: "role", ScopeID: "Dev", ScheduleName: "standup",
			FireLabel: tenBack.Format("20060102T1504"), TargetHandle: "dev"},
		ScheduledAt: tenBack, FiredAt: tenBack, Outcome: schedule.OutcomeFired,
	}); err != nil {
		t.Fatalf("record a fire: %v", err)
	}

	p, _, lines := newClockedPublisher(t, own, nil, c)
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	keep(t.Context(), t, own, "batch-floor",
		custodyPhase(t, "f40", daysBack(noon, 40, 10), "seat-2", 4000),
		custodyPhase(t, "f10", tenBack, "seat-2", 1000))
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := spendOn(t, own, daysBack(noon, 40, 10)); len(got) != 0 {
		t.Fatalf("a day forty days back — past the event log's %v floor — was derived "+
			"again and published %v", store.EventHistory, got)
	}
	if got := spendOn(t, own, tenBack); got["seat-2"] != 1000 {
		t.Fatalf("ten days back holds %v, want the batch's 1000: a day inside the floor "+
			"is still derived", got)
	}
	if fires := dump(t, own)["usage_schedule_runs"]; len(fires) != 0 {
		t.Fatalf("a day older than the dispatch ledger keeps a fire published its "+
			"schedules %v — derived from a ledger that has swept part of it, it would "+
			"replace the whole schedule-day", fires)
	}
	if marked(t, own) {
		t.Error("the mark outlived a pass that published every day it could")
	}
	if got := lines.said("usage_rederive_past_floor"); got != 1 {
		t.Fatalf("the floor was said %d time(s), want once", got)
	}

	// THE SAME DAYS NAMED AGAIN are not said again.
	keep(t.Context(), t, own, "batch-floor-2", custodyPhase(t, "g40", daysBack(noon, 40, 11), "seat-2", 1))
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := lines.said("usage_rederive_past_floor"); got != 1 {
		t.Errorf("the same days past the floor were said %d time(s) in all, want once", got)
	}
}

// racingStore settles one more batch, from an older day, while the publisher
// is deriving the day its mark named — the settle a pass must not forget.
type racingStore struct {
	*store.DB
	t     *testing.T
	day   string
	batch []store.EventRecord
	once  sync.Once
}

func (r *racingStore) UsageForDay(ctx context.Context, w store.UsageWindow) (store.UsageDay, error) {
	if w.Start.In(santiago).Format(time.DateOnly) == r.day {
		r.once.Do(func() { keep(ctx, r.t, r.DB, "batch-racing", r.batch...) })
	}
	return r.DB.UsageForDay(ctx, w)
}

// A SETTLE THAT LANDS DURING A PASS KEEPS THE MARK, and its day is derived on
// the next tick.
//
// The pass derives the days the mark named when it read it; a settle after
// that read changed a day the pass may never look at. The clear is a
// compare-and-clear at the seq the pass read, so the settle's moved seq keeps
// the mark — cleared unconditionally, the older day the settle changed would
// never be derived.
func TestASettleDuringAPassKeepsTheMark(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	noon := time.Date(2026, 9, 23, 12, 0, 0, 0, santiago)
	c := &clock{t: noon}
	(&events{t: t, db: own}).phase(noon.Add(-time.Hour), "seat-1", "t-today", "execute", "", 5, 1)
	racing := &racingStore{DB: own, t: t,
		day:   daysBack(noon, 3, 10).Format(time.DateOnly),
		batch: []store.EventRecord{custodyPhase(t, "r5", daysBack(noon, 5, 10), "seat-3", 500)}}
	p, _, _ := newClockedPublisher(t, own, racing, c)
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}

	keep(t.Context(), t, own, "batch-3", custodyPhase(t, "b3", daysBack(noon, 3, 10), "seat-2", 300))
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := spendOn(t, own, daysBack(noon, 3, 10)); got["seat-2"] != 300 {
		t.Fatalf("the pass did not publish the day its mark named: %v", got)
	}
	if !marked(t, own) {
		t.Fatal("a settle that landed during the pass lost its mark to the pass's clear, " +
			"so the day five back it changed is never derived")
	}
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := spendOn(t, own, daysBack(noon, 5, 10)); got["seat-3"] != 500 {
		t.Fatalf("the tick after the race left five days back at %v, want the racing "+
			"batch's 500", got)
	}
	if marked(t, own) {
		t.Error("the mark outlived the tick that published the racing settle's day")
	}
}

// AN UNRESOLVED PUBLISH OF AN OLDER DAY KEEPS THE MARK.
//
// The broker never said whether the day's record landed. Cleared anyway, the
// mark would be the only thing that knew the day had changed, and the day
// would keep its old value for good.
func TestAnUnresolvedRederiveKeepsTheMark(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	noon := time.Date(2026, 9, 23, 12, 0, 0, 0, santiago)
	c := &clock{t: noon}
	(&events{t: t, db: own}).phase(noon.Add(-time.Hour), "seat-1", "t-today", "execute", "", 5, 1)
	p, log, _ := newClockedPublisher(t, own, nil, c)
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	keep(t.Context(), t, own, "batch-3", custodyPhase(t, "b3", daysBack(noon, 3, 10), "seat-2", 300))

	// The older day is derived before the two a tick fingerprints, and both
	// of those are unchanged: this tick's one publish is the older day's.
	log.unresolved = 1
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !marked(t, own) {
		t.Fatal("an unresolved publish of the day the mark named cleared the mark")
	}
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := spendOn(t, own, daysBack(noon, 3, 10)); got["seat-2"] != 300 {
		t.Fatalf("the tick after the unresolved publish left the day at %v", got)
	}
	if marked(t, own) {
		t.Error("the mark outlived the tick that published its day")
	}
}

// A START DERIVES EVERY DAY THE NODE STILL OWED WHEN IT STOPPED — and nothing
// older.
//
// A node down across more than one midnight comes back with the day it stopped
// in older than yesterday, and the tail of that day — written after its last
// tick — is in no published record. The store's watermark names it: after every
// tick, the start of yesterday. It is not the NEWEST ROW the node holds:
// anything written before the first tick makes that today — the custody keeper
// writes and settles a batch the moment it attaches, and the scheduler's
// catch-up records a fire — so a boot keyed on it would derive nothing older
// than yesterday. A node that stopped a moment ago owes nothing
// older than yesterday, and derives nothing older.
//
// Mutation: derive the boot's day from the newest row, or ignore the
// watermark, and the first case goes red; derive from anything older than it,
// and the second does.
func TestAStartDerivesEveryDayTheNodeStillOwed(t *testing.T) {
	t.Parallel()
	noon := time.Date(2026, 9, 23, 12, 0, 0, 0, santiago)
	stopped := daysBack(noon, 5, 12)

	// writtenFirst is what a running node writes before its publisher's
	// first tick: the keeper's first batch, kept, and a catch-up fire.
	writtenFirst := func(t *testing.T, own *store.DB) {
		t.Helper()
		keep(t.Context(), t, own, "boot-batch", custodyPhase(t, "boot", noon.Add(-time.Minute), "seat-9", 9))
		if _, err := sqlledger.New(own.SQL()).Claim(t.Context(), schedule.Run{
			FireKey: schedule.FireKey{Scope: "role", ScopeID: "Dev", ScheduleName: "standup",
				FireLabel: noon.Format("20060102T1504"), TargetHandle: "dev"},
			ScheduledAt: noon.Add(-time.Minute), FiredAt: noon.Add(-time.Minute),
			Outcome: schedule.OutcomeFired,
		}); err != nil {
			t.Fatalf("record a fire: %v", err)
		}
	}

	t.Run("down for days", func(t *testing.T) {
		t.Parallel()
		own := openStore(t)
		ev := &events{t: t, db: own}
		ev.phase(daysBack(noon, 7, 10), "seat-1", "t7", "execute", "", 7, 1)
		ev.phase(stopped.Add(-time.Hour), "seat-1", "t5", "execute", "", 5, 1)
		before, log, _ := newClockedPublisher(t, own, nil, &clock{t: stopped})
		if err := before.Flush(t.Context()); err != nil {
			t.Fatal(err)
		}
		// THE TAIL: written after the stopped process's last tick.
		ev.phase(stopped.Add(10*time.Second), "seat-1", "t5b", "execute", "", 50, 1)

		writtenFirst(t, own)
		after, _ := publisherOn(t, own, nil, &clock{t: noon}, log)
		if err := after.Flush(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := spendOn(t, own, stopped); got["seat-1"] != 55 {
			t.Errorf("the day the node stopped in holds %v after its start, want its 5 and "+
				"the 50 written after its last tick — the keeper's first batch and the "+
				"scheduler's first fire were written before the first tick", got)
		}
		if got := spendOn(t, own, daysBack(noon, 7, 10)); len(got) != 0 {
			t.Errorf("a day the stopped process owed nothing of was derived at its start: %v", got)
		}
	})

	t.Run("stopped a moment ago", func(t *testing.T) {
		t.Parallel()
		own := openStore(t)
		ev := &events{t: t, db: own}
		ev.phase(daysBack(noon, 3, 10), "seat-1", "t3", "execute", "", 3, 1)
		before, log, _ := newClockedPublisher(t, own, nil, &clock{t: noon.Add(-time.Minute)})
		if err := before.Flush(t.Context()); err != nil {
			t.Fatal(err)
		}
		writtenFirst(t, own)
		after, _ := publisherOn(t, own, nil, &clock{t: noon}, log)
		if err := after.Flush(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := spendOn(t, own, daysBack(noon, 3, 10)); len(got) != 0 {
			t.Errorf("a node that stopped a moment ago derived a day three back at its start: %v", got)
		}
	})
}

// A DAY STILL OWED WHEN A NODE STOPS IS DERIVED BY ITS NEXT START, however
// many midnights later.
//
// Yesterday's last change, published unresolved, is owed by the process that
// sent it; after midnight it is older than anything a tick fingerprints, and
// a tick that fails to settle it again leaves it owed still. Held only in that
// process's memory, a crash lost it — the watermark has to stay on the owed
// day rather than move on to the new yesterday.
//
// Mutation: write the watermark as yesterday whatever the process owes, and
// this goes red.
func TestADayStillOwedAtAStopIsDerivedByTheNextStart(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	late := time.Date(2026, 9, 23, 23, 59, 50, 0, santiago)
	c := &clock{t: late}
	ev := &events{t: t, db: own}
	ev.phase(late.Add(-time.Hour), "seat-1", "t-today", "execute", "", 5, 1)
	p, log, _ := newClockedPublisher(t, own, nil, c)
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	owed := time.Date(2026, 9, 22, 23, 59, 0, 0, santiago)
	ev.phase(owed, "seat-1", "t-late", "execute", "", 70, 1)
	log.unresolved = 1
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	// PAST MIDNIGHT, and unresolved again: the 22nd is older than yesterday
	// now, and still owed.
	c.set(late.Add(30 * time.Second))
	log.unresolved = 1
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := spendOn(t, own, owed); got["seat-1"] != 0 {
		t.Fatalf("the premise: an unresolved publish landed (%v)", got)
	}
	// THE PROCESS IS GONE; the next one starts two midnights later.
	next, _ := publisherOn(t, own, nil, &clock{t: late.Add(36 * time.Hour)}, log)
	if err := next.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := spendOn(t, own, owed); got["seat-1"] != 70 {
		t.Fatalf("the day still owed when the node stopped holds %v after its next start, "+
			"want its 70", got)
	}
}

// AN OLDER DAY'S PASS PUBLISHES ONLY WHAT CHANGED.
//
// The process remembers what it last published for every day the event log's
// floor still lets it derive — not only the two it fingerprints — so a second
// settle on a day three back republishes the one object it moved, and not the
// day's every seat.
//
// Mutation: forget an older day's publishes once a tick no longer derives it,
// and the second pass sends the unchanged seat again.
func TestAnOlderDaysPassPublishesOnlyWhatChanged(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	noon := time.Date(2026, 9, 23, 12, 0, 0, 0, santiago)
	ev := &events{t: t, db: own}
	ev.phase(noon.Add(-time.Hour), "seat-1", "t-today", "execute", "", 5, 1)
	ev.phase(daysBack(noon, 3, 9), "seat-1", "t-old", "execute", "", 100, 1)
	p, log, _ := newClockedPublisher(t, own, nil, &clock{t: noon})
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	keep(t.Context(), t, own, "batch-a", custodyPhase(t, "a", daysBack(noon, 3, 10), "seat-2", 300))
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := spendOn(t, own, daysBack(noon, 3, 10)); got["seat-1"] != 100 || got["seat-2"] != 300 {
		t.Fatalf("the premise: the first pass left three days back at %v", got)
	}
	sent := log.sent()
	keep(t.Context(), t, own, "batch-b", custodyPhase(t, "b", daysBack(noon, 3, 11), "seat-2", 1))
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := log.sent() - sent; got != 1 {
		t.Errorf("a second settle on three days back sent %d record(s), want the one seat "+
			"it changed", got)
	}
	if got := spendOn(t, own, daysBack(noon, 3, 10)); got["seat-2"] != 301 {
		t.Errorf("three days back holds %v, want seat-2's 301", got)
	}
}

// A RESTART OVER A FULL LOG DOES NOT REOPEN ITS SKIPS.
//
// A full log takes a record that replaces its subject's last one and refuses
// one that adds a subject, so a skip for a full log waits for a NEW subject to
// land. A process that has just started has published nothing, and taking every
// subject it had not published for a new one, its first republish of today's
// seats — subjects the log has held all day — put yesterday's skip back in line
// to be refused and said again on the next tick, and on every older day's
// first pass after it. New is asked of the rows this node applied.
//
// Mutation: take a subject this process had not published for new, and the
// skip is said twice.
func TestARestartOverAFullLogDoesNotReopenItsSkips(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	noon := time.Date(2026, 9, 23, 12, 0, 0, 0, santiago)
	ev := &events{t: t, db: own}
	ev.phase(noon.Add(-time.Hour), "seat-1", "t-today", "execute", "", 5, 1)
	first, log, _ := newClockedPublisher(t, own, nil, &clock{t: noon})
	if err := first.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	// THE LOG FILLS, and yesterday gains a seat new to it.
	log.refuse = func(fresh bool) error {
		if fresh {
			return &statelog.Unavailable{Reason: statelog.ReasonLogFull}
		}
		return nil
	}
	ev.phase(daysBack(noon, 1, 22), "seat-2", "t-new", "execute", "", 7, 1)

	next, lines := publisherOn(t, own, nil, &clock{t: noon.Add(time.Minute)}, log)
	_ = next.Flush(t.Context())
	if got := lines.said("usage_publish_refused"); got != 1 {
		t.Fatalf("the premise: the restarted process said the full log %d time(s), want once", got)
	}
	sent := log.sent()
	_ = next.Flush(t.Context())
	_ = next.Flush(t.Context())
	if got := lines.said("usage_publish_refused"); got != 1 {
		t.Errorf("the skip was said %d time(s) over a log still full — the restart's "+
			"republish of a seat the log already held was taken for a new subject", got)
	}
	if log.sent() != sent {
		t.Errorf("ticks over a log still full sent %d record(s)", log.sent()-sent)
	}
}

// countedDays counts the days older than yesterday a publisher derives.
type countedDays struct {
	*store.DB
	yesterday time.Time
	mu        sync.Mutex
	older     int
	during    func(ctx context.Context)
}

func (c *countedDays) UsageForDay(ctx context.Context, w store.UsageWindow) (store.UsageDay, error) {
	c.mu.Lock()
	during := c.during
	if w.Start.Before(c.yesterday) {
		c.older++
		c.during = nil
	}
	c.mu.Unlock()
	if during != nil && w.Start.Before(c.yesterday) {
		during(ctx)
	}
	return c.DB.UsageForDay(ctx, w)
}

func (c *countedDays) derivedOlder() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.older
}

// A SETTLE OF TODAY'S ROWS DURING AN OLDER PASS COSTS THE NEXT TICK NOTHING
// OLDER.
//
// A keeper settles a stateless node's batch about every second, so a settle
// lands during almost every pass over older days. Folded into one mark with
// the older day the pass was deriving, it would keep that day's mark alive past
// the clear, and every tick would derive the ten days again for as long as
// custody traffic lasted. Its own mark carries its own day.
//
// Mutation: fold every settle into one mark, and the tick after the
// pass derives the ten days a second time.
func TestATodaySettleDuringAnOlderPassCostsTheNextTickNothingOlder(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	noon := time.Date(2026, 9, 23, 12, 0, 0, 0, santiago)
	(&events{t: t, db: own}).phase(noon.Add(-time.Hour), "seat-1", "t-today", "execute", "", 5, 1)
	counted := &countedDays{DB: own, yesterday: daysBack(noon, 1, 0)}
	p, _, _ := newClockedPublisher(t, own, counted, &clock{t: noon})
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	keep(t.Context(), t, own, "batch-10", custodyPhase(t, "b10", daysBack(noon, 10, 10), "seat-2", 1))
	counted.during = func(ctx context.Context) {
		keep(ctx, t, own, "batch-today", custodyPhase(t, "now", noon.Add(-time.Second), "seat-3", 1))
	}
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := counted.derivedOlder(); got != 9 {
		t.Fatalf("the pass derived %d older day(s), want the nine from ten days back "+
			"through the day before yesterday", got)
	}
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := counted.derivedOlder(); got != 9 {
		t.Errorf("the tick after the pass derived %d more older day(s) for a settle of "+
			"today's rows", got-9)
	}
	if got := spendOn(t, own, noon); got["seat-3"] != 1 {
		t.Errorf("today holds %v, want the settle during the pass", got)
	}
}

// A REVIEW THAT ARRIVES LATER MOVES ITS OWN DAY AND LEAVES THE TURN'S ALONE.
//
// A turn's review and its end routinely arrive in different custody batches,
// kept apart. The day the turn ENDED states its reviews off the end alone — the
// turn counts them as it runs — so a review settled later, on the day before,
// changes that day's spend, which the mark republishes, and nothing about the
// day the turn ended: no fingerprint it has to move, and no figure it would
// otherwise have published short — or, counted a second time beside the end's
// own tally, too high. The late review is the turn's own send-back, the one
// its tally already counted, and the turn's day is derived again after it
// lands, so a derivation that counted the review's record as well as the
// tally would publish it.
//
// Mutation: add the turn's self_iterate review records to its sent-back count
// beside its tally, and the turn's day goes to two send-backs.
func TestAReviewThatArrivesLaterMovesOnlyItsOwnDay(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	noon := time.Date(2026, 9, 23, 12, 0, 0, 0, santiago)
	ev := &events{t: t, db: own}
	ev.phase(noon.Add(-time.Hour), "seat-1", "t-today", "execute", "", 5, 1)
	ended := daysBack(noon, 1, 0).Add(5 * time.Minute)
	ev.turn(ended, "seat-1", "dev", "t-split", false, 3*time.Minute, "self_iterate", "done")
	p, _, _ := newClockedPublisher(t, own, nil, &clock{t: noon})
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	reviewed := func() string {
		t.Helper()
		for _, row := range dump(t, own)["usage_turns"] {
			if contains(row, "day="+ended.Format(time.DateOnly)) {
				return row
			}
		}
		return ""
	}
	if row := reviewed(); !contains(row, "reviewed=1") || !contains(row, "sent_back=1") {
		t.Fatalf("the day the turn ended reads %q before its reviews arrive, want them "+
			"stated by its end", row)
	}
	review := custodyReview(t, "late-review", daysBack(noon, 2, 23).Add(58*time.Minute),
		"seat-1", "t-split", "self_iterate", 40)
	keep(t.Context(), t, own, "batch-review", review)
	// THE TURN'S DAY IS DERIVED AGAIN on this tick: a row of its own moves
	// its fingerprint, so what it publishes is what its derivation now says
	// rather than the value it held. Without it the day is never derived
	// again and the assertion below could not fail.
	ev.phase(ended.Add(time.Hour), "seat-1", "t-other", "execute", "", 1, 1)
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if row := reviewed(); !contains(row, "reviewed=1") || !contains(row, "sent_back=1") {
		t.Errorf("the day the turn ended reads %q after a review arrived, want it unmoved", row)
	}
	if got := spendOn(t, own, daysBack(noon, 2, 23)); got["seat-1"] != 40 {
		t.Errorf("the review's own day holds %v, want its 40 through the mark", got)
	}
}

// A DAY A TICK COULD NOT SETTLE IS DERIVED AFTER MIDNIGHT TOO.
//
// Yesterday's last change, published unresolved on the day's last tick, used
// to be retried only while yesterday was still yesterday: one midnight later it
// was out of the two days a tick fingerprints and the day kept its short value
// for good — exactly the end of a day, which is when nothing else moves it.
func TestADayATickCouldNotSettleIsDerivedAfterMidnight(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	late := time.Date(2026, 9, 23, 23, 59, 50, 0, santiago)
	c := &clock{t: late}
	ev := &events{t: t, db: own}
	ev.phase(late.Add(-time.Hour), "seat-1", "t-today", "execute", "", 5, 1)
	p, log, _ := newClockedPublisher(t, own, nil, c)
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	// YESTERDAY'S LAST CHANGE, written late — and its publish unresolved.
	yesterday := time.Date(2026, 9, 22, 23, 59, 0, 0, santiago)
	ev.phase(yesterday, "seat-1", "t-late", "execute", "", 70, 1)
	log.unresolved = 1
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := spendOn(t, own, yesterday); got["seat-1"] != 0 {
		t.Fatalf("the premise: the unresolved publish landed (%v)", got)
	}
	// MIDNIGHT, and a little more: the 22nd is the day before yesterday now.
	c.set(late.Add(30 * time.Second))
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := spendOn(t, own, yesterday); got["seat-1"] != 70 {
		t.Fatalf("after midnight the day whose last publish was unresolved holds %v, "+
			"want its 70 — it left the two days a tick looks at unsettled", got)
	}
}

// AN ORDERLY STOP PUBLISHES THE DAY'S LAST CHANGE.
//
// The loop's context ending is the stop, and the change written after its last
// tick is the end of a day nothing will move again. A node that never starts
// again — decommissioned — has no next boot to publish it, so [Publisher.Run]
// flushes once more on a context of its own: on the loop's, which has just
// ended, every read it made would fail.
func TestAnOrderlyStopPublishesTheDaysLastChange(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	noon := time.Date(2026, 9, 23, 12, 0, 0, 0, santiago)
	marks := &countedMarks{DB: own}
	log := &loopback{t: t, into: own, node: "node-a"}
	p, err := usage.NewPublisher(usage.PublisherDeps{
		Store: marks, Log: log, Estate: own.Replicated(), NodeID: "node-a", Caught: caught,
		Generations: unseen,
		Zone:        func() *time.Location { return santiago },
		Now:         func() time.Time { return noon },
		// AN HOUR, so no tick after the first can be what publishes it.
		Every:       time.Hour,
		FireHistory: fireHistory,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	waitFor(t, "the publisher's first flush", func() bool { return marks.n.Load() >= 2 })

	(&events{t: t, db: own}).phase(noon.Add(-time.Minute), "seat-1", "t-last", "execute", "", 42, 1)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}
	if got := spendOn(t, own, noon); got["seat-1"] != 42 {
		t.Fatalf("after an orderly stop the day holds %v, want the 42 written after the "+
			"last tick", got)
	}
}

// THE STOP'S FLUSH DERIVES TODAY AND YESTERDAY, AND LEAVES AN OLDER DAY OWED.
//
// It runs inside [usage.FinalFlushBudget], which is sized for two days'
// fingerprints and what moved in them: a pass over every day a mark names,
// republishing each, can take longer, and runs first — so the end of the day,
// the one thing only the stop can send, was what an overrun cut. A mark that
// lands as the node stops stays in the store for its next start.
func TestTheStopsFlushLeavesAnOlderDayOwed(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	noon := time.Date(2026, 9, 23, 12, 0, 0, 0, santiago)
	(&events{t: t, db: own}).phase(noon.Add(-time.Hour), "seat-1", "t-today", "execute", "", 5, 1)
	log := &loopback{t: t, into: own, node: "node-a"}
	p, err := usage.NewPublisher(usage.PublisherDeps{
		Store: own, Log: log, Estate: own.Replicated(), NodeID: "node-a", Caught: caught,
		Generations: unseen,
		Zone:        func() *time.Location { return santiago },
		Now:         func() time.Time { return noon },
		Every:       time.Hour,
		FireHistory: fireHistory,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(ctx)
	}()
	waitFor(t, "the publisher's first flush", func() bool { return log.sent() >= 1 })
	keep(t.Context(), t, own, "batch-3", custodyPhase(t, "b3", daysBack(noon, 3, 10), "seat-2", 300))
	(&events{t: t, db: own}).phase(noon.Add(-time.Minute), "seat-1", "t-last", "execute", "", 42, 1)
	cancel()
	<-done
	if got := spendOn(t, own, noon); got["seat-1"] != 47 {
		t.Errorf("the stop's flush left today at %v, want its last change", got)
	}
	if got := spendOn(t, own, daysBack(noon, 3, 10)); len(got) != 0 {
		t.Errorf("the stop's flush derived three days back (%v) — an older day is the next start's", got)
	}
	if !marked(t, own) {
		t.Error("the stop's flush cleared the mark of a day it did not derive")
	}
}

// AN OLDER DAY'S OBJECT SKIPPED FOR A FULL LOG IS SENT WHEN THE LOG HAS ROOM.
//
// A full log is the log's state rather than the object's, so a skip waits for
// a new subject to land — proof of room — and not for the object's content to
// change, which on a day that is over it never will. Remembered only while its
// day was one of the two a tick fingerprints, the skip of a day a settle
// changed — or of yesterday, once midnight passed — was forgotten with the day,
// and the object never reached the fleet however much room the log made.
func TestAnOlderDaysFullLogSkipIsSentWhenTheLogHasRoom(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	noon := time.Date(2026, 9, 23, 12, 0, 0, 0, santiago)
	ev := &events{t: t, db: own}
	ev.phase(noon.Add(-time.Hour), "seat-1", "t-today", "execute", "", 5, 1)
	p, log, _ := newClockedPublisher(t, own, nil, &clock{t: noon})
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}

	full := true
	log.refuse = func(fresh bool) error {
		if full && fresh {
			return &statelog.Unavailable{Reason: statelog.ReasonLogFull}
		}
		return nil
	}
	keep(t.Context(), t, own, "batch-3", custodyPhase(t, "b3", daysBack(noon, 3, 10), "seat-2", 300))
	_ = p.Flush(t.Context())
	if got := spendOn(t, own, daysBack(noon, 3, 10)); len(got) != 0 {
		t.Fatalf("the premise: the full log took the older day's new subject (%v)", got)
	}
	sent := log.sent()
	_ = p.Flush(t.Context())
	if log.sent() != sent {
		t.Fatalf("a tick over a log still full sent %d record(s) — a skip for a full "+
			"log waits for room, not for the next tick", log.sent()-sent)
	}

	// ROOM AGAIN: a seat new to the log lands today, and the tick after it
	// derives the older day again and sends the skipped object.
	full = false
	ev.phase(noon.Add(-time.Minute), "seat-3", "t-new", "execute", "", 7, 1)
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := spendOn(t, own, daysBack(noon, 3, 10)); got["seat-2"] != 300 {
		t.Fatalf("after the log had room again three days back holds %v, want the 300 "+
			"skipped for the full log", got)
	}
}

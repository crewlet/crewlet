package usage_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/schedule/sqlledger"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/usage"
)

// spendOnLabel is node-a's input tokens per seat on one company day, named by
// its label, as the replicated history holds it.
func spendOnLabel(t *testing.T, own *store.DB, label string) map[string]int64 {
	t.Helper()
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

// forgetEvent deletes one row of a node's event log, as a store restored from
// a copy taken before the row was written lacks it.
func forgetEvent(t *testing.T, own *store.DB, id string) {
	t.Helper()
	res, err := own.SQL().ExecContext(t.Context(), `DELETE FROM crewlet_events WHERE event_id = ?`, id)
	if err != nil {
		t.Fatalf("forget %s: %v", id, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("the premise: %s named %d row(s)", id, n)
	}
}

// watermark is the store's watermark, failing a case that finds none.
func watermark(t *testing.T, own *store.DB) time.Time {
	t.Helper()
	at, ok, err := own.UsageWatermark(t.Context())
	if err != nil || !ok {
		t.Fatalf("read the watermark: ok=%v err=%v", ok, err)
	}
	return at
}

// AN OLDER DAY IS NEVER PUBLISHED BELOW WHAT THE FLEET HOLDS.
//
// A backup copies a node's store first and its streams after, and a fleet
// restore puts one node's store back beside a usage log another node's copy
// took later: the log holds what this node published after its store was
// copied, and its event log does not. The restored watermark names the day the
// copy was taken, so the first start derives that day again — from a log that
// stops at the copy — and published as derived, the short record would replace
// on every node the whole day the fleet holds. Inside the floor a day only
// grows, so what is published is the derivation JOINED with the fleet's record
// (usage.Join): the fleet's figure wherever it is larger. That a derivation
// came out short is said once, and the day still settles, so the watermark
// moves on.
//
// And it is said once for as long as the object STAYS short: the same day
// derived again in the same process — a batch kept on it — that is still short
// of the fleet's says nothing more, while the batch's new seat is published.
//
// Mutation: publish a day's derivation whatever the fleet holds, and the day
// shrinks to what the restored log kept; or say a short derivation every time
// one is derived, and the second pass says it again.
func TestAnOlderDayIsNeverPublishedBelowWhatTheFleetHolds(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	noon := time.Date(2026, 9, 23, 12, 0, 0, 0, santiago)
	ev := &events{t: t, db: own}
	ev.phase(noon.Add(-2*time.Hour), "seat-1", "t-a", "execute", "", 100, 1)
	ev.phase(noon.Add(-time.Hour), "seat-1", "t-b", "execute", "", 50, 1)
	first, log, _ := newClockedPublisher(t, own, nil, &clock{t: noon})
	if err := first.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := spendOnLabel(t, own, "2026-09-23"); got["seat-1"] != 150 {
		t.Fatalf("the premise: the day was published at %v", got)
	}

	// THE RESTORE: the event log comes back without the day's tail, and the
	// watermark as it was when the store was copied — naming the day.
	forgetEvent(t, own, "ev-0002")
	if err := own.SetUsageWatermark(t.Context(), daysBack(noon, 0, 0)); err != nil {
		t.Fatal(err)
	}
	later := noon.Add(48 * time.Hour)
	next, lines := publisherOn(t, own, nil, &clock{t: later}, log)
	if err := next.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := spendOnLabel(t, own, "2026-09-23"); got["seat-1"] != 150 {
		t.Fatalf("after a start over a restored event log the day holds %v, want the 150 "+
			"the fleet held — the short derivation replaced it", got)
	}
	if got := lines.said("usage_rederive_short"); got != 1 {
		t.Errorf("the short derivation was said %d time(s), want once", got)
	}
	if got, want := watermark(t, own), daysBack(later, 1, 0); !got.Equal(want) {
		t.Errorf("the watermark is %v after the day settled short, want yesterday's %v — "+
			"a day the fleet already holds more of is not owed", got, want)
	}
	// THE SAME DAY AGAIN, IN THE SAME PROCESS: a batch kept on the restored
	// day marks it, so the next tick derives it again — seat-1 grown by the
	// batch and still short of the fleet's 150, and seat-2 new.
	keep(t.Context(), t, own, "batch-again",
		custodyPhase(t, "again-1", noon.Add(30*time.Minute), "seat-1", 30),
		custodyPhase(t, "again-2", noon.Add(30*time.Minute), "seat-2", 5))
	if err := next.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := spendOnLabel(t, own, "2026-09-23"); got["seat-2"] != 5 || got["seat-1"] != 150 {
		t.Fatalf("the premise: the restored day was not derived again in this process: %v", got)
	}
	if got := lines.said("usage_rederive_short"); got != 1 {
		t.Errorf("an object still short of the fleet's was said short %d time(s) in all, "+
			"want once", got)
	}
}

// AN OLDER DAY IS NOT DERIVED BEFORE THE APPLIER HAS CAUGHT UP — and is owed
// until it has.
//
// What a day's derivation is joined with is the rows this node's applier wrote,
// and right after a restore those are the copy's: the records this node
// published after it are still being replayed. Joined with those, a short
// derivation stays short of the fleet's and replaces it. So the pass waits,
// and every day it owes stays owed — the watermark included — for the tick
// after the applier reaches the end of the log.
//
// Mutation: derive the older days whatever the applier has done, and the first
// tick derives them.
func TestAnOlderDayWaitsForTheApplierToCatchUp(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	noon := time.Date(2026, 9, 23, 12, 0, 0, 0, santiago)
	ev := &events{t: t, db: own}
	ev.phase(noon.Add(-2*time.Hour), "seat-1", "t-a", "execute", "", 100, 1)
	ev.phase(noon.Add(-time.Hour), "seat-1", "t-b", "execute", "", 50, 1)
	first, log, _ := newClockedPublisher(t, own, nil, &clock{t: noon})
	if err := first.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	forgetEvent(t, own, "ev-0002")
	owed := daysBack(noon, 0, 0)
	if err := own.SetUsageWatermark(t.Context(), owed); err != nil {
		t.Fatal(err)
	}

	later := noon.Add(48 * time.Hour)
	counted := &countedDays{DB: own, yesterday: daysBack(later, 1, 0)}
	var applied atomic.Bool
	next, _ := publisherWith(t, own, counted, &clock{t: later}, log,
		publisherOptions{caught: applied.Load})
	if err := next.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := counted.derivedOlder(); got != 0 {
		t.Fatalf("a tick before the applier caught up derived %d older day(s)", got)
	}
	if got := watermark(t, own); !got.Equal(owed) {
		t.Fatalf("a tick that derived nothing it owed moved the watermark to %v, want %v", got, owed)
	}

	applied.Store(true)
	if err := next.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := counted.derivedOlder(); got == 0 {
		t.Fatal("the tick after the applier caught up derived no older day")
	}
	if got := spendOnLabel(t, own, "2026-09-23"); got["seat-1"] != 150 {
		t.Errorf("the owed day holds %v once derived, want the fleet's 150", got)
	}
}

// A STORE STARTED EMPTY DOES NOT REPLACE A DAY WITH ONE REDELIVERED BATCH.
//
// A node that lost its store starts with an empty one under the same id. The
// record that this node settled a batch went with the store, so a redelivery of
// that batch is written and settled as kept again, and its mark derives the
// batch's day — from a log holding that batch and nothing else. Published, the
// seat's day on every node would be that batch alone.
//
// Mutation: publish a day's derivation whatever the fleet holds, and the seat's
// day shrinks to the redelivered batch.
func TestAnEmptiedStoreDoesNotReplaceADayWithARedeliveredBatch(t *testing.T) {
	t.Parallel()
	fleet := openStore(t)
	noon := time.Date(2026, 9, 23, 12, 0, 0, 0, santiago)
	(&events{t: t, db: fleet}).phase(noon.Add(-time.Hour), "seat-1", "t-today", "execute", "", 5, 1)
	b1 := custodyPhase(t, "b1", daysBack(noon, 3, 10), "seat-2", 1000)
	before, log, _ := newClockedPublisher(t, fleet, nil, &clock{t: noon})
	if err := before.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	keep(t.Context(), t, fleet, "batch-1", b1)
	keep(t.Context(), t, fleet, "batch-2", custodyPhase(t, "b2", daysBack(noon, 3, 11), "seat-2", 500))
	if err := before.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := spendOn(t, fleet, daysBack(noon, 3, 10)); got["seat-2"] != 1500 {
		t.Fatalf("the premise: three days back holds %v", got)
	}

	emptied := openStore(t)
	keep(t.Context(), t, emptied, "batch-1", b1)
	after, lines := publisherWith(t, fleet, emptied, &clock{t: noon.Add(time.Hour)}, log,
		publisherOptions{})
	if err := after.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := spendOn(t, fleet, daysBack(noon, 3, 10)); got["seat-2"] != 1500 {
		t.Fatalf("after a redelivered batch on an emptied store three days back holds %v, "+
			"want the 1500 the fleet held", got)
	}
	if got := lines.said("usage_rederive_short"); got != 1 {
		t.Errorf("the short derivation was said %d time(s), want once", got)
	}
	if marked(t, emptied) {
		t.Error("a pass that settled every day it derived — one of them short — kept its mark")
	}
}

// A RESTORED LOG HOLDING MORE THAN THE FLEET IS STILL PUBLISHED.
//
// The join raises only a figure the fleet holds MORE of: a day whose log grew
// past its record — a row written after its last publish — is exactly what an
// older day's pass exists to send, and is sent as derived.
func TestAnOlderDayThatGrewIsStillPublished(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	noon := time.Date(2026, 9, 23, 12, 0, 0, 0, santiago)
	ev := &events{t: t, db: own}
	ev.phase(noon.Add(-2*time.Hour), "seat-1", "t-a", "execute", "", 100, 1)
	first, log, _ := newClockedPublisher(t, own, nil, &clock{t: noon})
	if err := first.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	ev.phase(noon.Add(-time.Hour), "seat-1", "t-b", "execute", "", 50, 1)
	if err := own.SetUsageWatermark(t.Context(), daysBack(noon, 0, 0)); err != nil {
		t.Fatal(err)
	}
	next, lines := publisherOn(t, own, nil, &clock{t: noon.Add(48 * time.Hour)}, log)
	if err := next.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := spendOnLabel(t, own, "2026-09-23"); got["seat-1"] != 150 {
		t.Errorf("a day whose log grew past its record holds %v after its pass, want 150", got)
	}
	if got := lines.said("usage_rederive_short"); got != 0 {
		t.Errorf("a derivation larger than the fleet's was said short %d time(s)", got)
	}
}

// A YESTERDAY REOPENED ON THE DAY'S LAST TICK IS SENT AFTER MIDNIGHT.
//
// A landed publish that adds a subject to the log proves a full log has room,
// and puts every object skipped for it back in line — taking them out of the
// skips. Reopened on yesterday on the last tick before midnight, the day was
// out of the two a tick fingerprints by the next, and with its skips gone
// nothing derived it again: yesterday's last value, which nothing will ever
// change, never reached the fleet.
//
// Mutation: lower the process's bound only for a reopened day older than
// yesterday, and seat-2 never reaches the 22nd.
func TestAYesterdayReopenedBeforeMidnightIsSentAfterIt(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	noon := time.Date(2026, 9, 23, 12, 0, 0, 0, santiago)
	c := &clock{t: noon}
	ev := &events{t: t, db: own}
	ev.phase(noon.Add(-time.Hour), "seat-1", "t-today", "execute", "", 5, 1)
	p, log, _ := newClockedPublisher(t, own, nil, c)
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
	ev.phase(time.Date(2026, 9, 22, 22, 0, 0, 0, santiago), "seat-2", "t-y", "execute", "", 7, 1)
	_ = p.Flush(t.Context())
	if got := spendOnLabel(t, own, "2026-09-22"); len(got) != 0 {
		t.Fatalf("the premise: the full log took yesterday's new seat (%v)", got)
	}

	// ROOM AGAIN, on the last tick before midnight: a seat new to the log
	// lands today and reopens yesterday's skip.
	c.set(time.Date(2026, 9, 23, 23, 59, 50, 0, santiago))
	full = false
	ev.phase(time.Date(2026, 9, 23, 23, 59, 40, 0, santiago), "seat-3", "t-new", "execute", "", 3, 1)
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	c.set(time.Date(2026, 9, 24, 0, 0, 5, 0, santiago))
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := spendOnLabel(t, own, "2026-09-22"); got["seat-2"] != 7 {
		t.Fatalf("the 22nd holds %v after midnight, want seat-2's 7 skipped for the full "+
			"log and reopened on the 23rd's last tick", got)
	}
}

// A DAY DERIVED AGAIN AFTER THE COMPANY'S CLOCK MOVED KEEPS ITS FIRST CUT.
//
// A record names its day by label, cut into instants on the company's clock —
// which an apply can move. Recut on the clock in force when it is derived
// again, a label replaces the record its old cut published while the objects
// the new cut no longer reaches keep theirs: moved west, a seat's row just
// after midnight joins the day before and stays in its own, counted twice.
// Every label keeps the cut it was first given — the two a tick fingerprints
// at the very tick the clock moves, and an older one a mark derives later.
//
// Both of the 22nd's edges are held — a row after the 23rd's UTC midnight and
// rows before New York's — and the one that holds its START is a seat new to
// the day at the tick the clock moves: a row the fleet already holds would be
// kept by the join whichever cut the day was derived on, so only a row it does
// not hold yet shows which instants the day was cut on.
//
// Mutation: cut every day on the clock in force, and both the tick at the move
// and the later pass count the row twice; or derive a label already cut on a
// fresh cut abutting its neighbours, and the 22nd loses its hours before New
// York's midnight at the tick the clock moves.
func TestADayKeepsItsCutWhenTheClockMovesWest(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	zone := atomic.Pointer[time.Location]{}
	zone.Store(time.UTC)
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{t: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)}
	ev := &events{t: t, db: own}
	ev.phase(time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC), "seat-1", "t-a", "execute", "", 6, 1)
	ev.phase(time.Date(2026, 9, 22, 2, 0, 0, 0, time.UTC), "seat-1", "t-e", "execute", "", 4, 1)
	ev.phase(time.Date(2026, 9, 23, 2, 0, 0, 0, time.UTC), "seat-1", "t-b", "execute", "", 20, 1)
	log := &loopback{t: t, into: own, node: "node-a"}
	p, _ := publisherWith(t, own, nil, c, log, publisherOptions{zone: zone.Load})
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	check := func(when string) {
		t.Helper()
		if got := spendOnLabel(t, own, "2026-09-22"); got["seat-1"] != 10 {
			t.Errorf("%s, the 22nd holds %v, want its own 10 alone", when, got)
		}
		if got := spendOnLabel(t, own, "2026-09-23"); got["seat-1"] != 20 {
			t.Errorf("%s, the 23rd holds %v, want its own 20", when, got)
		}
	}
	check("under UTC")

	// THE CLOCK MOVES WEST, and a row lands so both days a tick fingerprints
	// are derived again on this tick.
	zone.Store(newYork)
	c.set(time.Date(2026, 9, 23, 13, 0, 0, 0, time.UTC))
	ev.phase(time.Date(2026, 9, 23, 12, 30, 0, 0, time.UTC), "seat-9", "t-c", "execute", "", 1, 1)
	ev.phase(time.Date(2026, 9, 22, 2, 0, 0, 0, time.UTC), "seat-9", "t-d", "execute", "", 1, 1)
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	check("at the tick the clock moved")
	newRow := func(when string) {
		t.Helper()
		if got := spendOnLabel(t, own, "2026-09-22")["seat-9"]; got != 1 {
			t.Errorf("%s, the 22nd holds %d of seat-9's row before New York's midnight, "+
				"want 1", when, got)
		}
	}
	newRow("at the tick the clock moved")

	// AND LATER, a kept settle marks the 20th, so the 20th through the day
	// before yesterday are derived again in full on the new clock.
	c.set(time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC))
	keep(t.Context(), t, own, "batch-20", custodyPhase(t, "b20",
		time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC), "seat-8", 1))
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := spendOnLabel(t, own, "2026-09-20"); got["seat-8"] != 1 {
		t.Fatalf("the premise: the mark's day was not derived (%v)", got)
	}
	check("after a later pass derived both days again")
	newRow("after a later pass derived both days again")
}

// AND MOVED EAST, a settled row is derived on the day whose cut holds it.
//
// On the new clock 20:00 UTC is the next day already; derived on that day, the
// row would join a record whose day has no such hour on the cut it was first
// published on, and the day that holds it keeps its value without the row.
//
// Mutation: cut every day on the clock in force, and the row lands on the 21st.
func TestASettledRowIsDerivedOnTheDayWhoseCutHoldsIt(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	zone := atomic.Pointer[time.Location]{}
	zone.Store(time.UTC)
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{t: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)}
	(&events{t: t, db: own}).phase(time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC),
		"seat-1", "t-a", "execute", "", 10, 1)
	log := &loopback{t: t, into: own, node: "node-a"}
	p, _ := publisherWith(t, own, nil, c, log, publisherOptions{zone: zone.Load})
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	zone.Store(tokyo)
	c.set(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC))
	keep(t.Context(), t, own, "batch-east", custodyPhase(t, "e1",
		time.Date(2026, 9, 20, 20, 0, 0, 0, time.UTC), "seat-2", 300))
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := spendOnLabel(t, own, "2026-09-20"); got["seat-1"] != 10 || got["seat-2"] != 300 {
		t.Errorf("the 20th holds %v, want its own 10 and the settled row's 300", got)
	}
	if got := spendOnLabel(t, own, "2026-09-21"); got["seat-2"] != 0 {
		t.Errorf("the 21st holds %v — the row went to a day whose cut does not hold it", got)
	}
}

// A PUBLISHER IS REFUSED WITHOUT KNOWING WHETHER ITS APPLIER CAUGHT UP: it is
// what keeps an older day off a restore's rows.
func TestAPublisherWithoutItsAppliersProgressIsRefused(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	_, err := usage.NewPublisher(usage.PublisherDeps{
		Store: own, Log: &loopback{t: t}, Estate: own.Replicated(), NodeID: "node-a",
		Zone:        func() *time.Location { return santiago },
		FireHistory: fireHistory,
	})
	if err == nil || !strings.Contains(err.Error(), "caught up") {
		t.Fatalf("a publisher with no Caught was built (%v)", err)
	}
}

// THE DAY THE CLOCK MOVES ON STARTS WHERE THE DAY BEFORE IT ENDED.
//
// The day before the move was cut on the old clock and keeps that cut; the
// first day cut on the new one starts where it ended, so it is longer or
// shorter by the move. Cut on the new clock alone, a move west leaves the hours
// between the old midnight and the new one in no day — their rows counted
// nowhere — and a move east puts the hours before the old midnight in two.
//
// Mutation: cut a day the first time without abutting the day before it, and
// both cases go red.
func TestTheDayTheClockMovesOnAbutsTheDayBefore(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		zone  string
		row   time.Time
		holds string
	}{
		// West: 02:00 UTC on the 24th is still the 23rd in New York, whose
		// day begins at 04:00 UTC — after the 23rd's UTC cut ended.
		{"west", "America/New_York", time.Date(2026, 9, 24, 2, 0, 0, 0, time.UTC), "2026-09-24"},
		// East: 20:00 UTC on the 23rd is the 24th in Tokyo already, whose day
		// began at 15:00 UTC — inside the 23rd's UTC cut.
		{"east", "Asia/Tokyo", time.Date(2026, 9, 23, 20, 0, 0, 0, time.UTC), "2026-09-23"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			moved, err := time.LoadLocation(c.zone)
			if err != nil {
				t.Fatal(err)
			}
			own := openStore(t)
			zone := atomic.Pointer[time.Location]{}
			zone.Store(time.UTC)
			at := &clock{t: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)}
			ev := &events{t: t, db: own}
			ev.phase(at.now().Add(-time.Hour), "seat-1", "t-a", "execute", "", 5, 1)
			log := &loopback{t: t, into: own, node: "node-a"}
			p, _ := publisherWith(t, own, nil, at, log, publisherOptions{zone: zone.Load})
			if err := p.Flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			zone.Store(moved)
			at.set(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC))
			ev.phase(c.row, "seat-6", "t-seam", "execute", "", 60, 1)
			if err := p.Flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			counted := 0
			for _, label := range []string{"2026-09-23", "2026-09-24"} {
				got := spendOnLabel(t, own, label)["seat-6"]
				if got != 0 {
					counted++
				}
				if want := int64(0); label == c.holds {
					want = 60
					if got != want {
						t.Errorf("the %s holds %d of the row at %v, want its 60", label, got, c.row)
					}
				} else if got != want {
					t.Errorf("the %s holds %d of the row at %v, which its cut does not hold",
						label, got, c.row)
				}
			}
			if counted != 1 {
				t.Errorf("the row at %v is counted in %d day(s), want exactly one", c.row, counted)
			}
		})
	}
}

// unreadCut is a node's store that cannot say which day an instant of one range
// falls in while `failing` is set — the store error that leaves an older day
// uncut on a tick that cut the two it fingerprints.
type unreadCut struct {
	*store.DB
	from, to time.Time
	failing  atomic.Bool
}

func (u *unreadCut) UsageDayContaining(ctx context.Context, t time.Time) (
	string, store.UsageWindow, bool, error) {

	if u.failing.Load() && !t.Before(u.from) && t.Before(u.to) {
		return "", store.UsageWindow{}, false, errors.New("the store did not answer")
	}
	return u.DB.UsageDayContaining(ctx, t)
}

// AND A DAY CUT LATE ENDS WHERE THE DAY AFTER IT STARTS.
//
// A day can be cut for the first time after the day following it was: the two
// a tick fingerprints are cut before any older day it owes, and a store that
// does not answer for the older one leaves it owed — while an apply moves the
// clock. Cut on the new clock alone, the late day reaches into the hours its
// successor's old cut already holds, and their rows count in both.
//
// Mutation: cut a day the first time without abutting the day after it, and the
// row is counted twice.
func TestADayCutLateEndsWhereTheDayAfterItStarts(t *testing.T) {
	t.Parallel()
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	own := openStore(t)
	st := &unreadCut{DB: own, from: time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC),
		to: time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)}
	zone := atomic.Pointer[time.Location]{}
	zone.Store(time.UTC)
	at := &clock{t: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)}
	ev := &events{t: t, db: own}
	ev.phase(at.now().Add(-time.Hour), "seat-1", "t-a", "execute", "", 5, 1)
	log := &loopback{t: t, into: own, node: "node-a"}
	p, _ := publisherWith(t, own, st, at, log, publisherOptions{zone: zone.Load})
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	// THREE DAYS ON, the 25th and 26th are cut on UTC, and the 24th — owed by
	// a kept settle's mark — is not: its cut cannot be read.
	ev.phase(time.Date(2026, 9, 25, 2, 0, 0, 0, time.UTC), "seat-6", "t-seam", "execute", "", 60, 1)
	keep(t.Context(), t, own, "batch-24", custodyPhase(t, "m24",
		time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC), "seat-8", 1))
	st.failing.Store(true)
	at.set(time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC))
	if err := p.Flush(t.Context()); err == nil {
		t.Fatal("the premise: a tick whose older day's cut could not be read reported no failure")
	}
	// AND THE CLOCK MOVES WEST before the 24th is cut.
	zone.Store(newYork)
	st.failing.Store(false)
	at.set(time.Date(2026, 9, 26, 13, 0, 0, 0, time.UTC))
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := spendOnLabel(t, own, "2026-09-25")["seat-6"]; got != 60 {
		t.Errorf("the 25th holds %d of the row its UTC cut holds, want 60", got)
	}
	day24 := spendOnLabel(t, own, "2026-09-24")
	if day24["seat-8"] != 1 {
		t.Fatalf("the premise: the 24th the mark owed was not derived (%v)", day24)
	}
	if got := day24["seat-6"]; got != 0 {
		t.Errorf("the 24th, cut late, holds %d of a row in the 25th's cut — counted twice", got)
	}
}

// A TURN THE BUILD BEFORE THE TALLY ENDED KEEPS ITS STATISTICS.
//
// Every `turn_completed` in a node's log at the upgrade states no tally, and
// the first tick after it derives today and yesterday in full and publishes
// them — over what the old build published, which it read off the turn's
// records. Read as zeros, every such turn went out zero-length, unreviewed and
// unfailed, for the 181 days the history keeps a day. The publisher hands the
// store the review decisions its records are counted in, and the turn is read
// as the old build read it.
//
// Mutation: hand the store a window without the decisions, or read an
// untallied turn's figures off its absent tally, and this goes red.
func TestATurnTheOldBuildEndedKeepsItsStatistics(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	noon := time.Date(2026, 9, 23, 12, 0, 0, 0, santiago)
	ev := &events{t: t, db: own}
	ev.phase(noon.Add(-3*time.Hour), "seat-1", "t-old", "review", "self_iterate", 5, 1)
	ev.phase(noon.Add(-2*time.Hour), "seat-1", "t-old", "review", "done", 5, 1)
	ev.add("agent_turn_completed", noon.Add(-time.Hour), "seat-1", "Dev", map[string]any{
		"agent_id": "seat-1", "role": "Dev", "turn_id": "t-old", "failed": true,
	})
	ev.add("turn_completed", noon.Add(-time.Hour), "seat-1", "Dev", map[string]any{
		"agent_id": "seat-1", "agent_handle": "dev", "role": "Dev", "turn_id": "t-old",
		"duration_ms": (2 * time.Minute).Milliseconds(),
	})
	p, _, _ := newClockedPublisher(t, own, nil, &clock{t: noon})
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	days, err := usage.SeatDays(t.Context(), own.Replicated(),
		usage.SpendQuery{From: "2026-09-23", To: "2026-09-23"})
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 1 {
		t.Fatalf("the day holds %d seat row(s), want one", len(days))
	}
	d := days[0]
	if d.Turns != 1 || d.Failed != 1 || d.Reviewed != 1 || d.FirstPass != 0 || d.SentBack != 1 {
		t.Errorf("the old build's turn reads turns=%d failed=%d reviewed=%d first_pass=%d "+
			"sent_back=%d, want 1 1 1 0 1 — its summary failed and its first review sent "+
			"it back", d.Turns, d.Failed, d.Reviewed, d.FirstPass, d.SentBack)
	}
	if q, ok := d.Durations.Quantile(0.5); !ok || q < 100*time.Second {
		t.Errorf("the old build's two-minute turn reads a median of %v (%v)", q, ok)
	}
}

// TODAY AND YESTERDAY ARE NEVER PUBLISHED BELOW WHAT THE FLEET HOLDS EITHER.
//
// A restore's watermark names the day the store was copied, and that is most
// often today or yesterday still: a backup taken this morning and restored this
// afternoon. The restored event log stops at the copy and never gets back the
// rows it lacks, so a derivation of either day stays below the record the fleet
// holds of it however long the day runs — published as derived, the first
// tick replaced the fleet's day with less, on every node, for the history.
//
// Mutation: publish today's and yesterday's derivation whatever the fleet
// holds, and the day shrinks to what the restored log kept.
func TestTodayAndYesterdayAreNeverPublishedBelowWhatTheFleetHolds(t *testing.T) {
	t.Parallel()
	noon := time.Date(2026, 9, 23, 12, 0, 0, 0, santiago)
	for _, c := range []struct {
		name  string
		start time.Time
	}{
		{"today", noon.Add(2 * time.Hour)},
		{"yesterday", noon.Add(24 * time.Hour)},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			own := openStore(t)
			ev := &events{t: t, db: own}
			ev.phase(noon.Add(-2*time.Hour), "seat-1", "t-a", "execute", "", 100, 1)
			ev.phase(noon.Add(-time.Hour), "seat-1", "t-b", "execute", "", 50, 1)
			first, log, _ := newClockedPublisher(t, own, nil, &clock{t: noon})
			if err := first.Flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			// THE RESTORE, and a row the node records after it.
			forgetEvent(t, own, "ev-0002")
			ev.phase(noon.Add(time.Hour), "seat-1", "t-c", "execute", "", 1, 1)
			next, lines := publisherOn(t, own, nil, &clock{t: c.start}, log)
			if err := next.Flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := spendOnLabel(t, own, "2026-09-23"); got["seat-1"] != 150 {
				t.Errorf("after a start over a restored event log the 23rd holds %v, want "+
					"the 150 the fleet held", got)
			}
			if got := lines.said("usage_rederive_short"); got != 1 {
				t.Errorf("the short derivation was said %d time(s), want once", got)
			}
		})
	}
}

// A FIGURE A RESTORE LEFT SHORT DOES NOT HOLD BACK EVERY OTHER.
//
// A same-day restore that lost one failed turn's ending records leaves today's
// failures short of the fleet's, and no turn the seat runs afterwards fails. A
// guard that held a short derivation back WHOLE published none of that day's
// later turns or spend — for the rest of the day, and for good once the day was
// past being derived. Joined, the failure the fleet holds is kept and the new
// turn's spend goes out.
//
// Mutation: hold back a derivation short in any figure, and the seat's day
// stays at the 150 the restore found.
func TestAFigureARestoreLeftShortDoesNotHoldBackEveryOther(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	noon := time.Date(2026, 9, 23, 12, 0, 0, 0, santiago)
	ev := &events{t: t, db: own}
	ev.phase(noon.Add(-3*time.Hour), "seat-1", "t-a", "execute", "", 100, 1)
	ev.turn(noon.Add(-2*time.Hour), "seat-1", "dev", "t-a", true, time.Minute)
	ev.phase(noon.Add(-time.Hour), "seat-1", "t-b", "execute", "", 50, 1)
	first, log, _ := newClockedPublisher(t, own, nil, &clock{t: noon})
	if err := first.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	// THE RESTORE loses the failed turn's two ending records.
	forgetEvent(t, own, "ev-0002")
	forgetEvent(t, own, "ev-0003")
	ev.phase(noon.Add(30*time.Minute), "seat-1", "t-c", "execute", "", 1000, 1)
	ev.turn(noon.Add(40*time.Minute), "seat-1", "dev", "t-c", false, time.Minute)
	next, lines := publisherOn(t, own, nil, &clock{t: noon.Add(time.Hour)}, log)
	if err := next.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := spendOnLabel(t, own, "2026-09-23"); got["seat-1"] != 1150 {
		t.Errorf("the day holds %v, want the 1150 the seat spent — the new turn's spend "+
			"was held back with the failure the restore lost", got)
	}
	days, err := usage.SeatDays(t.Context(), own.Replicated(),
		usage.SpendQuery{From: "2026-09-23", To: "2026-09-23"})
	if err != nil || len(days) != 1 {
		t.Fatalf("read the seat-day: %v (%d rows)", err, len(days))
	}
	if days[0].Failed != 1 {
		t.Errorf("the day counts %d failed turn(s), want the fleet's 1", days[0].Failed)
	}
	if got := lines.said("usage_rederive_short"); got != 1 {
		t.Errorf("the short derivation was said %d time(s), want once", got)
	}
}

// NO DAY IS DERIVED BEFORE THE APPLIER HAS CAUGHT UP — and a node still catching
// up across a midnight owes the day it stopped in.
//
// What a day is joined with is the rows this node's applier wrote, and right
// after a restore those are the copy's, so today and yesterday wait for the
// applier exactly as an older day does. Skipped and not owed, today would leave
// the two days a tick looks at at the next midnight with nothing naming it, and
// never be published.
//
// Mutation: skip a window day before the applier catches up without owing it,
// and the 23rd is never published; or derive it before, and the first tick
// publishes it.
func TestNoDayIsDerivedBeforeTheApplierCatchesUp(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	noon := time.Date(2026, 9, 23, 12, 0, 0, 0, santiago)
	(&events{t: t, db: own}).phase(noon.Add(-time.Hour), "seat-1", "t-a", "execute", "", 100, 1)
	c := &clock{t: noon}
	var applied atomic.Bool
	p, _ := publisherWith(t, own, nil, c, &loopback{t: t, into: own, node: "node-a"},
		publisherOptions{caught: applied.Load})
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := spendOnLabel(t, own, "2026-09-23"); len(got) != 0 {
		t.Fatalf("a tick before the applier caught up published today: %v", got)
	}
	c.set(noon.Add(48 * time.Hour))
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	applied.Store(true)
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := spendOnLabel(t, own, "2026-09-23"); got["seat-1"] != 100 {
		t.Errorf("the 23rd holds %v once the applier caught up two days later, want 100", got)
	}
}

// replicatedFigure reads one figure of node-a's 2026-09-23 off the replicated
// rows.
func replicatedFigure(t *testing.T, own *store.DB, query string) int64 {
	t.Helper()
	var n int64
	if err := own.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), query, "2026-09-23").Scan(&n)
	}); err != nil {
		t.Fatalf("read %q: %v", query, err)
	}
	return n
}

// EVERY FIGURE OF A DAY IS HELD AGAINST THE FLEET — not a seat's spend alone.
//
// A restore can lose any record a day counted: a turn's end (its count, its
// failure, its reviews, its send-backs and its duration with it), a read, a
// fire, a person's question. Each case loses one and starts the node over the
// restored log; every figure must come back as the fleet held it.
//
// Mutation: join any one figure by the derivation alone — a seat's turns,
// failures, reviews, send-backs, durations or reads, a schedule's fires, a
// person's spend — or read any of them back from the rows as nothing, and that
// case's figure shrinks to what the restored log kept.
func TestEveryFigureOfADayIsHeldAgainstTheFleet(t *testing.T) {
	t.Parallel()
	noon := time.Date(2026, 9, 23, 12, 0, 0, 0, santiago)
	fire := func(t *testing.T, own *store.DB, at time.Time) {
		t.Helper()
		if _, err := sqlledger.New(own.SQL()).Claim(t.Context(), schedule.Run{
			FireKey: schedule.FireKey{Scope: "role", ScopeID: "Dev", ScheduleName: "standup",
				FireLabel: at.Format("20060102T1504"), TargetHandle: "dev"},
			ScheduledAt: at, FiredAt: at, Outcome: schedule.OutcomeFired,
		}); err != nil {
			t.Fatalf("record a fire: %v", err)
		}
	}
	const seat = ` FROM usage_turns WHERE day = ? AND node = 'node-a' AND agent_id = 'seat-1'`
	cases := []struct {
		name   string
		write  func(t *testing.T, own *store.DB, ev *events)
		forget func(t *testing.T, own *store.DB)
		read   string
		want   int64
	}{{
		name: "a seat's turns",
		write: func(t *testing.T, own *store.DB, ev *events) {
			ev.turn(noon.Add(-2*time.Hour), "seat-1", "dev", "t-a", false, time.Minute)
			ev.turn(noon.Add(-time.Hour), "seat-1", "dev", "t-b", false, time.Minute)
		},
		forget: func(t *testing.T, own *store.DB) { forgetEvent(t, own, "ev-0004") }, // t-b's turn_completed
		read:   `SELECT turns` + seat,
		want:   2,
	}, {
		name: "a seat's failures",
		write: func(t *testing.T, own *store.DB, ev *events) {
			// UNTALLIED: a tallied turn's failure is its turn_completed's, so
			// forgetting its agent_turn_completed changes nothing.
			at := noon.Add(-time.Hour)
			ev.add("agent_turn_completed", at, "seat-1", "Dev", map[string]any{
				"agent_id": "seat-1", "role": "Dev", "turn_id": "t-a", "failed": true})
			ev.add("turn_completed", at, "seat-1", "Dev", map[string]any{
				"agent_id": "seat-1", "agent_handle": "dev", "role": "Dev", "turn_id": "t-a",
				"duration_ms": 60000})
		},
		forget: func(t *testing.T, own *store.DB) { forgetEvent(t, own, "ev-0001") },
		read:   `SELECT failed` + seat,
		want:   1,
	}, {
		name: "a seat's reviews",
		write: func(t *testing.T, own *store.DB, ev *events) {
			ev.turn(noon.Add(-2*time.Hour), "seat-1", "dev", "t-a", false, time.Minute, "done")
			ev.turn(noon.Add(-time.Hour), "seat-1", "dev", "t-b", false, time.Minute, "self_iterate", "done")
		},
		forget: func(t *testing.T, own *store.DB) { forgetEvent(t, own, "ev-0004") },
		read:   `SELECT reviewed` + seat,
		want:   2,
	}, {
		name: "a seat's turns a review sent back",
		write: func(t *testing.T, own *store.DB, ev *events) {
			ev.turn(noon.Add(-2*time.Hour), "seat-1", "dev", "t-a", false, time.Minute, "done")
			ev.turn(noon.Add(-time.Hour), "seat-1", "dev", "t-b", false, time.Minute, "self_iterate", "done")
		},
		forget: func(t *testing.T, own *store.DB) { forgetEvent(t, own, "ev-0004") },
		read:   `SELECT reviewed - first_pass` + seat,
		want:   1,
	}, {
		name: "a seat's send-backs",
		write: func(t *testing.T, own *store.DB, ev *events) {
			ev.turn(noon.Add(-2*time.Hour), "seat-1", "dev", "t-a", false, time.Minute, "done")
			ev.turn(noon.Add(-time.Hour), "seat-1", "dev", "t-b", false, time.Minute, "self_iterate", "done")
		},
		forget: func(t *testing.T, own *store.DB) { forgetEvent(t, own, "ev-0004") },
		read:   `SELECT sent_back` + seat,
		want:   1,
	}, {
		name: "a seat's long turns",
		write: func(t *testing.T, own *store.DB, ev *events) {
			ev.turn(noon.Add(-2*time.Hour), "seat-1", "dev", "t-a", false, time.Minute)
			ev.turn(noon.Add(-time.Hour), "seat-1", "dev", "t-b", false, 3*time.Hour)
		},
		forget: func(t *testing.T, own *store.DB) { forgetEvent(t, own, "ev-0004") },
		// THE TURNS OF AN HOUR OR MORE, off the histogram's own bins.
		read: `SELECT COALESCE(SUM(json_extract(h.value, '$[1]')), 0)
		         FROM usage_turns AS u, json_each(u.duration_hist) AS h
		        WHERE u.day = ? AND u.node = 'node-a' AND u.agent_id = 'seat-1'
		          AND json_extract(h.value, '$[0]') >= ` + fmt.Sprint(hourBin) + ``,
		want: 1,
	}, {
		name: "a seat's reads",
		write: func(t *testing.T, own *store.DB, ev *events) {
			ev.read(noon.Add(-2*time.Hour), "seat-1", "t-a", "get_page", "", "p-1")
			ev.read(noon.Add(-time.Hour), "seat-1", "t-a", "get_page", "", "p-2")
		},
		forget: func(t *testing.T, own *store.DB) { forgetEvent(t, own, "ev-0002") },
		read: `SELECT COUNT(*) FROM usage_reads
		        WHERE day = ? AND node = 'node-a' AND agent_id = 'seat-1'`,
		want: 2,
	}, {
		name: "a schedule's fires",
		write: func(t *testing.T, own *store.DB, ev *events) {
			fire(t, own, noon.Add(-2*time.Hour))
			fire(t, own, noon.Add(-time.Hour))
		},
		forget: func(t *testing.T, own *store.DB) {
			if _, err := own.SQL().ExecContext(t.Context(),
				`DELETE FROM scheduled_runs WHERE fire_label = ?`,
				noon.Add(-time.Hour).Format("20060102T1504")); err != nil {
				t.Fatal(err)
			}
		},
		read: `SELECT COUNT(*) FROM usage_schedule_runs WHERE day = ? AND node = 'node-a'`,
		want: 2,
	}, {
		name: "a person's spend",
		write: func(t *testing.T, own *store.DB, ev *events) {
			ev.personAsked(noon.Add(-2*time.Hour), "maya", "Founder", 400, 20)
			ev.personAsked(noon.Add(-time.Hour), "maya", "Founder", 300, 20)
		},
		forget: func(t *testing.T, own *store.DB) { forgetEvent(t, own, "ev-0002") },
		read:   `SELECT COALESCE(SUM(total), 0) FROM usage_person_tokens WHERE day = ? AND node = 'node-a'`,
		want:   740,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			own := openStore(t)
			tc.write(t, own, &events{t: t, db: own})
			first, log, _ := newClockedPublisher(t, own, nil, &clock{t: noon})
			if err := first.Flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := replicatedFigure(t, own, tc.read); got != tc.want {
				t.Fatalf("the premise: the day was published at %d, want %d", got, tc.want)
			}
			tc.forget(t, own)
			if err := own.SetUsageWatermark(t.Context(), daysBack(noon, 0, 0)); err != nil {
				t.Fatal(err)
			}
			next, lines := publisherOn(t, own, nil, &clock{t: noon.Add(48 * time.Hour)}, log)
			if err := next.Flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := replicatedFigure(t, own, tc.read); got != tc.want {
				t.Errorf("after a start over a restored log the day holds %d, want the %d "+
					"the fleet held", got, tc.want)
			}
			if got := lines.said("usage_rederive_short"); got != 1 {
				t.Errorf("the short derivation was said %d time(s), want once", got)
			}
		})
	}
}

// hourBin is the histogram bin an hour-long turn is counted in.
var hourBin = func() int {
	b, err := json.Marshal(histOf(time.Hour))
	if err != nil {
		panic(err)
	}
	var pairs [][2]int
	if err := json.Unmarshal(b, &pairs); err != nil || len(pairs) != 1 {
		panic(fmt.Sprintf("an hour's histogram is %s", b))
	}
	return pairs[0][0]
}()

// A TURN WHOSE EARLIER RECORDS THE SWEEP REMOVED KEEPS ITS STATISTICS.
//
// A turn whose end states no tally is read off its records, and those reach
// back to wherever the turn started: a park lasts as long as its executor,
// which nothing stops on a timer, or as long as a person takes to answer. So
// a day inside the event log's read floor can hold such a turn whose earlier
// records the sweep has already removed, and derived again — a kept settle
// marks the day — it reads a shorter turn with fewer reviews. Joined with the
// fleet's record, the turn keeps the duration and the send-back it was
// published with, and the settle's new spend still goes out.
//
// Mutation: join a seat's histogram by the derivation's alone, or its
// first-pass turns by their own larger, and the swept turn shrinks or reads as
// accepted first time.
func TestATurnWhoseEarlierRecordsWereSweptKeepsItsStatistics(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	now := time.Now().UTC()
	y, m, d := now.Add(-28 * 24 * time.Hour).Date()
	end := time.Date(y, m, d, 12, 0, 0, 0, time.UTC)
	parked := end.Add(-5 * 24 * time.Hour)
	label := end.Format(time.DateOnly)
	ev := &events{t: t, db: own}
	// THE PARKED SEGMENT, from before the sweep's horizon: an hour's work and a
	// review that sent it back.
	ev.add("turn_completed", parked, "seat-1", "Dev", map[string]any{
		"agent_id": "seat-1", "agent_handle": "dev", "role": "Dev", "turn_id": "t-old",
		"duration_ms": (2 * time.Hour).Milliseconds(), "suspended": true,
	})
	ev.phase(parked, "seat-1", "t-old", "review", "self_iterate", 5, 1)
	// THE RESUMED END, inside the read floor, by a build before the tally.
	ev.phase(end.Add(-time.Minute), "seat-1", "t-old", "review", "done", 5, 1)
	ev.add("agent_turn_completed", end, "seat-1", "Dev", map[string]any{
		"agent_id": "seat-1", "role": "Dev", "turn_id": "t-old"})
	ev.add("turn_completed", end, "seat-1", "Dev", map[string]any{
		"agent_id": "seat-1", "agent_handle": "dev", "role": "Dev", "turn_id": "t-old",
		"duration_ms": time.Minute.Milliseconds(),
	})
	utc := func() *time.Location { return time.UTC }
	p, lines := publisherWith(t, own, nil, &clock{t: now},
		&loopback{t: t, into: own, node: "node-a"}, publisherOptions{zone: utc})
	if err := own.SetUsageWatermark(t.Context(), time.Date(y, m, d, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	seatDay := func() usage.SeatDay {
		t.Helper()
		days, err := usage.SeatDays(t.Context(), own.Replicated(), usage.SpendQuery{From: label, To: label})
		if err != nil || len(days) != 1 {
			t.Fatalf("read the seat-day: %v (%d rows)", err, len(days))
		}
		return days[0]
	}
	long := func(day usage.SeatDay) bool {
		q, ok := day.Durations.Quantile(1)
		return ok && q >= time.Hour
	}
	if got := seatDay(); got.Reviewed != 1 || got.FirstPass != 0 || got.SentBack != 1 || !long(got) {
		t.Fatalf("the premise: the turn was published as reviewed=%d first_pass=%d "+
			"sent_back=%d (long: %v)", got.Reviewed, got.FirstPass, got.SentBack, long(got))
	}

	// THE SWEEP removes the parked segment's records, and a kept settle marks
	// the day.
	if _, err := own.Events().Purge(t.Context()); err != nil {
		t.Fatal(err)
	}
	derived, err := own.UsageForDay(t.Context(), store.UsageWindow{
		Start: time.Date(y, m, d, 0, 0, 0, 0, time.UTC), End: time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC),
		Accepted: "done", SentBack: "self_iterate"})
	if err != nil || len(derived.Seats) != 1 {
		t.Fatalf("derive the day after the sweep: %v (%d seats)", err, len(derived.Seats))
	}
	if got := derived.Seats[0].Turns; got.SentBack != 0 || got.FirstPass != 1 ||
		len(got.Durations) != 1 || got.Durations[0] >= time.Hour {
		t.Fatalf("the premise: after the sweep the day still derives the turn whole: %+v", got)
	}
	keep(t.Context(), t, own, "batch-late", custodyPhase(t, "late-1", end.Add(time.Hour), "seat-1", 9))
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := seatDay()
	if got.Reviewed != 1 || got.FirstPass != 0 || got.SentBack != 1 || !long(got) {
		t.Errorf("after the sweep the turn reads reviewed=%d first_pass=%d sent_back=%d "+
			"(long: %v), want what it was published with", got.Reviewed, got.FirstPass,
			got.SentBack, long(got))
	}
	if spend := spendOnLabel(t, own, label); spend["seat-1"] != 5+9 {
		t.Errorf("the day's spend is %v, want the settle's 9 beside its own review's 5", spend)
	}
	if got := lines.said("usage_rederive_short"); got != 1 {
		t.Errorf("the short derivation was said %d time(s), want once", got)
	}
}

// A TICK BETWEEN THE OLD CLOCK'S MIDNIGHT AND THE NEW ONE'S IS A DAY.
//
// Moved west, the hours between the old clock's midnight and the new clock's
// are on a label already cut — the day before, on the old clock — whose cut
// ended before them. They belong to the next label, cut for the first time to
// start where that one ended. Looked for on the new clock's own label alone, a
// tick in those hours found no day holding its instant and refused, on every
// data node, for the width of the move — publishing nothing, a stop's final
// flush included — with a line saying the clock had moved by more than a day.
//
// Mutation: answer only the label the instant falls on in the company's clock,
// and the tick inside the move fails.
func TestATickInsideAWestwardMoveIsADay(t *testing.T) {
	t.Parallel()
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	own := openStore(t)
	zone := atomic.Pointer[time.Location]{}
	zone.Store(time.UTC)
	at := &clock{t: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)}
	ev := &events{t: t, db: own}
	ev.phase(at.now().Add(-time.Hour), "seat-1", "t-a", "execute", "", 5, 1)
	log := &loopback{t: t, into: own, node: "node-a"}
	p, _ := publisherWith(t, own, nil, at, log, publisherOptions{zone: zone.Load})
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	zone.Store(newYork)
	at.set(time.Date(2026, 9, 24, 1, 0, 0, 0, time.UTC)) // 21:00 on the 23rd in New York
	ev.phase(time.Date(2026, 9, 24, 0, 30, 0, 0, time.UTC), "seat-6", "t-gap", "execute", "", 60, 1)
	if err := p.Flush(t.Context()); err != nil {
		t.Fatalf("a tick between the two midnights: %v", err)
	}
	for label, want := range map[string]int64{"2026-09-23": 0, "2026-09-24": 60} {
		if got := spendOnLabel(t, own, label)["seat-6"]; got != want {
			t.Errorf("the %s holds %d of the row in the gap, want %d", label, got, want)
		}
	}
	at.set(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC))
	if err := p.Flush(t.Context()); err != nil {
		t.Fatalf("a tick after the new clock's midnight: %v", err)
	}
	if got := spendOnLabel(t, own, "2026-09-24")["seat-6"]; got != 60 {
		t.Errorf("the 24th holds %d of the row in the gap later, want 60", got)
	}
}

// A MARK BEFORE THE FIRST CUT OF A MOVED-EAST CLOCK IS A DAY.
//
// Moved east, an instant before a label's first cut can fall on that label in
// the new clock: its cut, made on the old one, starts after it. The instant
// belongs to the label before, cut for the first time to end where that one
// starts. Refused, a kept settle's mark there stopped every older day the
// node owed, every tick, for as long as the mark stood — and once the mark
// aged past the floor, those days were never derived again.
//
// Mutation: answer only the label the instant falls on in the company's clock,
// and the mark's day and the later settle's are never published.
func TestAMarkBeforeAMovedClocksFirstCutIsADay(t *testing.T) {
	t.Parallel()
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	own := openStore(t)
	zone := atomic.Pointer[time.Location]{}
	zone.Store(time.UTC)
	at := &clock{t: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)}
	(&events{t: t, db: own}).phase(time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC),
		"seat-1", "t-a", "execute", "", 5, 1)
	p, _ := publisherWith(t, own, nil, at, &loopback{t: t, into: own, node: "node-a"},
		publisherOptions{zone: zone.Load})
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	zone.Store(tokyo)
	at.set(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC))
	keep(t.Context(), t, own, "batch-east", custodyPhase(t, "e1",
		time.Date(2026, 9, 19, 20, 0, 0, 0, time.UTC), "seat-2", 300))
	if err := p.Flush(t.Context()); err != nil {
		t.Fatalf("a tick owing a day before the moved clock's first cut: %v", err)
	}
	if got := spendOnLabel(t, own, "2026-09-19")["seat-2"]; got != 300 {
		t.Errorf("the 19th holds %d of the settled row, want 300", got)
	}
	if marked(t, own) {
		t.Error("a pass that derived the mark's day kept the mark")
	}
	keep(t.Context(), t, own, "batch-later", custodyPhase(t, "e2",
		time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC), "seat-1", 7))
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := spendOnLabel(t, own, "2026-09-21")["seat-1"]; got != 5+7 {
		t.Errorf("the 21st holds %d of seat-1's spend after a later settle, want 12", got)
	}
}

// A CLOCK MOVED BY MORE THAN A DAY DOES NOT COUNT A ROW TWICE.
//
// Moved twenty-six hours east, the label after the last one cut is left with no
// instants of its own — the cut before covers it — and the next label's natural
// day reaches back into that earlier cut. Abutting only the adjacent label,
// which is uncut, the new day took those instants too, and a row in them was
// published on both days.
//
// Mutation: clamp a first cut only by the adjacent label before it (drop
// `|| prev.End.After(cut.Start)`), and the row is counted on the 23rd and the
// 25th. The later side is [TestAFirstCutEndsWhereAFartherDayStarts]'s.
func TestAClockMovedMoreThanADayCountsARowOnce(t *testing.T) {
	t.Parallel()
	behind, err := time.LoadLocation("Etc/GMT+12")
	if err != nil {
		t.Fatal(err)
	}
	ahead, err := time.LoadLocation("Pacific/Kiritimati")
	if err != nil {
		t.Fatal(err)
	}
	own := openStore(t)
	zone := atomic.Pointer[time.Location]{}
	zone.Store(behind)
	at := &clock{t: time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)} // noon on the 23rd behind
	ev := &events{t: t, db: own}
	ev.phase(at.now().Add(-time.Hour), "seat-1", "t-a", "execute", "", 5, 1)
	p, _ := publisherWith(t, own, nil, at, &loopback{t: t, into: own, node: "node-a"},
		publisherOptions{zone: zone.Load})
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	zone.Store(ahead)
	ev.phase(time.Date(2026, 9, 24, 11, 0, 0, 0, time.UTC), "seat-6", "t-seam", "execute", "", 60, 1)
	at.set(time.Date(2026, 9, 24, 13, 0, 0, 0, time.UTC))
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	counted := 0
	for _, label := range []string{"2026-09-23", "2026-09-24", "2026-09-25"} {
		if got := spendOnLabel(t, own, label)["seat-6"]; got != 0 {
			counted++
			if label != "2026-09-23" || got != 60 {
				t.Errorf("the %s holds %d of the row the 23rd's cut holds", label, got)
			}
		}
	}
	if counted != 1 {
		t.Errorf("the row is counted on %d day(s), want exactly one", counted)
	}
}

// AND A CUT TWO LABELS ON CLAMPS A FIRST CUT FROM THE OTHER SIDE.
//
// A clock moved west by more than a day leaves the label after a new first cut
// uncut, and the label two on, cut on the old clock, starts before the new
// day's natural end. Abutting only the adjacent label, which is uncut, the new
// day ran to its natural end and took the farther day's first hours, so a row
// in them was published on both days.
//
// Mutation: clamp a first cut only by the adjacent label after it (drop
// `|| next.Start.Before(cut.End)`), and the row is counted on the 7th and the
// 9th.
func TestAFirstCutEndsWhereAFartherDayStarts(t *testing.T) {
	t.Parallel()
	ahead, err := time.LoadLocation("Pacific/Kiritimati")
	if err != nil {
		t.Fatal(err)
	}
	behind, err := time.LoadLocation("Etc/GMT+12")
	if err != nil {
		t.Fatal(err)
	}
	own := openStore(t)
	zone := atomic.Pointer[time.Location]{}
	zone.Store(ahead)
	at := &clock{t: time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)} // 02:00 on the 10th, ahead
	ev := &events{t: t, db: own}
	ev.phase(time.Date(2026, 9, 8, 11, 0, 0, 0, time.UTC), "seat-6", "t-seam", "execute", "", 60, 1)
	p, _ := publisherWith(t, own, nil, at, &loopback{t: t, into: own, node: "node-a"},
		publisherOptions{zone: zone.Load})
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	zone.Store(behind)
	at.set(time.Date(2026, 9, 10, 13, 0, 0, 0, time.UTC))
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	keep(t.Context(), t, own, "batch-w1", custodyPhase(t, "w1",
		time.Date(2026, 9, 8, 0, 30, 0, 0, time.UTC), "seat-2", 300))
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := spendOnLabel(t, own, "2026-09-07")["seat-2"]; got != 300 {
		t.Fatalf("the premise: the 7th the mark owed was not derived (seat-2 %d, want 300)", got)
	}
	cut, ok, err := own.UsageDayWindow(t.Context(), "2026-09-07")
	if err != nil || !ok {
		t.Fatalf("the 7th was not cut: ok=%v err=%v", ok, err)
	}
	if want := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC); !cut.End.Equal(want) {
		t.Errorf("the 7th ends %v, want %v — where the 9th's recorded cut starts", cut.End, want)
	}
	counted := 0
	for _, label := range []string{"2026-09-07", "2026-09-08", "2026-09-09", "2026-09-10", "2026-09-11"} {
		if got := spendOnLabel(t, own, label)["seat-6"]; got != 0 {
			counted++
			if label != "2026-09-09" || got != 60 {
				t.Errorf("the %s holds %d of the row the 9th's cut holds", label, got)
			}
		}
	}
	if counted != 1 {
		t.Errorf("the row is counted on %d day(s), want exactly one", counted)
	}
}

// A TICK INSIDE A WESTWARD MOVE OF MORE THAN A DAY IS A DAY.
//
// Moved twenty-six hours west, an instant can fall on a label whose cut ended
// before it, and the label after that has ended too. The instant belongs to
// the label after both, cut for the first time where the second one ended.
// Stepped once, a tick in those hours found no cut holding it and refused, so
// today, yesterday and a stop's final flush were never published.
//
// Mutation: let dayAt's walk take one step rather than up to three, and the
// tick fails with "no day this node cuts holds".
func TestAClockMovedMoreThanADayWestIsADay(t *testing.T) {
	t.Parallel()
	behind, err := time.LoadLocation("Etc/GMT+12")
	if err != nil {
		t.Fatal(err)
	}
	ahead, err := time.LoadLocation("Pacific/Kiritimati")
	if err != nil {
		t.Fatal(err)
	}
	own := openStore(t)
	zone := atomic.Pointer[time.Location]{}
	zone.Store(ahead)
	at := &clock{t: time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)} // 02:00 on the 10th, ahead
	ev := &events{t: t, db: own}
	ev.phase(at.now().Add(-time.Hour), "seat-1", "t-a", "execute", "", 5, 1)
	p, _ := publisherWith(t, own, nil, at, &loopback{t: t, into: own, node: "node-a"},
		publisherOptions{zone: zone.Load})
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	zone.Store(behind)
	ev.phase(time.Date(2026, 9, 10, 10, 30, 0, 0, time.UTC), "seat-6", "t-west", "execute", "", 60, 1)
	at.set(time.Date(2026, 9, 10, 11, 0, 0, 0, time.UTC)) // 23:00 on the 9th, behind
	if err := p.Flush(t.Context()); err != nil {
		t.Fatalf("a tick inside a westward move of more than a day: %v", err)
	}
	for label, want := range map[string]int64{"2026-09-09": 0, "2026-09-10": 0, "2026-09-11": 60} {
		if got := spendOnLabel(t, own, label)["seat-6"]; got != want {
			t.Errorf("the %s holds %d of the row, want %d", label, got, want)
		}
	}
}

// A DAY CUT AFTER AN OUTAGE DOES NOT SWALLOW THE UNCUT DAY BEFORE IT.
//
// A node down for a day leaves a label nobody cut, and the next tick cuts today
// before the day it missed. A recorded cut two labels back that ENDS before
// today begins is that outage, not a clock move, so today keeps its own start
// and the missed day is cut on its own.
//
// Mutation: abut the nearest recorded cut whichever label it is, and the 12th
// takes the 11th's instants and the 11th is never cut.
func TestADayCutAfterAnOutageKeepsItsOwnStart(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	at := &clock{t: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)}
	ev := &events{t: t, db: own}
	ev.phase(at.now().Add(-time.Hour), "seat-1", "t-a", "execute", "", 5, 1)
	utc := func() *time.Location { return time.UTC }
	p, _ := publisherWith(t, own, nil, at, &loopback{t: t, into: own, node: "node-a"},
		publisherOptions{zone: utc})
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	ev.phase(time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC), "seat-7", "t-out", "execute", "", 70, 1)
	at.set(time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC))
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	cut, ok, err := own.UsageDayWindow(t.Context(), "2026-09-11")
	if err != nil || !ok {
		t.Fatalf("the 11th was not cut: ok=%v err=%v", ok, err)
	}
	if want := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC); !cut.Start.Equal(want) ||
		!cut.End.Equal(want.AddDate(0, 0, 1)) {
		t.Errorf("the 11th is cut %v..%v, want its own day", cut.Start, cut.End)
	}
	if got := spendOnLabel(t, own, "2026-09-11")["seat-7"]; got != 70 {
		t.Errorf("the 11th holds %d of its row, want 70", got)
	}
	if got := spendOnLabel(t, own, "2026-09-12")["seat-7"]; got != 0 {
		t.Errorf("the 12th holds %d of the 11th's row", got)
	}
}

// AND A DAY CUT BEFORE AN OUTAGE KEEPS ITS OWN END.
//
// The later side of the same rule: a day cut for the first time behind an
// uncut day, with a recorded cut two labels on that STARTS after this day
// ends, is an outage — the node was down across the day in between — and not
// a clock move, so this day keeps its own end and the day between is cut on
// its own.
//
// Mutation: abut a farther later cut whether or not it overlaps (clamp on
// `true`), and the 10th takes the 11th's instants and the 11th is never cut.
func TestADayCutBeforeAnOutageKeepsItsOwnEnd(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	at := &clock{t: time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)}
	ev := &events{t: t, db: own}
	utc := func() *time.Location { return time.UTC }
	p, _ := publisherWith(t, own, nil, at, &loopback{t: t, into: own, node: "node-a"},
		publisherOptions{zone: utc})
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	ev.phase(time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC), "seat-7", "t-out", "execute", "", 70, 1)
	keep(t.Context(), t, own, "batch-b10", custodyPhase(t, "b10",
		time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC), "seat-2", 300))
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := spendOnLabel(t, own, "2026-09-10")["seat-2"]; got != 300 {
		t.Fatalf("the premise: the 10th the mark owed was not derived (seat-2 %d, want 300)", got)
	}
	cut, ok, err := own.UsageDayWindow(t.Context(), "2026-09-10")
	if err != nil || !ok {
		t.Fatalf("the 10th was not cut: ok=%v err=%v", ok, err)
	}
	if want := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC); !cut.End.Equal(want) {
		t.Errorf("the 10th ends %v, want its own end %v", cut.End, want)
	}
	if got := spendOnLabel(t, own, "2026-09-11")["seat-7"]; got != 70 {
		t.Errorf("the 11th holds %d of its row, want 70", got)
	}
	if got := spendOnLabel(t, own, "2026-09-10")["seat-7"]; got != 0 {
		t.Errorf("the 10th holds %d of the 11th's row", got)
	}
}

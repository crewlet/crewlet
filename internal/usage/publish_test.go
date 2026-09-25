package usage_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/schedule/sqlledger"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/usage"
)

// santiago is a company clock far from UTC, so a day cut in UTC and a day cut
// on the company's clock put the same event on different days.
var santiago = func() *time.Location {
	loc, err := time.LoadLocation("America/Santiago")
	if err != nil {
		panic(err)
	}
	return loc
}()

// newPublisher is node `node`'s publisher over its own store, publishing
// through a loopback into the replicated estate of `into`, at a fixed clock.
func newPublisher(t *testing.T, node string, own, into *store.DB, now time.Time) (*usage.Publisher, *loopback) {
	t.Helper()
	log := &loopback{t: t, into: into}
	p, err := usage.NewPublisher(usage.PublisherDeps{
		Store: own, Log: log, NodeID: node,
		Zone: func() *time.Location { return santiago },
		Now:  func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("build the publisher: %v", err)
	}
	return p, log
}

// A NODE'S DAY IS DERIVED FROM ITS OWN RECORDS, cut on the company's clock.
//
// Every number here has a reader that renders it: the spend by phase and model,
// the turns and how they ended, the pages a seat leaned on, the schedules that
// fired. Each is asserted against the records it came from, and one event is
// placed where a UTC cut and the company's cut disagree about the day.
func TestANodesDayIsDerivedFromItsOwnRecords(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	// 2026-09-23 in Santiago (UTC-3): 09:00 local is 12:00 UTC.
	local := func(h, m int) time.Time { return time.Date(2026, 9, 23, h, m, 0, 0, santiago) }
	ev := &events{t: t, db: db}

	// Turn t1: executed, reviewed, sent back once, then accepted.
	ev.phase(local(9, 0), "seat-1", "t1", "execute", "", 1000, 200)
	ev.phase(local(9, 1), "seat-1", "t1", "review", "self_iterate", 300, 50)
	ev.phase(local(9, 2), "seat-1", "t1", "execute", "", 800, 100)
	ev.phase(local(9, 3), "seat-1", "t1", "review", "done", 200, 40)
	ev.turn(local(9, 4), "seat-1", "dev", "t1", false, 4*time.Minute)
	// Turn t2: accepted by its first review — a first pass.
	ev.phase(local(10, 0), "seat-1", "t2", "execute", "", 500, 50)
	ev.phase(local(10, 1), "seat-1", "t2", "review", "done", 100, 20)
	ev.turn(local(10, 2), "seat-1", "dev", "t2", false, 2*time.Minute)
	// Turn t3: failed, never reviewed.
	ev.phase(local(11, 0), "seat-1", "t3", "execute", "", 50, 0)
	ev.turn(local(11, 1), "seat-1", "dev", "t3", true, 30*time.Second)
	// Reads: one search that reached two pages, one page read twice.
	ev.read(local(9, 30), "seat-1", "t1", "search", "deploy rollback", "p-deploy", "p-runbook")
	ev.read(local(9, 40), "seat-1", "t1", "get_page", "", "p-deploy")
	ev.read(local(10, 30), "seat-1", "t2", "get_page", "", "p-deploy")
	// 23:30 local is 02:30 UTC the NEXT day — the company's day, not UTC's.
	ev.phase(local(23, 30), "seat-1", "t4", "execute", "", 7, 3)
	// And a fire, dispatched to dev at 08:00.
	if _, err := sqlledger.New(db.SQL()).Claim(t.Context(), schedule.Run{
		FireKey: schedule.FireKey{Scope: "role", ScopeID: "Dev",
			ScheduleName: "standup", FireLabel: "20260923T0800", TargetHandle: "dev"},
		ScheduledAt: local(8, 0), FiredAt: local(8, 0),
		Outcome: schedule.OutcomeFired, TraceID: "trace-standup",
	}); err != nil {
		t.Fatalf("record a fire: %v", err)
	}

	p, _ := newPublisher(t, "node-a", db, db, local(23, 50))
	if err := p.Flush(t.Context()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	rows, err := usage.Spend(t.Context(), db.Replicated(), usage.SpendQuery{From: "2026-09-23", To: "2026-09-23"})
	if err != nil {
		t.Fatal(err)
	}
	var in, out, cache, calls int64
	for _, r := range rows {
		if r.Node != "node-a" || r.AgentID != "seat-1" || r.Handle != "dev" {
			t.Errorf("a spend row names node %q seat %q handle %q", r.Node, r.AgentID, r.Handle)
		}
		in, out, cache, calls = in+r.Input, out+r.Output, cache+r.CacheRead, calls+r.Calls
	}
	if in != 2957 || out != 463 || calls != 8 {
		t.Errorf("the day's spend is %d in / %d out over %d calls, want 2957 / 463 "+
			"over 8 — including the 23:30 phase, which is this day on the "+
			"company's clock and the next one in UTC", in, out, calls)
	}
	// Every phase's cache read is half its input, rounded down per phase.
	if cache != 1478 {
		t.Errorf("the day's cache read is %d, want 1478 — the promoted column, "+
			"never an addition to the input", cache)
	}

	d := dump(t, db)
	if len(d["usage_turns"]) != 1 {
		t.Fatalf("head rows: %v", d["usage_turns"])
	}
	head := d["usage_turns"][0]
	for _, want := range []string{"turns=3", "failed=1", "reviewed=2", "first_pass=1", "sent_back=1", "handle=dev"} {
		if !contains(head, want) {
			t.Errorf("the head row %q does not carry %s", head, want)
		}
	}
	if got := len(d["usage_reads"]); got != 3 {
		t.Errorf("the reads fold to %d (page, via) rows, want 3: %v", got, d["usage_reads"])
	}
	var deploySearch, deployGet string
	for _, r := range d["usage_reads"] {
		switch {
		case contains(r, "page_id=p-deploy") && contains(r, "via=search"):
			deploySearch = r
		case contains(r, "page_id=p-deploy") && contains(r, "via=get_page"):
			deployGet = r
		}
	}
	if !contains(deployGet, "reads=2") || !contains(deployGet, "last_turn_id=t2") {
		t.Errorf("p-deploy by get_page is %q — two reads, the newest in t2", deployGet)
	}
	if !contains(deploySearch, "last_query=deploy rollback") {
		t.Errorf("p-deploy by search is %q — it carries the search's query", deploySearch)
	}
	if fires := d["usage_schedule_runs"]; len(fires) != 1 ||
		!contains(fires[0], "name=standup") || !contains(fires[0], "target=dev") ||
		!contains(fires[0], "outcome=fired") || !contains(fires[0], "trace_id=trace-standup") {
		t.Errorf("the fires are %v", fires)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// A REPUBLISHED DAY WRITES BYTE-IDENTICAL ROWS, AND AN UNCHANGED ONE PUBLISHES
// NOTHING.
//
// Two halves of one property. A process that restarts republishes today and
// yesterday in full, and every peer applies it — so a derivation that is not a
// pure function of the node's records would move every peer's rows on every
// restart. And a tick that finds the day unchanged must cost the stream
// nothing, or a quiet company writes a record per seat every fifteen seconds.
func TestARepublishedDayWritesByteIdenticalRows(t *testing.T) {
	t.Parallel()
	own, peer := openStore(t), openStore(t)
	at := time.Date(2026, 9, 23, 15, 0, 0, 0, santiago)
	ev := &events{t: t, db: own}
	ev.phase(at.Add(-2*time.Hour), "seat-1", "t1", "execute", "", 400, 40)
	ev.phase(at.Add(-2*time.Hour+time.Minute), "seat-1", "t1", "review", "done", 100, 10)
	ev.turn(at.Add(-2*time.Hour+2*time.Minute), "seat-1", "dev", "t1", false, 3*time.Minute)
	ev.read(at.Add(-time.Hour), "seat-1", "t1", "prefetch", "q", "p-1", "p-2")

	first, log := newPublisher(t, "node-a", own, peer, at)
	if err := first.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	sent := log.sent()
	if sent == 0 {
		t.Fatal("the first flush published nothing")
	}
	before := dump(t, peer)

	if err := first.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if log.sent() != sent {
		t.Fatalf("a flush over an unchanged day published %d more record(s)",
			log.sent()-sent)
	}

	// A FRESH PROCESS — a restart — republishes everything it derives.
	restarted, log2 := newPublisher(t, "node-a", own, peer, at)
	if err := restarted.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if log2.sent() == 0 {
		t.Fatal("a restarted node republished nothing, so a day whose last flush " +
			"was lost across the restart would never reach the fleet")
	}
	for i, body := range log2.payloads {
		if string(body) != string(log.payloads[i]) {
			t.Errorf("record %d republished as different bytes:\n%s\n%s",
				i, log.payloads[i], body)
		}
	}
	after := dump(t, peer)
	for _, table := range sortedKeys(before) {
		if !slices.Equal(before[table], after[table]) {
			t.Errorf("%s moved on a republish of the same day:\nbefore %v\nafter  %v",
				table, before[table], after[table])
		}
	}
}

// A BOOT AFTER MIDNIGHT REPUBLISHES YESTERDAY.
//
// A node that stopped at 23:59:50 lost whatever its last tick would have sent.
// When it comes back the day it was in is YESTERDAY, and a publisher that
// looked only at today would leave that day short on every node for good.
func TestABootAfterMidnightRepublishesYesterday(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	late := time.Date(2026, 9, 23, 23, 59, 50, 0, santiago)
	ev := &events{t: t, db: own}
	ev.phase(late, "seat-1", "t9", "execute", "", 123, 4)

	booted, _ := newPublisher(t, "node-a", own, own,
		time.Date(2026, 9, 24, 0, 0, 30, 0, santiago))
	if err := booted.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows, err := usage.Spend(t.Context(), own.Replicated(), usage.SpendQuery{From: "2026-09-23", To: "2026-09-24"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Day != "2026-09-23" || rows[0].Input != 123 {
		t.Fatalf("a node booted half a minute past midnight holds %+v for the day "+
			"it stopped in", rows)
	}
}

// AN UNRESOLVED PUBLISH IS SENT AGAIN.
//
// An unknown outcome means the broker never said. Counted as sent, a day's last
// change — the one that happened right before nothing else did, which is the
// end of every day — would never be retried and every peer would hold the day
// short.
func TestAnUnresolvedPublishIsSentAgain(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	at := time.Date(2026, 9, 23, 12, 0, 0, 0, santiago)
	(&events{t: t, db: own}).phase(at.Add(-time.Minute), "seat-1", "t1", "execute", "", 10, 1)

	p, log := newPublisher(t, "node-a", own, own, at)
	log.unresolved = 1
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if log.sent() != 2 {
		t.Fatalf("an unresolved publish followed by a quiet tick sent %d record(s), "+
			"want the retry", log.sent())
	}
	if log.requests[0].OpID != log.requests[1].OpID {
		t.Fatalf("the retry carries op id %s and the first attempt %s — the same "+
			"content must be the same operation, or the broker cannot collapse a "+
			"copy that did land", log.requests[1].OpID, log.requests[0].OpID)
	}
	rows, err := usage.Spend(t.Context(), own.Replicated(), usage.SpendQuery{From: "2026-09-23", To: "2026-09-23"})
	if err != nil || len(rows) != 1 {
		t.Fatalf("after the retry the day holds %v (%v)", rows, err)
	}
}

// THE READS CAP COUNTS WHAT IT DROPPED.
//
// A seat that searched all day reaches more pages than one record should carry,
// so the writer keeps the most-read and says how many it left out. A cap that
// dropped silently would make "no reads of this page" and "too many reads to
// list" the same answer.
func TestTheReadsCapCountsWhatItDropped(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	at := time.Date(2026, 9, 23, 18, 0, 0, 0, santiago)
	ev := &events{t: t, db: own}
	total := usage.ReadsPerSeatDay + 44
	for i := range total {
		ev.read(at.Add(-time.Duration(total-i)*time.Second), "seat-1", "t1", "get_page", "",
			fmt.Sprintf("p-%04d", i))
	}
	// The favourite: read three more times, so it is the most-read and must
	// survive the cap whatever its key sorts as.
	for range 3 {
		ev.read(at.Add(-time.Second/2), "seat-1", "t1", "get_page", "", "p-0000")
	}

	p, _ := newPublisher(t, "node-a", own, own, at)
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	d := dump(t, own)
	if got := len(d["usage_reads"]); got != usage.ReadsPerSeatDay {
		t.Fatalf("the seat-day holds %d read rows, want the cap %d", got, usage.ReadsPerSeatDay)
	}
	if !contains(d["usage_turns"][0], "reads_elided=44") {
		t.Fatalf("the head row %q does not count the 44 it dropped", d["usage_turns"][0])
	}
	if !slices.ContainsFunc(d["usage_reads"], func(r string) bool {
		return contains(r, "page_id=p-0000 ") && contains(r, "reads=4")
	}) {
		t.Fatal("the most-read page did not survive the cap")
	}
}

package usage_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/usage"
)

// personRecord is a person-day with one answer cell per model.
func personRecord(node, day, person string, models ...string) usage.Record {
	r := usage.Record{
		RecordEnvelope: usage.RecordEnvelope{Writer: node,
			Subject: usage.Subject{Kind: usage.KindPerson, Node: node, Day: day, Person: person}},
		Role: "Founder",
	}
	for i, m := range models {
		r.Tokens = append(r.Tokens, usage.Tokens{Phase: "auxiliary",
			Worker: string(types.AuxAnswerKnowledge), Model: m,
			Input: int64(400 * (i + 1)), Output: 20, Total: int64(400*(i+1)) + 20, Calls: 1})
	}
	return r
}

// personAsked records one person's question the way the publish listener
// writes it: no agent, the person as the envelope's actor.
func (e *events) personAsked(at time.Time, handle, role string, input, output int) {
	e.t.Helper()
	e.seq++
	rec := types.AuxiliarySpend{ActorSeat: handle, ActorRole: role,
		Stage: types.AuxStageOperator, Purpose: types.AuxAnswerKnowledge,
		Model: "haiku", ProviderKey: "cheap", Calls: 1,
		InputTokens: input, OutputTokens: output, TotalTokens: input + output}
	body, err := json.Marshal(rec)
	if err != nil {
		e.t.Fatalf("encode a person's record: %v", err)
	}
	if err := e.db.Events().Append(e.t.Context(), store.EventRecord{
		ID: fmt.Sprintf("ev-%04d", e.seq), Type: rec.EventType(), Source: handle,
		Category: "system", Time: at, Actor: rec.Actor(),
		Tags: store.ExtractTags(body), Payload: body,
	}); err != nil {
		e.t.Fatalf("append a person's record: %v", err)
	}
}

// A PERSON'S RECORD IS VERSION 2, AND NOTHING ELSE IS.
//
// A record is stamped with the LOWEST version that reads it: a person's day is
// the kind version 2 introduced, so a version-1 build defers it rather than
// applying a kind it has no table for — and a seat's or a schedule's day stays
// version 1, so the older half of a rolling upgrade goes on applying every one
// of them. A person's record stamped below its kind is refused at the writer,
// since a version-1 peer would read it as a writer's fault on every
// redelivery.
func TestAPersonsRecordIsTheOneStampedAtVersionTwo(t *testing.T) {
	t.Parallel()
	stamped := func(r usage.Record) int {
		t.Helper()
		body, err := r.Encode()
		if err != nil {
			t.Fatalf("encode %s: %v", r.Subject, err)
		}
		env, err := usage.DecodeEnvelope(body)
		if err != nil {
			t.Fatalf("decode %s: %v", r.Subject, err)
		}
		return env.V
	}
	if v := stamped(personRecord("node-a", "2026-09-23", "maya", "haiku")); v != usage.PersonVersion {
		t.Errorf("a person's record is stamped %d, want %d", v, usage.PersonVersion)
	}
	if v := stamped(seatRecord("node-a", "2026-09-23", "seat-1", "m")); v != 1 {
		t.Errorf("a seat's record is stamped %d, want 1 — every older build reads it", v)
	}
	low := personRecord("node-a", "2026-09-23", "maya", "haiku")
	low.V = 1
	if _, err := low.Encode(); err == nil {
		t.Error("a person's record stamped at version 1 was written")
	}
	if err := usage.VersionedFields().Check(usage.RecordVersion); err != nil {
		t.Errorf("the versioned fields disagree with the build's version: %v", err)
	}
	for _, bad := range []usage.Subject{
		{Kind: usage.KindPerson, Node: "node-a", Day: "2026-09-23"},
		{Kind: usage.KindPerson, Node: "node-a", Day: "2026-09-23", Person: "maya", Seat: "s"},
		{Kind: usage.KindSeat, Node: "node-a", Day: "2026-09-23", Seat: "s", Person: "maya"},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%+v validated", bad)
		}
	}
	withTurns := personRecord("node-a", "2026-09-23", "maya", "haiku")
	withTurns.Turns = &usage.Turns{Count: 1}
	if _, err := withTurns.Encode(); err == nil {
		t.Error("a person's record carrying a seat's turns was written")
	}
}

// A PERSON'S DAY APPLIES AS A SEAT'S DOES: replaced whole, never moved
// backwards, and gone with the record that makes it older than the history.
func TestAPersonsDayIsReplacedGuardedAndExpired(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	const day = "2026-09-23"
	for seq, rec := range []usage.Record{
		personRecord("node-a", day, "maya", "m-morning", "m-both"),
		personRecord("node-a", day, "maya", "m-both"),
	} {
		if err := applyRecord(t.Context(), db, recordAt(t, rec, uint64(seq+2))); err != nil {
			t.Fatalf("apply %d: %v", seq, err)
		}
	}
	read := func() []usage.PersonRow {
		t.Helper()
		rows, err := usage.PersonSpend(t.Context(), db.Replicated(),
			usage.PersonQuery{From: day, To: day})
		if err != nil {
			t.Fatalf("read the people's spend: %v", err)
		}
		return rows
	}
	rows := read()
	if len(rows) != 1 || rows[0].Model != "m-both" || rows[0].Person != "maya" ||
		rows[0].Role != "Founder" || rows[0].Input != 400 {
		t.Fatalf("after the afternoon's record maya's day is %+v, want its one cell", rows)
	}
	// AN OLDER RECORD replayed after the newer one changes nothing.
	if err := applyRecord(t.Context(), db, recordAt(t,
		personRecord("node-a", day, "maya", "m-stale"), 1)); err != nil {
		t.Fatalf("apply the older record: %v", err)
	}
	if rows = read(); len(rows) != 1 || rows[0].Model != "m-both" {
		t.Fatalf("an older record moved the day back to %+v", rows)
	}
	// A narrowed read is one person's alone.
	if err := applyRecord(t.Context(), db, recordAt(t,
		personRecord("node-a", day, "ana", "haiku"), 9)); err != nil {
		t.Fatalf("apply ana: %v", err)
	}
	narrowed, err := usage.PersonSpend(t.Context(), db.Replicated(),
		usage.PersonQuery{From: day, To: day, Person: "ana"})
	if err != nil || len(narrowed) != 1 || narrowed[0].Person != "ana" {
		t.Fatalf("ana's day = %+v (%v)", narrowed, err)
	}
	// THE HORIZON: a record far enough past the day removes it.
	late := seatRecord("node-a", "2027-06-01", "seat-1", "m")
	if err := applyRecord(t.Context(), db, recordAt(t, late, 10)); err != nil {
		t.Fatalf("apply the late record: %v", err)
	}
	if rows = read(); len(rows) != 0 {
		t.Fatalf("a person's day older than the history survived: %+v", rows)
	}
}

// fixedReaders is the counted set's answer, switchable between ticks.
type fixedReaders struct {
	readers map[string]int
	err     error
	asked   int
}

func (f *fixedReaders) read(context.Context) (map[string]int, error) {
	f.asked++
	return f.readers, f.err
}

// A PERSON'S DAY WAITS FOR EVERY READER.
//
// A node on a build from before the person kind does not defer its record: its
// envelope refuses a kind it does not know, which stops its whole usage
// applier. So the record is HELD while any node applying the log reads below
// its version — and while that cannot be read at all — and published by the
// first tick that finds every node reading it, with no new event needed to
// move the day. The seats' records of the same day go out meanwhile: every
// build reads them. And a day nobody asked a question in never asks who reads.
func TestAPersonsDayWaitsForEveryReader(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	at := time.Date(2026, 9, 23, 12, 0, 0, 0, santiago)
	ev := &events{t: t, db: own}
	ev.phase(at.Add(-2*time.Minute), "seat-1", "t1", "execute", "", 10, 1)

	gate := &fixedReaders{readers: map[string]int{"node-a": 2, "node-old": 1}}
	log := &loopback{t: t, into: own, node: "node-a"}
	p, err := usage.NewPublisher(usage.PublisherDeps{
		Store: own, Log: log, NodeID: "node-a",
		Zone:    func() *time.Location { return santiago },
		Now:     func() time.Time { return at },
		Readers: gate.read,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if gate.asked != 0 {
		t.Fatalf("a day with no person in it asked who reads one %d times", gate.asked)
	}

	ev.personAsked(at.Add(-time.Minute), "maya", "Founder", 400, 20)
	people := func() []usage.PersonRow {
		t.Helper()
		rows, err := usage.PersonSpend(t.Context(), own.Replicated(),
			usage.PersonQuery{From: "2026-09-23", To: "2026-09-23"})
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	ev.phase(at.Add(-30*time.Second), "seat-1", "t2", "execute", "", 20, 2)
	sentBefore := log.sent()
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if rows := people(); len(rows) != 0 {
		t.Fatalf("a person's day was published while node-old reads version 1: %+v", rows)
	}
	if log.sent() != sentBefore+1 {
		t.Fatalf("the tick sent %d record(s), want the seat's moved day alone",
			log.sent()-sentBefore)
	}

	// UNKNOWN IS NOT OPEN.
	gate.readers, gate.err = map[string]int{"node-a": 2}, errors.New("register unreachable")
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if rows := people(); len(rows) != 0 {
		t.Fatalf("a person's day was published past a counted set nobody read: %+v", rows)
	}

	// EVERY READER ON THE NEW BUILD, and nothing moved in the day: the
	// held record goes out on this tick all the same.
	gate.err = nil
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows := people()
	if len(rows) != 1 || rows[0].Person != "maya" || rows[0].Total != 420 {
		t.Fatalf("once every node reads it, maya's day is %+v, want her 420 tokens", rows)
	}
	// And it is not sent again.
	sent := log.sent()
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if log.sent() != sent {
		t.Fatalf("a released person's day was sent again (%d more)", log.sent()-sent)
	}
}

// A HOLD OUTLASTS YESTERDAY. The publisher derives only today and yesterday, so
// a person's day held through a rolling upgrade longer than that is never
// derived again — and dropped with the other days' memory, it never reached
// the named windows at all. It is kept until it is published, back to the
// history's horizon.
func TestAPersonsDayHeldPastYesterdayIsStillPublished(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	day1 := time.Date(2026, 9, 23, 12, 0, 0, 0, santiago)
	(&events{t: t, db: own}).personAsked(day1.Add(-time.Minute), "maya", "Founder", 400, 20)

	now := day1
	gate := &fixedReaders{readers: map[string]int{"node-a": 2, "node-old": 1}}
	p, err := usage.NewPublisher(usage.PublisherDeps{
		Store: own, Log: &loopback{t: t, into: own, node: "node-a"}, NodeID: "node-a",
		Zone:    func() *time.Location { return santiago },
		Now:     func() time.Time { return now },
		Readers: gate.read,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	now = day1.Add(72 * time.Hour)
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	gate.readers = map[string]int{"node-a": 2}
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows, err := usage.PersonSpend(t.Context(), own.Replicated(),
		usage.PersonQuery{From: "2026-09-23", To: "2026-09-23"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Total != 420 {
		t.Fatalf("a person's day held three days was published as %+v, want her 420", rows)
	}
}

// A HOLD SURVIVES THE PROCESS. A day older than yesterday is never derived
// again, by this process or the next, so a person's day held in memory alone
// was lost with a restart during the rolling upgrade — on the counter and the
// live window, never in the named windows, with nothing to say so. The hold is
// kept in the node's own store, and a new publisher over that store publishes
// it once every reader reads it, without the day being derived again; once
// published, nothing is held.
//
// Mutation: keep the hold in memory alone, and the rebuilt publisher publishes
// nothing for the 23rd.
func TestAPersonsHeldDaySurvivesARestart(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	day1 := time.Date(2026, 9, 23, 12, 0, 0, 0, santiago)
	(&events{t: t, db: own}).personAsked(day1.Add(-time.Minute), "maya", "Founder", 400, 20)

	gate := &fixedReaders{readers: map[string]int{"node-a": 2, "node-old": 1}}
	build := func(now time.Time) *usage.Publisher {
		t.Helper()
		p, err := usage.NewPublisher(usage.PublisherDeps{
			Store: own, Log: &loopback{t: t, into: own, node: "node-a"}, NodeID: "node-a",
			Zone:    func() *time.Location { return santiago },
			Now:     func() time.Time { return now },
			Readers: gate.read,
		})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if err := build(day1).Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	held, err := own.HeldUsage(t.Context())
	if err != nil || len(held) != 1 || held[0].Day != "2026-09-23" {
		t.Fatalf("held = %+v (%v), want maya's day kept in the store", held, err)
	}

	// THE PROCESS IS GONE, and its successor starts three days on, when the
	// 23rd is no longer a day anybody derives — and every reader is upgraded.
	gate.readers = map[string]int{"node-a": 2}
	if err := build(day1.Add(72 * time.Hour)).Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows, err := usage.PersonSpend(t.Context(), own.Replicated(),
		usage.PersonQuery{From: "2026-09-23", To: "2026-09-23"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Person != "maya" || rows[0].Total != 420 {
		t.Fatalf("after a restart, maya's held day was published as %+v, want her 420", rows)
	}
	if held, err := own.HeldUsage(t.Context()); err != nil || len(held) != 0 {
		t.Fatalf("held after the release = %+v (%v), want nothing", held, err)
	}
}

// A STORED HOLD THIS BUILD CANNOT READ — one a newer build of this node wrote
// before it was rolled back — is the only copy of that day's spend, so it is
// left in the store for the build that wrote it; only once its day is past the
// history's horizon, where the applier would expire it on arrival, is it let
// go. Without that, a table empty outside an upgrade would keep it for the life
// of the node.
//
// Mutation: drop the horizon check, and the recent row is deleted; drop the
// release, and the ancient one is kept.
func TestAnUnreadableHoldIsKeptUntilItsDayLeavesTheHistory(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, santiago)
	for _, day := range []string{"2026-01-04", "2026-09-21"} {
		if err := own.HoldUsage(t.Context(), store.UsageHeld{Day: day,
			Subject: "person.node-a." + day + ".maya", Record: []byte(`{"v":99}`)},
			now); err != nil {
			t.Fatal(err)
		}
	}
	p, err := usage.NewPublisher(usage.PublisherDeps{
		Store: own, Log: &loopback{t: t, into: own, node: "node-a"}, NodeID: "node-a",
		Zone: func() *time.Location { return santiago },
		Now:  func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	held, err := own.HeldUsage(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 1 || held[0].Day != "2026-09-21" {
		t.Fatalf("held = %+v, want only the 21st kept: the 4th of January is past "+
			"the history's horizon", held)
	}
}

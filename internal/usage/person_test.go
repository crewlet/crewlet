package usage_test

import (
	"encoding/json"
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

// A PERSON'S RECORD IS THE BASE FORMAT, as every kind this build writes is.
//
// A record is stamped with the LOWEST version that reads it, and a person's day
// carries nothing the base format lacks: it is version 1 like a seat's, so
// every node applying the usage log applies it the moment it is published.
func TestAPersonsRecordIsTheBaseFormat(t *testing.T) {
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
	if v := stamped(personRecord("node-a", "2026-09-23", "maya", "haiku")); v != 1 {
		t.Errorf("a person's record is stamped %d, want the base format's 1", v)
	}
	if v := stamped(seatRecord("node-a", "2026-09-23", "seat-1", "m")); v != 1 {
		t.Errorf("a seat's record is stamped %d, want the base format's 1", v)
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

// A PERSON'S DAY IS PUBLISHED WITH THE REST OF ITS DAY. The tick that derives a
// day a person asked a question in publishes that person's record beside the
// seats' — no reader is waited on, since every node applying the log reads it —
// and a tick that finds the day unchanged sends it no second time.
//
// Mutation: leave a person's records out of [usage.Publisher.Records], and
// maya's day never reaches the replicated rows.
func TestAPersonsDayIsPublishedWithItsDay(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	at := time.Date(2026, 9, 23, 12, 0, 0, 0, santiago)
	ev := &events{t: t, db: own}
	ev.phase(at.Add(-2*time.Minute), "seat-1", "t1", "execute", "", 10, 1)
	ev.personAsked(at.Add(-time.Minute), "maya", "Founder", 400, 20)

	log := &loopback{t: t, into: own, node: "node-a"}
	p, err := usage.NewPublisher(usage.PublisherDeps{
		Store: own, Log: log, NodeID: "node-a",
		Zone: func() *time.Location { return santiago },
		Now:  func() time.Time { return at },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows, err := usage.PersonSpend(t.Context(), own.Replicated(),
		usage.PersonQuery{From: "2026-09-23", To: "2026-09-23"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Person != "maya" || rows[0].Role != "Founder" ||
		rows[0].Total != 420 {
		t.Fatalf("after one tick maya's day is %+v, want her 420 tokens", rows)
	}
	sent := log.sent()
	if sent != 2 {
		t.Fatalf("the tick sent %d record(s), want the seat's day and maya's", sent)
	}
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if log.sent() != sent {
		t.Fatalf("an unchanged person's day was sent again (%d more)", log.sent()-sent)
	}
}

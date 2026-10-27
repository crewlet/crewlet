package usage_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/schedule/sqlledger"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/usage"
)

// THE JOIN RAISES EVERY FIGURE THE FLEET HOLDS MORE OF, and nothing else.
//
// What a node publishes for an object is its derivation joined with the
// fleet's record of it, and the join is what makes a node's day never shrink
// without freezing it: each figure the fleet holds more of is raised to the
// fleet's, each the derivation holds more of is the derivation's, and every
// label the derivation states is the derivation's — a read entry's newest
// context included, where the derivation read it later. A handle or a role the
// derivation leaves EMPTY keeps the fleet's: that is a seat the chart no
// longer holds whose day here is spend alone, not a newer name. First-pass
// turns are the one figure that FALLS as a day grows (a review that sends a
// turn back takes it out), so the turns with an unaccepted review are what
// join and first-pass is read back.
//
// Mutation: join any one figure the other way — the smaller, or a first-pass
// count by its own larger — or keep the fleet's label over the derivation's,
// or a read entry's older context over the derivation's newer one, or take an
// empty handle or role as the derivation's, and that row of the expectation
// goes red.
func TestAJoinRaisesEveryFigureTheFleetHoldsMoreOf(t *testing.T) {
	t.Parallel()
	at := func(h int) time.Time { return time.Date(2026, 9, 23, h, 0, 0, 0, time.UTC) }
	day := func(kind usage.Kind) usage.Subject {
		s := usage.Subject{Kind: kind, Node: "node-a", Day: "2026-09-23"}
		switch kind {
		case usage.KindSeat:
			s.Seat = "seat-1"
		case usage.KindPerson:
			s.Person = "maya"
		case usage.KindSchedule:
			s.ScopeType, s.ScopeID, s.Schedule = "role", "Dev", "standup"
		}
		return s
	}
	record := func(kind usage.Kind) usage.Record {
		return usage.Record{RecordEnvelope: usage.RecordEnvelope{Subject: day(kind)}}
	}
	cell := func(phase string, input, calls int64) usage.Tokens {
		return usage.Tokens{Phase: phase, Model: "m-1", ProviderKey: "primary",
			Input: input, Output: 1, Total: input + 1, Calls: calls}
	}

	t.Run("a seat", func(t *testing.T) {
		t.Parallel()
		derived := record(usage.KindSeat)
		derived.Handle, derived.Role = "dev-now", "Dev"
		derived.Turns = &usage.Turns{Count: 2, Reviewed: 2, FirstPass: 2,
			Durations: histOf(time.Minute, time.Minute), LastEndedAt: at(10)}
		derived.Tokens = []usage.Tokens{cell("execute", 100, 1)}
		derived.Reads = []usage.Read{{Backend: "native", PageID: "p-1", Via: "get_page",
			Count: 1, LastAt: at(9), LastQuery: "now"}}

		held := record(usage.KindSeat)
		held.Handle, held.Role = "dev-then", "Lead"
		held.Turns = &usage.Turns{Count: 3, Failed: 1, Reviewed: 3, FirstPass: 1, SentBack: 2,
			Durations: histOf(time.Minute, 2*time.Hour, 3*time.Hour), LastEndedAt: at(11)}
		held.Tokens = []usage.Tokens{cell("review", 5, 1), cell("execute", 150, 2)}
		held.Reads = []usage.Read{
			{Backend: "native", PageID: "p-1", Via: "get_page", Count: 3, LastAt: at(11), LastQuery: "then"},
			{Backend: "native", PageID: "p-2", Via: "get_page", Count: 1, LastAt: at(8)},
		}
		held.ReadsElided = 4

		got := usage.Join(derived, held)
		if got.Handle != "dev-now" || got.Role != "Dev" {
			t.Errorf("the join names the seat %q/%q, want the derivation's labels", got.Handle, got.Role)
		}
		want := usage.Turns{Count: 3, Failed: 1, Reviewed: 3, FirstPass: 1, SentBack: 2,
			Durations: histOf(time.Minute, 2*time.Hour, 3*time.Hour), LastEndedAt: at(11)}
		if g := *got.Turns; g.Count != want.Count || g.Failed != want.Failed ||
			g.Reviewed != want.Reviewed || g.FirstPass != want.FirstPass ||
			g.SentBack != want.SentBack || !g.LastEndedAt.Equal(want.LastEndedAt) ||
			histBytes(t, g.Durations) != histBytes(t, want.Durations) {
			t.Errorf("the joined turns are %+v (durations %s), want %+v (durations %s)",
				g, histBytes(t, g.Durations), want, histBytes(t, want.Durations))
		}
		wantCells := []usage.Tokens{cell("execute", 150, 2), cell("review", 5, 1)}
		if !slices.Equal(got.Tokens, wantCells) {
			t.Errorf("the joined spend is %+v, want %+v", got.Tokens, wantCells)
		}
		if len(got.Reads) != 2 || got.Reads[0].Count != 3 || got.Reads[0].LastQuery != "then" ||
			!got.Reads[0].LastAt.Equal(at(11)) || got.Reads[1].PageID != "p-2" {
			t.Errorf("the joined reads are %+v, want p-1 at the fleet's three reads and its "+
				"newer context, and p-2 kept", got.Reads)
		}
		if got.ReadsElided != 4 {
			t.Errorf("the join elides %d reads, want the fleet's 4 — it held six entries",
				got.ReadsElided)
		}
	})

	t.Run("a seat whose day the derivation holds more of", func(t *testing.T) {
		t.Parallel()
		derived := record(usage.KindSeat)
		derived.Turns = &usage.Turns{Count: 4, Failed: 2, Reviewed: 4, FirstPass: 0, SentBack: 5,
			Durations: histOf(time.Hour, time.Hour, time.Hour, time.Hour), LastEndedAt: at(12)}
		derived.Tokens = []usage.Tokens{cell("execute", 300, 3)}
		held := record(usage.KindSeat)
		held.Turns = &usage.Turns{Count: 2, Failed: 1, Reviewed: 2, FirstPass: 1, SentBack: 1,
			Durations: histOf(time.Hour, time.Hour), LastEndedAt: at(9)}
		held.Tokens = []usage.Tokens{cell("execute", 100, 1)}
		// p-1 read again since the fleet's record, in a later turn about
		// other work: the newer context is the derivation's.
		derived.Reads = []usage.Read{{Backend: "native", PageID: "p-1", Via: "get_page", Count: 2,
			LastAt: at(12), LastTurnID: "t-now", LastWorkKey: "ENG-2", LastQuery: "now"}}
		held.Reads = []usage.Read{{Backend: "native", PageID: "p-1", Via: "get_page", Count: 1,
			LastAt: at(9), LastTurnID: "t-then", LastWorkKey: "ENG-1", LastQuery: "then"}}
		got := usage.Join(derived, held)
		if !slices.Equal(got.Reads, derived.Reads) || got.ReadsElided != 0 {
			t.Errorf("the joined reads are %+v, want the derivation's %+v — its context is newer",
				got.Reads, derived.Reads)
		}
		if g := *got.Turns; g.Count != 4 || g.Failed != 2 || g.Reviewed != 4 ||
			g.FirstPass != 0 || g.SentBack != 5 || !g.LastEndedAt.Equal(at(12)) {
			t.Errorf("the joined turns are %+v, want the derivation's — it holds more of "+
				"every figure, and a turn the fleet counted first-pass was sent back since", g)
		}
		if !slices.Equal(got.Tokens, derived.Tokens) {
			t.Errorf("the joined spend is %+v, want the derivation's %+v", got.Tokens, derived.Tokens)
		}
	})

	t.Run("a seat whose day here names nobody", func(t *testing.T) {
		t.Parallel()
		// A seat the chart no longer holds, whose day on this node is
		// spend alone: no row of it states a handle, so the derivation
		// names nobody.
		derived := record(usage.KindSeat)
		derived.Turns = &usage.Turns{}
		derived.Tokens = []usage.Tokens{cell("execute", 300, 3)}
		held := record(usage.KindSeat)
		held.Handle, held.Role = "alice", "Dev"
		held.Turns = &usage.Turns{}
		held.Tokens = []usage.Tokens{cell("execute", 100, 1)}
		got := usage.Join(derived, held)
		if got.Handle != "alice" || got.Role != "Dev" {
			t.Errorf("the join names the seat %q/%q, want the fleet's alice/Dev — the "+
				"derivation states no name, and a handle never changes", got.Handle, got.Role)
		}
		if !slices.Equal(got.Tokens, derived.Tokens) {
			t.Errorf("the joined spend is %+v, want the derivation's %+v", got.Tokens, derived.Tokens)
		}
	})

	t.Run("a person whose day here states no role", func(t *testing.T) {
		t.Parallel()
		derived := record(usage.KindPerson)
		derived.Tokens = []usage.Tokens{cell("answer", 300, 1)}
		held := record(usage.KindPerson)
		held.Role = "CEO"
		held.Tokens = []usage.Tokens{cell("answer", 100, 1)}
		got := usage.Join(derived, held)
		if got.Role != "CEO" || got.Handle != "" {
			t.Errorf("the joined person-day names %q/%q, want the fleet's role and no handle",
				got.Handle, got.Role)
		}
		if _, err := got.Encode(); err != nil {
			t.Errorf("the joined person-day does not encode: %v", err)
		}
	})

	t.Run("a person", func(t *testing.T) {
		t.Parallel()
		derived := record(usage.KindPerson)
		derived.Role = "Founder"
		derived.Tokens = []usage.Tokens{cell("answer", 300, 1)}
		held := record(usage.KindPerson)
		held.Role = "CEO"
		held.Tokens = []usage.Tokens{cell("answer", 700, 2)}
		got := usage.Join(derived, held)
		if got.Role != "Founder" || !slices.Equal(got.Tokens, []usage.Tokens{cell("answer", 700, 2)}) {
			t.Errorf("the joined person-day is %q %+v, want the derivation's role and the "+
				"fleet's larger spend", got.Role, got.Tokens)
		}
	})

	t.Run("a schedule", func(t *testing.T) {
		t.Parallel()
		derived := record(usage.KindSchedule)
		derived.Fires = []usage.Fire{{At: at(8), Target: "dev", Outcome: "fired", TraceID: "tr-8"}}
		held := record(usage.KindSchedule)
		held.Fires = []usage.Fire{
			{At: at(9), Target: "dev", Outcome: "fired", TraceID: "tr-9"},
			{At: at(8), Target: "dev", Outcome: "fired", TraceID: "tr-8", TurnID: "t-gone"},
		}
		got := usage.Join(derived, held)
		if len(got.Fires) != 2 || !got.Fires[0].At.Equal(at(8)) || got.Fires[0].TurnID != "" ||
			!got.Fires[1].At.Equal(at(9)) {
			t.Errorf("the joined fires are %+v, want the 08:00 fire under the derivation's "+
				"label — a not-kept settle can clear it — and the 09:00 fire the fleet holds", got.Fires)
		}
	})
}

// THE JOIN OF A DAY WITH AN EARLIER DERIVATION OF IT IS THE DAY, byte for byte.
//
// Every publish takes the join of its derivation and the fleet's record, so on
// the ordinary day — nothing lost, the fleet's record an earlier derivation of
// the same rows — the join must change nothing at all, or every node would
// publish a day nobody derived. The earlier derivation goes through the real
// publish, apply and read-back ([usage.Applied]), and the day grows in every way
// a day grows between the two: more spend, a tallied turn, an untallied turn
// whose parked segment arrives late (moving it UP a duration bin) along with a
// review that sends it back (taking it OUT of first-pass), reads that push
// entries the cap kept past it, an entry the cap kept read again with a newer
// turn and query, a fire, a fire's turn label, a person's spend, and a unit
// repository's readers the cap elided read again until they outrank readers
// it kept.
//
// Mutation: join first-pass by its own larger, or a histogram bin by bin, or
// count elided reads or readers off one side alone, or keep a read entry's
// older context over the derivation's newer one, and the joined day differs
// from the day.
func TestTheJoinOfADayWithAnEarlierDerivationOfItIsTheDay(t *testing.T) {
	t.Parallel()
	own := openStore(t)
	noon := time.Date(2026, 9, 23, 12, 0, 0, 0, santiago)
	ev := &events{t: t, db: own}
	fire := func(at time.Time, trace string) {
		t.Helper()
		if _, err := sqlledger.New(own.SQL()).Claim(t.Context(), schedule.Run{
			FireKey: schedule.FireKey{Scope: "role", ScopeID: "Dev", ScheduleName: "standup",
				FireLabel: at.Format("20060102T1504"), TargetHandle: "dev"},
			ScheduledAt: at, FiredAt: at, Outcome: schedule.OutcomeFired, TraceID: trace,
		}); err != nil {
			t.Fatalf("record a fire: %v", err)
		}
	}
	pages := func(prefix string, n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("%s-%03d", prefix, i)
		}
		return out
	}

	// THE EARLIER DERIVATION.
	ev.phase(noon.Add(-5*time.Hour), "seat-1", "t-a", "execute", "", 100, 1)
	ev.turn(noon.Add(-4*time.Hour), "seat-1", "dev", "t-a", false, time.Minute, "done")
	ev.phase(noon.Add(-3*time.Hour), "seat-1", "t-old", "review", "done", 5, 1)
	ev.add("turn_completed", noon.Add(-2*time.Hour), "seat-1", "Dev", map[string]any{
		"agent_id": "seat-1", "agent_handle": "dev", "role": "Dev", "turn_id": "t-old",
		"duration_ms": time.Minute.Milliseconds(),
	})
	ev.read(noon.Add(-5*time.Hour), "seat-1", "t-a", "search", "first", pages("p", usage.ReadsPerSeatDay+40)...)
	fire(noon.Add(-6*time.Hour), "trace-6")
	ev.personAsked(noon.Add(-time.Hour), "maya", "Founder", 400, 20)
	// A unit repository read by more readers than its day keeps: the last
	// three by key are elided.
	for i := range usage.ReadersPerRepoDay + 3 {
		ev.fetched(noon.Add(-4*time.Hour+time.Duration(i)*time.Millisecond),
			gitFetch("repo-1", fmt.Sprintf("pat:%04d", i), "maya", 10))
	}
	p, _ := newPublisher(t, "node-a", own, own, noon)
	if err := p.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}

	// THE DAY GROWS.
	ev.phase(noon.Add(-30*time.Minute), "seat-1", "t-b", "execute", "", 70, 1)
	ev.turn(noon.Add(-20*time.Minute), "seat-1", "dev", "t-b", true, 3*time.Minute, "self_iterate", "done")
	ev.add("turn_completed", noon.Add(-150*time.Minute), "seat-1", "Dev", map[string]any{
		"agent_id": "seat-1", "agent_handle": "dev", "role": "Dev", "turn_id": "t-old",
		"duration_ms": (2 * time.Hour).Milliseconds(), "suspended": true,
	})
	ev.phase(noon.Add(-170*time.Minute), "seat-1", "t-old", "review", "self_iterate", 5, 1)
	// Entries the cap elided read twice now, so they outrank entries it kept.
	ev.read(noon.Add(-10*time.Minute), "seat-1", "t-b", "search", "second",
		pages("p", usage.ReadsPerSeatDay+40)[usage.ReadsPerSeatDay:]...)
	// And an entry the cap kept, read again under the same (page, via) in a
	// later turn with another query: the same entry, newer context.
	ev.read(noon.Add(-5*time.Minute), "seat-1", "t-b", "search", "third", "p-000")
	fire(noon.Add(-15*time.Minute), "trace-11")
	if err := own.Events().Append(t.Context(), store.EventRecord{
		ID: "ev-trace", Type: "agent_phase_completed", Source: "Dev", Category: "agent",
		Time: noon.Add(-6 * time.Hour), Actor: "Dev", TraceID: "trace-6",
		Tags:    map[string]string{"agent_id": "seat-2", "agent_role": "Dev", "turn_id": "t-fired"},
		Payload: []byte(`{"agent_id":"seat-2","role":"Dev","turn_id":"t-fired","phase":"execute"}`),
	}); err != nil {
		t.Fatal(err)
	}
	ev.personAsked(noon.Add(-time.Minute), "maya", "Founder", 300, 20)
	// Readers the cap elided read again, so they outrank readers it kept.
	for i := usage.ReadersPerRepoDay; i < usage.ReadersPerRepoDay+3; i++ {
		ev.fetched(noon.Add(-8*time.Minute), gitFetch("repo-1", fmt.Sprintf("pat:%04d", i), "maya", 10))
	}

	window := period.At(period.Day, noon, santiago)
	derived, err := own.UsageForDay(t.Context(), store.UsageWindow{Start: window.Start, End: window.End,
		Accepted: "done", SentBack: "self_iterate"})
	if err != nil {
		t.Fatal(err)
	}
	records, err := p.Records(window.Label, derived)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[usage.Kind]bool{}
	for _, rec := range records {
		held, ok, err := usage.Applied(t.Context(), own.Replicated(), rec.Subject)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			continue
		}
		kinds[rec.Subject.Kind] = true
		want, err := rec.Encode()
		if err != nil {
			t.Fatal(err)
		}
		if string(want) == string(mustEncode(t, held)) {
			t.Errorf("the premise: %s did not grow between the two derivations", rec.Subject)
		}
		if got := mustEncode(t, usage.Join(rec, held)); string(got) != string(want) {
			t.Errorf("the join of %s with its earlier derivation is not the day:\n got %s\nwant %s",
				rec.Subject, got, want)
		}
	}
	for _, kind := range usage.Kinds {
		if !kinds[kind] {
			t.Errorf("the premise: no %s object was derived twice", kind)
		}
	}
}

// mustEncode is a record's bytes.
func mustEncode(t *testing.T, r usage.Record) []byte {
	t.Helper()
	b, err := r.Encode()
	if err != nil {
		t.Fatalf("encode %s: %v", r.Subject, err)
	}
	return b
}

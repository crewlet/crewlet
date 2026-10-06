package auxspend_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/usage"
)

// recorder is a publisher that keeps what it was handed, refusing while told
// to — the broker's two answers.
type recorder struct {
	mu      sync.Mutex
	got     []*events.Event
	refuse  bool
	refused int
}

func (r *recorder) Publish(_ context.Context, subject string, ev *events.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if subject != "crewlet.events."+ev.Type {
		return errors.New("published on " + subject)
	}
	if r.refuse {
		r.refused++
		return errors.New("broker unavailable")
	}
	r.got = append(r.got, ev)
	return nil
}

func (r *recorder) records(t *testing.T) []types.AuxiliarySpend {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]types.AuxiliarySpend, 0, len(r.got))
	for _, ev := range r.got {
		rec, ok := events.DataAs[*types.AuxiliarySpend](ev)
		if !ok {
			t.Fatalf("published a %s, want auxiliary_spend", ev.Type)
		}
		out = append(out, *rec)
	}
	return out
}

var t0 = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

var dev = auxspend.Seat{AgentID: "a-1", Handle: "dev", Role: "Dev"}

func call(purpose types.AuxPurpose, turn string, at time.Duration, in, out int) auxspend.Call {
	return auxspend.Call{
		Seat: dev,
		Use: auxspend.Use{Stage: types.AuxStageTurn, Purpose: purpose,
			TurnID: turn, WorkKey: "wk-" + turn},
		Model: "haiku", ProviderKey: "cheap", Day: "2026-09-23",
		Started: t0.Add(at), Ended: t0.Add(at + 2*time.Second),
		Spent: auxspend.Spent{Input: in, Output: out, CacheRead: in / 2},
	}
}

// ONE RECORD PER KEY PER FLUSH, carrying every call of it.
//
// A compaction of seventy rewrites as seventy rows would push a turn's own
// records out of every bounded read of it; as one row it is what it cost. The
// record is stamped with its LAST call's instant — the day the counters
// charged it in — and spans its first call's start to its last call's end,
// with the calls' own time summed beside.
func TestALedgerCoalescesOneKeysCallsIntoOneRecord(t *testing.T) {
	t.Parallel()
	pub := &recorder{}
	l := auxspend.NewLedger(pub)
	l.Add(call(types.AuxCondense("produced"), "t-1", 0, 1000, 100))
	failed := call(types.AuxCondense("produced"), "t-1", 10*time.Second, 500, 0)
	failed.Failed = true
	l.Add(failed)
	l.Add(call(types.AuxMemoryFilter, "t-1", 20*time.Second, 300, 20))
	l.Flush(t.Context())

	got := pub.records(t)
	if len(got) != 2 {
		t.Fatalf("records = %+v, want one per purpose", got)
	}
	byPurpose := map[types.AuxPurpose]types.AuxiliarySpend{}
	for _, r := range got {
		byPurpose[r.Purpose] = r
	}
	r := byPurpose[types.AuxCondense("produced")]
	if r.Calls != 2 || r.FailedCalls != 1 || r.InputTokens != 1500 || r.OutputTokens != 100 ||
		r.TotalTokens != 1600 || r.CacheReadTokens != 750 {
		t.Errorf("the compaction's record = %+v, want two calls (one failed) and every token", r)
	}
	if !r.StartedAt.Equal(t0) || !r.EndedAt.Equal(t0.Add(12*time.Second)) || r.DurationMS != 4000 {
		t.Errorf("span = %s..%s over %dms, want the first call's start, the last call's "+
			"end and four seconds of calls", r.StartedAt, r.EndedAt, r.DurationMS)
	}
	if r.Stage != types.AuxStageTurn || r.TurnID != "t-1" || r.WorkKey != "wk-t-1" ||
		r.Agent != "a-1" || r.AgentHandle != "dev" || r.RoleName != "Dev" ||
		r.Model != "haiku" || r.ProviderKey != "cheap" || r.Day != "2026-09-23" {
		t.Errorf("attribution = %+v", r)
	}
	for _, ev := range pub.got {
		rec, _ := events.DataAs[*types.AuxiliarySpend](ev)
		if !ev.Timestamp.Equal(rec.EndedAt) {
			t.Errorf("an envelope is stamped %s, want its last call's %s", ev.Timestamp, rec.EndedAt)
		}
	}
	// AND A FLUSH EMPTIES THE LEDGER: the next one publishes nothing again.
	l.Flush(t.Context())
	if len(pub.records(t)) != 2 {
		t.Error("a second flush published the same calls again")
	}
}

// A BUCKET CLOSES AT MIDNIGHT. The counters charged each call in the day it
// returned in, so two calls either side of the company's midnight are two
// records, each in its own day — a figure for one day never holds the other's.
func TestALedgersBucketNeverStraddlesTwoDays(t *testing.T) {
	t.Parallel()
	pub := &recorder{}
	l := auxspend.NewLedger(pub)
	late := call(types.AuxPersistDecider, "t-1", 0, 100, 10)
	late.Day = "2026-09-23"
	early := call(types.AuxPersistDecider, "t-1", time.Minute, 200, 20)
	early.Day = "2026-09-24"
	l.Add(late)
	l.Add(early)
	l.Flush(t.Context())
	got := pub.records(t)
	if len(got) != 2 || got[0].Day == got[1].Day {
		t.Fatalf("records = %+v, want one in each day", got)
	}
}

// A TURN'S BUCKETS FLUSH AS IT ENDS, and nobody else's: its page reads what its
// context and its rewrites cost when it reads the turn, while every other
// bucket waits for the timer it was going to be flushed by.
func TestATurnsEndFlushesThatTurnsBucketsAlone(t *testing.T) {
	t.Parallel()
	pub := &recorder{}
	l := auxspend.NewLedger(pub)
	l.Add(call(types.AuxMemoryFilter, "t-1", 0, 100, 10))
	l.Add(call(types.AuxMemoryFilter, "t-2", 0, 100, 10))
	l.FlushTurn(t.Context(), "t-1")
	if got := pub.records(t); len(got) != 1 || got[0].TurnID != "t-1" {
		t.Fatalf("records = %+v, want t-1's alone", got)
	}
	l.Flush(t.Context())
	if got := pub.records(t); len(got) != 2 || got[1].TurnID != "t-2" {
		t.Fatalf("records = %+v, want t-2's on the next flush", got)
	}
}

// A REFUSED PUBLISH IS PUBLISHED AGAIN, AS THE SAME EVENT.
//
// The record is the only account the rollups will ever have of those calls,
// so a broker that refused it is asked again by the next flush — with the id
// and the instant it was sealed with, so a publish reported failed that had in
// fact landed is collapsed by the store's (time, id) key and the live window's
// id index rather than counted twice.
func TestARefusedRecordIsPublishedAgainAsTheSameEvent(t *testing.T) {
	t.Parallel()
	pub := &recorder{refuse: true}
	l := auxspend.NewLedger(pub)
	l.Add(call(types.AuxKnowledgeQuery, "t-1", 0, 100, 10))
	l.Add(call(types.AuxMemoryFilter, "t-2", 0, 70, 7))
	l.Flush(t.Context())
	// ONE REFUSAL ENDS THE FLUSH: a broker that refused one record is down
	// for the next, and a backlog asked record by record would spend every
	// flush of an outage on refusals.
	if pub.refused != 1 || len(pub.got) != 0 {
		t.Fatalf("refused %d, published %d — want one attempt that was refused",
			pub.refused, len(pub.got))
	}
	sealedID := map[string]bool{}
	pub.mu.Lock()
	pub.refuse = false
	pub.mu.Unlock()
	l.Add(call(types.AuxKnowledgeQuery, "t-1", time.Minute, 50, 5))
	l.Flush(t.Context())
	got := pub.records(t)
	if len(got) != 3 {
		t.Fatalf("records = %+v, want the two refused and the new one", got)
	}
	var knowledge []int
	for _, r := range got {
		if r.Purpose == types.AuxKnowledgeQuery {
			knowledge = append(knowledge, r.InputTokens)
		}
	}
	if len(knowledge) != 2 || knowledge[0] != 100 || knowledge[1] != 50 {
		t.Errorf("the knowledge records = %v, want the refused 100 first and unchanged — a "+
			"later call is a new record, never added to one already sealed", knowledge)
	}
	for _, ev := range pub.got {
		if sealedID[ev.ID.String()] {
			t.Errorf("record %s was published twice", ev.ID)
		}
		sealedID[ev.ID.String()] = true
	}

	// AND THE RETRY IS THE SAME EVENT: refused once more and then let
	// through, a record keeps its id and its instant.
	pub.mu.Lock()
	pub.refuse = true
	pub.mu.Unlock()
	l.Add(call(types.AuxEpisodeSummary, "t-3", 0, 10, 1))
	l.Flush(t.Context())
	l.Flush(t.Context())
	pub.mu.Lock()
	pub.refuse = false
	pub.mu.Unlock()
	l.Flush(t.Context())
	l.Flush(t.Context())
	last := pub.got[len(pub.got)-1]
	if rec, _ := events.DataAs[*types.AuxiliarySpend](last); rec.Purpose != types.AuxEpisodeSummary ||
		!last.Timestamp.Equal(rec.EndedAt) || len(pub.got) != 4 {
		t.Errorf("after two refusals the record was published %d times in all, last %+v",
			len(pub.got)-3, rec)
	}
}

// THE BACKLOG IS BOUNDED, and what leaves it is the OLDEST: a broker that has
// refused for an hour costs the rollups that hour's oldest records, never an
// unbounded heap.
func TestARefusedBacklogIsBoundedOldestFirst(t *testing.T) {
	t.Parallel()
	pub := &recorder{refuse: true}
	l := auxspend.NewLedger(pub)
	for i := range auxspend.MaxPending + 3 {
		c := call(types.AuxMemoryFilter, "t", time.Duration(i)*time.Second, i, 0)
		c.Use.TurnID = "t-" + time.Duration(i).String()
		l.Add(c)
		l.Flush(t.Context())
	}
	pub.mu.Lock()
	pub.refuse = false
	pub.mu.Unlock()
	l.Flush(t.Context())
	got := pub.records(t)
	if len(got) != auxspend.MaxPending {
		t.Fatalf("published %d after the outage, want the %d the backlog holds",
			len(got), auxspend.MaxPending)
	}
	for _, r := range got {
		if r.InputTokens < 3 {
			t.Fatalf("record with %d input tokens survived: the oldest three should "+
				"have been dropped", r.InputTokens)
		}
	}
}

// THE STOP FLUSHES WHAT THE TIMER HAS NOT: the last calls a draining node's
// passes made are published before the stream it publishes on closes.
func TestAStopFlushesWhatTheTimerHasNot(t *testing.T) {
	t.Parallel()
	pub := &recorder{}
	l := auxspend.NewLedger(pub)
	l.Run(t.Context())
	l.Add(call(types.AuxEpisodeCompaction, "", 0, 100, 10))
	l.Stop(t.Context())
	if got := pub.records(t); len(got) != 1 || got[0].Purpose != types.AuxEpisodeCompaction {
		t.Fatalf("records = %+v, want the call made before the stop", got)
	}
	// And a stopped ledger still records — a stop is not a disconnect.
	l.Add(call(types.AuxEpisodeCompaction, "", time.Minute, 100, 10))
	l.Stop(t.Context())
	if len(pub.records(t)) != 2 {
		t.Error("a second stop did not flush what arrived after the first")
	}
}

// A PERSON'S SPEND IS FILED UNDER THE PERSON, never as an agent seat: a person
// is no agent role, and the live projection keys a seat's state on `role`.
func TestAPersonsSpendNamesThePerson(t *testing.T) {
	t.Parallel()
	pub := &recorder{}
	l := auxspend.NewLedger(pub)
	c := call(types.AuxAnswerKnowledge, "", 0, 100, 10)
	c.Use.Stage = types.AuxStageOperator
	c.Seat = auxspend.Seat{Person: "maya", PersonRole: "Founder"}
	l.Add(c)
	l.Flush(t.Context())
	got := pub.records(t)
	if len(got) != 1 || got[0].ActorSeat != "maya" || got[0].ActorRole != "Founder" ||
		got[0].RoleName != "" || got[0].Agent != "" {
		t.Fatalf("records = %+v, want maya's, under no agent role", got)
	}
	if pub.got[0].Source != "maya" {
		t.Errorf("source = %q, want the person", pub.got[0].Source)
	}
}

// THE LEDGER FLUSHES ON THE USAGE DOMAIN'S CADENCE, the one the live meters are
// pushed on: a different one would leave the auxiliary spend in a figure
// trailing the meter above it by more than one of the meter's refreshes.
func TestTheLedgerFlushesOnTheUsageCadence(t *testing.T) {
	t.Parallel()
	if auxspend.FlushInterval != usage.FlushInterval {
		t.Fatalf("auxspend.FlushInterval = %s, usage.FlushInterval = %s",
			auxspend.FlushInterval, usage.FlushInterval)
	}
}

// A NIL LEDGER RECORDS NOTHING AND PANICS NOWHERE: a node with no queue
// publishes no event of any kind, and the seam calls it unconditionally.
func TestANilLedgerIsInert(t *testing.T) {
	t.Parallel()
	var l *auxspend.Ledger
	l.Add(call(types.AuxMemoryFilter, "t-1", 0, 1, 1))
	l.Flush(t.Context())
	l.FlushTurn(t.Context(), "t-1")
	l.Stop(t.Context())
	auxspend.NewLedger(nil).Add(call(types.AuxMemoryFilter, "t-1", 0, 1, 1))
}

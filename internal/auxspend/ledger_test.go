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

// THE STOP FLUSHES WHAT THE TIMER HAS NOT: the last calls a draining node's
// passes made are published before the stream it publishes on closes. The
// timer, the refusals and the backlog are internal/coalesce's, and its suite
// holds them; this holds that the ledger runs on them.
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
// is no agent seat and has no agent id, so their spend is named by
// `actor_seat`/`actor_role`, never by `agent_id`/`role` — the second of which
// fills the store's `agent_role` column, read as an agent seat's name.
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

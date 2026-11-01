package store_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/store"
)

// usageDayOf is a usage window over [start, end), naming the review decisions
// the turn loop uses (internal/agent/phase) — the ones a turn whose ending
// record states no tally is counted in.
func usageDayOf(start, end time.Time) store.UsageWindow {
	return store.UsageWindow{Start: start, End: end, Accepted: "done", SentBack: "self_iterate"}
}

// usageRecord is one of a node's records the way the observe writer builds it:
// the promoted columns from the tags and the spend from the payload.
func usageRecord(t *testing.T, id, typ string, at time.Time, agent string,
	payload any) store.EventRecord {

	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode %s: %v", id, err)
	}
	tags := map[string]string{"agent_id": agent, "agent_role": "Dev"}
	if m, ok := payload.(map[string]any); ok {
		if turn, ok := m["turn_id"].(string); ok {
			tags["turn_id"] = turn
		}
	}
	if end, ok := payload.(types.TurnCompleted); ok {
		tags["turn_id"] = end.TurnID
	}
	return store.EventRecord{ID: id, Type: typ, Source: "Dev", Category: "agent",
		Time: at, Actor: "Dev", Tags: tags, Payload: body}
}

func phaseRecord(t *testing.T, id string, at time.Time, agent, turn, phase, decision string,
	input int) store.EventRecord {

	t.Helper()
	return usageRecord(t, id, "agent_phase_completed", at, agent, map[string]any{
		"agent_id": agent, "role": "Dev", "turn_id": turn, "phase": phase,
		"model": "m-1", "provider_key": "primary", "decision": decision,
		"input_tokens": input, "output_tokens": 1, "total_tokens": input + 1,
	})
}

// endedTurn is the record that ends a turn, stating the whole of it.
func endedTurn(t *testing.T, id string, at time.Time, agent, turn string,
	whole types.TurnTally) store.EventRecord {

	t.Helper()
	return usageRecord(t, id, "turn_completed", at, agent, types.TurnCompleted{
		Agent: agent, AgentHandle: "dev", RoleName: "Dev", TurnID: turn,
		DurationMS: 1, Turn: &whole,
	})
}

func readRecord(t *testing.T, id string, at time.Time, agent, page string) store.EventRecord {
	t.Helper()
	return usageRecord(t, id, "knowledge_read", at, agent, map[string]any{
		"agent_id": agent, "agent_handle": "dev", "role": "Dev", "via": "get_page",
		"backend": "native", "pages": []map[string]any{{"id": page, "rank": 1}},
	})
}

func personRecord(t *testing.T, id string, at time.Time, seat string, total int) store.EventRecord {
	t.Helper()
	rec := types.AuxiliarySpend{ActorSeat: seat, ActorRole: "Founder",
		Stage: types.AuxStageOperator, Purpose: types.AuxAnswerKnowledge, Model: "haiku",
		ProviderKey: "cheap", Calls: 1, InputTokens: total, TotalTokens: total}
	payload, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	return store.EventRecord{ID: id, Type: rec.EventType(), Actor: rec.Actor(), Time: at,
		Category: "system", Tags: store.ExtractTags(payload), Payload: payload}
}

// fetchRecord is a record of reads of a unit repository, as the node's reads
// ledger publishes it ([types.CodeFetched], internal/code/readaudit).
func fetchRecord(t *testing.T, id string, at time.Time, read types.CodeFetched) store.EventRecord {
	t.Helper()
	payload, err := json.Marshal(read)
	if err != nil {
		t.Fatal(err)
	}
	return store.EventRecord{ID: id, Type: read.EventType(), Actor: read.Actor(), Time: at,
		Category: "system", Tags: store.ExtractTags(payload), Payload: payload}
}

// usageTally is what a derived day says, reduced to what these cases count.
type usageTally struct {
	spend, turns, failed, reviewed, firstPass, sentBack, reads, person, repo int64
	duration                                                                 time.Duration
}

func tallyOf(t *testing.T, db *store.DB, w store.UsageWindow) (usageTally, store.UsageMark) {
	t.Helper()
	day, err := db.UsageForDay(t.Context(), w)
	if err != nil {
		t.Fatalf("derive the day: %v", err)
	}
	var out usageTally
	for _, s := range day.Seats {
		for _, c := range s.Tokens {
			out.spend += c.Input
		}
		out.turns += s.Turns.Count
		out.failed += s.Turns.Failed
		out.reviewed += s.Turns.Reviewed
		out.firstPass += s.Turns.FirstPass
		out.sentBack += s.Turns.SentBack
		for _, d := range s.Turns.Durations {
			out.duration += d
		}
		for _, r := range s.Reads {
			out.reads += r.Count
		}
	}
	for _, p := range day.People {
		for _, c := range p.Tokens {
			out.person += c.Total
		}
	}
	for _, r := range day.RepoReads {
		out.repo += r.Reads
	}
	mark, err := db.UsageMark(t.Context(), w)
	if err != nil {
		t.Fatalf("fingerprint the day: %v", err)
	}
	return out, mark
}

// A NODE'S DAY IS THE ROWS IT KEEPS — never a custody batch it has written and
// not settled, which may be another data node's to keep.
//
// Until a batch settles, two logs can hold its rows, and every reader of the
// replicated history sums the nodes' days: counted as held, a stateless node's
// spend was billed twice, and once the loser's copy was deleted the double
// count stayed in the history of every day the publisher no longer looked at.
// So the batch counts in neither the day nor its fingerprint until it settles;
// kept, it counts, and the fingerprint moves so today's publisher derives it
// again; not kept, its rows go and nothing ever counted them.
//
// Every statement a day is derived from is exercised — the spend, the ended
// turns, the reads, a person's spend, a unit repository's reads — so leaving
// the predicate off any one of them fails here.
func TestAUsageDayCountsOnlyTheRowsThisNodeKeeps(t *testing.T) {
	t.Parallel()
	day := time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)
	w := usageDayOf(day, day.Add(24*time.Hour))
	at := func(h int) time.Time { return day.Add(time.Duration(h) * time.Hour) }

	own := func(t *testing.T, log *store.EventLog) {
		t.Helper()
		for _, rec := range []store.EventRecord{
			phaseRecord(t, "own-exec", at(1), "seat-1", "t-own", "execute", "", 100),
			phaseRecord(t, "own-review", at(2), "seat-1", "t-own", "review", "done", 10),
			readRecord(t, "own-read", at(2), "seat-1", "p-own"),
			personRecord(t, "own-person", at(2), "maya", 7),
			fetchRecord(t, "own-fetch", at(2), unitFetch("pat:maya", "maya")),
			endedTurn(t, "own-end", at(3), "seat-1", "t-own",
				types.TurnTally{DurationMS: 60_000, Reviews: 1}),
		} {
			if err := log.Append(t.Context(), rec); err != nil {
				t.Fatalf("append %s: %v", rec.ID, err)
			}
		}
	}
	// THE BATCH: a turn of its own, end to end — reviewed twice, sent back
	// once — plus a review row of the KEPT turn above, which counts as spend
	// and nowhere else: that turn's figures are its own end's.
	batch := func(t *testing.T) store.CustodyBatch {
		return store.CustodyBatch{ID: "batch-1", Origin: "stateless-1", Records: []store.EventRecord{
			phaseRecord(t, "b-exec", at(4), "seat-2", "t-b", "execute", "", 1000),
			phaseRecord(t, "b-review-own", at(2), "seat-1", "t-own", "review", "self_iterate", 1),
			readRecord(t, "b-read", at(5), "seat-2", "p-b"),
			personRecord(t, "b-person", at(5), "maya", 70),
			fetchRecord(t, "b-fetch", at(5), unitFetch("token:ci", "")),
			endedTurn(t, "b-end", at(6), "seat-2", "t-b",
				types.TurnTally{DurationMS: 120_000, Failed: true, Reviews: 2, SentBack: 1, Unaccepted: 1}),
		}}
	}
	ownOnly := usageTally{spend: 100 + 10, turns: 1, reviewed: 1, firstPass: 1, reads: 1,
		person: 7, repo: 1, duration: time.Minute}
	withBatch := usageTally{spend: 110 + 1000 + 1, turns: 2, failed: 1, reviewed: 2,
		firstPass: 1, sentBack: 1, reads: 2, person: 77, repo: 2, duration: 3 * time.Minute}

	for _, kept := range []bool{true, false} {
		t.Run(fmt.Sprintf("settled kept=%v", kept), func(t *testing.T) {
			t.Parallel()
			db := open(t)
			log := db.Events()
			own(t, log)
			before, beforeMark := tallyOf(t, db, w)
			if before != ownOnly {
				t.Fatalf("the control: this node's own day is %+v, want %+v", before, ownOnly)
			}
			if err := log.WriteCustody(t.Context(), batch(t), at(7)); err != nil {
				t.Fatalf("write the batch: %v", err)
			}
			unsettled, unsettledMark := tallyOf(t, db, w)
			if unsettled != ownOnly {
				t.Errorf("with a batch written and not settled the day is %+v, want "+
					"this node's own %+v — the batch may be another node's to keep, "+
					"and every reader sums both nodes' days", unsettled, ownOnly)
			}
			if unsettledMark != beforeMark {
				t.Errorf("the fingerprint moved to %+v when a batch nobody keeps yet "+
					"was written (it was %+v)", unsettledMark, beforeMark)
			}

			if err := log.SettleCustody(t.Context(), "batch-1", kept); err != nil {
				t.Fatalf("settle: %v", err)
			}
			settled, settledMark := tallyOf(t, db, w)
			want := ownOnly
			if kept {
				want = withBatch
			}
			if settled != want {
				t.Errorf("settled kept=%v, the day is %+v, want %+v", kept, settled, want)
			}
			if moved := settledMark != beforeMark; moved != kept {
				t.Errorf("settled kept=%v, the fingerprint moved=%v: a kept batch "+
					"changes the day and must re-derive it; one that is not kept "+
					"changes nothing", kept, moved)
			}
		})
	}
}

// A TURN'S FIGURES ARE READ OFF THE RECORD THAT ENDS IT, and off nothing else.
//
// One turn's records are routinely on two nodes: a stateless node's events
// reach the data nodes in custody batches a second apart, each kept by whoever
// won its claim, and a turn resumed on another node leaves the segment before
// its park behind. Summed from the records each node held, the node with the
// end counted the turn's reviews, send-backs and duration short, and the node
// with the rest counted them nowhere. So here the end's node holds none of the
// turn's reviews and still states them all; the other node, holding the
// reviews and no end, states no turn — and a review of the turn's that turns
// up later, on its own day, moves that day's spend and leaves the turn alone.
func TestATurnsFiguresAreReadOffTheRecordThatEndsIt(t *testing.T) {
	t.Parallel()
	day := time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)
	w := usageDayOf(day, day.Add(24*time.Hour))
	before := usageDayOf(day.Add(-24*time.Hour), day)

	ends, reviews := open(t), open(t)
	if err := ends.Events().Append(t.Context(), endedTurn(t, "end", day.Add(5*time.Minute),
		"seat-1", "t-1", types.TurnTally{DurationMS: 90_000, Reviews: 2, SentBack: 1,
			Unaccepted: 1})); err != nil {
		t.Fatal(err)
	}
	for _, rec := range []store.EventRecord{
		phaseRecord(t, "r1", day.Add(-2*time.Minute), "seat-1", "t-1", "review", "self_iterate", 5),
		phaseRecord(t, "r2", day.Add(2*time.Minute), "seat-1", "t-1", "review", "done", 5),
	} {
		if err := reviews.Events().Append(t.Context(), rec); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := tallyOf(t, ends, w)
	if want := (usageTally{turns: 1, reviewed: 1, sentBack: 1, duration: 90 * time.Second}); got != want {
		t.Errorf("the node holding the turn's end and none of its reviews states %+v, "+
			"want %+v — the end states the whole turn", got, want)
	}
	if got, _ := tallyOf(t, reviews, w); got.turns != 0 || got.reviewed != 0 || got.sentBack != 0 {
		t.Errorf("the node holding the turn's reviews and not its end states %+v — a "+
			"turn is counted where it ended, once", got)
	}

	// A REVIEW OF THE TURN THAT LANDS LATER, ON THE DAY BEFORE: that day's
	// spend, and the turn's day unmoved.
	mark, _ := ends.UsageMark(t.Context(), w)
	if err := ends.Events().Append(t.Context(), phaseRecord(t, "late", day.Add(-time.Minute),
		"seat-1", "t-1", "review", "self_iterate", 7)); err != nil {
		t.Fatal(err)
	}
	if got, after := tallyOf(t, ends, w); got.sentBack != 1 || after != mark {
		t.Errorf("a review of the turn on the day before moved the day the turn ended "+
			"to %+v (fingerprint %+v from %+v) — a row changes only the day it falls on",
			got, after, mark)
	}
	if got, _ := tallyOf(t, ends, before); got.spend != 7 || got.turns != 0 {
		t.Errorf("the day before holds %+v, want the late review's 7 tokens of spend "+
			"and no turn", got)
	}
}

// A TURN WHOSE END STATES NO TALLY IS READ OFF ITS RECORDS, NOT AS ZEROS.
//
// Every `turn_completed` a build before [types.TurnTally] wrote — every one in
// a data node's log at the upgrade, every one a peer still on that build
// publishes during a rolling upgrade, and the end of a turn such a build parked
// — carries no `turn`. Read as a turn that measured nothing, the first
// derivation after the upgrade published every such turn as zero-length,
// unreviewed and unfailed over the history the old build had published, for
// the 181 days the history keeps it. So such a turn is folded from its records
// by its id, as that build folded every turn: every segment's duration (the
// parked one's included), whether its final summary failed, and what each of
// its reviews decided — a review on the day before included. A turn whose end
// does state a tally on the same day is still read off the tally alone.
//
// Mutation: read an untallied turn's figures off its (absent) tally, or drop
// any one fold of [DB.usageLegacyTurns], and this names the figure.
func TestATurnWhoseEndStatesNoTallyIsReadOffItsRecords(t *testing.T) {
	t.Parallel()
	day := time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)
	w := usageDayOf(day, day.Add(24*time.Hour))
	db := open(t)
	parked := usageRecord(t, "parked", "turn_completed", day.Add(-time.Hour), "seat-1",
		types.TurnCompleted{Agent: "seat-1", AgentHandle: "dev", RoleName: "Dev",
			TurnID: "t-old", DurationMS: 18_000, Suspended: true})
	end := usageRecord(t, "end", "turn_completed", day.Add(time.Hour), "seat-1",
		types.TurnCompleted{Agent: "seat-1", AgentHandle: "dev", RoleName: "Dev",
			TurnID: "t-old", DurationMS: 42_000})
	summary := usageRecord(t, "summary", "agent_turn_completed", day.Add(time.Hour), "seat-1",
		map[string]any{"agent_id": "seat-1", "role": "Dev", "turn_id": "t-old", "failed": true})
	for _, rec := range []store.EventRecord{
		parked,
		phaseRecord(t, "r1", day.Add(-30*time.Minute), "seat-1", "t-old", "review", "self_iterate", 0),
		phaseRecord(t, "r2", day.Add(30*time.Minute), "seat-1", "t-old", "review", "done", 0),
		end, summary,
		// A TALLIED TURN BESIDE IT, whose own records say something else
		// entirely: its end alone states it.
		phaseRecord(t, "r3", day.Add(2*time.Hour), "seat-2", "t-new", "review", "self_iterate", 0),
		endedTurn(t, "new-end", day.Add(3*time.Hour), "seat-2", "t-new",
			types.TurnTally{DurationMS: 5_000, Reviews: 1}),
	} {
		if err := db.Events().Append(t.Context(), rec); err != nil {
			t.Fatalf("append %s: %v", rec.ID, err)
		}
	}
	derived, err := db.UsageForDay(t.Context(), w)
	if err != nil {
		t.Fatal(err)
	}
	bySeat := map[string]store.UsageTurns{}
	for _, s := range derived.Seats {
		bySeat[s.AgentID] = s.Turns
	}
	old := bySeat["seat-1"]
	if old.Count != 1 || old.Failed != 1 || old.Reviewed != 1 || old.FirstPass != 0 ||
		old.SentBack != 1 || len(old.Durations) != 1 || old.Durations[0] != time.Minute {
		t.Errorf("the turn whose end states no tally reads count=%d failed=%d reviewed=%d "+
			"first_pass=%d sent_back=%d durations=%v, want 1 1 1 0 1 [1m0s] — its two "+
			"segments' 18 s and 42 s, its failed summary, and its two reviews, one sent back",
			old.Count, old.Failed, old.Reviewed, old.FirstPass, old.SentBack, old.Durations)
	}
	tallied := bySeat["seat-2"]
	if tallied.Count != 1 || tallied.Reviewed != 1 || tallied.FirstPass != 1 ||
		tallied.SentBack != 0 || len(tallied.Durations) != 1 || tallied.Durations[0] != 5*time.Second {
		t.Errorf("the tallied turn reads %+v, want its end's own tally — one accepted "+
			"review and 5 s — whatever its other records say", tallied)
	}
}

// ONE ENDING RECORDED TWICE, ONCE WITH A TALLY AND ONCE WITHOUT, IS READ OFF
// THE TALLY.
//
// A resume re-run after a failure publishes its end again, and across an
// upgrade one copy can come from each build. The copy without a tally reads
// every figure as zero, so the tally is the one read — never a fold of the
// records beside it.
//
// Mutation: read a turn as untallied unless EVERY copy states a tally, and this
// goes red.
func TestAnEndingRecordedTwiceIsReadOffTheCopyWithATally(t *testing.T) {
	t.Parallel()
	day := time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)
	w := usageDayOf(day, day.Add(24*time.Hour))
	db := open(t)
	for _, rec := range []store.EventRecord{
		phaseRecord(t, "r1", day.Add(time.Minute), "seat-1", "t-1", "review", "self_iterate", 0),
		usageRecord(t, "bare", "turn_completed", day.Add(time.Hour), "seat-1",
			types.TurnCompleted{Agent: "seat-1", AgentHandle: "dev", RoleName: "Dev",
				TurnID: "t-1", DurationMS: 1}),
		endedTurn(t, "tallied", day.Add(time.Hour+time.Second), "seat-1", "t-1",
			types.TurnTally{DurationMS: 90_000, Reviews: 2, SentBack: 1, Unaccepted: 1}),
	} {
		if err := db.Events().Append(t.Context(), rec); err != nil {
			t.Fatalf("append %s: %v", rec.ID, err)
		}
	}
	got, _ := tallyOf(t, db, w)
	if want := (usageTally{turns: 1, reviewed: 1, sentBack: 1, duration: 90 * time.Second}); got != want {
		t.Errorf("an ending recorded with and without a tally reads %+v, want the tally's %+v",
			got, want)
	}
}

// A USAGE DAY NAMES THE REVIEW DECISIONS, or is refused.
//
// A turn whose end states no tally is counted from its reviews by decision,
// and a window naming none would read every review as unaccepted and none as a
// send-back — a value that looks like a measurement.
func TestAUsageDayWithoutTheReviewDecisionsIsRefused(t *testing.T) {
	t.Parallel()
	day := time.Now().UTC().Truncate(24 * time.Hour)
	for _, w := range []store.UsageWindow{
		{Start: day, End: day.Add(time.Hour)},
		{Start: day, End: day.Add(time.Hour), Accepted: "done"},
		{Start: day, End: day.Add(time.Hour), SentBack: "self_iterate"},
	} {
		if _, err := open(t).UsageForDay(t.Context(), w); err == nil {
			t.Errorf("a usage day over %+v derived, want it refused for the decisions it does not name", w)
		}
	}
}

// A BATCH THIS NODE SETTLED IS NOT OPENED AGAIN BY ITS REDELIVERY.
//
// A group redelivers a batch whose acknowledgement was lost to whichever member
// asks next — this node, after it settled the batch. Written again, the batch
// was in flight again: kept, its counted rows left every usage count, and a
// tick in between published the day short over good history; not kept, its
// rows came back as a copy nobody would ask about. Either way the write now
// writes nothing, and the settle after it settles nothing.
func TestARedeliveredBatchThisNodeSettledIsNotOpenedAgain(t *testing.T) {
	t.Parallel()
	day := time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)
	w := usageDayOf(day, day.Add(24*time.Hour))
	for _, kept := range []bool{true, false} {
		t.Run(fmt.Sprintf("kept=%v", kept), func(t *testing.T) {
			t.Parallel()
			db := open(t)
			log := db.Events()
			b := store.CustodyBatch{ID: "batch", Origin: "stateless-1",
				Records: []store.EventRecord{phaseRecord(t, "r1", day.Add(time.Hour), "s", "t",
					"execute", "", 40)}}
			if err := log.WriteCustody(t.Context(), b, day); err != nil {
				t.Fatal(err)
			}
			if err := log.SettleCustody(t.Context(), "batch", kept); err != nil {
				t.Fatal(err)
			}
			settled, settledMark := tallyOf(t, db, w)
			if err := log.WriteCustody(t.Context(), b, day.Add(time.Minute)); err != nil {
				t.Fatalf("write the redelivered batch: %v", err)
			}
			again, againMark := tallyOf(t, db, w)
			if again != settled || againMark != settledMark {
				t.Errorf("a redelivery of a batch settled kept=%v moved the day from %+v to "+
					"%+v (fingerprint %+v to %+v)", kept, settled, again, settledMark, againMark)
			}
			if left := unsettled(t, log, time.Now().Add(time.Hour)); len(left) != 0 {
				t.Errorf("a redelivery of a settled batch put it back in flight: %v", left)
			}
		})
	}
}

// A KEPT SETTLE MARKS THE OLDEST DAY IT CHANGED, and nothing else does.
//
// The marks are what tell the publisher to derive a day older than the two it
// fingerprints: so the oldest is lowered by an older batch and never raised by
// a newer one, every settle that marks moves the newest seq — which is what
// the publisher clears against — a batch of rows no day is derived from marks
// nothing, a check's ending that names no seat among them, and a batch not
// kept marks nothing because none of its rows ever
// counted. The clear takes what it read and only that: a settle after the read
// survives it — and survives it ALONE, carrying its own day rather than the
// oldest one the cleared marks named, or a busy keeper's settles of today's
// rows kept a mark of last week alive and every tick derived the week again.
func TestAKeptSettleMarksTheOldestDayItChanged(t *testing.T) {
	t.Parallel()
	db := open(t)
	log := db.Events()
	// NOON UTC, so an hour either side of an instant is the same UTC day — the
	// day a mark is filed under.
	now := time.Now().UTC().Truncate(24 * time.Hour).Add(12 * time.Hour)
	days := func(n int) time.Time { return now.Add(-time.Duration(n) * 24 * time.Hour) }
	settle := func(id string, kept bool, recs ...store.EventRecord) {
		t.Helper()
		if err := log.WriteCustody(t.Context(), store.CustodyBatch{ID: id,
			Origin: "stateless-1", Records: recs}, now); err != nil {
			t.Fatalf("write %s: %v", id, err)
		}
		if err := log.SettleCustody(t.Context(), id, kept); err != nil {
			t.Fatalf("settle %s: %v", id, err)
		}
	}
	mark := func() (store.UsageRederive, bool) {
		t.Helper()
		m, ok, err := db.UsageRederive(t.Context())
		if err != nil {
			t.Fatalf("read the mark: %v", err)
		}
		return m, ok
	}
	if _, ok := mark(); ok {
		t.Fatal("a store nobody settled anything in holds a mark")
	}

	settle("a", true, phaseRecord(t, "a1", days(2), "s", "t", "execute", "", 1),
		phaseRecord(t, "a2", days(3), "s", "t", "execute", "", 1))
	if m, ok := mark(); !ok || !m.Since.Equal(days(3)) || m.Seq != 1 {
		t.Fatalf("after a kept batch of rows 2 and 3 days back the mark is (%+v, %v), "+
			"want its oldest row's instant at seq 1", m, ok)
	}
	settle("b", true, phaseRecord(t, "b1", days(5), "s", "t", "execute", "", 1))
	if m, _ := mark(); !m.Since.Equal(days(5)) || m.Seq != 2 {
		t.Fatalf("an older kept batch left the mark at %+v, want lowered to 5 days back at seq 2", m)
	}
	settle("c", true, phaseRecord(t, "c1", days(1), "s", "t", "execute", "", 1))
	if m, _ := mark(); !m.Since.Equal(days(5)) || m.Seq != 3 {
		t.Fatalf("a newer kept batch left the mark at %+v, want 5 days back still, at seq 3 "+
			"— raised, the older day would never be derived again", m)
	}
	settle("d", true, store.EventRecord{ID: "d1", Type: "notification_sent", Time: days(20),
		Category: "notification", Payload: []byte(`{}`)})
	if m, _ := mark(); !m.Since.Equal(days(5)) || m.Seq != 3 {
		t.Fatalf("a kept batch of rows no day is derived from moved the mark to %+v", m)
	}
	settle("d2", true, gateRecord(t, "d2", days(20), failedCheck("unit", 1, "oid-b", "passed", "dev", "a-dev")))
	if m, _ := mark(); !m.Since.Equal(days(5)) || m.Seq != 3 {
		t.Fatalf("a kept batch of a check's ending that names no seat — one of a type a day counts some "+
			"rows of — moved the mark to %+v", m)
	}
	settle("e", false, phaseRecord(t, "e1", days(10), "s", "t", "execute", "", 1))
	if m, _ := mark(); !m.Since.Equal(days(5)) || m.Seq != 3 {
		t.Fatalf("a batch NOT kept moved the mark to %+v — none of its rows ever counted", m)
	}

	// THE PUBLISHER READS (5 days back, seq 3), and a settle of today's rows
	// lands before it clears: the clear leaves that settle's mark, and only
	// today's — never the five days back it read and published.
	read, _ := mark()
	settle("f", true, phaseRecord(t, "f1", now, "s", "t", "execute", "", 1))
	if cleared, err := db.ClearUsageRederive(t.Context(), read.Seq); err != nil || !cleared {
		t.Fatalf("a clear at the seq it read answered (%v, %v)", cleared, err)
	}
	m, ok := mark()
	if !ok {
		t.Fatal("the clear took the mark of a settle that landed after its read: its " +
			"day would never be derived")
	}
	if !m.Since.Equal(now) {
		t.Fatalf("after the clear the oldest mark is %v, want the later settle's own %v — "+
			"a settle of today's rows that carried the five days back the publisher had "+
			"just published would have it derive them again on every tick", m.Since, now)
	}

	// AND A SETTLE ON THE DAY OF A MARK THE PUBLISHER READ survives the clear
	// too, at the older of the two instants.
	read = m
	settle("g", true, phaseRecord(t, "g1", now.Add(-time.Hour), "s", "t", "execute", "", 1))
	if cleared, err := db.ClearUsageRederive(t.Context(), read.Seq); err != nil || cleared {
		t.Fatalf("a clear of a mark a settle has since moved answered (%v, %v)", cleared, err)
	}
	if m, ok = mark(); !ok || !m.Since.Equal(now.Add(-time.Hour)) {
		t.Fatalf("after a settle on the day the publisher read, the mark is (%+v, %v), "+
			"want that day's, at the settle's older instant", m, ok)
	}
	if cleared, err := db.ClearUsageRederive(t.Context(), m.Seq); err != nil || !cleared {
		t.Fatalf("the clear at the newest seq answered (%v, %v)", cleared, err)
	}
	if _, ok := mark(); ok {
		t.Fatal("the clear at the newest seq left a mark")
	}
}

// A SETTLE WHOSE MARK CANNOT BE WRITTEN DOES NOT SETTLE.
//
// The two are one transaction because they are only true together: a batch
// settled as kept with no mark is rows counting on a day nothing will derive
// again, which is exactly the loss the mark exists to close. So when the mark
// cannot be written the batch stays unsettled — its rows still out of every
// count — and the keeper's next pass settles it again.
func TestASettleWhoseMarkCannotBeWrittenDoesNotSettle(t *testing.T) {
	t.Parallel()
	db := open(t)
	log := db.Events()
	at := time.Now().UTC().Add(-72 * time.Hour)
	if err := log.WriteCustody(t.Context(), store.CustodyBatch{ID: "batch", Origin: "stateless-1",
		Records: []store.EventRecord{phaseRecord(t, "r1", at, "s", "t", "execute", "", 5)}},
		at); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(t.Context(), `DROP TABLE usage_rederive`); err != nil {
		t.Fatal(err)
	}
	if err := log.SettleCustody(t.Context(), "batch", true); err == nil {
		t.Fatal("a kept settle whose mark could not be written answered success")
	}
	if got := unsettled(t, log, time.Now()); len(got) != 1 || got[0] != "batch" {
		t.Fatalf("after a settle whose mark failed the unsettled batches are %v — the "+
			"settle committed without the mark, so its rows count on a day nothing "+
			"derives again", got)
	}
}

// THE WATERMARK IS ONE VALUE, read back as written.
//
// It is the day a publisher that has just started derives everything from
// again, so "never written" must read as itself — a fresh node derives nothing
// old — and every write must replace the last.
func TestTheUsageWatermarkReadsBackWhatWasWritten(t *testing.T) {
	t.Parallel()
	db := open(t)
	if _, ok, err := db.UsageWatermark(t.Context()); err != nil || ok {
		t.Fatalf("a store nobody wrote a watermark in answered one (%v, %v)", ok, err)
	}
	for _, at := range []time.Time{
		time.Date(2026, 9, 21, 3, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 18, 3, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 22, 3, 0, 0, 0, time.UTC),
	} {
		if err := db.SetUsageWatermark(t.Context(), at); err != nil {
			t.Fatal(err)
		}
		if got, ok, err := db.UsageWatermark(t.Context()); err != nil || !ok || !got.Equal(at) {
			t.Fatalf("after writing %v the watermark reads (%v, %v, %v)", at, got, ok, err)
		}
	}
}

// A USAGE DAY KEEPS THE FIRST CUT IT WAS GIVEN, the instant it falls in is
// found by that cut, and the cut goes with the event log's rows.
//
// A label is cut into instants on the company's clock, which an apply can
// move, and a day derived again on another cut than its record was published on
// moves rows between published days. So a second cut of a label is not
// recorded — the first is answered — and the sweep that ages the event log out
// takes the cuts of the days it ages out, which nothing derives again.
//
// Mutation: let a later cut replace the first, or leave the cuts out of the
// event log's purge, and this goes red.
func TestAUsageDayKeepsItsFirstCut(t *testing.T) {
	t.Parallel()
	db := open(t)
	day := time.Now().UTC().Truncate(24 * time.Hour)
	first := store.UsageWindow{Start: day, End: day.Add(24 * time.Hour)}
	got, err := db.PinUsageDay(t.Context(), "today", first)
	if err != nil || got != first {
		t.Fatalf("the first cut recorded (%+v, %v), want %+v", got, err, first)
	}
	moved := store.UsageWindow{Start: day.Add(5 * time.Hour), End: day.Add(29 * time.Hour)}
	if got, err := db.PinUsageDay(t.Context(), "today", moved); err != nil || got != first {
		t.Fatalf("a second cut of the day answered (%+v, %v), want the first %+v", got, err, first)
	}
	if label, w, ok, err := db.UsageDayContaining(t.Context(), day.Add(2*time.Hour)); err != nil ||
		!ok || label != "today" || w != first {
		t.Fatalf("an instant inside the first cut is found in (%q, %+v, %v, %v)", label, w, ok, err)
	}
	if _, _, ok, err := db.UsageDayContaining(t.Context(), day.Add(25*time.Hour)); err != nil || ok {
		t.Fatalf("an instant only the refused cut held is found in a day (%v, %v)", ok, err)
	}
	if _, err := db.PinUsageDay(t.Context(), "empty", store.UsageWindow{Start: day, End: day}); err == nil {
		t.Fatal("a cut that does not end after it starts was recorded")
	}

	old := day.Add(-store.EventRetention - 48*time.Hour)
	if _, err := db.PinUsageDay(t.Context(), "aged",
		store.UsageWindow{Start: old, End: old.Add(24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Events().Purge(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := db.UsageDayWindow(t.Context(), "aged"); err != nil || ok {
		t.Errorf("the cut of a day past the event log's retention outlived its purge (%v, %v)", ok, err)
	}
	if _, ok, err := db.UsageDayWindow(t.Context(), "today"); err != nil || !ok {
		t.Errorf("today's cut went with the purge (%v, %v)", ok, err)
	}
}

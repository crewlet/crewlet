package executordomain

import (
	"database/sql"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// summaryFor is the summary a box read of turn kept, as its end names it.
func summaryFor(turn string) *executorkeep.Summary {
	return &executorkeep.Summary{Object: objstore.Object{Key: keyAt(monday.Add(time.Second)),
		Hash: objstore.HashOf([]byte("summary/" + turn)), Size: 300}, ContentKey: "k1", At: monday,
		Bytes: 256, Cut: true, Harness: "claude-code", Session: "s-" + turn}
}

// endingWithSummary is turn's end naming its summary.
func endingWithSummary(seat, conversation, turn, work string) End {
	return ending(seat, conversation, turn, work, executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted,
		Summary: summaryFor(turn)})
}

// summaryRows is how many summary rows seat's turn has, read off the table
// itself, so a summary left behind by a row that went is counted.
func (e *estate) summaryRows(seat, turn string) int {
	e.t.Helper()
	var n int
	if err := e.db.Read(e.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(e.t.Context(), `SELECT COUNT(*) FROM executor_session_summaries
			WHERE agent_handle = ? AND turn_id = ?`, seat, turn).Scan(&n)
	}); err != nil {
		e.t.Fatalf("count %s's summaries of turn %s: %v", seat, turn, err)
	}
	return n
}

// A SUMMARY GOES WITH ITS ROW, AND ONLY WITH IT. An end naming a summary
// writes it beside the turn's row, which reads it back as the end named it;
// a retirement, which takes a spent session off, keeps it, since it is what a
// run that cannot continue the session starts from; and the seat's budget
// releasing the row's session, an absorption, a dedupe, the retention and the
// seat's removal each take it with what they take of the row, so no summary
// outlives its row and keeps its object from the collector.
//
// Mutation: release the summary in retireSpent, or drop it from any release
// or delete of the row, and its case goes red.
func TestASummaryGoesWithItsRow(t *testing.T) {
	t.Parallel()
	t.Run("an end names it", func(t *testing.T) {
		t.Parallel()
		e := newEstate(t)
		run := newTurn()
		e.capture(capturing("ana", "chat:room", run, 1, 1, 100, budget))
		end := endingWithSummary("ana", "chat:room", run, "")
		e.end(end)
		s, _ := e.session("ana", run)
		want := end.Ending.Summary
		if got := s.Summary; got == nil || got.Object != want.Object || got.ContentKey != want.ContentKey ||
			!got.At.Equal(want.At) || got.Bytes != want.Bytes || got.Cut != want.Cut ||
			got.Harness != want.Harness || got.Session != want.Session {
			t.Fatalf("the ended row keeps the summary %+v, want the one its end named, %+v", got, want)
		}
	})
	t.Run("an end of a turn with no row names it", func(t *testing.T) {
		t.Parallel()
		e := newEstate(t)
		run := newTurn()
		e.end(endingWithSummary("ana", "chat:room", run, ""))
		if s, found := e.session("ana", run); !found || s.HasObject() || s.Summary == nil {
			t.Fatalf("the ending-only row is %+v (found %v), want it naming no session and keeping the summary",
				s, found)
		}
	})
	t.Run("a retirement keeps it", func(t *testing.T) {
		t.Parallel()
		e := newEstate(t)
		spent, next := newTurn(), newTurn()
		e.capture(capturingSession("ana", "chat:room", spent, "s-spent", 1))
		e.end(endingWithSummary("ana", "chat:room", spent, ""))
		e.capture(capturingSession("ana", "chat:room", next, "s-spent", 1))
		e.end(ending("ana", "chat:room", next, "", executorkeep.Ending{Outcome: executorkeep.OutcomeFailed,
			SpentSessions: []executorkeep.SpentSession{{Session: "s-spent",
				Loss: executorkeep.LossContextExhausted}}}))
		s, _ := e.session("ana", spent)
		if s.HasObject() || s.Summary == nil {
			t.Fatalf("the retired row is %+v, want its session gone and its summary kept", s)
		}
	})
	t.Run("the budget releases it with the session", func(t *testing.T) {
		t.Parallel()
		e := newEstate(t)
		old, fresh := newTurn(), newTurn()
		e.capture(capturing("ana", "chat:old", old, 1, 1, 100, budget))
		e.end(endingWithSummary("ana", "chat:old", old, ""))
		e.capture(capturing("ana", "chat:new", fresh, 1, 1, 100, 150))
		if s, _ := e.session("ana", old); s.HasObject() || e.summaryRows("ana", old) != 0 {
			t.Fatalf("the budget's release left the row %+v with %d summaries, want both released",
				s, e.summaryRows("ana", old))
		}
	})
	t.Run("an absorption takes it", func(t *testing.T) {
		t.Parallel()
		e := newEstate(t)
		head, current := newTurn(), newTurn()
		e.capture(capturing("ana", "chat:room", head, 1, 1, 100, budget))
		e.end(endingWithSummary("ana", "chat:room", head, ""))
		read := capturing("ana", "chat:room", current, 1, 1, 100, budget)
		read.ReadThrough, read.Absorbs = e.at().Packed(), []string{head}
		e.capture(read)
		e.end(ending("ana", "chat:room", current, "", executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}))
		if _, found := e.session("ana", head); found || e.summaryRows("ana", head) != 0 {
			t.Fatalf("the absorbed row stands (%v) with %d summaries, want both gone", found,
				e.summaryRows("ana", head))
		}
	})
	t.Run("a dedupe takes it", func(t *testing.T) {
		t.Parallel()
		e := newEstate(t)
		first, second := newTurn(), newTurn()
		e.capture(capturing("ana", "chat:room", first, 1, 1, 100, budget))
		e.end(endingWithSummary("ana", "chat:room", first, "work-1"))
		e.capture(capturing("ana", "chat:room", second, 1, 1, 100, budget))
		e.end(ending("ana", "chat:room", second, "work-1",
			executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}))
		if _, found := e.session("ana", first); found || e.summaryRows("ana", first) != 0 {
			t.Fatalf("the deduped row stands (%v) with %d summaries, want both gone", found,
				e.summaryRows("ana", first))
		}
	})
	t.Run("the retention takes it", func(t *testing.T) {
		t.Parallel()
		e := newEstate(t)
		run := newTurn()
		e.capture(capturing("ana", "chat:room", run, 1, 1, 100, budget))
		ended, _ := e.end(endingWithSummary("ana", "chat:room", run, ""))
		e.apply(ConversationSubject("ana", "chat:room", run), OpDelete, ScopeSet{Subject: true},
			Delete{V: 1, Handle: "ana", Turn: run, ConversationKey: "chat:room", Cutoff: stamped(ended.Seq)})
		if _, found := e.session("ana", run); found || e.summaryRows("ana", run) != 0 {
			t.Fatalf("the retained-past row stands (%v) with %d summaries, want both gone", found,
				e.summaryRows("ana", run))
		}
	})
	t.Run("a seat's removal takes it", func(t *testing.T) {
		t.Parallel()
		e := newEstate(t)
		run := newTurn()
		e.end(endingWithSummary("ana", "chat:room", run, ""))
		e.removeSeat("ana")
		if e.summaryRows("ana", run) != 0 {
			t.Fatal("a removed seat's summary was left behind")
		}
	})
}

// AN END NAMES NO SUMMARY PAST objstore.RecordWithin, BY ITS DECIDE'S OWN
// CLOCK, AND LANDS ALL THE SAME: the collector may already have deleted what
// no row named for so long, so the end leaves the summary out, as a capture of
// such an object is refused, and a turn's end is never refused for it.
//
// Mutation: name the summary whatever its age, or refuse the end for it, and
// a case goes red.
func TestAnEndNamesNoSummaryPastRecordWithin(t *testing.T) {
	t.Parallel()
	summary := summaryFor(newTurn())
	minted := summary.Object.Key.Minted()
	if got := nameableSummary(summary, minted.Add(objstore.RecordWithin)); got != summary {
		t.Errorf("an end decided at the window's edge names %+v, want the summary", got)
	}
	if got := nameableSummary(summary, minted.Add(objstore.RecordWithin+time.Second)); got != nil {
		t.Errorf("an end decided past the window names %+v, want none", got)
	}
	if got := nameableSummary(nil, minted); got != nil {
		t.Errorf("an end naming no summary names %+v", got)
	}
}

// AN END NAMES THE SUMMARY ITS ROW KEEPS. Summaries are Required, so a backup
// carries every object a record of the log names: the declaration's reader
// answers the row an end writes, with the summary's object, and none for an
// end naming none, a capture or the barrier.
//
// Mutation: answer an end's summary as none, or a capture's session as a
// summary, and this fails.
func TestAnEndNamesTheSummaryItsRowKeeps(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	records := SummaryObjectReferences.Records
	turn := newTurn()
	c := capturing("ana", "chat:room", turn, 1, 1, 100, budget)
	captured := e.encode(ConversationSubject(c.Handle, c.ConversationKey, c.Turn), OpCapture,
		seatScope(c.Handle), c)
	e.applyEncoded(OpCapture, captured)
	ended := e.encode(ConversationSubject("ana", "chat:room", turn), OpEnd, ScopeSet{Subject: true},
		endingWithSummary("ana", "chat:room", turn, ""))
	e.applyEncoded(OpEnd, ended)
	got, err := records(ended)
	row, _ := e.session("ana", turn)
	if err != nil || len(got) != 1 || got[0].NamedBy != objstore.JoinOwner("ana", turn) ||
		row.Summary == nil || got[0].Object != row.Summary.Object {
		t.Fatalf("the end: Records = %+v, %v; want the summary its row keeps, %+v", got, err, row.Summary)
	}
	other := newTurn()
	bare := e.encode(ConversationSubject("ana", "chat:room", other), OpEnd, ScopeSet{Subject: true},
		ending("ana", "chat:room", other, "", executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}))
	barrier, err := EncodeBarrier(statelog.Envelope{Kind: statelog.BarrierKind})
	if err != nil {
		t.Fatal(err)
	}
	for name, payload := range map[string][]byte{"a capture": captured, "an end naming none": bare,
		"the barrier": barrier} {
		if got, err := records(payload); err != nil || got != nil {
			t.Errorf("%s: Records = %+v, %v; want none", name, got, err)
		}
	}
}

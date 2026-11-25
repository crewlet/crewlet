package executordomain

import (
	"database/sql"
	"encoding/json"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The served rows (package doc, "A turn serves every conversation its run
// read"): a run in a chat read a wake of a ticket's thread, so the ticket's next
// turn continues the session the chat's turn kept.

const (
	servedSeat   = "ana"
	servedChat   = "chat:room"
	servedTicket = "ticket:42"
	servedBudget = int64(1 << 30)
)

var completedEnding = executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}

// ranIn is a turn of conversation that read its head, captured a session of
// size bytes and ended, naming served as the other conversations it served.
func ranIn(e *estate, conversation string, size int64, served ...string) string {
	e.t.Helper()
	turn := newTurn()
	e.capture(launching(e.head(servedSeat, conversation),
		capturing(servedSeat, conversation, turn, 1, 1, size, servedBudget)))
	end := ending(servedSeat, conversation, turn, "", completedEnding)
	end.Served = served
	e.end(end)
	return turn
}

// headTurn is the turn conversation's head names, "" for none.
func headTurn(e *estate, conversation string) string {
	e.t.Helper()
	if h := e.head(servedSeat, conversation); h.Session != nil {
		return h.Session.Turn
	}
	return ""
}

// servedRows is how many conversations turn serves beyond its own.
func (e *estate) servedRows(turn string) int {
	e.t.Helper()
	var n int
	if err := e.db.Read(e.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(e.t.Context(), `SELECT COUNT(*) FROM executor_session_serves
			WHERE agent_handle = ? AND turn_id = ?`, servedSeat, turn).Scan(&n)
	}); err != nil {
		e.t.Fatalf("count %s's served rows: %v", turn, err)
	}
	return n
}

// A CONVERSATION WHOSE WAKE A RUN READ CONTINUES THE SESSION ITS TURN KEPT, and
// the turn's own conversation still does: the end names the ticket, and the
// ticket's head is the chat's turn, its own older head told and offered beside
// it and handed out for its next end to absorb. A conversation the end does not
// name reads only its own.
//
// Mutation: write no served row at the end, or leave the served rows out of the
// head read, and the ticket's head is its older turn.
func TestAConversationWhoseWakeARunReadContinuesItsSession(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	older := ranIn(e, servedTicket, 1<<20)
	run := ranIn(e, servedChat, 1<<20, servedTicket)
	if got := headTurn(e, servedTicket); got != run {
		t.Errorf("the ticket's head is %q, want the chat's turn %s whose run read its wake", got, run)
	}
	if got := headTurn(e, servedChat); got != run {
		t.Errorf("the chat's head is %q, want its own turn %s", got, run)
	}
	h := e.head(servedSeat, servedTicket)
	if len(h.Endings) != 1 || h.Endings[0].Turn != older || len(h.Siblings) != 1 {
		t.Errorf("the ticket's read tells %d endings and offers %d siblings, want its older turn as both",
			len(h.Endings), len(h.Siblings))
	}
	if absorbs := h.Absorbs(); !slices.Contains(absorbs, run) || !slices.Contains(absorbs, older) {
		t.Errorf("the ticket's read hands out %v, want the served turn and its older one", absorbs)
	}
	if got := headTurn(e, "ticket:7"); got != "" {
		t.Errorf("a conversation no end named reads %q as its head", got)
	}
}

// A SERVED TURN TAKES ITS PLACE IN THE CONVERSATION'S LINE where its end applied:
// it outranks the conversation's older head and a turn of the conversation that
// read before that end, however late it ended; a turn that read at that very
// position continued it and outranks it by its later end, and its end absorbs
// the served row alone, the served turn's row staying the chat's head. That end
// absorbs the row it ties with, so the tie is read with the row put back.
//
// Mutation: order the served head by the served turn's own read, or let the
// served row win a tie, and the ticket's head is the wrong turn.
func TestAServedTurnTakesItsPlaceInTheConversationsLine(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	ranIn(e, servedTicket, 1<<20)
	early := newTurn()
	e.capture(launching(e.head(servedSeat, servedTicket),
		capturing(servedSeat, servedTicket, early, 1, 1, 1<<20, servedBudget)))
	run := ranIn(e, servedChat, 1<<20, servedTicket)
	later := newTurn()
	read := e.head(servedSeat, servedTicket)
	if read.Session == nil || read.Session.Turn != run || read.Position.Packed() != read.Session.EndedAtPosition.Packed() {
		t.Fatalf("the premise: the ticket is read at the served turn's end, its head (%v)", read.Session)
	}
	e.capture(launching(read, capturing(servedSeat, servedTicket, later, 1, 1, 1<<20, servedBudget)))
	e.end(ending(servedSeat, servedTicket, early, "", completedEnding))
	if got := headTurn(e, servedTicket); got != run {
		t.Errorf("the ticket's head is %q, want the served turn %s: the turn that read before its end "+
			"ended later and continued none of it", got, run)
	}
	e.end(ending(servedSeat, servedTicket, later, "", completedEnding))
	if got := headTurn(e, servedTicket); got != later {
		t.Errorf("the ticket's head is %q, want its later turn %s, which read where the served turn ended",
			got, later)
	}
	if n := e.servedRows(run); n != 0 {
		t.Errorf("the served turn serves %d conversations after the ticket's later end absorbed it", n)
	}
	if got := headTurn(e, servedChat); got != run {
		t.Errorf("the chat's head is %q, want its turn %s, whose row the ticket's end left", got, run)
	}
	if err := e.db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), insertServedStatement, servedSeat, run, servedTicket,
			read.Position.Packed())
		return err
	}); err != nil {
		t.Fatalf("put the served row back: %v", err)
	}
	if got := headTurn(e, servedTicket); got != later {
		t.Errorf("the ticket's head is %q beside the served row it ties with, want %s, which ended later",
			got, later)
	}
}

// A ROW ANOTHER CONVERSATION READS IS NOT ABSORBED BY ITS OWN: the chat's next
// end skips it, as it skips a held row, so the ticket still continues it; once
// the ticket's own end absorbs its served row, the chat's next end shown the
// row takes it.
//
// Mutation: drop the served check from the absorption and the ticket's head is
// gone with the chat's next end.
func TestARowAnotherConversationReadsIsNotAbsorbedByItsOwn(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	run := ranIn(e, servedChat, 1<<20, servedTicket)
	ranIn(e, servedChat, 1<<20)
	if _, found := e.session(servedSeat, run); !found {
		t.Fatal("the chat's next end took the row the ticket reads as served")
	}
	if got := headTurn(e, servedTicket); got != run {
		t.Errorf("the ticket's head is %q, want the served turn %s", got, run)
	}
	ranIn(e, servedTicket, 1<<20)
	if _, found := e.session(servedSeat, run); !found {
		t.Fatal("the ticket's end took the served turn's own row, the chat's to absorb")
	}
	ranIn(e, servedChat, 1<<20)
	if _, found := e.session(servedSeat, run); found {
		t.Error("the chat's end shown the row after no conversation read it as served left it")
	}
}

// A SERVED TURN'S SESSION IS ONE OBJECT, COUNTED ONCE: the seat's held bytes and
// the collector's references name it once with two conversations reading it,
// the budget's release takes it off both, and the retention takes the turn from
// both.
//
// Mutation: copy the object onto each served row and the seat counts it twice.
func TestAServedTurnsSessionIsCountedOnce(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	const size = 3 << 20
	run := ranIn(e, servedChat, size, servedTicket, "ticket:43")
	row, _ := e.session(servedSeat, run)
	var held, named int64
	if err := e.db.Read(t.Context(), func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(t.Context(), seatBytesStatement, servedSeat, newTurn()).Scan(&held); err != nil {
			return err
		}
		page, err := SessionObjectReferences.ReferencesAfter(100)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(t.Context(), page, "")
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var key, hash, handle, turn string
			var bytes int64
			if err := rows.Scan(&key, &hash, &bytes, &handle, &turn); err != nil {
				return err
			}
			if key == row.Object.Key.String() {
				named++
			}
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read the seat's bytes and references: %v", err)
	}
	if held != size || named != 1 {
		t.Errorf("the seat holds %d bytes and the references name the session %d times, want %d once",
			held, named, size)
	}
	// THE BUDGET RELEASES IT OFF BOTH: a capture with no room for both.
	other := newTurn()
	e.capture(launching(e.head(servedSeat, "chat:other"),
		capturing(servedSeat, "chat:other", other, 1, 1, size, size+1)))
	if h := e.head(servedSeat, servedTicket); h.Session != nil || len(h.Endings) != 1 {
		t.Errorf("after the release the ticket reads head %v and %d endings, want none and the served "+
			"turn's ending", h.Session, len(h.Endings))
	}
	e.apply(ConversationSubject(servedSeat, servedChat, run), OpDelete, seatScope(servedSeat),
		Delete{V: 1, Handle: servedSeat, Turn: run, ConversationKey: servedChat, Cutoff: stamped(e.seq + 1)})
	if _, found := e.session(servedSeat, run); found || e.servedRows(run) != 0 {
		t.Error("the retention left the turn's row or a conversation reading it")
	}
	if h := e.head(servedSeat, servedTicket); len(h.Endings) != 0 {
		t.Errorf("the ticket still tells %d endings of a turn the retention deleted", len(h.Endings))
	}
}

// A CONTINUATION OF A SERVED SESSION HOLDS IT: the ticket's turn's begin names
// the chat's turn as its base, and its row holds it although that turn is the
// chat's.
//
// Mutation: keep the base check to the begin's own conversation and the begin
// holds nothing.
func TestABeginHoldsTheServedSessionItContinues(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	run := ranIn(e, servedChat, 1<<20, servedTicket)
	next := newTurn()
	e.begin(beginning(e.head(servedSeat, servedTicket), servedSeat, servedTicket, next, ""))
	if row, _ := e.session(servedSeat, next); row.BaseTurn != run || !e.holds(servedSeat, run) {
		t.Errorf("the ticket's turn holds base %q, want the served turn %s held", row.BaseTurn, run)
	}
}

// A RETIREMENT DROPS THE SERVED ROW AND LEAVES THE TURN'S SESSION ITS OWN
// CONVERSATION'S: the ticket's turn ran out of context in the served session,
// so the ticket stops reading it, and the chat still continues it.
//
// Mutation: retire only the conversation's own rows and the ticket's next turn
// continues the session that ran out.
func TestARetirementDropsTheServedRowAlone(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	run := ranIn(e, servedChat, 1<<20, servedTicket)
	row, _ := e.session(servedSeat, run)
	out := newTurn()
	e.begin(beginning(e.head(servedSeat, servedTicket), servedSeat, servedTicket, out, ""))
	e.end(ending(servedSeat, servedTicket, out, "", executorkeep.Ending{Outcome: executorkeep.OutcomeFailed,
		SpentSessions: []executorkeep.SpentSession{{Session: row.SessionID,
			Loss: executorkeep.LossContextExhausted}}}))
	if got := headTurn(e, servedTicket); got != "" || e.servedRows(run) != 0 {
		t.Errorf("the ticket reads head %q after its turn ran out of context in it", got)
	}
	if kept, _ := e.session(servedSeat, run); !kept.HasObject() || headTurn(e, servedChat) != run {
		t.Error("the retirement in the ticket took the chat's session")
	}
}

// AN END'S SERVED CONVERSATIONS ARE READ BY ITS STAMP AND HELD TO THEIR RULE: an
// end naming them is stamped at servedVersion and one naming none below it, so
// a build reading less retains the first; one stamped below that carries them
// all the same is applied as that build applies it, naming nothing served; and
// one naming its own conversation writes nothing at all.
//
// Mutation: drop the versioned field's row, or apply the served rows whatever
// the stamp, and a case goes red.
func TestAnEndsServedConversationsAreReadByItsStamp(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	subject := ConversationSubject(servedSeat, servedChat, newTurn())
	stampOf := func(served []string) int {
		end := ending(servedSeat, servedChat, newTurn(), "", completedEnding)
		end.Served = served
		env, err := DecodeEnvelope(e.encode(subject, OpEnd, seatScope(servedSeat), end))
		if err != nil {
			t.Fatalf("decode the end: %v", err)
		}
		return env.V
	}
	if got := stampOf([]string{servedTicket}); got != servedVersion {
		t.Errorf("an end naming a conversation served is stamped %d, want %d", got, servedVersion)
	}
	if got := stampOf(nil); got >= servedVersion {
		t.Errorf("an end naming none is stamped %d, which a build reading %d retains", got, servedVersion-1)
	}

	// STAMPED BELOW: applied naming nothing served.
	forged := newTurn()
	e.capture(launching(e.head(servedSeat, servedChat),
		capturing(servedSeat, servedChat, forged, 1, 1, 1<<20, servedBudget)))
	end := ending(servedSeat, servedChat, forged, "", completedEnding)
	end.Served = []string{servedTicket}
	body, err := json.Marshal(end)
	if err != nil {
		t.Fatal(err)
	}
	// THE TABLE WITHOUT ITS ROW is a writer that stamps the field below it.
	raw, err := encodeWith(MutationRecord{RecordEnvelope: RecordEnvelope{V: wentOnVersion,
		OpID: statelog.NewOpID(monday, "harness"), Subject: ConversationSubject(servedSeat, servedChat, forged),
		Op: OpEnd, CreatedAt: monday, Gen: 1, Writer: "node-a", Scope: seatScope(servedSeat)},
		Mutation: body, Actor: "executor"}, slices.DeleteFunc(slices.Clone(versionedFields),
		func(f statelog.VersionedField) bool { return f.Name == "End.Served" }))
	if err != nil {
		t.Fatalf("encode an end stamped below servedVersion: %v", err)
	}
	e.applyEncoded(OpEnd, raw)
	if row, _ := e.session(servedSeat, forged); !row.Ended() || e.servedRows(forged) != 0 {
		t.Error("an end stamped below servedVersion wrote served rows, or did not end its turn")
	}

	// ITS OWN CONVERSATION AMONG THE OTHERS: the writer refuses it, and the
	// apply writes nothing.
	own := newTurn()
	bad := ending(servedSeat, servedChat, own, "", completedEnding)
	bad.Served = []string{servedChat}
	if _, n := e.end(bad); n != 0 {
		t.Errorf("an end naming its own conversation as served wrote %d rows", n)
	}
	if checkServed([]string{servedTicket, servedTicket}, servedChat) == nil ||
		checkServed([]string{""}, servedChat) == nil ||
		checkServed(make([]string, MaxServed+1), servedChat) == nil {
		t.Error("a served list naming one twice, an empty key, or past MaxServed passes")
	}
}

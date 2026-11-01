package executordomain

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// THE EXECUTOR STATEMENTS ARE THE ONES PLANNED, EVERY ONE OF THEM, AND EVERY
// INDEX ON THEIR TABLES SERVES ONE.
//
// # Why this is asserted rather than reviewed
//
// Most of these statements run inside an apply, which holds the store's one
// writer: a capture past its seat's budget reads the seat's ended sessions in
// the order it releases them, an end reads its conversation to dedupe and
// absorb, and a launch reads its conversation's head and its endings. A plan
// that sorts there sorts while every other domain's applier on the node waits,
// and one that reads a table whole reads every seat's sessions for one seat's
// question — neither ever fails, both show up as a slow log. The planner is
// the only thing that knows which a statement is, so this plans the
// statements this package runs, named where they are defined, as a deployment
// plans them: with no statistics, over an empty estate.
//
// # What it holds
//
//   - every statement in the module's source that reads `executor_sessions`,
//     `executor_session_tombs` or `executor_seat_removals` is registered here
//     ([storetest.StatementLiterals]): a reader nobody registered is one whose
//     plan nobody checked;
//   - a READ — a row by its key, a conversation's head and endings, the
//     budget's release order, a seat's page, the retention's nominations —
//     seeks an index it names, and one about an order sorts nothing;
//   - a CENSUS — a seat's held bytes, a conversation's counts, a seat's rows
//     deleted whole — reads no covered table from its heap;
//   - a PASS reads every row by definition — the seats holding sessions, the
//     sessions by sealing key — and is held to no sort;
//   - no plan is one the driver decorrelated ([storetest.Decorrelated]);
//   - and the inverse: every index the schema declares on a covered table is
//     in some registered plan, the collector's object index through the
//     statements the object store builds from [SessionObjectReferences].
//
// Mutation: drop `executor_sessions_ended_idx` and the budget's release sorts
// a seat's ended sessions inside the apply; order the endings by `end_seq DESC,
// turn_id DESC` with no index to serve the second column and the head's
// endings sort; page by `version` again and the inventory names
// `executor_sessions_seat_idx` as reached by nothing; read a covered table in
// a new literal anywhere in the module without registering it, and the walk
// names the file.
func TestTheExecutorStatementsArePlanned(t *testing.T) {
	t.Parallel()
	node, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"),
		store.Options{}, 1)
	t.Cleanup(func() {
		if err := node.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	covered := []string{"executor_sessions", "executor_session_tombs", "executor_seat_removals"}

	used := map[string]bool{}
	var planned []string
	plan := func(t *testing.T, statement string, args []any) []string {
		t.Helper()
		steps := storetest.Plan(t, db, statement, args)
		for _, index := range storetest.IndexesIn(steps) {
			used[index] = true
		}
		if line, rewritten := storetest.Decorrelated(steps); rewritten {
			t.Errorf("the driver rewrote a subquery into a join over its whole table "+
				"at %q:\n%s", line, strings.Join(steps, "\n"))
		}
		return steps
	}
	refuseHeap := func(t *testing.T, steps []string) {
		t.Helper()
		for _, table := range covered {
			if storetest.ScansHeap(steps, table) {
				t.Errorf("this read reads %s with no index:\n%s", table,
					strings.Join(steps, "\n"))
			}
		}
	}

	for name, read := range executorReads(t) {
		planned = append(planned, read.sql)
		t.Run(name, func(t *testing.T) {
			steps := plan(t, read.sql, read.args)
			refuseHeap(t, steps)
			shown := strings.Join(steps, "\n")
			if !slices.ContainsFunc(read.indexes, func(index string) bool {
				return storetest.Seeks(steps, index)
			}) {
				t.Errorf("this read seeks none of %v:\n%s", read.indexes, shown)
			}
			if read.ordered && storetest.Sorts(steps) {
				t.Errorf("this read reaches %v and then sorts what it read:\n%s",
					read.indexes, shown)
			}
		})
	}
	for name, census := range executorCensuses() {
		planned = append(planned, census.sql)
		t.Run(name, func(t *testing.T) {
			refuseHeap(t, plan(t, census.sql, census.args))
		})
	}
	for name, pass := range executorPasses() {
		planned = append(planned, pass.sql)
		t.Run(name, func(t *testing.T) {
			if steps := plan(t, pass.sql, pass.args); storetest.Sorts(steps) {
				t.Errorf("this pass sorts the rows it reads:\n%s", strings.Join(steps, "\n"))
			}
		})
	}

	// EVERY READER IN THE SOURCE IS ONE OF THEM.
	for _, literal := range storetest.StatementLiterals(t, "internal/executordomain", covered...) {
		if !slices.ContainsFunc(planned, func(statement string) bool {
			return strings.Contains(statement, literal.Text)
		}) {
			t.Errorf("%s reads an executor table in a statement this test does not "+
				"plan — register it with the reads, the censuses or the passes:\n%s",
				literal.File, literal.Text)
		}
	}

	// AND EVERY INDEX IS IN SOME PLAN.
	for _, index := range storetest.IndexesOn(t, db, covered) {
		if !used[index] {
			t.Errorf("no registered statement reaches %s: an index no plan reaches is "+
				"a write on every apply for no reader", index)
		}
	}
}

// executorPlanned is one registered statement and the arguments it is planned
// with.
type executorPlanned struct {
	sql  string
	args []any
}

// executorIndexed is a registered read, the indexes any one of which it may
// seek, and whether it is about the order it reads in.
type executorIndexed struct {
	executorPlanned
	indexes []string
	ordered bool
}

// The indexes the reads are held to.
const (
	sessionKey      = "sqlite_autoindex_executor_sessions_1"
	tombKey         = "sqlite_autoindex_executor_session_tombs_1"
	removalKey      = "sqlite_autoindex_executor_seat_removals_1"
	conversationIdx = "executor_sessions_conversation_idx"
	endedIdx        = "executor_sessions_ended_idx"
	seatIdx         = "executor_sessions_seat_idx"
	expiryIdx       = "executor_sessions_expiry_idx"
	unendedIdx      = "executor_sessions_unended_idx"
	objectIdx       = "executor_sessions_object_idx"
	tombsAtIdx      = "executor_session_tombs_at_idx"
	removalsAtIdx   = "executor_seat_removals_at_idx"
)

// executorReads is every read or write of one row by its key, every read of a
// conversation, of a seat in an order or of the retention's nominations, each
// with the index it seeks.
func executorReads(t *testing.T) map[string]executorIndexed {
	seek := func(index, statement string, args ...any) executorIndexed {
		return executorIndexed{executorPlanned: executorPlanned{statement, args},
			indexes: []string{index}}
	}
	inOrder := func(index, statement string, args ...any) executorIndexed {
		read := seek(index, statement, args...)
		read.ordered = true
		return read
	}
	const (
		seat, turn, key = "ana", "turn", "chat:room"
	)
	out := map[string]executorIndexed{
		// ONE TURN'S ROW, ITS TOMBSTONE AND ITS SEAT'S REMOVAL, by their
		// keys: the decides', the appliers' and the readers'.
		"a turn's row":                seek(sessionKey, readSessionStatement, seat, turn),
		"a turn's tombstone":          seek(tombKey, readTombStatement, seat, turn),
		"a seat's removal":            seek(removalKey, readRemovalStatement, seat),
		"a seat's removal instant":    seek(removalKey, removalAtStatement, seat),
		"a session released":          seek(sessionKey, releaseStatement, 1, seat, turn),
		"a later capture written":     seek(sessionKey, updateCaptureStatement, updateCaptureArgs()...),
		"a captured turn ended":       seek(sessionKey, endRowStatement, endRowArgs()...),
		"a turn's row deleted":        seek(sessionKey, deleteSessionStatement, seat, turn),
		"a turn's tombstone deleted":  seek(tombKey, deleteTombStatement, seat, turn),
		"a seat's removal forgotten":  seek(removalKey, forgetRemovalStatement, seat, 1),
		"a turn's conversation":       seek(sessionKey, turnConversationStatement, seat, turn),
		"a turn's whole row":          seek(sessionKey, sessionStatement, seat, turn),
		"a capture read back":         seek(sessionKey, captureReadBackStatement, seat, turn),
		"an older attempt delivered?": seek(conversationIdx, deliveredAttemptsStatement, seat, key, "w", turn),
		"an older attempt deduped":    seek(conversationIdx, dedupeStatement, seat, key, "w", turn),

		// A TURN THE READ HANDED OUT, absorbed by its key — or, should the
		// planner prefer it, through its conversation, the end's own.
		"a handed-out turn absorbed": {
			executorPlanned: executorPlanned{absorbStatement, []any{seat, turn, key, 1}},
			indexes:         []string{sessionKey, conversationIdx},
		},

		// A REMOVED SEAT'S LIVE TURNS, in the order of their key.
		"a removed seat's live turns": {
			executorPlanned: executorPlanned{seatLiveStatement, []any{seat}},
			indexes:         []string{sessionKey, endedIdx},
		},

		// THE BUDGET'S RELEASE, the oldest end first, inside the apply.
		"the seat's ended sessions": inOrder(endedIdx, seatReleasableStatement, seat, turn),

		// A LAUNCH'S READ of its conversation: the head and its endings.
		"a conversation's head":    inOrder(conversationIdx, headStatement, seat, key),
		"a conversation's endings": inOrder(conversationIdx, endingsStatement, seat, key, turn, MaxEndings),

		// A SEAT'S PAGE, the latest begun first.
		"a seat's page": inOrder(seatIdx, pageStatement, seat, 1<<62, DefaultPageSize+1),

		// THE RETENTION'S NOMINATIONS, the oldest first.
		"the rows past the retention":       inOrder(expiryIdx, expiredRowsStatement, 1, 10),
		"the tombstones past the retention": inOrder(tombsAtIdx, expiredTombsStatement, 1, 10),
		"the removals past the retention":   inOrder(removalsAtIdx, expiredRemovalsStatement, 1, 10),
		"the turns that never ended":        inOrder(unendedIdx, unendedStatement, 1, 10),
	}
	// THE COLLECTOR'S, THE AUDIT'S AND THE BACKUP'S, as the object store
	// builds them from the table's declaration: the row naming a key, and a
	// page of every key named.
	among, err := SessionObjectReferences.ObjectsAmong(2)
	if err != nil {
		t.Fatalf("the collector's question: %v", err)
	}
	out["the collector's question"] = seek(objectIdx, among, "k1", "k2")
	page, err := SessionObjectReferences.ReferencesAfter(100)
	if err != nil {
		t.Fatalf("the audit's page: %v", err)
	}
	out["the audit's page"] = inOrder(objectIdx, page, "")
	return out
}

// updateCaptureArgs are updateCaptureStatement's arguments, in its order.
func updateCaptureArgs() []any {
	return []any{"w", "claude-code", "2.1", "session", "object", "hash", 1, "k1", 1, 1, 1,
		"/home", "/work", 1, 1, "", 1, 1, "ana", "turn", 1}
}

// endRowArgs are endRowStatement's arguments, in its order.
func endRowArgs() []any {
	return []any{"w", 1, 1, "completed", 0, "{}", "ask", 1, 1, "ana", "turn"}
}

// executorCensuses is every statement whose question names a set of rows it
// reads whole — a seat's held bytes, a conversation's counts, a seat's rows
// deleted at its removal — held to no heap scan of a covered table.
func executorCensuses() map[string]executorPlanned {
	return map[string]executorPlanned{
		"a seat's held bytes":           {seatHeldStatement, []any{"ana", "turn"}},
		"a conversation's endings":      {endingsCountStatement, []any{"ana", "chat:room", "turn"}},
		"a conversation's sessions":     {siblingsCountStatement, []any{"ana", "chat:room", "turn"}},
		"a removed seat's rows deleted": {seatRowsStatement, []any{"ana"}},
	}
}

// executorPasses is every statement that reads every row it names by
// definition: the seats holding sessions, which a retirement with no handle
// matches its seat against, and the sessions counted by the key that sealed
// them, which a keyring rotation waits on.
func executorPasses() map[string]executorPlanned {
	return map[string]executorPlanned{
		"the seats holding sessions":  {handlesStatement, nil},
		"the sessions by sealing key": {sealedKeysStatement, nil},
	}
}

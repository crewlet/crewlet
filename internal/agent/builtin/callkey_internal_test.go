package builtin

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE TOOL IS PART OF EVERY KEY A TURN DERIVES, so two tools called with the
// same arguments at the same count in one turn are two calls: a join, a leave
// and a read of one room all say `{"channel": "eng"}`. Outside a turn the key
// is the caller's own, whatever the tool.
//
// Mutation: drop the tool from callKey's identity and the two keys are one.
func TestTwoToolsCalledAlikeInOneTurnDeriveTwoKeys(t *testing.T) {
	t.Parallel()
	turn := &turnctx.Turn{RunID: "run-1", WorkKey: "wk-1", Calls: turnctx.NewCallLog()}
	args := map[string]any{"channel": "eng"}
	join := callOpKey(turn, "join_channel", nil, args, "")
	read := callOpKey(turn, "read_channel", nil, args, "")
	if join == "" || join == read {
		t.Fatalf("join_channel and read_channel with one argument set derived "+
			"%q and %q, want two keys", join, read)
	}
	if again := callOpKey(turn, "join_channel", nil, args, ""); again != join {
		t.Errorf("the same call derived %q and then %q, want one key", join, again)
	}
	const sent = "k0123456789abcdef"
	if got := callOpKey(nil, "join_channel", nil, args, sent); got != sent {
		t.Errorf("outside a turn the key is %q, want the caller's own %q", got, sent)
	}
}

// seatInATurn is a seat's actor as a turn binds it: its run, the unit of work
// its ids are seeded from, the instant that work began, and the run's log.
func seatInATurn() Actor {
	began := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	return Actor{Handle: "eng", Kind: worklet.AuthorAgent, WrittenFor: types.ForTurn(nil),
		TurnID: statelog.NewOpID(began.Add(time.Minute), ""), WorkKey: "wk-1",
		WorkSince: began, Calls: turnctx.NewCallLog()}
}

// THE TRACKER'S IDS ARE DERIVED FROM THE CALL'S KEY, so two tools making one
// write are two operations. The repeat count is kept per tool, so two tools'
// first calls with the same arguments both count zero: without the tool, a
// project's tags declared by two tools, or two tools' comments on one task,
// were one operation or one comment, the second answered `applied` unwritten.
//
// Mutation: drop the tool from callKey's identity and both ids are one.
func TestTwoToolsMakingOneWriteAreTwoOperations(t *testing.T) {
	t.Parallel()
	actor := seatInATurn()
	args := map[string]any{"labels": []any{"regression"}}
	if a, b := opIDFor(actor, "write_project", "tags", "ENG", args),
		opIDFor(actor, "update_work_item", "tags", "ENG", args); a == b {
		t.Errorf("two tools declaring ENG's tags with one argument set were the "+
			"one operation %q", a)
	}
	if a, b := commentID(actor, "comment_on_work_item", "t-1", args),
		commentID(actor, "create_work_item", "t-1", args); a == b {
		t.Errorf("two tools commenting on t-1 with one argument set derived the "+
			"one comment %q", a)
	}
}

// A COMMENT KEEPS ITS ROW ACROSS A REBASE, while its operation moves.
//
// A rebased turn ([turnctx.Turn.RebasedTo], ADR-0029) re-mints its operations
// at the attempt's own instant, so a ledger that forgot them decides them
// again. A tracker comment's id is the key of its row, not an operation, so it
// is derived as the work began: the rebased attempt names the row already
// written, and a remark is not posted twice.
//
// Mutation: derive commentID from the rebased call key and the rebased
// comment takes a new id.
func TestACommentKeepsItsRowAcrossARebase(t *testing.T) {
	t.Parallel()
	args := map[string]any{"item": "ENG-1", "body": "Shipped."}
	first := seatInATurn()
	rebased := seatInATurn()
	rebased.RebasedTo = first.WorkSince.Add(40 * 24 * time.Hour)
	again := seatInATurn()
	again.RebasedTo = rebased.RebasedTo

	opFirst := opIDFor(first, "comment_on_work_item", "comment", "t-1", args)
	opRebased := opIDFor(rebased, "comment_on_work_item", "comment", "t-1", args)
	if opFirst == opRebased {
		t.Fatalf("the rebased attempt's operation is the first one's, %q — the "+
			"ledger that forgot it would answer it unknown", opFirst)
	}
	if at, ok := statelog.OpMintedAt(opRebased); !ok || !at.Equal(rebased.RebasedTo) {
		t.Errorf("the rebased operation %q carries %v, want the rebase's %s",
			opRebased, at, rebased.RebasedTo)
	}
	if got := opIDFor(again, "comment_on_work_item", "comment", "t-1", args); got != opRebased {
		t.Errorf("an attempt rebased onto the same instant derived the operation "+
			"%q, want the rebased one's %q", got, opRebased)
	}
	idFirst := commentID(first, "comment_on_work_item", "t-1", args)
	for name, attempt := range map[string]Actor{"rebased": rebased, "rebased again": again} {
		if got := commentID(attempt, "comment_on_work_item", "t-1", args); got != idFirst {
			t.Errorf("the %s attempt derived the comment %q, want the first "+
				"attempt's %q — its row, which the rebase does not move", name, got,
				idFirst)
		}
	}
	other := map[string]any{"item": "ENG-1", "body": "Rolled back."}
	if commentID(rebased, "comment_on_work_item", "t-1", other) == idFirst {
		t.Errorf("another remark in the rebased attempt derived the first one's "+
			"comment %q", idFirst)
	}
}

// A CREATED ITEM AND A CREATED VIEW KEEP THEIR IDS ACROSS A REBASE, as a
// comment does, while their operations move: each id is the subject its create
// arbitrates on and the key of its row, so an id that moved with a rebase
// filed a second item or view beside the first.
//
// Mutation: derive createdTaskID or createdViewID from the rebased actor and
// the rebased attempt names a new item or view.
func TestACreatedItemAndViewKeepTheirIdsAcrossARebase(t *testing.T) {
	t.Parallel()
	args := map[string]any{"title": "the follow-up", "project": "ENG"}
	first := seatInATurn()
	rebased := seatInATurn()
	rebased.RebasedTo = first.WorkSince.Add(40 * 24 * time.Hour)

	if opIDFor(first, CreateWorkItemTool, "create", "ENG", args) ==
		opIDFor(rebased, CreateWorkItemTool, "create", "ENG", args) {
		t.Fatal("the rebased create's operation is the first one's — the ledger " +
			"that forgot it would answer it unknown")
	}
	task := createdTaskID(first, CreateWorkItemTool, "ENG", args)
	if got := createdTaskID(rebased, CreateWorkItemTool, "ENG", args); got != task {
		t.Errorf("the rebased create names task %q, want the first attempt's %q", got, task)
	}
	other := map[string]any{"title": "another item", "project": "ENG"}
	if createdTaskID(rebased, CreateWorkItemTool, "ENG", other) == task {
		t.Errorf("another create in the rebased attempt names the first one's task %q", task)
	}

	view := map[string]any{"container": "project:ENG", "name": "Board", "type": "board"}
	saved := createdViewID(first, "save_work_view", view)
	if got := createdViewID(rebased, "save_work_view", view); got != saved {
		t.Errorf("the rebased save names view %q, want the first attempt's %q", got, saved)
	}
	if createdViewID(rebased, "save_work_view",
		map[string]any{"container": "project:ENG", "name": "List", "type": "list"}) == saved {
		t.Errorf("another new view in the rebased attempt names the first one's view %q", saved)
	}
}

// A REQUEST'S KEY IS ITS CALL'S, AS SENT, for the tracker as for the knowledge
// base and chat, and every id the request's tracker writes derive from it is
// bound to what the call asks: the same key sent with other arguments is
// another write.
//
// Mutation: derive a request's call key from its work key as a turn's is, and
// it is no longer the key the request sent.
func TestARequestsKeyIsItsCallsAsSent(t *testing.T) {
	t.Parallel()
	key := statelog.NewOpID(time.Now(), "")
	since, _ := statelog.OpMintedAt(key)
	request := Actor{Handle: "ops", Kind: worklet.AuthorOperator, WorkKey: key,
		WorkSince: since}
	args := map[string]any{"item": "ENG-1", "status": "done"}
	if got, page := request.callKey("update_work_item", nil, args, ""),
		callOpKey(nil, "save_page", nil, args, key); got != key || page != key {
		t.Fatalf("the request's call keys are %q (work) and %q (pages), want "+
			"the key it sent, %q", got, page, key)
	}
	op := opIDFor(request, "update_work_item", "update", "t-1", args)
	if at, ok := statelog.OpMintedAt(op); !ok || !at.Equal(since) {
		t.Errorf("the write's operation %q carries %v, want its key's instant %s",
			op, at, since)
	}
	other := map[string]any{"item": "ENG-1", "status": "in_progress"}
	if again := opIDFor(request, "update_work_item", "update", "t-1", other); again == op {
		t.Errorf("the same key sent with other arguments was the first write's "+
			"operation %q", op)
	}
	if again := opIDFor(request, "update_work_item", "update", "t-1", args); again != op {
		t.Errorf("the same request derived %q and then %q, want one operation",
			op, again)
	}
}

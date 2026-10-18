package builtin

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE TOOL IS PART OF EVERY KEY A TURN DERIVES, so two tools called with the
// same arguments at the same count in one turn are two calls.
//
// [callOpKey] is the one derivation every keyed domain's tools share, and the
// arguments' digest names only what was typed: `{"channel": "eng"}` is what a
// join, a leave and a read of one room all say. Without the tool those calls
// derive one key, and what tells their writes apart is left to each store's
// own verb — the derivation's doc promises the key names the call, and this
// is what holds it. Outside a turn the key is the caller's own, whatever the
// tool.
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
	return Actor{Handle: "eng", Kind: worklet.AuthorAgent,
		TurnID: statelog.NewOpID(began.Add(time.Minute), ""), WorkKey: "wk-1",
		WorkSince: began, Calls: turnctx.NewCallLog()}
}

// THE TRACKER'S IDS ARE DERIVED FROM THE CALL'S KEY, so two tools making one
// write are two operations.
//
// A work item's operation and a comment's id were each derived from the
// turn's seed, the verb and the object, the arguments and the call's repeat
// count — a copy of the call-key rule ([Actor.callKey]) that named no TOOL,
// while the count it carried is kept per tool. So two tools each making their
// first call with the same arguments both counted zero, and a write one made
// and a write the other made of the same verb on the same object were one
// operation and one comment: the second answered `applied` from the ledger
// with nothing of it written, or upserted onto the first one's comment row. A
// declaration of a project's tags is made by write_project and, through
// `labels_create_missing`, by every write that files a label.
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
// A turn whose work began further back than the operation ledger remembers is
// REBASED: every operation it derives carries the attempt's own instant
// ([turnctx.Turn.RebasedTo], ADR-0029), so the ledger decides them again
// rather than answer every one `unknown`. A tracker comment's id is not an
// operation but the key of its row, which the apply upserts — so derived as
// the work began, the rebased attempt's comment lands on the row the attempt
// before it wrote, and a remark already posted is not posted a second time
// beside it. Derived from the rebased key, it moved with the operation, and
// every rebased re-run of a turn that had commented said it twice.
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

// A REQUEST'S KEY IS ITS CALL'S, AS SENT, for the tracker as for the knowledge
// base and chat: a request's actor carries the key as its work key with no run
// behind it, and every id its tracker writes derive is derived from that key —
// the one a page or chat write the same request makes is handed — bound to
// what the call asks, so the same key sent with other arguments is another
// write.
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

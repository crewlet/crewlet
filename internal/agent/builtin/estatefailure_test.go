package builtin_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// unservedDetail is what the estate's router says it asked, as it words it:
// node ids and their copies' faults, which belong in the node's log and
// nowhere a model or a person reads.
const unservedDetail = "data-b: out_of_service: applier halted at tracker 2:41; " +
	"data-c: no answer"

// AN ESTATE NOBODY SERVED IS A CONDITION, NOT A FAULT. Every read and write a
// seat or a person makes of the tracker and the knowledge base goes through
// the estate's router, which answers [estate.ErrUnserved] once every data node
// it asked ran nothing — a copy out of service, a peer restarting, a node that
// did not answer. That clears when a data node serves again, so each tool
// answers `unavailable` and invites the retry, in fixed words: the router's
// detail names nodes and their faults. It wrapped no sentinel the rule knew,
// so every one of these was `internal_error` and told the caller that trying
// again could not help.
//
// Mutation: drop the ErrUnserved arm from [builtin.Condition] and every case
// goes red; tell it in the error's own words and the detail check does.
func TestAnEstateNobodyServedIsACondition(t *testing.T) {
	t.Parallel()
	// AS THE ROUTER ANSWERS IT, and as a second hop wraps it: a node
	// answering on another's behalf carries it back inside its own words.
	for name, unserved := range map[string]error{
		"bare":    &estate.ErrUnserved{Detail: unservedDetail},
		"wrapped": fmt.Errorf("estate: tracker.task: %w", &estate.ErrUnserved{Detail: unservedDetail}),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			words, ok := builtin.Condition(unserved)
			if !ok || words == "" {
				t.Fatalf("Condition(%v) = %q, %v — an estate nobody served is a "+
					"condition waiting clears", unserved, words, ok)
			}
			if strings.Contains(words, "data-b") {
				t.Errorf("the condition's words carry the router's detail: %q", words)
			}

			trk := newFakeTracker()
			reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
			trk.readErr = unserved
			assertUnserved(t, "a tracker read", callWork(t, reg,
				builtin.GetWorkItemTool, map[string]any{"item": "ENG-1"}))
			trk.readErr = nil
			trk.writeErr = unserved
			assertUnserved(t, "a tracker write", callWork(t, reg,
				builtin.UpdateWorkItemTool, map[string]any{"item": "ENG-1", "priority": "urgent"}))

			kb := newFakeKB()
			kbReg := kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: kb})
			kb.readErr = unserved
			assertUnserved(t, "a page read", callWork(t, kbReg,
				builtin.GetPageTool, map[string]any{"page": "p1"}))
			kb.readErr = nil
			kb.writeErr = unserved
			assertUnserved(t, "a page write", callWork(t, kbReg,
				builtin.SavePageTool, map[string]any{"page": "p1", "body": "text",
					"base_version": 4}))
		})
	}
}

// assertUnserved holds one tool's answer to an estate nobody served to what a
// condition answers.
func assertUnserved(t *testing.T, what string, got tools.Result) {
	t.Helper()
	if !got.Failed || tools.RefusalOf(got) != tools.RefusalUnavailable {
		t.Fatalf("%s nobody served answered %q (%s), want unavailable", what,
			got.Output, tools.RefusalOf(got))
	}
	if strings.Contains(got.Output, "data-b") || strings.Contains(got.Output, "applier halted") {
		t.Errorf("%s: the router's detail reached the sentence: %q", what, got.Output)
	}
	if !strings.Contains(got.Output, "no data node could serve it") {
		t.Errorf("%s: the sentence %q does not say what stopped it", what, got.Output)
	}
	var unserved *estate.ErrUnserved
	if !errors.As(got.Cause, &unserved) {
		t.Errorf("%s: the router's refusal is not beneath the class: %v", what, got.Cause)
	}
	if hint := statelog.RetryAfter(got.Cause, time.Second); hint != time.Second {
		t.Errorf("%s: a surface would answer it with a Retry-After of %s, want its "+
			"own hint", what, hint)
	}
}

// A PAGE WRITE NOBODY ANSWERED FOR IS AN UNKNOWN, NEVER A FAULT. A page write
// naming no operation of its own — an operator's assistant's, which carries no
// key — is sent to ONE data node, since a second would be a second write; a
// node that never answers may have run it, so the router answers
// [estate.ErrOutcomeUnknown]. That was a fault — `internal_error`, "do not make
// it again … say it could not be made" — which is the one thing it is not.
// Answered as an unknown, it says neither done nor failed, and to read the
// page before writing again; and a save whose RENAME met it says the edit
// landed and the rename may have.
//
// Mutation: drop the arm from pageWriteFailure and the first half goes red;
// drop the UnknownOf branch in save_page's rename and the second does.
func TestAPageWriteNobodyAnsweredForIsAnUnknown(t *testing.T) {
	t.Parallel()
	unanswered := fmt.Errorf("%w (pages.comment, asked of data-b)", estate.ErrOutcomeUnknown)
	operator := func(context.Context, *turnctx.Turn) (doclet.Actor, error) {
		return doclet.Actor{Handle: "founder", Kind: doclet.AuthorOperator,
			OperatorID: "founder"}, nil
	}

	kb := newFakeKB()
	kb.writeErr = unanswered
	got := callNoTurn(t, kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: kb,
		Actor: operator}), builtin.CommentOnPageTool,
		map[string]any{"page": "p1", "body": "a remark"})
	if !got.Failed || !tools.UnknownOf(got) || tools.RefusalOf(got) != tools.RefusalUnavailable {
		t.Fatalf("a page write nobody answered for answered %q (%s, unknown %v), "+
			"want an unknown outcome", got.Output, tools.RefusalOf(got), tools.UnknownOf(got))
	}
	for _, want := range []string{"unknown", "do not report it as done",
		"do not report it as failed", "get_page"} {
		if !strings.Contains(got.Output, want) {
			t.Errorf("the unknown never says %q: %s", want, got.Output)
		}
	}
	for _, never := range []string{"data-b", "fault in this node", "could not be made"} {
		if strings.Contains(got.Output, never) {
			t.Errorf("the unknown says %q: %s", never, got.Output)
		}
	}
	if !errors.Is(got.Cause, estate.ErrOutcomeUnknown) {
		t.Errorf("the router's answer is not beneath the unknown: %v", got.Cause)
	}

	// THE RENAME HALF OF A SAVE, after the edit itself landed.
	kb = newFakeKB()
	kb.renameErr = unanswered
	got = callNoTurn(t, kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: kb,
		Actor: operator}), builtin.SavePageTool,
		map[string]any{"page": "p1", "body": "text", "base_version": 4, "title": "Renamed"})
	if !got.Failed || !tools.UnknownOf(got) {
		t.Fatalf("a rename nobody answered for answered %q (unknown %v), want an "+
			"unknown outcome", got.Output, tools.UnknownOf(got))
	}
	if !strings.Contains(got.Output, "The edit was saved; whether the rename") ||
		strings.Contains(got.Output, "was not:") {
		t.Errorf("the save's answer %q does not say the rename may have landed",
			got.Output)
	}
}

// A WRITE NO DATA NODE ANSWERED IS AN UNKNOWN, NEVER "NOT MADE".
//
// A tracker write carries its caller's operation, so the estate's router asks
// every data node under it before giving up — and when none answered, each that
// stayed silent may have run it, so the router answers
// [estate.ErrOutcomeUnknown]. The mapper fell through to its remainder for it:
// "did not land … The change was NOT made", the one sentence nobody can say,
// and the one a seat acts on by doing it again by other means. One whose
// caller's context ended while it waited carries the deadline too, and the
// condition's "it ran out of time" said the same thing.
//
// So every place a write's error is told says it in the unknown's own fixed
// words, with the retry that settles it for THIS caller — the same call for a
// seat, the op_id for an operator — and never the router's node list: the
// write itself, the dependency write after a filed item, and the label
// declaration before one.
//
// Mutation: drop the arm from writeFailure and the first two cases go red;
// drop toldOf's arm and the dependency case does; drop the arm from
// labelFailure and the declaration case does.
func TestAWriteNoDataNodeAnsweredIsAnUnknown(t *testing.T) {
	t.Parallel()
	for name, unanswered := range map[string]error{
		"never answered": fmt.Errorf("%w (tracker.update_task, asked of data-b, data-c: "+
			"data-b: no answer; data-c: no answer)", estate.ErrOutcomeUnknown),
		"the caller stopped waiting": fmt.Errorf("%w (tracker.update_task, asked of data-b): %w",
			estate.ErrOutcomeUnknown, context.DeadlineExceeded),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			trk := newFakeTracker()
			trk.writeErr = unanswered
			got := callWork(t, workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as}),
				builtin.UpdateWorkItemTool, map[string]any{"item": "ENG-1", "priority": "urgent"})
			assertUnanswered(t, "a seat's write", got)
			if !strings.Contains(got.Output, builtin.UpdateWorkItemTool+
				" again with exactly the same arguments") {
				t.Errorf("a seat's unanswered write is not offered the same call: %s",
					got.Output)
			}

			// AN OPERATOR IS OFFERED ITS op_id, the operation the ledger
			// answers a repeat of with what landed.
			trk = newFakeTracker()
			trk.writeErr = unanswered
			got = callNoTurn(t, retrySurface(t, trk, nil), builtin.UpdateWorkItemTool,
				map[string]any{"item": "ENG-1", "priority": "urgent"})
			assertUnanswered(t, "an operator's write", got)
			var unknown *builtin.UnknownOutcome
			if op := answeredOp(t, got); !errors.As(got.Cause, &unknown) || unknown.OpID != op {
				t.Errorf("the operator is told op_id %q and the cause names %+v", op, unknown)
			}

			// THE DEPENDENCY WRITE AFTER A FILED ITEM: the item is
			// reported, and what stopped its dependencies is told in the
			// unknown's words.
			trk = newFakeTracker()
			trk.dependErr = unanswered
			got = callWork(t, workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as,
				Dependencies: trk.depends}), builtin.CreateWorkItemTool, map[string]any{
				"title": "the follow-up", "project": "ENG", "waiting_on": []any{"ENG-2"}})
			if got.Failed {
				t.Fatalf("a create whose dependencies nobody answered failed: %s", got.Output)
			}
			failure, _ := answerOf(t, got)["dependencies_failed"].(string)
			if !strings.Contains(failure, builtin.Unanswered) {
				t.Errorf("dependencies_failed does not say nobody answered: %q", failure)
			}
			for _, never := range []string{"data-b", "ran out of time"} {
				if strings.Contains(failure, never) {
					t.Errorf("dependencies_failed says %q: %q", never, failure)
				}
			}

			// THE LABEL DECLARATION BEFORE ONE: nothing after it ran, and
			// whether the declaration landed is not known.
			trk = newFakeTracker()
			trk.ensureErr = unanswered
			got = callWork(t, labelSurface(t, trk, false), builtin.CreateWorkItemTool,
				map[string]any{"title": "the regression", "project": "ENG",
					"labels": []any{"regression"}, "labels_create_missing": true})
			if !got.Failed || !tools.UnknownOf(got) {
				t.Fatalf("a declaration nobody answered answered %q (unknown %v), want "+
					"an unknown outcome", got.Output, tools.UnknownOf(got))
			}
			if !strings.Contains(got.Output, "whether that declaration landed is unknown") {
				t.Errorf("the declaration is not said to be unknown: %s", got.Output)
			}
			for _, never := range []string{"Nothing was written", "did not land", "data-b",
				"ran out of time"} {
				if strings.Contains(got.Output, never) {
					t.Errorf("the declaration nobody answered says %q: %s", never, got.Output)
				}
			}
			if len(trk.created) != 0 {
				t.Error("the create carried on past a declaration nobody answered")
			}
		})
	}
}

// assertUnanswered holds one tool's answer to a write no data node answered to
// what an unknown answers.
func assertUnanswered(t *testing.T, what string, got tools.Result) {
	t.Helper()
	if !got.Failed || !tools.UnknownOf(got) || tools.RefusalOf(got) != tools.RefusalUnavailable {
		t.Fatalf("%s nobody answered answered %q (%s, unknown %v), want an unknown "+
			"outcome", what, got.Output, tools.RefusalOf(got), tools.UnknownOf(got))
	}
	for _, want := range []string{builtin.Unanswered, "do not report it as done",
		"do not report it as failed"} {
		if !strings.Contains(got.Output, want) {
			t.Errorf("%s: the unknown never says %q: %s", what, want, got.Output)
		}
	}
	for _, never := range []string{"NOT made", "data-b", "ran out of time",
		"fault in this node"} {
		if strings.Contains(got.Output, never) {
			t.Errorf("%s: the unknown says %q: %s", what, never, got.Output)
		}
	}
	if !errors.Is(got.Cause, estate.ErrOutcomeUnknown) {
		t.Errorf("%s: the router's answer is not beneath the unknown: %v", what, got.Cause)
	}
}

// A KEYED PAGE WRITE NOBODY ANSWERED IS OFFERED ITS OWN REPEAT.
//
// A turn's comment carries its key, so the router asks every data node before
// it answers [estate.ErrOutcomeUnknown] — and the same call again is the same
// comment, the safe retry. The answer told every caller that writing it again
// makes it twice, which is true only of a caller with no key (an operator's
// assistant over MCP), whose answer still says to read first.
//
// Mutation: pass the comment tool's unkeyed sentence to pageWriteFailure and
// the turn's case goes red.
func TestAKeyedCommentNobodyAnsweredIsOfferedTheSameCall(t *testing.T) {
	t.Parallel()
	kb := newFakeKB()
	kb.writeErr = fmt.Errorf("%w (pages.comment, asked of data-b, data-c)",
		estate.ErrOutcomeUnknown)
	got := callWork(t, kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: kb}),
		builtin.CommentOnPageTool, map[string]any{"page": "p1", "body": "a remark"})
	if !got.Failed || !tools.UnknownOf(got) {
		t.Fatalf("a comment nobody answered answered %q, want unknown", got.Output)
	}
	if !strings.Contains(got.Output, builtin.CommentOnPageTool+
		" again with exactly the same arguments") {
		t.Errorf("a turn's comment nobody answered is not offered the same call: %s",
			got.Output)
	}
	if strings.Contains(got.Output, "makes it twice") ||
		strings.Contains(got.Output, "a second call is a new operation") {
		t.Errorf("a turn's comment nobody answered is warned its repeat duplicates, "+
			"which for a turn it does not: %s", got.Output)
	}

	// THE UNKEYED CALLER — an operator's assistant — reads first, since its
	// repeat IS a second comment.
	operator := func(context.Context, *turnctx.Turn) (doclet.Actor, error) {
		return doclet.Actor{Handle: "founder", Kind: doclet.AuthorOperator,
			OperatorID: "founder"}, nil
	}
	got = callNoTurn(t, kbRegistry(t, builtin.PageDeps{Reader: kb, Writer: kb,
		Actor: operator}), builtin.CommentOnPageTool,
		map[string]any{"page": "p1", "body": "a remark"})
	if !strings.Contains(got.Output, "Read the page's comments with get_page before "+
		"making it again") {
		t.Errorf("an unkeyed comment nobody answered is not told to look first: %s",
			got.Output)
	}
}

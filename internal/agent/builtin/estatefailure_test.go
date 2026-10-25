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

package builtin

import (
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/tools"
)

// Each of these is a non-nil value of its interface that is never called: the
// catalogues decide what to build from whether a dependency is there at all.
type (
	anyReader   struct{ WorkReader }
	anySearcher struct{ WorkSearcher }
	anyInbox    struct{ InboxReader }
)

// everyWriteWired is a WorkDeps under which every catalogue builds every
// tracker tool it has.
func everyWriteWired() WorkDeps {
	return WorkDeps{
		Reader: anyReader{}, Search: anySearcher{}, Inbox: anyInbox{},
		Writer:          func(Actor) WorkWriter { return nil },
		Dependencies:    func(Actor) WorkDepender { return nil },
		Merges:          func(Actor) WorkMerger { return nil },
		ViewWriter:      func(Actor) ViewWriter { return nil },
		GoalWriter:      func(Actor) GoalWriter { return nil },
		CatalogueWriter: func(Actor) CatalogueWriter { return nil },
		PersonWriter:    func(Actor) PersonWriter { return nil },
		TrashWriter:     func(Actor) TrashWriter { return nil },
		ProjectWriter:   func(Actor) ProjectWriter { return nil },
	}
}

// THE ANSWERS A WRITE IS NAMED AFTER ARE EXACTLY THE SEQUENCED TOOLS' ANSWERS.
//
// [operationsBefore] reads `operations` off the answers of the tools in
// [sequencedTools] and nobody else's, and a surface runs a tool one at a time
// only when it implements [tools.Sequenced]. A write tool missing from the
// list is one whose receipts are never read, so a second write after it is
// named as though it had not happened and collapsed into it; a write tool
// that is listed and not Sequenced can run beside another write and be handed
// the same earlier calls.
//
// Mutation: drop a name from the list, or the Sequenced method from one of
// the tools, and this fails.
func TestTheSequencedListIsEveryToolThatNamesItsWrites(t *testing.T) {
	t.Parallel()
	reg := tools.NewRegistry()
	if _, err := Register(reg, Deps{Work: everyWriteWired()}); err != nil {
		t.Fatalf("register: %v", err)
	}
	built := map[string]tools.Callable{}
	for _, e := range reg.Snapshot().Entries() {
		built[e.Name()] = e.Tool
	}
	for _, tool := range OperatorTools(OperatorDeps{Work: everyWriteWired()}) {
		built[tool.Name()] = tool
	}

	var sequenced []string
	for name, tool := range built {
		if _, ok := tool.(tools.Sequenced); ok {
			sequenced = append(sequenced, name)
		}
	}
	listed := slices.Clone(sequencedTools)
	slices.Sort(sequenced)
	slices.Sort(listed)
	if !slices.Equal(sequenced, listed) {
		t.Errorf("the catalogues build Sequenced tools %v and the list names "+
			"%v — every write tool is both, or its receipts go unread", sequenced, listed)
	}
}

// receiptOf is a call to name whose answer says it wrote under ops.
func receiptOf(name string, ops ...string) ledger.Call {
	answer, _ := receipt(map[string]any{"outcome": "applied"}, ops...)
	return ledger.Call{Name: name, Result: answer.Output}
}

// A WRITE IS NAMED AFTER THE WRITES BEFORE IT UNDER ITS BASE, AND ONLY THOSE.
//
// The first under a base is the base itself — the name a build that does not
// count gives it too, which is what keeps a redelivery across a rolling
// upgrade from landing it twice — and each later one the first `#n` no earlier
// write holds. A receipt under another base, and an answer that is not a
// receipt of this surface's, name nothing.
//
// Mutation: name every write its base, or count an MCP tool's answer, and
// this fails.
func TestAWriteIsNamedAfterTheWritesBeforeItUnderItsBase(t *testing.T) {
	t.Parallel()
	actor := Actor{Handle: "eng", TurnID: "run-1", WorkKey: "wk-1"}
	base := "wk-1-update-i1"
	for _, tc := range []struct {
		name    string
		earlier []ledger.Call
		want    string
	}{
		{"the first", nil, base},
		{"after one", []ledger.Call{receiptOf(UpdateWorkItemTool, base)}, base + "#2"},
		{"after two", []ledger.Call{
			receiptOf(UpdateWorkItemTool, base),
			receiptOf(CommentOnWorkTool, "wk-1-comment-x"),
			receiptOf(UpdateWorkItemTool, "wk-1-labels-ENG", base+"#2"),
		}, base + "#3"},
		{"after another object's", []ledger.Call{
			receiptOf(UpdateWorkItemTool, "wk-1-update-i2"),
		}, base},
		{"after another verb on it", []ledger.Call{
			receiptOf(CreateWorkItemTool, "wk-1-depend-i1"),
		}, base},
		// An MCP server's answer is whatever that server says.
		{"after a foreign answer", []ledger.Call{
			receiptOf("jira_update_issue", base),
		}, base},
		// A refusal made before anything was named is prose.
		{"after a refusal", []ledger.Call{
			{Name: UpdateWorkItemTool, Result: "update_work_item needs an `item`", Failed: true},
		}, base},
		// A refusal made after a write was named counts: its write may
		// have landed on an earlier attempt.
		{"after a named refusal", []ledger.Call{
			{Name: UpdateWorkItemTool, Result: refusedAfter("did not land", base).Output, Failed: true},
		}, base + "#2"},
	} {
		turn := (&turnctx.Turn{RunID: "run-1", WorkKey: "wk-1"}).WithEarlier(tc.earlier)
		if got := opIDFor(actor, operationsBefore(turn), "update", "i1"); got != tc.want {
			t.Errorf("%s: named %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A CREATE FILES A TASK NAMED BY ITS PLACE AMONG THE TURN'S CREATES, so a
// redelivery files the same task and a second create in one turn a second one.
//
// Mutation: draw the id at random in a turn, or leave the author out of it,
// and this fails.
func TestACreatesTaskIsDerivedFromItsPlaceAmongTheTurnsCreates(t *testing.T) {
	t.Parallel()
	actor := Actor{Handle: "eng", TurnID: "run-1", WorkKey: "wk-1"}
	firstID, firstOp := createFor(actor, operations{})
	againID, againOp := createFor(Actor{Handle: "eng", TurnID: "run-2", WorkKey: "wk-1"}, operations{})
	if firstID != againID || firstOp != againOp {
		t.Errorf("a redelivery files %s under %s where its first attempt filed "+
			"%s under %s — the same work filed twice", againID, againOp, firstID, firstOp)
	}
	if parsed, err := uuid.Parse(firstID); err != nil || parsed.Version() != 5 {
		t.Errorf("the task id %q is not a name-derived uuid (%v)", firstID, err)
	}
	if want := "wk-1-create-" + firstID; firstOp != want {
		t.Errorf("the create is named %q, want %q", firstOp, want)
	}
	secondID, secondOp := createFor(actor, operations{firstOp: true})
	if secondID == firstID || secondOp == firstOp {
		t.Errorf("a second create in the turn files %s under %s, the first's", secondID, secondOp)
	}
	// ANOTHER SEAT'S CREATE IN THE SAME WORK is another task: a work key is
	// derived from events alone, and nothing makes an event one seat's.
	if other, _ := createFor(Actor{Handle: "pm", TurnID: "run-9", WorkKey: "wk-1"}, operations{}); other == firstID {
		t.Errorf("two seats woken by the same events file the same task %s", other)
	}
	// OUTSIDE A TURN every create is its own.
	a, _ := createFor(Actor{Handle: "ops"}, operations{})
	b, _ := createFor(Actor{Handle: "ops"}, operations{})
	if a == b {
		t.Errorf("two operator creates filed one task %s", a)
	}
}

// A TURN'S FIRST COMMENT ON A TASK IS NAMED AS A BUILD THAT DOES NOT COUNT
// NAMES IT, and each later one is its own comment.
//
// The id is a primary key on every node and durable in every comment row, and
// a redelivery can cross builds: a first comment named differently by the two
// would post twice.
//
// Mutation: fold the comment's place into the first comment's id, or name
// every comment the first's, and this fails.
func TestATurnsCommentsAreNamedByTheirPlace(t *testing.T) {
	t.Parallel()
	actor := Actor{Handle: "eng", TurnID: "run-1", WorkKey: "wk-1"}
	id, op := commentFor(actor, operations{}, "i1")
	older := uuid.NewSHA1(commentNamespace, []byte("wk-1\x00i1\x00eng")).String()
	if id != older || op != "wk-1-comment-"+older {
		t.Errorf("the first comment is %s under %s, want %s under wk-1-comment-%s",
			id, op, older, older)
	}
	second, secondOp := commentFor(actor, operations{op: true}, "i1")
	if second == id || secondOp != op+"#2" {
		t.Errorf("the second comment is %s under %s, want a new id under %s#2",
			second, secondOp, op)
	}
	if again, againOp := commentFor(actor, operations{op: true}, "i1"); again != second || againOp != secondOp {
		t.Errorf("the second comment re-derives as %s under %s, not %s under %s",
			again, againOp, second, secondOp)
	}
}

// A PERSON'S RECORD IS WRITTEN UNDER ITS PREFIX AND THE TURN, counted the same
// way, and fresh outside a turn — the first in a turn is `prefix + seed`, what
// a build that does not count names every one.
func TestAPersonWriteIsCountedLikeEveryOther(t *testing.T) {
	t.Parallel()
	actor := Actor{Handle: "eng", TurnID: "run-1", WorkKey: "wk-1"}
	first := callKey(actor, operations{}, "pins-eng-")
	second := callKey(actor, operations{first: true}, "pins-eng-")
	if first != "pins-eng-wk-1" || second != "pins-eng-wk-1#2" {
		t.Errorf("two pin writes in one turn are %q and %q, want pins-eng-wk-1 "+
			"and pins-eng-wk-1#2", first, second)
	}
	a := callKey(Actor{Handle: "ops"}, operations{}, "pins-ops-")
	b := callKey(Actor{Handle: "ops"}, operations{}, "pins-ops-")
	if a == b || !strings.HasPrefix(a, "pins-ops-operator-") {
		t.Errorf("two operator pin writes are %q and %q, want two fresh names "+
			"under pins-ops-operator-", a, b)
	}
}

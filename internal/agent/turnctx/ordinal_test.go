package turnctx_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
)

// The names one id space shares, as a caller passes them: native chat's three
// posting tools all derive a message id from (turn, channel, ordinal).
var posting = []string{"post_message", "reply_in_thread", "send_dm"}

// THE ORDINAL NUMBERS THE GESTURE, NOT THE TOOL.
//
// A turn that posts in a room and then answers a thread has said two things,
// and both ids come out of one space — so the reply must be call one rather
// than a second call zero. Counting per tool name would derive one id twice,
// the applier's insert would decline the second, and the turn would believe it
// said two things.
func TestTheOrdinalCountsEveryToolThatSharesAnIDSpace(t *testing.T) {
	t.Parallel()
	turn := &turnctx.Turn{RunID: "run-1", Calls: []turnctx.Call{
		{Name: "read_channel"},
		{Name: "post_message"},
		{Name: "search_messages"},
		{Name: "send_dm"},
	}}
	if got := turn.CallOrdinal(posting...); got != 2 {
		t.Errorf("the next posting call is numbered %d, want 2", got)
	}
	// AND NOTHING ELSE COUNTS. A turn that read the room four times has
	// still said nothing, and numbering its first message four would make
	// the id depend on how much recon the model happened to do — which a
	// re-run reproduces only by making the same calls in the same order.
	reader := &turnctx.Turn{Calls: []turnctx.Call{
		{Name: "read_channel"}, {Name: "read_channel"},
		{Name: "list_channels"}, {Name: "search_messages"},
	}}
	if got := reader.CallOrdinal(posting...); got != 0 {
		t.Errorf("a turn that only read numbered its first message %d, want 0", got)
	}
}

// A FAILED CALL WROTE NOTHING, so it consumes no number.
//
// A model whose message was refused — a body over the cap, a room it named
// wrongly — fixes it and calls again, and that retry is still the turn's FIRST
// message. Counting the refusal would make the ids depend on how many times
// the model got it wrong.
func TestAFailedCallDoesNotAdvanceTheOrdinal(t *testing.T) {
	t.Parallel()
	turn := &turnctx.Turn{Calls: []turnctx.Call{
		{Name: "post_message", Failed: true},
		{Name: "post_message", Failed: true},
	}}
	if got := turn.CallOrdinal(posting...); got != 0 {
		t.Errorf("two refusals advanced the ordinal to %d, want 0", got)
	}
	turn.Calls = append(turn.Calls, turnctx.Call{Name: "post_message"})
	if got := turn.CallOrdinal(posting...); got != 1 {
		t.Errorf("a message that landed did not advance the ordinal: got %d, want 1", got)
	}
}

// A RESUMED TURN DOES NOT RESTART THE ORDINAL.
//
// A detached run parks mid-loop and is re-entered later, possibly in another
// process — so the calls it made before it suspended live on its own row
// rather than in this process's surface. Composed oldest-first with what the
// live surface has run, the numbering continues; without the parked half the
// first call after a resume would be numbered zero again and would derive the
// id of a message the turn had already posted.
func TestAResumedTurnDoesNotRestartTheOrdinal(t *testing.T) {
	t.Parallel()
	parked := []turnctx.Call{{Name: "post_message"}, {Name: "run_sandbox"}}
	live := []turnctx.Call{{Name: "read_channel"}, {Name: "reply_in_thread"}}

	resumed := (&turnctx.Turn{RunID: "run-1"}).WithCalls(slices.Concat(parked, live))
	if got := resumed.CallOrdinal(posting...); got != 2 {
		t.Errorf("the resumed turn's next message is numbered %d, want 2 — it "+
			"would derive the id of one it already posted", got)
	}
	// The half that makes it wrong, stated so the failure is recognisable:
	// the live surface alone knows nothing about what was said before the
	// run parked.
	if got := (&turnctx.Turn{}).WithCalls(live).CallOrdinal(posting...); got != 1 {
		t.Fatalf("the live half alone numbers %d; this case is not testing what "+
			"it says it tests", got)
	}
}

// THE RECORD IS A VALUE, AND A DERIVED TURN IS A NEW TURN.
//
// A Turn is documented immutable so a goroutine that captured one cannot
// observe a turn moving underneath it. The loop appends to its own record
// after handing a call over, so sharing that slice would let a call already in
// flight see calls made after it — which is the same class of bug as the
// closure this design rejected, reintroduced through a backing array.
func TestDerivingACallsTurnCopiesBothTheTurnAndTheRecord(t *testing.T) {
	t.Parallel()
	turn := &turnctx.Turn{RunID: "run-1", WorkKey: "wk-1"}
	// THE LOOP'S OWN RECORD, with room to grow, which is what a slice it
	// keeps appending to looks like.
	record := make([]turnctx.Call, 1, 4)
	record[0] = turnctx.Call{Name: "post_message"}

	derived := turn.WithCalls(record)
	if len(turn.Calls) != 0 {
		t.Error("deriving rewrote the turn it was derived from")
	}
	if derived.RunID != "run-1" || derived.WorkKey != "wk-1" {
		t.Errorf("the derived turn lost its identity: %+v", derived)
	}

	// The loop rewrites the row it already handed over — an outcome
	// arriving late, a row re-filled in place. What a call was given must
	// not move underneath it.
	record[0] = turnctx.Call{Name: "post_message", Failed: true}
	record = append(record, turnctx.Call{Name: "send_dm"})
	if got := derived.CallOrdinal(posting...); got != 1 {
		t.Errorf("a call in flight saw the loop's later writes: ordinal %d, "+
			"want 1 — the record was shared rather than copied", got)
	}
	if len(record) != 2 {
		t.Fatal("this case is not exercising the shared backing array")
	}
}

// NO NAMES IS NO ORDINAL, and a nil turn answers rather than panicking: a tool
// surface built outside a turn — a validate run, a test driving a runner
// directly — legitimately has none, and the tools that need one refuse
// elsewhere on their own terms.
func TestTheOrdinalIsSafeOutsideATurn(t *testing.T) {
	t.Parallel()
	var none *turnctx.Turn
	if got := none.CallOrdinal(posting...); got != 0 {
		t.Errorf("a turnless call numbered %d, want 0", got)
	}
	if none.WithCalls([]turnctx.Call{{Name: "post_message"}}) != nil {
		t.Error("deriving from no turn invented one")
	}
	turn := &turnctx.Turn{Calls: []turnctx.Call{{Name: "post_message"}}}
	if got := turn.CallOrdinal(); got != 0 {
		t.Errorf("an ordinal over no names counted %d", got)
	}
}

// A SUB-AGENT'S RECORD IS ITS OWN. Parent and child share a run id, so a child
// numbering from the parent's record would place its first call after work it
// did not do — and two loops numbering from separate records collide on any id
// derived from (run, ordinal). What makes the second safe is that every tool
// so derived writes to a shared surface, which the sub-agent guard denies a
// worker outright.
func TestASubagentDoesNotInheritTheCallRecord(t *testing.T) {
	t.Parallel()
	parent := &turnctx.Turn{
		RunID: "run-1", WorkKey: "wk-1",
		Calls: []turnctx.Call{{Name: "post_message"}, {Name: "send_dm"}},
	}
	child, err := parent.ForSubagent(nil, 4)
	if err != nil {
		t.Fatalf("ForSubagent: %v", err)
	}
	if got := child.CallOrdinal(posting...); got != 0 {
		t.Errorf("the child's first call is numbered %d, want 0", got)
	}
}

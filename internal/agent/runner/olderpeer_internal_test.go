package runner

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// olderLeadKinds is sandbox.LeadKinds as cf65c6ae22 ships it, the last build
// before the engine's own answers kept leads of their own kinds: the older
// peer a rolling upgrade mixes with this build. PINNED, never read off
// sandbox.LeadKinds, which is this build's.
var olderLeadKinds = []sandbox.LeadKind{"answer", "review", "task", "fresh"}

// olderTells is what that build tells of kept as its brief tells it again:
// its LeadSeal.Open refuses a kind outside its list (ErrLeadKindUnknown), which
// its leadAgain tells as prompts.LeadUnread; a lead it opens is told by its
// kind, an answer as a person's answer to the run's question.
func olderTells(t *testing.T, seal *sandbox.LeadSeal, kept sandbox.KeptLead) string {
	t.Helper()
	if !slices.Contains(olderLeadKinds, kept.Kind) {
		return "unreadable"
	}
	lead, err := seal.Open("t1", kept)
	if err != nil {
		return "unreadable"
	}
	return string(lead.Kind)
}

// AN OLDER PEER NEVER TELLS THE ENGINE'S ANSWER AS A PERSON'S. In a rolling
// upgrade, a node on the build before this one may take a seat over and tell a
// run's leads again. The lead of a run whose parked calls returned, and of a
// `wait_for_run` the engine answered, carry keys that build drops; sealed as an
// answer, it would tell them as "the person it asked answered". Each is kept
// under a kind that build does not know, so it is told as a message it cannot
// read (compatibility.md's forward mechanism for a kind a later build adds),
// while a person's answer, with calls parked beside it, stays one it reads.
//
// Mutation: keep every lead as sandbox.LeadKindAnswer in Asked.lead, or name
// either new kind "answer", and the older peer tells it as a person's answer.
func TestAnOlderPeerNeverTellsTheEnginesAnswerAsAPersons(t *testing.T) {
	t.Parallel()
	seal, err := sandbox.NewLeadSeal(recordseal.OneKey("leads-k1", "the runner rig's fleet key material"))
	if err != nil {
		t.Fatal(err)
	}
	called := []types.CallReturned{{Tool: "delegate", Seq: 4}}
	for name, tc := range map[string]struct {
		asked Asked
		told  string
	}{
		"its calls returned": {asked: Asked{Question: "Waiting for its call to return: delegate",
			Answer: "Your call to delegate returned.", Called: called, CallsReturned: true}, told: "unreadable"},
		"a wait answered": {asked: Asked{Question: "Waiting for another run", Answer: "It left its seat.",
			Waited: &types.RunWaited{Turn: "run-t", Seat: "swe", Left: types.RunLeftEnded,
				Pushed: types.RunPushedNone}}, told: "unreadable"},
		"a person's answer beside its calls": {asked: Asked{Question: "Which branch?", Answer: "Use main.",
			Called: called}, told: "answer"},
	} {
		lead := tc.asked.lead()
		lead.Job, lead.Seq, lead.Round = "job-1", 1, 1
		kept, err := seal.Seal("t1", *lead)
		if err != nil {
			t.Fatalf("%s: Seal: %v", name, err)
		}
		if told := olderTells(t, seal, kept); told != tc.told {
			t.Errorf("%s: the older peer tells the lead as %q, want %q", name, told, tc.told)
		}
	}
}

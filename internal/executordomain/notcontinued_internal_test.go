package executordomain

import (
	"testing"

	"github.com/crewlet/crewlet/internal/executorkeep"
)

// AN END THAT SAYS WHY ITS SESSION WAS LOST IS STAMPED FOR THE BUILD THAT READS
// IT: an end naming its unkept capture's loss or its runs' verdicts is stamped
// at notContinuedVersion, so a build reading less retains it rather than apply
// it without them and say a session was lost with no reason; an end naming
// neither is stamped below it.
//
// Mutation: drop either versioned field's row, and its case goes red.
func TestAnEndsVerdictsAreStampedForTheBuildThatReadsThem(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	turn := newTurn()
	subject := ConversationSubject(servedSeat, servedChat, turn)
	stampOf := func(end executorkeep.Ending) int {
		env, err := DecodeEnvelope(e.encode(subject, OpEnd, seatScope(servedSeat),
			ending(servedSeat, servedChat, turn, "", end)))
		if err != nil {
			t.Fatalf("decode the end: %v", err)
		}
		return env.V
	}
	for name, c := range map[string]struct {
		ending executorkeep.Ending
		want   int
	}{
		"an unkept capture's loss": {executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted,
			Unkept: &executorkeep.Unkept{LaunchSeq: 1, CaptureSeq: 1, Loss: executorkeep.LossTooLarge,
				Reason: "past its bound"}}, notContinuedVersion},
		"its runs' verdicts": {executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted,
			NotContinued: []executorkeep.NotContinued{{Loss: executorkeep.LossTooLarge,
				Instead: executorkeep.InsteadFresh}}}, notContinuedVersion},
	} {
		if got := stampOf(c.ending); got != c.want {
			t.Errorf("an end naming %s is stamped %d, want %d", name, got, c.want)
		}
	}
	if got := stampOf(completedEnding); got >= notContinuedVersion {
		t.Errorf("an end naming neither is stamped %d, which a build reading %d retains", got,
			notContinuedVersion-1)
	}
}

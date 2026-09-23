package learning

import (
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
)

// The parties past the bound are COUNTED, once each however often they spoke,
// and the line counting them names the turn whose event still holds what they
// said.
func TestThePartiesPastTheBoundAreCountedUnderTheirTurn(t *testing.T) {
	t.Parallel()
	p := &Profiler{maxSubjects: maxProfiledSubjects}
	var spoke []types.InboundInteraction
	speaker := func(i int) types.InboundInteraction {
		return types.InboundInteraction{Sender: types.CanonicalIdentity{
			ExternalID: fmt.Sprintf("u-%02d", i), Platform: "mattermost",
		}, Body: "a line"}
	}
	for i := range maxProfiledSubjects + 3 {
		spoke = append(spoke, speaker(i))
	}
	// A party past the bound speaking twice is still one party left out.
	spoke = append(spoke, speaker(maxProfiledSubjects))
	turn := Turn{Event: types.TurnCompleted{AgentHandle: "dev", TurnID: "run-1", Interactions: spoke}}

	profiled, unprofiled := p.subjectsOf(turn)
	if len(profiled) != maxProfiledSubjects || unprofiled != 3 {
		t.Fatalf("profiled %d and left out %d, want %d and 3",
			len(profiled), unprofiled, maxProfiledSubjects)
	}

	fields := map[string]any{}
	line := unprofiledLogFields(turn, len(profiled), unprofiled)
	for i := 0; i+1 < len(line); i += 2 {
		fields[line[i].(string)] = line[i+1]
	}
	if fields["turn_id"] != "run-1" || fields["not_profiled"] != 3 ||
		fields["profiled"] != maxProfiledSubjects || fields["max"] != maxProfiledSubjects {
		t.Errorf("line = %v, want the turn and both counts against the bound", line)
	}

	// Under the bound nothing is left out, and nothing is counted.
	if _, none := p.subjectsOf(Turn{Event: types.TurnCompleted{Interactions: spoke[:2]}}); none != 0 {
		t.Errorf("left out %d of two speakers", none)
	}
}

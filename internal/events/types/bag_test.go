package types_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/jsoncarry"
)

// AN INTEGER PAST 2^53 IN A TURN'S A2A CONTEXT OR ITS ROUND IN FLIGHT REACHES
// THE NODE THAT DECODES THE EVENT AS ITS WRITER'S DIGITS, and leaves that node
// the same way when it publishes the event again — as a json.Number, the type a
// reader of a number in a bag asserts.
//
// Mutation: declare either bag a plain map[string]any and its number comes
// back a float64, re-published rounded.
func TestANumberInATurnsBagsCrossesTheBrokerExactly(t *testing.T) {
	t.Parallel()
	const big = "9007199254740993"
	for name, payload := range map[string]events.Payload{
		"completed a2a context": types.AgentTurnCompleted{
			A2AContext: jsoncarry.Bag{"channel_id": "a2a-1", "n": json.Number(big)},
		},
		"progress a2a context": types.AgentTurnProgress{
			A2AContext: jsoncarry.Bag{"channel_id": "a2a-1", "n": json.Number(big)},
		},
		"progress partial round": types.AgentTurnProgress{
			PartialRound: jsoncarry.Bag{"round": json.Number(big), "content": "half"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			raw, err := json.Marshal(events.NewFrom(payload, events.TraceContext{}))
			if err != nil {
				t.Fatalf("encode the event: %v", err)
			}
			var back events.Event
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Fatalf("decode the event: %v", err)
			}
			var bag jsoncarry.Bag
			switch data := back.Data.(type) {
			case *types.AgentTurnCompleted:
				bag = data.A2AContext
			case *types.AgentTurnProgress:
				bag = data.A2AContext
				if strings.HasSuffix(name, "partial round") {
					bag = data.PartialRound
				}
			default:
				t.Fatalf("the body decoded as %T", back.Data)
			}
			var number any
			for _, key := range []string{"n", "round"} {
				if v, ok := bag[key]; ok {
					number = v
				}
			}
			if n, ok := number.(json.Number); !ok || n.String() != big {
				t.Errorf("the number decoded as %#v, want the json.Number %s", number, big)
			}
			again, err := json.Marshal(&back)
			if err != nil {
				t.Fatalf("re-publish the event: %v", err)
			}
			if !strings.Contains(string(again), ":"+big) {
				t.Errorf("the event re-published lost %s: %s", big, again)
			}
		})
	}
}

package engine

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/node"
)

// sandboxDefaultCapDoc is a company whose seat names no run cap.
const sandboxDefaultCapDoc = `
name: Nimbus
providers:
  llm:
    zulu:
      model: claude-sonnet-5
  sandbox:
    fake: true
roles:
  - name: SWE
    handle: swe
    llm: zulu
`

// A SEAT THAT NAMES NO RUN CAP HOLDS TWO AT ONCE, ONE PER CONVERSATION, on both
// counts that admit them (ADR-0169): the coordinator's cap of the seat's runs
// and the node's admission of its turns read the role's default, so two
// conversations are admitted side by side and a second turn of one is refused.
//
// Mutation: read the default as one at either wiring, or leave
// org.DefaultMaxConcurrentRuns at one, and this goes red.
func TestAnUnsetSeatAdmitsTwoRunsOfDifferentConversations(t *testing.T) {
	t.Parallel()
	e := sandboxNode(t, nil)
	applyOK(t, e, sandboxDefaultCapDoc)
	waitHeld(t, e, "swe")
	if got := e.sandbox.Load().coordinator.SeatCount("swe").Cap; got != 2 {
		t.Fatalf("the coordinator caps the unset seat at %d runs, want the default of two", got)
	}
	ctx := t.Context()
	for _, conversation := range []string{"a", "b"} {
		_, release, err := e.node.AdmitTurn(ctx, "swe", conversation)
		if err != nil {
			t.Fatalf("a turn of conversation %s on an unset seat = %v, want admitted beside the other", conversation, err)
		}
		t.Cleanup(release)
	}
	if _, again, err := e.node.AdmitTurn(ctx, "swe", "a"); !errors.Is(err, node.ErrConversationBusy) {
		if again != nil {
			again()
		}
		t.Fatalf("a second turn of conversation a = %v, want ErrConversationBusy", err)
	}
}

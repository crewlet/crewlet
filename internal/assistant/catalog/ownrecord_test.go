package catalog_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/authz"
)

// A CALL THAT CHANGES ONLY THE PERSON'S OWN RECORD SAYS SO, and one that
// changes the company's work does not: the card reads "Only for you" for the
// first, never "For the company". Mutation: drop Own from the preview, or set
// it for every call, and this is red.
func TestACallOnThePersonsOwnRecordIsTheirsAlone(t *testing.T) {
	t.Parallel()
	own, react := string(authz.ActionAssistantInstructionsSave), string(authz.ActionChatReact)
	_, _, change := generic(t, admin, own, react)
	for tool, args := range map[string]map[string]any{
		own:   {"instructions": map[string]any{"length": "brief"}, "if_match": 0},
		react: {"channel": "c1", "message_id": "m1", "emoji": "tada"},
	} {
		p, err := change.Preview(t.Context(), resource("/nowhere", "", nil), wednesday,
			map[string]any{"route": catalog.ActRoute, "path": map[string]any{"tool": tool},
				"body": map[string]any{"args": args}})
		if err != nil {
			t.Fatal(err)
		}
		if want := tool == own; p.Own != want {
			t.Errorf("%s's call is the person's own record: %v, want %v", tool, p.Own, want)
		}
	}
}

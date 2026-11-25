package toolname_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/toolname/toolnametest"
)

// EVERY TOOL THE ENGINE OFFERS IS ONE ITS OWN SERVER LISTS: a name no other
// server can spell in a harness joining server and tool with `__` or `-`
// ([harnessprofile.BridgeToolName]). The bridge's own server lists any other
// name under a hashed alias (internal/api/mcpbridge), so a built-in outside
// the grammar would reach every box under a name no prompt or skill uses.
//
// Mutation: declare a tool `ask__and_wait` here, or `_ask`, and this is red.
func TestEveryToolTheEngineOffersIsOneItsOwnServerLists(t *testing.T) {
	t.Parallel()
	names := toolnametest.Names(t)
	if len(names) == 0 {
		t.Fatal("no tool name was read, so the grammar was held to nothing")
	}
	for _, name := range names {
		if !harnessprofile.BridgeToolName(name) {
			t.Errorf("the engine offers %q, a name the bridge's own server does not list: letters and digits "+
				"joined by single underscores only", name)
		}
	}
}

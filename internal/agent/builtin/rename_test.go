package builtin_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
)

// AN AGENT REACHES A RENAMED COLLEAGUE BY THE NAME IT REMEMBERS.
//
// End to end through the tool, because the corpus is where the two halves
// meet: the resolver grew a tier for retired addresses and it answers nothing
// unless this walk puts them in. A model addresses whoever its ledger, the
// page it read and the channel topic call that seat, and none of those is
// rewritten by a rename — so without this the tool answered "no such
// colleague" over a seat sitting right there, and the ask never happened.
func TestALookupFindsAColleagueByTheHandleItUsedToHave(t *testing.T) {
	t.Parallel()
	o := organization(t)
	renamed := o.AgentSeatByHandle("agent-cto")
	if renamed == nil {
		t.Fatal("no agent-cto in the fixture")
	}
	// A HANDLE NOTHING ELSE IN THIS COMPANY COMES NEAR, so the case cannot
	// pass through the fuzzy tier: the control below asserts the same query
	// finds nobody when no seat has ever answered to it.
	renamed.FormerHandles = []string{"platform-lead"}

	tool := registered(t, builtin.Deps{}, builtin.LookupColleagueTool)
	turn := &turnctx.Turn{RunID: "run-1", WorkKey: "wk-1",
		Seat: o.AgentSeatByHandle("agent-ceo"), Org: o}

	res := callFor(t, tool, turn, map[string]any{"query": "platform-lead"})
	if res.Failed {
		t.Fatalf("looking a colleague up by the handle it used to have failed:\n%s",
			res.Output)
	}
	if !strings.Contains(res.Output, "agent-cto") {
		t.Errorf("the answer does not name the seat that holds that handle "+
			"today:\n%s", res.Output)
	}
}

// AND A HANDLE NOBODY HAS EVER HELD STILL FINDS NOBODY.
//
// The control. Without it the case above passes on a corpus that matched
// anything, and "no such colleague" is the answer that keeps an agent from
// addressing the wrong one.
func TestALookupOnAHandleNobodyHasEverHeldFindsNobody(t *testing.T) {
	t.Parallel()
	tool := registered(t, builtin.Deps{}, builtin.LookupColleagueTool)
	res := callFor(t, tool, turnFor(t, "agent-ceo"),
		map[string]any{"query": "platform-lead"})
	if !res.Failed {
		t.Fatalf("a handle no seat has ever answered to resolved to somebody:\n%s",
			res.Output)
	}
}

package phase_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
)

func TestDeliveredIsNamePreciseWhenThePlanNamedRealTools(t *testing.T) {
	t.Parallel()
	d := phase.Delivery{
		Called:          []string{"lookup_colleague", "slack_post"},
		PlannedResolved: []string{"slack_post"},
		MCPTools:        []string{"slack_post", "slack_read"},
	}
	if !phase.Delivered(d) {
		t.Error("calling the planned tool did not read as delivered")
	}

	// The counterfactual: recon only, no delivery tool. Without this the
	// assertion above passes for a gate that always says yes.
	d.Called = []string{"lookup_colleague"}
	if phase.Delivered(d) {
		t.Error("recon alone read as delivered against a named plan")
	}
}

func TestANamedPlanDoesNotFallThroughToTheMCPFallback(t *testing.T) {
	t.Parallel()
	// The two rules are EXCLUSIVE, and this is the case that says so.
	// The plan named a tool that resolves, so the name is the contract —
	// calling some other MCP write instead is not delivering what was
	// promised, it is doing something else.
	//
	// Found by mutation: removing the early `return false` so a named plan
	// falls through to the fallback passed every other case in this file.
	// Without this one the exclusivity is asserted nowhere, and the gate
	// silently becomes "any MCP write counts", which is the loose failure
	// the fallback was carefully written to avoid.
	d := phase.Delivery{
		Called:          []string{"jira_create_issue"},
		PlannedResolved: []string{"slack_post"},
		MCPTools:        []string{"slack_post", "jira_create_issue"},
	}
	if phase.Delivered(d) {
		t.Error("a plan that named slack_post and called jira_create_issue instead " +
			"read as delivered")
	}
}

func TestAPhantomOnlyPlanFallsBackToServerBackedTools(t *testing.T) {
	t.Parallel()
	// The planner named MCP tools it cannot see and got them wrong, so
	// there is nothing to name-match. What counts is that the phase found
	// and called a real delivery tool, whatever its name turned out to be.
	d := phase.Delivery{
		Called:          []string{"slack_send_message"},
		PlannedResolved: nil, // every planned name was a phantom
		MCPTools:        []string{"slack_send_message", "slack_history"},
		KnownReads:      []string{"slack_history"},
	}
	if !phase.Delivered(d) {
		t.Error("a real delivery through a discovered MCP tool did not read as delivered")
	}
}

func TestAPhantomOnlyPlanThatCalledNoDeliveryToolIsNotDelivered(t *testing.T) {
	t.Parallel()
	// The bug the fallback would otherwise leave open: "reply hi" produces
	// text, never calls Slack, and completes silently having acted on
	// nothing — even when it called a builtin first.
	d := phase.Delivery{
		Called:     []string{"lookup_colleague", "reflect_and_persist"},
		MCPTools:   []string{"slack_send_message"},
		KnownReads: nil,
	}
	if phase.Delivered(d) {
		t.Error("a phase that called only builtins read as delivered")
	}
}

func TestAKnownReadIsNeverADelivery(t *testing.T) {
	t.Parallel()
	// A delivery to a shared surface is a WRITE. An explicit read through
	// the same server is recon.
	d := phase.Delivery{
		Called:     []string{"slack_history"},
		MCPTools:   []string{"slack_history", "slack_send_message"},
		KnownReads: []string{"slack_history"},
	}
	if phase.Delivered(d) {
		t.Error("an explicitly read-only MCP tool read as a delivery")
	}
}

func TestOnlyPositivelyAnnotatedReadsAreExempt(t *testing.T) {
	t.Parallel()
	// The trap this rule exists for: an UNANNOTATED tool is not a known
	// read. Treating unknown as read exempts every unannotated tool from
	// the fence, which is most of them on a fresh MCP server.
	d := phase.Delivery{
		Called:     []string{"jira_transition_issue"},
		MCPTools:   []string{"jira_transition_issue"},
		KnownReads: []string{}, // annotations absent, not "read: false"
	}
	if !phase.Delivered(d) {
		t.Error("an unannotated MCP tool was treated as a known read")
	}
}

func TestABuiltinNamedInThePlanStillCountsWhenItResolves(t *testing.T) {
	t.Parallel()
	// The server-backed requirement belongs to the FALLBACK only. When
	// the planner named a tool that resolves, the exact name is the
	// contract — including a first-party one, because a plan that said it
	// would call it and did has delivered what it promised.
	d := phase.Delivery{
		Called:          []string{"spawn_subagent"},
		PlannedResolved: []string{"spawn_subagent"},
		MCPTools:        nil,
	}
	if !phase.Delivered(d) {
		t.Error("a resolved first-party tool named in the plan did not count")
	}
}

func TestResolvePlannedSplitsTheTwoQuestionsApart(t *testing.T) {
	t.Parallel()
	// The halves answer different questions and the engine reads them
	// separately: delivery keys off the resolved half, INTENT off the raw
	// list. A plan naming only phantoms still intended to act, and reading
	// it as intending nothing turns a failed delivery into a clean turn.
	resolved, phantoms := phase.ResolvePlanned(
		[]string{"slack_post", "slack_send_msg", "confluence_page"},
		[]string{"slack_post", "jira_create"},
	)
	if !slices.Equal(resolved, []string{"slack_post"}) {
		t.Errorf("resolved = %v, want [slack_post]", resolved)
	}
	if !slices.Equal(phantoms, []string{"confluence_page", "slack_send_msg"}) {
		t.Errorf("phantoms = %v, want the two unresolved names sorted", phantoms)
	}
}

func TestPhantomsAreReportedRatherThanSwallowed(t *testing.T) {
	t.Parallel()
	// A planner naming tools that do not exist is usually guessing at a
	// surface it cannot see — which is a signal about the PROMPT, not
	// about the model.
	got := phase.Phantoms([]string{"b_tool", "a_tool"}, []string{"a_tool"})
	if !slices.Equal(got, []string{"b_tool"}) {
		t.Errorf("phantoms = %v, want [b_tool]", got)
	}
	if got := phase.Phantoms([]string{"a_tool"}, []string{"a_tool"}); len(got) != 0 {
		t.Errorf("phantoms = %v, want none", got)
	}
}

func TestAPhaseThatCalledNothingNeverDelivers(t *testing.T) {
	t.Parallel()
	// Both branches, since they fail differently: the name-precise one has
	// nothing to match, the fallback has nothing to classify.
	for name, d := range map[string]phase.Delivery{
		"named plan":   {PlannedResolved: []string{"slack_post"}, MCPTools: []string{"slack_post"}},
		"phantom plan": {MCPTools: []string{"slack_post"}},
	} {
		if phase.Delivered(d) {
			t.Errorf("%s: a phase that called nothing read as delivered", name)
		}
	}
}

package skills_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/skills"
)

// withLoader is the surface a guard can arm on: it has the unlock.
func withLoader(tools []string, servers ...string) prompts.Surface {
	return prompts.Surface{
		Tools:      append(slices.Clone(tools), "load_tool_skill"),
		MCPServers: servers,
	}
}

// THE GUARD REFUSES the tools a required skill covers until it is loaded.
// Asking harder in the prompt does not stop a model going straight for the
// tool; refusing the call does.
func TestARequiredSkillGatesTheToolsItCovers(t *testing.T) {
	t.Parallel()
	r := registry(t, skill("code", skills.Trigger{MCPServer: "gitlab"}, true))
	g := skills.NewGuard(r, prompts.PhaseExecute, withLoader(nil, "gitlab"))
	if g == nil {
		t.Fatal("the guard did not arm for a required skill")
	}

	blocked := g.Check("create_mr", "gitlab")
	if blocked == "" {
		t.Fatal("a covered tool was allowed before its skill was loaded")
	}
	// THE ERROR IS THE RECOVERY PATH, so it names the exact call: a model
	// told only "load a skill" spends a round discovering which.
	for _, want := range []string{`load_tool_skill(key="code")`, "create_mr", "Code"} {
		if !strings.Contains(blocked, want) {
			t.Fatalf("the refusal does not say %q:\n%s", want, blocked)
		}
	}

	g.Loaded("code")
	if got := g.Check("create_mr", "gitlab"); got != "" {
		t.Fatalf("a loaded skill still blocks: %s", got)
	}
}

// A skill gates only the tools its trigger NAMES, never every tool on a
// surface that happened to catalogue it.
func TestOnlyTheCoveredToolsAreGated(t *testing.T) {
	t.Parallel()
	r := registry(t, skill("code", skills.Trigger{MCPServer: "gitlab"}, true))
	g := skills.NewGuard(r, prompts.PhaseExecute,
		withLoader([]string{"lookup_colleague"}, "gitlab", "mattermost"))

	for _, tc := range []struct{ tool, server string }{
		{"post_message", "mattermost"}, // another server's tool
		{"lookup_colleague", ""},       // a builtin
	} {
		if got := g.Check(tc.tool, tc.server); got != "" {
			t.Fatalf("%q on %q was gated: %s", tc.tool, tc.server, got)
		}
	}
}

// AN ADVISORY SKILL gates nothing: operators mark orientation-grade content
// required: false precisely so it stays a hint.
func TestAnAdvisorySkillGatesNothing(t *testing.T) {
	t.Parallel()
	// With nothing required there is nothing to arm for.
	r := registry(t, skill("code", skills.Trigger{MCPServer: "gitlab"}, false))
	if g := skills.NewGuard(r, prompts.PhaseExecute, withLoader(nil, "gitlab")); g != nil {
		t.Fatal("the guard armed with nothing required")
	}

	// And ALONGSIDE a required skill — where the guard is armed and the
	// advisory one is in the same catalogue — its tools stay open. Without
	// this the arming check alone would pass a guard that enforced
	// everything it was shown.
	mixed := registry(t,
		skill("code", skills.Trigger{MCPServer: "gitlab"}, true),
		skill("hints", skills.Trigger{MCPServer: "mattermost"}, false),
	)
	g := skills.NewGuard(mixed, prompts.PhaseExecute,
		withLoader(nil, "gitlab", "mattermost"))
	if g == nil {
		t.Fatal("the guard did not arm for the required skill")
	}
	if got := g.Check("post_message", "mattermost"); got != "" {
		t.Fatalf("an advisory skill gated its tool: %s", got)
	}
	if g.Check("create_mr", "gitlab") == "" {
		t.Fatal("the required skill alongside it stopped gating")
	}
}

// THE EXEMPT SET IS ABOUT DEADLOCK, not policy. A misauthored trigger can
// cost a phase some tools; it must never cost the phase.
func TestTheGuardNeverBlocksWhatWouldBrickTheSession(t *testing.T) {
	t.Parallel()
	// A trigger naming the plumbing itself — the pathological case the
	// exemption exists for.
	everything := skills.Trigger{AnyOf: []skills.Trigger{
		{Tool: "load_tool_skill"}, {Tool: "activate_tool"},
		{Tool: "list_mcp_server_tools"}, {Tool: "submit_plan"},
		{Tool: "submit_review"}, {Tool: "create_mr"},
	}}
	r := registry(t, skill("greedy", everything, true))
	g := skills.NewGuard(r, prompts.PhasePlan,
		withLoader([]string{"activate_tool", "list_mcp_server_tools",
			"submit_plan", "submit_review", "create_mr"}))

	for _, exempt := range skills.ExemptTools {
		if got := g.Check(exempt, ""); got != "" {
			t.Fatalf("%q was blocked: %s", exempt, got)
		}
	}
	// And the tool that is not plumbing IS still gated, so the exemption
	// is narrow rather than a disarm.
	if g.Check("create_mr", "") == "" {
		t.Fatal("a genuinely covered tool was allowed")
	}
}

// NIL WHEN THE SESSION CANNOT RECOVER: with no loader on the surface there
// is no way to satisfy the guard, so arming it would refuse tools the model
// has no path to unlock.
func TestTheGuardRefusesToArmWithNoUnlock(t *testing.T) {
	t.Parallel()
	r := registry(t, skill("code", skills.Trigger{MCPServer: "gitlab"}, true))
	if g := skills.NewGuard(r, prompts.PhaseExecute,
		prompts.Surface{MCPServers: []string{"gitlab"}}); g != nil {
		t.Fatal("the guard armed on a surface with no loader")
	}
}

// REVIEW IS EXEMPT ENTIRELY: it has no domain tools and no loader, so there
// is nothing to gate — the same exception the catalogue's marker makes.
func TestReviewIsNeverGated(t *testing.T) {
	t.Parallel()
	r := registry(t, skill("code", skills.Trigger{MCPServer: "gitlab"}, true))
	if g := skills.NewGuard(r, prompts.PhaseReview, withLoader(nil, "gitlab")); g != nil {
		t.Fatal("the reviewer was gated")
	}
}

// Pending names what a stalled session is waiting for. The blocked call
// alone names only the first, and an operator looking at the turn needs the
// set.
func TestPendingNamesEveryUnloadedRequirement(t *testing.T) {
	t.Parallel()
	r := registry(t,
		skill("code", skills.Trigger{MCPServer: "gitlab"}, true),
		skill("chat", skills.Trigger{MCPServer: "mattermost"}, true),
		skill("hints", skills.Trigger{MCPServer: "gitlab"}, false),
	)
	g := skills.NewGuard(r, prompts.PhaseExecute, withLoader(nil, "gitlab", "mattermost"))
	if got := g.Pending(); !slices.Equal(got, []string{"chat", "code"}) {
		t.Fatalf("pending = %v, want both required skills sorted", got)
	}
	g.Loaded("code")
	if got := g.Pending(); !slices.Equal(got, []string{"chat"}) {
		t.Fatalf("pending after a load = %v", got)
	}
}

// A NIL GUARD is what most sessions have, and every method must answer
// rather than panic — Check runs on every tool call in the company.
func TestANilGuardAllowsEverything(t *testing.T) {
	t.Parallel()
	var g *skills.Guard
	if got := g.Check("create_mr", "gitlab"); got != "" {
		t.Fatalf("a nil guard blocked: %s", got)
	}
	if g.Pending() != nil || g.Blocking("create_mr", "gitlab") != nil {
		t.Fatal("a nil guard reported state")
	}
	g.Loaded("code")
	if got := g.Render("${tenant}"); got != "${tenant}" {
		t.Fatalf("Render = %q", got)
	}
}

// A guard built over a nil registry cannot arm — there is nothing to enforce.
func TestNoRegistryMeansNoGuard(t *testing.T) {
	t.Parallel()
	if g := skills.NewGuard(nil, prompts.PhaseExecute, withLoader(nil, "gitlab")); g != nil {
		t.Fatal("a guard armed with no registry")
	}
}

// THE UNLOCK IS A TOOL CALL, watched rather than reported: the tool is
// registered once per company while the guard is per phase session, so a
// tool holding a guard would hold whichever session registered last.
func TestASuccessfulLoadUnlocksTheToolsItCovers(t *testing.T) {
	t.Parallel()
	r := registry(t, skill("code", skills.Trigger{MCPServer: "gitlab"}, true))
	g := skills.NewGuard(r, prompts.PhaseExecute, withLoader(nil, "gitlab"))

	g.Observe(skills.LoaderTool, map[string]any{"key": "code"})
	if got := g.Check("create_mr", "gitlab"); got != "" {
		t.Fatalf("the tool is still gated after its skill loaded: %s", got)
	}
}

// A load naming a key nobody has, or any other tool, unlocks nothing —
// otherwise a typo would open every tool the real skill was gating.
func TestOnlyALoadOfTheRightSkillUnlocksIt(t *testing.T) {
	t.Parallel()
	r := registry(t, skill("code", skills.Trigger{MCPServer: "gitlab"}, true))

	for _, tc := range []struct {
		name string
		tool string
		args map[string]any
	}{
		{"another skill's key", skills.LoaderTool, map[string]any{"key": "chat"}},
		{"an empty key", skills.LoaderTool, map[string]any{"key": "  "}},
		{"no key at all", skills.LoaderTool, map[string]any{}},
		{"a key of the wrong type", skills.LoaderTool, map[string]any{"key": 7}},
		{"a different tool entirely", "create_mr", map[string]any{"key": "code"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := skills.NewGuard(r, prompts.PhaseExecute, withLoader(nil, "gitlab"))
			g.Observe(tc.tool, tc.args)
			if g.Check("create_mr", "gitlab") == "" {
				t.Fatalf("%s unlocked the tool", tc.name)
			}
		})
	}
}

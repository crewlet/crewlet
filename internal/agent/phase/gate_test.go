package phase_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
)

func TestDirectPlansSkipReviewOnlyWhenExecuteItselfDelivered(t *testing.T) {
	t.Parallel()
	// `direct` means the planner committed to EXECUTE doing the work in one
	// shot. If Execute did not deliver, Review runs anyway — otherwise the
	// turn completes as a silent no-op.
	g := phase.Gate{
		ExpectedAction:  true,
		PlannedResolved: []string{"slack_post"},
		MCPTools:        []string{"slack_post"},
		ExecuteCalled:   []string{"slack_post"},
	}
	if g.MustReview() {
		t.Error("Execute delivered, yet Review was forced")
	}
	g.ExecuteCalled = []string{"lookup_colleague"}
	if !g.MustReview() {
		t.Error("Execute skipped the delivery tool and Review was not forced")
	}
}

func TestForcingReviewIgnoresPlansOwnDelivery(t *testing.T) {
	t.Parallel()
	// THE ASYMMETRY, and the one most likely to be "tidied" into a bug.
	// Nothing has judged anything yet at this point, so a Plan-phase call
	// cannot stand in for the one-shot Execute the planner committed to.
	// OverrideDone takes the opposite view for a reason it states itself.
	g := phase.Gate{
		ExpectedAction:  true,
		PlannedResolved: []string{"slack_post"},
		MCPTools:        []string{"slack_post"},
		PlanCalled:      []string{"slack_post"},
		ExecuteCalled:   []string{"lookup_colleague"},
	}
	if !g.MustReview() {
		t.Error("a Plan-phase delivery satisfied the direct-plan safety net")
	}
	// And the same facts do NOT trigger the post-Review override, because
	// there Review read the full Plan log before deciding.
	if override, _ := g.OverrideDone(); override {
		t.Error("the post-Review override ignored a genuine Plan-phase delivery, " +
			"which would make the next round double-post")
	}
}

func TestAPlanThatIntendedNothingIsNeverForcedOrOverridden(t *testing.T) {
	t.Parallel()
	// A turn that was only ever going to think has nothing to deliver.
	g := phase.Gate{ExpectedAction: false, ExecuteCalled: []string{"lookup_colleague"}}
	if g.MustReview() {
		t.Error("Review was forced on a turn that intended no action")
	}
	if override, _ := g.OverrideDone(); override {
		t.Error("done was overridden on a turn that intended no action")
	}
}

func TestDoneIsOverturnedWhenTheNamedToolWasNeverCalled(t *testing.T) {
	t.Parallel()
	// The failure this exists for: Review judges from the produced TEXT and
	// says done even though nothing delivered it. Without the override the
	// seat appears to have answered and the message never reaches Slack.
	g := phase.Gate{
		ExpectedAction:  true,
		PlannedResolved: []string{"slack_post", "jira_comment"},
		MCPTools:        []string{"slack_post", "jira_comment"},
		ExecuteCalled:   []string{"jira_comment"},
	}
	// jira_comment WAS called, so this delivered — the counterfactual that
	// keeps the assertion below from passing for an override that always
	// fires.
	if override, _ := g.OverrideDone(); override {
		t.Fatal("a partial but real delivery was overturned")
	}

	g.ExecuteCalled = []string{"lookup_colleague"}
	override, correction := g.OverrideDone()
	if !override {
		t.Fatal("done stood despite nothing being delivered")
	}
	for _, want := range []string{"jira_comment", "slack_post", "Re-plan"} {
		if !strings.Contains(correction, want) {
			t.Errorf("the correction does not mention %q:\n%s", want, correction)
		}
	}
	if strings.Contains(correction, "activate_tool") {
		t.Errorf("a named-tool miss got the phantom correction:\n%s", correction)
	}
}

func TestAPhantomOnlyPlanGetsTheDiscoveryCorrection(t *testing.T) {
	t.Parallel()
	// The two corrections differ because the two failures do. Telling a
	// planner to "call the required tool" when the tool it named does not
	// exist sends it round the same loop again.
	g := phase.Gate{
		ExpectedAction: true,
		PlannedPhantom: []string{"slack_send_msg"},
		MCPTools:       []string{"slack_post"},
		ExecuteCalled:  []string{"lookup_colleague"},
	}
	override, correction := g.OverrideDone()
	if !override {
		t.Fatal("a phantom-only plan that delivered nothing was not overturned")
	}
	for _, want := range []string{"slack_send_msg", "list_mcp_server_tools", "activate_tool"} {
		if !strings.Contains(correction, want) {
			t.Errorf("the correction does not mention %q:\n%s", want, correction)
		}
	}
	if strings.Contains(correction, "Re-plan and ensure") {
		t.Errorf("a phantom plan got the named-tool correction:\n%s", correction)
	}
}

func TestADiscoveredDeliveryToolSatisfiesAPhantomPlan(t *testing.T) {
	t.Parallel()
	// The planner guessed the name wrong but Execute found the real tool
	// and used it. Overturning that would make the next round post twice.
	g := phase.Gate{
		ExpectedAction: true,
		PlannedPhantom: []string{"slack_send_msg"},
		MCPTools:       []string{"slack_post", "slack_history"},
		KnownReads:     []string{"slack_history"},
		ExecuteCalled:  []string{"slack_history", "slack_post"},
	}
	if override, c := g.OverrideDone(); override {
		t.Errorf("a real discovered delivery was overturned:\n%s", c)
	}
}

func TestCorrectionsAreDeterministic(t *testing.T) {
	t.Parallel()
	// The correction goes into a prompt. An unordered one changes the
	// prefix on every round and defeats provider prompt caching for the
	// rest of the turn.
	g := phase.Gate{
		ExpectedAction:  true,
		PlannedResolved: []string{"zulu", "alpha", "mike"},
		MCPTools:        []string{"zulu", "alpha", "mike"},
		ExecuteCalled:   []string{"lookup_colleague"},
	}
	_, first := g.OverrideDone()
	if !strings.Contains(first, "alpha, mike, zulu") {
		t.Errorf("missing tools are not sorted:\n%s", first)
	}
	for range 30 {
		if _, again := g.OverrideDone(); again != first {
			t.Fatalf("unstable correction:\n%s\nvs\n%s", first, again)
		}
	}

	// The phantom branch has its own list and its own sort. Found by
	// mutation: with only one phantom in the test above, removing that
	// sort changed nothing and the ordering was asserted nowhere.
	ph := phase.Gate{
		ExpectedAction: true,
		PlannedPhantom: []string{"zulu_send", "alpha_send", "mike_send"},
		MCPTools:       []string{"slack_post"},
		ExecuteCalled:  []string{"lookup_colleague"},
	}
	_, phFirst := ph.OverrideDone()
	if !strings.Contains(phFirst, "alpha_send, mike_send, zulu_send") {
		t.Errorf("phantoms are not sorted:\n%s", phFirst)
	}
	for range 30 {
		if _, again := ph.OverrideDone(); again != phFirst {
			t.Fatalf("unstable phantom correction:\n%s\nvs\n%s", phFirst, again)
		}
	}
}

func TestTheMergeLosesNeitherPhase(t *testing.T) {
	t.Parallel()
	// The merge must not lose either side: losing Execute's calls fires the
	// override on every turn that planned no recon, and losing Plan's
	// re-creates the double-post the asymmetry above exists to prevent.
	g := phase.Gate{
		ExpectedAction:  true,
		PlannedResolved: []string{"slack_post"},
		MCPTools:        []string{"slack_post"},
		PlanCalled:      []string{"slack_post"},
		ExecuteCalled:   []string{"slack_post"},
	}
	if override, c := g.OverrideDone(); override {
		t.Errorf("a delivery called by both phases was overturned:\n%s", c)
	}
	g.PlanCalled = nil
	if override, c := g.OverrideDone(); override {
		t.Errorf("Execute's own delivery was lost in the merge:\n%s", c)
	}
	g.PlanCalled = []string{"slack_post"}
	g.ExecuteCalled = nil
	if override, c := g.OverrideDone(); override {
		t.Errorf("Plan's delivery was lost in the merge:\n%s", c)
	}
}

func TestTheEngineCorrectionIsTheLastThingThePlannerReads(t *testing.T) {
	t.Parallel()
	// On the override path Review said done and wrote no correction of its
	// own, so the engine's is the only instruction there is — and it must
	// not be buried above the reviewer's prose.
	got := phase.AppendCorrection("looks fine to me", "but nothing was delivered")
	if !strings.HasSuffix(got, "but nothing was delivered") {
		t.Errorf("the engine correction is not last:\n%s", got)
	}
	if !strings.HasPrefix(got, "looks fine to me") {
		t.Errorf("the reviewer's notes were dropped:\n%s", got)
	}
	// Neither half may leave a dangling blank block when the other is
	// absent — that reads to a model as a section that was cut off.
	if got := phase.AppendCorrection("", "engine says no"); got != "engine says no" {
		t.Errorf("empty notes left padding: %q", got)
	}
	if got := phase.AppendCorrection("   \n ", "engine says no"); got != "engine says no" {
		t.Errorf("whitespace-only notes left padding: %q", got)
	}
	if got := phase.AppendCorrection("reviewer said this", ""); got != "reviewer said this" {
		t.Errorf("an absent correction left padding: %q", got)
	}
}

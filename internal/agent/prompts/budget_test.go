package prompts

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/tools"
)

// The measure is part of the contract: someone "simplifying" it to len(s)/4
// would change every budget without touching a number.
//
// [approxTokens] itself lives in sections.go now, beside the roster allowance
// it also measures. ONE implementation: a roster planned against one
// approximation and asserted against another is a budget that holds here and
// not in the prompt.
func TestApproxTokensCountsCodePointsNotBytes(t *testing.T) {
	t.Parallel()
	const fourEmDashes = "————" // 4 code points, 12 bytes
	if got := approxTokens(fourEmDashes); got != 1 {
		t.Errorf("approxTokens(4 em dashes) = %d, want 1 (a byte count would give 3)", got)
	}
}

// withinBudget reports the measurement whether it passes or fails, so a
// change that spends the remaining headroom is visible in `go test -v`
// rather than only on the day it breaks.
func withinBudget(t *testing.T, name, prompt string, budget int) {
	t.Helper()
	got := approxTokens(prompt)
	if got >= budget {
		t.Errorf("%s prompt too large: ~%d tokens, budget %d", name, got, budget)
		return
	}
	t.Logf("%s prompt: ~%d tokens (budget %d, headroom %d)", name, got, budget, budget-got)
}

// Reference role: a lead, with a roster, and the surface a production
// executor has — see referenceExecutor.
//
// The budget covers the identity scaffold, the turn contract (act with tool
// calls, finish the arc and discover what is missing, stay reachable) and
// EVERY section a tool on the surface turns on: the escalation guidance, the
// sandbox section, the workers list, the code guidance and the tools to
// discover. Measured with
// all of them rendered, because that is the seat the budget is for: a lead on
// the default work tracker holds `comment_on_work_item` and so carries the
// escalation section on every turn, and a budget measured on a seat without
// it bounded a prompt no such lead is sent. The review budget below turns
// every gate on for the same reason.
//
// It used to be measured against a synthetic catalogue of a hundred one-line
// tools, which measured nothing production sends: the executor holds every
// first-party tool from its first round, whose definitions reach the model
// with the request, so what its system prompt lists is its MCP servers and
// nothing else.
//
// 2200 -> 870 when the prompt stopped saying what the model gets elsewhere:
// the first-party tool catalogue (every description a second time), the rules
// the submission and `decision` schemas state beside their fields, and the
// meta sentences about what the prompt already holds.
//
// 870 -> 930 for a section, deliberately: the code guidance (`code_history`),
// which a seat of a company hosting code renders, measures ~55, and took the
// prompt from ~807 to ~862 — inside 870, but with the headroom gone that lets
// the next sentence land without a decision. It measures ~68 since it names
// who last changed a line and how a file's history went beside what it named
// (code_blame and code_log, the same family): the sentence the headroom was
// left for.
//
// 930 -> 1010 for a section, deliberately: the guidance on changing code
// (`code_writes`), which a seat of a company serving the code host's writes
// renders, measures ~72 after it was cut to the one choice no tool's
// description can carry, and took the prompt from ~875 to ~947.
//
// Measured at ~947, so this leaves ~6%: enough that a sentence can be added,
// tight enough that a section cannot. The plain contract alone — no tracker,
// no workers, no sandbox, no code — measures ~508 of it.
//
// IT BOUNDS THE PROSE, NOT THE ROSTER. The reference lead has one direct
// report, so what this number holds down is the contract text — the part that
// grows when somebody writes a paragraph. What a roster costs is bounded by
// rosterAllowanceTokens instead, because it grows with the company rather
// than with the diff, and the two together are measured by
// TestTheWholeTurnStaysUnderBudget below.
func TestTheExecutorPromptStaysUnderBudget(t *testing.T) {
	t.Parallel()
	p := BuildExecutor(lead(), referenceExecutor(t)).Text
	withinBudget(t, "executor", p, 1010)
}

// THE REFERENCE RENDERS EVERY GATED SECTION. A budget measured on an input
// that turns one off is a budget on a prompt smaller than the one production
// sends, which is how the executor's ceiling was once set ~100 tokens under
// what every lead on the default tracker was sent: nothing failed, because
// nothing measured that lead.
func TestTheBudgetReferenceRendersEveryGatedSection(t *testing.T) {
	t.Parallel()
	p := BuildExecutor(lead(), referenceExecutor(t))
	for _, key := range []string{"escalation", "sandbox", "workers", KeyCodeHistory, KeyCodeWrites,
		KeyDiscoverableTools} {
		if _, ok := slice(p, key); !ok {
			t.Errorf("the reference executor renders no %q section, so the budgets "+
				"below are measured on a smaller prompt than production's", key)
		}
	}
}

// Review is identity + the rules that decide its one decision: the evidence
// pointer, the settled-delivery note, the incomplete, duplicate, missing-tool
// and blocked rules — measured with BOTH gated rules rendered (the sandbox
// rule and `a2a_ask`), which is the largest a review prompt gets.
//
// The original budget was 300 (identity + a six-line enum), and it grew to
// 800 as each rule arrived for a turn-ending failure — the sandbox rule, the
// cross-round duplicate rule keyed on target and content, the settled
// delivery and incomplete notes, and the blocked rule's three-way split on
// whether the person has been told.
//
// 800 -> 590 when each rule was stated once: what a decision means and what
// `notes` and `completed_work` carry is the submission schema's, beside the
// fields, and the sandbox rule and `a2a_ask` render only for a seat holding
// them. Measured at ~545 with both gates on, so ~8% headroom: the next rule
// should have to justify itself here, not slip in.
//
// The roster allowance does not reach this prompt and the number is unmoved
// by it: review renders [BuildIdentityLine], which names the reviewer's
// manager and not one of its reports, so a lead of 500 measures the same as
// the reference lead. That is why the whole turn's budget below is the
// allowance plus the two prompts, with nothing added for a second copy of the
// roster.
func TestReviewPromptIsSmall(t *testing.T) {
	t.Parallel()
	withinBudget(t, "review", BuildReview(lead(), ReviewInput{OffersSandbox: true, OffersA2A: true}).Text, 590)
}

// sectionText is sections rendered as one prompt renders them: their lines
// joined by a newline. It is what [BuildExecutor]'s [Builder] produces for the
// same parts — the outline never changes a byte of the text — so a section
// measured or matched here is the section the prompt carries.
func sectionText(sections ...[]string) string {
	var lines []string
	for _, section := range sections {
		lines = append(lines, section...)
	}
	return strings.Join(lines, "\n")
}

// referenceExecutor is the executor input the budgets measure: a lead's
// surface with every tool that gates a section on it, the workers the
// shipped example company configures, the sandbox section, and the tools to
// discover a production snapshot renders.
//
// The surface is NAMES, because that is all a gate reads — what each tool
// says reaches the model with the request, never through this prompt. The
// names are the ones production's surface carries: `comment_on_work_item`
// and `create_work_item` from the default work tracker, `delegate` for a
// seat with workers, `run_sandbox` for one with the sandbox gate on, the code
// tools for a seat of a company that hosts code (`code.native`), and the two
// tools every executor adds (`submit_work` and the discovery pair).
func referenceExecutor(t *testing.T) ExecutorInput {
	t.Helper()
	return ExecutorInput{
		Discoverable: referenceDiscoverable(t),
		AvailableTools: append([]string{
			"activate_tool", escalationTool, "create_work_item", "delegate",
			"list_mcp_server_tools", "run_sandbox", "submit_work",
		}, slices.Concat(CodeHistoryTools(), CodeWriteTools())...),
		Workers:       referenceWorkers(),
		OffersSandbox: true,
	}
}

// referenceWorkers is the `## Your workers` list of the Nimbus example
// (examples/nimbus.company.yaml): its two templates, rendered by the function
// production renders them with and in the order it renders them in.
func referenceWorkers() string {
	workers := map[string]config.Worker{
		"researcher": {
			Description: "reads the team's own pages and reports what they actually " +
				"say, with the page titles it read",
			Tools: []string{"search_knowledge", "get_page"},
			Output: map[string]any{"type": "object", "properties": map[string]any{
				"findings": map[string]any{}, "sources": map[string]any{}, "gaps": map[string]any{},
			}},
		},
		"synthesist": {
			Description: "reconciles several workers' findings into one " +
				"recommendation with its open questions",
			Output: map[string]any{"type": "object", "properties": map[string]any{
				"recommendation": map[string]any{}, "disagreements": map[string]any{},
				"open_questions": map[string]any{},
			}},
		},
	}
	lines := make([]string, 0, len(workers))
	for _, name := range config.WorkerNames(workers) {
		lines = append(lines, config.DescribeWorker(name, workers[name]))
	}
	return strings.Join(lines, "\n")
}

// referenceDiscoverable is the reference executor's tools to discover,
// rendered by the renderer production uses over a snapshot shaped like
// production's: first-party tools with real-length descriptions, every one of
// them ACTIVE, as the executor holds them from its first round — and the
// three MCP servers an engineering lead typically carries (a tracker, a code
// host, a chat), several tools each.
//
// What renders is one line per server, and the case checks that it is: a
// first-party description in this text would mean the prompt is paying for a
// definition the request already carries, which is the regression this
// fixture exists to catch.
func referenceDiscoverable(t *testing.T) string {
	t.Helper()
	reg := tools.NewRegistry()
	var active []string
	for i := range 45 {
		name := fmt.Sprintf("first_party_%02d", i)
		desc := strings.Repeat("What this first-party tool does and when to call it. ", 8)
		if err := reg.Register(stubTool{name, desc}, tools.OriginBuiltin); err != nil {
			t.Fatal(err)
		}
		active = append(active, name)
	}
	for _, server := range []string{"atlassian", "github", "slack"} {
		for i := range 30 {
			name := fmt.Sprintf("%s_tool_%02d", server, i)
			if err := reg.Register(stubTool{name, "A vendor's tool."}, tools.Origin(server)); err != nil {
				t.Fatal(err)
			}
		}
	}
	out := reg.Snapshot().Discoverable(active)
	if strings.Contains(out, "first_party_") || strings.Count(out, "\n") != 2 {
		t.Fatalf("the reference executor's tools to discover are not its three "+
			"servers alone:\n%s", out)
	}
	return out
}

// stubTool is a name and a description and nothing else: what a budget
// measures is what a catalogue renders, never what a tool does.
type stubTool struct{ name, desc string }

func (s stubTool) Name() string               { return s.name }
func (s stubTool) Description() string        { return s.desc }
func (s stubTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (s stubTool) Call(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{}, nil
}

// THE WHOLE TURN, which is the number that actually bills.
//
// Two prompts where there were three, and the executor's is the only one that
// carries the scaffold. A budget on each prompt separately cannot catch the
// regression that matters here — moving a section from one prompt to another
// leaves both under their own budgets while the turn costs the same.
//
// IT IS MEASURED ON A LEAD OF 500, not on the reference lead, because the
// roster is the one section that grows with the company instead of with the
// diff. A budget measured on a one-report seat says nothing about the seat
// the defect was found on.
//
// 3000 -> 4000 for the roster allowance, and the raise was the allowance.
// 4000 -> 2430 when the prompts stopped saying what the model gets elsewhere:
// the turn's own prose measures ~1,258 with every gated section of both
// prompts rendered (referenceExecutor, and the review's two gated rules), the
// roster allowance is 1000 (see rosterAllowanceTokens) and spends ~987 of it
// here, and a mixed company pays another ~93 for the `## Human colleagues`
// note this fixture triggers. Measured at ~2,338, so ~4% headroom —
// deliberately tighter than the ~8% the prose budgets carry, because the next
// thing that wants room here should take it out of the allowance rather than
// out of the budget.
//
// 2430 -> 2490 for the code guidance (~55 tokens), the one exception that rule
// has met, and why: the section is rendered only for a seat of a company
// hosting code, while the allowance is every lead's in every company — taken
// out of it, the room would be charged to leads whose prompt never carries the
// section. The prose was then ~1,313 with every gated section rendered and the
// turn ~2,393, ~4% headroom again; the guidance has since grown to ~68 tokens
// (it names history and blame too), so the prose is ~1,326 and the turn
// ~2,406, ~3% headroom — inside the budget, with nothing taken from the
// allowance.
//
// 2490 -> 2565 for the code writes guidance (~72 tokens), the same exception
// for the same reason: rendered only for a seat of a company serving the code
// host's writes. The turn is ~2,478 with it, so ~3% headroom, as before it.
func TestTheWholeTurnStaysUnderBudget(t *testing.T) {
	t.Parallel()
	seat := bigLead(500)
	whole := BuildExecutor(seat, referenceExecutor(t)).Text +
		BuildReview(seat, ReviewInput{OffersSandbox: true, OffersA2A: true}).Text
	withinBudget(t, "turn", whole, wholeTurnBudget)

	// THE CONTROL. The same 500 reports with the allowance taken off: every
	// one of them rendered in full, which is what this prompt did until now
	// and what no budget could hold. Without it the assertion above passes on
	// a fixture that was never big enough to test anything.
	uncapped := planRoster(seat, newRosterAllowance(1<<20))
	control := sectionText(BuildIdentitySection(seat, uncapped), BuildRosterSection(seat, uncapped))
	if got := approxTokens(control); got < wholeTurnBudget {
		t.Fatalf("the uncapped roster measures ~%d tokens — under the whole "+
			"turn's own budget, so this fixture does not exercise the allowance", got)
	} else {
		t.Logf("control: the uncapped roster alone is ~%d tokens (budget %d)", got, wholeTurnBudget)
	}
}

// wholeTurnBudget is the budget TestTheWholeTurnStaysUnderBudget holds the
// turn to, and the size its control has to exceed: one number, so the control
// cannot go on proving the allowance against a budget the assertion left.
const wholeTurnBudget = 2565

// THE LADDER, and it is an order rather than a set: a lead reads profiles,
// then names, then one line telling it where the rest of its team went.
//
// Each rung buys something the rung below cannot. A profile is what a lead
// assigns work from; a name is what stops it believing the team ends at the
// last profile; and the closing line is the only one that gives it a MOVE —
// which is why it names `lookup_colleague` rather than saying that some
// colleagues were omitted, since a model told only that something is missing
// invents the missing thing.
func TestTheRosterLadderReachesEveryRungInOrder(t *testing.T) {
	t.Parallel()
	seat := bigLead(500)
	r := planRoster(seat, newRosterAllowance(rosterAllowanceTokens))
	if len(r.full) == 0 || len(r.handles) == 0 || r.omitted == 0 {
		t.Fatalf("the ladder did not reach all three rungs: %d full, %d handle-only, "+
			"%d omitted — a case that skips a rung asserts nothing about its order",
			len(r.full), len(r.handles), r.omitted)
	}
	t.Logf("ladder: %d full profiles, %d handle-only, %d omitted",
		len(r.full), len(r.handles), r.omitted)

	// NOBODY IS LOST BETWEEN THE RUNGS. A report that fell off the bottom
	// without being counted is a lead quietly told it has a smaller team.
	if got := len(r.full) + len(r.handles) + r.omitted; got != 500 {
		t.Errorf("the ladder accounts for %d of 500 reports", got)
	}

	section := sectionText(BuildRosterSection(seat, r))
	order(t, section,
		"  - Background: ",            // rung 1: the first full profile
		rosterHead(r.handles[0]),      // rung 2: the first handle-only line
		"- "+rosterGapLine(r.omitted), // rung 3: the closing line
	)
	contains(t, section, "`lookup_colleague` names them")

	// Rung 2 is handle-only, which is the whole reason it is cheap: exactly
	// as many profiles rendered as the plan admitted at rung 1.
	if got := strings.Count(section, "  - Background: "); got != len(r.full) {
		t.Errorf("%d profiles rendered for %d planned — rung 2 is not handle-only",
			got, len(r.full))
	}
	// And a human capped to rung 2 keeps the marker, which is not decoration:
	// it is how the lead knows this colleague answers asynchronously instead
	// of taking an engine turn.
	human := false
	for _, report := range r.handles {
		if report.IsHuman() {
			human = true
			contains(t, section, "**"+report.Name+"** ("+report.Handle()+") — **human teammate**")
			break
		}
	}
	if !human {
		t.Error("no human report reached rung 2 — the marker assertion above ran on nothing")
	}

	// THE LADDER DESCENDS, so the profiles are the head of the chart's own
	// order rather than whichever colleagues happened to have a short
	// backstory. A lead reading down a list wants the same list its chart
	// has, cut off — not a selection nobody can predict.
	reports := seat.reports()
	for i, report := range append(slices.Clone(r.full), r.handles...) {
		if report != reports[i] {
			t.Fatalf("rung position %d holds %q, the chart holds %q — the ladder "+
				"is picking reports rather than descending through them",
				i, report.Name, reports[i].Name)
		}
	}

	// And it is only OBSERVABLE when a later report is cheaper than an
	// earlier one, so here is one that is: leave the report that failed rung 1
	// expensive — it is what makes the ladder descend — and strip the profiles
	// from everything behind it. A descending ladder leaves them at rung 2,
	// queued behind the cost that closed rung 1. One that climbed back up
	// would promote them past colleagues the lead was told about first.
	varied := bigLead(500)
	for _, report := range varied.reports()[len(r.full)+1:] {
		report.Backstory, report.Goal, report.Responsibilities, report.Availability = "", "", nil, ""
	}
	if v := planRoster(varied, newRosterAllowance(rosterAllowanceTokens)); len(v.full) != len(r.full) {
		t.Errorf("%d reports took rung 1 where only the first %d can afford it — the "+
			"ladder climbed back up to a cheaper report behind the cut",
			len(v.full), len(r.full))
	}

	// The identity line lands on the SAME seats and closes with the SAME
	// sentence: one plan, two renderings.
	names := reportsLabel(r)
	for _, report := range append(slices.Clone(r.full), r.handles...) {
		if !strings.Contains(names, seatLabel(report)) {
			t.Errorf("the roster renders %q and the identity line does not", report.Name)
		}
	}
	if !strings.HasSuffix(names, rosterGapLine(r.omitted)) {
		t.Errorf("the identity line does not close with the roster's own gap line: %q",
			names[max(0, len(names)-120):])
	}
}

// ONE COUNTER, NOT TWO.
//
// The identity line and the roster are two renderings of one team, and two
// caps of a thousand each would let the prompt pay two thousand: neither
// knows what the other spent, and nothing above them adds the two up. This is
// the property a budget on the whole turn cannot see, because a turn that
// doubled its roster and lost a paragraph elsewhere would still measure the
// same.
func TestTheAllowanceIsOneCounterNotTwo(t *testing.T) {
	t.Parallel()
	seat := bigLead(500)
	r := planRoster(seat, newRosterAllowance(rosterAllowanceTokens))

	one := approxTokens(spentOn(seat, r))
	if one > rosterAllowanceTokens {
		t.Errorf("the two renderings measure ~%d tokens against an allowance of %d",
			one, rosterAllowanceTokens)
	}
	t.Logf("one counter: ~%d tokens (allowance %d)", one, rosterAllowanceTokens)

	// THE CONTROL: the design this one is not. Give the identity line a
	// thousand tokens of its own — the obvious second cap, naming reports
	// until its own budget runs out — and leave the roster with the thousand
	// it already has.
	two := approxTokens(namesUnderTheirOwnCap(seat, rosterAllowanceTokens)) +
		approxTokens(sectionText(BuildRosterSection(seat, r)))
	if two <= rosterAllowanceTokens {
		t.Fatalf("two independent caps measured ~%d tokens, inside a single "+
			"allowance of %d — the control proves nothing", two, rosterAllowanceTokens)
	}
	t.Logf("control, two counters: ~%d tokens (allowance %d)", two, rosterAllowanceTokens)

	// And the signature of one shared counter: what the ROSTER spends changes
	// what the IDENTITY line can say. Strip the profiles and the same 500
	// reports leave room for many more names. Under two independent caps the
	// identity line would name exactly as many either way, because nothing
	// the roster spent would ever reach it.
	bare := bigLead(500)
	for role := range bare.Org.AllRoles() {
		role.Backstory, role.Goal, role.Responsibilities, role.Availability = "", "", nil, ""
	}
	cheap := planRoster(bare, newRosterAllowance(rosterAllowanceTokens))
	if len(cheap.full)+len(cheap.handles) <= len(r.full)+len(r.handles) {
		t.Errorf("a cheaper roster named %d reports and an expensive one %d — the "+
			"identity line is not spending the roster's counter",
			len(cheap.full)+len(cheap.handles), len(r.full)+len(r.handles))
	}

	// And the prompt itself is built from ONE plan. Two counters inside
	// BuildExecutor would leave these two sections naming different sets of
	// colleagues, each within a budget of its own.
	p := BuildExecutor(seat, ExecutorInput{}).Text
	if !strings.Contains(p, "**Direct reports:** "+reportsLabel(r)) ||
		!strings.Contains(p, sectionText(BuildRosterSection(seat, r))) {
		t.Errorf("the executor's two roster renderings are not the plan this one "+
			"allowance made (%d full, %d handle-only, %d omitted)",
			len(r.full), len(r.handles), r.omitted)
	}
}

// spentOn is what the allowance actually bought: the two renderings of the
// reports, and neither section's fixed chrome. The heading and the
// "**Direct reports:** " label are the same handful of tokens for a team of
// one and a team of five hundred, so charging them to a budget that exists to
// bound GROWTH would measure the wrong thing.
func spentOn(seat Seat, r roster) string {
	return reportsLabel(r) + "\n" + strings.Join(BuildRosterSection(seat, r)[1:], "\n")
}

// namesUnderTheirOwnCap is the identity line as a SECOND counter would render
// it: reports named until a budget of its own is spent, knowing nothing about
// what the roster spent. It exists for the control above and nowhere else.
func namesUnderTheirOwnCap(seat Seat, tokens int) string {
	a := newRosterAllowance(tokens)
	var names []string
	for _, report := range seat.reports() {
		label := seatLabel(report)
		if !a.take(", " + label) {
			break
		}
		names = append(names, label)
	}
	return strings.Join(names, ", ")
}

// THE CAP IS RENDERING, NEVER AUTHORIZATION.
//
// `manages` is the chart's answer to who this seat may direct work to, and it
// is read from the chart on every lookup — the roster allowance never touches
// it. Read as an authorization cap it would be the worst possible reading of
// this change: a lead silently stripped of 457 reports it is still
// accountable for, with the engine agreeing that they are not its people.
func TestCappingTheRosterDoesNotCapManages(t *testing.T) {
	t.Parallel()
	o := profiledReports(500)
	seat := seatIn(o, "lead")
	p := BuildExecutor(seat, referenceExecutor(t)).Text

	if got := len(seat.Role.Manages); got != 500 {
		t.Errorf("the chart says this seat manages %d reports, want 500", got)
	}
	if got := len(o.Reports(seat.Role)); got != 500 {
		t.Errorf("the chart resolves %d reports, want 500", got)
	}
	// Guard the guard: the prompt really was capped, so the two assertions
	// above are about a roster that did NOT fit rather than one that did.
	contains(t, p, "`lookup_colleague` names them")
	excludes(t, p, "Report 499")
}

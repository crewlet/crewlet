package prompts

import (
	"fmt"
	"slices"
	"strings"
	"testing"
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

// Reference role: a lead, with a roster, three MCP servers, and a 100-tool
// catalogue.
//
// The budget covers the identity scaffold, the finish-the-arc rule, the
// stay-reachable rule, the verbose-decline rule, the submission contract, and
// the slim-catalogue + list_mcp_server_tools discovery flow. The ~50 tokens
// of discovery prose replaces the typical 1500-token MCP tool listing in a
// real workload — a net win as soon as one MCP server with more than five
// tools is wired.
//
// 2400 -> 2200 when the turn collapsed to one loop, which is DOWN: the
// executor's prompt is the old Plan prompt's identity scaffold plus the act
// contract Execute carried, minus everything the two-phase split needed — the
// tools_needed declaration, the phantom-tool warning, and the model-split
// hint. The frame that DECIDES is now the frame that acts, so nothing has to
// be described to a second conversation.
//
// Measured at ~2034 with a 100-tool catalogue, so this leaves ~7%: enough
// that a sentence can be added, tight enough that a section cannot.
//
// IT BOUNDS THE PROSE, NOT THE ROSTER. The reference lead has one direct
// report, so what this number holds down is the contract text and the
// catalogue — the part that grows when somebody writes a paragraph. What a
// roster costs is bounded by rosterAllowanceTokens instead, because it grows
// with the company rather than with the diff, and the two together are
// measured by TestTheWholeTurnStaysUnderBudget below.
func TestTheExecutorPromptStaysUnderBudgetWithABigCatalogue(t *testing.T) {
	t.Parallel()
	p := BuildExecutor(lead(), ExecutorInput{ToolCatalogue: bigCatalogue()})
	withinBudget(t, "executor", p, 2200)
}

// Review is identity + the decision enum + the settled-delivery note, the
// incomplete / sandbox / duplicate-delivery / missing-tool / blocked rules and
// the completed_work instruction.
//
// The original budget was 300 (identity + a six-line enum). The rules added
// ~290 tokens to prevent real production failures: the sandbox rule, which
// stops the reviewer looping a turn forever by misreading a
// run_sandbox-delegated investigation as fabrication; and the cross-round
// duplicate rule, which stops a second pass re-firing a side effect that
// already landed. Each maps to a turn-ending bug that was actually observed.
//
// Raised 560 -> 600 when the duplicate rule had to be keyed on target and
// content rather than tool name: keyed on the name alone it fired on the
// in-thread follow-up PriorWorkHeader explicitly asks for, so every corrected
// turn looped to max_iterations and terminated failed.
//
// 600 -> 750 for the two-stage turn: the tool-delivery rule is gone (the
// engine settles delivery before this prompt is built) and what replaced it
// is the note saying so plus the incomplete rule, which is what stops a
// reviewer grading an engine-written outcome as the agent's own verdict.
//
// 750 -> 800 when the blocked rule became a three-way split. It had named a
// single outcome, `self_iterate`, while saying in the same breath that the
// colleague "reply asynchronously and that re-triggers the agent" — and both
// halves cannot hold at once. A seat that had correctly put a clarifying
// question to its founder was sent back by the reviewer, asked again two
// minutes later, and ended on the max-iterations guard. Telling the three
// cases apart — not asked yet, already asked, nobody's reply would finish it
// — measures at 53 tokens over the one-outcome instruction, and buys back the
// two extra rounds every such turn was burning.
//
// The headroom is small on purpose: the next addition should have to justify
// itself here, not slip in. 800 keeps it at ~30 tokens, which is what 750
// left before the split — the raise pays for the rule, not for room.
//
// The roster allowance does not reach this prompt and the number is unmoved
// by it: review renders [BuildIdentityLine], which names the reviewer's
// manager and not one of its reports, so a lead of 500 measures the same 771
// as the reference lead. That is why the whole turn's raise below is the
// allowance exactly, with nothing added for a second copy of the roster.
func TestReviewPromptIsSmall(t *testing.T) {
	t.Parallel()
	withinBudget(t, "review", BuildReview(lead(), ReviewInput{}), 800)
}

// bigCatalogue is the 100-tool catalogue the budgets are measured against:
// 100 tools x ~60 chars = ~6k chars = ~1500 tokens.
func bigCatalogue() string {
	lines := make([]string, 100)
	for i := range lines {
		lines[i] = fmt.Sprintf("- tool_%d: Short description of tool number %d.", i, i)
	}
	return strings.Join(lines, "\n")
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
// 3000 -> 4000 for the roster allowance, and the raise IS the allowance: the
// turn's own prose measures ~2806 with a 100-tool catalogue, the roster
// allowance is 1000 (see rosterAllowanceTokens), and a mixed company pays
// another ~152 for the `## Human colleagues` note this fixture triggers.
// Measured at ~3838, so ~4% headroom — deliberately tighter than the ~7% the
// prose budgets carry, because the next thing that wants room here should
// take it out of the allowance rather than out of the budget.
func TestTheWholeTurnStaysUnderBudget(t *testing.T) {
	t.Parallel()
	seat := bigLead(500)
	whole := BuildExecutor(seat, ExecutorInput{ToolCatalogue: bigCatalogue()}) +
		BuildReview(seat, ReviewInput{})
	withinBudget(t, "turn", whole, 4000)

	// THE CONTROL. The same 500 reports with the allowance taken off: every
	// one of them rendered in full, which is what this prompt did until now
	// and what no budget could hold. Without it the assertion above passes on
	// a fixture that was never big enough to test anything.
	uncapped := planRoster(seat, newRosterAllowance(1<<20))
	control := joinSections(BuildIdentitySection(seat, uncapped), BuildRosterSection(seat, uncapped))
	if got := approxTokens(control); got < 4000 {
		t.Fatalf("the uncapped roster measures ~%d tokens — under the whole "+
			"turn's own budget, so this fixture does not exercise the allowance", got)
	} else {
		t.Logf("control: the uncapped roster alone is ~%d tokens (budget %d)", got, 4000)
	}
}

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

	section := joinSections(BuildRosterSection(seat, r))
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
		approxTokens(joinSections(BuildRosterSection(seat, r)))
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
	p := BuildExecutor(seat, ExecutorInput{})
	if !strings.Contains(p, "**Direct reports:** "+reportsLabel(r)) ||
		!strings.Contains(p, joinSections(BuildRosterSection(seat, r))) {
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
// this change: a lead silently stripped of 461 reports it is still
// accountable for, with the engine agreeing that they are not its people.
func TestCappingTheRosterDoesNotCapManages(t *testing.T) {
	t.Parallel()
	o := profiledReports(500)
	seat := seatIn(o, "lead")
	p := BuildExecutor(seat, ExecutorInput{ToolCatalogue: bigCatalogue()})

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

package prompts

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// The measure is part of the contract: someone "simplifying" it to len(s)/4
// would change every budget without touching a number. [approxTokens] has ONE
// implementation, in sections.go, which the roster allowance measures with too:
// a roster planned against one approximation and asserted against another is a
// budget that holds here and not in the prompt.
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

// Reference role: a lead with a roster and a production executor's surface
// (referenceExecutor), measured with EVERY section a tool on it turns on
// (escalation, workers, the code guidance): a lead on the default tracker
// carries the escalation section on every turn. No tool catalogue is measured,
// since the harness delivers the definitions and the prompt lists none. The
// code sections render only for a seat of a company hosting code, so their room
// is charged to those prompts alone. Measured at ~1,030, so ~3% headroom: the
// next sentence must justify a raise here.
//
// IT BOUNDS THE PROSE, NOT THE ROSTER: the reference lead has one report. A
// roster grows with the company, so rosterAllowanceTokens bounds it instead,
// and TestTheWholeTurnStaysUnderBudget measures the two together.
func TestTheExecutorPromptStaysUnderBudget(t *testing.T) {
	t.Parallel()
	p := BuildExecutor(lead(), referenceExecutor(t)).Text
	withinBudget(t, "executor", p, 1060)
}

// THE REFERENCE RENDERS EVERY GATED SECTION: a budget measured on an input
// that turns one off bounds a prompt smaller than the one production sends,
// and nothing fails.
func TestTheBudgetReferenceRendersEveryGatedSection(t *testing.T) {
	t.Parallel()
	p := BuildExecutor(lead(), referenceExecutor(t))
	for _, key := range []string{"escalation", "workers", KeyCodeHistory, KeyCodeSearch, KeyCodeWrites,
		KeyCodeMerging} {
		if _, ok := slice(p, key); !ok {
			t.Errorf("the reference executor renders no %q section, so the budgets "+
				"below are measured on a smaller prompt than production's", key)
		}
	}
}

// Review is identity plus the rules that decide its one decision (the evidence
// pointer, the run's-work-is-in-no-log rule, the settled-delivery note, the
// incomplete, duplicate, missing-tool and blocked rules), measured with the
// gated `a2a_ask` rule rendered, the largest a review prompt gets. What a
// decision means and what `notes` and `completed_work` carry is the submission
// schema's, beside its fields. Measured at ~589, so ~8% headroom: the next rule
// must justify itself here, not slip in.
//
// The roster allowance never reaches this prompt: review renders
// [BuildIdentityLine], naming the reviewer's manager and none of its reports,
// so the whole turn's budget below adds nothing for a second roster.
func TestReviewPromptIsSmall(t *testing.T) {
	t.Parallel()
	withinBudget(t, "review", BuildReview(lead(), ReviewInput{OffersA2A: true}).Text, 640)
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
// surface with every tool that gates a section on it, and the workers the
// shipped example company configures. The surface is NAMES, all a gate reads:
// what a tool says reaches the model through its harness, never this prompt.
// The names are production's: `comment_on_work_item` and `create_work_item`
// from the default work tracker, `delegate` for a seat with workers, the code
// tools for a company with `code.backend: gitlet`, and `submit_work`, which
// every executor adds.
func referenceExecutor(t *testing.T) ExecutorInput {
	t.Helper()
	return ExecutorInput{
		AvailableTools: append([]string{
			escalationTool, "create_work_item", "delegate", "submit_work",
		}, slices.Concat(CodeHistoryTools(), CodeSearchTools(), CodeWriteTools(), CodeMergeTools())...),
		Workers: referenceWorkers(),
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

// THE WHOLE TURN, the number that actually bills: moving a section between the
// two prompts leaves each under its own budget while the turn costs the same.
// IT IS MEASURED ON A LEAD OF 500, because the roster is the one section that
// grows with the company rather than with the diff.
//
// Measured at ~2,604 (the prose of both prompts with every gated section, the
// ~1,000-token roster allowance and the `## Human colleagues` note this fixture
// triggers), ~0.8% under wholeTurnBudget: the next thing that wants room takes
// it out of the allowance, or raises the budget with its reason recorded here.
// The code sections are the exception, charged here rather than to the
// allowance: they render only for a seat of a company hosting code, while the
// allowance is every lead's in every company.
func TestTheWholeTurnStaysUnderBudget(t *testing.T) {
	t.Parallel()
	seat := bigLead(500)
	whole := BuildExecutor(seat, referenceExecutor(t)).Text +
		BuildReview(seat, ReviewInput{OffersA2A: true}).Text
	withinBudget(t, "turn", whole, wholeTurnBudget)

	// THE CONTROL: the same 500 reports uncapped, every one in full. Without it
	// the assertion above passes on a fixture never big enough to test anything.
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
const wholeTurnBudget = 2625

// THE LADDER, and it is an order rather than a set: a lead reads profiles,
// then names, then one line telling it where the rest of its team went. A
// profile is what a lead assigns work from; a name stops it believing the team
// ends at the last profile; and the closing line gives it a MOVE, naming
// `lookup_colleague` rather than saying colleagues were omitted, since a model
// told only that something is missing invents the missing thing.
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
	// A human capped to rung 2 keeps the marker: it is how the lead knows this
	// colleague answers asynchronously instead of taking an engine turn.
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

	// THE LADDER DESCENDS: the profiles are the head of the chart's own order,
	// not whichever colleagues have a short backstory, so the lead reads its
	// chart's list cut off rather than a selection nobody can predict.
	reports := seat.reports()
	for i, report := range append(slices.Clone(r.full), r.handles...) {
		if report != reports[i] {
			t.Fatalf("rung position %d holds %q, the chart holds %q — the ladder "+
				"is picking reports rather than descending through them",
				i, report.Name, reports[i].Name)
		}
	}

	// That is only OBSERVABLE when a later report is cheaper than an earlier one:
	// keep the report that failed rung 1 expensive and strip the profiles behind
	// it. A descending ladder leaves them at rung 2; one that climbed back up
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

// ONE COUNTER, NOT TWO: the identity line and the roster render one team, and
// two caps of a thousand each would let the prompt pay two thousand, neither
// knowing what the other spent. A whole-turn budget cannot see this: a turn that
// doubled its roster and lost a paragraph elsewhere measures the same.
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

	// THE CONTROL, the design this is not: the identity line naming reports
	// under a thousand tokens of its own, beside the roster's thousand.
	two := approxTokens(namesUnderTheirOwnCap(seat, rosterAllowanceTokens)) +
		approxTokens(sectionText(BuildRosterSection(seat, r)))
	if two <= rosterAllowanceTokens {
		t.Fatalf("two independent caps measured ~%d tokens, inside a single "+
			"allowance of %d — the control proves nothing", two, rosterAllowanceTokens)
	}
	t.Logf("control, two counters: ~%d tokens (allowance %d)", two, rosterAllowanceTokens)

	// One shared counter's signature: what the ROSTER spends changes what the
	// IDENTITY line can say, so stripped profiles leave room for more names.
	// Under two independent caps it would name exactly as many either way.
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

	// And the prompt is built from ONE plan: two counters inside BuildExecutor
	// would leave these sections naming different colleagues.
	p := BuildExecutor(seat, ExecutorInput{}).Text
	if !strings.Contains(p, "**Direct reports:** "+reportsLabel(r)) ||
		!strings.Contains(p, sectionText(BuildRosterSection(seat, r))) {
		t.Errorf("the executor's two roster renderings are not the plan this one "+
			"allowance made (%d full, %d handle-only, %d omitted)",
			len(r.full), len(r.handles), r.omitted)
	}
}

// spentOn is what the allowance bought: the two renderings of the reports,
// without either section's fixed chrome (the heading, the "**Direct reports:** "
// label), which costs the same for a team of one or five hundred and so has no
// place in a budget that bounds GROWTH.
func spentOn(seat Seat, r roster) string {
	return reportsLabel(r) + "\n" + strings.Join(BuildRosterSection(seat, r)[1:], "\n")
}

// namesUnderTheirOwnCap is the identity line as a SECOND counter would render
// it, naming reports until its own budget is spent, blind to the roster's.
// It exists for the control above alone.
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

// THE CAP IS RENDERING, NEVER AUTHORIZATION: `manages` is the chart's answer
// to who this seat may direct work to, read from the chart on every lookup, and
// the roster allowance never touches it. Read as an authorization cap it would
// strip a lead of reports it is still accountable for, with the engine
// agreeing they are not its people.
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

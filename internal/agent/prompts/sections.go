package prompts

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/org"
)

// The shared section builders. Each returns the section's lines, or nil when
// the section does not apply — an empty policy list must leave no heading
// behind, not an empty bullet list.
//
// They live in this package rather than beside the agent definition because
// they are prompt text: the words below are as load-bearing as the phase
// contracts, and one source of truth for them keeps the executor's prompt and any
// combined single-shot prompt from drifting apart.

// BuildIdentitySection renders the professional identity for the executor.
//
// The executor needs enough to decide *what* to do: role, company, unit,
// goal, manager, and direct reports (for delegation decisions). The long-form
// backstory / guidelines / team goals render in the role-profile and
// unit-context sections instead.
//
// `##`, like every other section of the executor's prompt. It was the one `#`
// in the document, which under any structural reading made the company's
// policies, the roster, the turn contract and the tool catalogue parts of the
// agent's IDENTITY rather than the next things it reads. They are peers — the
// builders below all emit `##` — and the dashboard's prompt outline reads the
// same structure. The words are untouched.
func BuildIdentitySection(s Seat, reports roster) []string {
	if !s.ok() {
		return nil
	}
	parts := []string{
		"## Your Identity",
		"You are **" + s.Role.Name + "** at **" + s.Org.Name + "**.",
	}
	unit := s.unit()
	if unit != nil {
		parts = append(parts, "You belong to the **"+unit.Name+"** "+string(unit.Type)+".")
	}
	if s.Role.Goal != "" {
		parts = append(parts, "**Your goal:** "+s.Role.Goal)
	}
	parts = append(parts,
		"**Reports to:** "+managerLabel(s.manager()),
		"**Direct reports:** "+reportsLabel(reports),
	)
	if unit != nil && unit.Channel != "" {
		parts = append(parts, "**Team channel:** "+unit.Channel)
	}
	return parts
}

// BuildIdentityLine is the ultra-compact one-line identity for the review and
// onboarding phases.
//
// Both carry what they are about in the user message (for review, the round's
// own account and the executor's artifact), so the full reporting line and
// unit context are not needed. One sentence is enough to ground the model in
// "who is writing this".
func BuildIdentityLine(s Seat) string {
	if !s.ok() {
		return ""
	}
	return "You are **" + s.Role.Name + "** at **" + s.Org.Name +
		"** (reports to: " + managerLabel(s.manager()) + ")."
}

// seatLabel renders a colleague reference, marking human seats.
//
// The marker is load-bearing: it is how an agent knows its manager or report
// replies asynchronously over an external surface rather than running a turn
// (see [BuildHumanColleaguesNote]).
func seatLabel(r *org.Role) string {
	if r.IsHuman() {
		return r.Name + " (human)"
	}
	return r.Name
}

// managerLabel and reportsLabel keep both identity renderings answering the
// same way for a top-level seat — "None (top-level)", never a bare "none".
// An executor that read its own chart differently from its reviewer would be
// a difference no assertion in a phase test is looking for.

func managerLabel(manager *org.Role) string {
	if manager == nil {
		return "None (top-level)"
	}
	return seatLabel(manager)
}

// reportsLabel renders the roster's decision as a name list.
//
// It names exactly the seats [BuildRosterSection] renders and closes with
// exactly that section's gap line, because the two are one plan: a name here
// that the roster says nothing about tells the lead about a colleague it has
// no way to reason about, and a name here that the roster's own closing line
// counts as omitted contradicts it outright.
func reportsLabel(r roster) string {
	if len(r.full) == 0 && len(r.handles) == 0 && r.omitted == 0 {
		return "None"
	}
	labels := make([]string, 0, len(r.full)+len(r.handles))
	for _, report := range r.full {
		labels = append(labels, seatLabel(report))
	}
	for _, report := range r.handles {
		labels = append(labels, seatLabel(report))
	}
	if r.omitted == 0 {
		return strings.Join(labels, ", ")
	}
	gap := rosterGapLine(r.omitted)
	if len(labels) == 0 {
		return gap
	}
	return strings.Join(labels, ", ") + ", " + gap
}

// -- The roster allowance -------------------------------------------------
//
// A lead's direct reports render TWICE: as a name list on the identity
// section's "Direct reports:" line, and as a profile block each under
// "## Your Team". Both grew with the team and neither stopped, so a lead of
// an ORDINARY unit could not be prompted at all: 25 reports measure ~4,028
// tokens of roster alone, against a whole-turn budget that was 3,000.
//
// What follows bounds the RENDERING and nothing else. It does not bound
// `manages`: who a seat may direct work to is an authorization fact read from
// the chart on every lookup, and a seat still manages every report the
// allowance had no room to describe. Read as an authorization cap this would
// be the worst possible reading of it — a lead silently stripped of reports
// it is still accountable for.

// charsPerToken is the approximation every token figure in this package is
// measured in: four Unicode CODE POINTS to a token.
//
// An APPROXIMATION, not a tokenizer, and deliberately so — a real one would
// tie a pure text package to one vendor's vocabulary file, and would give
// numbers nobody could trace back to the measurement that set them. Code
// points rather than bytes because these prompts are dense with em dashes and
// arrows: counting bytes inflates every measurement by roughly 4%, which
// would quietly re-tighten the allowance and every budget in budget_test.go.
const charsPerToken = 4

// approxTokens is the measure the allowance below and the prompt budgets in
// budget_test.go share. ONE implementation: a roster planned against one
// approximation and asserted against another is a budget that holds in the
// test and not in the prompt.
func approxTokens(s string) int { return utf8.RuneCountInString(s) / charsPerToken }

// rosterAllowanceTokens is the ONE budget the identity line and the roster
// section spend, together.
//
// 1,000 is what the whole turn has room for, and what it buys is worth
// having. Measured against `profiledReports` in fixtures_test.go, which
// carries the profile a real company writes: a full agent profile costs ~119
// tokens, a full human one ~193 (a person also renders contact ids,
// availability and how to hand them work), a name-and-handle line 7 to 12,
// and the closing line 11. So 1,000 pays for three full profiles and then
// NAMES ~36 MORE COLLEAGUES — which names an ordinary unit outright (a lead
// of 25 reports omits nobody) and gives the lead of a large one real profiles
// for the head of its list and a lookup for the rest.
//
// The ceiling comes from the other end: the turn's own prose measures ~2,806
// with a 100-tool catalogue, a mixed company pays another ~152 for the
// `## Human colleagues` note, and the whole turn is held under 4,000 (see
// TestTheWholeTurnStaysUnderBudget). 1,000 is that headroom, spent
// deliberately on the section a lead assigns work from rather than taken
// silently by whichever team grew.
//
// It is a CEILING, not a target. A team that fits costs what it costs.
const rosterAllowanceTokens = 1000

// rosterAllowance is that budget as a live counter.
//
// It counts CHARACTERS, converting the token figure once here, because a
// per-line division by four rounds a fraction of every line away and a long
// roster loses whole reports to the rounding.
type rosterAllowance struct{ chars int }

func newRosterAllowance(tokens int) *rosterAllowance {
	return &rosterAllowance{chars: tokens * charsPerToken}
}

// lineCost is what lines cost the counter: their own runes, plus one each for
// the separator that joins a line to the next — a newline between roster
// lines, ", " between names on the identity line.
func lineCost(lines ...string) int {
	cost := 0
	for _, line := range lines {
		cost += utf8.RuneCountInString(line) + 1
	}
	return cost
}

// take deducts what lines cost and reports whether the counter could pay.
//
// A REFUSAL LEAVES THE COUNTER UNTOUCHED, so the next rung down gets an
// honest chance at what is left rather than inheriting a counter the rung
// above already half-spent on a line nobody will read.
func (a *rosterAllowance) take(lines ...string) bool {
	cost := lineCost(lines...)
	if cost > a.chars {
		return false
	}
	a.chars -= cost
	return true
}

// reserve deducts unconditionally, flooring at zero. It is for the one line
// that has to be affordable whatever else happened.
func (a *rosterAllowance) reserve(lines ...string) {
	if !a.take(lines...) {
		a.chars = 0
	}
}

// roster is the ladder's decision about one seat's direct reports: who
// renders in full, who renders as a name and a handle, and how many are left
// for the closing line.
//
// Decided ONCE and rendered twice, by [BuildIdentitySection] and
// [BuildRosterSection]. Deciding it per section would let the identity line
// name a colleague the roster then says nothing about — and, worse, name one
// the roster's own closing line counts among the omitted.
type roster struct {
	full    []*org.Role
	handles []*org.Role
	omitted int
}

// planRoster walks a seat's reports once and spends a on them.
//
// ONE COUNTER, NOT TWO. Every admitted report is charged for BOTH of its
// renderings — its name on the identity line and its lines under the roster —
// because two independent caps each spend their own budget without knowing
// what the other spent, and the prompt pays the sum.
//
// The ladder only ever DESCENDS. A report that will not fit at rung 1 sends
// every later one to rung 2 rather than being stepped over, so a lead reads
// full profiles, then names, then the gap — not an interleaving decided by
// whose backstory happened to be short.
//
// THE CLOSING LINE IS RESERVED BEFORE THE FIRST PROFILE, at the width it
// would have if nobody fitted at all. It is the rung that tells the lead
// where the rest of its team went; buying one more profile with the tokens
// that pay for it leaves a truncated roster claiming to be the whole team.
//
// RUNG 1 MAY SPEND AT MOST HALF, which is not a second cap — it is an order
// of priority inside the one counter, and the total is still the counter's.
// It is here because the two rungs are an order of magnitude apart: a profile
// measures 119 tokens for an agent and 193 for a human against 7 to 12 for a
// name, so an undivided counter buys six profiles and then cannot afford the
// SEVENTH COLLEAGUE'S NAME. That is a lead which can describe six people and
// does not know who else works for it — and not knowing a report exists is a
// worse failure than not knowing their backstory, because `lookup_colleague`
// answers the second question and cannot be asked the first. Half buys a
// handful of profiles for the head of the list and names ~36 more, which
// covers an ordinary unit outright.
func planRoster(s Seat, a *rosterAllowance) roster {
	reports := s.reports()
	if len(reports) == 0 {
		return roster{}
	}
	gap := rosterGapLine(len(reports))
	a.reserve("- "+gap, ", "+gap)
	profiles := a.chars / 2

	var out roster
	rung := 1
	for i, report := range reports {
		// What this report costs the IDENTITY line: its label and the ", "
		// that joins it to the previous one. Charged here, from this same
		// counter, which is the whole of "one allowance, two renderings".
		name := ", " + seatLabel(report)
		if rung == 1 {
			lines := append([]string{name}, rosterProfile(s, report)...)
			if cost := lineCost(lines...); cost <= profiles && a.take(lines...) {
				profiles -= cost
				out.full = append(out.full, report)
				continue
			}
			rung = 2
		}
		if a.take(name, rosterHead(report)) {
			out.handles = append(out.handles, report)
			continue
		}
		out.omitted = len(reports) - i
		break
	}
	return out
}

// rosterGapLine is the ladder's THIRD rung, and the tool name in it IS the
// rung.
//
// A lead told only that some of its reports were left out has no move: it
// cannot hand work to a colleague it cannot name, and a model with no move
// invents one. `lookup_colleague` resolves any seat in the company from a
// name, a handle or a platform id, so an omission is one call away instead of
// a dead end.
func rosterGapLine(n int) string {
	return "and " + strconv.Itoa(n) + " more — `lookup_colleague` names them"
}

// rosterHead is the ladder's SECOND rung: the one line that names a report
// and marks a human one.
//
// The **human teammate** marker survives the cap deliberately. It is not
// decoration: it is how the lead knows this colleague answers asynchronously
// over an external surface rather than taking an engine turn, and a name
// stripped of it reads as an agent that can simply be asked.
func rosterHead(report *org.Role) string {
	head := "- **" + report.Name + "**"
	switch {
	case report.IsHuman():
		head += " (" + report.Handle() + ") — **human teammate**"
	case report.DeclaredHandle != "":
		head += " (" + report.DeclaredHandle + ")"
	}
	return head
}

// rosterProfile is the ladder's FIRST rung: the head line plus everything a
// lead reasons over when it decides who to give a piece of work to.
//
// Contact identities render generically from the seat's resolved identities
// (${VAR} references resolved, unresolved ones omitted — never shown
// verbatim, since a literal "${SLACK_ID}" in a prompt is a mention that can
// never match an account), so this renderer is tied to no one platform.
func rosterProfile(s Seat, report *org.Role) []string {
	parts := []string{rosterHead(report)}
	if report.Backstory != "" {
		parts = append(parts, "  - Background: "+report.Backstory)
	}
	if report.Goal != "" {
		parts = append(parts, "  - Goal: "+report.Goal)
	}
	if len(report.Responsibilities) > 0 {
		parts = append(parts, "  - Responsibilities: "+strings.Join(report.Responsibilities, "; "))
	}
	if !report.IsHuman() {
		return parts
	}
	addressable := false
	if report.Contact != nil {
		seen := make(map[string]bool)
		for _, id := range report.Contact.ResolvedIdentities(s.Env) {
			// One account id can serve several transports (an
			// Atlassian id covers both Jira and Confluence). Show
			// each id once, labelled by its first transport —
			// repeating it reads as two different accounts.
			//
			// An UNREACHABLE transport is left out entirely: an
			// operator id identifies this person on the engine's own
			// surface, where nothing is ever sent, so listing it under
			// how to reach them would have an agent @-mentioning a
			// name no platform resolves.
			if seen[id.ExternalID] || !id.Transport.Reachable() {
				continue
			}
			seen[id.ExternalID] = true
			addressable = true
			parts = append(parts, "  - "+capitalize(string(id.Transport))+" ID: "+id.ExternalID)
		}
	}
	if report.Availability != "" {
		parts = append(parts, "  - Availability: "+report.Availability)
	}
	if addressable {
		parts = append(parts,
			"  - Working with them: hand work over in the PM tool "+
				"and @-mention them on their team's chat or issue "+
				"tracker; they reply asynchronously — don't expect an "+
				"engine turn from them.")
		return parts
	}
	// NOBODY TO @-MENTION. A person may hold an operator credential and
	// no chat account at all — they read their dashboard inbox — and
	// telling an agent to mention them anyway produces a message
	// addressed to a handle that resolves to nobody, which reads to
	// everyone else as work handed over.
	parts = append(parts,
		"  - Working with them: hand work over in the PM tool and "+
			"assign it to them; they have no chat or code-host account "+
			"here, so there is nobody to @-mention. They read their "+
			"queue and reply asynchronously — don't expect an engine "+
			"turn from them.")
	return parts
}

// BuildRosterSection renders the team roster — the ladder's three rungs, in
// order.
//
// For a seat with no direct reports: nothing.
//
// For a lead: a compact profile (backstory, goal, responsibilities) per
// member for as many as the allowance pays for, so the lead can reason about
// who to assign work to without a separate knowledge fetch; then a name and a
// handle each; then one line saying how many are left and which tool names
// them. Human members render with their external contact identities, their
// availability, and how to hand them work — assignment in the PM tool plus a
// mention, because there is no engine task assignment that reaches a person.
//
// It renders a DECISION it did not make. [planRoster] made it, from the same
// counter [BuildIdentitySection] spends, which is why the two sections can
// never disagree about who is on this team.
func BuildRosterSection(s Seat, r roster) []string {
	if len(r.full) == 0 && len(r.handles) == 0 && r.omitted == 0 {
		return nil
	}
	parts := []string{"\n## Your Team"}
	for _, report := range r.full {
		parts = append(parts, rosterProfile(s, report)...)
	}
	for _, report := range r.handles {
		parts = append(parts, rosterHead(report))
	}
	if r.omitted > 0 {
		parts = append(parts, "- "+rosterGapLine(r.omitted))
	}
	return parts
}

// capitalize upper-cases the first rune and lower-cases the rest.
//
// It is what renders "Github ID:" rather than "GitHub ID:" — carried as-is
// because the roster line is prompt text, and prompt text is not where a
// cosmetic improvement is worth an unverifiable behaviour change.
func capitalize(s string) string {
	if s == "" {
		return ""
	}
	runes := []rune(strings.ToLower(s))
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

// BuildHumanColleaguesNote renders the working-with-humans contract, present
// only in mixed orgs.
//
// A platform-level note, not role prose: it tells every agent how human seats
// behave (external surfaces, asynchronous replies, no A2A, no engine turns)
// so handoffs and mentions are shaped correctly on the first attempt. Empty
// when the org has no human seats, which keeps pure-agent prompts unchanged.
func BuildHumanColleaguesNote(s Seat) []string {
	if !s.ok() {
		return nil
	}
	human := false
	for r := range s.Org.AllRoles() {
		if r.IsHuman() {
			human = true
			break
		}
	}
	if !human {
		return nil
	}
	return []string{
		"\n## Human colleagues",
		"Some seats in this org are held by human teammates — marked " +
			"**(human)** in your identity and roster, and " +
			"`kind: human` in `lookup_colleague` results. When you need one " +
			"of them:",
		"- Reach them where humans read: an @-mention on the team's " +
			"chat, or a comment on the issue / doc where the work lives. " +
			"They are NOT on A2A.",
		"- They reply asynchronously (think hours, not seconds). Put " +
			"the full context they need into your message, then finish " +
			"your turn — their reply re-triggers you. Never wait or poll.",
		"- Hand them work through the PM tool (assign + mention), not " +
			"through engine task assignment.",
	}
}

// BuildOrgMissionVisionSection renders org-wide mission + vision, when set.
//
// Both fields are short by convention (a sentence or two each); inlining is a
// token-cheap way to keep them in front of the executor without a knowledge
// round-trip.
func BuildOrgMissionVisionSection(s Seat) []string {
	if !s.ok() || (s.Org.Mission == "" && s.Org.Vision == "") {
		return nil
	}
	parts := []string{"\n## Company Context"}
	if s.Org.Mission != "" {
		parts = append(parts, "**Mission:** "+s.Org.Mission)
	}
	if s.Org.Vision != "" {
		parts = append(parts, "**Vision:** "+s.Org.Vision)
	}
	return parts
}

// BuildPoliciesSection inlines the full company-policy text.
//
// Policies are short by convention — a couple of sentences each — so the full
// text is inlined rather than truncated to one-liners, keeping the executor's
// context complete.
func BuildPoliciesSection(s Seat) []string {
	if !s.ok() || len(s.Org.Policies) == 0 {
		return nil
	}
	parts := []string{"\n## Company policies"}
	for _, policy := range s.Org.Policies {
		parts = append(parts, "- "+strings.TrimSpace(policy))
	}
	return parts
}

// BuildRoleProfileSection renders the agent's own backstory, responsibilities
// and behavioral guidelines. Goal is already covered by
// [BuildIdentitySection] and is not repeated here.
func BuildRoleProfileSection(s Seat) []string {
	if !s.ok() {
		return nil
	}
	var parts []string
	if s.Role.Backstory != "" {
		parts = append(parts, "\n## Your Background", s.Role.Backstory)
	}
	if len(s.Role.Responsibilities) > 0 {
		parts = append(parts, "\n## Your Responsibilities")
		for _, item := range s.Role.Responsibilities {
			parts = append(parts, "- "+item)
		}
	}
	if len(s.Role.BehavioralGuidelines) > 0 {
		parts = append(parts, "\n## Behavioral Guidelines")
		for _, item := range s.Role.BehavioralGuidelines {
			parts = append(parts, "- "+item)
		}
	}
	return parts
}

// BuildUnitContextSection renders the containing unit's purpose and goals.
//
// Nothing for a root-level seat (no containing unit), or for a unit that has
// neither a purpose nor any goals.
func BuildUnitContextSection(s Seat) []string {
	unit := s.unit()
	if unit == nil || (unit.Purpose == "" && len(unit.Goals) == 0) {
		return nil
	}
	parts := []string{"\n## Your Unit (" + unit.Name + ")"}
	if unit.Purpose != "" {
		parts = append(parts, "**Purpose:** "+unit.Purpose)
	}
	if len(unit.Goals) > 0 {
		parts = append(parts, "**Goals:**")
		for _, goal := range unit.Goals {
			parts = append(parts, "- "+goal)
		}
	}
	return parts
}

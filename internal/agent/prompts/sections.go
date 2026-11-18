package prompts

import (
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/toolname"
)

// The shared section builders. Each returns the section's lines, or nil when
// the section does not apply: an empty policy list leaves no heading behind.
// They are prompt text, so they live here, under the package's rule (package
// doc, "The engine states only what the model cannot know").

// BuildIdentitySection renders the executor's professional identity: role,
// company, unit, goal, manager and direct reports, enough to decide what to
// do. Backstory, guidelines and team goals render in the role-profile and
// unit-context sections.
//
// Its heading is `##`, a peer of every other section
// (TestEveryHeadingAPromptBuilderEmitsIsASibling): a `#` would make the
// policies, roster and turn contract parts of the identity, to the model and
// to the dashboard's prompt outline alike.
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

// BuildIdentityLine is the one-line identity for the review and onboarding
// phases: their user message carries what they are about, so the reporting
// line and unit context are not needed.
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

// managerLabel and reportsLabel make both identity renderings answer alike
// for a top-level seat ("None (top-level)", never "none"): an executor and a
// reviewer reading the chart differently is a difference no phase test checks.

func managerLabel(manager *org.Role) string {
	if manager == nil {
		return "None (top-level)"
	}
	return seatLabel(manager)
}

// reportsLabel renders the roster's decision as a name list: exactly the
// seats [BuildRosterSection] renders, closed by that section's gap line, so it
// never names a colleague the roster omits or counts as omitted.
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
// A lead's direct reports render twice: as names on the identity section's
// "Direct reports:" line and as profiles under "## Your Team". Unbounded, both
// grow with the team until an ordinary unit's lead cannot be prompted at all.
//
// This bounds the RENDERING only, never `manages`: who a seat may direct work
// to is an authorization fact read from the chart, and a seat still manages
// every report the allowance had no room to describe.

// charsPerToken is the approximation every token figure in this package uses:
// four Unicode code points to a token. Not a tokenizer, which would tie a pure
// text package to one vendor's vocabulary; code points rather than bytes
// because these prompts are dense with em dashes and arrows, and bytes would
// inflate every figure ~4% and quietly re-tighten every budget in
// budget_test.go.
const charsPerToken = 4

// approxTokens is the one measure the allowance below and budget_test.go's
// budgets share: a roster planned against one approximation and asserted
// against another holds in the test and not in the prompt.
func approxTokens(s string) int { return utf8.RuneCountInString(s) / charsPerToken }

// rosterAllowanceTokens is the ONE budget the identity line and the roster
// section spend together: a ceiling, not a target.
//
// 1,000 for what it buys, measured on fixtures_test.go's `profiledReports`: a
// full profile costs ~119 tokens for an agent and ~148 for a person, a
// name-and-handle line ~11 and ~18, the closing line ~11. So it pays for three
// profiles and names ~40 more colleagues: an ordinary unit (25 reports) omits
// nobody. More buys backstories on every turn, which `lookup_colleague`
// answers on the one turn that needs them. The whole turn's budget is this
// plus the prose (TestTheWholeTurnStaysUnderBudget).
const rosterAllowanceTokens = 1000

// rosterAllowance is that budget as a live counter, in CHARACTERS converted
// once here: dividing each line by four would round a fraction of every line
// away, and a long roster would lose whole reports to the rounding.
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

// take deducts what lines cost and reports whether the counter could pay. A
// refusal leaves the counter untouched, so the next rung down gets an honest
// chance at what is left.
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

// roster is the ladder's decision about a seat's direct reports: who renders
// in full, who as a name and a handle, and how many the closing line counts.
// Decided once and rendered by both [BuildIdentitySection] and
// [BuildRosterSection], so the identity line never names a colleague the
// roster omits or counts among the omitted.
type roster struct {
	full    []*org.Role
	handles []*org.Role
	omitted int
}

// planRoster walks a seat's reports once and spends a on them, charging each
// admitted report for both renderings: two independent caps would each spend
// their own budget and the prompt would pay the sum.
//
// The ladder only descends: a report that misses rung 1 sends every later one
// to rung 2, so a lead reads profiles, then names, then the gap. The closing
// line is reserved first, at its widest, so a truncated roster never claims to
// be the whole team. Rung 1 may spend at most half, a priority inside the one
// counter: a profile costs ~10 names, so an undivided counter buys seven
// profiles and cannot name the ninth colleague. Not knowing a report exists is
// worse than not knowing their backstory, which `lookup_colleague` answers.
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
		// What this report costs the IDENTITY line (its label and the ", "
		// joining it), charged from this same counter.
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

// rosterGapLine is the ladder's third rung, and its tool name is the point: a
// lead told only that reports were left out cannot hand work to a colleague it
// cannot name, and a model with no move invents one. `lookup_colleague`
// resolves any seat from a name, a handle or a platform id.
func rosterGapLine(n int) string {
	return "and " + strconv.Itoa(n) + " more — `" + toolname.LookupColleague + "` names them"
}

// rosterHead is the ladder's second rung: one line naming a report. The
// **human teammate** marker survives the cap because it is how the lead knows
// this colleague answers asynchronously off-engine; stripped of it, the name
// reads as an agent that can simply be asked.
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

// rosterProfile is the ladder's first rung: the head line plus what a lead
// reasons over when choosing who gets work. Contact identities render
// generically from the seat's resolved identities, unresolved ${VAR}s omitted
// (a literal "${SLACK_ID}" is a mention that never matches an account). A seat
// whose identities the directory withholds renders none ([Seat.Withheld]); on
// the engine's own chat a person is also reachable by handle ([Seat.Chatlet]).
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
	if s.withholds(report.Handle()) {
		// No identity, and not the "no account" line either, which says the
		// person reads their queue. The seat stays (work filed to it waits
		// for its next holder); the reason stays out, since a person's
		// standing is not an agent's business.
		if report.Availability != "" {
			parts = append(parts, "  - Availability: "+report.Availability)
		}
		return append(parts,
			"  - They cannot be reached on any chat or code-host account "+
				"right now: put work for this seat in the PM tool, assigned "+
				"to the seat, and expect no reply meanwhile.")
	}
	addressable := false
	if s.Chatlet {
		// The engine's own chat reads a handle, so a person is addressable
		// there whatever accounts they hold elsewhere.
		parts = append(parts, "  - Chat: @"+report.Handle())
	}
	if report.Contact != nil {
		seen := make(map[string]bool)
		for _, id := range report.Contact.ResolvedIdentities(s.Env) {
			// One account id can serve several transports (an Atlassian
			// id covers Jira and Confluence): show it once, under its
			// first transport, since repeating it reads as two accounts.
			// Every transport listed must be one a message reaches.
			if seen[id.ExternalID] {
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
	if s.Chatlet {
		// Nothing more on the engine's own chat: the handle reaches them,
		// and the rest is the `## Human colleagues` note's, said once.
		return parts
	}
	if addressable {
		// Nothing more for a person with an account: how a human is reached
		// and handed work is the `## Human colleagues` note's, said once
		// rather than per profile out of the allowance.
		return parts
	}
	// Nobody to @-mention, on a vendor's chat or none: the one way this person
	// differs from the note. Telling an agent to mention them anyway addresses
	// a handle that resolves to nobody, which reads to everyone else as work
	// handed over.
	return append(parts,
		"  - No chat or code-host account, so nobody to @-mention: assign "+
			"their work to them in the PM tool, where they read it.")
}

// BuildRosterSection renders the team roster, the ladder's three rungs in
// order: nothing for a seat with no reports; for a lead, a compact profile per
// member while the allowance pays (so it can assign work without a knowledge
// fetch), then a name and handle each, then one line counting the rest and
// naming the tool that finds them. A person renders with their contact
// identities and availability, plus how their work reaches them only where
// they cannot be mentioned; the rest is the `## Human colleagues` note's.
//
// It renders [planRoster]'s decision, made from the counter
// [BuildIdentitySection] spends too, so the two never disagree about the team.
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

// capitalize upper-cases the first rune and lower-cases the rest, so it
// renders "Github ID:": a model reads the label the same either way, and only
// the id after it has to be exact.
func capitalize(s string) string {
	if s == "" {
		return ""
	}
	runes := []rune(strings.ToLower(s))
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

// BuildHumanColleaguesNote renders the working-with-humans contract, only in
// an org with a human seat, so pure-agent prompts are unchanged. It tells
// every agent how human seats behave (external surfaces, asynchronous
// replies, no A2A, no engine turns) so handoffs and mentions land first time.
//
// It is the one copy: a roster profile says only how its person differs from
// it. It names both markers, since the identity line and the roster mark a
// person differently ([seatLabel], [rosterHead]).
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
	note := "Seats marked (human) or **human teammate** — `kind: human` in " +
		"`" + toolname.LookupColleague + "` — are held by people. Reach them with an " +
		"@-mention on the team's chat or a comment where the work lives, " +
		"never over A2A, and hand them work by assigning it in the PM " +
		"tool. They reply asynchronously: give them the full context, " +
		"then finish your turn — their reply wakes you."
	if s.Chatlet {
		// On the engine's own chat a person answers to their profile's
		// handle and a DM is a tool of its own: said once, here.
		note += " On the company's own chat, mention a person by the @handle " +
			"their profile gives, or message them directly with `" + toolname.SendDM + "`."
	}
	return []string{"\n## Human colleagues", note}
}

// BuildOrgMissionVisionSection renders org-wide mission and vision, when set:
// short by convention, so inlined rather than left to a knowledge fetch.
//
// No line of either stands on its own: a company line at the margin could
// read as the prompt's own, a `## ` heading included. The mission is kept to
// one line ([inline]); a multi-line vision is QUOTED under its label, every
// line, so each stays inside the vision ([labelled]).
func BuildOrgMissionVisionSection(s Seat) []string {
	if !s.ok() || (s.Org.Mission == "" && s.Org.Vision == "") {
		return nil
	}
	parts := []string{"\n## Company Context"}
	if s.Org.Mission != "" {
		parts = append(parts, "**Mission:** "+inline(s.Org.Mission))
	}
	if s.Org.Vision != "" {
		parts = append(parts, labelled("**Vision:**", s.Org.Vision))
	}
	return parts
}

// BuildPoliciesSection inlines the full company-policy text, short by
// convention, rather than one-line truncations. Each policy is one list item
// on one line ([inline]): a line break would end the item, and what followed
// would read as a line of the prompt.
func BuildPoliciesSection(s Seat) []string {
	if !s.ok() || len(s.Org.Policies) == 0 {
		return nil
	}
	parts := []string{"\n## Company policies"}
	for _, policy := range s.Org.Policies {
		parts = append(parts, "- "+inline(policy))
	}
	return parts
}

// isLineBreak reports whether r ends a line: a line feed, a carriage return,
// or one of the separators Unicode defines as ending one.
func isLineBreak(r rune) bool {
	switch r {
	case '\n', '\r', '\v', '\f', '\u0085', '\u2028', '\u2029':
		return true
	}
	return false
}

// inline is text the company wrote, on ONE line: every line of it trimmed and
// joined by a space, the blank ones dropped. Text written on one line comes
// back as it was, trimmed.
func inline(text string) string {
	lines := strings.FieldsFunc(text, isLineBreak)
	kept := lines[:0]
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, " ")
}

// labelled is text the company wrote, under its label: on the label's own
// line when it is one line, and otherwise QUOTED beneath it, every line
// prefixed `> ` (a blank one `>`), so no line of it begins at the margin.
func labelled(label, text string) string {
	text = strings.TrimSpace(text)
	if !strings.ContainsFunc(text, isLineBreak) {
		return label + " " + text
	}
	// One break per line ending, whichever spelling it came in.
	text = strings.Map(func(r rune) rune {
		if isLineBreak(r) {
			return '\n'
		}
		return r
	}, strings.ReplaceAll(text, "\r\n", "\n"))
	var b strings.Builder
	b.WriteString(label)
	for line := range strings.SplitSeq(text, "\n") {
		b.WriteString("\n>")
		if line = strings.TrimSpace(line); line != "" {
			b.WriteString(" " + line)
		}
	}
	return b.String()
}

// The agent's own role profile renders as three sections: backstory,
// responsibilities and behavioral guidelines (the goal is
// [BuildIdentitySection]'s). Three builders, not one, because each is an
// outline section under its own heading; one list of lines would leave the
// outline guessing the headings from the text.

func buildBackgroundSection(s Seat) []string {
	if !s.ok() || s.Role.Backstory == "" {
		return nil
	}
	return []string{"\n## Your Background", s.Role.Backstory}
}

func buildResponsibilitiesSection(s Seat) []string {
	if !s.ok() || len(s.Role.Responsibilities) == 0 {
		return nil
	}
	return bulleted("\n## Your Responsibilities", s.Role.Responsibilities)
}

func buildGuidelinesSection(s Seat) []string {
	if !s.ok() || len(s.Role.BehavioralGuidelines) == 0 {
		return nil
	}
	return bulleted("\n## Behavioral Guidelines", s.Role.BehavioralGuidelines)
}

// bulleted is a heading followed by one "- " line per item.
func bulleted(heading string, items []string) []string {
	parts := make([]string, 0, len(items)+1)
	parts = append(parts, heading)
	for _, item := range items {
		parts = append(parts, "- "+item)
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

// escalationTool is the tool the escalation guidance names — the one a seat
// asks a decision through. The guidance renders only when the seat holds it.
const escalationTool = toolname.CommentOnWorkItem

// BuildEscalationSection is how a seat asks for a decision it cannot make.
//
// The engine says it, not the role's guidelines, because the shape is the
// engine's: the person answers by choosing an option and the asker is woken
// with the choice; what a role escalates stays the role's. The shape's fields
// are the `decision` argument's schema, which reaches the model with the tool,
// so this says only WHEN to ask, that every question to a person is asked so,
// a request included (ADR-0100), and that asking ends the branch: a seat that
// asked and kept going has chosen for the person or waits in a turn that
// cannot receive the reply, which is a wake.
//
// Gated on the tool: a prompt never tells a model to call what it lacks.
func BuildEscalationSection(availableTools []string) []string {
	if !slices.Contains(availableTools, escalationTool) {
		return nil
	}
	return []string{
		"\n## When a decision is not yours to make",
		"When your work reaches a choice above your authority, one that trades " +
			"off something your role does not own, or one you are genuinely " +
			"torn on, ask the person who decides with an `ask` and a " +
			"`decision` — on the work item with `" + escalationTool + "`, or " +
			"with `" + toolname.CreateWorkItem + "` when there is no item yet.",
		"Ask a person EVERY question this way, a request included: offer " +
			"the answers you expect, with the one you recommend and why.",
		"Then END THIS TURN BLOCKED ON THAT BRANCH. Do not pick an option " +
			"yourself and do not wait or poll: finish whatever does not depend " +
			"on the answer, and stop. Their answer wakes you with the option " +
			"they chose.",
	}
}

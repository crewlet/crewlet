package prompts

import (
	"strings"
	"unicode"

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
// Plan needs enough to decide *what* to do: role, company, unit, goal,
// manager, and direct reports (for delegation decisions). The long-form
// backstory / guidelines / team goals render in the role-profile and
// unit-context sections instead.
func BuildIdentitySection(s Seat) []string {
	if !s.ok() {
		return nil
	}
	parts := []string{
		"# Your Identity",
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
		"**Direct reports:** "+reportsLabel(s.reports()),
	)
	if unit != nil && unit.Channel != "" {
		parts = append(parts, "**Team channel:** "+unit.Channel)
	}
	return parts
}

// BuildIdentityLine is the ultra-compact one-line identity for the Execute,
// Review and Onboarding phases.
//
// Those phases already carry the plan (and, for Review, Execute's artifact)
// in the user message, so the full reporting line and unit context are not
// needed. One sentence is enough to ground the model in "who is writing this".
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
// An executor that read its own chart differently from its planner would be a
// difference no assertion in a phase test is looking for.

func managerLabel(manager *org.Role) string {
	if manager == nil {
		return "None (top-level)"
	}
	return seatLabel(manager)
}

func reportsLabel(reports []*org.Role) string {
	if len(reports) == 0 {
		return "None"
	}
	labels := make([]string, 0, len(reports))
	for _, r := range reports {
		labels = append(labels, seatLabel(r))
	}
	return strings.Join(labels, ", ")
}

// BuildRosterSection renders the team roster.
//
// For a seat with no direct reports: nothing.
//
// For a lead: names + handles + a compact per-member profile (backstory,
// goal, responsibilities) so the lead can reason about who to assign work to
// without a separate knowledge fetch. Human members render with their
// external contact identities, their availability, and how to hand them work
// — assignment in the PM tool plus a mention, because there is no engine task
// assignment that reaches a person.
//
// Contact identities render generically from the seat's resolved identities
// (${VAR} references resolved, unresolved ones omitted — never shown
// verbatim, since a literal "${SLACK_ID}" in a prompt is a mention that can
// never match an account), so this renderer is tied to no one platform.
func BuildRosterSection(s Seat) []string {
	reports := s.reports()
	if len(reports) == 0 {
		return nil
	}
	parts := []string{"\n## Your Team"}
	for _, report := range reports {
		head := "- **" + report.Name + "**"
		switch {
		case report.IsHuman():
			head += " (" + report.Handle() + ") — **human teammate**"
		case report.DeclaredHandle != "":
			head += " (" + report.DeclaredHandle + ")"
		}
		parts = append(parts, head)
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
			continue
		}
		if report.Contact != nil {
			seen := make(map[string]bool)
			for _, id := range report.Contact.ResolvedIdentities(s.Env) {
				// One account id can serve several transports (an
				// Atlassian id covers both Jira and Confluence). Show
				// each id once, labelled by its first transport —
				// repeating it reads as two different accounts.
				if seen[id.ExternalID] {
					continue
				}
				seen[id.ExternalID] = true
				parts = append(parts, "  - "+capitalize(string(id.Transport))+" ID: "+id.ExternalID)
			}
		}
		if report.Availability != "" {
			parts = append(parts, "  - Availability: "+report.Availability)
		}
		parts = append(parts,
			"  - Working with them: hand work over in the PM tool "+
				"and @-mention them on their team's chat or issue "+
				"tracker; they reply asynchronously — don't expect an "+
				"engine turn from them.")
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
// token-cheap way to keep them in front of the planner without a knowledge
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
// text is inlined rather than truncated to one-liners, keeping the planner's
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

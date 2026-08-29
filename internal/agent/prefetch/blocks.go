package prefetch

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/learning"
)

// The four blocks with no auxiliary judgement in them — plus one optional
// summary pass over the episodes.

// RecallCharBudget, ProfileCharBudget and SkillsCharBudget cap each rendered
// block.
//
// Smaller than memory's and knowledge's, and deliberately: these three are
// BACKGROUND — what happened before, who this is, what procedures exist —
// while those two are the ones the planner reasons from. A prompt where the
// background outweighs the task is one where the model plans the background.
const (
	RecallCharBudget  = 1200
	ProfileCharBudget = 1200
	SkillsCharBudget  = 1200

	// recallHits is how many past turns are recalled.
	//
	// Three, and low on purpose: an episode is a whole turn compressed
	// into two sentences, so each one is expensive to read and the fourth
	// most-similar is rarely the one that helps.
	recallHits = 3

	// recallSummaryTokens caps the optional summary of those hits.
	recallSummaryTokens = 400

	// maxRenderedTraits caps one counterparty's traits.
	//
	// A profiler pass contributes at most a handful of keys, but the store
	// merges them and the set only grows — a counterparty worked with for
	// months accumulates traits without limit, and every one of them
	// landed in every prompt that mentioned them. The cap is on the
	// RENDERING rather than the record: the observations are worth
	// keeping, the prompt is what has a budget. It says how many it
	// dropped, so a reader is not misled into thinking that is all the
	// seat knows.
	maxRenderedTraits = 24

	// skillsListed caps the skills catalogue.
	//
	// The block is a MENU, not the skills: each line is a name and a
	// description, and the seat loads the one it wants. Twelve is enough
	// for a seat that has been running for months to see its whole
	// repertoire without the menu becoming the prompt.
	skillsListed = 12
)

// EmptyRecallHint is what the recall block says when the search was skipped
// on a thin trigger.
const EmptyRecallHint = "(no similar prior work surfaced at turn start — " +
	"query your episode history once you know what this task actually " +
	"involves)"

// recallSummarySystemPrompt compresses episode bullets.
const recallSummarySystemPrompt = `You compress an AI agent's record of its own past turns into a short briefing for its next one.

Output format (strict):
- At most 4 short bullet lines, no preamble and no conclusion.
- Each line: what the past turn was about, and what came of it.
- Keep identifiers, tool names and outcomes verbatim.
- Drop anything that does not bear on doing similar work again.

If none of the past turns would help with the current task, output nothing at all.`

// episodeRecall renders similar prior work.
func (f *Fetcher) episodeRecall(ctx context.Context, r Request) string {
	if f.src.Episodes == nil || r.Seat == nil || strings.TrimSpace(r.Task) == "" {
		return ""
	}
	handle := r.Seat.Handle()
	if handle == "" {
		return ""
	}
	if r.RequiresRecon {
		// Same gate as memory and knowledge, and the same reason: a
		// similarity search against a pointer returns the seat's most
		// recent work rather than its most relevant.
		return EmptyRecallHint
	}
	vector, ok := f.embed(ctx, r.Task)
	if !ok {
		// NO FALLBACK TO RECENCY. Episode recall's whole claim is "this
		// resembles what you are doing now"; the three most recent turns
		// carry no such claim, and a planner told they are similar work
		// will treat them as precedent.
		return ""
	}
	hits, err := f.src.Episodes.Recall(ctx, learning.RecallQuery{
		Handle: handle, Embedding: vector, Limit: recallHits,
	})
	if err != nil {
		log.Warn("episode_recall_failed", "seat", handle, "error", err.Error())
		return ""
	}
	if len(hits) == 0 {
		return ""
	}

	bullets := make([]string, 0, len(hits))
	for _, hit := range hits {
		bullets = append(bullets, renderEpisode(hit))
	}
	raw := budget(bullets, RecallCharBudget)
	if raw == "" || !f.src.SummarizeEpisodes {
		return raw
	}
	// THE SUMMARY IS OPTIONAL AND ITS FAILURE IS FREE: the raw bullets are
	// already a usable block, so a model that is slow or unreachable costs
	// verbosity rather than the block.
	summary, ok := f.auxCall(ctx, r.Seat, recallSummarySystemPrompt,
		"Current task:\n"+truncate(r.Task, 800)+
			"\n\nPast turns by this agent:\n"+raw+
			"\n\nBriefing:", recallSummaryTokens)
	if !ok || strings.TrimSpace(summary) == "" {
		return raw
	}
	return summary
}

// renderEpisode renders one past turn.
//
// The TASK and the OUTCOME, because those are the two things that make a
// past turn useful: what it was, and whether it worked. The tool sequence
// rides along because it is the cheapest possible answer to "how did I do
// this last time".
func renderEpisode(hit learning.Hit) string {
	ep := hit.Episode
	summary := collapse(firstNonEmpty(ep.TaskSummary, ep.PlanSummary))
	if summary == "" {
		return ""
	}
	line := "- " + truncate(summary, 300)
	var notes []string
	if ep.ReviewOutcome != "" {
		notes = append(notes, "outcome: "+ep.ReviewOutcome)
	}
	if len(ep.ToolSequence) > 0 {
		notes = append(notes, "tools: "+strings.Join(ep.ToolSequence, " → "))
	}
	if len(notes) > 0 {
		line += " _(" + strings.Join(notes, "; ") + ")_"
	}
	return line
}

// counterpartyProfile renders what this seat has observed about whoever
// triggered the turn.
//
// ONE BLOCK PER DISTINCT SENDER. A coalesced trigger is several people
// speaking, and rendering only the latest would hand the planner a profile
// of whoever happened to speak last while it answers all of them.
func (f *Fetcher) counterpartyProfile(ctx context.Context, r Request) string {
	if f.src.Counterparties == nil || r.Seat == nil || len(r.Senders) == 0 {
		return ""
	}
	observer := r.Seat.Handle()
	if observer == "" {
		return ""
	}
	var (
		blocks []string
		seen   = map[learning.Subject]bool{}
	)
	for _, subject := range r.Senders {
		if !subject.Valid() || seen[subject] {
			continue
		}
		seen[subject] = true
		profile, ok, err := f.src.Counterparties.Get(ctx, observer, subject)
		if err != nil {
			log.Warn("counterparty_lookup_failed", "observer", observer,
				"error", err.Error())
			continue
		}
		if !ok {
			// Nobody has been profiled yet, which is the ordinary case
			// for a first interaction and not worth a line saying so.
			continue
		}
		blocks = append(blocks, renderProfile(profile))
	}
	return budget(blocks, ProfileCharBudget)
}

// renderProfile renders one counterparty.
//
// THE SUBJECT HEADER IS NOT OPTIONAL. This block arrives with no
// conversational context around it, so a list of traits with no name on it
// tells the planner what somebody prefers without saying who — which is
// worse than nothing, because it invites applying it to whoever is asking.
func renderProfile(p learning.Profile) string {
	var b strings.Builder
	b.WriteString("Subject: " + firstNonEmpty(subjectLabel(p.Subject), "(unknown)"))
	if p.Subject.Platform != "" {
		b.WriteString("\nPlatform: " + p.Subject.Platform)
	}
	b.WriteString("\n")

	if len(p.Traits) == 0 {
		b.WriteString("Observed by you: (no traits yet)\n")
	} else {
		b.WriteString("Observed by you:\n")
		// SORTED, because a map's order is randomised and a profile that
		// reorders itself between two otherwise-identical turns breaks
		// the provider's prompt cache for nothing.
		keys := slices.Sorted(maps.Keys(p.Traits))
		shown := min(len(keys), maxRenderedTraits)
		for _, key := range keys[:shown] {
			b.WriteString("  - " + key + ": " + renderTrait(p.Traits[key]) + "\n")
		}
		if dropped := len(keys) - shown; dropped > 0 {
			b.WriteString("  … and " + strconv.Itoa(dropped) +
				" more observed traits\n")
		}
	}
	b.WriteString("(interactions: " + strconv.Itoa(p.InteractionCount))
	if !p.LastUpdatedAt.IsZero() {
		b.WriteString(", last updated: " + p.LastUpdatedAt.UTC().Format("2006-01-02"))
	}
	b.WriteString(")")
	return b.String()
}

// renderTrait renders one trait value, whatever shape the model invented.
func renderTrait(value any) string {
	switch v := value.(type) {
	case string:
		return collapse(v)
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			parts = append(parts, collapse(fmt.Sprint(item)))
		}
		return strings.Join(parts, ", ")
	default:
		return collapse(fmt.Sprint(v))
	}
}

// synthesizedSkills renders the procedures this seat wrote for itself.
func (f *Fetcher) synthesizedSkills(ctx context.Context, r Request) (string, []string) {
	if f.src.Skills == nil || r.Seat == nil {
		return "", nil
	}
	handle := r.Seat.Handle()
	if handle == "" {
		return "", nil
	}
	// ARCHIVED EXCLUDED, STALE KEPT. Archived is "aged out of the
	// catalogue" and bringing one back is an operator's decision; stale is
	// a marker on a skill that still works and revives the moment it is
	// used, so hiding it is how a useful skill starves.
	skills, err := f.src.Skills.List(ctx, handle, learning.ListOptions{})
	if err != nil {
		log.Warn("skills_list_failed", "seat", handle, "error", err.Error())
		return "", nil
	}
	if len(skills) == 0 {
		return "", nil
	}
	if len(skills) > skillsListed {
		skills = skills[:skillsListed]
	}
	bullets := make([]string, 0, len(skills)+1)
	ids := make([]string, 0, len(skills))
	for _, skill := range skills {
		line := renderSkill(skill)
		if line == "" {
			// A skill with no name renders nothing, so it was not
			// offered and must not be reported as used.
			continue
		}
		bullets = append(bullets, line)
		ids = append(ids, skill.ID)
	}
	// THE IDS FOLLOW THE BUDGET, which is why this takes the count back:
	// the budget drops the tail that does not fit, and a skill the model
	// never saw must not have its staleness clock reset — that is
	// precisely how a never-read skill would live for ever.
	rendered, kept := budgetN(bullets, SkillsCharBudget)
	if rendered == "" {
		return "", nil
	}
	ids = ids[:kept]
	// THE MENU NEEDS ITS VERB. A list of skill names with no instruction
	// is a list the model reads and does not act on — it has to be told
	// that loading one is a thing it can do.
	return rendered + "\nLoad any of these by name with your skill tool " +
		"before doing the work it covers.", ids
}

// renderSkill renders one skill as a menu line.
func renderSkill(s learning.Skill) string {
	name := strings.TrimSpace(s.Name)
	if name == "" {
		return ""
	}
	line := "- **" + name + "**"
	if description := collapse(s.Description); description != "" {
		line += ": " + truncate(description, 200)
	}
	if s.State == learning.SkillStale {
		// MARKED, not hidden. The seat can still load it — loading is
		// what revives it — and knowing it has gone unused is exactly
		// the context for deciding whether to.
		line += " _(unused lately)_"
	}
	return line
}

// onboardingHint renders the first-turn nudge, for a seat that has not been
// through it.
//
// The MARKER IS READ, not assumed: a seat is onboarded per ORG CHAIN, so a
// reorganisation legitimately un-onboards a seat whose management line
// changed, and a hint rendered from a stale answer would either nag a
// settled agent forever or skip the pass for one that genuinely moved.
func (f *Fetcher) onboardingHint(ctx context.Context, r Request) string {
	if f.src.Onboarding == nil || r.Org == nil || r.Seat == nil || r.AgentID == "" {
		return ""
	}
	hint := learning.Hint(r.Org, r.Seat)
	if hint == "" {
		return ""
	}
	done, err := f.src.Onboarding.Onboarded(ctx, r.AgentID, learning.ChainHash(r.Org, r.Seat))
	if err != nil {
		// FAIL CLOSED, which here means rendering nothing. The
		// alternative nags a seat that finished onboarding months ago on
		// every turn a database blip lasts, and a missed hint costs one
		// turn of context while a false one costs a paragraph of every
		// prompt.
		log.Warn("onboarding_marker_unreadable", "agent_id", r.AgentID,
			"error", err.Error())
		return ""
	}
	if done {
		return ""
	}
	return hint
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

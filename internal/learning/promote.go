package learning

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// Cross-agent promotion: what a whole team worked out, written where a person
// can read it.
//
// # Why this is a knowledge-base page and not a skill row
//
// Every other skill here is AGENT-SCOPE: one seat's row, in one seat's
// catalogue, loaded into one seat's prompt. That is deliberate — a skill is a
// procedure a particular seat follows, and there is no unit-scope skill table
// because a procedure four seats share is a procedure the TEAM has, which
// makes it documentation rather than per-agent memory.
//
// So the output is a DRAFT PAGE under the unit's `Auto-Drafted Skills`
// parent, which the Plan-phase knowledge search deliberately excludes: an
// unvetted draft never reaches another agent. A unit lead reviews it and
// publishes by moving it out of that parent, at which point it is an ordinary
// knowledge-base page every seat can find. Nothing here promotes itself past
// a person.
//
// # What "cross-agent" has to mean
//
// DISTINCT SEATS, not skills. One seat that drafted four near-identical
// skills is a catalogue that needs curating, not a team convergence — and
// counting rows rather than owners would promote it, presenting one agent's
// habit as what the unit does. `min_sibling_count` is a count of AGENTS.
//
// # The knobs this closes
//
// `skill_promotion.enabled`, `min_sibling_count`, `jaccard_threshold` and
// `budget_tokens` all validated, shipped in the example company, and had no
// reader outside internal/config: `Skills.ListFor` existed for this pass and
// had no caller anywhere.

// PromotionInterval is how often the promotion pass runs.
//
// DAILY, unlike the clustering pass's hourly tick, and for a reason the
// cadences make plain: clustering waits for one seat to repeat itself, which
// can happen in an afternoon, while this waits for THREE SEATS to
// independently arrive at the same procedure — a thing that takes weeks. An
// hourly tick would pay a catalogue scan per unit per hour to reach the same
// answer twenty-four times.
const PromotionInterval = 24 * time.Hour

// DefaultPromotionTokens caps one promotion draft.
//
// Larger than a single-turn synthesis draft, because the prompt carries
// several agents' whole procedures and the answer is a page a person reads
// rather than a body a model follows.
const DefaultPromotionTokens = 4000

// DefaultMinSiblings is how many distinct seats must converge. Below three it
// is a coincidence — two seats sharing a procedure is as likely to be two
// seats copying one trigger as it is a team practice.
const DefaultMinSiblings = 3

// DefaultPromotionJaccard pools two seats' skills. Deliberately the same
// value clustering uses: the question is identical — "is this the same kind
// of work" — and two constants for one question drift.
const DefaultPromotionJaccard = 0.6

// PromotionUnit is one unit the pass considers, resolved fresh per tick.
//
// CONTAINER RESOLVED BY THE CALLER, not by the writer: which space or project
// a unit files into is a fact about the ORG, and a vendor package that read
// it would have to know what a unit is. The writer's job is to create a page
// in a container it is handed.
type PromotionUnit struct {
	// ID is the unit's name, which is its stable identity in the event.
	ID string

	// Lead is a representative role for the unit — its lead — so a
	// dashboard can file the event somewhere. Not an author: a promotion
	// has several.
	Lead *org.Role

	// Handles are the unit's agent seats, whose catalogues are pooled.
	Handles []string

	// Container is the unit's configured knowledge container (a Confluence
	// space key, a Plane project). Empty when it has none.
	Container string

	// Hint names what an operator must set when Container is empty. A unit
	// with nowhere to file is SOFT-SKIPPED with this in the log rather than
	// failing the pass: a company that configured knowledge for one team
	// and not another is a supported state, and a hard failure would stop
	// the configured team's promotions too.
	Hint string
}

// PromotionWriter creates the draft in whichever knowledge base the company
// runs.
//
// ONE METHOD, and cross-tick dedup is behind it. The pass re-clusters the
// same persisted rows every tick, so without dedup one converging team would
// yield one draft per day forever. Each backend already has the mechanism —
// Confluence refuses a duplicate title in a space, Plane's external id is
// unique per project — so the honest place for it is the writer, which
// returns the EXISTING page rather than an error when the draft is already
// there.
type PromotionWriter interface {
	// CreateDraft creates the draft under the container's auto-drafted
	// parent, or returns the one already there for this name.
	//
	// The bool reports whether this call CREATED it. A pass that published
	// SkillPromoted on every tick would put the same promotion in the feed
	// daily for the life of the company.
	CreateDraft(ctx context.Context, container, name, markdown string) (knowledge.DraftPage, bool, error)
}

// PromotionUnits lists the units a pass walks, read fresh each tick.
//
// A FUNCTION for the same reason the background roster is one: an apply
// changes the org, and a pass holding the units it started with would keep
// promoting into a space the company has moved off.
type PromotionUnits func() []PromotionUnit

// Promoter distils what several seats in a unit independently learned.
type Promoter struct {
	writer PromotionWriter
	skills *Skills
	models Models
	units  PromotionUnits

	minSiblings int
	poolAt      float64
	timeout     time.Duration
	maxTokens   int
}

// PromoterOptions configures the pass. Zero values take the shipped defaults,
// which are the numbers config.DefaultLearning carries.
type PromoterOptions struct {
	Writer PromotionWriter
	Skills *Skills
	Models Models
	Units  PromotionUnits

	// MinSiblings is how many DISTINCT seats must converge; zero takes
	// DefaultMinSiblings.
	MinSiblings int

	// JaccardThreshold pools two seats' skills; zero takes
	// DefaultPromotionJaccard.
	JaccardThreshold float64

	// CallTimeout bounds one auxiliary call; zero takes DefaultAuxTimeout.
	CallTimeout time.Duration

	// MaxTokens caps one promotion draft; zero takes
	// DefaultPromotionTokens.
	MaxTokens int
}

// NewPromoter builds the pass.
func NewPromoter(opts PromoterOptions) (*Promoter, error) {
	switch {
	case opts.Writer == nil:
		return nil, fmt.Errorf("learning: skill promotion needs a knowledge " +
			"base to draft into — configure integrations.confluence or " +
			"integrations.plane, or set learning.skill_promotion.enabled: false")
	case opts.Skills == nil:
		return nil, fmt.Errorf("learning: skill promotion needs a skill store to read")
	case opts.Models == nil:
		return nil, fmt.Errorf("learning: skill promotion needs a model registry")
	case opts.Units == nil:
		return nil, fmt.Errorf("learning: skill promotion needs the company's units")
	}
	p := &Promoter{
		writer: opts.Writer, skills: opts.Skills,
		models: opts.Models, units: opts.Units,
		minSiblings: opts.MinSiblings, poolAt: opts.JaccardThreshold,
		timeout: opts.CallTimeout, maxTokens: opts.MaxTokens,
	}
	if p.minSiblings <= 0 {
		p.minSiblings = DefaultMinSiblings
	}
	if p.poolAt <= 0 {
		p.poolAt = DefaultPromotionJaccard
	}
	if p.timeout <= 0 {
		p.timeout = DefaultAuxTimeout
	}
	if p.maxTokens <= 0 {
		p.maxTokens = DefaultPromotionTokens
	}
	return p, nil
}

// Pass walks every unit, promoting at most one cluster in each.
//
// Returns the events to publish. A unit whose write FAILED contributes
// nothing and is not an error the pass reports upward: the next tick
// re-clusters the same rows and tries again, which is the retry, and failing
// the pass would take the units after it down with the one that broke.
func (p *Promoter) Pass(ctx context.Context) []events.Payload {
	var out []events.Payload
	for _, unit := range p.units() {
		if ctx.Err() != nil {
			return out
		}
		payload, err := p.promoteUnit(ctx, unit)
		if err != nil {
			log.Warn("skill_promotion_failed", "unit", unit.ID, "error", err.Error())
			continue
		}
		if payload != nil {
			out = append(out, payload)
		}
	}
	return out
}

// promoteUnit promotes one unit's strongest convergence, or reports why not.
func (p *Promoter) promoteUnit(ctx context.Context, unit PromotionUnit) (events.Payload, error) {
	if len(unit.Handles) < p.minSiblings {
		// Fewer seats than the threshold, so no cluster in this unit can
		// ever reach it. Checked before the catalogue read because it is
		// free and the read is not.
		return nil, nil
	}
	if unit.Container == "" {
		// SOFT SKIP, with the remediation in the log. A company that
		// configured knowledge for one team and not another is supported,
		// and failing here would stop the configured team's promotions.
		log.Info("skill_promotion_skipped", "unit", unit.ID,
			"reason", "no_knowledge_container", "detail", unit.Hint)
		return nil, nil
	}

	skills, err := p.skills.ListFor(ctx, unit.Handles, ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing %s's skills: %w", unit.ID, err)
	}
	clusters := poolSiblings(skills, p.poolAt)
	best, ok := strongest(clusters, p.minSiblings)
	if !ok {
		log.Debug("skill_promotion_found_nothing", "unit", unit.ID,
			"skills", len(skills), "clusters", len(clusters),
			"min_siblings", p.minSiblings)
		return nil, nil
	}

	member, err := p.models.Head(unit.Lead, phase.Auxiliary)
	if err != nil {
		return nil, fmt.Errorf("no auxiliary model for promotion: %w", err)
	}
	call, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	completion, err := member.Provider.Complete(call, llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: PromotionSystemPrompt},
			{Role: llm.RoleUser, Content: buildPromotionPrompt(unit, best)},
		},
		Temperature: llm.Temp(auxTemperature),
		MaxTokens:   p.maxTokens,
	})
	if err != nil {
		return nil, fmt.Errorf("drafting a promotion for %s: %w", unit.ID, err)
	}
	draft, ok := parseSkillDraft(completion)
	if !ok {
		// The model looked at four seats' near-identical procedures and
		// could not name a shared one. Rare and still not an error.
		log.Debug("skill_promotion_declined", "unit", unit.ID,
			"distinct_agents", best.DistinctAgents())
		return nil, nil
	}

	// THE PREFIX IS WHAT HIDES IT. The Plan-phase knowledge search excludes
	// the auto-drafted parent by title prefix, so a draft published without
	// it reaches every agent in the company unreviewed.
	title := knowledge.AutoDraftTitlePrefix + draft.Name
	page, created, err := p.writer.CreateDraft(ctx, unit.Container, title,
		renderPromotion(unit, best, draft))
	if err != nil {
		return nil, fmt.Errorf("drafting %q into %s: %w", title, unit.Container, err)
	}
	if !created {
		// Already drafted on an earlier tick. Publishing again would put
		// the same promotion in the feed every day for the life of the
		// company.
		log.Debug("skill_promotion_already_drafted", "unit", unit.ID,
			"page_id", page.ID, "title", page.Title)
		return nil, nil
	}

	log.Info("skill_promoted", "unit", unit.ID, "skill", draft.Name,
		"page_id", page.ID, "container", unit.Container,
		"siblings", len(best.Skills), "distinct_agents", best.DistinctAgents())
	return types.SkillPromoted{
		RoleName:       seatName(unit.Lead),
		UnitID:         unit.ID,
		SkillName:      draft.Name,
		PageID:         page.ID,
		PageTitle:      page.Title,
		ContainerKey:   unit.Container,
		SiblingCount:   len(best.Skills),
		DistinctAgents: best.DistinctAgents(),
	}, nil
}

// SiblingCluster is one procedure several seats arrived at independently.
type SiblingCluster struct {
	// Sequence is the representative tool run.
	Sequence []string
	// Skills are the contributing agent-scope rows.
	Skills []Skill
}

// DistinctAgents is how many different seats contributed.
//
// THE NUMBER THAT DECIDES, not len(Skills): one seat with four near-identical
// skills is a catalogue that needs curating, and promoting it would present
// one agent's habit as the team's practice.
func (c SiblingCluster) DistinctAgents() int {
	seen := map[string]struct{}{}
	for _, sk := range c.Skills {
		seen[sk.AgentHandle] = struct{}{}
	}
	return len(seen)
}

// poolSiblings groups a unit's skills by how similar their tool runs are.
//
// The same greedy single pass the episode clustering uses, over skills rather
// than turns: each skill joins the first cluster whose representative it is
// close enough to, or starts one.
func poolSiblings(skills []Skill, threshold float64) []SiblingCluster {
	var clusters []SiblingCluster
	for _, sk := range skills {
		if len(sk.ToolSequence) == 0 {
			// A skill with no recorded run cannot be compared to one, and
			// pooling it by name would group two procedures that share a
			// word. Skipped rather than made its own singleton cluster.
			continue
		}
		joined := false
		for i := range clusters {
			if jaccard(sk.ToolSequence, clusters[i].Sequence) >= threshold {
				clusters[i].Skills = append(clusters[i].Skills, sk)
				joined = true
				break
			}
		}
		if !joined {
			clusters = append(clusters, SiblingCluster{
				Sequence: slices.Clone(sk.ToolSequence),
				Skills:   []Skill{sk},
			})
		}
	}
	return clusters
}

// strongest picks the cluster with the most distinct contributors.
//
// ONE PER UNIT PER TICK, like the other passes: each promotion is an
// auxiliary call plus a page write, and a unit with two convergences gets the
// stronger one today and the other tomorrow.
func strongest(clusters []SiblingCluster, minAgents int) (SiblingCluster, bool) {
	best, bestAt := SiblingCluster{}, 0
	for _, c := range clusters {
		if agents := c.DistinctAgents(); agents >= minAgents && agents > bestAt {
			best, bestAt = c, agents
		}
	}
	return best, bestAt > 0
}

// promotionPromptSkills is how many of a cluster's skills reach the prompt.
//
// Four is enough to show what the seats agree on and where they differ, which
// is the whole question. Past that the bodies are repetition at prompt prices.
const promotionPromptSkills = 4

// PromotionSystemPrompt asks for the practice a team shares.
const PromotionSystemPrompt = `Several agents on one team independently wrote
down nearly the same procedure. Write the version the team should share, as a
page a person will review before it is published.

Answer with a JSON object and nothing else:
{"name":"kebab-case-name","description":"one line on when to use it",
 "content":"the procedure, as numbered steps"}

Write what the agents AGREE on. Where they differ, say what the difference
depends on rather than picking one — a reviewer needs to see the choice. Keep
it general: no ticket numbers, no names, no dates, nothing about one agent.

Answer exactly {} if they do not actually share a procedure — similar tools do
not always mean the same work.`

// buildPromotionPrompt renders the converging skills for the model.
func buildPromotionPrompt(unit PromotionUnit, c SiblingCluster) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Team: %s\n%d agents wrote %d skills around this tool run:\n%s\n",
		unit.ID, c.DistinctAgents(), len(c.Skills), strings.Join(c.Sequence, " -> "))
	for i, sk := range c.Skills {
		if i >= promotionPromptSkills {
			fmt.Fprintf(&b, "\n(and %d more, all similar)\n", len(c.Skills)-promotionPromptSkills)
			break
		}
		fmt.Fprintf(&b, "\n### %s (by %s)\n%s\n\n%s\n",
			sk.Name, sk.AgentHandle, sk.Description, sk.Content)
	}
	return b.String()
}

// renderPromotion is the page body, in markdown.
//
// THE PROVENANCE IS PART OF THE PAGE, not just of the event. A reviewer
// opening an auto-drafted page needs to know who converged on it and from
// what before deciding whether the team should adopt it — and the event that
// carried those numbers is in a feed they are not reading.
func renderPromotion(unit PromotionUnit, c SiblingCluster, draft skillDraft) string {
	var b strings.Builder
	fmt.Fprintf(&b, "> **Auto-drafted, not reviewed.** %d agents in %s "+
		"independently arrived at this procedure. Publish it by moving this "+
		"page out of %q; until then no agent can find it.\n\n",
		c.DistinctAgents(), unit.ID, knowledge.AutoDraftedParent)
	fmt.Fprintf(&b, "%s\n\n## Procedure\n\n%s\n\n## Where it came from\n\n",
		draft.Description, draft.Content)
	for _, sk := range c.Skills {
		fmt.Fprintf(&b, "- `%s` — %s's %q\n", sk.AgentHandle, sk.AgentHandle, sk.Name)
	}
	fmt.Fprintf(&b, "\nCommon tool run: `%s`\n", strings.Join(c.Sequence, " -> "))
	return b.String()
}

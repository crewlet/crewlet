package engine

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
)

// Wiring cross-agent skill promotion: which knowledge base a draft lands in,
// and which unit's seats are pooled. internal/learning knows nothing of
// Confluence or org units, and the app writer nothing of units; the two meet
// only here.

// promotionWired reports whether a knowledge base is wired to draft into.
//
// It mirrors [Engine.Knowledge] deliberately: the pages a lead reviews must
// land where the company's search reads.
func (e *Engine) promotionWired() bool {
	e.notify.mu.Lock()
	defer e.notify.mu.Unlock()
	return e.notify.confluence.pages != nil
}

// promotionWriter builds the writer for the wired knowledge base.
//
// READ AT PASS TIME, as the resolver [learning.PromotionWriterFor]: the
// background passes are armed BEFORE the inbound service builds its app
// clients, so a writer captured at arm time is nil for every company.
//
// Nil disables promotion rather than failing it. There is no fallback: a draft
// written into an agent's own catalogue is the unreviewed cross-agent skill the
// review step exists to prevent.
func (e *Engine) promotionWriter() learning.PromotionWriter {
	e.notify.mu.Lock()
	defer e.notify.mu.Unlock()
	// Checked before conversion: a typed nil in the interface would pass
	// NewPromoter's `Writer == nil` guard and panic on the first draft.
	if c := e.notify.confluence.pages; c != nil {
		return confluence.NewPromotionWriter(c)
	}
	return nil
}

// promotionUnits is every unit whose seats could converge, read FRESH.
//
// A function rather than a slice: an apply changes the org, and a pass holding
// the units it started with would keep drafting into a space the company left.
func (e *Engine) promotionUnits() []learning.PromotionUnit {
	company := e.Company()
	if company == nil || company.Org == nil {
		return nil
	}
	// Asked once: constant within a walk, and each ask takes the mutex.
	wired := e.promotionWired()
	// One index for every unit's lead: [org.Organization.EffectiveLead]
	// walks every seat per call, units x seats over the walk
	// ([org.Index.EffectiveLead]).
	leads := company.Org.Index()
	var out []learning.PromotionUnit
	for unit := range company.Org.AllUnits() {
		handles := agentHandlesIn(unit)
		if len(handles) == 0 {
			continue
		}
		container, hint := promotionContainer(unit, wired)
		out = append(out, learning.PromotionUnit{
			// THE UNIT'S KEY, never its display name: the key is what
			// `manages:`, a seat's `unit:` and every stored row resolve,
			// so skill_promoted's `unit_id` can be filtered and joined.
			ID: unit.Key(), Lead: leads.EffectiveLead(unit),
			Handles: handles, Container: container, Hint: hint,
		})
	}
	return out
}

// agentHandlesIn is the unit's OWN agent seats, not its descendants'.
//
// Direct members only: a parent pooling its descendants would draft its
// child's convergence again one level up, naming a team that never converged.
func agentHandlesIn(unit *org.Unit) []string {
	var out []string
	for _, role := range unit.Roles {
		if role.IsAgent() {
			if handle := role.Handle(); handle != "" {
				out = append(out, handle)
			}
		}
	}
	return out
}

// promotionContainer is where this unit files a draft, or what to set.
//
// THE UNIT'S WIKI SPACE, never its tracker project: under the project's name
// the wiki writer would file into whatever space shares it, or fail.
//
// The HINT names the field to set: a unit with no container is soft-skipped,
// and nothing else would say why it never promotes.
func promotionContainer(unit *org.Unit, wired bool) (container, hint string) {
	if !wired {
		// Reached only by a caller inspecting the roster: buildPromoter
		// refuses the pass first.
		return "", "no knowledge base is configured for this company, so " +
			"there is nowhere to file a draft at all"
	}
	if unit.Space != "" {
		return unit.Space, ""
	}
	return "", fmt.Sprintf(
		"unit %q has no `space`, so there is nowhere to file a draft its "+
			"seats could review — set one on the unit",
		unit.Name)
}

// buildPromoter builds the promotion pass, or says why it is not armed.
//
// A FLEET SINGLETON on the background passes' duty: the writers' dedup stops a
// second draft of one page only once the first has committed, which two nodes
// racing need not have.
func (e *Engine) buildPromoter(c *Company) *learning.Promoter {
	cfg := c.Config.Learning.SkillPromotion
	if !cfg.Promotes() || e.backends.Store == nil {
		return nil
	}
	promoter, err := learning.NewPromoter(learning.PromoterOptions{
		// THE RESOLVER, not a writer: see [Engine.promotionWriter].
		Writer:           e.promotionWriter,
		Skills:           learning.NewSkills(e.backends.Store),
		Models:           e.auxiliaryFor(c),
		Units:            e.promotionUnits,
		MinSiblings:      cfg.MinSiblingCount,
		JaccardThreshold: cfg.JaccardThreshold,
		Budget:           e.auxGate(c, string(types.AuxSkillPromotion)),
	})
	if err != nil {
		log.Warn("skill_promotion_unavailable", "error", err,
			"detail", "what several seats in a unit worked out stays in their "+
				"own catalogues and reaches nobody else")
		return nil
	}
	return promoter
}

// The writer satisfies the consumer-declared seam, checked here so a signature
// drift fails at the integration rather than in later wiring.
var _ learning.PromotionWriter = (*confluence.PromotionWriter)(nil)

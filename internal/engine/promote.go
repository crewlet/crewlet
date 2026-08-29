package engine

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
)

// Wiring cross-agent skill promotion: which knowledge base a draft lands in,
// and which unit's seats are pooled.
//
// The ENTANGLEMENT the rest of the subsystem is written to avoid. The pass in
// internal/learning knows nothing about Confluence or org units; the vendor
// writer knows nothing about units. Both facts meet here, which is what this
// package is for.

// promotionWired reports whether a knowledge base is wired to draft into.
//
// It mirrors [Engine.Knowledge] deliberately: the pages a lead reviews must
// land in the same place the company's search reads, and asking one question
// in two ways is how those two drift apart.
func (e *Engine) promotionWired() bool {
	e.notify.mu.Lock()
	defer e.notify.mu.Unlock()
	return e.notify.confluence.pages != nil
}

// promotionWriter builds the writer for the wired knowledge base.
//
// READ AT PASS TIME, never captured — it is handed to the pass as the
// resolver [learning.PromotionWriterFor], because the background passes are
// armed BEFORE the inbound service builds its vendor clients. A writer read
// at arm time is nil for every company, and the symptom is one boot line
// saying no knowledge base is configured while one is.
//
// Nil means no backend is wired, which disables promotion rather than failing
// it. A company with no wiki has nowhere to put a page a person reviews, and
// there is no fallback worth having — writing the draft into an agent's own
// catalogue would be exactly the unreviewed cross-agent skill the review step
// exists to prevent.
func (e *Engine) promotionWriter() learning.PromotionWriter {
	e.notify.mu.Lock()
	defer e.notify.mu.Unlock()
	// The concrete pointer is checked before conversion: a typed nil
	// assigned into the interface is not nil, so NewPromoter's
	// `Writer == nil` guard would pass and the first draft would panic.
	if c := e.notify.confluence.pages; c != nil {
		return confluence.NewPromotionWriter(c)
	}
	return nil
}

// promotionUnits is every unit whose seats could converge, read FRESH.
//
// A function rather than a slice for the reason every roster here is one: an
// apply changes the org, and a pass holding the units it started with would
// keep drafting into a space the company has moved off.
func (e *Engine) promotionUnits() []learning.PromotionUnit {
	company := e.Company()
	if company == nil || company.Org == nil {
		return nil
	}
	// ASKED ONCE, before the walk: whether a draft has anywhere to go at
	// all cannot change inside one walk, and asking per unit would take
	// the mutex once per unit for the same answer.
	wired := e.promotionWired()
	var out []learning.PromotionUnit
	for unit := range company.Org.AllUnits() {
		handles := agentHandlesIn(unit)
		if len(handles) == 0 {
			continue
		}
		container, hint := promotionContainer(unit, wired)
		out = append(out, learning.PromotionUnit{
			ID: unit.Name, Lead: company.Org.EffectiveLead(unit),
			Handles: handles, Container: container, Hint: hint,
		})
	}
	return out
}

// agentHandlesIn is the unit's OWN agent seats, not its descendants'.
//
// Direct members only, deliberately. A parent unit that pooled every
// descendant's catalogue would find the same convergence its child already
// promoted and draft it a second time, one level up — and the page a lead
// reviews would name a team that never converged on anything.
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
// THE UNIT'S WIKI SPACE, never its tracker project. A unit carries both
// identities, and they are different things: the tracker project is where
// its work is filed, and a draft handed to the wiki writer under that name
// would create a page in whatever space happens to be called the same, or
// fail against nothing at all.
//
// The HINT is not decoration: a unit with no container is soft-skipped, and
// without the field name in the log an operator sees a team that never
// promotes anything and nothing saying why.
func promotionContainer(unit *org.Unit, wired bool) (container, hint string) {
	if !wired {
		// No knowledge base at all. buildPromoter refuses the pass before
		// any unit is walked, so this is only reached by a caller
		// inspecting the roster; say what is actually missing.
		return "", "no knowledge base is configured for this company, so " +
			"there is nowhere to file a draft at all"
	}
	if unit.ConfluenceSpace != "" {
		return unit.ConfluenceSpace, ""
	}
	return "", fmt.Sprintf(
		"unit %q has no integrations.confluence.space, so there is nowhere "+
			"to file a draft its seats could review — set one on the unit",
		unit.Name)
}

// buildPromoter builds the promotion pass, or says why it is not armed.
//
// A FLEET SINGLETON on the same duty the other background passes claim: two
// nodes promoting one unit would draft the same page twice, and the writers'
// dedup would make the second a silent no-op only if the first had already
// committed — which across two nodes it may not have.
func (e *Engine) buildPromoter(c *Company) *learning.Promoter {
	cfg := c.Config.Learning.SkillPromotion
	if !cfg.Promotes() || e.backends.Store == nil {
		return nil
	}
	promoter, err := learning.NewPromoter(learning.PromoterOptions{
		// THE RESOLVER, not a writer: see [Engine.promotionWriter].
		Writer:           e.promotionWriter,
		Skills:           learning.NewSkills(e.backends.Store),
		Models:           e.meteredModelsFor(c),
		Units:            e.promotionUnits,
		MinSiblings:      cfg.MinSiblingCount,
		JaccardThreshold: cfg.JaccardThreshold,
		MaxTokens:        cfg.BudgetTokens,
	})
	if err != nil {
		log.Warn("skill_promotion_unavailable", "error", err,
			"detail", "what several seats in a unit worked out stays in their "+
				"own catalogues and reaches nobody else")
		return nil
	}
	return promoter
}

// Compile-time proof that the vendor writer satisfies the pass's seam. The
// interface is declared by the consumer, so nothing else would notice a
// signature drift until the wiring above failed to build — which is later
// than a reader of the vendor package would want to find out.
var _ learning.PromotionWriter = (*confluence.PromotionWriter)(nil)

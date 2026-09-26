package engine

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/pages"
)

// Wiring cross-agent skill promotion: which knowledge base a draft lands in,
// and which unit's seats are pooled.
//
// The ENTANGLEMENT the rest of the subsystem is written to avoid. The pass in
// internal/learning knows nothing about a knowledge backend or an org unit,
// and neither writer knows what a unit is. Both facts meet here, which is
// what this package is for.

// promotionWriter is the writer for the knowledge base this company's search
// reads, or nil and why there is none.
//
// IT ASKS [Engine.KnowledgeServed] AND READS THE BACKEND OFF ITS ANSWER, so
// the decision is made once and in one place. A draft's only protection is
// that the company's search leaves out the auto-drafted subtree; a writer that
// chose its backend by a second rule could file drafts in a wiki the search is
// not reading — or file none at all, silently, while the company's search runs
// on the other backend. Each backend has exactly one writer, built beside the
// searcher it answers for, so the served searcher names the writer.
//
// READ AT PASS TIME, never captured — it is handed to the pass as the
// resolver [learning.PromotionWriterFor], because the background passes are
// armed BEFORE the inbound service builds its third-party app clients, and a
// writer read at arm time would be nil for every company on Confluence.
//
// Nil disables promotion rather than failing it, and the reason is the
// knowledge search's own ([knowledge.Refusal.Detail]), so the pass's idle line
// names what the search would. A company with no knowledge base has nowhere to
// put a page a person reviews, and there is no fallback worth having — writing
// the draft into an agent's own catalogue would be exactly the unreviewed
// cross-agent skill the review step exists to prevent.
func (e *Engine) promotionWriter() (learning.PromotionWriter, string) {
	searcher, refusal := e.KnowledgeServed()
	switch served := searcher.(type) {
	case nil:
		return nil, refusal.Detail
	case *pages.Searcher:
		// BUILT IN THE SAME STEP AS THE SEARCHER ([Engine.startNative]),
		// so a node serving one holds the other.
		return e.native.drafts, ""
	case *confluence.Searcher:
		// THE CLIENT THAT BELONGS TO THE SEARCHER THE DECISION RETURNED.
		// An apply replaces the Confluence wiring whole, and between the
		// decision and this read it may have: a client read from the
		// newer wiring would draft through a connection the decision
		// never looked at. Both are read under one hold of the lock, and
		// a mismatch waits for the next pass rather than guessing. A
		// matching searcher never stands beside a nil client:
		// [Engine.startConfluence] builds the searcher from that client.
		e.notify.mu.Lock()
		held, client := e.notify.confluence.searcher, e.notify.confluence.pages
		e.notify.mu.Unlock()
		if held != served {
			return nil, "the Confluence connection was rebuilt while this pass " +
				"resolved it; the next pass drafts through whichever is current then"
		}
		return confluence.NewPromotionWriter(client), ""
	default:
		return nil, fmt.Sprintf("the company's knowledge base (%s) has no "+
			"promotion writer in this build", served.Backend())
	}
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
	// WHETHER A DRAFT HAS ANYWHERE TO GO IS NOT ASKED HERE. The pass
	// resolves its writer before it reads this roster and stops when there
	// is none ([learning.Promoter.Pass]), so a container blanked here for a
	// company with no knowledge base would be a second answer to that
	// question which nothing reads.
	var out []learning.PromotionUnit
	for unit := range company.Org.AllUnits() {
		handles := agentHandlesIn(unit)
		if len(handles) == 0 {
			continue
		}
		container, hint := promotionContainer(unit)
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
func promotionContainer(unit *org.Unit) (container, hint string) {
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

// Compile-time proof that both writers satisfy the pass's seam. The interface
// is declared by the consumer, so nothing else would notice a signature drift
// until the wiring above failed to build — which is later than a reader of
// either writer would want to find out.
var (
	_ learning.PromotionWriter = (*confluence.PromotionWriter)(nil)
	_ learning.PromotionWriter = (*nativeDrafts)(nil)
)

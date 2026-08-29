package engine

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/plane"
)

// Wiring cross-agent skill promotion: which knowledge base a draft lands in,
// and which unit's seats are pooled.
//
// The ENTANGLEMENT the rest of the subsystem is written to avoid. The pass in
// internal/learning knows nothing about Confluence, Plane or org units; the
// two vendor writers know nothing about units. Both facts meet here, which is
// what this package is for.

// promotionBackend names the knowledge base this company drafts into.
//
// EXACTLY ONE per org — the config validator enforces Confluence XOR Plane —
// so this is a lookup rather than a choice, and it mirrors [Engine.Knowledge]
// deliberately: a company whose search reads Confluence must not have its
// drafts land in Plane.
type promotionBackend int

const (
	promotionNone promotionBackend = iota
	promotionConfluence
	promotionPlane
)

// promotionBackend reports which knowledge base is wired, if any.
func (e *Engine) promotionBackend() promotionBackend {
	e.notify.mu.Lock()
	defer e.notify.mu.Unlock()
	return e.backendLocked()
}

// backendLocked is the selection itself, for callers already holding the
// lock. ONE implementation, so the writer and the container resolution can
// never disagree about which backend a company drafts into.
func (e *Engine) backendLocked() promotionBackend {
	switch {
	case e.notify.confluence.pages != nil:
		return promotionConfluence
	case e.notify.plane.pages != nil:
		return promotionPlane
	}
	return promotionNone
}

// promotionWriter builds the writer for the wired backend.
//
// SWITCHED ON promotionBackend rather than repeating its lookup, because the
// two answers must never disagree: a container resolved from a unit's
// Confluence space and handed to the Plane writer would create a page in
// whatever project happens to be named the same, or fail against nothing.
//
// Nil means no backend is wired, which disables promotion rather than failing
// it. A company with no wiki has nowhere to put a page a person reviews, and
// there is no fallback worth having — writing the draft into an agent's own
// catalogue would be exactly the unreviewed cross-agent skill the review step
// exists to prevent.
func (e *Engine) promotionWriter() learning.PromotionWriter {
	e.notify.mu.Lock()
	defer e.notify.mu.Unlock()
	// UNDER ONE LOCK with the selection, so an apply that swaps the
	// knowledge base between choosing a backend and reading its client
	// cannot hand back a writer over a nil one. The concrete pointers are
	// checked before conversion for the same reason: a typed nil assigned
	// into the interface is not nil, so NewPromoter's `Writer == nil`
	// guard would pass and the first draft would panic.
	switch e.backendLocked() {
	case promotionConfluence:
		if c := e.notify.confluence.pages; c != nil {
			return confluence.NewPromotionWriter(c)
		}
	case promotionPlane:
		if p := e.notify.plane.pages; p != nil {
			return plane.NewPromotionWriter(p)
		}
	case promotionNone:
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
	// THE BACKEND FIRST, and once: which field holds a unit's container
	// depends on which knowledge base is wired, and asking per unit would
	// take the mutex once per unit for an answer that cannot change inside
	// one walk.
	backend := e.promotionBackend()
	var out []learning.PromotionUnit
	for unit := range company.Org.AllUnits() {
		handles := agentHandlesIn(unit)
		if len(handles) == 0 {
			continue
		}
		container, hint := promotionContainer(unit, backend)
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
// THE FIELD THAT MATCHES THE WIRED BACKEND, never a preference order. A unit
// may carry both identities — Plane as its tracker, Confluence as its wiki —
// and handing a Confluence space to the Plane writer would create a page in
// whatever project happens to be named "ENG", or fail against nothing at all.
//
// The HINT is not decoration: a unit with no container is soft-skipped, and
// without the field name in the log an operator sees a team that never
// promotes anything and nothing saying why. It names the field for THIS
// backend, because the other one is not where a draft could go.
func promotionContainer(unit *org.Unit, backend promotionBackend) (container, hint string) {
	var got, field string
	switch backend {
	case promotionConfluence:
		got, field = unit.ConfluenceSpace, "integrations.confluence.space"
	case promotionPlane:
		got, field = unit.PlaneProject, "integrations.plane.project"
	case promotionNone:
		// No knowledge base at all. buildPromoter refuses the pass before
		// any unit is walked, so this is only reached by a caller
		// inspecting the roster; say what is actually missing.
		return "", "no knowledge base is configured for this company, so " +
			"there is nowhere to file a draft at all"
	}
	if got != "" {
		return got, ""
	}
	return "", fmt.Sprintf(
		"unit %q has no %s, so there is nowhere to file a draft its seats "+
			"could review — set one on the unit", unit.Name, field)
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
	writer := e.promotionWriter()
	if writer == nil {
		log.Info("skill_promotion_idle",
			"reason", "no knowledge base is configured",
			"detail", "a promoted skill is a draft page a unit lead reviews, "+
				"and there is nowhere to put one; configure "+
				"integrations.confluence or integrations.plane, or set "+
				"learning.skill_promotion.enabled: false")
		return nil
	}
	promoter, err := learning.NewPromoter(learning.PromoterOptions{
		Writer:           writer,
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

// Compile-time proof that both vendor writers satisfy the pass's seam. The
// interface is declared by the consumer, so nothing else would notice a
// signature drift until the wiring above failed to build — which is later
// than a reader of either vendor package would want to find out.
var (
	_ learning.PromotionWriter = (*confluence.PromotionWriter)(nil)
	_ learning.PromotionWriter = (*plane.PromotionWriter)(nil)
)

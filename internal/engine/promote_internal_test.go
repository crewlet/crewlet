package engine

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/plane"
)

// THE PROMOTION WIRING.
//
// `skill_promotion.enabled`, `min_sibling_count`, `jaccard_threshold` and
// `budget_tokens` validated, shipped in the example company, and had no
// reader outside internal/config — the pass they configure did not exist.
// These cases are the seam between the pass and the company.

// promotionCompany builds an epoch with units and a knowledge container.
func promotionCompany(t *testing.T, units string) *Company {
	t.Helper()
	return companyFor(t, `
name: Acme
providers:
  llm:
    gateway:
      type: openai
      model: gpt-4o
      api_keys: ["${OPENAI_API_KEY}"]
`+units+`
`)
}

// A UNIT'S OWN SEATS ARE POOLED, and its container comes off the unit.
func TestPromotionUnitsCarryTheirSeatsAndContainer(t *testing.T) {
	t.Parallel()
	e := engineOver(t)
	setEpoch(e, promotionCompany(t, `units:
  - name: Platform
    integrations:
      confluence: {space: ENG}
    roles:
      - name: Engineer
        handle: eng
        llm: gateway
      - name: Reliability
        handle: sre
        llm: gateway
`))
	wireConfluence(e)
	units := e.promotionUnits()
	if len(units) != 1 {
		t.Fatalf("units = %d, want 1", len(units))
	}
	if units[0].ID != "Platform" || units[0].Container != "ENG" {
		t.Fatalf("unit = %+v", units[0])
	}
	if len(units[0].Handles) != 2 {
		t.Fatalf("handles = %v, want both of the unit's seats", units[0].Handles)
	}
}

// A UNIT WITH NO CONTAINER CARRIES THE REMEDIATION. Without the field name in
// the hint an operator sees a team that never promotes and nothing saying why.
func TestAUnitWithNoContainerCarriesItsRemediation(t *testing.T) {
	t.Parallel()
	e := engineOver(t)
	setEpoch(e, promotionCompany(t, `units:
  - name: Platform
    roles:
      - name: Engineer
        handle: eng
        llm: gateway
`))
	wireConfluence(e)
	units := e.promotionUnits()
	if len(units) != 1 {
		t.Fatalf("units = %d, want 1", len(units))
	}
	if units[0].Container != "" {
		t.Fatalf("Container = %q, want empty", units[0].Container)
	}
	for _, want := range []string{"integrations.confluence.space", "Platform"} {
		if !strings.Contains(units[0].Hint, want) {
			t.Fatalf("the hint omits %q: %q", want, units[0].Hint)
		}
	}
}

// A PARENT UNIT DOES NOT POOL ITS CHILDREN'S SEATS. It would find the same
// convergence the child already promoted and draft it a second time one level
// up, naming a team that never converged on anything.
func TestAParentUnitDoesNotPoolItsChildrensSeats(t *testing.T) {
	t.Parallel()
	e := engineOver(t)
	setEpoch(e, promotionCompany(t, `units:
  - name: Engineering
    integrations:
      confluence: {space: ORG}
    roles:
      - name: VP
        handle: vp
        llm: gateway
    children:
      - name: Platform
        integrations:
          confluence: {space: ENG}
        roles:
          - name: Engineer
            handle: eng
            llm: gateway
`))
	wireConfluence(e)
	for _, unit := range e.promotionUnits() {
		if unit.ID != "Engineering" {
			continue
		}
		if len(unit.Handles) != 1 || unit.Handles[0] != "vp" {
			t.Fatalf("the parent pooled %v, want only its own direct seat",
				unit.Handles)
		}
	}
}

// THE CONTAINER MATCHES THE WIRED BACKEND, never a preference order. A unit
// may carry both identities — Plane as its tracker, Confluence as its wiki —
// and handing a Confluence space to the Plane writer would create a page in
// whatever project happens to be named "ENG", or fail against nothing at all.
func TestTheContainerFollowsTheWiredBackend(t *testing.T) {
	t.Parallel()
	c := promotionCompany(t, `units:
  - name: Platform
    integrations:
      confluence: {space: WIKI}
      plane: {project: TRACKER}
    roles:
      - name: Engineer
        handle: eng
        llm: gateway
`)
	for _, tc := range []struct {
		name  string
		wire  func(e *Engine)
		want  string
		field string
	}{
		{"confluence", wireConfluence, "WIKI", "confluence"},
		{"plane", wirePlane, "TRACKER", "plane"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := engineOver(t)
			setEpoch(e, c)
			tc.wire(e)
			units := e.promotionUnits()
			if len(units) != 1 {
				t.Fatalf("units = %d", len(units))
			}
			if units[0].Container != tc.want {
				t.Fatalf("Container = %q, want the %s one (%q) — a draft filed "+
					"into the other backend's identity lands nowhere a lead "+
					"will find it", units[0].Container, tc.field, tc.want)
			}
		})
	}
}

// A UNIT MISSING THE WIRED BACKEND'S CONTAINER IS SKIPPED, and the hint names
// THAT backend's field rather than the other one, which is not where a draft
// could go.
func TestTheHintNamesTheWiredBackendsField(t *testing.T) {
	t.Parallel()
	e := engineOver(t)
	setEpoch(e, promotionCompany(t, `units:
  - name: Platform
    integrations:
      plane: {project: TRACKER}
    roles:
      - name: Engineer
        handle: eng
        llm: gateway
`))
	wireConfluence(e)
	units := e.promotionUnits()
	if len(units) != 1 || units[0].Container != "" {
		t.Fatalf("unit = %+v, want no container under a Confluence backend", units)
	}
	if !strings.Contains(units[0].Hint, "confluence") {
		t.Fatalf("the hint does not name the wired backend's field: %q", units[0].Hint)
	}
	if strings.Contains(units[0].Hint, "plane") {
		t.Fatalf("the hint points at the backend a draft cannot go to: %q", units[0].Hint)
	}
}

// A COMPANY WITH A KNOWLEDGE BASE BUILDS THE PASS — and its toggle is still
// what decides. Without a writer wired, both answers are nil for the wrong
// reason.
func TestPromotionIsBuiltWhenThereIsSomewhereToDraft(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		block string
		want  bool
	}{
		{"on by default", "", true},
		{"off when told", "learning:\n  skill_promotion:\n    enabled: false\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := engineOver(t)
			c := promotionCompany(t, tc.block+`units:
  - name: Platform
    integrations:
      confluence: {space: ENG}
    roles:
      - name: Engineer
        handle: eng
        llm: gateway
`)
			setEpoch(e, c)
			wireConfluence(e)
			if got := e.buildPromoter(c) != nil; got != tc.want {
				t.Fatalf("promoter built = %v, want %v", got, tc.want)
			}
		})
	}
}

// WITH NO KNOWLEDGE BASE THERE IS NO PROMOTION. A promoted skill is a page a
// person reviews, and there is nowhere to put one — writing it into an
// agent's own catalogue instead would be exactly the unreviewed cross-agent
// skill the review step exists to prevent.
func TestPromotionIsIdleWithoutAKnowledgeBase(t *testing.T) {
	t.Parallel()
	e := engineOver(t)
	c := promotionCompany(t, `units:
  - name: Platform
    integrations:
      confluence: {space: ENG}
    roles:
      - name: Engineer
        handle: eng
        llm: gateway
`)
	setEpoch(e, c)
	if got := e.buildPromoter(c); got != nil {
		t.Fatal("a promoter was built with no knowledge base to draft into")
	}
}

// THE TOGGLE GATES IT. `skill_promotion.enabled: false` must leave the pass
// unbuilt rather than built and skipping.
func TestPromotionFollowsItsToggle(t *testing.T) {
	t.Parallel()
	e := engineOver(t)
	c := companyFor(t, `
name: Acme
providers:
  llm:
    gateway:
      type: openai
      model: gpt-4o
      api_keys: ["${OPENAI_API_KEY}"]
learning:
  skill_promotion:
    enabled: false
units:
  - name: Platform
    integrations:
      confluence: {space: ENG}
    roles:
      - name: Engineer
        handle: eng
        llm: gateway
`)
	setEpoch(e, c)
	if got := e.buildPromoter(c); got != nil {
		t.Fatal("a promoter was built for a company that turned promotion off")
	}
}

// wireConfluence and wirePlane give the engine a knowledge base, which is
// what the promotion writer is selected from.
func wireConfluence(e *Engine) {
	client, err := confluence.NewClient(confluence.ClientOptions{
		URL: "https://wiki.example.com", Token: "t",
	})
	if err != nil {
		panic(err)
	}
	e.notify.mu.Lock()
	defer e.notify.mu.Unlock()
	e.notify.confluence.pages = client
}

func wirePlane(e *Engine) {
	client, err := plane.NewClient(plane.ClientOptions{
		URL: "https://plane.example.com", Workspace: "acme", APIKey: "k",
	})
	if err != nil {
		panic(err)
	}
	e.notify.mu.Lock()
	defer e.notify.mu.Unlock()
	e.notify.plane.pages = client
}

// setEpoch publishes an epoch, which is what the fresh-read rosters resolve
// against — see Engine.promotionUnits.
func setEpoch(e *Engine, c *Company) { e.epoch.current.Store(c) }

// Compile-time proof the pass's seam is what the engine hands it.
var _ = func() learning.PromotionUnit { return learning.PromotionUnit{} }

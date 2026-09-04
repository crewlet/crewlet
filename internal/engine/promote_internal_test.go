package engine

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/learning"
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
    space: ENG
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
	for _, want := range []string{"has no `space`", "Platform"} {
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
    space: ORG
    roles:
      - name: VP
        handle: vp
        llm: gateway
    children:
      - name: Platform
        space: ENG
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

// THE CONTAINER IS THE UNIT'S WIKI SPACE, never its tracker project. A unit
// carries both identities and they are different places: a draft filed under
// the tracker's key would create a page in whatever space happens to be
// named "TRACKER", or fail against nothing at all.
func TestTheContainerIsTheWikiSpaceNotTheTrackerProject(t *testing.T) {
	t.Parallel()
	e := engineOver(t)
	setEpoch(e, promotionCompany(t, `units:
  - name: Platform
    space: WIKI
    project: TRACKER
    roles:
      - name: Engineer
        handle: eng
        llm: gateway
`))
	wireConfluence(e)
	units := e.promotionUnits()
	if len(units) != 1 {
		t.Fatalf("units = %d", len(units))
	}
	if units[0].Container != "WIKI" {
		t.Fatalf("Container = %q, want the wiki space (\"WIKI\") — a draft "+
			"filed into the tracker's identity lands nowhere a lead will "+
			"find it", units[0].Container)
	}
}

// A UNIT WITH NO WIKI SPACE IS SKIPPED, and the hint names the field that
// would fix it rather than the tracker project it does carry, which is not
// where a draft could go.
func TestTheHintNamesTheWikiSpaceField(t *testing.T) {
	t.Parallel()
	e := engineOver(t)
	setEpoch(e, promotionCompany(t, `units:
  - name: Platform
    project: TRACKER
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
	if !strings.Contains(units[0].Hint, "`space`") {
		t.Fatalf("the hint does not name the field that would fix it: %q", units[0].Hint)
	}
	if strings.Contains(units[0].Hint, "project") {
		t.Fatalf("the hint points at an identity a draft cannot go to: %q", units[0].Hint)
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
    space: ENG
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

// THE WRITER IS READ AT PASS TIME, NOT WHEN THE PASS IS ARMED.
//
// The background passes are armed BEFORE startNotifications builds the
// vendor clients, so a promoter that resolved its writer at arm time held a
// nil for every company that ever ran — and the only symptom was one boot
// line saying no knowledge base was configured while one was. A pass that
// runs after the wiring catches up must find it.
func TestAPromoterArmedBeforeTheKnowledgeBaseStillFindsIt(t *testing.T) {
	t.Parallel()
	e := engineOver(t)
	c := promotionCompany(t, `units:
  - name: Platform
    space: ENG
    roles:
      - name: Engineer
        handle: eng
        llm: gateway
`)
	setEpoch(e, c)

	// Armed with nothing wired — the boot order this reproduces.
	promoter := e.buildPromoter(c)
	if promoter == nil {
		t.Fatal("no promoter was armed, so a company whose knowledge base " +
			"wires moments later can never promote")
	}
	if e.promotionWriter() != nil {
		t.Fatal("a writer resolved before the knowledge base was wired")
	}

	// The inbound service catches up, as it does two lines later at boot.
	wireConfluence(e)
	if e.promotionWriter() == nil {
		t.Fatal("the writer stayed nil after the knowledge base was wired")
	}
	// And the armed pass runs against it rather than the nil it was armed
	// with: a captured writer would panic here, and a pass that re-checked
	// nothing would report idle for ever.
	promoter.Pass(t.Context())
}

// THE TOGGLE IS WHAT DECIDES, not the wiring. A company with promotion on
// and no knowledge base yet is armed; the pass itself reports the idleness.
func TestPromotionIsArmedBeforeTheKnowledgeBaseIsWired(t *testing.T) {
	t.Parallel()
	e := engineOver(t)
	c := promotionCompany(t, `units:
  - name: Platform
    space: ENG
    roles:
      - name: Engineer
        handle: eng
        llm: gateway
`)
	setEpoch(e, c)
	if got := e.buildPromoter(c); got == nil {
		t.Fatal("promotion was left unarmed because the knowledge base was " +
			"not wired yet, which is the boot order it always sees")
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
    space: ENG
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

// wireConfluence gives the engine a knowledge base, which is what the
// promotion writer is built from.
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

// setEpoch publishes an epoch, which is what the fresh-read rosters resolve
// against — see Engine.promotionUnits.
func setEpoch(e *Engine, c *Company) { e.epoch.current.Store(c) }

// Compile-time proof the pass's seam is what the engine hands it.
var _ = func() learning.PromotionUnit { return learning.PromotionUnit{} }

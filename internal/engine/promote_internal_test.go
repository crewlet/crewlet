package engine

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/learning"
)

// THE PROMOTION WIRING: the seam between the pass and the company.

// promotionCompany builds an epoch with units and a knowledge container.
func promotionCompany(t *testing.T, units string) *Company {
	t.Helper()
	return companyFor(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    gateway:
      model: sonnet
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
	if units[0].ID != "platform" || units[0].Container != "ENG" {
		t.Fatalf("unit = %+v", units[0])
	}
	if !slices.Equal(units[0].Handles, []string{"eng", "sre"}) {
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

// A PARENT UNIT DOES NOT POOL ITS CHILDREN'S SEATS, or it would draft its
// child's convergence again one level up.
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
	// By the unit's key, which PromotionUnit.ID carries: matched on the
	// display name the loop would assert nothing.
	var found bool
	for _, unit := range e.promotionUnits() {
		if unit.ID != "engineering" {
			continue
		}
		found = true
		if !slices.Equal(unit.Handles, []string{"vp"}) {
			t.Fatalf("the parent pooled %v, want only its own direct seat",
				unit.Handles)
		}
	}
	if !found {
		t.Fatal("the parent unit is not among the promotion units")
	}
}

// THE CONTAINER IS THE UNIT'S WIKI SPACE, never its tracker project: filed
// under the tracker's key a draft lands in whatever space is named "TRACKER".
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
// would fix it, not the tracker project it does carry.
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

// A COMPANY WITH A KNOWLEDGE BASE BUILDS THE PASS, and its toggle still
// decides; the writer is wired so a nil answer is never for the wrong reason.
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

// THE WRITER IS READ AT PASS TIME, NOT WHEN THE PASS IS ARMED: the passes are
// armed BEFORE startNotifications builds the app clients, so a writer resolved
// at arm time is nil for every company.
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

	// Armed with nothing wired: the boot order.
	promoter := e.buildPromoter(c)
	if promoter == nil {
		t.Fatal("no promoter was armed, so a company whose knowledge base " +
			"wires moments later can never promote")
	}
	if e.promotionWriter() != nil {
		t.Fatal("a writer resolved before the knowledge base was wired")
	}

	// The inbound service catches up, as at boot.
	wireConfluence(e)
	if e.promotionWriter() == nil {
		t.Fatal("the writer stayed nil after the knowledge base was wired")
	}
	// The armed pass runs against it: a captured nil writer would panic here.
	promoter.Pass(t.Context())
}

// THE TOGGLE DECIDES, not the wiring: promotion on with no knowledge base yet
// is armed, and the pass itself reports the idleness.
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
  sandbox: {fake: true}
  llm:
    gateway:
      model: sonnet
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

// wireConfluence gives the engine the knowledge base the promotion writer is
// built from.
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

// setEpoch publishes the epoch the fresh-read rosters resolve against
// (Engine.promotionUnits).
func setEpoch(e *Engine, c *Company) { e.epoch.current.Store(c) }

// Compile-time proof the pass's seam is what the engine hands it.
var _ = func() learning.PromotionUnit { return learning.PromotionUnit{} }

// A PROMOTION NAMES ITS UNIT BY KEY, NOT BY DISPLAY NAME: the key is what
// `manages:`, a seat's `unit:` and every stored row resolve, so a
// skill_promoted event's `unit_id` can be filtered and joined on and survives
// a rename.
func TestAPromotionNamesItsUnitByKey(t *testing.T) {
	t.Parallel()
	e := engineOver(t)
	setEpoch(e, promotionCompany(t, `units:
  - name: Platform Engineering
    id: plat
    space: ENG
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
	if units[0].ID != "plat" {
		t.Errorf("the promotion names unit %q, want its key %q — the display name "+
			"is %q, and an event carrying that can be neither filtered nor joined on",
			units[0].ID, "plat", "Platform Engineering")
	}
}

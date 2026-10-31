package engine

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/config"
)

// A DECLARED PUSH MIRROR'S SEAT READS WHAT THE DOMAIN GRANTS IT: validation
// refuses a code.repos push mirror whose `as` seat does not read the
// repository by config's reading of the grant (CodeRepo.mirrorSeatReads, which
// cannot import the domain), and a person's create or settings change is
// refused by the domain's own (code.MirrorSeatRefusal over the chart's
// roster). The two are one rule: for every seat of a chart with every relation
// a grant reads — a root seat, a human seat, a led unit with a led child and a
// child inheriting its lead, a unit with no lead above it, a unit led from
// outside it (Sales, by the root seat ceo), tiers from read_only to
// full_access — asked of a repository owned by every unit and by seats inside
// and outside them, at both visibilities, the document is refused at
// mirror_to.as exactly where the domain refuses the seat.
//
// Mutations: let config's rule read a lead chain from the owner's PARENT
// rather than the owner — the rows of ceo, who leads Sales from outside it,
// disagree, red; let it read every agent at `visibility: unit` — the
// outsiders' rows disagree, red.
func TestADeclaredMirrorSeatReadsWhatTheDomainGrantsIt(t *testing.T) {
	t.Parallel()
	// SALES LED FROM OUTSIDE IT: the one relation where only the owning
	// unit's own lead reads, so a lead walk begun above the owner misses it.
	chart := strings.Replace(rosterChart(""), "  - name: Sales\n    roles:",
		"  - name: Sales\n    lead: ceo\n    roles:", 1)
	if !strings.Contains(chart, "lead: ceo") {
		t.Fatal("the premise: rosterChart's Sales unit is not where this test gives it the lead ceo")
	}
	roster, o := rosterOfChart(t, chart)
	sales := code.RepoRef{ID: "web", Name: "web", Owner: code.Owner{Kind: code.OwnerUnit, Ref: "Sales"},
		Visibility: code.VisibilityUnit}
	if why := code.MirrorSeatRefusal("ceo", sales, roster); why != "" {
		t.Fatalf("the premise: ceo leads Sales and reads what it keeps to itself, but the domain says %s", why)
	}
	owners := []struct {
		yaml  string
		owner code.Owner
	}{
		{"{unit: eng}", code.Owner{Kind: code.OwnerUnit, Ref: "eng"}},
		{"{unit: Platform}", code.Owner{Kind: code.OwnerUnit, Ref: "Platform"}},
		{"{unit: Infra}", code.Owner{Kind: code.OwnerUnit, Ref: "Infra"}},
		{"{unit: Tools}", code.Owner{Kind: code.OwnerUnit, Ref: "Tools"}},
		{"{unit: Sales}", code.Owner{Kind: code.OwnerUnit, Ref: "Sales"}},
		{"{seat: dev}", code.Owner{Kind: code.OwnerSeat, Ref: "dev"}},
		{"{seat: toolsmith}", code.Owner{Kind: code.OwnerSeat, Ref: "toolsmith"}},
		{"{seat: rep}", code.Owner{Kind: code.OwnerSeat, Ref: "rep"}},
		{"{seat: ceo}", code.Owner{Kind: code.OwnerSeat, Ref: "ceo"}},
	}
	var refused, admitted int
	for r := range o.AllRoles() {
		as := r.Handle()
		if as == "" {
			continue
		}
		for _, owner := range owners {
			for _, visibility := range []code.Visibility{code.VisibilityUnit, code.VisibilityCompany} {
				doc := chart + fmt.Sprintf(`code:
  native: true
  repos:
    - name: web
      owner: %s
      visibility: %s
      protect: [{branch: main}]
      mirror_to: {url: 'https://forge.example.com/acme/web.git', credential: '${FORGE_TOKEN}', as: %s}
`, owner.yaml, visibility, as)
				_, err := config.ParseCompany([]byte(doc))
				configRefuses := false
				for _, p := range config.Problems(err) {
					if p.Path != "code.repos[0].mirror_to.as" {
						t.Fatalf("%s as %s at %s: refused at %s (%s), the premise wants only mirror_to.as",
							owner.yaml, as, visibility, p.Path, p.Message)
					}
					configRefuses = true
				}
				repo := code.RepoRef{ID: "web", Name: "web", Owner: owner.owner, Visibility: visibility}
				domainRefuses := code.MirrorSeatRefusal(as, repo, roster) != ""
				if configRefuses != domainRefuses {
					t.Errorf("a push mirror of a repository owned by %s at visibility %s, as %s: validation "+
						"refuses=%v, the domain refuses=%v", owner.yaml, visibility, as, configRefuses,
						domainRefuses)
				}
				if domainRefuses {
					refused++
				} else {
					admitted++
				}
			}
		}
	}
	if refused == 0 || admitted == 0 {
		t.Fatalf("the premise: %d refused and %d admitted, want both verdicts reached", refused, admitted)
	}
}

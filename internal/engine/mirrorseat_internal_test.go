package engine

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitlet"
)

// A DECLARED PUSH MIRROR'S SEAT READS WHAT THE DOMAIN GRANTS IT: config's copy
// of the grant (CodeRepo.mirrorSeatReads, which cannot import the domain) and
// the domain's (gitlet.MirrorSeatRefusal over the chart's roster) are one
// rule. For every seat of a chart with every relation a grant reads (root and
// human seats, led units and inherited leads, a unit with no lead above it,
// Sales led from outside by ceo, tiers read_only to full_access), against a
// repository owned by every unit and by seats inside and outside them, at both
// visibilities, validation refuses at mirror_to.as exactly where the domain does.
//
// Mutations: walk the lead chain from the owner's PARENT (ceo's rows disagree,
// red); let every agent read at `visibility: unit` (outsiders' rows, red).
func TestADeclaredMirrorSeatReadsWhatTheDomainGrantsIt(t *testing.T) {
	t.Parallel()
	// Sales led from outside: only the owning unit's own lead reads, so a lead
	// walk begun above the owner misses it.
	chart := strings.Replace(rosterChart(""), "  - name: Sales\n    roles:",
		"  - name: Sales\n    lead: ceo\n    roles:", 1)
	if !strings.Contains(chart, "lead: ceo") {
		t.Fatal("the premise: rosterChart's Sales unit is not where this test gives it the lead ceo")
	}
	roster, o := rosterOfChart(t, chart)
	sales := gitlet.RepoRef{ID: "web", Name: "web", Owner: gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Sales"},
		Visibility: gitlet.VisibilityUnit}
	if why := gitlet.MirrorSeatRefusal("ceo", sales, roster, false); why != "" {
		t.Fatalf("the premise: ceo leads Sales and reads what it keeps to itself, but the domain says %s", why)
	}
	owners := []struct {
		yaml  string
		owner gitlet.Owner
	}{
		{"{unit: eng}", gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "eng"}},
		{"{unit: Platform}", gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Platform"}},
		{"{unit: Infra}", gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Infra"}},
		{"{unit: Tools}", gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Tools"}},
		{"{unit: Sales}", gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Sales"}},
		{"{seat: dev}", gitlet.Owner{Kind: gitlet.OwnerSeat, Ref: "dev"}},
		{"{seat: toolsmith}", gitlet.Owner{Kind: gitlet.OwnerSeat, Ref: "toolsmith"}},
		{"{seat: rep}", gitlet.Owner{Kind: gitlet.OwnerSeat, Ref: "rep"}},
		{"{seat: ceo}", gitlet.Owner{Kind: gitlet.OwnerSeat, Ref: "ceo"}},
	}
	var refused, admitted int
	for r := range o.AllRoles() {
		as := r.Handle()
		if as == "" {
			continue
		}
		for _, owner := range owners {
			for _, visibility := range []gitlet.Visibility{gitlet.VisibilityUnit, gitlet.VisibilityCompany} {
				doc := chart + fmt.Sprintf(`code:
  backend: gitlet
  remote_credentials:
    FORGE_TOKEN: {hosts: [forge.example.com]}
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
				repo := gitlet.RepoRef{ID: "web", Name: "web", Owner: owner.owner, Visibility: visibility}
				// The chart alone: no identity directory answers a Tier B
				// validation, so a person's app is the push's to judge
				// (gitlet.MirrorSeatRefusal's noGitlet).
				domainRefuses := gitlet.MirrorSeatRefusal(as, repo, roster, false) != ""
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

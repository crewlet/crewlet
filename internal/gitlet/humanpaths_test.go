package gitlet_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet"
)

// THE HUMAN PATHS DIGEST IS OF WHAT HOLDS A TARGET: the digest a revision's
// recorded human-path match is held to is a function of the human paths the
// repository's rules hold a target to and nothing else — the same in any
// order of the rules, the same whatever a rule's merge_by, and another once a
// rule names another glob or unless, matches another pattern, or stops
// holding the checks' own directory by setting agents_edit_checks. And the
// home's match names each entry by its place in the target's human paths, its
// digest the rules'.
//
// Mutation: digest a rule's own human paths alone, leaving out the checks'
// directory a rule requiring checks holds — agents_edit_checks digests alike,
// red.
func TestTheHumanPathsDigestIsOfWhatHoldsATarget(t *testing.T) {
	t.Parallel()
	deploy := bareGate()
	deploy.HumanPaths = []gitlet.HumanPath{{Glob: "deploy/**"}}
	checked := bareGate()
	checked.Checks = []string{"unit"}
	gates := []gitlet.RuleGate{{Pattern: mainRef, Gate: deploy}, {Pattern: "refs/heads/release/*", Gate: checked}}
	digest := gitlet.HumanPathsDigest(gates)

	if got := gitlet.HumanPathsDigest([]gitlet.RuleGate{gates[1], gates[0]}); got != digest {
		t.Error("one repository's rules digest apart in two orders")
	}
	same := slices.Clone(gates)
	same[0].Gate.MergeBy = gitlet.MergeByMaintainers
	if gitlet.HumanPathsDigest(same) != digest {
		t.Error("a rule's merge_by moves the digest of its human paths")
	}
	for name, change := range map[string]func(g []gitlet.RuleGate){
		"another glob":       func(g []gitlet.RuleGate) { g[0].Gate.HumanPaths = []gitlet.HumanPath{{Glob: "ops/**"}} },
		"another pattern":    func(g []gitlet.RuleGate) { g[0].Pattern = "refs/heads/trunk" },
		"agents_edit_checks": func(g []gitlet.RuleGate) { g[1].Gate.AgentsEditChecks = true },
		"an unless": func(g []gitlet.RuleGate) {
			g[0].Gate.HumanPaths = []gitlet.HumanPath{{Glob: "deploy/**",
				Unless: []gitlet.ChangeClass{gitlet.ChangeClassCommentOnly}}}
		},
	} {
		changed := slices.Clone(gates)
		change(changed)
		if gitlet.HumanPathsDigest(changed) == digest {
			t.Errorf("%s: the rules digest their human paths as they did before", name)
		}
	}

	m, got, err := gitlet.MatchTargetHumanPaths(gates, "refs/heads/release/1", []string{gitlet.ChecksFilePath, "app.go"},
		false, gitlet.RevisionClasses{})
	if err != nil || got != digest || !slices.Equal(m.Held, []int{0}) || len(m.Waived) > 0 {
		t.Fatalf("the home's match of release/1 is %+v under %s (%v), want its one human path, %s, holding it "+
			"under the rules' digest", m, got, err, gitlet.ChecksDirGlob)
	}
}

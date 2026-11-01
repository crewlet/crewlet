package engine

import "testing"

// A NODE'S CATALOGUE CREDITS A RUN'S COMMITS AS THE COMPANY SAYS: on for a
// company that says nothing (providers.sandbox.commit_attribution is on unless
// turned off), naming the run it is asked about, and off for one that set
// false — so a revision that turns the credit off reaches every launch made
// under the catalogue it builds.
//
// Mutation: hand the manager no AttributeCommits, or always true, and this
// goes red.
func TestTheCatalogueCreditsCommitsAsTheCompanySays(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		extra   string
		credits bool
	}{
		"a company saying nothing": {extra: "", credits: true},
		"a company turning it on":  {extra: "    commit_attribution: true", credits: true},
		"a company turning it off": {extra: "    commit_attribution: false", credits: false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			manager, err := buildSandbox(parseCompany(t, sandboxDoc(tc.extra)), nil, nil, nil)
			if err != nil || manager == nil {
				t.Fatalf("buildSandbox = %v, %v", manager, err)
			}
			got := manager.CommitAttribution("turn-1")
			if (got != nil) != tc.credits {
				t.Fatalf("the catalogue credits commits: %v, want %v", got != nil, tc.credits)
			}
			if got != nil && got.Run != "turn-1" {
				t.Errorf("the credit names the run %q, want the one asked about", got.Run)
			}
		})
	}
}

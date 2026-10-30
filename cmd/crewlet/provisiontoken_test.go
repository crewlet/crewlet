package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A PROVISION WHOSE ORG TOKEN RESOLVED EMPTY SAYS TO SET IT, and why this run
// needs it. Joined to the facts with "and", the sentence was a list with no
// remedy: "resolved empty, and this run registers the webhooks with it, and a
// token that can do that needs admin on each repository". Mutation: put
// either sentence back and its case fails.
func TestAProvisionWithAnEmptyTokenSaysToSetIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, block string
		run         func(path string, out, errs *bytes.Buffer) error
		want        string
	}{
		{"github", "  github:\n    token: \"${GH_PROVISION_TOKEN}\"\n",
			func(path string, out, errs *bytes.Buffer) error {
				return runGitHubProvision(invocation{environ: noEnviron}, []string{path, "-dry-run"}, out, errs)
			},
			`integrations.github.token ("${GH_PROVISION_TOKEN}") resolved empty. Set it: ` +
				`this run registers the webhooks with it`},
		{"jira", "  jira:\n    url: https://example.atlassian.net\n    email: bot@example.com\n" +
			"    token: \"${JIRA_PROVISION_TOKEN}\"\n",
			func(path string, out, errs *bytes.Buffer) error {
				return runJiraProvision(invocation{environ: noEnviron}, []string{path, "-dry-run"}, out, errs)
			},
			`integrations.jira.token ("${JIRA_PROVISION_TOKEN}") resolved empty. Set it: ` +
				`this run reads the instance as the org account it belongs to`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			doc := "name: Nimbus\nintegrations:\n" + tc.block +
				"roles:\n  - name: SWE\n    handle: swe\n    goal: ship\n"
			path := filepath.Join(t.TempDir(), "company.yaml")
			if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
				t.Fatal(err)
			}
			var out, errs bytes.Buffer
			err := tc.run(path, &out, &errs)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one saying %q", err, tc.want)
			}
		})
	}
}

package atlassian_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/envref"
)

// A PRODUCT READS ITS OWN SPELLINGS. CredentialReads names what CredentialAt
// may read for one product — each token spelling in each block it tries, and
// the address beside it — and so never a credential only the other product
// uses: a Confluence Data Center PAT in the shared block is not Jira's to
// read, and a Jira command must not have a running node reveal it.
func TestAProductReadsOnlyItsOwnSpellings(t *testing.T) {
	t.Parallel()
	env := map[string]map[string]string{
		"atlassian": {
			"JIRA_PERSONAL_TOKEN":       "${JIRA_PAT}",
			"CONFLUENCE_PERSONAL_TOKEN": "${CONFLUENCE_PAT}",
			"ATLASSIAN_EMAIL":           "${EMAIL}",
			"ATLASSIAN_SITE":            "${SITE}",
		},
		"jira":       {"JIRA_API_TOKEN": "${JIRA_TOKEN}"},
		"confluence": {"CONFLUENCE_API_TOKEN": "${CONFLUENCE_TOKEN}"},
	}
	for _, tc := range []struct {
		product atlassian.Product
		want    []string
	}{
		{atlassian.ProductJira, []string{"EMAIL", "JIRA_PAT", "JIRA_TOKEN"}},
		{atlassian.ProductConfluence, []string{"CONFLUENCE_PAT", "CONFLUENCE_TOKEN", "EMAIL"}},
	} {
		got := atlassian.CredentialReads(tc.product, env)
		slices.Sort(got)
		got = slices.Compact(got)
		if !slices.Equal(got, tc.want) {
			t.Errorf("CredentialReads(%q) = %v, want %v", tc.product, got, tc.want)
		}
		// Whatever the values resolve to, the scan reads nothing else: an
		// empty answer walks every spelling, a held one pairs an address.
		for _, answer := range []string{"", "a-held-value"} {
			atlassian.CredentialAt(tc.product, env, func(value string) string {
				for _, name := range envref.Names(value) {
					if !slices.Contains(tc.want, name) {
						t.Errorf("%q's scan read ${%s}, which CredentialReads omits", tc.product, name)
					}
				}
				if value == "" {
					return ""
				}
				return answer
			})
		}
	}
}

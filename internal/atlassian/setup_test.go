package atlassian_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/jira"
	"github.com/crewlet/crewlet/internal/setup"
)

// A SHARED FIELD IS ONE INPUT, so its two declarations have to say the same
// thing about it.
//
// Jira and Confluence both declare the account email, the API token, the
// cloud id and the link address, and the dialog renders exactly one of each.
// Which declaration it renders is a property of the form's layout rather than
// of either vendor, so two that disagree make the description of an input
// depend on the order its sections happen to be listed in. The API token
// carried a sentence naming the product it was declared in, and moving a
// shared field to the last section that declares it silently changed which
// product the one input claimed to be for.
func TestBothAtlassianProductsDescribeAsharedFieldTheSameWay(t *testing.T) {
	byField := map[string]setup.Requirement{}
	nothing := func(string) (string, bool) { return "", false }
	for _, r := range jira.Requirements(nil, nothing) {
		if r.Shared {
			byField[r.Field] = r
		}
	}

	for _, theirs := range confluence.Requirements(nil, nothing) {
		if !theirs.Shared {
			continue
		}
		ours, found := byField[theirs.Field]
		if !found {
			continue
		}
		for _, disagreement := range []struct {
			what string
			a, b string
		}{
			{"Label", ours.Label, theirs.Label},
			{"Help", ours.Help, theirs.Help},
			{"Where", ours.Where, theirs.Where},
			{"LinkText", ours.LinkText, theirs.LinkText},
			{"VendorURL", ours.VendorURL, theirs.VendorURL},
		} {
			if disagreement.a != disagreement.b {
				t.Errorf("shared field %q: Jira's %s is %q, Confluence's is %q",
					theirs.Field, disagreement.what, disagreement.a, disagreement.b)
			}
		}
	}
}

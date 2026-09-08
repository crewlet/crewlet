package atlassian_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/config"

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
	for _, r := range jira.Requirements(nil, true, nothing) {
		if r.Shared {
			byField[r.Field] = r
		}
	}

	for _, theirs := range confluence.Requirements(nil, true, nothing) {
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

// THE ORGANIZATION ID IS A VALUE IN THE SAME DOCUMENT AS THE KEY, so it is
// read the same way.
//
// A company keeping it in the sealed store had the literal text
// "${ATLASSIAN_ORG_ID}" reported as present and resolved, and sent to
// Atlassian as the subject of every admin call. The field is not a credential
// and does not need to be: what makes a value referenceable is that the
// engine reads it through the resolver, and the form has to report the two
// facts separately or a reference naming nothing shows green.
func TestTheOrganizationIdReportsWhetherItsReferenceResolves(t *testing.T) {
	store := map[string]string{"HAVE": "org-42"}
	resolve := func(name string) (string, bool) { v, ok := store[name]; return v, ok }

	for name, tc := range map[string]struct {
		stored       string
		wantPresent  bool
		wantResolved bool
	}{
		"a literal":               {stored: "org-42", wantPresent: true, wantResolved: true},
		"a reference that is":     {stored: "${HAVE}", wantPresent: true, wantResolved: true},
		"a reference that is not": {stored: "${GONE}", wantPresent: true, wantResolved: false},
		"nothing":                 {stored: "", wantPresent: false},
	} {
		t.Run(name, func(t *testing.T) {
			reqs := atlassian.Requirements(&config.Atlassian{OrgID: tc.stored}, resolve)
			for _, r := range reqs {
				if r.Field != "org_id" {
					continue
				}
				if r.Present != tc.wantPresent {
					t.Fatalf("present = %v, want %v", r.Present, tc.wantPresent)
				}
				if !tc.wantPresent {
					return
				}
				if r.Resolved == nil || *r.Resolved != tc.wantResolved {
					t.Errorf("resolved = %v, want %v", r.Resolved, tc.wantResolved)
				}
				return
			}
			t.Fatal("no org_id requirement")
		})
	}
}

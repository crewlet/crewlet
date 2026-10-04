package atlassian_test

import (
	"context"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/provision"
)

// A TEARDOWN REPORTS AN END STATE, AND NAMES BOTH OF A SEAT'S SLOTS.
//
// The account is already absent here, which is the case that makes the rule
// necessary rather than convenient: every teardown is "remove this if it is
// there" and is RETRIED on failure, so a result that named only what THIS call
// deleted would come back empty on the retry, and the block would drop with
// the credentials still sealed — the original defect, reached through the
// recovery path.
//
// BOTH SLOTS, because Atlassian assigns the account's address at creation and
// its products authenticate base64(address:token): a pass seals two values per
// agent and a deletion strands two. Naming only the token leaves the address
// resolving, which is half a stale identity rather than none.
func TestATeardownReportsAnEndStateAndNamesBothSlots(t *testing.T) {
	t.Parallel()
	o := &stubOrg{empty: true}
	plan := &provision.Plan{}
	plan.Add(provision.Seat{
		Handle: "sre-lead", Role: "SRE Lead",
		TokenVar: "SRE_ATLASSIAN_TOKEN", EmailVar: "SRE_ATLASSIAN_EMAIL",
	})

	removed, err := atlassian.Teardown(context.Background(), atlassian.TeardownOptions{
		Client: atlassian.NewClient(atlassian.ClientOptions{BaseURL: o.server(t).URL}),
		OrgID:  "org-1", Key: "key", Plan: plan,
	})
	if err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	want := []string{"SRE_ATLASSIAN_EMAIL", "SRE_ATLASSIAN_TOKEN"}
	if got := removed.Secrets(); !slices.Equal(got, want) {
		t.Errorf("secrets = %v, want %v", got, want)
	}
}

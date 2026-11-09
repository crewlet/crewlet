package seataccess_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/seataccess/seataccesstest"

	_ "github.com/crewlet/crewlet/internal/atlassian/agentaccess"
	_ "github.com/crewlet/crewlet/internal/aws"
	_ "github.com/crewlet/crewlet/internal/cloudflare"
	_ "github.com/crewlet/crewlet/internal/datadog"
	_ "github.com/crewlet/crewlet/internal/expo"
	_ "github.com/crewlet/crewlet/internal/github"
	_ "github.com/crewlet/crewlet/internal/gitlab"
	_ "github.com/crewlet/crewlet/internal/googlecloud"
	_ "github.com/crewlet/crewlet/internal/mattermost"
	_ "github.com/crewlet/crewlet/internal/slack"
	_ "github.com/crewlet/crewlet/internal/ssh"
)

// THE DOCUMENT'S RULES AND THE VENDORS AGREE ON WHICH LANES REQUIRE A SCOPE, so
// `crewlet validate`, the access routes and the sync refuse the same agent.
func TestTheDocumentAndTheVendorsAgreeOnARequiredScope(t *testing.T) {
	t.Parallel()
	seen := 0
	for _, v := range seataccess.Registered() {
		for _, spec := range v.Catalog().Lanes {
			seen++
			if got := config.SeatScopeRequired(string(spec.Lane)); got != spec.ScopeRequired {
				t.Errorf("%s: config says scope required = %v, the vendor %v", spec.Lane, got, spec.ScopeRequired)
			}
		}
	}
	if seen != len(config.SeatAccessLanes()) {
		t.Fatalf("checked %d lanes of the %d the document has: a vendor is not registered here",
			seen, len(config.SeatAccessLanes()))
	}
}

// EVERY EXISTING SCOPED LANE'S REACH IS THE DOCUMENT'S, KNOWN WHETHER OR NOT ITS
// VENDOR ANSWERS: an offer nobody could read still carries the list the
// company wrote, or reads open where it wrote none, and says it was read, so
// an empty authoritative list is never drawn as "could not read". Mutation:
// drop a vendor's Reach, and its lane reads as unread here.
func TestEveryDocumentScopedLaneReadsItsReachWithoutItsVendor(t *testing.T) {
	t.Parallel()
	listed := &config.Company{
		Integrations: config.Integrations{
			GitLab:     &config.GitLab{Provisioning: &config.GitLabProvisioning{Projects: []string{"acme/api"}}},
			GitHub:     &config.GitHub{Provisioning: &config.GitHubProvisioning{Repos: []string{"acme/web"}}},
			Mattermost: &config.Mattermost{Provisioning: &config.MattermostProvisioning{Channels: []string{" ops ", "ops"}}},
		},
		Roles: []config.Role{{Name: "SRE", Handle: "sre", Project: "ENG", Space: "OPS"}},
	}
	roster, err := listed.Organization()
	if err != nil {
		t.Fatal(err)
	}
	empty := &config.Company{Roles: []config.Role{{Name: "SRE", Handle: "sre"}}}
	none, err := empty.Organization()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		lane seataccess.Lane
		want []string
	}{
		{seataccess.LaneGitLab, []string{"acme/api"}},
		{seataccess.LaneGitHub, []string{"acme/web"}},
		{seataccess.LaneMattermost, []string{"ops"}},
		{seataccess.LaneJira, []string{"ENG"}},
		{seataccess.LaneConfluence, []string{"OPS"}},
	} {
		v, ok := seataccess.Lookup(tc.lane.Kind())
		if !ok {
			t.Fatalf("%s: no vendor registered", tc.lane)
		}
		unread := seataccess.UnreadOffers(tc.lane.Kind(), false)[tc.lane]
		got := seataccess.WithReach(unread, v, listed, roster)
		if !got.Scoped || !got.ScopeLive || got.ScopeOpen || !slices.Equal(got.Scope, tc.want) {
			t.Errorf("%s with a list: %+v, want scope %v, read", tc.lane, got, tc.want)
		}
		open := seataccess.WithReach(unread, v, empty, none)
		if !open.ScopeLive || !open.ScopeOpen || len(open.Scope) != 0 {
			t.Errorf("%s with none: %+v, want an open reach, read", tc.lane, open)
		}
	}
	// A VENDOR THAT READS ITS REACH FROM ITSELF is left as it answered.
	fake := seataccesstest.NewVendor(integration.KindDatadog, "APP_KEY")
	unread := seataccess.Options{Lane: seataccess.LaneDatadog, Scoped: true}
	if got := seataccess.WithReach(unread, fake, listed, roster); got.ScopeLive {
		t.Errorf("a vendor that reads its own reach was marked read: %+v", got)
	}
}

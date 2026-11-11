package seataccess_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/seataccess"
)

// EVERY LANE'S GRANT IS NAMED ON THE CARD THE DASHBOARD DRAWS FOR ITS
// INTEGRATION: an agent told to ask for access there, or a person told where to
// set it, finds a card of that name with an Agents tab.
func TestEveryLanesGrantIsPlacedOnItsIntegrationsCard(t *testing.T) {
	t.Parallel()
	card := map[seataccess.Lane]string{
		seataccess.LaneSlack: "Slack", seataccess.LaneMattermost: "Mattermost",
		seataccess.LaneJira: "Atlassian", seataccess.LaneConfluence: "Atlassian",
		seataccess.LaneGitHub: "GitHub", seataccess.LaneGitLab: "GitLab",
		seataccess.LaneDatadog: "Datadog", seataccess.LaneCloudflare: "Cloudflare",
		seataccess.LaneExpo: "Expo", seataccess.LaneDigitalOcean: "DigitalOcean",
		seataccess.LaneScaleway: "Scaleway", seataccess.LaneAWS: "AWS",
		seataccess.LaneGoogleCloud: "Google Cloud", seataccess.LaneSSH: "SSH",
	}
	for _, lane := range seataccess.Lanes() {
		name, ok := card[lane]
		if !ok {
			t.Errorf("lane %s has no integration card named here; add the card's title", lane)
			continue
		}
		if got, want := seataccess.GrantPlace(lane), "Settings, Integrations, "+name+", Agents"; got != want {
			t.Errorf("GrantPlace(%s) = %q, want %q", lane, got, want)
		}
	}
}

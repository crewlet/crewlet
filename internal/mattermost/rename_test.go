package mattermost_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provision"
)

// A RENAMED SEAT KEEPS THE BOT IT IS ALREADY WORKING AS.
//
// A bot's username is the only thing a later pass can match an account on,
// and Mattermost keeps no field of this engine's own. Derived from the LIVE
// handle it moved on every rename, and one rename did three things at once,
// in somebody's real instance: the next pass found nothing under the new
// name and created a SECOND bot; the first stayed live, in the channels,
// holding a token this engine had already sealed and would never revoke; and
// `-decommission` read the first as a departed seat and disabled the account
// the agent was actually posting as.
//
// Asserted as a COMPARISON between the same seat before and after the rename,
// rather than against a spelled-out string: what matters is that the name did
// not move, and a literal here would go on passing if both halves changed
// together.
func TestARenamedSeatKeepsItsBot(t *testing.T) {
	t.Parallel()
	cfg := enabledChat()

	before := chatSeat("SWE", "${MM_TOKEN_SWE}", "eng")
	after := chatSeat("SWE", "${MM_TOKEN_SWE}", "eng")
	after.DeclaredHandle, after.OriginHandle = "platform-swe", "swe"

	was := planSeat(t, before, cfg)
	is := planSeat(t, after, cfg)

	if was.Handle == is.Handle {
		t.Fatalf("the fixture did not rename the seat: both plans say %q", was.Handle)
	}
	if is.Handle != "platform-swe" {
		t.Errorf("plan handle = %q, want the seat as the chart addresses it today", is.Handle)
	}
	// THE ACCOUNT, which is what must not move.
	if got, want := mattermost.BotUsername(cfg.Provisioning, is.Origin),
		mattermost.BotUsername(cfg.Provisioning, was.Origin); got != want {
		t.Errorf("bot username after the rename = %q, want the one it already "+
			"has, %q: a second bot is created under the new name and the first "+
			"stays live holding a sealed token", got, want)
	}
	if is.Email != was.Email {
		t.Errorf("bot email after the rename = %q, want %q", is.Email, was.Email)
	}
	// AND THE TOKEN IT MINTS UNDER, which the decommission sweep recovers
	// from the username: the two spellings have to be one.
	if got, want := mattermost.TokenDescription(is.Origin),
		mattermost.TokenDescription(was.Origin); got != want {
		t.Errorf("token description after the rename = %q, want %q — the "+
			"sweep would leave every earlier token live and unrecognised",
			got, want)
	}
}

// planSeat is the one seat mattermost.PlanFor makes of a role.
func planSeat(t *testing.T, role *org.Role, cfg *config.Mattermost) provision.Seat {
	t.Helper()
	plan, err := mattermost.PlanFor(&org.Organization{
		Name: "Nimbus", Roles: []*org.Role{role},
	}, cfg)
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	if len(plan.Seats) != 1 {
		t.Fatalf("planned %d seats, want 1: %+v", len(plan.Seats), plan)
	}
	return plan.Seats[0]
}

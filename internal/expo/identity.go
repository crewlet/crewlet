package expo

import (
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/seatname"
)

// identitySpec is Expo's identity: each seat's own robot, made and handed in
// by a person, never the company's (ADR-0179). Its token is the lane's own
// sealed credential, so nothing beside it is sealed here.
var identitySpec = seataccess.IdentitySpec{
	Ways: []seataccess.IdentityWay{seataccess.WayHandedIn}, Default: seataccess.IdentityOwn,
	Name: seatname.Expo,
}

// robotChecks are the reads a pasted token's check makes, as the sheet lists them.
var robotChecks = []seataccess.GuideCheck{
	{ID: "who", Label: "Read the robot behind the token"},
	{ID: "role", Label: "Read its role"},
	{ID: "account", Label: "Read the account it belongs to"},
}

// IdentityGuide is the sheet that sets up a seat's robot, in Expo's words.
func (accessVendor) IdentityGuide(c *config.Company, seat seataccess.SeatRef, _ seataccess.Lane,
	want seataccess.Desired) seataccess.IdentityGuide {

	name := RobotName(c.Name, seat.Handle)
	role := LevelName(want.Level)
	if want.Level == "" {
		role = LevelName(config.ExpoLevelViewer)
	}
	account, link := "the account", "https://expo.dev/settings"
	if b := c.Integrations.Expo; b != nil && b.Provisioning != nil {
		if a := strings.TrimSpace(b.Provisioning.AccountName); a != "" && !envref.Has(a) {
			account, link = "the "+a+" account", AccessTokensURL(a)
		}
	}
	return seataccess.IdentityGuide{
		Vendor: "Expo", Noun: "robot", Records: "its builds and updates",
		Why:  "Each agent acts at Expo as its own robot, so its builds and updates name it.",
		Name: name, NameRule: seatname.Expo.Words, NameWhat: seatname.Expo.What, Role: role,
		Never: "Admin",
		Steps: []seataccess.GuideStep{
			{Text: "Open Account settings, then Access tokens, in " + account + "."},
			{Text: "Under Robots, press Add robot."},
			{Text: "Name:", Copy: &seataccess.GuideCopy{Label: "robot's name", Value: name, Rule: seatname.Expo.Words}},
			{Text: "Role: " + role + ". Never Admin."},
			{Text: "On the robot's row, press Create token, then copy it. Expo shows it once."},
		},
		Link: seataccess.GuideLink{Label: "Open Access tokens", URL: link},
		Note: "Only an account owner or admin can add robots.",
		Back: seataccess.GuideBack{Kind: seataccess.BackToken, Label: "Token",
			Hint: "Crewlet checks it with Expo and seals it."},
		Checks: robotChecks,
	}
}

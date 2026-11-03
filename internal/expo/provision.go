package expo

import (
	"net/url"
	"strings"

	"github.com/crewlet/crewlet/internal/org"
)

// ProvisionerSecret is the sealed name the provisioner token is kept under.
const ProvisionerSecret = "EXPO_ACCESS_TOKEN"

// TokenField is the one credential the engine seals per seat for Expo: the
// robot's access token, under EXPO_TOKEN_<HANDLE>.
const TokenField = "TOKEN"

// ToolEnv is the variable the EAS CLI reads its access token from, which a
// seat's `role.sandbox.env` sets to the seat's sealed token.
const ToolEnv = "EXPO_TOKEN"

// CLIRelease is the one pinned EAS CLI release, which every page and skill
// launching `npx eas-cli` names (TestEveryEASCLILaunchNamesThePinnedRelease):
// the newest on npm on 2026-11-02.
const CLIRelease = "27.2.0"

// RobotName is the name one seat's robot is created under, so a person
// reading the account's robots can tell which agent of which company it is.
// It labels the robot and never identifies it.
func RobotName(company, handle string) string {
	name := "crewlet-"
	if slug := org.Slugify(company); slug != "" {
		name += slug + "-"
	}
	return name + strings.TrimSpace(handle)
}

// TokenNote is the note every token the engine mints for a seat carries, which
// is how the engine tells its own tokens on a robot from any a person added.
func TokenNote(handle string) string { return "crewlet seat " + strings.TrimSpace(handle) }

// engineTokens are the live tokens on robot that carry the seat's note.
func engineTokens(robot Robot, handle string) []Token {
	var out []Token
	note := TokenNote(handle)
	for _, t := range robot.Tokens {
		if !t.Revoked && t.Note == note {
			out = append(out, t)
		}
	}
	return out
}

// AccessTokensURL is the account's settings page listing its robot users and
// access tokens, where a person settles what only Expo can.
func AccessTokensURL(account string) string { return accountURL(account, "access-tokens") }

// MembersURL is the account's members page, where a person gives a member the
// Admin role.
func MembersURL(account string) string { return accountURL(account, "members") }

// accountURL is one of the account's settings pages on expo.dev.
func accountURL(account, page string) string {
	return "https://expo.dev/accounts/" + url.PathEscape(strings.TrimSpace(account)) + "/settings/" + page
}

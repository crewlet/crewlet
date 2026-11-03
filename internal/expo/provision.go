package expo

import (
	"net/url"
	"strings"

	"github.com/crewlet/crewlet/internal/org"
)

// TokenField is the one credential the engine seals per seat for Expo: the
// token a person created for the seat's robot, under EXPO_TOKEN_<HANDLE>.
const TokenField = "TOKEN"

// ToolEnv is the variable the EAS CLI reads its access token from, which a
// seat's `role.sandbox.env` sets to the seat's sealed token.
const ToolEnv = "EXPO_TOKEN"

// CLIRelease is the one pinned EAS CLI release, which every page and skill
// launching `npx eas-cli` names (TestEveryEASCLILaunchNamesThePinnedRelease):
// the newest on npm on 2026-11-02.
const CLIRelease = "27.2.0"

// RobotName is the name a person gives one seat's robot at Expo, so the
// account's robot list says which agent of which company it is. It labels the
// robot and never identifies it.
func RobotName(company, handle string) string {
	name := "crewlet-"
	if slug := org.Slugify(company); slug != "" {
		name += slug + "-"
	}
	return name + strings.TrimSpace(handle)
}

// AccessTokensURL is the account's settings page listing its robot users and
// access tokens, where a person settles what only Expo can.
func AccessTokensURL(account string) string { return accountURL(account, "access-tokens") }

// accountURL is one of the account's settings pages on expo.dev.
func accountURL(account, page string) string {
	return "https://expo.dev/accounts/" + url.PathEscape(strings.TrimSpace(account)) + "/settings/" + page
}

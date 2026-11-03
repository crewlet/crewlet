package digitalocean

import (
	"strings"

	"github.com/crewlet/crewlet/internal/org"
)

// TokenField is the credential the engine seals per seat: the token a person
// created for the seat, under DIGITALOCEAN_TOKEN_<HANDLE>. RetiredField is a
// replaced or turned-off seat's token, sealed where no box reads it, only
// until Crewlet's revoke of it at DigitalOcean succeeds.
const (
	TokenField   = "TOKEN"
	RetiredField = "RETIRED_TOKEN"
)

// CredentialKeys are the keys a DigitalOcean token is read under by a
// recognised tool server: DigitalOcean's MCP servers read it as a bearer
// Authorization header.
//
//nolint:gochecknoglobals // an immutable list, not state
var CredentialKeys = []string{"Authorization"}

// ToolEnv is the variable doctl reads its token from, which a seat's
// `role.sandbox.env` sets to the seat's sealed token.
const ToolEnv = "DIGITALOCEAN_ACCESS_TOKEN"

// DoctlRelease is the one pinned doctl release, which every page installing
// doctl names (TestEveryDoctlInstallNamesThePinnedRelease): the newest on
// GitHub on 2026-11-03.
const DoctlRelease = "2.1.0"

// TokensURL is the control panel's personal access tokens list, where each
// token's row offers Regenerate Token, and NewTokenURL the form that creates
// one, which a wait for a new token links.
const (
	TokensURL   = "https://cloud.digitalocean.com/account/api/tokens"
	NewTokenURL = "https://cloud.digitalocean.com/account/api/tokens/new"
)

// TokenName is the name a person gives one seat's token at one level, so the
// team's token list says which agent of which company it is and at what level:
// a level change makes a second token, and the person deletes the older by its
// name. It labels the token and never identifies it.
func TokenName(company, handle, level string) string {
	name := "crewlet-"
	if slug := org.Slugify(company); slug != "" {
		name += slug + "-"
	}
	return name + strings.TrimSpace(handle) + "-" + level
}

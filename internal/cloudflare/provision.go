package cloudflare

import (
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/org"
)

// TokenField is the one credential the engine seals per seat for Cloudflare:
// the seat's account API token, under CLOUDFLARE_API_TOKEN_<HANDLE>.
const TokenField = "API_TOKEN"

// SeatEnv is the `mcp_env` block a seat's bound token is found under: the
// name the MCP catalogue gives Cloudflare's own server.
const SeatEnv = "cloudflare"

// CredentialKeys are the keys a Cloudflare token is read under by a recognised
// tool server: Cloudflare's MCP server reads it as a bearer Authorization
// header.
//
//nolint:gochecknoglobals // an immutable list, not state
var CredentialKeys = []string{"Authorization"}

// Token lifetime: every seat token expires TokenLifetime after it is minted
// or rolled, and is rolled once fewer than RenewWithin remain, so a value
// lives at most 90 days and a pass has 30 days of retries before it lapses.
const (
	TokenLifetime = 90 * 24 * time.Hour
	RenewWithin   = 30 * 24 * time.Hour
)

// TokenName is the name one seat's token is created under, so a person
// reading the account's token list can tell which agent of which company it
// is. It labels the token and never identifies it.
func TokenName(company, handle string) string {
	name := "crewlet-"
	if slug := org.Slugify(company); slug != "" {
		name += slug + "-"
	}
	return name + strings.TrimSpace(handle)
}

// renewDue reports a token that expires inside the renewal window. A token
// with no expiry is due too: every seat token is minted with one.
func renewDue(expires, now time.Time) bool {
	return expires.IsZero() || expires.Sub(now) < RenewWithin
}

// expiry is when a token minted or rolled at now expires, to the second.
func expiry(now time.Time) time.Time {
	return now.Add(TokenLifetime).UTC().Truncate(time.Second)
}

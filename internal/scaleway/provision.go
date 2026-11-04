package scaleway

import (
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/org"
)

// ProvisionerSecret is the sealed name the provisioner's secret key is kept
// under.
const ProvisionerSecret = "SCALEWAY_SECRET_KEY"

// The two values the engine seals per seat: the seat's API key's secret, the
// credential, under SCALEWAY_SECRET_KEY_<HANDLE>, and its access key, which
// names it, under SCALEWAY_ACCESS_KEY_<HANDLE>.
const (
	SecretKeyField = "SECRET_KEY"
	AccessKeyField = "ACCESS_KEY"
)

// The variables the scw CLI and its MCP server read the seat's key from.
const (
	SecretKeyEnv = "SCW_SECRET_KEY"
	AccessKeyEnv = "SCW_ACCESS_KEY"
)

// The settings the engine puts in a seat's box beside its key
// ([accessVendor.BoxDefaults]).
const (
	OrganizationEnv = "SCW_DEFAULT_ORGANIZATION_ID"
	ProjectEnv      = "SCW_DEFAULT_PROJECT_ID"
	// CheckVersionEnv off keeps the CLI's update notice out of the agent's
	// output.
	CheckVersionEnv = "SCW_DISABLE_CHECK_VERSION"
)

// SeatEnv is the `mcp_env` block a seat's bound key is found under: the name
// the MCP catalogue gives the scw CLI's own server.
const SeatEnv = "scaleway"

// CredentialKeys are the keys a recognised tool server reads a seat's key
// under: the scw CLI's MCP server reads both from its environment.
//
//nolint:gochecknoglobals // an immutable list, not state
var CredentialKeys = []string{SecretKeyEnv, AccessKeyEnv}

// CLIRelease is the one pinned scw CLI release, which every page and skill
// downloading it names (TestEveryScwDownloadNamesThePinnedRelease): the
// newest on GitHub on 2026-11-03.
const CLIRelease = "2.65.2"

// Key lifetime: every seat key expires KeyLifetime after it is minted and is
// replaced once fewer than RenewWithin remain, so a key lives at most 90 days
// and a pass has 30 days of retries before it lapses. Scaleway lets no key's
// expiry change after it is created, so a renewal is a new key.
const (
	KeyLifetime = 90 * 24 * time.Hour
	RenewWithin = 30 * 24 * time.Hour
)

// ApplicationName is the name one seat's application and policy are created
// under, so a person reading the organization's IAM can tell which agent of
// which company it is. It labels them and never identifies them.
func ApplicationName(company, handle string) string {
	name := "crewlet-"
	if slug := org.Slugify(company); slug != "" {
		name += slug + "-"
	}
	return name + strings.TrimSpace(handle)
}

// KeyDescription is the description every key the engine mints for a seat
// carries, which is how the engine tells its own keys on an application from
// any a person added.
func KeyDescription(handle string) string { return "crewlet seat " + strings.TrimSpace(handle) }

// description is what the engine writes on an application and its policy.
func description(handle string) string {
	return "Crewlet agent " + strings.TrimSpace(handle) + ". Managed by Crewlet: change it on the Agents tab."
}

// renewDue reports a key that expires inside the renewal window. A key with no
// expiry is due too: every seat key is minted with one.
func renewDue(expires, now time.Time) bool {
	return expires.IsZero() || expires.Sub(now) < RenewWithin
}

// expiry is when a key minted at now expires, to the second.
func expiry(now time.Time) time.Time {
	return now.Add(KeyLifetime).UTC().Truncate(time.Second)
}

// Package googlecloud gives each agent seat its own Google Cloud service
// account, granted the seat's level on the projects it names, through a
// provisioner service account the engine reaches with no service account key.
//
// # No key anywhere
//
// Google blocks service account key creation by default and advises against
// keys. So the engine holds an ES256 key of its own, whose public half the
// owner uploads to a Workload Identity Federation provider. The engine signs a
// ten-minute token for that one provider, exchanges it at Google's Security
// Token Service, and impersonates the provisioner for an hour ([Client]). An
// agent's own token is minted the same way, by impersonating its account with
// the provisioner's token ([Client.SeatToken]), and lives only in memory.
//
// # The provisioner is contained twice
//
// On each project agents may use, the provisioner holds Project IAM Admin three
// times, each under a condition listing one level's roles, so it can grant and
// revoke only the level table, one list per write. In the identity project it
// holds Service Account Admin, which reaches only Crewlet's own accounts. A
// project is offered to agents only when all three conditions and the owner's
// crewletOperator role are exactly this package's ([discover]).
//
// # Only what the ledger records is the engine's
//
// A service account is addressed by the unique ID the ledger recorded when the
// engine made it. Nothing is adopted by its name: a deleted name made again is
// a different principal.
//
// # Google answers a read after a write late
//
// A new account can be unknown to another project's policy for minutes, and
// any binding, disable or delete can read back stale. Every read that follows
// the engine's own write waits through one bound ([awaitObserved]), within
// which a stale answer is never drift and never a fault.
//
// # The atlas reads the projects as the provisioner
//
// Every active project the provisioner can read is an account of the atlas,
// and the APIs enabled on it say what it is used for, as closed words
// (ADR-0137, atlas.go). The read takes no role beyond the provisioner's own,
// and a read's 429 is waited out where a write's never is ([Client.do]).
//
// Where the company asks, what each project runs is searched as the atlas's
// own service account, crewlet-atlas, which the federation impersonates
// directly and which holds one permission (ADR-0149, assets.go).
package googlecloud

import "net/url"

// Backend is the integration's name: the config block's key, the integration
// kind and the seat access lane.
const Backend = "google_cloud"

// AccessTokenField is the one value the engine holds per seat: the access
// token it issues at use for an hour, GOOGLE_CLOUD_ACCESS_TOKEN_<HANDLE>,
// which is never sealed.
const AccessTokenField = "ACCESS_TOKEN"

// CredentialKeys are the keys a seat's access token is read under by a
// recognised tool server: Google's managed MCP servers read it as a bearer
// Authorization header.
//
//nolint:gochecknoglobals // an immutable list, not state
var CredentialKeys = []string{"Authorization"}

// FederationKeySecret is the sealed name the engine's federation key is kept
// under.
const FederationKeySecret = "GOOGLE_CLOUD_FEDERATION_KEY"

// ScopeCloudPlatform is the OAuth scope every token is minted with. What a
// token reaches is the account's IAM roles, never its scope.
const ScopeCloudPlatform = "https://www.googleapis.com/auth/cloud-platform"

// The console pages where a person settles what only Google can.
const (
	IAMURL             = "https://console.cloud.google.com/iam-admin/iam"
	ServiceAccountsURL = "https://console.cloud.google.com/iam-admin/serviceaccounts"
	ProvidersURL       = "https://console.cloud.google.com/iam-admin/workload-identity-pools"
	APIsURL            = "https://console.cloud.google.com/apis/dashboard"
	QuotasURL          = "https://console.cloud.google.com/iam-admin/quotas"
	RolesURL           = "https://console.cloud.google.com/iam-admin/roles"
	CloudShellURL      = "https://shell.cloud.google.com/"
)

// projectURL is a console page for one project, which the console selects by
// its `project` query.
func projectURL(page, project string) string { return page + "?project=" + url.QueryEscape(project) }

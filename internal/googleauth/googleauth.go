// Package googleauth owns every way the engine gets a Google token: the
// company's Google identity reached by federation, the accounts it
// impersonates, the token caches, and the one path a request to Google is
// built and its refusal read. Google Cloud and every Google product call it;
// there is no second copy of federation, impersonation, token caching, refusal
// holds or Google's error parsing.
//
// # No key anywhere
//
// Google blocks service account key creation by default and advises against
// keys. So the engine holds an ES256 key of its own, whose public half the
// owner uploads to a Workload Identity Federation provider. The engine signs a
// ten-minute token for that one provider, exchanges it at Google's Security
// Token Service, and impersonates the provisioner for an hour ([Connection]).
// An agent's own token is minted the same way, by impersonating its account
// with the provisioner's token ([Connection.SeatToken]), and lives only in
// memory.
//
// # A token is keyed by its scopes
//
// The STS exchange is always cloud-platform. Only the impersonation names the
// scopes a token carries, so one account's tokens for two scope sets are two
// cache entries ([SeatIssuer]), and a token is never handed to a caller that
// asked for scopes it was not minted with.
//
// # The configuration stays where it is
//
// The identity is configured by integrations.google_cloud.provisioning (the
// provisioner, the provider and the federation key), which every stored
// document already carries. [For] reads it from there.
package googleauth

import "time"

// ScopeCloudPlatform is the scope the federation exchange is made with, and
// the one Google Cloud mints its tokens with. What such a token reaches is the
// account's IAM roles, never its scope.
const ScopeCloudPlatform = "https://www.googleapis.com/auth/cloud-platform"

// Google's token hosts. Fixed rather than configurable, so the federation
// key's signatures and the provisioner's token only ever go to Google.
const (
	stsURL         = "https://sts.googleapis.com/v1/token"
	credentialsAPI = "https://iamcredentials.googleapis.com/v1"
)

// ClientTimeout bounds one call. Every call runs inside a pass or a tool call
// with its own deadline, so this is slack for a slow answer rather than a
// budget to spend.
const ClientTimeout = 15 * time.Second

// MaxTokenLifetime is the longest token Google issues by impersonation without
// an organization policy extending it: an hour. A federated account's token is
// minted for that long and renewed federatedRenew before it ends.
const (
	MaxTokenLifetime = time.Hour
	federatedRenew   = 5 * time.Minute
)

// maxResponse bounds one answer: a policy or a page of services is tens of
// kilobytes, and the cap stops a proxy's page being read whole.
const maxResponse = 8 << 20

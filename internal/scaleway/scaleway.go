// Package scaleway gives each agent seat its own Scaleway IAM application,
// with one policy granting the seat's level on its projects and its own API
// key.
//
// # An application per seat
//
// Scaleway's non-human principal is the IAM application: it holds API keys and
// is the principal of policies, so each agent acts as itself and its actions
// are attributable and revocable on their own. The per-seat access vendor
// ([seataccess.Vendor]) makes the application through the company's provisioner
// key, writes its one policy, mints its key, rewrites the policy's rules when
// the seat's level or projects change, rotates the key before it expires, and
// deletes all three when the seat is turned off.
//
// # Only what the ledger records is the engine's
//
// Application names are not unique and anyone holding IAM rights can create
// one under any name, so a name is never proof of provenance. The engine acts
// only on application IDs its ledger recorded when it made them. On such an
// application, the policy named for the seat is the engine's and every other
// policy is a grant somebody else made, reported and never deleted.
//
// # The provisioner can reach the whole organization
//
// Scaleway checks no grant against what its granter holds, and no condition
// can tell one application's keys from another's. So the provisioner key can
// mint a key for, and grant anything to, any application in the organization.
// What bounds an agent is this package's fixed level table and its
// never-granted list, both held by tests.
//
// # The key reaches the agent's box by reference
//
// The seat's secret key and access key are sealed as
// SCALEWAY_SECRET_KEY_<HANDLE> and SCALEWAY_ACCESS_KEY_<HANDLE>. The scw CLI in
// the seat's box reads them as SCW_SECRET_KEY and SCW_ACCESS_KEY, two lines of
// `role.sandbox.env`, and the engine supplies the organization and the default
// project itself ([accessVendor.BoxDefaults]).
//
// # The atlas reads the projects under the provisioner key
//
// The Organization and each of its projects are accounts of the atlas
// (ADR-0137, atlas.go): one listing, under the permission set the provisioner
// already holds, and nothing inside a project.
//
// # The atlas reads the products under an application of its own
//
// Where a company switches `integrations.scaleway.atlas` on, the company pass
// makes one more application, named for the atlas, with one policy of nine
// read-only permission sets on every project and one key it rotates
// (ADR-0144, atlasapp.go), and the atlas reads what the projects hold under
// that key (atlasproducts.go). It is the atlas's alone: sealed under fixed
// names, bound to no seat, recorded by its ID like every application the
// engine makes, and deleted when the switch goes off. An application somebody
// deleted at Scaleway is a person revoking the atlas there, and is not made
// again unasked.
package scaleway

// Backend is the integration's name: the config block's key, the integration
// kind and the seat access lane.
const Backend = "scaleway"

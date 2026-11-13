// Package cloudflare gives each agent seat its own Cloudflare account API
// token, scoped to the company's zones at the seat's level.
//
// # The token is the account
//
// Cloudflare has no service account to create: an account-owned API token is
// the principal. So the per-seat access vendor ([seataccess.Vendor]) treats the
// token as the seat's account and its value as the seat's credential. The
// engine mints it through the company's provisioner token, narrows it when
// the seat's level changes, rolls its value and renews its 90-day expiry
// before it lapses, and disables then deletes it when the seat is turned off.
//
// # Only what the ledger records is the engine's
//
// Token names are not unique at Cloudflare and anyone holding token rights can
// create one under any name, so a name is never proof of provenance. The
// engine acts only on token IDs its ledger recorded when it created them, and
// adopts nothing by name.
//
// # Permission groups by name, resolved live
//
// A level is a list of Cloudflare's permission group names ([Groups]),
// resolved to IDs from the account's own catalogue on every pass, because
// Cloudflare treats the name as cosmetic and the ID as the contract.
//
// # The atlas reads more where the token may, and asks for nothing
//
// The atlas reads the account's names and certificates under the Read groups
// the provisioner token already holds (ADR-0129, atlas.go). Four more read
// groups (Workers Scripts, Pages, Workers R2 Storage, Cloudflare Tunnel) show
// it those products by name (ADR-0145, atlasoptional.go). They are OPTIONAL:
// no level grants one, so no agent's token carries it, the connect check and
// the pass say a missing one as a suggestion on a token that works, and the
// read leaves its part unread without failing.
package cloudflare

// Backend is the integration's name: the config block's key, the
// integration kind and the seat access lane.
const Backend = "cloudflare"

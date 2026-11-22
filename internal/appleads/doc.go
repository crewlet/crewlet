// Package appleads is the Apple Ads integration: every operation of the Apple
// Ads Platform API, reached by the engine for the agents as the company's API
// users (ADR-0170). The Campaign Management API v5 is not called: it shuts
// down on 2027-01-26.
//
// # Keys and tokens
//
// A connection is one API user: its client, team and key IDs and the P-256
// key whose public half the person uploaded, sealed. The engine signs a client
// secret with it (ES256, ten minutes from a minute back), exchanges it for an
// access token and keeps the token in memory until a minute before it ends
// ([Client]). No token, secret or key reaches a log, an error, a record or an
// answer.
//
// # Roles and scope
//
// Apple gives a user a role per ad account, which the engine reads live from
// `GET /v1/acls` and refuses an operation the role cannot make, naming the
// roles that can. A seat names its ad accounts; every call carries the one it
// acts on in `X-AP-Context`, and the engine checks the ad account each answer
// names itself, since Apple does not document refusing another account's
// object. Budget orders and new ad accounts are the organization's, sent with
// no header, and a budget order must name the seat's ad accounts alone.
//
// # Classes and the spend guard
//
// An update is classed by the fields it changes against its target as read
// just before: a pause, a lower and a rename are operate, anything else spend.
// Spend passes internal/spendguard at the ask and again at the apply, under
// the ad account's lease: capacity is the daily budgets of its enabled,
// unended campaigns.
//
// # Rate budget
//
// Apple says each answer's window, its remaining calls and when it resets.
// Agents' reads stop at a tenth of the window, which is kept for approved
// acts and the pass. A read waits out a 429 of at most thirty seconds; a write
// is never sent twice.
package appleads

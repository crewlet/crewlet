// Package ticket mints and opens view tickets: the credential a sandboxed
// artifact frame carries in its URL path, worth one artifact version for one
// principal for one hour.
//
// # Minted on one origin, opened on another
//
// The dashboard's API mints a ticket for the signed-in principal, and the
// artifact origin opens it, often on a different node. So a ticket is
// self-describing and signed rather than a key into a table: it is an
// internal/runtoken token in the domain [Domain], whose subject is the
// canonical JSON of [artifacts.TicketClaims] in unpadded base64url (a runtoken
// subject may not contain a dot, and base64url has none). The token names the
// keyring entry that signed it, so a keyring rotation never invalidates a
// frame minted under a key the node still holds.
//
// # Three answers, and the third is the one a viewer recovers from
//
// [Codec.Open] answers [Valid], [Expired] or [Invalid]. The runtoken lives
// [artifacts.TicketExpiredGrace] past the claims' own expiry, so a ticket that
// simply ran out is still recognised as one this fleet minted and is answered
// "expired" (the origin's empty 401, which tells the host to mint another),
// while anything forged, truncated, signed under a dropped key or long dead is
// "invalid" (the origin's empty 404, which says nothing). Telling a holder
// which of those a bad ticket was would tell an attacker the same.
//
// # Nothing here is secret but the signature
//
// The claims travel in the URL, so they name nothing a reader of a proxy log
// could use on its own: an artifact id, a version, a seat handle, the name of
// the credential the ticket was minted through, and an expiry. What makes a
// ticket worth anything is the signature, and the artifact origin re-decides
// every document request against the live rows anyway: a ticket names who may
// ask, never what they may see.
//
// Imported by the API that mints and the serving handler that opens, and by
// nothing that should also see the handler.
package ticket

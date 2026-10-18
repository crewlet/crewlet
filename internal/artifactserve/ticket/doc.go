// Package ticket mints and opens view tickets: the credential a sandboxed
// artifact frame carries in its URL path, worth one artifact version for one
// principal for one hour.
//
// Minted on the dashboard origin for the signed-in principal and opened on the
// artifact origin, often on a different node, so it is self-describing and
// signed under the fleet keyring with the key it was signed under named in the
// token: a rotation never invalidates a frame minted under a key the node still
// holds. Imported by the API that mints and the handler that opens, and by
// nothing that should also see the handler.
package ticket

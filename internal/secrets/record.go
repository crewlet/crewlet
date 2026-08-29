package secrets

import (
	"errors"
	"time"
)

// Record is one stored secret's row, in whichever store holds it.
//
// ONE SHAPE FOR BOTH STORES, and that is the point rather than a convenience.
// A secret lives on the coordination KV, where every node reads it, and — for
// a node that has not finished migrating off it — in that node's own database.
// Two record types would have made the CLI, the API and the provisioning
// sinks each pick a side, and a caller that could only speak one of them
// would be a caller that only works on one of the two stores.
type Record struct {
	Name string

	// Value is the SEALED envelope, never plaintext. It is present on the
	// path between a store and the keyring that opens it, and blank
	// everywhere else — every listing deliberately drops it, because a
	// listing is printed and the one thing that must never reach a
	// terminal is the ciphertext.
	Value string

	// KeyID is the keyring key this row is sealed under, denormalised out
	// of the envelope so a rotation sweep can find stale rows without
	// decrypting any of them.
	KeyID string

	UpdatedAt time.Time

	// UpdatedBy and Source are provenance: who wrote it and through what —
	// an operator at the CLI, a provisioning run that minted a token.
	// Answering "where did this credential come from" months later is the
	// whole reason they are fields rather than a log line.
	UpdatedBy string
	Source    string
}

// ErrNoKeyring reports a secret store asked to work without one.
//
// Every store refuses rather than falling back to plaintext: an operator who
// configured no encryption gets the environment, not a store that quietly
// holds credentials in the clear.
var ErrNoKeyring = errors.New(
	"secrets: this needs a keyring; set bootstrap secrets.keys")

// ErrNotFound reports a name with no row.
//
// Its own sentinel because the caller's answer differs: a missing secret is
// an operator's unset variable, while a decrypt failure is a keyring that no
// longer opens what it wrote. Collapsing them would have a rotation that
// dropped a key look exactly like a name nobody set.
var ErrNotFound = errors.New("secrets: no such secret")

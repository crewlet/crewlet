package secrets

import (
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/envref"
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

	// UpdatedBy, UpdatedByKind, OperatorID and Source are provenance: who
	// wrote it, what sort of author that is, the credential they acted
	// through, and the surface it arrived by — an operator at the CLI, a
	// provisioning run that minted a token. Answering "where did this
	// credential come from" months later is the whole reason they are
	// fields rather than a log line. See [Author] for why the first three
	// are three.
	UpdatedBy     string
	UpdatedByKind string
	OperatorID    string
	Source        string

	// Version is the store's own revision of the row, where the store has
	// one: what a write or a delete conditioned on "nothing has changed
	// since I read it" names. Zero where the store keeps none.
	Version uint64
}

// Author is who writes a secret: the three facts every other trail in the
// engine records beside a change — the NAME the change is attributed to, what
// KIND of author that is, and the CREDENTIAL the write was made through.
//
// # Why two names and not one
//
// The store used to record one, and it was the credential: right for a Tier A
// token, whose name is its credential, and a loss for everybody else. A person
// writing through their own machine token was recorded as `pat:<id>`, and the
// token's row — the one thing that says whose it was — is swept a week after
// it expires, so the credential's name outlived the only record of its owner.
// The author is whose authority was exercised; the credential is what it was
// exercised through; a trail that can answer only one of "who" and "how"
// cannot answer an investigation, which asks both.
//
// # Why it restates iam.Actor rather than importing it
//
// This package imports nothing from the rest of the engine, which is what lets
// config depend on it. So the shape is restated as strings, and the surface
// that holds a principal fills it from internal/iam's ActorFor — the one
// function every trail's author comes from.
type Author struct {
	// Name is the author: a seat's handle for a person bound to one, a
	// login otherwise, `token:<id>` for a Tier A token, the node for the
	// engine's own writes.
	Name string

	// Kind is internal/iam's actor kind — agent, human, operator, system.
	Kind string

	// OperatorID is the credential the write was made through: a machine
	// token's `pat:<id>`, a browser session's `session:<lineage>`, a Tier
	// A token's login. EMPTY for a write no credential made — the engine's
	// own, and a command run on the host while the engine was stopped.
	OperatorID string
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

// ErrInvalidName reports a name no ${VAR} reference could ever reach.
//
// Its own sentinel because it is the caller's mistake rather than the
// store's: an HTTP surface answers it as a 400 naming the field, where every
// other failure here is a 500 or a 404.
var ErrInvalidName = errors.New(
	"secrets: a secret's name is an environment-variable name")

// CheckName refuses a name the reference grammar cannot name.
//
// EVERY STORE CALLS THIS, and it is a guard rather than a courtesy. The store
// is keyed by environment-variable name — that is what a ${VAR} in the
// company document resolves through, and what the conventional-key fallbacks
// look up — so a row stored under "gitlab-token" or "my token" is sealed,
// listed, reported as written and then resolved by nothing at all. The
// operator's evidence is a provider failing to authenticate hours later, with
// nothing pointing back at the name they chose.
func CheckName(name string) error {
	if envref.ValidName(name) {
		return nil
	}
	if name == "" {
		return fmt.Errorf("%w: the name is empty", ErrInvalidName)
	}
	return fmt.Errorf(
		"%w: %q is not one, so no ${VAR} in the company config can reach it; "+
			"use letters, digits and underscores, starting with a letter or "+
			"an underscore", ErrInvalidName, name)
}

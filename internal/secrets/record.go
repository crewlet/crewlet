package secrets

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/words"
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
	// through, and the write path it arrived by — the API, an integration's
	// setup, a provisioning pass that minted a token. Answering "where did
	// this credential come from" months later is the whole reason they are
	// fields rather than a log line. See [Author] for why the first three
	// are three, and [Source] for why the fourth is the path's own word.
	UpdatedBy     string
	UpdatedByKind string
	OperatorID    string
	Source        Source

	// Version is the store's own revision of the row, where the store has
	// one: what a write or a delete conditioned on "nothing has changed
	// since I read it" names. Zero where the store keeps none.
	Version uint64
}

// Removal is a secret an operator removed while this node's engine was
// stopped: the name, when, by whom and through which path — a DATED REMOVAL,
// which the engine carries onto the fleet at its next start exactly as it
// carries a value written then.
//
// # Why a removal is a row and not just a missing one
//
// A stopped node cannot reach the fleet's store (on the default topology its
// broker is inside the engine's process), so `crewlet secrets unset` there can
// only change this node's own table — and deleting a row the fleet's value
// never came from changes nothing the fleet reads. The removal has to be
// RECORDED, with the instant it was made, so that the next start can remove
// the fleet's value when the removal is the later write and keep it when it
// is not: the same later-wins rule an offline write is carried by. A table of
// values alone can say what was set, never what was taken away.
type Removal struct {
	Name string

	// RemovedAt is when the operator removed it, on the writing node's own
	// clock, which is what the fleet's write is compared against.
	RemovedAt time.Time

	// RemovedBy, RemovedByKind, OperatorID and Source are the provenance a
	// value's row carries ([Record]), for the removal.
	RemovedBy     string
	RemovedByKind string
	OperatorID    string
	Source        Source
}

// Source is the write path a stored secret arrived by, stamped by that path.
//
// # A fact of the write path, never a claim of the request
//
// It was a word the caller chose: `PUT /secrets/{name}?source=` stored
// whatever it was sent and `crewlet secrets set -source` sent whatever it was
// given, so any holder of the write could stamp any word — and a client that
// judged which rows were its own by the word read a row somebody else wrote
// over, under that client's word, as still its own. Provenance the writer
// picks answers nothing. Who wrote a row is what the author and the
// credential say, both taken from the credential the write authenticated with
// ([Author]); the source says only which path carried it, and each path
// stamps its own constant below.
//
// # Closed, and refused at the write
//
// The set is the paths, so it is closed, and [CheckSource] refuses any other
// word at every store's write. The type alone cannot: an untyped string
// constant converts to a Source wherever one is expected, so
// `"gitlab-provision"` compiled as one and, unchecked, was stored as one.
type Source string

const (
	// SourceAPI is a value stored through a running node's
	// `PUT /secrets/{name}`, whoever called it: the dashboard, and `crewlet
	// secrets set`, a `crewlet <vendor> provision` run and `crewlet llm
	// login` or `export` against a running node, as much as any other
	// client. The route stamps it and takes no word from the request.
	SourceAPI Source = "api"

	// SourceSetup is a value the dashboard's integration setup stored: a
	// submitted form, or the GitHub App a person created from it.
	SourceSetup Source = "setup"

	// SourceProvision is a credential an integration's provisioning pass
	// minted: the engine's own pass, or `crewlet <vendor> provision
	// -secret-store` against a stopped node's own table.
	SourceProvision Source = "provision"

	// SourceMigrated is a row the engine moved onto the fleet from a node's
	// own table at boot. It replaces the word the stopped node stamped, so
	// [SourceCLI] and [SourceLLMLogin] are never on the fleet.
	SourceMigrated Source = "migrated"

	// SourceCLI is `crewlet secrets set` against a stopped node's own table.
	SourceCLI Source = "cli"

	// SourceLLMLogin is a coding CLI's login — `crewlet llm login`'s headless
	// token, or `crewlet llm export -secret-store`'s bundle — stored against
	// a stopped node's own table.
	SourceLLMLogin Source = "llm-login"

	// SourceIAM is the engine's own key material, minted by the engine
	// ([Reserved] names, which no listing shows).
	SourceIAM Source = "iam"
)

// Sources are every write path's word.
var Sources = []Source{SourceAPI, SourceSetup, SourceProvision, SourceMigrated,
	SourceCLI, SourceLLMLogin, SourceIAM}

// Valid reports whether a source off the wire is one this build stamps.
func (s Source) Valid() bool { return slices.Contains(Sources, s) }

// CheckSource refuses a source no write path stamps.
//
// EVERY STORE CALLS IT AT THE WRITE, for the reason [Source] gives: the type
// alone does not close the set, and a row stored under a word no path stamps
// is a row every reader of provenance has to guess at.
func CheckSource(s Source) error {
	if s.Valid() {
		return nil
	}
	return fmt.Errorf("secrets: %q is not a write path's source; a row "+
		"records one of %s, stamped by the path that wrote it", string(s), words.Or(Sources))
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
	// Name is the author: the seat's handle for a person (every person
	// holds one) or a credential bound to a seat, the login of a credential
	// bound to none — `token:<id>` for a Tier A token — and the node for the
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
// Every store refuses rather than falling back to plaintext, a store that
// quietly holds credentials in the clear. No node reaches it: every node's
// Tier A carries a keyring, so meeting it is a caller built without one.
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

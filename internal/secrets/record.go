package secrets

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/words"
)

// Record is one stored secret's row, whichever way it was reached.
//
// ONE SHAPE FOR EVERY WAY IN, and that is the point rather than a convenience.
// A secret lives on the coordination KV, where every node reads it, and a
// caller reaches it either through a running node's API or directly — the
// engine, and a `crewlet` command with its node's engine stopped. One record
// type is what lets the CLI, the API and the provisioning sinks speak to the
// store without picking a side.
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

	// WriteOnly is a value only the engine's resolver opens ([ErrWriteOnly]).
	WriteOnly bool
}

// Source is the write path a stored secret arrived by, stamped by that path.
//
// Never a word the caller picks: a client judging its own rows by a
// writer-chosen word would take a row somebody overwrote as still its own. Who
// wrote a row is [Author]; the source says only which path carried it, each
// path stamping its own constant. The set is closed and [CheckSource] refuses
// any other word at every store's write; the type alone cannot, as an untyped
// string constant converts to a Source.
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
	// -secret-store` written to the fleet's store directly, with the node's
	// engine stopped.
	SourceProvision Source = "provision"

	// SourceCLI is `crewlet secrets set` written to the fleet's store
	// directly, with the node's engine stopped. Through a running node the
	// same command's row is [SourceAPI].
	SourceCLI Source = "cli"

	// SourceLLMLogin is a harness's login — `crewlet llm login`'s headless
	// token, or `crewlet llm export -secret-store`'s bundle — written to the
	// fleet's store directly, with the node's engine stopped.
	SourceLLMLogin Source = "llm-login"

	// SourceModels is a model's credential written by the engine for the
	// Models page: the login a device sign-in made, exported as the entry's
	// bundle once the person approved it.
	SourceModels Source = "models"

	// SourceIAM is the engine's own key material, minted by the engine
	// ([Reserved] names, which no listing shows).
	SourceIAM Source = "iam"

	// SourceMCP is an MCP server's header or variable stored from its page
	// in Settings › MCP & tools (`PUT /mcp-servers/{name}/values/{key}`),
	// sealed write-only ([Record.WriteOnly]).
	SourceMCP Source = "mcp"

	// SourceMCPSignIn is an MCP server's OAuth grant, written by the engine
	// when a person signs the company in at the server's vendor and each
	// time the engine renews it ([Reserved] names, which no listing shows).
	SourceMCPSignIn Source = "mcp-sign-in"
)

// Sources are every write path's word.
var Sources = []Source{SourceAPI, SourceSetup, SourceProvision, SourceCLI,
	SourceLLMLogin, SourceIAM, SourceModels, SourceMCP, SourceMCPSignIn}

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

// Author is who writes a secret: the NAME the change is attributed to, its
// KIND, and the CREDENTIAL the write was made through.
//
// Two names because a credential alone loses the owner: a person's machine
// token would record `pat:<id>`, and that row is swept a week after it
// expires. An investigation asks both "who" and "how".
//
// It restates iam.Actor as strings because this package imports nothing from
// the engine, which is what lets config depend on it; the surface that holds a
// principal fills it from internal/iam's ActorFor.
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

// ErrWriteOnly reports a read of a value stored write-only (an MCP server's
// header stored from its page): the engine resolves it, and nothing reveals it.
var ErrWriteOnly = errors.New("secrets: the value is write-only: the engine reads it for the " +
	"${VAR} references that name it, and no reveal answers it")

// ErrInvalidName reports a name no ${VAR} reference could ever reach.
//
// Its own sentinel because it is the caller's mistake rather than the
// store's: an HTTP surface answers it as a 400 naming the field, where every
// other failure here is a 500 or a 404.
var ErrInvalidName = errors.New(
	"secrets: a secret's name is an environment-variable name")

// MaxValueBytes bounds one secret's value, in bytes, in every store and on
// every path that writes one.
//
// A service-account JSON or PEM key is a few kilobytes; 64 KiB is an order of
// magnitude above, bounding a mistake (a `curl -d @big.tar` typo), not an
// attacker.
//
// ONE LIMIT, enforced in every store (including `crewlet secrets set` with the
// engine stopped, which writes the fleet's store directly), because every
// reader of a revealed value through a node is sized from it, in the
// characters JSON escapes longest. A larger value would be stored yet
// unreadable through the node.
const MaxValueBytes = 64 << 10

// ErrValueTooLarge reports a value longer than [MaxValueBytes].
//
// Its own sentinel because it is the caller's mistake rather than the store's,
// as [ErrInvalidName] is: what fixes it is different input, never a retry or
// a repair of the store.
var ErrValueTooLarge = errors.New("secrets: a secret's value is longer than any store keeps")

// CheckValue refuses a value longer than [MaxValueBytes], naming its length
// and the limit and never the value.
//
// EVERY STORE CALLS IT AT THE WRITE, as it calls [CheckName] and
// [CheckSource], because the limit is what every reader through a node is
// sized from: a store that took more could hold a value a reveal cannot give
// back.
func CheckValue(value string) error {
	if len(value) <= MaxValueBytes {
		return nil
	}
	return fmt.Errorf("%w: it is %d bytes, and a value is at most %d (%d KiB); "+
		"a credential is a few kilobytes, so this is most likely the wrong input",
		ErrValueTooLarge, len(value), MaxValueBytes, MaxValueBytes>>10)
}

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

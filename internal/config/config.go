// Package config is the two-tier configuration a Crewlet company runs on.
//
// # The two tiers
//
// [Bootstrap] (Tier A) is ops-owned: it lives on disk beside the binary and
// changing it means restarting the process. It carries only what the engine
// needs to bring its own infrastructure up — the store file, the stream and
// coordination slots, the API socket and its auth posture, the secret
// keyring, and this node's identity. Nothing in it describes the company.
//
// [Company] (Tier B) is founder-owned: it lives in the store as a versioned
// revision and is edited live. It carries what the company IS — identity,
// seats, units, model providers, tool servers, integrations, budgets, and
// the turn-engine knobs. A revision is applied to a running engine; no
// restart is involved.
//
// The split is not cosmetic. Tier A is the root of trust: it holds the
// address of the secret store and the keys that decrypt it, so it can never
// read a value out of that store (see [Resolver] and the use of
// [EnvOnly] on the Tier A load path). Tier B is the opposite — its secrets
// are ${VAR} POINTERS, stored verbatim, resolved only at the moment a
// provider or transport is constructed, so a revision exported from the
// store or shown in the dashboard carries no credential.
//
// # Three rules this package exists to hold
//
// **An unknown key is an error.** Every field here has a default, so a
// typo'd key does not fail — it silently does nothing, and the company runs
// without the setting its operator believes they set. `backstroy:` spawns a
// seat with an empty backstory and no diagnostic anywhere. Decoding runs
// with yaml.Decoder.KnownFields(true) and the few custom decoders re-enter
// it through [decodeKnown], so there is no shape a typo can hide in.
//
// **Parsing never resolves.** `crewlet validate` has to check a config as
// REFERENCES, on a laptop, before any secret exists — so a ${VAR} survives
// parsing untouched and resolution is a separate pass ([Resolver]). It is
// also what keeps a stored revision free of resolved secrets.
//
// **A validation error names the field path.** `providers.llm.default.model`
// is an operator's first debugging tool; "invalid model" is not. Validators
// report EVERY failure they find rather than the first, because a config
// author fixing one field at a time through a validate-edit loop pays a
// round trip per mistake, and the mistakes here are usually made together.
//
// # What Go changes
//
// The Python engine expressed all of this through Pydantic: field types
// carried constraints, model validators raised, and `extra="forbid"` did the
// unknown-key work. None of that machinery is reproduced. These are plain
// structs with yaml/json tags; validation is ordinary Go code returning
// ordinary errors joined with errors.Join; and the fields that accept a
// scalar OR a list get a named type with its own decoder rather than an
// `any` every reader has to type-switch on.
package config

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// The sentinels a caller branches on. Deliberately few: nothing decides
// differently between a bad cron field count and an out-of-range timeout,
// and a sentinel per message would make the set unreadable without making
// anything decidable. Every error a validator reports wraps one of these
// and names the field path that produced it.
var (
	// ErrMissing reports a required value that is absent or empty.
	ErrMissing = errors.New("required value missing")

	// ErrUnknownValue reports a value outside a closed set — a provider
	// type, a containment mode, a node role. Closed sets are rejected
	// rather than defaulted: the engine dispatches on them, and an
	// unrecognised value used to fall through every branch and silently
	// produce nothing at all.
	ErrUnknownValue = errors.New("value not in the allowed set")

	// ErrOutOfRange reports a number outside the bounds its consumer can
	// honour.
	ErrOutOfRange = errors.New("value out of range")

	// ErrConflict reports two settings that cannot both be true — a block
	// that only applies to another type, two mutually exclusive
	// integrations, a knowledge scope for a disabled backend.
	ErrConflict = errors.New("conflicting settings")

	// ErrUnknownField reports a key the schema does not define. It is
	// surfaced as its own sentinel because it is the one class of failure
	// that is ALWAYS a typo, and the one an editor can be pointed at.
	ErrUnknownField = errors.New("unknown field")

	// ErrShape reports a value whose YAML shape is wrong — a mapping where
	// a list belongs, a scalar where a block belongs.
	ErrShape = errors.New("wrong shape")
)

// fault builds one validation error: the field path an operator can search
// their file for, the sentinel a caller branches on, and the detail that
// says what to do about it.
//
// The path comes first because that is what a reader scans for. It is the
// authored path (providers.llm.default.model), never a Go field name the
// operator has never seen.
func fault(path string, kind error, detail string, args ...any) error {
	if len(args) > 0 {
		detail = fmt.Sprintf(detail, args...)
	}
	if path == "" {
		return fmt.Errorf("%w: %s", kind, detail)
	}
	return fmt.Errorf("%s: %w: %s", path, kind, detail)
}

// at joins a parent path with a child field. The empty parent is the
// document root, where a path is just the field name.
func at(parent, field string) string {
	if parent == "" {
		return field
	}
	return parent + "." + field
}

// idx renders a list element's path as the operator sees it in their file:
// the index they can count to, not an opaque identity.
func idx(parent string, i int) string {
	return fmt.Sprintf("%s[%d]", parent, i)
}

// problems accumulates validation failures so a Validate reports all of
// them at once. The zero value is ready to use.
type problems []error

// add records one failure.
func (p *problems) add(path string, kind error, detail string, args ...any) {
	*p = append(*p, fault(path, kind, detail, args...))
}

// wrap records an error produced further down, already carrying its own
// path. A nil error records nothing, so callers need no conditional.
func (p *problems) wrap(err error) {
	if err != nil {
		*p = append(*p, err)
	}
}

// err returns the joined failures, or nil when there were none.
func (p problems) err() error { return errors.Join(p...) }

// oneOf reports whether value is in the allowed set, and renders the set
// for the error message. Closed sets are declared as package-level slices
// so the validator and the JSON Schema generator read the SAME list — a
// schema that accepts what the validator rejects is worse than no schema,
// because an editor then blesses a config that will not boot.
func oneOf[T ~string](value T, allowed []T) bool {
	for _, a := range allowed {
		if value == a {
			return true
		}
	}
	return false
}

// names renders a closed set for an error message or a schema enum.
func names[T ~string](allowed []T) string {
	out := make([]string, len(allowed))
	for i, a := range allowed {
		out[i] = string(a)
	}
	return strings.Join(out, ", ")
}

// strs converts a closed set to plain strings, for the schema generator.
func strs[T ~string](allowed []T) []string {
	out := make([]string, len(allowed))
	for i, a := range allowed {
		out[i] = string(a)
	}
	return out
}

// sortedKeys gives a map a stable iteration order.
//
// Every error a validator reports over a map goes through here. An error
// that names a different one of two offending keys on each run is one
// nobody can write a test against, and one an operator reasonably reads as
// two different problems.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

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
// # No validation framework
//
// A schema library would carry the constraints on the field types, raise from
// model validators, and do the unknown-key work itself. None of that is used
// here. These are plain structs with yaml/json tags; validation is ordinary Go
// code returning ordinary errors joined with errors.Join; and the fields that
// accept a
// scalar OR a list get a named type with its own decoder rather than an
// `any` every reader has to type-switch on.
package config

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
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
func fault(path Path, kind error, detail string, args ...any) error {
	if len(args) > 0 {
		detail = fmt.Sprintf(detail, args...)
	}
	return &Fault{Path: path, Kind: kind, Detail: detail}
}

// Fault is one validation failure, with its parts still separate.
//
// # Why a type rather than a formatted string
//
// Because two consumers need the parts back. `crewlet validate -json` emits
// {path, type, message} so an authoring loop can jump to the field, and the
// API's write surface reports the same three to a caller that never sees a
// terminal. Both were re-deriving them by splitting the rendered message on
// colons, which a detail containing a colon breaks — and details routinely
// contain one, because they name settings.
//
// It renders EXACTLY as the formatted string did, because that text is what
// an operator reads at a prompt and reflowing it would churn every test that
// asserts on a message.
type Fault struct {
	// Path is the AUTHORED path (providers.llm.default.model), never a Go
	// field name the operator has never seen. Empty at the document root.
	Path Path
	// Kind is one of the ErrMissing / ErrShape / … sentinels, kept as the
	// error itself so errors.Is still answers.
	Kind error
	// Detail says what to do about it.
	Detail string
}

func (f *Fault) Error() string {
	if len(f.Path) == 0 {
		return f.Kind.Error() + ": " + f.Detail
	}
	return f.Path.String() + ": " + f.Kind.Error() + ": " + f.Detail
}

// Unwrap exposes the sentinel, so errors.Is(err, ErrMissing) keeps working.
func (f *Fault) Unwrap() error { return f.Kind }

// Faults flattens a validation error into its parts, in the order they were
// reported.
//
// An error that is NOT a fault — a file that could not be read, a YAML
// document that would not parse — comes back as one Fault with an empty path
// and [ErrShape], because a caller rendering machine-readable output needs
// every failure in one shape or it has to grow a second branch for the ones
// that arrive differently.
func Faults(err error) []Fault {
	if err == nil {
		return nil
	}
	var out []Fault
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		// JOINED ERRORS FIRST, because a *Fault never wraps a join and a
		// join is how every validator reports more than one problem.
		if joined, ok := e.(interface{ Unwrap() []error }); ok {
			for _, inner := range joined.Unwrap() {
				walk(inner)
			}
			return
		}
		var f *Fault
		if errors.As(e, &f) {
			out = append(out, *f)
			return
		}
		out = append(out, Fault{Kind: ErrShape, Detail: e.Error()})
	}
	walk(err)
	return out
}

// KindName is the machine-readable name of a fault's kind.
//
// A CLOSED SET with a fallback, because the name travels in JSON that a fix
// loop branches on: a kind this build does not know renders as "invalid"
// rather than as an empty string, which would look like a field the consumer
// forgot to read.
func (f Fault) KindName() string {
	switch {
	case errors.Is(f.Kind, ErrMissing):
		return "missing"
	case errors.Is(f.Kind, ErrOutOfRange):
		return "out_of_range"
	case errors.Is(f.Kind, ErrConflict):
		return "conflict"
	case errors.Is(f.Kind, ErrShape):
		return "shape"
	case errors.Is(f.Kind, ErrUnknownField):
		return "unknown_field"
	case errors.Is(f.Kind, ErrUnknownValue):
		return "unknown_value"
	default:
		return "invalid"
	}
}

// Path is a place in an authored document, held as its SEGMENTS: a string
// for a field or a map key, an int for a list index.
//
// # Why segments rather than the rendered string
//
// Because the string cannot be taken apart again. A path renders as dotted
// keys with `[i]` indexes (roles[0].mcp_env.jira.API_TOKEN), and a map key is
// the operator's own text, which may itself hold a dot or a bracket: a
// provider called `claude-3.5` renders as providers.llm.claude-3.5.model, and
// nothing reading that string can tell where the key ends. A consumer that
// has to find the field (an editor pointing at a line, a form marking the
// input a problem is about) needs the segments, so they are what every
// validator builds and the string is only ever rendered from them.
//
// It is a value: every helper that extends a path returns a new one and never
// writes into the parent's backing array, because sibling fields are built
// from one parent and a shared array would make each overwrite the other.
type Path []any

// String renders the path as an operator reads it in their file: fields and
// map keys joined with dots, list indexes as `[i]`. The empty path renders as
// the empty string, which is the document root.
func (p Path) String() string {
	var b strings.Builder
	for _, segment := range p {
		switch s := segment.(type) {
		case int:
			b.WriteString("[" + strconv.Itoa(s) + "]")
		case string:
			if b.Len() > 0 {
				b.WriteByte('.')
			}
			b.WriteString(s)
		}
	}
	return b.String()
}

// extend returns parent with segments appended, in a new backing array.
func extend(parent Path, segments ...any) Path {
	out := make(Path, 0, len(parent)+len(segments))
	out = append(out, parent...)
	return append(out, segments...)
}

// at extends a path with FIELD NAMES, written dotted when there are several
// (at(path, "integrations.github")). Only ever a schema's own field names,
// which never contain a dot; a map key goes through [entry], because it is the
// operator's text and a dot in it is part of the key.
func at(parent Path, fields string) Path {
	names := strings.Split(fields, ".")
	segments := make([]any, len(names))
	for i, name := range names {
		segments[i] = name
	}
	return extend(parent, segments...)
}

// field is a path from the document root, for the rules that name a fixed
// place (field("integrations.datadog.route_to")).
func field(fields string) Path { return at(nil, fields) }

// entry extends a path with one MAP KEY, verbatim: a provider name, an MCP
// server, a variable. See [at] for why it is not split.
func entry(parent Path, k string) Path { return extend(parent, k) }

// idx extends a path with a list index, the position the operator can count
// to in their file rather than an opaque identity.
func idx(parent Path, i int) Path { return extend(parent, i) }

// problems accumulates validation failures so a Validate reports all of
// them at once. The zero value is ready to use.
type problems []error

// add records one failure.
func (p *problems) add(path Path, kind error, detail string, args ...any) {
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

// names renders a closed set for an error message or a schema enum.
//
// EVERY CLOSED SET IN THIS PACKAGE IS A PACKAGE-LEVEL SLICE, and this is why:
// the validator (through [slices.Contains]) and the JSON Schema generator
// (through here) read the SAME list. A schema that accepts what the validator
// rejects is worse than no schema, because an editor then blesses a config
// that will not boot.
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
	return slices.Sorted(maps.Keys(m))
}

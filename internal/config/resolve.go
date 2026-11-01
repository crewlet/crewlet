package config

import (
	"fmt"
	"log/slog"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/logging"
)

var resolveLog = logging.Get("config.resolve")

// Source is one place a ${VAR} reference can be answered from.
//
// Consumer-defined and one method wide: the secret store, the process
// environment and a test map are all this, and nothing that resolves a
// reference needs to know which it got.
//
// ok distinguishes "this source has nothing to say" from "this source holds
// an empty value". A stored empty value is AUTHORITATIVE and stops the
// chain — an operator who deliberately stored an empty credential has said
// something, and falling through to a stale environment export would undo
// it.
type Source interface {
	Lookup(name string) (value string, ok bool)
}

// namedSource is a [Source] that says what it is, for the shadow warning to
// name: a source the engine builds for one caller — a seat's issued token —
// is neither the secret store nor the environment, and a warning that called
// it either would send the operator to remove the wrong value.
type namedSource interface {
	SourceName() string
}

// sourceName is how the shadow warning names src: its own name where it has
// one, the environment for [EnvSource], and otherwise the secret store, the
// one other source a chain is built over ([WithStore]).
func sourceName(src Source) string {
	switch s := src.(type) {
	case namedSource:
		return s.SourceName()
	case EnvSource:
		return "the environment"
	}
	return "the secret store"
}

// EnvSource reads the process environment. It is the last link of every
// chain: the fallback that made ${VAR} work before there was a store, and
// the only source Tier A is allowed to consult.
type EnvSource struct{}

// Lookup implements [Source] over os.LookupEnv.
func (EnvSource) Lookup(name string) (string, bool) { return os.LookupEnv(name) }

// MapSource is a fixed set of values — a boot snapshot of the secret store,
// or a test's fixture. A nil map is a source that answers nothing, which is
// exactly what an unconfigured store is.
type MapSource map[string]string

// Lookup implements [Source].
func (m MapSource) Lookup(name string) (string, bool) {
	v, ok := m[name]
	return v, ok
}

// Resolver expands ${VAR} references through an ordered chain of sources.
//
// # The order is a security property, not a preference
//
// THE STORE WINS AND THE ENVIRONMENT IS THE FALLBACK. The reverse would let
// a stale .env shadow a freshly rotated secret, which surfaces days later as
// an auth error from a provider rather than at the point of rotation — and
// that failure is precisely what a secret store exists to remove. When a
// name is answered by an earlier source and a LATER one holds a different
// value, the resolver logs secret_shadowed_env once, naming the source that
// answered and the one it hid, with NAMES ONLY: a value never reaches a log
// line.
//
// A Resolver is safe for concurrent use. It is passed explicitly rather than
// installed as a process global. A global is what call sites that cannot be
// handed one require; every one of them is a parameter here.
type Resolver struct {
	sources []Source

	// told is the shadow warnings already logged, SHARED with every chain
	// [Resolver.Ahead] lays over this one: see [shadowsTold].
	told *shadowsTold

	log *slog.Logger
}

// shadowsTold is the shadow warnings a chain has logged, each a name and the
// source that answered it, so a name that is resolved on every turn logs its
// warning once rather than forever.
//
// A POINTER THE CHAINS LAID OVER ONE SHARE, because a seat's resolution is a
// chain of its own ([Resolver.Ahead]) built for every turn and every pass
// over the seat's values: a record of its own would start empty each time,
// and a name the company's chain shadows would be warned again by every one
// of them. Shared, it is warned once for as long as the chain under them
// lives — until the next apply rebuilds it, as for a chain read directly.
type shadowsTold struct {
	mu   sync.Mutex
	seen map[shadowTold]struct{}
}

// shadowTold is one warning: a name, and the source that answered it. The
// source is part of it because one name can tell two stories — a seat's
// issued token hiding a stored value, and the store hiding an export — each
// with its own remedy.
type shadowTold struct{ name, winner string }

// first reports whether story has not been told yet, and records it as told.
func (t *shadowsTold) first(story shadowTold) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, already := t.seen[story]; already {
		return false
	}
	t.seen[story] = struct{}{}
	return true
}

// NewResolver builds a chain. The FIRST source that answers wins, so the
// authoritative one goes first: NewResolver(store, EnvSource{}).
func NewResolver(sources ...Source) *Resolver {
	return &Resolver{
		sources: sources,
		told:    &shadowsTold{seen: map[shadowTold]struct{}{}},
		log:     resolveLog,
	}
}

// EnvOnly resolves from the process environment alone.
//
// This is the Tier A load path, and the flag is structural rather than a
// happy accident of boot ordering: the bootstrap file carries the store's
// address and the keyring that decrypts it — exactly what is needed to OPEN
// the store — so it can never source a value from it.
func EnvOnly() *Resolver { return NewResolver(EnvSource{}) }

// WithStore is the Tier B chain: the secret store first, the environment
// behind it.
func WithStore(store Source) *Resolver { return NewResolver(store, EnvSource{}) }

// Ahead is a chain with first answering before every source of r: r's own
// chain, with one source laid in front of it.
//
// FOR A VALUE ONE CALLER ALONE MAY READ. A seat's own credential that the
// engine issues at use rather than seals (its GitHub App's installation
// token) is answered by a source built for that seat and laid in front of
// the company's chain for that seat's resolution only, so no other seat's
// values can name it. The chain r keeps is shared, never changed.
//
// Its shadow warnings are r's ([shadowsTold]): a seat's chain is built for
// every turn, and a name r's own sources disagree on is told once for r, not
// once for every chain laid over it.
//
// A nil r is the environment alone, as everywhere else here.
func (r *Resolver) Ahead(first Source) *Resolver {
	if r == nil {
		return NewResolver(first, EnvSource{})
	}
	sources := append([]Source{first}, r.sources...)
	return &Resolver{sources: sources, told: r.told, log: r.log}
}

// LookupOK answers one variable by NAME, reporting whether any source held
// it.
//
// For the handful of callers that read a variable directly rather than
// expanding a reference — a provider's conventional-key fallback
// (OPENAI_API_KEY, ANTHROPIC_API_KEY). Without this they would answer
// "unset" for a credential the operator deliberately stored, so
// `crewlet secrets set OPENAI_API_KEY` would work through a config
// reference but not through the fallback.
func (r *Resolver) LookupOK(name string) (string, bool) {
	if r == nil {
		return os.LookupEnv(name)
	}
	for i, src := range r.sources {
		v, ok := src.Lookup(name)
		if !ok {
			continue
		}
		r.warnShadowed(name, v, i)
		return v, true
	}
	return "", false
}

// Lookup is [Resolver.LookupOK] for callers that treat unset as empty.
func (r *Resolver) Lookup(name string) string {
	v, _ := r.LookupOK(name)
	return v
}

// warnShadowed reports a name a later source also holds, with a different
// value: the source at index answered it with winner. Names only — a
// credential never reaches a log line.
//
// IT NAMES BOTH SIDES, the one that answered and the first one it hid,
// because the remedy is to remove the hidden value, and a chain is not always
// the store over the environment: a seat's issued GitHub App token answers in
// front of both ([Resolver.Ahead]), and a stale value stored or exported
// under its name is the one to remove.
func (r *Resolver) warnShadowed(name, winner string, index int) {
	if index >= len(r.sources)-1 {
		return // the last source cannot be shadowing anything
	}
	var hidden Source
	for _, src := range r.sources[index+1:] {
		if v, ok := src.Lookup(name); ok && v != winner {
			hidden = src
			break
		}
	}
	if hidden == nil {
		return
	}
	won, lost := sourceName(r.sources[index]), sourceName(hidden)
	if !r.told.first(shadowTold{name: name, winner: won}) {
		return
	}
	remedy := "Remove the stale value " + lost + " holds under this name to silence this."
	switch hidden.(type) {
	case EnvSource:
		remedy = "Remove the stale export or .env entry to silence this."
	case MapSource:
		remedy = "Remove the stale stored value (crewlet secrets unset " + name +
			") to silence this."
	}
	r.log.Warn("secret_shadowed_env",
		"name", name,
		"answered_by", won,
		"shadowed", lost,
		"detail", won+" and "+lost+" disagree on this name, and the value "+
			won+" holds wins. "+remedy)
}

// Expand substitutes every reference in value and reports the names that
// resolved to nothing.
//
// An unresolved reference expands to the empty string, matching the shell —
// and it is REPORTED rather than swallowed, because
// the empty string is not detectable downstream. "Bearer ${TOKEN}" with
// TOKEN unset resolves to "Bearer ", which is truthy-but-broken: a caller
// that only checked for emptiness would hand it straight to an API and read
// the 401 as a bad credential rather than a missing one.
func (r *Resolver) Expand(value string) (string, []string) {
	return envref.Expand(value, r.LookupOK)
}

// Value expands a scalar for a caller that has already decided what to do
// about unresolved names — usually nothing, because the empty string is the
// documented result.
func (r *Resolver) Value(value string) string {
	out, _ := r.Expand(value)
	return out
}

// Missing names every variable a value references that resolves to nothing
// NON-EMPTY, in first-seen order and each once — nil when there is none.
//
// This is the presence check a caller makes before it commits to something
// that would act with a credential — spawning a seat's MCP child, offering a
// harness a server — and it NAMES what it finds, because the one thing
// the caller does next is tell somebody which variable to set. It
// deliberately differs from [Resolver.Expand] in two ways.
//
// It tests the REFERENCES, not the resolved value: "Bearer ${TOKEN}" with
// TOKEN unset resolves to "Bearer ", which is non-empty, so a caller
// checking the result would miss exactly the composite shapes config allows.
//
// And it treats an empty answer as absent, where Expand treats it as found.
// The two questions are different: Expand asks "did a source answer", which
// is what substitution needs; this asks "is there a credential here", and an
// empty credential is not one.
func (r *Resolver) Missing(value string) []string {
	var missing []string
	for _, name := range envref.Names(value) {
		if r.Lookup(name) == "" {
			missing = append(missing, name)
		}
	}
	return missing
}

// Unresolved names one place in a config whose references resolved to
// nothing, so a caller can warn about the PATH and the NAMES rather than
// about a value it must not print.
type Unresolved struct {
	// Path is the config path that carried the reference.
	Path string
	// Names are the variables nothing answered for, in first-seen order.
	Names []string
}

// Map expands a map of values — the shape MCP credentials, sandbox env and
// setup-step env all take — reporting each key whose references went
// unresolved.
//
// path is a RENDERED path, because its callers build it for a log line and
// nothing reads its segments. It is carried as one opaque segment, so the key
// is appended by the same renderer every validation path uses.
func (r *Resolver) Map(path string, in map[string]string) (map[string]string, []Unresolved) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(in))
	var missing []Unresolved
	keys := slices.Collect(maps.Keys(in))
	// Sorted so a caller's log line and a test's expectation are stable;
	// map order would make both flap.
	slices.Sort(keys)
	for _, k := range keys {
		v, names := r.Expand(in[k])
		out[k] = v
		if len(names) > 0 {
			missing = append(missing, Unresolved{Path: entry(Path{path}, k).String(), Names: names})
		}
	}
	return out, missing
}

// Document expands every string in a parsed YAML document IN PLACE, reading
// each resolved value as the field it lands in reads one, and reports what
// went unresolved with the path of each site. into is the Go type the document
// will be decoded into, which is what decides that reading.
//
// This is the Tier A load path: the keyring, the broker URL and the API
// tokens are needed the instant the process starts, so Tier A is resolved once
// before it is decoded. Tier B is deliberately NOT run through this — its
// references are stored verbatim and resolved at the moment a provider or
// transport is built, which is what keeps an exported revision free of
// resolved secrets.
//
// # A resolved value is read as the field reads a literal
//
// Into TEXT, the resolved value is the string, character for character —
// it is data, never YAML, so a value of `null`, `~` or `8080` is that text
// and not an absence or a number. Into a NUMBER or a SWITCH it is read
// exactly as the same characters written in the file would be: `port:
// ${API_PORT}` with API_PORT=8080 is `port: 8080`, and `debug: ${DEBUG}`
// with DEBUG=true is `debug: true` — trimmed of the space around it first,
// because what it came out of so often ends in a newline that the same
// characters written in the file would not carry. It used to be text
// everywhere, so no number field could take a reference at all and a switch
// took only the YAML 1.1 words — while the documentation promised a reference
// anywhere in Tier A.
//
// Two shapes are refused there rather than guessed at, each as a fault on
// the field:
//
//   - A reference that is not the WHOLE value (`80${N}`). A number or a
//     switch is one value from one variable; composing one out of text
//     would make its type a property of how two strings concatenate.
//   - A reference that resolves to NOTHING. The empty value is how a file
//     says "unset", and a number that silently fell back to its default
//     because a variable was missing would boot on a port nobody chose. A
//     text field keeps its own reading of empty, where empty is often a
//     real setting — the log file's is guarded where it is decided (see
//     [refuseUnresolvedLogFile]).
func (r *Resolver) Document(node *yaml.Node, into reflect.Type) ([]Unresolved, error) {
	var (
		missing []Unresolved
		refused problems
	)
	eachScalar(node, into, nil, func(n *yaml.Node, t reflect.Type, path Path) {
		if n.Tag != "" && n.Tag != "!!str" {
			// Substituting into a number or a boolean the author wrote
			// could only corrupt it; a quoted reference is a string.
			return
		}
		if !envref.Has(n.Value) {
			return
		}
		literal := literalKind(t)
		if literal && !wholeReference(n.Value) {
			refused = append(refused, &Fault{Path: path, Kind: ErrShape, Line: n.Line, Detail: fmt.Sprintf(
				"%q mixes a reference with other text, and this setting is a %s: write "+
					"the whole value as one ${VAR}, whose value is read as if it were "+
					"written here", n.Value, kindNoun(t))})
			return
		}
		expanded, names := r.Expand(n.Value)
		if len(names) > 0 {
			missing = append(missing, Unresolved{Path: path.String(), Names: names})
		}
		if literal && strings.TrimSpace(expanded) == "" {
			refused = append(refused, &Fault{Path: path, Kind: ErrMissing, Line: n.Line, Detail: fmt.Sprintf(
				"%s resolved to nothing, and a %s cannot be empty: set the variable, "+
					"or write the value", n.Value, kindNoun(t))})
			return
		}
		if literal {
			// TRIMMED FIRST, as every Tier A string is (see [normalize]
			// for where the whitespace comes from): the same characters
			// written plain in the file never carry the space around them
			// — `port: 8080` ends at the 0 — while a value out of a mounted
			// file, a .env line or a captured command routinely keeps a
			// trailing newline. Kept, it made the text unwritable plain, so
			// the encoder quoted it and the field refused it as a word: a
			// port read from a file failed the boot where a host name read
			// the same way did not.
			//
			// READ AS A LITERAL: no tag, plain style, so the decoder
			// resolves the text as it resolves the same characters in a
			// file. Text that still cannot be written plain (a flow
			// indicator, a line break inside it) is quoted by the encoder
			// and reaches the field as a string, which it refuses — so a
			// value is never read as YAML syntax it merely contains.
			n.Value = strings.TrimSpace(expanded)
			n.Tag = ""
			n.Style = 0
			return
		}
		n.Value = expanded
		// TEXT STAYS TEXT, whatever it looks like: an unquoted ${PORT}
		// resolving to "8080" into a string field must stay a string, or
		// re-encoding would retag it as an integer — and one resolving to
		// "null" must not become an absence.
		n.Tag = "!!str"
		n.Style = yaml.DoubleQuotedStyle
	})
	return missing, refused.err()
}

// wholeReference reports whether value is exactly one ${VAR} and nothing
// else — not even surrounding space, which in a quoted scalar is part of the
// value and would make the resolved text something no number is.
func wholeReference(value string) bool {
	_, whole := envref.Whole(value)
	return whole && strings.TrimSpace(value) == value
}

// kindNoun names what a field holds, for a refusal an operator reads.
func kindNoun(t reflect.Type) string {
	if t != nil && t.Kind() == reflect.Bool {
		return "switch (true or false)"
	}
	return "number"
}

// LogUnresolved emits one warning per unresolved site: the path and the
// names, never the value. A caller that has nothing to report calls it
// anyway and it does nothing.
//
// This is deliberately not an error. A reference with no value is how a
// config is authored before the credential exists — `crewlet validate` on a
// laptop, a first boot before provisioning — and failing the load would
// make the check impossible. What must not happen is the failure being
// SILENT, which is what it was when the empty string was the only signal.
func LogUnresolved(what string, missing []Unresolved) {
	for _, m := range missing {
		resolveLog.Warn("config_reference_unresolved",
			"config", what,
			"path", m.Path,
			"names", strings.Join(m.Names, ","),
			"detail", "nothing answered for these variables; they resolve to "+
				"the empty string, which usually surfaces later as an "+
				"authentication failure")
	}
}

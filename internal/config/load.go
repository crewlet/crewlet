package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// LoadBootstrap reads and validates Tier A from a YAML file.
//
// ${VAR} references ARE resolved here, before decoding, because the values
// they carry — the store path, the broker URL, the API tokens, the key
// material — are needed the instant the process starts. r must be
// environment-only ([EnvOnly]); passing a chain that reaches the secret
// store would make Tier A read from the store whose address and keys it
// carries.
func LoadBootstrap(path string, r *Resolver) (*Bootstrap, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("bootstrap config %s: %w", path, err)
	}
	cfg, err := ParseBootstrap(data, r)
	if err != nil {
		return nil, fmt.Errorf("bootstrap config %s: %w", path, err)
	}
	return cfg, nil
}

// ParseBootstrap decodes, resolves and validates Tier A from bytes.
func ParseBootstrap(data []byte, r *Resolver) (*Bootstrap, error) {
	if r == nil {
		r = EnvOnly()
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrShape, err)
	}
	if empty(&doc) {
		// An empty Tier A file is legitimate: every field defaults, and a
		// company can run on nothing but the defaults.
		cfg := DefaultBootstrap()
		return &cfg, cfg.Validate()
	}
	if err := requireMapping(&doc); err != nil {
		return nil, err
	}

	LogUnresolved("bootstrap", r.Document(&doc))

	cfg := DefaultBootstrap()
	if err := decodeKnown(&doc, &cfg); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// LoadCompany reads and validates Tier B from a YAML file.
//
// Used by `crewlet config import` to populate a revision from a file on
// disk. At runtime the engine reads Tier B from the store, never from YAML.
//
// ${VAR} references are preserved VERBATIM — in the returned value and in
// the revision that gets stored. Resolution happens where a provider,
// transport or MCP server is constructed, which is what keeps a stored
// revision, a YAML export and the dashboard free of resolved secrets. It is
// also what lets `crewlet validate` check a config on a laptop where no
// credential exists.
func LoadCompany(path string) (*Company, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("company config %s: %w", path, err)
	}
	cfg, err := ParseCompany(data)
	if err != nil {
		return nil, fmt.Errorf("company config %s: %w", path, err)
	}
	return cfg, nil
}

// ParseCompany decodes and validates Tier B from bytes.
func ParseCompany(data []byte) (*Company, error) {
	cfg, err := ParseCompanyDocument(data)
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// ParseCompanyDocument decodes Tier B WITHOUT validating it.
//
// The shape is still enforced — an unknown field, a list where a mapping
// belongs, a malformed value all fail here — and only the whole-document rules
// are deferred. It exists for the one caller that cannot validate yet: the
// config write path, where a submitted document may carry redaction masks in
// place of credentials and has to have them resolved against the previous
// revision first. Validating before that would reject an operator's document
// for carrying "__redacted__" in a field they never touched.
//
// Everything else uses [ParseCompany]. A caller that skipped validation and
// forgot to run it later would be a config that fails at the first turn, and
// that is the worst place to learn it.
func ParseCompanyDocument(data []byte) (*Company, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrShape, err)
	}
	if empty(&doc) {
		return nil, fault("", ErrMissing, "the company config is empty; it needs at least a name")
	}
	if err := requireMapping(&doc); err != nil {
		return nil, err
	}

	cfg := DefaultCompany()
	if err := decodeKnown(&doc, &cfg); err != nil {
		return nil, err
	}
	// The declaration order of providers.llm exists only in the document —
	// a Go map has none — and per-phase resolution's last resort is "the
	// first provider configured". Read here, against the whole document,
	// because the strict decoder serialises a subtree alone and an alias
	// inside one pointing at an anchor defined elsewhere would stop
	// resolving. See llmKeyOrder.
	//
	// Only when the document did not state one. A document that made a
	// round trip through the config surface carries the authored order in
	// llm_order and NOT in its key order, because Go marshals a map with
	// sorted keys — so deriving unconditionally would silently reorder
	// every provider chain the moment somebody read a config and sent it
	// back.
	if len(cfg.Providers.LLMOrder) == 0 {
		cfg.Providers.LLMOrder = llmKeyOrder(&doc)
	}
	return &cfg, nil
}

// DecodeCompany reads Tier B from its STORED form.
//
// The stored form is JSON produced by marshalling a parsed [Company], not a
// document a person wrote — which makes it a different reader from
// [ParseCompany] in two ways that both matter.
//
// It carries fields the AUTHORED form does not. providers.llm_order is the
// declaration order of a Go map, recoverable only while the YAML document
// exists; it is written into the stored form precisely so a node booting from
// a revision resolves an unpinned seat to the same model the authoring node
// did. Reading the stored form through the YAML parser would reject it as an
// unknown setting.
//
// And it is LENIENT about fields it does not know, where ParseCompany fails
// closed on them. The two are answering different questions: a typo in a file
// a person wrote is a mistake to catch at the door, while an unrecognised key
// in a stored revision is a peer running a newer build — and rejecting that
// makes a mixed-version fleet an outage in the older direction. Strictness
// lives at the import, which is where a person's document arrives.
//
// ${VAR} references stay VERBATIM here as everywhere else: they are resolved
// where a provider, transport or MCP server is constructed, which is what
// makes re-activating an unchanged revision pick up a rotated credential.
func DecodeCompany(payload []byte) (*Company, error) {
	// Onto the DEFAULTS, not onto a zero value. A field the payload omits
	// must land on the same default the authored path gives it, or the
	// same company behaves differently depending on which door it came in
	// through.
	cfg := DefaultCompany()
	if err := json.Unmarshal(payload, &cfg); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrShape, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// empty reports a document with no content — an empty file, or one that is
// nothing but comments.
func empty(doc *yaml.Node) bool {
	return doc.Kind == 0 || len(doc.Content) == 0 ||
		(len(doc.Content) == 1 && doc.Content[0].Tag == "!!null")
}

// requireMapping rejects a document whose top level is a list or a scalar.
// The failure is worth its own message: yaml's own error for it names Go
// types, and a file that starts with a "- " is a recognisable mistake.
func requireMapping(doc *yaml.Node) error {
	root := doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return fault("", ErrShape, "the config file must be a YAML mapping of settings")
	}
	return nil
}

// decodeKnown decodes a node into out with unknown fields rejected.
//
// yaml.v3 only offers KnownFields on a Decoder, and a custom UnmarshalYAML
// that reaches for node.Decode gets a fresh decoder without it — which is a
// hole a typo can hide in. Every decoder in this package that has to decode
// a STRUCT out of a node re-enters through here, so there is exactly one
// strict path and no shape that escapes it.
//
// Round-tripping through the encoder is the price. It buys a guarantee that
// holds for shapes this package has not been written yet, on sub-documents
// that are a handful of keys wide.
func decodeKnown(node *yaml.Node, out any) error {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	if err := enc.Encode(node); err != nil {
		return fmt.Errorf("%w: %w", ErrShape, err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("%w: %w", ErrShape, err)
	}
	dec := yaml.NewDecoder(&buf)
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return decodeError(err, retiredFor(out))
	}
	return nil
}

// retiredFor is the retired-key table that applies to the type being
// decoded, and nil for every type that has retired nothing.
//
// A KEY IS RETIRED FROM A TIER, NOT FROM THE PACKAGE. `debug` was Tier A's,
// and decodeError is the one translation both tiers and every nested
// sub-document go through — so an ungated table answers a `debug:` in a
// COMPANY document with advice about a `logging:` block that does not exist
// there, sending its author to edit a file they are not in. An unknown key
// somewhere that never had one is an ordinary typo and has to read like one.
func retiredFor(out any) map[string]string {
	if _, isBootstrap := out.(*Bootstrap); isBootstrap {
		return retiredBootstrapFields
	}
	return nil
}

// unknownFieldRE matches yaml.v3's own phrasing for a key the struct does
// not define. The Go type name in it is meaningless to an operator, so the
// message is rewritten around the KEY, which is what they can search their
// file for.
var unknownFieldRE = regexp.MustCompile(`^line (\d+): field (\S+) not found in type \S+$`)

// retiredBootstrapFields are TIER A keys this build no longer accepts,
// mapped to what an operator should write instead.
//
// # A removed key is not a typo, and must not be reported as one
//
// The loader refuses anything it does not define, which is right — a
// misspelled setting that decoded to nothing is how a company boots with
// half its configuration silently absent. But the same refusal turns a
// key this project ITSELF told people to write into "check the spelling",
// and there is no spelling of `debug` that works any more. Every entry here
// is a name that shipped in a release, an example or the quickstart; nothing
// belongs in this table that operators were never given.
//
// Entries are permanent. A file written against any past release stays
// diagnosable, and the cost is one map entry.
var retiredBootstrapFields = map[string]string{
	"debug": "`debug` is no longer a setting — it was a second way to say " +
		"the log level and it is gone. Write `logging:` with `level: debug` " +
		"under it (and `level: info` is the default, so a `debug: false` " +
		"can simply be deleted)",
}

// decodeError translates yaml's decode failures into this package's
// sentinels, so a caller can tell a typo from a wrong shape.
func decodeError(err error, retired map[string]string) error {
	// An error from a custom unmarshaler has already been translated —
	// including by a nested decodeKnown, which is how a typo inside the
	// per-phase llm mapping or a tool-annotation block gets here. Wrapping
	// it again would bury the sentinel a caller branches on under a
	// generic one.
	for _, sentinel := range []error{ErrUnknownField, ErrShape, ErrMissing, ErrUnknownValue, ErrConflict, ErrOutOfRange} {
		if errors.Is(err, sentinel) {
			return err
		}
	}
	var typeErr *yaml.TypeError
	if !errors.As(err, &typeErr) {
		return fmt.Errorf("%w: %w", ErrShape, err)
	}
	var out problems
	for _, line := range typeErr.Errors {
		if m := unknownFieldRE.FindStringSubmatch(line); m != nil {
			// A key that was REMOVED needs its own message. "debug is not
			// a setting" is true and useless to someone reading a file the
			// quickstart told them to write: they need the line that
			// replaced it, not a spelling check.
			if replacement, gone := retired[strings.Trim(m[2], `"`)]; gone {
				out.add("line "+m[1], ErrUnknownField, "%s", replacement)
				continue
			}
			out.add("line "+m[1], ErrUnknownField,
				"%q is not a setting — check the spelling, or the block it belongs under", m[2])
			continue
		}
		out.add("", ErrShape, "%s", strings.TrimSpace(line))
	}
	return out.err()
}

// EncodeCompanyYAML renders a company config back to YAML.
//
// # Why YAML rather than the JSON the store holds
//
// Both round-trip, and the store's payload is JSON — but this output is for
// a PERSON: it is what `crewlet config export` prints, what a diff compares,
// and what an operator edits and imports back. The authored form is YAML, so
// handing back JSON would make every export a translation the reader has to
// undo before they can use it.
//
// The struct tags are shared, so a field that exports is a field the loader
// accepts: an export is importable by construction rather than by a rule
// somebody maintains.
func EncodeCompanyYAML(c *Company) ([]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("config: nothing to encode")
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	// TWO SPACES, matching every shipped example and the modeline they
	// carry. An export that indents differently from the file it came from
	// makes a `diff` against that file unreadable, which is exactly when
	// somebody reaches for one.
	enc.SetIndent(2)
	if err := enc.Encode(c); err != nil {
		return nil, fmt.Errorf("config: encode: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("config: encode: %w", err)
	}
	return buf.Bytes(), nil
}

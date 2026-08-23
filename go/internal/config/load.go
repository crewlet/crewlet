package config

import (
	"bytes"
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
		return nil, fmt.Errorf("%w: %s", ErrShape, err)
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
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrShape, err)
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
		return fmt.Errorf("%w: %s", ErrShape, err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("%w: %s", ErrShape, err)
	}
	dec := yaml.NewDecoder(&buf)
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return decodeError(err)
	}
	return nil
}

// unknownFieldRE matches yaml.v3's own phrasing for a key the struct does
// not define. The Go type name in it is meaningless to an operator, so the
// message is rewritten around the KEY, which is what they can search their
// file for.
var unknownFieldRE = regexp.MustCompile(`^line (\d+): field (\S+) not found in type \S+$`)

// decodeError translates yaml's decode failures into this package's
// sentinels, so a caller can tell a typo from a wrong shape.
func decodeError(err error) error {
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
		return fmt.Errorf("%w: %s", ErrShape, err)
	}
	var out problems
	for _, line := range typeErr.Errors {
		if m := unknownFieldRE.FindStringSubmatch(line); m != nil {
			out.add("line "+m[1], ErrUnknownField,
				"%q is not a setting — check the spelling, or the block it belongs under", m[2])
			continue
		}
		out.add("", ErrShape, "%s", strings.TrimSpace(line))
	}
	return out.err()
}

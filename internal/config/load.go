package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
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
		return nil, syntaxFault(err)
	}
	if empty(&doc) {
		// An empty Tier A file takes every default — and the keyring has
		// none, deliberately, since a default key would be one every
		// deployment shares. So it decodes, and Validate refuses it
		// naming secrets.keys, as it refuses any file without one.
		cfg := DefaultBootstrap()
		return &cfg, cfg.Validate()
	}
	if err := requireMapping(&doc); err != nil {
		return nil, err
	}

	missing, err := r.Document(&doc, reflect.TypeFor[Bootstrap]())
	LogUnresolved("bootstrap", missing)
	if err != nil {
		return nil, err
	}

	cfg := DefaultBootstrap()
	if err := decodeDocument(&doc, &cfg); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := refuseUnresolvedLogFile(missing); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// refuseUnresolvedLogFile stops a boot whose log file was named by a `${VAR}`
// nothing answered for.
//
// An unresolved reference expands to the empty string, whose meaning is the
// field's own business. Elsewhere it is caught (an empty `store.path` is a
// missing store, an empty credential an authentication failure). But empty
// `logging.file.path` is a legitimate setting meaning "write no file", so an
// unset variable there would silently start a node with no durable log. A path
// that cannot be opened already stops the boot; one that never arrived must too.
func refuseUnresolvedLogFile(missing []Unresolved) error {
	for _, u := range missing {
		if u.Path != "logging.file.path" {
			continue
		}
		return fault(field(u.Path), ErrMissing,
			"nothing answered for %s, so the log file has no name and this node "+
				"would start with no durable log at all. Set the variable, or "+
				"write the path literally: an empty path means \"no file\" and "+
				"cannot be told apart from this",
			strings.Join(u.Names, ", "))
	}
	return nil
}

// PriorRevision answers the company a file may be a redacted read of: the
// revision active where the file is about to be written or run, or nil where
// there is none. It is asked only of a file that carries a redaction mask.
//
// A FUNCTION RATHER THAN A COMPANY because answering it opens a store, which
// a file with no mask in it — the overwhelming case — has no reason to do.
type PriorRevision func() (*Company, error)

// LoadCompany reads and validates Tier B from a YAML file written into the
// store as a revision (`crewlet config import`).
//
// ${VAR} references stay VERBATIM, so revisions, exports and the dashboard
// hold no resolved secrets and `crewlet validate` works where no credential
// exists; they resolve where a provider, transport or MCP server is built.
//
// A REDACTED READ IS RESTORED FIRST against prior ([PriorRevision]; nil where
// the caller has none): `GET /config`, Copy as YAML and `crewlet config show`
// mask inline credentials, each restored from the revision it replaces
// ([Company.RestoreRedacted]). A mask the prior cannot answer stays, and
// validation names its field.
func LoadCompany(path string, prior PriorRevision) (*Company, error) {
	return loadCompanyFile(path, prior, (*Company).Validate)
}

// LoadCompanyToRun reads Tier B from a YAML file that is RUN or acted on, not
// written into the store (`crewlet run`'s `-company` and `-import-company`
// seed, the vendor commands), and holds it to the RUNNABLE rules only.
//
// It skips [LoadCompany]'s admission rules ([Company.ValidateRunnable]): a file
// that breaks one must still start a node whose store already holds it, and
// still be provisionable. They apply where the file IS written as a revision:
// the seed checks [Company.ValidateAdmission] first, and `crewlet config
// import` and `crewlet validate` use [LoadCompany].
//
// Masks are restored against prior as in [LoadCompany]; commands that write no
// revision pass nil, and a mask in the file is refused by name.
func LoadCompanyToRun(path string, prior PriorRevision) (*Company, error) {
	return loadCompanyFile(path, prior, (*Company).ValidateRunnable)
}

// loadCompanyFile reads and parses a Tier B file, restores what a redacted
// read masked from prior, then holds it to validate, naming the file in every
// failure.
func loadCompanyFile(path string, prior PriorRevision, validate func(*Company) error) (*Company, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("company config %s: %w", path, err)
	}
	cfg, err := ParseCompanyDocument(data)
	if err == nil && prior != nil && len(cfg.UnresolvedMasks()) > 0 {
		// RESTORED BEFORE THE RULES, as the config surface restores a
		// document sent back to it: a masked credential is judged as the
		// value it stands for, and one left standing is the rule's to name.
		var was *Company
		if was, err = prior(); err != nil {
			err = fmt.Errorf("it carries redacted credentials, and the "+
				"revision they would be restored from cannot be read: %w", err)
		} else {
			cfg.RestoreRedacted(was)
		}
	}
	if err == nil {
		err = validate(cfg)
	}
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

// ParseCompanyDocument decodes Tier B WITHOUT validating it. The shape is
// still enforced (unknown fields, wrong kinds fail); only the whole-document
// rules are deferred.
//
// It is for callers that cannot validate yet: the config write path and the
// file loaders ([LoadCompany], [LoadCompanyToRun]), whose document may carry
// redaction masks that must be restored from the previous revision first.
// Validating earlier would reject "__redacted__" in a field the operator never
// touched. Everything else uses [ParseCompany]: a caller that forgot to
// validate later would learn of a bad config at the first turn.
func ParseCompanyDocument(data []byte) (*Company, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, syntaxFault(err)
	}
	return ParseCompanyNode(&doc)
}

// ParseCompanyNode is [ParseCompanyDocument] over a document already parsed,
// for a caller that took the document apart first (the write surface lifts a
// `_summary` key out of a body).
//
// THE NODES KEEP THEIR PARSED LINES and every failure is placed at its node,
// so it names the line in the text the caller sent; re-encoding the edited
// document renumbered lines and misplaced a typo by a line or two. doc is
// only read.
func ParseCompanyNode(doc *yaml.Node) (*Company, error) {
	if empty(doc) {
		return nil, fault(nil, ErrMissing, "the company config is empty; it needs at least a name")
	}
	if err := requireMapping(doc); err != nil {
		return nil, err
	}

	cfg := DefaultCompany()
	if err := decodeDocument(doc, &cfg); err != nil {
		return nil, err
	}
	// EVERY UNIT GETS A KEY AND EVERY SEAT ITS HANDLE, written into the
	// document where it declares none. Units are referenced by key and seats
	// by handle, and a seat's agent id, mailbox and memory derive from its
	// handle: left to derive from the display name they would move when
	// somebody fixes a typo in it, a removal and creation nobody asked for.
	// The config surface mints the same way on the two writes that read the
	// stored form ([MintIdentities]).
	MintIdentities(&cfg)
	// The declaration order of providers.llm exists only in the document (a Go
	// map has none), and per-phase resolution's last resort is "the first
	// provider configured". Read against the whole document, because the
	// strict decoder serialises a subtree alone and an alias to an anchor
	// defined elsewhere would stop resolving. See llmKeyOrder.
	//
	// Only when the document did not state one: a round trip through the config
	// surface carries the order in llm_order, not in key order (Go marshals
	// maps sorted).
	if len(cfg.Providers.LLMOrder) == 0 {
		cfg.Providers.LLMOrder = llmKeyOrder(doc)
	}
	return &cfg, nil
}

// ParseMember reads one member of a company document sent on its own (a
// seat, a unit, a model provider, an MCP server) into out, by the rules the
// whole document is read by: unknown keys refused, and every failure a
// [Fault] with its path inside the member and its line in data.
//
// The per-entity write surface reads these. encoding/json's strict decoder
// named a key and no place (`json: unknown field "gaol"`) and classified
// nothing. ONE READER for a member and its document keeps them agreeing on
// what a seat may carry.
//
// The caller knows where the member sits in the document and places the
// faults there; a path here starts at the member.
func ParseMember(data []byte, out any) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return syntaxFault(err)
	}
	return ParseMemberNode(&doc, out)
}

// ParseMemberNode is [ParseMember] over a document already parsed, for the
// reason [ParseCompanyNode] gives: the failures keep the lines of the text the
// caller sent. doc is only read.
func ParseMemberNode(doc *yaml.Node, out any) error {
	if empty(doc) {
		return fault(nil, ErrMissing, "the body is empty; send the whole entity, "+
			"as reading it from the same route answers it")
	}
	root := doc
	if root.Kind == yaml.DocumentNode {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return fault(nil, ErrShape, "an entity is a mapping of its fields, as "+
			"reading it from the same route answers it, not a list or a value")
	}
	return decodeDocument(doc, out)
}

// DecodeCompany reads Tier B from its STORED form: JSON marshalled from a
// parsed [Company]. It carries what the AUTHORED form does not
// (providers.llm_order, so a booting node resolves an unpinned seat as the
// authoring node did) and is LENIENT about unknown fields, where [ParseCompany]
// fails closed: an unknown key in a stored revision is a peer on a newer build,
// and rejecting it makes a mixed-version fleet an outage.
//
// It does NOT validate: a revision was valid under the build that WROTE it, and
// validating here made one a newer peer stored unreadable to every caller,
// repairing ones included (GET /config 500, PUT and PATCH failing on their
// merge base). Callers pick their rules: none to read, export, diff or serve as
// a prior; the runnable rules to apply; all, after masks, for a person's input.
func DecodeCompany(payload []byte) (*Company, error) {
	// Onto the DEFAULTS, not onto a zero value. A field the payload omits
	// must land on the same default the authored path gives it, or the
	// same company behaves differently depending on which door it came in
	// through.
	cfg := DefaultCompany()
	if err := json.Unmarshal(payload, &cfg); err != nil {
		return nil, &Fault{Kind: ErrShape, Detail: err.Error()}
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
		return fault(nil, ErrShape, "the config file must be a YAML mapping of settings")
	}
	return nil
}

// decodeKnown decodes a node into out with unknown fields rejected, for a
// custom UnmarshalYAML decoding a struct out of the node it was given.
//
// yaml.v3 only offers KnownFields on a Decoder, and a custom UnmarshalYAML
// that reaches for node.Decode gets a fresh decoder without it — which is a
// hole a typo can hide in. Every decoder in this package that has to decode
// a STRUCT out of a node re-enters through here or through [decodeDocument],
// so there is exactly one strict path and no shape that escapes it.
//
// Its failures go back to the calling decoder as a TypeError it keeps
// collecting after, placed in that decoder's text (see [carryFaults]).
func decodeKnown(node *yaml.Node, out any) error {
	return carryFaults(decodeNode(node, out))
}

// decodeDocument decodes a whole parsed document into out with unknown
// fields rejected, and gives every failure its authored path and its line in
// the text doc was parsed from.
func decodeDocument(doc *yaml.Node, out any) error {
	if err := decodeNode(doc, out); err != nil {
		return placeInDocument(doc, err)
	}
	return nil
}

// decodeNode is the strict decode both of those share, with its failures as
// faults on the nodes of the input they are about.
//
// Round-tripping through the encoder is the price of the strictness. It buys
// a guarantee that holds for shapes this package has not been written yet, on
// sub-documents that are a handful of keys wide. What it costs a failure is
// its line, which is a line of the encoded buffer: every failure is moved
// back onto the node it came from before it is returned (see position.go).
func decodeNode(node *yaml.Node, out any) error {
	// A FRACTION BOUND FOR A WHOLE NUMBER is refused before the decoder
	// sees it, because the decoder truncates it without a word — see
	// [refuseFractions] — and so is a number that is not finite, which it
	// reads faithfully into a field nothing can then bound
	// ([refuseNonFinite]). Collected beside whatever the decode itself
	// refuses, so one pass reports all of them.
	faults := append(refuseFractions(node, reflect.TypeOf(out)),
		refuseNonFinite(node, reflect.TypeOf(out))...)
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	if err := enc.Encode(blockStyle(node)); err != nil {
		return &Fault{Kind: ErrShape, Detail: err.Error()}
	}
	if err := enc.Close(); err != nil {
		return &Fault{Kind: ErrShape, Detail: err.Error()}
	}
	dec := yaml.NewDecoder(bytes.NewReader(buf.Bytes()))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		faults = append(faults, leafErrors(decodeError(err, indexBuffer(buf.Bytes(), node)))...)
	}
	return faults.err()
}

// decodeError translates yaml's decode failures into faults on the nodes of
// the input they are about, so a caller can tell a typo from a wrong shape
// and find either.
//
// A yaml.TypeError is every failure the decoder collected, one line each. Any
// other error stopped the decode inside a custom unmarshaler: a fault that a
// nested decodeKnown or a custom decoder already built, still placed in this
// buffer, which is moved onto the input without being wrapped again, since a
// second wrap would bury the sentinel a caller branches on.
func decodeError(err error, idx *bufferIndex) error {
	var typeErr *yaml.TypeError
	if !errors.As(err, &typeErr) {
		return idx.relocate(err)
	}
	var out problems
	for _, line := range typeErr.Errors {
		out = append(out, idx.typeFault(line))
	}
	return out.err()
}

// EncodeCompanyYAML renders a company config back to YAML, not the JSON the
// store holds: the output is for a PERSON (`crewlet config export`, diffs,
// edit and re-import), and the authored form is YAML. The struct tags are
// shared, so an export is importable by construction.
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

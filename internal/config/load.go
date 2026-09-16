package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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
		// An empty Tier A file is legitimate: every field defaults, and a
		// company can run on nothing but the defaults.
		cfg := DefaultBootstrap()
		return &cfg, cfg.Validate()
	}
	if err := requireMapping(&doc); err != nil {
		return nil, err
	}

	missing := r.Document(&doc)
	LogUnresolved("bootstrap", missing)

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
// # Why this one field and not every unresolved reference
//
// Because an unresolved reference expands to the empty string, and what that
// MEANS is the field's own business. Almost everywhere it is caught: an empty
// `store.path` is refused as a missing store, an empty credential surfaces as
// the authentication failure the resolver's warning predicts. `logging.file.path`
// is the exception, and a dangerous one — empty is a legitimate, common
// SETTING there, meaning "write no file". So `path: "${LOG_PATH}"` with the
// variable unset does not fail, or warn twice, or look wrong: it silently
// becomes the deployment that asked for no durable log at all.
//
// That is the precise failure this whole surface is arranged against. A path
// that cannot be OPENED already stops the boot rather than running without
// the record an operator configured; a path that never arrived has to do the
// same, or the policy holds only for the failures that are easy to notice.
func refuseUnresolvedLogFile(missing []Unresolved) error {
	for _, u := range missing {
		if u.Path != "logging.file.path" {
			continue
		}
		return fault(field(u.Path), ErrMissing,
			"nothing answered for %s, so the log file has no name and this node "+
				"would start with no durable log at all. Set the variable, or "+
				"write the path literally — an empty path means \"no file\" and "+
				"cannot be told apart from this",
			strings.Join(u.Names, ", "))
	}
	return nil
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
	return loadCompanyFile(path, (*Company).Validate)
}

// LoadCompanyToRun reads Tier B from a YAML file that is RUN or acted on
// rather than written into the store, and holds it to the RUNNABLE rules only.
//
// Its callers are `crewlet run`'s `-company` and `-import-company` seed, and
// the vendor commands (`crewlet gitlab provision` and its siblings) that act
// on the company the file describes. The difference from [LoadCompany] is
// the admission rules (see [Company.ValidateRunnable]). A company file that
// ran yesterday and breaks an admission rule added since must still start
// the node that runs it and still be provisionable, or every upgrade of a
// file-based deployment carrying an old duplicate is an outage. Most boots
// never write the file anywhere: it is byte for byte the revision the store
// already holds, or a bootstrap seed the store's own company outranks. The
// admission rules apply where the file IS written as a revision, which the
// seed decides and checks with [Company.ValidateAdmission] before it imports,
// and `crewlet config import` and `crewlet validate` use [LoadCompany].
func LoadCompanyToRun(path string) (*Company, error) {
	return loadCompanyFile(path, (*Company).ValidateRunnable)
}

// loadCompanyFile reads and parses a Tier B file, then holds it to validate,
// naming the file in every failure.
func loadCompanyFile(path string, validate func(*Company) error) (*Company, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("company config %s: %w", path, err)
	}
	cfg, err := ParseCompanyDocument(data)
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
		return nil, syntaxFault(err)
	}
	return ParseCompanyNode(&doc)
}

// ParseCompanyNode is [ParseCompanyDocument] over a document already parsed,
// for a caller that has taken the document apart before handing it on: the
// write surface lifts a `_summary` key out of a body before the company in it
// is read.
//
// THE NODES KEEP THE LINES THEY WERE PARSED FROM, and every failure is placed
// at its node, so a failure names the line it has in the text the caller sent.
// Encoding the edited document again to hand it to [ParseCompanyDocument]
// renumbered every line: in a YAML body opening with its summary, a typo
// twenty lines down was reported a line or two above where it was written.
// doc is only read.
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
		cfg.Providers.LLMOrder = llmKeyOrder(doc)
	}
	return &cfg, nil
}

// ParseMember reads one member of a company document sent on its own (a
// seat, a unit, a model provider, an MCP server) into out, by the rules the
// whole document is read by: unknown keys refused, and every failure a
// [Fault] with its path inside the member and its line in data.
//
// The per-entity write surface is what reads one. It used encoding/json's own
// strict decoder, whose refusal names a key and no place (`json: unknown field
// "gaol"`) and classifies as nothing, so the surface a person is most likely to
// edit by hand was the one whose mistakes could not be put beside the field.
// ONE READER for a member and for the document holding it, so the two cannot
// disagree about what a seat may carry.
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
//
// # It does not validate, and every caller decides what to hold it to
//
// A stored revision was valid under the rules of the build that WROTE it,
// which is not necessarily this one: a later build adds a rule, and a peer on
// an older build keeps activating documents that break it. A reader that
// validated here made such a revision unreadable to every caller at once,
// including the ones that could repair it. GET /config answered 500, a PUT
// and a PATCH failed opening their own merge base, and export refused, so the
// only way out was stopping the engine.
//
// So the decode is only a decode, and the rules live with the question being
// asked:
//
//   - Reading, exporting, diffing, and using a revision as the prior a write
//     restores its masks from or merges onto: no rules at all. Those readers
//     never run the company, and refusing them is what locks it out.
//   - Applying a revision (engine apply, boot, reload, revert): the rules a
//     running company depends on, before anything is built.
//   - A document a person submits: every rule, after its masks are restored.
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
		return decodeError(err, retiredFor(out), indexBuffer(buf.Bytes(), node))
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
//
// A TIER B MEMBER SENT ON ITS OWN IS STILL TIER B, which is why the member
// types [ParseMember] reads answer the company's table. The tier picks the
// table; the block inside it is picked one level down by [retiredKey], out of
// the Go type name yaml puts in its own message. So an `integrations:` on a
// unit reads the same whether it arrived inside a whole document or as the
// body of a per-entity write, and the entries that can only ever appear on a
// seat or a unit stay reachable from the surface most likely to carry them.
func retiredFor(out any) map[string]string {
	switch out.(type) {
	case *Bootstrap:
		return retiredBootstrapFields
	case *Company, *Role, *Unit, *LLMProvider, *MCPServer:
		return retiredCompanyFields
	}
	return nil
}

// retiredKey is how a retired-field table is addressed: the block the key
// belonged to, then the key. yaml.v3 names the Go type it was decoding
// ("config.Store"), and the package qualifier is dropped so the table reads
// as the document does.
//
// KEYED ON THE BLOCK, not on the bare name, because the same word means
// different things in different blocks. `driver` under `store:` was the
// storage engine and is retired; a `driver:` typed under `stream:` never
// existed there and has to read as the ordinary typo it is — the same
// distinction the two TIERS draw, one level down.
func retiredKey(goType, field string) string {
	if dot := strings.LastIndex(goType, "."); dot >= 0 {
		goType = goType[dot+1:]
	}
	return goType + "." + strings.Trim(field, `"`)
}

// retiredBootstrapFields are TIER A keys this build no longer accepts, keyed
// by the block they belonged to (see retiredKey) and mapped to what an
// operator should write instead.
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
	"Bootstrap.debug": "`debug` is no longer a setting: it was a second way to say " +
		"the log level and it is gone. Write `logging:` with `level: debug` " +
		"under it (and `level: info` is the default, so a `debug: false` " +
		"can simply be deleted)",
	"Stream.tracker_snapshot_max_bytes": "`stream.tracker_snapshot_max_bytes` " +
		"is retired: snapshots are files on this node's disk rather than " +
		"objects in the broker, so what bounds them is where they are kept. " +
		"Set `store.snapshot_dir`, and give that directory the space — the " +
		"snapshot loop refuses rather than filling the volume the database " +
		"is committing to",
	"Store.driver": "`store.driver` is no longer a setting: it chose between " +
		"two store implementations and there is one. Turso is the database; " +
		"the mainline-SQLite fallback and the CREWLET_STORE_DRIVER variable " +
		"that selected it are both gone. Delete the line; the file it names " +
		"opens unchanged either way, because both drivers wrote the same " +
		"SQLite file format",
}

// retiredCompanyFields are TIER B keys this build no longer accepts, on the
// same terms as [retiredBootstrapFields]: every entry is a name that shipped
// in the example company or the quickstart, and every entry is permanent.
//
// These are the keys the tracker and knowledge backends took over. The
// company document gained a choice it never had — the engine now HOLDS work
// items and pages rather than only reading somebody else's — and the identity
// keys that named a Jira project and a Confluence space became vendor-neutral
// in the same move, because a unit's project is the company's fact and not a
// product's.
var retiredCompanyFields = map[string]string{
	// The four horizons the native tracker was going to carry, and does
	// not. Each names what replaced it, and two of them say plainly that
	// the replacement is not the same thing — which is the whole reason
	// they are refused rather than ignored.
	"TrackerNativeConfig.trash_retention_days": "`trash_retention_days` is " +
		"retired: a removal on the native tracker has NO horizon at all. A " +
		"removed item is marked removed and stays that way, because a " +
		"tracker that quietly deleted what somebody removed by mistake is " +
		"one nobody can undo a mistake in. Delete the line",
	"TrackerNativeConfig.trash_compaction_days": "`trash_compaction_days` is " +
		"retired, on the same terms as `trash_retention_days`: nothing " +
		"compacts a removal, because the removal IS the record. Delete the line",
	"TrackerNativeConfig.change_compaction_days": "`change_compaction_days` is " +
		"retired. The nearest thing is `stream.tracker_retention.min_age` in " +
		"the OPERATOR's config, and it is NOT the same thing: it is a safety " +
		"floor on trimming the log's replay window, never a horizon after " +
		"which history is deleted. The history is kept. See " +
		"docs/getting-started/configuration.md",
	"TrackerNativeConfig.turn_compaction_days": "`turn_compaction_days` is " +
		"retired, on the same terms as `change_compaction_days`: " +
		"`stream.tracker_retention.min_age` bounds the log's replay window " +
		"and deletes no history. See `stream.tracker_retention` in " +
		"docs/getting-started/configuration.md",
	"Tracker.retention": "a `tracker.native.retention:` block is retired. " +
		"Those settings describe the OPERATOR's estate rather than the " +
		"company's policy — how they back up, how long their disk holds a " +
		"replay window — so they live in Tier A under " +
		"`stream.tracker_retention`",

	"Knowledge.confluence_spaces": "`knowledge.confluence_spaces` is now " +
		"`knowledge.scope`, and it scopes whichever knowledge base the " +
		"company runs rather than Confluence specifically. The values are " +
		"unchanged — rename the key",
	"Unit.integrations": "a unit's `integrations:` block is retired. Its two " +
		"identities are now direct keys on the unit: write `project: ENG` " +
		"where you wrote `integrations.jira.project`, and `space: ENG` where " +
		"you wrote `integrations.confluence.space`. They name whichever " +
		"tracker and knowledge base the company runs, so the org chart no " +
		"longer changes when the backend does",
	"RoleIntegrations.jira": "`integrations.jira.project` on a seat is now the " +
		"seat's own `project:` key, one level up beside `handle:`. It names " +
		"whichever tracker the company runs",
	"RoleIntegrations.confluence": "`integrations.confluence.space` on a seat " +
		"is now the seat's own `space:` key, one level up beside `handle:`. " +
		"It names whichever knowledge base the company runs",
	"Confluence.skills_space": "`integrations.confluence.skills_space` is now " +
		"`knowledge.skills_container`, because tool skills live in whichever " +
		"knowledge base the company runs. It is still three-valued: absent " +
		"takes the reserved default, a name takes that container, and an " +
		"explicit \"\" turns tool skills off",
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
func decodeError(err error, retired map[string]string, idx *bufferIndex) error {
	var typeErr *yaml.TypeError
	if !errors.As(err, &typeErr) {
		return idx.relocate(err)
	}
	var out problems
	for _, line := range typeErr.Errors {
		out = append(out, idx.typeFault(line, retired))
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

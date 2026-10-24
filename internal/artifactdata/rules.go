package artifactdata

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/artifactdata/canon"
	"github.com/crewlet/crewlet/internal/artifactdata/query"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/words"
)

// THE COLLECTION RULES: the name grammar, the presets and what they compile to,
// and what a collection's specification must hold to be stored.
//
// # One vocabulary, one evaluator
//
// A preset is a name people and agents pick by intent (`shared`, `contribute`,
// `author`, `private`); what the applier evaluates is the rule fields it
// compiles to (who reads, the least role that creates, the least role that
// deletes, and each column's own write role and condition). The compilation
// happens before a collection is stored, so the stored specification is the
// whole of the policy and there is exactly one evaluator for it.
//
// # Who may change which row, stated once
//
// A writer at EDITOR or above may write any row the field rules admit their
// role to. A writer BELOW editor (a contributor, a commenter, a viewer) may
// change only rows they created, except where a column EXPLICITLY names a write
// role, whose condition then decides (`own_row`, `assigned_to_viewer`, or any
// row when it names none): that is how a review sheet lets a contributor tick
// the box on a row an editor wrote and assigned to them, and why a contributor
// cannot rewrite another participant's sign-up. A `private` collection is the
// writer's alone at every role: per-viewer state is private even from the
// artifact's owner.

// docIDPattern is the document grammar. The collection grammar is
// [artifacts.ValidCollectionName]'s, the vocabulary every surface that names a
// collection shares (a dashboard spec's store dataset included).
var docIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// StateCollection is the engine's per-viewer state collection (D15): implicit
// in every store, schemaless and `private`, keyed per viewer. A page whose
// version declares the `state` capability keeps what a browser's storage would
// keep there, so it follows the viewer across devices and survives an opaque
// origin on which local storage throws.
const StateCollection = "_state"

// reservedDocIDs are document ids the surface's path segments take under a
// collection's route.
var reservedDocIDs = []string{"query"}

// ValidDocID reports whether id is a document id a collection may hold.
func ValidDocID(id string) bool {
	return len(id) <= artifacts.MaxDocIDBytes && docIDPattern.MatchString(id) &&
		!slices.Contains(reservedDocIDs, id)
}

// StateKeyLen is how much of a per-viewer state document's id the viewer's
// token takes, with its separator.
const stateTokenHex = 16

// StateDocID is the document id one viewer's state key is kept under in
// [StateCollection]: a token of the viewer's record, an underscore, and the
// key. A KEY IS THE VIEWER'S OWN, so two viewers' keys of one name are two
// documents, and the applier refuses a state document whose token is not its
// writer's.
func StateDocID(record, key string) (string, error) {
	id := StateToken(record) + "_" + key
	if key == "" || !ValidDocID(id) {
		return "", fmt.Errorf("%w: state key %q: a key is 1..%d letters, digits, `_` "+
			"and `-`", ErrInvalid, key, artifacts.MaxDocIDBytes-stateTokenHex-1)
	}
	return id, nil
}

// StateToken is the prefix a viewer's state documents carry: the first eight
// bytes of SHA-256 over the record the viewer acts under, in lowercase hex. A
// record is not a secret and this hides nothing; it fits a document id's
// grammar where a login's dots and colons do not.
func StateToken(record string) string {
	sum := sha256.Sum256([]byte(record))
	return hex.EncodeToString(sum[:stateTokenHex/2])
}

// StateKey is the key a state document id carries after its viewer's token.
func StateKey(docID string) string {
	if len(docID) > stateTokenHex+1 {
		return docID[stateTokenHex+1:]
	}
	return ""
}

// stateSpec is [StateCollection]'s specification: private, schemaless, any
// participant writes their own.
func stateSpec() artifacts.Collection {
	return artifacts.Collection{
		Name: StateCollection, Preset: artifacts.PresetPrivate,
		Read: artifacts.ReadPrivate, Create: artifacts.RoleViewer, Delete: artifacts.RoleViewer,
	}
}

// closedDefault is the specification of a collection a write creates without
// a schema change: participants read, editors write (03 section 4.1).
func closedDefault(name string) artifacts.Collection {
	return artifacts.Collection{
		Name: name, Preset: artifacts.PresetShared,
		Read: artifacts.ReadAll, Create: artifacts.RoleEditor, Delete: artifacts.RoleEditor,
	}
}

// presetRules is what each preset compiles to.
var presetRules = map[artifacts.Preset]struct {
	read           artifacts.ReadRule
	create, delete artifacts.Role
}{
	// Participants read, editors write: review tables and trackers.
	artifacts.PresetShared: {artifacts.ReadAll, artifacts.RoleEditor, artifacts.RoleEditor},
	// Anyone who can view adds rows and changes their own; editors change
	// any: sign-up sheets, polls, feedback forms.
	artifacts.PresetContribute: {artifacts.ReadAll, artifacts.RoleViewer, artifacts.RoleViewer},
	// The owner (and the authoring agent, which owns what it made) writes:
	// a dashboard an agent maintains.
	artifacts.PresetAuthor: {artifacts.ReadAll, artifacts.RoleOwner, artifacts.RoleOwner},
	// Each writer reads and writes only their own rows.
	artifacts.PresetPrivate: {artifacts.ReadPrivate, artifacts.RoleViewer, artifacts.RoleViewer},
}

// Compile fills a collection's rule fields from its preset, which is what is
// stored. A collection naming neither a preset nor rules takes the closed
// default (`shared`); one naming rules beside a preset must name exactly what
// the preset compiles to, so a preset never silently overrides what a caller
// wrote.
func Compile(c artifacts.Collection) (artifacts.Collection, error) {
	if c.Preset == "" && c.Read == "" && c.Create == "" && c.Delete == "" {
		c.Preset = artifacts.PresetShared
	}
	if c.Preset != "" {
		rules, known := presetRules[c.Preset]
		if !known {
			return c, fmt.Errorf("%w: collection %q: preset %q is not one of %s", ErrInvalid,
				c.Name, c.Preset, words.Or(artifacts.Presets))
		}
		for _, clash := range []struct {
			field      string
			got, wants string
		}{
			{"read", string(c.Read), string(rules.read)},
			{"create", string(c.Create), string(rules.create)},
			{"delete", string(c.Delete), string(rules.delete)},
		} {
			if clash.got != "" && clash.got != clash.wants {
				return c, fmt.Errorf("%w: collection %q: preset %s compiles %s to %q and the "+
					"collection names %q; name one or the other", ErrInvalid, c.Name, c.Preset,
					clash.field, clash.wants, clash.got)
			}
		}
		c.Read, c.Create, c.Delete = rules.read, rules.create, rules.delete
	}
	return c, nil
}

// ValidateCollection holds a compiled collection to everything a stored
// specification must be: the name, the rules, the bounds, and every column.
func ValidateCollection(c artifacts.Collection) error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%w: collection %q: %s", ErrInvalid, c.Name, fmt.Sprintf(format, args...))
	}
	switch {
	case !artifacts.ValidCollectionName(c.Name):
		return fail("a collection name is a lowercase letter, then lowercase letters, digits "+
			"and `_`, at most %d bytes, and not one of %s", artifacts.MaxCollectionNameBytes,
			words.Or(artifacts.ReservedCollections))
	case len(c.Title) > MaxLabelBytes:
		return fail("the title is %d bytes and the maximum is %d", len(c.Title), MaxLabelBytes)
	case c.Preset != "" && !c.Preset.Valid():
		return fail("preset %q is not one of %s", c.Preset, words.Or(artifacts.Presets))
	case !c.Read.Valid():
		return fail("read rule %q is not one of %s", c.Read, words.Or(artifacts.ReadRules))
	case !c.Create.Valid() || !c.Delete.Valid():
		return fail("the create and delete roles are each one of %s", words.Or(artifacts.Roles))
	case c.MaxRows < 0 || c.MaxRows > artifacts.MaxDocsPerStore:
		return fail("max_rows is 0..%d", artifacts.MaxDocsPerStore)
	case c.MaxRowsPerPrincipal < 0 || c.MaxRowsPerPrincipal > artifacts.MaxDocsPerStore:
		return fail("max_rows_per_principal is 0..%d", artifacts.MaxDocsPerStore)
	case len(c.Columns) > artifacts.MaxColumns:
		return fail("it declares %d columns and the maximum is %d", len(c.Columns), artifacts.MaxColumns)
	case len(c.Indexed) > artifacts.MaxIndexedPaths:
		return fail("it indexes %d paths and the maximum is %d", len(c.Indexed), artifacts.MaxIndexedPaths)
	}
	keys := map[string]bool{}
	for _, col := range c.Columns {
		if keys[col.Key] {
			return fail("column %q is declared twice", col.Key)
		}
		keys[col.Key] = true
		if err := validateColumn(col); err != nil {
			return fmt.Errorf("collection %q: %w", c.Name, err)
		}
	}
	for _, col := range c.Columns {
		if col.When == artifacts.WhenAssignedToViewer {
			i := slices.IndexFunc(c.Columns, func(o artifacts.Column) bool { return o.Key == col.AssignedField })
			if i < 0 || c.Columns[i].Type != artifacts.ColumnPerson {
				return fail("column %q is written when assigned to the viewer, so its "+
					"assigned_field names a person column, and %q is not one",
					col.Key, col.AssignedField)
			}
		}
	}
	seen := map[string]bool{}
	for _, p := range c.Indexed {
		head, _, dotted := strings.Cut(p, ".")
		switch {
		case !query.ValidPath(p) || !dotted:
			return fail("indexed path %q is a dotted path one level deep (every top-level "+
				"field is already indexed)", p)
		case seen[p]:
			return fail("indexed path %q is named twice", p)
		case c.Columns != nil && !keys[head]:
			return fail("indexed path %q is under %q, which no column declares", p, head)
		}
		seen[p] = true
	}
	return nil
}

// validateColumn holds one column to its type's rules.
func validateColumn(col artifacts.Column) error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%w: column %q: %s", ErrInvalid, col.Key, fmt.Sprintf(format, args...))
	}
	switch {
	case !query.ValidField(col.Key):
		return fail("a column key is a field name: a letter, then letters, digits and `_`, at "+
			"most %d bytes", artifacts.MaxFieldNameBytes)
	case !col.Type.Valid():
		return fail("type %q is not one of %s", col.Type, words.Or(artifacts.ColumnTypes))
	case len(col.Label) > MaxLabelBytes:
		return fail("the label is %d bytes and the maximum is %d", len(col.Label), MaxLabelBytes)
	case col.Write != "" && !col.Write.Valid():
		return fail("write role %q is not one of %s", col.Write, words.Or(artifacts.Roles))
	case col.When != "" && !col.When.Valid():
		return fail("condition %q is not one of %s", col.When, words.Or(artifacts.FieldConditions))
	case col.When != "" && col.Write == "":
		return fail("a condition narrows a write role the column names, and it names none")
	case col.When != artifacts.WhenAssignedToViewer && col.AssignedField != "":
		return fail("assigned_field is read only by the assigned_to_viewer condition")
	case col.Mode != "" && !col.Mode.Valid():
		return fail("mode %q is not one of %s", col.Mode, words.Or(artifacts.FieldModes))
	case col.Mode == artifacts.FieldAppend && !appendable(col.Type):
		return fail("mode append is for text, long text and multiselect columns")
	case col.MaxLength < 0 || col.MaxItems < 0:
		return fail("max_length and max_items are not negative")
	case col.Min != nil && col.Max != nil && *col.Min > *col.Max:
		return fail("min is above max")
	case len(col.Options) > MaxOptions:
		return fail("it offers %d options and the maximum is %d", len(col.Options), MaxOptions)
	}
	switch col.Type {
	case artifacts.ColumnText:
		if col.MaxLength > MaxTextLength {
			return fail("max_length is at most %d", MaxTextLength)
		}
	case artifacts.ColumnLongText:
		if col.MaxLength > MaxLongTextLength {
			return fail("max_length is at most %d", MaxLongTextLength)
		}
	case artifacts.ColumnMultiSelect:
		if col.MaxItems > MaxItemsCeiling {
			return fail("max_items is at most %d", MaxItemsCeiling)
		}
	case artifacts.ColumnRating:
		if col.Max != nil && (*col.Max < 1 || *col.Max > MaxRatingMax || *col.Max != float64(int(*col.Max))) {
			return fail("a rating's max is a whole number 1..%d", MaxRatingMax)
		}
	}
	if hasOptions(col.Type) {
		if len(col.Options) == 0 {
			return fail("a %s column offers at least one option", col.Type)
		}
		ids := map[string]bool{}
		for _, o := range col.Options {
			switch {
			case !optionIDPattern.MatchString(o.ID):
				return fail("option id %q is 1..64 letters, digits, `_` and `-`", o.ID)
			case ids[o.ID]:
				return fail("option id %q is offered twice", o.ID)
			case len(o.Label) > MaxLabelBytes:
				return fail("option %q's label is %d bytes and the maximum is %d", o.ID,
					len(o.Label), MaxLabelBytes)
			case o.Color != "" && !colorPattern.MatchString(o.Color):
				return fail("option %q's color is a design-token hue name such as `green`, "+
					"never a literal colour", o.ID)
			case col.Type == artifacts.ColumnStatus && !slices.Contains(StatusCategories, o.Category):
				return fail("status option %q names a category, one of %s", o.ID, words.Or(StatusCategories))
			case col.Type != artifacts.ColumnStatus && o.Category != "":
				return fail("option %q names a category, which only a status column reads", o.ID)
			}
			ids[o.ID] = true
		}
	} else if len(col.Options) > 0 {
		return fail("a %s column offers no options", col.Type)
	}
	if len(col.Default) > 0 {
		v, err := canon.Parse(col.Default, MaxValueDepth)
		if err != nil {
			return fail("the default is not one JSON value: %v", err)
		}
		if v.Kind != canon.KindNull {
			if err := checkValue(col, v); err != nil {
				return fmt.Errorf("the default: %w", err)
			}
		}
	}
	return nil
}

// appendable reports a column type a value may grow in: text gains
// characters, a multiselect gains options.
func appendable(t artifacts.ColumnType) bool {
	return t == artifacts.ColumnText || t == artifacts.ColumnLongText || t == artifacts.ColumnMultiSelect
}

// hasOptions reports a column type whose values are option ids.
func hasOptions(t artifacts.ColumnType) bool {
	return t == artifacts.ColumnSelect || t == artifacts.ColumnMultiSelect || t == artifacts.ColumnStatus
}

// canonicalSpec is a collection's stored specification: its canonical JSON.
func canonicalSpec(c artifacts.Collection) ([]byte, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("artifactdata: encode collection %q: %w", c.Name, err)
	}
	return canon.Canonical(raw, canon.MaxDepth)
}

// decodeSpec reads a stored specification.
func decodeSpec(spec string) (artifacts.Collection, error) {
	var c artifacts.Collection
	if err := json.Unmarshal([]byte(spec), &c); err != nil {
		return c, fmt.Errorf("artifactdata: decode a stored collection: %w", err)
	}
	return c, nil
}

// Loosening is one authority control a schema change loosens, for the rule
// that only a person loosens one (03 section 4.2).
type Loosening struct {
	Collection string
	What       string
}

// Loosenings is every authority control new loosens relative to old: a read
// rule that admits more readers, a create, delete or column write role that
// admits lower roles, a condition removed or an `assigned_to_viewer` column
// repointed at another assignee, `humans_only` removed, a column's
// append mode or immutability removed, and a collection dropped (its rows'
// protection with it). A collection new adds is not a loosening: it protects
// nothing yet.
func Loosenings(old, new []artifacts.Collection) []Loosening {
	var out []Loosening
	byName := map[string]artifacts.Collection{}
	for _, c := range new {
		byName[c.Name] = c
	}
	for _, o := range old {
		n, kept := byName[o.Name]
		if !kept {
			out = append(out, Loosening{o.Name, "the collection is dropped"})
			continue
		}
		if readRank(n.Read) > readRank(o.Read) {
			out = append(out, Loosening{o.Name, fmt.Sprintf("read %s becomes %s", o.Read, n.Read)})
		}
		if o.Create.Includes(n.Create) && n.Create != o.Create {
			out = append(out, Loosening{o.Name, fmt.Sprintf("create %s becomes %s", o.Create, n.Create)})
		}
		if o.Delete.Includes(n.Delete) && n.Delete != o.Delete {
			out = append(out, Loosening{o.Name, fmt.Sprintf("delete %s becomes %s", o.Delete, n.Delete)})
		}
		cols := map[string]artifacts.Column{}
		for _, c := range n.Columns {
			cols[c.Key] = c
		}
		for _, oc := range o.Columns {
			nc, kept := cols[oc.Key]
			if !kept {
				// A DROPPED PROTECTED COLUMN IS ITS PROTECTION DROPPED: an
				// agent that may not lift `humans_only` must not drop the
				// approval column and declare it again without it.
				if oc.HumansOnly || oc.Immutable || oc.Mode == artifacts.FieldAppend ||
					oc.When != "" || effectiveWrite(oc, o) != o.Create {
					out = append(out, Loosening{o.Name, fmt.Sprintf("column %s, which "+
						"restricts its writers, is dropped", oc.Key)})
				}
				continue
			}
			was, is := effectiveWrite(oc, o), effectiveWrite(nc, n)
			if was.Includes(is) && was != is {
				out = append(out, Loosening{o.Name, fmt.Sprintf("column %s is written by %s "+
					"rather than %s", oc.Key, is, was)})
			}
			if oc.When != "" && nc.When != oc.When {
				out = append(out, Loosening{o.Name, fmt.Sprintf("column %s loses its %s condition",
					oc.Key, oc.When)})
			}
			// THE ASSIGNEE IS THE CONDITION: `assigned_to_viewer` admits
			// whoever the assigned person column names, so pointing it at
			// another column hands the write to whoever may fill that one
			// (and to everybody it already names), whatever the condition's
			// name still says.
			if oc.When == artifacts.WhenAssignedToViewer && nc.When == oc.When &&
				nc.AssignedField != oc.AssignedField {
				out = append(out, Loosening{o.Name, fmt.Sprintf("column %s takes its assignee "+
					"from %s rather than %s", oc.Key, nc.AssignedField, oc.AssignedField)})
			}
			if oc.HumansOnly && !nc.HumansOnly {
				out = append(out, Loosening{o.Name, fmt.Sprintf("column %s is no longer humans only",
					oc.Key)})
			}
			if oc.Mode == artifacts.FieldAppend && nc.Mode != artifacts.FieldAppend {
				out = append(out, Loosening{o.Name, fmt.Sprintf("column %s is no longer append only",
					oc.Key)})
			}
			if oc.Immutable && !nc.Immutable {
				out = append(out, Loosening{o.Name, fmt.Sprintf("column %s is no longer immutable",
					oc.Key)})
			}
		}
	}
	return out
}

// readRank orders read rules by how many readers each admits.
func readRank(r artifacts.ReadRule) int {
	switch r {
	case artifacts.ReadPrivate:
		return 1
	case artifacts.ReadEditors:
		return 2
	case artifacts.ReadOwnRows:
		return 3
	case artifacts.ReadAll:
		return 4
	}
	// A RULE THIS BUILD CANNOT RANK admits everybody as far as a loosening
	// is concerned: the direction that keeps an agent from replacing it.
	return 5
}

// effectiveWrite is the least role that writes a column: its own write role,
// or its collection's create role.
func effectiveWrite(col artifacts.Column, c artifacts.Collection) artifacts.Role {
	if col.Write != "" {
		return col.Write
	}
	return c.Create
}

// agentMayChange reports whether an actor of kind may make a schema change
// with these loosenings: a person and an operator may; an agent may tighten
// freely and may loosen nothing (03 section 4.2).
func agentMayChange(kind iam.ActorKind, loose []Loosening) error {
	if kind != iam.ActorAgent || len(loose) == 0 {
		return nil
	}
	whats := make([]string, 0, len(loose))
	for _, l := range loose {
		whats = append(whats, l.Collection+": "+l.What)
	}
	return fmt.Errorf("%w: an agent tightens a schema and never loosens one, and this change "+
		"loosens %s; ask a person to make it", ErrForbidden, strings.Join(whats, "; "))
}

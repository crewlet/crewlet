package artifacts

import (
	"encoding/json"
	"regexp"
	"slices"
)

// THE DATASTORE'S SHAPES: what a write carries and what a collection declares.
// Validation, merge semantics and the canonical encoding are
// internal/artifactdata's; these are the words every surface (the HTTP
// surface, the agent tools, the dashboard's contract) shares.
//
// VALUES TRAVEL AS RAW JSON so a number keeps its decimal text end to end: a
// float64 on the way through rewrites a large integer, and two builds that
// round differently would write different bytes for one record.

// DataOpKind is what one datastore op does to one document.
type DataOpKind string

const (
	// DataSet replaces the document's fields, creating it if absent.
	DataSet DataOpKind = "set"

	// DataMerge sets the named top-level fields and leaves the rest.
	DataMerge DataOpKind = "merge"

	// DataDelete tombstones the document, so a reconnecting client
	// catching up by version sees the delete.
	DataDelete DataOpKind = "delete"

	// DataIncrement adds a numeric delta to one field.
	DataIncrement DataOpKind = "increment"

	// DataAppend grows one long-text field, or adds one element to an
	// array field, under a field rule whose mode is append.
	DataAppend DataOpKind = "append"

	// DataAdd adds one element to an array field (a multiselect) if absent.
	DataAdd DataOpKind = "add"

	// DataRemove removes one element from an array field if present.
	DataRemove DataOpKind = "remove"
)

// DataOpKinds are every op this build applies.
var DataOpKinds = []DataOpKind{
	DataSet, DataMerge, DataDelete, DataIncrement, DataAppend, DataAdd, DataRemove,
}

// Valid reports whether k is an op this build applies.
func (k DataOpKind) Valid() bool { return slices.Contains(DataOpKinds, k) }

// DataOp is one operation on one document.
type DataOp struct {
	Op         DataOpKind `json:"op"`
	Collection string     `json:"collection"`
	Doc        string     `json:"doc"`

	// IfVersion is the document's version (a packed log position) the
	// writer saw; the applier refuses the whole batch as a conflict when the
	// document has moved since. Nil is a blind write, which is what a click
	// on a checkbox is.
	IfVersion *int64 `json:"if_version,omitempty"`

	// Fields are the field values of a set or a merge.
	Fields map[string]json.RawMessage `json:"fields,omitempty"`

	// Field and Value are the one field and operand of an increment, an
	// append, an add or a remove.
	Field string          `json:"field,omitempty"`
	Value json.RawMessage `json:"value,omitempty"`
}

// DataWrite is one record's worth of ops: 1 to [MaxBatchOps], applied all or
// nothing.
type DataWrite struct {
	Ops []DataOp `json:"ops"`

	// ClientID names the client session, so a page can recognise its own
	// writes in the pushes it receives.
	ClientID string `json:"client_id,omitempty"`
}

// DataVerdict is what the applier decided about one write, identically on
// every data node.
type DataVerdict string

// The verdicts a datastore batch can be given.
const (
	DataApplied   DataVerdict = "applied"
	DataConflict  DataVerdict = "conflict"  // an IfVersion guard moved
	DataInvalid   DataVerdict = "invalid"   // grammar, schema or type
	DataForbidden DataVerdict = "forbidden" // a collection or field rule refused the stamped role or kind
	DataQuota     DataVerdict = "quota"     // a stamped limit or a store cap
	DataGone      DataVerdict = "gone"      // the store was purged or the artifact trashed
)

// DataVerdicts are every verdict an applier writes.
var DataVerdicts = []DataVerdict{DataApplied, DataConflict, DataInvalid, DataForbidden, DataQuota, DataGone}

// Valid reports whether v is a verdict this build writes.
func (v DataVerdict) Valid() bool { return slices.Contains(DataVerdicts, v) }

// ---- schema -------------------------------------------------------------- //

// ColumnType is how one field is stored, validated and drawn.
type ColumnType string

// The types a collection's column can declare.
const (
	ColumnText        ColumnType = "text"        // single line
	ColumnLongText    ColumnType = "longtext"    // whole-field guarded writes plus append
	ColumnNumber      ColumnType = "number"      // finite, min, max
	ColumnCheckbox    ColumnType = "checkbox"    // bool
	ColumnSelect      ColumnType = "select"      // one option id, drawn as a chip
	ColumnMultiSelect ColumnType = "multiselect" // option ids, add and remove
	ColumnStatus      ColumnType = "status"      // one option id whose option has a category
	ColumnPerson      ColumnType = "person"      // a seat handle in the running org
	ColumnDate        ColumnType = "date"        // YYYY-MM-DD, or RFC 3339 UTC
	ColumnURL         ColumnType = "url"         // absolute https or http only
	ColumnRating      ColumnType = "rating"      // 1..max
)

// ColumnTypes are every column type this build validates.
var ColumnTypes = []ColumnType{
	ColumnText, ColumnLongText, ColumnNumber, ColumnCheckbox, ColumnSelect,
	ColumnMultiSelect, ColumnStatus, ColumnPerson, ColumnDate, ColumnURL, ColumnRating,
}

// Valid reports whether t is a column type this build validates.
func (t ColumnType) Valid() bool { return slices.Contains(ColumnTypes, t) }

// ReadRule is who reads a collection's rows, on every read path: the list, a
// query, the history, a live push and an export.
type ReadRule string

// The read rules a collection can carry.
const (
	ReadAll     ReadRule = "all"      // every participant
	ReadOwnRows ReadRule = "own_rows" // each participant their own rows; editors and above every row
	ReadEditors ReadRule = "editors"  // editors and above only
	ReadPrivate ReadRule = "private"  // the writer alone: per-viewer state
)

// ReadRules are every read rule this build enforces.
var ReadRules = []ReadRule{ReadAll, ReadOwnRows, ReadEditors, ReadPrivate}

// Valid reports whether r is a read rule this build enforces.
func (r ReadRule) Valid() bool { return slices.Contains(ReadRules, r) }

// FieldCondition narrows a field's write rule to some rows.
type FieldCondition string

const (
	// WhenOwnRow admits a write only on rows the writer created.
	WhenOwnRow FieldCondition = "own_row"

	// WhenAssignedToViewer admits a write only on rows whose person field
	// (Column.AssignedField) names the writer's seat.
	WhenAssignedToViewer FieldCondition = "assigned_to_viewer"
)

// FieldConditions are every condition this build evaluates. The zero value
// is "no condition" and is not a member.
var FieldConditions = []FieldCondition{WhenOwnRow, WhenAssignedToViewer}

// Valid reports whether c is a condition this build evaluates.
func (c FieldCondition) Valid() bool { return slices.Contains(FieldConditions, c) }

// FieldMode is how a field's value may change.
type FieldMode string

// FieldAppend admits only growth: text appended, elements added.
const FieldAppend FieldMode = "append"

// FieldModes are every mode this build enforces. The zero value is
// "replace" and is not a member.
var FieldModes = []FieldMode{FieldAppend}

// Valid reports whether m is a mode this build enforces.
func (m FieldMode) Valid() bool { return slices.Contains(FieldModes, m) }

// Preset names a collection policy people and agents pick by intent. The API
// compiles it into the rule fields before the collection is stored, so one
// evaluator reads one vocabulary.
type Preset string

// The presets a collection can start from.
const (
	PresetShared     Preset = "shared"     // participants read, editors write
	PresetContribute Preset = "contribute" // participants add rows and edit their own
	PresetAuthor     Preset = "author"     // the owner and the authoring agent write
	PresetPrivate    Preset = "private"    // each writer reads and writes only their own rows
)

// Presets are every preset this build compiles.
var Presets = []Preset{PresetShared, PresetContribute, PresetAuthor, PresetPrivate}

// Valid reports whether p is a preset this build compiles.
func (p Preset) Valid() bool { return slices.Contains(Presets, p) }

// Option is one choice of a select, multiselect or status column. Its ID is
// stable: relabelling or recolouring never rewrites data, and deleting an
// option archives it.
type Option struct {
	ID    string `json:"id"`
	Label string `json:"label"`

	// Color is a design-token hue name, never a literal colour.
	Color string `json:"color,omitempty"`

	// Category is a status option's category: todo, in_progress, done or
	// blocked.
	Category string `json:"category,omitempty"`

	Archived bool `json:"archived,omitempty"`
}

// Column is one declared field of a collection, with its type and its rules.
type Column struct {
	Key       string     `json:"key"`
	Type      ColumnType `json:"type"`
	Label     string     `json:"label,omitempty"`
	Required  bool       `json:"required,omitempty"`
	Primary   bool       `json:"primary,omitempty"`
	Hidden    bool       `json:"hidden,omitempty"`
	Min       *float64   `json:"min,omitempty"`
	Max       *float64   `json:"max,omitempty"`
	MaxLength int        `json:"max_length,omitempty"`
	MaxItems  int        `json:"max_items,omitempty"`
	Options   []Option   `json:"options,omitempty"`

	// Default is the value a read fills when the field is absent.
	Default json.RawMessage `json:"default,omitempty"`

	// Write is the least role that may write this field; empty takes the
	// collection's create role.
	Write Role `json:"write,omitempty"`

	When          FieldCondition `json:"when,omitempty"`
	AssignedField string         `json:"assigned_field,omitempty"`
	Mode          FieldMode      `json:"mode,omitempty"`
	Immutable     bool           `json:"immutable,omitempty"`

	// HumansOnly refuses an agent seat writing this field whatever its role:
	// the approval column a model must never tick. An agent may add it and
	// never remove it.
	HumansOnly bool `json:"humans_only,omitempty"`
}

// Collection is one named set of documents in a store, with its rules.
type Collection struct {
	Name   string `json:"name"`
	Title  string `json:"title,omitempty"`
	Preset Preset `json:"preset,omitempty"`

	Read   ReadRule `json:"read"`
	Create Role     `json:"create"`
	Delete Role     `json:"delete"`

	MaxRows             int  `json:"max_rows,omitempty"`
	MaxRowsPerPrincipal int  `json:"max_rows_per_principal,omitempty"`
	OncePerPrincipal    bool `json:"once_per_principal,omitempty"`

	// Columns nil is a schemaless collection under the closed default:
	// participants read, editors write.
	Columns []Column `json:"columns,omitempty"`

	// Indexed are dotted paths indexed beside the top-level scalar fields.
	Indexed []string `json:"indexed,omitempty"`
}

// collectionPattern is the collection grammar. A leading underscore is
// reserved for the engine and refused by its first character.
var collectionPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// ReservedCollections are names the HTTP surface's own path segments take
// under `/artifacts/{id}/data/`, so a collection of that name could never be
// read by its own route.
var ReservedCollections = []string{"schema", "history", "state", "writes", "undo", "export"}

// ValidCollectionName reports whether name is a collection a store may hold:
// the one grammar the datastore's schema, its writes and a dashboard spec's
// store dataset are each held to.
func ValidCollectionName(name string) bool {
	return len(name) <= MaxCollectionNameBytes && collectionPattern.MatchString(name) &&
		!slices.Contains(ReservedCollections, name)
}

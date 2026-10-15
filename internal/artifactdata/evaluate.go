package artifactdata

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/artifactdata/canon"
	"github.com/crewlet/crewlet/internal/artifactdata/query"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
)

// THE DECISION A WRITE GETS: a pure function of the batch, the stamped
// authority and the rows this transaction reads, so every data node reaches the same
// verdict for every record.
//
// It is TOTAL. Every input a record can carry, hostile or merely wrong, ends in
// a verdict rather than an error: an error from the applier stops the log on
// every data node at once (the datastore's whole company read-only until a build
// ships), while a verdict is one refused write. The only errors here are the
// store's own (a read that failed), which the framework retries.

// collState is one collection as the evaluation sees it.
type collState struct {
	no       int64
	spec     artifacts.Collection
	columns  map[string]artifacts.Column
	implicit bool

	// stored says the collection has a row; a collection a write creates
	// under the closed default (or the state collection's first write) has
	// none until the plan is applied.
	stored bool

	// docs is the collection's live documents before the batch.
	docs int64
}

// newCollState wraps a specification.
func newCollState(spec artifacts.Collection, no int64, implicit, stored bool, docs int64) *collState {
	c := &collState{no: no, spec: spec, implicit: implicit, stored: stored, docs: docs}
	if spec.Columns != nil {
		c.columns = make(map[string]artifacts.Column, len(spec.Columns))
		for _, col := range spec.Columns {
			c.columns[col.Key] = col
		}
	}
	return c
}

// schemaless reports a collection with no declared columns.
func (c *collState) schemaless() bool { return c.spec.Columns == nil }

// docState is one document before and during a batch.
type docState struct {
	coll *collState
	id   string

	// The document as the batch found it.
	row      bool // a row (live or tombstone) exists
	live0    bool
	version0 int64
	fields0  map[string]canon.Value
	body0    []byte
	entries0 int
	metered0 int64 // what the row and its index entries cost as found ([meteredDoc])

	createdBy, createdByKind string
	createdAt                int64

	// The document as the batch leaves it.
	live    bool
	fields  map[string]canon.Value
	created bool // the batch created (or brought back) this document
	changed bool
	listed  bool // the batch has this document in its order
	reindex bool // a schema change re-indexes it whatever its body

	// takeover marks a create over another principal's tombstone in a
	// collection whose read rule scopes rows to their creator: the row
	// changes hands, so whoever held it before stops seeing it by version
	// and must read the store whole ([storeRow.tombstoneFloor]).
	takeover bool

	// The canonical body and field entries of the result, computed once.
	body    []byte
	entries []fieldEntry
}

// fieldEntry is one row of the field index.
type fieldEntry struct {
	path string
	ord  int
	kind string // s, n, b, z, or p for a presence marker
	num  float64
	txt  string
}

// A PRESENCE MARKER is the one index entry a top-level field holding nothing
// scalar takes (an empty list, an object, a list of objects), at its own ord
// and kind, so every top-level field a document holds has at least one row in
// the field index. A schema change finds the documents holding a removed or
// retyped column through that index; without the marker, a row whose list was
// emptied by removing its last chip would keep the removed column for good, and
// a whole-row write of it would be refused as naming an undeclared field. No
// query clause reads either value: a filter, `contains` and the order all
// read ord 0 or above.
const (
	presenceOrd  = -1
	presenceKind = "p"
)

// view is what an evaluation reads: the store's rows, through the applier's
// transaction or through a test's own.
type view interface {
	collection(name string) (*collState, error)
	doc(c *collState, id string) (*docState, error)
	ownedLive(c *collState, actor string) (int64, error)
	totals() storeTotals
}

// storeTotals are a store's counters before the batch.
type storeTotals struct {
	docs, docBytes, historyBytes, collections int64

	// companyFootprint is every store's footprint together.
	companyFootprint int64
}

// footprint is the store's footprint from its counters ([storeRow.computeFootprint]).
func (t storeTotals) footprint() int64 { return t.docBytes + t.historyBytes }

// refusal is a verdict other than applied, and why.
//
// IT IS A VALUE AND NEVER AN ERROR: the record's verdict, written as a history
// row on every data node, or a decide-time answer the writer restates in its
// caller's terms ([configRefusal]), so it never leaves this package as itself.
// Every function that can refuse returns one BESIDE its error, which is only
// ever the store's. As an error it shared one return with a failed read, told
// apart by an errors.As at the end of the chain, and it was an unexported
// error type, which the estate router's wire cannot rebuild: one that escaped
// a writer would have crossed as its message alone.
type refusal struct {
	verdict   artifacts.DataVerdict
	reason    string
	conflicts []docRef

	// creators is each conflicting document's creator as the record found
	// it, which its history row is shown under.
	creators map[docRef]string

	// class is the sentinel a decide-time refusal answers its caller with,
	// where the verdict alone does not say which.
	class error
}

// docRef names one document.
type docRef struct {
	Collection string `json:"c"`
	Doc        string `json:"d"`
}

// refuse builds a refusal.
func refuse(v artifacts.DataVerdict, format string, args ...any) *refusal {
	return &refusal{verdict: v, reason: fmt.Sprintf(format, args...)}
}

// fromErr is a validation error as an invalid refusal, in the validator's own
// words without the package's prefix.
func fromErr(err error) *refusal {
	return &refusal{verdict: artifacts.DataInvalid, reason: strings.TrimPrefix(err.Error(),
		ErrInvalid.Error()+": ")}
}

// batch is one evaluation in progress.
type batch struct {
	v         view
	authority artifacts.Authority
	undo      bool

	colls   map[string]*collState
	newColl []*collState
	docs    map[docRef]*docState
	order   []*docState
}

// adopt notes a collection the batch writes that has no row yet: one a write
// creates under the closed default, or the state collection's first write.
func (b *batch) adopt(c *collState) {
	if !c.stored && !slices.Contains(b.newColl, c) {
		b.newColl = append(b.newColl, c)
	}
}

// plan is what an evaluation decided: the documents it changed and the
// collections it created, or the refusal.
type plan struct {
	refusal *refusal
	docs    []*docState
	newColl []*collState

	// The store's counters after the plan, before its history row.
	bodyDelta, entriesDelta, docsDelta, tombstoneDelta int64

	// meteredDelta is how far the plan moves the store's metered document
	// bytes ([meteredDoc]): what the quotas are decided on.
	meteredDelta int64
}

// evaluate decides one batch of ops under authority. A refusal is a plan
// carrying it; an error is the store's.
func evaluate(v view, authority artifacts.Authority, ops []artifacts.DataOp, undo bool) (plan, error) {
	b := &batch{v: v, authority: authority, undo: undo,
		colls: map[string]*collState{}, docs: map[docRef]*docState{}}
	if r := checkAuthority(authority); r != nil {
		return plan{refusal: r}, nil
	}
	if len(ops) == 0 || len(ops) > artifacts.MaxBatchOps {
		return plan{refusal: refuse(artifacts.DataInvalid, "a write carries 1..%d ops and this "+
			"one carries %d", artifacts.MaxBatchOps, len(ops))}, nil
	}
	// GRAMMAR FIRST, for every op: a batch that names something no store can
	// hold is invalid whatever the rows say.
	for i, op := range ops {
		if err := checkOpShape(op); err != nil {
			return plan{refusal: refuse(artifacts.DataInvalid, "op %d: %v", i,
				strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": "))}, nil
		}
	}
	// THE GUARDS, ALL OF THEM, against the documents as the record found
	// them: a conflict names every document that moved, so the writer can
	// merge them all at once.
	var conflicts []docRef
	creators := map[docRef]string{}
	for _, op := range ops {
		if op.IfVersion == nil {
			continue
		}
		c, err := b.collection(op.Collection)
		if err != nil {
			return plan{}, err
		}
		if c == nil {
			if *op.IfVersion != 0 {
				conflicts = appendRef(conflicts, docRef{op.Collection, op.Doc})
			}
			continue
		}
		d, err := b.doc(c, op.Doc)
		if err != nil {
			return plan{}, err
		}
		if d.version0 != *op.IfVersion {
			ref := docRef{op.Collection, op.Doc}
			conflicts = appendRef(conflicts, ref)
			creators[ref] = d.createdBy
		}
	}
	if len(conflicts) > 0 {
		return plan{refusal: &refusal{verdict: artifacts.DataConflict,
			reason: fmt.Sprintf("%d document(s) moved since the version the write named",
				len(conflicts)), conflicts: conflicts, creators: creators}}, nil
	}
	for i, op := range ops {
		r, err := b.apply(op)
		if err != nil {
			return plan{}, err
		}
		if r != nil {
			r.reason = fmt.Sprintf("op %d: %s", i, r.reason)
			return plan{refusal: r}, nil
		}
	}
	return b.finish()
}

// appendRef adds one document to a list once.
func appendRef(refs []docRef, r docRef) []docRef {
	if slices.Contains(refs, r) {
		return refs
	}
	return append(refs, r)
}

// checkAuthority refuses a stamp no API forms: a write is always stamped with
// who made it, their role and the limits in force.
func checkAuthority(a artifacts.Authority) *refusal {
	switch {
	case strings.TrimSpace(a.Actor.Name) == "" || !a.Actor.Kind.Valid():
		return refuse(artifacts.DataInvalid, "the write's authority names no actor")
	case !a.Role.Valid():
		return refuse(artifacts.DataForbidden, "the write's authority carries no role on the artifact")
	case a.Limits.DataStoreBytes <= 0 || a.Limits.DataBytes <= 0:
		return refuse(artifacts.DataInvalid, "the write's authority carries no datastore limits")
	}
	return nil
}

// checkOpShape refuses an op whose names or operands no store holds.
func checkOpShape(op artifacts.DataOp) error {
	if !op.Op.Valid() {
		return fmt.Errorf("%w: op %q is not one of %v", ErrInvalid, op.Op, artifacts.DataOpKinds)
	}
	if op.Collection != StateCollection && !artifacts.ValidCollectionName(op.Collection) {
		return fmt.Errorf("%w: collection %q is a lowercase letter, then lowercase letters, "+
			"digits and `_`, at most %d bytes, and not one of %v", ErrInvalid, op.Collection,
			artifacts.MaxCollectionNameBytes, artifacts.ReservedCollections)
	}
	if !ValidDocID(op.Doc) {
		return fmt.Errorf("%w: document id %q is 1..%d letters, digits, `_` and `-`, and "+
			"not one of %v", ErrInvalid, op.Doc, artifacts.MaxDocIDBytes, reservedDocIDs)
	}
	if op.IfVersion != nil && *op.IfVersion < 0 {
		return fmt.Errorf("%w: if_version is a version a read returned, never negative", ErrInvalid)
	}
	switch op.Op {
	case artifacts.DataSet, artifacts.DataMerge:
		if op.Field != "" || len(op.Value) > 0 {
			return fmt.Errorf("%w: a %s carries fields, never field and value", ErrInvalid, op.Op)
		}
		if op.Op == artifacts.DataMerge && len(op.Fields) == 0 {
			return fmt.Errorf("%w: a merge names at least one field", ErrInvalid)
		}
		for name := range op.Fields {
			if !query.ValidField(name) {
				return fmt.Errorf("%w: field %q is a letter, then letters, digits and `_`, at "+
					"most %d bytes; a leading underscore is the engine's", ErrInvalid, name,
					artifacts.MaxFieldNameBytes)
			}
		}
	case artifacts.DataDelete:
		if len(op.Fields) > 0 || op.Field != "" || len(op.Value) > 0 {
			return fmt.Errorf("%w: a delete names only its document", ErrInvalid)
		}
	default:
		if len(op.Fields) > 0 || !query.ValidField(op.Field) || len(op.Value) == 0 {
			return fmt.Errorf("%w: %s names one field and one value", ErrInvalid, op.Op)
		}
	}
	return nil
}

// collection is the named collection, or nil where the store holds none and a
// write could not create one.
func (b *batch) collection(name string) (*collState, error) {
	if c, ok := b.colls[name]; ok {
		return c, nil
	}
	c, err := b.v.collection(name)
	if err != nil {
		return nil, err
	}
	b.colls[name] = c
	return c, nil
}

// doc is one document, as the batch has left it so far.
func (b *batch) doc(c *collState, id string) (*docState, error) {
	ref := docRef{c.spec.Name, id}
	if d, ok := b.docs[ref]; ok {
		return d, nil
	}
	d, err := b.v.doc(c, id)
	if err != nil {
		return nil, err
	}
	d.coll = c
	b.docs[ref] = d
	return d, nil
}

// apply is one op, against the batch's working state: its refusal, or nil,
// beside the store's error.
func (b *batch) apply(op artifacts.DataOp) (*refusal, error) {
	c, err := b.collection(op.Collection)
	if err != nil {
		return nil, err
	}
	if c == nil {
		// A COLLECTION NOBODY DECLARED is created by the write under the
		// closed default (participants read, editors write, no schema),
		// which is what a page's schemaless `db` writes into.
		c = newCollState(closedDefault(op.Collection), 0, true, false, 0)
		b.colls[op.Collection] = c
	}
	b.adopt(c)
	d, err := b.doc(c, op.Doc)
	if err != nil {
		return nil, err
	}
	if c.spec.Name == StateCollection && !strings.HasPrefix(op.Doc, StateToken(b.authority.Actor.Name)+"_") {
		return refuse(artifacts.DataForbidden, "a state document is its viewer's own, and %s "+
			"is not %s's", op.Doc, b.authority.Actor.Name), nil
	}
	if !d.listed {
		d.listed = true
		b.order = append(b.order, d)
	}
	switch op.Op {
	case artifacts.DataDelete:
		return b.delete(c, d), nil
	case artifacts.DataSet, artifacts.DataMerge:
		return b.setFields(c, d, op), nil
	}
	return b.fieldOp(c, d, op), nil
}

// role is the stamped role.
func (b *batch) role() artifacts.Role { return b.authority.Role }

// editor reports a writer at editor or above.
func (b *batch) editor() bool { return b.role().Includes(artifacts.RoleEditor) }

// owns reports whether the writer created d.
func (b *batch) owns(d *docState) bool {
	return d.createdBy == b.authority.Actor.Name
}

// create makes d live for the writer, under the collection's create role.
func (b *batch) create(c *collState, d *docState) *refusal {
	if !b.role().Includes(c.spec.Create) {
		return refuse(artifacts.DataForbidden, "creating a row in %s needs %s, and the writer is %s",
			c.spec.Name, c.spec.Create, b.role())
	}
	d.live, d.created, d.changed = true, true, true
	d.fields = map[string]canon.Value{}
	// AN UNDO BRINGS BACK THE ROW AS IT WAS, its creator included, so
	// undoing a delete does not hand the row to whoever pressed undo.
	if b.undo && d.row && d.createdBy != "" {
		return nil
	}
	d.takeover = d.row && d.createdBy != "" && d.createdBy != b.authority.Actor.Name &&
		scopedRead(c.spec.Read)
	d.createdBy, d.createdByKind, d.createdAt = b.authority.Actor.Name, string(b.authority.Actor.Kind), 0
	return nil
}

// scopedRead reports a read rule that shows a reader below editor only the
// rows they created, so a row's creator decides who sees it.
func scopedRead(rule artifacts.ReadRule) bool {
	return rule == artifacts.ReadOwnRows || rule == artifacts.ReadPrivate
}

// delete tombstones d.
func (b *batch) delete(c *collState, d *docState) *refusal {
	if !d.live {
		// DELETING WHAT IS NOT THERE CHANGES NOTHING, and is not an
		// error: two people deleting one row at once both succeed.
		return nil
	}
	if !b.role().Includes(c.spec.Delete) {
		return refuse(artifacts.DataForbidden, "deleting a row of %s needs %s, and the writer is %s",
			c.spec.Name, c.spec.Delete, b.role())
	}
	if (c.spec.Read == artifacts.ReadPrivate || !b.editor()) && !b.owns(d) {
		return refuse(artifacts.DataForbidden, "%s may delete only rows they created in %s",
			b.authority.Actor.Name, c.spec.Name)
	}
	for _, col := range c.spec.Columns {
		if _, held := d.fields[col.Key]; held && col.HumansOnly && b.authority.Actor.Kind == iam.ActorAgent {
			return refuse(artifacts.DataForbidden, "the row holds %s, which only a person "+
				"writes, so only a person deletes it", col.Key)
		}
	}
	d.live, d.changed = false, true
	d.fields = nil
	return nil
}

// setFields applies a set (every field replaced) or a merge (the named fields
// set, a null removing one).
func (b *batch) setFields(c *collState, d *docState, op artifacts.DataOp) *refusal {
	creating := !d.live
	if creating {
		if r := b.create(c, d); r != nil {
			return r
		}
	}
	next := map[string]canon.Value{}
	if op.Op == artifacts.DataMerge {
		for k, v := range d.fields {
			next[k] = v
		}
	}
	names := make([]string, 0, len(op.Fields))
	for name := range op.Fields {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		v, err := canon.Parse(op.Fields[name], MaxValueDepth+1)
		if err != nil {
			return refuse(artifacts.DataInvalid, "field %s: %v", name, err)
		}
		if v.Kind == canon.KindNull {
			delete(next, name)
			continue
		}
		next[name] = v
	}
	// EVERY FIELD THE OP NAMES OR REMOVES IS WRITTEN, and each is held to
	// its column's rules: a write the rules refuse is refused even where it
	// would leave the value as it is, so whether a write is admitted never
	// depends on which of two racing writers got there first.
	for _, name := range unionKeys(d.fields, next) {
		old, had := d.fields[name]
		val, has := next[name]
		changed := !had || !has || !sameValue(old, val)
		_, named := op.Fields[name]
		if !changed && !named {
			continue
		}
		if r := b.mayWrite(c, d, name, op, creating, had, has, changed); r != nil {
			return r
		}
		if has && changed {
			if err := b.checkField(c, name, val); err != nil {
				return fromErr(err)
			}
		}
	}
	d.fields, d.changed = next, true
	return nil
}

// fieldOp applies an increment, an append, an add or a remove to one field.
func (b *batch) fieldOp(c *collState, d *docState, op artifacts.DataOp) *refusal {
	if op.Op == artifacts.DataRemove && !d.live {
		// REMOVING FROM A ROW THAT IS NOT THERE CHANGES NOTHING, and creates
		// nothing either.
		return nil
	}
	creating := !d.live
	if creating {
		if r := b.create(c, d); r != nil {
			return r
		}
	}
	operand, err := canon.Parse(op.Value, MaxValueDepth+1)
	if err != nil {
		return refuse(artifacts.DataInvalid, "the value of %s: %v", op.Field, err)
	}
	col, declared := c.columns[op.Field]
	old, had := d.fields[op.Field]
	var next canon.Value
	switch op.Op {
	case artifacts.DataIncrement:
		if declared && col.Type != artifacts.ColumnNumber {
			return refuse(artifacts.DataInvalid, "%s is a %s column, and only a number is incremented",
				op.Field, col.Type)
		}
		if operand.Kind != canon.KindNumber {
			return refuse(artifacts.DataInvalid, "an increment adds a number")
		}
		if _, err := finite(operand.Number); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
			return refuse(artifacts.DataInvalid, "%v", err)
		}
		base := "0"
		if had {
			if old.Kind != canon.KindNumber {
				return refuse(artifacts.DataInvalid, "%s holds a value that is not a number", op.Field)
			}
			base = old.Number
		}
		sum, err := addDecimal(base, operand.Number) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		if err != nil {
			return refuse(artifacts.DataInvalid, "%s: %v", op.Field, err)
		}
		next = canon.Value{Kind: canon.KindNumber, Number: sum}
	case artifacts.DataAppend:
		textual := declared && (col.Type == artifacts.ColumnText || col.Type == artifacts.ColumnLongText)
		listy := declared && col.Type == artifacts.ColumnMultiSelect
		if declared && !textual && !listy {
			return refuse(artifacts.DataInvalid, "%s is a %s column, and append grows text or a list",
				op.Field, col.Type)
		}
		switch {
		case textual || (!declared && operand.Kind == canon.KindString &&
			(!had || old.Kind == canon.KindString)):
			if operand.Kind != canon.KindString {
				return refuse(artifacts.DataInvalid, "appending to text appends text")
			}
			base := ""
			if had {
				if old.Kind != canon.KindString {
					return refuse(artifacts.DataInvalid, "%s holds a value that is not text", op.Field)
				}
				base = old.String
			}
			next = canon.Value{Kind: canon.KindString, String: base + operand.String}
		default:
			var r *refusal
			if next, r = withElement(op.Field, old, had, operand, listy); r != nil {
				return r
			}
		}
	case artifacts.DataAdd, artifacts.DataRemove:
		if declared && col.Type != artifacts.ColumnMultiSelect {
			return refuse(artifacts.DataInvalid, "%s is a %s column, and only a list gains or "+
				"loses an element", op.Field, col.Type)
		}
		if op.Op == artifacts.DataAdd {
			var r *refusal
			if next, r = withElement(op.Field, old, had, operand, true); r != nil {
				return r
			}
			break
		}
		if !had {
			return nil
		}
		if old.Kind != canon.KindArray {
			return refuse(artifacts.DataInvalid, "%s holds a value that is not a list", op.Field)
		}
		next = canon.Value{Kind: canon.KindArray, Array: []canon.Value{}}
		for _, e := range old.Array {
			if !sameValue(e, operand) {
				next.Array = append(next.Array, e)
			}
		}
	}
	changed := !had || !sameValue(old, next)
	if r := b.mayWrite(c, d, op.Field, op, creating, had, true, changed); r != nil {
		return r
	}
	if !changed {
		return nil
	}
	if err := b.checkField(c, op.Field, next); err != nil {
		return fromErr(err)
	}
	if d.fields == nil {
		d.fields = map[string]canon.Value{}
	}
	d.fields[op.Field] = next
	d.changed = true
	return nil
}

// withElement is a list with one element added. unique says the list is a set
// (a multiselect, or an add), where an element already held changes nothing.
func withElement(field string, old canon.Value, had bool, elem canon.Value, unique bool) (canon.Value, *refusal) {
	if elem.Kind == canon.KindArray || elem.Kind == canon.KindObject {
		return canon.Value{}, refuse(artifacts.DataInvalid, "a list element added to %s is a scalar",
			field)
	}
	out := canon.Value{Kind: canon.KindArray, Array: []canon.Value{}}
	if had {
		if old.Kind != canon.KindArray {
			return canon.Value{}, refuse(artifacts.DataInvalid, "%s holds a value that is not a list",
				field)
		}
		out.Array = append(out.Array, old.Array...)
	}
	if unique && slices.ContainsFunc(out.Array, func(e canon.Value) bool { return sameValue(e, elem) }) {
		return out, nil
	}
	out.Array = append(out.Array, elem)
	return out, nil
}

// mayWrite holds one field write to the collection's and the column's rules.
// had and has say whether the field held a value before and holds one after,
// and changed whether the write moves it: who may write a field is asked of
// every write naming it, and what a value may become only of one that moves
// it.
func (b *batch) mayWrite(c *collState, d *docState, field string, op artifacts.DataOp,
	creating, had, has, changed bool) *refusal {

	col, declared := c.columns[field]
	if !c.schemaless() && !declared {
		return refuse(artifacts.DataInvalid, "%s declares no field %q; a typo is refused rather "+
			"than dropped", c.spec.Name, field)
	}
	writeRole := c.spec.Create
	explicit := declared && col.Write != ""
	if explicit {
		writeRole = col.Write
	}
	actor := b.authority.Actor
	switch {
	case !b.role().Includes(writeRole):
		return refuse(artifacts.DataForbidden, "writing %s needs %s, and the writer is %s",
			field, writeRole, b.role())
	case declared && col.HumansOnly && actor.Kind == iam.ActorAgent:
		return refuse(artifacts.DataForbidden, "%s is written by a person only, never an agent", field)
	case c.spec.Read == artifacts.ReadPrivate && !creating && !b.owns(d):
		return refuse(artifacts.DataForbidden, "%s is private to whoever wrote each row", c.spec.Name)
	}
	if !creating && !b.editor() {
		// BELOW EDITOR, A ROW IS THE WRITER'S OWN unless the column names a
		// write role, whose condition then decides.
		switch {
		case !explicit && !b.owns(d):
			return refuse(artifacts.DataForbidden, "%s may change only rows they created",
				actor.Name)
		case explicit && col.When == artifacts.WhenOwnRow && !b.owns(d):
			return refuse(artifacts.DataForbidden, "%s is written only on the writer's own rows", field)
		case explicit && col.When == artifacts.WhenAssignedToViewer:
			assigned, ok := d.fields[col.AssignedField]
			if !ok || assigned.Kind != canon.KindString || assigned.String != actor.Name {
				return refuse(artifacts.DataForbidden, "%s is written only on rows assigned to "+
					"the writer in %s", field, col.AssignedField)
			}
		}
	}
	if !changed {
		return nil
	}
	if declared && !creating && col.Immutable && had {
		return refuse(artifacts.DataForbidden, "%s is set when a row is created and never changed", field)
	}
	if declared && !creating && col.Mode == artifacts.FieldAppend && had {
		if !has || (op.Op != artifacts.DataAppend && op.Op != artifacts.DataAdd) {
			return refuse(artifacts.DataForbidden, "%s only grows: append to it, never replace "+
				"or remove what it holds", field)
		}
	}
	// LONG TEXT IS WRITTEN GUARDED (D18): replacing a long-text value on a
	// row the batch did not create names the version the writer edited,
	// so a concurrent edit is a conflict the writer is told about rather
	// than a paragraph silently lost. Appending needs no guard: it never
	// loses anything.
	if declared && col.Type == artifacts.ColumnLongText && d.live0 && !d.created &&
		(op.Op == artifacts.DataSet || op.Op == artifacts.DataMerge) && op.IfVersion == nil {
		return refuse(artifacts.DataInvalid, "%s is long text, written whole only guarded: name "+
			"the if_version you edited, or append", field)
	}
	return nil
}

// checkField holds one value to its column, or to the schemaless shape.
func (b *batch) checkField(c *collState, field string, v canon.Value) error {
	if col, declared := c.columns[field]; declared {
		return checkValue(col, v)
	}
	return checkLoose(field, v, 1)
}

// finish checks every changed document whole, the collection and store caps
// and the stamped limits, and returns the plan.
func (b *batch) finish() (plan, error) {
	p := plan{newColl: b.newColl}
	totals := b.v.totals()
	if totals.collections+int64(len(b.newColl)) > artifacts.MaxCollections {
		return plan{refusal: refuse(artifacts.DataQuota, "the store would hold %d collections "+
			"and the maximum is %d", totals.collections+int64(len(b.newColl)),
			artifacts.MaxCollections)}, nil
	}
	// net is each collection's live rows gained per creator: a row a
	// participant deletes in the same batch makes room for one they add.
	net := map[*collState]map[string]int64{}
	count := func(c *collState, who string, n int64) {
		if net[c] == nil {
			net[c] = map[string]int64{}
		}
		net[c][who] += n
	}
	collDocs := map[*collState]int64{}
	for _, d := range b.order {
		if !d.changed && !d.created {
			continue
		}
		if d.live {
			if r := checkWhole(d); r != nil {
				return plan{refusal: r}, nil
			}
		}
		if sameDocument(d) {
			continue
		}
		p.docs = append(p.docs, d)
		p.bodyDelta += int64(len(d.body)) - int64(len(d.body0))
		p.entriesDelta += int64(len(d.entries)) - int64(d.entries0)
		p.meteredDelta += meteredDoc(d, b.authority.Actor.Name, string(b.authority.Actor.Kind)) -
			d.metered0
		switch {
		case d.live && !d.live0:
			p.docsDelta++
			collDocs[d.coll]++
			if d.row {
				p.tombstoneDelta--
			}
			count(d.coll, d.createdBy, 1)
		case !d.live && d.live0:
			p.docsDelta--
			p.tombstoneDelta++
			collDocs[d.coll]--
			count(d.coll, d.createdBy, -1)
		}
	}
	if totals.docs+p.docsDelta > artifacts.MaxDocsPerStore && p.docsDelta > 0 {
		return plan{refusal: refuse(artifacts.DataQuota, "the store would hold %d documents and "+
			"the maximum is %d", totals.docs+p.docsDelta, artifacts.MaxDocsPerStore)}, nil
	}
	colls := make([]*collState, 0, len(collDocs))
	for c := range collDocs {
		colls = append(colls, c)
	}
	slices.SortFunc(colls, func(x, y *collState) int { return strings.Compare(x.spec.Name, y.spec.Name) })
	for _, c := range colls {
		if c.spec.MaxRows > 0 && collDocs[c] > 0 && c.docs+collDocs[c] > int64(c.spec.MaxRows) {
			return plan{refusal: refuse(artifacts.DataQuota, "%s would hold %d rows and its "+
				"maximum is %d", c.spec.Name, c.docs+collDocs[c], c.spec.MaxRows)}, nil
		}
		creators := make([]string, 0, len(net[c]))
		for who, n := range net[c] {
			if n > 0 {
				creators = append(creators, who)
			}
		}
		slices.Sort(creators)
		for _, who := range creators {
			if c.spec.MaxRowsPerPrincipal == 0 && !c.spec.OncePerPrincipal {
				break
			}
			held, err := b.v.ownedLive(c, who)
			if err != nil {
				return plan{}, err
			}
			total := held + net[c][who]
			if c.spec.OncePerPrincipal && total > 1 {
				return plan{refusal: refuse(artifacts.DataForbidden, "%s keeps one row per "+
					"participant, and %s already has one", c.spec.Name, who)}, nil
			}
			if c.spec.MaxRowsPerPrincipal > 0 && total > int64(c.spec.MaxRowsPerPrincipal) {
				return plan{refusal: refuse(artifacts.DataQuota, "%s keeps at most %d rows per "+
					"participant, and %s would hold %d", c.spec.Name, c.spec.MaxRowsPerPrincipal,
					who, total)}, nil
			}
		}
	}
	// THE STAMPED LIMITS, counted in the bytes the rows cost on disk
	// (meter.go). Only a write that GROWS the data is refused: one that
	// shrinks it always lands, so a full store can always be emptied.
	if grow := p.meteredDelta; grow > 0 {
		limits := b.authority.Limits
		after := totals.footprint() + grow
		if after > limits.DataStoreBytes {
			return plan{refusal: refuse(artifacts.DataQuota, "the store's footprint would be "+
				"%d bytes and artifacts.limits.data_store_bytes is %d; delete rows or ask the "+
				"operator to raise it", after, limits.DataStoreBytes)}, nil
		}
		if company := totals.companyFootprint + grow; company > limits.DataBytes {
			return plan{refusal: refuse(artifacts.DataQuota, "the company's datastores would "+
				"hold %d bytes and artifacts.limits.data_bytes is %d; delete rows or ask the "+
				"operator to raise it", company, limits.DataBytes)}, nil
		}
	}
	return p, nil
}

// checkWhole checks one live document whole: required columns, its canonical
// size and its field-index entries. It fills d.body and d.entries.
func checkWhole(d *docState) *refusal {
	c := d.coll
	for _, col := range c.spec.Columns {
		if _, held := d.fields[col.Key]; !held && col.Required && len(col.Default) == 0 {
			return refuse(artifacts.DataInvalid, "%s requires %s", c.spec.Name, col.Key)
		}
	}
	d.body = encodeFields(d.fields)
	if int64(len(d.body)) > artifacts.MaxDocBytes {
		return refuse(artifacts.DataInvalid, "document %s is %d bytes and the maximum is %d",
			d.id, len(d.body), artifacts.MaxDocBytes)
	}
	d.entries = explode(d.fields, c.spec.Indexed)
	if len(d.entries) > artifacts.MaxIndexedPaths {
		return refuse(artifacts.DataInvalid, "document %s indexes %d values and the maximum "+
			"is %d", d.id, len(d.entries), artifacts.MaxIndexedPaths)
	}
	return nil
}

// sameDocument reports a document the batch left exactly as it found it.
func sameDocument(d *docState) bool {
	if d.live != d.live0 {
		return false
	}
	if !d.live {
		return true
	}
	return string(d.body) == string(d.body0)
}

// encodeFields is a body's canonical bytes.
func encodeFields(fields map[string]canon.Value) []byte {
	members := make([]canon.Member, 0, len(fields))
	for k, v := range fields {
		members = append(members, canon.Member{Key: k, Value: v})
	}
	slices.SortFunc(members, func(a, b canon.Member) int { return strings.Compare(a.Key, b.Key) })
	return canon.Encode(canon.Value{Kind: canon.KindObject, Members: members})
}

// explode is a body's field-index entries: every top-level scalar at ord
// zero, every scalar element of a top-level array at ord one and up, a
// presence marker for a top-level field holding no scalar at all, and every
// declared dotted path the body holds, in path order.
func explode(fields map[string]canon.Value, indexed []string) []fieldEntry {
	var out []fieldEntry
	add := func(path string, v canon.Value) int {
		before := len(out)
		switch v.Kind {
		case canon.KindArray:
			for i, e := range v.Array {
				if e.Kind != canon.KindArray && e.Kind != canon.KindObject {
					out = append(out, entryOf(path, i+1, e))
				}
			}
		case canon.KindObject:
		default:
			out = append(out, entryOf(path, 0, v))
		}
		return len(out) - before
	}
	for _, name := range sortedKeys(fields) {
		if add(name, fields[name]) == 0 {
			out = append(out, fieldEntry{path: name, ord: presenceOrd, kind: presenceKind})
		}
	}
	for _, p := range slices.Sorted(slices.Values(indexed)) {
		head, tail, _ := strings.Cut(p, ".")
		parent, ok := fields[head]
		if !ok || parent.Kind != canon.KindObject {
			continue
		}
		for _, m := range parent.Members {
			if m.Key == tail {
				add(p, m.Value)
			}
		}
	}
	return out
}

// entryOf is one scalar's index entry.
func entryOf(path string, ord int, v canon.Value) fieldEntry {
	e := fieldEntry{path: path, ord: ord}
	switch v.Kind {
	case canon.KindString:
		e.kind, e.txt = "s", query.TruncateText(v.String)
	case canon.KindNumber:
		e.kind = "n"
		e.num, _ = strconv.ParseFloat(v.Number, 64)
	case canon.KindBool:
		e.kind = "b"
		if v.Bool {
			e.num = 1
		}
	default:
		e.kind = "z"
	}
	return e
}

// sameValue reports two values with one canonical encoding.
func sameValue(a, b canon.Value) bool {
	return string(canon.Encode(a)) == string(canon.Encode(b))
}

// unionKeys is every key of either map, sorted.
func unionKeys(a, b map[string]canon.Value) []string {
	out := make([]string, 0, len(a)+len(b))
	for k := range a {
		out = append(out, k)
	}
	for k := range b {
		if _, dup := a[k]; !dup {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// sortedKeys is a map's keys, sorted.
func sortedKeys(m map[string]canon.Value) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

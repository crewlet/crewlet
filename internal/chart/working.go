package chart

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// THE REPLAY: the chart as a batch's decide sees it, operation by operation.
//
// A COPY THAT IS DISCARDED. What gets published is the record; the applier is
// what writes rows. This exists so each operation is checked against the state
// the ones before it produced, which is the only reading under which a batch
// that creates a unit and then places a seat in it is valid — and so what the
// record carries is the state the WHOLE batch produced, per object, rather than
// whatever each operation happened to name.
//
// # One node per object, and why not maps keyed by address
//
// The copy used to be maps keyed by an object's address, and an edge was
// assembled from the operation that touched it — which is how a move published
// an empty lead (the operation named none) and took the unit's authored lead
// away. A NODE holds one object's whole structural state; the batch edits the
// node, and each edge is read off the node at the end. An edge is FULL
// POST-STATE, so it has to come from the one place the post-state is.

// working is the replay's state.
type working struct {
	// live is every object the chart holds as of the operations so far,
	// keyed by its address.
	live map[string]*wnode

	// removedKeys is every address a removal tombstoned — on the log, or
	// earlier in this batch — so a later operation on one is refused rather
	// than applied against a row the applier will delete.
	removedKeys map[string]bool

	// identities maps every address that is a RENAMED object's identity —
	// the key it was created under — to that object, so a creation onto one
	// is refused (see [refuseCreate]).
	identities map[string]*wnode

	// aliases maps every retired address to the object still answering to
	// it.
	aliases map[string]*wnode

	// touched is every object an operation named, in the order it was first
	// named, which is the order the record states their edges in.
	touched []*wnode
}

// wnode is one object in the replay.
type wnode struct {
	kind ObjectKind

	// key is the address the object answers to as of the operations so far.
	key string

	// parent is the unit key it sits under, "" the org root.
	parent string

	// lead is a unit's AUTHORED lead, "" where it inherits one.
	lead string

	// seatKind is what holds a seat. Empty for a unit.
	seatKind SeatKind

	// manages is a seat's authored `manages:` list, folded and sorted as
	// the apply stores it. Empty for a unit, and for a seat that manages
	// nobody.
	manages []string

	// origin is the address the object was CREATED under — its identity,
	// which no rename moves.
	origin string

	// op is the highest-ranked of what this batch did to the object's place
	// in the tree ([opRank]), or "" while nothing has. A rename is not among
	// them: it is recorded in `from`.
	op OperationKind

	// from is the address the object answered to when the batch began, set
	// by its first rename and "" while nothing has renamed it.
	from string

	// named marks an object an operation in this batch named, so its edge
	// is considered for the record.
	named bool

	// removed marks an object an operation in this batch took out.
	removed bool
}

// opRank orders what a batch can do to one object's place in the tree, for the
// one verb its edge is published under.
//
// ONE VERB PER EDGE, because the record states one edge per object — a second
// edge for the same object would leave the applied result dependent on which
// the applier wrote last — and the edge carries the object's whole post-state
// whatever the verb. The verb says what KIND of change it was, and the highest
// rank is the one that decides what the apply does: a create must be applied as
// a create even when the batch also moved the object afterwards, because only
// a create is declined when its address turns out to be held.
//
// A RENAME OUTRANKS A MOVE AND A LEAD CHANGE and never meets a create (a batch
// may not rename what it creates — [RuleRenameCreated]), so it is not ranked
// here: an object whose `from` differs from its key is published as a rename
// whatever else the batch did to it ([working.edges]).
var opRank = map[OperationKind]int{
	OpSetLead:    1,
	OpSetKind:    1,
	OpSetManages: 1,
	OpMove:       2,
	OpCreateUnit: 3,
	OpCreateSeat: 3,
}

// touch records that an operation named this object.
func (w *working) touch(n *wnode) {
	if !n.named {
		n.named = true
		w.touched = append(w.touched, n)
	}
}

// did records that op changed this object's place in the tree, and raises its
// verb.
func (w *working) did(n *wnode, op OperationKind) {
	w.touch(n)
	if opRank[op] > opRank[n.op] {
		n.op = op
	}
}

// seat is the replay's copy of the object ref names, as it stands before the
// next operation, and whether the replay holds it.
func (w *working) seat(ref ObjectRef) (wnode, bool) {
	n, held := w.live[refKey(ref)]
	if !held {
		return wnode{}, false
	}
	return *n, true
}

// created reports whether an earlier operation in this batch created n.
func (n *wnode) created() bool {
	return n.op == OpCreateUnit || n.op == OpCreateSeat
}

// found is the address the rows hold n at: the one it answered to when the
// batch began, whatever this batch has renamed it to since.
//
// IT IS WHAT A REMOVAL NAMES THE OBJECT BY. A removal is a record of its own,
// published without the batch's placements ([Writer.WriteBatch]), so it
// applies against the rows as the batch found them — where a rename earlier in
// the batch never happened. Named by the address the rename moved it onto, the
// removal tombstoned an address that had never held anything, deleted nothing,
// and left the object it was meant to remove running under its old one.
func (n *wnode) found() string {
	return cmp.Or(n.from, n.key)
}

// refKey composes an object reference into the replay's own key.
//
// THE KIND IS PART OF IT, because a unit key and a seat handle are different
// namespaces: `NormalizeKey` folds both and neither reserves a prefix, so a
// company may legitimately hold a unit and a seat that answer to one spelling.
func refKey(ref ObjectRef) string {
	return string(ref.Kind) + "/" + NormalizeKey(ref.ID)
}

// apply checks one operation against the replay and advances it.
func (w *working) apply(ctx context.Context, index int, op Operation) *RefusalError {
	refuse := func(rule, detail string, args ...any) *RefusalError {
		return &RefusalError{Index: index, Operation: op, Rule: rule,
			Detail: fmt.Sprintf(detail, args...)}
	}
	if !op.Kind.Valid() {
		return refuse(RuleUnknownKind, "%q is not one of %v", op.Kind, OperationKinds)
	}
	id := NormalizeKey(op.Object.ID)
	if id == "" {
		return refuse(RuleBadKey, "an operation names no object")
	}
	if op.Object.Kind != KindUnit && op.Object.Kind != KindSeat {
		return refuse(RuleUnknownKind, "%s is not an object in the chart — "+
			"only a unit and a seat have a place in the tree", op.Object.Kind)
	}
	if refused := unusedField(refuse, op); refused != nil {
		return refused
	}
	ref := ObjectRef{Kind: op.Object.Kind, ID: id}

	switch op.Kind {
	case OpCreateUnit, OpCreateSeat:
		if want := createFor(op.Object.Kind); op.Kind != want {
			return refuse(RuleUnknownKind, "%s creates a %s, and this operation "+
				"names a %s — use %s", op.Kind, kindOfCreate(op.Kind),
				op.Object.Kind, want)
		}
		if refused := w.checkCreate(ctx, refuse, ref); refused != nil {
			return refused
		}
		parent := NormalizeKey(op.Parent)
		if refused := w.checkParent(refuse, parent); refused != nil {
			return refused
		}
		n := &wnode{kind: ref.Kind, key: id, origin: id, parent: parent}
		switch ref.Kind {
		case KindUnit:
			// A UNIT MAY BE CREATED LED, which is one gesture — "add the
			// platform team, led by the SRE" — and the lead a set_lead
			// would otherwise have to state a second time.
			n.lead = NormalizeKey(op.Lead)
		case KindSeat:
			// A SEAT IS CREATED KNOWING WHAT HOLDS IT. Its content comes
			// in a second record on another subject, and a seat whose kind
			// waited for that record was, between the two, an agent with
			// no model chain — which a node would claim and give a mailbox.
			if refused := checkSeatKind(refuse, op); refused != nil {
				return refused
			}
			n.seatKind = op.SeatKind
		}
		w.live[refKey(ref)] = n
		w.did(n, op.Kind)
		return nil

	case OpMove:
		n, refused := w.checkPresent(refuse, ref)
		if refused != nil {
			return refused
		}
		parent := NormalizeKey(op.Parent)
		if refused := w.checkParent(refuse, parent); refused != nil {
			return refused
		}
		if refused := w.checkCycle(refuse, n, parent); refused != nil {
			return refused
		}
		n.parent = parent
		w.did(n, op.Kind)
		return nil

	case OpSetLead:
		if ref.Kind != KindUnit {
			return refuse(RuleUnknownKind, "only a unit has a lead")
		}
		n, refused := w.checkPresent(refuse, ref)
		if refused != nil {
			return refused
		}
		n.lead = NormalizeKey(op.Lead)
		w.did(n, op.Kind)
		return nil

	case OpSetKind:
		if ref.Kind != KindSeat {
			return refuse(RuleUnknownKind, "only a seat has a kind — a unit is "+
				"held by nobody")
		}
		n, refused := w.checkPresent(refuse, ref)
		if refused != nil {
			return refused
		}
		if refused := checkSeatKind(refuse, op); refused != nil {
			return refused
		}
		n.seatKind = op.SeatKind
		w.did(n, op.Kind)
		return nil

	case OpSetManages:
		if ref.Kind != KindSeat {
			return refuse(RuleUnknownKind, "only a seat manages anybody — a "+
				"unit's authority over its members is its lead's")
		}
		n, refused := w.checkPresent(refuse, ref)
		if refused != nil {
			return refused
		}
		list, bad := manageList(op.Manages)
		if bad != nil {
			return refuse(bad.rule, "%s", bad.detail)
		}
		n.manages = list
		w.did(n, op.Kind)
		return nil

	case OpRename:
		n, refused := w.checkPresent(refuse, ref)
		if refused != nil {
			return refused
		}
		if n.created() {
			return refuse(RuleRenameCreated, "%q is created by an earlier "+
				"operation in this batch, and the address an object is created "+
				"under is its identity for ever — create it under the address "+
				"it should answer to", n.key)
		}
		to := NormalizeKey(op.To)
		if to == "" {
			return refuse(RuleBadKey, "a rename names no new address: state `to`")
		}
		if to == n.key {
			return refuse(RuleRenameUnchanged, "%q is the address this %s "+
				"already answers to, so the rename would move nothing", to, n.kind)
		}
		// [refuseCreate]'s rules, with the object as SELF: a rename may take
		// back an address it used to answer to, and never another object's
		// alias or identity. See the function for why the two differ.
		if refused, _ := refuseCreate(ctx, w, n.kind, to, n.key); refused != nil {
			return refuse(refused.Rule, "%q cannot take the address %q — %s",
				n.key, to, refused.Detail)
		}
		w.moveManages(ctx, n, to)
		w.rename(n, to)
		w.touch(n)
		return nil

	case OpRemoveObject:
		n, refused := w.checkPresent(refuse, ref)
		if refused != nil {
			return refused
		}
		if ref.Kind == KindUnit {
			if held := w.holders(id); len(held) > 0 {
				return refuse(RuleUnitNotEmpty, "%s still holds %s — an "+
					"orphaned subtree is reachable from nothing and "+
					"removable by nothing, so its contents move or go first",
					id, strings.Join(held, ", "))
			}
		}
		delete(w.live, refKey(ref))
		// THE ADDRESS THE ROWS HOLD IT AT, which is the one the removal
		// record names and the apply tombstones ([wnode.found]): an
		// address this batch renamed it onto was never the object's on
		// the log, and the replay says so exactly as the log will.
		w.removedKeys[refKey(ObjectRef{Kind: ref.Kind, ID: n.found()})] = true
		// AND ITS IDENTITY, which the apply tombstones beside the address
		// it held ([Applier.tombstoneIdentity]) — so the replay refuses a
		// later creation onto it exactly as the log will.
		for key, holder := range w.identities {
			if holder == n {
				w.removedKeys[key] = true
			}
		}
		n.removed = true
		return nil
	}
	return refuse(RuleUnknownKind, "%q is not one of %v", op.Kind, OperationKinds)
}

// unusedField refuses an operation carrying a field its kind does not take.
//
// A FIELD NOTHING READS IS A REFUSAL, not a silence. A parent stated on a
// set_lead or a lead stated on a move was dropped, so the batch answered as
// though it asked for less than it said — a caller who believed they had moved
// a unit while naming its lead was told it landed, and it had not moved.
//
// An empty value is not a field supplied: the org root is the empty parent and
// "clear the lead" is the empty lead, so neither can be told from an omitted
// one, and neither is refused.
func unusedField(refuse refuseFunc, op Operation) *RefusalError {
	takes := map[OperationKind][]string{
		OpCreateUnit:   {"parent", "lead"},
		OpCreateSeat:   {"parent", "seat_kind"},
		OpMove:         {"parent"},
		OpSetLead:      {"lead"},
		OpSetKind:      {"seat_kind"},
		OpSetManages:   {"manages"},
		OpRename:       {"to"},
		OpRemoveObject: nil,
	}[op.Kind]
	// A LIST IS SUPPLIED WHEN IT HOLDS ANYTHING, for the empty value's
	// reason: a set_manages clearing a seat's list sends none.
	manages := ""
	if len(op.Manages) > 0 {
		manages = "stated"
	}
	for field, value := range map[string]string{
		"parent": op.Parent, "lead": op.Lead, "to": op.To,
		"seat_kind": string(op.SeatKind), "manages": manages} {
		if value != "" && !slices.Contains(takes, field) {
			return refuse(RuleUnusedField, "%s takes %s and no %q — nothing "+
				"would read it, and a batch that dropped it would answer as "+
				"though it asked for less than it said", op.Kind,
				fieldList(takes), field)
		}
	}
	return nil
}

// checkSeatKind refuses a create_seat or a set_kind whose seat kind this build
// does not serve — the empty one included, because a default would be `agent`,
// which is the one kind that runs.
func checkSeatKind(refuse refuseFunc, op Operation) *RefusalError {
	if op.SeatKind.Valid() {
		return nil
	}
	if op.SeatKind == "" {
		return refuse(RuleSeatKind, "%s states what holds the seat: "+
			"`seat_kind` is %q or %q", op.Kind, SeatAgent, SeatHuman)
	}
	return refuse(RuleSeatKind, "%q is not a seat kind this build serves — "+
		"want %q or %q", op.SeatKind, SeatAgent, SeatHuman)
}

// fieldList renders the fields an operation takes, for a refusal.
func fieldList(fields []string) string {
	if len(fields) == 0 {
		return "no field beside its object"
	}
	return "`" + strings.Join(fields, "` and `") + "`"
}

// createFor is the create operation for an object of kind.
func createFor(kind ObjectKind) OperationKind {
	if kind == KindUnit {
		return OpCreateUnit
	}
	return OpCreateSeat
}

// kindOfCreate is the object kind a create operation makes.
func kindOfCreate(op OperationKind) ObjectKind {
	if op == OpCreateUnit {
		return KindUnit
	}
	return KindSeat
}

type refuseFunc func(rule, detail string, args ...any) *RefusalError

// checkCreate refuses a create onto an address it may not take.
//
// THE RULES ARE [refuseCreate]'s, asked of this replay — so the batch refuses
// exactly what the apply declines for an import and what a rename is refused,
// each against the state it can see. What the replay adds is the batch's own
// earlier operations: an address removed two operations ago is removed here,
// and one created two operations ago is taken.
//
// A UNIT AND A SEAT MAY SHARE A SPELLING. They are different namespaces and
// neither reserves a prefix, so the replay is asked about the operation's own
// kind alone — stated so the next reader does not "fix" it into a cross-kind
// check that would refuse a legal chart.
func (w *working) checkCreate(ctx context.Context, refuse refuseFunc,
	ref ObjectRef) *RefusalError {

	// THE REPLAY ANSWERS FROM MEMORY, so the book never fails and the
	// only error [refuseCreate] could return is one it has no source for.
	refused, _ := refuseCreate(ctx, w, ref.Kind, ref.ID, "")
	if refused != nil {
		return refuse(refused.Rule, "%s", refused.Detail)
	}
	return nil
}

// removed is [addressBook]'s tombstone question, answered by the replay: a
// removal already on the log, or one earlier in this batch.
func (w *working) removed(_ context.Context, kind ObjectKind, key string) (bool, error) {
	return w.removedKeys[refKey(ObjectRef{Kind: kind, ID: key})], nil
}

// holder is [addressBook]'s who-answers question, answered by the replay in
// the order the rows answer it: a live key, then an identity, then an alias.
func (w *working) holder(_ context.Context, kind ObjectKind, key string) (
	string, holding, error) {

	ref := refKey(ObjectRef{Kind: kind, ID: key})
	if n, live := w.live[ref]; live {
		return n.key, heldAsKey, nil
	}
	if n, held := w.identities[ref]; held && !n.removed {
		return n.key, heldAsIdentity, nil
	}
	if n, held := w.aliases[ref]; held && !n.removed {
		return n.key, heldAsAlias, nil
	}
	return "", heldByNothing, nil
}

// rename moves n onto to, and advances every reference the apply's cascade will
// move with it ([Applier.moveUnitReferences], [Applier.rekeySeat]) — so an
// operation later in the batch, and the cycle walk, see the chart the rename
// leaves rather than the one it found.
func (w *working) rename(n *wnode, to string) {
	old := n.key
	delete(w.live, refKey(ObjectRef{Kind: n.kind, ID: old}))
	// THE ADDRESS IT LEAVES GOES ON RESOLVING TO IT, as a retired alias —
	// and the one it was created under as its identity, which is how the
	// apply's rows will answer from here on.
	w.aliases[refKey(ObjectRef{Kind: n.kind, ID: old})] = n
	w.identities[refKey(ObjectRef{Kind: n.kind, ID: n.origin})] = n
	if n.from == "" {
		n.from = old
	}
	n.key = to
	w.live[refKey(ObjectRef{Kind: n.kind, ID: to})] = n
	for _, other := range w.live {
		switch {
		case n.kind == KindUnit && other.parent == old:
			other.parent = to
		case n.kind == KindSeat && other.kind == KindUnit && other.lead == old:
			other.lead = to
		}
	}
}

// moveManages moves, in every seat's list the replay holds, the entries that
// reach n onto to — the address n's rename is about to land on — exactly as the
// apply's cascade will move the stored ones ([Applier.moveManages]).
//
// ASKED BEFORE THE REPLAY MOVES n, because which object an entry reaches is a
// question about the chart as the rename found it, as the apply asks it of the
// rows before its rekey ([managesNaming]).
//
// WHY THE REPLAY HAS TO: a seat's edge states its whole list ([Edge.Manages])
// and the apply writes it after the cascade, so a list the replay left on the
// old address would be written back over the one the cascade moved — the
// very reversion version 3 exists to end, reached from inside one batch. An
// entry already naming to is the rename coming back onto an address the entry
// already names, and stays.
//
// THE REPLAY ANSWERS FROM MEMORY, so the book never fails.
func (w *working) moveManages(ctx context.Context, n *wnode, to string) {
	if follow, _ := managesFollow(ctx, w, n.kind, to); !follow {
		return
	}
	for _, seat := range w.live {
		if seat.kind != KindSeat || len(seat.manages) == 0 {
			continue
		}
		moved := false
		next := make([]string, 0, len(seat.manages))
		for _, entry := range seat.manages {
			if entry != to {
				if reaches, _ := managesResolves(ctx, w, n.kind, entry, n.key); reaches {
					entry, moved = to, true
				}
			}
			next = append(next, entry)
		}
		if moved {
			seat.manages = sortedKeys(next)
		}
	}
}

// checkPresent refuses an operation on an object the chart does not hold.
//
// AN ADDRESS THE OBJECT USED TO ANSWER TO IS NOT ITS NAME HERE. A batch names
// each object by the address it has at that point — an earlier operation's
// rename included — because a structural record states current addresses and
// nothing else, and resolving a retired one would make "which object" depend on
// how many renames ago the caller last looked.
func (w *working) checkPresent(refuse refuseFunc, ref ObjectRef) (*wnode, *RefusalError) {
	key := refKey(ref)
	if w.removedKeys[key] {
		return nil, refuse(RuleNoSuchObject, "%q was removed", ref.ID)
	}
	n, held := w.live[key]
	if !held {
		if renamed, alias := w.aliases[key]; alias && !renamed.removed {
			return nil, refuse(RuleNoSuchObject, "%q is a former address of %s "+
				"%q — name the object by the address it answers to now",
				ref.ID, renamed.kind, renamed.key)
		}
		return nil, refuse(RuleNoSuchObject, "%q is not in the chart", ref.ID)
	}
	return n, nil
}

// checkParent refuses a placement under a unit that is not there.
//
// THE ORG ROOT IS A REAL PARENT and is never checked: an empty parent is where
// a top-level department and an org-wide seat both sit.
func (w *working) checkParent(refuse refuseFunc, parent string) *RefusalError {
	if parent == "" {
		return nil
	}
	key := refKey(ObjectRef{Kind: KindUnit, ID: parent})
	if w.removedKeys[key] {
		return refuse(RuleNoSuchParent, "%q was removed", parent)
	}
	if _, held := w.live[key]; !held {
		if renamed, alias := w.aliases[key]; alias && !renamed.removed {
			return refuse(RuleNoSuchParent, "%q is a former address of unit "+
				"%q — name the parent by the address it answers to now",
				parent, renamed.key)
		}
		return refuse(RuleNoSuchParent, "%q is not a unit in the chart — a "+
			"parent may be created by an earlier operation in this batch, and "+
			"nothing in this one creates it", parent)
	}
	return nil
}

// checkCycle refuses a move that would make an object its own ancestor.
//
// THE WALK IS OVER THE REPLAY, which is the whole reason this validation is a
// replay: two moves that are each locally valid can jointly close a cycle, and
// neither writer's own subject would ever have shown it to them. Walking the
// stored rows instead would accept exactly that pair.
//
// A SEAT CANNOT CLOSE A CYCLE — nothing sits under a seat — so the walk runs
// for a unit alone. It is still called for both, because "this kind cannot" is
// a fact worth stating once here rather than in every caller.
func (w *working) checkCycle(refuse refuseFunc, n *wnode, parent string) *RefusalError {
	if n.kind != KindUnit || parent == "" {
		return nil
	}
	// WALK UP FROM THE NEW PARENT. If the object being moved is on that
	// path, the move puts the object under itself.
	seen := map[*wnode]bool{}
	at := w.live[refKey(ObjectRef{Kind: KindUnit, ID: parent})]
	for at != nil {
		if at == n {
			return refuse(RuleCycle, "moving %q under %q would put it under "+
				"itself — %q is already somewhere beneath it", n.key, parent, parent)
		}
		if seen[at] {
			// A CYCLE ALREADY IN THE ROWS, which this move did not
			// make. It is still refused, because a walk that returned
			// here would loop for ever and a chart holding one cannot
			// answer "who leads this team" at all. Naming it as a
			// cycle is the honest report.
			return refuse(RuleCycle, "the chart already holds a cycle through "+
				"%q, so this move cannot be checked against it", parent)
		}
		seen[at] = true
		if at.parent == "" {
			return nil
		}
		at = w.live[refKey(ObjectRef{Kind: KindUnit, ID: at.parent})]
	}
	return nil
}

// holders is what a unit still contains: its child units and the seats in it.
func (w *working) holders(unit string) []string {
	var out []string
	for key, n := range w.live {
		if n.parent == unit {
			out = append(out, key)
		}
	}
	slices.Sort(out)
	return out
}

// edges is what the record states: one edge per object an operation named and
// did not remove, in the order each was first named, read off its node.
func (w *working) edges() []Edge {
	out := make([]Edge, 0, len(w.touched))
	for _, n := range w.touched {
		if n.removed {
			// A BATCH THAT CREATES A UNIT AND THEN REMOVES IT PUBLISHES
			// NO PLACEMENT: it would create the row the removal's
			// tombstone then has to delete, a record whose only effect
			// is on itself.
			continue
		}
		verb := n.op
		if n.from != "" && n.from != n.key {
			verb = OpRename
		}
		if verb == "" {
			// RENAMED AND RENAMED BACK, and nothing else: the batch
			// leaves the object exactly as it found it, so it states
			// nothing about it. An edge with no verb would be read as a
			// placement that creates what is absent.
			continue
		}
		edge := Edge{
			Object: ObjectRef{Kind: n.kind, ID: n.key},
			Parent: n.parent,
			Op:     verb,
		}
		if verb == OpRename {
			edge.From = n.from
		}
		switch n.kind {
		case KindUnit:
			edge.Lead = n.lead
		case KindSeat:
			// THE SEAT'S WHOLE STRUCTURAL POST-STATE, its list included:
			// the apply replaces the stored list with this one, so an edge
			// that left it out would clear it ([Edge.Manages]).
			edge.Kind = n.seatKind
			edge.Manages = slices.Clone(n.manages)
		}
		out = append(out, edge)
	}
	return out
}

// readWorking reads the whole chart's structure out of one transaction.
//
// THE WHOLE CHART, and that is affordable precisely here: a company has
// hundreds of units and seats, not the hundreds of thousands of rows the
// tracker holds, so the structure is a map a page of memory fits. A walk that
// queried per ancestor would be N round trips inside the transaction that holds
// this store's only writer — and the cycle check is a walk by construction.
//
// THE STRUCTURE FROM THE COLUMNS, the identity from the document. The columns
// are what a structural record wrote and what the rest of the decide compares
// against; the identity lives only in a renamed row's document, so only those
// rows are decoded.
func readWorking(ctx context.Context, tx *sql.Tx) (*working, error) {
	state := &working{
		live:        map[string]*wnode{},
		removedKeys: map[string]bool{},
		identities:  map[string]*wnode{},
		aliases:     map[string]*wnode{},
	}
	if err := state.scan(ctx, tx, KindUnit,
		`SELECT key, parent_key, lead, former_keys_json, document FROM chart_units`,
		func(doc []byte) (string, error) {
			unit, err := DecodeUnit(doc)
			return unit.Origin(), err
		}); err != nil {
		return nil, err
	}
	if err := state.scan(ctx, tx, KindSeat,
		`SELECT handle, unit_key, kind, former_keys_json, document FROM chart_seats`,
		func(doc []byte) (string, error) {
			seat, err := DecodeSeat(doc)
			return seat.Origin(), err
		}); err != nil {
		return nil, err
	}
	// EVERY SEAT'S `manages:` LIST, because it is structure an edge states
	// whole ([Edge.Manages]): a batch that moves a seat publishes the list
	// it has, and a rename moves the entries of every list that reaches it.
	manages, err := readManages(ctx, tx)
	if err != nil {
		return nil, err
	}
	for manager, targets := range manages {
		// A LIST WHOSE MANAGER THE ROWS DO NOT HOLD belongs to nothing a
		// batch can name, so nothing reads it here: a removal deletes the
		// rows a seat owns, and a rename moves them with it.
		if n, live := state.live[refKey(ObjectRef{Kind: KindSeat, ID: manager})]; live {
			n.manages = targets
		}
	}
	// THE TOMBSTONES TOO, because a removed address never resolves again
	// and a create onto one must be refused rather than silently writing a
	// row the gate then drops on every node.
	rows, err := tx.QueryContext(ctx,
		`SELECT object_kind, object_id FROM chart_removed`)
	if err != nil {
		return nil, fmt.Errorf("chart: read the removed objects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var kind, id string
		if err := rows.Scan(&kind, &id); err != nil {
			return nil, fmt.Errorf("chart: read a removed object: %w", err)
		}
		state.removedKeys[refKey(ObjectRef{Kind: ObjectKind(kind), ID: id})] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chart: read the removed objects: %w", err)
	}
	return state, nil
}

// scan reads one object table into the replay.
//
// A RENAMED ROW WHOSE DOCUMENT WILL NOT DECODE FAILS THE BATCH rather than
// being stepped over: a batch validated without its identity could create a
// second object with it, and a refusal naming an unreadable row is one somebody
// can act on.
func (w *working) scan(ctx context.Context, tx *sql.Tx, kind ObjectKind,
	query string, origin func([]byte) (string, error)) error {

	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("chart: read the %s structure: %w", kind, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		// THE THIRD COLUMN is the one structural field the kind has beyond
		// its place: a unit's lead, a seat's kind.
		var key, parent, third, formerJSON string
		var document []byte
		if err := rows.Scan(&key, &parent, &third, &formerJSON, &document); err != nil {
			return fmt.Errorf("chart: read a %s row: %w", kind, err)
		}
		n := &wnode{kind: kind, key: key, origin: key, parent: parent}
		switch kind {
		case KindUnit:
			n.lead = third
		case KindSeat:
			n.seatKind = SeatKind(third)
		}
		w.live[refKey(ObjectRef{Kind: kind, ID: key})] = n
		if formerJSON == "" || formerJSON == "[]" {
			continue
		}
		var former []string
		if err := json.Unmarshal([]byte(formerJSON), &former); err != nil {
			return fmt.Errorf("chart: read the retired addresses of %s %s: %w",
				kind, key, err)
		}
		for _, alias := range former {
			w.aliases[refKey(ObjectRef{Kind: kind, ID: alias})] = n
		}
		identity, err := origin(document)
		if err != nil {
			return fmt.Errorf("chart: decode the renamed %s %s: %w", kind, key, err)
		}
		n.origin = identity
		if identity != key {
			w.identities[refKey(ObjectRef{Kind: kind, ID: identity})] = n
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("chart: read the %s structure: %w", kind, err)
	}
	return nil
}

package chart

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// THE BATCH: what a structural change is, and why it is validated WHOLE.
//
// A structural change to an org chart is never one edge. Moving a seat between
// teams touches the seat, the team it left and the team it joined; dissolving a
// unit touches every child it had; an import rewrites whatever a config
// revision changed. So the caller-visible operation is a BATCH, and the batch
// is what the broker arbitrates — on the one subject the whole structure
// shares, so exactly one batch at a time can change the shape of the company.
//
// # Why it is refused WHOLE and names the FIRST failing operation
//
// A batch that applied the operations it could and skipped the ones it could
// not would produce a chart nobody asked for: half a reorganisation, with no
// record of which half. Worse, the half that landed would be arbitrary — it
// depends on the order the caller happened to write them in. So the whole
// batch is refused, and the refusal names ONE operation: the first that failed,
// with the rule it broke and the objects involved.
//
// ONE rather than all of them, deliberately. A batch that violates a cycle rule
// usually violates it in several places at once, and a list of twelve failures
// derived from one mistake is harder to act on than the first. The caller fixes
// it and resubmits, which is one round trip either way.
//
// # Why it is validated against the DECIDE'S OWN SNAPSHOT, replayed
//
// Every rule here is about the chart AFTER the batch: a move closes a cycle
// only in combination with the moves beside it, a parent may be created by an
// earlier operation in the same batch, and a unit may be emptied by one
// operation and removed by the next. Validating each operation against the
// stored rows alone would refuse every one of those — and validating against a
// tree the caller described would be trusting the caller about the state.
//
// So the batch is REPLAYED over a working copy of the rows read in the decide's
// own transaction: each operation is checked against the state the ones before
// it produced, and the state it produces is what the ones after it see. The
// working copy is discarded; what is published is the record, and the applier
// writes the rows.

// MaxBatchOperations bounds one structural batch.
//
// FIVE HUNDRED, and the number is a size rather than a taste. A batch is ONE
// record on the wire, and a record is refused permanently by the broker above
// [queue.MaxPayloadBytes] — 8 MiB — with no retry that can ever place it. A
// structural operation carries keys rather than content: an object reference, a
// parent key and a lead handle, each bounded at [MaxKey], which is at most
// about 200 bytes of JSON per operation. Five hundred of them is ~100 KiB,
// comfortably inside the ceiling with room for the envelope, the scope and the
// signature frame — and an order of magnitude above the largest reorganisation
// a company of a thousand seats performs in one gesture.
//
// It also bounds the TRANSACTION the applier runs. Every operation is a read
// and a write inside one transaction holding this store's only writer, so a
// batch that took a second to apply would stall every other write on the node
// for that second. Five hundred is tens of milliseconds.
//
// A caller with more to do submits more batches. That is not a workaround: two
// batches are two arbitrated records, and a company being restructured beyond
// this in one gesture is being rebuilt rather than edited — which is what an
// import is for.
const MaxBatchOperations = 500

// OperationKind is what one operation in a batch does.
type OperationKind string

const (
	// OpCreateUnit adds a unit. Its key must be free — including of a
	// tombstone, because a removed key never resolves again.
	OpCreateUnit OperationKind = "create_unit"

	// OpCreateSeat adds a seat, on the same terms.
	OpCreateSeat OperationKind = "create_seat"

	// OpMove reparents an object. The one operation that can close a
	// cycle, and therefore the reason this whole file exists.
	OpMove OperationKind = "move"

	// OpSetLead sets or clears a unit's authored lead.
	OpSetLead OperationKind = "set_lead"

	// OpRemoveObject takes an object out of the chart. A unit that still
	// holds anything is refused: an orphaned subtree is reachable from
	// nothing and removable by nothing.
	OpRemoveObject OperationKind = "remove"
)

// OperationKinds are the five.
var OperationKinds = []OperationKind{
	OpCreateUnit, OpCreateSeat, OpMove, OpSetLead, OpRemoveObject,
}

// Valid reports whether an operation kind is one this build performs.
func (o OperationKind) Valid() bool { return slices.Contains(OperationKinds, o) }

// Operation is one step of a structural batch.
type Operation struct {
	Kind OperationKind `json:"kind"`

	// Object is what the operation is about.
	Object ObjectRef `json:"object"`

	// Parent is the unit key for a create or a move. Empty is the ORG
	// ROOT, which is a real placement rather than a missing one — so it
	// cannot be distinguished from "unspecified" and is not meant to be:
	// a batch that meant to leave a parent alone does not state a move.
	Parent string `json:"parent,omitempty"`

	// Lead is the handle for a set_lead. Empty CLEARS it, which is the
	// ordinary state of a unit that inherits its lead from an ancestor.
	Lead string `json:"lead,omitempty"`
}

// Batch is one caller-visible structural change.
type Batch struct {
	// Operations run in order, each against the state the ones before it
	// produced.
	Operations []Operation

	// Reason rides a removal into its tombstone, which is the one row an
	// operator asking why next month has to read.
	Reason string
}

// ErrRefused reports a write this chart's own rules refuse.
//
// A SENTINEL because the caller has to tell it from a contention failure and
// from a broker that is down: a refusal is the caller's to fix and the other
// two are not, and a surface that reported all three the same way would have
// somebody retrying a write that can never land.
//
// ITS TEXT NAMES NO SHAPE OF WRITE, because it is a suffix on every one of
// them: a batch, a key claim, a removal, one object's content. It read "the
// batch is refused" and was wrapped by ten sentences, six of which are about
// something that is not a batch — so a person renaming a unit was handed a
// sentence ending in a noun their command never used, and the sentence in
// front of it is the one that already says what was refused and why.
var ErrRefused = errors.New("chart: refused by this chart's own rules")

// RefusalError names the one operation that failed and the rule it broke.
type RefusalError struct {
	// Index is the operation's position in the batch, so a caller
	// submitting five hundred knows which one to fix.
	Index int

	Operation Operation

	// Rule is what it broke, in the vocabulary this file's rules are named
	// in — so a surface can branch on it without parsing prose.
	Rule string

	// Detail is the sentence a person reads, naming the objects involved.
	Detail string
}

func (e *RefusalError) Error() string {
	return fmt.Sprintf("chart: operation %d (%s on %s) is refused — %s: %s",
		e.Index, e.Operation.Kind, e.Operation.Object, e.Rule, e.Detail)
}

// Is makes every refusal answer [ErrRefused].
func (e *RefusalError) Is(target error) bool { return target == ErrRefused }

// The rules, named. A surface branches on these rather than on prose.
const (
	// RuleUnknownKind is an operation this build does not perform.
	RuleUnknownKind = "unknown operation"

	// RuleKeyTaken is a create onto an address something already holds.
	RuleKeyTaken = "the key is taken"

	// RuleKeyRemoved is a create onto an address a removal retired. It is
	// its own rule rather than a flavour of the one above, because the
	// remedy is different: a taken key needs a different name, and a
	// removed one can never be used again at all.
	RuleKeyRemoved = "the key was removed"

	// RuleNoSuchObject is an operation on something the chart does not
	// hold, including something an earlier operation in this batch removed.
	RuleNoSuchObject = "no such object"

	// RuleNoSuchParent is a placement under a unit that is not there.
	RuleNoSuchParent = "no such parent"

	// RuleCycle is a move that would make an object its own ancestor.
	RuleCycle = "the move closes a cycle"

	// RuleUnitNotEmpty is a removal of a unit that still holds children or
	// seats.
	RuleUnitNotEmpty = "the unit is not empty"

	// RuleReservedKey is a key this engine reserves.
	RuleReservedKey = "the key is reserved"

	// RuleBadKey is a key whose shape no address can take.
	RuleBadKey = "the key is not an address"

	// RuleTooManyOperations is a batch past [MaxBatchOperations].
	RuleTooManyOperations = "the batch is too large"

	// RuleSeatHeld is a removal of a seat somebody in the identity
	// directory is bound to.
	//
	// ADVISORY, and the doc on [Batch.Validate] says why at length: this
	// is a read of ANOTHER domain's rows inside this domain's snapshot,
	// so it cannot arbitrate. What it buys is that the ordinary mistake —
	// removing a seat somebody is still using — is refused with that
	// person's name in the message.
	RuleSeatHeld = "somebody holds the seat"

	// RuleDirectoryUnreadable is a seat removal on a node that cannot see
	// the identity directory at all.
	//
	// A REFUSAL AND NOT A PASS, which is the whole of it: a node that does
	// not run the identity domain has a legitimately EMPTY copy of those
	// rows, and reading that emptiness as "nobody holds this seat" would
	// make a satellite the one place every removal succeeds. It names the
	// node, because the remedy is to make the removal somewhere else.
	RuleDirectoryUnreadable = "this node cannot read the directory"
)

// ReservedKeys are the unit keys and seat handles this engine will not let a
// company take.
//
// THREE, and each is reserved because something already means it. `root` is the
// unit a seat names when it sits at the org root ([RootUnit]), and a real unit
// keyed on it would make "at the root" and "in the root team" the same string
// in every scope path and every routing decision. `tree` and `barrier` are
// SUBJECT KINDS on this log, and a key that collided with one would produce a
// subject an applier dispatches to the wrong case.
var ReservedKeys = []string{RootUnit, string(KindTree), string(KindBarrier)}

// Scope is every object this batch may write, stated BEFORE the replay.
//
// THE FRAMEWORK NEEDS IT UP FRONT, because the deferral probe runs before the
// decide: a record this node cannot apply may already be sitting across the
// objects this one is about, and finding that out after deciding would mean
// deciding against rows a deferred record has not reached.
//
// So it is derived from what the operations NAME rather than from what the
// replay concludes they touch. That is always a SUPERSET — a refused batch
// names objects it never writes — and a superset is the safe direction: scope
// is a claim about what a record MAY write, and a narrow one is a deferral
// that does not block the writes it makes stale.
//
// It states the PARENTS too. A move writes only the moved object's row, but a
// reader asking about the unit it moved into gets a different answer
// afterwards, and the scope is what tells them their read was taken before it.
func (b Batch) Scope() ScopeSet {
	terms := make([]ScopeTerm, 0, len(b.Operations)*2)
	for _, op := range b.Operations {
		id := NormalizeKey(op.Object.ID)
		parent := NormalizeKey(op.Parent)
		switch op.Object.Kind {
		case KindUnit:
			terms = append(terms, ScopeTerm{Kind: TermUnit, ID: id})
		case KindSeat:
			terms = append(terms, ScopeTerm{
				Kind: TermSeat, Unit: parent, ID: id})
		}
		if parent != "" {
			terms = append(terms, ScopeTerm{Kind: TermUnit, ID: parent})
		}
	}
	// BatchScope collapses to the root past [MaxScopeTerms], which is the
	// honest blast radius for a batch that wide — see its own doc.
	return BatchScope(terms)
}

// working is the chart as a batch's replay sees it: the rows read in the
// decide's transaction, plus every change the operations before this one made.
//
// A COPY THAT IS DISCARDED. What gets published is the record; the applier is
// what writes rows. This exists so each operation is checked against the state
// the ones before it produced, which is the only reading under which a batch
// that creates a unit and then places a seat in it is valid.
type working struct {
	// parents maps an object's composed reference to its parent unit key.
	// Absent means the object is not in the chart.
	parents map[string]string

	// kinds maps the same reference to what the object is, so the cycle
	// walk can tell a unit's parent from a seat's.
	kinds map[string]ObjectKind

	// removed is what this batch has taken out, so a later operation on it
	// is refused rather than applied against a row the applier will delete.
	removed map[string]bool

	// identities maps the composed reference of every address that is a
	// RENAMED object's identity — the key it was created under — to the
	// object's current key, so a create onto one is refused. See
	// [identityHolder] for why a create may take a retired alias and never
	// a retired identity.
	identities map[string]string
}

// refKey composes an object reference into the working copy's own key.
//
// THE KIND IS PART OF IT, because a unit key and a seat handle are different
// namespaces: `NormalizeKey` folds both and neither reserves a prefix, so a
// company may legitimately hold a unit and a seat that answer to one spelling.
func refKey(ref ObjectRef) string {
	return string(ref.Kind) + "/" + NormalizeKey(ref.ID)
}

// Validate replays a batch over the rows in tx and refuses it whole.
//
// It returns the EDGES the batch produces and the objects it removes, which is
// exactly what the records published for it carry — so the validation and the
// record are one pass rather than two that can disagree.
func (b Batch) Validate(ctx context.Context, tx *sql.Tx, holders Holders) (
	edges []Edge, removed []ObjectRef, err error) {

	if len(b.Operations) > MaxBatchOperations {
		return nil, nil, &RefusalError{
			Index: MaxBatchOperations, Rule: RuleTooManyOperations,
			Detail: fmt.Sprintf("the batch holds %d operations and the cap is "+
				"%d — one batch is one record, and a record past the broker's "+
				"maximum payload is refused permanently rather than retried. "+
				"Submit it as several batches, each arbitrated on its own",
				len(b.Operations), MaxBatchOperations),
		}
	}
	state, err := readWorking(ctx, tx)
	if err != nil {
		return nil, nil, err
	}
	// EDGES ARE KEYED BY OBJECT AND OVERWRITTEN, because a batch that moves
	// one seat twice publishes its FINAL placement: the record is full
	// post-state, so two edges for one object would leave the applier's
	// result dependent on which it wrote last.
	byObject := map[string]Edge{}
	order := []string{}
	var gone []ObjectRef

	for i, op := range b.Operations {
		refused := state.apply(i, op)
		if refused != nil {
			return nil, nil, refused
		}
		switch op.Kind {
		case OpRemoveObject:
			if err := checkHeld(ctx, tx, i, op, holders); err != nil {
				return nil, nil, err
			}
			gone = append(gone, ObjectRef{
				Kind: op.Object.Kind, ID: NormalizeKey(op.Object.ID)})
			// AND ANY EDGE THIS BATCH ALREADY STATED FOR IT GOES TOO.
			// A batch that creates a unit and then removes it must
			// publish neither — the placement would create the row
			// the removal's tombstone then has to delete, which is a
			// record whose only effect is on itself.
			key := refKey(op.Object)
			if _, held := byObject[key]; held {
				delete(byObject, key)
				order = slices.DeleteFunc(order, func(k string) bool {
					return k == key
				})
			}
		default:
			key := refKey(op.Object)
			edge := byObject[key]
			edge.Object = ObjectRef{
				Kind: op.Object.Kind, ID: NormalizeKey(op.Object.ID)}
			switch op.Kind {
			case OpSetLead:
				edge.Lead = NormalizeKey(op.Lead)
				// THE PARENT COMES FROM THE WORKING COPY, not from
				// the operation: a set_lead states no parent, and
				// an edge is FULL POST-STATE — so publishing a
				// zero parent here would move the unit to the org
				// root as a side effect of naming its lead.
				edge.Parent = state.parents[key]
			default:
				edge.Parent = NormalizeKey(op.Parent)
			}
			if _, held := byObject[key]; !held {
				order = append(order, key)
			}
			byObject[key] = edge
		}
	}
	for _, key := range order {
		edges = append(edges, byObject[key])
	}
	return edges, gone, nil
}

// Holders reads WHO, IN THE IDENTITY DIRECTORY, IS BOUND TO A SEAT — from
// inside this domain's own snapshot transaction.
//
// # It is ADVISORY, and that word is load-bearing
//
// The chart and the identity estate are two state-log domains: two logs, two
// appliers, two arbitration anchors. A read of one inside the other's decide
// sees whatever that node has applied, which is not what the OTHER log has
// committed — so a bind and a removal can each pass their own decide and both
// land. The residue is a person bound to a seat that no longer exists, which
// is a NAMED legal state a duty reports, never corruption.
//
// What this buys is the ordinary mistake: somebody removes a seat a colleague
// is still using, and the refusal carries that colleague's name. What it must
// never be mistaken for is a boundary that did the work — an arbitration
// across two domains would need one log, and one log for the chart and the
// directory would serialise every hire against every sign-in.
//
// # Nil is a REFUSAL, not a pass
//
// A node that does not run the identity domain holds an empty copy of those
// rows, so reading them would answer "nobody holds anything" for the whole
// company. The removal is refused NAMING THE NODE instead — see
// [RuleDirectoryUnreadable]. That is the one place this seam's absence is not
// the third value but the second: a report may skip a finding it cannot
// compute, and a WRITE may not proceed on evidence it does not have.
type Holders interface {
	// HolderOf names the person bound to a seat, empty for a seat nobody
	// holds, and an ERROR for rows this node could not read.
	//
	// THE SEAT IS NAMED BY ITS IDENTITY — the handle it was created under,
	// [Seat.Origin] — because that is what a binding names (ADR-0020). Asked
	// by the handle the seat answers to now, a renamed seat read as held by
	// nobody, and its holder's removal went through without a word.
	HolderOf(ctx context.Context, tx *sql.Tx, seat string) (string, error)
}

// checkHeld refuses a seat removal the directory contradicts.
//
// AN ERROR RATHER THAN A REFUSAL when the chart's own row cannot be read, which
// is the one failure here that is neither a rule nor the directory's: the
// identity the directory is asked about is on that row.
func checkHeld(ctx context.Context, tx *sql.Tx, index int, op Operation,
	holders Holders) error {

	if op.Object.Kind != KindSeat {
		return nil
	}
	handle := NormalizeKey(op.Object.ID)
	if holders == nil {
		return &RefusalError{
			Index: index, Rule: RuleDirectoryUnreadable,
			Detail: fmt.Sprintf("seat %q cannot be removed here: this node "+
				"does not run the identity domain, so its copy of the "+
				"directory is empty for every seat and cannot say whether "+
				"anybody holds this one. Make the removal on a node that "+
				"serves people", handle),
		}
	}
	// THE IDENTITY, off the row. A seat created earlier in this same batch
	// has no row yet, and its identity is the handle it is being created
	// under.
	identity := handle
	seat, found, err := readSeat(ctx, tx, handle)
	if err != nil {
		return fmt.Errorf("chart: read seat %q to ask who holds it: %w", handle, err)
	}
	if found {
		identity = seat.Origin()
	}
	holder, err := holders.HolderOf(ctx, tx, identity)
	if err != nil {
		return &RefusalError{
			Index: index, Rule: RuleDirectoryUnreadable,
			Detail: fmt.Sprintf("seat %q cannot be removed: the directory "+
				"could not be read on this node, and a removal decided "+
				"without it is one that silently orphans whoever holds the "+
				"seat: %v", handle, err),
		}
	}
	if holder != "" {
		return &RefusalError{
			Index: index, Rule: RuleSeatHeld,
			Detail: fmt.Sprintf("seat %q is held by %s — removing it leaves "+
				"them signed in as a seat that is not in the chart, so every "+
				"authority rule asking what they lead falls through. Unbind "+
				"them first, or remove the person", handle, holder),
		}
	}
	return nil
}

// apply checks one operation against the working copy and advances it.
func (w *working) apply(index int, op Operation) *RefusalError {
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
	key := refKey(op.Object)

	switch op.Kind {
	case OpCreateUnit, OpCreateSeat:
		if err := w.checkCreate(refuse, op, id, key); err != nil {
			return err
		}
		if err := w.checkParent(refuse, op); err != nil {
			return err
		}
		w.parents[key] = NormalizeKey(op.Parent)
		w.kinds[key] = op.Object.Kind
		return nil

	case OpMove:
		if err := w.checkPresent(refuse, key, id); err != nil {
			return err
		}
		if err := w.checkParent(refuse, op); err != nil {
			return err
		}
		if err := w.checkCycle(refuse, op, key); err != nil {
			return err
		}
		w.parents[key] = NormalizeKey(op.Parent)
		return nil

	case OpSetLead:
		if op.Object.Kind != KindUnit {
			return refuse(RuleUnknownKind, "only a unit has a lead")
		}
		return w.checkPresent(refuse, key, id)

	case OpRemoveObject:
		if err := w.checkPresent(refuse, key, id); err != nil {
			return err
		}
		if op.Object.Kind == KindUnit {
			if held := w.holders(id); len(held) > 0 {
				return refuse(RuleUnitNotEmpty, "%s still holds %s — an "+
					"orphaned subtree is reachable from nothing and "+
					"removable by nothing, so its contents move or go first",
					id, strings.Join(held, ", "))
			}
		}
		delete(w.parents, key)
		w.removed[key] = true
		return nil
	}
	return refuse(RuleUnknownKind, "%q is not one of %v", op.Kind, OperationKinds)
}

type refuseFunc func(rule, detail string, args ...any) *RefusalError

// checkCreate refuses a create onto an address something already holds.
func (w *working) checkCreate(refuse refuseFunc, op Operation, id, key string) *RefusalError {
	if slices.Contains(ReservedKeys, id) {
		return refuse(RuleReservedKey, "%q is reserved: %v name the org root "+
			"and this log's own subject kinds, so a company object keyed on "+
			"one would collide with them in every scope path and every "+
			"routing decision", id, ReservedKeys)
	}
	if len(id) > MaxKey {
		return refuse(RuleBadKey, "%q is %d bytes and the cap is %d",
			id, len(id), MaxKey)
	}
	if w.removed[key] {
		return refuse(RuleKeyRemoved, "%q was removed from the chart, and a "+
			"removed address never resolves again — its history, its "+
			"references and the tombstone that stops its old records "+
			"applying are all keyed on it", id)
	}
	if _, held := w.parents[key]; held {
		return refuse(RuleKeyTaken, "%s %q is already in the chart",
			op.Object.Kind, id)
	}
	if holder, held := w.identities[key]; held {
		return refuse(RuleKeyTaken, "%q is the address %s %q was created "+
			"under — its identity, which everything durable it owns and every "+
			"person bound to it is keyed on, and which it keeps however often "+
			"it is renamed. An identity is never issued twice; pick another "+
			"address", id, op.Object.Kind, holder)
	}
	// AND THE OTHER NAMESPACE'S CREATE IN THIS BATCH. Two operations
	// creating one address in two kinds are legal — a unit and a seat may
	// share a spelling — so this checks only the kind's own map, which the
	// composed key already does. Stated so the next reader does not "fix"
	// it into a cross-kind check that would refuse a legal chart.
	return nil
}

// checkPresent refuses an operation on an object the chart does not hold.
func (w *working) checkPresent(refuse refuseFunc, key, id string) *RefusalError {
	if w.removed[key] {
		return refuse(RuleNoSuchObject, "%q was removed earlier in this batch",
			id)
	}
	if _, held := w.parents[key]; !held {
		return refuse(RuleNoSuchObject, "%q is not in the chart", id)
	}
	return nil
}

// checkParent refuses a placement under a unit that is not there.
//
// THE ORG ROOT IS A REAL PARENT and is never checked: an empty parent is where
// a top-level department and an org-wide seat both sit.
func (w *working) checkParent(refuse refuseFunc, op Operation) *RefusalError {
	parent := NormalizeKey(op.Parent)
	if parent == "" {
		return nil
	}
	key := refKey(ObjectRef{Kind: KindUnit, ID: parent})
	if w.removed[key] {
		return refuse(RuleNoSuchParent, "%q was removed earlier in this batch",
			parent)
	}
	if _, held := w.parents[key]; !held {
		return refuse(RuleNoSuchParent, "%q is not a unit in the chart — a "+
			"parent may be created by an earlier operation in this batch, and "+
			"nothing in this one creates it", parent)
	}
	return nil
}

// checkCycle refuses a move that would make an object its own ancestor.
//
// THE WALK IS OVER THE WORKING COPY, which is the whole reason this validation
// is a replay: two moves that are each locally valid can jointly close a cycle,
// and neither writer's own subject would ever have shown it to them. Walking
// the stored rows instead would accept exactly that pair.
//
// A SEAT CANNOT CLOSE A CYCLE — nothing sits under a seat — so the walk runs
// for a unit alone. It is still called for both, because "this kind cannot" is
// a fact worth stating once here rather than in every caller.
func (w *working) checkCycle(refuse refuseFunc, op Operation, key string) *RefusalError {
	if op.Object.Kind != KindUnit {
		return nil
	}
	parent := NormalizeKey(op.Parent)
	if parent == "" {
		return nil
	}
	// WALK UP FROM THE NEW PARENT. If the object being moved is on that
	// path, the move puts the object under itself.
	seen := map[string]bool{}
	at := refKey(ObjectRef{Kind: KindUnit, ID: parent})
	for at != "" {
		if at == key {
			return refuse(RuleCycle, "moving %q under %q would put it under "+
				"itself — %q is already somewhere beneath it",
				NormalizeKey(op.Object.ID), parent, parent)
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
		at = w.parents[at]
		if at != "" {
			at = refKey(ObjectRef{Kind: KindUnit, ID: at})
		}
	}
	return nil
}

// holders is what a unit still contains: its child units and the seats in it.
func (w *working) holders(unit string) []string {
	var out []string
	for key, parent := range w.parents {
		if parent != unit || w.removed[key] {
			continue
		}
		out = append(out, key)
	}
	slices.Sort(out)
	return out
}

// readWorking reads the whole chart's structure out of one transaction.
//
// THE WHOLE CHART, and that is affordable precisely here: a company has
// hundreds of units and seats, not the hundreds of thousands of rows the
// tracker holds, so the structure is a map a page of memory fits. A walk that
// queried per ancestor would be N round trips inside the transaction that holds
// this store's only writer — and the cycle check is a walk by construction.
func readWorking(ctx context.Context, tx *sql.Tx) (*working, error) {
	state := &working{
		parents:    map[string]string{},
		kinds:      map[string]ObjectKind{},
		removed:    map[string]bool{},
		identities: map[string]string{},
	}
	if err := scanInto(ctx, tx,
		`SELECT key, parent_key FROM chart_units`, KindUnit, state); err != nil {
		return nil, err
	}
	if err := scanInto(ctx, tx,
		`SELECT handle, unit_key FROM chart_seats`, KindSeat, state); err != nil {
		return nil, err
	}
	// THE RENAMED OBJECTS' IDENTITIES, because a create onto one would be a
	// second object with the first one's identity. Only a renamed row has an
	// identity other than its key, so this decodes those rows and no other.
	if err := scanIdentities(ctx, tx, "chart_units", KindUnit, DecodeUnit,
		Unit.Origin, func(u Unit) string { return u.Key }, state); err != nil {
		return nil, err
	}
	if err := scanIdentities(ctx, tx, "chart_seats", KindSeat, DecodeSeat,
		Seat.Origin, func(s Seat) string { return s.Handle }, state); err != nil {
		return nil, err
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
		state.removed[refKey(ObjectRef{Kind: ObjectKind(kind), ID: id})] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chart: read the removed objects: %w", err)
	}
	return state, nil
}

// scanIdentities reads every renamed object's identity into the working copy.
//
// A ROW WHOSE DOCUMENT WILL NOT DECODE FAILS THE BATCH rather than being
// stepped over: a batch validated without it could create a second object with
// that row's identity, and a refusal naming an unreadable row is one somebody
// can act on.
func scanIdentities[T any](ctx context.Context, tx *sql.Tx, table string,
	kind ObjectKind, decode func([]byte) (T, error), origin, key func(T) string,
	state *working) error {

	rows, err := tx.QueryContext(ctx,
		`SELECT document FROM `+table+` WHERE former_keys_json <> '[]'`)
	if err != nil {
		return fmt.Errorf("chart: read the renamed %ss: %w", kind, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var document []byte
		if err := rows.Scan(&document); err != nil {
			return fmt.Errorf("chart: read a renamed %s: %w", kind, err)
		}
		object, err := decode(document)
		if err != nil {
			return fmt.Errorf("chart: decode a renamed %s: %w", kind, err)
		}
		if identity, current := origin(object), key(object); identity != current {
			state.identities[refKey(ObjectRef{Kind: kind, ID: identity})] = current
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("chart: read the renamed %ss: %w", kind, err)
	}
	return nil
}

// scanInto reads one object table's structure into the working copy.
func scanInto(ctx context.Context, tx *sql.Tx, query string, kind ObjectKind,
	state *working) error {

	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("chart: read the %s structure: %w", kind, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, parent string
		if err := rows.Scan(&id, &parent); err != nil {
			return fmt.Errorf("chart: read a %s row: %w", kind, err)
		}
		key := refKey(ObjectRef{Kind: kind, ID: id})
		state.parents[key] = parent
		state.kinds[key] = kind
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("chart: read the %s structure: %w", kind, err)
	}
	return nil
}

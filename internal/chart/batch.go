package chart

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
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

	// Lead is the handle for a set_lead, and the lead a create_unit makes
	// the new unit with. Empty CLEARS it, which is the ordinary state of a
	// unit that inherits its lead from an ancestor.
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

	// RuleUnusedField is an operation carrying a field its kind does not
	// take — a parent on a set_lead, a lead on a move — which nothing would
	// read, so the batch would answer as though it asked for less.
	RuleUnusedField = "the operation does not take the field"

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

// Validate replays a batch over the rows in tx and refuses it whole.
//
// It returns the EDGES the batch produces and the objects it removes, which is
// exactly what the records published for it carry — so the validation and the
// record are one pass rather than two that can disagree.
//
// EACH EDGE IS ONE OBJECT'S WHOLE STRUCTURAL POST-STATE, read off the replay
// once every operation has run, under the one verb that says what kind of
// change it was ([Edge.Op]). A batch that moves one seat twice publishes its
// final placement, a move publishes the lead the unit has, and a create_unit
// the lead it named.
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
	var gone []ObjectRef
	for i, op := range b.Operations {
		if refused := state.apply(ctx, i, op); refused != nil {
			return nil, nil, refused
		}
		if op.Kind == OpRemoveObject {
			if err := checkHeld(ctx, tx, i, op, holders); err != nil {
				return nil, nil, err
			}
			gone = append(gone, ObjectRef{
				Kind: op.Object.Kind, ID: NormalizeKey(op.Object.ID)})
		}
	}
	return state.edges(), gone, nil
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

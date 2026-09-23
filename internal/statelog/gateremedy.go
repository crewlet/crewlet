package statelog

import "slices"

// GateAction names one thing an operator can do about a node gate — an
// eviction or a readmission — that a log did not finish, or that was refused
// before anything was written.
//
// # An action, and never a flag spelling
//
// The remedy used to be one sentence, and it was written in the command line's
// vocabulary: "evict it with -force", "run it again without -op-id", "from
// another node (-url)". The dashboard rendered that sentence word for word
// beside a dialog that has no -force, no -op-id and no -url, so an operator in
// a browser was told to pass flags the screen in front of them could not send.
// Two surfaces with two vocabularies need the WHAT separated from the HOW: the
// engine says which of these to do, in a closed set a surface can switch on,
// and each surface renders it its own way — a Finish button and `-op-id`, a
// Force control and `-force`. The sentence beside it ([GateRemedy.Detail])
// names neither.
//
// A NAMED STRING rather than an int, so an action a newer node sends is a value
// an older client shows rather than a panic, and [GateAction.Valid] says
// whether this build knows it.
type GateAction string

const (
	// GateRetrySameOp — the same gesture again, under the same operation
	// id, finishes this log: a log that already holds the record answers
	// from its own rows and one that does not is written.
	GateRetrySameOp GateAction = "retry_same_op"

	// GateNewGesture — this operation cannot be finished and a NEW gesture,
	// under a fresh operation id, is what the operator runs if they still
	// want the effect: the id names another object's record, or a later
	// gate has undone this one.
	GateNewGesture GateAction = "new_gesture"

	// GateForce — evict past the presence-lease judgement: a node wedged in
	// a way that still renews its lease, or a lease listing this node
	// could not read. An eviction only; a readmission has nothing to force.
	GateForce GateAction = "force"

	// GateOtherNode — run the gesture through another node the fleet still
	// counts, under the same operation id: this one cannot write the log
	// (it is evicted, holds a record it cannot decode, or is adopting).
	GateOtherNode GateAction = "other_node"

	// GateReanchor — re-anchor the log first: it is not the one this node's
	// rows were derived from, and nothing can be written to it until then.
	// Then the same gesture, under the same operation id, finishes it.
	GateReanchor GateAction = "reanchor"

	// GateSetCapacity — raise the log's ceiling: it is full past even the
	// reserve kept for gate records, which no retry refills. Then the same
	// gesture, under the same operation id, finishes it.
	GateSetCapacity GateAction = "set_capacity"

	// GateRestore — restore the store and the stream from one backup: they
	// were restored out of step, which no retry clears.
	GateRestore GateAction = "restore"

	// GateWait — wait for what the detail names to clear on its own — a
	// lease to lapse, a node to catch up — and then run the gesture again.
	GateWait GateAction = "wait"
)

// GateActions is every [GateAction] this build names, in declaration order.
//
// THE DASHBOARD CARRIES A COPY — it is a separate build and cannot import a Go
// identifier — and the copy is held to this list by a gate in both directions,
// because an action the engine sends and the screen does not know is a remedy
// the operator never sees.
func GateActions() []GateAction {
	return []GateAction{
		GateRetrySameOp, GateNewGesture, GateForce, GateOtherNode,
		GateReanchor, GateSetCapacity, GateRestore, GateWait,
	}
}

// Valid reports whether a is an action this build names.
func (a GateAction) Valid() bool { return slices.Contains(GateActions(), a) }

// KeepsOperation reports whether, once the operator has done a, the gesture is
// finished under its OWN operation id — sent again now, through another node,
// or after the log is re-anchored or given room.
//
// # Why a surface has to know this, and not only whether to retry now
//
// A log refused `log_full` cannot be finished by sending the gesture again
// straight away, and it was once advised no operation id at all — so the
// operator raised the ceiling and ran the eviction afresh. A fresh id is a
// SECOND gesture: every log that already held the first one's record is
// written again, its eviction re-dated and its fence window restarted, and the
// first id answers `superseded` to anyone finishing it. The gesture's own id is
// what finishes it after every one of these remedies, and a surface that
// discarded it once nothing could be retried NOW threw away the only safe way
// on. False for [GateNewGesture], whose operation can never be finished, and
// for [GateRestore] and [GateWait], after which the gesture's own answer says
// what next.
func (a GateAction) KeepsOperation() bool {
	switch a {
	case GateRetrySameOp, GateOtherNode, GateReanchor, GateSetCapacity:
		return true
	}
	return false
}

// GateActionsKeepingOperation is every action [GateAction.KeepsOperation]
// reports true for, in [GateActions]' order — the list the dashboard's copy is
// held to.
func GateActionsKeepingOperation() []GateAction {
	var out []GateAction
	for _, a := range GateActions() {
		if a.KeepsOperation() {
			out = append(out, a)
		}
	}
	return out
}

// GateRemedy is what an operator does about a node gate a log did not finish,
// or one refused before anything was written: the actions, in the order to try
// them, and the sentence that says why.
//
// THE ZERO VALUE IS "NOTHING TO DO" — a log that holds its record — and a
// remedy with a detail and no action is a condition no gesture on any surface
// clears, which the detail then names (a broker's payload limit, a refusal
// this build has no word for). Neither is a retry, which is the point: a
// surface offers the same gesture again only where [GateRetrySameOp] says so.
type GateRemedy struct {
	Actions []GateAction
	Detail  string
}

// Offers reports whether the remedy includes action a.
func (r GateRemedy) Offers(a GateAction) bool { return slices.Contains(r.Actions, a) }

// IsZero reports a remedy with nothing in it — a log the gesture finished.
func (r GateRemedy) IsZero() bool { return len(r.Actions) == 0 && r.Detail == "" }

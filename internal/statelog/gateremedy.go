package statelog

import "slices"

// GateAction names one thing an operator can do about a node gate — an
// eviction or a readmission — that a log did not finish, or that was refused
// before anything was written.
//
// It is the WHAT, never a flag spelling: the CLI and the dashboard each render
// it their own way (`-force` and a Force control), so [GateRemedy.Detail]
// names neither surface's controls. A named string, so an action a newer node
// sends is a value an older client shows, and [GateAction.Valid] says whether
// this build knows it.
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
	// (it is evicted, holds a record it cannot decode, or is adopting), or
	// cannot tell whether its record landed (its operation ledger may have
	// lost the row this operation needs — [Result.Unvouched]).
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
// The dashboard carries a copy, held to this list in both directions: an
// action it does not know is a remedy the operator never sees.
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
// A surface must keep the id even when nothing can be retried NOW: a fresh id
// is a SECOND gesture, rewriting every log that held the first one's record
// (re-dating its eviction, restarting its fence window) while the first id
// answers `superseded`. False for [GateNewGesture], whose operation can never
// be finished, and for [GateRestore] and [GateWait], after which the
// gesture's own answer says what next.
func (a GateAction) KeepsOperation() bool {
	switch a {
	case GateRetrySameOp, GateOtherNode, GateReanchor, GateSetCapacity:
		return true
	}
	return false
}

// GateActionsKeepingOperation is every action [GateAction.KeepsOperation]
// reports true for, in [GateActions]' order: the dashboard's copy is held to it.
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
// The zero value is "nothing to do" (the log holds its record); a detail with
// no action is a condition no gesture clears (a broker's payload limit, a
// refusal this build has no word for). Neither is a retry: a surface offers
// the same gesture again only where [GateRetrySameOp] says so.
type GateRemedy struct {
	Actions []GateAction
	Detail  string
}

// Offers reports whether the remedy includes action a.
func (r GateRemedy) Offers(a GateAction) bool { return slices.Contains(r.Actions, a) }

// IsZero reports a remedy with nothing in it — a log the gesture finished.
func (r GateRemedy) IsZero() bool { return len(r.Actions) == 0 && r.Detail == "" }

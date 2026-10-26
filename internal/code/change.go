package code

import "slices"

// A CHANGE'S CLOSED SETS — the values code_changes holds (replicated migration
// 0058), each a named string with typed constants, Valid, and a function
// answering the set in order: the one list a validator, a schema and the
// dashboard's contract read.

// ChangeState is where a change (web!42) is in its life.
type ChangeState string

const (
	// ChangeStateDraft is opened and not yet asking to be merged: its rows,
	// its revisions and its reviews are a live change's.
	ChangeStateDraft ChangeState = "draft"
	// ChangeStateOpen asks to be merged into its target.
	ChangeStateOpen ChangeState = "open"
	// ChangeStateClosed was closed without being merged; it may be opened
	// again while its source branch is live.
	ChangeStateClosed ChangeState = "closed"
	// ChangeStateMerged reached its target, how its MergedVia says. Final.
	ChangeStateMerged ChangeState = "merged"
)

// ChangeStates is the closed set, in this order — a change's life, the live
// states first.
func ChangeStates() []ChangeState {
	return []ChangeState{ChangeStateDraft, ChangeStateOpen, ChangeStateClosed, ChangeStateMerged}
}

// Valid reports a state this build knows.
func (s ChangeState) Valid() bool { return slices.Contains(ChangeStates(), s) }

// Live reports a state a change still moves from — draft or open: the one a
// push to its source revises, a push of its target to its head merges, and an
// open on its source branch is refused beside. The two indexes partial on the
// live states (code_changes_open_idx, code_changes_target_idx) hold exactly
// these rows, which TestTheLiveIndexesHoldTheLiveStates holds their predicate
// to.
func (s ChangeState) Live() bool { return s == ChangeStateDraft || s == ChangeStateOpen }

// MergedVia is how a change reached its target.
type MergedVia string

// MergedViaPush is a change whose target came to hold its head by a push:
// one that set the target to exactly the change's head, or one that moved the
// target past it, which the repository's home git node finds and records.
const MergedViaPush MergedVia = "push"

// MergedVias is the closed set.
func MergedVias() []MergedVia { return []MergedVia{MergedViaPush} }

// Valid reports a value this build knows.
func (m MergedVia) Valid() bool { return slices.Contains(MergedVias(), m) }

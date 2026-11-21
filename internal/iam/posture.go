package iam

import "slices"

// AdminPosture is whether THIS NODE grants admin to those who hold it: Tier A's
// `api.auth.admin`, the per-node ceiling on what a principal composed here
// carries. In the leaf, beside [SecondFactor], so internal/config can validate
// `api.auth` without pulling in the request guard.
//
// TWO VALUES AND NOT A LIST OF GRANTS: the only thing worth withholding on a
// node is admin itself; a node refusing an app would refuse colleagues their
// board.
//
// THE ZERO IS INVALID and confers nothing ([AdminPosture.Effective]):
// internal/config refuses it on a node serving the API, and a principal
// composed under one anyway holds no permission rather than every one.
type AdminPosture string

const (
	// AdminServe grants admin to those who hold it: the node an
	// administrator signs in on.
	AdminServe AdminPosture = "serve"

	// AdminRefuse clamps admin away and keeps every app: a node published
	// to the people who use the company, where nobody should be able to
	// run it however their row reads. An administrator signed in here
	// keeps every app ([Expand] runs before the clamp) and is told what
	// they lack by the authority table.
	AdminRefuse AdminPosture = "refuse"
)

// AdminPostures are the two.
var AdminPostures = []AdminPosture{AdminServe, AdminRefuse}

// Valid reports whether a posture is one this build knows.
func (a AdminPosture) Valid() bool { return slices.Contains(AdminPostures, a) }

// Effective is what a principal composed on this node carries, given what its
// row holds.
//
// IN [AllGrants] ORDER, every g in Expand(held) that this build knows and that
// is not admin on a node refusing it. [Expand] runs BEFORE admin is clamped, so
// [admin] on a refusing node keeps every app. Unknown names are dropped HERE,
// from what the principal carries, and stay in their row.
//
// AN INVALID POSTURE, THE ZERO INCLUDED, CONFERS NOTHING (a non-nil empty
// slice), failing closed rather than guessing which of the two the operator
// meant.
func (a AdminPosture) Effective(held []Grant) []Grant {
	out := []Grant{}
	if !a.Valid() {
		return out
	}
	expanded := Expand(held)
	for _, g := range AllGrants {
		if g == GrantAdmin && a != AdminServe {
			continue
		}
		if slices.Contains(expanded, g) {
			out = append(out, g)
		}
	}
	return out
}

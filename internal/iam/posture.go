package iam

import "slices"

// AdminPosture is whether THIS NODE grants admin to those who hold it — Tier
// A's `api.auth.admin`, the per-node ceiling on what a principal composed here
// carries.
//
// HERE, IN THE LEAF, beside [SecondFactor] and for that type's reason:
// internal/config has to be able to name the posture in order to validate
// `api.auth`, and naming it must not pull in the request guard that applies
// it.
//
// TWO VALUES AND NOT A LIST OF GRANTS, which is what it replaced. A node's
// ceiling used to be any subset of the eleven grants, so a fleet mid-rollout
// could hold two ceilings that agreed on nothing, and the one question an
// operator ever asked of it — "can somebody administer the company through
// THIS node?" — had to be read off a set. With admin and one permission per
// app the only thing worth withholding on one node is admin itself: an app is
// the company's ordinary work, and a node that refused it would refuse its
// own colleagues their board.
//
// THE ZERO IS INVALID and confers nothing ([AdminPosture.Effective]):
// internal/config refuses it on a node that serves the API, naming the field,
// and a principal composed under one anyway — a wiring mistake — holds no
// permission at all rather than every one.
type AdminPosture string

const (
	// AdminServe grants admin to those who hold it: the node an
	// administrator signs in on.
	AdminServe AdminPosture = "serve"

	// AdminRefuse clamps admin away and keeps every app: a node published
	// to the people who use the company, where nobody should be able to
	// run it however their row reads. An administrator signed in here
	// keeps the tracker, the knowledge base and the chat — [Expand] runs
	// before the clamp — and is told what they lack by the authority table.
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
// is not admin on a node refusing it. [Expand] runs BEFORE admin is clamped,
// so a row of [admin] on a refusing node keeps every app: the clamp takes away
// running the company and nothing a colleague has. Unknown names are dropped
// HERE, from what the principal carries, and stay in the row they came from.
//
// AN INVALID POSTURE, THE ZERO INCLUDED, CONFERS NOTHING — a non-nil empty
// slice — which fails closed: a node whose configuration somehow reached the
// request path without a posture grants nobody anything, rather than reading
// the zero as either of the two and guessing which one the operator meant.
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

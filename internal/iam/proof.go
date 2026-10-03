package iam

import "slices"

// WHAT A PRINCIPAL HAS TO PROVE, as VALUES — the two settings a deployment's
// own configuration states about proof, held here rather than beside the
// arithmetic that enforces them.
//
// internal/iam/credential is where a password is hashed and a TOTP code is
// checked, and it is the natural home for both of these until you notice who
// reads them: internal/config, which has to be able to say "this deployment
// requires a second factor" and "a password here is at least twelve
// characters" in order to validate `api.auth.local`. This package's whole
// premise is that naming an identity concept must not pull in a password
// hasher — so the two numbers config reads live in the leaf, and credential
// imports them rather than declaring a second copy nothing compares.
//
// [Recency] is here for the same premise one layer over: internal/authz states
// on every rule how recently a principal must have proved who they are, and a
// rule table naming a step-up window must not pull in the sessions that
// measure one.

// SecondFactor is whether a company REQUIRES a second factor or merely offers
// one.
//
// A NAMED STRING WHOSE ZERO VALUE IS INVALID, like every closed set in this
// tree, and here the zero value is the one that would be dangerous: a bool
// would default to false, which reads as "optional", so a company that
// declared nothing would silently be the company with no second factor. An
// invalid zero means the configuration is REFUSED and somebody decides.
type SecondFactor string

const (
	// SecondFactorRequired is: nobody acts on a password alone. A person
	// who holds a second factor is asked for it at every password sign-in
	// and step-up; a person who holds none — freshly invited, the founder,
	// one an administrator reset — signs in to a session that may do
	// nothing but ENROL one, and enrolling replaces it with a whole
	// session. internal/api/authapi decides which sessions those are and
	// internal/api/auth refuses every other route to them.
	SecondFactorRequired SecondFactor = "required"

	// SecondFactorOptional is: a person may enrol one and is not made to.
	//
	// IT IS A REAL CHOICE where nobody else can reach the password: a
	// deployment a browser reaches on loopback — a laptop, a development
	// box — has no network for a password alone to cross. Anywhere else
	// internal/config refuses it unless `accept_insecure` says so out
	// loud, because there it is a password alone signing somebody in over
	// the network.
	SecondFactorOptional SecondFactor = "optional"
)

// SecondFactors are the two.
var SecondFactors = []SecondFactor{SecondFactorRequired, SecondFactorOptional}

// Valid reports whether a value off the wire is one this build knows.
func (s SecondFactor) Valid() bool { return slices.Contains(SecondFactors, s) }

// Requires reports whether a second factor must be enrolled.
//
// AN ALLOWLIST OF ONE, for [Stage.MayAct]'s reason turned round: a value
// this build does not know answers TRUE here, because the safe direction for
// "must you prove more" is yes. A denylist would have read an unknown value as
// optional and quietly dropped the requirement on a rolling upgrade.
func (s SecondFactor) Requires() bool { return s != SecondFactorOptional }

// MinPasswordChars is the shortest password this engine accepts, and the floor
// under `api.auth.local.min_password_length`.
//
// TWELVE, AND NO COMPOSITION RULES — no required digit, no required symbol, no
// forbidden repeat. That is the current guidance and it is guidance because
// composition rules are measurably counter-productive: they shrink the space
// people actually choose from (everybody appends `1!`), they are enumerable by
// an attacker who knows the rule, and they push people to write the result
// down. LENGTH is the only property that buys entropy from a human without
// costing them anything.
//
// Measured in CHARACTERS rather than bytes, for the reason
// [secrets.MinSharedTokenChars] gives: a byte count quietly passes a 12-byte
// value that is four characters of UTF-8.
const MinPasswordChars = 12

// Recency is how recently a gesture needs whoever makes it to have PROVED who
// they are — the step-up requirement, as a value a rule states.
//
// ONE WINDOW, and it is the configuration's: `step_up` is
// `api.auth.session.step_up` (an hour by default), spelled as its key so the
// setting an operator would tune and the word a refusal names are one string.
// Every gesture that asks for a recent proof asks this one — revealing a
// secret, changing somebody's authority or how they prove who they are,
// ending every session in the company, writing the configuration — for the
// reason internal/config gives at the setting: a second, shorter window beside
// it sent people to re-prove inside the hour they had already proved in, and
// the gestures it guarded are bounded by what no proof's age can change — a
// grant no machine token may carry, and a refusal of any request a machine
// token presented.
//
// A NAMED STRING WHOSE ZERO VALUE IS INVALID, and the zero is the one that
// would be dangerous: read as "no proof needed", a rule somebody wrote without
// deciding would ship a step-up gesture open to a session proved last week.
// So every rule in internal/authz states one of the two, and a walk there
// refuses the zero.
type Recency string

const (
	// RecencyAny asks for no recent proof: signing in was enough. Every
	// read, and every gesture a seat's own tools make, is this — a seat has
	// no keyboard to prove anything at, and the operator's MCP surface is
	// not a step-up surface.
	RecencyAny Recency = "any"

	// RecencyStepUp asks for a proof inside `api.auth.session.step_up`: the
	// company's configuration, its chart, its integrations, its credential
	// store, the identity directory's writes, how somebody proves who they
	// are, and the deployment's own controls.
	RecencyStepUp Recency = "step_up"
)

// Recencies are the two, weakest first.
var Recencies = []Recency{RecencyAny, RecencyStepUp}

// Valid reports whether a value is one this build knows.
func (r Recency) Valid() bool { return slices.Contains(Recencies, r) }

// Demands reports whether r asks for any recent proof at all.
func (r Recency) Demands() bool { return r.Valid() && r != RecencyAny }

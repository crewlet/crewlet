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
	// SecondFactorRequired is: nobody may hold an active password
	// credential without a second factor enrolled, and a person who has
	// not enrolled one is sent to do so before they can act.
	SecondFactorRequired SecondFactor = "required"

	// SecondFactorOptional is: a person may enrol one and is not made to.
	//
	// IT IS A REAL CHOICE rather than a lesser one. A company where the
	// only credential anybody holds is an identity provider's has nothing
	// for this to require — the second factor is the provider's, and
	// demanding one here would be asking for a factor on top of a factor
	// the engine cannot see.
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

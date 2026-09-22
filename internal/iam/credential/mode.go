package credential

import "slices"

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
// AN ALLOWLIST OF ONE, for [iam.Stage.MayAct]'s reason turned round: a value
// this build does not know answers TRUE here, because the safe direction for
// "must you prove more" is yes. A denylist would have read an unknown value as
// optional and quietly dropped the requirement on a rolling upgrade.
func (s SecondFactor) Requires() bool { return s != SecondFactorOptional }

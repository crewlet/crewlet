package iam

import "slices"

// WHAT A PRINCIPAL HAS TO PROVE, as VALUES: the settings a deployment states
// about proof, held here rather than beside the arithmetic that enforces them.
//
// internal/config must say "this deployment requires a second factor" and "a
// password is at least twelve characters" to validate `api.auth`, and naming an
// identity concept must not pull in a password hasher, so the values live in
// the leaf and internal/iam/credential imports them rather than declaring a
// copy nothing compares.
//
// [Recency] is here for the same reason: internal/authz states on every rule
// how recently a principal must have proved who they are, and a rule table
// must not pull in the sessions that measure it.

// SecondFactor is whether a company REQUIRES a second factor or merely offers
// one.
//
// A NAMED STRING WHOSE ZERO VALUE MEANS REQUIRED, and never a bool: a bool
// would default to false, which reads as "optional", so a company that
// declared nothing would silently be the company with no second factor. The
// zero is the secure value instead — internal/config's `APIAuth.SecondFactor`
// reads it, and anything else but `optional`, as required — so `optional` is
// always somebody's explicit choice. [SecondFactor.Valid] is about the two
// values a file may write; internal/config accepts the zero beside them as the
// unset setting.
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

// MinPasswordChars is the shortest password this engine accepts, and the floor
// under the company's `sign_in.min_password_length`.
//
// TWELVE, AND NO COMPOSITION RULES (no required digit, symbol or forbidden
// repeat): they shrink the space people choose from (everybody appends `1!`),
// are enumerable by an attacker who knows the rule, and push people to write
// the password down. LENGTH is the one property that buys human entropy free.
//
// Measured in CHARACTERS, for [secrets.MinSharedTokenChars]'s reason: a byte
// count passes a 12-byte value that is four characters of UTF-8.
const MinPasswordChars = 12

// Recency is how recently a gesture needs whoever makes it to have PROVED who
// they are: the step-up requirement, as a value a rule states.
//
// ONE WINDOW, the company's: `step_up` is `sign_in.step_up_seconds` (an hour by
// default), for the reason internal/config gives at the setting. What keeps a
// machine token, fresh by construction, off gestures needing a person is never
// a proof's age: it is internal/authz's `presence` on the row and
// internal/iamdomain's refusal of an administrative record written through one.
//
// THE ZERO IS INVALID: read as "no proof needed", a rule written without
// deciding would ship a step-up gesture open to a session proved last week.
// Every rule in internal/authz states one of the two; a walk refuses the zero.
type Recency string

const (
	// RecencyAny asks for no recent proof: signing in was enough. Every
	// read, and every gesture a seat's own tools make, is this — a seat has
	// no keyboard to prove anything at, and the operator's MCP surface is
	// not a step-up surface.
	RecencyAny Recency = "any"

	// RecencyStepUp asks for a proof inside `sign_in.step_up_seconds`: the
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

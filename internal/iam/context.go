package iam

import (
	"context"
	"errors"
	"slices"
)

// Resolution is what a resolver managed to say about a request.
//
// THREE VALUES, NEVER TWO. "This is who it is", "this is definitively nobody"
// and "this node could not tell" are three different facts, and collapsing the
// last two into one is the single most incident-hardened lesson carried into
// this engine — internal/coord's package doc is where it is written down,
// because treating unknown as loss tears a healthy company down over a
// two-second store blip. Here the same collapse would answer 401 to a person
// holding a perfectly good credential, and keep answering it for as long as
// the identity store was unreachable, which teaches everybody to go and check
// their password.
//
// A [Resolution] rather than an error, although an error is this tree's usual
// third value, because the CLASSIFICATION is what every gate branches on and
// only the unknown arm has a reason to carry. Making it an error would put two
// sentinels and an errors.Is at the top of every handler to recover a fact a
// closed set states outright. The reason is still there: [Reason].
type Resolution string

const (
	// Resolved: the request carried a credential and it named somebody.
	Resolved Resolution = "resolved"

	// Anonymous: the resolver ran and found DEFINITIVELY nobody — no
	// credential was presented, or the guard is off. A fact about the
	// request, and a fine answer for a surface an open read posture
	// serves.
	Anonymous Resolution = "anonymous"

	// Unknown: the question could not be answered — the identity store
	// was unreachable, a session lookup failed, or nothing resolved this
	// request at all. NOT a denial. A gate that reaches this refuses, but
	// it refuses as "ask again" rather than as "you are not who you say",
	// and it says so to whoever is watching.
	Unknown Resolution = "unknown"
)

// Resolutions are the three.
var Resolutions = []Resolution{Resolved, Anonymous, Unknown}

// Valid reports whether a resolution off the wire is one this build knows.
func (r Resolution) Valid() bool { return slices.Contains(Resolutions, r) }

// ErrUnresolved is the reason behind an [Unknown] that nobody supplied one
// for, including a context no resolver ever touched.
var ErrUnresolved = errors.New("iam: no resolver has answered for this request")

// principalKey carries the resolution down the handler chain. A struct{} key
// in this package, so nothing outside it can write one.
type principalKey struct{}

// resolution is what a context actually holds. The Principal and the failure
// travel together because exactly one of them is meaningful at a time, and two
// keys would let a later writer attach one and not the other.
type resolution struct {
	principal Principal
	how       Resolution
	why       error
}

// WithPrincipal attaches a resolved principal.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, resolution{principal: p, how: Resolved})
}

// WithAnonymous records that the resolver ran and found nobody.
//
// AN EXPLICIT CALL, because "nobody presented a credential" is a finding, and
// a finding has to be stated by whoever made it. See [From] for what silence
// means instead.
func WithAnonymous(ctx context.Context) context.Context {
	return context.WithValue(ctx, principalKey{}, resolution{how: Anonymous})
}

// WithUnresolved records that the question could not be answered, and why.
//
// The reason is kept rather than logged and dropped: the gate downstream is
// what chooses the status code, and "the store timed out" and "this session id
// decodes to nothing" send an operator to opposite places.
func WithUnresolved(ctx context.Context, why error) context.Context {
	if why == nil {
		why = ErrUnresolved
	}
	return context.WithValue(ctx, principalKey{}, resolution{how: Unknown, why: why})
}

// From returns the principal a resolver attached and HOW it got there.
//
// A CONTEXT NOBODY RESOLVED IS [Unknown], NOT [Anonymous]. The absence of an
// answer is not evidence of absence: a middleware that did not run, a handler
// reached by a path nobody wired through the guard and a caller that forgot to
// thread the context all look exactly like this, and reading any of them as
// "definitively nobody" is how a surface ends up deciding it is safe to serve
// because nothing told it otherwise. Anonymous is something a resolver says;
// silence is not.
//
// The principal is the zero value on both of the other two, so a caller that
// ignores the resolution gets a principal carrying no grants — the failure
// direction that refuses rather than the one that admits.
func From(ctx context.Context) (Principal, Resolution) {
	if ctx == nil {
		return Principal{}, Unknown
	}
	held, ok := ctx.Value(principalKey{}).(resolution)
	if !ok || !held.how.Valid() {
		return Principal{}, Unknown
	}
	if held.how != Resolved {
		return Principal{}, held.how
	}
	return held.principal, Resolved
}

// Reason returns why a resolution came back [Unknown], and nil for the other
// two. A context nobody resolved answers [ErrUnresolved], which is a different
// sentence from any store failure and is meant to be read as a wiring bug.
func Reason(ctx context.Context) error {
	if ctx == nil {
		return ErrUnresolved
	}
	held, ok := ctx.Value(principalKey{}).(resolution)
	if !ok || !held.how.Valid() {
		return ErrUnresolved
	}
	if held.how != Unknown {
		return nil
	}
	if held.why == nil {
		return ErrUnresolved
	}
	return held.why
}

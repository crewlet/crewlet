package sandbox

import (
	"errors"
	"net/http"
)

// Why is why a coding run was not started, or a box could not be reached: the
// one fact a caller decides what to say from, classed by WHAT WOULD CLEAR IT
// rather than by which backend failed.
//
// # Why a class and not the error's text
//
// The run_sandbox tool used to answer every failure with "the sandbox could
// not be started: " and the Go error after it, and told the model a wait
// would clear it. The text was whatever the chain said — an E2B status and its
// body, a container runtime's stderr, a coordination store's transport error
// naming a host — written for whoever runs the node, and the wait was a guess
// that was wrong for every configuration a person has to change. Each backend
// knows what its failure means at the point it fails (a status, a refused
// image, a setup command that exited non-zero), and nothing above it does
// without reading words, so each one MARKS its failure with a Why here and the
// tool reads the mark ([WhyOf]). The error itself goes to the node's log.
//
// AN UNMARKED FAILURE IS NOT GUESSED AT. A launch that failed for a reason no
// backend classed is the caller's to word as what it is — a condition of the
// node (a coordination store that did not answer, a context that ran out),
// or a fault of the node — never filed under the nearest Why.
type Why string

const (
	// WhyConfiguration is a run the deployment's configuration cannot
	// start, which no wait changes and a person does: a node or a seat
	// with no sandbox, a cell no backend serves, a coding agent with no
	// runner, a model or a sign-in the seat's coding agent cannot use, a run
	// environment a person has to complete or that names a credential its
	// issuer refused, a setup step that exits non-zero with any status but 75
	// ([WhySetupUnavailable]), a create the provider refuses as
	// malformed or a template it does not have, a provider domain that
	// does not resolve or whose certificate does not match it, a container
	// image the runtime will not start (a pull that failed on a network
	// blip among them, which only the runtime's own words tell apart), a
	// mount it cannot share or the box may not write, a probe of the box the
	// runtime refused to run, or a box that cannot run its shell, or find or
	// head to list a build.
	WhyConfiguration Why = "configuration"

	// WhySetupUnavailable is a setup step a command of which exited 75
	// (EX_TEMPFAIL): the company's recipe saying that a download it makes
	// is failing on the far side, and still was once the command's own
	// retries ran out. It clears when that service answers again
	// ([SetupError.why]).
	WhySetupUnavailable Why = "setup_unavailable"

	// WhyOwnSession is a seat that does its code work in its own agent
	// session (role.sandbox.run_in: self), which already has a shell, an
	// editor and a checkout: a second box would be work the turn cannot
	// see.
	WhyOwnSession Why = "own_session"

	// WhyBudget is a seat whose remaining token budget is below the floor
	// a coding run is started at (turn_engine.sandbox_min_budget_tokens):
	// a run costs a box, a clone and a toolchain before it produces a
	// token. It clears when the budget's window turns over or its ceiling
	// is raised, never by asking again sooner.
	WhyBudget Why = "budget"

	// WhyAwaitingCredential is a run whose environment is waiting for a
	// credential an integration is setting up for the seat: a value its
	// provisioning pass is minting, or the agent's GitHub App token, which
	// the engine issues once the reconcile records the app's installation.
	// It clears by itself.
	WhyAwaitingCredential Why = "awaiting_credential"

	// WhyCredentialUnavailable is a run whose environment names a
	// credential the engine issues the seat when its values are read (the
	// agent's GitHub App token), whose issuer did not answer, or answered
	// that it could not just now (a transport error, 408, its rate limit, a
	// 5xx). It clears when the issuer answers again: each launch asks for it
	// anew.
	// An issuer that REFUSED the mint (a revoked key, a suspended
	// installation) is a person's to fix, [WhyConfiguration].
	WhyCredentialUnavailable Why = "credential_unavailable"

	// WhyProviderRefused is a provider that refused the company's
	// credentials (401, 403): a person changes the key.
	WhyProviderRefused Why = "provider_refused"

	// WhyProviderLimit is a provider at a limit the company's account sets
	// — too many boxes running, too many requests (429). It clears when a
	// box ends or the window passes.
	WhyProviderLimit Why = "provider_limit"

	// WhyProviderBilling is a provider that wants the company's account
	// paid for before it starts anything (402 Payment Required): a billing
	// state a person settles, which no wait and no box ending clears. It
	// was classed a limit, so a model was told to try again later for as
	// long as its turn lasted.
	WhyProviderBilling Why = "provider_billing"

	// WhyProviderUnavailable is a provider, or the box it runs, that did
	// not answer or failed on its side (a transport error, 408, a 5xx, a
	// stream that stopped before it said how a command ended, a command the
	// box timed out or killed, a container runtime that did not answer a
	// probe in time, a read of a running box still in flight). It clears
	// when the provider answers again.
	WhyProviderUnavailable Why = "provider_unavailable"
)

// Whys is every class this build knows, in the order a reader's table lists
// them.
var Whys = []Why{
	WhyConfiguration, WhySetupUnavailable, WhyOwnSession, WhyBudget,
	WhyAwaitingCredential, WhyCredentialUnavailable, WhyProviderRefused, WhyProviderBilling,
	WhyProviderLimit, WhyProviderUnavailable,
}

// Valid reports whether w is a class this build knows.
func (w Why) Valid() bool {
	for _, known := range Whys {
		if w == known {
			return true
		}
	}
	return false
}

// Waits reports whether waiting, with nobody changing anything, can clear w.
func (w Why) Waits() bool {
	switch w {
	case WhySetupUnavailable, WhyAwaitingCredential, WhyCredentialUnavailable,
		WhyProviderLimit, WhyProviderUnavailable:
		return true
	}
	return false
}

// classed is an error that knows its [Why]. ok is false where this instance
// does not — an [E2BError] whose status says nothing about what clears it —
// and [WhyOf] then reads on down the chain.
type classed interface {
	why() (Why, bool)
}

// Because marks err as failing for why. Nil stays nil.
//
// A WRAPPER, so the mark costs err nothing: [WhyOf] reads the mark, its text
// is err's own, and errors.Is and errors.As still reach every sentinel and
// type err carries, so a caller that checks for one goes on finding it.
func Because(why Why, err error) error {
	if err == nil {
		return nil
	}
	return &becauseError{class: why, err: err}
}

// becauseError is [Because]'s mark.
type becauseError struct {
	class Why
	err   error
}

func (e *becauseError) Error() string    { return e.err.Error() }
func (e *becauseError) Unwrap() error    { return e.err }
func (e *becauseError) why() (Why, bool) { return e.class, true }

// WhyOf is the class err was marked with, the outermost mark winning, and
// false for an error nothing classed.
//
// A BOX THAT IS GONE OR PAUSED IS NEVER CLASSED HERE: each is its own
// sentinel ([ErrBoxGone], [ErrBoxPaused]) with its own answer — a gone box is
// settled at once, a paused one read later — and [boxGone] wraps the E2B 404
// that found it, which read as a status would be a template the provider does
// not have.
func WhyOf(err error) (Why, bool) {
	if err == nil || errors.Is(err, ErrBoxGone) || errors.Is(err, ErrBoxPaused) {
		return "", false
	}
	return whyIn(err)
}

// whyIn walks err's chain, both shapes of Unwrap, for the first mark that
// classes it.
//
// A WALK OF ITS OWN rather than errors.As, which stops at the first error of
// the type it asks for: a mark that declines to class ([SetupError] for a
// step the box never ran, an [E2BError] whose status says nothing) has to
// let the walk go on to the box's own answer beneath it.
func whyIn(err error) (Why, bool) {
	if c, ok := err.(classed); ok {
		if why, ok := c.why(); ok {
			return why, true
		}
	}
	if u, ok := err.(interface{ Unwrap() error }); ok {
		if next := u.Unwrap(); next != nil {
			return whyIn(next)
		}
		return "", false
	}
	if u, ok := err.(interface{ Unwrap() []error }); ok {
		for _, next := range u.Unwrap() {
			if next == nil {
				continue
			}
			if why, ok := whyIn(next); ok {
				return why, true
			}
		}
	}
	return "", false
}

// statusWhy is what an HTTP status a provider answered means for whether the
// request can succeed: its credentials refused, its account unpaid or at a
// limit, its side failing — or nothing this can class, which is false.
//
// SHARED by the control plane ([E2BError]) and a box's own agent (envd), which
// answer the same statuses for the same reasons — except a 401 or 403 from a
// box's agent, which refuses the box's own access token the engine was handed
// at its create rather than the company's key, and is no person's to fix:
// refusedIsKey says which plane answered.
func statusWhy(status int, refusedIsKey bool) (Why, bool) {
	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return WhyProviderRefused, refusedIsKey
	case status == http.StatusPaymentRequired:
		return WhyProviderBilling, true
	case status == http.StatusTooManyRequests:
		return WhyProviderLimit, true
	case status == http.StatusRequestTimeout, status >= http.StatusInternalServerError:
		return WhyProviderUnavailable, true
	}
	return "", false
}

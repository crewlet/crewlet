package sandbox

import (
	"context"
	"errors"
)

// Why is why an executor was not launched, or a box could not be reached,
// classed by WHAT WOULD CLEAR IT rather than by which backend failed.
//
// Not the error's text, which is the operator's (a runtime's stderr, a store's
// transport error) and never says whether a wait clears it.
// Each backend knows what its failure means where it fails, so it MARKS the
// failure ([Because]) and whatever tells it to a model or a person reads the
// mark ([WhyOf]).
//
// AN UNMARKED FAILURE IS NOT GUESSED AT: the caller words it as what it is (a
// node condition such as a store that did not answer or a context that ran
// out, or a node fault), never under the nearest Why.
type Why string

const (
	// WhyConfiguration is a run the deployment's configuration cannot
	// start, which no wait changes and a person does: no sandbox runtime or
	// dialable tool bridge, a cell no backend serves, a chain with no member
	// a box can run, a setup step exiting non-zero but not 75
	// ([WhySetupUnavailable]), an image the runtime holds and will not start,
	// a mount it cannot share or the box may not write, a probe the runtime
	// refused, or a box that cannot run its shell, or find or head to list a
	// build.
	WhyConfiguration Why = "configuration"

	// WhySetupUnavailable is a setup step a command of which exited 75
	// (EX_TEMPFAIL): the company's recipe saying that a download it makes
	// is failing on the far side, and still was once the command's own
	// retries ran out. It clears when that service answers again
	// ([SetupError.why]).
	WhySetupUnavailable Why = "setup_unavailable"

	// WhyProviderUnavailable is a provider, or the box it runs, that did
	// not answer or failed on its side (a command the box timed out or
	// killed, an image the runtime could not fetch, a container runtime that
	// did not answer a probe in time, a read of a running box still in
	// flight). It clears when the provider answers again.
	WhyProviderUnavailable Why = "provider_unavailable"
)

// Whys is every class this build knows, in the order a reader's table lists
// them.
var Whys = []Why{WhyConfiguration, WhySetupUnavailable, WhyProviderUnavailable}

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
	case WhySetupUnavailable, WhyProviderUnavailable:
		return true
	}
	return false
}

// classed is an error that knows its [Why]. ok is false where this instance
// does not — a [SetupError] for a step the box never ran, a [LocalError] its
// backend did not class — and [WhyOf] then reads on down the chain.
type classed interface {
	why() (Why, bool)
}

// Because marks err as failing for why. Nil stays nil.
//
// A WRAPPER, so the mark costs err nothing: its text is err's own, and
// errors.Is and errors.As still reach every sentinel and type err carries.
func Because(why Why, err error) error {
	if err == nil {
		return nil
	}
	return &becauseError{class: why, err: err}
}

// unanswered marks err, a request a provider or a box didn't answer, as
// [WhyProviderUnavailable], unless the CALLER CANCELLED it: a drain, a lost
// lease or a stopped turn is the caller's own act, and marked, a reader that
// can't see the context (runner.ErrorKind, a tool's sentence) would send an
// operator to the provider. A request its own bound ended (a command's
// timeout, a caller's deadline) is still marked: the provider didn't answer in
// it.
func unanswered(ctx context.Context, err error) error {
	if cancelledBy(ctx) {
		return err
	}
	return Because(WhyProviderUnavailable, err)
}

// cancelledBy reports whether ctx, a caller's context, was cancelled rather
// than run out of time.
func cancelledBy(ctx context.Context) bool {
	return errors.Is(ctx.Err(), context.Canceled)
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
// sentinel ([ErrBoxGone], [ErrBoxPaused]) with its own answer (a gone box is
// settled at once, a paused one read later), whatever mark a caller later
// wraps around it; classed, a reader would tell it as a launch that failed.
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
// step the box never ran) has to let the walk go on to the box's own answer
// beneath it.
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

// Package backoff is how long a repeated attempt waits: a doubling delay with
// a ceiling, and the jitter that keeps a fleet from waking in lockstep.
//
// # One rule, because the copies had already started to differ in form
//
// The integration reconcile loop doubled in a loop that exits at the ceiling,
// the broker's redelivery delay shifted a duration by a count read off the
// wire, and the config reconcile poll and the sandbox waiter each spread
// their interval by a fifth with their own arithmetic. The four agreed on
// the answer and disagreed on how they got there, which is the stage at
// which a fifth copy (the tool-skill walk's retry was about to be one) is
// the copy that gets the overflow wrong. Each rule here is the one its
// callers had already converged on, written once.
//
// A leaf: it imports nothing from the engine, so any layer may use it.
package backoff

import (
	"math/rand/v2"
	"time"
)

// Doubling returns base doubled once per attempt after the first, capped at
// ceiling.
//
// attempt counts from 1, and anything below 1 is the first attempt rather
// than an instant retry: a caller that has not counted yet must not be handed
// a zero wait.
//
// DOUBLED IN A LOOP THAT EXITS AT THE CEILING rather than computed as
// base<<(attempt-1), and the difference is not style. An attempt count
// accumulates for as long as a dependency stays down (or arrives off the wire,
// where it is whatever a peer sent), and the closed form overflows
// time.Duration well before that count becomes unreasonable. An overflowed
// duration is NEGATIVE, which every caller reads as "already due", so the
// longest wait in the system would turn into a request every tick against
// the dependency least likely to answer differently. Exiting at the ceiling
// also bounds the loop without a magic iteration cap: it runs
// log2(ceiling/base) times however large attempt grows.
//
// A ceiling at or below base is the ceiling. A base that is not positive is
// also the ceiling, because doubling zero is zero for ever and a retry loop
// handed that would spin; the long wait is the safe reading of a nonsense
// argument.
func Doubling(attempt int, base, ceiling time.Duration) time.Duration {
	if base <= 0 || base >= ceiling {
		return ceiling
	}
	delay := base
	for i := 1; i < attempt; i++ {
		if delay >= ceiling {
			return ceiling
		}
		delay *= 2
	}
	return min(delay, ceiling)
}

// Jitter spreads d uniformly across plus or minus fraction of itself.
//
// For an INTERVAL rather than for an action: a fleet whose nodes booted
// together would otherwise poll, walk or retry against one dependency on the
// same second for the life of the deployment, and a dependency recovering from
// an outage meets every node at once. The spread is uniform so the expected
// interval is still d, which is the number its caller justified.
//
// A fraction outside (0, 1) is clamped into it: a negative spread is no
// spread, and a spread of the whole interval or more could return a wait of
// zero or less, which reads as "already due".
func Jitter(d time.Duration, fraction float64) time.Duration {
	if d <= 0 || fraction <= 0 {
		return d
	}
	fraction = min(fraction, maxFraction)
	spread := float64(d) * fraction
	// math/rand/v2 rather than crypto/rand: this spreads wake-ups, and
	// nothing about a node's schedule is a secret an attacker profits from
	// predicting.
	return d + time.Duration((rand.Float64()*2-1)*spread)
}

// maxFraction keeps the lowest jittered wait a tenth of the interval.
//
// Below one, strictly, so a jittered wait is always positive; a tenth rather
// than a hair under one, so even a caller that asks for the widest spread
// still waits a meaningful part of the interval it named.
const maxFraction = 0.9

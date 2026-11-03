package googlecloud

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Google's IAM reads are eventually consistent after a write: a new account
// can be ungrantable on another project's policy, and a create, a binding, a
// disable, an enable or a delete can read back stale. The live probe measured
// each settling in seconds to under two minutes; Google documents propagation
// as typically two and up to seven minutes for an effective grant, which no
// read here waits on.
const (
	// observeFirst is the first wait, doubling to observeCap.
	observeFirst = 2 * time.Second
	observeCap   = 30 * time.Second
	// observeBudget bounds one wait: past it the read-back is a fault the
	// next pass retries.
	observeBudget = 3 * time.Minute
)

// clock is the time a client reads and waits by: the wall clock in
// production, a test's own so a three-minute bound passes in no time.
type clock struct {
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

// wallClock is the real time, waits ending early with their context.
func wallClock() clock {
	return clock{
		now: func() time.Time { return time.Now().UTC() },
		sleep: func(ctx context.Context, d time.Duration) error {
			timer := time.NewTimer(d)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
	}
}

// readBack is one kind of read that follows the engine's own write, by what
// it waits to see.
type readBack string

const (
	readCreated    readBack = "the new account to read back"
	readGrantable  readBack = "the new account to become grantable on a project"
	readDelegated  readBack = "Crewlet's Token Creator grant on the account to read back"
	readBindings   readBack = "the account's roles to read back as written"
	readDisabled   readBack = "the account to read back as disabled"
	readEnabled    readBack = "the account to read back as enabled"
	readDeleted    readBack = "the deleted account to read back as gone"
	readLastWrites readBack = "the account to read back as the last sync left it"
)

// The two sentences a seat's tool waits on while Google settles, which the
// Agents tab draws as "Waiting: <reason>".
const (
	ReasonCreating = "Google is still creating the agent's account"
	ReasonApplying = "Google is still applying the change"
)

// reason is what a seat waits on while this read settles.
func (r readBack) reason() string {
	if r == readCreated || r == readGrantable || r == readDelegated {
		return ReasonCreating
	}
	return ReasonApplying
}

// Settling is a read-back that had not shown what the engine's write did when
// its bound ran out: Google's delay, never a refusal, so the next pass retries
// it. Reason is the sentence a seat waits on meanwhile.
type Settling struct {
	What   string
	Reason string
	Waited time.Duration
	Last   error
}

func (s *Settling) Error() string {
	msg := fmt.Sprintf("googlecloud: %s: %s did not happen within %s", s.Reason, s.What, s.Waited.Round(time.Second))
	if s.Last != nil {
		msg += ": " + s.Last.Error()
	}
	return msg
}

func (s *Settling) Unwrap() error { return s.Last }

// SettlingReason is the sentence a seat waits on where err is Google still
// settling the engine's write, and false for any other error.
func SettlingReason(err error) (string, bool) {
	if s, ok := errors.AsType[*Settling](err); ok {
		return s.Reason, true
	}
	return "", false
}

// awaitObserved calls check until it reports done, starting after observeFirst
// and doubling to observeCap, and answers how long it took. Done with an error
// is final at once; past observeBudget it answers a [*Settling] naming the last
// thing check saw. A context ended first answers the context's error.
func (c clock) awaitObserved(ctx context.Context, what readBack, check func() (bool, error)) (time.Duration, error) {
	start := c.now()
	wait := observeFirst
	for {
		done, err := check()
		if done {
			return c.now().Sub(start), err
		}
		if c.now().Sub(start)+wait > observeBudget {
			waited := c.now().Sub(start)
			return waited, &Settling{What: string(what), Reason: what.reason(), Waited: waited, Last: err}
		}
		if serr := c.sleep(ctx, wait); serr != nil {
			return c.now().Sub(start), serr
		}
		wait = min(wait*2, observeCap)
	}
}

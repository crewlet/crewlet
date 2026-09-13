package integration

import "time"

// Schedule is how long each outcome waits before the next pass.
//
// Derived from WHO has to act, never from what failed. That is the whole
// design: a third-party app applying a grant it already accepted finishes in seconds,
// a person told to install an app is usually installing it as they read, and
// a ${VAR} nobody set will still be unset in an hour. Three different waits,
// and a single retry interval would be wrong for all three at once.
type Schedule struct {
	// Settled is how long a converged integration is trusted before it is
	// read back. This is the ONLY thing that finds access removed by hand
	// in the third-party app's own console, so it cannot be forever.
	Settled time.Duration

	// WaitingBase is the first retry delay for work the engine or the
	// third-party app is doing, doubled per attempt up to WaitingMax.
	WaitingBase time.Duration
	WaitingMax  time.Duration

	// AdminBase and AdminMax bound the wait for something a person must do
	// AT THE THIRD-PARTY APP. Short at first, because they are usually
	// doing it as they read, and the point of the loop is that it resumes
	// the moment they finish without them pressing anything. Stretching out
	// is what stops a tab left open overnight costing a request every
	// fifteen seconds until morning.
	AdminBase time.Duration
	AdminMax  time.Duration

	// Operator is how often an integration waiting on THIS DEPLOYMENT'S own
	// configuration is retried. Flat and long: nothing at the third-party app will
	// ever change it, so backing off buys nothing and asking often only
	// spends requests against a credential that does not work.
	Operator time.Duration
}

// DefaultSchedule is tuned to what the third-party apps actually do.
//
// Every value here is anchored to an observable rather than chosen for
// roundness:
//
//   - Settled at ten minutes is the horizon on which an administrator who
//     revokes an agent's access by hand is noticed.
//
//     What a converged pass COSTS is the other half of that choice, and it is
//     not "a handful of API calls", which is what this said while three of the
//     seven reconcilers were writing at their vendor on every pass. Every one
//     of them is certified against integrationtest now, so the write half is
//     zero by construction. The READ half scales with the company: a pass
//     asks each seat's own credential who it is, and the membership and hook
//     reads that replaced the blind writes are per seat and per project. Call
//     it O(seats x projects) requests every ten minutes, per surface — tens
//     for a small company, a few hundred for a large one on GitLab or
//     Mattermost. That is affordable at this interval and would not be at one.
//     A surface whose reads are rate limited hard enough to care can take a
//     settled interval of its own; none does today.
//
//   - WaitingBase at thirty seconds and WaitingMax at five minutes bracket
//     what a grant takes to land: seconds to low minutes on every third-party app
//     here. Starting shorter would poll a propagation delay, and capping
//     higher would leave a company idle long after the third-party app was done.
//
//   - AdminBase at fifteen seconds is fast enough that "install the app"
//     followed by installing the app looks immediate. AdminMax at ten
//     minutes is where a person who has not acted in ten minutes is not
//     acting right now.
//
//   - Operator at one hour catches a credential somebody fixed without
//     telling anyone, at a cost of twenty-four failed calls a day.
var DefaultSchedule = Schedule{
	Settled:     10 * time.Minute,
	WaitingBase: 30 * time.Second,
	WaitingMax:  5 * time.Minute,
	AdminBase:   15 * time.Second,
	AdminMax:    10 * time.Minute,
	Operator:    time.Hour,
}

// WithDefaults fills anything left unset.
//
// A zero duration would otherwise mean "look again immediately", so a field
// nobody wired becomes a pass every tick against a third-party app's API. Falling back
// is the difference between a missing setting being a missing setting and it
// being a self-inflicted rate limit.
func (s Schedule) WithDefaults() Schedule {
	if s.Settled <= 0 {
		s.Settled = DefaultSchedule.Settled
	}
	if s.WaitingBase <= 0 {
		s.WaitingBase = DefaultSchedule.WaitingBase
	}
	if s.WaitingMax <= 0 {
		s.WaitingMax = DefaultSchedule.WaitingMax
	}
	if s.AdminBase <= 0 {
		s.AdminBase = DefaultSchedule.AdminBase
	}
	if s.AdminMax <= 0 {
		s.AdminMax = DefaultSchedule.AdminMax
	}
	if s.Operator <= 0 {
		s.Operator = DefaultSchedule.Operator
	}
	return s
}

// Next returns how long to wait after a report, given how many consecutive
// passes have not settled.
//
// ONE SCHEDULE FOR EVERY SURFACE. A per-surface override lived here, justified
// by Slack's app-manifest methods being rate limited to about one request a
// minute — but Slack registers no reconciler at all, so the surface it was
// written for could never have used it, and nothing ever set it. It is
// collapsed rather than left half-wired: a knob with no caller is
// indistinguishable to the next reader from one whose caller nobody found.
//
// If a surface with a real reconciler and a measured rate limit needs its own
// interval, this is where it goes back — with that surface setting it.
func (s Schedule) Next(report Report, attempts int) time.Duration {
	switch {
	case report.Phase == PhaseReady:
		return s.Settled
	case report.Actor == ActorAdmin:
		return backoff(attempts, s.AdminBase, s.AdminMax)
	case report.Actor == ActorOperator:
		return s.Operator
	default:
		return backoff(attempts, s.WaitingBase, s.WaitingMax)
	}
}

// Cadence names which of the four waits a report falls under.
//
// It exists so [Observe] can tell that the WAIT ITSELF changed. Attempts drive
// the backoff, and a backoff is only meaningful within one class: a surface
// that spent ten ticks waiting on the engine has an attempt count that says
// nothing at all about how long to wait for a PERSON.
type Cadence string

// The four waits, one per branch of [Schedule.Next].
const (
	CadenceSettled  Cadence = "settled"
	CadenceAdmin    Cadence = "admin"
	CadenceOperator Cadence = "operator"
	CadenceWaiting  Cadence = "waiting"
)

// CadenceOf is which wait a report is on. It mirrors [Schedule.Next]'s own
// switch, and the two must agree — a class this did not distinguish would let
// a cadence change without resetting the count that paces it.
func CadenceOf(report Report) Cadence {
	switch {
	case report.Phase == PhaseReady:
		return CadenceSettled
	case report.Actor == ActorAdmin:
		return CadenceAdmin
	case report.Actor == ActorOperator:
		return CadenceOperator
	default:
		return CadenceWaiting
	}
}

// backoff doubles base per consecutive unsettled attempt, capped at ceiling.
//
// DOUBLED IN A LOOP THAT EXITS AT THE CEILING rather than computed as
// base<<(attempts-1), and the difference is not style. A row parked on a
// person accumulates attempts for as long as they do not act, and the closed
// form overflows time.Duration well before that count becomes unreasonable.
// An overflowed duration is not merely wrong: it is NEGATIVE, which every
// caller here reads as "already due", so the longest wait in the system turns
// into a request every tick against the third-party app least likely to answer
// differently.
//
// Exiting at the ceiling also bounds the loop without a magic iteration cap:
// delay is strictly below ceiling whenever it is doubled, so the next value
// cannot exceed twice a duration somebody wrote in a config file, and the
// loop runs log2(ceiling/base) times however large attempts grows.
func backoff(attempts int, base, ceiling time.Duration) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	delay := base
	for i := 1; i < attempts; i++ {
		if delay >= ceiling {
			return ceiling
		}
		delay *= 2
	}
	if delay > ceiling {
		return ceiling
	}
	return delay
}

package integration

import "time"

// Schedule is how long each outcome waits before the next pass.
//
// Derived from WHO has to act, never from what failed. That is the whole
// design: a vendor applying a grant it already accepted finishes in seconds,
// a person told to install an app is usually installing it as they read, and
// a ${VAR} nobody set will still be unset in an hour. Three different waits,
// and a single retry interval would be wrong for all three at once.
type Schedule struct {
	// Settled is how long a converged integration is trusted before it is
	// read back. This is the ONLY thing that finds access removed by hand
	// in the vendor's own console, so it cannot be forever.
	Settled time.Duration

	// WaitingBase is the first retry delay for work the engine or the
	// vendor is doing, doubled per attempt up to WaitingMax.
	WaitingBase time.Duration
	WaitingMax  time.Duration

	// AdminBase and AdminMax bound the wait for something a person must do
	// AT THE VENDOR. Short at first, because they are usually doing it as
	// they read, and the point of the loop is that it resumes the moment
	// they finish without them pressing anything. Stretching out is what
	// stops a tab left open overnight costing a request every fifteen
	// seconds until morning.
	AdminBase time.Duration
	AdminMax  time.Duration

	// Operator is how often an integration waiting on THIS DEPLOYMENT'S own
	// configuration is retried. Flat and long: nothing at the vendor will
	// ever change it, so backing off buys nothing and asking often only
	// spends requests against a credential that does not work.
	Operator time.Duration
}

// DefaultSchedule is tuned to what the vendors actually do.
//
// Every value here is anchored to an observable rather than chosen for
// roundness:
//
//   - Settled at ten minutes is the horizon on which an administrator who
//     revokes an agent's access by hand is noticed. It is also what a
//     converged pass costs: each vendor's reconcile is written to issue no
//     writes and few reads when nothing has changed, so this is a handful of
//     API calls six times an hour.
//   - WaitingBase at thirty seconds and WaitingMax at five minutes bracket
//     what a grant takes to land: seconds to low minutes on every vendor
//     here. Starting shorter would poll a propagation delay, and capping
//     higher would leave a company idle long after the vendor was done.
//   - AdminBase at fifteen seconds is fast enough that "install the app"
//     followed by installing the app looks immediate. AdminMax at ten
//     minutes is where a person who has not acted in ten minutes is not
//     acting right now.
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
// nobody wired becomes a pass every tick against a vendor's API. Falling back
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
// settled overrides [Schedule.Settled] for one vendor and is zero when that
// vendor has no reason to differ. It exists because the cost of a converged
// pass is not comparable across surfaces: Slack's app-manifest methods are
// rate limited to roughly one request a minute, so re-reading twenty seats
// costs twenty minutes of waiting, while GitLab answers the same question in
// one listing. One number would either hammer Slack or let everything else
// drift for an hour.
func (s Schedule) Next(report Report, attempts int, settled time.Duration) time.Duration {
	switch {
	case report.Phase == PhaseReady:
		if settled > 0 {
			return settled
		}
		return s.Settled
	case report.Actor == ActorAdmin:
		return backoff(attempts, s.AdminBase, s.AdminMax)
	case report.Actor == ActorOperator:
		return s.Operator
	default:
		return backoff(attempts, s.WaitingBase, s.WaitingMax)
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
// into a request every tick against the vendor least likely to answer
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

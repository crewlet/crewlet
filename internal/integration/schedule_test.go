package integration

import (
	"testing"
	"time"
)

// The cadence is derived from WHO has to act, not from what failed. Two
// reports in the same phase and two in different phases both come apart on
// the actor, which is the property the whole schedule rests on.
func TestNextTurnsOnTheActor(t *testing.T) {
	s := DefaultSchedule
	degradedAdmin := Report{Phase: PhaseDegraded, Actor: ActorAdmin}
	degradedOperator := Report{Phase: PhaseDegraded, Actor: ActorOperator}

	admin := s.Next(degradedAdmin, 1)
	operator := s.Next(degradedOperator, 1)
	if admin != s.AdminBase {
		t.Fatalf("a first admin wait is %s, want %s", admin, s.AdminBase)
	}
	if operator != s.Operator {
		t.Fatalf("an operator wait is %s, want the flat %s", operator, s.Operator)
	}
	if admin >= operator {
		t.Fatalf("a person acting at the third-party app (%s) is not watched more "+
			"closely than a config nobody has edited (%s)", admin, operator)
	}
}

// An operator wait is FLAT. Backing it off buys nothing, because nothing at
// the third-party app will ever change a ${VAR} this deployment did not set, and a
// stretching interval only delays noticing the moment somebody does.
func TestOperatorWaitDoesNotBackOff(t *testing.T) {
	s := DefaultSchedule
	report := Report{Phase: PhaseUnconfigured, Actor: ActorOperator}
	for _, attempts := range []int{1, 2, 10, 1000} {
		if got := s.Next(report, attempts); got != s.Operator {
			t.Fatalf("after %d attempts the operator wait is %s, want %s",
				attempts, got, s.Operator)
		}
	}
}

// A wait on a person starts brisk and stretches out. Somebody told to install
// an app is usually installing it as they read; somebody who has not acted in
// ten minutes is not acting right now.
func TestAdminWaitDoublesAndCaps(t *testing.T) {
	s := DefaultSchedule
	report := Report{Phase: PhaseAwaitingAdmin, Actor: ActorAdmin}

	var previous time.Duration
	for attempts := 1; attempts <= 20; attempts++ {
		got := s.Next(report, attempts)
		if got <= 0 {
			t.Fatalf("attempt %d produced a wait of %s, which every caller "+
				"reads as already due", attempts, got)
		}
		if got > s.AdminMax {
			t.Fatalf("attempt %d produced %s, past the %s ceiling", attempts, got, s.AdminMax)
		}
		if got < previous {
			t.Fatalf("attempt %d produced %s, shorter than the previous %s",
				attempts, got, previous)
		}
		previous = got
	}
	if previous != s.AdminMax {
		t.Fatalf("twenty attempts settled at %s, want the %s ceiling", previous, s.AdminMax)
	}
}

// A row parked on a person for a long time accumulates attempts. The closed
// form base<<(attempts-1) overflows time.Duration well before that count
// becomes unreasonable, and an overflowed duration is NEGATIVE, which reads
// as already due: the longest wait in the system becomes a request every
// tick.
func TestBackoffNeverGoesNegative(t *testing.T) {
	for _, attempts := range []int{1, 32, 62, 63, 64, 1000, 1 << 20} {
		got := backoff(attempts, 30*time.Second, 5*time.Minute)
		if got <= 0 {
			t.Fatalf("%d attempts produced %s", attempts, got)
		}
		if got > 5*time.Minute {
			t.Fatalf("%d attempts produced %s, past the ceiling", attempts, got)
		}
	}
}

// Zero and negative attempt counts are the first attempt, not an instant
// retry. A caller that has not counted yet must not be handed a zero wait.
func TestBackoffTreatsNoAttemptsAsTheFirst(t *testing.T) {
	base := 30 * time.Second
	for _, attempts := range []int{-1, 0, 1} {
		if got := backoff(attempts, base, time.Hour); got != base {
			t.Fatalf("%d attempts produced %s, want the base %s", attempts, got, base)
		}
	}
}

// A SETTLED SURFACE TAKES THE SHARED INTERVAL, and there is only one.
//
// A per-surface override lived here, justified by Slack's app-manifest rate
// limit — but Slack registers no reconciler, so the surface it was written for
// could never have used it and nothing ever set it. It is collapsed rather
// than left half-wired; this pins that there is one cadence, so re-adding a
// knob means re-adding a caller with it.
func TestASettledSurfaceTakesTheSharedInterval(t *testing.T) {
	s := DefaultSchedule
	if got := s.Next(Ready(), 0); got != s.Settled {
		t.Fatalf("a settled surface waited %s, want the shared %s", got, s.Settled)
	}
	// And a surface still working takes the waiting cadence, not the
	// settled one, however many passes it has had.
	working := Report{Phase: PhaseProvisioning, Actor: ActorEngine}
	if got := s.Next(working, 1); got != s.WaitingBase {
		t.Fatalf("a provisioning surface waited %s, want the %s waiting base",
			got, s.WaitingBase)
	}
}

// A zero duration means "look again immediately", so a schedule nobody wired
// would be a pass every tick against every third-party app's API.
func TestWithDefaultsFillsEveryField(t *testing.T) {
	got := Schedule{}.WithDefaults()
	if got != DefaultSchedule {
		t.Fatalf("an empty schedule filled to %+v, want %+v", got, DefaultSchedule)
	}
	// A value somebody set is kept.
	custom := Schedule{Settled: time.Minute}.WithDefaults()
	if custom.Settled != time.Minute {
		t.Fatalf("a set Settled became %s", custom.Settled)
	}
	if custom.Operator != DefaultSchedule.Operator {
		t.Fatalf("setting one field cleared another: Operator is %s", custom.Operator)
	}
}

// The shipped defaults have to be internally consistent, or the loop's
// behaviour stops matching what the doc comment claims.
func TestDefaultScheduleIsOrdered(t *testing.T) {
	s := DefaultSchedule
	switch {
	case s.AdminBase >= s.AdminMax:
		t.Errorf("AdminBase %s is not below AdminMax %s", s.AdminBase, s.AdminMax)
	case s.WaitingBase >= s.WaitingMax:
		t.Errorf("WaitingBase %s is not below WaitingMax %s", s.WaitingBase, s.WaitingMax)
	case s.AdminBase >= s.WaitingBase:
		t.Errorf("a person acting now (%s) is watched no more closely than a "+
			"third-party app applying a grant (%s)", s.AdminBase, s.WaitingBase)
	case s.Operator <= s.AdminMax:
		t.Errorf("an unedited config (%s) is retried as often as a person "+
			"mid-install (%s)", s.Operator, s.AdminMax)
	}
	// Interval has to be no coarser than the finest wait the schedule can
	// ask for, or that wait is a claim the loop cannot keep.
	if Interval > s.AdminBase {
		t.Errorf("the loop ticks every %s but the schedule asks for %s",
			Interval, s.AdminBase)
	}
}

// A CHANGE OF WAIT RESTARTS THE BACKOFF.
//
// Attempts pace the backoff and [Schedule.Next] reads them against whichever
// wait the row is NOW on, so a count accumulated under one wait was carried
// straight into another. A surface that spent ten ticks waiting on the engine
// entered the wait for a PERSON already at its ceiling.
//
// That is the one cadence where the ceiling is wrong. The brisk admin
// interval exists so an operator who installs an app sees provisioning
// continue without pressing anything, and inherited attempts skipped every
// fast retry: the screen would not move for ten minutes.
func TestTheBackoffRestartsWhenTheWaitChanges(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	schedule := Schedule{}.WithDefaults()

	// Ten passes waiting on the engine.
	state := State{Kind: KindGitHub}
	for range 10 {
		state, _ = Observe(state, KindGitHub,
			[]Finding{{Kind: FindingIngressPending}}, nil, now)
	}
	if state.Attempts < 10 {
		t.Fatalf("attempts = %d, want the engine wait to have accumulated", state.Attempts)
	}
	waiting := schedule.Next(state.Report, state.Attempts)
	if waiting != schedule.WaitingMax {
		t.Fatalf("the engine wait is %s, want it at its ceiling %s", waiting, schedule.WaitingMax)
	}

	// The same surface now waits on a PERSON.
	state, _ = Observe(state, KindGitHub,
		[]Finding{{Kind: FindingApprovalRequired}}, nil, now)

	admin := schedule.Next(state.Report, state.Attempts)
	if admin > 2*schedule.AdminBase {
		t.Errorf("the first wait for a person is %s, want it near %s: an operator "+
			"installing the app sees nothing happen for %s",
			admin, schedule.AdminBase, admin)
	}
}

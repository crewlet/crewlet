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

	admin := s.next(degradedAdmin, 1, 0)
	operator := s.next(degradedOperator, 1, 0)
	if admin != s.AdminBase {
		t.Fatalf("a first admin wait is %s, want %s", admin, s.AdminBase)
	}
	if operator != s.Operator {
		t.Fatalf("an operator wait is %s, want the flat %s", operator, s.Operator)
	}
	if admin >= operator {
		t.Fatalf("a person acting at the vendor (%s) is not watched more "+
			"closely than a config nobody has edited (%s)", admin, operator)
	}
}

// An operator wait is FLAT. Backing it off buys nothing, because nothing at
// the vendor will ever change a ${VAR} this deployment did not set, and a
// stretching interval only delays noticing the moment somebody does.
func TestOperatorWaitDoesNotBackOff(t *testing.T) {
	s := DefaultSchedule
	report := Report{Phase: PhaseUnconfigured, Actor: ActorOperator}
	for _, attempts := range []int{1, 2, 10, 1000} {
		if got := s.next(report, attempts, 0); got != s.Operator {
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
		got := s.next(report, attempts, 0)
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

// A settled surface takes its own interval when it has one. Slack's
// app-manifest methods are rate limited to roughly one request a minute, so
// re-reading twenty seats on the shared ten-minute cadence would spend the
// whole interval waiting on a rate limit.
func TestSettledOverrideAppliesOnlyWhenReady(t *testing.T) {
	s := DefaultSchedule
	const slack = 2 * time.Hour

	if got := s.next(Ready(), 0, slack); got != slack {
		t.Fatalf("a settled surface waited %s, want its own %s", got, slack)
	}
	if got := s.next(Ready(), 0, 0); got != s.Settled {
		t.Fatalf("a surface with no override waited %s, want the shared %s", got, s.Settled)
	}
	// The override is a SETTLED interval. A surface that is waiting on the
	// vendor must not inherit it, or a rate-limited vendor would also be
	// the slowest one to finish provisioning.
	working := Report{Phase: PhaseProvisioning, Actor: ActorEngine}
	if got := s.next(working, 1, slack); got != s.WaitingBase {
		t.Fatalf("a provisioning surface waited %s, want the %s waiting base",
			got, s.WaitingBase)
	}
}

// A zero duration means "look again immediately", so a schedule nobody wired
// would be a pass every tick against every vendor's API.
func TestWithDefaultsFillsEveryField(t *testing.T) {
	got := Schedule{}.withDefaults()
	if got != DefaultSchedule {
		t.Fatalf("an empty schedule filled to %+v, want %+v", got, DefaultSchedule)
	}
	// A value somebody set is kept.
	custom := Schedule{Settled: time.Minute}.withDefaults()
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
			"vendor applying a grant (%s)", s.AdminBase, s.WaitingBase)
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

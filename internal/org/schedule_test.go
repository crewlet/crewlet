package org

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestScheduleDefaults(t *testing.T) {
	t.Parallel()
	// A schedule nobody annotated fires, catches up one missed tick, and
	// is capped — none of which an operator should have to write out.
	var s Schedule
	if !s.IsEnabled() {
		t.Error("an unannotated schedule is disabled")
	}
	if !s.CatchesUp() {
		t.Error("an unannotated schedule does not catch up")
	}
	if got := s.Timeout(); got != DefaultScheduleTimeout {
		t.Errorf("Timeout() = %v, want %v", got, DefaultScheduleTimeout)
	}
	if s.TargetsLead() {
		t.Error("an unannotated unit schedule targets the lead")
	}
	held := Schedule{Enabled: Off(), Catchup: Off(), TimeoutSeconds: 30}
	if held.IsEnabled() || held.CatchesUp() {
		t.Error("an explicit false did not stick")
	}
	if got := held.Timeout(); got != 30*time.Second {
		t.Errorf("Timeout() = %v, want 30s", got)
	}
}

func TestScheduleValidation(t *testing.T) {
	t.Parallel()
	valid := Schedule{Name: "standup", Cron: "0 9 * * 1-5", Task: "post the standup"}
	for _, tc := range []struct {
		name     string
		mutate   func(*Schedule)
		wantErr  bool
		mentions string
	}{
		{name: "valid", mutate: func(*Schedule) {}},
		{name: "valid with a zone", mutate: func(s *Schedule) { s.Timezone = "Europe/Amsterdam" }},
		{name: "valid lead target", mutate: func(s *Schedule) { s.Target = TargetLead }},
		{name: "no name", mutate: func(s *Schedule) { s.Name = " " }, wantErr: true, mentions: "name"},
		{name: "no task", mutate: func(s *Schedule) { s.Task = "" }, wantErr: true, mentions: "task"},
		{name: "four cron fields", mutate: func(s *Schedule) { s.Cron = "0 9 * *" }, wantErr: true, mentions: "cron"},
		{name: "six cron fields", mutate: func(s *Schedule) { s.Cron = "0 0 9 * * 1" }, wantErr: true, mentions: "cron"},
		{name: "unknown zone", mutate: func(s *Schedule) { s.Timezone = "Mars/Olympus" }, wantErr: true, mentions: "timezone"},
		{name: "negative timeout", mutate: func(s *Schedule) { s.TimeoutSeconds = -1 }, wantErr: true, mentions: "timeout_seconds"},
		{
			// Pinning a unit schedule to one seat is a role schedule wearing
			// a disguise, and the error says so.
			name: "role-shaped target", mutate: func(s *Schedule) { s.Target = "Tech Lead" },
			wantErr: true, mentions: "each or lead",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := valid
			tc.mutate(&s)
			err := s.Validate("unit \"Team\"")
			switch {
			case tc.wantErr && !errors.Is(err, ErrInvalidSchedule):
				t.Fatalf("Validate() = %v, want ErrInvalidSchedule", err)
			case !tc.wantErr && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			}
			if tc.mentions != "" && !strings.Contains(err.Error(), tc.mentions) {
				t.Errorf("error does not mention %q: %v", tc.mentions, err)
			}
		})
	}
}

// TestDuplicateScheduleNamesRejected: the name is half the idempotency key,
// so two schedules sharing one would each suppress the other's fire.
func TestDuplicateScheduleNamesRejected(t *testing.T) {
	t.Parallel()
	standup := Schedule{Name: "standup", Cron: "0 9 * * *", Task: "post"}
	r := &Role{Name: "Dev", Schedules: []Schedule{standup, standup}}
	if err := r.Validate(); !errors.Is(err, ErrInvalidSchedule) {
		t.Errorf("role Validate() = %v, want ErrInvalidSchedule", err)
	}
	u := &OrgUnit{Name: "Team", Roles: []*Role{{Name: "Dev"}}, Schedules: []Schedule{standup, standup}}
	if err := u.Validate(); !errors.Is(err, ErrInvalidSchedule) {
		t.Errorf("unit Validate() = %v, want ErrInvalidSchedule", err)
	}
}

// TestFanOutNeedsADirectAgentMember: `each` never reaches descendants and
// never wakes a human, so a unit without a direct agent member has nothing
// to fan out to — and would silently no-op every minute it was due.
func TestFanOutNeedsADirectAgentMember(t *testing.T) {
	t.Parallel()
	standup := Schedule{Name: "standup", Cron: "0 9 * * *", Task: "post the standup"}
	for _, tc := range []struct {
		name    string
		unit    *OrgUnit
		wantErr bool
	}{
		{
			name:    "humans only",
			unit:    &OrgUnit{Name: "Team", Roles: []*Role{human()}, Schedules: []Schedule{standup}},
			wantErr: true,
		},
		{
			name: "agents live in a child unit only",
			unit: &OrgUnit{
				Name: "Dept", Schedules: []Schedule{standup},
				Children: []*OrgUnit{{Name: "Team", Roles: []*Role{{Name: "Dev"}}}},
			},
			wantErr: true,
		},
		{
			name: "mixed unit is fine — the fan-out skips the human",
			unit: &OrgUnit{Name: "Team", Roles: []*Role{human(), {Name: "Dev"}}, Schedules: []Schedule{standup}},
		},
		{
			name: "a lead target does not fan out at all",
			unit: &OrgUnit{
				Name: "Dept", Lead: "Dev",
				Schedules: []Schedule{{Name: "report", Cron: "0 17 * * 5", Task: "report", Target: TargetLead}},
				Children:  []*OrgUnit{{Name: "Team", Roles: []*Role{{Name: "Dev"}}}},
			},
		},
		{
			name: "a disabled schedule is config an operator is holding",
			unit: &OrgUnit{
				Name: "Team", Roles: []*Role{human()},
				Schedules: []Schedule{{Name: "standup", Cron: "0 9 * * *", Task: "post", Enabled: Off()}},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.unit.Validate()
			if got := errors.Is(err, ErrUnrunnableSchedule); got != tc.wantErr {
				t.Errorf("ErrUnrunnableSchedule = %v, want %v (err: %v)", got, tc.wantErr, err)
			}
		})
	}
}

// TestRoleScheduleIgnoresTarget: the runner of a role schedule is always
// that role, so a target there is inert rather than wrong.
func TestRoleScheduleIgnoresTarget(t *testing.T) {
	t.Parallel()
	r := &Role{Name: "Dev", Schedules: []Schedule{
		{Name: "report", Cron: "0 17 * * 5", Task: "weekly report", Target: TargetLead},
	}}
	if err := r.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}

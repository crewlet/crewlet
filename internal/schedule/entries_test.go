package schedule_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/schedule"
)

// Ported from tests/test_schedule/test_describe.py, plus the resolved
// next-run the Python left to its API layer.

func describeOrg() *org.Organization {
	return &org.Organization{
		Name: "Acme",
		Roles: []*org.Role{{
			Name: "Ops", DeclaredHandle: "ops",
			Schedules: []org.Schedule{{Name: "nightly", Cron: "0 2 * * *", Task: "rotate"}},
		}},
		Units: []*org.OrgUnit{{
			Name: "Quality", Type: org.UnitTypeTeam, Lead: "QA Lead",
			Roles: []*org.Role{
				{Name: "QA Lead", DeclaredHandle: "qa-lead"},
				{Name: "QA Dev", DeclaredHandle: "qa-dev"},
			},
			Schedules: []org.Schedule{
				{Name: "standup", Cron: "30 9 * * 1-5", Task: "standup"},
				{Name: "report", Cron: "0 16 * * 5", Task: "report", Target: org.TargetLead},
			},
		}},
	}
}

func rowsByName(t *testing.T, rows []schedule.Row) map[string]schedule.Row {
	t.Helper()
	out := map[string]schedule.Row{}
	for _, r := range rows {
		if _, dup := out[r.Name]; dup {
			t.Fatalf("two rows named %q", r.Name)
		}
		out[r.Name] = r
	}
	return out
}

func TestDescribeShapeAndDefaults(t *testing.T) {
	t.Parallel()
	rows := schedule.Describe(describeOrg(), schedule.DescribeOptions{
		DefaultTimezone: "Europe/Amsterdam",
		Now:             time.Date(2026, time.June, 8, 0, 0, 0, 0, time.UTC),
	})
	byName := rowsByName(t, rows)

	nightly := byName["nightly"]
	if nightly.ScopeType != types.ScheduleScopeRole || nightly.ScopeID != "ops" {
		t.Errorf("nightly scope = %s/%s, want role/ops", nightly.ScopeType, nightly.ScopeID)
	}
	// A role schedule's target is not "each" — it is meaningless, and
	// reporting a default would invite a reader to change it and expect
	// something to happen.
	if nightly.Target != "" {
		t.Errorf("nightly target = %q, want empty for a role schedule", nightly.Target)
	}
	if !slices.Equal(nightly.Runners, []string{"ops"}) {
		t.Errorf("nightly runners = %v, want [ops]", nightly.Runners)
	}
	if nightly.Timezone != "Europe/Amsterdam" {
		t.Errorf("nightly timezone = %q, want the applied default", nightly.Timezone)
	}
	if nightly.TimeoutSeconds != 180 {
		t.Errorf("nightly timeout = %d, want the 180s default", nightly.TimeoutSeconds)
	}
	if !nightly.Enabled || !nightly.Catchup {
		t.Errorf("nightly enabled/catchup = %v/%v, want both true by default", nightly.Enabled, nightly.Catchup)
	}

	standup := byName["standup"]
	if standup.ScopeType != types.ScheduleScopeUnit || standup.ScopeID != "Quality" {
		t.Errorf("standup scope = %s/%s, want unit/Quality", standup.ScopeType, standup.ScopeID)
	}
	if standup.Target != org.TargetEach {
		t.Errorf("standup target = %q, want the each default made explicit", standup.Target)
	}
	got := slices.Clone(standup.Runners)
	slices.Sort(got)
	if !slices.Equal(got, []string{"qa-dev", "qa-lead"}) {
		t.Errorf("standup runners = %v, want both members", standup.Runners)
	}

	report := byName["report"]
	if report.Target != org.TargetLead {
		t.Errorf("report target = %q, want lead", report.Target)
	}
	if !slices.Equal(report.Runners, []string{"qa-lead"}) {
		t.Errorf("report runners = %v, want [qa-lead]", report.Runners)
	}
}

func TestDescribePreservesAnExplicitTimezone(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "Acme", Roles: []*org.Role{{
		Name: "Ops", DeclaredHandle: "ops",
		Schedules: []org.Schedule{{
			Name: "x", Cron: "0 9 * * *", Task: "t", Timezone: "America/New_York",
		}},
	}}}
	rows := schedule.Describe(o, schedule.DescribeOptions{DefaultTimezone: "UTC"})
	if rows[0].Timezone != "America/New_York" {
		t.Fatalf("timezone = %q, want the schedule's own", rows[0].Timezone)
	}
}

func TestDescribeResolvesTheNextRunInTheSchedulesOwnZone(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "Acme", Roles: []*org.Role{{
		Name: "Ops", DeclaredHandle: "ops",
		Schedules: []org.Schedule{{
			Name: "ams", Cron: "30 9 * * *", Task: "t", Timezone: "Europe/Amsterdam",
		}},
	}}}
	rows := schedule.Describe(o, schedule.DescribeOptions{
		Now: time.Date(2026, time.June, 8, 0, 0, 0, 0, time.UTC),
	})
	// 09:30 Amsterdam (CEST) is 07:30 UTC in June.
	want := time.Date(2026, time.June, 8, 7, 30, 0, 0, time.UTC)
	if !rows[0].NextRun.Equal(want) {
		t.Fatalf("next run = %v, want %v", rows[0].NextRun, want)
	}
	if rows[0].Problem != "" {
		t.Fatalf("problem = %q, want none", rows[0].Problem)
	}
}

func TestDescribeSaysWhyARowHasNoNextRun(t *testing.T) {
	t.Parallel()
	// The alternative is a blank cell, and a schedule whose timezone was
	// renamed then looks exactly like one that is simply not due for a
	// while. A disabled row is the one case where blank is honest — the
	// operator chose it.
	o := &org.Organization{Name: "Acme", Roles: []*org.Role{{
		Name: "Ops", DeclaredHandle: "ops",
		Schedules: []org.Schedule{
			{Name: "bad-cron", Cron: "nonsense here now please", Task: "t"},
			{Name: "bad-zone", Cron: "0 9 * * *", Task: "t", Timezone: "Mars/Olympus"},
			{Name: "impossible", Cron: "0 0 30 2 *", Task: "t"},
			{Name: "off", Cron: "0 9 * * *", Task: "t", Enabled: org.Off()},
			{Name: "fine", Cron: "0 9 * * *", Task: "t"},
		},
	}}}
	byName := rowsByName(t, schedule.Describe(o, schedule.DescribeOptions{
		Now: time.Date(2026, time.June, 8, 0, 0, 0, 0, time.UTC),
	}))

	for _, name := range []string{"bad-cron", "bad-zone", "impossible"} {
		row := byName[name]
		if !row.NextRun.IsZero() {
			t.Errorf("%s next run = %v, want none", name, row.NextRun)
		}
		if row.Problem == "" {
			t.Errorf("%s has no next run and says nothing about why", name)
		}
	}
	if off := byName["off"]; !off.NextRun.IsZero() || off.Problem != "" {
		t.Errorf("a disabled row = %v / %q, want a blank next run and no problem",
			off.NextRun, off.Problem)
	}
	// One unparseable row must not blank the others: this is a display
	// projection, not a validation pass.
	if fine := byName["fine"]; fine.NextRun.IsZero() {
		t.Error("a healthy row lost its next run because a sibling was broken")
	}
}

func TestDescribeAndTheTickResolveRunnersIdentically(t *testing.T) {
	t.Parallel()
	// One implementation, because two — one for firing and one for display —
	// is how a dashboard comes to show a runner that never receives the task.
	o := describeOrg()
	rows := rowsByName(t, schedule.Describe(o, schedule.DescribeOptions{}))
	for _, e := range schedule.Entries(o) {
		row, ok := rows[e.Schedule.Name]
		if !ok {
			t.Fatalf("Describe dropped the schedule %q that Entries reports", e.Schedule.Name)
		}
		if !slices.Equal(row.Runners, e.Runners(o)) {
			t.Fatalf("%s: Describe says %v, Entry.Runners says %v",
				e.Schedule.Name, row.Runners, e.Runners(o))
		}
	}
}

func TestEachRunnersExcludeHumanSeats(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "Acme", Units: []*org.OrgUnit{{
		Name: "Quality", Type: org.UnitTypeTeam, Lead: "QA Lead",
		Roles: []*org.Role{
			{Name: "QA Lead", DeclaredHandle: "qa-lead"},
			{Name: "Sarah Chen", Kind: org.KindHuman, Contact: &org.HumanContact{SlackUserID: "U0HUMAN"}},
			{Name: "QA Dev", DeclaredHandle: "qa-dev"},
		},
		Schedules: []org.Schedule{{Name: "standup", Cron: "30 9 * * 1-5", Task: "standup"}},
	}}}
	got := slices.Clone(schedule.Describe(o, schedule.DescribeOptions{})[0].Runners)
	slices.Sort(got)
	if !slices.Equal(got, []string{"qa-dev", "qa-lead"}) {
		t.Fatalf("runners = %v, want the two agent seats", got)
	}
}

func TestLeadRunnerIsEmptyWhenTheEffectiveLeadIsHuman(t *testing.T) {
	t.Parallel()
	// Config validation rejects an ENABLED lead schedule under a human lead,
	// so this covers the lenient piecewise-bootstrap window: the runtime
	// filter is what keeps a fire from being addressed to an inbox nothing
	// consumes.
	o := &org.Organization{Name: "Acme", Units: []*org.OrgUnit{{
		Name: "Quality", Type: org.UnitTypeTeam, Lead: "Sarah Chen",
		Roles: []*org.Role{
			{Name: "Sarah Chen", Kind: org.KindHuman, Contact: &org.HumanContact{SlackUserID: "U0HUMAN"}},
			{Name: "QA Dev", DeclaredHandle: "qa-dev"},
		},
		Schedules: []org.Schedule{{
			Name: "report", Cron: "0 16 * * 5", Task: "report",
			Target: org.TargetLead, Enabled: org.Off(),
		}},
	}}}
	entries := schedule.Entries(o)
	if len(entries) != 1 {
		t.Fatalf("Entries = %d, want 1", len(entries))
	}
	if got := entries[0].Runners(o); len(got) != 0 {
		t.Fatalf("runners = %v, want none — a human lead runs no turns", got)
	}
}

func TestALeadInheritedFromAnAncestorStillResolves(t *testing.T) {
	t.Parallel()
	// The reason `lead` survives as a target at all: it is DYNAMIC, and an
	// inherited lead lives outside the unit's own subtree.
	o := &org.Organization{Name: "Acme", Units: []*org.OrgUnit{{
		Name: "Engineering", Type: org.UnitTypeDepartment, Lead: "VP Eng",
		Roles: []*org.Role{{Name: "VP Eng", DeclaredHandle: "vp-eng"}},
		Children: []*org.OrgUnit{{
			Name:  "Quality",
			Type:  org.UnitTypeTeam,
			Roles: []*org.Role{{Name: "QA Dev", DeclaredHandle: "qa-dev"}},
			Schedules: []org.Schedule{{
				Name: "report", Cron: "0 16 * * 5", Task: "report", Target: org.TargetLead,
			}},
		}},
	}}}
	o.Normalize()

	entries := schedule.Entries(o)
	if len(entries) != 1 {
		t.Fatalf("Entries = %d, want 1", len(entries))
	}
	if got := entries[0].Runners(o); !slices.Equal(got, []string{"vp-eng"}) {
		t.Fatalf("runners = %v, want the inherited lead [vp-eng]", got)
	}
}

func TestEachResolvesDirectMembersOnly(t *testing.T) {
	t.Parallel()
	// A standup that fanned out to every descendant of a division would wake
	// the whole company. Schedules are not inherited downward either, which
	// is the same rule seen from the other end.
	o := &org.Organization{Name: "Acme", Units: []*org.OrgUnit{{
		Name: "Engineering", Type: org.UnitTypeDepartment, Lead: "VP Eng",
		Roles: []*org.Role{{Name: "VP Eng", DeclaredHandle: "vp-eng"}},
		Schedules: []org.Schedule{{
			Name: "sync", Cron: "0 9 * * *", Task: "sync", Target: org.TargetEach,
		}},
		Children: []*org.OrgUnit{{
			Name:  "Quality",
			Type:  org.UnitTypeTeam,
			Roles: []*org.Role{{Name: "QA Dev", DeclaredHandle: "qa-dev"}},
		}},
	}}}
	o.Normalize()

	entries := schedule.Entries(o)
	if len(entries) != 1 {
		t.Fatalf("Entries = %d, want 1 — a child unit must not inherit its parent's schedule", len(entries))
	}
	if got := entries[0].Runners(o); !slices.Equal(got, []string{"vp-eng"}) {
		t.Fatalf("runners = %v, want only the direct member", got)
	}
}

func TestEntriesReadAnEmptyOrNilCompany(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		o    *org.Organization
	}{
		{"nil", nil},
		{"empty", &org.Organization{Name: "Acme"}},
		{"seats with no schedules", &org.Organization{
			Name:  "Acme",
			Roles: []*org.Role{{Name: "QA", DeclaredHandle: "qa"}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if schedule.HasSchedules(tc.o) {
				t.Error("HasSchedules = true, want false")
			}
			if got := schedule.CountSchedules(tc.o); got != 0 {
				t.Errorf("CountSchedules = %d, want 0", got)
			}
			if got := schedule.Describe(tc.o, schedule.DescribeOptions{}); len(got) != 0 {
				t.Errorf("Describe = %v, want nothing", got)
			}
		})
	}
}

func TestHasAndCountSchedules(t *testing.T) {
	t.Parallel()
	// The engine gates the whole tick loop on this: a company with no
	// schedules never spins up a loop, a duty claim, or a ledger connection.
	if !schedule.HasSchedules(describeOrg()) {
		t.Error("HasSchedules = false for a company with three schedules")
	}
	if got := schedule.CountSchedules(describeOrg()); got != 3 {
		t.Errorf("CountSchedules = %d, want 3", got)
	}
}

func TestSortRowsIsTotalAndStable(t *testing.T) {
	t.Parallel()
	// Entries come out of the org in TREE order, which is meaningful to
	// nobody reading a flat table — and two dashboard reads of one company
	// must draw the same list.
	rows := schedule.Describe(describeOrg(), schedule.DescribeOptions{})
	schedule.SortRows(rows)
	var got []string
	for _, r := range rows {
		got = append(got, string(r.ScopeType)+"/"+r.ScopeID+"/"+r.Name)
	}
	want := []string{"role/ops/nightly", "unit/Quality/report", "unit/Quality/standup"}
	if !slices.Equal(got, want) {
		t.Fatalf("sorted rows = %v, want %v", got, want)
	}
}

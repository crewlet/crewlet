package schedule

import (
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
)

// EVERY FIRE OF ONE SCHEDULE SHARES ONE INBOX PARTITION, AND SAYS WHEN IT WAS
// DUE.
//
// A seat that cannot work holds its inbox, and a schedule keeps firing into
// it. With a partition per fire, every fire that waited was its own turn when
// the seat came back: three health checks due five hours apart ran within
// four minutes of a limit clearing. Sharing one partition is what hands the
// waiting fires to one handler call, where the seat runs only the latest, and
// the instant each was due is what decides which one that is.
func TestEveryFireOfAScheduleSharesOnePartition(t *testing.T) {
	t.Parallel()
	h := build(t, roleOrg(
		org.Schedule{Name: "health-check", Cron: "0 * * * *", Task: "check the services"},
		org.Schedule{Name: "standup", Cron: "0 * * * *", Task: "write the standup"},
	))
	h.seed(tickAt(8, 59, 30))
	h.tick(tickAt(9, 0, 30))
	h.tick(tickAt(10, 0, 30))

	byName := map[string][]*events.Event{}
	for _, p := range h.q.inboxTasks() {
		fire, _ := events.DataAs[*types.TaskAssigned](p.ev)
		byName[fire.Schedule] = append(byName[fire.Schedule], p.ev)
	}
	checks := byName["health-check"]
	if len(checks) != 2 || len(byName["standup"]) != 2 {
		t.Fatalf("fired %d health checks and %d standups, want two of each",
			len(checks), len(byName["standup"]))
	}
	first, second := notify.KeyOf(checks[0]), notify.KeyOf(checks[1])
	if first != second {
		t.Fatalf("two fires of one schedule key partitions %q and %q; fires that "+
			"wait together would each be their own turn", first, second)
	}
	if notify.KeyOf(byName["standup"][0]) == first {
		t.Fatal("two schedules of one seat share a partition, so one would run in " +
			"place of the other")
	}
	for i, want := range []int{9, 10} {
		fire, _ := events.DataAs[*types.TaskAssigned](checks[i])
		if !fire.ScheduledAt.Equal(tickAt(want, 0, 0)) {
			t.Errorf("fire %d says it was due at %s, want %s", i, fire.ScheduledAt,
				tickAt(want, 0, 0))
		}
	}
}

// A SEPARATOR IN A NAME DOES NOT MERGE TWO SCHEDULES, for the reason the
// ledger's identity is a struct rather than a joined string.
func TestAFirePartitionCannotCollideAcrossScopes(t *testing.T) {
	t.Parallel()
	a := FirePartition(Entry{Scope: types.ScheduleScopeUnit, ScopeID: "Q3/Launch",
		Schedule: org.Schedule{Name: "review"}})
	b := FirePartition(Entry{Scope: types.ScheduleScopeUnit, ScopeID: "Q3",
		Schedule: org.Schedule{Name: "Launch/review"}})
	if a == b {
		t.Fatalf("two different schedules share the partition %q", a)
	}
}

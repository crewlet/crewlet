package coord_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// A CHECK NOBODY ENDED IS LOST AT ITS DEADLINE, AND EVERY CHECK LAPSES A
// LINGER AFTER IT IS OVER. A page polling a check reads a running one as
// running until the deadline its node gives it up at, and past it as lost,
// since nothing will end it; and it reads how a check ended for a linger
// after the end, or after the deadline for one that never ended, and then
// nothing, since the record answers no page past that.
//
// Mutation: read a running check's lifetime from its start rather than its
// deadline, or let an ended check be lost, and this is red.
func TestAModelCheckIsLostAtItsDeadlineAndLapsesALingerAfterItIsOver(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	running := coord.ModelCheck{StartedAt: start, Deadline: start.Add(3 * time.Minute)}
	ended := running
	ended.EndedAt = start.Add(time.Minute)
	for name, tc := range map[string]struct {
		check        coord.ModelCheck
		at           time.Time
		lost, lapsed bool
	}{
		"running, before its deadline": {check: running, at: start.Add(2 * time.Minute)},
		"running, at its deadline":     {check: running, at: running.Deadline},
		"running, past its deadline":   {check: running, at: running.Deadline.Add(time.Second), lost: true},
		"running, a linger past its deadline": {check: running, at: running.Deadline.Add(coord.ModelCheckLinger),
			lost: true},
		"running, past that linger": {check: running,
			at: running.Deadline.Add(coord.ModelCheckLinger + time.Second), lost: true, lapsed: true},
		"ended, past its deadline":     {check: ended, at: running.Deadline.Add(time.Minute)},
		"ended, a linger past its end": {check: ended, at: ended.EndedAt.Add(coord.ModelCheckLinger)},
		"ended, past that linger": {check: ended, at: ended.EndedAt.Add(coord.ModelCheckLinger + time.Second),
			lapsed: true},
	} {
		if got := tc.check.Lost(tc.at); got != tc.lost {
			t.Errorf("%s: Lost = %v, want %v", name, got, tc.lost)
		}
		if got := tc.check.Lapsed(tc.at); got != tc.lapsed {
			t.Errorf("%s: Lapsed = %v, want %v", name, got, tc.lapsed)
		}
	}
}

// A CHECK'S KEY NAMES ITS ENTRY AND NOTHING ELSE: the class's listing reads
// the entry back from the key, and a key of another class, or one naming no
// entry, is none of the class's.
func TestAModelChecksKeyNamesItsEntry(t *testing.T) {
	t.Parallel()
	if entry, ok := coord.ModelCheckEntry(coord.ModelCheckKey("sonnet")); !ok || entry != "sonnet" {
		t.Errorf("ModelCheckEntry(ModelCheckKey(sonnet)) = %q, %v", entry, ok)
	}
	for _, key := range []string{coord.DeviceSignInKey("sonnet"), coord.ModelCheckKey(""),
		coord.DocumentKey(coord.ModelCheckClass, "sonnet", "extra")} {
		if entry, ok := coord.ModelCheckEntry(key); ok {
			t.Errorf("ModelCheckEntry(%q) = %q, want none", key, entry)
		}
	}
}

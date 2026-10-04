package statelog_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// AN ATTEMPT MINTS AT THE INSTANT IT INHERITS WHILE THE LEDGER CAN VOUCH FOR IT
// TO THE END OF THE ATTEMPT, and at its own instant once it cannot.
//
// Short of the horizon the inherited instant is kept, because a retry that
// repeats an earlier attempt's write collapses onto the first copy only under
// the same id — a rule that always rebased is red on those rows. Past it the
// attempt's own instant, because an id minted at an instant the sweep has
// passed, whose row is gone, is answered `unknown` on every node and never
// published — a rule that never rebased is red on those. And INSIDE THE
// RETENTION'S LAST DAY it rebases too: an attempt judged there goes on deciding
// writes, and a sweep in the next quarter hour moves past the instant while it
// does — a horizon at the bare retention is red on that row.
func TestAnAttemptRebasesOnlyPastTheHorizon(t *testing.T) {
	t.Parallel()
	began := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		carried time.Time
		after   time.Duration
		rebased bool
	}{
		{"an attempt the same day", began, time.Hour, false},
		{"an attempt a week on", began, 7 * 24 * time.Hour, false},
		{"an attempt exactly at the horizon", began, statelog.MintHorizon, false},
		{"an attempt just past the horizon", began, statelog.MintHorizon + time.Millisecond, true},
		{"an attempt a minute short of the retention", began, statelog.OpsRetention - time.Minute, true},
		{"an attempt just past the retention", began, statelog.OpsRetention + time.Millisecond, true},
		{"an attempt forty days on", began, 40 * 24 * time.Hour, true},
		// THE START IS NOT KNOWN: a seed an older build minted with no
		// instant in it. The zero instant is behind every loss the ledger
		// ever recorded, so keeping it answers every write `unknown` on a
		// node whose ledger has swept once.
		{"an attempt whose inherited instant is unknown", time.Time{}, time.Hour, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			at := began.Add(tc.after)
			got, moved := statelog.MintAt(tc.carried, at)
			want := tc.carried
			if tc.rebased {
				want = at
			}
			if moved != tc.rebased || !got.Equal(want) {
				t.Fatalf("MintAt(%s, %s) = (%s, %v), want (%s, %v)",
					tc.carried, at, got, moved, want, tc.rebased)
			}
		})
	}
}

// THE HORIZON LEAVES A WHOLE ATTEMPT INSIDE THE RETENTION.
//
// The attempt is judged when it starts and decides writes until it ends, and
// every one of those writes is vouched for only while the instant its id
// carries is no older than the retention when the write is decided. So an
// attempt that keeps an instant right at the horizon must still have room for
// a turn's worth of rounds before the sweep's cutoff reaches it — the day the
// horizon's own doc names. A horizon moved to the retention is red here.
func TestTheHorizonLeavesAnAttemptInsideTheRetention(t *testing.T) {
	t.Parallel()
	if room := statelog.OpsRetention - statelog.MintHorizon; room < 24*time.Hour {
		t.Fatalf("an attempt that keeps an instant %s old has %s before the "+
			"sweep's cutoff passes it; a turn deciding writes for longer than "+
			"that has them answered `unknown` on every node", statelog.MintHorizon, room)
	}
	if statelog.MintHorizon <= 0 {
		t.Fatalf("MintHorizon is %s: every attempt would rebase, and no retry "+
			"would ever collapse onto the attempt before it", statelog.MintHorizon)
	}
}

// A REBASED INSTANT IS THE ONE THE IDS CARRY, to the millisecond.
//
// The instant is recorded so that a later attempt inherits it, and every id
// minted at it keeps only its millisecond. Recorded finer, a reader holding
// the record and an id minted from it would find two different instants for
// one decision.
func TestARebasedInstantIsWhatTheIdsCarry(t *testing.T) {
	t.Parallel()
	began := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	at := began.Add(40*24*time.Hour + 345_678_901*time.Nanosecond)
	got, moved := statelog.MintAt(began, at)
	if !moved {
		t.Fatalf("an attempt forty days on kept %s", got)
	}
	minted, ok := statelog.OpMintedAt(statelog.DeriveOpID(got, "update-task-1", "wk-1"))
	if !ok || !minted.Equal(got) {
		t.Fatalf("the rebased instant is %s and the id minted at it carries %s",
			got, minted)
	}
	if got.Location() != time.UTC {
		t.Fatalf("the rebased instant is in %s, want UTC", got.Location())
	}
}

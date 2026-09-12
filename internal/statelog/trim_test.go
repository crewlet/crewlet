package statelog_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

func baseInputs() statelog.TrimInputs {
	now := time.Now()
	return statelog.TrimInputs{
		Generation:      1,
		Now:             now,
		CountedReadable: true,
		Counted: []statelog.NodePosition{
			{NodeID: "a", Generation: 1, Seq: 9_000, SnapshotSeq: 7_500, HasSnapshot: true, At: now},
			{NodeID: "b", Generation: 1, Seq: 8_500, SnapshotSeq: 7_300, HasSnapshot: true, At: now},
			{NodeID: "c", Generation: 1, Seq: 8_800, SnapshotSeq: 7_400, HasSnapshot: true, At: now},
		},
		HoldsReadable:  true,
		BackupFloor:    7_000,
		BackupAt:       now,
		BackupFloorGen: 1,
		HasBackupFloor: true,
		BackupMaxAge:   24 * time.Hour,
		FeedAckFloor:   8_900,
		HasFeed:        true,
		FeedReadable:   true,
		AgeFloor:       6_000,
		HoldStale:      statelog.TrimHoldStale,
	}
}

// THE SIX TERMS ENTER A MINIMUM, which is the whole grammar of this gate.
//
// Every term can only move the trim point DOWN. That is what makes the age
// term a LOWER bound on how long the log keeps a record rather than an upper
// one — an inversion three readers of this design got backwards, which is why
// it is asserted rather than commented.
func TestTheTrimTakesTheLowestTermAndNothingElse(t *testing.T) {
	t.Parallel()
	in := baseInputs()
	d := statelog.Trim(in.Terms())
	if d.Blocked() {
		t.Fatalf("the trim is blocked by %s: %s", d.BlockedBy, d.Detail)
	}
	// The age floor at 6 000 is the lowest of the six.
	if d.To != 6_000 {
		t.Fatalf("the trim would remove up to %d, want 6000 — the age floor is "+
			"the lowest term and every term enters a minimum", d.To)
	}
}

// RAISING THE AGE FLOOR CAN ONLY MOVE THE TRIM POINT DOWN.
//
// This is the inversion, stated as an assertion: the age is a floor on
// TRIMMING and therefore a LOWER bound on how long the log keeps a record. It
// is not, and cannot be, a ceiling on retention — and it says nothing whatever
// about any node's own store file, which trimming the stream does not touch.
func TestMinAgeOnlyLowersTheTrimPoint(t *testing.T) {
	t.Parallel()
	var last uint64 = ^uint64(0)
	for _, age := range []uint64{9_000, 6_000, 3_000, 0} {
		in := baseInputs()
		in.AgeFloor = age
		d := statelog.Trim(in.Terms())
		if d.Blocked() && age != 0 {
			t.Fatalf("age floor %d blocked the trim: %s", age, d.Detail)
		}
		if d.To > last {
			t.Fatalf("a LOWER age floor (%d) moved the trim point UP, from %d to "+
				"%d — the terms enter a minimum, so every one of them can only "+
				"move it down", age, last, d.To)
		}
		last = d.To
	}

	// AND A HIGHER ONE NEVER RAISES IT PAST ANOTHER TERM. At an age floor
	// above every other term, the trim is held by whichever of them is
	// lowest — never by the age.
	in := baseInputs()
	in.AgeFloor = 1_000_000
	d := statelog.Trim(in.Terms())
	if d.To != in.BackupFloor {
		t.Fatalf("with an age floor above every other term the trim is at %d, "+
			"want the backup floor's %d", d.To, in.BackupFloor)
	}
}

// A COUNTED NODE'S OWN POSITION IS THE `applied` TERM, so the trim can never
// pass it while it stays counted.
//
// The register has no expiry, so an offline counted node pins the floor
// indefinitely and no age setting bounds what it still holds. The only exit is
// an eviction — which advances the trim and deletes nothing on that machine.
func TestACountedNodesPositionIsNeverTrimmedPast(t *testing.T) {
	t.Parallel()
	in := baseInputs()
	// One node far behind, reporting long ago. It is still counted.
	in.Counted = append(in.Counted, statelog.NodePosition{
		NodeID: "offline", Generation: 1, Seq: 100,
		At: in.Now.Add(-30 * 24 * time.Hour),
	})
	in.AgeFloor = 1_000_000
	in.BackupFloor = 1_000_000

	d := statelog.Trim(in.Terms())
	if d.Blocked() {
		t.Fatalf("the trim is blocked by %s: %s", d.BlockedBy, d.Detail)
	}
	if d.To > 100 {
		t.Fatalf("the trim would remove up to %d, past the offline node's own "+
			"position of 100 — an offline counted node pins the floor "+
			"indefinitely, and the only exit is an eviction", d.To)
	}
}

// A TERM THAT CANNOT BE READ BLOCKS, on the same rule the read path uses for
// an unreadable floor: the third value is not the optimistic one.
func TestAnUnreadableTermBlocksTheTrim(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		break_ func(*statelog.TrimInputs)
		want   statelog.TermName
	}{
		"the register could not be listed": {
			break_: func(in *statelog.TrimInputs) { in.CountedReadable = false },
			want:   statelog.TermApplied,
		},
		"the holds could not be listed": {
			break_: func(in *statelog.TrimInputs) { in.HoldsReadable = false },
			want:   statelog.TermMinHold,
		},
		"nothing has ever been backed up": {
			break_: func(in *statelog.TrimInputs) { in.HasBackupFloor = false },
			want:   statelog.TermBackupFloor,
		},
		"the newest backup is too old": {
			break_: func(in *statelog.TrimInputs) {
				in.BackupAt = in.Now.Add(-48 * time.Hour)
			},
			want: statelog.TermBackupFloor,
		},
		"too few nodes hold a snapshot": {
			break_: func(in *statelog.TrimInputs) {
				for i := range in.Counted {
					in.Counted[i].HasSnapshot = false
				}
			},
			want: statelog.TermSnapshotFloor,
		},
		"the wake consumer could not be read": {
			break_: func(in *statelog.TrimInputs) { in.FeedReadable = false },
			want:   statelog.TermFeedAckFloor,
		},
		"a node reports from a dead generation": {
			break_: func(in *statelog.TrimInputs) { in.Counted[1].Generation = 0 },
			want:   statelog.TermApplied,
		},
		"the backup is from a dead generation": {
			break_: func(in *statelog.TrimInputs) { in.BackupFloorGen = 0 },
			want:   statelog.TermBackupFloor,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			in := baseInputs()
			tc.break_(&in)
			d := statelog.Trim(in.Terms())
			if !d.Blocked() {
				t.Fatalf("the trim advanced to %d with %s", d.To, name)
			}
			if d.BlockedBy != tc.want {
				t.Fatalf("blocked by %q, want %q", d.BlockedBy, tc.want)
			}
			if d.Detail == "" {
				t.Error("the block says nothing an operator could act on")
			}
			if d.To != 0 {
				t.Errorf("a blocked trim reports removing up to %d", d.To)
			}
		})
	}
}

// AN ABSENT TERM IS `n/a`, NOT ZERO.
//
// A compacted domain has no wake feed at all, and reporting its position as
// zero would block its trim for ever on a term it does not have.
func TestADomainWithNoWakeFeedIsNotBlockedByOne(t *testing.T) {
	t.Parallel()
	in := baseInputs()
	in.HasFeed = false
	in.FeedAckFloor = 0
	in.FeedReadable = false

	d := statelog.Trim(in.Terms())
	if d.Blocked() {
		t.Fatalf("a domain with no wake feed is blocked by %s: %s — an absent "+
			"term is not a zero one", d.BlockedBy, d.Detail)
	}
	if d.To != 6_000 {
		t.Fatalf("the trim would remove up to %d, want 6000", d.To)
	}
	for _, term := range d.Terms {
		if term.Name == statelog.TermFeedAckFloor && !term.Absent {
			t.Error("the wake feed term is reported as present on a domain that " +
				"has none")
		}
	}
}

// THE SNAPSHOT TERM IS THE k-TH HIGHEST, not the minimum and not the maximum.
//
// The minimum blocks for ever on any node that has not snapshotted yet; the
// maximum makes one donor's disk the whole fleet's recovery plan. The k-th
// means losing any single donor still leaves a usable artefact.
func TestTheSnapshotTermSurvivesLosingOneDonor(t *testing.T) {
	t.Parallel()
	in := baseInputs()
	in.AgeFloor = 1_000_000
	in.BackupFloor = 1_000_000
	in.FeedAckFloor = 1_000_000

	d := statelog.Trim(in.Terms())
	if d.Blocked() {
		t.Fatalf("blocked by %s: %s", d.BlockedBy, d.Detail)
	}
	// Snapshots at 7 500, 7 400 and 7 300; the second highest is 7 400, so
	// the trim may reach one past it. The MAXIMUM would have been 7 500 —
	// which is exactly the artefact whose donor might be the one lost.
	if d.To != 7_401 {
		t.Fatalf("the trim would remove up to %d, want 7401 — the second-highest "+
			"snapshot is at 7400, and taking the highest would make one donor's "+
			"disk the whole fleet's recovery plan", d.To)
	}

	// A SOLO FLEET IS SATISFIED BY CONSTRUCTION, because its snapshot loop
	// does not run at all and its recovery artefact is a backup.
	solo := baseInputs()
	solo.Counted = solo.Counted[:1]
	solo.Counted[0].HasSnapshot = false
	if d := statelog.Trim(solo.Terms()); d.BlockedBy == statelog.TermSnapshotFloor {
		t.Fatal("a single node is blocked by the snapshot term — its loop does " +
			"not run, and concluding otherwise deadlocks one node for ever")
	}
}

// A STALE HOLD IS IGNORED, because a crashed adopter must not pin the log for
// the life of the deployment.
func TestACrashedAdoptersHoldDoesNotPinTheLogForEver(t *testing.T) {
	t.Parallel()
	in := baseInputs()
	in.Holds = []statelog.Hold{
		{Owner: "adopt-dead", Generation: 1, Seq: 10, At: in.Now.Add(-time.Hour)},
		{Owner: "adopt-live", Generation: 1, Seq: 6_500, At: in.Now},
	}
	d := statelog.Trim(in.Terms())
	if d.Blocked() {
		t.Fatalf("blocked by %s: %s", d.BlockedBy, d.Detail)
	}
	if d.To <= 10 {
		t.Fatalf("the trim is held at %d by a hold last renewed an hour ago — a "+
			"single failed join would stop the fleet trimming for ever", d.To)
	}
}

// EVERY TERM NAME IS ONE THIS BUILD KNOWS, and the set is closed.
func TestTheTrimTermsAreAClosedSet(t *testing.T) {
	t.Parallel()
	if len(statelog.TermNames) != 6 {
		t.Fatalf("%d terms, want 6", len(statelog.TermNames))
	}
	seen := map[statelog.TermName]struct{}{}
	for _, name := range statelog.TermNames {
		if !name.Valid() {
			t.Errorf("%q is in the set and reports itself invalid", name)
		}
		if _, dup := seen[name]; dup {
			t.Errorf("%q appears twice", name)
		}
		seen[name] = struct{}{}
	}
	if statelog.TermName("whatever").Valid() {
		t.Error("an unknown term reports itself valid")
	}
	// AND EVERY ONE IS EVALUATED, so a term nobody computes cannot hide.
	in := baseInputs()
	for _, name := range statelog.TermNames {
		var found bool
		for _, term := range in.Terms() {
			if term.Name == name {
				found = true
			}
		}
		if !found {
			t.Errorf("%q is a term and nothing evaluates it", name)
		}
	}
}

// THE TRIM AND A TRANSFER RACE, AND THE HOLD IS WHAT DECIDES IT.
//
// A joining node replays the tail its artefact stops at. The trim's job is to
// delete that tail. Four arms, because the hold has four states and each one
// produces a different correct answer — and getting any of them wrong looks
// identical from the joiner's side: a snapshot installs, the replay starts,
// and the records between the artefact and the live stream are simply gone.
func TestTrimRacesATransfer(t *testing.T) {
	t.Parallel()

	// ARM 1 — THE HOLD IS TAKEN BEFORE THE TICK READS IT. The adopter
	// pins the tail as step 1 of nine, before a byte is fetched, so every
	// tick from then on sees it and the minimum cannot pass it.
	t.Run("a hold taken before the tick pins the tail", func(t *testing.T) {
		t.Parallel()
		in := baseInputs()
		in.AgeFloor = 9_000
		in.Holds = []statelog.Hold{{
			Owner: "joiner", Generation: 1, Seq: 4_000, At: in.Now,
		}}
		d := statelog.Trim(in.Terms())
		if d.Blocked() {
			t.Fatalf("blocked by %s: %s", d.BlockedBy, d.Detail)
		}
		if d.To != 4_000 {
			t.Fatalf("the trim would remove up to %d while a transfer holds "+
				"4000 — the hold is the whole mechanism, not a hint", d.To)
		}
	})

	// ARM 2 — THE HOLD IS RENEWED FOR THE WHOLE TRANSFER. A gigabyte-scale
	// copy outlives many ticks, and a hold that only covered the first one
	// would be a race the joiner loses by being slow.
	t.Run("a renewed hold pins the tail for as long as the copy runs", func(t *testing.T) {
		t.Parallel()
		in := baseInputs()
		in.AgeFloor = 9_000
		for tick := range 20 {
			// The adopter renews on its own cadence; the tick moves on.
			in.Now = in.Now.Add(statelog.TrimHoldStale / 4)
			in.Holds = []statelog.Hold{{
				Owner: "joiner", Generation: 1, Seq: 4_000, At: in.Now,
			}}
			if d := statelog.Trim(in.Terms()); d.To != 4_000 {
				t.Fatalf("tick %d would remove up to %d, want 4000", tick, d.To)
			}
		}
	})

	// ARM 3 — THE HOLDER CRASHED. A hold nothing renews cannot pin the log
	// for ever: the joiner is gone, and the alternative is a company whose
	// log grows until somebody notices a machine that never came back.
	t.Run("a crashed holder stops pinning the tail", func(t *testing.T) {
		t.Parallel()
		in := baseInputs()
		in.AgeFloor = 9_000
		in.Holds = []statelog.Hold{{
			Owner: "joiner", Generation: 1, Seq: 4_000,
			At: in.Now.Add(-statelog.TrimHoldStale - time.Second),
		}}
		d := statelog.Trim(in.Terms())
		if d.Blocked() {
			t.Fatalf("blocked by %s: %s", d.BlockedBy, d.Detail)
		}
		if d.To == 4_000 {
			t.Fatal("a hold nobody has renewed for longer than the stale bound " +
				"still pins the log, which is a crashed joiner holding a " +
				"company's retention open indefinitely")
		}
	})

	// ARM 4 — THE TRIM WON ANYWAY. The hold is a belt: a fleet that
	// trimmed past the artefact regardless — an operator's purge, a
	// hold that was never taken, a floor that moved under a retry — must
	// not have its snapshot installed, because the joiner would come up
	// pointing at a replay tail that no longer exists.
	t.Run("an artefact the fleet trimmed past is not installed", func(t *testing.T) {
		t.Parallel()
		h := newJoinHarness(t)
		h.stillUsable = func(context.Context, statelog.Manifest) error {
			return errors.New("the trim floor is 6000 and this artefact stops at 4000")
		}
		_, err := h.adopter(t).Join(t.Context())
		if err == nil {
			t.Fatal("an artefact the fleet trimmed past during the transfer was " +
				"installed: the node comes up with a checkpoint below the log's " +
				"first surviving sequence and nothing to replay from")
		}
		if !strings.Contains(err.Error(), "stopped being usable") {
			t.Fatalf("the refusal is %q and does not say the artefact went stale "+
				"during the transfer", err)
		}
		// AND THE LIVE DATABASE SURVIVED. Step 7 is before step 8, which
		// is the one place the engine replaces a database.
		if h.closes.Load() != 0 {
			t.Errorf("the live database was closed %d time(s) for an install "+
				"that must never have started", h.closes.Load())
		}
		if h.held.Load() != 1 || h.released.Load() != 1 {
			t.Errorf("held=%d released=%d: a refused join must not leave the log "+
				"pinned", h.held.Load(), h.released.Load())
		}
	})
}

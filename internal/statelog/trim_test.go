package statelog_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/statelog"
)

func baseInputs() statelog.TrimInputs {
	now := time.Now()
	return statelog.TrimInputs{
		Generation:      1,
		Now:             now,
		CountedReadable: true,
		Counted: []statelog.NodePosition{
			{NodeID: "a", Generation: 1, Seq: 9_000, SnapshotSeq: 7_500,
				HasSnapshot: true, SnapshotGeneration: 1, At: now},
			{NodeID: "b", Generation: 1, Seq: 8_500, SnapshotSeq: 7_300,
				HasSnapshot: true, SnapshotGeneration: 1, At: now},
			{NodeID: "c", Generation: 1, Seq: 8_800, SnapshotSeq: 7_400,
				HasSnapshot: true, SnapshotGeneration: 1, At: now},
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

// THE SIX TERMS ENTER A MINIMUM, so every term can only move the trim point
// DOWN, and the age term is a LOWER bound on how long the log keeps a record.
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

// RAISING THE AGE FLOOR CAN ONLY MOVE THE TRIM POINT DOWN: the age is a LOWER
// bound on how long the log keeps a record, never a ceiling on retention.
func TestMinAgeOnlyLowersTheTrimPoint(t *testing.T) {
	t.Parallel()
	last := ^uint64(0)
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

	// AND A HIGHER ONE NEVER RAISES IT PAST ANOTHER TERM: above them all,
	// the lowest of the others holds the trim.
	in := baseInputs()
	in.AgeFloor = 1_000_000
	d := statelog.Trim(in.Terms())
	if d.To != in.BackupFloor {
		t.Fatalf("with an age floor above every other term the trim is at %d, "+
			"want the backup floor's %d", d.To, in.BackupFloor)
	}
}

// A COUNTED NODE'S OWN POSITION IS THE `applied` TERM, so the trim never passes
// it while the node stays counted, however long offline and whatever the age
// setting: only an eviction frees it.
//
// AND THE RECORD AT THE POSITION ITSELF IS KEPT, the bound being exclusive: a
// node whose log diverged from its rows is judged by it, and every other term
// here licenses removing it.
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
		breaks func(*statelog.TrimInputs)
		want   statelog.TermName
	}{
		"the register could not be listed": {
			breaks: func(in *statelog.TrimInputs) { in.CountedReadable = false },
			want:   statelog.TermApplied,
		},
		"the holds could not be listed": {
			breaks: func(in *statelog.TrimInputs) { in.HoldsReadable = false },
			want:   statelog.TermMinHold,
		},
		"nothing has ever been backed up": {
			breaks: func(in *statelog.TrimInputs) { in.HasBackupFloor = false },
			want:   statelog.TermBackupFloor,
		},
		"the newest backup is too old": {
			breaks: func(in *statelog.TrimInputs) {
				in.BackupAt = in.Now.Add(-48 * time.Hour)
			},
			want: statelog.TermBackupFloor,
		},
		"too few nodes hold a snapshot": {
			breaks: func(in *statelog.TrimInputs) {
				for i := range in.Counted {
					in.Counted[i].HasSnapshot = false
				}
			},
			want: statelog.TermSnapshotFloor,
		},
		"the wake consumer could not be read": {
			breaks: func(in *statelog.TrimInputs) { in.FeedReadable = false },
			want:   statelog.TermFeedAckFloor,
		},
		"a node reports from a dead generation": {
			breaks: func(in *statelog.TrimInputs) { in.Counted[1].Generation = 0 },
			want:   statelog.TermApplied,
		},
		"the backup is from a dead generation": {
			breaks: func(in *statelog.TrimInputs) { in.BackupFloorGen = 0 },
			want:   statelog.TermBackupFloor,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			in := baseInputs()
			tc.breaks(&in)
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

// AN ABSENT TERM IS `n/a`, NOT ZERO: zero would block a domain with no wake
// feed for ever on a term it does not have.
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

// THE SNAPSHOT TERM IS THE k-TH HIGHEST, not the maximum, so losing any single
// donor still leaves a usable artefact ([statelog.TermSnapshotFloor]).
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
	// Snapshots at 7 500, 7 400 and 7 300: the trim may reach one past the
	// second highest. The MAXIMUM's donor might be the one lost.
	if d.To != 7_401 {
		t.Fatalf("the trim would remove up to %d, want 7401 — the second-highest "+
			"snapshot is at 7400, and taking the highest would make one donor's "+
			"disk the whole fleet's recovery plan", d.To)
	}

	// A SOLO FLEET IS SATISFIED BY CONSTRUCTION: no snapshot loop runs, and
	// it recovers from a backup.
	solo := baseInputs()
	solo.Counted = solo.Counted[:1]
	solo.Counted[0].HasSnapshot = false
	if d := statelog.Trim(solo.Terms()); d.BlockedBy == statelog.TermSnapshotFloor {
		t.Fatal("a single node is blocked by the snapshot term — its loop does " +
			"not run, and concluding otherwise deadlocks one node for ever")
	}
	// AND IT SAYS SO IN THE DASHBOARD'S WORDS: Settings › Backups draws this
	// sentence beside the product's Artifacts, so it never says "artefact".
	for _, term := range solo.Terms() {
		if term.Name != statelog.TermSnapshotFloor {
			continue
		}
		if want := "a single node takes no snapshots; it is recovered from a " +
			"backup, which the backup term gates"; term.Detail != want {
			t.Errorf("the solo snapshot term reads %q, want %q", term.Detail, want)
		}
	}
}

// A SNAPSHOT FROM ANOTHER GENERATION IS NOT A DONOR. After a reanchor a node's
// row moves to the NEW generation at its next position heartbeat but carries
// an artefact naming the OLD one until its next daily snapshot. Counted, it
// licenses removing records no adoptable artefact covers, which a bare
// sequence could not detect.
func TestASnapshotFromAnEarlierGenerationIsNotCounted(t *testing.T) {
	t.Parallel()
	in := baseInputs()
	in.Counted[0].SnapshotGeneration = 0
	in.Counted[1].SnapshotGeneration = 0
	in.AgeFloor = 1_000_000
	in.BackupFloor = 1_000_000
	in.FeedAckFloor = 1_000_000

	d := statelog.Trim(in.Terms())
	if !d.Blocked() {
		t.Fatalf("the trim advanced to %d with one adoptable artefact in the "+
			"fleet — two are needed, and a snapshot on a dead generation names "+
			"a sequence space this log no longer has", d.To)
	}
	if d.BlockedBy != statelog.TermSnapshotFloor {
		t.Fatalf("blocked by %q, want %q", d.BlockedBy, statelog.TermSnapshotFloor)
	}
	if !strings.Contains(d.Detail, "earlier generation") {
		t.Errorf("the block reads %q — an operator seeing three nodes with "+
			"snapshots and a term saying one holds none needs the reason named",
			d.Detail)
	}

	// AND IT IS NOT A BLOCK BY ITSELF: two current artefacts beside a stale
	// one is a fleet that can recover.
	in.Counted[1].SnapshotGeneration = 1
	if d := statelog.Trim(in.Terms()); d.Blocked() {
		t.Fatalf("two current artefacts beside one stale one blocked the trim: "+
			"%s: %s", d.BlockedBy, d.Detail)
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

// THE TRIM AND A TRANSFER RACE, AND THE HOLD DECIDES IT. A joining node replays
// the tail its artefact stops at, the tail the trim deletes. One arm per state
// of the hold, each with its own correct answer; any wrong one looks the same
// to the joiner: the records between the artefact and the live stream are gone.
func TestTrimRacesATransfer(t *testing.T) {
	t.Parallel()

	// ARM 1 — THE HOLD IS TAKEN BEFORE THE TICK READS IT: the adopter pins
	// the tail before a byte is fetched, so no later tick's minimum passes it.
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

	// ARM 2 — THE HOLD IS RENEWED FOR THE WHOLE TRANSFER, which outlives many
	// ticks. It renews through the engine's one holder ([statelog.HoldTail]),
	// never by hand, so the case certifies the holder that runs. Its clock
	// runs through five stale bounds, the trim asked after every renewal.
	t.Run("a renewed hold pins the tail for as long as the copy runs", func(t *testing.T) {
		t.Parallel()
		clock := &stepClock{now: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
		register := memory.NewFleet()
		pinAt(t.Context(), t, register, clock, time.Millisecond, 4_000, nil)
		for tick := range 5 * 4 {
			now := clock.advance(statelog.TrimHoldStale / 4)
			in := baseInputs()
			in.Now, in.BackupAt, in.AgeFloor = now, now, 9_000
			in.Holds = renewedAt(t, register, now)
			if d := statelog.Trim(in.Terms()); d.To != 4_000 {
				t.Fatalf("tick %d, %s into the copy, would remove up to %d (%s), "+
					"want 4000", tick, time.Duration(tick+1)*statelog.TrimHoldStale/4,
					d.To, d.Detail)
			}
		}
	})

	// ARM 3 — THE HOLDER CRASHED: a hold nothing renews cannot pin the log
	// for ever.
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

	// ARM 4 — THE TRIM WON ANYWAY (an operator's purge, a hold never taken,
	// a floor that moved under a retry): the snapshot is not installed, or
	// the joiner would come up on a replay tail that no longer exists.
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
		// AND THE LIVE DATABASE SURVIVED. Step 7 is before step 9, which
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

// TestANodeWhoseArtefactOmitsADomainIsNotCountedInThatDomainsFloor: a node
// holding no usable artefact is not a donor, since counting it licenses
// removing records no adoptable artefact covers. The engine's own case proves
// a node whose artefact is short of a domain stamps nothing, which arrives here
// as HasSnapshot false.
func TestANodeWhoseArtefactOmitsADomainIsNotCountedInThatDomainsFloor(t *testing.T) {
	t.Parallel()
	in := baseInputs()
	// node-c's newest artefact predates a domain this build registers, so
	// it stamped nothing and reaches the trim holding none.
	in.Counted[2].HasSnapshot = false
	in.Counted[2].SnapshotSeq = 0

	d := statelog.Trim(in.Terms())
	if d.Blocked() {
		t.Fatalf("two donors out of three blocked the trim on %s: %s", d.BlockedBy, d.Detail)
	}

	// CONTROL: with one donor left, the floor must stop being known —
	// losing any single donor still has to leave a usable artefact.
	in.Counted[1].HasSnapshot = false
	in.Counted[1].SnapshotSeq = 0
	if d := statelog.Trim(in.Terms()); !d.Blocked() {
		t.Errorf("one donor and two nodes holding nothing trimmed to %d — a node "+
			"whose artefact no joiner can adopt must not count as a donor", d.To)
	} else if d.BlockedBy != statelog.TermSnapshotFloor {
		t.Errorf("blocked by %s, want %s", d.BlockedBy, statelog.TermSnapshotFloor)
	}
}

// ONE COUNTED NODE IS NOBODY'S STRAGGLER: a healthy single-node company's
// applied term must not read as a fleet member behind, to bring back or evict.
func TestTheAppliedTermOnASingleNodeNamesNoStraggler(t *testing.T) {
	t.Parallel()
	in := baseInputs()
	in.Counted = in.Counted[:1]
	terms := in.Terms()
	applied := terms[0]
	if applied.Name != statelog.TermApplied {
		t.Fatalf("the first term is %q, want applied", applied.Name)
	}
	if strings.Contains(applied.Detail, "behind") || strings.Contains(applied.Detail, "(s)") {
		t.Errorf("the applied term on one node reads %q", applied.Detail)
	}
	if !strings.Contains(applied.Detail, "only counted node") {
		t.Errorf("the applied term on one node reads %q, want it to say the node is the only one", applied.Detail)
	}
}

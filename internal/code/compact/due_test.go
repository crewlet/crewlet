package compact_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/compact"
)

// dueAt is the instant every Due case is judged at.
var dueAt = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

// candidate is a repository of live packs and objects whose last publication,
// at publishedAt and taking took, covered publishedPacks packs and
// publishedObjects objects, and that was last pushed to at pushedAt.
func candidate(live, publishedPacks, liveObjects, publishedObjects int64, pushedAt, publishedAt time.Time,
	took time.Duration) code.PublicationCandidate {

	return code.PublicationCandidate{ID: "r", Name: "acme", LivePacks: live, PublishedPacks: publishedPacks,
		LiveObjects: liveObjects, PublishedObjects: publishedObjects, LastPushAt: pushedAt,
		PublishedAt: publishedAt, PublishTook: took}
}

// A PUBLICATION IS DUE BY ITS ROW, and every clause of the rule is one of its
// own: the tail must hold a pack; it must be worth a layer — the repository
// never published, or its tail at compact.PublishPacks packs, or at a
// compact.PublishShare-th of its objects, or at compact.PublishTailObjects
// objects, whichever of the last two is fewer; it must have settled — stood
// still compact.PublishDebounce, or waited compact.PublishMaxWait since the
// tail began (its first sighting by the pass, or the last publication where
// that is later); and the duty cycle must allow it —
// compact.PublishMinInterval, or compact.PublishDutyCycle times the last
// build, whichever is longer, since the last publication. Each row turns one
// clause, against a candidate due by every other.
//
// Mutations: drop the duty cycle's build term (pace by PublishMinInterval
// alone), and the long-build row is due; read the tail's packs alone, and the
// share row is not due; count the ceiling from the last publication alone,
// and a burst into a repository idle since then is due at its first pack;
// drop the ceiling, and the repository pushed to without a pause is never
// due; hold a repository never published to the worth rule, and its first
// layer waits for PublishPacks packs.
func TestAPublicationIsDueByItsRow(t *testing.T) {
	t.Parallel()
	settled := dueAt.Add(-time.Hour)
	long := dueAt.Add(-24 * time.Hour)
	never := time.Time{}
	// A repository of a million objects in 100 packs: its share is 15 625
	// objects (a sixty-fourth), under compact.PublishTailObjects, so the share
	// is the objects' bound here.
	const live, objects = int64(100), int64(1_000_000)
	share := objects / compact.PublishShare
	for _, c := range []struct {
		name      string
		cand      code.PublicationCandidate
		tailSince time.Time
		due       bool
	}{
		{"every clause holds", candidate(live, live-compact.PublishPacks, objects, objects-1, settled, long, 0),
			settled, true},
		{"no tail: the live packs are the published ones",
			candidate(live, live, objects, objects, settled, long, 0), settled, false},
		{"a tail of packs past the published ones, of no objects, still holds a pack",
			candidate(live, live-compact.PublishPacks, objects, objects, settled, long, 0), settled, true},
		{"a tail short of every bound is not worth a layer",
			candidate(live, live-compact.PublishPacks+1, objects, objects-share+1, settled, long, 0), settled,
			false},
		{"a tail at PublishPacks packs is worth one",
			candidate(live, live-compact.PublishPacks, objects, objects-1, settled, long, 0), settled, true},
		{"a tail at a PublishShare-th of the objects is worth one",
			candidate(live, live-1, objects, objects-share, settled, long, 0), settled, true},
		{"a repository never published is worth its first layer, however small",
			candidate(1, 0, 10, 0, settled, never, 0), settled, true},
		{"a repository never published with no pack has no tail",
			candidate(0, 0, 0, 0, never, never, 0), never, false},
		{"a repository never published, pushed to a moment ago, waits for its burst to settle",
			candidate(3, 0, 30, 0, dueAt.Add(-time.Second), never, 0), dueAt, false},
		{"a repository never published, stood still PublishDebounce, has settled",
			candidate(3, 0, 30, 0, dueAt.Add(-compact.PublishDebounce), never, 0), dueAt, true},
		{"a burst into a repository idle since its last publication is not settled at its first pack",
			candidate(live, live-compact.PublishPacks, objects, objects-1, dueAt.Add(-time.Second), long, 0),
			dueAt.Add(-time.Minute), false},
		{"a burst that stood still PublishDebounce has settled",
			candidate(live, live-compact.PublishPacks, objects, objects-1, dueAt.Add(-compact.PublishDebounce),
				long, 0), dueAt.Add(-time.Minute), true},
		{"a tail pushed to without a pause is due once it has waited PublishMaxWait",
			candidate(live, live-compact.PublishPacks, objects, objects-1, dueAt, long, 0),
			dueAt.Add(-compact.PublishMaxWait), true},
		{"a tail pushed to without a pause, short of PublishMaxWait, is not",
			candidate(live, live-compact.PublishPacks, objects, objects-1, dueAt, long, 0),
			dueAt.Add(-compact.PublishMaxWait + time.Second), false},
		{"within PublishMinInterval of the last publication is not due",
			candidate(live, live-compact.PublishPacks, objects, objects-1, settled,
				dueAt.Add(-compact.PublishMinInterval+time.Second), 0), settled, false},
		{"at PublishMinInterval of the last publication is due",
			candidate(live, live-compact.PublishPacks, objects, objects-1, settled,
				dueAt.Add(-compact.PublishMinInterval), 0), settled, true},
		{"a long build holds the next to PublishDutyCycle times it",
			candidate(live, live-compact.PublishPacks, objects, objects-1, settled, dueAt.Add(-time.Hour),
				10*time.Minute), settled, false},
		{"once PublishDutyCycle times the last build has passed it is due",
			candidate(live, live-compact.PublishPacks, objects, objects-1, settled,
				dueAt.Add(-compact.PublishDutyCycle*10*time.Minute), 10*time.Minute), long, true},
	} {
		if got := compact.Due(c.cand, c.tailSince, dueAt); got != c.due {
			t.Errorf("%s: Due(%+v, tail since %s) = %v, want %v", c.name, c.cand, c.tailSince, got, c.due)
		}
	}
}

// A LARGE REPOSITORY'S TAIL IS BOUNDED IN OBJECTS, never by its share alone: a
// tail's cost is its objects — every clone and the installer's canary walk
// each one outside the chain — so the largest repository the defaults admit,
// some 60 million objects, is due once its tail holds
// compact.PublishTailObjects objects (a second of walk at
// gitproc.WalkPerObject) in a few packs, where its share, a sixty-fourth,
// would wait for 937 500 (some ten seconds of walk added to every clone);
// while a small one, whose share is fewer, is due at that share first.
//
// Mutation: hold the tail's objects to the share alone, and the 60 million
// arm is not due.
func TestALargeRepositorysTailIsBoundedInObjects(t *testing.T) {
	t.Parallel()
	long := dueAt.Add(-24 * time.Hour)
	const largest = int64(60_000_000)
	if largest/compact.PublishShare <= compact.PublishTailObjects {
		t.Fatalf("the case needs a repository whose share passes the bound: %d objects' share is %d, the bound %d",
			largest, largest/compact.PublishShare, compact.PublishTailObjects)
	}
	if !compact.Due(candidate(1_020, 1_000, largest, largest-compact.PublishTailObjects, long, long, 0), long,
		dueAt) {
		t.Errorf("a tail of %d objects in 20 packs of a %d-object repository is not due: its share alone would "+
			"wait for %d", compact.PublishTailObjects, largest, largest/compact.PublishShare)
	}
	if compact.Due(candidate(1_020, 1_000, largest, largest-compact.PublishTailObjects+1, long, long, 0), long,
		dueAt) {
		t.Errorf("a tail one object short of %d, in 20 packs, of a %d-object repository is due",
			compact.PublishTailObjects, largest)
	}
	const small = int64(200_000)
	share := small / compact.PublishShare
	if share >= compact.PublishTailObjects {
		t.Fatalf("the case needs a repository whose share is under the bound: %d objects' share is %d",
			small, share)
	}
	if !compact.Due(candidate(30, 10, small, small-share, long, long, 0), long, dueAt) {
		t.Errorf("a tail of its share, %d objects, of a %d-object repository is not due", share, small)
	}
	if compact.Due(candidate(30, 10, small, small-share+1, long, long, 0), long, dueAt) {
		t.Errorf("a tail one object short of its share of a %d-object repository is due", small)
	}
}

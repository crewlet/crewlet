package work_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/work"
)

// swept builds a store on a controllable clock, with its sweeper.
func swept(t *testing.T) (*work.Store, *work.Sweeper, coord.Documents, *workClock) {
	t.Helper()
	docs := memory.NewFleet()
	c := &workClock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	s, err := work.NewStore(work.Options{Documents: docs, Now: c.now})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return s, work.NewSweeper(docs, c.now), docs, c
}

type workClock struct{ at time.Time }

func (c *workClock) now() time.Time { return c.at }

// keysOfClass is how many keys of one class the family holds.
func keysOfClass(t *testing.T, docs coord.Documents, class string) int {
	t.Helper()
	records, err := docs.Documents(t.Context(), coord.FamilyWork, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var n int
	for _, rec := range records {
		if got, ok := work.ClassOf(rec.Key); ok && got == class {
			n++
		}
	}
	return n
}

// A CHANGE RECORD IS THE ONLY CLASS THAT AGES OUT. Items and their threads are
// the company's record of what it decided; the change keys are the audit
// trail behind them, and they are written on every single edit — so a bucket
// that kept both for ever would grow one subject per edit for the life of the
// deployment, while a bucket age that expired both would silently delete the
// board.
func TestOnlyChangesAgeOut(t *testing.T) {
	s, sweeper, docs, c := swept(t)

	old := file(t, s, agent("eng"), work.NewItem{Title: "filed long ago"})
	// Two more edits, all still on the old clock.
	if _, err := s.Update(t.Context(), agent("eng"), old.Item.ID, 0,
		work.Edit{Title: ptr("renamed")}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, _, err := s.Comment(t.Context(), agent("eng"), old.Item.ID,
		work.NewComment{Body: "a remark"}); err != nil {
		t.Fatalf("comment: %v", err)
	}

	// A year and a day later, one fresh change.
	c.at = c.at.Add(work.ChangeRetention + 24*time.Hour)
	fresh := file(t, s, agent("eng"), work.NewItem{Title: "filed today"})

	before := keysOfClass(t, docs, work.ClassChange)
	if before != 4 {
		t.Fatalf("expected 4 change keys before the sweep, got %d", before)
	}

	n, err := sweeper.SweepChanges(t.Context(), c.at.Add(-work.ChangeRetention))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 3 {
		t.Errorf("swept %d changes, want the 3 that are past the horizon", n)
	}
	if got := keysOfClass(t, docs, work.ClassChange); got != 1 {
		t.Errorf("%d change keys survive, want only the fresh one", got)
	}

	// THE ITEMS AND THE THREAD ARE UNTOUCHED. This is the assertion that
	// makes the sweep safe to run at all: a company keeps its closed work
	// for ever, and the sweep must not be the thing that ends that.
	if got := keysOfClass(t, docs, work.ClassItem); got != 2 {
		t.Errorf("%d items survive the change sweep, want both", got)
	}
	if got := keysOfClass(t, docs, work.ClassComment); got != 1 {
		t.Errorf("%d comments survive the change sweep, want the one written", got)
	}
	if _, _, err := s.Item(t.Context(), old.Item.ID); err != nil {
		t.Errorf("the item whose whole history was swept is unreadable: %v", err)
	}
	if _, _, err := s.Item(t.Context(), fresh.Item.ID); err != nil {
		t.Errorf("the fresh item did not survive: %v", err)
	}
}

// A COMMENT WRITTEN CONCURRENTLY WITH A REMOVAL LOOKS EXACTLY LIKE ONE A CRASH
// LEFT BEHIND, and only time tells them apart. Sweeping inside the grace
// would delete somebody's live write; never sweeping leaves a key no reader
// can reach and no pass can find.
func TestAnOrphanCommentIsSweptOnlyPastItsGrace(t *testing.T) {
	s, sweeper, docs, c := swept(t)

	item := file(t, s, agent("eng"), work.NewItem{Title: "to be removed"})
	if _, _, err := s.Comment(t.Context(), agent("eng"), item.Item.ID,
		work.NewComment{Body: "a remark"}); err != nil {
		t.Fatalf("comment: %v", err)
	}
	// The head goes, the comment stays: exactly what a crash between the
	// two purges in Remove leaves behind.
	rec, found, err := docs.Document(t.Context(), coord.FamilyWork, work.ItemKey(item.Item.ID))
	if err != nil || !found {
		t.Fatalf("read the head: %v (found %v)", err, found)
	}
	if _, err := docs.PurgeDocument(t.Context(), coord.FamilyWork,
		work.ItemKey(item.Item.ID), rec.Version); err != nil {
		t.Fatalf("purge the head: %v", err)
	}

	// INSIDE THE GRACE: nothing goes.
	c.at = c.at.Add(work.OrphanGrace / 2)
	n, err := sweeper.SweepOrphans(t.Context(), c.at)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Errorf("swept %d comments inside the grace, want none — a comment "+
			"written a moment ago is indistinguishable from an orphan", n)
	}
	if got := keysOfClass(t, docs, work.ClassComment); got != 1 {
		t.Fatalf("%d comments survive inside the grace, want 1", got)
	}

	// PAST IT: only a crash explains the comment.
	c.at = c.at.Add(work.OrphanGrace)
	if n, err = sweeper.SweepOrphans(t.Context(), c.at); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Errorf("swept %d comments past the grace, want the orphan", n)
	}
	if got := keysOfClass(t, docs, work.ClassComment); got != 0 {
		t.Errorf("%d orphan comments survive the sweep, want none", got)
	}
}

// A COMMENT WHOSE ITEM IS ALIVE IS NEVER AN ORPHAN, however old it is. The
// sweep keys on the item's existence, not on the comment's age — a thread on
// a five-year-old item is the record the company keeps.
func TestAnOldCommentOnALiveItemStays(t *testing.T) {
	s, sweeper, docs, c := swept(t)

	item := file(t, s, agent("eng"), work.NewItem{Title: "still open"})
	if _, _, err := s.Comment(t.Context(), agent("eng"), item.Item.ID,
		work.NewComment{Body: "a remark"}); err != nil {
		t.Fatalf("comment: %v", err)
	}

	c.at = c.at.Add(5 * 365 * 24 * time.Hour)
	n, err := sweeper.SweepOrphans(t.Context(), c.at)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Errorf("swept %d comments off a live item, want none", n)
	}
	if got := keysOfClass(t, docs, work.ClassComment); got != 1 {
		t.Errorf("%d comments survive, want the one on the live item", got)
	}
}

// A RECORD THIS BUILD CANNOT READ IS ONE A PEER WROTE. Deleting it is how a
// rolling upgrade loses the newer half's history — and the sweep is the one
// pass that could do it silently, because nothing reads a change key back.
func TestAnUndecodableRecordIsLeftAlone(t *testing.T) {
	_, sweeper, docs, c := swept(t)

	key := work.ChangeKey("some-item", "a-change")
	if _, err := docs.CreateDocument(t.Context(), coord.FamilyWork, key,
		[]byte("{not json at all")); err != nil {
		t.Fatalf("plant: %v", err)
	}

	c.at = c.at.Add(10 * work.ChangeRetention)
	n, err := sweeper.SweepChanges(t.Context(), c.at.Add(-work.ChangeRetention))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Errorf("swept %d undecodable records, want none", n)
	}
	if _, found, err := docs.Document(t.Context(), coord.FamilyWork, key); err != nil || !found {
		t.Error("a record this build cannot decode was deleted by the sweep")
	}
}

package doclet_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// reaper is the actor the trash reaper purges as, keyed as it keys each purge.
func reaper() doclet.Actor {
	a := doclet.SystemActor("trash-reaper")
	a.OpKey = statelog.NewOpID(time.Now(), "trashreap")
	return a
}

// THE TRASH LISTS WHAT WENT IN BEFORE THE CUTOFF, OLDEST FIRST, and nothing
// else: a published page, a draft and a page restored from the trash are not
// the trash's, and a page trashed after the cutoff is still inside its term.
//
// Mutation: drop the status from the listing's predicate and a page trashed
// and restored is listed (its `trashed_at` is cleared on restore, so drop that
// too and it is listed again); order by id alone and the oldest is not first.
func TestTheTrashListsWhatWentInBeforeTheCutoffOldestFirst(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	// THE IDS ARE DERIVED, so the order they sort in says nothing about
	// the order the pages went into the trash; the case trashes them in
	// the reverse of their id order where it can, so a listing by id fails.
	a := r.write(author("jane"), doclet.NewPage{Title: "Alpha", Body: "a"}).Page.ID
	b := r.write(author("jane"), doclet.NewPage{Title: "Bravo", Body: "b"}).Page.ID
	first, second := a, b
	if a < b {
		first, second = b, a
	}
	restored := r.write(author("jane"), doclet.NewPage{Title: "Back", Body: "r"}).Page.ID
	r.write(author("jane"), doclet.NewPage{Title: "Live", Body: "l"})
	r.write(author("jane"), doclet.NewPage{Title: "Draft", Body: "d",
		Status: doclet.StatusDraft})

	before := time.Now().Add(-time.Hour)
	for _, id := range []string{first, restored, second} {
		if _, err := r.store.Trash(t.Context(), author("jane"), id); err != nil {
			t.Fatalf("trash %s: %v", id, err)
		}
		r.drain()
		// The broker stamps the instant; a millisecond apart keeps the
		// two trashes distinct at the column's resolution.
		time.Sleep(2 * time.Millisecond)
	}
	if _, err := r.store.Restore(t.Context(), author("jane"), restored); err != nil {
		t.Fatalf("restore: %v", err)
	}
	r.drain()

	later := time.Now().Add(time.Hour)
	got, err := r.store.TrashedBefore(t.Context(), later, 10)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{first, second}; !slices.Equal(got, want) {
		t.Fatalf("the trash lists %v, want %v (oldest first, nothing restored)", got, want)
	}
	if got, err := r.store.TrashedBefore(t.Context(), later, 1); err != nil ||
		!slices.Equal(got, []string{first}) {
		t.Errorf("a listing of one answers %v (%v), want the oldest", got, err)
	}
	if got, err := r.store.TrashedBefore(t.Context(), before, 10); err != nil || len(got) != 0 {
		t.Errorf("before anything went in the trash lists %v (%v)", got, err)
	}
	for name, call := range map[string]func() error{
		"a zero cutoff": func() error {
			_, err := r.store.TrashedBefore(t.Context(), time.Time{}, 10)
			return err
		},
		"no limit": func() error {
			_, err := r.store.TrashedBefore(t.Context(), later, 0)
			return err
		},
	} {
		if err := call(); !errors.Is(err, doclet.ErrInvalid) {
			t.Errorf("%s answers %v, want a field refusal", name, err)
		}
	}
}

// THE REAPER PURGES ONLY A PAGE STILL IN THE TRASH SINCE BEFORE THE CUTOFF, and
// decides that in the record's own snapshot: a page restored, published or
// trashed after the cutoff is refused, and keeps every row it had.
//
// Mutation: drop the admit from [doclet.Store.PurgeTrashed] and each refused
// case below purges a page a person kept.
func TestTheReaperPurgesOnlyAPageStillInTheTrashPastTheCutoff(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	doomed := r.write(author("jane"), doclet.NewPage{Title: "Doomed", Body: "x"}).Page.ID
	kept := r.write(author("jane"), doclet.NewPage{Title: "Kept", Body: "k"}).Page.ID
	young := r.write(author("jane"), doclet.NewPage{Title: "Young", Body: "y"}).Page.ID
	back := r.write(author("jane"), doclet.NewPage{Title: "Back", Body: "b"}).Page.ID
	for _, id := range []string{doomed, back} {
		if _, err := r.store.Trash(t.Context(), author("jane"), id); err != nil {
			t.Fatal(err)
		}
	}
	r.drain()
	cutoff := time.Now().Add(time.Hour)
	// A PAGE RESTORED after it was listed: the purge was asked for on the
	// listing, and the restore landed first.
	listed, err := r.store.TrashedBefore(t.Context(), cutoff, 10)
	if err != nil || !slices.Contains(listed, back) {
		t.Fatalf("the trash lists %v (%v), want the page about to be restored", listed, err)
	}
	if _, err := r.store.Restore(t.Context(), author("jane"), back); err != nil {
		t.Fatal(err)
	}
	r.drain()
	// A PAGE TRASHED AFTER THE CUTOFF: its term has not run.
	youngCutoff := time.Now().Add(-time.Second)
	if _, err := r.store.Trash(t.Context(), author("jane"), young); err != nil {
		t.Fatal(err)
	}
	r.drain()

	for name, c := range map[string]struct {
		id     string
		cutoff time.Time
	}{
		"a published page":                {kept, cutoff},
		"a page restored after its list":  {back, cutoff},
		"a page trashed after the cutoff": {young, youngCutoff},
	} {
		if _, err := r.store.PurgeTrashed(t.Context(), reaper(), c.id, c.cutoff); !errors.Is(err, doclet.ErrNotInTrash) {
			t.Errorf("%s: the purge answers %v, want ErrNotInTrash", name, err)
		}
		r.drain()
		if got := r.get(c.id); got.Page.ID != c.id {
			t.Errorf("%s: the page is gone after a refused purge", name)
		}
	}

	written, err := r.store.PurgeTrashed(t.Context(), reaper(), doomed, cutoff)
	if err != nil {
		t.Fatalf("purge the page past its term: %v", err)
	}
	r.drain()
	if written.Outcome.Outcome != statelog.OutcomeApplied {
		t.Errorf("the purge answered %s", written.Outcome.Outcome)
	}
	if _, err := r.reader.Get(t.Context(), doomed,
		statelog.Freshness{Level: statelog.ReadSession}); !errors.Is(err, doclet.ErrNotFound) {
		t.Errorf("the purged page still reads: %v", err)
	}
	// A SECOND REAPER finds it gone, and is told so rather than purging.
	if _, err := r.store.PurgeTrashed(t.Context(), reaper(), doomed, cutoff); !errors.Is(err, doclet.ErrNotFound) {
		t.Errorf("a second purge answers %v, want ErrNotFound", err)
	}
	if _, err := r.store.PurgeTrashed(t.Context(), reaper(), doomed, time.Time{}); !errors.Is(err, doclet.ErrInvalid) {
		t.Errorf("a purge with no cutoff answers %v, want a field refusal", err)
	}
}

// THE REAPER'S PURGE IS THE ENGINE'S, SAYS WHY, AND WAKES NOBODY: the record
// names the reaper under the operator kind the knowledge base records the
// engine with, the deletion marker keeps the cutoff as its reason, and the
// record carries no routing snapshot, so no watcher is woken a month after the
// trash already told them.
//
// Mutation: purge loudly and the record carries a Notify.
func TestTheReapersPurgeIsTheEnginesAndWakesNobody(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	doomed := r.write(agent("bob"), doclet.NewPage{Title: "Watched", Body: "w"}).Page.ID
	if _, err := r.store.Trash(t.Context(), author("jane"), doomed); err != nil {
		t.Fatal(err)
	}
	r.drain()
	cutoff := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	if _, err := r.store.PurgeTrashed(t.Context(), reaper(), doomed, cutoff); err != nil {
		t.Fatal(err)
	}
	r.drain()

	record, err := doclet.Decode(r.lastRecord())
	if err != nil {
		t.Fatal(err)
	}
	if record.Op != doclet.OpPurge || record.Actor != "trash-reaper" ||
		record.ActorKind != doclet.AuthorOperator {
		t.Errorf("the purge is recorded as %s by %q (%s)", record.Op, record.Actor,
			record.ActorKind)
	}
	if record.Notify != nil {
		t.Errorf("the reaper's purge routes to %v; it must wake nobody", record.Notify.Recipients)
	}
	var by, reason string
	if err := storetest.ReplicatedDB(t, r.db).SQL().QueryRowContext(t.Context(),
		`SELECT by, reason FROM pages_deletions WHERE page_id = ?`, doomed).
		Scan(&by, &reason); err != nil {
		t.Fatalf("read the deletion marker: %v", err)
	}
	if by != "trash-reaper" || !strings.Contains(reason, cutoff.Format(time.RFC3339)) {
		t.Errorf("the marker says %q purged it because %q", by, reason)
	}
}

// A RETRY OF THE REAPER'S PURGE UNDER ITS KEY IS THE FIRST PURGE: the reaper
// retries within a pass under the operation it derived, and the ledger answers
// the first purge's position rather than finding the page gone.
func TestAReapersRetryUnderItsKeyIsTheFirstPurge(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	doomed := r.write(author("jane"), doclet.NewPage{Title: "Again", Body: "a"}).Page.ID
	if _, err := r.store.Trash(t.Context(), author("jane"), doomed); err != nil {
		t.Fatal(err)
	}
	r.drain()
	cutoff := time.Now().Add(time.Hour)
	actor := reaper()
	first, err := r.store.PurgeTrashed(t.Context(), actor, doomed, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	r.drain()
	again, err := r.store.PurgeTrashed(t.Context(), actor, doomed, cutoff)
	if err != nil || again.ChangeID != first.ChangeID || !again.Outcome.Collapsed {
		t.Fatalf("the retry answered %q (collapsed %v, %v), want the first purge %q",
			again.ChangeID, again.Outcome.Collapsed, err, first.ChangeID)
	}
}

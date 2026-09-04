package coordtest

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// documentCases certify the fleet's document store.
//
// Every case here is an invariant a projection or a change feed depends on,
// and each one names what breaks without it. A memory twin that satisfied
// only itself would certify the bug: the KV backend is the one that has to
// hold under a replicated stream, a lagging replica and a compacting bucket,
// and these are the questions whose answers must not differ between them.
var documentCases = []fleetCase{
	{"a create is first-writer-wins and losing is not a fault", func(h *fleetHarness) {
		key := coord.DocumentKey("i", "one")
		created, err := h.f.CreateDocument(h.ctx, coord.FamilyWork, key, []byte(`{"n":1}`))
		if err != nil || !created {
			h.t.Fatalf("first create = %v, %v; want true, nil", created, err)
		}
		// A SECOND CREATE IS NOT AN ERROR. A re-run turn mints the same
		// deterministic id and creates again; if that came back as a
		// failure the turn would report work it had actually done as
		// broken, and a rescue path would do it twice.
		created, err = h.f.CreateDocument(h.ctx, coord.FamilyWork, key, []byte(`{"n":2}`))
		if err != nil {
			h.t.Fatalf("second create errored: %v", err)
		}
		if created {
			h.t.Error("a second create reported the document as new")
		}
		record, ok, err := h.f.Document(h.ctx, coord.FamilyWork, key)
		if err != nil || !ok {
			h.t.Fatalf("read back = %v, %v", ok, err)
		}
		if string(record.Value) != `{"n":1}` {
			h.t.Errorf("value = %s, want the first writer's", record.Value)
		}
	}},

	{"an update at a stale version is a lost race, not a fault", func(h *fleetHarness) {
		key := coord.DocumentKey("i", "two")
		if _, err := h.f.CreateDocument(h.ctx, coord.FamilyWork, key, []byte(`a`)); err != nil {
			h.t.Fatal(err)
		}
		record, _, err := h.f.Document(h.ctx, coord.FamilyWork, key)
		if err != nil {
			h.t.Fatal(err)
		}
		held, err := h.f.UpdateDocument(h.ctx, coord.FamilyWork, key, []byte(`b`), record.Version)
		if err != nil || !held {
			h.t.Fatalf("update at the read version = %v, %v; want true, nil", held, err)
		}
		// The SAME version again: somebody else moved the document on.
		// False and no error, because the caller's next move is to
		// re-read and re-decide — an error would send it to a retry loop
		// for a store that is working perfectly.
		held, err = h.f.UpdateDocument(h.ctx, coord.FamilyWork, key, []byte(`c`), record.Version)
		if err != nil {
			h.t.Fatalf("a stale update errored: %v", err)
		}
		if held {
			h.t.Error("a stale version was accepted")
		}
	}},

	{"a purge at a stale version leaves the document", func(h *fleetHarness) {
		key := coord.DocumentKey("i", "three")
		if _, err := h.f.CreateDocument(h.ctx, coord.FamilyWork, key, []byte(`a`)); err != nil {
			h.t.Fatal(err)
		}
		first, _, _ := h.f.Document(h.ctx, coord.FamilyWork, key)
		if _, err := h.f.UpdateDocument(h.ctx, coord.FamilyWork, key, []byte(`b`), first.Version); err != nil {
			h.t.Fatal(err)
		}
		gone, err := h.f.PurgeDocument(h.ctx, coord.FamilyWork, key, first.Version)
		if err != nil {
			h.t.Fatalf("a stale purge errored: %v", err)
		}
		if gone {
			h.t.Error("a purge at a superseded version removed the document")
		}
		if _, ok, _ := h.f.Document(h.ctx, coord.FamilyWork, key); !ok {
			h.t.Error("the document is gone after a refused purge")
		}
	}},

	{"a purged key can be created again", func(h *fleetHarness) {
		// A DELETE LEAVES A TOMBSTONE and a purge does not, which is why
		// every removal here is a purge: these buckets have no age, so a
		// tombstone would outlive the deployment and a create stepping
		// over it is the difference between reusing an id and refusing
		// one forever.
		key := coord.DocumentKey("i", "four")
		if _, err := h.f.CreateDocument(h.ctx, coord.FamilyWork, key, []byte(`a`)); err != nil {
			h.t.Fatal(err)
		}
		record, _, _ := h.f.Document(h.ctx, coord.FamilyWork, key)
		if gone, err := h.f.PurgeDocument(h.ctx, coord.FamilyWork, key, record.Version); err != nil || !gone {
			h.t.Fatalf("purge = %v, %v", gone, err)
		}
		created, err := h.f.CreateDocument(h.ctx, coord.FamilyWork, key, []byte(`b`))
		if err != nil {
			h.t.Fatalf("re-create errored: %v", err)
		}
		if !created {
			h.t.Error("a purged key refused a fresh create")
		}
	}},

	{"a listing selects whole key classes", func(h *fleetHarness) {
		// The sharpest shape of the class rule: "c" must not select
		// "counter", or a change-key sweep purges the project counters
		// and every item minted afterwards reuses a number.
		for _, key := range []string{
			coord.DocumentKey("c", "item", "01"),
			coord.DocumentKey("c", "item", "02"),
			coord.DocumentKey("counter", "ENG"),
			coord.DocumentKey("i", "item"),
		} {
			if _, err := h.f.CreateDocument(h.ctx, coord.FamilyWork, key, []byte(`x`)); err != nil {
				h.t.Fatal(err)
			}
		}
		got, err := h.f.Documents(h.ctx, coord.FamilyWork, "c")
		if err != nil {
			h.t.Fatal(err)
		}
		var keys []string
		for _, r := range got {
			keys = append(keys, r.Key)
		}
		slices.Sort(keys)
		want := []string{coord.DocumentKey("c", "item", "01"), coord.DocumentKey("c", "item", "02")}
		if !slices.Equal(keys, want) {
			h.t.Errorf("prefix listing = %v, want %v", keys, want)
		}
	}},

	{"an unknown family is refused rather than answered", func(h *fleetHarness) {
		_, _, err := h.f.Document(h.ctx, coord.Family("ledger"), coord.DocumentKey("i", "x"))
		if err == nil {
			h.t.Error("an unknown family answered instead of refusing")
		}
		if _, err := h.f.CreateDocument(h.ctx, coord.Family(""), "k", nil); err == nil {
			h.t.Error("an empty family was accepted")
		}
	}},

	{"a watch opens with what is there, marks caught up, then follows", func(h *fleetHarness) {
		seeded := coord.DocumentKey("i", "seeded")
		if _, err := h.f.CreateDocument(h.ctx, coord.FamilyWork, seeded, []byte(`a`)); err != nil {
			h.t.Fatal(err)
		}
		w, err := h.f.WatchDocuments(h.ctx, coord.FamilyWork, 0)
		if err != nil {
			h.t.Fatal(err)
		}
		defer func() { _ = w.Stop() }()

		initial := drainToMarker(h.t, w)
		if len(initial) != 1 || initial[0].Key != seeded {
			h.t.Fatalf("initial pass = %v, want the one seeded document", keysOf(initial))
		}

		// LIVE, after the marker. A projector that could not tell the two
		// apart would either serve an empty board as though it were the
		// company's or wait forever for a family that is genuinely empty.
		live := coord.DocumentKey("i", "live")
		if _, err := h.f.CreateDocument(h.ctx, coord.FamilyWork, live, []byte(`b`)); err != nil {
			h.t.Fatal(err)
		}
		change := next(h.t, w)
		if change == nil || change.Key != live || change.Op != coord.OpPut {
			h.t.Fatalf("live change = %#v, want a put of %s", change, live)
		}
		if change.Revision <= initial[0].Revision {
			h.t.Errorf("revision %d did not advance past the seeded %d",
				change.Revision, initial[0].Revision)
		}
	}},

	{"a watch delivers a purge", func(h *fleetHarness) {
		// DELETES TRAVEL, unlike the memory changelog's. Nothing
		// re-converges a work item: a projection that never saw the
		// removal keeps it on somebody's board until a person deletes it
		// a second time.
		key := coord.DocumentKey("i", "doomed")
		if _, err := h.f.CreateDocument(h.ctx, coord.FamilyWork, key, []byte(`a`)); err != nil {
			h.t.Fatal(err)
		}
		w, err := h.f.WatchDocuments(h.ctx, coord.FamilyWork, 0)
		if err != nil {
			h.t.Fatal(err)
		}
		defer func() { _ = w.Stop() }()
		drainToMarker(h.t, w)

		record, _, _ := h.f.Document(h.ctx, coord.FamilyWork, key)
		if gone, err := h.f.PurgeDocument(h.ctx, coord.FamilyWork, key, record.Version); err != nil || !gone {
			h.t.Fatalf("purge = %v, %v", gone, err)
		}
		change := next(h.t, w)
		if change == nil || change.Op != coord.OpPurge || change.Key != key {
			h.t.Fatalf("change = %#v, want a purge of %s", change, key)
		}
	}},

	{"a watch resumed from a revision replays only what followed", func(h *fleetHarness) {
		first := coord.DocumentKey("i", "first")
		if _, err := h.f.CreateDocument(h.ctx, coord.FamilyWork, first, []byte(`a`)); err != nil {
			h.t.Fatal(err)
		}
		mark, _, _ := h.f.Document(h.ctx, coord.FamilyWork, first)
		second := coord.DocumentKey("i", "second")
		if _, err := h.f.CreateDocument(h.ctx, coord.FamilyWork, second, []byte(`b`)); err != nil {
			h.t.Fatal(err)
		}

		w, err := h.f.WatchDocuments(h.ctx, coord.FamilyWork, mark.Version+1)
		if err != nil {
			h.t.Fatal(err)
		}
		defer func() { _ = w.Stop() }()

		// A resumed watch is how a node that was away catches up without
		// re-reading a family it already holds. Handing it the first
		// document again would be harmless; NOT handing it the second
		// would leave a hole nothing detects.
		change := next(h.t, w)
		if change == nil || change.Key != second {
			h.t.Fatalf("resumed change = %#v, want %s", change, second)
		}
	}},

	{"an exact revision read never answers absent for a store that is behind", func(h *fleetHarness) {
		key := coord.DocumentKey("i", "exact")
		if _, err := h.f.CreateDocument(h.ctx, coord.FamilyWork, key, []byte(`a`)); err != nil {
			h.t.Fatal(err)
		}
		record, _, _ := h.f.Document(h.ctx, coord.FamilyWork, key)
		got, ok, err := h.f.DocumentAt(h.ctx, coord.FamilyWork, key, record.Version)
		if err != nil || !ok {
			h.t.Fatalf("exact read = %v, %v; want the document", ok, err)
		}
		if got.Version != record.Version {
			h.t.Errorf("version = %d, want %d", got.Version, record.Version)
		}
		// A revision this store does not hold is UNKNOWN or absent, never
		// an error the caller mistakes for a document that was deleted.
		// Both backends answer it without panicking, which is the whole
		// assertion: the KV backend raises (a replica may simply be
		// behind), the twin reports absent, and neither invents a value.
		if _, ok, err := h.f.DocumentAt(h.ctx, coord.FamilyWork, key, record.Version+99); ok {
			h.t.Errorf("a revision nobody wrote was answered: %v", err)
		}
	}},

	{"families are separate stores", func(h *fleetHarness) {
		// One key, two families. A projection reads one family at a time
		// and a cursor is a position in one bucket's sequence, so a
		// backend that shared a namespace would make a page's revision
		// advance because somebody filed a work item.
		key := coord.DocumentKey("x", "shared")
		if _, err := h.f.CreateDocument(h.ctx, coord.FamilyWork, key, []byte(`work`)); err != nil {
			h.t.Fatal(err)
		}
		if _, err := h.f.CreateDocument(h.ctx, coord.FamilyPages, key, []byte(`pages`)); err != nil {
			h.t.Fatal(err)
		}
		work, _, err := h.f.Document(h.ctx, coord.FamilyWork, key)
		if err != nil {
			h.t.Fatal(err)
		}
		pages, _, err := h.f.Document(h.ctx, coord.FamilyPages, key)
		if err != nil {
			h.t.Fatal(err)
		}
		if string(work.Value) != "work" || string(pages.Value) != "pages" {
			h.t.Errorf("families share a namespace: %s / %s", work.Value, pages.Value)
		}
	}},

	{"an unreachable store never reports a document as absent", func(h *fleetHarness) {
		// THE TRI-STATE, on the read a seat acts on. "There is no such
		// item" makes a turn file a duplicate or abandon work it was
		// asked to do; a store that could not be reached must never be
		// able to say it.
		faulty, ok := h.f.(interface{ FailNext(error) })
		if !ok {
			h.t.Skip("this backend cannot be made to fail on demand")
		}
		want := errors.New("the store is unreachable")
		faulty.FailNext(want)
		_, exists, err := h.f.Document(h.ctx, coord.FamilyWork, coord.DocumentKey("i", "any"))
		if err == nil {
			h.t.Fatal("an unreachable store answered a read")
		}
		if exists {
			h.t.Error("a failed read reported the document as existing")
		}
	}},
}

// drainToMarker reads the watch's opening pass, stopping at the caught-up
// marker.
func drainToMarker(t *testing.T, w coord.Watcher) []coord.Change {
	t.Helper()
	var out []coord.Change
	for {
		select {
		case change, open := <-w.Changes():
			if !open {
				t.Fatal("the watch closed before its caught-up marker")
			}
			if change == nil {
				return out
			}
			out = append(out, *change)
		case <-time.After(watchBudget):
			t.Fatalf("the watch produced no caught-up marker in %v", watchBudget)
		}
	}
}

// next reads one change, failing the test if none arrives.
func next(t *testing.T, w coord.Watcher) *coord.Change {
	t.Helper()
	for {
		select {
		case change, open := <-w.Changes():
			if !open {
				t.Fatal("the watch closed")
			}
			if change == nil {
				// A marker from a resumed watch: not the change we are
				// waiting for, and not an error either.
				continue
			}
			return change
		case <-time.After(watchBudget):
			t.Fatalf("no change arrived in %v", watchBudget)
			return nil
		}
	}
}

func keysOf(changes []coord.Change) []string {
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		out = append(out, c.Key)
	}
	return out
}

// watchBudget is how long a case waits for a change.
//
// Generous, because it bounds a REAL BROKER round trip on the KV backend and
// a scheduler hop on the twin — and because a watch that is merely slow and
// one that is broken look identical until the budget expires, so the cost of
// setting it too low is a suite that fails on a loaded machine.
const watchBudget = 10 * time.Second

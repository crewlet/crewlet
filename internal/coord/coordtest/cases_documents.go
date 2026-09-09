package coordtest

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/queue"
)

// documentCases certify the fleet's document store.
//
// Every case here is an invariant a projection or a change feed depends on,
// and each one names what breaks without it. A memory twin that satisfied
// only itself would certify the bug: the KV backend is the one that has to
// hold under a replicated stream, a lagging replica and a compacting bucket,
// and these are the questions whose answers must not differ between them.
var documentCases = []fleetCase{
	{"a trim hold round-trips, and it never reads back as a node", func(h *fleetHarness) {
		// THE TWO KEY CLASSES SHARE ONE REGISTER, and this case is the
		// whole reason that is safe. A hold decoded as a positions row
		// is a node id of "" with a domains map of zero values, which
		// the trim reads as a node that has applied NOTHING — so the
		// pin becomes a permanent floor at zero, from a key nobody
		// thinks of as a node, and the log grows to its ceiling.
		if err := h.f.PutPositions(h.ctx, coord.NodePositions{
			NodeID:  "node-a",
			Domains: map[string]coord.DomainPosition{"tracker": {Seq: 90, Generation: 1}},
		}); err != nil {
			h.t.Fatalf("PutPositions: %v", err)
		}
		hold := coord.TrimHold{
			Owner:  coord.HoldOwner("node-a", "backup"),
			Reason: "a backup is copying the store",
			Domains: map[string]coord.Position{
				"tracker": {Stream: "CREWLET_TRACKER_LOG", Generation: 1, Seq: 88},
			},
		}
		if err := h.f.PutHold(h.ctx, hold); err != nil {
			h.t.Fatalf("PutHold: %v", err)
		}

		nodes, err := h.f.Positions(h.ctx)
		if err != nil {
			h.t.Fatalf("Positions: %v", err)
		}
		if len(nodes) != 1 || nodes[0].NodeID != "node-a" {
			h.t.Fatalf("the register lists %d node row(s) with a hold beside "+
				"one node: %+v — a hold read as a node pins the trim at zero "+
				"for ever", len(nodes), nodes)
		}

		holds, err := h.f.Holds(h.ctx)
		if err != nil {
			h.t.Fatalf("Holds: %v", err)
		}
		if len(holds) != 1 || holds[0].Owner != hold.Owner {
			h.t.Fatalf("the register lists %d hold(s): %+v", len(holds), holds)
		}
		if got := holds[0].Domains["tracker"]; got.Seq != 88 || got.Stream == "" {
			h.t.Fatalf("the hold came back pinning %+v — a bare sequence from "+
				"before a reanchor names a dead number space", got)
		}
		if holds[0].At.IsZero() {
			h.t.Fatal("the hold came back with no renewal instant, so the " +
				"stale bound that stops a crashed holder pinning the log for " +
				"ever has nothing to compare")
		}

		// RELEASE IS THE NORMAL PATH, and it must not take the node's
		// row with it.
		if err := h.f.ReleaseHold(h.ctx, hold.Owner); err != nil {
			h.t.Fatalf("ReleaseHold: %v", err)
		}
		holds, err = h.f.Holds(h.ctx)
		if err != nil {
			h.t.Fatalf("Holds after release: %v", err)
		}
		if len(holds) != 0 {
			h.t.Fatalf("%d hold(s) survive a release", len(holds))
		}
		nodes, err = h.f.Positions(h.ctx)
		if err != nil {
			h.t.Fatalf("Positions after release: %v", err)
		}
		if len(nodes) != 1 {
			h.t.Fatalf("releasing a hold left %d node row(s)", len(nodes))
		}
	}},
	{"a hold that pins nothing is refused", func(h *fleetHarness) {
		// A hold with no owner would be written over somebody else's pin
		// and released by their work finishing; one naming no domain
		// pins nothing while looking like a pin.
		if err := h.f.PutHold(h.ctx, coord.TrimHold{
			Domains: map[string]coord.Position{"tracker": {Stream: "S", Seq: 1}},
		}); err == nil {
			h.t.Error("a hold with no owner was written")
		}
		if err := h.f.PutHold(h.ctx, coord.TrimHold{Owner: "node-a/backup"}); err == nil {
			h.t.Error("a hold naming no domain was written")
		}
		if err := h.f.PutHold(h.ctx, coord.TrimHold{
			Owner:   "node-a/backup",
			Domains: map[string]coord.Position{"tracker": {Seq: 1}},
		}); err == nil {
			h.t.Error("a hold pinning a bare sequence with no stream was written")
		}
	}},

	{"a node's positions round-trip, and only an operator removes a row", func(h *fleetHarness) {
		row := coord.NodePositions{
			NodeID:        "node-a",
			EngineVersion: "v-test",
			Domains: map[string]coord.DomainPosition{
				"tracker": {Seq: 41, Generation: 2, AppliedThrough: 40, Deferred: 1},
				"vectors": {Seq: 9, Generation: 2, AppliedThrough: 9},
			},
		}
		if err := h.f.PutPositions(h.ctx, row); err != nil {
			h.t.Fatalf("PutPositions: %v", err)
		}
		// A SECOND WRITE REPLACES rather than merges: the row is one
		// node's whole answer, and a merge would leave a domain it no
		// longer runs behind for ever, pinning the trim on a log nothing
		// consumes.
		row.Domains = map[string]coord.DomainPosition{
			"tracker": {Seq: 60, Generation: 2, AppliedThrough: 60},
		}
		if err := h.f.PutPositions(h.ctx, row); err != nil {
			h.t.Fatalf("second PutPositions: %v", err)
		}

		rows, err := h.f.Positions(h.ctx)
		if err != nil {
			h.t.Fatalf("Positions: %v", err)
		}
		if len(rows) != 1 {
			h.t.Fatalf("register holds %d rows, want 1", len(rows))
		}
		got := rows[0]
		if got.NodeID != "node-a" || len(got.Domains) != 1 {
			h.t.Fatalf("row = %+v, want node-a with one domain", got)
		}
		if d := got.Domains["tracker"]; d.Seq != 60 || d.AppliedThrough != 60 {
			h.t.Errorf("tracker = %+v, want seq 60 applied 60", d)
		}
		if got.At.IsZero() {
			h.t.Error("At is zero: the register's own freshness is what an " +
				"operator reads to tell a stalled node from a departed one")
		}

		// AN UNATTRIBUTED ROW IS REFUSED. It would be read back as some
		// other node's progress, and the trim takes a minimum across
		// these rows.
		if err := h.f.PutPositions(h.ctx, coord.NodePositions{}); err == nil {
			h.t.Error("a row with no node id was accepted")
		}
		// AND SO IS ONE THAT APPLIED PAST WHAT IT CONSUMED.
		if err := h.f.PutPositions(h.ctx, coord.NodePositions{
			NodeID:  "node-b",
			Domains: map[string]coord.DomainPosition{"tracker": {Seq: 5, AppliedThrough: 9}},
		}); err == nil {
			h.t.Error("a row applied past its own checkpoint was accepted")
		}

		if err := h.f.ForgetPositions(h.ctx, "node-a"); err != nil {
			h.t.Fatalf("ForgetPositions: %v", err)
		}
		rows, err = h.f.Positions(h.ctx)
		if err != nil {
			h.t.Fatalf("Positions after forget: %v", err)
		}
		if len(rows) != 0 {
			h.t.Errorf("register holds %d rows after the operator's gesture, want 0", len(rows))
		}
		// FORGETTING TWICE IS NOT A FAULT: an eviction that is retried
		// must not fail on the half it already did.
		if err := h.f.ForgetPositions(h.ctx, "node-a"); err != nil {
			h.t.Errorf("a second forget errored: %v", err)
		}
	}},
	{"a key listing is the same set as a listing, without the values", func(h *fleetHarness) {
		// THE TWO ANSWERS MUST AGREE, because the whole reason
		// DocumentKeys exists is that a sweep asking "which keys are
		// there" was transferring every value in the family to find out.
		// A filter that disagreed with the listing's would make a sweep
		// miss records or reach into another class's.
		for _, key := range []string{
			coord.DocumentKey("i", "one"),
			coord.DocumentKey("i", "two"),
			coord.DocumentKey("c", "one"),
			// The prefix ITSELF is a key this engine writes, and a
			// filter of `i.>` alone would miss it.
			"i",
			// A byte-wise prefix would take this for an "i" key, which
			// is the reason the match is whole-segment.
			"index",
		} {
			if _, err := h.f.CreateDocument(h.ctx, coord.FamilyPages, key, []byte(`{}`)); err != nil {
				h.t.Fatalf("seed %s: %v", key, err)
			}
		}

		for _, prefix := range []string{"", "i", "c"} {
			records, err := h.f.Documents(h.ctx, coord.FamilyPages, prefix)
			if err != nil {
				h.t.Fatalf("Documents(%q): %v", prefix, err)
			}
			keys, err := h.f.DocumentKeys(h.ctx, coord.FamilyPages, prefix)
			if err != nil {
				h.t.Fatalf("DocumentKeys(%q): %v", prefix, err)
			}
			want := make([]string, 0, len(records))
			for _, r := range records {
				want = append(want, r.Key)
			}
			slices.Sort(want)
			got := slices.Clone(keys)
			slices.Sort(got)
			if !slices.Equal(got, want) {
				h.t.Errorf("prefix %q: DocumentKeys = %v, Documents = %v: the "+
					"server-side filter and the client-side one have to select "+
					"the same set, or a sweep reaches into another class",
					prefix, got, want)
			}
		}
	}},
	{"a create is first-writer-wins and losing is not a fault", func(h *fleetHarness) {
		key := coord.DocumentKey("i", "one")
		created, err := h.f.CreateDocument(h.ctx, coord.FamilyPages, key, []byte(`{"n":1}`))
		if err != nil || !created {
			h.t.Fatalf("first create = %v, %v; want true, nil", created, err)
		}
		// A SECOND CREATE IS NOT AN ERROR. A re-run turn mints the same
		// deterministic id and creates again; if that came back as a
		// failure the turn would report work it had actually done as
		// broken, and a rescue path would do it twice.
		created, err = h.f.CreateDocument(h.ctx, coord.FamilyPages, key, []byte(`{"n":2}`))
		if err != nil {
			h.t.Fatalf("second create errored: %v", err)
		}
		if created {
			h.t.Error("a second create reported the document as new")
		}
		record, ok, err := h.f.Document(h.ctx, coord.FamilyPages, key)
		if err != nil || !ok {
			h.t.Fatalf("read back = %v, %v", ok, err)
		}
		if string(record.Value) != `{"n":1}` {
			h.t.Errorf("value = %s, want the first writer's", record.Value)
		}
	}},

	{"an update at a stale version is a lost race, not a fault", func(h *fleetHarness) {
		key := coord.DocumentKey("i", "two")
		if _, err := h.f.CreateDocument(h.ctx, coord.FamilyPages, key, []byte(`a`)); err != nil {
			h.t.Fatal(err)
		}
		record, _, err := h.f.Document(h.ctx, coord.FamilyPages, key)
		if err != nil {
			h.t.Fatal(err)
		}
		held, err := h.f.UpdateDocument(h.ctx, coord.FamilyPages, key, []byte(`b`), record.Version)
		if err != nil || !held {
			h.t.Fatalf("update at the read version = %v, %v; want true, nil", held, err)
		}
		// The SAME version again: somebody else moved the document on.
		// False and no error, because the caller's next move is to
		// re-read and re-decide — an error would send it to a retry loop
		// for a store that is working perfectly.
		held, err = h.f.UpdateDocument(h.ctx, coord.FamilyPages, key, []byte(`c`), record.Version)
		if err != nil {
			h.t.Fatalf("a stale update errored: %v", err)
		}
		if held {
			h.t.Error("a stale version was accepted")
		}
	}},

	{"a purge at a stale version leaves the document", func(h *fleetHarness) {
		key := coord.DocumentKey("i", "three")
		if _, err := h.f.CreateDocument(h.ctx, coord.FamilyPages, key, []byte(`a`)); err != nil {
			h.t.Fatal(err)
		}
		first, _, _ := h.f.Document(h.ctx, coord.FamilyPages, key)
		if _, err := h.f.UpdateDocument(h.ctx, coord.FamilyPages, key, []byte(`b`), first.Version); err != nil {
			h.t.Fatal(err)
		}
		gone, err := h.f.PurgeDocument(h.ctx, coord.FamilyPages, key, first.Version)
		if err != nil {
			h.t.Fatalf("a stale purge errored: %v", err)
		}
		if gone {
			h.t.Error("a purge at a superseded version removed the document")
		}
		if _, ok, _ := h.f.Document(h.ctx, coord.FamilyPages, key); !ok {
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
		if _, err := h.f.CreateDocument(h.ctx, coord.FamilyPages, key, []byte(`a`)); err != nil {
			h.t.Fatal(err)
		}
		record, _, _ := h.f.Document(h.ctx, coord.FamilyPages, key)
		if gone, err := h.f.PurgeDocument(h.ctx, coord.FamilyPages, key, record.Version); err != nil || !gone {
			h.t.Fatalf("purge = %v, %v", gone, err)
		}
		created, err := h.f.CreateDocument(h.ctx, coord.FamilyPages, key, []byte(`b`))
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
			if _, err := h.f.CreateDocument(h.ctx, coord.FamilyPages, key, []byte(`x`)); err != nil {
				h.t.Fatal(err)
			}
		}
		got, err := h.f.Documents(h.ctx, coord.FamilyPages, "c")
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
		if _, err := h.f.CreateDocument(h.ctx, coord.FamilyPages, seeded, []byte(`a`)); err != nil {
			h.t.Fatal(err)
		}
		w, err := h.f.WatchDocuments(h.ctx, coord.FamilyPages, 0)
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
		if _, err := h.f.CreateDocument(h.ctx, coord.FamilyPages, live, []byte(`b`)); err != nil {
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
		if _, err := h.f.CreateDocument(h.ctx, coord.FamilyPages, key, []byte(`a`)); err != nil {
			h.t.Fatal(err)
		}
		w, err := h.f.WatchDocuments(h.ctx, coord.FamilyPages, 0)
		if err != nil {
			h.t.Fatal(err)
		}
		defer func() { _ = w.Stop() }()
		drainToMarker(h.t, w)

		record, _, _ := h.f.Document(h.ctx, coord.FamilyPages, key)
		if gone, err := h.f.PurgeDocument(h.ctx, coord.FamilyPages, key, record.Version); err != nil || !gone {
			h.t.Fatalf("purge = %v, %v", gone, err)
		}
		change := next(h.t, w)
		if change == nil || change.Op != coord.OpPurge || change.Key != key {
			h.t.Fatalf("change = %#v, want a purge of %s", change, key)
		}
	}},

	{"a watch resumed from a revision replays only what followed", func(h *fleetHarness) {
		first := coord.DocumentKey("i", "first")
		if _, err := h.f.CreateDocument(h.ctx, coord.FamilyPages, first, []byte(`a`)); err != nil {
			h.t.Fatal(err)
		}
		mark, _, _ := h.f.Document(h.ctx, coord.FamilyPages, first)
		second := coord.DocumentKey("i", "second")
		if _, err := h.f.CreateDocument(h.ctx, coord.FamilyPages, second, []byte(`b`)); err != nil {
			h.t.Fatal(err)
		}

		w, err := h.f.WatchDocuments(h.ctx, coord.FamilyPages, mark.Version+1)
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
		if _, err := h.f.CreateDocument(h.ctx, coord.FamilyPages, key, []byte(`a`)); err != nil {
			h.t.Fatal(err)
		}
		record, _, _ := h.f.Document(h.ctx, coord.FamilyPages, key)
		got, ok, err := h.f.DocumentAt(h.ctx, coord.FamilyPages, key, record.Version)
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
		if _, ok, err := h.f.DocumentAt(h.ctx, coord.FamilyPages, key, record.Version+99); ok {
			h.t.Errorf("a revision nobody wrote was answered: %v", err)
		}
	}},

	{"families are separate stores", func(h *fleetHarness) {
		// ONE KEY, EVERY FAMILY. A projection reads one family at a time
		// and a cursor is a position in one bucket's sequence, so a
		// backend that shared a namespace would make one family's
		// revision advance because somebody wrote to another.
		//
		// DRIVEN OFF [coord.Families] rather than off two names, which
		// is what keeps this honest as the set changes: today there is
		// ONE family — the tracker and the embeddings became state-log
		// domains — so the pairwise half below compares nothing and the
		// case is deliberately vacuous rather than deleted. A second
		// family returning re-arms it with no edit, which is the only
		// shape under which "we checked that" stays true.
		key := coord.DocumentKey("x", "shared")
		for _, family := range coord.Families() {
			if _, err := h.f.CreateDocument(h.ctx, family, key,
				[]byte(family)); err != nil {
				h.t.Fatal(err)
			}
		}
		for _, family := range coord.Families() {
			got, held, err := h.f.Document(h.ctx, family, key)
			switch {
			case err != nil:
				h.t.Fatal(err)
			case !held:
				h.t.Fatalf("%s lost the document written under it", family)
			case string(got.Value) != string(family):
				h.t.Errorf("reading %s at %q answered %q — the families share "+
					"a namespace, so one's revision advances when another is "+
					"written", family, key, got.Value)
			}
		}

		// AND A FAMILY THIS BUILD DOES NOT SERVE IS REFUSED rather than
		// answered from some other family's bucket. That half is not
		// vacuous at any family count, and it is the one that catches a
		// backend resolving an unknown name to a default.
		if _, _, err := h.f.Document(h.ctx, coord.Family("work"), key); err == nil {
			h.t.Error("a family this build does not serve was answered rather " +
				"than refused")
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
		_, exists, err := h.f.Document(h.ctx, coord.FamilyPages, coord.DocumentKey("i", "any"))
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

// A DOCUMENT TOO BIG FOR THE TRANSPORT IS ITS OWN FAILURE, not an outage.
//
// The distinction is the whole point. A broker that is down is worth
// retrying, and an oversized document never will be — so a caller that could
// not tell them apart would retry a page a hundred kilobytes too big for ever,
// which is the exact loop [queue.MaxPayloadBytes] was written down to end.
//
// Certified on BOTH backends because the twin has to refuse what the broker
// refuses: a document a test accepts must be one production accepts, and
// without this case the path that reports it would be exercised nowhere.
var oversizeCases = []fleetCase{
	{"a document over the transport ceiling is refused, not reported as unavailable", func(h *fleetHarness) {
		key := coord.DocumentKey("p", "huge")
		// Past the ceiling by a comfortable margin rather than by a byte:
		// the two backends bound slightly different things — the twin
		// counts the value it was handed, the client counts the whole
		// published message — and a case sitting exactly on the boundary
		// would be measuring which.
		huge := make([]byte, queue.MaxPayloadBytes*2)
		for i := range huge {
			huge[i] = 'x'
		}

		_, err := h.f.CreateDocument(h.ctx, coord.FamilyPages, key, huge)
		if err == nil {
			h.t.Fatal("an oversized create was accepted")
		}
		if !errors.Is(err, coord.ErrDocumentTooLarge) {
			h.t.Errorf("an oversized create failed with %v, which a caller "+
				"cannot tell from a broker being down", err)
		}
		// AND IT IS NOT UNAVAILABLE. A caller branching on that would
		// retry, for ever, a write that can never succeed.
		if errors.Is(err, coord.ErrUnavailable) {
			h.t.Error("an oversized create reported the store as unavailable")
		}

		// The same on the update path, which is the one a page SAVE takes
		// — a page grows past the ceiling by being edited, not by being
		// created.
		small := coord.DocumentKey("p", "small")
		if _, err := h.f.CreateDocument(h.ctx, coord.FamilyPages, small, []byte(`{}`)); err != nil {
			h.t.Fatalf("seed create: %v", err)
		}
		record, ok, err := h.f.Document(h.ctx, coord.FamilyPages, small)
		if err != nil || !ok {
			h.t.Fatalf("read back = %v, %v", ok, err)
		}
		_, err = h.f.UpdateDocument(h.ctx, coord.FamilyPages, small, huge, record.Version)
		if !errors.Is(err, coord.ErrDocumentTooLarge) {
			h.t.Errorf("an oversized update failed with %v", err)
		}
	}},
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

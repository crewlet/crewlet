package engine

import (
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/collect"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// A NEWER BUILD'S LOG IS READ OFF THE NODES THE FLEET COUNTS, and only there:
// a counted node running a log this build does not register holds collection
// back, a node the fleet has stopped counting (evicted past the fence window)
// does not, and a log this build knows is never one it cannot see.
//
// Mutation: read every row whatever counts it, or report a known log, and a
// case below names the wrong logs.
func TestOnlyACountedNodesUnknownLogIsUnseen(t *testing.T) {
	t.Parallel()
	rows := []coord.NodePositions{
		{NodeID: "data-a", Domains: map[string]coord.DomainPosition{"tracker": {}, "pages": {}}},
		{NodeID: "data-b", Domains: map[string]coord.DomainPosition{"tracker": {}, "a_newer_log": {}}},
		{NodeID: "data-c", Domains: map[string]coord.DomainPosition{"tracker": {}, "a_newer_log": {}, "an_evicted_log": {}}},
		{NodeID: "data-d", Domains: map[string]coord.DomainPosition{"tracker": {}, "an_evicted_log": {}}},
	}
	counted := map[string]bool{"data-a": true, "data-b": true, "data-c": true}
	got := foreignLogs(rows, counted, []string{"tracker", "pages"})
	if len(got) != 2 {
		t.Fatalf("foreignLogs = %q, want one sentence for each log a counted node runs "+
			"and this build does not", got)
	}
	if !strings.Contains(got[0], "a_newer_log") || !strings.Contains(got[0], "data-b and data-c") {
		t.Errorf("the newer log is reported as %q, want it named with both counted nodes", got[0])
	}
	if !strings.Contains(got[1], "an_evicted_log") || strings.Contains(got[1], "data-d") {
		t.Errorf("the second log is reported as %q: data-c runs it and is counted, "+
			"data-d runs it and is not", got[1])
	}
	for _, sentence := range got {
		if strings.Contains(sentence, "tracker") || strings.Contains(sentence, "pages") {
			t.Errorf("%q names a log this build knows", sentence)
		}
	}
}

// unseenNode is an engine with just enough to answer what it cannot see: an
// in-memory fleet holding rows, the in-memory lease backend, and store when
// given.
func unseenNode(t *testing.T, db *store.DB, rows ...coord.NodePositions) *Engine {
	t.Helper()
	fleet := coordmemory.NewFleet()
	for _, row := range rows {
		if err := fleet.PutPositions(t.Context(), row); err != nil {
			t.Fatal(err)
		}
	}
	return &Engine{id: "data-a",
		backends: &Backends{Fleet: fleet, Coord: coordmemory.New(), Store: db}}
}

// THE ENGINE'S COLLECTOR KEEPS A NEWER BUILD'S OBJECTS. The collector the duty
// runs is built by [Engine.newCollector], and it asks [Engine.unseen] before
// it judges: with a data node the fleet counts running a log this build does
// not register, an object past the grace that no table this build declares
// names is kept, where the same store with no such node collects it.
//
// Mutation: build the collector without Unseen, and the first case deletes
// the object a newer build's row may name.
func TestTheEnginesCollectorKeepsWhatANewerBuildMayName(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		rows []coord.NodePositions
		kept bool
	}{
		"a counted node runs a newer log": {
			rows: []coord.NodePositions{{NodeID: "data-b",
				Domains: map[string]coord.DomainPosition{"tracker": {}, "a_newer_log": {}}}},
			kept: true,
		},
		"every node runs this build's logs, the control": {
			rows: []coord.NodePositions{{NodeID: "data-b",
				Domains: map[string]coord.DomainPosition{"tracker": {}}}},
			kept: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			then := time.Now().Add(-2 * collect.PendingGrace)
			var aged atomic.Bool
			aged.Store(true)
			backend := memobj.NewAt(func() time.Time {
				if aged.Load() {
					return then
				}
				return time.Now()
			})
			key := objstore.KeyAt(then)
			if err := backend.Put(t.Context(), key.Name(),
				strings.NewReader("an object a newer build named"), objstore.PutMeta{}); err != nil {
				t.Fatal(err)
			}
			aged.Store(false)
			objects, err := objstore.NewStore(backend)
			if err != nil {
				t.Fatal(err)
			}

			e := unseenNode(t, nil, tc.rows...)
			c, err := e.newCollector(objects, collect.References{&namedNothing{}})
			if err != nil {
				t.Fatal(err)
			}
			r, err := c.Collect(t.Context())
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			_, statErr := objects.Stat(t.Context(), key)
			if kept := statErr == nil; kept != tc.kept {
				t.Fatalf("object kept = %v, want %v (report %+v)", kept, tc.kept, r)
			}
			if tc.kept && !strings.Contains(r.Skipped, "a_newer_log") {
				t.Errorf("the pass says %q, want it to name the log it cannot see", r.Skipped)
			}
		})
	}
}

// A ROLLED-BACK NODE'S OWN COPY IS A WITNESS THE REGISTER CANNOT BE: its row
// names the logs it runs now, so on a fleet of one data node the tables a
// newer build migrated its copy to are the only sign that a newer build's
// rows may name objects.
//
// Mutation: read the register alone, and the answer is "".
func TestARolledBackCopyOfTheEstateIsUnseen(t *testing.T) {
	t.Parallel()
	db, estate := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "rolled-back.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = db.Close() })
	e := unseenNode(t, db, coord.NodePositions{NodeID: "data-a",
		Domains: map[string]coord.DomainPosition{"tracker": {}}})
	if why, err := e.unseen(t.Context()); err != nil || why != "" {
		t.Fatalf("a copy this build migrated is unseen: %q, %v", why, err)
	}

	const newer = "9999_a_newer_build.sql"
	replicated := storetest.ReplicatedDB(t, estate)
	if _, err := replicated.SQL().ExecContext(t.Context(),
		`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
		newer, store.EncodeTime(time.Now())); err != nil {
		t.Fatalf("record a newer build's migration: %v", err)
	}
	why, err := e.unseen(t.Context())
	if err != nil {
		t.Fatalf("unseen: %v", err)
	}
	if !strings.Contains(why, newer) {
		t.Fatalf("unseen = %q, want it to name %s, the newer build's migration", why, newer)
	}
}

// AN ANSWER THAT CANNOT BE HAD IS AN ERROR, never "nothing unseen": a node with
// no coordination store cannot read the register, and a collector reading ""
// would go ahead.
func TestANodeThatCannotReadTheFleetCannotSayNothingIsUnseen(t *testing.T) {
	t.Parallel()
	e := &Engine{id: "data-a", backends: &Backends{}}
	if why, err := e.unseen(t.Context()); err == nil {
		t.Fatalf("unseen answered %q with no fleet to read", why)
	}
}

package upkeep

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// namedEstate is an estate the source builder only ever asks its name.
type namedEstate string

func (e namedEstate) Name() string { return string(e) }

func (namedEstate) Barrier(context.Context) (statelog.Position, error) {
	return statelog.Position{}, nil
}

func (namedEstate) Read(context.Context, statelog.Position, func(*sql.Tx) error) (bool, error) {
	return true, nil
}

// THE SOURCES ARE THE DECLARATIONS OR NOTHING: every way the declared tables
// and the estates given can disagree is refused, because each one either
// reads a table as naming nothing — which deletes its files — or reads
// something nobody declared.
func TestSourcesRefuseEveryDisagreement(t *testing.T) {
	t.Parallel()
	files := objstore.ReferenceTable{Domain: "tracker", Table: "tracker_file_chunks", Column: "chunk", Slot: "slot"}
	chats := objstore.ReferenceTable{Domain: "chat", Table: "chat_attachments", Column: "chunk", Slot: "slot"}
	for _, tc := range []struct {
		name    string
		tables  []objstore.ReferenceTable
		estates []Estate
		refused string
	}{
		{"nothing declared", nil, []Estate{namedEstate("tracker")}, "no table"},
		{"a table whose domain has no estate", []objstore.ReferenceTable{files, chats},
			[]Estate{namedEstate("tracker")}, "chat_attachments"},
		{"an estate no table is in", []objstore.ReferenceTable{files},
			[]Estate{namedEstate("tracker"), namedEstate("pages")}, "pages"},
		{"an estate given twice", []objstore.ReferenceTable{files},
			[]Estate{namedEstate("tracker"), namedEstate("tracker")}, "twice"},
		{"a nil estate", []objstore.ReferenceTable{files}, []Estate{nil}, "nil"},
		{"a declaration that is not an identifier",
			[]objstore.ReferenceTable{{Domain: "tracker", Table: "t; DROP TABLE x", Column: "chunk", Slot: "slot"}},
			[]Estate{namedEstate("tracker")}, "identifier"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Sources(tc.tables, tc.estates...)
			if err == nil {
				t.Fatal("built")
			}
			if !strings.Contains(err.Error(), tc.refused) {
				t.Errorf("refused as %q, want it to name %q", err, tc.refused)
			}
		})
	}
}

// One source per estate, holding every table of its domain, so a domain's
// tables are read at one position and one completeness.
func TestSourcesGroupTablesByDomain(t *testing.T) {
	t.Parallel()
	a := objstore.ReferenceTable{Domain: "tracker", Table: "tracker_file_chunks", Column: "chunk", Slot: "slot"}
	b := objstore.ReferenceTable{Domain: "tracker", Table: "tracker_other_chunks", Column: "chunk", Slot: "slot"}
	c := objstore.ReferenceTable{Domain: "chat", Table: "chat_attachments", Column: "chunk", Slot: "slot"}
	got, err := Sources([]objstore.ReferenceTable{a, c, b}, namedEstate("tracker"), namedEstate("chat"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("%d sources, want one per estate", len(got))
	}
	tracker := got[0].(*tableSource)
	if tracker.Name() != "tracker" || len(tracker.tables) != 2 {
		t.Errorf("the tracker source holds %v", tracker.tables)
	}
}

// sqlEstate is a domain whose rows are a real database's.
type sqlEstate struct{ db *store.DB }

func (sqlEstate) Name() string { return "tracker" }

func (sqlEstate) Barrier(context.Context) (statelog.Position, error) {
	return statelog.Position{}, nil
}

func (e sqlEstate) Read(ctx context.Context, _ statelog.Position, fn func(*sql.Tx) error) (bool, error) {
	return true, e.db.Tx(ctx, fn)
}

// A READ OF A RUN OF SLOTS ANSWERS EXACTLY THE CHUNKS WHOSE SLOTS ARE IN IT —
// the first slot included, the one past the last not — through the statement
// the declaration builds, against a real database. A slot read one too wide
// or one too narrow at either end is a chunk a pass over a neighbouring group
// counts twice or not at all: repaired to a node it is not placed on, or
// judged unreferenced and deleted.
func TestAReadOfASlotRunAnswersExactlyThatRun(t *testing.T) {
	t.Parallel()
	db, err := store.OpenNode(t.Context(), filepath.Join(t.TempDir(), "refs.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.SQL().ExecContext(t.Context(),
		`CREATE TABLE scratch_chunks (chunk TEXT NOT NULL, slot INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	chunkAt := func(slot int) objstore.Hash {
		return objstore.Hash(fmt.Sprintf("%04x", slot) + strings.Repeat("a", 60))
	}
	const lo, hi = 0x1200, 0x1300
	for _, slot := range []int{lo - 1, lo, lo + 7, hi - 1, hi} {
		if _, err := db.SQL().ExecContext(t.Context(),
			`INSERT INTO scratch_chunks (chunk, slot) VALUES (?, ?)`, string(chunkAt(slot)), slot); err != nil {
			t.Fatal(err)
		}
	}
	sources, err := Sources([]objstore.ReferenceTable{{Domain: "tracker", Table: "scratch_chunks",
		Column: "chunk", Slot: "slot"}}, sqlEstate{db: db})
	if err != nil {
		t.Fatal(err)
	}
	got, complete, err := sources[0].Referenced(t.Context(), lo, hi, statelog.Position{})
	if err != nil || !complete {
		t.Fatalf("Referenced = %v, %v", complete, err)
	}
	want := []objstore.Hash{chunkAt(lo), chunkAt(lo + 7), chunkAt(hi - 1)}
	if keys := slices.Sorted(maps.Keys(got)); !slices.Equal(keys, want) {
		t.Fatalf("slots [%#x, %#x) answered %v, want %v", lo, hi, keys, want)
	}

	// A ROW THAT IS NOT A CONTENT ADDRESS fails the read by name rather
	// than being skipped — skipped, it would read as a chunk nothing names.
	if _, err := db.SQL().ExecContext(t.Context(),
		`INSERT INTO scratch_chunks (chunk, slot) VALUES ('not-a-hash', ?)`, lo+1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := sources[0].Referenced(t.Context(), lo, hi, statelog.Position{}); err == nil ||
		!strings.Contains(err.Error(), "scratch_chunks") {
		t.Fatalf("a malformed row = %v, want an error naming the table", err)
	}
}

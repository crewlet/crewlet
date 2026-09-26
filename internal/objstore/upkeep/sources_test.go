package upkeep

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
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
	files := objstore.ReferenceTable{Domain: "tracker", Table: "tracker_file_chunks", Column: "chunk", Group: "pg"}
	chats := objstore.ReferenceTable{Domain: "chat", Table: "chat_attachments", Column: "chunk", Group: "pg"}
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
			[]objstore.ReferenceTable{{Domain: "tracker", Table: "t; DROP TABLE x", Column: "chunk", Group: "pg"}},
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
	a := objstore.ReferenceTable{Domain: "tracker", Table: "tracker_file_chunks", Column: "chunk", Group: "pg"}
	b := objstore.ReferenceTable{Domain: "tracker", Table: "tracker_other_chunks", Column: "chunk", Group: "pg"}
	c := objstore.ReferenceTable{Domain: "chat", Table: "chat_attachments", Column: "chunk", Group: "pg"}
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

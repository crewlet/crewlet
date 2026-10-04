package store_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
	"github.com/crewlet/crewlet/internal/usage"
)

// EVERY DOMAIN'S TABLES ARE IN THE REPLICATED SCHEMA, AND NOTHING ELSE IS.
//
// The replicated estate's file is built from one migration sequence —
// `replicated/` — and every registered domain's log is applied into it. A
// domain that declares a table that sequence does not create has an applier
// that fails on its first record, on every data node at once — and a table the
// sequence creates that no domain declares carries rows no snapshot claims, no
// identity covers and no applier writes, which is the phantom the estate's
// derived-table gates already refuse one level down. So it is EQUALITY: the
// sequence creates the framework's own `statelog_` tables and exactly the
// tables the registered domains declare, nothing else.
//
// The domains are the ENGINE's layout 0, never a copy: a domain registered
// there is the case this exists for.
func TestEveryDomainTableIsInTheReplicatedSchema(t *testing.T) {
	t.Parallel()

	schema := tablesIn(t, store.EstatePartition)
	framework := map[string]bool{}
	for name := range schema {
		if strings.HasPrefix(name, frameworkTablePrefix) {
			framework[name] = true
		}
	}
	if len(framework) == 0 {
		t.Fatalf("no %s table in the replicated schema, so the framework's "+
			"own checkpoint has nowhere to commit — or this derivation is "+
			"reading the wrong schema", frameworkTablePrefix)
	}

	// EVERY DOMAIN THE ENGINE CAN NAME, by name. Registered in the engine
	// and named here, because a domain's tables are the domain's to declare
	// and the engine exports the names only; a name with no entry is a
	// domain this test was never shown, and it fails rather than passes.
	known := map[string]statelog.Domain{}
	for _, d := range []statelog.Domain{tracker.Domain{}, pages.Domain{}, search.Domain{}, usage.Domain{}} {
		known[d.Name()] = d
	}

	declared := map[string]bool{}
	for _, space := range engine.LayoutZero().Spaces {
		for _, name := range space.Domains {
			d, ok := known[name]
			if !ok {
				t.Errorf("the engine registers the domain %q, which this test "+
					"has no Domain for — add it to known, so its tables are "+
					"held against the schema its log is applied into", name)
				continue
			}
			for table := range d.Tables() {
				declared[table] = true
				if strings.HasPrefix(table, frameworkTablePrefix) {
					t.Errorf("the domain %s declares %s, a table in the "+
						"framework's own namespace — a domain's rows and the "+
						"framework's checkpoint would be one claim",
						name, table)
				}
				if !schema[table] {
					t.Errorf("the domain %s declares the table %s, and the "+
						"replicated schema does not create it: the domain's "+
						"applier fails on its first record on every data node",
						name, table)
				}
			}
		}
	}
	var unclaimed []string
	for table := range schema {
		if !framework[table] && !declared[table] {
			unclaimed = append(unclaimed, table)
		}
	}
	slices.Sort(unclaimed)
	if len(unclaimed) > 0 {
		t.Errorf("the replicated schema creates %v, which no registered "+
			"domain declares and which is not the framework's: rows there "+
			"are in no snapshot claim and no applier's scope. Declare each in "+
			"its domain's Tables(), or drop it with a new migration", unclaimed)
	}
	t.Logf("replicated schema: %d table(s), %d of them the framework's",
		len(schema), len(framework))
}

// frameworkTablePrefix is the namespace of the state-log framework's own
// tables — the cursor, the arbitration anchor and the record of operation
// ledgers lost — which the replicated file carries beside every domain's, and
// which no domain declares.
const frameworkTablePrefix = "statelog_"

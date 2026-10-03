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

// EVERY DOMAIN'S TABLES ARE IN THE SCHEMA OF EVERY SPACE THAT LISTS IT.
//
// A partition's file is built from its space's schema, and a domain's log in
// that partition is applied into it. A domain that declares a table the
// file's schema does not create has an applier that fails on its first
// record, on every node holding the partition at once — and one whose schema
// creates a table no domain declares carries rows no snapshot claims, no
// identity covers and no applier writes, which is the phantom the estate's
// derived-table gates already refuse one level down.
//
// # What a space's schema is, today
//
// Until the activation commit, every partition file of every layout carries
// ONE sequence — `replicated/`, the sequence layout 0's one file has always
// run (§C2 of the partitioning contract, and [store.EstatePartition]). So the
// question per space is containment: every domain the space lists declares
// only tables that sequence creates. For layout 0's one space, which lists
// every registered domain, it is EQUALITY: the sequence creates the
// framework's own `statelog_` tables and exactly the tables the registered
// domains declare, nothing else. When the activation composes a schema per
// space — the framework, then each of its domains in order — this reads each
// space's own and the equality holds for every one.
//
// The layouts are the ENGINE's, never a copy: a space whose domain list
// changes there is the case this exists for.
func TestEveryDomainTableIsInItsSpaceSchema(t *testing.T) {
	t.Parallel()

	schema := tablesIn(t, store.EstatePartition)
	framework := map[string]bool{}
	for name := range schema {
		if strings.HasPrefix(name, frameworkTablePrefix) {
			framework[name] = true
		}
	}
	if len(framework) == 0 {
		t.Fatalf("no %s table in the partition schema, so the framework's "+
			"own checkpoint has nowhere to commit — or this derivation is "+
			"reading the wrong schema", frameworkTablePrefix)
	}

	// EVERY DOMAIN A LAYOUT CAN NAME, by name. Registered in the engine and
	// named here, because a domain's tables are the domain's to declare and
	// the engine exports the names only; a name with no entry is a domain
	// this test was never shown, and it fails rather than passes.
	known := map[string]statelog.Domain{}
	for _, d := range []statelog.Domain{tracker.Domain{}, pages.Domain{}, search.Domain{}, usage.Domain{}} {
		known[d.Name()] = d
	}

	for _, layout := range []statelog.Layout{engine.LayoutZero(), engine.DefaultLayoutOne()} {
		for _, space := range layout.Spaces {
			declared := map[string]bool{}
			for _, name := range space.Domains {
				d, ok := known[name]
				if !ok {
					t.Errorf("layout %d's space %s lists the domain %q, which "+
						"this test has no Domain for — add it to known, so its "+
						"tables are held against the schema its partitions "+
						"are built from", layout.Number, space.Space, name)
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
						t.Errorf("layout %d's space %s lists the domain %s, "+
							"which declares the table %s, and the schema that "+
							"space's partitions are built from does not create "+
							"it: the domain's applier fails on its first record "+
							"in every one of them", layout.Number, space.Space,
							name, table)
					}
				}
			}
			if layout.Number != 0 {
				continue
			}
			// LAYOUT 0 CARRIES EVERY REGISTERED DOMAIN, so its one
			// schema is exactly theirs plus the framework's.
			var unclaimed []string
			for table := range schema {
				if !framework[table] && !declared[table] {
					unclaimed = append(unclaimed, table)
				}
			}
			slices.Sort(unclaimed)
			if len(unclaimed) > 0 {
				t.Errorf("the partition schema creates %v, which no domain "+
					"layout 0 carries declares and which is not the "+
					"framework's: rows there are in no snapshot claim and no "+
					"applier's scope. Declare each in its domain's Tables(), "+
					"or drop it with a new migration", unclaimed)
			}
		}
	}
	t.Logf("partition schema: %d table(s), %d of them the framework's",
		len(schema), len(framework))
}

// frameworkTablePrefix is the namespace of the state-log framework's own
// tables — the cursor, the arbitration anchor and the record of operation
// ledgers lost — which every partition carries whatever its space, and which
// no domain declares.
const frameworkTablePrefix = "statelog_"

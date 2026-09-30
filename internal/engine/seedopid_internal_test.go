package engine

import (
	"database/sql"
	"testing"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY OPERATION A BOOT PUBLISHES CARRIES THE INSTANT IT WAS MINTED AT.
//
// The ledger's vouching reads that instant off the id itself
// ([statelog.OpMintedAt]), and an id spelled in any other shape is read as
// minted at the zero instant: on a node whose ledger has ever lost a row — to
// its retention sweep, or to a snapshot from a donor that scrubbed its own —
// such a write is answered `unknown` without being published, every time. The
// seed's ids were `seed:<revision>:<kind>:<key>`, so a company file's chart
// could never be seeded on such a node, and nothing said why.
//
// Asked of every ledger the boot wrote to rather than of the seed alone, so a
// later boot step that mints its own spelling fails here too.
func TestEveryOperationTheBootPublishedCarriesItsInstant(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	s := e.native.Load().log
	seen := 0
	for _, name := range s.order {
		running := s.domains[name]
		table := running.domain.OpsTable()
		if table == "" {
			continue
		}
		waitApplied(t, running)
		var ids []string
		if err := e.backends.Store.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
			rows, err := tx.QueryContext(t.Context(), `SELECT op_id FROM `+table)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					return err
				}
				ids = append(ids, id)
			}
			return rows.Err()
		}); err != nil {
			t.Fatalf("read %s: %v", table, err)
		}
		for _, id := range ids {
			seen++
			if _, ok := statelog.OpMintedAt(id); !ok {
				t.Errorf("%s holds operation %q, which carries no instant — the "+
					"ledger reads it as minted before every loss it has had", table, id)
			}
		}
	}
	if seen == 0 {
		t.Fatal("the boot published nothing to any ledger, so this case proves " +
			"nothing — the company it boots has a seat, and seeding it is a write")
	}
}

// THE SEED'S OPERATION IS THE SAME ON EVERY NODE AND IN EVERY RETRY, and its
// instant is the chart log's creation — read off the broker, not a clock.
func TestTheSeedsOperationIsDerivedFromTheChartLogAndTheFile(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	running := e.native.Load().log.Domain(chart.Domain{}.Name())
	if running == nil {
		t.Fatal("this node runs no chart log")
	}
	id := e.seedGesture("revision-a")
	at, ok := statelog.OpMintedAt(id)
	if !ok {
		t.Fatalf("the seed's operation %q carries no instant", id)
	}
	if created := running.runner.StreamCreatedAt(); created.IsZero() ||
		at.UnixMilli() != created.UnixMilli() {
		t.Errorf("the seed's operation was minted at %s, want the chart log's "+
			"creation %s — the one instant every node reads identically", at, created)
	}
	if again := e.seedGesture("revision-a"); again != id {
		t.Errorf("one file's seed derived %q and then %q — a second node seeding "+
			"it would write every seat again", id, again)
	}
	if other := e.seedGesture("revision-b"); other == id {
		t.Errorf("two different charts derived one seed operation %q", id)
	}
}

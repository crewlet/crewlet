package store

import (
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/tokens"
)

// THE BACKFILL READS A SPEND RECORD'S PAYLOAD AS THE WRITER DOES.
//
// schema/0032 fills the price, the split and the unreported mark from each
// row's own payload, so the history a node already holds reads as the rows
// written after it. Held by writing each payload twice — as a row a build that
// predates the columns left, which the migration then backfills, and through
// Append beside it — and reading both back through the spend read: every
// backfilled record answers what its twin does, and a row the columns do not
// describe keeps their defaults.
func TestTheBackfillReadsASpendRecordsPayloadAsTheWriterDoes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	file := filepath.Join(t.TempDir(), "node.db")
	cases := []struct {
		name, eventType, payload string
		// cost, split and unreported are what both rows must answer, so a
		// fault shared by the backfill and the writer still fails.
		cost       float64
		split      []tokens.ModelSpend
		unreported bool
	}{
		{"priced, split and a floor", "agent_phase_completed",
			`{"cost_usd":0.25,"models":[{"model":"sonnet","input_tokens":100,"output_tokens":10},` +
				`{"model":"haiku","input_tokens":5,"output_tokens":1,"cost_usd":0.25}],"run_spend_unreported":true}`,
			0.25, []tokens.ModelSpend{
				{Model: "sonnet", InputTokens: 100, OutputTokens: 10},
				{Model: "haiku", InputTokens: 5, OutputTokens: 1, CostUSD: 0.25},
			}, true},
		{"a whole-dollar price", "agent_phase_completed", `{"cost_usd":3}`, 3, nil, false},
		// One of the three alone, each: the backfill rewrites only a row
		// that carries one, so each is on its own what makes a row carry.
		{"a split alone", "agent_phase_completed",
			`{"cost_usd":0,"models":[{"model":"opus","input_tokens":4}]}`,
			0, []tokens.ModelSpend{{Model: "opus", InputTokens: 4}}, false},
		{"the mark alone", "agent_phase_completed", `{"cost_usd":0,"run_spend_unreported":true}`, 0, nil, true},
		{"an empty split and no price", "agent_phase_completed", `{"cost_usd":0,"models":[]}`, 0, nil, false},
		{"members of the wrong type", "agent_phase_completed",
			`{"cost_usd":"0.5","models":{"model":"x"},"run_spend_unreported":1}`, 0, nil, false},
		{"none of the three", "agent_phase_completed", `{"model":"sonnet"}`, 0, nil, false},
		{"an auxiliary call", "auxiliary_call_completed",
			`{"cost_usd":0.1,"models":[{"model":"mini","input_tokens":2}]}`,
			0.1, []tokens.ModelSpend{{Model: "mini", InputTokens: 2}}, false},
		{"a payload that is not JSON", "agent_phase_completed", `{"cost_usd":0.5,`, 0, nil, false},
	}

	// A NODE ESTATE AS A BUILD THAT PREDATES THE COLUMNS LEAVES IT: every
	// migration before 0032 applied, and rows written by an insert that
	// names none of the three.
	pool, err := openPrepared(ctx, file, Options{}.forEstate(EstateNode))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	before := &DB{sql: pool, path: file, estate: EstateNode}
	if _, err := pool.ExecContext(ctx, `CREATE TABLE schema_migrations (
		version TEXT NOT NULL PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatalf("schema_migrations: %v", err)
	}
	names, err := schemaVersions(EstateNode)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if strings.HasPrefix(name, "0032_") {
			break
		}
		body, err := schemaFS.ReadFile(path.Join("schema", string(EstateNode), name))
		if err != nil {
			t.Fatal(err)
		}
		if err := before.applyOne(ctx, name, string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	at := time.Now().UTC().Add(-time.Hour)
	legacy := func(id, eventType, payload string, n int) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, `INSERT INTO crewlet_events
			(event_time, event_id, event_type, category, payload) VALUES (?, ?, ?, 'lifecycle', ?)`,
			EncodeTime(at.Add(time.Duration(n)*time.Second)), id, eventType, payload); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	for i, c := range cases {
		legacy(fmt.Sprintf("legacy-%d", i), c.eventType, c.payload, i)
	}
	// A row of another type carrying all three members: nothing reads them
	// off it, so the backfill leaves it as the writer does.
	legacy("legacy-other", "turn_completed",
		`{"cost_usd":5,"models":[{"model":"ghost"}],"run_spend_unreported":true}`, len(cases))
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}

	// OPEN APPLIES 0032, and the twins go in through the writer.
	db, err := Open(ctx, file, Options{})
	if err != nil {
		t.Fatalf("open with the migration: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	log := db.Events()
	for i, c := range cases {
		if err := log.Append(ctx, EventRecord{
			ID: fmt.Sprintf("twin-%d", i), Type: c.eventType, Category: "lifecycle",
			Time: at.Add(time.Duration(i)*time.Second + time.Millisecond), Payload: []byte(c.payload),
		}); err != nil {
			t.Fatalf("append the twin of %q: %v", c.name, err)
		}
	}

	records, err := log.PhaseTokens(ctx, PhaseTokenQuery{SinceDays: 1})
	if err != nil {
		t.Fatalf("phase tokens: %v", err)
	}
	byID := map[string]tokens.Record{}
	for _, r := range records {
		byID[r.EventID] = r
	}
	for i, c := range cases {
		for _, id := range []string{fmt.Sprintf("legacy-%d", i), fmt.Sprintf("twin-%d", i)} {
			r, ok := byID[id]
			if !ok {
				t.Errorf("%s: %s is not among the spend records", c.name, id)
				continue
			}
			if r.CostUSD != c.cost || r.Unreported != c.unreported || !slices.Equal(r.Models, c.split) {
				t.Errorf("%s: %s reads $%v, split %+v, unreported %v; want $%v, %+v, %v",
					c.name, id, r.CostUSD, r.Models, r.Unreported, c.cost, c.split, c.unreported)
			}
		}
	}

	var (
		cost       float64
		split      string
		unreported bool
	)
	if err := db.SQL().QueryRowContext(ctx, `SELECT cost_usd, models, run_spend_unreported
		FROM crewlet_events WHERE event_id = 'legacy-other'`).Scan(&cost, &split, &unreported); err != nil {
		t.Fatalf("read the other row: %v", err)
	}
	if cost != 0 || split != "[]" || unreported {
		t.Errorf("a turn_completed row was backfilled: $%v, %s, %v; want the defaults", cost, split, unreported)
	}
}

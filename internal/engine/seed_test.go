package engine_test

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE BOOT SEED, AND THE THREE THINGS IT MUST NOT DO.
//
// A company has to start somewhere, and what an operator has on a first run is
// a file. Without the seed a fresh deployment boots with an empty chart, which
// is a company with no seats — no mailbox attached, no placement claiming
// anything, and a dashboard rendering an organisation of nobody. The file
// validates, the node comes up, and nothing says why.
//
// What it must not do is act like an import: it is a SEED, so it fires when
// the chart has nothing in it and never again.

// A FILE'S UNITS AND SEATS BECOME ROWS, WITH THEIR CONTENT.
//
// BOTH HALVES, because a chart with the structure and no content is a company
// of empty seats: every handle in the right unit, with no model, no
// credentials and no name. The import record carries the structure — it is one
// graph and has to be arbitrated whole — and each object's own content is a
// record on its own subject.
func TestABootSeedTurnsAFileIntoChartRows(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{Company: parsedCompany(t, seedCompanyDoc)})

	rows := readChart(t, e)
	var units []string
	for _, u := range rows.Units {
		units = append(units, u.Key)
	}
	if !slices.Equal(units, []string{"eng"}) {
		t.Errorf("units = %v, want the file's one", units)
	}

	var handles []string
	for _, s := range rows.Seats {
		handles = append(handles, s.Handle)
	}
	if !slices.Equal(handles, []string{"ceo", "dev"}) {
		t.Fatalf("seats = %v, want the file's two", handles)
	}

	// THE STRUCTURE, from the import record.
	for _, seat := range rows.Seats {
		want := ""
		if seat.Handle == "dev" {
			want = "eng"
		}
		if seat.UnitKey != want {
			t.Errorf("seat %q sits in %q, want %q", seat.Handle, seat.UnitKey, want)
		}
	}

	// AND THE CONTENT, from each seat's own record. Read through the view
	// rather than off the row, because what a turn holds is the view and a
	// row nothing decodes onto a seat is a row nobody reads.
	company := e.Company()
	dev := company.Org.Role("dev")
	if dev == nil {
		t.Fatal("the seeded seat did not reach the company view")
	}
	if dev.Name != "Dev" {
		t.Errorf("name = %q, want the file's", dev.Name)
	}
	if got := dev.MCPEnv["tracker"]["SEAT_TOKEN"]; got != "dev-secret" {
		t.Errorf("the seat's credentials did not reach the view: %v", dev.MCPEnv)
	}
	if !slices.Contains(dev.LLM, "zulu") {
		t.Errorf("llm = %v, want the file's — a seat with no model chain "+
			"cannot take a turn", dev.LLM)
	}
	// AND THE UNIT'S OWN CONTENT, which its whole team inherits.
	unit := company.Org.Unit("eng")
	if unit == nil {
		t.Fatal("the seeded unit did not reach the company view")
	}
	if unit.Channel != "c-eng" {
		t.Errorf("channel = %q, want the file's", unit.Channel)
	}
}

// AND A CHART THAT IS NOT EMPTY IS NEVER SEEDED OVER.
//
// # The failure this prevents
//
// An operator who still passes `-company` restarts a node. If the seed ran
// again it would re-place every object the file names — so a seat moved to
// another team last week would silently move back, on a restart nobody
// connected to the chart at all. A chart edited after the seed belongs to
// whoever edited it; changing it from a file afterwards is
// `crewlet config import`, which diffs and asks.
func TestASecondBootOverAnEditedChartSeedsNothing(t *testing.T) {
	t.Parallel()
	// ONE ESTATE, TWO BOOTS: the store and the stream directory are what
	// carry the chart between them, so both engines are built from one
	// bootstrap rather than from newEngine's per-call temporaries.
	boot := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	})
	first, err := engine.New(t.Context(), engine.Options{
		Bootstrap: boot, Company: parsedCompany(t, seedCompanyDoc),
	})
	if err != nil {
		t.Fatalf("the first boot: %v", err)
	}
	readChart(t, first)

	// THE EDIT: the file's seat is moved out of the file's unit, which is
	// exactly what the seed would undo.
	if _, err := first.ChartWriter().WriteBatch(t.Context(), "move-dev", chart.Batch{
		Operations: []chart.Operation{{
			Kind:   chart.OpMove,
			Object: chart.ObjectRef{Kind: chart.KindSeat, ID: "dev"},
		}},
	}); err != nil {
		t.Fatalf("move the seat: %v", err)
	}
	first.Stop(context.Background())

	// A SECOND BOOT OVER THE SAME FILE, against the same estate.
	second, err := engine.New(t.Context(), engine.Options{
		Bootstrap: boot, Company: parsedCompany(t, seedCompanyDoc),
	})
	if err != nil {
		t.Fatalf("the second boot: %v", err)
	}
	t.Cleanup(func() { second.Stop(context.Background()) })

	rows := readChart(t, second)
	for _, seat := range rows.Seats {
		if seat.Handle == "dev" && seat.UnitKey != "" {
			t.Errorf("the second boot moved the seat back into %q — a "+
				"restart re-placed an object somebody had deliberately "+
				"moved", seat.UnitKey)
		}
	}
}

// A COMPANY WITH NO UNITS AND NO SEATS SEEDS NOTHING.
//
// An operator who has written their providers and not their people is a real
// authoring state. Importing nothing would write a ledger row saying an empty
// structure had landed, which the next, edited file would then have to be
// distinguished from.
func TestASeedOfAnEmptyChartWritesNothing(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{Company: parsedCompany(t, `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${K}"]
`)})
	rows := readChart(t, e)
	if len(rows.Units) != 0 || len(rows.Seats) != 0 {
		t.Errorf("a company that names nobody produced %d units and %d seats",
			len(rows.Units), len(rows.Seats))
	}
	if imports, _, err := e.Chart().Imports(t.Context(), 10,
		statelog.Freshness{Level: statelog.ReadStale}); err != nil {
		t.Fatalf("read the import ledger: %v", err)
	} else if len(imports) != 0 {
		t.Errorf("the ledger holds %d imports, want none — a row here makes "+
			"the next edited file indistinguishable from a re-seed", len(imports))
	}
}

// seedCompanyDoc is a company with one unit, one seat in it and one at the
// root, each carrying content the chart's rows do not have columns for.
const seedCompanyDoc = `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${K}"]
mcp_servers:
  - name: tracker
    shared: false
    command: tracker-mcp
roles:
  - name: CEO
    handle: ceo
    llm: zulu
units:
  - name: Engineering
    id: eng
    channel: c-eng
    roles:
      - name: Dev
        handle: dev
        llm: zulu
        mcp_env:
          tracker:
            SEAT_TOKEN: dev-secret
`

// readChart is this node's whole chart at its own applied position.
func readChart(t *testing.T, e *engine.Engine) chart.Chart {
	t.Helper()
	reader := e.Chart()
	if reader == nil {
		t.Fatal("this engine runs no chart")
	}
	// THE WRITE AND THE APPLY ARE NOT ONE STEP, so this waits rather than
	// reading once: a read that raced the seed would report an empty chart
	// and every case here would pass for a seed that never ran.
	deadline := time.Now().Add(10 * time.Second)
	for {
		rows, err := reader.Read(t.Context(),
			statelog.Freshness{Level: statelog.ReadStale})
		if err != nil {
			t.Fatalf("read the chart: %v", err)
		}
		if len(rows.Units) > 0 || len(rows.Seats) > 0 || time.Now().After(deadline) {
			return rows
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// THE COMPANY'S NAME REACHES THE VIEW, AND EVERY AGENT ID DERIVES FROM IT.
//
// # Why this has a case of its own
//
// The view is a function of BOTH halves: the rows give the seats and the
// settings give the company's name. A seat's agent id is a UUIDv5 over the
// company name and the handle — which is what lets every node compute the same
// id with no database and no running instance — so a view derived with the
// wrong name gives every seat an identity no peer would compute. Its mailbox,
// its budget rows and its memory would all be filed under it.
//
// The pairing is easy to get wrong in exactly one place and the failure is
// silent: at boot the first chart is read BEFORE the first settings epoch is
// installed, so a derivation that ran where the rows arrived would pair them
// with no settings at all and derive every id from an empty company name.
func TestTheViewDerivesEveryAgentIDFromTheCompanyName(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{Company: parsedCompany(t, seedCompanyDoc)})
	readChart(t, e)

	company := e.Company()
	if company.Org.Name != "Acme" {
		t.Fatalf("the view's company name is %q, want the settings' — every "+
			"seat's agent id derives from it", company.Org.Name)
	}
	seat := company.Org.Role("dev")
	if seat == nil {
		t.Fatal("the seeded seat did not reach the view")
	}
	got, ok := company.Org.AgentIDFor(seat)
	if !ok {
		t.Fatal("the seat has no agent id, so nothing can route to its inbox")
	}
	// THE ID EVERY OTHER NODE WOULD COMPUTE, derived here from the same
	// function rather than written down: what this asserts is that the
	// view's inputs were paired correctly, not what uuid5 produces.
	want, derived := org.DeriveAgentID("Acme", "dev")
	if !derived {
		t.Fatal("the derivation refused a company name and a handle")
	}
	if got != want {
		t.Errorf("the view derives %s for `dev` and every peer derives %s — "+
			"this node's mailboxes, budget rows and memory are filed under an "+
			"identity nothing else in the fleet computes", got, want)
	}
}

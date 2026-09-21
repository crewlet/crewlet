package engine_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/org"
)

// THE COMPANY VIEW: what a reader gets when the two halves move independently.
//
// A company is a SETTINGS epoch and a CHART this node has applied, composed
// into one value on the write side. Every case here is about that composition
// rather than about either half — what a reader holds while one of them moves,
// what a rebuild costs when nothing moved, and what happens when the estate
// cannot be read at all.

// A READER HOLDS ONE VALUE, AND NEITHER HALF MOVES UNDER IT.
//
// # Why this is the case the whole design exists for
//
// Everything a turn reads comes from one of these: the round caps, the model
// chain, the roster, the prompt. Two reads could straddle a publish, and a
// turn that built its runner from one epoch and took its round caps from the
// next would be running a company that never existed.
//
// Publishing instead of mutating is what makes that unrepresentable, and this
// is what says so: the value a reader took stays exactly as it was while both
// halves move underneath it. A composition that edited the published value in
// place — which is the obvious way to make a chart change cheap — would pass
// every other case here and fail this one.
func TestAValueAReaderHoldsNeverMovesUnderIt(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{Company: parsedCompany(t, seedCompanyDoc)})
	readChart(t, e)

	// WHAT A TURN WOULD PIN, taken once, exactly as runTurn takes it.
	held := e.Company()
	if held == nil || held.Org.Role("dev") == nil {
		t.Fatal("the engine published no company to pin")
	}
	name, seats := held.Config.Name, len(held.Org.Roles)
	dev := held.Org.Role("dev")
	devName := dev.Name

	// THE CHART MOVES FIRST, and it is asserted BEFORE the settings do.
	// Order is the whole of this half: a rebuild edits whatever is
	// published at the moment it runs, so a later apply that replaced the
	// published value would hide an in-place edit of the one this reader
	// took.
	if err := hire(t, e, "cfo"); err != nil {
		t.Fatal(err)
	}
	if len(held.Org.Roles) != seats {
		t.Errorf("a chart rebuild moved a held company: %d seats, want %d",
			len(held.Org.Roles), seats)
	}
	if held.Org.Role("cfo") != nil {
		t.Error("a seat hired after the pin appeared in the pinned company")
	}
	if dev.Name != devName {
		t.Errorf("a seat the reader was holding was rewritten in place: %q", dev.Name)
	}

	// AND THEN THE SETTINGS, through an apply.
	grown := parsedCompany(t, strings.Replace(seedCompanyDoc,
		"name: Acme\n", "name: Acme\nmission: ship it\n", 1))
	if _, _, err := e.Apply(t.Context(), grown); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if held.Config.Name != name || held.Config.Mission != "" {
		t.Errorf("an apply moved the settings of a held company: name=%q mission=%q",
			held.Config.Name, held.Config.Mission)
	}
	if len(held.Org.Roles) != seats || held.Org.Role("cfo") != nil {
		t.Errorf("the apply's own composition moved a held company: %d seats",
			len(held.Org.Roles))
	}

	// AND THE NEXT READER GETS BOTH CHANGES, or this proves only that
	// nothing happened at all.
	current := e.Company()
	if current == held {
		t.Fatal("the engine published no new company, so nothing above was tested")
	}
	if current.Config.Mission != "ship it" {
		t.Errorf("mission = %q, want the applied settings", current.Config.Mission)
	}
	if current.Org.Role("cfo") == nil {
		t.Error("the hire did not reach the current company")
	}
}

// A REBUILD AT AN EQUAL CURSOR DERIVES NOTHING.
//
// The triggers fire on every committed record, on a timer, and at two points
// during boot and rejoin, so most of them find the view already current.
// Re-deriving anyway would rebuild a twenty-thousand-seat tree to arrive at
// the same bytes — the comparison is what makes a per-record trigger
// affordable at all.
//
// Asserted on the published POINTER, which is the only observable a no-op has:
// a rebuild that ran would compose a new value and store it, so a reader would
// see a different pointer for a company nobody changed.
func TestARebuildAtAnEqualCursorPublishesNothingNew(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{Company: parsedCompany(t, seedCompanyDoc)})
	readChart(t, e)

	before := e.Company()
	for range 5 {
		at, err := engine.RefreshChartForTest(t.Context(), e)
		if err != nil {
			t.Fatalf("refresh: %v", err)
		}
		if at == (org.ViewPosition{}) {
			t.Fatal("the refresh reports no position, so the view was never built")
		}
	}
	if after := e.Company(); after != before {
		t.Errorf("a refresh at an unchanged cursor republished the company, so "+
			"every committed record costs a full derivation: %p -> %p",
			before, after)
	}
}

// A SETTINGS APPLY AND A CHART SWAP RACE CLEANLY.
//
// The two halves are written by different goroutines on a running node — the
// reconcile loop installs epochs, the chart applier's committed hook rebuilds
// the view — and readers load the composition on every turn. Under `-race`
// this is what says the composition has one writer and its readers take a
// published pointer rather than reaching into either half.
func TestASettingsApplyAndAChartSwapRaceCleanly(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{Company: parsedCompany(t, seedCompanyDoc)})
	readChart(t, e)

	// TWO GROUPS, because the readers only stop when the writers are done:
	// one wait that covered both would wait for readers that are waiting
	// for it to finish.
	var writers, readers sync.WaitGroup
	stop := make(chan struct{})
	hired := make(chan error, 1)

	writers.Add(1)
	go func() {
		defer writers.Done()
		for i := range 3 {
			mission := strings.Repeat("a", i+1)
			grown := parsedCompany(t, strings.Replace(seedCompanyDoc,
				"name: Acme\n", "name: Acme\nmission: "+mission+"\n", 1))
			if _, _, err := e.Apply(t.Context(), grown); err != nil {
				return
			}
		}
	}()

	writers.Add(1)
	go func() {
		defer writers.Done()
		for i := range 2 {
			if err := hire(t, e, "hire-"+string(rune('a'+i))); err != nil {
				hired <- err
				return
			}
		}
	}()

	// AND READERS THROUGHOUT, because the hazard is a read that straddles
	// a publish rather than the two writes meeting each other.
	readers.Add(2)
	for range 2 {
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if c := e.Company(); c != nil && c.Org != nil {
					_ = len(c.Org.Roles)
					_ = c.Config.Name
				}
				// A BREATH between loads. A reader spinning as
				// fast as it can starves the writers under the
				// detector, and what this case is about is the
				// two of them overlapping rather than one of
				// them monopolising a core.
				time.Sleep(time.Millisecond)
			}
		}()
	}

	done := make(chan struct{})
	go func() { writers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Minute):
		t.Error("the writers did not finish")
	}
	close(stop)
	readers.Wait()

	select {
	case err := <-hired:
		t.Errorf("a hire during the race failed: %v", err)
	default:
	}

	company := e.Company()
	if company == nil || company.Org == nil {
		t.Fatal("the engine published no company after the race")
	}
}

// hire writes one seat onto the chart and waits for the view to carry it.
//
// THROUGH THE CHART'S OWN WRITER, which is the point: a hire is not a config
// apply any more, and a test that added a seat by applying a company would be
// exercising the settings path under the name of the chart one.
//
// IT REPORTS rather than failing, because the race case calls it from a
// goroutine of its own: `t.Fatal` off the test's goroutine stops that one and
// lets the run continue, which is a failure reported as a hang.
func hire(t *testing.T, e *engine.Engine, handle string) error {
	t.Helper()
	writer := e.ChartWriter()
	if writer == nil {
		return fmt.Errorf("this engine runs no chart writer")
	}
	// TWO RECORDS, in the order the seed uses: the STRUCTURE on the one
	// subject the whole tree arbitrates on, then the seat's own CONTENT on
	// its own. A batch alone is an empty seat — the right handle in the
	// right place, with no name and no model.
	if _, err := writer.WriteBatch(t.Context(), "test:hire:"+handle, chart.Batch{
		Operations: []chart.Operation{{
			Kind:   chart.OpCreateSeat,
			Object: chart.ObjectRef{Kind: chart.KindSeat, ID: handle},
		}},
	}); err != nil {
		return fmt.Errorf("hire %s: %w", handle, err)
	}
	if _, err := writer.WriteSeat(t.Context(), "test:content:"+handle, chart.SeatContent{
		Handle: handle, Kind: chart.SeatAgent, Name: handle,
	}); err != nil {
		return fmt.Errorf("give %s its content: %w", handle, err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := engine.RefreshChartForTest(t.Context(), e); err != nil {
			return fmt.Errorf("refresh after hiring %s: %w", handle, err)
		}
		if c := e.Company(); c != nil && c.Org.Role(handle) != nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the hire of %s never reached the view", handle)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

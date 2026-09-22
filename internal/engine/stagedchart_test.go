package engine_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// TestAStagedChartIsPublishedOnceAndThenGone.
//
// An offline `crewlet config import` cannot publish a chart — that is a
// record on an ordered log and the command opens no broker — so it stages
// one. This is where the stage is redeemed, and the two things that must be
// true of it are that the structure lands and that the stage does not come
// back: a second publish writes another record on the subject every
// structural write in the company serialises behind.
func TestAStagedChartIsPublishedOnceAndThenGone(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})
	staged := e.Backends().Store.StagedCharts()

	// A CHART THIS COMPANY DOES NOT HOLD, so the case is about the
	// publish rather than about a no-op.
	authored := chart.Authored{
		Units: []chart.AuthoredUnit{{Key: "staged-team", Name: "Staged Team"}},
		Seats: []chart.AuthoredSeat{{
			Handle: "staged-seat", Unit: "staged-team",
			Kind: chart.SeatAgent, Name: "Staged Seat",
		}},
	}
	body, err := json.Marshal(authored)
	if err != nil {
		t.Fatal(err)
	}
	// SEALED WITH NOTHING, which is a real configuration: company_config
	// supports a plaintext mode, and Open answers the bytes back.
	payload, err := secrets.Seal(nil, body)
	if err != nil {
		t.Fatal(err)
	}
	key := chart.ImportKey(authored)
	if err := staged.Stage(t.Context(), store.StagedChart{
		ID: key, Payload: payload, SourcePath: "company.yaml", StagedBy: "ops",
	}); err != nil {
		t.Fatalf("Stage: %v", err)
	}

	if err := e.PublishStagedChartForTest(t.Context()); err != nil {
		t.Fatalf("publish the staged chart: %v", err)
	}
	got, err := e.Chart().Read(t.Context(), statelog.Freshness{
		Level: statelog.ReadLinearizable,
	})
	if err != nil {
		t.Fatalf("read the chart: %v", err)
	}
	if !holdsUnit(got, "staged-team") || !holdsSeat(got, "staged-seat") {
		t.Fatalf("the staged chart did not land: units=%v seats=%v",
			keysOf(got), handlesOf(got))
	}

	// AND THE STAGE IS SPENT. A stage that survived its publish would be
	// redeemed again on every boot, which is an operator's abandoned
	// structure re-applied for ever.
	if _, found, err := staged.Take(t.Context()); err != nil || found {
		t.Errorf("the stage survived its publish (found=%v, err=%v)", found, err)
	}
	// A SECOND PUBLISH DOES NOTHING, which is what makes a crash between
	// the take and the publish cost a re-run rather than a wrong company.
	if err := e.PublishStagedChartForTest(t.Context()); err != nil {
		t.Errorf("publishing with nothing staged failed: %v", err)
	}
}

// A STAGE REPLACES A STAGE, because it is a PENDING INTENT and not a history:
// two offline imports in a row mean the operator changed their mind, and
// publishing both would replay a structure they have already abandoned.
func TestStagingTwiceKeepsOnlyTheSecond(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})
	staged := e.Backends().Store.StagedCharts()
	for _, id := range []string{"file:first", "file:second"} {
		if err := staged.Stage(t.Context(), store.StagedChart{
			ID: id, Payload: []byte("sealed"), SourcePath: id,
		}); err != nil {
			t.Fatalf("Stage %s: %v", id, err)
		}
	}
	got, found, err := staged.Take(t.Context())
	if err != nil || !found {
		t.Fatalf("Take: found=%v err=%v", found, err)
	}
	if got.ID != "file:second" {
		t.Errorf("the staged chart is %q, want the second one", got.ID)
	}
	if _, found, _ := staged.Take(t.Context()); found {
		t.Error("a second row survived, so two imports would both publish")
	}
}

// AN OFFLINE IMPORT'S CHART SURVIVES A ROUND TRIP THROUGH THE STAGE.
//
// The command seals an encoded chart.Authored and the boot opens it; a
// mismatch between the two would be a stage that is written, kept and then
// silently discarded at the one moment it matters.
func TestTheStagedPayloadRoundTripsAsAnAuthoredChart(t *testing.T) {
	t.Parallel()
	cfg, err := config.ParseCompany([]byte(companyDoc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	authored := config.AuthoredChart(cfg)
	if len(authored.Edges()) == 0 {
		t.Fatal("the fixture declares no chart, so this proves nothing")
	}
	body, err := json.Marshal(authored)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := secrets.Seal(nil, body)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := secrets.Open(nil, payload)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	var back chart.Authored
	if err := json.Unmarshal(opened, &back); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(back.Edges()) != len(authored.Edges()) {
		t.Errorf("the round trip changed the edge count: %d -> %d",
			len(authored.Edges()), len(back.Edges()))
	}
	if chart.ImportKey(back) != chart.ImportKey(authored) {
		t.Error("the round trip changed the chart's content key, so the " +
			"ledger would treat it as a different structure")
	}
}

func holdsUnit(c chart.Chart, key string) bool {
	for _, u := range c.Units {
		if u.Key == key {
			return true
		}
	}
	return false
}

func holdsSeat(c chart.Chart, handle string) bool {
	for _, s := range c.Seats {
		if s.Handle == handle {
			return true
		}
	}
	return false
}

func keysOf(c chart.Chart) string {
	var out []string
	for _, u := range c.Units {
		out = append(out, u.Key)
	}
	return strings.Join(out, ",")
}

func handlesOf(c chart.Chart) string {
	var out []string
	for _, s := range c.Seats {
		out = append(out, s.Handle)
	}
	return strings.Join(out, ",")
}

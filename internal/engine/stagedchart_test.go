package engine_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/org"
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
	stageChart(t, e, authored)

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

// A STAGED FILE THAT DECLARES NO RUNTIME FOR AN OBJECT TAKES THE OBJECT'S AWAY.
//
// A content write that leaves the runtime half out KEEPS the one the object
// has, which is what lets a lead correct a goal they may not send the half back
// with. A staged file is not that: it is an operator's "this file is the chart
// again", so an object it declares no runtime for is an object with none, and
// the seed clears the half rather than leaving it out — for a unit and for a
// seat alike, since each is its own content write. Carried instead, an
// offline import that removed a seat's model chain and credentials, or a
// team's `mcp_env`, would publish, report success, and leave the object
// running on what the operator took away. Mutation: drop either of the seed's
// clears and that object keeps its half.
func TestAStagedChartThatDeclaresNoRuntimeClearsTheObjects(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})
	if len(seatRuntime(t, e, "ceo")) == 0 {
		t.Fatal("the fixture's ceo holds no runtime half, so this proves nothing")
	}

	// FIRST, THE COMPANY'S OWN FILE WITH A TEAM THAT HAS A RUNTIME HALF, so
	// both kinds of object hold one before the file that takes them away.
	authored := config.AuthoredChart(parsedCompany(t, companyDoc))
	teamRuntime, err := org.UnitRuntime(&org.Unit{MCPEnv: org.MCPEnv{
		"tracker": {"TRACKER_TOKEN": "${TRACKER_TOKEN_PLATFORM}"},
	}})
	if err != nil || len(teamRuntime) == 0 {
		t.Fatalf("encode a unit's runtime half: %v (%s)", err, teamRuntime)
	}
	authored.Units = append(authored.Units, chart.AuthoredUnit{
		Key: "platform", Name: "Platform", Runtime: teamRuntime,
	})
	stageChart(t, e, authored)
	if err := e.PublishStagedChartForTest(t.Context()); err != nil {
		t.Fatalf("publish the staged chart: %v", err)
	}
	if len(unitRuntime(t, e, "platform")) == 0 {
		t.Fatal("the first staged file left platform with no runtime half, so " +
			"the clear below would prove nothing")
	}

	// THEN THE SAME FILE, declaring no runtime for the team or for ceo and
	// every other object exactly as before.
	authored.Units[len(authored.Units)-1].Runtime = nil
	var declared bool
	for i := range authored.Seats {
		if authored.Seats[i].Handle == "ceo" {
			authored.Seats[i].Runtime = nil
			declared = true
		}
	}
	if !declared {
		t.Fatal("the fixture's file declares no ceo, so this proves nothing")
	}
	stageChart(t, e, authored)
	if err := e.PublishStagedChartForTest(t.Context()); err != nil {
		t.Fatalf("publish the staged chart: %v", err)
	}

	if got := unitRuntime(t, e, "platform"); len(got) != 0 {
		t.Errorf("a file declaring no runtime for platform left the unit's own "+
			"in place: %s", got)
	}
	if got := seatRuntime(t, e, "ceo"); len(got) != 0 {
		t.Errorf("a file declaring no runtime for ceo left the seat's own in "+
			"place: %s", got)
	}
	// AND A SEAT THE FILE DOES DECLARE ONE FOR KEEPS IT, which is what says
	// the staged content was published rather than that every runtime half
	// was lost on the way.
	if len(seatRuntime(t, e, "cto")) == 0 {
		t.Error("the staged publish cleared cto's runtime half, which the file declares")
	}
}

// A STAGE THAT DOES NOT OPEN SAYS THE ONE THING THAT BRINGS IT BACK.
//
// An offline import by a build older than the mandatory keyring staged its
// chart in the clear, and the keyring refuses to open that. The stage is taken
// before it is opened, so it is spent either way, and the remedy is the
// import's own: re-run it against a running node — which every failure past
// the take says, so the boot's warning carries it whatever went wrong.
//
// Mutation: return the open's error without the stage's remedy and the case
// fails.
func TestAStageThatDoesNotOpenNamesTheImport(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})
	body, err := json.Marshal(chart.Authored{
		Units: []chart.AuthoredUnit{{Key: "clear-team", Name: "Clear Team"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Backends().Store.StagedCharts().Stage(t.Context(), store.StagedChart{
		ID: "file:clear", Payload: body, SourcePath: "company.yaml", StagedBy: "ops",
	}); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	err = e.PublishStagedChartForTest(t.Context())
	if !errors.Is(err, secrets.ErrUnsealedWithKey) {
		t.Fatalf("publishing a stage stored in the clear = %v, want it refused unsealed", err)
	}
	for _, says := range []string{"company.yaml", "re-run the import against a running node"} {
		if !strings.Contains(err.Error(), says) {
			t.Errorf("the refusal does not say %q: %v", says, err)
		}
	}
	// AND ONLY THAT: the keyring's refusal used to name `crewlet config
	// seal`, which seals a revision and never a stage, so the boot's
	// warning named two remedies and one of them did nothing.
	if strings.Contains(err.Error(), "config seal") {
		t.Errorf("the refusal names a command that seals a revision, not a stage: %v", err)
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
	cipher, err := bootstrap(t, nil).Secrets.Cipher()
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	payload, err := secrets.Seal(cipher, body)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := secrets.Open(cipher, payload)
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

// stageChart stages authored as an offline `crewlet config import` would.
func stageChart(t *testing.T, e *engine.Engine, authored chart.Authored) {
	t.Helper()
	body, err := json.Marshal(authored)
	if err != nil {
		t.Fatal(err)
	}
	// SEALED UNDER THE NODE'S KEYRING, as the offline command seals what it
	// stages: every node holds one, and a stage stored in the clear is one
	// the boot refuses to open ([TestAStageThatDoesNotOpenNamesTheImport]).
	payload, err := secrets.Seal(e.Cipher(), body)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Backends().Store.StagedCharts().Stage(t.Context(), store.StagedChart{
		ID: chart.ImportKey(authored), Payload: payload,
		SourcePath: "company.yaml", StagedBy: "ops",
	}); err != nil {
		t.Fatalf("Stage: %v", err)
	}
}

// seatRuntime is the runtime half the chart's rows hold for one seat.
func seatRuntime(t *testing.T, e *engine.Engine, handle string) json.RawMessage {
	t.Helper()
	detail, err := e.Chart().Seat(t.Context(), handle, statelog.Freshness{
		Level: statelog.ReadLinearizable,
	})
	if err != nil {
		t.Fatalf("read the seat %s: %v", handle, err)
	}
	if detail.Seat.Handle == "" {
		t.Fatalf("the chart holds no seat %s", handle)
	}
	return detail.Seat.Runtime
}

// unitRuntime is the runtime half the chart's rows hold for one unit.
func unitRuntime(t *testing.T, e *engine.Engine, key string) json.RawMessage {
	t.Helper()
	detail, err := e.Chart().Unit(t.Context(), key, statelog.Freshness{
		Level: statelog.ReadLinearizable,
	})
	if err != nil {
		t.Fatalf("read the unit %s: %v", key, err)
	}
	if detail.Unit.Key == "" {
		t.Fatalf("the chart holds no unit %s", key)
	}
	return detail.Unit.Runtime
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

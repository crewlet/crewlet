package engine_test

import (
	"os"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// THE ENVIRONMENT AN ENGINE IS HANDED IS THE ONE IT READS, AND THE ONLY ONE.
//
// A company's `${VAR}` references fall back to it, and the per-run endpoints'
// settings are read from it, so cases configure an engine without t.Setenv
// (which refuses t.Parallel). Nothing reads the process BEHIND it, or a case
// would pass on whatever its runner exported: so PATH must not be found.
func TestAnEngineReadsTheEnvironmentItIsHanded(t *testing.T) {
	t.Parallel()
	if _, ok := os.LookupEnv("PATH"); !ok {
		t.Fatal("the premise: this process has a PATH for the engine not to read")
	}
	e := newEngine(t, engine.Options{Environment: config.MapSource{
		"CREWLET_TEST_HANDED":      "from-the-handed-environment",
		mcpbridge.BaseURLVar:       "https://bridge.example.com",
		sandbox.OtelReceiverURLVar: "https://otel.example.com",
	}})
	if got := e.Resolve("${CREWLET_TEST_HANDED}"); got != "from-the-handed-environment" {
		t.Errorf("a reference resolved to %q, want the handed environment's value", got)
	}
	if got := e.Resolve("${PATH}"); got != "" {
		t.Errorf("${PATH} resolved to %q: the process environment is read behind "+
			"the one the engine was handed", got)
	}
	if e.Bridge() == nil {
		t.Error("no MCP bridge, though the handed environment names its base URL")
	}
	if e.OtelReceiver() == nil {
		t.Error("no telemetry receiver, though the handed environment names its URL")
	}
}

// A NODE THAT BOUND A LISTENER HANDS ITS OWN BOXES BOTH ROUTES THERE. The
// address a `direct` box dials is handed in as BoxHostURL by whoever bound the
// listener, and is all such a box needs: the bridge is built from it with no
// fleet address set, and where this node forwards telemetry (the standard
// upstream variable set) so is the receiver. Where it does not forward, it
// builds no receiver, since every export would be dropped.
//
// Mutation: build the bridge from the environment alone and the first engine
// has none; build the receiver from the environment alone and it has no
// receiver; build it whatever the upstream and the second engine has one.
func TestAnEngineHandsItsOwnBoxesTheListenerItWasGiven(t *testing.T) {
	t.Parallel()
	const host = "http://127.0.0.1:8080"
	e := newEngine(t, engine.Options{BoxHostURL: host, Environment: config.MapSource{
		sandbox.OtelUpstreamEndpointVar: "https://collector.example",
	}})
	if bridge := e.Bridge(); bridge == nil || bridge.Reaches(true) != host {
		t.Errorf("the bridge a direct box dials = %v, want one at %s", bridge, host)
	}
	if receiver := e.OtelReceiver(); receiver == nil || receiver.Reaches(true) != host {
		t.Errorf("the receiver a direct box exports to = %v, want one at %s", receiver, host)
	}
	silent := newEngine(t, engine.Options{BoxHostURL: host, Environment: config.MapSource{}})
	if silent.Bridge() == nil {
		t.Error("a node with its own listener and no fleet address built no bridge")
	}
	if receiver := silent.OtelReceiver(); receiver != nil {
		t.Errorf("a node that forwards no telemetry built a receiver at %q", receiver.Reaches(true))
	}
}

// AND A PERSON'S CONTACT IS READ FROM IT TOO, by the surfaces that name people:
// a seat's lookup_colleague finds the founder by an id only the handed
// environment holds, as notification routing (through the node's chain) does.
func TestAColleagueIsFoundByAnIdTheHandedEnvironmentHolds(t *testing.T) {
	t.Parallel()
	const variable = "CREWLET_ENGINE_TEST_HANDED_FOUNDER_SLACK"
	if _, set := os.LookupEnv(variable); set {
		t.Fatalf("the premise: %s is set in no process", variable)
	}
	e := newEngine(t, engine.Options{
		Company: parsedCompany(t, strings.Replace(companyDoc,
			"slack_user_id: U0FOUNDER", "slack_user_id: ${"+variable+"}", 1)),
		Environment: config.MapSource{variable: "U0HANDED"},
	})
	entry, ok := e.ToolsFor("ceo").Lookup(builtin.LookupColleagueTool)
	if !ok {
		t.Fatal("the seat's surface has no lookup_colleague")
	}
	seated, ok := entry.Tool.(tools.SeatCallable)
	if !ok {
		t.Fatal("lookup_colleague cannot know who called it")
	}
	o := e.Company().Org
	res, err := seated.CallForTurn(t.Context(), &turnctx.Turn{RunID: "run-1",
		WorkKey: "wk-1", Seat: o.AgentSeatByHandle("ceo"), Org: o},
		map[string]any{"query": "U0HANDED"})
	if err != nil {
		t.Fatalf("lookup_colleague: %v", err)
	}
	if res.Failed || !strings.Contains(res.Output, "handle: founder") {
		t.Fatalf("the founder's id in the handed environment found nobody:\n%s", res.Output)
	}
}

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
// settings are read from it. Read from the process instead, every case that
// configured an engine through a variable had to set it with t.Setenv, which
// Go refuses beside t.Parallel — so they ran one at a time. And read from the
// process BEHIND the handed one, a case would pass on whatever its runner
// happened to export: so a variable every process has must not be found.
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

// AND A PERSON'S CONTACT IS READ FROM IT TOO, by the surfaces that name people
// as well as by the configuration: a seat's lookup_colleague finds the founder
// by an id the handed environment holds and no process sets. Read from the
// process instead, notification routing — which resolves through the node's
// chain — mentioned a person no lookup could find.
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

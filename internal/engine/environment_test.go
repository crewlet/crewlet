package engine_test

import (
	"os"
	"testing"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/sandbox"
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

package engine_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/tools"
)

// A SEAT'S OWN write_page REFUSES BOTH RESERVED CONTAINERS, AS THE ENGINE WIRES IT.
//
// The tool-skills container holds the guidance the engine injects into seats'
// phases, so a seat writing there would be rewriting its own instructions with
// nobody reviewing it; the org root holds what the company publishes rather
// than what one seat decides. The refusal itself is internal/agent/builtin's
// and is tested there against deps that test builds; what only this case can
// see is whether the registry a seat's turns run against is equipped with the
// company's two containers. The control is the same write into an ordinary
// container, which lands.
func TestASeatsWritePageRefusesTheReservedContainers(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})
	waitFor(t, "the native backends to hydrate", e.NativeHydrated)
	c := e.Company()
	entry, ok := c.Tools.Lookup(builtin.WritePageTool)
	if !ok {
		t.Fatal("the default company's seats have no write_page")
	}
	writePage, ok := entry.Tool.(tools.SeatCallable)
	if !ok {
		t.Fatal("write_page is not seat-callable")
	}
	turn := &turnctx.Turn{
		RunID: "run-1", WorkKey: "work-1",
		Seat: c.Org.AgentSeatByHandle("ceo"), Org: c.Org,
	}
	write := func(container string) tools.Result {
		t.Helper()
		got, err := writePage.CallForTurn(t.Context(), turn, map[string]any{
			"title": "Onboarding", "body": "What every seat reads first.",
			"container": container,
		})
		if err != nil {
			t.Fatalf("write_page into %s: %v", container, err)
		}
		return got
	}

	for _, container := range []string{config.DefaultSkillsContainer, config.DefaultRootSpace} {
		if got := write(container); !got.Failed || !strings.Contains(got.Output, "reserved") {
			t.Errorf("a seat's write into %s answered %q, want the reserved refusal",
				container, got.Output)
		}
	}
	if got := write("ENG"); got.Failed {
		t.Errorf("a seat's write into an ordinary container was refused: %s", got.Output)
	}
}

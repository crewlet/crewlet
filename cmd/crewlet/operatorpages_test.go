package main

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/tools"
)

// AN OPERATOR'S OWN ASSISTANT WRITES BOTH RESERVED CONTAINERS, AS THIS BINARY
// WIRES IT.
//
// write_page is the only thing that creates a page on the native knowledge
// base, and a seat's own surface refuses the tool-skills container and the org
// root. So the operator's surface is how a native company publishes its tool
// skills and its root Onboarding page, and a reservation on it would leave the
// company no way to publish either. The seat half is held in internal/engine,
// against the registry the engine equips; this is the other surface, through
// the page deps [operatorMCP] serves it with.
func TestTheOperatorSurfaceWritesTheReservedContainers(t *testing.T) {
	t.Parallel()
	e := testEngine(t)
	for deadline := time.Now().Add(15 * time.Second); !e.NativeHydrated(); {
		if time.Now().After(deadline) {
			t.Fatal("the native backends never hydrated")
		}
		time.Sleep(20 * time.Millisecond)
	}

	var writePage tools.SeatCallable
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{Pages: operatorPages(e)}) {
		if tool.Name() == builtin.WritePageTool {
			writePage, _ = tool.(tools.SeatCallable)
		}
	}
	if writePage == nil {
		t.Fatal("the operator surface offers no write_page on a native company")
	}

	// THE OPERATOR'S OWN CREDENTIAL, which is who the write is attributed to:
	// the surface reads it off the request, and there is no turn.
	ctx := auth.WithOperator(t.Context(), "founder")
	for _, container := range []string{config.DefaultSkillsContainer, config.DefaultRootSpace} {
		got, err := writePage.CallForTurn(ctx, nil, map[string]any{
			"title": "Onboarding", "body": "What every seat reads first.",
			"container": container,
		})
		if err != nil {
			t.Fatalf("write_page into %s: %v", container, err)
		}
		if got.Failed {
			t.Errorf("the operator's write into %s was refused: %s", container, got.Output)
		}
	}
}

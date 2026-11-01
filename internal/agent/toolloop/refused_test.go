package toolloop_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
)

// recordingSurface is a [surface] that keeps a record of the calls it was told
// did not run, as the Dashboard Assistant's surface draws a refused step for
// each ([toolloop.Refuser]).
type recordingSurface struct {
	*surface
	mu      sync.Mutex
	refused []string
}

func (s *recordingSurface) Refused(_ context.Context, call llm.ToolCall, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refused = append(s.refused, call.Name+": "+reason)
}

// A CALL THE LOOP DOES NOT RUN IS ON THE SURFACE'S RECORD, never a request
// that silently asked for nothing: a call whose arguments did not parse is
// answered without running, and the surface is told so, with the sentence the
// model was answered with.
//
// Mutation: drop tellRefused from the dispatcher's refusal path and the
// surface's record is empty.
func TestACallTheLoopDoesNotRunIsOnTheRecord(t *testing.T) {
	t.Parallel()
	s := &recordingSurface{surface: newSurface("read")}
	prov := script(runOf(
		llmtest.Response{Calls: []llmtest.Call{
			{Name: "read", Args: `["not", "an object"]`},
			{Name: "read", Args: `{"id": 7}`},
		}},
		says("Read it."),
	))
	if _, err := run(t, toolloop.Config{Provider: prov, Surface: s, MaxRounds: 5}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	s.mu.Lock()
	refused := append([]string(nil), s.refused...)
	s.mu.Unlock()
	if len(refused) != 1 || !strings.HasPrefix(refused[0], "read: ") {
		t.Fatalf("refused %q, want the call whose arguments did not parse, with why", refused)
	}
	s.surface.mu.Lock()
	ran := strings.Join(s.surface.ran, ",")
	s.surface.mu.Unlock()
	if ran != "read" {
		t.Errorf("ran %q, want only the call whose arguments parsed", ran)
	}
}

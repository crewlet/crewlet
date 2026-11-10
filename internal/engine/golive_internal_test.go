package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE GO-LIVE REVIEW READS EACH AGENT'S ACCESS AS ITS NEXT TURN IS TOLD IT:
// one row per agent seat, never a person's, with the box its runs use and what
// a run last found in it.
func TestEveryAgentsAccessIsReadForTheGoLiveReview(t *testing.T) {
	t.Parallel()
	e, _ := boxedEngine(t)
	ctx := t.Context()
	company := boxedCompany(t, "registry.example.com/coding:1")
	e.epoch.current.Store(company)
	e.keepBoxReading(ctx, company, "ceo", sandbox.Container,
		sandbox.BoxFacts{MemoryBytes: 4 << 30, Browser: "chromium"})
	got, err := e.AgentCapabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Handle != "ceo" || got[0].Name != "CEO" {
		t.Fatalf("agents = %+v, want the CEO alone", got)
	}
	boxes := got[0].Access.Boxes
	if len(boxes) != 1 || !boxes[0].Observed || boxes[0].Browser != "chromium" ||
		boxes[0].MemoryBytes != 4<<30 {
		t.Errorf("the CEO's boxes are %+v, want its container with the browser a run found", boxes)
	}
}

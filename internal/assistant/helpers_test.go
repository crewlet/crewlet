package assistant_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// steps are a stream's last frame of each step, by index.
func steps(frames []wire.Frame) map[int]wire.StepEvent {
	out := map[int]wire.StepEvent{}
	for _, f := range frames {
		if f.Kind == wire.EventStep && f.Step != nil {
			out[f.Step.Index] = *f.Step
		}
	}
	return out
}

// offered is what the model was offered in its first round, by name.
func offered(t *testing.T, f *assistanttest.Rig) []string {
	t.Helper()
	if len(f.Model.Requests()) == 0 {
		t.Fatal("the model was never asked")
	}
	var names []string
	for _, def := range f.Model.OfferedAt(0) {
		names = append(names, def.Name)
	}
	return names
}

// lastToolAnswer is the model's last reading of a call, by its id: the answer
// its harness was given, or, for a call a turn parked on, the answer the
// resumed run's conversation carries for it.
func lastToolAnswer(f *assistanttest.Rig, id string) (llm.Message, bool) {
	var out llm.Message
	found := false
	for _, m := range f.Model.Seen() {
		if m.Role == llm.RoleTool && m.ToolCallID == id {
			out, found = m, true
		}
	}
	return out, found
}

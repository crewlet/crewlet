package runner

import (
	"context"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/tools"
)

// sequencedStub is a write named after the calls before it in its turn.
type sequencedStub struct{ name string }

func (s sequencedStub) Name() string             { return s.name }
func (sequencedStub) Description() string        { return "A write." }
func (sequencedStub) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (sequencedStub) Sequenced()                 {}
func (sequencedStub) Call(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{}, nil
}

// plainStub is any other tool.
type plainStub struct{ name string }

func (s plainStub) Name() string             { return s.name }
func (plainStub) Description() string        { return "A read." }
func (plainStub) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (plainStub) Call(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{}, nil
}

// THE ONBOARDING PASS'S TOOLS ARE A COPY. The seat's registry is what every
// later phase of the turn reads, so the pass leaving a write out of its own
// universe must leave it in the seat's: removed from the registry itself, the
// turn's executor would be offered no tracker write at all.
func TestTheOnboardingPassLeavesTheSeatsRegistryWhole(t *testing.T) {
	t.Parallel()
	reg := tools.NewRegistry()
	for _, tool := range []tools.Callable{plainStub{"read_page"}, sequencedStub{"update_item"}} {
		if err := reg.Register(tool, tools.OriginBuiltin); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}

	pass := unsequenced(reg)
	if got := pass.Names(); !slices.Equal(got, []string{"read_page"}) {
		t.Errorf("the pass may call %v, want the read alone", got)
	}
	if got := reg.Names(); !slices.Equal(got, []string{"read_page", "update_item"}) {
		t.Errorf("the seat's registry holds %v after the pass's copy was made, want both", got)
	}
}

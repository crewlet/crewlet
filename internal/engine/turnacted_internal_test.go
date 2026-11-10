package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events/types"
)

// A turn acted outside the engine when a successful call could reach somebody
// on the seat's surface: a vendor's post counts, a read or a failed post not.
func TestATurnActedOnlyThroughASuccessfulDelivery(t *testing.T) {
	t.Parallel()
	surface := turn.Surface{Deliveries: map[string]string{"chat__post": "slack"}}
	call := func(name string, ok bool) types.ToolExecution {
		return types.ToolExecution{"name": name, "success": ok}
	}
	for _, tc := range []struct {
		name  string
		execs []types.ToolExecution
		want  bool
	}{
		{"a vendor's post", []types.ToolExecution{call("lookup_colleague", true), call("chat__post", true)}, true},
		{"a read only", []types.ToolExecution{call("lookup_colleague", true)}, false},
		{"a failed post", []types.ToolExecution{call("chat__post", false)}, false},
		{"no calls", nil, false},
	} {
		if got := actedOutside(tc.execs, surface); got != tc.want {
			t.Errorf("%s: acted = %v, want %v", tc.name, got, tc.want)
		}
	}
	if actedOutside([]types.ToolExecution{call("chat__post", true)}, turn.Surface{}) {
		t.Error("a seat with no surface reached somebody")
	}
}

package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events/types"
)

// A turn acted outside the engine when a successful call could reach somebody
// on the seat's surface: a vendor's post counts, a read or a failed post not —
// and so does a post its record counts by tool without listing (a long list
// past its share keeps its first and newest calls), where one of the calls it
// counts succeeded.
//
// Mutation: read the listed calls alone, and a turn whose post fell in a long
// list's dropped middle is said not to have acted; count a tally's failures as
// successes, and a turn whose every post failed is.
func TestATurnActedOnlyThroughASuccessfulDelivery(t *testing.T) {
	t.Parallel()
	surface := turn.Surface{Deliveries: map[string]string{"chat__post": "slack"}}
	call := func(name string, ok bool) types.ToolExecution {
		return types.ToolExecution{"name": name, "success": ok}
	}
	for _, tc := range []struct {
		name   string
		execs  []types.ToolExecution
		byTool []types.ElidedCalls
		want   bool
	}{
		{"a vendor's post", []types.ToolExecution{call("lookup_colleague", true), call("chat__post", true)}, nil, true},
		{"a read only", []types.ToolExecution{call("lookup_colleague", true)}, nil, false},
		{"a failed post", []types.ToolExecution{call("chat__post", false)}, nil, false},
		{"no calls", nil, nil, false},
		{"a post the record counts", nil, []types.ElidedCalls{{Name: "chat__post", Calls: 2, Failed: 1}}, true},
		{"posts the record counts, all failed", nil, []types.ElidedCalls{{Name: "chat__post", Calls: 2, Failed: 2}},
			false},
		{"calls to tools the record did not keep", nil, []types.ElidedCalls{{Calls: 3}}, false},
	} {
		if got := actedOutside(runner.RecordCalls{Rows: tc.execs, ByTool: tc.byTool}, surface); got != tc.want {
			t.Errorf("%s: acted = %v, want %v", tc.name, got, tc.want)
		}
	}
	if actedOutside(runner.RecordCalls{Rows: []types.ToolExecution{call("chat__post", true)}}, turn.Surface{}) {
		t.Error("a seat with no surface reached somebody")
	}
}

package mcp

import (
	"testing"
	"time"
)

// A TOOL IS BOUND BY ITS SERVER'S REQUEST TIMEOUT. The client cuts a call
// that has not answered at the server's request timeout, so that is the longest
// one call of the tool runs, and what the tool declares as its bound: a box's
// harness is told to wait for a call as long as the longest bound its tools
// declare (internal/api/mcpbridge), and a server whose entry sets a longer
// timeout than the engine's default is waited on that long.
//
// Mutation: declare the engine's default whatever the entry sets, and the
// entry that sets its own is cut short.
func TestAToolIsBoundByItsServersRequestTimeout(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		set  time.Duration
		want time.Duration
	}{
		{"an entry that sets none", 0, DefaultRequestTimeout},
		{"an entry that sets its own", 2 * time.Hour, 2 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tool := &Tool{srv: &server{spec: Spec{Name: "slow", Command: "slow-server", RequestTimeout: tc.set}}}
			if got := tool.CallBound(); got != tc.want {
				t.Errorf("CallBound = %s, want %s: the timeout the tool's client cuts a call at", got, tc.want)
			}
		})
	}
}

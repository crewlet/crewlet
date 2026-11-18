package mcp

import (
	"testing"
	"time"
)

// A TOOL IS BOUND BY ITS SERVER'S REQUEST TIMEOUT, after the wait for its
// server's replacement. The client cuts a call at the request timeout, and a
// call made while the server is replaced first waits for the calls in flight
// (that timeout again), the shutdown ladder and the new start: a box's harness
// is told to wait for a call as long as the longest bound its tools declare
// (internal/api/mcpbridge), so one that waited out a restart is not given up.
//
// Mutation: declare the engine's default whatever the entry sets, or leave the
// replacement's wait out, and a call is cut short.
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
			srv := NewBridge(discardLogger(), Supervision{}).newServer(
				Spec{Name: "slow", Command: "slow-server", RequestTimeout: tc.set}, func() {})
			tool := &Tool{srv: srv}
			ladder := 2*shutdownGrace + stderrDrainTimeout + stderrReapGrace
			if got, want := tool.CallBound(), 2*tc.want+ladder+2*DefaultStartupTimeout; got != want {
				t.Errorf("CallBound = %s, want %s: the request timeout, after a replacement's wait", got, want)
			}
		})
	}
}

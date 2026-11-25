package mcpbridge_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
)

// A HARNESS CONNECTS ITS BRIDGE BY LISTING THE ENGINE'S OWN TOOLS, and the
// session is told so once per opening, under the run's context: not by the
// handshake alone (Claude Code's probe, a hook's client), nor by a list of a
// seat server's tools, which come without the server named `crewlet`; a second
// list, or a second client listing, tells it nothing more.
//
// Mutation: tell the session on `initialize`, on any server's list, under the
// request's context, or on every list, and a check goes red.
func TestAHarnessConnectsItsBridgeByListingTheEnginesOwnTools(t *testing.T) {
	t.Parallel()
	var told atomic.Int32
	var hookCtx atomic.Value
	f := openServers(t, serversSurface(t), func(s *mcpbridge.Session) {
		s.Connected = func(ctx context.Context) {
			told.Add(1)
			hookCtx.Store(ctx)
		}
	})
	own := dial(t, f.endpoint)
	if n := told.Load(); n != 0 {
		t.Fatalf("the session was told %d times by the handshake alone, want none", n)
	}
	if names, _ := listed(t, mcpbridge.ServerURL(f.endpoint, "github")); len(names) == 0 {
		t.Fatal("the seat's github server listed nothing")
	}
	if n := told.Load(); n != 0 {
		t.Fatalf("the session was told %d times by a seat server's list, want none", n)
	}
	list := func() {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		if _, err := own.ListTools(ctx, nil); err != nil {
			t.Fatalf("ListTools: %v", err)
		}
	}
	list()
	if n := told.Load(); n != 1 {
		t.Fatalf("the session was told %d times by the harness's list of the engine's tools, want once", n)
	}
	if ctx, _ := hookCtx.Load().(context.Context); ctx == nil || ctx.Err() != nil {
		t.Errorf("the session was told under a context that ended with its request (%v), want the run's", ctx)
	}
	list()
	if names, _ := listed(t, f.endpoint); len(names) == 0 {
		t.Fatal("a second client listed nothing")
	}
	if n := told.Load(); n != 1 {
		t.Errorf("the session was told %d times, want once per opening", n)
	}
}

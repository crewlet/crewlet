package mcp

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// within is what ch delivers, failing the case where nothing comes in time.
func within[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(time.Minute): //fixedwait:liveness
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

// fileExists reports whether path exists.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// A TOOL'S LEVEL, PREFIX OR HINTS MOVE WITHOUT RESTARTING ITS SERVER. Always
// allow writes a level, which reaches the bridge as a spec differing there
// alone: the server is refiled from the listing it kept and its process runs
// on, so the call a person just approved, through the Tool a registry already
// held, is made rather than refused by a restart.
//
// Mutation: have Reconcile or Runs compare every field (equal), or refile
// without the kept listing, and this is red.
func TestAToolsLevelMovesWithoutRestartingItsServer(t *testing.T) {
	t.Parallel()
	b, _ := supervisedBridge(t, Supervision{})
	asking, starts := countedSpec(t, "cloud", "serve", map[string]string{
		helperToolsEnv: toolsJSON([3]string{"bucket_update", "d", ""}, [3]string{"bucket_list", "d", ""}),
	})
	asking.Levels = Levels{Tools: map[string]Level{"bucket_update": LevelApprove}}
	if _, err := b.Add(t.Context(), asking); err != nil {
		t.Fatalf("Add: %v", err)
	}
	held, _ := b.Tool("bucket_update")
	if held.Level() != LevelApprove {
		t.Fatalf("precondition: bucket_update at %q, want approve", held.Level())
	}

	withLevels := func(levels map[string]Level) Spec {
		spec := asking
		spec.Levels = Levels{Tools: levels}
		return spec
	}
	allowed := withLevels(map[string]Level{"bucket_update": LevelAllow})
	if !b.Runs(allowed) {
		t.Fatal("a spec moving only a level reads as another process")
	}
	if _, err := b.Reconcile(t.Context(), allowed); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if now, _ := b.Tool("bucket_update"); now == nil || now.Level() != LevelAllow {
		t.Fatal("the refiled tool is not at allow")
	}
	if res, err := held.Call(t.Context(), map[string]any{"bucket": "b"}); err != nil || res.Failed {
		t.Fatalf("the call through the Tool held before the refile = %+v, %v; want it made", res, err)
	}

	// BLOCKED AND BACK, from the kept listing.
	if _, err := b.Reconcile(t.Context(), withLevels(map[string]Level{"bucket_update": LevelBlocked})); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := b.Tool("bucket_update"); ok || len(b.BlockedTools("cloud")) != 1 {
		t.Fatal("a tool its level blocks is still filed")
	}
	if _, err := b.Reconcile(t.Context(), allowed); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := b.Tool("bucket_update"); !ok {
		t.Fatal("a tool let through again was not filed from the kept listing")
	}

	// RENAMED by a prefix, the same way.
	prefixed := allowed
	prefixed.ToolPrefix = "cloud_"
	if _, err := b.Reconcile(t.Context(), prefixed); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, ok := b.Tool("cloud_bucket_list"); !ok {
		t.Fatal("a new prefix did not rename the server's tools")
	}
	if _, ok := b.Tool("bucket_list"); ok {
		t.Fatal("the old name outlived the prefix that replaced it")
	}
	if n := startsIn(starts); n != 1 {
		t.Fatalf("the server was started %d times, want once: a filing change restarted it", n)
	}
}

// A RESTART LETS THE CALLS IN FLIGHT FINISH AND MAKES THOSE MADE MEANWHILE ON
// THE REPLACEMENT. A rotated credential restarts the child: the call already
// at the server is answered before it stops, and a call through the old Tool
// (the one an approval releases) is made on the new child, never refused.
//
// Mutation: drop the wait for the calls in flight, or refuse a call handed on,
// and this is red.
func TestARestartFinishesTheCallsInFlightAndHandsOnTheRest(t *testing.T) {
	t.Parallel()
	b, rec := supervisedBridge(t, Supervision{})
	dir := t.TempDir()
	seen, gate := filepath.Join(dir, "seen"), filepath.Join(dir, "gate")
	first, starts := countedSpec(t, "cloud", "serve", map[string]string{
		helperToolsEnv: toolsJSON([3]string{"bucket_update", "d", ""}),
		"SEAT_TOKEN":   "first", helperCallSeenEnv: seen, helperCallGateEnv: gate,
	})
	if _, err := b.Add(t.Context(), first); err != nil {
		t.Fatalf("Add: %v", err)
	}
	tool, _ := b.Tool("bucket_update")

	inFlight := make(chan Result, 1)
	go func() {
		res, _ := tool.Call(context.Background(), nil)
		inFlight <- res
	}()
	eventually(t, "the call to reach the server", func() bool { return fileExists(seen) })

	rotated := first
	rotated.Env = maps.Clone(first.Env)
	rotated.Env["SEAT_TOKEN"] = "second"
	reached := filepath.Join(dir, "reached")
	rotated.Env[helperCallSeenEnv] = reached
	restarted := make(chan error, 1)
	go func() {
		_, err := b.Restart(context.Background(), rotated)
		restarted <- err
	}()
	eventually(t, "the restart to wait for the call in flight", func() bool {
		return rec.hasAttr("server_replacement_waiting", "calls_in_flight", int64(1))
	})
	handed := make(chan Result, 1)
	go func() {
		res, _ := tool.Call(context.Background(), nil)
		handed <- res
	}()
	if n := startsIn(starts); n != 1 {
		t.Fatalf("the restart replaced the child under a call in flight (%d starts)", n)
	}

	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if res := within(t, "the call in flight", inFlight); res.Failed {
		t.Fatalf("the call in flight = %q, want its answer", res.Output)
	}
	if err := within(t, "the restart", restarted); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if res := within(t, "the call made meanwhile", handed); res.Failed {
		t.Fatalf("the call made while the server was replaced = %q, want it made on the replacement", res.Output)
	}
	if !fileExists(reached) || startsIn(starts) != 2 {
		t.Fatalf("the call was not made on the replacement (%d starts)", startsIn(starts))
	}
}

// A CALL HANDED ON IS MADE ONLY ON THE TOOL AS IT WAS CALLED. A replacement
// that lists the tool otherwise (the definition a person approved, changed),
// or asks before it where the call was not asked, refuses the call unsent; one
// at a level asking less (Always allow landing with a rotated credential)
// makes it; one that does not come up refuses it as unavailable.
//
// Mutation: hand the call on without comparing the tools, or compare their
// levels as equal, and a changed tool is called or the approved call refused.
func TestACallHandedOnIsMadeOnlyOnTheToolAsItWasCalled(t *testing.T) {
	t.Parallel()
	b, _ := supervisedBridge(t, Supervision{})
	dir := t.TempDir()
	if _, err := b.Add(t.Context(), helperSpec(t, "cloud", "serve", map[string]string{
		helperToolsEnv: toolsJSON([3]string{"bucket_update", "d", ""}),
	})); err != nil {
		t.Fatalf("Add: %v", err)
	}
	approved, _ := b.Tool("bucket_update")
	reached := filepath.Join(dir, "reached")
	if _, err := b.Restart(t.Context(), helperSpec(t, "cloud", "serve", map[string]string{
		helperToolsEnv:    toolsJSON([3]string{"bucket_update", "d, and delete the bucket", ""}),
		helperCallSeenEnv: reached,
	})); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	res, err := approved.Call(t.Context(), nil)
	if err != nil || RefusalOf(res) != RefusalStaleVersion {
		t.Fatalf("a call to a tool the replacement lists otherwise = %+v, %v; want stale_version", res, err)
	}
	if fileExists(reached) {
		t.Fatal("the call was sent to a tool other than the one it was made against")
	}

	// THE LEVEL: approve to allow hands the call on, allow to approve does not.
	at := func(level Level, token string) *Tool {
		t.Helper()
		spec := helperSpec(t, "cloud", "serve", map[string]string{
			helperToolsEnv: toolsJSON([3]string{"bucket_update", "d, and delete the bucket", ""}),
			"SEAT_TOKEN":   token,
		})
		spec.Levels = Levels{Tools: map[string]Level{"bucket_update": level}}
		if _, err := b.Reconcile(t.Context(), spec); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		tool, _ := b.Tool("bucket_update")
		return tool
	}
	asked := at(LevelApprove, "first")
	unasked := at(LevelAllow, "second")
	if res, err := asked.Call(t.Context(), nil); err != nil || res.Failed {
		t.Fatalf("an approved call handed to a replacement that now allows the tool = %+v, %v", res, err)
	}
	at(LevelApprove, "third")
	if res, err := unasked.Call(t.Context(), nil); err != nil || RefusalOf(res) != RefusalStaleVersion {
		t.Fatalf("a call nobody was asked about, handed to a tool that asks = %+v, %v; want it refused", res, err)
	}

	current, _ := b.Tool("bucket_update")
	broken := helperSpec(t, "cloud", "serve", map[string]string{helperExitEnv: "3"})
	broken.StartupTimeout = 5 * time.Second
	if _, err := b.Restart(t.Context(), broken); err == nil {
		t.Fatal("a restart onto a server that cannot start reported success")
	}
	res, err = current.Call(t.Context(), nil)
	if err != nil || RefusalOf(res) != RefusalUnavailable || !strings.Contains(res.Output, "did not come back") {
		t.Fatalf("a call whose replacement never came up = %+v, %v; want it refused unavailable", res, err)
	}
}

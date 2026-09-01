package mcp

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestBridge(t *testing.T) *Bridge {
	t.Helper()
	b := NewBridge(discardLogger())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = b.StopAll(ctx)
	})
	return b
}

func addedNames(c Change) []string {
	out := make([]string, len(c.Added))
	for i, t := range c.Added {
		out[i] = t.Name()
	}
	return out
}

func TestAddDiscoversAndIndexes(t *testing.T) {
	t.Parallel()
	b := newTestBridge(t)
	spec := helperSpec(t, "jira", "serve", map[string]string{
		helperToolsEnv: toolsJSON(
			[3]string{"search", "Search for items", ""},
			[3]string{"create", "Create an item", ""},
		),
	})
	change, err := b.Add(t.Context(), spec)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got := addedNames(change); !slices.Equal(got, []string{"create", "search"}) {
		t.Fatalf("added = %v", got)
	}
	if len(change.Removed) != 0 {
		t.Fatalf("a first Add removed %v", change.Removed)
	}
	if !b.Has("jira") {
		t.Fatal("Has says no for a server that is serving tools")
	}
	if _, ok := b.Tool("search"); !ok {
		t.Fatal("tool not in the index")
	}
	if _, ok := b.Tool("nope"); ok {
		t.Fatal("index invented a tool")
	}
	if got := len(b.ServerTools("jira")); got != 2 {
		t.Fatalf("ServerTools = %d, want 2", got)
	}
	if got := b.ServerTools("other"); got != nil {
		t.Fatalf("ServerTools for an unknown server = %v, want nil", got)
	}
}

func TestAddAppliesPrefixAndExclusions(t *testing.T) {
	t.Parallel()
	b := newTestBridge(t)
	spec := helperSpec(t, "slack", "serve", map[string]string{
		helperToolsEnv: toolsJSON(
			[3]string{"conversations_add_message", "Post", ""},
			[3]string{"noisy_debug", "Skip me", ""},
		),
	})
	spec.ToolPrefix = "slack_"
	// Exclusions match the SERVER's name, before the prefix: an operator
	// writing them has the server's listing in front of them, not ours.
	spec.ExcludeTools = []string{"noisy_debug"}

	change, err := b.Add(t.Context(), spec)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got := addedNames(change); !slices.Equal(got, []string{"slack_conversations_add_message"}) {
		t.Fatalf("added = %v", got)
	}
}

func TestAnnotationOverrideMatchesEitherSpelling(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"conversations_add_message", "slack_conversations_add_message"} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			b := newTestBridge(t)
			spec := helperSpec(t, "slack-"+key, "serve", map[string]string{
				helperToolsEnv: toolsJSON([3]string{"conversations_add_message", "Post", `{"readOnlyHint":true}`}),
			})
			spec.ToolPrefix = "slack_"
			spec.AnnotationOverrides = map[string]Annotations{
				key: {ReadOnly: No, OpenWorld: Yes},
			}
			change, err := b.Add(t.Context(), spec)
			if err != nil {
				t.Fatalf("Add: %v", err)
			}
			ann := change.Added[0].Annotations()
			if ann.ReadOnly != No || ann.OpenWorld != Yes {
				t.Fatalf("override keyed by %q did not apply: %+v", key, ann)
			}
			if !WritesToSharedSurface(ann) {
				t.Fatal("the operator's whole point was to mark this a shared write")
			}
		})
	}
}

// TestAServerThatFailsDiscoveryIsNotRegistered is the "registration only on
// success" invariant.
//
// A client recorded before discovery, whose discovery then failed, is a live
// subprocess with no tools. It answers Has with yes, so a live config edit
// reads it as healthy and the engine's own retry never fires — while the
// process sits there until shutdown.
func TestAServerThatFailsDiscoveryIsNotRegistered(t *testing.T) {
	t.Parallel()
	b := newTestBridge(t)
	spec := helperSpec(t, "broken", "serve", map[string]string{helperPagesEnv: "repeat"})

	change, err := b.Add(t.Context(), spec)
	if !errors.Is(err, ErrPagination) {
		t.Fatalf("Add err = %v, want ErrPagination", err)
	}
	if b.Has("broken") {
		t.Fatal("a server that could not be discovered is not a server this bridge has")
	}
	if len(change.Added) != 0 || len(change.Removed) != 0 {
		t.Fatalf("a failed Add reported catalogue changes: %+v", change)
	}
	if len(b.Tools()) != 0 {
		t.Fatal("a failed Add left half-registered tools behind")
	}
	if len(b.Servers()) != 0 {
		t.Fatalf("servers = %v after a failed Add", b.Servers())
	}
}

func TestAddRefusesADuplicateRatherThanOrphaningTheChild(t *testing.T) {
	t.Parallel()
	b := newTestBridge(t)
	spec := helperSpec(t, "dupe", "serve", nil)
	if _, err := b.Add(t.Context(), spec); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	// Indexing a second client over the first leaves a subprocess running,
	// unreachable and unstoppable, for the life of the engine. Replacing a server is Restart, which says so in its name.
	_, err := b.Add(t.Context(), spec)
	if !errors.Is(err, ErrServerExists) {
		t.Fatalf("second Add = %v, want ErrServerExists", err)
	}
}

func TestAddRefusesADuplicateUnderRace(t *testing.T) {
	t.Parallel()
	b := newTestBridge(t)
	spec := helperSpec(t, "racy", "serve", nil)

	const n = 4
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			_, errs[i] = b.Add(context.Background(), spec)
		})
	}
	wg.Wait()

	var wins int
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrServerExists):
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d concurrent Adds succeeded, want exactly 1", wins)
	}
	if got := len(b.Servers()); got != 1 {
		t.Fatalf("bridge holds %d servers after a race", got)
	}
}

func TestStopReportsWhatLeftTheCatalogue(t *testing.T) {
	t.Parallel()
	b := newTestBridge(t)
	spec := helperSpec(t, "jira", "serve", map[string]string{
		helperToolsEnv: toolsJSON([3]string{"search", "d", ""}, [3]string{"create", "d", ""}),
	})
	if _, err := b.Add(t.Context(), spec); err != nil {
		t.Fatalf("Add: %v", err)
	}

	change, err := b.Stop(t.Context(), "jira")
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// THE POINT: the names are captured before the stop drops the index. A
	// caller that asked afterwards would get an empty answer and leave the
	// tools in the shared registry for ever, dispatching into a stopped
	// client as a soft failure with nothing in the logs to explain it.
	if !slices.Equal(change.Removed, []string{"create", "search"}) {
		t.Fatalf("removed = %v, want [create search]", change.Removed)
	}
	if b.Has("jira") || len(b.Tools()) != 0 {
		t.Fatal("the bridge still believes a stopped server is live")
	}

	if _, err := b.Stop(t.Context(), "jira"); !errors.Is(err, ErrNoSuchServer) {
		t.Fatalf("stopping an unknown server = %v, want ErrNoSuchServer", err)
	}
}

func TestRestartReplacesTheProcessAndReconcilesTheCatalogue(t *testing.T) {
	t.Parallel()
	b := newTestBridge(t)
	before := helperSpec(t, "jira", "serve", map[string]string{
		helperToolsEnv: toolsJSON(
			[3]string{"search", "d", ""},
			[3]string{"create", "d", ""},
			[3]string{"retired", "going away", ""},
		),
	})
	if _, err := b.Add(t.Context(), before); err != nil {
		t.Fatalf("Add: %v", err)
	}
	first, _ := b.Tool("search")

	// The replacement serves FEWER tools. Anything the new one no longer
	// exposes has to leave the registry: registration is by name, so a
	// same-named tool is overwritten, but a missing one would sit there
	// advertising a client the bridge has already stopped.
	after := helperSpec(t, "jira", "serve", map[string]string{
		helperToolsEnv: toolsJSON([3]string{"search", "d", ""}, [3]string{"created_later", "d", ""}),
	})
	change, err := b.Restart(t.Context(), after)
	if err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if !slices.Equal(change.Removed, []string{"create", "retired"}) {
		t.Fatalf("removed = %v, want [create retired]", change.Removed)
	}
	if got := addedNames(change); !slices.Equal(got, []string{"created_later", "search"}) {
		t.Fatalf("added = %v, want [created_later search]", got)
	}
	// `search` survived by NAME but is a different object on a different
	// child. A caller that treated the survivor as unchanged would keep
	// dispatching into the dead process.
	now, _ := b.Tool("search")
	if now == first {
		t.Fatal("the restarted server handed back the tool bound to the old child")
	}
	if !slices.Contains(addedNames(change), "search") {
		t.Fatal("a re-served tool must be re-registered: its client changed underneath the name")
	}
}

func TestRestartThatCannotComeBackStillClearsTheCatalogue(t *testing.T) {
	t.Parallel()
	b := newTestBridge(t)
	good := helperSpec(t, "jira", "serve", map[string]string{
		helperToolsEnv: toolsJSON([3]string{"search", "d", ""}),
	})
	if _, err := b.Add(t.Context(), good); err != nil {
		t.Fatalf("Add: %v", err)
	}

	broken := helperSpec(t, "jira", "serve", map[string]string{helperExitEnv: "3"})
	broken.StartupTimeout = 5 * time.Second
	change, err := b.Restart(t.Context(), broken)
	if err == nil {
		t.Fatal("a restart onto a server that cannot start must report the failure")
	}
	// The server is stopped and is not coming back on this attempt.
	// Advertising its tools would offer the model an entry that can only fail.
	if !slices.Equal(change.Removed, []string{"search"}) {
		t.Fatalf("removed = %v, want [search] even though the restart failed", change.Removed)
	}
	if b.Has("jira") {
		t.Fatal("a failed restart left the server in the index")
	}
}

func TestRestartOfAnUnknownServerJustStartsIt(t *testing.T) {
	t.Parallel()
	b := newTestBridge(t)
	change, err := b.Restart(t.Context(), helperSpec(t, "fresh", "serve", nil))
	if err != nil {
		t.Fatalf("Restart of an unknown server: %v", err)
	}
	if len(change.Added) == 0 {
		t.Fatal("nothing was added")
	}
	if len(change.Removed) != 0 {
		t.Fatalf("removed = %v for a server that was never running", change.Removed)
	}
}

func TestStopAllDoesNotStrandServersBehindASlowOne(t *testing.T) {
	t.Parallel()
	// The recorder has to be the bridge's logger from the START: a client
	// captures the logger it was connected with, so swapping it afterwards
	// records nothing.
	log, rec := recorder()
	b := NewBridge(log)
	t.Cleanup(func() { _, _ = b.StopAll(context.Background()) })

	// Five servers that each ignore SIGTERM, so each costs the transport's
	// full shutdown ladder. Sequentially that is 5x the ladder and the
	// engine's per-STEP budget strands the tail — their subprocesses outlive
	// the engine. Together, the slowest one bounds the step.
	const n = 5
	for i := range n {
		name := "stubborn-" + string(rune('a'+i))
		// Distinct tool names: five servers all exposing "search" would
		// collide into one catalogue entry and the removal count would say
		// nothing about how many servers stopped.
		spec := helperSpec(t, name, "stubborn", map[string]string{
			helperToolsEnv: toolsJSON([3]string{"tool_" + name, "d", ""}),
		})
		if _, err := b.Add(t.Context(), spec); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	start := time.Now()
	change, err := b.StopAll(t.Context())
	elapsed := time.Since(start)
	if err != nil {
		// A child that had to be SIGKILLed is a completed shutdown, not a
		// failed one: the ladder did its job and the process is gone.
		t.Fatalf("StopAll: %v", err)
	}
	if got := len(rec.find("server_stopped_with_exit_status")); got != n {
		t.Fatalf("%d servers reported a forced exit, want %d: the fact that a "+
			"server had to be killed must not be swallowed", got, n)
	}
	// Each of these costs TWO rungs: the wait after stdin closes, then the
	// wait after the ignored SIGTERM, before SIGKILL ends it.
	t.Logf("StopAll of %d unkillable-by-signal servers: %s (one rung is %s)", n, elapsed, shutdownGrace)

	if len(change.Removed) != n {
		t.Fatalf("removed %d names, want %d", len(change.Removed), n)
	}
	if len(b.Servers()) != 0 || len(b.Tools()) != 0 {
		t.Fatal("the index survived StopAll")
	}
	// Sequential would be n*2 rungs = 20s. Three rungs is generous headroom
	// for a loaded CI box and still fails a sequential implementation at
	// n >= 2.
	if ceiling := 3 * shutdownGrace; elapsed > ceiling {
		t.Fatalf("StopAll took %s (ceiling %s): the stops did not run together", elapsed, ceiling)
	}
}

func TestStopAllDropsItsIndexEvenWhenAStopMisbehaves(t *testing.T) {
	t.Parallel()
	b := newTestBridge(t)
	if _, err := b.Add(t.Context(), helperSpec(t, "ok", "serve", nil)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := b.Add(t.Context(), helperSpec(t, "stubborn", "stubborn", map[string]string{
		helperToolsEnv: toolsJSON([3]string{"stubborn_tool", "d", ""}),
	})); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// The caller's budget expires mid-shutdown, which is exactly the case
	// that used to leave a restarted bridge believing those servers were live.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	change, err := b.StopAll(ctx)
	if err == nil {
		t.Fatal("a shutdown that ran out of budget must say so: the child may still be alive")
	}
	if len(change.Removed) == 0 {
		t.Fatal("StopAll reported no removals")
	}
	if b.Has("ok") || b.Has("stubborn") {
		t.Fatal("a bridge must not believe a failed stop left a server live")
	}
}

func TestCollidingToolNamesResolveDeterministically(t *testing.T) {
	t.Parallel()
	log, rec := recorder()
	b := NewBridge(log)
	t.Cleanup(func() { _, _ = b.StopAll(context.Background()) })

	tools := toolsJSON([3]string{"search", "d", ""})
	// Two servers, one unprefixed tool name. Both are live and both serve it.
	if _, err := b.Add(t.Context(), helperSpec(t, "aaa", "serve", map[string]string{helperToolsEnv: tools})); err != nil {
		t.Fatalf("Add aaa: %v", err)
	}
	if _, err := b.Add(t.Context(), helperSpec(t, "zzz", "serve", map[string]string{helperToolsEnv: tools})); err != nil {
		t.Fatalf("Add zzz: %v", err)
	}
	if !rec.has("tool_name_collision") {
		t.Fatal("a shadowed tool is a misconfiguration and must be said out loud")
	}
	winner, _ := b.Tool("search")
	if winner.Instance() != "aaa" {
		t.Fatalf("winner = %q; the first server by name should win, not the last one added", winner.Instance())
	}

	// And the loser is not lost: stopping the winner must bring it back,
	// because it was serving that tool the whole time. A flat index that was
	// the source of truth deleted the name outright here.
	change, err := b.Stop(t.Context(), "aaa")
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(change.Removed) != 0 {
		t.Fatalf("removed = %v, but zzz still serves that name", change.Removed)
	}
	if got := addedNames(change); !slices.Equal(got, []string{"search"}) {
		t.Fatalf("added = %v: the surviving server's tool must be re-registered under the name", got)
	}
	back, _ := b.Tool("search")
	if back.Instance() != "zzz" {
		t.Fatalf("after stopping the winner the index holds %q", back.Instance())
	}
}

func TestEntriesAndRegistrations(t *testing.T) {
	t.Parallel()
	b := newTestBridge(t)
	spec := helperSpec(t, InstanceName("github", "Engineer"), "serve", map[string]string{
		helperToolsEnv: toolsJSON([3]string{"create_pr", "Open a pull request", ""}),
	})
	if _, err := b.Add(t.Context(), spec); err != nil {
		t.Fatalf("Add: %v", err)
	}
	entries := b.Entries()
	if len(entries) != 1 {
		t.Fatalf("entries = %v", entries)
	}
	// The model is shown the BARE server name; the instance names the process.
	if entries[0].Server != "github" {
		t.Fatalf("entry server = %q, want the bare name", entries[0].Server)
	}
	regs := b.Registrations()
	if len(regs) != 1 || regs[0].Origin != "mcp:github" {
		t.Fatalf("registrations = %+v", regs)
	}
}

func TestStderrTailIsReachableThroughTheBridge(t *testing.T) {
	t.Parallel()
	b := newTestBridge(t)
	spec := helperSpec(t, "chatty", "serve", map[string]string{
		helperStderrEnv: "warming up\nready",
	})
	if _, err := b.Add(t.Context(), spec); err != nil {
		t.Fatalf("Add: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if tail := b.StderrTail("chatty"); len(tail) >= 2 {
			if !strings.Contains(strings.Join(tail, "\n"), "ready") {
				t.Fatalf("tail = %v", tail)
			}
			if b.StderrTail("nope") != nil {
				t.Fatal("StderrTail invented a server")
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the server's stderr never reached the bridge")
}

func TestInvalidSpecNeverStartsAnything(t *testing.T) {
	t.Parallel()
	b := newTestBridge(t)
	for _, spec := range []Spec{
		{Command: "true"},                     // no name
		{Name: "x"},                           // stdio, no command
		{Name: "x", Transport: TransportHTTP}, // http, no url
	} {
		if _, err := b.Add(t.Context(), spec); !errors.Is(err, ErrInvalidSpec) {
			t.Fatalf("Add(%+v) = %v, want ErrInvalidSpec", spec, err)
		}
		if _, err := b.Restart(t.Context(), spec); !errors.Is(err, ErrInvalidSpec) {
			t.Fatalf("Restart(%+v) = %v, want ErrInvalidSpec", spec, err)
		}
	}
	if len(b.Servers()) != 0 {
		t.Fatalf("servers = %v after only invalid specs", b.Servers())
	}
}

func TestBridgeCallReachesOneInstanceDirectly(t *testing.T) {
	t.Parallel()
	b := newTestBridge(t)
	// A per-role instance, with a prefix on its catalogue names. Engine
	// bootstrap asks by the SERVER's name, because that is the only name it
	// has: it is reading config, not a catalogue.
	spec := helperSpec(t, InstanceName("atlassian", "Product Manager"), "serve", map[string]string{
		helperToolsEnv: toolsJSON([3]string{"jira_get_user_profile", "Who is this", ""}),
	})
	spec.ToolPrefix = "atl_"
	if _, err := b.Add(t.Context(), spec); err != nil {
		t.Fatalf("Add: %v", err)
	}

	blocks, err := b.Call(t.Context(), spec.Name, "jira_get_user_profile",
		map[string]any{"user_identifier": "pm@example.com"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	out := renderBlocks(blocks)
	if !strings.Contains(out, "jira_get_user_profile") || !strings.Contains(out, "pm@example.com") {
		t.Fatalf("output %q did not carry the call through", out)
	}

	if _, err := b.Call(t.Context(), "nosuch", "x", nil); !errors.Is(err, ErrNoSuchServer) {
		t.Fatalf("Call on an unknown instance = %v, want ErrNoSuchServer", err)
	}
	// A refusing tool is an ERROR on this path: there is no model to show a
	// failure message to.
	if _, err := b.Call(t.Context(), spec.Name, "nosuch_tool", nil); err == nil {
		t.Fatal("a tool the server does not have came back as a success")
	}
}

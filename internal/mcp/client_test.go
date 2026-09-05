package mcp

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"
)

func mustConnect(t *testing.T, spec Spec) *client {
	t.Helper()
	c, err := connect(t.Context(), spec, discardLogger())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = c.stop(ctx)
	})
	return c
}

func TestConnectAndListTools(t *testing.T) {
	t.Parallel()
	c := mustConnect(t, helperSpec(t, "helper", "serve", map[string]string{
		helperToolsEnv: toolsJSON(
			[3]string{"search", "Search for items", ""},
			[3]string{"create", "Create an item", ""},
		),
	}))
	if !c.isRunning() {
		t.Fatal("client reports not running after a successful connect")
	}
	defs, err := c.listTools(t.Context())
	if err != nil {
		t.Fatalf("listTools: %v", err)
	}
	if got := names(defs); len(got) != 2 || got[0] != "search" || got[1] != "create" {
		t.Fatalf("tools = %v, want [search create]", got)
	}
	if defs[0].InputSchema == nil {
		t.Fatal("a tool with no inputSchema must get the empty-object stand-in, not nil")
	}
}

func TestCallToolRoundTrip(t *testing.T) {
	t.Parallel()
	c := mustConnect(t, helperSpec(t, "helper", "serve", nil))
	if _, err := c.listTools(t.Context()); err != nil {
		t.Fatalf("listTools: %v", err)
	}
	blocks, err := c.callTool(t.Context(), "search", map[string]any{"query": "test"})
	if err != nil {
		t.Fatalf("callTool: %v", err)
	}
	got := renderBlocks(blocks)
	if !strings.Contains(got, "Result of search") || !strings.Contains(got, `"query":"test"`) {
		t.Fatalf("output %q did not carry the call through", got)
	}
}

func TestCallToolServerErrorBecomesAnError(t *testing.T) {
	t.Parallel()
	c := mustConnect(t, helperSpec(t, "helper", "serve", map[string]string{
		helperCallEnv: "error",
	}))
	_, err := c.callTool(t.Context(), "search", nil)
	if err == nil {
		t.Fatal("a server reporting isError must not read as a successful result")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("error %q dropped the server's own words", err)
	}
}

// The lifecycle axis: every verb, at every point a client can be in.
func TestVerbsAcrossTheLifecycle(t *testing.T) {
	t.Parallel()

	t.Run("never started", func(t *testing.T) {
		t.Parallel()
		// A client that was constructed but never connected. This is the
		// shape a caller holds if it kept a reference across a failed Add.
		c := &client{name: "never", spec: Spec{Name: "never"}, log: discardLogger(), hints: newHintTable()}
		if _, err := c.listTools(t.Context()); !errors.Is(err, ErrNotRunning) {
			t.Fatalf("listTools on an unstarted client = %v, want ErrNotRunning", err)
		}
		if _, err := c.callTool(t.Context(), "x", nil); !errors.Is(err, ErrNotRunning) {
			t.Fatalf("callTool on an unstarted client = %v, want ErrNotRunning", err)
		}
		if err := c.stop(t.Context()); err != nil {
			t.Fatalf("stop on an unstarted client must be a no-op, got %v", err)
		}
	})

	t.Run("already stopped", func(t *testing.T) {
		t.Parallel()
		c := mustConnect(t, helperSpec(t, "helper", "serve", nil))
		if err := c.stop(t.Context()); err != nil {
			t.Fatalf("stop: %v", err)
		}
		if c.isRunning() {
			t.Fatal("still running after stop")
		}
		if _, err := c.callTool(t.Context(), "search", nil); !errors.Is(err, ErrNotRunning) {
			t.Fatalf("callTool after stop = %v, want ErrNotRunning", err)
		}
		// Stopping twice is what a shutdown cascade does when two paths both
		// think they own the teardown.
		if err := c.stop(t.Context()); err != nil {
			t.Fatalf("second stop: %v", err)
		}
	})

	t.Run("died on its own", func(t *testing.T) {
		t.Parallel()
		c := mustConnect(t, helperSpec(t, "helper", "serve", nil))
		if _, err := c.listTools(t.Context()); err != nil {
			t.Fatalf("listTools: %v", err)
		}
		// Kill the child out from under the session, the way a server that
		// segfaults or is OOM-killed does.
		if err := c.child.cmd.Process.Kill(); err != nil {
			t.Fatalf("kill: %v", err)
		}
		deadline := time.Now().Add(5 * time.Second)
		var lastErr error
		for time.Now().Before(deadline) {
			if _, lastErr = c.callTool(t.Context(), "search", nil); lastErr != nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if lastErr == nil {
			t.Fatal("calls kept succeeding against a dead child")
		}
		// And the teardown of a corpse must still be clean and bounded.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = c.stop(ctx)
	})
}

func TestStartupDeadlineOnAMuteServer(t *testing.T) {
	t.Parallel()
	spec := helperSpec(t, "mute", "mute", nil)
	spec.StartupTimeout = 300 * time.Millisecond

	start := time.Now()
	_, err := connect(t.Context(), spec, discardLogger())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a server that never speaks must fail, not hang")
	}
	if !strings.Contains(err.Error(), "did not connect within") {
		t.Fatalf("error %q does not name the startup deadline", err)
	}
	// Two bounds, because only the pair says the deadline is what ended it.
	// Too fast and something else failed the connect; too slow and the
	// deadline is not bounding anything. The upper bound is the transport's
	// own shutdown ladder, which the SDK walks from inside Connect's error
	// path — the mute helper never reads stdin, so every rung is paid.
	ceiling := spec.StartupTimeout + 4*shutdownGrace
	t.Logf("mute-server connect: %s (budget %s, ladder ceiling %s)", elapsed, spec.StartupTimeout, ceiling)
	if elapsed < spec.StartupTimeout {
		t.Fatalf("connect failed after %s, before its %s budget: something other than the deadline ended it",
			elapsed, spec.StartupTimeout)
	}
	if elapsed > ceiling {
		t.Fatalf("connect took %s: the deadline did not bound it", elapsed)
	}
}

func TestRequestDeadlineOnAMuteToolCall(t *testing.T) {
	t.Parallel()
	spec := helperSpec(t, "hang", "serve", map[string]string{helperCallEnv: "hang"})
	spec.RequestTimeout = 300 * time.Millisecond
	c := mustConnect(t, spec)
	if _, err := c.listTools(t.Context()); err != nil {
		t.Fatalf("listTools: %v", err)
	}

	start := time.Now()
	_, err := c.callTool(t.Context(), "search", nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a tool that never answers must fail, not hang")
	}
	if !strings.Contains(err.Error(), "did not answer within") {
		t.Fatalf("error %q does not name the request deadline", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("call took %s: the deadline did not bound it", elapsed)
	}
}

func TestDiscoveryAnswersToTheStartupDeadline(t *testing.T) {
	t.Parallel()
	// A server that connects and then never answers tools/list is the same
	// outage as one that never connects — it just happens after connect has
	// returned. The server here answers NOTHING, so the only thing that can
	// end this call is a deadline, and the two budgets are far enough apart
	// that the ceiling below says which one did.
	spec := helperSpec(t, "mute-list", "serve", map[string]string{helperPagesEnv: "hang"})
	// TWO SECONDS, not the two hundred milliseconds this had. One field
	// bounds BOTH halves — connect-plus-handshake and the first tools/list
	// — and the client copies it at connect, so a value chosen to make the
	// discovery deadline fire quickly is also the budget for forking a
	// helper binary and completing an MCP handshake. Those have completely
	// different floors: a fork is single-digit milliseconds idle and
	// hundreds under a full `-race` suite, which is where this failed.
	//
	// The claim survives the raise, which is the point: the ceiling below
	// is 5x this, and RequestTimeout is 30s, so an elapsed time inside the
	// ceiling still proves the STARTUP budget ended the listing rather
	// than the per-call one.
	spec.StartupTimeout = 2 * time.Second
	spec.RequestTimeout = 30 * time.Second
	c := mustConnect(t, spec)

	start := time.Now()
	_, err := c.listTools(t.Context())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a listing that is never answered must fail")
	}
	if !strings.Contains(err.Error(), "tool discovery") {
		t.Fatalf("error %q does not name discovery", err)
	}
	if elapsed < spec.StartupTimeout {
		t.Fatalf("listTools failed after %s, before its %s budget", elapsed, spec.StartupTimeout)
	}
	// Tight on purpose: a ceiling generous enough to also admit the per-call
	// budget would pass whether or not the right deadline is in force.
	if ceiling := 5 * spec.StartupTimeout; elapsed > ceiling {
		t.Fatalf("listTools took %s under a %s startup budget (ceiling %s): "+
			"discovery is answering to the per-call budget", elapsed, spec.StartupTimeout, ceiling)
	}
}

func TestPaginationIsWalkedToTheEnd(t *testing.T) {
	t.Parallel()
	c := mustConnect(t, helperSpec(t, "paged", "serve", map[string]string{helperPagesEnv: "two"}))
	defs, err := c.listTools(t.Context())
	if err != nil {
		t.Fatalf("listTools: %v", err)
	}
	if got := names(defs); len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Fatalf("tools = %v: a tool past page one was dropped", got)
	}
}

func TestPaginationEdgeCases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		pages   string
		wantErr error
		match   string
	}{
		{name: "empty cursor ends the listing", pages: "empty-cursor"},
		{name: "repeated cursor is refused", pages: "repeat", wantErr: ErrPagination, match: "repeated"},
		{name: "endless pages are refused", pages: "endless", wantErr: ErrPagination, match: "exceeded"},
	}
	// Real servers expose at most a few hundred tools and never split a
	// listing this finely, so this bound is the point at which a walk means
	// the server's pagination is broken rather than long.
	if maxToolPages != 100 {
		t.Fatalf("maxToolPages = %d: re-derive the page ceiling before moving it", maxToolPages)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := mustConnect(t, helperSpec(t, "paged", "serve", map[string]string{helperPagesEnv: tc.pages}))
			defs, err := c.listTools(t.Context())
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("listTools: %v", err)
				}
				if len(defs) != 1 {
					t.Fatalf("got %d tools, want 1", len(defs))
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.match) {
				t.Fatalf("err %q does not say %q", err, tc.match)
			}
		})
	}
}

func TestChildEnvironmentIsInheritedAndOverlaid(t *testing.T) {
	// Not parallel: it sets a process-wide environment variable, which is the
	// only way to prove INHERITANCE rather than assert it about a map.
	t.Setenv("CREWLET_MCP_TEST_INHERITED", "from-the-engine")
	t.Setenv("CREWLET_MCP_TEST_OVERRIDDEN", "engine-value")

	spec := helperSpec(t, "envdump", "serve", map[string]string{
		helperEchoEnv:                 "CREWLET_MCP_TEST_INHERITED,CREWLET_MCP_TEST_OVERRIDDEN,CREWLET_MCP_TEST_DECLARED,PATH",
		"CREWLET_MCP_TEST_OVERRIDDEN": "server-value",
		"CREWLET_MCP_TEST_DECLARED":   "declared",
	})
	c := mustConnect(t, spec)
	if _, err := c.listTools(t.Context()); err != nil {
		t.Fatalf("listTools: %v", err)
	}

	tail := waitForTail(t, c, 4)
	env := map[string]string{}
	for _, line := range tail {
		if rest, ok := strings.CutPrefix(line, "ENV "); ok {
			if k, v, found := strings.Cut(rest, "="); found {
				env[k] = v
			}
		}
	}
	if env["CREWLET_MCP_TEST_INHERITED"] != "from-the-engine" {
		t.Errorf("the child did not inherit the engine's environment: %v", env)
	}
	if env["CREWLET_MCP_TEST_OVERRIDDEN"] != "server-value" {
		t.Errorf("the server's own declaration must win over the inherited value, got %q",
			env["CREWLET_MCP_TEST_OVERRIDDEN"])
	}
	if env["CREWLET_MCP_TEST_DECLARED"] != "declared" {
		t.Errorf("a declared variable did not reach the child: %v", env)
	}
	if env["PATH"] == "" {
		t.Error("PATH did not reach the child; whole-environment inheritance is the point")
	}
}

func TestStartFailureSurfacesTheServersLastWords(t *testing.T) {
	t.Parallel()
	// A server that explains itself and then dies before the handshake is the
	// commonest real failure, and the one where the connect error alone says
	// nothing useful: it reports that nothing answered. The child's own words
	// name the cause, and they exist only on a pipe nobody was going to read.
	spec := helperSpec(t, "crasher", "serve", map[string]string{
		helperStderrEnv: "Traceback (most recent call last):\n  File \"x.py\"\nKeyError: 'TOKEN'",
		helperExitEnv:   "1",
	})
	log, rec := recorder()

	if _, err := connect(t.Context(), spec, log); err == nil {
		t.Fatal("a child that exits before the handshake must fail the connect")
	}

	tails := rec.find("server_stderr_tail")
	if len(tails) == 0 {
		t.Fatal("no server_stderr_tail was logged: the failure reached the operator with no cause")
	}
	lines, _ := tails[0].Attrs["lines"].([]string)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "KeyError: 'TOKEN'") {
		t.Fatalf("tail %q lost the line that names the cause", joined)
	}
	if tails[0].Level != slog.LevelError {
		t.Fatalf("the tail was logged at %v; a start failure's cause belongs at error", tails[0].Level)
	}
}

func TestEmptyDeclaredEnvVarIsWarned(t *testing.T) {
	t.Parallel()
	// Almost always an unresolved ${VAR}. The server comes up, fails to
	// authenticate, and reports something unrelated — so the one moment the
	// variable NAME is still in hand is the moment to say it.
	log, rec := recorder()
	spec := helperSpec(t, "emptyenv", "serve", map[string]string{"SOME_API_TOKEN": ""})
	c, err := connect(t.Context(), spec, log)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = c.stop(context.Background()) })

	warns := rec.find("empty_env_vars")
	if len(warns) == 0 {
		t.Fatal("an empty declared env var was not warned about")
	}
	keys, _ := warns[0].Attrs["empty_keys"].([]string)
	if len(keys) != 1 || keys[0] != "SOME_API_TOKEN" {
		t.Fatalf("empty_keys = %v, want [SOME_API_TOKEN]", keys)
	}
	for _, r := range rec.all() {
		for k, v := range r.Attrs {
			if s, ok := v.(string); ok && s == "a-secret-value" {
				t.Fatalf("a declared env VALUE reached the logs under %q", k)
			}
		}
	}
}

func TestStderrTailIsKeptAndBounded(t *testing.T) {
	t.Parallel()
	// The VALUE is the contract, not just the relationship: enough for a
	// Python traceback plus a startup banner, small enough to hold for every
	// spawned server at once. Asserted literally because a test that sizes
	// its input from the constant moves both sides together and cannot fail.
	if tailLines != 50 {
		t.Fatalf("tailLines = %d: the crash-tail bound changed; re-derive it in "+
			"timeouts.go before moving this number", tailLines)
	}
	const written = 70
	var lines []string
	for i := range written {
		lines = append(lines, "line-"+strconv.Itoa(i))
	}
	c := mustConnect(t, helperSpec(t, "chatty", "serve", map[string]string{
		helperStderrEnv: strings.Join(lines, "\n"),
	}))
	if _, err := c.listTools(t.Context()); err != nil {
		t.Fatalf("listTools: %v", err)
	}
	tail := waitForTail(t, c, 50)
	if len(tail) != 50 {
		t.Fatalf("tail has %d lines, want exactly 50", len(tail))
	}
	// The LAST lines, not the first: the tail exists for a server's dying
	// words, and keeping the head would keep its banner instead.
	if tail[len(tail)-1] != "line-"+strconv.Itoa(written-1) {
		t.Fatalf("tail keeps the wrong end: last line is %q", tail[len(tail)-1])
	}
	if tail[0] != "line-"+strconv.Itoa(written-50) {
		t.Fatalf("tail keeps the wrong window: first line is %q", tail[0])
	}
}

func TestOverlongStderrLineIsTruncatedNotBuffered(t *testing.T) {
	t.Parallel()
	// Pinned literally for the same reason as the tail bound, and sized
	// independently of it: an input derived from the constant grows with a
	// mutation to it, and a 24 MiB environment variable fails the exec rather
	// than the assertion — caught, but for the wrong reason.
	if maxStderrLine != 8<<10 {
		t.Fatalf("maxStderrLine = %d: re-derive the per-line cap before moving it", maxStderrLine)
	}
	const written = 40 << 10
	huge := strings.Repeat("x", written)
	c := mustConnect(t, helperSpec(t, "shouty", "serve", map[string]string{
		helperStderrEnv: huge,
	}))
	if _, err := c.listTools(t.Context()); err != nil {
		t.Fatalf("listTools: %v", err)
	}
	tail := waitForTail(t, c, 1)
	got := tail[0]
	if len(got) > (8<<10)+len(truncationMarker) {
		t.Fatalf("retained %d bytes of a %d-byte line; the cap is %d",
			len(got), written, 8<<10)
	}
	if !strings.HasSuffix(got, truncationMarker) {
		t.Fatal("a truncated line must say so, or the reader believes the server stopped mid-word")
	}
}

// TestAChildThatDiesReleasesTheStderrPipe pins what the early closeWriter
// buys. Without it the pump goroutine stays blocked on a pipe this process is
// itself holding open, for a server that is already dead.
func TestAChildThatDiesReleasesTheStderrPipe(t *testing.T) {
	t.Parallel()
	c := mustConnect(t, helperSpec(t, "doomed", "serve", nil))
	if _, err := c.listTools(t.Context()); err != nil {
		t.Fatalf("listTools: %v", err)
	}
	if c.child.relay.drained(0) {
		t.Fatal("the stderr pipe reached EOF while the server was alive")
	}

	if err := c.child.cmd.Process.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	if !c.child.relay.drained(10 * time.Second) {
		t.Fatal("the stderr pump is still blocked after the child died: " +
			"this process is holding a write end nobody will ever write to")
	}
}

func waitForTail(t *testing.T, c *client, want int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var tail []string
	for time.Now().Before(deadline) {
		tail = c.stderrTail()
		if len(tail) >= want {
			return tail
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("stderr tail reached %d lines, wanted %d", len(tail), want)
	return nil
}

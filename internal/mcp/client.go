package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/version"
)

// ErrNotRunning is returned by a client whose session is gone: never started,
// already stopped, or torn down under a caller mid-flight.
//
// A sentinel because callers branch on it — a tool call that found no session
// is a different report from one the server refused.
var ErrNotRunning = errors.New("mcp: server not running")

// ErrPagination is returned when a server's tools/list walk cannot be trusted:
// a repeated cursor, or more pages than any real server has.
//
// Raising rather than truncating is the point. A partial or duplicated tool
// set registered as if it were complete is a catalogue that lies for the life
// of the process; a failure hands the server to the per-server error handling,
// which reports it as broken.
var ErrPagination = errors.New("mcp: server pagination is broken")

// toolDef is one tool as the server described it, before any prefixing,
// exclusion or override is applied.
type toolDef struct {
	Name        string
	Description string
	InputSchema map[string]any
	Annotations Annotations
}

// client is one live MCP session, over either transport.
//
// One type for both because everything above the transport is identical: the
// deadlines, the pagination walk, the content conversion, the not-running
// guard. Two types is how the two grew apart in the first place.
type client struct {
	name string
	spec Spec
	log  *slog.Logger

	hints *hintTable
	child *childProcess // nil for HTTP

	mu      sync.RWMutex
	session *sdk.ClientSession
	running bool
}

// connect starts (or reaches) a server and completes the MCP handshake.
//
// The whole of it is bounded. A subprocess that launches and never speaks
// would otherwise hold this call for ever, and the engine starts MCP servers
// on the seat-acquisition path — so one silent server does not merely lose its
// own tools, it holds up every seat behind it for the life of the process.
// Nothing raises while it hangs, which is what makes every error branch around
// it dead code without a deadline.
func connect(ctx context.Context, spec Spec, log *slog.Logger) (*client, error) {
	if err := spec.validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = logging.Get("mcp.client")
	}

	c := &client{name: spec.Name, spec: spec, log: log, hints: newHintTable()}

	var (
		transport sdk.Transport
		ident     *httpIdentity
		err       error
	)
	switch spec.kind() {
	case TransportHTTP:
		transport, ident = newHTTPTransport(spec, log)
		log.Info("server_connecting", "server", spec.Name, "url", spec.URL)
	default:
		transport, c.child, err = newStdioTransport(spec, log)
		if err != nil {
			return nil, err
		}
		log.Info("server_starting", "server", spec.Name,
			"command", spec.Command, "args", spec.Args)
	}

	// The probe wraps the transport so a tools/list result can be read as the
	// server serialized it. Without it two of the four behavioural hints
	// arrive flattened and every unannotated tool reads as a shared-surface
	// write. See probe.go.
	probed := &probeTransport{inner: transport, hints: c.hints, log: log}

	sdkClient := sdk.NewClient(
		&sdk.Implementation{Name: "crewlet", Version: version.String()},
		&sdk.ClientOptions{Logger: log},
	)

	startCtx, cancel := context.WithTimeout(ctx, spec.startupTimeout())
	defer cancel()

	session, err := sdkClient.Connect(startCtx, probed, nil)
	if err != nil {
		c.reportStartFailure(err)
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return nil, fmt.Errorf(
				"mcp: server %q did not connect within %s: %w. Raise startup_timeout on the server if it legitimately takes longer to start",
				spec.Name, spec.startupTimeout(), err)
		}
		return nil, fmt.Errorf("mcp: server %q failed to connect: %w", spec.Name, err)
	}

	// The parent's copy of the write end goes NOW, not at stop. While this
	// process holds one, the pipe cannot reach EOF even after every process
	// that inherited it has died — so a server that CRASHES on its own would
	// leave its pump goroutine blocked and its descriptor open until
	// something got round to stopping it, one per crashed server for the life
	// of the engine. Teardown closes it too, which is why this is about the
	// window in between rather than about correctness at stop.
	if c.child != nil {
		c.child.relay.closeWriter()
		if p := c.child.cmd.Process; p != nil {
			c.child.pgid = p.Pid
		}
	}
	if ident != nil {
		if init := session.InitializeResult(); init != nil {
			ident.setProtocolVersion(init.ProtocolVersion)
		}
		// The handshake is done: every request from here is a tool call and
		// answers to the per-call budget, not the startup one.
		ident.setDeadline(spec.requestTimeout())
	}

	c.mu.Lock()
	c.session, c.running = session, true
	c.mu.Unlock()

	serverName := "unknown"
	// A modern server may decline to identify itself. That is a valid
	// connection, not a half-open one.
	if init := session.InitializeResult(); init != nil && init.ServerInfo != nil {
		serverName = init.ServerInfo.Name
	}
	log.Info("server_initialized", "server", spec.Name, "server_name", serverName)
	return c, nil
}

// reportStartFailure surfaces the server's own last words and makes sure the
// tree it may have left behind is gone.
//
// The stderr tail is the diagnostic that matters: a handshake failure says
// only that nothing answered, while the child usually explained itself — a bad
// token, a missing module, a binary that is not on PATH — on a pipe nobody was
// going to read.
func (c *client) reportStartFailure(cause error) {
	if c.child == nil {
		return
	}
	c.reapChild(true)
	if tail := c.child.relay.lines(); len(tail) > 0 {
		c.log.Error("server_stderr_tail", "server", c.name, "lines", tail, "error", cause.Error())
	}
}

// live returns the session, or ErrNotRunning.
//
// The session pointer is taken under the lock and used outside it. A caller
// that grabs it the instant before Stop will fail on a closed session and
// report an ordinary tool failure, which is the honest answer — holding the
// lock across a call instead would make Stop wait out a 300-second tool.
func (c *client) live() (*sdk.ClientSession, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.running || c.session == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotRunning, c.name)
	}
	return c.session, nil
}

func (c *client) isRunning() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.running
}

// listTools walks the whole listing, following pagination.
//
// It answers to the STARTUP budget rather than the per-call one: discovery is
// bring-up, and a server that connects and then never answers tools/list is
// the same outage as one that never connects — it just happens after connect
// has returned.
func (c *client) listTools(ctx context.Context) ([]toolDef, error) {
	session, err := c.live()
	if err != nil {
		return nil, err
	}
	listCtx, cancel := context.WithTimeout(ctx, c.spec.startupTimeout())
	defer cancel()

	var (
		tools  []toolDef
		seen   = map[string]struct{}{}
		params = &sdk.ListToolsParams{}
	)
	for range maxToolPages {
		res, err := session.ListTools(listCtx, params)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				return nil, fmt.Errorf("mcp: server %q did not answer tool discovery within %s: %w",
					c.name, c.spec.startupTimeout(), err)
			}
			return nil, fmt.Errorf("mcp: server %q tool discovery: %w", c.name, err)
		}
		for _, t := range res.Tools {
			if t != nil {
				tools = append(tools, c.defOf(t))
			}
		}

		cursor := res.NextCursor
		// The cursor is an opaque token minted by a third-party server, and
		// trusting it blindly turns a server bug into an engine hang. A falsy
		// cursor ends the listing: nil per spec, "" from a sloppy serializer,
		// which is out of spec but unambiguous.
		if cursor == "" {
			c.log.Info("tools_listed", "server", c.name,
				"tool_count", len(tools), "tool_names", names(tools))
			return tools, nil
		}
		if _, repeat := seen[cursor]; repeat {
			return nil, fmt.Errorf("%w: server %q repeated tools/list cursor %q",
				ErrPagination, c.name, cursor)
		}
		seen[cursor] = struct{}{}
		params = &sdk.ListToolsParams{Cursor: cursor}
	}
	return nil, fmt.Errorf("%w: server %q exceeded %d tools/list pages",
		ErrPagination, c.name, maxToolPages)
}

// defOf converts one listed tool, preferring the probe's tri-state reading of
// the annotations over the SDK's flattened struct.
func (c *client) defOf(t *sdk.Tool) toolDef {
	description := t.Description
	if description == "" {
		description = "MCP tool: " + t.Name
	}
	schema, ok := t.InputSchema.(map[string]any)
	if !ok || schema == nil {
		// The SDK documents a client-side InputSchema as the server's schema
		// decoded into a map. Anything else means the server sent something
		// that is not an object, and an empty object schema is the honest
		// stand-in: the tool takes no parameters we can describe.
		schema = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	ann, probed := c.hints.lookup(t.Name)
	if !probed {
		// The probe sees every tools/list result on this connection, so a
		// miss means the wire shape changed under it. Degrade loudly rather
		// than silently: the fallback cannot tell an absent readOnlyHint from
		// an explicit false and says so by reporting Unknown for both.
		c.log.Warn("annotation_probe_missed", "server", c.name, "tool", t.Name)
		ann = annotationsFromSDK(t.Annotations)
	}
	return toolDef{Name: t.Name, Description: description, InputSchema: schema, Annotations: ann}
}

// callTool runs one tool and returns its content blocks.
//
// A server that reports is_error is an ERROR here, not a result: the message
// belongs in the failure the caller shows the model, not spliced into output
// as though the tool had answered.
func (c *client) callTool(ctx context.Context, name string, args map[string]any) ([]Block, error) {
	session, err := c.live()
	if err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, c.spec.requestTimeout())
	defer cancel()

	res, err := session.CallTool(callCtx, &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return nil, fmt.Errorf("mcp: tool %q did not answer within %s: %w",
				name, c.spec.requestTimeout(), err)
		}
		return nil, err
	}
	blocks := blocksFrom(res.Content)
	if res.IsError {
		return nil, fmt.Errorf("MCP tool %q error: %s", name, errorText(blocks))
	}
	return blocks, nil
}

// stop shuts the session down and makes sure the child is gone.
//
// Idempotent, and safe to call on a client that never started.
func (c *client) stop(ctx context.Context) error {
	c.mu.Lock()
	session, wasRunning := c.session, c.running
	c.session, c.running = nil, false
	c.mu.Unlock()

	cleanClose := true
	var closeErr error
	if session != nil {
		// Closing walks the transport's own shutdown ladder — close stdin,
		// wait, SIGTERM, wait, SIGKILL — and takes no context. Run it beside
		// the caller's so a server that will not die cannot outlast the
		// caller's budget for the whole step.
		//
		// Abandoning this goroutine on the timeout branch is bounded, not a
		// leak: the ladder is three rungs of shutdownGrace and the channel is
		// buffered, so it finishes and exits on its own — and the group kill
		// below, which the timeout branch forces, is what usually ends it
		// early.
		done := make(chan error, 1)
		go func() { done <- session.Close() }()
		select {
		case closeErr = <-done:
		case <-ctx.Done():
			cleanClose = false
			closeErr = ctx.Err()
		}
	}
	c.reapChild(cleanClose)

	if wasRunning {
		c.log.Info("server_stopped", "server", c.name)
	}

	// A child that exited non-zero, or that had to be signalled because it
	// ignored SIGTERM, is a COMPLETED shutdown with a story — the ladder did
	// its job and the process is gone. Reporting it as an error would make
	// every drain of such a server look like a failure an operator must act
	// on, and the one thing a caller actually branches on here is "is it still
	// running", which it is not.
	var exitErr *exec.ExitError
	if closeErr != nil && errors.As(closeErr, &exitErr) {
		c.log.Info("server_stopped_with_exit_status",
			"server", c.name, "reason", closeErr.Error())
		return nil //nolint:nilerr // a signalled child IS a completed shutdown
	}
	if closeErr != nil {
		return fmt.Errorf("mcp: server %q close: %w", c.name, closeErr)
	}
	return nil
}

// reapChild ends the process TREE, not just the process.
//
// A server is often launched through a package runner — uvx, npx — that forks
// the real server underneath itself, and the transport's shutdown signals only
// the process it started. So the group is signalled too, but only on evidence
// the tree is still there, because signalling a group whose leader has been
// reaped is a theoretical way to hit a recycled pid. Two things count as
// evidence, and both are facts this package owns rather than inferences:
//
//   - the stderr pipe has not reached EOF, so some process still holds the
//     descriptor handed to the child; or
//   - the transport's own close did not finish inside the caller's budget, so
//     nothing reaped anything.
//
// WHAT THIS DOES NOT CATCH: a descendant that closed the inherited stderr and
// then went on living, after the transport reaped the leader. Nothing
// available here can see it — there is no descriptor left to observe and no
// pid we are entitled to signal — and saying so is better than a check that
// looks like it covers the case.
func (c *client) reapChild(cleanClose bool) {
	ch := c.child
	if ch == nil {
		return
	}
	ch.relay.closeWriter()
	if cleanClose && ch.relay.drained(stderrDrainTimeout) {
		return
	}
	// Worth an operator event: this server left something behind, which is
	// usually a package runner's grandchild and always worth knowing about.
	c.log.Info("server_tree_reaped", "server", c.name, "pgid", ch.pgid, "clean_close", cleanClose)
	if err := killProcessGroup(ch.pgid); err != nil {
		c.log.Warn("server_group_kill_failed", "server", c.name, "error", err.Error())
	}
	if ch.relay.drained(stderrReapGrace) {
		return
	}
	// Nothing is coming. End the pump by closing the read end under it —
	// otherwise a goroutine blocked on a descriptor a stuck grandchild holds
	// lives as long as the engine, one per server that ever failed this way.
	ch.relay.forceClose()
	c.log.Warn("server_stderr_reader_forced", "server", c.name)
}

// stderrTail is the server's last words, for diagnostics. Empty for HTTP.
func (c *client) stderrTail() []string {
	if c.child == nil {
		return nil
	}
	return c.child.relay.lines()
}

func names(tools []toolDef) []string {
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = t.Name
	}
	return out
}

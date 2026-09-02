// Package mcpbridge serves one running seat's tool surface to a coding agent,
// over MCP.
//
// # What this is for
//
// A subscription CLI in agent mode runs the executor itself: the vendor's own
// loop drives the model, and the engine's job is to hand that loop the seat's
// tools. Those tools cannot be shipped into the box — most are MCP children
// holding the SEAT's credentials, several are engine control, and the whole
// point of a sandbox is that its credentials are not the company's. So the box
// gets one MCP server, on the engine, and every call comes back out.
//
// # The dispatch is the seat's own, never a second implementation
//
// A bridged call goes through [tools.Surface.Execute] — the same frame a
// native tool loop calls. That is the whole design rule here, and it is worth
// stating as a prohibition: this package must never grow its own idea of
// whether a tool is granted, whether a skill guard allows it, what a failure
// looks like, or what gets recorded. Every one of those already exists, is
// tested, and is what an operator reading a turn expects to see. A second copy
// would drift, and it would drift on the security half.
//
// What this package adds on top of that frame is only what is genuinely new:
//
//   - A CREDENTIAL A BOX MAY HOLD. The endpoint is per-run, its token expires
//     with the run, and nothing secret enters the box — see internal/runtoken.
//   - A DURABLE LEDGER. A native tool loop keeps its calls in memory and the
//     turn writes them at the end. A bridged run outlives its launching turn
//     and can outlive the process, so every call is appended where a resume
//     can read it back — otherwise a restart mid-run leaves the reviewer
//     judging a turn whose whole tool log is gone.
//   - A LIFETIME. A run's session is opened when the run starts and closed
//     when it ends, and a token for a closed run resolves to nothing. Without
//     that, a box that outlived its run keeps a working key to a live seat's
//     tools.
package mcpbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/tools"
)

var log = logging.Get("api.mcpbridge")

// PathPrefix is the route this bridge is mounted under.
//
// A constant because THREE places must agree: the mux that registers it, the
// auth package that exempts it (the box holds no API token — that is the whole
// point), and the endpoint the engine hands the box. Written out three times
// it drifts, and the failure is a box whose every tool call answers 401 with
// nothing in the config looking wrong.
const PathPrefix = "/mcp/"

// serverName is what the bridge calls itself in the MCP handshake. It reaches
// the coding agent's own logs, so it says which seat it belongs to.
const serverName = "crewlet"

// Session is one run's live tool surface.
//
// It is the seat's, not the run's: the surface, the guard and the recording
// are the ones the turn built, so a bridged call and a native one are the same
// call made from two places.
type Session struct {
	// RunID identifies the run. It is the token's subject and the ledger's
	// key, and it is in every log line here.
	RunID string

	// Handle and Role name the seat, for the logs and the handshake.
	Handle string
	Role   string

	// Surface is the seat's live tool surface for this run — the SAME
	// object a native loop would execute against. See the package doc for
	// why nothing here reimplements what it does.
	Surface *tools.Surface

	// Ledger receives every call as it completes, so a resume after a
	// restart can rebuild what the run did. Nil keeps the calls in the
	// surface alone, which is correct for a run whose whole life is one
	// process — a test, or a host-placement run the engine is watching.
	Ledger Ledger
}

// Ledger is the durable record of a bridged run's calls.
//
// Declared here, by the consumer, and deliberately narrow: this package
// appends and never reads. The reader is the resume path, which has its own
// view of the same rows — see internal/sandbox.
type Ledger interface {
	// Append records one finished call. An error is LOGGED, never
	// returned to the box: the tool already ran and its effect already
	// happened, so failing the call would tell a coding agent to retry
	// work that has been done.
	Append(ctx context.Context, runID string, call tools.Call) error
}

// Bridge holds the live sessions and the signer that admits them.
type Bridge struct {
	signer *runtoken.Signer
	// base is the URL a box dials, without the token.
	base string
	// ttl bounds a minted endpoint.
	ttl time.Duration

	mu       sync.RWMutex
	sessions map[string]*Session
}

// Options configure [New].
type Options struct {
	// Key signs the per-run tokens and must be the same in every process
	// that mints or verifies one. See [runtoken.Options].
	Key []byte

	// Now is the clock, for the token expiry. Nil takes wall-clock time.
	Now func() time.Time

	// BaseURL is where a box reaches this engine, e.g.
	// "https://engine.example.com". Empty mints no endpoint, which is what
	// a deployment with no reachable API has — and is reported as an
	// absent bridge rather than as a broken one.
	BaseURL string

	// TTL bounds a minted endpoint. Zero takes [DefaultTTL].
	TTL time.Duration
}

// DefaultTTL is how long a run's endpoint stays valid.
//
// Four hours, matched to the thing it has to outlive: a coding run that stops
// to ask a person something is parked until they answer, and the ordinary
// human latency on that is hours rather than minutes. Shorter and a run comes
// back to a dead endpoint through no fault of its own; much longer and a
// token recovered from a box's environment stays useful past any plausible
// run. A run that ends early takes its session with it whatever the token
// says — see [Bridge.Close] — so this bounds only the window in which a
// LEAKED token could match a still-running session.
const DefaultTTL = 4 * time.Hour

// New builds a bridge.
func New(opts Options) *Bridge {
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Bridge{
		signer:   runtoken.New(runtoken.Options{Key: opts.Key, Now: opts.Now}),
		base:     strings.TrimSuffix(opts.BaseURL, "/"),
		ttl:      ttl,
		sessions: map[string]*Session{},
	}
}

// Open registers a run's session and returns the URL its box should dial.
//
// The URL is EMPTY when no base is configured, and that is a real answer
// rather than an error: a deployment whose API is not reachable from a box
// cannot bridge, and the caller's move is to refuse agent mode for that seat
// with a message naming the setting — not to start a run that will fail on its
// first tool call.
func (b *Bridge) Open(s *Session) string {
	if b == nil || s == nil {
		return ""
	}
	// A SESSION THAT CANNOT DISPATCH IS WORSE THAN NONE. Registered, it
	// would answer a box's handshake and then take the process down on its
	// first tools/list — an assembly mistake in one seat's wiring becoming
	// a fleet node's crash. Refused, the caller gets an empty endpoint and
	// declines agent mode for that seat, which is the same shape as a
	// deployment with no bridge URL.
	if s.RunID == "" || s.Surface == nil {
		log.Error("mcp_bridge_session_incomplete",
			"run_id", s.RunID, "seat", s.Handle, "has_surface", s.Surface != nil)
		return ""
	}
	b.mu.Lock()
	b.sessions[s.RunID] = s
	b.mu.Unlock()
	if b.base == "" {
		log.Warn("mcp_bridge_no_base_url", "run_id", s.RunID, "seat", s.Handle)
		return ""
	}
	return b.base + PathPrefix + b.signer.Mint(s.RunID, b.ttl)
}

// Close ends a run's session. A token for a closed run resolves to nothing.
//
// IDEMPOTENT, because the paths that call it are: a run ends once, but the
// engine cleans up on the completion path, on the failure path and on the
// boot-recovery sweep, and each of those has to be safe to run over a run the
// others already finished.
func (b *Bridge) Close(runID string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	delete(b.sessions, runID)
	b.mu.Unlock()
}

// Live reports how many sessions are open. For the health surface and for a
// test that has to know a close actually landed.
func (b *Bridge) Live() int {
	if b == nil {
		return 0
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.sessions)
}

// session resolves a token to its live session.
//
// TWO GATES, and both are needed. The signature and expiry say the token was
// minted by this fleet and has not aged out; the map says the run it names is
// still going. A token that passes the first and fails the second is the
// ordinary case — a box that outlived its run — and it must not reach a
// surface.
func (b *Bridge) session(token string) *Session {
	runID := b.signer.Validate(token)
	if runID == "" {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.sessions[runID]
}

// Handler serves the bridge under [PathPrefix].
//
// The token is read off the path by the mux pattern the caller registers, so
// this handler takes it as an argument rather than re-parsing the URL: two
// parses of one path is how a route ends up authenticating a different string
// from the one it dispatches on.
func (b *Bridge) Handler() http.Handler {
	streamable := mcp.NewStreamableHTTPHandler(
		func(r *http.Request) *mcp.Server {
			s := b.session(r.PathValue("token"))
			if s == nil {
				return nil
			}
			return b.serverFor(s)
		}, nil)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// REFUSED BEFORE THE MCP MACHINERY SEES IT, so an unauthenticated
		// caller cannot make this process allocate a session, and so the
		// refusal is a plain 401 rather than a JSON-RPC error a client
		// would retry.
		//
		// NO DETAIL: forged, expired and "that run is over" are three
		// different facts, and telling the caller which one it was tells
		// an attacker the same.
		if b.session(r.PathValue("token")) == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		streamable.ServeHTTP(w, r)
	})
}

// serverFor builds the MCP server one session exposes.
//
// PER REQUEST rather than cached with the session, because the surface's
// active set MOVES: a coding agent that activates a tool mid-run must see it
// on its next tools/list, and a server built once at Open would advertise the
// set the run started with for its whole life.
func (b *Bridge) serverFor(s *Session) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    serverName,
		Title:   s.Role + " (" + s.Handle + ")",
		Version: "1",
	}, nil)
	for _, def := range s.Surface.ToolDefs() {
		server.AddTool(&mcp.Tool{
			Name:        def.Name,
			Description: def.Description,
			InputSchema: def.Parameters,
		}, s.handler(def.Name))
	}
	return server
}

// handler dispatches one bridged call through the seat's own surface.
func (s *Session) handler(name string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := decodeArgs(req)
		if err != nil {
			// BACK TO THE CALLER AS A FAILED RESULT, not a protocol
			// error: malformed arguments are the one failure a model
			// reliably fixes, and a protocol error tears down the
			// session it would have fixed them in.
			return failure(err.Error()), nil
		}

		res, err := s.Surface.Execute(ctx, llm.ToolCall{Name: name, Arguments: args})
		if err != nil {
			// The surface returns an error only for something that
			// genuinely could not run — a torn-down turn. That IS a
			// protocol error: there is nothing for the caller to fix
			// and nothing to retry against.
			log.ErrorContext(ctx, "mcp_bridge_dispatch_failed",
				"run_id", s.RunID, "seat", s.Handle, "tool", name, "error", err)
			return nil, fmt.Errorf("crewlet: %s: %w", name, err)
		}
		if res.Suspend {
			// A SUSPEND CANNOT CROSS THIS BOUNDARY. It stops the
			// ENGINE's tool loop with the call unanswered so the
			// conversation can be persisted and re-entered; there is no
			// engine loop here, and the conversation belongs to a vendor
			// CLI that would simply hang. The tool that wants it is
			// run_sandbox, which is offered to a bridged run as a
			// handle-plus-wait pair instead — see the engine's bridge
			// surface.
			log.ErrorContext(ctx, "mcp_bridge_suspend_refused",
				"run_id", s.RunID, "seat", s.Handle, "tool", name)
			return failure("This tool cannot be used from a coding run; " +
				"it suspends the caller's own loop."), nil
		}

		s.appendCall(ctx, name, args, res.Output, res.Failed)
		return &mcp.CallToolResult{
			IsError: res.Failed,
			Content: []mcp.Content{&mcp.TextContent{Text: res.Output}},
		}, nil
	}
}

// appendCall records one finished call in the durable ledger.
//
// BEST EFFORT, and the reason is that the alternative is worse. The tool has
// already run and its effect has already happened, so failing the call would
// tell a coding agent to retry work that is done — the duplicate-post failure
// the whole delivery machinery exists to prevent. A lost ledger row costs the
// reviewer one line of evidence after a restart; a duplicated side effect
// costs somebody two identical messages.
func (s *Session) appendCall(ctx context.Context, name string, args map[string]any, out string, failed bool) {
	if s.Ledger == nil {
		return
	}
	call := tools.Call{Name: name, Args: args, Output: out, Failed: failed}
	if err := s.Ledger.Append(ctx, s.RunID, call); err != nil {
		log.WarnContext(ctx, "mcp_bridge_ledger_append_failed",
			"run_id", s.RunID, "seat", s.Handle, "tool", name, "error", err)
	}
}

// decodeArgs turns a call's raw arguments into the map the surface takes.
//
// THROUGH json.Number, so a large id in a tool's arguments survives: Go's
// default decodes every JSON number to float64, which silently rounds an
// issue id past 2^53 and sends the wrong one to a tracker.
func decodeArgs(req *mcp.CallToolRequest) (map[string]any, error) {
	raw := req.Params.Arguments
	// A tool with no arguments arrives as an absent or empty raw message,
	// which is a call with an empty object rather than a malformed one.
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("arguments are not a JSON object: %w", err)
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// failure is a tool result the caller reads and can act on.
func failure(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}

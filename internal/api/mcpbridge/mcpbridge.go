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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/config"
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

// serverName is what the bridge calls itself in the MCP handshake, and the
// key the engine writes it under in a box's server list — one name, so the
// tool prefix a coding agent sees (`mcp__crewlet__…`) and the server its logs
// name agree. Defined by config because config is where the name is RESERVED:
// an mcp_servers entry called the same thing would be overwritten by the
// bridge in every agent-mode box, silently.
const serverName = config.BridgeServerName

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

	// nonce distinguishes THIS session from an earlier one for the same
	// run. It is half the token's subject, so a token minted for a
	// superseded session no longer resolves — see [Bridge.Open].
	nonce string

	// synced is the surface revision the server was last rendered from,
	// so a bridged call that changed nothing re-renders nothing.
	synced uint64

	// server is the ONE MCP server this session exposes, built at Open and
	// kept for the run's life. One rather than one per request because the
	// SDK asks for a server only when a box opens a new MCP session — every
	// later request on that session is served by the server it was opened
	// with — so a server built per request would advertise, to a box that
	// connected once and stayed connected, exactly the tool set the run
	// started with. The live set is kept current by [Session.sync] instead.
	srv *mcp.Server

	mu sync.Mutex
	// advertised is what server currently carries, by name, so sync can
	// tell an activation from a re-read and only notify a box when
	// something actually changed.
	advertised map[string]struct{}
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
//
// NOTHING IS REGISTERED FOR AN EMPTY ANSWER. The minted token is the only way
// to reach a session, so a session registered with no endpoint to mint is a
// live tool surface nothing can dispatch to — and, since the caller reads the
// empty URL as a refusal and never launches a run, nothing ever closes it
// either.
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
	if b.base == "" {
		log.Warn("mcp_bridge_no_base_url", "run_id", s.RunID, "seat", s.Handle)
		return ""
	}
	// THE PREVIOUS SERVER, WHATEVER HELD IT. Captured before attach
	// replaces it, and disconnected below on both shapes this takes: a
	// relaunch handing in a fresh Session for a run id that already has
	// one, and a caller re-opening the SAME Session value. The second is
	// the one an identity check would miss — attach would swap the server
	// out from under the sessions a box had already opened on it, and
	// those would go on serving from this surface, unreachable and
	// unclosable, which is the leak Close exists to prevent.
	b.mu.Lock()
	previous := b.sessions[s.RunID]
	b.mu.Unlock()
	stale := previous.server()

	s.attach()
	b.mu.Lock()
	b.sessions[s.RunID] = s
	b.mu.Unlock()

	if stale != nil {
		// A relaunch: a resumed executor back in a box under the turn id
		// it already had. The coordinator deliberately does not end a run
		// its own turn relaunched, so nothing else closes the earlier
		// session — and the earlier box must not keep a working key to
		// the surface the new run now owns. Its TOKEN stops resolving
		// too, because the subject names this session and not the run:
		// closing the transport only ends a connection the box would
		// reopen, and it holds a signed token that has not expired.
		log.Info("mcp_bridge_session_replaced", "run_id", s.RunID, "seat", s.Handle)
		closeAll(stale)
	}
	return b.base + PathPrefix + b.signer.Mint(s.subject(), b.ttl)
}

// subject is what a run's token names: this SESSION, not its run.
//
// The run id alone was the subject once, and a relaunch reuses the turn id —
// so the superseded box's token went on validating and resolving, to the new
// session, whose surface and durable call log it could then drive. The nonce
// is what makes a token stop working when the session it was minted for is
// replaced.
//
// The separator is "~" and the nonce comes FIRST, so the parse is a cut at
// the first one whatever a run id contains. Two characters are unavailable
// for it: "." delimits [runtoken.Signer.Mint]'s own payload, and the whole
// token is a URL PATH SEGMENT, so anything reserved there is worse than
// wrong — "#" opens the fragment, and a client sends the engine everything
// before it and keeps the rest, which authenticates as a truncated token
// rather than failing as a malformed one. "~" is unreserved (RFC 3986 §2.3)
// and appears in neither a hex nonce nor a UUID.
func (s *Session) subject() string { return s.nonce + subjectSep + s.RunID }

// subjectSep joins a session's nonce to its run id. See [Session.subject].
const subjectSep = "~"

// newNonce is a session's half of its token subject.
//
// Sixteen bytes of crypto/rand: it is not a secret on its own — the token's
// HMAC is what makes it unforgeable — but it must not be GUESSABLE either,
// or a superseded box could mint nothing while still naming the session that
// replaced it. rand.Read never returns an error.
func newNonce() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Close ends a run's session. A token for a closed run resolves to nothing,
// and every MCP session a box opened on it is closed.
//
// BOTH HALVES, because the handler holds the second on its own. The route's
// gate stops a new request the moment the map entry is gone, but the SDK keeps
// each MCP session a box opened — its transport, its server and, through the
// server's handlers, this session's live surface — until the client sends a
// DELETE or an idle timeout fires, and a box torn down mid-run sends nothing.
// Closed here, they leave the handler's table at once; left alone, every
// agent-mode run this process ever served would pin its seat's surface for
// the life of the process.
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
	s := b.sessions[runID]
	delete(b.sessions, runID)
	b.mu.Unlock()
	closeAll(s.server())
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
//
// # The session is not fleet state, and cannot be
//
// A session holds a live [tools.Surface]: the seat's MCP children, its skill
// guard, its per-turn recording. Those are objects in the process that claimed
// the seat, so THE NODE THAT OPENED A SESSION IS THE ONLY ONE THAT CAN SERVE
// IT. Signing shares authentication across a fleet; it does not and could not
// share the surface.
//
// So the bridge URL a box is handed must resolve to that node. Each node mints
// its endpoint from its OWN CREWLET_MCP_BRIDGE_URL, which makes a
// per-node-addressable value correct and a shared load-balancer address wrong:
// behind a balancer, calls land on peers that never held the session and
// answer 401 forever. That misconfiguration used to be indistinguishable from
// a forged token, so [Bridge.miss] tells the two apart in the LOG — never in
// the response, where naming the reason tells an attacker the same thing.
func (b *Bridge) session(token string) *Session {
	s, _, _ := b.resolve(token)
	return s
}

// resolve is [Bridge.session] plus WHY it missed, for the log only.
//
// One function because the signature check is the expensive half and the two
// answers come out of the same parse: asking separately verified the HMAC
// twice on every rejected request, on the one route that has no credential in
// front of it.
//
// The reason is never in the RESPONSE — see [Bridge.Handler] — and its two
// values are logged at different levels for the same reason they are told
// apart at all. A token this fleet never signed is unauthenticated traffic on
// an unauthenticated route: an operator can do nothing about it and anyone who
// can reach the engine can produce it at will, so it is a debug line rather
// than a warning an attacker can print at will. A token this fleet DID sign,
// naming a session this node does not hold, is either an ended run's box
// (ordinary, self-limiting) or the deployment error below — and producing one
// takes the signing key, so warning about it cannot be abused.
func (b *Bridge) resolve(token string) (s *Session, runID, reason string) {
	subject := b.signer.Validate(token)
	if subject == "" {
		return nil, "", "the token is forged, malformed or expired"
	}
	nonce, runID, ok := strings.Cut(subject, subjectSep)
	if !ok {
		return nil, "", "the token is forged, malformed or expired"
	}
	b.mu.RLock()
	held := b.sessions[runID]
	b.mu.RUnlock()
	if held != nil && held.nonce == nonce {
		return held, runID, ""
	}
	if held != nil {
		return nil, runID, "this node holds a DIFFERENT session for that run — " +
			"the run was relaunched and this token names the superseded one, " +
			"which is a box that outlived the run it was started for"
	}
	return nil, runID, "this fleet signed the token, but this node holds no session " +
		"for that run — the run has ended, or " + BaseURLVar + " resolves to a " +
		"node other than the one that owns the seat (a load balancer in front of " +
		"several, or a standalone API process). It must address the node that " +
		"opened the session: a session is a live tool surface, not fleet state"
}

// Miss says WHY a token did not resolve, for the log only. Exported for the
// test that holds the reasons apart; see [Bridge.resolve] for what they mean.
func (b *Bridge) Miss(token string) (runID, reason string) {
	_, runID, reason = b.resolve(token)
	return runID, reason
}

// Handler serves the bridge under [PathPrefix].
//
// The token is read off the path by the mux pattern the caller registers, so
// this handler takes it as an argument rather than re-parsing the URL: two
// parses of one path is how a route ends up authenticating a different string
// from the one it dispatches on.
func (b *Bridge) Handler() http.Handler {
	// NO IDLE TIMEOUT, deliberately. A session's lifetime is its RUN's —
	// [Bridge.Close] ends every MCP session the moment the run ends — and
	// a coding agent legitimately goes quiet for as long as a build or a
	// test suite takes, so an idle clock here would cut a live run off from
	// its tools mid-job for no reason the run could see.
	streamable := mcp.NewStreamableHTTPHandler(
		func(r *http.Request) *mcp.Server {
			s := b.session(r.PathValue("token"))
			if s == nil {
				return nil
			}
			return s.server()
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
		token := r.PathValue("token")
		if s, runID, reason := b.resolve(token); s == nil {
			// LEVELLED BY WHO COULD HAVE CAUSED IT, not by how it reads.
			// This route is deliberately exempt from authentication (the
			// box holds no API token — see [PathPrefix]), so a line
			// written for every bad token is a line anyone who can reach
			// the engine can write without limit. Only the half that
			// takes the signing key to produce is a warning.
			if runID == "" {
				log.DebugContext(r.Context(), "mcp_bridge_unresolved", "reason", reason)
			} else {
				log.WarnContext(r.Context(), "mcp_bridge_unresolved",
					"run_id", runID, "reason", reason)
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		streamable.ServeHTTP(w, r)
	})
}

// attach builds the MCP server this session exposes, carrying the surface's
// current tool set.
func (s *Session) attach() {
	s.mu.Lock()
	s.nonce = newNonce()
	s.srv = mcp.NewServer(&mcp.Implementation{
		Name:    serverName,
		Title:   s.Role + " (" + s.Handle + ")",
		Version: "1",
	}, nil)
	// Nil rather than empty, so the first sync renders unconditionally
	// however the surface's revision happens to be numbered.
	s.advertised = nil
	s.mu.Unlock()
	s.sync()
}

// sync brings the server's tool list up to the surface's active set.
//
// AFTER EVERY BRIDGED CALL, because the active set MOVES and a bridged call is
// the only thing that moves it: a coding agent activates a tool by calling
// `activate_tool` over this same bridge, and the engine's own loop — the other
// writer of an active set — is suspended for as long as the box runs. So a
// sync at each call's end sees every activation the moment it lands, and
// the SDK's list-changed notification tells a connected box to re-list.
//
// A server built once at Open and never touched would advertise the set the
// run started with for its whole life, and the MCP-backed delivery tools —
// which every run starts without and activates when it needs one — would be
// listed as active by the surface and refused as unknown by the server.
func (s *Session) sync() {
	// ASKED BEFORE THE LOCK IS WORTH TAKING. Almost no bridged call is an
	// activation, and ToolDefs deep-clones every active tool's schema — so
	// on a seat with a large grant the unconditional render was the cost of
	// the whole surface on every call, serialised behind this mutex.
	rev := s.Surface.Revision()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.advertised != nil && s.synced == rev {
		return
	}
	if s.advertised == nil {
		s.advertised = map[string]struct{}{}
	}
	s.synced = rev
	live := make(map[string]struct{})
	for _, def := range s.Surface.ToolDefs() {
		live[def.Name] = struct{}{}
		if _, have := s.advertised[def.Name]; have {
			continue
		}
		s.srv.AddTool(&mcp.Tool{
			Name:        def.Name,
			Description: def.Description,
			InputSchema: def.Parameters,
		}, s.handler(def.Name))
		s.advertised[def.Name] = struct{}{}
	}
	// An active set only grows today; removal is here so that the server
	// is a rendering of the surface rather than a record of what was once
	// added, should that ever change.
	var gone []string
	for name := range s.advertised {
		if _, still := live[name]; !still {
			gone = append(gone, name)
		}
	}
	if len(gone) > 0 {
		s.srv.RemoveTools(gone...)
		for _, name := range gone {
			delete(s.advertised, name)
		}
	}
}

// server is this session's MCP server, nil-safe on the session so a caller
// holding a map miss does not have to branch before asking.
func (s *Session) server() *mcp.Server {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.srv
}

// closeAll ends every MCP session a box opened on one server.
//
// Closing a server session runs the handler's own removal hook, so the
// session leaves the SDK's table as well as ending on the wire — which is what
// makes [Bridge.Close] release the surface rather than merely hide it.
//
// It takes the SERVER rather than the Session because the two callers hold
// the object at different moments: Close has a session it just removed, and
// Open has the server a replaced session held BEFORE attach swapped it.
func closeAll(server *mcp.Server) {
	if server == nil {
		return
	}
	for session := range server.Sessions() {
		if err := session.Close(); err != nil {
			log.Debug("mcp_bridge_session_close", "error", err)
		}
	}
}

// Connections reports how many MCP sessions the server currently holds for
// this run. For the health surface and for the test that has to know a close
// reached the SDK's table and not only the bridge's own map.
//
// Sessions, not boxes: one client connect opens more than one — the SDK
// client probes with a stateless server/discover before it initializes, and
// that probe's session is never retired — so the number says "something is
// still attached", never "how many boxes".
func (s *Session) Connections() int {
	server := s.server()
	if server == nil {
		return 0
	}
	n := 0
	for range server.Sessions() {
		n++
	}
	return n
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
		// THE CALL MAY HAVE BEEN AN ACTIVATION. See [Session.sync].
		s.sync()
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

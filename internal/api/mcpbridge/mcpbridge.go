// Package mcpbridge serves one box-mode run's tool surface to the coding CLI in
// its box, over MCP on HTTP.
//
// # What this is for
//
// A providers.llm entry in box mode runs a seat's executor as a detached run of
// its CLI in a sandbox box: the vendor's own loop drives the model, with its own
// shell and editor, and the engine's job is to hand that loop the seat's tools.
// Those tools cannot be shipped into the box — most are MCP children holding the
// SEAT's credentials, several are engine control, and the whole point of a
// sandbox is that its credentials are not the company's. So the box gets one MCP
// server, on the engine, and every call comes back out.
//
// # A transport, beside the one every host-mode run uses
//
// A CLI that runs on this host reaches its phase's tools through a unix socket
// (internal/toolsession). A box is another machine, or another network
// namespace, so it reaches them over HTTP instead, on a listener this node
// binds. The MCP server behind both is the same [toolsession.Server], kept in
// step with what the surface offers; what this package adds is the route, the
// credential that admits a box to it, and the dispatcher a DETACHED run's calls
// go through.
//
// # The dispatch is the seat's own, never a second implementation
//
// A bridged call goes through [tools.Surface] — the same frame a tool loop
// calls. That is the design rule here, stated as a prohibition: this package
// must never grow its own idea of whether a tool is granted, whether a skill
// guard allows it, what a failure looks like, or what gets recorded on the
// surface. Every one of those already exists and is what an operator reading a
// turn expects to see. A second copy would drift, and it would drift on the
// security half.
//
// What the dispatcher adds is what a detached run's calls are owed that a
// surface cannot know, because they are about the RUN and not about one call:
//
//   - THE SEAT'S FENCE, per call. The run outlives the turn that launched it,
//     and the node it calls may stop holding the seat, or a person may ask
//     the seat to stop, while it runs. A call is refused once either has
//     happened — and every call after it — rather than made as a seat this
//     node no longer speaks for.
//   - THE ORDER CALLS ARRIVED IN. A CLI makes a response's calls at once, and
//     each is placed in the run's call log when it is admitted rather than
//     when it returns ([tools.Surface.Admit]), so the repeat count an
//     operation id carries does not depend on which tool finished first.
//   - A CALL IS THE RUN'S, NOT THE REQUEST'S. It runs under the context the
//     run was launched with, stripped of that context's cancellation (the
//     launching turn has suspended), and never under the HTTP request's: a
//     box that disconnects mid-call abandons only the answer, never a tool
//     half-way through a write.
//   - A PANIC IS CONTAINED. A tool that panics is answered as failed, said in
//     the log with its stack, and the run's tools are closed to it: the
//     process stays up, and a defect does not run again on the next call.
//   - A RUN THAT IS OVER REFUSES. A call that reaches a session after its run
//     ended, or after a relaunch replaced it, is answered and not run.
//   - A CREDENTIAL A BOX MAY HOLD. The endpoint is per-job, its token expires
//     with the run, and nothing secret enters the box — see internal/runtoken.
//   - A DURABLE LEDGER. A run outlives its launching turn and can outlive the
//     process, so every call is appended where a resume can read it back,
//     with when it ran, how long it took and who answered it.
//   - AN IDENTITY. The per-run token is this route's credential, so once it
//     has verified, every call is the run's SEAT acting — attached here, per
//     call. See [binding.actingAs] for why the request's own answer cannot be
//     the one.
//
// What it does NOT add is what belongs to a tool loop: rounds, a terminator, a
// budget's per-request refusal, a person's notes. A box-mode run's loop is its
// CLI's own, the engine's phase is suspended while it runs, and those are
// settled when the run is collected and its turn resumes.
//
// # A run outlives the process that opened its session
//
// The box goes on calling for as long as its CLI runs, and the process that
// launched it can restart in the meantime, or its seat move to a peer. So
// nothing the box holds names THAT PROCESS. Its token is signed under the
// fleet's keyring (internal/runtoken over the keys every node holds), and it
// names the run and the JOB — the launch id the run's durable row carries,
// [Session.Launch] — never an id only one process knows. Whichever node takes
// the run over re-opens its session under that same job, with a surface
// rebuilt from the run's own state (internal/engine, as it recovers the seat),
// and the box's token, unchanged, resolves there: a node restarted at the
// address the box dials serves the run again before its seat's mailbox opens.
//
// NOTHING SECRET IS KEPT FOR IT. A job's id is no credential — a token is
// worth something only with the fleet's signature over it — so the row a
// recovery reads holds nothing a reader could present, and the bearer exists
// only in the box and in the URL it dials. Sealing the token into the
// coordination store for the next node to compare against would keep a second
// copy of a working bearer for a check the signature already makes.
//
// WHAT THE TOKEN DOES NOT CARRY IS AN ADDRESS. The box dials the URL it was
// launched with, on the launching node's [BaseURLVar]; a run a PEER takes over
// is served on the peer while the box still dials the node it was given — see
// [Bridge.resolve] for what that node answers.
package mcpbridge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/toolsession"
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
// bridge in every box-mode run's box, silently.
const serverName = config.BridgeServerName

// Session is one run's live tool surface, as the engine opens it.
//
// It is the seat's, not the run's: the surface, the guard and the recording
// are the ones the turn built, so a bridged call and a host-mode one are the
// same call made from two places.
type Session struct {
	// RunID identifies the run. Its token names it, the ledger is keyed by
	// it, and it is in every log line here.
	RunID string

	// Launch is the JOB the session serves: the launch id the run's durable
	// row names (internal/sandbox), minted by the store when the job opened.
	// A token names it beside the run, so a token is good for one job only —
	// a relaunch of the run is a new job, whose session the old job's token
	// does not reach — and a recovery of the SAME job, in this process or a
	// fresh one, opens the same session again, which it does. Required, and
	// never a value with "~" or "." in it, which delimit a token's parts.
	Launch string

	// LastSeq is the place, in its job's admission order, of the last call
	// an EARLIER opening of the job admitted — zero for a job's first
	// opening, and the highest [Call.Seq] the run's row records for a job
	// opened again after a recovery. This opening numbers its calls after
	// it, so the order a resume reads the run's calls in runs on across the
	// recovery rather than starting again at one beside the calls before it.
	LastSeq int

	// Handle and Role name the seat, for the logs and the handshake.
	Handle string
	Role   string

	// Surface is the seat's live tool surface for this run — the SAME
	// object a tool loop would execute against. See the package doc for
	// why nothing here reimplements what it does.
	//
	// It must be bound to the run's TURN ([tools.Surface.ForTurn]): the
	// turn's seat is who every bridged call acts as, and [Bridge.Open]
	// refuses a surface that names none.
	Surface *tools.Surface

	// Ledger receives every call as it completes, so a resume after a
	// restart can rebuild what the run did. Nil keeps the calls in the
	// surface alone, which is correct for a run whose whole life is one
	// process — a test.
	Ledger Ledger

	// Fence is the seat's fence, asked before every call and again after
	// it: nil while this node still holds the seat the run acts for and
	// nobody has asked it to stop. Once it answers otherwise, the call in
	// hand and every call after it are refused — see [binding.Call]. Nil
	// is an open fence, which is a test's.
	Fence func() error

	mu sync.Mutex
	// live is the session's current opening: what a token minted at its
	// latest [Bridge.Open] reaches. Nil until it is opened.
	live *binding
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
	Append(ctx context.Context, runID string, call Call) error
}

// Call is one finished bridged call, as the ledger records it.
type Call struct {
	// Call is the call as the surface records it: the tool, its arguments,
	// what it answered, whether it failed and why.
	tools.Call

	// Seq is the call's place in the order the session ADMITTED its calls,
	// from 1 — the order the CLI made them in, which is not the order they
	// finish in when it makes several at once.
	Seq int

	// StartedAt is when the call was handed to the surface, in UTC, and
	// Duration how long the surface took to answer, on the monotonic clock.
	// Both zero on a call answered without running — arguments that did not
	// parse.
	StartedAt time.Time
	Duration  time.Duration

	// Origin and Server are who answered the call — [toolloop.ToolResult]'s
	// — and empty where no tool did.
	Origin string
	Server string
}

// Bridge holds the live sessions and the signer that admits them.
type Bridge struct {
	signer *runtoken.Signer
	// base is the URL a box dials, without the token.
	base string
	// ttl bounds a minted endpoint.
	ttl time.Duration

	mu sync.RWMutex
	// sessions is every live opening, by its id — which a token names, so
	// one opening never answers for another.
	sessions map[string]*binding
	// runs is each run's ONE live opening. A relaunch replaces it, and the
	// run's end closes it — both by run id, the only key the paths that
	// end a run hold.
	runs map[string]*binding

	// mounted records that this process built the bridge's HTTP handler,
	// which is the only way a box's call reaches a session. See
	// [Bridge.Mounted] for why [Bridge.Open] refuses without it.
	mounted atomic.Bool
}

// Options configure [New].
type Options struct {
	// Material is the fleet's keyring. The per-run tokens are signed under
	// its active key and name it, so a rotation leaves a live run's token
	// valid. One that cannot sign for the fleet is refused. See
	// [runtoken.Options].
	Material runtoken.Material

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

// New builds a bridge, or refuses a keyring that cannot sign for the fleet
// ([runtoken.ErrNoKeyring]).
func New(opts Options) (*Bridge, error) {
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	signer, err := runtoken.New(runtoken.Options{
		Domain: KeyDomain, Material: opts.Material, Now: opts.Now,
	})
	if err != nil {
		return nil, fmt.Errorf("mcpbridge: %w", err)
	}
	return &Bridge{
		signer:   signer,
		base:     strings.TrimSuffix(opts.BaseURL, "/"),
		ttl:      ttl,
		sessions: map[string]*binding{},
		runs:     map[string]*binding{},
	}, nil
}

// Open registers a run's session and returns the URL its box should dial.
//
// ctx is the context the run was launched under, and where every bridged
// call's context comes from: its values — the turn's trace, its attribution —
// and NEVER its cancellation. The launching turn suspends once the run has
// started and its context ends with it; the run's calls go on for as long as
// the run does.
//
// OPENING A JOB'S SESSION AGAIN IS HOW A RECOVERED RUN IS SERVED. A node that
// takes a running job over — this process after a restart, or a peer — opens
// its session under the same [Session.Launch], and the token the box already
// holds resolves to it: the subject a token names is the run and the job, and
// both are the run's own. The URL that second opening returns names this
// node; the box never sees it.
//
// The URL is EMPTY when no base is configured, and that is a real answer
// rather than an error: a deployment whose API is not reachable from a box
// cannot bridge, and the caller's move is to refuse box mode for that seat
// with a message naming the setting — not to start a run that will fail on its
// first tool call.
//
// NOTHING IS REGISTERED FOR AN EMPTY ANSWER. The minted token is the only way
// to reach a session, so a session registered with no endpoint to mint is a
// live tool surface nothing can dispatch to — and, since the caller reads the
// empty URL as a refusal and never launches a run, nothing ever closes it
// either.
func (b *Bridge) Open(ctx context.Context, s *Session) string {
	if b == nil || s == nil {
		return ""
	}
	// A SESSION THAT CANNOT DISPATCH IS WORSE THAN NONE. Registered, it
	// would answer a box's handshake and then take the process down on its
	// first tools/list — an assembly mistake in one seat's wiring becoming
	// a fleet node's crash. Refused, the caller gets an empty endpoint and
	// declines box mode for that seat, which is the same shape as a
	// deployment with no bridge URL.
	//
	// A SURFACE WITH NO SEAT IS THE SAME MISTAKE. Every call is decided as
	// the turn's seat (see [binding.actingAs]), so a session whose surface
	// was never bound to a turn would answer the box's handshake and then
	// refuse every gated builtin as undecidable — a run that burns its
	// budget finding out that nothing it holds can be called.
	//
	// AND A SESSION NAMING NO JOB, or one a token cannot carry: the job is
	// half of what a token names, and the half that tells a relaunch's box
	// from its predecessor's.
	if s.RunID == "" || !validLaunch(s.Launch) || s.Surface == nil ||
		s.Surface.Turn().Handle() == "" {
		log.Error("mcp_bridge_session_incomplete",
			"run_id", s.RunID, "launch_id", s.Launch, "seat", s.Handle,
			"has_surface", s.Surface != nil,
			"has_seat", s.Surface != nil && s.Surface.Turn().Handle() != "")
		return ""
	}
	if b.base == "" {
		log.Warn("mcp_bridge_no_base_url", "run_id", s.RunID, "seat", s.Handle)
		return ""
	}
	// NO LISTENER, NO SESSION, for the same reason as no base URL: the
	// endpoint would name this node and nothing here would answer it, so
	// the box's every tool call fails and the run is rescued as incomplete
	// with no sign of why.
	if !b.Mounted() {
		log.Warn("mcp_bridge_not_mounted", "run_id", s.RunID, "seat", s.Handle)
		return ""
	}

	opened := newBinding(ctx, s)
	s.mu.Lock()
	s.live = opened
	s.mu.Unlock()

	// THE RUN'S PREVIOUS OPENING, WHATEVER HELD IT, is replaced in the same
	// step the new one is filed: a relaunch hands in a fresh Session for a
	// run id that already has one, or the same Session again. Either way
	// the earlier box must not keep a working key to the surface the new
	// run now owns. A recovery re-opening the same job replaces it too, and
	// the same token then reaches the new opening's surface.
	b.mu.Lock()
	previous := b.runs[s.RunID]
	if previous != nil {
		delete(b.sessions, previous.id)
	}
	b.sessions[opened.id] = opened
	b.runs[s.RunID] = opened
	b.mu.Unlock()

	if previous != nil {
		// A relaunch — a resumed executor back in a box under the turn id
		// it already had — or a recovery of the same job. The coordinator
		// deliberately does not end a run its own turn relaunched, so
		// nothing else closes the earlier opening. A relaunch's earlier
		// TOKEN stops resolving too, because the subject names the job and
		// not only the run: closing the transport only ends a connection
		// the box would reopen, and it holds a signed token that has not
		// expired.
		log.Info("mcp_bridge_session_replaced", "run_id", s.RunID, "launch_id", s.Launch,
			"seat", s.Handle, "recovered", previous.id == opened.id)
		previous.end()
	}
	return b.base + PathPrefix + b.signer.Mint(opened.subject(), b.ttl)
}

// subjectSep joins an opening's job to its run id in a token's subject.
//
// "~", and the job FIRST, so the parse is a cut at the first one whatever a run
// id contains. Two characters are unavailable for it: "." delimits
// [runtoken.Signer.Mint]'s own payload, and the whole token is a URL PATH
// SEGMENT, so anything reserved there is worse than wrong — "#" opens the
// fragment, and a client sends the engine everything before it and keeps the
// rest, which authenticates as a truncated token rather than failing as a
// malformed one. "~" is unreserved (RFC 3986 §2.3) and appears in no UUID,
// which is what a job's id is.
const subjectSep = "~"

// validLaunch reports whether a job id can be half of a token's subject: set,
// and free of the two characters that delimit a token's parts.
func validLaunch(launch string) bool {
	return launch != "" && !strings.ContainsAny(launch, subjectSep+".")
}

// Close ends a run's session. A token for a closed run resolves to nothing,
// every MCP session a box opened on it is closed, and a call that still
// reaches it is refused.
//
// ALL THREE, because the handler holds the second on its own. The route's gate
// stops a new request the moment the map entry is gone, but the SDK keeps each
// MCP session a box opened — its transport, its server and, through the
// server's handlers, this session's live surface — until the client sends a
// DELETE or an idle timeout fires, and a box torn down mid-run sends nothing.
// Closed here, they leave the handler's table at once; left alone, every
// box-mode run this process ever served would pin its seat's surface for the
// life of the process.
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
	opened := b.runs[runID]
	delete(b.runs, runID)
	if opened != nil {
		delete(b.sessions, opened.id)
	}
	b.mu.Unlock()
	opened.end()
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

// resolve finds a token's live opening, or says WHY it missed, for the log
// only.
//
// TWO GATES, and both are needed. The signature and expiry say the token was
// minted by this fleet and has not aged out; the map says the opening it names
// is still the run's. A token that passes the first and fails the second is
// the ordinary case — a box that outlived its run — and it must not reach a
// surface.
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
//
// # The session is the seat holder's, and only the token is the fleet's
//
// A session holds a live [tools.Surface]: the seat's MCP children, its skill
// guard, its per-turn recording. Those are objects in the process that holds
// the seat, so ONLY THE NODE HOLDING THE RUN'S SEAT CAN SERVE ITS SESSION —
// the node that launched it, or the one that took the run over and opened its
// job's session again ([Bridge.Open]). Signing shares authentication across a
// fleet, which is what lets that second node accept the token the first one
// minted; it does not and could not share the surface. So a call has to reach
// the holder, and the box dials the address it was launched with: the
// launching node's own [BaseURLVar], which makes a per-node-addressable value
// correct and a shared load-balancer address wrong — behind a balancer, calls
// land on peers that do not hold the session and answer 401.
//
// A RUN A PEER TOOK OVER is the case that leaves: its session is served on
// the peer, and its box still dials this node. While this node is up and has
// not restarted, it still holds its own opening of the job, fenced off the
// moment the seat left, and every call is answered that the run's tools are
// closed and its CLI should end its turn — the run then ends and the seat's
// new holder collects it. A node that restarted since holds nothing for it
// and answers 401, and a node that is gone answers nothing; either way the
// rest of the run's calls fail until it ends.
func (b *Bridge) resolve(token string) (opened *binding, runID, reason string) {
	subject := b.signer.Validate(token)
	if subject == "" {
		return nil, "", "the token is forged, malformed or expired"
	}
	launch, runID, ok := strings.Cut(subject, subjectSep)
	if !ok {
		return nil, "", "the token is forged, malformed or expired"
	}
	b.mu.RLock()
	held, current := b.sessions[launch], b.runs[runID]
	b.mu.RUnlock()
	if held != nil && held.session.RunID == runID {
		return held, runID, ""
	}
	if current != nil {
		return nil, runID, "this node holds a DIFFERENT job's session for that run — " +
			"the run was relaunched and this token names the superseded job, " +
			"which is a box that outlived the run it was started for"
	}
	return nil, runID, "this fleet signed the token, but this node holds no session " +
		"for that run's job: the run has ended; this node restarted and has not yet " +
		"taken the run's seat back (it re-opens the session as it recovers the seat, " +
		"before the seat's mailbox opens); the seat is now held by another node, " +
		"which serves the session while the box still dials this one; or " +
		BaseURLVar + " resolves to a node other than the one holding the seat (a " +
		"load balancer in front of several). It must address the node holding the " +
		"seat, because a session is a live tool surface rather than fleet state"
}

// Miss says WHY a token did not resolve, for the log only. Exported for the
// test that holds the reasons apart; see [Bridge.resolve] for what they mean.
func (b *Bridge) Miss(token string) (runID, reason string) {
	_, runID, reason = b.resolve(token)
	return runID, reason
}

// Mounted reports whether this process built the bridge's HTTP handler.
//
// A session is reachable only through [Bridge.Handler] on the node that opened
// it (see [Bridge.resolve]), so a bridge whose handler no listener took serves
// nobody: a node with api.port 0 has a base URL and a signer and no socket.
// Building the handler is the signal rather than a separate call a wiring
// could forget, because the handler is the one thing a listener cannot serve
// the bridge without. The caller that builds it binds the listener in the same
// step or fails the process, so "built" and "served" do not drift apart.
func (b *Bridge) Mounted() bool { return b != nil && b.mounted.Load() }

// Handler serves the bridge under [PathPrefix], and marks it mounted.
//
// The token is read off the path by the mux pattern the caller registers, so
// this handler takes it as an argument rather than re-parsing the URL: two
// parses of one path is how a route ends up authenticating a different string
// from the one it dispatches on.
func (b *Bridge) Handler() http.Handler {
	b.mounted.Store(true)
	// NO IDLE TIMEOUT, deliberately. A session's lifetime is its RUN's —
	// [Bridge.Close] ends every MCP session the moment the run ends — and
	// a coding agent legitimately goes quiet for as long as a build or a
	// test suite takes, so an idle clock here would cut a live run off from
	// its tools mid-job for no reason the run could see.
	streamable := mcp.NewStreamableHTTPHandler(
		func(r *http.Request) *mcp.Server {
			opened, _, _ := b.resolve(r.PathValue("token"))
			if opened == nil {
				return nil
			}
			return opened.server.MCP()
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
		if opened, runID, reason := b.resolve(r.PathValue("token")); opened == nil {
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
			httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
			return
		}
		streamable.ServeHTTP(w, r)
	})
}

// Connections reports how many MCP sessions the session's current opening
// holds. For the health surface and for the test that has to know a close
// reached the SDK's table and not only the bridge's own map.
//
// Sessions, not boxes: one client connect opens more than one — the SDK
// client probes with a stateless server/discover before it initializes, and
// that probe's session is never retired — so the number says "something is
// still attached", never "how many boxes".
func (s *Session) Connections() int {
	s.mu.Lock()
	opened := s.live
	s.mu.Unlock()
	if opened == nil {
		return 0
	}
	n := 0
	for range opened.server.MCP().Sessions() {
		n++
	}
	return n
}

// --- one opening: the dispatcher a box's calls go through ------------------

// binding is one opening of a session: the MCP server a token minted for its
// job reaches, and the gates every call on it passes.
//
// ITS OWN VALUE, apart from the [Session] the engine fills in, so that a
// session opened twice is two openings: the first ends and its calls are
// refused, while the second serves. A relaunch's token stops resolving with
// its opening, because the relaunch is a new job; a recovery's does not,
// because it is the same one.
type binding struct {
	// id is the job the opening serves — [Session.Launch] as it was opened,
	// held here because the Session value can be opened again for another.
	id      string
	session *Session
	server  *toolsession.Server

	// ctx is the run's launch context with its cancellation removed — what
	// every call runs under. Held, against the rule that a context is
	// threaded rather than stored, because a call arrives on an HTTP
	// request whose context is the box's connection, and the run's own
	// context has no other way to reach it; see [Bridge.Open].
	ctx context.Context

	mu sync.Mutex
	// ended is an opening whose run ended or was relaunched: every call is
	// refused.
	ended bool
	// stopped is why the opening's calls are refused from here on: the
	// seat's fence closed, or a tool panicked. Nil while it serves.
	stopped error
	// admitted is the place of the last call admitted in the job's order —
	// from [Session.LastSeq] — and so the one before the next [Call.Seq].
	admitted int
}

var _ toolsession.Dispatcher = (*binding)(nil)

func newBinding(ctx context.Context, s *Session) *binding {
	opened := &binding{id: s.Launch, session: s, ctx: context.WithoutCancel(ctx), admitted: s.LastSeq}
	opened.server = toolsession.NewServer(serverName, s.Role+" ("+s.Handle+")", opened)
	return opened
}

// subject is what a token minted for this opening names: the job, and its run.
//
// The run id alone was the subject once, and a relaunch reuses the turn id —
// so the superseded box's token went on resolving, to the new session, whose
// surface and durable call log it could then drive. A random id per opening
// came next, and it was one only the process that drew it knew: a restart lost
// it, and the box's every call was refused for the rest of its run. The job's
// id is neither: a relaunch mints a new one, and the run's durable row carries
// it to whichever node opens the job's session again.
func (o *binding) subject() string { return o.id + subjectSep + o.session.RunID }

// end refuses every later call on the opening and closes every MCP session a
// box holds on it. Nil-safe, and safe to call twice.
//
// Closing a server session runs the handler's own removal hook, so the session
// leaves the SDK's table as well as ending on the wire — which is what makes
// [Bridge.Close] release the surface rather than merely hide it.
func (o *binding) end() {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.ended = true
	o.mu.Unlock()
	for ss := range o.server.MCP().Sessions() {
		if err := ss.Close(); err != nil {
			log.Debug("mcp_bridge_session_close", "error", err)
		}
	}
}

// Offered is what the surface offers now, with the hints the registry holds
// for each, so a box's client can tell a read from a write.
func (o *binding) Offered() []toolsession.Tool {
	surface := o.session.Surface
	defs := surface.ToolDefs()
	out := make([]toolsession.Tool, 0, len(defs))
	for _, def := range defs {
		out = append(out, toolsession.Tool{Def: def, Annotations: surface.AnnotationsOf(def.Name)})
	}
	return out
}

// Revision is the surface's: an activation over the bridge is the only thing
// that moves it while the run is in its box.
func (o *binding) Revision() uint64 { return o.session.Surface.Revision() }

// The answers a call gets when it is not run. None of them says more than the
// run needs to stop: what stopped it is in the engine's log.
const (
	// endedNote answers a call on an opening whose run ended or was
	// replaced by a relaunch.
	endedNote = "This run has ended; this call was not run. End your turn now."

	// stoppedNote answers a call once the run's tools are closed: the seat
	// moved off this node, a person asked it to stop, or a tool failed
	// inside the engine.
	stoppedNote = "This run's tools are closed and this call was not run. " +
		"End your turn now."

	// failedInsideNote answers the call that broke inside the engine.
	failedInsideNote = "This call failed inside the engine, and this run's tools are " +
		"now closed. End your turn now."

	// suspendNote answers a tool that would park the caller's own loop,
	// which a box-mode run does not have.
	suspendNote = "This tool cannot be used from a coding run; it suspends the " +
		"caller's own loop."
)

// Call decides and runs one call a box made. It never fails as a Go error: a
// call that could not run is a failed result the CLI hands its model.
//
// The request's context is deliberately not used — see the package doc, "a
// call is the run's, not the request's".
func (o *binding) Call(_ context.Context, in toolsession.Call) toolsession.Result {
	s := o.session
	// THE FENCE BEFORE ANY CALL, outside the lock: it reads the node's own
	// copy of the seat's grant and pauses, and nothing it answers depends on
	// another call of this opening.
	fenced := fence(s)

	o.mu.Lock()
	if o.ended {
		o.mu.Unlock()
		return refused(endedNote)
	}
	if o.stopped == nil && fenced != nil {
		o.stopLocked(fenced)
	}
	if o.stopped != nil {
		o.mu.Unlock()
		return refused(stoppedNote)
	}
	o.admitted++
	seq := o.admitted
	if in.ArgumentsError != "" {
		// ANSWERED, NOT RUN, and recorded: the call was made, and an empty
		// object in place of what the model wrote is a call it never made.
		// Not admitted to the run's call log, since nothing ran.
		o.mu.Unlock()
		out := toolloop.UnparsedArguments(in.Name, in.ArgumentsError)
		o.record(Call{Call: tools.Call{Name: in.Name, Output: out, Failed: true,
			Cause: tools.RefusalInvalid}, Seq: seq})
		return refused(out)
	}
	args := in.Arguments
	if args == nil {
		args = map[string]any{}
	}
	// ITS PLACE IN THE RUN'S CALL LOG, fixed here, under the lock that
	// orders admissions — see [tools.Surface.Admit].
	admitted := s.Surface.Admit(in.Name, args)
	o.mu.Unlock()

	started := time.Now()
	res, broke, err := o.execute(admitted, in.Name, args)
	took := time.Since(started)
	switch {
	case broke != nil:
		log.ErrorContext(o.ctx, "mcp_bridge_tool_panicked", "run_id", s.RunID,
			"seat", s.Handle, "tool", in.Name, "panic", broke.Value, "stack", broke.Stack)
		o.stop(broke)
		res = toolloop.ToolResult{Output: failedInsideNote, Failed: true,
			Cause: tools.RefusalInternalError}
	case err != nil:
		// The surface returns an error only for a call that could not run
		// at all — nothing was recorded on it, and nothing is here: the
		// call did not happen as far as any record says.
		log.ErrorContext(o.ctx, "mcp_bridge_dispatch_failed",
			"run_id", s.RunID, "seat", s.Handle, "tool", in.Name, "error", err)
		return refused(fmt.Sprintf("This call could not be run: %v", err))
	case res.Suspend:
		// A SUSPEND CANNOT CROSS THIS BOUNDARY. It parks a tool loop with
		// the call unanswered so the conversation can be persisted and
		// re-entered; a box-mode run's loop is its CLI's own, which would
		// simply wait on an answer that never comes. The one tool that
		// wants it is run_sandbox, which a box-mode seat is never offered:
		// its own box is where its code work runs.
		log.ErrorContext(o.ctx, "mcp_bridge_suspend_refused",
			"run_id", s.RunID, "seat", s.Handle, "tool", in.Name)
		return refused(suspendNote)
	}

	// AND AGAIN ONCE THE TOOL HAS RETURNED. Its answer is delivered — it
	// ran, and what it did is done — but nothing after it runs as a seat
	// this node no longer holds.
	if after := fence(s); after != nil {
		o.stop(after)
	}
	o.record(Call{
		Call: tools.Call{Name: in.Name, Args: args, Output: res.Output, Failed: res.Failed,
			Cause: res.Cause},
		Seq: seq, StartedAt: started.UTC(), Duration: took,
		Origin: res.Origin, Server: res.Server,
	})
	return toolsession.Result{Output: res.Output, Failed: res.Failed}
}

// execute runs one admitted call as the run's seat, under the run's context,
// and contains a panic — broke is it, recovered.
func (o *binding) execute(admitted toolloop.Admitted, name string, args map[string]any,
) (res toolloop.ToolResult, broke *turn.PanicError, err error) {
	defer func() {
		if broke = turn.Recovered(recover()); broke != nil {
			res, err = toolloop.ToolResult{}, nil
		}
	}()
	res, err = admitted(o.actingAs(o.ctx), llm.ToolCall{Name: name, Arguments: args})
	return res, nil, err
}

// fence asks the session's fence, an open one where it has none.
func fence(s *Session) error {
	if s.Fence == nil {
		return nil
	}
	return s.Fence()
}

// stop closes the opening's tools for good, saying why once.
func (o *binding) stop(why error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.stopLocked(why)
}

func (o *binding) stopLocked(why error) {
	if o.stopped != nil {
		return
	}
	o.stopped = why
	log.WarnContext(o.ctx, "mcp_bridge_run_stopped", "run_id", o.session.RunID,
		"seat", o.session.Handle, "reason", why.Error(),
		"detail", "every later tool call of this run is refused, and the run's "+
			"coding CLI is told to end its turn")
}

// actingAs is the context one bridged call runs under: the run's own SEAT,
// replacing whatever identity the context carries.
//
// # The token is the credential, so the seat is the caller
//
// A call reaches here only on a request whose per-run token verified and named
// this live opening — that is the whole of this route's authentication, and
// what it establishes is that the caller is the box the run was started in,
// acting for the seat the run belongs to. So "who is calling" has exactly one
// answer here, and it is the turn's seat, as [turnctx.Principal] states it.
//
// # And no other context can say so
//
// The route is exempt from the credential guard, because the box holds no API
// token — but the guard still RESOLVES every request, exempt or not, so a box
// presenting nothing arrives carrying [iam.Anonymous] (and, on a development
// build started with -dev-principal, the development principal); and the
// launch context the call runs under is the turn's, which may carry the person
// whose message the turn answered. [turnctx.WithPrincipal] deliberately lets an
// answer already on the context win — correct for a surface reached through
// somebody's own credential — so either would reach every gated builtin in the
// seat's place.
//
// REPLACED HERE, per call, and nowhere else. Teaching WithPrincipal to prefer
// the turn would let the turn a person's request happens to carry overwrite
// that person; this is the one surface where the credential that admitted the
// call IS the run, so it is the one place that may set the answer outright —
// and setting it outright is also what keeps a development principal from
// ever riding a seat's bridged call.
func (o *binding) actingAs(ctx context.Context) context.Context {
	seat := turnctx.Principal(o.session.Surface.Turn())
	if seat.Seat == "" {
		// UNREACHABLE past [Bridge.Open], which refuses a surface with no
		// seat. Answered as unknown rather than left alone, so a surface
		// that somehow lost its turn is never decided as whoever the
		// context happened to name.
		return iam.WithUnresolved(ctx, errNoSeat)
	}
	return iam.WithPrincipal(ctx, seat)
}

// errNoSeat is why a bridged call with no seat to act as cannot be decided.
var errNoSeat = errors.New("mcpbridge: this run's tool surface is bound to no " +
	"seat, so a bridged call has nobody to act as")

// record appends one finished call to the durable ledger.
//
// BEST EFFORT, and the reason is that the alternative is worse. The tool has
// already run and its effect has already happened, so failing the call would
// tell a coding agent to retry work that is done — the duplicate-post failure
// the whole delivery machinery exists to prevent. A lost ledger row costs the
// reviewer one line of evidence after a restart; a duplicated side effect
// costs somebody two identical messages.
//
// Under the run's context rather than the request's, so a box that hangs up
// does not take the row with it.
func (o *binding) record(call Call) {
	s := o.session
	if s.Ledger == nil {
		return
	}
	if err := s.Ledger.Append(o.ctx, s.RunID, call); err != nil {
		log.WarnContext(o.ctx, "mcp_bridge_ledger_append_failed",
			"run_id", s.RunID, "seat", s.Handle, "tool", call.Name, "error", err)
	}
}

// refused is a call answered without the tool's own output.
func refused(msg string) toolsession.Result {
	return toolsession.Result{Output: msg, Failed: true}
}

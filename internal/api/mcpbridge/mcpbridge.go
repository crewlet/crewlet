// Package mcpbridge serves one detached run's tool surface to the coding CLI in
// its box, over MCP on HTTP: a seat's executor, or a person's Dashboard
// Assistant deep work (ADR-0054), each decided as its own [Caller].
//
// # What this is for
//
// Every seat's executor is its coding CLI's own agentic run, detached in a
// sandbox box: the vendor's own loop drives the model, with its own shell and
// editor, and the engine's job is to hand that loop the seat's tools.
// Those tools cannot be shipped into the box — most are MCP children holding the
// SEAT's credentials, several are engine control, and the whole point of a
// sandbox is that its credentials are not the company's. So the box gets one MCP
// server, on the engine, and every call comes back out.
//
// # A transport, beside the one every run on this host uses
//
// A CLI that runs on this host — the reviewer, a worker, the onboarding pass,
// a side pass — reaches its phase's tools through a unix socket
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
//     happened — and every call after it, each on the run's record as
//     refused — rather than made as a seat this node no longer speaks for.
//   - THE ORDER CALLS ARRIVED IN. A CLI makes a response's calls at once, and
//     each is placed in the run's call log when it is admitted rather than
//     when it returns ([tools.Surface.Admit]), so the repeat count an
//     operation id carries does not depend on which tool finished first.
//   - A CALL IS THE RUN'S, NOT THE REQUEST'S. It runs under the context the
//     run was launched with, stripped of that context's cancellation (the
//     launching turn has suspended), and never under the HTTP request's,
//     which the session does not even hand over: a box that disconnects
//     mid-call abandons only the answer, never a tool half-way through a
//     write.
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
//     call. See [seatCaller.Acting] for why the request's own answer cannot be
//     the one.
//
// What it does NOT add is what belongs to a tool loop: rounds, a terminator, a
// budget's per-request refusal. A detached run's loop is its CLI's own, the
// engine's phase is suspended while it runs, and those are settled when the run
// is collected and its turn resumes. A person's notes to the turn it serves
// from the run's note inbox, at the CLI's own boundaries — see steer.go, "A
// NOTE INBOX" — and decides nothing about them.
//
// # A run outlives the process that opened its session
//
// The box goes on calling for as long as its CLI runs, and the process that
// launched it can restart in the meantime, or its seat move to a peer. So
// nothing the box holds names THAT PROCESS. Its token is signed under the
// fleet's keyring (internal/runtoken over the keys every node holds), and it
// names the run, the SEAT it acts as and the JOB — the launch id the run's
// durable row carries, [Session.Launch] — never an id only one process knows.
// Whichever node takes the run over re-opens its session under that same job,
// with a surface rebuilt from the run's own state (internal/engine, as it
// recovers the seat), and the box's token, unchanged, resolves there.
//
// NOTHING SECRET IS KEPT FOR IT. A job's id is no credential — a token is
// worth something only with the fleet's signature over it — so the row a
// recovery reads holds nothing a reader could present, and the bearer exists
// only in the box and in the URL it dials. Sealing the token into the
// coordination store for the next node to compare against would keep a second
// copy of a working bearer for a check the signature already makes.
//
// # One address for the fleet
//
// WHAT THE TOKEN DOES NOT CARRY IS AN ADDRESS, and the box needs none of the
// holder's. It dials [BaseURLVar], which is the FLEET's: any node that serves
// the route, a load balancer in front of several included. The node a call
// lands on finds the run's holder by the seat the token names — its lease —
// and the holder answers, relayed over the broker where it is another node
// ([Bridge.Join], internal/httprelay). So a run whose seat moves goes on
// reaching its tools through the address it started with, and a node that
// serves no HTTP at all — one without the ingress role — runs seats as well as
// one that does. A session with a caller of its own has no seat to
// move, so its token names the process that opened it, and a call for it is
// relayed straight there ([route]). ADR-0056 records the decision, and why
// neither a proxy to the holder's address nor an MCP session on the node a
// call lands on would hold it.
//
// A BOX ON ITS LAUNCHING NODE'S OWN HOST — the `direct` cell — needs no fleet
// address at all: it dials that node's own listener on loopback
// ([Options.HostURL]), which every node that runs seats and binds a listener
// serves it — an ingress node on the listener it serves the route on to
// anyone, and a node without the role on api.port to this host's loopback
// alone ([FromLoopback]), beside its probes. The route is the same handler
// there, so a call for a seat the node has since handed on is relayed to the
// seat's holder exactly as a call that landed on an ingress node is.
//
// # The Host a call may name
//
// ITS OWN GUARD AGAINST DNS REBINDING, in place of the SDK's: a call that
// arrives on a loopback address is admitted under a loopback host name or a
// container runtime's name for the engine host, and refused 403 under any
// other ([hostAdmitted]), on a box's own request and a relayed one alike, to
// the MCP endpoint and to the run's notes route ([refusedHost]). The
// SDK's guard admitted loopback names alone, and Docker Desktop and Podman's
// machine deliver a container box's call to the host's loopback under the
// runtime's name, so every executor in a container there had none of its
// seat's tools.
package mcpbridge

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/httprelay"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
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
// key the engine writes it under in a box's server list — the name a run on
// this host lists the same tools under, so the tool prefix a coding agent sees
// (`mcp__crewlet__…`) is one spelling in every phase. Defined by config
// because config is where the name is RESERVED: an mcp_servers entry called
// the same thing would be overwritten by the bridge in every executor's box,
// silently.
const serverName = config.ToolServerName

// Session is one run's live tool surface, as the engine opens it.
//
// It is the caller's, not the run's: the surface, the guard and the recording
// are the ones the caller built, so a bridged call and one made on this host
// are the same call made from two places. A seat's session serves its turn's
// [tools.Surface] as the turn's seat; the Dashboard Assistant's deep work
// serves its own surface under a [Caller] of its own (ADR-0054).
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

	// LastSeq is the place, in its run's order, of the last call the run's
	// row records from before this opening — zero for a round's first job,
	// and the highest [Call.Seq] the row records for a job opened again
	// after a recovery, or for a job continuing one that asked a person,
	// whose calls the row keeps as the same round's. This opening numbers
	// its calls after it, so the order a resume reads the run's calls in
	// runs on across the recovery or the question rather than starting again
	// at one beside the calls before it.
	LastSeq int

	// Handle and Role name the seat, for the logs and the handshake.
	Handle string
	Role   string

	// Surface is the live tool surface for this run — the SAME object a
	// tool loop would execute against. See the package doc for why nothing
	// here reimplements what it does.
	//
	// A seat's is its [*tools.Surface], bound to the run's TURN
	// ([tools.Surface.ForTurn]): the turn's seat is who every bridged call
	// acts as, and [Bridge.Open] refuses a surface that names none.
	Surface Surface

	// Caller is who every bridged call is decided as. Nil is a seat's
	// session: the seat its [*tools.Surface]'s turn names. Any other
	// surface must name its caller, and [Bridge.Open] refuses a session
	// whose caller is bound to nobody.
	Caller Caller

	// TTL bounds this session's token below the bridge's own, for a run
	// whose life is known to be shorter: a deep-work run's wall clock.
	// Zero takes the bridge's.
	TTL time.Duration

	// Loopback admits a call only from this host's loopback interface: a
	// run confined on the engine host dials the engine there, so a token
	// it leaks is worth nothing from anywhere else.
	Loopback bool

	// OnHost is whether the run's box is on this node's own host — the
	// `direct` cell — and so dials the bridge at this node's own listener
	// ([Options.HostURL]) where the node serves one, rather than at
	// [BaseURLVar]'s address. It moves only the address the token is
	// minted under, never who may call: a seat's call that lands here is
	// still relayed to the seat's holder as any other is, so a seat
	// session never sets [Session.Loopback] for it.
	OnHost bool

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

	// Steer is the run's note inbox, which the bridge serves to the run's
	// CLI between its steps — see steer.go. Closed with the opening that
	// serves it: the run's end, a relaunch that replaces it and the seat
	// leaving this node all close it. Nil serves no notes: the notes route
	// answers 404 and steer_take answers nothing, which is what a run with
	// nothing to steer — the assistant's deep work — has.
	Steer SteerInbox

	mu sync.Mutex
	// live is the session's current opening: what a token minted at its
	// latest [Bridge.Open] reaches. Nil until it is opened.
	live *binding
}

// Surface is what a session serves: the four things the bridge asks of a live
// tool surface, declared here by the consumer. A seat's is *tools.Surface.
type Surface interface {
	// ToolDefs is the set to advertise.
	ToolDefs() []llm.ToolDef
	// Revision moves whenever the advertised set may have changed.
	Revision() uint64
	// AnnotationsOf is one tool's hints.
	AnnotationsOf(name string) crewletmcp.Annotations
	// Admit fixes one call's place in the run's call log and returns it,
	// to be run once ([toolloop.Surface.Admit]).
	Admit(name string, args map[string]any) toolloop.Admitted
}

// Caller is who a session's every call is decided as.
type Caller interface {
	// Acting is the context one bridged call runs under: the context's own
	// identity REPLACED, never kept. See [seatCaller.Acting] for why.
	Acting(ctx context.Context) context.Context
	// Bound reports whether there is anybody to act as.
	Bound() bool
}

// caller is the session's [Caller]: its own, or for a seat's surface the seat
// its turn names. Nil when there is neither.
func (s *Session) caller() Caller {
	if s.Caller != nil {
		return s.Caller
	}
	if seat, ok := s.Surface.(*tools.Surface); ok && seat != nil {
		return seatCaller{surface: seat}
	}
	return nil
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

	// Seq is the call's place in the order the calls REACHED the session,
	// after [Session.LastSeq] — the order the CLI made them in, which is not
	// the order they finish in when it makes several at once.
	Seq int

	// StartedAt is when the call was handed to the surface, in UTC, and
	// Duration how long the surface took to answer, on the monotonic clock.
	// Both zero on a call answered without running — arguments that did not
	// parse, a call made once the run's tools had closed.
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
	// host is the URL a box on this node's own host dials — see
	// [Options.HostURL].
	host string
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
	// ready is every seat this node holds and has finished taking: its
	// running jobs' sessions are open again here ([Bridge.SeatReady]), so a
	// token for one of its jobs that names no opening names a run that is
	// over. See [Bridge.verdict].
	ready map[uuid.UUID]bool

	// streamable is the ONE MCP handler this process serves every session
	// through, a box's own request and a relayed one alike: the SDK keeps
	// each MCP session a box opens in its handler's table, so a second
	// handler would be a second table that knows none of them.
	streamable http.Handler

	// mounted records that this process built the bridge's route, and
	// fleet that it joined its fleet ([Bridge.Join]): either is how a box's
	// call reaches a session opened here. See [Bridge.Reachable].
	mounted atomic.Bool
	fleet   atomic.Pointer[fleet]

	// shutdown is closed by [Bridge.Shutdown], once: every poll for notes
	// held here is let go at once, and every one that arrives after is
	// turned away, each as a 503 a poller pauses on ([Bridge.pollNotes]).
	shutdown     chan struct{}
	shutdownOnce sync.Once

	// hold is how long a poll for notes is held when none waits —
	// [steerHold], which only a case of this package's shortens.
	hold time.Duration
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

	// BaseURL is where a box reaches the fleet's bridge, e.g.
	// "https://engine.example.com" — [BaseURLVar]'s value. Empty mints no
	// endpoint for a box that is not on this node's host, which is what a
	// deployment with no address a container or a remote box can reach has
	// — and is reported as an absent bridge rather than as a broken one.
	BaseURL string

	// HostURL is where a box on THIS NODE'S OWN HOST — the `direct` cell,
	// whose run is a process of this host — reaches the bridge: the node's
	// own listener that serves the route, on loopback, e.g.
	// "http://127.0.0.1:8080".
	//
	// DERIVED, NEVER CONFIGURED, by the process that bound the listener, on
	// every node that runs seats and binds one: an ingress node serves the
	// route there to anyone, and a node without the role serves it on
	// api.port to this host's loopback alone, beside its probes. A box on
	// the host can always dial the host's own socket, so asking the operator
	// for an address there — the one setting the quickstart's company, and
	// every seat pool with no address of its own, would otherwise need
	// before its first turn — asked for something this node already knows.
	// PREFERRED to BaseURL for such a box, because loopback never leaves the
	// host. Empty on a node that binds no listener its own boxes reach on
	// loopback, whose direct boxes dial BaseURL like any other.
	HostURL string

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
	b := &Bridge{
		signer:   signer,
		base:     strings.TrimSuffix(opts.BaseURL, "/"),
		host:     strings.TrimSuffix(opts.HostURL, "/"),
		ttl:      ttl,
		sessions: map[string]*binding{},
		runs:     map[string]*binding{},
		ready:    map[uuid.UUID]bool{},
		shutdown: make(chan struct{}),
		hold:     steerHold,
	}
	// NO IDLE TIMEOUT, deliberately. A session's lifetime is its RUN's —
	// [Bridge.Close] ends every MCP session the moment the run ends — and
	// a coding agent legitimately goes quiet for as long as a build or a
	// test suite takes, so an idle clock here would cut a live run off from
	// its tools mid-job for no reason the run could see.
	//
	// THE HOST CHECK IS THIS PACKAGE'S OWN ([admitHosts]), in front of the
	// handler every request reaches, a box's own and a relayed one alike,
	// in place of the SDK's: see [hostAdmitted] for what the SDK's refused.
	b.streamable = admitHosts(mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		sub, ok := b.parse(r.PathValue("token"))
		if !ok {
			return nil
		}
		opened := b.held(sub)
		if opened == nil {
			return nil
		}
		return opened.server.MCP()
	}, &mcp.StreamableHTTPOptions{DisableLocalhostProtection: true}))
	return b, nil
}

// admitHosts is next behind the bridge's DNS-rebinding guard ([refusedHost]),
// before the MCP machinery sees the request.
func admitHosts(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if refusedHost(w, r) {
			return
		}
		next.ServeHTTP(w, r)
	})
}

// refusedHost is the bridge's DNS-rebinding guard, for every route under its
// prefix: a request that arrived on a loopback address and names a Host
// [hostAdmitted] does not admit is refused 403, as the SDK's own guard refuses
// it, and reported refused. Judged on the node that serves the call — the MCP
// endpoint's handler ([admitHosts]) and the notes route's ([Bridge.serveSteer])
// alike, a relayed call's included — so the two routes cannot come to disagree
// about which names reach the bridge.
func refusedHost(w http.ResponseWriter, r *http.Request) bool {
	if hostAdmitted(r) {
		return false
	}
	// DEBUG, as every refusal anyone who reaches the listener can cause is
	// ([Bridge.refuse]).
	log.DebugContext(r.Context(), "mcp_bridge_host_refused", "host", r.Host,
		"detail", "a request on a loopback address named a host that is neither "+
			"loopback nor a container runtime's name for the engine host")
	http.Error(w, fmt.Sprintf("Forbidden: invalid Host header %q", r.Host), http.StatusForbidden)
	return true
}

// hostAdmitted reports whether r's Host may reach the bridge over the
// connection r arrived on.
//
// DNS REBINDING is what this guards against: a page in a browser on the engine
// host, under a name its author points at 127.0.0.1, can send requests to a
// loopback listener, and every one carries that page's own name as its Host.
// So a request that arrived on a loopback address is admitted only under a
// Host that is loopback ([sandbox.LoopbackHost]) — which is where the SDK's
// guard stops — or one of the names a container runtime gives the engine host
// inside a box ([sandbox.EngineHostName]).
//
// THE RUNTIME'S NAMES, because a container box dials the engine host by one
// ([sandbox.EngineAddress]), and Docker Desktop and Podman's machine on macOS
// and Windows deliver that call to the host's loopback, under the name the
// box dialled. The SDK's guard refused every one of them with 403, so a seat's
// executor in a container on such a host started with none of its seat's
// tools. Neither name is one a page from the internet is served under: both
// are in `.internal`, reserved for private use, and resolve, where they
// resolve at all, through the runtime. Docker Engine and Podman on Linux
// deliver the same call from the container's bridge network, to an address
// this guard does not judge.
//
// A request on any other address came over a network the operator chose to
// bind, and is the token's to admit or refuse. Read from the connection's own
// address, which a relayed request carries from the node it landed on
// (internal/httprelay), never from a header.
func hostAdmitted(r *http.Request) bool {
	local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok || local == nil || !loopbackAddr(local.String()) {
		return true
	}
	host := r.Host
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	}
	return sandbox.LoopbackHost(host) || sandbox.EngineHostName(host)
}

// loopbackAddr reports whether addr, a host and port or a bare host, is a
// loopback address.
func loopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
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
// holds resolves to it: the subject a token names is the job, the seat and the
// run, and all three are the run's own. The box never sees the URL that second
// opening returns.
//
// The URL is EMPTY when no base reaches the session's box ([Bridge.Reaches]),
// and that is a real answer rather than an error: a deployment whose API is
// not reachable from a box
// cannot bridge, and the caller's move is to refuse the launch with a message
// naming the setting — not to start a run that will fail on its first tool
// call.
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
	// refuses the launch, which is the same shape as a deployment with no
	// bridge URL.
	//
	// A SURFACE WITH NO SEAT IS THE SAME MISTAKE. Every call is decided as
	// the turn's seat (see [seatCaller.Acting]), so a session whose surface
	// was never bound to a turn would answer the box's handshake and then
	// refuse every gated builtin as undecidable — a run that burns its
	// budget finding out that nothing it holds can be called.
	//
	// AND A SESSION NAMING NO JOB, or one a token cannot carry: the job is
	// half of what a token names, and the half that tells a relaunch's box
	// from its predecessor's.
	//
	// AND A RUN THE TOKEN CANNOT ROUTE: beside the job and the run, a token
	// names where the run's session lives ([route]) — the seat whose lease
	// names its holder, or the process that opened a caller's own session —
	// and a node the box's call lands on finds the session by it
	// ([Bridge.Join]). Only an agent seat has the id a seat is named by.
	caller := s.caller()
	where, routeErr := b.routeOf(s)
	if s.RunID == "" || !validLaunch(s.Launch) || s.Surface == nil ||
		caller == nil || !caller.Bound() || routeErr != nil {
		log.Error("mcp_bridge_session_incomplete",
			"run_id", s.RunID, "launch_id", s.Launch, "seat", s.Handle,
			"has_surface", s.Surface != nil,
			"has_caller", caller != nil && caller.Bound(), "routed", routeErr == nil)
		return ""
	}
	base := b.Reaches(s.OnHost)
	if base == "" {
		log.Warn("mcp_bridge_no_base_url", "run_id", s.RunID, "seat", s.Handle,
			"on_host", s.OnHost)
		return ""
	}
	// NOTHING REACHES IT, NO SESSION, for the same reason as no base URL: a
	// box's every tool call would fail and the run be rescued as incomplete
	// with no sign of why.
	if !b.Reachable() {
		log.Warn("mcp_bridge_unreachable", "run_id", s.RunID, "seat", s.Handle)
		return ""
	}

	opened := newBinding(ctx, s, where)
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
		// AND ITS NOTE INBOX, unless the new opening serves the same one —
		// the same session opened again.
		previous.endKeeping(opened.steer)
	}
	ttl := b.ttl
	if s.TTL > 0 && s.TTL < ttl {
		ttl = s.TTL
	}
	return base + PathPrefix + b.signer.Mint(opened.subject(), ttl)
}

// Reaches is the base URL a box dials this bridge at — this node's own
// listener for a box on its host where it has one ([Options.HostURL]), and
// [BaseURLVar]'s address otherwise — or "" where it has none to dial.
func (b *Bridge) Reaches(onHost bool) string {
	if b == nil {
		return ""
	}
	if onHost && b.host != "" {
		return b.host
	}
	return b.base
}

// subjectSep joins the three parts of a token's subject: the opening's job,
// where its session lives ([route]), and its run id.
//
// "~", and the run LAST, so the parse is a cut at the first two whatever a run
// id contains — a job's id is refused one ([validLaunch]), and a route is a
// UUID or base64url. Two characters are unavailable for it: "." delimits
// [runtoken.Signer.Mint]'s own payload, and the whole token is a URL PATH
// SEGMENT, so anything reserved there is worse than wrong — "#" opens the
// fragment, and a client sends the engine everything before it and keeps the
// rest, which authenticates as a truncated token rather than failing as a
// malformed one. "~" is unreserved (RFC 3986 §2.3) and appears in no UUID.
const subjectSep = "~"

// validLaunch reports whether a job id can be the first part of a token's
// subject: set, and free of the two characters that delimit a token's parts.
func validLaunch(launch string) bool {
	return launch != "" && !strings.ContainsAny(launch, subjectSep+".")
}

// ErrSeatless is a seat's surface bound to no agent seat: every bridged call
// acts as the surface's seat, and its token names the seat, so such a surface
// could decide nothing and be found nowhere.
var ErrSeatless = errors.New("mcpbridge: the tool surface is bound to no agent seat")

// SeatOf is the id of the agent seat a surface acts as — the seat whose lease
// says which node holds a run — or [ErrSeatless]. [Bridge.Open] refuses a
// session whose surface has none, and a caller that wants to refuse by name
// first asks this same predicate.
func SeatOf(surface *tools.Surface) (uuid.UUID, error) {
	if surface == nil || surface.Turn().Handle() == "" {
		return uuid.Nil, ErrSeatless
	}
	seat, err := uuid.Parse(surface.Turn().AgentID())
	if err != nil {
		return uuid.Nil, ErrSeatless
	}
	return seat, nil
}

// route is where a run's session lives: the part of what its token names that
// a node the box's call lands on finds the session by ([Bridge.Join]). It is
// the agent SEAT whose lease names the node holding the seat, or, for a
// session with a [Caller] of its own, the PROCESS that opened it.
//
// # Why a caller's own session is pinned to its opener
//
// A seat's run outlives the process that launched it: the seat's next holder
// recovers the run's job and opens its session again, and the box's token
// resolves there, so a seat's token names the seat and never a process. A
// session with a caller of its own, the Dashboard Assistant's deep work
// (ADR-0054), has no seat and no successor: the turn that opened it watches
// the run to its end, and a node that dies ends that turn. Nothing but its
// opener can ever serve it, so its token names that incarnation outright, and
// a call that lands elsewhere is relayed straight to it.
type route struct {
	// seat is the agent seat whose lease names the session's holder. Nil
	// for a pinned session.
	seat uuid.UUID
	// owner is the incarnation that opened a pinned session, and empty for
	// one opened by a bridge that had joined no fleet, which only its own
	// node's route reaches.
	owner string
}

// ownerMark opens a route that names an incarnation. "_" because no seat's
// UUID begins with it, it is unreserved in a URL path segment, and it is
// neither "." nor [subjectSep]. The incarnation follows base64url-encoded,
// since a node's id may carry a dot.
const ownerMark = "_"

// pinned reports whether the route names its opener rather than a seat.
func (r route) pinned() bool { return r.seat == uuid.Nil }

func (r route) String() string {
	if !r.pinned() {
		return r.seat.String()
	}
	return ownerMark + base64.RawURLEncoding.EncodeToString([]byte(r.owner))
}

// parseRoute reads a route off a token's subject.
func parseRoute(text string) (route, bool) {
	if encoded, ok := strings.CutPrefix(text, ownerMark); ok {
		owner, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			return route{}, false
		}
		return route{owner: string(owner)}, true
	}
	seat, err := uuid.Parse(text)
	if err != nil || seat == uuid.Nil {
		return route{}, false
	}
	return route{seat: seat}, true
}

// routeOf is where s's session will live: its seat's for a seat's session, or
// this process for one with a caller of its own ([route]). [ErrSeatless] for a
// seat's session whose surface names no agent seat.
func (b *Bridge) routeOf(s *Session) (route, error) {
	if s.Caller != nil {
		var owner string
		if f := b.fleet.Load(); f != nil {
			owner = f.relay.Owner()
		}
		return route{owner: owner}, nil
	}
	surface, _ := s.Surface.(*tools.Surface)
	seat, err := SeatOf(surface)
	return route{seat: seat}, err
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
// detached run this process ever served would pin its seat's surface for the
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

// subject is what a token names: the job its opening serves, where its
// session lives, and the run.
type subject struct {
	launch string
	route  route
	run    string
}

func (s subject) String() string {
	return s.launch + subjectSep + s.route.String() + subjectSep + s.run
}

// parse verifies a token — this fleet signed it, and it has not expired — and
// reads its subject. False for anything else, which is all a forged,
// malformed and expired token have in common that this route may act on.
func (b *Bridge) parse(token string) (subject, bool) {
	raw := b.signer.Validate(token)
	if raw == "" {
		return subject{}, false
	}
	launch, rest, ok := strings.Cut(raw, subjectSep)
	if !ok {
		return subject{}, false
	}
	routeText, run, ok := strings.Cut(rest, subjectSep)
	if !ok || run == "" || !validLaunch(launch) {
		return subject{}, false
	}
	where, ok := parseRoute(routeText)
	if !ok {
		return subject{}, false
	}
	return subject{launch: launch, route: where, run: run}, true
}

// held is this node's live opening of the job a subject names, or nil.
//
// TWO GATES, and both are needed. The signature and expiry ([Bridge.parse])
// say the token was minted by this fleet and has not aged out; the map says
// the opening it names is still the run's. A token that passes the first and
// fails the second is the ordinary case — a box that outlived its run, or one
// whose run is served on another node — and it must not reach a surface here.
func (b *Bridge) held(sub subject) *binding {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if opened := b.sessions[sub.launch]; opened != nil && opened.session.RunID == sub.run {
		return opened
	}
	return nil
}

// verdict is what THIS NODE can do for a token's job: serve its opening, say
// that the run is over, or say it does not hold the run's seat — not yet, or no
// longer.
//
// # The session is the seat holder's, and only the token is the fleet's
//
// A session holds a live [tools.Surface]: the seat's MCP children, its skill
// guard, its per-turn recording. Those are objects in the process that holds
// the seat, so ONLY THE NODE HOLDING THE RUN'S SEAT CAN SERVE ITS SESSION —
// the node that launched it, or the one that took the run over and opened its
// job's session again ([Bridge.Open]). Signing shares authentication across a
// fleet; it does not and could not share the surface. So a call is answered
// by the holder wherever it lands ([Bridge.Join]), and the holder answers it
// from here.
//
// "OVER" ONLY ONCE THIS NODE HAS FINISHED TAKING THE SEAT ([Bridge.SeatReady]):
// a node that has just taken a seat opens its running jobs' sessions again as
// it recovers the seat, and until it has, a missing opening says nothing about
// the run.
//
// A PINNED SESSION IS ITS OPENER'S FOR LIFE ([route]), and the relay has
// already held a call to the incarnation its token names, so an opening that
// is not here is a run that is over.
func (b *Bridge) verdict(sub subject) httprelay.Verdict {
	if sub.route.pinned() {
		if b.held(sub) != nil {
			return httprelay.Serve
		}
		return httprelay.Refuse
	}
	b.mu.RLock()
	ready := b.ready[sub.route.seat]
	b.mu.RUnlock()
	switch {
	case !ready:
		return httprelay.Elsewhere
	case b.held(sub) != nil:
		return httprelay.Serve
	default:
		return httprelay.Refuse
	}
}

// Miss says WHY a token reaches no opening on this node, for the log only:
// the run id where the token is the fleet's, and empty where it is not. See
// [Bridge.Handler] for why the two are told apart, and why neither is ever
// in a response.
func (b *Bridge) Miss(token string) (runID, reason string) {
	sub, ok := b.parse(token)
	if !ok {
		return "", "the token is forged, malformed or expired"
	}
	b.mu.RLock()
	current := b.runs[sub.run]
	b.mu.RUnlock()
	switch {
	case b.held(sub) != nil:
		return sub.run, ""
	case current != nil:
		return sub.run, "this node holds a DIFFERENT job's session for that run — " +
			"the run was relaunched and this token names the superseded job, " +
			"which is a box that outlived the run it was started for"
	case b.fleet.Load() == nil:
		return sub.run, "this fleet signed the token, and this node holds no session " +
			"for that run's job and has no fleet to find its holder in: the run has " +
			"ended, or this node restarted and has not yet taken the run's seat back " +
			"(it re-opens the session as it recovers the seat, before the seat's " +
			"mailbox opens)"
	default:
		return sub.run, "the run has ended: the node holding its seat holds no session for its job"
	}
}

// LoopbackBase reports whether the URL a box dials names this host's loopback
// interface: the one base a run confined on the engine host can be held to
// ([Session.Loopback]), because its calls then arrive from loopback alone.
func (b *Bridge) LoopbackBase() bool {
	if b == nil {
		return false
	}
	u, err := url.Parse(b.base)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// FromLoopback reports whether a request arrived on this host's loopback
// interface, read from the connection's own address and never from a header
// a caller could write.
//
// EXPORTED for the one other place that admits a caller by it: the listener of
// a node without the ingress role, which serves the routes its own boxes dial
// to this host's loopback alone (internal/api's probe surface). One predicate,
// so "a caller on this host" cannot mean two things on two routes a box dials.
func FromLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// SeatReady records that this node has finished taking a seat: every job of
// the seat still running in its box has had its session opened again here
// ([Bridge.Open]), so from now on a token for one of the seat's jobs that
// reaches no opening names a run that is over. The engine calls it as it
// recovers the seat's detached runs, after they are recovered.
func (b *Bridge) SeatReady(seat uuid.UUID) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ready[seat] = true
}

// Ready reports whether this node has finished taking a seat
// ([Bridge.SeatReady]) and not released it since. For a test that has to know
// the engine told it.
func (b *Bridge) Ready(seat uuid.UUID) bool {
	if b == nil {
		return false
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.ready[seat]
}

// ReleaseSeat ends every session this node opened for a seat it no longer
// holds, and forgets that it took the seat.
//
// THE SESSIONS GO WITH THE SEAT, not with their runs: the runs go on, served
// by the seat's next holder, and an opening left here would pin the seat's
// live surface — its MCP children, holding the seat's credentials — in a
// process that no longer speaks for it, for as long as the process lived.
// Ending it also ends every MCP session a box holds on it, so the box's next
// request is routed afresh and reaches the holder.
func (b *Bridge) ReleaseSeat(seat uuid.UUID) {
	// NIL NAMES NO SEAT, and every pinned opening's route carries it.
	if b == nil || seat == uuid.Nil {
		return
	}
	b.mu.Lock()
	delete(b.ready, seat)
	var released []*binding
	for runID, opened := range b.runs {
		if opened.route.seat == seat {
			delete(b.runs, runID)
			delete(b.sessions, opened.id)
			released = append(released, opened)
		}
	}
	b.mu.Unlock()
	for _, opened := range released {
		opened.end()
	}
}

// Reachable reports whether a box's call can reach a session this node opens:
// through this node's own route ([Bridge.Handler]), or relayed to it by any
// node of the fleet that serves one ([Bridge.Join]).
//
// A bridge that neither serves the route nor has joined a fleet serves nobody:
// a node with api.port 0 has a base URL and a signer and no way in.
// [Bridge.Open] refuses on that rather than mint an endpoint every call to
// which would fail.
func (b *Bridge) Reachable() bool {
	return b != nil && (b.mounted.Load() || b.fleet.Load() != nil)
}

// Handler serves the bridge under [PathPrefix], and marks it mounted.
//
// The token is read off the path by the mux pattern the caller registers, so
// this handler takes it as an argument rather than re-parsing the URL: two
// parses of one path is how a route ends up authenticating a different string
// from the one it dispatches on.
//
// # Which node answers
//
// THE ONE HOLDING THE RUN'S SEAT, wherever the call landed. A node with a live
// opening of the token's job whose fence is open is that node, and answers at
// once. Any other node that has joined its fleet relays the call to the seat's
// holder ([Bridge.Join]), so the address a box dials can be any node that
// serves this route — one address for the fleet, a load balancer in front of
// several included — and a run whose seat moves goes on reaching its tools. A
// node that has joined no fleet is the fleet: it answers from its own opening,
// fenced or not, or refuses.
func (b *Bridge) Handler() http.Handler {
	b.mounted.Store(true)
	return b.routed(func(w http.ResponseWriter, r *http.Request, _ *binding) {
		b.streamable.ServeHTTP(w, r)
	})
}

// routed is the routing every route under the bridge's prefix takes — the MCP
// endpoint ([Bridge.Handler]) and the notes route ([Bridge.SteerHandler]) alike,
// so the two cannot come to disagree about which node answers a token: serve
// is handed the opening where this node is the one to answer.
func (b *Bridge) routed(serve func(http.ResponseWriter, *http.Request, *binding)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// REFUSED BEFORE THE MCP MACHINERY SEES IT, so an unauthenticated
		// caller cannot make this process allocate a session, and so the
		// refusal is a plain 401 rather than a JSON-RPC error a client
		// would retry.
		sub, ok := b.parse(r.PathValue("token"))
		if !ok {
			b.refuse(w, r, "", "the token is forged, malformed or expired")
			return
		}
		opened := b.held(sub)
		// A LOOPBACK SESSION ADMITS THIS HOST'S LOOPBACK ALONE
		// ([Session.Loopback]), read from the connection and never from a
		// header, and is never forwarded: a call that reached another node
		// is refused by this one's relay ([Bridge.admitRelayed]).
		if opened != nil && opened.session.Loopback && !FromLoopback(r) {
			b.refuse(w, r, sub.run, "the session admits only this host's loopback, and the "+
				"call came from "+r.RemoteAddr)
			return
		}
		if opened != nil && fence(opened.session) == nil {
			serve(w, r, opened)
			return
		}
		if f := b.fleet.Load(); f != nil {
			if isNotesPoll(r) {
				b.forwardPoll(w, r, sub, f)
				return
			}
			b.forward(w, r, sub, f)
			return
		}
		if opened != nil {
			// FENCED, AND NO FLEET TO DEFER TO: the opening's own answer —
			// that the run's tools are closed, and that nothing more is
			// taken from its notes — is the answer.
			serve(w, r, opened)
			return
		}
		_, reason := b.Miss(r.PathValue("token"))
		b.refuse(w, r, sub.run, reason)
	})
}

// heldOnly serves a call relayed to this node on the opening the call's token
// names, which the relay's admission already found here
// ([Bridge.admitRelayed]); a token whose opening has gone since is refused.
func (b *Bridge) heldOnly(serve func(http.ResponseWriter, *http.Request, *binding)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sub, ok := b.parse(r.PathValue("token"))
		if !ok {
			b.refuse(w, r, "", "the token is forged, malformed or expired")
			return
		}
		opened := b.held(sub)
		if opened == nil {
			_, reason := b.Miss(r.PathValue("token"))
			b.refuse(w, r, sub.run, reason)
			return
		}
		serve(w, r, opened)
	})
}

// refuse answers a call no opening will take.
//
// NO DETAIL: forged, expired and "that run is over" are three different
// facts, and telling the caller which one it was tells an attacker the same.
//
// LEVELLED BY WHO COULD HAVE CAUSED IT, not by how it reads. This route is
// deliberately exempt from authentication (the box holds no API token — see
// [PathPrefix]), so a line written for every bad token is a line anyone who
// can reach the engine can write without limit. Only the half that takes the
// signing key to produce — runID set — is a warning.
func (b *Bridge) refuse(w http.ResponseWriter, r *http.Request, runID, reason string) {
	if runID == "" {
		log.DebugContext(r.Context(), "mcp_bridge_unresolved", "reason", reason)
	} else {
		log.WarnContext(r.Context(), "mcp_bridge_unresolved", "run_id", runID, "reason", reason)
	}
	httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
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
	// caller is who the opening's every call is decided as, fixed when it
	// opened ([Session.caller]).
	caller Caller
	// route is where the opening lives ([route]): the second part of what a
	// token names, and, for a seat's, what [Bridge.ReleaseSeat] ends it by.
	route route

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
	// arrived is the place of the last call that reached the opening, in
	// the job's order — from [Session.LastSeq] — and so the one before the
	// next [Call.Seq].
	arrived int

	// steer is the run's note inbox, fixed when the opening opened
	// ([Session.Steer]), and closed when it ends; nil once it has.
	steer SteerInbox

	// unsubstituted says once, per opening, that the CLI's hooks name no
	// agent the bridge can tell apart — see [binding.takeSteer].
	unsubstituted sync.Once
}

var _ toolsession.Dispatcher = (*binding)(nil)

func newBinding(ctx context.Context, s *Session, where route) *binding {
	opened := &binding{id: s.Launch, session: s, caller: s.caller(), route: where,
		ctx: context.WithoutCancel(ctx), arrived: s.LastSeq, steer: s.Steer}
	opened.server = toolsession.NewServer(serverName, s.Role+" ("+s.Handle+")", opened)
	// steer_take, ANSWERED BEFORE THE TOOLS ARE: never listed, never handed
	// to the run's surface, never recorded — see steer.go.
	opened.server.MCP().AddReceivingMiddleware(opened.steerMiddleware)
	// THE ANSWER TO AN ACTIVATION WAITS FOR THE BOX'S RE-LIST
	// ([toolsession.Server.HoldAnswersForRelist]): the coding CLIs that run in
	// a box (Claude Code, OpenCode) re-read their tool list when told it
	// changed, and answered at once one asks its model again before the
	// re-list lands, offering the tool it just activated a request late.
	// Unconditional, because the bridge is told nothing of which CLI dials
	// it, and the cost on one that never re-lists is the hold's own short
	// bound on an activation's answer, never a refusal.
	opened.server.HoldAnswersForRelist()
	return opened
}

// subject is what a token minted for this opening names: the job, the seat,
// and the run.
//
// THE SEAT, so a node the box's call lands on can find the run's holder from
// the token alone — by the seat's lease — with nothing of the run's to read
// first.
//
// The run id alone was the subject once, and a relaunch reuses the turn id —
// so the superseded box's token went on resolving, to the new session, whose
// surface and durable call log it could then drive. A random id per opening
// came next, and it was one only the process that drew it knew: a restart lost
// it, and the box's every call was refused for the rest of its run. The job's
// id is neither: a relaunch mints a new one, and the run's durable row carries
// it to whichever node opens the job's session again.
func (o *binding) subject() string {
	return subject{launch: o.id, route: o.route, run: o.session.RunID}.String()
}

// end refuses every later call on the opening and closes every MCP session a
// box holds on it. Nil-safe, and safe to call twice.
//
// Closing a server session runs the handler's own removal hook, so the session
// leaves the SDK's table as well as ending on the wire — which is what makes
// [Bridge.Close] release the surface rather than merely hide it.
func (o *binding) end() { o.endKeeping(nil) }

// endKeeping is [binding.end], closing the opening's note inbox unless it is
// keep — the inbox an opening replacing this one serves too.
func (o *binding) endKeeping(keep SteerInbox) {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.ended = true
	inbox := o.steer
	o.steer = nil
	o.mu.Unlock()
	if inbox != nil && inbox != keep {
		inbox.Close()
	}
	for ss := range o.server.MCP().Sessions() {
		if err := ss.Close(); err != nil {
			log.Debug("mcp_bridge_session_close", "error", err)
		}
	}
}

// inbox is the opening's note inbox, nil once it ended or where it has none.
func (o *binding) inbox() SteerInbox {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.ended {
		return nil
	}
	return o.steer
}

// over reports whether the opening has ended.
func (o *binding) over() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.ended
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
	// which a detached run does not have.
	suspendNote = "This tool cannot be used from a coding run; it suspends the " +
		"caller's own loop."
)

// Call decides and runs one call a box made. It never fails as a Go error: a
// call that could not run is a failed result the CLI hands its model.
//
// The request's context is not handed over ([toolsession.Dispatcher.Call]) —
// see the package doc, "a call is the run's, not the request's".
func (o *binding) Call(in toolsession.Call) toolsession.Result {
	s := o.session
	// THE FENCE BEFORE ANY CALL, outside the lock: it reads the node's own
	// copy of the seat's grant and pauses, and nothing it answers depends on
	// another call of this opening.
	fenced := fence(s)

	o.mu.Lock()
	if o.ended {
		// NOT RECORDED: the run is over, and its record with it — the job's
		// row is gone, or holds the job that replaced it.
		o.mu.Unlock()
		return refused(endedNote)
	}
	if o.stopped == nil && fenced != nil {
		o.stopLocked(fenced)
	}
	o.arrived++
	seq := o.arrived
	args := in.Arguments
	if args == nil {
		args = map[string]any{}
	}
	if o.stopped != nil {
		// REFUSED, AND RECORDED in its place: the run is live, and its box
		// made this call after its tools closed — which a reviewer reading
		// the run should see, and a surface that is not a model can act on
		// only by its class.
		o.mu.Unlock()
		o.record(Call{Call: tools.Call{Name: in.Name, Args: args, Output: stoppedNote,
			Failed: true, Cause: tools.RefusalNotRunning}, Seq: seq})
		return refused(stoppedNote)
	}
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
		// re-entered; a detached run's loop is its CLI's own, which would
		// simply wait on an answer that never comes. No seat tool asks for
		// it — the Dashboard Assistant's quick mode parks on a proposal
		// this way, and its deep work's surface answers a proposal with a
		// waiting note instead — so this is the guard a tool added later
		// meets, rather than a hang inside somebody's run.
		//
		// REFUSED, AND RECORDED as a call that ran, which it did: the tool
		// started whatever it would have parked on, and the run's record,
		// like the tool loop's, says so — forbidden, refused for where it
		// was called.
		log.ErrorContext(o.ctx, "mcp_bridge_suspend_refused",
			"run_id", s.RunID, "seat", s.Handle, "tool", in.Name)
		res = toolloop.ToolResult{Output: suspendNote, Failed: true,
			Cause: tools.RefusalForbidden, Origin: res.Origin, Server: res.Server}
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
	res, err = admitted(o.caller.Acting(o.ctx), llm.ToolCall{Name: name, Arguments: args})
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

// seatCaller decides a call as the seat a surface's turn names.
type seatCaller struct{ surface *tools.Surface }

// Bound reports whether the surface's turn names a seat.
func (c seatCaller) Bound() bool { return c.surface.Turn().Handle() != "" }

// Acting is the context one bridged call runs under: the run's own SEAT,
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
func (c seatCaller) Acting(ctx context.Context) context.Context {
	seat := turnctx.Principal(c.surface.Turn())
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

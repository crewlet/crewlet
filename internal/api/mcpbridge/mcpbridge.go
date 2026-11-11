// Package mcpbridge serves one detached run's tool surface to the harness in
// its box, over MCP on HTTP: a seat's executor, or a person's Dashboard
// Assistant deep work (ADR-0054), each decided as its own [Caller].
//
// # What this is for
//
// Every seat's executor is its harness's own agentic run, detached in a
// sandbox box: the vendor's own loop drives the model, with its own shell and
// editor, and the engine's job is to hand that loop the seat's tools.
// Those tools cannot be shipped into the box — most are MCP children holding the
// SEAT's credentials, several are engine control, and the whole point of a
// sandbox is that its credentials are not the company's. So the box gets one MCP
// server, on the engine, and every call comes back out.
//
// # A transport, beside the one every run on this host uses
//
// A harness that runs on this host — the reviewer, a worker, the onboarding pass,
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
//   - THE ORDER CALLS ARRIVED IN. A harness makes a response's calls at once, and
//     each is placed in the run's call log when it is admitted rather than
//     when it returns ([tools.Surface.Admit]), so the repeat count an
//     operation id carries does not depend on which tool finished first.
//   - A CALL IS THE RUN'S, NOT THE REQUEST'S. It runs under the context the
//     run was launched with, stripped of that context's cancellation (the
//     launching turn has suspended), and never under the HTTP request's,
//     which the session does not even hand over: a box that disconnects
//     mid-call abandons only the answer, never a tool half-way through a
//     write. And the record says so ([Call.Abandoned]): a call whose
//     harness had given up on it — cancelled it, or hung up on the response
//     its answer was kept going on (below) — or whose opening had ended, by
//     the time it returned is one its job's session holds with no outcome
//     or a failed one, though it ran.
//   - A PANIC IS CONTAINED. A tool that panics is answered as failed, said in
//     the log with its stack, and the run's tools are closed to it: the
//     process stays up, and a defect does not run again on the next call.
//   - A RUN THAT IS OVER REFUSES. A call that reaches a session after its run
//     ended, or after a relaunch replaced it, is answered and not run.
//   - A CREDENTIAL A BOX MAY HOLD. The endpoint is per-job, its token expires
//     with the run, and nothing secret enters the box — see internal/runtoken.
//   - THE MODEL A CALL WAS MADE ON. A tool loop tells each call its round's
//     model; a bridged call was made by a harness's own loop, whose rounds
//     the engine never sees, so each is told the model the run's launch
//     pointed that harness at ([Session.Model]) — the one a seat's code write
//     names on its push, so a run's pushes name one model whichever path
//     made them.
//   - A DURABLE LEDGER. A run outlives its launching turn and can outlive the
//     process, so every call is appended where a resume can read it back,
//     with when it ran, how long it took and who answered it.
//   - AN IDENTITY. The per-run token is this route's credential, so once it
//     has verified, every call is the run's SEAT acting — attached here, per
//     call. See [seatCaller.Acting] for why the request's own answer cannot be
//     the one.
//
// What it does NOT add is what belongs to a tool loop: rounds, a terminator, a
// budget's per-request refusal. A detached run's loop is its harness's own, the
// engine's phase is suspended while it runs, and those are settled when the run
// is collected and its turn resumes. A person's notes to the turn it serves
// from the run's note inbox, at the harness's own boundaries — see steer.go, "A
// NOTE INBOX" — and decides nothing about them. A load of one of the run's own
// skills, which its harness reports, it records as the run's own load of the
// Tool Skill it stands for — see skills.go. And which session the job's box
// started — the one its launch continued, or one of its own in that one's place
// — it reads off the token the box dials, one of the job's two, and has the
// run's skill guard held to it before the run's first call — see started.go.
//
// # A call's answer starts at once and keeps going
//
// A box's harness times a call's response, and the SDK's streamable HTTP
// transport writes nothing on it until the response's first event: with
// nothing else sent, the answer itself. Claude Code 2.1.310 gives an HTTP
// response's first byte a minute where the server's entry sets no timeout,
// then answers its model that the call timed out and cancels nothing, while
// the call runs on and its effect happens (measured: a 90-second call cut at
// sixty seconds, run to its end here, its answer written on no response). So
// every call this bridge runs starts its answer the moment its tool starts
// ([toolsession.Call.Running]): a progress on the call's own token, another
// every [answerEvery], none once the tool has returned, on every server of
// the opening. A call answered without running is answered at once and gets
// none, and a request with no progress token gets its answer alone: both
// harnesses that dial the bridge give one on every call (measured). A
// progress that can no longer be written is the call's response gone, and the
// call is recorded as one nobody waited for.
//
// # How long a box waits for a call
//
// Kept going, a call keeps its harness waiting for as long as it runs: every
// silence check a harness keeps on a call is answered, so only the harness's
// own tool timeout bounds the wait, which neither sets short (Claude Code's
// 1e8 ms, OpenCode's twelve hours, by reading their bundles). So the box is
// told the bridge's own bound for a call on every server it is handed
// ([Bridge.CallTimeout], rendered as each harness's per-server timeout):
// past it the harness gives the call up and cancels it, whatever progress
// flows, and the call is recorded as one nobody waited for. The bridge
// itself cuts nothing: a call past every bound the engine sets is a defect or
// a hang, and a tool half-way through a write is the run's.
//
// # A run outlives the process that opened its session
//
// The box goes on calling for as long as its harness runs, and the process that
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
// ANY, ON A TOKEN THAT RESOLVES, for every session but one held to this host's
// loopback. The route is authenticated by the signed per-run token in its own
// path, which a page in a browser cannot make, so the SDK's guard against DNS
// rebinding — a call on a loopback address refused under any Host that is not
// loopback — is off ([mcp.StreamableHTTPOptions.DisableLocalhostProtection]),
// and no guard of this package's stands in its place for a seat. That guard
// refused the hop an executor's call commonly takes into a node: a reverse
// proxy or tunnel on the engine host that connects to the node on 127.0.0.1
// and passes the public Host on, which Caddy, cloudflared and ngrok all do
// unless told otherwise, so every call of an e2b executor behind one was
// refused and the run had none of its seat's tools. Nor did it ever hold a
// seat's token to an address: such a token is good wherever the route is
// served, and what bounds one that leaks is its run — it stops resolving the
// moment the run's session closes, and expires in any case ([DefaultTTL]).
//
// A SESSION HELD TO LOOPBACK ([Session.Loopback]) is the one whose token is
// worth nothing anywhere but this host's loopback, and there the Host is
// judged beside the address ([loopbackRefusal]): a call is admitted only from
// loopback, under a loopback host name ([sandbox.LoopbackHost]) or a container
// runtime's name for the engine host ([sandbox.EngineHostName]) — the name
// Docker Desktop and Podman's machine deliver a container box's call under —
// so a page in a browser on this host, under a name its author points at
// loopback, cannot spend a token leaked from such a run either. Judged in the
// routing both routes share ([Bridge.routed]), so the MCP endpoint and the
// run's notes route cannot come to disagree about it, and never on a relayed
// call, because a session held to loopback is never relayed
// ([Bridge.admitRelayed]).
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
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/httprelay"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/panics"
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
// this host lists the same tools under, so the tool prefix a harness sees
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
	// log records from before this opening — zero for a round's first job,
	// and the highest [Call.Seq] the log records for a job opened again
	// after a recovery, or for a job continuing one that asked a person,
	// whose calls the log keeps as the same round's. This opening numbers
	// its calls after it, so the order a resume reads the run's calls in
	// runs on across the recovery or the question rather than starting again
	// at one beside the calls before it.
	LastSeq int

	// Handle and Role name the seat, for the logs and the handshake.
	Handle string
	Role   string

	// Model is the model the run's launch pointed its harness at — the
	// value the launch records on the run's row (sandbox.LaunchRecord.Model)
	// — or "" where it named none and left the harness to choose. Every bridged
	// call carries it as the model it was made on ([toolloop.CallModel]), the
	// value a tool loop gives a call from its round, so a seat's code write
	// made through this bridge names the model its run's other writes name.
	Model string

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

	// Loopback admits a call only from this host's loopback interface,
	// under a loopback host name or a container runtime's name for the
	// engine host ([loopbackRefusal]): a run confined on the engine host
	// dials the engine there, so a token it leaks is worth nothing from
	// anywhere else, nor to a page in a browser on this host under a name
	// its author points at loopback. The one kind of session whose call's
	// Host is judged at all — see the package doc.
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
	// harness between its steps — see steer.go. Closed with the opening that
	// serves it: the run's end, a relaunch that replaces it and the seat
	// leaving this node all close it. Nil serves no notes: the notes route
	// answers 404 and steer_take answers nothing, which is what a run with
	// nothing to steer — the assistant's deep work — has.
	Steer SteerInbox

	// Skills are the skills the run's harness was handed as its OWN
	// (internal/sandbox, RunRequest.Skills), by the name it lists each
	// under to the key of the Tool Skill it is: a load the harness reports
	// by that name is recorded as the run's own load of the key — see
	// skills.go. Nil hands none: the skill route answers 404, skill_loaded
	// records nothing, and load_tool_skill is listed as any other tool.
	Skills map[string]string

	// Started is told which session the job's box started, the first time a
	// request of the job reaches an opening of its session, BEFORE that
	// request is served: fellBack where it came on the job's other token
	// ([Bridge.FreshEndpoint]) — a session of its own its script started in
	// place of the one its launch continued — and false on the endpoint
	// [Bridge.Open] answered. It holds the run's skill guard to that session,
	// so no call of the run is admitted against the guard of a session it is
	// not in — see started.go. Nil tells nobody, which a session whose box
	// is handed one token only (the assistant's deep work) has.
	Started func(ctx context.Context, fellBack bool)

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
	// DigestOf is the identity a call is admitted under in the run's call
	// log ([tools.Surface.DigestOf]), which the record of the call carries
	// ([Call.Digest]); empty for a surface that keeps no call log a later
	// segment is seeded from.
	DigestOf(name string, args map[string]any) string
}

// committing is a [Surface] whose calls commit to an agent's turn: the set
// they commit their changes to, which each call's record carries
// ([Call.Committed]). A seat's *tools.Surface is one; Ask Crewlet's is not.
type committing interface {
	Written() *turnctx.Written
}

// A seat's surface must stay one, or its runs' writes would settle nothing.
var _ committing = (*tools.Surface)(nil)

// writtenOf is the set s's calls commit to, nil (which records nothing) for a
// surface that commits to no agent's turn.
func writtenOf(s Surface) *turnctx.Written {
	if c, ok := s.(committing); ok {
		return c.Written()
	}
	return nil
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
	if seat := s.seat(); seat != nil {
		return seatCaller{surface: seat}
	}
	return nil
}

// seat is the session's surface where it is a seat's, or nil: Ask Crewlet's
// deep work hands the bridge a surface of its own, which acts for a person and
// holds no MCP server's tools.
func (s *Session) seat() *tools.Surface { return seatOf(s.Surface) }

// Ledger is the durable record of a bridged run's calls.
//
// Declared here, by the consumer, and deliberately narrow: this package
// appends and never reads. The reader is the resume path, which has its own
// view of the same rows — see internal/sandbox.
type Ledger interface {
	// Append records one finished call. An error is LOGGED, never
	// returned to the box: the tool already ran and its effect already
	// happened, so failing the call would tell a harness to retry
	// work that has been done.
	Append(ctx context.Context, runID string, call Call) error

	// Settle carries into the run's log whatever calls the ledger still
	// owes it — ones whose own append did not land, which it carries on a
	// later call's — before it returns, under a context and a bound of its
	// own, and reports whether it carried any. The run's collection asks for
	// it through [Bridge.Settle] before the job's calls are read for its
	// resume, since a job's last call has no later call of its own.
	Settle(ctx context.Context, runID string) bool

	// End is Settle as the session's opening ENDS ([Bridge.Close], a
	// relaunch's opening replacing it, [Bridge.ReleaseSeat]): no call of the
	// session comes after that to carry what it owes, and a session that
	// ended with calls owed would leave them off the run's record even where
	// the store answers a second later. AND IT IS THE SESSION'S LAST CARRY OF
	// ITS OWN, made once a Settle still out as the opening ends is done —
	// taking up what that one could not land — and returning only after it:
	// a call still in flight as the opening ends records itself after it, and
	// one whose record then fails is on no record unless a later call of the
	// session carries it — which the ledger says of it, rather than promise a
	// carry that is not coming.
	End(ctx context.Context, runID string) bool
}

// Call is one finished bridged call, as the ledger records it.
type Call struct {
	// Call is the call as the surface records it: the tool, its arguments,
	// what it answered, whether it failed and why.
	tools.Call

	// Seq is the call's place in the order the calls REACHED the session,
	// after [Session.LastSeq] — the order the harness made them in, which is not
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

	// Source is whose tool the call named, answered or refused
	// ([toolloop.ToolResult.Source]), and empty for a call nothing
	// resolved: one made after the run's tools closed, or whose arguments
	// did not parse.
	Source string

	// Held marks a call to a tool at Needs approval that the seat's surface
	// recorded as the run's question rather than made
	// ([toolloop.ToolResult.Held], ADR-0077): the call that ends the job.
	Held bool

	// Digest is the identity the call was ADMITTED under in the run's call
	// log ([tools.Surface.DigestOf]), taken under the lock that orders the
	// admission, before anything records it; empty for a call never admitted
	// (made after the run's tools closed, or whose arguments did not parse).
	// The record of the call carries it so a later segment seeds the log by
	// the identity it counted, whatever that record keeps of the arguments.
	//
	// AND EMPTY FOR A HELD CALL, whose arguments are not logged (Args): a
	// digest of them in the clear beside the tool's name would confirm a
	// guess at what their seal hides (ADR-0077). The record counts such a
	// call as one of its tool whose arguments are not known.
	Digest string

	// Absorbed is the calls the run's call log took in from the call's OWN
	// WORK — a `delegate`'s workers — by identity
	// ([toolloop.ToolResult.Absorbed]): recorded with the call, because no
	// call of the run's record is one of them and a later segment's log must
	// count them all the same.
	Absorbed []toolloop.Called

	// Committed is the kinds of change the call committed to the run's turn
	// (turnctx.Written), which its collection settles the turn's outcome by.
	Committed []types.CommittedChange

	// Abandoned marks a call NOBODY WAITED FOR BY THE TIME IT RETURNED: its
	// harness had given up on it and cancelled its request, or hung up on
	// the response its answer was kept going on, which the next progress
	// found gone ([toolsession.Call.Abandoned]), or the opening it was made
	// on had ended. The call ran and its effect happened, but the session of
	// the job that made it holds it with no outcome or a failed one, so no
	// save point may hold it (sandbox.BridgeCall.Abandoned). False for every
	// call answered without running, which returns at once.
	Abandoned bool
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
// Four hours, matched to the thing it has to outlive: an executor that stops
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
	// a harness legitimately goes quiet for as long as a build or a
	// test suite takes, so an idle clock here would cut a live run off from
	// its tools mid-job for no reason the run could see.
	//
	// NO HOST CHECK, the SDK's included: the token in the path is the
	// route's credential, and the one session whose call's Host is judged
	// — one held to this host's loopback — is judged in the routing in
	// front of this handler ([loopbackRefusal]). See the package doc.
	//
	// A REQUEST'S BODY IS HELD TO WHAT THE RELAY CARRIES ([httprelay.MaxBody]):
	// a call that lands on a node other than its seat's holder is relayed
	// there whole, and one past that is refused before it is sent, so a
	// larger body would be a call served on one node and refused on another.
	// Stated here rather than left to the SDK's own default, which happens
	// to be the same figure today and answers to nothing of the engine's.
	b.streamable = mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		sub, ok := b.parse(r.PathValue("token"))
		if !ok {
			return nil
		}
		opened := b.held(sub)
		if opened == nil {
			return nil
		}
		// THE SERVER THE ROUTE NAMES: the engine's own at the job's endpoint,
		// one of the seat's MCP servers beneath it ([ServerPattern]), and
		// none for a server the opening does not serve.
		ep := opened.endpoint(r.PathValue("server"))
		if ep == nil {
			return nil
		}
		return ep.server.MCP()
	}, &mcp.StreamableHTTPOptions{DisableLocalhostProtection: true, MaxRequestBodyBytes: httprelay.MaxBody})
	return b, nil
}

// loopbackRefusal says why a call may not reach a session held to this host's
// loopback ([Session.Loopback]), or nothing where it may: it must arrive from
// loopback ([FromLoopback]) and name a loopback host ([sandbox.LoopbackHost])
// or one of the names a container runtime gives the engine host inside a box
// ([sandbox.EngineHostName]).
//
// THE ADDRESS, because the hold is what makes such a run's token worth nothing
// from any other machine: read from the connection and never from a header.
//
// THE HOST, because an address on this host's loopback is not yet this host's
// own caller. A page in a browser on the engine host, under a name its author
// points at 127.0.0.1, sends its requests to the loopback listener under that
// page's own name (DNS rebinding), so a token leaked from a held run would be
// spendable by any page a person on the host opens. A process on the host
// can name any Host it likes, and is not what the hold keeps out; a page can
// name only its own.
//
// THE RUNTIME'S NAMES, because a container box dials the engine host by one
// ([sandbox.EngineAddress]), and Docker Desktop and Podman's machine on macOS
// and Windows deliver that call to the host's loopback, under the name the box
// dialled — so a held container run on such a host has its tools. Neither is
// a name a page from the internet is served under: both are in `.internal`,
// reserved for private use, and resolve, where they resolve at all, through
// the runtime.
func loopbackRefusal(r *http.Request) string {
	if !FromLoopback(r) {
		return "the session admits only this host's loopback, and the call came from " + r.RemoteAddr
	}
	host := r.Host
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	}
	if sandbox.LoopbackHost(host) || sandbox.EngineHostName(host) {
		return ""
	}
	return fmt.Sprintf("the session admits only this host's loopback, and the call named "+
		"the host %q, which is neither loopback nor a container runtime's name for the "+
		"engine host", r.Host)
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
	return base + PathPrefix + b.signer.Mint(opened.subject(false), b.ttlOf(s))
}

// FreshEndpoint is the endpoint of s's live opening under the job's OTHER token:
// the one its box is wired with where its script starts a session of its own in
// place of the one its launch continues (internal/sandbox, RunRequest.FreshWiring).
// A request on it reaches the same opening as one on [Bridge.Open]'s, and says
// the box started that session of its own ([Session.Started]). "" where s has
// no live opening, or no base reaches its box — the cases Open answers "" for.
func (b *Bridge) FreshEndpoint(s *Session) string {
	if b == nil || s == nil {
		return ""
	}
	s.mu.Lock()
	opened := s.live
	s.mu.Unlock()
	base := b.Reaches(s.OnHost)
	if opened == nil || base == "" {
		return ""
	}
	return base + PathPrefix + b.signer.Mint(opened.subject(true), b.ttlOf(s))
}

// ttlOf is how long a token minted for s's opening lasts: the bridge's own
// bound, or the session's where it is shorter ([Session.TTL]).
func (b *Bridge) ttlOf(s *Session) time.Duration {
	if s.TTL > 0 && s.TTL < b.ttl {
		return s.TTL
	}
	return b.ttl
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

// freshMark closes the first part of the subject of a job's OTHER token: the
// one its box dials where its script started a session of its own in place of
// the one its launch continues ([Bridge.FreshEndpoint]). So the session a box
// started is IN THE TOKEN, under the fleet's signature, and nothing the run's
// model writes can change it — see started.go.
//
// ON THE JOB'S PART rather than as a part of its own, so a node of an older
// build, which reads no mark, still finds the run's seat in the route that
// follows and relays the call to the seat's holder as it relays any. "_" is
// unreserved, and refused in a job's id ([validLaunch]), so the mark is never
// part of one.
const freshMark = "_fresh"

// validLaunch reports whether a job id can be the first part of a token's
// subject: set, and free of the two characters that delimit a token's parts and
// of the one that opens the mark of a job's other token ([freshMark]).
func validLaunch(launch string) bool {
	return launch != "" && !strings.ContainsAny(launch, subjectSep+"._")
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
//
// AND IT RETURNS ONCE THE SESSION'S LEDGER HAS CARRIED WHAT IT OWES
// ([Ledger.End]), bounded by the ledger: the paths that end a run go on to
// read its record, or to delete it.
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

// Settle asks the session serving job `launch` of a run to carry into the run's
// log whatever calls it still owes ([Ledger.Settle]), and reports whether it
// carried any. For the run's collection, which asks before the job's calls are
// read for its resume: the job is over, so no call of its own will carry them,
// and its session has not ended yet — a run's session lives until its run
// settles or its next job replaces it, both after the calls are read.
//
// THE JOB'S OWN SESSION, or nothing: a run whose opening on this node serves
// another job — its next one — or none at all owes this job nothing here.
func (b *Bridge) Settle(ctx context.Context, runID, launch string) bool {
	if b == nil {
		return false
	}
	b.mu.RLock()
	opened := b.runs[runID]
	b.mu.RUnlock()
	if opened == nil || opened.id != launch || opened.session.Ledger == nil {
		return false
	}
	return opened.session.Ledger.Settle(ctx, runID)
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
// session lives, and the run — and, on the job's other token, that its box
// started a session of its own in place of the one its launch continued
// ([freshMark]).
type subject struct {
	launch string
	fresh  bool
	route  route
	run    string
}

func (s subject) String() string {
	job := s.launch
	if s.fresh {
		job += freshMark
	}
	return job + subjectSep + s.route.String() + subjectSep + s.run
}

// parse verifies a token — this fleet signed it, and it has not expired — and
// reads its subject. False for anything else, which is all a forged,
// malformed and expired token have in common that this route may act on.
func (b *Bridge) parse(token string) (subject, bool) {
	raw := b.signer.Validate(token)
	if raw == "" {
		return subject{}, false
	}
	job, rest, ok := strings.Cut(raw, subjectSep)
	if !ok {
		return subject{}, false
	}
	launch, fresh := strings.CutSuffix(job, freshMark)
	routeText, run, ok := strings.Cut(rest, subjectSep)
	if !ok || run == "" || !validLaunch(launch) {
		return subject{}, false
	}
	where, ok := parseRoute(routeText)
	if !ok {
		return subject{}, false
	}
	return subject{launch: launch, fresh: fresh, route: where, run: run}, true
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
		return sub.run, "this node holds a DIFFERENT job's session for that run: " +
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
//
// LOOPBACK AS [sandbox.LoopbackHost] READS IT — `localhost` or a name under
// it, in any case, or a loopback address in either family — because that is
// the predicate that hands a container box the runtime's name for the engine
// host in this base's place ([sandbox.EngineAddress]), and the one the hold
// judges a call's Host by. A narrower one here moved a container run on a
// base such as `http://engine.localhost:8000` to the runtime's name without
// holding it to loopback.
func (b *Bridge) LoopbackBase() bool {
	if b == nil {
		return false
	}
	u, err := url.Parse(b.base)
	if err != nil {
		return false
	}
	return sandbox.LoopbackHost(u.Hostname())
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
	// SIDE BY SIDE: each ending waits on its ledger's carry of what it owes
	// ([Ledger.End]), which a store in an outage holds to the ledger's
	// bound, and a seat leaving this node waits on the slowest of them rather
	// than on their sum.
	var ended sync.WaitGroup
	for _, opened := range released {
		ended.Go(opened.end)
	}
	ended.Wait()
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
		// A LOOPBACK SESSION ADMITS THIS HOST'S LOOPBACK ALONE, under a
		// loopback name or a runtime's for the engine host
		// ([loopbackRefusal]), and is never forwarded: a call that reached
		// another node is refused by this one's relay ([Bridge.admitRelayed]).
		// Every other session takes any Host its token arrives under.
		if opened != nil && opened.session.Loopback {
			if reason := loopbackRefusal(r); reason != "" {
				b.refuse(w, r, sub.run, reason)
				return
			}
		}
		if opened != nil && fence(opened.session) == nil {
			b.serveBegun(w, r, sub, opened, serve)
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
			b.serveBegun(w, r, sub, opened, serve)
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
		b.serveBegun(w, r, sub, opened, serve)
	})
}

// serveBegun serves a request on the opening its token names once the opening
// is held to the session the job's box started ([binding.begin]) — the one
// step every route under the prefix takes before it is served, here or relayed
// to the node holding the opening — and refuses one on the job's other token
// once the box has said which it started ([otherSession]).
func (b *Bridge) serveBegun(w http.ResponseWriter, r *http.Request, sub subject, opened *binding,
	serve func(http.ResponseWriter, *http.Request, *binding)) {

	//nolint:contextcheck // the session is told under the run's own context, as every call of the run is made, never the request's (started.go)
	if !opened.begin(sub) {
		b.refuse(w, r, sub.run, otherSession)
		return
	}
	serve(w, r, opened)
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
	for _, ep := range opened.order {
		for range ep.server.MCP().Sessions() {
			n++
		}
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
	// servers are the MCP servers the opening serves, by the name a route
	// names each by: "" for the engine's own tools, and each of the seat's
	// MCP servers under its own name (servers.go). order is the same, the
	// engine's own first. Set once as the opening is built, and read-only
	// after.
	servers map[string]*endpoint
	order   []*endpoint
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

	// unsubstituted says once, per opening, that the harness's hooks name no
	// agent the bridge can tell apart — see [binding.takeSteer].
	unsubstituted sync.Once

	// skillUnsubstituted says once, per opening, that the harness's hook names
	// no skill it loaded — see [binding.loadSkill].
	skillUnsubstituted sync.Once

	// startMu holds every request of the opening behind the first, while the
	// session is told which session the job's box started; started says it
	// has been, and startedFresh that it was a session of the box's own — see
	// [binding.begin]. A mutex of its own, never mu, which every call takes:
	// what the session is told can write the run's record.
	startMu      sync.Mutex
	started      bool
	startedFresh bool
}

var _ toolsession.Dispatcher = (*binding)(nil)

// answerEvery is how often a running call's answer is kept going, a
// `notifications/progress` on the call's own token, after the one written the
// moment it starts running ([toolsession.Server.KeepAnswersGoing]).
//
// TWENTY SECONDS, held to the watchdogs Claude Code keeps on an HTTP call,
// which are what the beat answers (2.1.310: the first-byte budget measured,
// the rest by reading its bundle), none of which cancels the call it gives up:
//   - a response's FIRST BYTE within a minute where the server's entry sets no
//     timeout, and within the entry's timeout where it does: met by the first
//     progress, at once, whatever the cadence — what keeps a call past that
//     minute on a release that does not read the entry's timeout
//     ([Bridge.CallTimeout]) — once the call has reached the node that runs
//     it: one that first waits for a node that can answer it
//     ([FleetOptions.HolderWait]) has nothing written on its response while it
//     waits;
//   - an IDLE CHECK that gives up a call that "sent no response or progress"
//     for five minutes on HTTP where the entry sets no timeout, and for the
//     entry's timeout where it does: every progress starts it again;
//   - a TRANSPORT-LOSS CHECK, run every 30 seconds, that gives up a call whose
//     transport was marked lost more than 90 seconds before, unless a
//     progress has cleared the mark since. The mark is routine: Claude Code
//     loses the session's standing GET stream 360 seconds after it opens it,
//     with the entry's timeout and without (measured), counts that a terminal
//     connection error, and marks every call then in flight (by reading its
//     bundle). A cadence of twenty puts four beats inside the ninety, any one
//     of which clears the mark, so one or two delayed on a loaded node or a
//     relayed frame still clear it, and each 30-second check sees a beat no
//     older than twenty.
//
// And a proxy between a box and the node that drops a response silent for
// longer than its own idle bound sees none silent for longer than the cadence.
// Measured on 2.1.310 against this bridge, at this cadence: a 90-second call
// answered whole with no timeout on the server's entry, the minute's budget
// met by the first progress; a 90-second, a 330-second and a 540-second call
// answered whole with the bridge's own, the last through the loss of its
// session's GET stream at 359.2 seconds; and the same 540-second call with no
// progress token, on which nothing is kept going, given up at 449.9 seconds,
// 90 after that loss, with no cancel. OpenCode keeps no silence check on a
// call (2.1.4, measured; its timeout is absolute), and reads a progress
// harmlessly.
//
// WHAT A BEAT COSTS: one SSE event and a flush per running call (116 bytes on
// a progress token of one digit, 117 from the tenth beat on, measured), and
// for a call relayed from another node one broker round trip a frame, half
// the rate of the relay's own heartbeat on every open stream
// ([httprelay.Heartbeat]); and a goroutine and a ticker per running call.
// Not a setting: nothing about a deployment moves the harnesses' clocks.
const answerEvery = 20 * time.Second

func newBinding(ctx context.Context, s *Session, where route) *binding {
	opened := &binding{id: s.Launch, session: s, caller: s.caller(), route: where,
		ctx: context.WithoutCancel(ctx), arrived: s.LastSeq, steer: s.Steer}
	// EVERY ENDPOINT IS FILED BEFORE ANY SERVER IS BUILT: a server lists its
	// tools as it is built, and which endpoint lists a tool is read off the
	// map — a server built before its siblings were filed would list theirs,
	// and keep listing them until the surface next changed.
	names := Servers(s.Surface)
	opened.servers = make(map[string]*endpoint, 1+len(names))
	for _, name := range append([]string{""}, names...) {
		ep := &endpoint{opening: opened, name: name}
		opened.servers[name] = ep
		opened.order = append(opened.order, ep)
	}
	title := s.Role + " (" + s.Handle + ")"
	for _, ep := range opened.order {
		instructions := ""
		if ep.name != "" {
			instructions = instructionsOf(s.seat(), ep.name)
		}
		ep.server = toolsession.NewServer(ep.listedName(), title, instructions, ep)
		// EVERY RUNNING CALL'S ANSWER STARTS AT ONCE AND KEEPS GOING, on
		// every server of the opening ([answerEvery]): a seat's MCP server's
		// call can take as long as the engine's own.
		ep.server.KeepAnswersGoing(answerEvery)
	}
	own := opened.servers[""].server
	// steer_take, ANSWERED BEFORE THE TOOLS ARE: never listed, never handed
	// to the run's surface, never recorded — see steer.go. On the engine's
	// own server, where the run's hooks name it.
	own.MCP().AddReceivingMiddleware(opened.steerMiddleware)
	// skill_loaded, ANSWERED THE SAME WAY, for the same reason: a hook's
	// report, never listed and never a call of the model's — see skills.go.
	own.MCP().AddReceivingMiddleware(opened.skillMiddleware)
	// THE ANSWER TO AN ACTIVATION WAITS FOR THE BOX'S RE-LIST
	// ([toolsession.Server.HoldAnswersForRelist]): the harnesses that run in
	// a box (Claude Code, OpenCode) re-read their tool list when told it
	// changed, and answered at once one asks its model again before the
	// re-list lands, offering the tool it just activated a request late.
	// Unconditional, because the bridge is told nothing of which harness dials
	// it, and the cost on one that never re-lists is the hold's own short
	// bound on an activation's answer, never a refusal.
	own.HoldAnswersForRelist()
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
//
// fresh names the job's OTHER token ([Bridge.FreshEndpoint]), the one its box
// dials where it started a session of its own in place of the one its launch
// continued.
func (o *binding) subject(fresh bool) string {
	return subject{launch: o.id, fresh: fresh, route: o.route, run: o.session.RunID}.String()
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
//
// AND IT ENDS THE SESSION'S LEDGER ([Ledger.End]), which carries what it owes,
// before it returns, once, by the call that ended it: no call of the opening
// comes after its end to carry what the ledger owes, and every path that ends
// one — the run's settle, a stop, a relaunch's opening, the seat leaving this
// node — goes on to read the run's record, delete it, or hand the run to a node
// whose ledger owes it nothing. Last, once the opening refuses every new call
// and its box's sessions are closed, so nothing it owes is still to come but
// what a call already in flight records itself — which the ledger, told the
// session ended, says is on no record where it does not land.
func (o *binding) endKeeping(keep SteerInbox) {
	if o == nil {
		return
	}
	o.mu.Lock()
	first := !o.ended
	o.ended = true
	inbox := o.steer
	o.steer = nil
	o.mu.Unlock()
	if inbox != nil && inbox != keep {
		inbox.Close()
	}
	for _, ep := range o.order {
		for ss := range ep.server.MCP().Sessions() {
			if err := ss.Close(); err != nil {
				log.Debug("mcp_bridge_session_close", "error", err)
			}
		}
	}
	if first && o.session.Ledger != nil {
		o.session.Ledger.End(o.ctx, o.session.RunID)
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
//
// The bridge's own skill loader is left out for a harness that loads its
// skills itself ([binding.offeredTool]).
func (o *binding) Offered() []toolsession.Tool {
	surface := o.session.Surface
	defs := surface.ToolDefs()
	out := make([]toolsession.Tool, 0, len(defs))
	for _, def := range defs {
		if !o.offeredTool(def.Name) {
			continue
		}
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

	// stoppedNote answers a call once the executor's tools are closed: the
	// seat moved off this node, a person asked it to stop, or a tool failed
	// inside the engine.
	stoppedNote = "Your tools are closed and this call was not run. " +
		"End your turn now."

	// unrunNote answers a call the seat's surface could not run at all,
	// because the turn the executor works for was torn down under it.
	unrunNote = "This call was not run, because the turn you are working for " +
		"has ended. End your turn now."

	// failedInsideNote answers the call that broke inside the engine.
	failedInsideNote = "This call failed inside the engine, and your tools are " +
		"now closed. End your turn now."

	// suspendNote answers a tool that would park the caller's own loop,
	// which an executor in its box does not have.
	suspendNote = "This tool cannot be used from inside a box; it suspends the " +
		"caller's own loop."
)

// Call decides and runs one call a box made. It never fails as a Go error: a
// call that could not run is a failed result the harness hands its model.
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
	// orders admissions — see [tools.Surface.Admit] — and the identity it is
	// counted by there, which the record of the call carries.
	admitted := s.Surface.Admit(in.Name, args)
	digest := s.Surface.DigestOf(in.Name, args)
	o.mu.Unlock()

	// ITS ANSWER STARTS HERE, past every answer given without running it, and
	// is kept going while its tool runs ([toolsession.Call.Running]): a call
	// the surface goes on to refuse or hold is answered a moment after its
	// first progress, and telling those apart here would be this package's
	// own idea of the surface's gates. Nil for a call no request carries.
	if in.Running != nil {
		in.Running()
	}

	written := writtenOf(s.Surface)
	before := written.Changes()
	started := time.Now()
	res, broke, err := o.execute(admitted, in.Name, args)
	took := time.Since(started)
	switch {
	case broke != nil:
		log.ErrorContext(o.ctx, "mcp_bridge_tool_panicked", "run_id", s.RunID,
			"seat", s.Handle, "tool", in.Name, "panic", broke.Value, "stack", broke.Stack)
		o.stop(broke)
		res = toolloop.ToolResult{Output: failedInsideNote, Failed: true,
			Cause: tools.RefusalInternalError, Absorbed: res.Absorbed}
	case err != nil:
		// The surface returns an error only for a call that could not run
		// at all — nothing was recorded on it, and nothing is here: the
		// call did not happen as far as any record says.
		//
		// IN WORDS OF ITS OWN, never the error's: the caller is the harness
		// in the box, a model, and the error is a Go error's text
		// ("toolloop: context canceled") it can do nothing with. The error
		// goes to this node's log.
		log.ErrorContext(o.ctx, "mcp_bridge_dispatch_failed",
			"run_id", s.RunID, "seat", s.Handle, "tool", in.Name, "error", err)
		return refused(unrunNote)
	case res.Suspend:
		// A SUSPEND CANNOT CROSS THIS BOUNDARY. It parks a tool loop with
		// the call unanswered so the conversation can be persisted and
		// re-entered; a detached run's loop is its harness's own, which would
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
			Cause: tools.RefusalForbidden, Origin: res.Origin, Server: res.Server,
			Source: res.Source, Absorbed: res.Absorbed}
	}

	// AND AGAIN ONCE THE TOOL HAS RETURNED. Its answer is delivered — it
	// ran, and what it did is done — but nothing after it runs as a seat
	// this node no longer holds.
	if after := fence(s); after != nil {
		o.stop(after)
	}
	// WHETHER ANYBODY STILL WAITS FOR THE ANSWER, read as the tool returns and
	// before the record: the harness cancelled its request, a progress kept
	// going on its response found the response gone, or the opening ended and
	// its session is closed behind it ([Call.Abandoned]). A harness that hung
	// up on its POST is seen at the next progress after it, and only then: a
	// request this transport carries runs on when its harness hangs up, so a
	// hang-up in the last cadence before the tool returns is not seen here.
	abandoned := gone(in.Abandoned) || o.over()
	recorded := args
	if res.Held {
		// A HELD CALL'S ARGUMENTS ARE NOT LOGGED: they ride the run's row
		// sealed beside its question (ADR-0077), and the call log is
		// read in the clear by everybody who reads the seat's trail. NOR
		// THEIR DIGEST, which would let anyone reading the record confirm
		// a guess at them: the record counts the call as one of its tool
		// whose arguments are not known.
		recorded, digest = nil, ""
	}
	output := res.Output
	if res.Unkept {
		output = tools.UnkeptOutput(output)
	}
	o.record(Call{
		Call: tools.Call{Name: in.Name, Args: recorded, Output: output, Failed: res.Failed,
			Cause: res.Cause},
		Seq: seq, StartedAt: started.UTC(), Duration: took,
		Origin: res.Origin, Server: res.Server, Source: res.Source, Held: res.Held,
		Digest: digest, Absorbed: res.Absorbed, Committed: written.Since(before),
		Abandoned: abandoned,
	})
	return toolsession.Result{Output: res.Output, Failed: res.Failed}
}

// execute runs one admitted call as the run's seat, under the run's context,
// and contains a panic — broke is it, recovered.
func (o *binding) execute(admitted toolloop.Admitted, name string, args map[string]any,
) (res toolloop.ToolResult, broke *turn.PanicError, err error) {
	defer func() {
		if v := recover(); v != nil {
			// WHAT THE CALL'S OWN WORK HAD RECORDED by the time it broke, which
			// the run's log holds all the same, kept for the call's record
			// ([toolloop.CallPanic]); the panic read as the call raised it.
			value, absorbed := toolloop.PanicValue(v)
			broke = turn.Recovered(panics.ScopeToolCall, value)
			res, err = toolloop.ToolResult{Absorbed: absorbed}, nil
		}
	}()
	// THE SESSION'S JOB rides the call, so a tool that writes the run's own
	// row (a question it parks on) writes the job that made the call, or
	// nothing ([turnctx.WithJob]).
	//
	// AND THE MODEL THE CALL WAS MADE ON, set here and nowhere else on this
	// path: a harness's own loop made it, and the one model the engine
	// knows that loop runs is the one the run's launch named
	// ([Session.Model]).
	ctx := turnctx.WithJob(toolloop.WithCallModel(o.caller.Acting(o.ctx), o.session.Model),
		o.session.Launch)
	res, err = admitted(ctx, llm.ToolCall{Name: name, Arguments: args})
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
			"harness is told to end its turn")
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
// NEVER FAILED BACK TO THE BOX, and the reason is that the alternative is
// worse. The tool has already run and its effect has already happened, so
// failing the call would tell a harness to retry work that is done — the
// duplicate-post failure the whole delivery machinery exists to prevent. The
// ledger does what it can before the answer goes back — the engine's tries the
// append again, and owes a call it still could not record to the session's
// later appends and its carries ([Ledger.Settle], [Ledger.End]), whole — and an
// error it reports here is logged: a call no record holds costs the reviewer a
// line of evidence and a resumed segment's call log a count, where a duplicated
// side effect costs somebody two identical messages.
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

// gone reports whether abandoned is closed now: the harness has stopped
// waiting for the call ([toolsession.Call.Abandoned]). A nil channel, a call no
// request carries, is never gone.
func gone(abandoned <-chan struct{}) bool {
	select {
	case <-abandoned:
		return true
	default:
		return false
	}
}

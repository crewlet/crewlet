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
//     operation id carries does not depend on which tool finished first. A
//     call is admitted once its entry is on the run's record (below), and
//     only once the call that arrived before it has been admitted or refused,
//     so the order is still the order the calls arrived in, whichever entry's
//     write came back first.
//   - A CALL IS THE RUN'S, NOT THE REQUEST'S. It runs under the context the
//     run was launched with, stripped of that context's cancellation (the
//     launching turn has suspended), and never under the HTTP request's,
//     which the session does not even hand over: a box that disconnects
//     mid-call abandons only the answer, never a tool half-way through a
//     write. And the record says so ([Call.Abandoned]): a call whose
//     harness had given up on it by the time it returned — cancelled it, or
//     hung up on the response its answer was kept going on (below) — or
//     that its opening's end answered as still running (below) is one its
//     job's session holds with no outcome, a failed one or that one, though
//     it ran.
//   - A PANIC IS CONTAINED. A tool that panics is answered as failed, said in
//     the log with its stack, and the run's tools are closed to it: the
//     process stays up, and a defect does not run again on the next call.
//   - A RUN THAT IS OVER REFUSES. A call that reaches a session after its run
//     ended, or after a relaunch replaced it, is answered and not run; one
//     that reaches it after the seat left this node, or after a recovery
//     opened its job again, is told to make the call again, which reaches
//     the opening that serves its box now.
//   - A CREDENTIAL A BOX MAY HOLD. The endpoint is per-job, its token expires
//     with the run, and nothing secret enters the box — see internal/runtoken.
//   - THE MODEL A CALL WAS MADE ON. A tool loop tells each call its round's
//     model; a bridged call was made by a harness's own loop, whose rounds
//     the engine never sees, so each is told the model the run's launch
//     pointed that harness at ([Session.Model]) — the one a seat's code write
//     names on its push, so a run's pushes name one model whichever path
//     made them.
//   - A DURABLE LEDGER. A run outlives its launching turn and can outlive the
//     process, so every call is written where a resume can read it back —
//     its identity BEFORE it runs, and with when it ran, how long it took and
//     who answered it once it returns (below, "A call is on its run's record
//     before it runs").
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
// # A call is on its run's record before it runs
//
// A call that ran with no record of it is one no later segment of its run can
// count, and a later call that asks for something else then derives its
// operation id again: a tool keyed on it takes the new call for a retry of the
// old one, or the other way round. A call recorded only once it returned met
// that wherever the coordination store refused its record for longer than its
// job's session lasted. So the bridge WRITES EACH CALL'S ENTRY AHEAD OF IT
// ([Ledger.Begin]) — its identity ([Begun]): its tool, its place, the instant
// the bridge took it, the digest it was admitted under where its tool can
// never ask a person first, and the instant past which nobody waits for it;
// and beside it the id its harness gave it, which is no part of that identity
// ([Begun.CallID], "A call carries the id its harness gave it" below) — on the
// call's own goroutine, and RUNS THE CALL ONLY ONCE THE ENTRY IS TAKEN
// ([binding.writeAhead]). Its one update then records what it did
// ([binding.conclude], [Ledger.Record]), found by that identity.
//
// AND THE CALL IS IN THE RUN'S CALL LOG ONLY ONCE ITS ENTRY IS: it is admitted
// there ([tools.Surface.Admit]) as it is decided to run, never as it arrives.
// The call log is what a call's operation id counts its repeats from
// (internal/agent/turnctx's CallLog), and a later segment of the run seeds its
// own from the run's record alone: so a call this segment's log counted and no
// record holds is the very under-count the entry exists to close — a call
// admitted as it arrived and then refused at its entry raised the next call's
// count here and nowhere else, and a resumed segment derived, for a new call,
// an operation id a call of this one had already used.
//
// A CALL WHOSE ENTRY CANNOT BE WRITTEN IS NOT RUN: answered, FAILED, that it
// could not be recorded and may be made again ([unrecordedNote]) — a store that
// refused every attempt within the ledger's bound, or a log holding as many
// calls in flight as it keeps — or, where the log takes no more calls of the
// job, that its run has ended ([endedNote]). Nothing ran, so making it again is
// safe. A call whose entry was written and that is then not run — its opening
// ended while the entry was written, its seat's fence found closed after it —
// has the entry withdrawn in the same step ([Ledger.Withdraw]), so nothing
// waits on a call nobody runs; it was never admitted, so no call log counts it
// either. One the call log admitted and the surface then could not run has its
// entry updated to a call that was not run, failed, so the record keeps the
// identity the log counted.
//
// AN UPDATE THE STORE REFUSES is asked for again: the ledger's own attempts
// first, before the call's answer goes back, and past them the same update
// again once the answer is out, spaced from a second to five, until it lands or
// the call's own bound passes ([binding.recordOwed]). So a store's outage
// delays what the call answered, cost and committed — a submission among them —
// and loses it only where the store does not come back before the call's bound,
// past which no collection waits for it: the entry then stands, its identity on
// the record and its outcome unknown, which every later reader of the run reads
// as a call whose outcome is unknown (internal/sandbox's BridgeLog.InFlight).
//
// WHAT IT COSTS is one coordination-store write before every bridged call runs
// — one compare-and-swap of the run's log, measured on the engine's own
// embedded broker of three members from a node that does not lead the runs'
// bucket, with the log at its full count of calls, at 11.89 ms at the median
// and 17.04 ms at the 99th percentile over a thousand writes — after
// which the call's update is the one write a call made before. A call made
// beside others waits, to be admitted, for the entries of the calls that
// arrived before it, each written at the same time as its own. And a store in
// an outage now refuses the calls it used to let run off the record, and its
// refused updates are asked for again, one ask of a call's about every seven
// seconds while it is out.
//
// # A call carries the id its harness gave it
//
// A harness names each call it makes: Claude Code hands the server the model's
// tool_use id in the request's `_meta`, under `claudecode/toolUseId` — the id
// its session's transcript files the call's use and its result under, on its
// main thread and in its subagents alike, over this transport, a tool its tool
// search loaded included (measured on 2.1.310). The bridge reads it as the call
// arrives, under the key the box's runner declares and no other
// ([Session.CallIDMeta], [Session.callIDOf]), and records it on the call's
// entry and on its one update alike ([Begun.CallID], [Call.CallID]), on every
// path that records the call — one that ran, one refused before its tool ran,
// one the surface could not run, one held for a person, one whose arguments did
// not parse, one made once the run's tools had closed, and one whose run's
// tools closed while its entry was being written — so the run's log names each
// call as its harness's session does. ONLY AN ID THE LOG KEEPS WHOLE IS
// TAKEN (sandbox.ValidCallID): a value that is not a string, or an id past the
// bound or outside its characters, is recorded as none, NEVER CUT, since two
// ids cut alike would name two calls as one.
//
// THE BOX'S WORD, AND NO PART OF THE CALL'S IDENTITY: the update finds its
// entry by the call's place, its instant and its tool, as before, and two
// calls under one id are two calls — a harness that makes a call again may
// send the id it sent the first time. A harness whose runner declares no key
// (OpenCode, whose `_meta` carries no id of the call) has every call recorded
// with none, and so does a skill load its hook reports, which is the bridge's
// own call of the loader ([binding.loadSkill]).
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
// # An opening ends without waiting for its calls
//
// Every way an opening ends — the run's settle, a stop, a reclaim, its cap's
// cut, or its launch failing ([Bridge.Close]); a relaunch's opening of another
// job, or a recovery's of the same one ([Bridge.Open]); the seat leaving this
// node ([Bridge.ReleaseSeat]) — closes the MCP sessions a box holds on it, and
// the SDK's close of a session waits for every request in flight on it and
// writes no answer once it has begun (go-sdk v1.8.0). So an ending that closed
// first waited out every call still running, and that call's answer was never
// written: measured with the SDK's own client as the box, a close and a
// relaunch's opening each returned only once the held call's tool did, and the
// box read "request terminated without response"; the seat leaving this node,
// and a recovery's opening, had not returned when the measurement stopped
// waiting — and a node gives a seat's lease back only once its release of the
// seat has returned.
//
// AN ENDING ANSWERS EVERY CALL STILL RUNNING, THEN CLOSES. Under the opening's
// lock it refuses every later call and answers each call it took whose tool
// has not returned, in the words of how it ended ([ending]): where the run is
// over for the box — a close, a relaunch's opening — that the run has ended,
// the call is still running and must not be made again, and the turn should
// end; where the box goes on — the seat leaving, a recovery — that the call is
// still running and must not be made again. NOT A FAILURE, as the tool loop's
// own "still running" is not (internal/agent/toolloop): a harness hands its
// model a failed result as a call that failed, and a model makes a failed call
// again, where this one is running and its effect is still to come. The call's
// handler stops its beat and waits for it, as before any answer, so no progress
// follows. The ending waits for each answer it gave to be out
// ([toolsession.Call.Over]); for the answer of a call whose tool returned just
// before it, which its own result answers, once its record is made; and for
// every answer the opening gave without running a call, the refusal of one that
// reached it after its end among them — for at most [endAnswerWait], and only
// then closes the sessions, which by then hold no request a tool is behind: so
// it returns once those answers are out, which a box that reads takes no time
// over, and a seat leaving this node gives its lease back then rather than once
// the call returns. A call that reaches the opening as that close begins is
// refused all the same, and its box reads the close's failure in place of that
// refusal: the SDK writes no answer once a close has begun.
//
// A HOOK'S ANSWER IS ONE OF THOSE: steer_take's and skill_loaded's are given
// by the opening's own server, in a middleware rather than through a call, and
// the ending waits for each as it waits for a refusal ([binding.holdHook]). A
// take of the run's notes whose write is on the run's record, answered into
// the close, would leave the notes handed to a step that never had them, with
// no cancel to say so; and a hook's call that fails costs its run the bridge,
// which its harness drops and re-initialises.
//
// AND SO IS A REQUEST THAT OPENED A SESSION ON THE OPENING before its end
// ([binding.holdSessionOpen]): the SDK opens the session as it serves the
// request, so the ending waits for the MCP handler to return the request,
// whatever became of its box's connection, before it closes the box's
// sessions, and that session is one of those it closes. A request that opens a
// session once the opening has ended is never served there: its routing found
// the opening a moment before, and it is turned away to ask again
// ([Bridge.serveMCP]), since a session opened on an ended opening is one no
// ending closes.
//
// PAST THAT BOUND THE ENDING DOES NOT WAIT: an answer still on its way is one a
// box is not reading, and a box that does not read must not hold an ending. It
// ends the opening's ledger and returns, and the bridge closes the sessions
// behind it once those answers are out: once the box reads them, or its
// connection ends — at the latest when its box is reclaimed, or this node's
// listener closes its connections as the node stops. An answer whose write
// fails is one its box never reads: its harness sees the call's request fail,
// and the call is recorded as nobody waited for it, as below.
//
// THE CALL'S TOOL RUNS ON TO ITS OWN END, and the call's one update is made
// once it returns ([binding.conclude]) — after the opening ended — as one
// nobody waited for, since its harness was handed the ending's answer and not
// the tool's. ITS IDENTITY IS ON THE RUN'S LOG FROM BEFORE IT RAN; WHAT LANDS
// LATER IS ITS OUTCOME, AND ONLY WHERE THE RUN'S LOG STILL TAKES IT: every path
// that ends a run goes on, not waiting for the call, to read the run's log and
// delete its record — a stop's collection, the cap's, a reclaim's, the run's
// settle — so a read before the update holds the call as one still in flight,
// what it committed in no outcome, and an update after the delete finds no
// record of the run, though the call's effect happened. A relaunch's job takes
// it as a late call of the round, and a run whose box goes on takes it as its
// own while its record stands. WHO WAITS FOR IT IS THE
// BRIDGE, under the run's context, which nothing cancels, until the call's
// own bound ends it — a seat MCP server's request timeout, `delegate`'s call
// cap, a builtin's own (wait.go): [binding.Call] starts the goroutine that
// runs the call's tool as it admits the call, the bridge holds it in its
// registry ([bridgedCalls]) until the call is recorded, which
// [Bridge.AwaitCalls] waits for, and the tool's return ends it.
// [Bridge.Shutdown] leaves it running; a node's drain, once it has given its
// seats back, waits for it (internal/engine's Engine.Drain); and a process
// killed first ends it with the process, its entry left in flight, the call
// on the record by its identity and its outcome unknown.
//
// WHAT A LATER RUN OF THE WORK IS TOLD. A call so recorded is held by no save
// point, so every continuation of its round built after it landed is told its
// result from the run's log (ADR-0073), while the session that made it holds
// the ending's answer, never the call's output.
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
	"github.com/crewlet/crewlet/internal/backoff"
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

	// CallIDMeta is the key of a call's `_meta` the box's harness hands its own
	// id for the call under, as the runner that starts the box declares it
	// (sandbox.OpenedJob.CallIDMeta): the one key every call's id is read by
	// ([Call.CallID]). EMPTY READS NONE, whatever the box sends under any key,
	// the empty one included: a session that declares no key reads every
	// call's id as none — a test's, or the assistant's deep work's, whose
	// harness names its calls but whose session records none of them, with no
	// [Session.Ledger] to record them on.
	CallIDMeta string

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
// writes and never reads. The reader is the resume path, which has its own
// view of the same rows — see internal/sandbox.
//
// ONE RECORD A CALL, IN TWO STEPS: its entry, written before it runs (Begin),
// and its one update as it finishes (Record) — or, where it was then not run,
// the entry's withdrawal (Withdraw). See the package doc, "A call is on its
// run's record before it runs".
type Ledger interface {
	// Begin writes the call's ENTRY on the run's log before its tool runs:
	// its identity, and beside it the id its harness gave it ([Begun]). The
	// call runs only once this answered nil. [ErrNotTaken] is a log that no
	// longer takes the calls of this job — its run ended, or a relaunch
	// replaced the job — and any other error a write the ledger could not
	// land within its own bound; either way the call is not run, and its box
	// is told so.
	Begin(ctx context.Context, runID string, call Begun) error

	// Record is a finished call's ONE UPDATE: its entry, found by its
	// identity ([Begun]), taken out and the call listed in its place, with
	// what it answered, what it cost and what it committed; and, for a call
	// answered without running, which wrote no entry, the call listed as it
	// is. An error is LOGGED, never returned to the box: the tool already ran
	// and its effect already happened, so failing the call would tell a
	// harness to retry work that has been done — and the call's entry stands,
	// its identity on the record. [ErrUnrecorded] is an update the ledger
	// could not land within its own attempts and the run's log may still
	// take: the bridge asks for the same update again, once the call's answer
	// is out, until it lands or the call's bound passes, so asked again a
	// ledger lands nothing twice. Nil is an update taken, or one the run's
	// log no longer takes — its round over, its record gone — which the
	// ledger says where it is.
	Record(ctx context.Context, runID string, call Call) error

	// Withdraw takes back the entry of a call that, its entry written, was
	// then not run: its opening ended before it began, its seat's fence
	// closed meanwhile. An error is logged: the entry stands, a call whose
	// outcome a reader cannot know, until its bound.
	Withdraw(ctx context.Context, runID string, call Begun) error
}

// ErrNotTaken is what a [Ledger] answers a [Ledger.Begin] with where the run's
// log no longer takes calls of the session's job: the run ended and its record
// with it, a relaunch replaced the job, or its round is over. Declared here, by
// the consumer, which tells it from a write that did not land: the call it
// answers is told the run has ended for its box ([endedNote]), not to make it
// again.
var ErrNotTaken = errors.New("mcpbridge: the run's log takes no more calls of this job")

// ErrUnrecorded is what a [Ledger] answers a [Ledger.Record] with where the
// call's update did not land within the ledger's own attempts and the run's log
// may still take it: the coordination store refused every one. Declared here,
// by the consumer, which asks for the update again once the call's answer is
// out ([binding.recordOwed]), and tells it from an update the log no longer
// takes, which is nil.
var ErrUnrecorded = errors.New("mcpbridge: the call's update did not land, and the run's log may still take it")

// Begun is a call's IDENTITY as its entry is written ahead of it
// ([Ledger.Begin]), and what its update and its withdrawal find the entry by:
// its tool, its place and the instant the bridge took it — two openings of a
// job can reuse a place, never at one instant. And beside it, the id the call's
// harness gave it ([Begun.CallID]), which the entry carries and which is no
// part of that identity.
type Begun struct {
	// Name is the tool the call names.
	Name string
	// Seq is the call's place among the run's calls ([Call.Seq]).
	Seq int
	// StartedAt is when the bridge took the call, in UTC: the instant its
	// record carries too ([Call.StartedAt]).
	StartedAt time.Time
	// Digest is the identity the call was admitted under ([Call.Digest]),
	// and EMPTY WHERE ITS TOOL MAY ASK A PERSON FIRST: a held call's
	// arguments ride sealed, and a digest of them in the clear would confirm
	// a guess at them (ADR-0077). A call without one is counted as a call of
	// its tool whose arguments are not known, an over-count, the safe
	// direction.
	Digest string
	// Until is the instant past which nobody waits for the call to finish:
	// StartedAt, its tool's own bound and the bridge's margin (wait.go).
	Until time.Time
	// CallID is the id the call's harness gave it ([Call.CallID]), which the
	// entry carries for its update to carry too, and NEVER WHAT THE ENTRY IS
	// FOUND BY: it is the box's word.
	CallID string
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

	// StartedAt is when the bridge took the call, in UTC — before it wrote
	// the call's entry ahead of it ([Ledger.Begin]), so the call's span
	// includes that write — and Duration how long it took from then to its
	// answer, on the monotonic clock. With Seq and the tool's name, what the
	// call's update finds its entry by ([Begun]). Both zero on a call answered
	// without being taken — arguments that did not parse, a call made once the
	// run's tools had closed.
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
	// log ([tools.Surface.DigestOf]), taken as the call arrived, before its
	// entry carries it and before anything records it; empty for a call never
	// admitted (made after the run's tools closed, or whose arguments did not
	// parse).
	// The record of the call carries it so a later segment seeds the log by
	// the identity it counted, whatever that record keeps of the arguments.
	//
	// AND EMPTY FOR A HELD CALL, whose arguments are not logged (Args): a
	// digest of them in the clear beside the tool's name would confirm a
	// guess at what their seal hides (ADR-0077). The record counts such a
	// call as one of its tool whose arguments are not known.
	Digest string

	// CallID is the id the call's HARNESS gave it, as the box sent it in the
	// request's `_meta` under [Session.CallIDMeta] — for Claude Code, the
	// model's tool_use id — read once as the call arrives ([Session.callIDOf])
	// and carried by its entry ([Begun.CallID]) and its one update alike, a
	// held call's included, since the id says nothing of its sealed
	// arguments. "" where the box sent none the log keeps whole
	// (sandbox.ValidCallID), on a session that declares no key, and on a
	// skill load a hook reported. The box's word, and NO PART OF THE CALL'S
	// IDENTITY (sandbox.BridgeCall.CallID).
	CallID string

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
	// found gone ([toolsession.Call.Abandoned]); or the opening it was made
	// on ended while its tool ran, and answered it as still running
	// ([binding.endKeeping]). The call ran and its effect happened, but the
	// session of the job that made it holds it with no outcome, a failed one
	// or the end's "still running", so no save point may hold it
	// (sandbox.BridgeCall.Abandoned). Decided once, with who answers the
	// call ([binding.conclude]), so no call handed its own answer is marked
	// for its opening's end, and carried by the call's one update
	// ([Ledger.Record]), as the late mark the run's log gives in the same
	// write is. False for every call answered without running, which returns
	// at once.
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

	// calls is every call this bridge's openings took, until it is
	// recorded, and every close of an opening's sessions its end left
	// running ([bridgedCalls]).
	calls *bridgedCalls
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
		calls:    newBridgedCalls(),
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
	//
	// THE OPENING IS THE ONE THE REQUEST'S ROUTING FOUND, handed to the
	// handler on the request ([Bridge.serveMCP]), and the token is never read
	// again: read again, it named no opening where the opening ended between
	// the routing and the handler, or named the opening a recovery put in its
	// place, which no step had held to the session the box started
	// ([Bridge.serveBegun]); and a nil server here is the SDK's 400 "no server
	// available", which Claude Code never makes again at a box's startup
	// ([Bridge.serveMCP]).
	b.streamable = mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		opened, _ := r.Context().Value(servedOpening{}).(*binding)
		if opened == nil {
			return nil
		}
		// THE SERVER THE ROUTE NAMES: the engine's own at the job's endpoint,
		// one of the seat's MCP servers beneath it ([ServerPattern]), and
		// none for a server the opening does not serve, which keeps the SDK's
		// 400: a request no opening of the job could serve.
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

	opened := newBinding(ctx, s, where, b.calls)
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
		recovered := previous.id == opened.id
		log.Info("mcp_bridge_session_replaced", "run_id", s.RunID, "launch_id", s.Launch,
			"seat", s.Handle, "recovered", recovered)
		// AND ITS NOTE INBOX, unless the new opening serves the same one —
		// the same session opened again.
		//
		// A RECOVERY'S BOX GOES ON, served by the new opening under the token
		// it holds; a relaunch's earlier box has had its run end, the new job
		// carrying it on. Which is what the earlier opening's calls still
		// running, and any that reach it after, are told ([ending]).
		how := runEnded
		if recovered {
			how = boxGoesOn
		}
		previous.endKeeping(opened.steer, how)
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
// IT WAITS FOR NO CALL STILL RUNNING: each is answered that the run has ended
// and the call is still running ([endedRunningNote]), and runs on, its one
// update made as its tool returns ([binding.endKeeping]) — its identity on the
// run's log from before it ran ([Ledger.Begin]), so whatever reads the log
// after this reads it as a call still in flight (see the package doc, "An
// opening ends without waiting for its calls"). A call whose entry was still
// being written is answered that it was not run, and never runs.
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
	opened.end(runEnded)
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
	return b.heldLocked(sub)
}

// heldLocked is [Bridge.held], under b.mu.
func (b *Bridge) heldLocked(sub subject) *binding {
	if opened := b.sessions[sub.launch]; opened != nil && opened.session.RunID == sub.run {
		return opened
	}
	return nil
}

// verdict is what THIS NODE can do for a token's job: serve its opening, say
// that the run is over, or say it does not hold the run's seat — not yet, or no
// longer. And the job's live opening here, nil where it holds none
// ([Bridge.held]), read in the same moment as the verdict.
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
//
// ONE MOMENT FOR THE OPENING AND THE SEAT'S READINESS, read under one lock: a
// seat leaving this node takes its readiness and its openings in one step
// ([Bridge.ReleaseSeat]), and read apart, the readiness from before that step
// and the opening from after it said that a run whose box goes on with the
// seat's next holder is over, which the box's harness takes for good.
func (b *Bridge) verdict(sub subject) (*binding, httprelay.Verdict) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	opened := b.heldLocked(sub)
	switch {
	case sub.route.pinned() && opened != nil:
		return opened, httprelay.Serve
	case sub.route.pinned():
		return nil, httprelay.Refuse
	case !b.ready[sub.route.seat]:
		return opened, httprelay.Elsewhere
	case opened != nil:
		return opened, httprelay.Serve
	default:
		return nil, httprelay.Refuse
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
	// SIDE BY SIDE: each ending waits on the answers it hands its box
	// ([endAnswerWait]); a seat leaving this node waits on the slowest of them
	// rather than on their sum. AND ON NO CALL STILL RUNNING: each is answered
	// that it is still running, since the box goes on with the seat's next
	// holder ([stillRunningNote]), and runs on here to make its one update, so
	// the node gives the seat's lease back without waiting out a call — whose
	// identity is on the run's log already ([Ledger.Begin]), for the next
	// holder to read.
	var ended sync.WaitGroup
	for _, opened := range released {
		ended.Go(func() { opened.end(boxGoesOn) })
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
	return b.routed(b.serveMCP)
}

// servedOpening is the request context key [Bridge.serveMCP] hands the MCP
// handler the request's opening under.
type servedOpening struct{}

// sessionHeader is the header a request of MCP's streamable HTTP transport names
// its session in, and one that opens a session names none (the transport's
// "Session Management": a client's initialize carries none, and every request
// after it carries the id the server answered).
const sessionHeader = "Mcp-Session-Id"

// opensSession reports whether r asks the MCP handler to open a session: a POST
// that names none. Every other request is served on the session it names, which
// the opening's end closes, or refused by the handler for naming none.
func opensSession(r *http.Request) bool {
	return r.Method == http.MethodPost && r.Header.Get(sessionHeader) == ""
}

// serveMCP serves a request of the job's endpoint, or of one of the seat's MCP
// servers beneath it ([ServerPattern]), on the opening its routing found
// ([Bridge.routed], or [Bridge.heldOnly] for a relayed call) and held to the
// session the job's box started ([Bridge.serveBegun]).
//
// ON THAT OPENING AND NO OTHER: the MCP handler is handed it on the request
// ([servedOpening]), and never reads the token again. The routing reads it once,
// as the request arrives, and that is the request's credential to its end.
//
// A REQUEST THAT OPENS A SESSION ([opensSession]) IS SERVED ONLY BY AN OPENING
// THAT HAS NOT ENDED, and that opening's end waits for the MCP handler to
// return it before it closes the box's sessions ([binding.holdSessionOpen]), so
// the session the SDK opens as it serves the request is among those the end
// closes. One whose opening ended after its routing found it is TURNED AWAY TO
// ASK AGAIN, a 503 with a Retry-After, as a call to the notes route is
// ([Bridge.serveSteer]): the run ended, the seat left this node, or a recovery
// opened the job again, and the request's next try is routed afresh, to the
// opening that serves the box now, here or on the seat's next holder, or
// refused where the run is over. NEVER SERVED THERE: a session opened on an
// ended opening is one its end never closes, which answers every call made on
// it as one not run, a box told to call again calling on it again. And never
// the SDK's 400 "no server available", which Claude Code never makes again at
// a box's startup (below).
//
// WHAT A BOX DOES WITH THE 503, measured on Claude Code 2.1.310 and 2.1.322,
// which behave alike and read no Retry-After. A request opening a session at
// the box's startup is made again three times, about 0.5, 2 and 6 seconds
// after the first, and the server is given up for the run where all three
// meet a 503 too; one opening a session again after the box's session was
// lost mid-run is made again five times within about 15 seconds, then up to
// eight more times in the background, minutes apart, while the answer stays a
// 503. A call that meets a 503 ([Bridge.forward], [Bridge.heldOnly]) is not
// made again: the model's call fails, a hook's call is a hook error the run
// goes on past, and the CLI keeps the server and its session. The 400 this
// 503 replaced was never made again at a box's startup, which left the box
// without its tools for the run. OpenCode's own MCP client was not measured;
// its notes plugin, the notes route's one client, asks again on its own
// backoff and reads no Retry-After either.
//
// THE BRIDGE DOES NOT ROUTE THE REQUEST AGAIN ITSELF: the 503 fires only where
// the opening ended in the instant between the routing that found it and the
// session's hold, in which [Bridge.serveBegun] runs nothing but
// [binding.begin], and the box's own next try is routed afresh.
//
// A request on a session it names needs neither: that session is closed by the
// opening's end, and the SDK answers a request on a closed one with the 404
// that has a client open a new session, through the routing.
func (b *Bridge) serveMCP(w http.ResponseWriter, r *http.Request, opened *binding) {
	if opensSession(r) {
		// HELD UNTIL THE HANDLER HAS RETURNED THE REQUEST, never only until
		// the request's context ends ([binding.holdSessionOpen]).
		served := make(chan struct{})
		defer close(served)
		if !opened.holdSessionOpen(served) {
			log.InfoContext(r.Context(), "mcp_bridge_session_open_turned_away", "run_id", opened.session.RunID,
				"launch_id", opened.id, "seat", opened.session.Handle,
				"detail", "the run's session on this node ended as a request opening an MCP session on it "+
					"reached it; the box is told to ask again, which reaches the opening that serves it now")
			httpjson.Unavailable(w, httpjson.CodeUnavailable, httpjson.RetrySeconds(holderRetryCeiling))
			return
		}
	}
	b.streamable.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), servedOpening{}, opened)))
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
// ([Bridge.admitRelayed]).
//
// AN OPENING GONE SINCE IS ANSWERED AS THIS NODE CAN ANSWER IT NOW
// ([Bridge.verdict]): the relay admits a call and starts its handler after, and
// the opening can leave in between. Where the run is over, closed or replaced
// by a relaunch's job, the call is refused, as one reaching this node's own
// route is. Where the seat left this node ([Bridge.ReleaseSeat]), the run goes
// on with the seat's next holder: the call is turned away to ask again, a 503
// with a Retry-After, as one that found no holder in time is
// ([Bridge.forward]), and its next try is relayed to that holder. NEVER
// REFUSED THERE: a refusal says the run is over, which OpenCode's plugins read
// as the end of the run's notes and of its skill reports, and a box's MCP
// client as a refused credential, for a run that goes on.
func (b *Bridge) heldOnly(serve func(http.ResponseWriter, *http.Request, *binding)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sub, ok := b.parse(r.PathValue("token"))
		if !ok {
			b.refuse(w, r, "", "the token is forged, malformed or expired")
			return
		}
		opened, verdict := b.verdict(sub)
		switch {
		case opened != nil:
			b.serveBegun(w, r, sub, opened, serve)
		case verdict == httprelay.Elsewhere:
			log.InfoContext(r.Context(), "mcp_bridge_relayed_seat_left", "run_id", sub.run,
				"detail", "the seat left this node after the relay admitted the call; the box is "+
					"told to call again, which reaches the seat's next holder")
			httpjson.Unavailable(w, httpjson.CodeUnavailable, httpjson.RetrySeconds(holderRetryCeiling))
		default:
			_, reason := b.Miss(r.PathValue("token"))
			b.refuse(w, r, sub.run, reason)
		}
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

	// calls is the bridge's registry of the calls it runs ([bridgedCalls]),
	// which holds each call the opening admits until it is recorded.
	calls *bridgedCalls

	mu sync.Mutex
	// ended is an opening that has ended ([binding.endKeeping]): every later
	// call is refused, as endedAs says it ended.
	ended   bool
	endedAs ending
	// stopped is why the opening's calls are refused from here on: the
	// seat's fence closed, or a tool panicked. Nil while it serves.
	stopped error
	// arrived is the place of the last call that reached the opening, in
	// the job's order — from [Session.LastSeq] — and so the one before the
	// next [Call.Seq].
	arrived int
	// lastDecided is the decision of the last call held to be admitted or
	// refused ([bridgedCall.decided]), which the next one waits for
	// ([bridgedCall.admitAfter]); nil before the first, which waits for none.
	lastDecided <-chan struct{}
	// running is every call the opening took whose answer is not yet
	// decided and out ([bridgedCall]): what its end answers and waits for.
	// A call leaves it as a later call is taken, once its answer is out.
	running map[*bridgedCall]struct{}
	// refusals is the request's end of every answer the opening gave without
	// running a call that may still be on its way
	// ([binding.holdRefusalLocked]), a hook's answer among them
	// ([binding.holdHook]), and of every request it let open a session
	// ([binding.holdSessionOpen]): what its end waits for too, before it
	// closes the box's sessions.
	refusals []<-chan struct{}

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

func newBinding(ctx context.Context, s *Session, where route, calls *bridgedCalls) *binding {
	opened := &binding{id: s.Launch, session: s, caller: s.caller(), route: where,
		ctx: context.WithoutCancel(ctx), arrived: s.LastSeq, steer: s.Steer, calls: calls,
		running: map[*bridgedCall]struct{}{}}
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
	// endedNote answers a call that reaches an opening after its end, where
	// the run ended for its box: it settled, was stopped, or a relaunch
	// replaced its job ([runEnded]). One whose box goes on is told
	// [closingNote].
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
//
// ITS ENTRY IS WRITTEN AHEAD OF IT, AND ITS TOOL RUNS, ON A GOROUTINE OF THE
// BRIDGE'S ([bridgedCalls]): the call's identity goes on the run's log first,
// and only a call whose entry the log took runs ([binding.writeAhead]) — see the
// package doc, "A call is on its run's record before it runs". The call is
// answered by whichever comes first: the refusal of a call that was not run,
// its own result once its record is made ([binding.conclude]), or the
// opening's end, which answers every call it holds before it closes the box's
// sessions ([binding.endKeeping]) — see the package doc, "An opening ends
// without waiting for its calls".
//
// THE ID ITS HARNESS GAVE IT is read once, as it arrives ([Session.callIDOf]),
// and every record of the call carries it: its entry, and its one update on
// every path that makes one — see the package doc, "A call carries the id its
// harness gave it".
func (o *binding) Call(in toolsession.Call) toolsession.Result {
	s := o.session
	// THE FENCE BEFORE ANY CALL, outside the lock: it reads the node's own
	// copy of the seat's grant and pauses, and nothing it answers depends on
	// another call of this opening.
	fenced := fence(s)
	// THE ID ITS HARNESS GAVE IT, outside the lock too: the session's key is
	// fixed before it opens, and the call's `_meta` is its own.
	callID := s.callIDOf(in.Meta)

	o.mu.Lock()
	if o.ended {
		// NOT RECORDED: the call never ran, and the record is not this
		// opening's to write — the run is over and its record with it, a
		// relaunch's job holds it, or the run goes on through the opening
		// that serves its box now. TOLD AS THE OPENING ENDED
		// ([ending.notRun]): to end its turn where the run ended, and to call
		// again where the box goes on, its next call reaching that opening.
		// And HELD until that answer is out, which the end waits for before it
		// closes the box's sessions.
		why := o.endedAs.notRun()
		o.holdRefusalLocked(in.Over)
		o.mu.Unlock()
		return refused(why)
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
		o.holdRefusalLocked(in.Over)
		o.mu.Unlock()
		o.record(Call{Call: tools.Call{Name: in.Name, Args: args, Output: stoppedNote,
			Failed: true, Cause: tools.RefusalNotRunning}, Seq: seq, CallID: callID}, time.Time{})
		return refused(stoppedNote)
	}
	if in.ArgumentsError != "" {
		// ANSWERED, NOT RUN, and recorded: the call was made, and an empty
		// object in place of what the model wrote is a call it never made.
		// Not admitted to the run's call log, since nothing ran.
		o.holdRefusalLocked(in.Over)
		o.mu.Unlock()
		out := toolloop.UnparsedArguments(in.Name, in.ArgumentsError)
		o.record(Call{Call: tools.Call{Name: in.Name, Output: out, Failed: true,
			Cause: tools.RefusalInvalid}, Seq: seq, CallID: callID}, time.Time{})
		return refused(out)
	}
	// THE IDENTITY IT WILL BE COUNTED BY in the run's call log, which its
	// entry and its record carry. NOT ITS PLACE THERE YET: the call is
	// admitted to the log only once its entry is on the run's record, in
	// the order calls arrived ([binding.writeAhead]).
	digest := s.Surface.DigestOf(in.Name, args)
	// HELD BY THE OPENING FROM HERE, under the lock its end takes: an end
	// either came before — and the call is refused above — or finds the call
	// here, and answers it: that it was not run, where its entry is still
	// being written or it waits for its admission, and that it is still
	// running where its tool runs. And IN THE ORDER IT ARRIVED: it is
	// admitted or refused only once the call that reached the opening before
	// it has been ([bridgedCall.admitAfter]).
	c := o.holdLocked(in.Over)
	c.admitAfter, c.decided = o.lastDecided, make(chan struct{})
	o.lastDecided = c.decided
	o.mu.Unlock()

	// ITS IDENTITY, taken now and written ahead of it ([Begun]): the instant
	// the bridge took the call, which its record carries too, so the call's
	// span includes the write; the bound past which nobody waits for it
	// ([callBound]); and its digest, where its tool can never hold it for a
	// person first ([asksFirst]).
	started := time.Now()
	entry := Begun{Name: in.Name, Seq: seq, StartedAt: started.UTC(), Digest: digest,
		Until: started.Add(callBound(s.Surface, in.Name) + callMargin).UTC(), CallID: callID}
	if asksFirst(s.Surface, in.Name) {
		entry.Digest = ""
	}

	// THE ENTRY IS WRITTEN, THE CALL ADMITTED, ITS TOOL RUN AND THE CALL
	// RECORDED, ON A GOROUTINE OF ITS OWN, which the bridge holds until the
	// record is made: the answer can then come before the tool returns, where
	// the opening ends first, and the tool runs on to its end all the same. A
	// call whose entry is refused is answered so at once; and what is left of
	// a call once its answer is out — the completion of what its refusal left
	// on the log, or an update the store did not take yet — is done after that
	// answer, so the box waits for none of it ([binding.writeAhead],
	// [binding.conclude]).
	concluded := make(chan answer, 1)
	go func() {
		defer o.calls.done(c)
		refusal, admitted, after := o.writeAhead(c, entry, args, digest)
		if admitted == nil {
			concluded <- refusal
			if after != nil {
				after()
			}
			return
		}
		written := writtenOf(s.Surface)
		r := ran{name: in.Name, args: args, digest: digest, seq: seq, callID: callID, abandoned: in.Abandoned,
			written: written, before: written.Changes(), started: started, until: entry.Until}
		r.res, r.broke, r.err = o.execute(admitted, in.Name, args)
		r.took = time.Since(r.started)
		a, after := o.conclude(c, r)
		concluded <- a
		if after != nil {
			after()
		}
	}()

	// ITS ANSWER STARTS ONCE ITS TOOL RUNS, past every answer given without
	// running it, and is kept going while it runs ([toolsession.Call.Running]):
	// a call the surface goes on to refuse or hold is answered a moment after
	// its first progress, and telling those apart here would be this
	// package's own idea of the surface's gates. Nil for a call no request
	// carries.
	select {
	case <-c.began:
		if in.Running != nil {
			in.Running()
		}
	case a := <-concluded:
		return c.answerOf(a)
	case <-c.cut:
		return c.endsAnswer()
	}
	select {
	case a := <-concluded:
		return c.answerOf(a)
	case <-c.cut:
		return c.endsAnswer()
	}
}

// writeAhead writes call c's entry on the run's log ahead of it ([Ledger.Begin]),
// on the call's own goroutine and never under the box's request, and decides
// ONCE, under the opening's lock, whether its tool runs: where it does, the
// call ADMITTED to the run's call log there and then ([Surface.Admit]) and
// BEGUN, so an end after it answers it as still running — and admitted is the
// call bound to its place in the log, to run once. Where it does not, admitted
// is nil, and the answer it gets — its own, decided here — and what is left to
// make of its entry once that answer is handed over (after, nil for nothing):
//
//   - THE OPENING'S END ANSWERED IT while its entry was written, or while it
//     waited for its admission, that it was not run, failed
//     ([binding.endKeeping]): the end's answer stands, and the entry is
//     withdrawn wherever the log may have taken it — a write that landed, or
//     one that landed and lost its answer — which finds none where none
//     landed.
//   - THE LOG DID NOT TAKE ITS ENTRY: a store that refused every attempt within
//     the ledger's bound, or a log holding as many calls in flight as it
//     keeps, told the call could not be recorded and may be made again
//     ([unrecordedNote]); or a log that takes no more calls of this job
//     ([ErrNotTaken]), told the run has ended for its box ([endedNote]). Both
//     failed: nothing ran. An entry whose write landed and lost its answer is
//     withdrawn all the same, which finds none where none landed.
//   - THE OPENING'S TOOLS WERE CLOSED MEANWHILE, by a tool that broke inside
//     the engine or by the seat's fence another call found closed
//     ([binding.stop]): refused as every call that reaches closed tools is
//     ([stoppedNote]), and RECORDED in its place — its entry updated to that
//     refusal, its one update, which carries the id the call arrived with.
//   - THE SEAT'S FENCE, ASKED AGAIN NOW THE ENTRY IS WRITTEN, IS CLOSED: told
//     the session it reached was closing and to call again ([closingNote]),
//     failed, and its entry withdrawn. THE OPENING IS NOT STOPPED: the end
//     that follows a fence that closed — the seat's release, a person's stop
//     — decides what the box is told of the run, and a stop here would tell a
//     box that goes on to end its turn.
//
// SO A CALL RUNS ONLY WITH ITS IDENTITY ON THE RECORD, and an entry whose call
// was then not run is completed as not run in the same step, never left in
// flight for a collection to wait on and a continuation to be told of.
//
// AND IT IS IN THE RUN'S CALL LOG ONLY ONCE ITS ENTRY IS. The log is what a
// call's operation id counts its repeats from ([turnctx.CallLog]), and a later
// segment of the run seeds its own from the run's record alone: a call admitted
// at its arrival and then not run — its entry refused by the store, its opening
// ended while the entry was written — raised every later call's count in this
// segment and in no record, so a resumed segment derived, for a new call, the
// operation id a call of this one had already used. Admitted here instead, a
// call that is not run is in no call log, and its withdrawn entry in no record
// either. IN THE ORDER CALLS ARRIVED, which the count is a function of
// ([tools.Surface.Admit]): each call is decided only once the call that reached
// the opening before it has been ([bridgedCall.admitAfter]), whichever entry's
// write came back first — so side-by-side calls still count in arrival order,
// and a call's admission waits at most for the writes of the calls that arrived
// before it, each bounded by the ledger.
func (o *binding) writeAhead(c *bridgedCall, entry Begun, args map[string]any, digest string,
) (answer, toolloop.Admitted, func()) {
	s := o.session
	var err error
	if s.Ledger != nil {
		err = s.Ledger.Begin(o.ctx, s.RunID, entry)
	}
	// ITS TURN: every call that arrived before it decided, so the calls that
	// are admitted are admitted in the order they arrived.
	if c.admitAfter != nil {
		<-c.admitAfter
	}
	// THE FENCE AGAIN, outside the lock as before the call: the seat may have
	// left this node, or been asked to stop, while the entry was written. Read
	// once the call's turn has come, so the decision below reads it as it
	// stands; no later call is decided until this one is.
	fenced := fence(s)
	withdraw := func() { o.withdraw(entry) }
	o.mu.Lock()
	defer o.mu.Unlock()
	defer close(c.decided)
	switch {
	case c.cutByEnd:
		if errors.Is(err, ErrNotTaken) {
			return answer{}, nil, nil
		}
		return answer{}, nil, withdraw
	case err != nil:
		c.own = true
		if errors.Is(err, ErrNotTaken) {
			return answer{result: refused(endedNote), own: true}, nil, nil
		}
		return answer{result: refused(unrecordedNote), own: true}, nil, withdraw
	case o.stopped != nil:
		c.own = true
		return answer{result: refused(stoppedNote), own: true}, nil, func() {
			o.recordUntil(Call{Call: tools.Call{Name: entry.Name, Args: args, Output: stoppedNote,
				Failed: true, Cause: tools.RefusalNotRunning}, Seq: entry.Seq,
				StartedAt: entry.StartedAt, Digest: digest, CallID: entry.CallID}, entry.Until)
		}
	case fenced != nil:
		c.own = true
		return answer{result: refused(closingNote), own: true}, nil, withdraw
	}
	admitted := s.Surface.Admit(entry.Name, args)
	c.begun = true
	close(c.began)
	return answer{}, admitted, nil
}

// withdraw takes back the entry of a call that was then not run
// ([Ledger.Withdraw]), saying so where the ledger could not: the entry stands,
// a call in flight nobody will finish, waited for by its bound alone.
func (o *binding) withdraw(entry Begun) {
	s := o.session
	if s.Ledger == nil {
		return
	}
	if err := s.Ledger.Withdraw(o.ctx, s.RunID, entry); err != nil {
		log.WarnContext(o.ctx, "mcp_bridge_ledger_withdraw_failed", "run_id", s.RunID,
			"seat", s.Handle, "tool", entry.Name, "seq", entry.Seq, "error", err,
			"detail", "the call was not run, but its entry stays on the run's log as one in "+
				"flight until its bound")
	}
}

// ran is one admitted call as its tool returned: what its record step reads.
type ran struct {
	name   string
	args   map[string]any
	digest string
	seq    int
	// callID is the id the call's harness gave it ([Call.CallID]), which
	// every update of the call carries.
	callID string
	// abandoned is the call's request's [toolsession.Call.Abandoned].
	abandoned <-chan struct{}
	// written is the set the call commits to, and before what it held as the
	// call's tool started.
	written *turnctx.Written
	before  []types.CommittedChange
	// started is when the bridge took the call, before its entry was written
	// ahead of it, and took how long it took from then to its answer.
	started time.Time
	took    time.Duration
	// until is the call's entry's bound ([Begun.Until]): past it nobody waits
	// for the call, and an update the store has not taken by then is asked
	// for no longer ([binding.recordOwed]).
	until time.Time
	// res, broke and err are what the surface answered ([binding.execute]).
	res   toolloop.ToolResult
	broke *turn.PanicError
	err   error
}

// answer is what a call's record step hands back: the call's own answer, and
// whether that is the call's answer at all — own, where the opening had not
// ended as the tool returned; where it had, the end's answer stands.
type answer struct {
	result toolsession.Result
	own    bool
}

// answerOf is the box's answer to c, handed a its record step's or its begin's
// answer: a's own, unless the opening's end answered it first, though a came
// in before the handler saw the end — the answer is then the end's, as the
// record says.
func (c *bridgedCall) answerOf(a answer) toolsession.Result {
	if a.own {
		return a.result
	}
	return c.endsAnswer()
}

// conclude is a bridged call's ONE RECORD STEP: everything that follows its
// tool's return, called once, on the call's own goroutine, after the tool
// returned, and nowhere else. Its contract:
//
//   - WHO ANSWERS THE CALL is decided first, once, under the opening's lock
//     ([binding.claim]): its own result where the opening has not ended —
//     whose end, finding the call answered so, waits for that answer rather
//     than answering it — and the end's answer where it has, which the end
//     handed the box already ([binding.endKeeping]). So the answer and the
//     record never disagree about whether anybody waited for it.
//   - IT RECORDS THE CALL ONCE ([binding.record]): ONE UPDATE of the entry
//     written ahead of it, by its identity ([Ledger.Record]) — where the
//     surface could not run it, as a call that was not run, failed, since the
//     run's call log admitted it — with what it answered, what it cost and
//     what it committed to the run's turn, and as ONE NOBODY WAITED FOR
//     ([Call.Abandoned]) where the end answered it, or its harness had given
//     up on its request by then. Every mark a call carries rides this one
//     update: the late one the run's log gives in the same write too.
//   - AN UPDATE THE LEDGER COULD NOT LAND ([ErrUnrecorded]) is the same update
//     asked again, once the call's answer is out, until it lands or the
//     call's bound passes: after, which the call's goroutine runs once it has
//     handed the answer over ([binding.recordOwed]); nil where nothing is
//     left to do.
//   - IT MAY RUN AFTER THE OPENING ENDED: under the run's context, never the
//     request's, which the call has no part of. And after the run's
//     collection read its log, or its ending deleted the run's record, which
//     the ending of an opening does not wait for: the call's identity is on
//     the log from before it ran, so such a read holds it as a call still in
//     flight, and an update after the delete finds no record.
//   - IT NEVER FAILS THE CALL BACK over its record ([binding.record]): the
//     tool ran, and its effect happened.
//
// It also closes the opening's tools where the call broke inside the engine,
// or the seat's fence closed while it ran. A late call's mark is not decided
// here: the run's log gives it as it takes the record
// ([sandbox.BridgeCall.Late]).
func (o *binding) conclude(c *bridgedCall, r ran) (answer, func()) {
	s := o.session
	// FIRST, as the tool returns, so an end from here on leaves the call to
	// its own answer and waits for it.
	own := o.claim(c)
	res := r.res
	switch {
	case r.broke != nil:
		log.ErrorContext(o.ctx, "mcp_bridge_tool_panicked", "run_id", s.RunID,
			"seat", s.Handle, "tool", r.name, "panic", r.broke.Value, "stack", r.broke.Stack)
		o.stop(r.broke)
		res = toolloop.ToolResult{Output: failedInsideNote, Failed: true,
			Cause: tools.RefusalInternalError, Absorbed: res.Absorbed}
	case r.err != nil:
		// The surface returns an error only for a call that could not run
		// at all — the turn it works for torn down under it — and nothing
		// was recorded on it.
		//
		// IN WORDS OF ITS OWN, never the error's: the caller is the harness
		// in the box, a model, and the error is a Go error's text
		// ("toolloop: context canceled") it can do nothing with. The error
		// goes to this node's log.
		log.ErrorContext(o.ctx, "mcp_bridge_dispatch_failed",
			"run_id", s.RunID, "seat", s.Handle, "tool", r.name, "error", r.err)
		// ITS ENTRY UPDATED TO A CALL THAT WAS NOT RUN, failed — never
		// withdrawn: the run's call log admitted it ([binding.writeAhead]),
		// so the record keeps the identity this segment's log counts it by,
		// for a later segment's to count it too. Nothing ran, so the count is
		// an over-count, the safe direction ([turnctx.CallLog]); a call off
		// the record that this segment counted is the under-count no record
		// may make.
		after := o.record(Call{Call: tools.Call{Name: r.name, Args: r.args, Output: unrunNote,
			Failed: true, Cause: tools.RefusalNotRunning}, Seq: r.seq, StartedAt: r.started.UTC(),
			Duration: r.took, Digest: r.digest, CallID: r.callID, Abandoned: !own || gone(r.abandoned)}, r.until)
		return answer{result: refused(unrunNote), own: own}, after
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
			"run_id", s.RunID, "seat", s.Handle, "tool", r.name)
		res = toolloop.ToolResult{Output: suspendNote, Failed: true,
			Cause: tools.RefusalForbidden, Origin: res.Origin, Server: res.Server,
			Source: res.Source, Absorbed: res.Absorbed}
	}

	// AND THE FENCE AGAIN ONCE THE TOOL HAS RETURNED. It ran, and what it did
	// is done, whichever answer its box was handed — its own, or its
	// opening's end's — but nothing after it runs as a seat this node no
	// longer holds.
	if after := fence(s); after != nil {
		o.stop(after)
	}
	// WHETHER ANYBODY WAITED FOR THE ANSWER ([Call.Abandoned]): nobody, where
	// the opening's end answered the call before its tool returned; and
	// nobody, where the harness had cancelled its request, or a progress kept
	// going on its response found the response gone, by the time it returned.
	// A harness that hung up on its POST is seen at the next progress after
	// it, and only then: a request this transport carries runs on when its
	// harness hangs up, so a hang-up in the last cadence before the tool
	// returns is not seen here.
	abandoned := !own || gone(r.abandoned)
	recorded, digest := r.args, r.digest
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
	after := o.record(Call{
		Call: tools.Call{Name: r.name, Args: recorded, Output: output, Failed: res.Failed,
			Cause: res.Cause},
		Seq: r.seq, StartedAt: r.started.UTC(), Duration: r.took,
		Origin: res.Origin, Server: res.Server, Source: res.Source, Held: res.Held,
		Digest: digest, CallID: r.callID, Absorbed: res.Absorbed, Committed: r.written.Since(r.before),
		Abandoned: abandoned,
	}, r.until)
	return answer{result: toolsession.Result{Output: res.Output, Failed: res.Failed}, own: own}, after
}

// claim decides who answers c now that its tool has returned, once, under the
// opening's lock: its own result, unless the opening's end answered it first.
func (o *binding) claim(c *bridgedCall) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if c.cutByEnd {
		return false
	}
	c.own = true
	return true
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
//
// AN OPENING THAT HAS ENDED IS NOT STOPPED: its end refuses every later call
// already, in its own words ([ending.notRun]), so a stop would change nothing
// and say what is false of it — that the run's harness is told to end its
// turn, of a run whose box may go on. Which is what a call still running as
// its seat leaves this node meets as its tool returns ([binding.conclude]):
// the seat leaves the node's grants before its release ends the opening, so
// the fence after the call is closed, on an opening that has ended.
func (o *binding) stop(why error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.stopLocked(why)
}

func (o *binding) stopLocked(why error) {
	if o.stopped != nil || o.ended {
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

// record makes one call's one update on the durable ledger ([Ledger.Record]),
// and answers what is left of it to do once the call's answer is out: nil where
// the update landed, or is not to be asked again; and, where the ledger could
// not land it within its own attempts and the run's log may still take it
// ([ErrUnrecorded]), the same update asked again until the call's bound, until
// ([binding.recordOwed]). A call with no bound — one answered without being
// taken, which wrote no entry — is asked for once.
//
// NEVER FAILED BACK TO THE BOX, and the reason is that the alternative is
// worse. The tool has already run and its effect has already happened, so
// failing the call would tell a harness to retry work that is done — the
// duplicate-post failure the whole delivery machinery exists to prevent. The
// ledger does what it can before the answer goes back — the engine's tries the
// update again — and what it could not land is asked for again after the
// answer, while the call's entry stands, its identity on the record and its
// outcome not yet: a resumed segment's call log counts it all the same.
//
// Under the run's context rather than the request's, so a box that hangs up
// does not take the row with it.
func (o *binding) record(call Call, until time.Time) func() {
	s := o.session
	if s.Ledger == nil {
		return nil
	}
	err := s.Ledger.Record(o.ctx, s.RunID, call)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrUnrecorded) && !until.IsZero():
		log.WarnContext(o.ctx, "mcp_bridge_call_update_owed", "run_id", s.RunID, "seat", s.Handle,
			"tool", call.Name, "seq", call.Seq, "until", until, "error", err,
			"detail", "the run's log did not take the call's update; its entry stands, and the update "+
				"is asked for again once the call's answer is out, until it lands or the call's bound passes")
		return func() { o.recordOwed(call, until) }
	}
	log.WarnContext(o.ctx, "mcp_bridge_ledger_record_failed",
		"run_id", s.RunID, "seat", s.Handle, "tool", call.Name, "error", err)
	return nil
}

// recordUntil is [binding.record] for a step made once the call's answer is
// out already: the update, and every time it is asked for again.
func (o *binding) recordUntil(call Call, until time.Time) {
	if owed := o.record(call, until); owed != nil {
		owed()
	}
}

// owedUpdateBase, owedUpdateCeiling and owedUpdateJitter space the asks of an
// update the ledger could not land ([binding.recordOwed]): from ONE SECOND —
// the last of the engine's waits between its own attempts at the update
// (internal/engine's appendRetryWaits), so the spacing goes on where those
// left off — doubling to FIVE, jittered by a fifth. Five is past the four
// seconds and more a stream's followers wait to replace a leader that was
// lost, the outage the ledger's own attempts do not outlast, and is what a
// write ahead of a call is given (internal/engine's beginBudget): a store that
// answers again takes the update within about that, and a collection waiting
// on the run's log ends on it. Each ask is the ledger's own four attempts, 1.75
// seconds of waits between them, so a store out for long sees one call's update
// about once every seven seconds. Jittered by a fifth, as a call's wait for a
// holder is (fleet.go), so a run's calls owed together do not ask in step.
// NONE BEFORE: an update the ledger could not land was given up, its outcome
// lost.
const (
	owedUpdateBase    = time.Second
	owedUpdateCeiling = 5 * time.Second
	owedUpdateJitter  = 0.2
)

// recordOwed asks again for the update of a call the ledger could not land
// within its own attempts ([ErrUnrecorded]), on the call's own goroutine and
// once its answer is out, so the box waits for none of it: spaced from
// [owedUpdateBase], doubling to [owedUpdateCeiling], until it lands, or the
// ledger answers anything else — the run's log no longer takes the call, its
// round over or its record gone — or the call's own bound, until, passes.
//
// WHY ASKED AGAIN: the update is everything the call did — what it answered,
// cost and committed, a submission among them — and the store refusing it for
// longer than the ledger's own attempts is an outage the store comes back
// from: given up then, the call's outcome was lost although the store answered
// a moment later, a run's submission was read as never made, and every
// collection of the run waited out the call's bound for an update nobody would
// make. Asked again, it lands as soon as the store takes it, and a collection
// waiting on the run's log ends on it.
//
// UNTIL THE CALL'S BOUND, past which no collection waits for the call
// ([Begun.Until]): the entry then stands, the call's identity on the record
// and its outcome unknown to every reader of the run, said at warning
// (`mcp_bridge_call_update_unrecorded`). The call's goroutine is held in the
// bridge's registry for as long as it asks ([bridgedCalls]), so a node's drain
// waits for it within its own bound ([Bridge.AwaitCalls]).
func (o *binding) recordOwed(call Call, until time.Time) {
	s := o.session
	for attempt := 1; ; attempt++ {
		pause := backoff.Jitter(backoff.Doubling(attempt, owedUpdateBase, owedUpdateCeiling), owedUpdateJitter)
		if !time.Now().Add(pause).Before(until) {
			log.WarnContext(o.ctx, "mcp_bridge_call_update_unrecorded", "run_id", s.RunID, "seat", s.Handle,
				"tool", call.Name, "seq", call.Seq, "attempts", attempt,
				"detail", "the run's log took no update of the call before its bound: its entry stands, the "+
					"call's identity on the record and what it answered, cost and committed unknown to every "+
					"reader of the run")
			return
		}
		wait := time.NewTimer(pause)
		<-wait.C
		err := s.Ledger.Record(o.ctx, s.RunID, call)
		switch {
		case err == nil:
			// TAKEN, or no longer the log's to take — its round over, or the
			// run's record gone — which the ledger says where it is.
			log.InfoContext(o.ctx, "mcp_bridge_call_update_answered", "run_id", s.RunID, "seat", s.Handle,
				"tool", call.Name, "seq", call.Seq, "attempts", attempt+1)
			return
		case !errors.Is(err, ErrUnrecorded):
			log.WarnContext(o.ctx, "mcp_bridge_ledger_record_failed",
				"run_id", s.RunID, "seat", s.Handle, "tool", call.Name, "error", err)
			return
		}
	}
}

// asking is a [Surface] that says whether a call to a tool may wait for a
// person's answer before it runs (ADR-0077), declared here by the consumer: a
// seat's *tools.Surface is one.
type asking interface {
	AsksFirst(name string) bool
}

var _ asking = (*tools.Surface)(nil)

// asksFirst reports whether a call to name on s may be held for a person's
// answer, so its entry carries no digest of its arguments ([Begun.Digest]):
// what s says, and true for a surface that says nothing — a call whose entry
// carries none is counted as one of its tool whose arguments are not known, the
// safe direction for the seal and for the count alike.
func asksFirst(s Surface, name string) bool {
	if a, ok := s.(asking); ok {
		return a.AsksFirst(name)
	}
	return true
}

// callIDOf is the id a call's harness gave it, read off the request's `_meta`
// under the one key the session declares ([Session.CallIDMeta]), as the engine
// host reads a call's id (internal/agent/toolloop's callIDFromMeta), and kept
// only where the run's log keeps it whole (sandbox.ValidCallID): a value that
// is not a string, or an id past the bound or outside its characters, is none,
// never cut, so no record of the bridge's carries what the log would drop. A
// session that declares no key reads none — even a value under the empty key,
// which a box can send and a map lookup would find.
func (s *Session) callIDOf(meta map[string]any) string {
	if s.CallIDMeta == "" {
		return ""
	}
	id, _ := meta[s.CallIDMeta].(string)
	if !sandbox.ValidCallID(id) {
		return ""
	}
	return id
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

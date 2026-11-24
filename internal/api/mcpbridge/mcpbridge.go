// Package mcpbridge serves one detached run's tool surface to the harness in
// its box, over MCP on HTTP: a seat's executor, every call decided as the
// run's seat ([seatCaller]).
//
// # What this is for
//
// A seat's executor is its harness's own run, detached in a sandbox box. Its
// tools cannot be shipped into the box: most are MCP children holding the SEAT's
// credentials, which a sandbox exists to keep out. So the box gets one MCP server, on
// the engine, and every call comes back out, over HTTP on a listener this node binds
// (a harness on this host uses a unix socket, internal/toolsession). The MCP server
// behind both is [toolsession.Server].
//
// # The dispatch is the seat's own, never a second implementation
//
// A bridged call goes through [tools.Surface], the frame a tool loop calls. This
// package must never grow its own idea of whether a tool is granted, whether a skill
// guard allows it, what a failure looks like or what is recorded: a second copy would
// drift, on the security half. The session a job's box started is read off its token (started.go).
// That includes the argument check ([tools.Invoke], ADR-0059), so a box and a tool
// loop are refused alike.
//
// The dispatcher adds what a surface cannot know, being about the RUN:
//
//   - THE SEAT'S FENCE, per call. The node may stop holding the seat, or a person
//     may stop it, while the run goes on; a call is then refused and recorded
//     refused, not made as a seat this node no longer speaks for.
//   - THE ORDER CALLS ARRIVED IN. A call is placed in the run's call log when
//     admitted, after the call before it was admitted or refused, not when it
//     returns ([tools.Surface.Admit]), so the repeat count an operation id carries
//     does not depend on which tool finished first.
//   - A CALL IS THE RUN'S, NOT THE REQUEST'S. It runs under the launch context
//     stripped of cancellation, never the HTTP request's: a box that disconnects
//     abandons only the answer, never a tool half-way through a write. A call its
//     harness gave up on, or its opening answered still running, is recorded as one
//     nobody waited for ([Call.Abandoned]).
//   - A PANIC IS CONTAINED: answered failed, the run's tools closed to it.
//   - A RUN THAT IS OVER REFUSES: a call reaching a session after its run ended or
//     a relaunch replaced it is answered, not run.
//   - THE MODEL A CALL WAS MADE ON. A harness's rounds are invisible to the
//     engine, so each call is told the model the launch pointed the harness at
//     ([Session.Model]), which a seat's code write names on its push.
//   - AN IDENTITY. The per-run token (per-job, ending with the job's opening;
//     nothing secret in the box: internal/runtoken) makes every call the run's
//     SEAT acting; see [seatCaller.Acting] for why the request's own answer
//     cannot be the one.
//
// # A note inbox
//
// A person's notes to the run's turn, at the harness's own boundaries (Claude Code's
// hooks, OpenCode's next step). A run's session carries a note INBOX
// ([Session.Steer]), served two ways, both behind the run's own token only:
//
//   - THE steer_take TOOL ([SteerTool]), a hook's call. NEVER LISTED, so no model
//     is offered it; answered by the server's receiving middleware, never reaching
//     the surface or ledger. Its answer is ALWAYS text, empty for nothing, never an
//     error: an error answer made Claude Code drop and re-initialise the whole
//     bridge.
//   - THE NOTES ROUTE ([SteerPattern]) a plugin polls: GET `notes?after=<seq>` holds
//     up to [steerHold] and answers the notes past the cursor, each recorded served
//     before it is answered ([SteerInbox.Wait]); POST `read` records the notes the
//     harness put before its model plain; POST `again` grants, by id alone, the second
//     admission of notes a compaction folded ([SteerInbox.Again]); POST `folded` names
//     the ones it gave up ([SteerInbox.Folded]).
//
// THE BRIDGE DECIDES NOTHING ABOUT NOTES: what waits, fits, is read and recorded
// is the inbox's (internal/engine, runsteer.go). The bridge routes a call to the
// session holding it ([Bridge.Join]), keeps a hook's call to the main agent's
// boundaries, and bounds how long either is held.
//
// A NOTES CALL THAT REACHES ITS OPENING AS IT ENDS IS TURNED AWAY TO ASK AGAIN
// ([Bridge.serveSteer], [Bridge.pollNotes], [readNotes]): a 503 with a
// Retry-After, its next try routed afresh. NEVER THE 404 of a session with no inbox:
// the OpenCode plugin reads that as a refused token and stops asking for the run.
//
// A READ ([readNotes]) IS RECORDED OR TURNED AWAY TO ASK AGAIN, NEVER ANSWERED
// DONE WITH NOTHING RECORDED: the plugin would never confirm those notes again and
// the record would keep them served and unread. It is turned away when the run's
// tools are closed (a seat leaving closes the fence first, its box going on with the
// next holder) and when the inbox serves no job, which [ErrNoteUntaken] says. The inbox decides under its own lock.
//
// # A run's own skills, and what loading one unlocks
//
// A seat's executor can be handed the company's tool skills as its harness's OWN
// (internal/sandbox, RunRequest.Skills), loaded with the harness's skill tool, which
// tells nobody. So a hook (Claude Code) or plugin (OpenCode) reports each load and the
// bridge records it as THE RUN'S OWN LOAD of the Tool Skill: a load_tool_skill call made
// as a call of the run's ([binding.Call]), so the required-skill guard unlocks what the
// skill covers and every rebuild of the surface replays it. load_tool_skill IS NEVER
// LISTED to such a harness: a second loader would put the same text before the model
// twice. The hook's skill_loaded tool ([SkillTool]) is NEVER LISTED either, so no model
// can unlock a skill it never read, and answers ALWAYS empty text, never an error; the plugin's route ([SkillPattern], [Bridge.serveSkill]) answers 204,
// and 404 for a session handed no skills. A name the run was not handed is answered 204
// and recorded nowhere.
//
// A REPORT THE OPENING CANNOT COUNT IS ANSWERED 204 ALL THE SAME, NEVER TURNED AWAY: the
// skills plugin never posts a load again whatever it is answered and ends its reports on
// a 401 or 404, so a 503 would buy nothing and a refusal would end the reports of a run
// going on with the next holder. The guard makes good a load that did not count: the next
// call to a covered tool is refused naming the skill.
//
// # A call is on its run's record before it runs
//
// A call that ran with no record is one no later segment can count, so a later call
// derives an operation id again, and a tool keyed on it takes the new call for a
// retry of the old or the reverse. So the bridge WRITES EACH CALL'S ENTRY AHEAD OF IT
// ([Ledger.Begin], its identity [Begun]) and RUNS THE CALL ONLY ONCE THE ENTRY IS
// TAKEN ([binding.writeAhead]). Its one update records what it did
// ([binding.conclude], [Ledger.Record]), found by that identity.
//
// AND THE CALL IS IN THE RUN'S CALL LOG ONLY ONCE ITS ENTRY IS. A later segment seeds
// its call log from the run's record alone, so a call admitted on arrival and then
// refused at its entry would raise the next call's count here and nowhere else.
//
// A CALL WHOSE ENTRY CANNOT BE WRITTEN IS NOT RUN: answered FAILED, not recorded and
// may be made again ([unrecordedNote]), or that its run has ended
// ([endedNote]). A call whose entry was written and then not run has it withdrawn
// ([Ledger.Withdraw]); one admitted and then not run by the surface has it updated to
// not run, failed. AN UPDATE THE STORE REFUSES is asked for again until it lands or
// the call's own bound passes ([binding.recordOwed]); the entry then stands with its
// outcome unknown, which every later reader reads as such (internal/sandbox's
// BridgeLog.InFlight).
//
// # A call carries the id its harness gave it
//
// Claude Code hands the server the model's tool_use id in the request's `_meta`. The
// bridge reads it under the key the box's runner declares and no other
// ([Session.CallIDMeta], [Session.callIDOf]) and records it on the entry and its
// update ([Begun.CallID], [Call.CallID]). ONLY AN ID THE LOG KEEPS WHOLE IS TAKEN
// (sandbox.ValidCallID): anything else is recorded as none, NEVER CUT, since two ids
// cut alike would name two calls as one. THE BOX'S WORD, AND NO PART OF THE CALL'S
// IDENTITY: two calls under one id are two calls, since a harness that retries may
// resend its id.
//
// # A call's answer starts at once and keeps going
//
// The SDK's streamable HTTP transport writes nothing on a response until its first
// event, and Claude Code gives that first byte a minute where the server's entry sets
// no timeout, then tells its model the call timed out and cancels nothing, while the
// call runs on. So every call starts its answer the moment its tool starts
// ([toolsession.Call.Running]): a progress on the call's own token, another every
// [answerEvery], none once the tool has returned, and one ahead of the answer however
// soon it returns, since whether a call ran decides it ([binding.Call]). A progress that
// can no longer be written is the response gone: recorded as one nobody waited for.
//
// # How long a box waits for a call
//
// Kept going, a call keeps its harness waiting as long as it runs. So the box is told the
// bridge's bound on every server ([Bridge.CallTimeout]) as its tool timeout: past it the
// harness cancels the call, recorded as one nobody waited for.
//
// # A call longer than its harness waits
//
// A harness may give up on a call at a clock no setting moves, so its runner STATES that
// ceiling (Harness.MCPCallCeiling), and a call still running near it BY THE TOOL LOOP'S
// CLOCK (toolloop's ParkBefore) is answered still running: END YOUR TURN, you will be
// resumed with its result ([binding.park]). NOT A FAILURE. It is first put on the run's row
// among the calls its job parks on ([Session.Park]), which every resume tells once. A call
// sent again once its request was lost JOINS the one still running ([binding.resentLocked]),
// the bridge marking each POST ([bridgedCalls.watchPost]): the SDK cancels nothing of a
// dropped one. Why: docs/concepts/code-sandbox.md, "A call longer than its harness waits".
//
// # An opening ends without waiting for its calls
//
// Every way an opening ends ([Bridge.Close], [Bridge.Open] for a relaunch or a
// recovery, [Bridge.ReleaseSeat]) closes the box's MCP sessions, and the SDK's close
// of a session waits for every request in flight and writes no answer once begun
// (go-sdk): an ending that closed first waited out every running call and
// never wrote its answer.
//
// AN ENDING ANSWERS EVERY CALL STILL RUNNING, THEN CLOSES. Under the opening's lock
// it refuses later calls and answers each call whose tool has not returned
// ([ending]): where the run is over for the box, that it has ended, the call is still
// running and must not be made again, and the turn should end; where the box goes on
// (seat leaving, recovery), that the call is still running and must not be made again.
// NOT A FAILURE, as the tool loop's own "still running" is not
// (internal/agent/toolloop): a model makes a failed call again, where this one's effect
// is still to come. A call whose entry is still being written, or that waits for
// admission, is answered not run, failed ([ending.notRun]), and never runs. The ending
// waits for each answer it gave to be out ([toolsession.Call.Over]) for at most
// [endAnswerWait], then closes the sessions. PAST THAT BOUND IT DOES NOT WAIT: a box
// that does not read its answer must not hold an ending, so the bridge closes the
// sessions behind it ([bridgedCalls]) once the box reads or its connection ends.
//
// THE CALL'S TOOL RUNS ON TO ITS OWN END, and its update is made once it returns
// ([binding.conclude]), as one nobody waited for. WHAT LANDS LATER IS ITS OUTCOME, AND
// ONLY WHERE THE RUN'S LOG STILL TAKES IT: every path that ends a run reads the log and
// deletes its record without waiting for the call, so an update after the delete finds
// no record though the effect happened. THE BRIDGE WAITS FOR IT ([bridgedCalls],
// [Bridge.AwaitCalls]), a node's drain once its seats are given back (internal/engine's
// Engine.Drain). A call so recorded is held by no save point, so the job on its round is
// handed its result from the run's log as it lands, and every later continuation
// is told it but one of a session that took it (ADR-0073).
//
// # The end's bound
//
// [endAnswerWait] is FIVE SECONDS. The end's own answers are written at once, so the
// bound is sized for a call whose tool returned just before the end, whose answer
// follows its record: the entry's update and retries (internal/engine's
// appendRetryWaits, 1.75 s) and, after an activation, the box's re-list hold
// (internal/toolsession's relistWait, two seconds). FIVE, as internal/engine's
// beginBudget. NOTHING HOLDS THE SUM TO THE BOUND: both waits live in other packages, so
// moving either is reading this again. A hook's take is bounded by [steerTakeBudget],
// the same five.
//
// # A hook's answer, held for the end
//
// A HOOK'S CALL NEVER REACHES [binding.Call], which holds every answer it gives: the
// opening's own server answers it in middleware ([binding.steerMiddleware],
// [binding.skillMiddleware]). Unheld, an answer after the end began closing the sessions
// was refused by the SDK, and a take already recorded left its notes handed to a step
// that never had them (a box sends no cancel for an answer it never got). So [binding.holdHook]
// holds the answer as a refused call's is held and reads the note inbox under the same
// lock, the hold first: an end either came before, and the hook takes nothing, or finds
// the hold and waits for the answer ([binding.answersOut]) before closing the sessions.
// THE HOLD ENDS WITH THE REQUEST, BEFORE ANY WRITE where the box gave up or its
// connection failed, which loses nothing: a box that gave up discards a late answer.
//
// # A session opened as its opening ends
//
// THE SDK OPENS THE SESSION AS IT SERVES THE REQUEST, and the end closes the sessions
// the servers hold as it finds them ([binding.closeSessions]). So the end waits for a
// request that opens a session ([binding.holdSessionOpen]), by THE HANDLER'S RETURN,
// NEVER THE REQUEST'S CONTEXT, which ends first where the box gave up while the handler
// goes on, still opening a session it would file after the close looked. All this is
// UNDER ONE LOCK WITH THE END, which marks the opening ended before it waits
// ([binding.endKeeping]).
//
// A REQUEST THAT OPENS A SESSION ONCE ITS OPENING HAS ENDED is TURNED AWAY TO ASK AGAIN
// ([Bridge.serveMCP]), a 503 with a Retry-After, as a notes call is, never the SDK's 400
// "no server available", which Claude Code never retries at startup, leaving the box
// without tools. A call that meets a 503 ([Bridge.forward], [Bridge.heldOnly]) fails,
// the model's or a hook's, and the bridge never routes it again itself. A server
// the opening does not serve keeps the 400. THE MCP HANDLER IS HANDED
// THE OPENING THE ROUTING FOUND ([servedOpening]) and never reads the token again: read
// again, it named no opening where the opening ended between routing and handler, or the
// one a recovery put in its place ([Bridge.serveBegun]).
//
// # A job's token ends with its opening, never at a time
//
// A job's tokens carry NO EXPIRY (runtoken's Lifelong). What ends one is the
// job's opening on the node holding the run's seat, asked on every request
// ([Bridge.held]): the run's end closes it, a relaunch replaces it, the seat
// leaving takes it, and a job id is never minted again. A CAPPED RUN TOO: the
// holder's collection after the cap closes its opening before ending the job
// (internal/sandbox's Coordinator.collect). An expiry at the cap would be read
// on each node's own clock, refusing the run's calls before its cap on a node
// running ahead, and with a bare 401, which a harness reads as a refused
// credential, between the cap and the collection. Without a cap a run works,
// or waits on a person, as long as its turn needs, its token renewed by nothing.
//
// # A run outlives the process that opened its session
//
// The launching process can restart or its seat move while the box goes on calling, so
// nothing the box holds names THAT PROCESS. Its token is signed under the fleet's
// keyring (internal/runtoken) and names the run, the SEAT it acts as and the JOB
// ([Session.Launch]). Whichever node takes the run over re-opens its session under that
// job; the unchanged token resolves there. NOTHING SECRET IS KEPT FOR IT: a sealed
// copy of the bearer would duplicate a check the signature already makes.
//
// # One address for the fleet
//
// WHAT THE TOKEN DOES NOT CARRY IS AN ADDRESS. The box dials [BaseURLVar], the FLEET's:
// any node that serves the route, a load balancer in front of several included. The node
// a call lands on finds the run's holder by the seat's lease and the holder answers,
// relayed over the broker where it is another node ([Bridge.Join], internal/httprelay).
// ADR-0056 records why. A box sharing its launching node's loopback (the `direct`
// cell) dials that node's listener there instead ([Options.HostURL], [FromLoopback];
// api.port, to loopback alone, where it has no ingress role).
//
// # The session is the seat holder's, and only the token is the fleet's
//
// A session holds a live [tools.Surface] (the seat's MCP children, skill guard and
// recording) in the process that holds the seat. So ONLY THE NODE HOLDING THE
// RUN'S SEAT CAN SERVE ITS SESSION. Signing shares authentication across a fleet, not
// the surface. A call is answered by the holder wherever it lands ([Bridge.Join]),
// starting from what it can do for the call's job ([Bridge.verdict]).
//
// "OVER" ONLY ONCE THIS NODE HAS FINISHED TAKING THE SEAT ([Bridge.SeatReady]): until
// then a missing opening says nothing about the run. The opening and the seat's readiness
// are read under ONE LOCK ([Bridge.verdict]), since a seat leaving takes both in one step
// ([Bridge.ReleaseSeat]), and read apart they said that a run whose box goes on with the
// next holder is over.
//
// A RELAYED CALL WHOSE OPENING LEFT AFTER ITS ADMISSION is answered as this node can
// answer it now ([Bridge.heldOnly]): refused where the run is over, turned away to ask
// again where the seat left this node, served by the new opening where a recovery
// opened the job again. Never told that a run going on with the next holder is over: a
// refusal ends OpenCode's notes and skill reports and reads as a refused credential.
//
// # The Host a call may name
//
// ANY, ON A TOKEN THAT RESOLVES. The signed per-run token in the route's path, which a
// page in a browser cannot make, authenticates it, so the SDK's DNS-rebinding guard is off
// ([mcp.StreamableHTTPOptions.DisableLocalhostProtection]): it refused the hop an
// executor's call commonly takes, a reverse proxy or tunnel connecting on 127.0.0.1 and
// passing the public Host on. A leaked token ends with its job's opening.
//
// # A hook's cancel is heard before the SDK
//
// A harness that gives a hook's call up sends `notifications/cancelled`, and the note
// inbox must hear it to serve the note again at the job's next step. The steer
// middleware cannot: the SDK keeps a call's id under an unexported key, answers a
// cancel for a session it never held 404 before any middleware, and a cancel can
// overtake its call. So the bridge's front reads every POST first ([Bridge.steerFront]).
//
// # A harness that never connected its bridge ran without it
//
// Box config can switch `crewlet` off or shadow it, and a harness not reaching the
// bridge at startup goes on without its seat's tools. So the harness LISTING THE
// ENGINE'S OWN SERVER'S TOOLS, never `initialize`, which probes send too, tells the
// session ([Session.Connected]); the engine fails a job ended without it. Every in-box
// harness lists before its first model request (docs/concepts/code-sandbox.md).
//
// # A name `crewlet` cannot list
//
// An unserved server's tool whose name fails [harnessprofile.BridgeToolName] is LISTED
// UNDER AN ALIAS ([aliased]), never dropped: config cannot refuse a name only its
// server lists. An alias hangs on its name alone, so it stays that tool's.
package mcpbridge

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
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

// PathPrefix is the route this bridge is mounted under. The mux, the auth
// exemption (the box holds no API token) and the box's endpoint must agree, or
// every tool call answers 401 with nothing in the config looking wrong.
const PathPrefix = "/mcp/"

// serverName is the bridge's name in the MCP handshake and in a box's server
// list, so a harness sees one tool prefix (`mcp__crewlet__…`) in every phase.
// Config reserves it: an mcp_servers entry of that name would be silently
// overwritten.
const serverName = config.ToolServerName

// Session is one run's live tool surface, as the engine opens it.
//
// It is the caller's, not the run's: the surface, the guard and the recording
// are the ones the caller built, so a bridged call and one made on this host
// are the same call made from two places. It serves its turn's [tools.Surface]
// as the turn's seat.
type Session struct {
	// RunID identifies the run: its token names it and the ledger is keyed by it.
	RunID string

	// Launch is the JOB the session serves: the launch id the run's durable row
	// names. A token names it, so it is good for one job only (a relaunch is a new
	// job); a recovery of the SAME job opens the same session again. Required, and
	// never holding "~" or ".", which delimit a token's parts.
	Launch string

	// LastSeq is the highest [Call.Seq] the run's log records from before this
	// opening (zero for a round's first job): this opening numbers its calls after
	// it, so a resume reads one order across a recovery or a question.
	LastSeq int

	// Handle and Role name the seat, for the logs and the handshake.
	Handle string
	Role   string

	// Model is the model the launch pointed its harness at
	// (sandbox.LaunchRecord.Model), or "" where it left the harness to choose.
	// Every bridged call carries it ([toolloop.CallModel]), so a seat's code write
	// names the same model as its run's other writes.
	Model string

	// CallCeiling is how long the box's harness waits for one call whatever
	// its settings say, as the runner that starts the box states it
	// (sandbox.OpenedJob.CallCeiling); zero for none. A call still running
	// near it is answered first ([binding.park]); none is answered early in a
	// session stating none.
	CallCeiling time.Duration

	// ParkedCalls is how many calls the job is parked on already, as its row
	// holds them (sandbox.OpenedJob.ParkedCalls): zero for a job just launched,
	// the row's count for one served again after its seat moved. An opening
	// counts its own parks after it ([binding.parksFullLocked]).
	ParkedCalls int

	// CallIDMeta is the key of a call's `_meta` the box's harness hands its own
	// id for the call under, as the runner that starts the box declares it
	// (sandbox.OpenedJob.CallIDMeta): the one key every call's id is read by
	// ([Call.CallID]). EMPTY READS NONE, whatever the box sends under any key,
	// the empty one included: a session that declares no key, a test's, reads
	// every call's id as none.
	CallIDMeta string

	// Surface is the live tool surface: the SAME object a tool loop executes
	// against. A seat's is its [*tools.Surface] bound to the run's turn
	// ([tools.Surface.ForTurn]); [Bridge.Open] refuses one that names no seat.
	Surface Surface

	// OnHost is whether the run's box shares this node's loopback — the
	// `direct` cell — and so dials the bridge at this node's own listener
	// ([Options.HostURL]) where the node serves one, rather than at
	// [BaseURLVar]'s address. It moves only the address the token is
	// minted under, never who may call: a seat's call that lands here is
	// still relayed to the seat's holder as any other is.
	OnHost bool

	// Ledger receives every call as it completes. Nil keeps calls in the surface
	// alone, correct for a run living in one process (a test).
	Ledger Ledger

	// Fence is the seat's fence, asked before every call and again after: nil
	// while this node holds the seat and nobody asked it to stop. Otherwise the
	// call in hand and every later one are refused ([binding.Call]). A nil fence
	// is open, a test's.
	Fence func() error

	// Steer is the run's note inbox, which the bridge serves to the run's
	// harness between its steps — see steer.go. Closed with the opening that
	// serves it: the run's end, a relaunch that replaces it and the seat
	// leaving this node all close it. Nil serves no notes: the notes route
	// answers 404 and steer_take answers nothing, as on an engine assembled
	// without note inboxes.
	Steer SteerInbox

	// Skills maps the name the harness lists each skill under to the Tool Skill
	// key: a reported load is recorded as the run's own load of the key (skills.go).
	// Nil hands none: the route answers 404 and load_tool_skill is listed normally.
	Skills map[string]string

	// Park parks the job on a call of its own answered still running before
	// its harness's ceiling ([binding.park]): marked on the run's row among
	// the calls its job parks on (sandbox.Ask.Call), whose resume tells each
	// with its result, whatever answers the job. Nil marks nothing, a test's:
	// the call is then answered still running alone.
	Park func(ctx context.Context, call sandbox.CallWait) (sandbox.AskOutcome, error)

	// Started is told which session the job's box started, the first time a
	// request of the job reaches an opening of its session, BEFORE that
	// request is served: fellBack where it came on the job's other token
	// ([Bridge.FreshEndpoint]) — a session of its own its script started in
	// place of the one its launch continued — and false on the endpoint
	// [Bridge.Open] answered. It holds the run's skill guard to that session,
	// so no call of the run is admitted against the guard of a session it is
	// not in (see started.go). Nil tells nobody, which a test's session has.
	Started func(ctx context.Context, fellBack bool)

	// Connected is told once per opening that the box's harness connected its
	// bridge: it listed the engine's own server's tools (connected.go), under the
	// run's context, as that list is answered. Nil tells nobody, which a
	// test's session has.
	Connected func(ctx context.Context)

	mu sync.Mutex
	// live is the session's current opening; nil until opened.
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
	// DigestOf is the identity a call is admitted under ([tools.Surface.DigestOf]);
	// empty for a surface keeping no call log.
	DigestOf(name string, args map[string]any) string
}

// committing is a [Surface] whose calls commit to an agent's turn: the set
// they commit their changes to, which each call's record carries
// ([Call.Committed]). A seat's *tools.Surface is one.
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

// turnBound is a surface bound to an agent's turn: a seat's *tools.Surface, or
// a surface built on one. Every call it serves acts as the turn's seat.
type turnBound interface {
	Turn() *turnctx.Turn
}

// turn is the agent turn the session's surface is bound to, or nil.
func (s *Session) turn() *turnctx.Turn {
	switch surface := s.Surface.(type) {
	case *tools.Surface:
		if surface != nil {
			return surface.Turn()
		}
	case turnBound:
		return surface.Turn()
	}
	return nil
}

// caller is who the session's every call is decided as: the seat its turn names.
func (s *Session) caller() seatCaller { return seatCaller{turn: s.turn()} }

// seat is the session's surface where it is a seat's own [*tools.Surface], or
// nil: the surface whose MCP servers and tool bounds an opening reads.
func (s *Session) seat() *tools.Surface { return seatOf(s.Surface) }

// Ledger is the durable record of a bridged run's calls, declared by the
// consumer and narrow: this package writes and never reads.
//
// ONE RECORD A CALL, IN TWO STEPS: its entry, written before it runs (Begin),
// and its one update as it finishes (Record), or the entry's withdrawal
// (Withdraw) where it was not run. See the package doc.
type Ledger interface {
	// Begin writes the call's ENTRY before its tool runs ([Begun]); the call runs
	// only once this answered nil. [ErrNotTaken] is a log no longer taking this
	// job's calls; any other error is a write that did not land in the ledger's
	// bound. Either way the call is not run and its box is told so.
	Begin(ctx context.Context, runID string, call Begun) error

	// Record is a finished call's ONE UPDATE: the entry, found by its identity
	// ([Begun]), replaced by the call with its answer, cost and commits; a call
	// answered without running is listed as is. An error is LOGGED, never returned
	// to the box: the tool ran, and failing it would make a harness retry done
	// work. [ErrUnrecorded] is an update the log may still take; the bridge asks
	// again. Nil is taken, or no longer takeable (round over, record gone).
	Record(ctx context.Context, runID string, call Call) error

	// Withdraw takes back the entry of a call written and then not run (opening
	// ended, fence closed). An error is logged: the entry stands as an unknown
	// outcome until its bound.
	Withdraw(ctx context.Context, runID string, call Begun) error
}

// ErrNotTaken is a [Ledger.Begin]'s answer where the run's log no longer takes
// this job's calls (run ended, job replaced by a relaunch, round over): the
// call is told the run has ended ([endedNote]), not to make it again.
var ErrNotTaken = errors.New("mcpbridge: the run's log takes no more calls of this job")

// ErrUnrecorded is a [Ledger.Record]'s answer where the update did not land
// within the ledger's attempts and the log may still take it: the bridge asks
// again once the answer is out ([binding.recordOwed]).
var ErrUnrecorded = errors.New("mcpbridge: the call's update did not land, and the run's log may still take it")

// Begun is a call's IDENTITY as its entry is written ahead of it
// ([Ledger.Begin]), which its update and withdrawal find the entry by: tool,
// place and the instant the bridge took it (two openings of a job can reuse a
// place, never an instant). The harness's [Begun.CallID] rides beside it, no
// part of it.
type Begun struct {
	// Name is the tool the call names.
	Name string
	// Seq is the call's place among the run's calls ([Call.Seq]).
	Seq int
	// StartedAt is when the bridge took the call, in UTC: the instant its
	// record carries too ([Call.StartedAt]).
	StartedAt time.Time
	// Digest is the identity the call was admitted under ([Call.Digest]), EMPTY
	// WHERE ITS TOOL MAY ASK A PERSON FIRST: a digest of sealed arguments would
	// confirm a guess at them (ADR-0077). Such a call counts as one of its tool
	// with unknown arguments, an over-count, the safe direction.
	Digest string
	// Until is the instant past which nobody waits for the call to finish:
	// StartedAt, its tool's own bound and the bridge's margin (wait.go).
	Until time.Time
	// CallID is the id the call's harness gave it ([Call.CallID]), carried by the
	// entry and update; NEVER WHAT THE ENTRY IS FOUND BY, it is the box's word.
	CallID string
}

// Call is one finished bridged call, as the ledger records it.
type Call struct {
	// Call is the call as the surface records it: the tool, its arguments,
	// what it answered, whether it failed and why.
	tools.Call

	// Seq is the call's place in the order calls REACHED the session, after
	// [Session.LastSeq]: the order the harness made them, not the order they finish.
	Seq int

	// StartedAt is when the bridge took the call, in UTC, before its entry was
	// written, so the span includes that write; Duration is the monotonic time to
	// its answer. With Seq and the tool's name it finds the entry ([Begun]). Both
	// zero on a call answered without being taken.
	StartedAt time.Time
	Duration  time.Duration

	// Origin and Server are who answered the call — [toolloop.ToolResult]'s
	// — and empty where no tool did.
	Origin string
	Server string

	// Source is whose tool the call named ([toolloop.ToolResult.Source]); empty
	// where nothing resolved it.
	Source string

	// Held marks a call to a Needs-approval tool recorded as the run's question
	// rather than made ([toolloop.ToolResult.Held], ADR-0077): it ends the job.
	Held bool

	// Digest is the identity the call was ADMITTED under, taken as it arrived;
	// empty for a call never admitted. The record carries it so a later segment
	// seeds the log by the identity it counted.
	//
	// EMPTY FOR A HELD CALL too: a digest beside the tool's name would confirm a
	// guess at its sealed arguments (ADR-0077); it counts as one of its tool with
	// unknown arguments.
	Digest string

	// ArgsUnparsed marks a call whose arguments did not parse: answered, not run,
	// Args empty because the bridge holds no object, not because the model sent
	// none (sandbox.BridgeCall.ArgsUnparsed).
	ArgsUnparsed bool

	// CallID is the id the call's HARNESS gave it, read once from the request's
	// `_meta` under [Session.CallIDMeta] and carried by the entry and its one
	// update, a held call's included. "" where the box sent none the log keeps
	// whole (sandbox.ValidCallID), no key is declared, or a hook reported a skill
	// load. The box's word, NO PART OF THE CALL'S IDENTITY.
	CallID string

	// Absorbed is the calls the run's call log took from the call's OWN WORK (a
	// `delegate`'s workers), recorded with it so a later segment's log counts them
	// ([toolloop.ToolResult.Absorbed]).
	Absorbed []toolloop.Called

	// Committed is the kinds of change the call committed to the run's turn
	// (turnctx.Written), which its collection settles the turn's outcome by.
	Committed []types.CommittedChange

	// Wrote is the records the call's committed writes created or wrote
	// (turnctx.Written.Wrote), named as a wake about each is, which the run's
	// router reads to hand such a wake to the running run (ADR-0167).
	Wrote []types.WrittenRecord

	// Abandoned marks a call NOBODY WAITED FOR when it returned: its harness
	// cancelled or hung up ([toolsession.Call.Abandoned]), or its opening ended
	// while the tool ran ([binding.endKeeping]). The effect happened, but the
	// job's session holds it with no outcome, so no save point may hold it
	// (sandbox.BridgeCall.Abandoned). Decided once ([binding.conclude]); carried
	// by the one update. False for every call answered without running.
	Abandoned bool
}

// Bridge holds the live sessions and the signer that admits them.
type Bridge struct {
	signer *runtoken.Signer
	// base is the URL a box dials, without the token.
	base string
	// host is the URL a box that shares this node's loopback dials — see
	// [Options.HostURL].
	host string

	mu sync.RWMutex
	// sessions is every live opening by id, which a token names.
	sessions map[string]*binding
	// runs is each run's ONE live opening, by run id, the only key the paths that
	// end a run hold.
	runs map[string]*binding
	// ready is every seat this node holds and has finished taking
	// ([Bridge.SeatReady]); see [Bridge.verdict].
	ready map[uuid.UUID]bool

	// streamable is the ONE MCP handler every session is served through: the SDK
	// keeps sessions in its handler's table, so a second handler would know none.
	streamable http.Handler

	// mounted: this process built the route; fleet: it joined its fleet
	// ([Bridge.Join]). Either lets a box's call reach a session opened here.
	mounted atomic.Bool
	fleet   atomic.Pointer[fleet]

	// shutdown is closed once by [Bridge.Shutdown]: held polls are let go and later
	// ones turned away with a 503 ([Bridge.pollNotes]).
	shutdown     chan struct{}
	shutdownOnce sync.Once

	// hold is how long a notes poll is held when none waits: [steerHold].
	hold time.Duration

	// calls is every call the bridge's openings took until recorded, and every
	// close left running ([bridgedCalls]).
	calls *bridgedCalls
}

// Options configure [New].
type Options struct {
	// Material is the fleet's keyring: tokens are signed under its active key and
	// name it, so a rotation leaves live tokens valid. One that cannot sign is
	// refused ([runtoken.Options]).
	Material runtoken.Material

	// BaseURL is where a box reaches the fleet's bridge, e.g.
	// "https://engine.example.com" ([BaseURLVar]). Empty mints no endpoint for a
	// box that does not share this node's loopback, reported as an absent bridge.
	BaseURL string

	// HostURL is where a box sharing THIS NODE'S LOOPBACK (the `direct` cell)
	// reaches the bridge: its own listener on loopback, e.g. "http://127.0.0.1:8080".
	// DERIVED, NEVER CONFIGURED, by the process that bound the listener; preferred
	// to BaseURL because loopback never leaves the host. Empty where the node
	// binds none, whose direct boxes dial BaseURL.
	HostURL string
}

// New builds a bridge, or refuses a keyring that cannot sign for the fleet
// ([runtoken.ErrNoKeyring]).
func New(opts Options) (*Bridge, error) {
	// LIFELONG: a job's token ends with its opening, never at a time (package
	// doc, "A job's token ends with its opening").
	signer, err := runtoken.New(runtoken.Options{Domain: KeyDomain, Material: opts.Material, Lifelong: true})
	if err != nil {
		return nil, fmt.Errorf("mcpbridge: %w", err)
	}
	b := &Bridge{
		signer:   signer,
		base:     strings.TrimSuffix(opts.BaseURL, "/"),
		host:     strings.TrimSuffix(opts.HostURL, "/"),
		sessions: map[string]*binding{},
		runs:     map[string]*binding{},
		ready:    map[uuid.UUID]bool{},
		shutdown: make(chan struct{}),
		hold:     steerHold,
		calls:    newBridgedCalls(),
	}
	// NO IDLE TIMEOUT: a session lives as long as its run, and a harness goes quiet
	// as long as a build takes. NO HOST CHECK, the SDK's included (package doc,
	// "The Host a call may name"). A BODY IS HELD TO WHAT THE RELAY CARRIES
	// ([httprelay.MaxBody]): a call relayed whole is never served on one node and
	// refused on another. THE OPENING IS THE ONE THE ROUTING FOUND
	// ([Bridge.serveMCP]); the token is never read again (package doc, "A session
	// opened as its opening ends"); nil here is the SDK's 400.
	b.streamable = mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		opened, _ := r.Context().Value(servedOpening{}).(*binding)
		if opened == nil {
			return nil
		}
		// THE SERVER THE ROUTE NAMES: the engine's own, or one of the seat's MCP
		// servers ([ServerPattern]); none for one the opening does not serve (400).
		ep := opened.endpoint(r.PathValue("server"))
		if ep == nil {
			return nil
		}
		return ep.server.MCP()
	}, &mcp.StreamableHTTPOptions{DisableLocalhostProtection: true, MaxRequestBodyBytes: httprelay.MaxBody})
	return b, nil
}

// Open registers a run's session and returns the URL its box should dial.
//
// ctx supplies every call's context values and NEVER its cancellation: the
// launching turn's context ends, the run's calls go on.
// OPENING A JOB'S SESSION AGAIN SERVES A RECOVERED RUN, under the same
// [Session.Launch]. The URL is EMPTY when no base reaches the box
// ([Bridge.Reaches]): a real answer, and NOTHING IS REGISTERED THEN, since a
// session with no endpoint is a live surface nothing could close.
func (b *Bridge) Open(ctx context.Context, s *Session) string {
	if b == nil || s == nil {
		return ""
	}
	// A SESSION THAT CANNOT DISPATCH IS WORSE THAN NONE: registered, it would
	// answer a box's handshake and then take the process down on its first
	// tools/list; refused, the caller launches nothing. That covers a surface
	// with no seat (every call is decided as the turn's seat, see
	// [seatCaller.Acting]), a session naming no job (the job tells a
	// relaunch's box from its predecessor's), and a run the token cannot
	// route: a token also names the seat whose holder serves the session
	// ([route]), which the node a call lands on finds it by ([Bridge.Join]).
	// Only an agent seat has the id a seat is named by.
	bound := s.caller().Bound()
	where, routeErr := b.routeOf(s)
	if s.RunID == "" || !validLaunch(s.Launch) || s.Surface == nil || !bound || routeErr != nil {
		log.Error("mcp_bridge_session_incomplete",
			"run_id", s.RunID, "launch_id", s.Launch, "seat", s.Handle,
			"has_surface", s.Surface != nil, "has_caller", bound, "routed", routeErr == nil)
		return ""
	}
	base := b.Reaches(s.OnHost)
	if base == "" {
		log.Warn("mcp_bridge_no_base_url", "run_id", s.RunID, "seat", s.Handle,
			"on_host", s.OnHost)
		return ""
	}
	// NOTHING REACHES IT, NO SESSION: every tool call would fail with no sign of
	// why.
	if !b.Reachable() {
		log.Warn("mcp_bridge_unreachable", "run_id", s.RunID, "seat", s.Handle)
		return ""
	}

	opened := newBinding(ctx, s, where, b.calls)
	// AND A TOKEN ITS SUBJECT CANNOT BE CARRIED IN: a run id holding what
	// internal/redact ends a token's subject at, a dot or a space, or one so
	// long the subject runs past what redaction reads, makes a token no
	// surface knows by its shape, which runtoken refuses to mint
	// (runtoken.ErrSubject). BOTH OF THE JOB'S TOKENS ARE MINTED HERE, its
	// own and its other ([Bridge.FreshEndpoint]), before anything is filed,
	// for the reason no empty answer files anything: the other one, minted
	// when asked for as it was, could be refused only after its session was
	// filed, which a caller reading the refusal as no session opened would
	// leave live.
	token, err := b.signer.MintLifelong(opened.subject(false))
	if err == nil {
		opened.other, err = b.signer.MintLifelong(opened.subject(true))
	}
	if err != nil {
		log.Error("mcp_bridge_token_refused", "run_id", s.RunID, "launch_id", s.Launch, "seat", s.Handle,
			"error", err)
		return ""
	}
	s.mu.Lock()
	s.live = opened
	s.mu.Unlock()

	// THE RUN'S PREVIOUS OPENING is replaced in the same step the new one is
	// filed, so an earlier box keeps no working key to the surface the new run
	// owns.
	b.mu.Lock()
	previous := b.runs[s.RunID]
	if previous != nil {
		delete(b.sessions, previous.id)
	}
	b.sessions[opened.id] = opened
	b.runs[s.RunID] = opened
	b.mu.Unlock()

	if previous != nil {
		// A relaunch or a recovery of the same job. The coordinator does not end a
		// run its own turn relaunched, so nothing else closes the earlier opening; a
		// relaunch's earlier TOKEN stops resolving too, since the subject names the
		// job.
		recovered := previous.id == opened.id
		log.Info("mcp_bridge_session_replaced", "run_id", s.RunID, "launch_id", s.Launch,
			"seat", s.Handle, "recovered", recovered)
		// AND ITS NOTE INBOX, unless the new opening serves the same one. A
		// RECOVERY'S BOX GOES ON under the token it holds; a relaunch's earlier box
		// has had its run end. The earlier opening's calls are told so ([ending]).
		how := runEnded
		if recovered {
			how = boxGoesOn
		}
		previous.endKeeping(opened.steer, how)
	}
	return base + PathPrefix + token
}

// FreshEndpoint is s's live opening's endpoint under the job's OTHER token,
// for a box whose script starts a session of its own (RunRequest.FreshWiring).
// A request on it reaches the same opening and says so ([Session.Started]).
// "" where s has no live opening or no base reaches its box.
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
	return base + PathPrefix + opened.other
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

// subjectSep joins a token's subject: job, route ([route]), run id.
//
// "~", and the run LAST, so the parse is a cut at the first two whatever a run
// id contains — a job's id is refused one ([validLaunch]), and a route is a
// UUID. Two characters are unavailable for it: "." delimits
// [runtoken.Signer.Mint]'s own payload, and the whole token is a URL PATH
// SEGMENT, so anything reserved there is worse than wrong — "#" opens the
// fragment, and a client sends the engine everything before it and keeps the
// rest, which authenticates as a truncated token rather than failing as a
// malformed one. "~" is unreserved (RFC 3986 §2.3) and appears in no UUID.
const subjectSep = "~"

// freshMark closes the job part of the job's OTHER token
// ([Bridge.FreshEndpoint]), so the session a box started is IN THE TOKEN under
// the fleet's signature (started.go). On the job's part, not its own, so an
// older node still finds the seat in the route. "_" is unreserved and refused
// in a job id.
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

// SeatOf is the id of the agent seat a surface acts as, or [ErrSeatless].
// [Bridge.Open] refuses a session whose surface has none.
func SeatOf(surface *tools.Surface) (uuid.UUID, error) {
	if surface == nil {
		return uuid.Nil, ErrSeatless
	}
	return seatIn(surface.Turn())
}

// seatIn is the id of the agent seat turn names, or [ErrSeatless].
func seatIn(turn *turnctx.Turn) (uuid.UUID, error) {
	if turn.Handle() == "" {
		return uuid.Nil, ErrSeatless
	}
	seat, err := uuid.Parse(turn.AgentID())
	if err != nil {
		return uuid.Nil, ErrSeatless
	}
	return seat, nil
}

// route is where a run's session lives: what a node the box's call lands on
// finds the session by ([Bridge.Join]). It is the agent SEAT whose lease names
// its holder: a seat's run outlives its launching process (the next holder
// recovers the job and opens its session again), so its token names the seat.
type route struct {
	// seat is the agent seat whose lease names the session's holder.
	seat uuid.UUID
}

func (r route) String() string { return r.seat.String() }

// parseRoute reads a route off a token's subject.
func parseRoute(text string) (route, bool) {
	seat, err := uuid.Parse(text)
	if err != nil || seat == uuid.Nil {
		return route{}, false
	}
	return route{seat: seat}, true
}

// routeOf is where s's session will live: its seat's ([route]), or
// [ErrSeatless] for a session whose surface names no agent seat.
func (b *Bridge) routeOf(s *Session) (route, error) {
	seat, err := seatIn(s.turn())
	return route{seat: seat}, err
}

// Close ends a run's session: its token resolves to nothing, every MCP session
// a box opened on it is closed, and a call reaching it is refused. The SDK
// keeps an MCP session until a DELETE or idle timeout, and a box torn down
// sends nothing, so each would otherwise pin its seat's surface. IDEMPOTENT:
// completion, failure and recovery all call it. IT WAITS FOR NO CALL STILL
// RUNNING: each is answered that the run ended and the call still runs
// ([endedRunningNote]), its update made as its tool returns
// ([binding.endKeeping]). A call whose entry was still being written is
// answered not run.
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

// subject is what a token names: job, route, run, and whether it is the job's
// other token ([freshMark]).
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

// parse verifies a token's signature and reads its subject; false for
// anything else.
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

// held is this node's live opening of the job a subject names, or nil. TWO
// GATES: the signature ([Bridge.parse]) says this fleet minted the token; the
// map says its opening is still the run's, the one gate that ends a token
// (package doc, "A job's token ends with its opening"). Passing the first and
// failing the second is the ordinary case (a box outliving its run, or a run
// served elsewhere) and must not reach a surface here.
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
// ([Bridge.held]), read with the seat's readiness under one lock, since a seat
// leaving this node takes both in one step ([Bridge.ReleaseSeat]). "OVER" ONLY
// ONCE THIS NODE HAS FINISHED TAKING THE SEAT ([Bridge.SeatReady]). The package
// doc's "The session is the seat holder's, and only the token is the fleet's"
// has why.
func (b *Bridge) verdict(sub subject) (*binding, httprelay.Verdict) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	opened := b.heldLocked(sub)
	switch {
	case !b.ready[sub.route.seat]:
		return opened, httprelay.Elsewhere
	case opened != nil:
		return opened, httprelay.Serve
	default:
		return nil, httprelay.Refuse
	}
}

// Miss says why a token reaches no opening on this node, for the log only: the
// run id where the token is the fleet's, "" where not ([Bridge.Handler]).
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

// FromLoopback reports whether a request arrived on this host's loopback
// interface, read from the connection's own address and never from a header
// a caller could write.
//
// EXPORTED for the listener of a node without the ingress role, which serves
// the routes its own boxes dial to this host's loopback alone (internal/api's
// probe surface).
func FromLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// SeatReady records that this node finished taking a seat: its running jobs'
// sessions are open again here ([Bridge.Open]), so a token for one that reaches
// no opening now names a run that is over. Called after recovery.
func (b *Bridge) SeatReady(seat uuid.UUID) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ready[seat] = true
}

// Ready reports whether this node finished taking a seat and has not released
// it since. For a test.
func (b *Bridge) Ready(seat uuid.UUID) bool {
	if b == nil {
		return false
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.ready[seat]
}

// ReleaseSeat ends every session this node opened for a seat it no longer
// holds, and forgets it took the seat.
//
// THE SESSIONS GO WITH THE SEAT, not their runs, which go on with the next
// holder: an opening left here would pin the seat's live surface and its MCP
// children's credentials in a process that no longer speaks for it. The box's
// next request is routed afresh to the holder.
func (b *Bridge) ReleaseSeat(seat uuid.UUID) {
	// NIL NAMES NO SEAT.
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
	// ([endAnswerWait]), so the seat leaves after the slowest, not the sum. NO CALL
	// STILL RUNNING IS WAITED FOR: each is answered still running
	// ([stillRunningNote]) and makes its update on here; its identity is on the
	// log ([Ledger.Begin]).
	var ended sync.WaitGroup
	for _, opened := range released {
		ended.Go(func() { opened.end(boxGoesOn) })
	}
	ended.Wait()
}

// Reachable reports whether a box's call can reach a session this node opens:
// through its own route ([Bridge.Handler]) or relayed by a fleet node
// ([Bridge.Join]). A bridge with neither (api.port 0) has a base URL and signer
// and no way in; [Bridge.Open] refuses rather than mint a dead endpoint.
func (b *Bridge) Reachable() bool {
	return b != nil && (b.mounted.Load() || b.fleet.Load() != nil)
}

// Handler serves the bridge under [PathPrefix], and marks it mounted.
//
// The token is read off the path once, by the caller's mux pattern: two parses
// of one path let a route authenticate a different string than it dispatches on.
//
// THE NODE HOLDING THE RUN'S SEAT answers, wherever the call landed: a node
// with a live opening whose fence is open answers at once, any other relays to
// the holder ([Bridge.Join]). A node that joined no fleet is the fleet: it
// answers from its own opening, fenced or not, or refuses.
func (b *Bridge) Handler() http.Handler {
	b.mounted.Store(true)
	return b.routed(b.serveMCP)
}

// servedOpening is the request context key [Bridge.serveMCP] hands the MCP
// handler the request's opening under.
type servedOpening struct{}

// sessionHeader names a request's MCP session; a request opening one names none.
const sessionHeader = "Mcp-Session-Id"

// opensSession reports whether r asks the MCP handler to open a session: a
// POST naming none.
func opensSession(r *http.Request) bool {
	return r.Method == http.MethodPost && r.Header.Get(sessionHeader) == ""
}

// serveMCP serves a request on the opening its routing found ([Bridge.routed],
// [Bridge.heldOnly]), held to the session the box started ([Bridge.serveBegun]),
// handed to the MCP handler on the request ([servedOpening]).
// A REQUEST THAT OPENS A SESSION ([opensSession]) is served only by an opening
// not ended, whose end waits for the handler ([binding.holdSessionOpen]); else
// it is TURNED AWAY TO ASK AGAIN, a 503 with a Retry-After, never the SDK's
// 400. Package doc, "A session opened as its opening ends".
func (b *Bridge) serveMCP(w http.ResponseWriter, r *http.Request, opened *binding) {
	if opensSession(r) {
		// HELD UNTIL THE HANDLER HAS RETURNED THE REQUEST, never only until its
		// context ends.
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
	b.steerFront(r, opened)
	// THE REQUEST A CALL CAME ON, marked so its call can tell when the box lost
	// it ([bridgedCalls.watchPost]): the SDK cancels no call whose POST the box
	// dropped.
	post := b.calls.watchPost(r)
	b.streamable.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), servedOpening{}, opened)))
	b.calls.postEnded(post, r.Context().Err() != nil)
}

// routed is the routing every route under the prefix takes, so the MCP
// endpoint and the notes route cannot disagree on which node answers a token;
// serve is handed the opening where this node answers.
func (b *Bridge) routed(serve func(http.ResponseWriter, *http.Request, *binding)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// REFUSED BEFORE THE MCP MACHINERY SEES IT: an unauthenticated caller cannot
		// make this process allocate a session, and the refusal is a plain 401, not a
		// JSON-RPC error a client would retry.
		sub, ok := b.parse(r.PathValue("token"))
		if !ok {
			b.refuse(w, r, "", "the token is forged, malformed or expired")
			return
		}
		opened := b.held(sub)
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
			// FENCED, AND NO FLEET TO DEFER TO: the opening's own answer (tools closed, no
			// more notes) is the answer.
			b.serveBegun(w, r, sub, opened, serve)
			return
		}
		_, reason := b.Miss(r.PathValue("token"))
		b.refuse(w, r, sub.run, reason)
	})
}

// heldOnly serves a call relayed here on the opening its token names, which
// the relay's admission found ([Bridge.admitRelayed]).
//
// AN OPENING GONE SINCE IS ANSWERED AS THIS NODE CAN NOW ([Bridge.verdict]):
// refused where the run is over, turned away to ask again (503 with a
// Retry-After) where the seat left ([Bridge.ReleaseSeat]). NEVER REFUSED
// THERE: OpenCode's plugins read it as the end of notes and skill reports, an
// MCP client as a refused credential, for a run that goes on.
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

// serveBegun serves a request once its opening is held to the session the box
// started ([binding.begin]), the step every route takes, and refuses one on
// the job's other token once the box said which it started ([otherSession]).
func (b *Bridge) serveBegun(w http.ResponseWriter, r *http.Request, sub subject, opened *binding,
	serve func(http.ResponseWriter, *http.Request, *binding)) {

	//nolint:contextcheck // the session is told under the run's own context, as every call of the run is made, never the request's (started.go)
	if !opened.begin(sub) {
		b.refuse(w, r, sub.run, otherSession)
		return
	}
	serve(w, r, opened)
}

// refuse answers a call no opening will take. NO DETAIL: forged, expired and
// "run over" differ, and saying which tells an attacker too.
//
// LEVELLED BY WHO COULD HAVE CAUSED IT: this route is exempt from
// authentication (see [PathPrefix]), so a line per bad token is one anyone can
// write without limit. Only runID set, which takes the signing key, warns.
func (b *Bridge) refuse(w http.ResponseWriter, r *http.Request, runID, reason string) {
	if runID == "" {
		log.DebugContext(r.Context(), "mcp_bridge_unresolved", "reason", reason)
	} else {
		log.WarnContext(r.Context(), "mcp_bridge_unresolved", "run_id", runID, "reason", reason)
	}
	httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
}

// Connections reports how many MCP sessions the current opening holds, for
// health and tests. Sessions, not boxes: a client connect opens more than one
// (the SDK probes with a stateless server/discover whose session is never
// retired).
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

// binding is one opening of a session: the MCP server a job's token reaches
// and the gates every call passes. ITS OWN VALUE, apart from the [Session], so
// a session opened twice is two openings: the first ends and is refused, the
// second serves. A relaunch's token stops resolving (a new job); a recovery's
// does not.
type binding struct {
	// id is the job the opening serves — [Session.Launch] as it was opened,
	// held here because the Session value can be opened again for another.
	id      string
	session *Session
	// servers are the MCP servers served, by route name: "" the engine's own, the
	// seat's MCP servers under theirs (servers.go); order is the same, the
	// engine's first. Set once at build, read-only after.
	servers map[string]*endpoint
	order   []*endpoint
	// caller is who the opening's every call is decided as, fixed when it
	// opened ([Session.caller]).
	caller seatCaller
	// route is where the opening lives ([route]): the second part of what a
	// token names, and, for a seat's, what [Bridge.ReleaseSeat] ends it by.
	route route
	// other is the job's OTHER token ([Bridge.FreshEndpoint]), minted with
	// its own when the opening opened ([Bridge.Open]) and set before it is
	// filed, so read-only after.
	other string

	// ctx is the launch context without its cancellation, what every call runs
	// under. Held, against the rule, because a call arrives on a request whose
	// context is the box's connection ([Bridge.Open]).
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
	// arrived is the place of the last call to reach the opening, from
	// [Session.LastSeq].
	arrived int
	// parked is the calls the job is parked on: [Session.ParkedCalls], then
	// each park the opening made or is making ([binding.park]), one the row
	// did not take given back.
	parked int
	// lastDecided is the last call's decision (admitted or refused), which the next
	// waits for ([bridgedCall.admitAfter]); nil before the first.
	lastDecided <-chan struct{}
	// running is every call taken whose answer is not yet decided and out: what
	// the end answers and waits for. A call leaves as a later one is taken.
	running map[*bridgedCall]struct{}
	// refusals is the request's end of every answer given without running a call
	// ([binding.holdRefusalLocked], [binding.holdHook]) and of every request let
	// open a session ([binding.holdSessionOpen]): what the end waits for before
	// closing the box's sessions.
	refusals []<-chan struct{}

	// steer is the note inbox ([Session.Steer]), closed when the opening ends and
	// nil after; endedSteer is the one its end took, which a cancel still reaches
	// ([binding.cancelInbox]).
	steer, endedSteer SteerInbox

	// callUnread says once, per opening, that a hook's steer_take came with
	// an id the bridge could not read — see [binding.steerMiddleware].
	callUnread sync.Once

	// unsubstituted says once, per opening, that the harness's hooks name no
	// agent the bridge can tell apart — see [binding.takeSteer].
	unsubstituted sync.Once

	// skillUnsubstituted says once, per opening, that the harness's hook names
	// no skill it loaded — see [binding.loadSkill].
	skillUnsubstituted sync.Once

	// startMu holds every request behind the first while the session is told which
	// session the box started ([binding.begin]); started and startedFresh record
	// it. Not mu, which every call takes: what the session is told can write the
	// run's record.
	startMu      sync.Mutex
	started      bool
	startedFresh bool

	// listed tells the session once, per opening, that the harness listed the
	// engine's own server's tools ([binding.listMiddleware]).
	listed sync.Once
}

var _ toolsession.Dispatcher = (*binding)(nil)

// answerEvery is how often a running call's answer is kept going, a
// `notifications/progress` after the one written at start
// ([toolsession.Server.KeepAnswersGoing]). TWENTY SECONDS, held to Claude
// Code's HTTP watchdogs (2.1.310; none cancels the call it gives up): a first
// byte within a minute (the first progress once the call reaches its node); an
// idle check of five minutes, restarted by each progress; and a transport-loss
// check every 30 s giving up a call whose transport was marked lost over 90 s
// ago unless a progress cleared it (routine: the standing GET stream is lost
// 360 s after it opens). Four beats fit in 90 s, so one or two delayed still
// clear it. Not a setting: no deployment moves the harnesses' clocks.
const answerEvery = 20 * time.Second

func newBinding(ctx context.Context, s *Session, where route, calls *bridgedCalls) *binding {
	opened := &binding{id: s.Launch, session: s, caller: s.caller(), route: where,
		ctx: context.WithoutCancel(ctx), arrived: s.LastSeq, parked: s.ParkedCalls, steer: s.Steer,
		calls: calls, running: map[*bridgedCall]struct{}{}}
	// EVERY ENDPOINT IS FILED BEFORE ANY SERVER IS BUILT: a server lists its tools
	// as built, reading which endpoint lists each off the map; one built before
	// its siblings were filed would list theirs.
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
		// EVERY RUNNING CALL'S ANSWER STARTS AT ONCE AND KEEPS GOING ([answerEvery]),
		// a seat's MCP server's included.
		ep.server.KeepAnswersGoing(answerEvery)
	}
	own := opened.servers[""].server
	// steer_take, ANSWERED BEFORE THE TOOLS ARE: never listed, never reaches the
	// surface or record (steer.go); on the engine's own server, where hooks name it.
	own.MCP().AddReceivingMiddleware(opened.steerMiddleware)
	// skill_loaded, ANSWERED THE SAME WAY: a hook's report, never listed (skills.go).
	own.MCP().AddReceivingMiddleware(opened.skillMiddleware)
	// AND THE HARNESS'S LIST OF THESE TOOLS, its word that it connected its bridge
	// (connected.go): on the engine's own server alone.
	own.MCP().AddReceivingMiddleware(opened.listMiddleware)
	// THE ANSWER TO AN ACTIVATION WAITS FOR THE BOX'S RE-LIST
	// ([toolsession.Server.HoldAnswersForRelist]): Claude Code and OpenCode
	// re-read their tool list when told it changed, and answered at once one asks
	// its model again before the re-list lands, offering the activated tool a
	// request late. Unconditional, the bridge not knowing which harness dials; a
	// harness that never re-lists pays only the hold's short bound.
	own.HoldAnswersForRelist()
	return opened
}

// subject is what a token minted for this opening names: the job, the seat,
// and the run.
//
// THE SEAT, so a node a call lands on finds the holder from the token alone.
// THE JOB'S ID, not the run id (a relaunch reuses it, so the superseded box
// would drive the new session) and not a random id per opening (a restart
// loses it): the run's durable row carries it to whichever node reopens the
// job. fresh names the job's OTHER token ([Bridge.FreshEndpoint]).
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

// Offered is what the surface offers now, with the registry's hints so a
// client can tell a read from a write; the bridge's own skill loader is left
// out for a harness loading skills itself ([binding.offeredTool]).
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
	// endedNote answers a call reaching an opening after its end where the run
	// ended for its box ([runEnded]); a box that goes on is told [closingNote].
	endedNote = "This run has ended; this call was not run. End your turn now."

	// stoppedNote answers a call once the executor's tools are closed (seat moved,
	// stopped, or a tool failed).
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
// The request's context is not handed over (package doc, "a call is the run's,
// not the request's"). ITS ENTRY IS WRITTEN AHEAD OF IT, AND ITS TOOL RUNS ON
// A GOROUTINE OF THE BRIDGE'S ([bridgedCalls]): only a call whose entry the
// log took runs ([binding.writeAhead]). It is answered by whichever comes
// first: a refusal, its own result once recorded ([binding.conclude]), or the
// opening's end ([binding.endKeeping]). THE ID ITS HARNESS GAVE IT is read
// once ([Session.callIDOf]) and every record carries it.
func (o *binding) Call(in toolsession.Call) toolsession.Result {
	s := o.session
	// THE FENCE BEFORE ANY CALL, outside the lock: it reads the node's copy of the
	// seat's grant and pauses, and nothing it answers depends on another call.
	fenced := fence(s)
	// THE ID ITS HARNESS GAVE IT, outside the lock: the key is fixed before the
	// session opens.
	callID := s.callIDOf(in.Meta)

	o.mu.Lock()
	if o.ended {
		// NOT RECORDED: the call never ran, and the record is not this
		// NOT RECORDED: it never ran, and the record is not this opening's to write
		// (the run is over, a relaunch's job holds it, or the opening serving its box
		// now does). TOLD AS THE OPENING ENDED ([ending.notRun]), and HELD until that
		// answer is out, which the end waits for before closing the box's sessions.
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
		// REFUSED, AND RECORDED in its place: the box made this call after its tools
		// closed, which a reviewer should see.
		o.holdRefusalLocked(in.Over)
		o.mu.Unlock()
		o.record(Call{Call: tools.Call{Name: in.Name, Args: args, Output: stoppedNote,
			Failed: true, Cause: tools.RefusalNotRunning}, Seq: seq, CallID: callID}, time.Time{})
		return refused(stoppedNote)
	}
	if in.ArgumentsError != "" {
		// ANSWERED, NOT RUN, and recorded: an empty object in place of what the model
		// wrote is a call it never made, so the record says its arguments did not parse
		// (ArgsUnparsed). Not admitted to the call log, since nothing ran.
		o.holdRefusalLocked(in.Over)
		o.mu.Unlock()
		out := toolloop.UnparsedArguments(in.Name, in.ArgumentsError)
		o.record(Call{Call: tools.Call{Name: in.Name, Output: out, Failed: true,
			Cause: tools.RefusalInvalid}, Seq: seq, CallID: callID, ArgsUnparsed: true}, time.Time{})
		return refused(out)
	}
	// THE IDENTITY IT WILL BE COUNTED BY in the call log, carried by its entry and
	// record. NOT ITS PLACE YET: it is admitted only once its entry is on the
	// run's record, in arrival order ([binding.writeAhead]).
	digest := s.Surface.DigestOf(in.Name, args)
	// THE SAME CALL SENT AGAIN by a harness whose request for it was lost, while
	// it runs and nobody waits for it, JOINS IT ([binding.join]).
	request := callRequest{over: in.Over, lost: o.calls.lostOf(in.Header)}
	if first := o.resentLocked(in.Name, digest); first != nil {
		first.waiters = append(first.waiters, request)
		o.holdRefusalLocked(in.Over)
		o.mu.Unlock()
		return o.join(first, in, seq)
	}
	// NOT RUN, NOT RECORDED, AND ANSWERED AT ONCE where it could park the job
	// past its cap ([binding.parksFullLocked]): no call is admitted that a park
	// could not then hold.
	if note := o.parksFullLocked(); note != "" {
		o.holdRefusalLocked(in.Over)
		o.mu.Unlock()
		log.WarnContext(o.ctx, "mcp_bridge_call_refused_parks_full", "run_id", s.RunID, "launch_id", o.id,
			"seat", s.Handle, "tool", in.Name, "seq", seq, "cap", sandbox.MaxParkedCalls,
			"detail", "the box made a call while as many of its calls as its job may park on were parked "+
				"or still running; it was answered at once and not run")
		return refused(note)
	}
	// HELD BY THE OPENING FROM HERE, under the lock its end takes: an end either
	// came before (the call is refused above) or finds the call and answers it:
	// not run where its entry is still being written or it awaits admission, still
	// running where its tool runs. IN ARRIVAL ORDER: it is admitted or refused
	// only once the call before it has been ([bridgedCall.admitAfter]).
	c := o.holdLocked(in.Over)
	c.admitAfter, c.decided = o.lastDecided, make(chan struct{})
	c.name, c.digest, c.request = in.Name, digest, request
	o.lastDecided = c.decided
	o.mu.Unlock()

	// ITS IDENTITY, written ahead of it ([Begun]): the instant the bridge took the
	// call (also in its record, so the span includes the write), the bound past
	// which nobody waits ([callBound]), and a digest unless its tool may hold it
	// for a person first ([asksFirst]).
	started := time.Now()
	// ITS HARNESS'S CEILING COUNTS FROM HERE, as its own clock does from the
	// request ([binding.park]).
	parks, stopParks := parkClock(s.CallCeiling)
	defer stopParks()
	entry := Begun{Name: in.Name, Seq: seq, StartedAt: started.UTC(), Digest: digest,
		Until: started.Add(callBound(s.Surface, in.Name) + callMargin).UTC(), CallID: callID}
	if asksFirst(s.Surface, in.Name) {
		entry.Digest = ""
	}

	// THE ENTRY IS WRITTEN, THE CALL ADMITTED, ITS TOOL RUN AND RECORDED, ON A
	// GOROUTINE OF ITS OWN held by the bridge until the record is made: the answer
	// can then come before the tool returns where the opening ends first, and the
	// tool still runs to its end. A call whose entry is refused is answered at
	// once; what is left after its answer (completing a refusal's entry, or an
	// update the store has not taken) runs after it ([binding.writeAhead],
	// [binding.conclude]).
	//
	// notRun answers a call the opening refused; concluded one whose tool ran.
	notRun, concluded := make(chan answer, 1), make(chan answer, 1)
	go func() {
		defer o.calls.done(c)
		refusal, admitted, after := o.writeAhead(c, entry, args, digest)
		if admitted == nil {
			notRun <- refusal
			if after != nil {
				after()
			}
			return
		}
		written := writtenOf(s.Surface)
		r := ran{name: in.Name, args: args, digest: digest, seq: seq, callID: callID, abandoned: in.Abandoned,
			written: written, before: written.Changes(), wroteBefore: written.Records(), started: started,
			until: entry.Until}
		r.res, r.broke, r.err = o.execute(admitted, in.Name, args)
		r.took = time.Since(r.started)
		a, after := o.conclude(c, r)
		if a.own {
			// FOR EVERY REQUEST THAT JOINED IT ([binding.join]) too.
			c.final = a.result
			close(c.settled)
		}
		concluded <- a
		if after != nil {
			after()
		}
	}()

	// ITS ANSWER STARTS ONCE ITS TOOL RUNS, past every answer given without
	// running it, and is kept going while it runs ([toolsession.Call.Running]); a
	// call the surface goes on to refuse or hold is answered a moment after its
	// first progress, since telling those apart here would be this package's own
	// idea of the surface's gates. Nil for a call no request carries.
	//
	// began and notRun are never both ready; once cut is closed, unrun says
	// whether the end found the call begun.
	if o.calls.answering != nil {
		o.calls.answering(c.recorded)
	}
	select {
	case <-c.began:
	case a := <-notRun:
		return c.answerOf(a)
	case <-c.cut:
		if c.unrun {
			return c.cutAnswer()
		}
	}
	if in.Running != nil {
		in.Running()
	}
	for {
		select {
		case a := <-concluded:
			return c.answerOf(a)
		case <-c.cut:
			return c.cutAnswer()
		case <-parks:
			// Answered here where its own answer is not decided first.
			o.park(c, in.Name, seq, s.CallCeiling, time.Since(started))
			parks = nil
		}
	}
}

// resentLocked is the call running on the opening that a call to name with
// arguments of digest sends again, nil where none is: one begun whose every
// request is gone ([toolsession.Call.Over]), its own and each that joined it,
// and whose answer is not decided or is a park's no request read ([parkUnread]).
// Under mu.
//
// WHY: Kimi Code sends a call again, under an id of its own, once the
// connection its request was on fails (measured on 2.7.2: twice after Node's
// fetch dropped a silent response), and the box's run never sees the first's
// answer, a park's included. Run again, the tool takes effect twice. A
// duplicate while a request for the first still waits, or once the box read its
// park, is a call the run made twice on purpose, and runs.
func (o *binding) resentLocked(name, digest string) *bridgedCall {
	for c := range o.running {
		if c.begun && c.name == name && c.digest == digest &&
			(!c.answeredLocked() && c.request.gone() && !joinerWaitsLocked(c) || parkUnread(c)) {
			return c
		}
	}
	return nil
}

// parkUnread reports whether c was answered by a park ([binding.park]) that
// reached the box on no request: its own POST and each joiner's dropped
// ([bridgedCalls.watchPost]) before the answer could be read. Under mu. A POST
// that ended normally carried the answer to the box, which then knows the call.
func parkUnread(c *bridgedCall) bool {
	return c.cutFirst && !c.own && !c.unrun && gone(c.request.lost) &&
		!slices.ContainsFunc(c.waiters, func(r callRequest) bool { return !gone(r.lost) })
}

// join answers a call its harness sent again ([binding.resentLocked]) with the
// first's answer: its own result once it returns, or the opening's answer
// before it (its end, or a park before its harness's ceiling, which the
// first's clock, started earlier, reaches first). Nothing runs and nothing is
// recorded for it: it is the same call. seq is the number it arrived as, said
// in the log line.
func (o *binding) join(c *bridgedCall, in toolsession.Call, seq int) toolsession.Result {
	s := o.session
	log.InfoContext(o.ctx, "mcp_bridge_call_joined", "run_id", s.RunID, "launch_id", o.id,
		"seat", s.Handle, "tool", in.Name, "seq", seq,
		"detail", "the box sent a call again whose request was lost while the first still ran; it is "+
			"answered with the first's answer and runs nothing")
	if in.Running != nil {
		in.Running()
	}
	if o.calls.joined != nil {
		o.calls.joined()
	}
	select {
	case <-c.settled:
		return c.final
	case <-c.cut:
		return c.cutAnswer()
	}
}

// nobodyJoined reports whether no request that joined c ([binding.join]) is
// still there to take its answer.
func (o *binding) nobodyJoined(c *bridgedCall) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return !joinerWaitsLocked(c)
}

// joinerWaitsLocked reports whether a request that joined c is still there.
// Under the opening's mu.
func joinerWaitsLocked(c *bridgedCall) bool {
	return slices.ContainsFunc(c.waiters, func(r callRequest) bool { return !r.gone() })
}

// callRequest is the request a call came on, as the bridge can tell it ended
// before its answer: over is its [toolsession.Call.Over] (a cancel, or a
// relay's socket closing), lost the box dropping the POST it came on
// ([bridgedCalls.watchPost]). Nil for a signal nothing gives.
type callRequest struct{ over, lost <-chan struct{} }

// gone reports whether the request ended before its answer, as far as the
// bridge can tell: a call no request carries is never gone.
func (r callRequest) gone() bool { return gone(r.over) || gone(r.lost) }

// parkClock is when a call arriving now is answered still running, its tool
// still running ([binding.park]): [toolloop.ParkBefore] the run's ceiling
// ([Session.CallCeiling]), the clock the tool loop parks a call by, and never
// where its runner states none. stop releases the clock.
func parkClock(ceiling time.Duration) (parks <-chan time.Time, stop func() bool) {
	if ceiling <= 0 {
		return nil, func() bool { return false }
	}
	t := time.NewTimer(toolloop.ParkBefore(ceiling))
	return t.C, t.Stop
}

// parkedNote answers a call still running near its harness's ceiling, its job
// parked on it ([binding.park]): NOT A FAILURE, as [endedRunningNote] is not.
// The run is carried on once it ends, its session told the call and what it
// returned, as the tool loop's own "still running" tells (internal/agent/toolloop).
const parkedNote = "This call is still running, longer than your harness waits for one call. " +
	"END YOUR TURN NOW: you will be resumed with its result. Do not make the call again."

// parksFullNote answers a call past the calls its job may park on, the job
// parked on some ([binding.parksFullLocked]): FAILED, as nothing ran, and
// telling the turn to end, as the parked calls' answers did.
const parksFullNote = "Too many calls are waiting on this run; this one was not run. " +
	"END YOUR TURN NOW: you will be resumed with the results of the calls already running."

// callsFullNote answers a call past the calls its job may park on, none of
// them parked yet ([binding.parksFullLocked]): FAILED, and promising no resume,
// since the calls running may all return in time.
const callsFullNote = "Too many of this run's calls are running at once; this one was not run. " +
	"Make it again once they have returned, if it is still needed."

// park answers c, still running, before its harness's ceiling gives it up,
// unless its answer is decided already, and parks its job on it: the run's
// row is marked ([Session.Park]) BEFORE the answer, so a harness that ends its
// turn at once is collected parked on the call, and BEFORE the call's record
// ([binding.conclude] waits for the mark), so the job working on as it lands
// is handed no item of it that its relaunch then would not tell. NOT A
// FAILURE. Where the mark is not made, the answer promises no resume: an
// ended run is told so ([endedRunningNote]), any other only that the call
// runs on ([stillRunningNote]). See the package doc, "A call longer than its
// harness waits".
func (o *binding) park(c *bridgedCall, name string, seq int, ceiling, after time.Duration) {
	o.mu.Lock()
	if c.answeredLocked() {
		o.mu.Unlock()
		return
	}
	marked := make(chan struct{})
	c.cutFirst, c.marked = true, marked
	o.parked++
	o.mu.Unlock()

	s := o.session
	note, outcome := stillRunningNote, sandbox.AskNotRunning
	var err error
	if s.Park != nil {
		outcome, err = s.Park(o.ctx, sandbox.CallWait{Tool: name, Seq: seq})
	}
	switch {
	case err != nil:
		log.WarnContext(o.ctx, "mcp_bridge_call_park_unrecorded", "run_id", s.RunID, "launch_id", o.id,
			"seat", s.Handle, "tool", name, "seq", seq, "error", err.Error(),
			"detail", "the call was answered still running without parking its job on it, so its result "+
				"reaches the run only as a call nobody waited for")
	case outcome == sandbox.AskRecorded:
		// PARKED: the call is on the job's row, whose resume tells it,
		// whatever else the job asked (internal/sandbox's callwait.go).
		note = parkedNote
	case outcome == sandbox.AskNotRunning && s.Park != nil:
		note = endedRunningNote
	}
	close(marked)
	o.mu.Lock()
	if note != parkedNote {
		o.parked--
	}
	c.note = note
	close(c.cut)
	o.mu.Unlock()
	log.InfoContext(o.ctx, "mcp_bridge_call_parked", "run_id", s.RunID, "launch_id", o.id,
		"seat", s.Handle, "tool", name, "seq", seq, "after", after.String(), "ceiling", ceiling.String(),
		"parked", string(outcome),
		"detail", "the box was answered that the call is still running, before its harness gives up "+
			"on it; the call runs on to its own end, and the run is carried on with it once collected")
}

// parksFullLocked is the answer of a call reaching the opening while the
// calls its job is parked on and those of its calls that could still park
// (taken, their answer not decided) number [sandbox.MaxParkedCalls], and ""
// where the call may be taken; under mu. Only on a session with a ceiling,
// the one that parks. Where the job is parked, it is told to end its turn
// ([parksFullNote]), the resume telling it the parked calls' results; where
// none is parked yet, nothing promises a resume ([callsFullNote]).
func (o *binding) parksFullLocked() string {
	if o.session.CallCeiling <= 0 {
		return ""
	}
	could := 0
	for c := range o.running {
		if !c.answeredLocked() {
			could++
		}
	}
	switch {
	case o.parked+could < sandbox.MaxParkedCalls:
		return ""
	case o.parked > 0:
		return parksFullNote
	default:
		return callsFullNote
	}
}

// writeAhead writes call c's entry ahead of it ([Ledger.Begin]) on the call's
// goroutine and decides ONCE, under the opening's lock, whether its tool runs.
// Where it does, admitted is the call ADMITTED to the call log
// ([Surface.Admit]) and BEGUN, so an end after answers it still running. Where
// not, admitted is nil, with the call's answer and what is left for its entry
// once answered (after). SO A CALL RUNS ONLY WITH ITS IDENTITY ON THE RECORD,
// and an unrun entry is completed in the same step. IT IS IN THE CALL LOG ONLY
// ONCE ITS ENTRY IS: a resumed segment seeds its log from the record alone, so
// a call admitted then not run would make it reuse an operation id
// ([turnctx.CallLog]).
func (o *binding) writeAhead(c *bridgedCall, entry Begun, args map[string]any, digest string,
) (answer, toolloop.Admitted, func()) {
	s := o.session
	var err error
	if s.Ledger != nil {
		err = s.Ledger.Begin(o.ctx, s.RunID, entry)
	}
	// IN ARRIVAL ORDER, which the count depends on ([tools.Surface.Admit]): a call
	// is decided only once the one before it has been, whichever write returned
	// first.
	if c.admitAfter != nil {
		<-c.admitAfter
	}
	// THE FENCE AGAIN, outside the lock: the seat may have left or been stopped
	// while the entry was written. Read at this call's turn.
	fenced := fence(s)
	withdraw := func() { o.withdraw(entry) }
	o.mu.Lock()
	defer o.mu.Unlock()
	defer close(c.decided)
	switch {
	case c.cutFirst:
		// THE OPENING'S END ANSWERED IT ([binding.endKeeping]) as not run, the one
		// answer a call not yet begun can get before its own: that stands,
		// and the entry is withdrawn wherever the log may have taken it.
		if errors.Is(err, ErrNotTaken) {
			return answer{}, nil, nil
		}
		return answer{}, nil, withdraw
	case err != nil:
		// THE LOG DID NOT TAKE ITS ENTRY: a store refusing every attempt, or a log at
		// its in-flight limit, tells the call it may be made again ([unrecordedNote]);
		// a log taking no more calls of this job ([ErrNotTaken]), that the run ended
		// ([endedNote]). Nothing ran.
		c.own = true
		if errors.Is(err, ErrNotTaken) {
			return answer{result: refused(endedNote), own: true}, nil, nil
		}
		return answer{result: refused(unrecordedNote), own: true}, nil, withdraw
	case o.stopped != nil:
		// TOOLS CLOSED MEANWHILE ([binding.stop]): refused ([stoppedNote]) and
		// RECORDED in its place, its one update, carrying its call id.
		c.own = true
		return answer{result: refused(stoppedNote), own: true}, nil, func() {
			o.recordUntil(Call{Call: tools.Call{Name: entry.Name, Args: args, Output: stoppedNote,
				Failed: true, Cause: tools.RefusalNotRunning}, Seq: entry.Seq,
				StartedAt: entry.StartedAt, Digest: digest, CallID: entry.CallID}, entry.Until)
		}
	case fenced != nil:
		// THE SEAT'S FENCE, ASKED AGAIN, IS CLOSED: told the session was closing and
		// to call again ([closingNote]); entry withdrawn. THE OPENING IS NOT STOPPED:
		// the end that follows decides what the box is told, and a stop here would
		// tell a box that goes on to end its turn.
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
	// callID is the harness's id ([Call.CallID]), carried by every update.
	callID string
	// abandoned is the call's request's [toolsession.Call.Abandoned].
	abandoned <-chan struct{}
	// written is the set the call commits to; before and wroteBefore are its
	// kinds and records as the tool started.
	written     *turnctx.Written
	before      []types.CommittedChange
	wroteBefore []types.WrittenRecord
	// started is when the bridge took the call; took how long to its answer.
	started time.Time
	took    time.Duration
	// until is the entry's bound ([Begun.Until]): past it nobody waits and an
	// unlanded update is no longer asked for.
	until time.Time
	// res, broke and err are what the surface answered ([binding.execute]).
	res   toolloop.ToolResult
	broke *turn.PanicError
	err   error
}

// answer is a record step's result: the call's answer, and whether it is the
// call's at all (own) or the opening's end answered first.
type answer struct {
	result toolsession.Result
	own    bool
}

// answerOf is the box's answer to c: a's own, unless the opening answered first
// (its end or a park), though a arrived before the handler saw it.
func (c *bridgedCall) answerOf(a answer) toolsession.Result {
	if a.own {
		return a.result
	}
	return c.cutAnswer()
}

// conclude is a bridged call's ONE RECORD STEP, once, on its goroutine after its
// tool returned. WHO ANSWERS is decided first, under the opening's lock
// ([binding.claim]): its own result where the opening has not answered it,
// else the opening's ([binding.endKeeping], [binding.park]), so answer and
// record agree on whether anybody waited. It records ONE UPDATE ([binding.record], [Ledger.Record]), marked
// abandoned where nobody waited; an update the ledger could not land
// ([ErrUnrecorded]) is asked again ([binding.recordOwed]). It MAY RUN AFTER
// THE OPENING ENDED, even after the record was deleted: the identity is on the
// log from before it ran. It NEVER FAILS THE CALL BACK over its record, closes
// the tools where the call broke or the fence closed, and leaves a late mark to
// the log ([sandbox.BridgeCall.Late]).
func (o *binding) conclude(c *bridgedCall, r ran) (answer, func()) {
	s := o.session
	// FIRST, as the tool returns, so an end from here on leaves the call to its own
	// answer.
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
		// The surface errors only for a call that could not run at all (its turn torn
		// down), and nothing was recorded.
		//
		// IN WORDS OF ITS OWN, never the error's: the caller is a model that can do
		// nothing with a Go error's text. The error goes to this node's log.
		log.ErrorContext(o.ctx, "mcp_bridge_dispatch_failed",
			"run_id", s.RunID, "seat", s.Handle, "tool", r.name, "error", r.err)
		// ITS ENTRY UPDATED TO A CALL THAT WAS NOT RUN, failed, never withdrawn: the
		// call log admitted it ([binding.writeAhead]), so the record keeps the identity
		// this segment counted for a later segment to count too. The over-count is the
		// safe direction ([turnctx.CallLog]); an uncounted-on-record call that this
		// segment counted is the under-count no record may make.
		after := o.record(Call{Call: tools.Call{Name: r.name, Args: r.args, Output: unrunNote,
			Failed: true, Cause: tools.RefusalNotRunning}, Seq: r.seq, StartedAt: r.started.UTC(),
			Duration: r.took, Digest: r.digest, CallID: r.callID, Abandoned: !own || gone(r.abandoned)}, r.until)
		return answer{result: refused(unrunNote), own: own}, after
	case res.Suspend:
		// A SUSPEND CANNOT CROSS THIS BOUNDARY: it parks a tool loop with the
		// call unanswered, and a detached run's loop is its harness's own,
		// which would wait forever. No seat tool asks for it, so this guards
		// a tool added later. REFUSED, AND RECORDED as a call that ran, which
		// it did: forbidden, refused for where it was called.
		log.ErrorContext(o.ctx, "mcp_bridge_suspend_refused",
			"run_id", s.RunID, "seat", s.Handle, "tool", r.name)
		res = toolloop.ToolResult{Output: suspendNote, Failed: true,
			Cause: tools.RefusalForbidden, Origin: res.Origin, Server: res.Server,
			Source: res.Source, Absorbed: res.Absorbed}
	}

	// AND THE FENCE AGAIN ONCE THE TOOL HAS RETURNED. What it did is done,
	// whichever answer its box got, but nothing after runs as a seat this node no
	// longer holds.
	if after := fence(s); after != nil {
		o.stop(after)
	}
	// WHETHER ANYBODY WAITED ([Call.Abandoned]): nobody where the opening
	// answered first (its end, or a park), or the harness cancelled, or a
	// progress found the response gone, or the box dropped the POST the call
	// came on, by the time the tool returned, unless a call sent again joined
	// it ([binding.join]). A hang-up is seen only at the next progress, so one
	// in the last cadence before the tool returns is not seen.
	abandoned := !own || ((gone(r.abandoned) || gone(c.request.lost)) && o.nobodyJoined(c))
	recorded, digest := r.args, r.digest
	if res.Held {
		// A HELD CALL'S ARGUMENTS ARE NOT LOGGED: they ride the run's row sealed
		// (ADR-0077) and the call log is read in the clear. NOR THEIR DIGEST, which
		// would confirm a guess at them; the call counts as one of its tool with
		// unknown arguments.
		recorded, digest = nil, ""
	}
	output := res.Output
	if res.Unkept {
		output = tools.UnkeptOutput(output)
	}
	after := o.record(Call{
		Call: tools.Call{Name: r.name, Args: recorded, Output: output, Failed: res.Failed,
			Images: crewletmcp.ImageRecords(res.Images), Cause: res.Cause},
		Seq: r.seq, StartedAt: r.started.UTC(), Duration: r.took,
		Origin: res.Origin, Server: res.Server, Source: res.Source, Held: res.Held,
		Digest: digest, CallID: r.callID, Absorbed: res.Absorbed, Committed: r.written.Since(r.before),
		Wrote: r.written.RecordsSince(r.wroteBefore), Abandoned: abandoned,
	}, r.until)
	return answer{result: toolsession.Result{Output: res.Output, Failed: res.Failed, Images: res.Images},
		own: own}, after
}

// claim decides who answers c now that its tool has returned, once, under the
// opening's lock: its own result, unless the opening answered it first (its
// end, or its harness's ceiling: [binding.park], whose mark on the run's row
// it waits for).
func (o *binding) claim(c *bridgedCall) bool {
	o.mu.Lock()
	marked := c.marked
	if c.cutFirst {
		o.mu.Unlock()
		if marked != nil {
			// A PARK'S MARK FIRST ([binding.park]): its record is the job's
			// park's to tell.
			<-marked
		}
		return false
	}
	c.own = true
	o.mu.Unlock()
	return true
}

// execute runs one admitted call as the run's seat, under the run's context,
// and contains a panic — broke is it, recovered.
func (o *binding) execute(admitted toolloop.Admitted, name string, args map[string]any,
) (res toolloop.ToolResult, broke *turn.PanicError, err error) {
	defer func() {
		if v := recover(); v != nil {
			// WHAT THE CALL'S OWN WORK HAD RECORDED when it broke stays on the run's log;
			// keep it for the call's record ([toolloop.CallPanic]).
			value, absorbed := toolloop.PanicValue(v)
			broke = turn.Recovered(panics.ScopeToolCall, value)
			res, err = toolloop.ToolResult{Absorbed: absorbed}, nil
		}
	}()
	// THE SESSION'S JOB rides the call, so a tool writing the run's own row (a
	// question it parks on) writes the job that made the call, or nothing
	// ([turnctx.WithJob]).
	//
	// AND THE MODEL THE CALL WAS MADE ON, set here only: the one model the engine
	// knows a harness's loop runs is the one the launch named ([Session.Model]).
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
// AN ENDED OPENING IS NOT STOPPED: its end already refuses later calls in its
// own words ([ending.notRun]), and a stop would falsely tell the harness to end
// its turn on a run whose box may go on. A call running as its seat leaves
// meets exactly this as its tool returns ([binding.conclude]): the fence is
// closed, on an ended opening.
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
type seatCaller struct{ turn *turnctx.Turn }

// Bound reports whether the surface's turn names a seat.
func (c seatCaller) Bound() bool { return c.turn.Handle() != "" }

// Acting is the context one bridged call runs under: the run's own SEAT,
// replacing whatever identity the context carries.
//
// The per-run token is the route's whole authentication: the caller is the box
// acting for the turn's seat ([turnctx.Principal]). The credential guard still
// RESOLVES the request ([iam.Anonymous], or the -dev-principal), and the launch
// context may carry a person; WithPrincipal lets either win, so the seat is
// REPLACED HERE, per call, which also keeps a development principal off a
// seat's call.
func (c seatCaller) Acting(ctx context.Context) context.Context {
	seat := turnctx.Principal(c.turn)
	if seat.Seat == "" {
		// UNREACHABLE past [Bridge.Open]. Answered as unknown rather than left alone,
		// so a surface that lost its turn is never decided as whoever the context names.
		return iam.WithUnresolved(ctx, errNoSeat)
	}
	return iam.WithPrincipal(ctx, seat)
}

// errNoSeat is why a bridged call with no seat to act as cannot be decided.
var errNoSeat = errors.New("mcpbridge: this run's tool surface is bound to no " +
	"seat, so a bridged call has nobody to act as")

// record makes one call's one update on the ledger ([Ledger.Record]) and
// answers what is left once the call's answer is out: nil where the update
// landed or is not to be asked again; else ([ErrUnrecorded]) the same update
// asked again until the call's bound ([binding.recordOwed]); a call with no
// bound (no entry written) is asked once. NEVER FAILED BACK TO THE BOX: the
// tool ran, and failing the call would make a harness retry done work. Under
// the run's context, not the request's, so a hang-up does not take the row.
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
// update the ledger could not land ([binding.recordOwed]): from ONE SECOND, the
// last of the engine's waits between its attempts (internal/engine's
// appendRetryWaits), doubling to FIVE, past the four seconds and more a
// stream's followers wait to replace a lost leader and what a write ahead of a
// call is given (internal/engine's beginBudget); jittered by a fifth so calls
// owed together do not ask in step. Each ask is the ledger's four attempts
// (1.75 s of waits), so a store out for long sees about one update per seven
// seconds.
const (
	owedUpdateBase    = time.Second
	owedUpdateCeiling = 5 * time.Second
	owedUpdateJitter  = 0.2
)

// recordOwed asks again for the update of a call the ledger could not land
// ([ErrUnrecorded]), on the call's goroutine once its answer is out: spaced from
// [owedUpdateBase], doubling to [owedUpdateCeiling], until it lands, the ledger
// answers anything else, or the bound ([Begun.Until]) passes. WHY: the update is
// everything the call did, and an outage outlasting the ledger's attempts is
// one the store recovers from; given up, a submission would read as never made
// and every collection would wait out the bound. PAST THE BOUND the entry
// stands, its outcome unknown, said at warning
// (`mcp_bridge_call_update_unrecorded`). The registry holds the goroutine
// ([bridgedCalls]), so a drain waits for it ([Bridge.AwaitCalls]).
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
			// TAKEN, or no longer the log's to take, which the ledger says.
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

// asking is a [Surface] that says whether a call may wait for a person's
// answer before it runs (ADR-0077); a seat's *tools.Surface is one.
type asking interface {
	AsksFirst(name string) bool
}

var _ asking = (*tools.Surface)(nil)

// asksFirst reports whether a call to name on s may be held for a person, so
// its entry carries no digest ([Begun.Digest]): true for a surface that says
// nothing, the safe direction for the seal and the count.
func asksFirst(s Surface, name string) bool {
	if a, ok := s.(asking); ok {
		return a.AsksFirst(name)
	}
	return true
}

// callIDOf is the id a call's harness gave it, read off `_meta` under the one
// key the session declares ([Session.CallIDMeta]), as the engine host reads it
// (internal/agent/toolloop's callIDFromMeta), and kept only where the log keeps
// it whole (sandbox.ValidCallID): a non-string, over-long or odd-character id
// is none, never cut. A session declaring no key reads none, even under the
// empty key, which a box can send.
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

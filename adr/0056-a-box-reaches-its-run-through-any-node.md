# ADR-0056: A box reaches its run's tools through any node, and the seat's holder answers

- **Status:** accepted
- **Authority:** `internal/api/mcpbridge`
- **Enforced-by:** `internal/api/mcpbridge.TestTheRelayedBridgeKeepsTheToolSessionContract`, `internal/api/mcpbridge.TestARunWhoseSeatMovedIsServedByItsNewHolder`, `internal/api/mcpbridge.TestAFencedOpeningDefersToTheSeatsHolder`, `internal/api/mcpbridge.TestACallersOwnSessionIsServedByItsOpenerThroughAnyNode`, `internal/api/mcpbridge.TestACallersOwnSessionEndsWithItsOpener`, `internal/api/mcpbridge.TestALoopbackSessionIsNeverRelayed`, `internal/httprelay.TestAStreamReachesTheClientAsItIsWritten`, `internal/engine.TestTheBridgeFollowsTheSeat`, `internal/api.TestTheProbeSurfaceServesItsOwnBoxesOnThisHostAlone`, `cmd/crewlet.TestADirectBoxDialsTheListenerItsNodeBound`, `cmd/crewlet.TestASeatsNodeServesItsOwnBoxesTheirToolsOnLoopback`, `internal/e2e.TestAStatelessNodesBoxReachesItsToolsOnItsOwnListener`, `internal/api/mcpbridge.TestABridgedCallsAnswerStartsBeforeItsToolReturns`, `internal/api/mcpbridge.TestACallTheBridgeAnswersWithoutRunningGetsNoProgress`, `internal/api/mcpbridge.TestABoxWaitsAsLongAsTheBridgeCanTakeToAnswer`, `internal/toolsession.TestARunningCallsFirstProgressReachesAnHTTPClientWhileItsToolIsHeld`, `internal/toolsession.TestARunningCallsProgressKeepsComingUntilItsToolReturns`, `internal/toolsession.TestACallAnsweredWithoutRunningGetsNoProgress`, `internal/toolsession.TestARequestWithNoProgressTokenGetsOnlyItsAnswer`, `internal/toolsession.TestAServerThatKeepsNothingGoingSendsNoProgress`, `internal/sandbox/executor.TestABridgedEntryTellsEitherHarnessHowLongToWait`, `internal/engine.TestEveryServerABoxIsHandedWaitsAsLongAsItsBridgeCanTakeToAnswer`, `internal/mcp.TestAToolIsBoundByItsServersRequestTimeout`, `internal/agent/subagent.TestTheDelegateToolIsBoundByItsCallCap`, `internal/engine.TestADeepRunReachesCrewletOnlyThroughItsSurface`, `internal/engine.TestARelaunchsBoxWaitsAsLongAsItsBridgeCanTakeToAnswerWhicheverSessionItStarts`, `internal/toolsession.TestARunningCallsAnswerWaitsForAProgressAlreadyOnItsWay`, `internal/toolsession.TestACancelledCallIsSentNoMoreProgress`, `internal/api/mcpbridge.TestEveryEndingOfAnOpeningReturnsWhileACallIsHeld`, `internal/api/mcpbridge.TestAnOpeningsEndAnswersItsRunningCallsBeforeItClosesTheirSessions`, `internal/api/mcpbridge.TestAnOpeningsEndIsHeldNoLongerThanItsBoundByABoxThatDoesNotRead`, `internal/api/mcpbridge.TestACallWhoseToolReturnedBeforeItsOpeningEndedIsAnsweredWithItsOwnResult`, `internal/api/mcpbridge.TestACallReachingAnOpeningAfterItsEndIsToldWhatTheEndMeans`, `internal/api/mcpbridge.TestABridgeAwaitsEveryCallItRunsUntilItIsRecorded`, `internal/toolsession.TestACallsRequestIsOverOnlyOnceItsAnswerIsWritten`, `internal/engine.TestASeatHandedOnWhileACallOfItsRunIsHeldIsReleasedAtOnce`, `internal/engine.TestADrainGivesItsSeatsBackAndThenWaitsForTheirBridgedCalls`, `internal/api/mcpbridge.TestAnOpeningHoldsACallUntilItsAnswerIsOutWhateverItAdmitsAfter`, `internal/api/mcpbridge.TestAnOpeningsEndWaitsForTheAnswerOfACallItRefusedBeforeItCloses`
- **Cost-when-tried:** the box dialled its launching node's own address. A load balancer in front of several nodes sent calls to peers that never held the session, and they answered 401 to a live run. A run whose seat moved to a peer kept dialling the node that lost it, which answered that the run's tools were closed until the run ended. And a node without the ingress role had to bind a public listener for its seats' bridge alone, so a satellite that accepts no inbound connection could run no seat whose executor ran in a box.
- **Tag-status:** unreleased

## The decision

**`CREWLET_MCP_BRIDGE_URL` is the fleet's address, the same on every node.**
It is any node that serves the API, and that includes a load balancer in
front of several. The bridge's route is served where the API is, by the
`ingress` nodes. A node without that role serves its probes and nothing else.

**Only the node holding the run's seat answers a call.** A session is a live
tool surface in that process: the seat's MCP children, its skill guard, its
recording. Signing shares authentication across a fleet; it cannot share the
surface. So the run's token names the seat. The node a call lands on reads the
seat's lease, finds the holder's incarnation, and relays the whole HTTP
exchange to it over the broker (`internal/httprelay`), event stream included.
The holder answers from the same MCP handler that serves a box's own request.

**The holder decides, from what it alone knows.** It answers that a job it
holds no session for is over only once it has finished taking the seat, after
the seat's running jobs are recovered. Until then it is not yet the holder,
and the call waits for it, up to two seat-lease lifetimes, then answers 503.
An opening whose fence has closed defers to the lease rather than refusing. A
node that hands a seat on ends that seat's sessions as it releases it, waiting
for no call still running on them (the last amendment).

**A session with a caller of its own is pinned to the process that opened
it.** The Dashboard Assistant's deep work
([ADR-0054](0054-deep-work-runs-the-assistants-cli-with-its-own-tools-only-in-a-box.md))
opens a bridge session that belongs to no seat. Nothing takes it over: the
turn that opened it watches the run to its end, and a node that dies ends that
turn. So its token names the opening incarnation in place of a seat. A call
that lands elsewhere is relayed straight to that incarnation, and once the node
runs a later incarnation the call is refused at once rather than waiting for a
holder. A session admitted from its host's loopback alone is never relayed,
because a relayed call's client address is the entry's, not the box's.

**The relay rides the broker, never a node's address.** Each exchange has a
subject of its own. The holder pushes the response as frames: the status and
header once, then the body as it is written. Each frame is acknowledged once
the entry has written it to its client, one in flight at a time. Heartbeats run
both ways, and either side gives the exchange up after three missed beats. Every
message is signed under the fleet keyring
([ADR-0031](0031-every-state-log-record-is-signed.md)).

## Why the obvious alternatives are wrong

**A reverse proxy to the holder's HTTP address** needs an address for every
node that every other node can dial. No node records one. A node without the
ingress role serves none, and a satellite accepts no inbound connection. What
reaches every node is the broker, which the estate router
([ADR-0025](0025-a-node-without-data-reaches-the-estate-through-one-that-holds-it.md))
and the steer channel already ride.

**Terminating MCP on the node the call lands on**, and forwarding only each tool
call, would put the MCP session on whichever node took the first request. A
load balancer sends the next request elsewhere, and that node knows nothing
of the session. Running MCP without sessions instead loses the server-to-client
stream that tells a box's CLI its tool list changed, and the bridge's
activations depend on that notice.

**Long-polling the holder over the node's one request subject** holds one of
that registration's answer slots (`queue.ConcurrentAnswers`) for the whole
life of each event stream. An event stream lives for as long as its run, so the
sixty-fifth stream on a node would wait behind the others for hours.

**Keeping the launcher's address and telling a moved run to end** was the
previous shape. It made a seat's move cost the run its tools for the rest of
its life, through nothing the run did.

## What this does not decide

Which nodes may hold a seat, and when a seat moves: that is seat ownership's.
What a call may do once it reaches the holder: that is the dispatcher's
(ADR-0049), the same on every route a call takes. A box's MCP session lived on
the holder that served it. After a move, the box's next request is answered
404 by the new holder, and the MCP protocol has the client open a new session
through the same address.

## Amendment: a node serves its own host's boxes on loopback

The fleet's address is for a box off its launching node's host. A `direct`
box is a process of that host, so every node that runs seats and binds
`api.port` hands it that listener on loopback, and builds its bridge from that
address when `CREWLET_MCP_BRIDGE_URL` is unset. A node with the `ingress` role
serves the route there as it always did. A node without the role serves the
same route on `api.port` beside its probes, to a caller on this host's
loopback alone, read from the connection's address. Every other caller meets
the route's absence, so the network still reaches that node's probes and
nothing else. It derives the address only where loopback reaches the listener:
`api.host` on every interface or on loopback. The telemetry receiver follows
the same rule where the node forwards to an upstream
(`OTEL_EXPORTER_OTLP_ENDPOINT`).

The handler is the bridge's own, so a call for a seat the node has since
handed on is relayed to the seat's holder exactly as a call that landed on an
`ingress` node is. Nothing about where a session lives or who answers it
changes.

**Cost-when-tried:** a node without the role derived no address and mounted no
route, so its direct boxes needed `CREWLET_MCP_BRIDGE_URL`. That is the one
address a seat pool may not have: a satellite on a private host, or a
Kubernetes seat pool with no route in. Without it, every executor launch on the
node was refused, with only a warning at apply.

## Amendment: a call's answer starts at once and keeps going, and a box waits as long as the bridge can take to answer it

The bridge serves a box's harness over streamable HTTP, and the SDK writes
nothing on a call's response until the response's first event: with nothing
else sent, the answer. Claude Code 2.1.310 gives an HTTP response's first
byte a minute where the server's entry sets no timeout, then tells its model
the call timed out and cancels nothing, while the call runs on and its effect
happens. Measured against this bridge, a 90-second call was given up at 60
seconds and run to its end, its answer written on no response.

**Every call the bridge runs starts its answer at once and keeps it going.**
The tool session writes a `notifications/progress` on the call's own progress
token the moment the dispatcher starts running the call, past every answer it
gives without running it, and another every 20 seconds until the dispatcher
returns. It stops, and is waited for, before the answer is handed back, so
nothing races the answer or follows it. A call answered without running, and a
request with no progress token, get their answer alone: both harnesses that
dial the bridge put a token on every call. A progress that cannot be written
means the harness hung up on the call's response, and the call is recorded as
one nobody waited for
([ADR-0073](0073-a-seats-executor-session-is-a-sealed-object-its-turns-row-names.md)'s
amendment on such a call). Twenty seconds is held to the checks Claude Code
2.1.310 keeps on a call, none of which cancels the call it gives up: its
budget for a response's first byte and its idle check, a minute and five
minutes where the server's entry sets no timeout and the entry's timeout where
it does (below), which the first progress and every one after it meet; and its
transport-loss check, made every 30 seconds, which gives up a call 90 seconds
after its transport was marked lost unless a progress has cleared the mark
since. That mark is routine: Claude Code loses the session's standing GET
stream 360 seconds after opening it, with the entry's timeout and without
(measured), and marks every call then in flight (by reading its bundle). Four
beats fall inside the ninety seconds, any one of which clears it. Measured on
2.1.310 against this bridge, with the bridge's bound in the server's entry: a
540-second call that ran through that loss, at 359.2 seconds, was answered
whole, and the same call with no progress token was given up at 449.9 seconds
(`transport error 90s ago, response presumed lost`), cancelling nothing. The
relay's sessions keep nothing going: no harness on stdio times a response's
first byte, and the tool loop answers a call still running before the
harness's own tool timeout.

**A box waits for a call as long as the bridge can take to answer it.** Kept
going, a call answers every silence check, so only a harness's own tool
timeout bounds the wait: Claude Code's default of 1e8 ms, OpenCode's of twelve
hours (by reading their bundles), which a call that hangs would hold a seat's
executor run for. So the engine hands every bridged server the bridge's own
bound for a call, which each harness's settings carry as the server's timeout,
Claude Code's `timeout` and OpenCode's `timeout.execution`; past it the
harness gives the call up and cancels it, whatever progress flows (measured on
Claude Code 2.1.310 and OpenCode 2.1.4). The bound is the two seat-lease
lifetimes a call may wait for a holder, plus the longest bound the surface's
tools declare (a seat MCP server's request timeout, `delegate`'s call cap),
never under the engine's one-call ceiling, plus a minute for the bridge's own
work around a call. The bridge cuts nothing at it: a call past every bound the
engine sets is a defect or a hang, and a tool half-way through a write is the
run's. A call still running as its opening ends is answered at once that it is
still running, before the box's sessions on the opening are closed (the next
amendment), so no harness waits out this bound for an answer an ending never
writes.

**Cost-when-tried:** with nothing written on a call's response until its
answer, a box's Claude Code gave up every call that ran past a minute and told
its model the call had failed, while the bridge ran it to its end and its
effect happened, so the model could make it again.

## Amendment: an opening ends without waiting for the calls still running on it

Every way an opening ends closes the MCP sessions its box holds on it: the
run's close (its settle, a stop, a reclaim, its cap's cut, a launch that failed
once its job's session had opened), a relaunch's opening of the run's next job,
a recovery's opening of the same job, and the seat leaving the node. The SDK's
close of a session (go-sdk v1.8.0) waits for every request in flight on it, and
once it has begun it writes no answer. So an ending waited out every call still
running on the opening, and that call's answer was never written. A seat handed
on with such a call kept its lease until the call returned, since a node gives
a seat's lease back only once its release of the seat has returned, so no peer
could take the seat over meanwhile, and the box's next calls waited for a
holder, up to two seat-lease lifetimes, and past that were answered 503. And
with the previous amendment's progress, which the SDK still writes on a session
that is closing while a call is in flight on it, the call's harness waited for
that answer until the box's own wait for a call ran out: 1050 seconds at the
defaults on a seat that can `delegate`, 450 on one that cannot.

**An ending answers every call still running, then closes.** Under the
opening's lock it refuses every later call and answers each call it admitted
whose tool has not returned, in the words of how it ended. Where the run is over
for the box (a close, a relaunch's opening) the answer is "This run has ended;
this call is still running, so do not make it again. End your turn now." Where
the box goes on (the seat leaving, a recovery) it is "This call is still
running; do not make it again." Neither says where the call's result goes,
which would not hold for every call so answered (below). Neither is marked
failed, as the tool loop's own answer to a call still running near its
harness's tool timeout is not: a harness hands its model a failed result as a
call that failed, and a model makes a failed call again, while this call is
running and its effect is still to come. The call's handler stops its beat and
waits for it, as it does before any answer, so no progress follows the answer.

**The ending waits for those answers, five seconds at most, then closes.** The
tool session tells the dispatcher when a call's request is over on its
transport (`toolsession.Call.Over`), which the SDK reaches only once the
answer's write has returned. The ending waits for that on every call it
answered; on a call whose tool returned just before it, which its own result
answers once its record is made; and on every call the opening answered
without running it, a call that reached it after its end among them, which is
refused and told what the end means. Only then does it close the box's
sessions, which by then hold no request a tool is behind. A call that reaches
the opening as that close begins is refused all the same, and its box reads the
close's failure in place of the refusal. Five seconds covers that record's
retries (1.75 seconds of waits between four attempts) and an activation's hold
for the box's re-list (two seconds) on a coordination store that answers. Past
the bound the ending waits no longer, since an answer still on its way is one a
box is not reading: it returns, and the bridge closes the sessions once those
answers are out, at the latest when the box's connection ends.

**The call runs on, and the bridge holds it.** The call's tool runs on a
goroutine of the bridge's, under the run's context, until the call's own bound
ends it, and the call's record is appended once its tool returns, in one step,
as one nobody waited for, after the session's ledger has ended
([ADR-0073](0073-a-seats-executor-session-is-a-sealed-object-its-turns-row-names.md)'s
amendment on such a call). It lands only where the run's log still takes it: the
paths that end a run go on, waiting for no call, to read the run's log and
delete its record, so a call still running as a stop, the turn's cap, a reclaim
or the run's settle ends its job is in no collection of the run where it returns
after that read, and on no record of it where it returns after the delete. A
relaunch's job takes it as a late call of the round, and a run whose box goes on
takes it as its own while its record stands. A call that reaches an opening
after its end is refused and runs nothing, as before, now in the words of how
the opening ended: told to end its turn where the run ended, and, where the box
goes on, that the session it reached was closing and to call again, which
reaches the opening that serves the box now.

**A stopping node waits for those calls once its seats are given back.** Its
drain releases every seat, which ends each seat's openings and answers their
calls still running, so each seat's lease is given back at once; then it waits
for each such call to be recorded before the node tears down the store and the
broker the record goes through, as the release used to wait for it while
holding the lease. That wait has no bound of the engine's own, as the drain's
wait for running turns has none: on a signal, the supervisor's grace bounds it,
and a call still running when the process is killed is on no record.

**Cost-when-tried:** measured with the SDK's own client as the box, against the
bridge on `main` at 28ce95e56c, each opening ended with a call held in its tool:
a close of the run and a relaunch's opening returned only when the call's tool
did, released at 247.9 seconds, and the box read `calling "tools/call": request
terminated without response`; the seat leaving the node and a recovery's
opening had not returned when the measurement stopped waiting, at 15.6 and 31.1
seconds; and each time the ledger took the call's record and then the session's
end, after the release.

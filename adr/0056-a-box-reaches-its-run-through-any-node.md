# ADR-0056: A box reaches its run's tools through any node, and the seat's holder answers

- **Status:** accepted
- **Authority:** `internal/api/mcpbridge`
- **Enforced-by:** `internal/api/mcpbridge.TestTheRelayedBridgeKeepsTheToolSessionContract`, `internal/api/mcpbridge.TestARunWhoseSeatMovedIsServedByItsNewHolder`, `internal/api/mcpbridge.TestAFencedOpeningDefersToTheSeatsHolder`, `internal/api/mcpbridge.TestACallersOwnSessionIsServedByItsOpenerThroughAnyNode`, `internal/api/mcpbridge.TestACallersOwnSessionEndsWithItsOpener`, `internal/api/mcpbridge.TestALoopbackSessionIsNeverRelayed`, `internal/httprelay.TestAStreamReachesTheClientAsItIsWritten`, `internal/engine.TestTheBridgeFollowsTheSeat`, `internal/api.TestTheProbeSurfaceServesItsOwnBoxesOnThisHostAlone`, `cmd/crewlet.TestADirectBoxDialsTheListenerItsNodeBound`, `cmd/crewlet.TestASeatsNodeServesItsOwnBoxesTheirToolsOnLoopback`, `internal/e2e.TestAStatelessNodesBoxReachesItsToolsOnItsOwnListener`
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
node that hands a seat on ends that seat's sessions as it releases it.

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
that registration's answer slots (`queue.MaxConcurrentAnswers`) for the whole
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

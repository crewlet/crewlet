# ADR-0025: A node without data reaches the estate through a node that holds it

- **Status:** accepted
- **Authority:** `internal/estate`
- **Enforced-by:** `internal/e2e.TestASeatOnAStatelessNodeWritesThroughADataNode`, `internal/estate.TestADataNodeRunsNothingOfARequestItsBuildCannotReadWhole`, `internal/estate.TestARequestOneBuildCannotReadIsServedByAnother`, `internal/estate.TestARequestNoDataNodeCanReadWholeSaysWhatWouldServeIt`, `internal/estate.TestARefusalNeverHidesAWriteThatMayHaveLanded`, `internal/estate.TestEveryFieldAnOperationTakesLeavesTheWireAtItsZero`, `internal/estate.TestEveryZeroAMethodDecidesDropsNothingAReaderMisses`, `internal/estate.TestNoTypeAnOperationTakesDecodesAKeyAwayByItself`, `internal/estate.TestNoPointerAtAZeroCrossesWhereAReaderWouldMissIt`, `internal/estate.TestADateClearedThroughTheRouterReachesTheDataNodeAsAClear`, `internal/estate.TestAWriteHoldingTheZeroInstantIsRefusedBeforeItIsAskedFromEveryNode`, `internal/estate.TestAHomeThatCannotReadARequestWholePassesTheFill`, `internal/estate.TestARefusalMovesOnlyTheRequestsCarryingWhatItRefused`, `internal/estate.TestAnAnswerACallerWritesBackIsReadWholeOrRefused`, `internal/estate.TestACarriedReadIsReadWholeAndTheShownOneIsNot`, `internal/estate.TestAViewSavedAgainIsListedWholeAndTheStripShownIsNot`, `internal/estate.TestEveryAnswerReadWholeIsOneACallerCarriesIntoAnotherRequest`, `internal/engine.TestTheLogsAnswerIsReadAsWhatTheSandboxDoesNext`
- **Tag-status:** unreleased

## The decision

`data` is a node role, and it is the one role that is a promise about the DISK
rather than about work. A node that holds it keeps a full copy of the
replicated estate, is a member of the fleet's broker, and applies every state
log. A node WITHOUT it keeps nothing that has to outlive it: its store is
scratch (deleted, under the store's own lock, at every boot and opened with no
replicated estate at all), its embedded broker is a leaf of the members' with
JetStream off, and it runs no applier, no index, no snapshotter and no feed.
Its seats still use the same tools, and every one of those tools is handed a
seam answered by a data node over the broker — the ESTATE SERVICE — so the tool
layer cannot tell which kind of node it is on. The identity directory is part
of the estate like any other domain: who holds a login and who holds each human
seat, which a seat's tools and its notification routing ask, are read through
the same service, floored on the identity log.

Four rules make that sound, and each is stated once, at `internal/estate`:
the client picks the node and the failover is per operation (a read and a
tracker write carrying an operation id move on, a page or chat write moves on
only when its caller named the operation, a chat erase or prune moves on under
its own operation id, and an unnamed one that went unanswered is reported unknown and never
repeated); every request carries the
asking node's SESSION FLOOR, so whichever data node answers has applied the
node's own writes; what a stateless node publishes is persisted on exactly one
data node's event log (custody: published durably, taken by a fleet-wide group,
and kept by the data node whose create-only claim wins) rather than in a store
deleted at its next boot;
and every question about "which nodes hold data" — the trim's counted set, the
eviction gate, the search roster, the capacity handshake — reads the role off
the presence lease, so a stateless node is never counted as a copy. And
because a data node runs a write request AS the actor it names, every request
and every reply on that channel is signed under the fleet keyring
([ADR-0031](0031-every-state-log-record-is-signed.md)), exactly as a record on
a log is.

What a node without `data` may run is `seats` alone: ingress and workers read
and write this node's own estate directly, and Tier A refuses them without it.

## Why the obvious alternative is wrong

**A scratch copy of the estate** — hydrate the replicated estate from a
snapshot at every boot and run the appliers as a data node does — is what a
"disposable" node would get for free from the existing machinery, and it is
the opposite of what the role is for. It costs disk equal to the whole estate
and a hydration measured in minutes on every restart, which is exactly the
size and the start-up an operator taking `data` away from a node is trying not
to pay.

**A queue group in front of the data nodes** — let the broker hand each
request to whichever member it likes — makes the broker choose, and then the
asker cannot say which node an unanswered write went to. A retry on "whoever
answers next" is safe only where the write itself says so, and the page writes
say no unless their caller named the operation: the store otherwise mints the
operation id per call, so a repeat is a second comment or a create refused by
its own first copy. A per-node subject keeps the
choice, and therefore the retry rule, with the asker.

**A non-voting replica on the stateless node** — a JetStream member that never
leads — would keep a copy of every stream on the node that was meant to hold
none, and NATS offers no supported way to pin a member out of leadership in
any case.

## What this does not decide

Where the bulk of the company's data lives. Every data node still holds the
whole replicated estate; this record decides how a node that holds none reaches
it, not how the estate is divided between the nodes that hold it.

## Amendment — every node routes

The client a node without `data` ran is now a ROUTER that every node runs,
and the decision above holds through it unchanged: per-node subjects, the
asker's choice of node, the failover rule stated per operation and the session
floor on every request. (A serving node answers a push's proposal on a subject
of its own beside the one every other operation is asked on, because a
proposal waits for its batch and must not hold a slot the node's reads are
answered through — still the node the asker chose, so the retry rule stays
with the asker.) Every data node holds the whole estate, so
the router asks for nothing but the operation; what the amendment adds is four
consequences of routing on every node rather than on the nodes that hold none:

- A data node reaches its OWN copy through the same router, in-process, held to
  the same floors as a remote data node — its seats may have written through
  another data node while its copy was out of service — and asks a copy that
  LAGS its logs only once every data node whose copy does not has run nothing,
  exactly as a remote asker does: a worse node to ask, never no node.
- A tracker write one data node answered UNVOUCHED is asked of the next under
  the same operation id before the caller is told the outcome is unknown: that
  node's ledger cannot say whether it landed, and another's may.
- A data node whose copy is WRONG rather than behind takes it OUT OF SERVICE
  and keeps its seats: a peer asking it is told `out_of_service`, having run
  nothing, and its own seats reach the estate through another data node — which
  is this record's decision, applied to the node itself. An estate no data node
  serves refuses the operation saying so, never an empty answer.
- The OPERATOR'S surfaces go through the same router as the seats' tools: the
  API's tracker, knowledge-base and chat routes, a project's file rows and the
  operator's own MCP. Read straight off the node's own copy, a data node whose
  copy was out of service answered its seats from a peer and its operator from
  the copy it had stopped serving. What still keeps `ingress` on a data node is
  what acts on a node's own state log rather than reads the estate — the
  retention report, capacity, reanchor, eviction and backup.

What a fleet sees of this is routing through the watched view of the presence
leases, a data node's reads waiting on its floors, and the request gate asking
whether a copy lags rather than whether it is level this instant — and choosing
a lagging copy last rather than never.
`internal/estate` states the rules; the authority and the gate are unchanged.

A copy goes out of service one DOMAIN at a time — ADR-0079.

## Amendment — a repository's git work is asked of its home git node

The router has a second placement beside the data nodes:
[ADR-0089](0089-a-repositorys-git-work-is-asked-of-its-home-git-node.md). An
operation on a repository's git OBJECTS — a read of its content by object id —
is asked of the nodes with the `gitlet` role rather than of the data nodes, in the
repository's rendezvous order over them, its home first, and answered from the
Gitlet node's own cache. It reads no domain's copy, so it takes no copy verdict,
no dispatch floor and no lag gate: its floor travels in its arguments, as the
cache's sync level. The decision above holds through it unchanged — a
per-node subject (the git reads on a lane of their own beside the shared one
and the proposals'), the asker's choice of node, the failover rule stated for
the operation, and every request and reply signed.

## Amendment — a data node runs what it is asked whole, or none of it

The failover rule above assumed every data node reads a request alike. A rolling
upgrade breaks that: a node of a newer build asks a data node of an older one,
whose types declare less than the request holds. Decoded with a plain
`json.Unmarshal`, a key the older build did not declare vanished and the write
ran as something its writer never asked for — recorded by the older rule, under
the lowest version that reads what was left, on every node, the newer ones
included, with nothing said. The rest of the upgrade path keeps what a build
cannot read rather than drop it (a state-log record is deferred, a parked run's
unknown keys ride its row's `Extra`), and so does the service now. An
operation's INPUTS — its arguments and the actor it runs as — cross under one
rule whose two halves are the standard library's own (`internal/estate`'s
`wirerule.go`):

- **The router leaves every input field at its zero off the request**, at every
  depth (`omitzero` on every field), so a field a later build adds reaches a
  data node only in a request that sets it. A field's zero must mean what a
  build without the field does: a request that does not use it is then served
  by a data node of either build exactly as before, and only one that would
  lose something is refused. A type's own `IsZero` decides that zero, and the
  encoder asks it THROUGH a pointer, so a pointer at a value its type calls
  zero crosses as nil: no field's meaning may rest on one. A field whose
  readers would tell the two apart is reshaped — a cleared date is a flag of
  its own, a query's bound a value whose zero means unset — or the value no
  write may hold is refused where the request is asked, in process as routed
  (`op.refusedBeforeAsking`); the wire gate lists every such pointer with why
  (`zeroThroughPointer`).
- **A data node decodes the inputs refusing every member its build does not
  declare** (the rule `DisallowUnknownFields` sets), at every depth — a map's
  keys and a raw JSON document are data — before it waits for a floor or runs
  anything, and answers the refusal as its own typed error,
  `estate.ErrUnknownKeys`, naming the operation, each key and itself. A read
  carrying a newer argument is refused the same way, never answered with the
  argument ignored.
- **The router reads that answer as one from a node that ran nothing**, whatever
  the operation's class, and asks the next data node: the failover rule above,
  applied to a node whose build cannot serve the request. A keyed operation
  asks the refusing node last for requests carrying what it refused, and for
  those alone — it serves every other — and a repository's home that refused
  passes the copy it would fill to its runner-up. Where no node serves it, the
  caller is handed the refusal over the walk's account — an estate nobody
  served, which tells an operator what would serve it: a data node of the
  asking node's build. A write some node may have run is still an unknown
  outcome. A caller that keeps a write owed keeps it owed on the refusal: the
  engine's record of how a turn ended stays on the run's record until a data
  node of its writer's build records it.

**The other direction**: a newer data node's ANSWER holds a key an older
caller's build does not declare. Most answers are only shown, and dropping a
field a reader cannot show loses nothing; an answer a caller CARRIES INTO
ANOTHER REQUEST through the router would lose it the same way — a version's
files a publish carries into the next version, a view's prior a save hands
back, a container's views a tool hands a model to save one of again, the
catalogue, a project's field declarations or a datastore's schema a tool hands
a model to send back whole, a list's query its read asks, a repository
snapshot's page cursor the next page asks by, the queue work whose entries a
repository's home lands, ejects and settles as it read them. Such a read is
declared (`op.answeredWhole`) and its answer crosses under the same rule: the
serving node leaves its fields at their zero off it, and the asking router
reads it whole or refuses it. Where the same answer is also only shown, the
carried read is an operation of its own (`artifacts.version_carried` beside
`artifacts.version`), so a reader that only shows it is not blinded to it.
The write such a read feeds stays a whole replacement, never a merge with what
it replaces: a field it leaves at its zero crosses exactly as one an older
build never sent, so a merge could not tell a field cleared from one unknown,
and it is the read that keeps an older caller from writing back what it never
saw. Neither a write that replaces a whole list nor a view's save is decided
against the version its caller read (a save's prior is the view's owner and
container, not its fields), which is the lost update any two writers meet, not
this amendment's (issue #893).

**What stays outside the rule** is the rest of the envelope — the floors, the
deadline, what a node asked before already paid for — which is the routers' own
protocol: every build reads it as it always has, and a later build adds there
only what an earlier one may ignore. And a data node answering its own seats
from its own copy, in process, reads nothing off the wire. Nor is a value a
caller hands back as it took it: a `gitlet.CheckKey` a check runner reports
under, a `statelog.Position` a later read floors at, a credential's actor. A
write's answer, which carries a position, is never refused once the write has
run, and a member a later build adds to one of these changes what it names,
which no reading of an answer makes safe: such a member comes with its own
plan for a fleet of two builds.

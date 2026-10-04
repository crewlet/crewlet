// Package estate routes every node's reads and writes of the replicated estate
// to a data node whose copy of it serves.
//
// The decision it carries is ADR-0025: a node without the `data` role reaches
// the estate through a node that holds it, and every rule below is one clause
// of what makes that sound. Its amendment: the client that node ran became a
// ROUTER that every node runs, and a data node's own seats reach its own copy
// through the same path, with the same floors.
//
// A node WITHOUT the `data` role keeps nothing that has to outlive it: its
// store is scratch, its broker is a leaf with JetStream off, and it holds no
// copy of the tracker's or the knowledge base's rows. Its seats still read and
// write both, constantly — every board, every task, every page a tool touches
// — so something has to answer them. EVERY DATA NODE HOLDS THE WHOLE ESTATE
// and SERVES it over the broker ([Serve]), and every node's tools are handed
// facades of one [Router], which answers in-process where this node's copy
// serves and asks another data node otherwise. So are the operator's surfaces
// — the API's tracker and knowledge-base routes and the operator's own MCP —
// for the reason a data node's own seats are: read straight off the node's
// copy, a node whose copy was out of service answered its operator from the
// very copy it had stopped serving its seats from.
//
// # One subject per serving node, never a scatter
//
// The request goes to exactly one node — `crewlet.estate.<node>` — over the
// queue's ephemeral request/reply verbs. A scatter would run a write on every
// data node, and a queue group would let the BROKER pick which node answers,
// which leaves the router unable to say which node it asked when the answer
// does not come: a failover that retries a write on "whoever answers next" is
// only safe where the write itself says so. So the router picks the node —
// this node first where its copy serves, then the node that answered last,
// then a rendezvous order, with a node that went silent last — and the
// failover rule is stated per operation (see [opClass]).
//
// # What makes a retry safe, per operation
//
// A READ is safe to ask anywhere. A TRACKER write carries an operation id the
// tool minted once per caller-visible operation, and the tracker's ledger
// answers a repeat with what the first copy wrote — so a write that went
// unanswered is asked again of the next data node, which is exactly the
// lost-acknowledgement case that id exists for; and so is one a node answered
// UNVOUCHED, whose ledger cannot say whether it landed while another node's
// may. A PAGE write has no id a caller holds: the store mints one per call, so
// a repeat is a second write (a second comment) or a refusal of its own first
// copy (a create whose title is now taken). An unanswered page write is
// therefore NEVER repeated, and is reported as [ErrOutcomeUnknown] — which is
// the honest answer, and the one a tool turns into "read the page before
// writing again".
//
// A node that answers "I did not run it" — its copy is out of service, it runs
// no native backend, its copy lags its logs, or it could not reach the
// caller's floor in time — did not execute anything, so every class moves on
// from it; and so does a write the write authority refused at gate 3
// (`not_holder`, `holding_unknown`), which appended nothing. When no data node
// runs the operation, it is refused as [ErrUnserved], naming who was asked and
// what each said — never an empty answer, which would say the company has none
// of what was asked for.
//
// # A copy that is WRONG is out of service; one that LAGS is a worse node
//
// A copy that is wrong — an applier halted, the node evicted, its rows below
// the log — answers nothing ([LocalBackends.For] false): a peer asking is told
// `out_of_service`, and the node's own seats are answered by the other data
// nodes. The seats were never the problem, the copy was, so the node keeps
// them.
//
// Whether a copy answers requests ([Backend.Answers]) is a different question:
// its distance from its logs, not its correctness. The floors and the read's
// own level hold an answer to what the caller must see, and a write is decided
// from the authority's own snapshot and arbitrated by the broker. So a copy
// that lags is passed over only for one that does not, and asked again — told
// to take the request anyway ([request.AcceptLagging]) — once every data node
// whose copy does not lag has run nothing. A single data node a burst put past
// the snapshot slack would otherwise refuse its own seats until it caught up,
// and a fleet the same burst put behind together would refuse everybody's.
//
// # Which data nodes to ask is answered from memory
//
// Every request asks the [Placement] who holds the estate, so the placement
// must answer from what the node already knows: listing the fleet's presence
// leases per request was an O(fleet) read of the coordination store, across the
// leaf link, on every tool call. The engine's placement is every live data
// node, from a WATCHED view of the presence leases that lists once per
// heartbeat and answers UNKNOWN rather than an empty or stale fleet once its
// last listing is older than a lease survives; a node it named that went silent
// is reported ([Placement.Unanswered]) so it drops out at once. An unknown
// placement fails the request with the placement's own reason: "give a node the
// data role" is the wrong remedy for a fleet whose coordination blinked.
//
// # Read-your-writes: ONE floor table per node, consulted for local reads too
//
// A write goes to the fleet's log, and the next read may be answered by a
// different data node than the one that took the write — or by this node,
// which may not have applied it yet. So each node keeps ONE [Session]: a
// HIGH-WATER per stream of the furthest position any write it made (or any
// wait it was asked for) reached. Every request carries the floor on the log
// of the operation's own domain ([opSpec.floorStream]), and whichever data
// node answers — this node's own copy included — waits for its applier to
// reach it, bounded by [statelog.ReadBudget], and says it is behind rather
// than answer from before a write this node has already been told landed. A
// data node's own copy is held to its floors like any other because its seats
// may have written through another data node while its copy was out of
// service. Floors on another DOMAIN's log are never waited for, though the
// estate carries every domain's log: no operation reads another domain's
// rows, so that applier's lag is not one the operation should wait out
// ([ready]).
//
// The searches carry no floor at all: they read an index each node's own walk
// maintains behind its applier, which no log position describes.
//
// # What does not cross the wire, and who supplies it
//
// Everything that is in-process by nature — the chart seams a query carries
// ([tracker.Units]), the lead map a dependency consults ([tracker.Leads]), the
// org a knowledge search is scoped by — is supplied by the SERVING node from
// its own current epoch ([ServerSeams]). Every node reads the same activation,
// so the two differ only for the instant an apply is landing, which is the same
// window every surface already has. The gate in wire_test.go walks every type
// an operation carries and fails on any field that would silently not arrive,
// unless it is named, with its reason, in [carriedByServer].
//
// # Errors keep their identity
//
// A tool decides what to tell a model by asking errors.Is and errors.As — "no
// such task", "stale version", a tag clash, a refusal with a reason. A string
// would answer none of them, so an error crosses as its message plus every
// registered sentinel it matches and every registered type it carries (see
// errors.go), and the far side rebuilds an error that answers the same
// questions. A sentinel or an error type added to the packages whose errors
// cross — this one's included — is a build failure until it is registered or
// exempted by name. An operation answered in-process returns its own error,
// which answers every question without the wire's help.
package estate

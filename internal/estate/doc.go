// Package estate routes every node's reads and writes of the replicated estate
// to a node that serves the partition they address.
//
// The decision it carries is ADR-0018: a node without the `data` role reaches
// the estate through a node that holds it, and every rule below is one clause
// of what makes that sound. Its amendment: the client that node ran became a
// ROUTER that every node runs, and a data node's own seats reach its own copy
// through the same path, with the same floors.
//
// A node WITHOUT the `data` role keeps nothing that has to outlive it: its
// store is scratch, its broker is a leaf with JetStream off, and it holds no
// copy of the tracker's or the knowledge base's rows. Its seats still read and
// write both, constantly — every board, every task, every page a tool touches
// — so something has to answer them. A data node SERVES the partitions it holds
// over the broker ([Serve]), and every node's tools are handed facades of one
// [Router], which answers in-process where this node serves the partition and
// asks a holder that does otherwise. So are the operator's surfaces — the
// API's tracker and knowledge-base routes and the operator's own MCP — for the
// reason a data node's own seats are: read straight off the node's copy, a
// node whose copy was out of service answered its operator from the very copy
// it had stopped serving its seats from.
//
// # Every operation addresses a partition
//
// An operation declares, beside its server half, how its arguments resolve to
// the partition holding what it reads or writes (partition.go). Under layout 0
// that is always `estate.000`, the whole estate every data node holds; under a
// layout that divides a domain it is the domain's own partition function, read
// through a [Resolver] for an operation that names an object by id. The asking
// node names the partition on the request, and a node that does not serve it
// answers `not_holder` with its own map epoch rather than running anything.
//
// # One subject per serving node, never a scatter
//
// The request goes to exactly one node — `crewlet.estate.<node>` — over the
// queue's ephemeral request/reply verbs. A scatter would run a write on every
// holder, and a queue group would let the BROKER pick which node answers,
// which leaves the router unable to say which node it asked when the answer
// does not come: a failover that retries a write on "whoever answers next" is
// only safe where the write itself says so. So the router picks the node —
// this node first where it serves the partition, then the node that answered
// last for it, then a rendezvous order, with a node that went silent last —
// and the failover rule is stated per operation (see [opClass]).
//
// # What makes a retry safe, per operation
//
// A READ is safe to ask anywhere. A TRACKER write carries an operation id the
// tool minted once per caller-visible operation, and the tracker's ledger
// answers a repeat with what the first copy wrote — so a write that went
// unanswered is asked again of the next holder, which is exactly the
// lost-acknowledgement case that id exists for; and so is one a holder answered
// UNVOUCHED, whose ledger cannot say whether it landed while another holder's
// may. A PAGE write has no id a caller holds: the store mints one per call, so
// a repeat is a second write (a second comment) or a refusal of its own first
// copy (a create whose title is now taken). An unanswered page write is
// therefore NEVER repeated, and is reported as [ErrOutcomeUnknown] — which is
// the honest answer, and the one a tool turns into "read the page before
// writing again".
//
// A node that answers "I did not run it" — it does not serve the partition,
// cannot tell whether it does, runs no native backend, its copy lags its logs,
// or it could not reach the caller's floor in time — did not execute anything,
// so every class moves on from it; and so does a write the write authority
// refused at gate 3 (`not_holder`, `holding_unknown`), which appended nothing.
// When no holder serves, the operation is refused as [ErrPartitionUnserved],
// naming the partition — never an empty answer, which would say the company
// has none of what was asked for.
//
// # A copy that lags is a worse holder, never no holder
//
// Whether a copy answers requests ([Backend.Answers]) is its distance from its
// logs, not its correctness: the floors and the read's own level hold an answer
// to what the caller must see, and a write is decided from the authority's own
// snapshot and arbitrated by the broker. So a copy that lags is passed over
// only for one that does not, and asked again — told to take the request
// anyway ([request.AcceptLagging]) — once every holder whose copy does not lag
// has run nothing. A single data node a burst put past the snapshot slack would
// otherwise refuse its own seats until it caught up, and a fleet the same burst
// put behind together would refuse everybody's.
//
// # `not_holder` carries the server's map epoch
//
// A server newer than the asker's view means the asker routed by an old map:
// it reads the map again ([Placement.Refresh]), resolves again and asks the
// holders the fresh map names — once per request. A server no newer is the one
// behind (a joiner not serving yet) or on its way out, and the asker moves on.
//
// A server that cannot TELL whether it serves the partition — the holding
// answer gate 3 reads could not be read — answers `holding_unknown` instead,
// with no epoch: nothing about the asker's map is in question, so the asker
// moves on without reading it again. The two are kept apart for the reason
// gate 3 keeps them apart, and [LocalBackends.For] is three-valued to carry the
// difference, which the contract's two-valued `For(p) (Backend, bool)` could
// not.
//
// # Which holders to ask is answered from memory
//
// Every request asks the [Placement] who serves its partition, so the placement
// must answer from what the node already knows: listing the fleet's presence
// leases per request was an O(fleet) read of the coordination store, across the
// leaf link, on every tool call. Under layout 0 the engine's placement is every
// live data node, from a WATCHED view of the presence leases that lists once per
// heartbeat and answers UNKNOWN rather than an empty or stale fleet once its
// last listing is older than a lease survives; a node it named that went silent
// is reported ([Placement.Unanswered]) so it drops out at once. An unknown
// placement fails the request with the placement's own reason: "give a node the
// data role" is the wrong remedy for a fleet whose coordination blinked.
//
// # Read-your-writes: ONE floor table per node, consulted for local reads too
//
// A write goes to the fleet's log, and the next read may be answered by a
// different holder than the one that took the write — or by this node, which
// may not have applied it yet. So each node keeps ONE [Session]: a HIGH-WATER per
// stream of the furthest position any write it made (or any wait it was asked
// for) reached. Every request for a partition carries the floors of the
// partition's log of the operation's own domain ([address]), and whichever
// holder answers — this node's own copy included — waits for its applier to
// reach them, bounded by [statelog.ReadBudget], and says it is behind rather
// than answer from before a write this node has already been told landed. A
// data node's own copy is held to its floors like any other because its seats
// may have written the partition through another holder while it was not
// serving it. Floors on another partition's logs are never carried: no holder
// of this one could reach them. Nor are floors on another DOMAIN's log in the
// partition: no operation reads another domain's rows, so that applier's lag
// is not one the operation should wait out.
//
// # A read across partitions is a GATHER, answered at a cut
//
// An operation that addresses several partitions — the searches, over every
// partition holding their corpus — is declared with [defineGather]: a server
// half answering ONE partition's slice, and a merge over every slice. The
// router resolves the partitions, asks each holder ONCE for all of its
// partitions ([request.Slices]), answers this node's own in-process, asks a
// partition its holder failed of the next holder — never again of one that
// failed it in this gather — and merges by the operation's own sort key, a
// paged list resuming each partition from its own cursor (cursor.go). What it
// covered travels with the answer ([statelog.Coverage]): the partitions that
// answered, the cut each was read at, and every one that did not, NAMED with
// why — unserved, unreachable, behind, not a holder, or an error — so an
// answer short of a partition is never a short list. Nothing answering at all
// is an error, as it is for one partition. A gather that addresses ONE
// partition is a single-partition read, asking for the whole answer exactly as
// a build before gathers does; under layout 0 every gather is one.
//
// A slice reads at [statelog.GatherLevel]: a seat's at `session`, floored at
// this node's writes and at the record whose wake started the turn, and an
// operator's at `linearizable`, whose holder appends a barrier on the
// partition's log of the operation's own domain and answers at or after it.
// A batch that outgrows one reply ([queue.MaxPayloadBytes]) is answered in
// pages: what fits, then what did not, asked again of the same holder; one
// that outgrows an attempt is answered a margin before the asker stops
// waiting, with what finished, the rest asked again the same way — and only
// ever beside a partition the reply DECIDED ([partReply.decisive]), so a
// batch asked of a holder again is always smaller: a reply in which the
// holder decided anything carries a decided partition — a slice no reply can
// carry is the error naming its size — and a reply in which it finished
// nothing moves its partitions on. And a copy
// that lags its logs is asked last, one at a time, as a single read's last
// resort is.
//
// A slice's QUERY takes one of this node's [CPUs], and only its query — its
// floor and barrier waits run at once. There is ONE per node, which
// [LocalBackends] hands to both its server and its router, so every batch the
// node answers for another and every gather it answers in-process run at most
// a query per CPU between them: a bound per batch was multiplied by however
// many batches the node was answering at once.
//
// A single-partition read whose answer a gather will one day assemble — a
// board, a person's day, an inbox — reports its coverage too
// ([op.covered]): one partition, at the cut its holder measured.
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

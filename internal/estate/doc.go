// Package estate serves the replicated estate to nodes that hold none.
//
// The decision it carries is ADR-0018: a node without the `data` role reaches
// the estate through a node that holds it, and every rule below is one clause
// of what makes that sound.
//
// A node WITHOUT the `data` role keeps nothing that has to outlive it: its
// store is scratch, its broker is a leaf with JetStream off, and it holds no
// copy of the tracker's or the knowledge base's rows. Its seats still read and
// write both, constantly — every board, every task, every page a tool touches
// — so something has to answer them, and this package is that: a data node
// SERVES its own copy over the broker, and a stateless node's tools are a
// CLIENT of whichever data node answers.
//
// # One subject per serving node, never a scatter
//
// The request goes to exactly one node — `crewlet.estate.<node>` — over the
// queue's ephemeral request/reply verbs. A scatter would run a write on every
// data node, and a queue group would let the BROKER pick which node answers,
// which leaves the client unable to say which node it asked when the answer
// does not come: a failover that retries a write on "whoever answers next" is
// only safe where the write itself says so. So the client picks the node, and
// the failover rule is stated per operation (see [opClass]).
//
// # What makes a retry safe, per operation
//
// A READ is safe to ask anywhere. A TRACKER write carries an operation id the
// tool minted once per caller-visible operation, and the tracker's ledger
// answers a repeat with what the first copy wrote — so a write that went
// unanswered is asked again of the next node, which is exactly the
// lost-acknowledgement case that id exists for. A PAGE write has no id a
// caller holds: the store mints one per call, so a repeat is a second write
// (a second comment) or a refusal of its own first copy (a create whose title
// is now taken). An unanswered page write is therefore NEVER repeated, and is
// reported as [ErrOutcomeUnknown] — which is the honest answer, and the one a
// tool turns into "read the page before writing again".
//
// A node that answers "I did not run it" — it runs no native backend, it is
// not established, or it could not reach the caller's floor in time — did not
// execute anything, so every class moves on from it.
//
// # Read-your-writes across nodes: the session floor
//
// On a data node a write goes to the fleet's log and every read to this
// node's own rows, and the tools close that gap by waiting for this node's
// applier after every write. A stateless node has no applier, and its next
// read may be answered by a DIFFERENT data node than the one that took the
// write. So the client keeps a HIGH-WATER per stream — the furthest position
// any write it made (or any wait it was asked for) reached — and every request
// carries it as a FLOOR. The serving node waits for its own applier to reach
// the floor before it runs the operation, bounded by [statelog.ReadBudget],
// and says it is behind rather than answer from before a write this node has
// already been told landed. That is the `session` guarantee, made to hold
// across nodes by carrying the session rather than pinning it to one.
//
// # What does not cross the wire, and who supplies it
//
// Everything that is in-process by nature — the chart seams a query carries
// ([tracker.Units]), the lead map a dependency consults ([tracker.Leads]), the
// org a knowledge search is scoped by — is supplied by the SERVING node from
// its own current epoch. Every node reads the same activation, so the two
// differ only for the instant an apply is landing, which is the same window
// every surface already has. The gate in wire_test.go walks every type an
// operation carries and fails on any field that would silently not arrive,
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
// cross is a build failure until it is registered or exempted by name.
package estate

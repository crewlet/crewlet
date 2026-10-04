# ADR-0025 — A node without data reaches the estate through a node that holds it

- **Status:** accepted
- **Authority:** `internal/estate`
- **Enforced-by:** `internal/e2e.TestASeatOnAStatelessNodeWritesThroughADataNode`
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
layer cannot tell which kind of node it is on.

Four rules make that sound, and each is stated once, at `internal/estate`:
the client picks the node and the failover is per operation (a read and a
tracker write carrying an operation id move on, a page write that went
unanswered is reported unknown and never repeated); every request carries the
asking node's SESSION FLOOR, so whichever data node answers has applied the
node's own writes; what a stateless node publishes is persisted on exactly one
data node's event log (custody: published durably, taken by a fleet-wide group,
and kept by the data node whose create-only claim wins) rather than in a store
deleted at its next boot;
and every question about "which nodes hold data" — the trim's counted set, the
eviction gate, the search roster, the capacity handshake — reads the role off
the presence lease, so a stateless node is never counted as a copy.

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
say no: the store mints their operation id per call, so a repeat is a second
comment or a create refused by its own first copy. A per-node subject keeps the
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
and the decision above holds through it unchanged: one subject per serving
node, the asker's choice of node, the failover rule stated per operation and
the session floor on every request. Every data node holds the whole estate, so
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
  API's tracker and knowledge-base routes, a project's file rows and the
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

# ADR-0018 — A node without data reaches the estate through a node that holds it

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
node's own writes; what a stateless node publishes is persisted on a data
node's event log (custody) rather than in a store deleted at its next boot;
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

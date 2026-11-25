# ADR-0174: A node's holds ride a connection nothing else rides

- **Status:** accepted
- **Authority:** `internal/coord/kv`
- **Enforced-by:**
  - `internal/coord/kv.TestARenewalCrossesALeafsLinkAheadOfAnUploadOnIt`
  - `internal/coord/kv.TestAHoldRidesTheHoldClientAndEveryFleetReadTheReadClient`
  - `internal/coord/kv.TestContractAcrossTheTwoClients`
  - `internal/queue/jetstream.TestALeafsTwoLinksToOneMemberStandSideBySide`
  - `internal/engine.TestTheLeaseStoreRidesTheLeaseConnection`
- **Measured:** a seat renewal is about 573 bytes up and 110 down, and a presence at its worst 8,610 bytes (estimated from their subjects and records, not counted on a wire); at the floor of 30 s a MiB a lease link renews 33 to 46 seats within a one-second beat, and no lease lapses below 887 seats at the floor TTL (about 2,700 at the shipped 45 s); in the case, at a fair 64 KiB/s link, each renewal is answered while half a mebibyte still crosses the data link, where the same renewal over the data connection waits for all of it.
- **Cost-when-tried:** one link for everything a node sends (#1035): a renewal behind one 8 MiB write waited 240 s at the floor, past every lease, and a put window sized for the shipped heartbeat held a renewal 20 s on a 15 s lease, so a node lost its seats to its own uploads. A first draft moved the whole lease store to the new link and put the fleet's listings, watches and gate view on it: 2.4 times the floor at 32 nodes.
- **Tag-status:** unreleased

## The decision

**A node's holds ride a connection of their own, and nothing else rides it.**
The lease store (`internal/coord/kv`) is opened over two clients. The **hold
client** carries what takes, keeps or gives up a hold: a claim's reads,
writes and epochs, a renewal, a release, the presence claim and the epochs
counter. The **read client** carries every read of the fleet's leases: `Get`,
the listings, the watches, the placement hints and the protocol gate's view.

The engine (`internal/engine`) hands the store the **lease connection** as its
hold client and the coordination connection as its read client. The lease
connection is `internal/queue/jetstream`'s `Queue.DialLeases`: on a leaf, an
in-process client in a second local account whose own leaf link is isolated
from other leaves' interest; on an external broker, a second dial with pings
and a write deadline of one heartbeat at the shortest lease; on a member, a
third in-process connection. The lease link then carries what this node holds
and nothing that grows with the fleet.

Both clients' answers are leader reads and leader-decided writes, so a renewal
acknowledged on one is read on the other.

## Why the obvious alternative is wrong

**Bounding every message instead.** Every write the node sends (an event, a
dead letter, a record) would have to cross within a heartbeat at the floor,
carried another way past that. It changes every write path to protect one
kind of traffic.

**Moving the whole lease store.** The fleet's lease reads grow with the
fleet: every node lists every presence each sweep, watches every write and
runs the gate's view of every seat. On the lease link they queue a renewal
behind the whole fleet's records. A slow read connection makes a view stale
or a gated claim unknown; it never costs a held lease.

## What this does not decide

The data connection's own head of line: what a node sends behind a large
write on it still waits. A `[data, seats]` member on a slow link: its renewals
are raft proposals across its routes, which no client connection takes off
them, so the slow-link shape is the stateless `[seats]` satellite. A FIFO
bottleneck the two links share carries a renewal behind whatever is ahead of
it there, which the windows bound (ADR-0161) and unwindowed bulk does not.
Whether the two links reach the fleet together is ADR-0173's.

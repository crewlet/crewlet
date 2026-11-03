# ADR-0074: Every node publishes what its MCP servers list, and the fleet reads the union

- **Status:** accepted
- **Authority:** `internal/coord`
- **Enforced-by:** `internal/coord.TestAListingCountsOnlyForItsNodesCurrentIncarnation`, `internal/coord/memory.TestFleetContract`, `internal/coord/kv.TestFleetContract`, `internal/engine.TestANodesListingIsTheUnionOfItsInstances`, `internal/engine.TestAListingKeepsItsSchemasWithinItsBudgetInNameOrder`, `internal/engine.TestAListingIsWrittenWhenItMovesAndAgainUnderANewEpoch`, `internal/engine.TestAnotherNodesListingReachesThisNodesCatalogueWhileItsNodeIsLive`, `internal/engine.TestTheSweepRemovesOnlyWhatNoReaderCounts`, `internal/engine.TestANodeThatStopsRemovesItsOwnListings`, `internal/api/queries.TestTwoNodesListingsOfAServerMergeIntoItsDetail`, `internal/api/queries.TestAListedToolJoinsTheCatalogueUnderANameNoRowHolds`
- **Cost-when-tried:** the tool catalogue was each node's own registry, which holds a shared server's tools and none of a per-agent server's, whose children run only on the node holding each seat. A company running GitHub's and Datadog's servers per agent showed none of their tools on any screen, though every agent was offered them, and a node's answer about a server depended on which seats it happened to hold.
- **Tag-status:** unreleased

## The decision

**Each node publishes, per MCP server, the union of what its own instances
list; every reader takes the union of the live nodes' listings.** A listing is
one coordination record per (server, node) in the positions register, the
bucket with no age: each tool's name as offered and as the server names it,
its title, description and hints (overrides applied), how many of the node's
instances list it, and the digest of the server's own definition, beside the
server's identity and instructions from its handshake. Each definition's
schemas are one more record, stored create-only under that digest, so a
schema forty seats' children list is stored once.

**A listing carries the presence epoch it was written under, and counts only
while its node holds its presence at that epoch** (`coord.LiveMCPListings`, the
one filter every reader uses). A node that dies stops counting when its
presence lapses; one that restarted or claimed its presence again is a new
incarnation and writes everything again under its new epoch.

**A node derives its listing at every beat and writes it only when it moved**,
schemas first. It removes its own listings when it shuts down; a drain has
already stopped them counting, since a draining node gives up its presence. The maintenance
sweep removes a listing no reader counts, reading the listings before the
presence and removing each at the version it read, and a schema no listing has
named for two sweeps in a row.

**Every listing is bounded where it is built and again by the store**: at most
256 tools (the count travels whole), each text cut, schemas past 64 KiB listed
without being stored. **The schemas one listing stores are bounded too**, to
1 MiB in all (`coord.MaxMCPListingSchemaBytes`), kept in tool-name order while
they fit, by the node that builds the listing, since the store names schemas
by digest and cannot weigh them: 256 definitions at their own bound would be
16 MiB per (server, node), minted again by every restart that changes them. A
record's key names its server and node and the store stamps both from it, so
a node describes only its own listing of the server the key names.

## Why the obvious alternatives are wrong

**On the presence heartbeat**, beside each server's status row: a listing is
tens of kilobytes where a status row is a few hundred bytes, and the heartbeat
is re-sent every beat, so the fleet would rewrite every catalogue every fifteen
seconds to say nothing had changed.

**A fan-out at query time**, asking every node over the router: a screen's poll
would cost a request per node and an answer would wait on the slowest, and the
catalogue push, which is built with no request to wait in, could not be made
from one at all.

**A bucket with an age**, so a dead node's listing ages out on its own: an age
forgets a listing that has not changed, which is a server listing exactly what
it did, unless every node rewrites every listing inside the age, which is the
heartbeat's cost again.

**Filtering by the node alone**, without the epoch: a node that restarted is
counted for what its previous incarnation listed until the new one writes over
it, and a sweep that removed a lapsed node's listing could never tell the node
to write it again.

## What this does not decide

It does not decide what a tool's level is, or whether a changed definition is
held for review: the digest names a definition so that a later decision can be
held to it, and that decision is its own record.

It does not make the listing current to the moment: a node writes at its beat,
and a reader on another node sees it when its watch delivers it. A screen that
needs the moment asks the server, which this does not.

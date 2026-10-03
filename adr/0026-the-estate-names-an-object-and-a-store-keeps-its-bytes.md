# ADR-0026 — The estate names an object, and a store the fleet shares keeps its bytes

- **Status:** accepted
- **Authority:** `internal/objstore`
- **Enforced-by:** `internal/objstore/references.TestEveryTableThatNamesAChunkIsDeclared`
- **Cost-when-tried:** an engine-placed store — a placement map over groups of chunk slots, an integer straw2 draw, a balancer, per-node repair, scrub and collection passes, an `objects:` membership lease with its own health, probation and absence counted in ticks — took thirteen packages and files to keep the bytes where one map said, and every one of those rules was a second copy of what the broker or a bucket already does for its own data
- **Tag-status:** unreleased

## The decision

A company's FILES are split in two. Everything the company has to agree on
about a file — that it exists, what it is called, where it lives, which chunks
make it up — is a row in the replicated estate, written by a state log's
applier like any other. The BYTES are cut into content-addressed 1 MiB chunks
and kept in ONE store the whole fleet shares, chosen in Tier A
(`store.objects`): by default a JetStream object store bucket on the fleet's
own broker, replicated at `stream.replicas` across the members exactly as the
logs are, or an S3-compatible bucket. Every node — a node without `data`
included — reads and writes chunks through that store directly, and the fleet
records which store it is so a node configured with another refuses to boot.

The engine places nothing and repairs nothing: the store keeps the copies. What
is left for the engine is the one thing the store cannot know — which chunks a
row still names — and that is the cross-package half that makes this a record
rather than a package doc. The list of tables that name chunks lives with
neither the consumer nor the collector: a consumer declares its table (and the
state log whose applier writes it) in `internal/objstore/references`, and a
test holds that list against the replicated schema in both directions, because
a table naming chunks that nobody declared is a table whose files the collector
deletes a day after they were written. The list is the ONLY input the readers
have — the collector, its audit and the backup each build their statements from
the declarations — so there is no second statement for it to disagree with.

## Why the obvious alternatives are wrong

**Every data node holds every file in its database** is what the rest of the
estate does, and it puts bytes nobody reads on a turn into every snapshot,
every catch-up and every backup of the estate file.

**The engine places the bytes itself**, on a few data nodes chosen by a map, is
what this record decided first. It divides the storage bill, and it costs a
second distributed system beside the broker: membership, health, a balancer,
repair after every change, a scrub for rot, a collector that may only delete a
copy once every other holder vouches for its own — each a rule the broker
already runs for its streams and a bucket runs for its objects. A fleet runs
three or five data nodes; at three copies on three members a map places every
chunk on every member anyway. A company whose files outgrow a member's disk
takes a bucket, whose capacity is its provider's.

**A store per node** — each data node keeping what it was sent — makes a
download depend on which node is up, and turns a node's loss into the loss of
its files.

## What this does not decide

Where anything other than a file's bytes lives: the estate is still whole on
every data node. And when a chunk may be deleted, which is
[ADR-0027](0027-a-deletion-from-the-shared-store-is-judged-under-the-chunks-lock.md).

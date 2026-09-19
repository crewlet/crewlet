# ADR-0013 — A seat's identity is derived, never looked up

- **Status:** accepted
- **Authority:** `internal/org`
- **Enforced-by:** `internal/org.TestDeriveAgentIDIsStable`
- **Measured:** a UUIDv5 over `(org name, handle)` — `uuid.NewSHA1`, one hash, no allocation that matters and no I/O at all, against a store read on a node that by construction holds none of that seat's rows.
- **Tag-status:** unreleased

## The decision

An agent seat's runtime id is a UUIDv5 over the organisation name and the
seat's handle. Every node computes the same id from configuration alone — no
database, no running instance, no lookup, and no agreement protocol.

That is what lets a node act on a seat it does not run. A delivery won by node
A for a seat node B owns still has to be attributed, routed, budgeted and
recorded against that seat's identity, and node A has none of B's rows. The
same derivation is why a seat's subjects, its budget scope and its mailbox
name can all be formed anywhere in the fleet.

The corollary is stated where it is easy to get wrong: a miss in this node's
seat pool means "not on this node", never "does not exist".

## Why the obvious alternative is wrong

The obvious alternative is a row: mint a uuid when a seat is first created,
store it, and look it up. It is what every system with a users table does, and
it has one property this one cannot have — the id survives a rename.

It is wrong here because the lookup has to succeed on a node that has never run
the seat, and there is no estate that answers. The node's own database is
per-node and holds only the seats it runs. The replicated estate is derived
from a log and would make seat identity a thing to replicate, order and wait
for, so a node would be unable to route to a colleague until its applier caught
up. And coordination would put a network round trip, a cache and a cache
invalidation on the path of every attribution the engine makes.

The cost of the derivation is real and is the honest price: **renaming a seat's
handle mints a new identity.** The old id's memory, budget scope and history
stay where they were, under a handle nothing now names. That is a
configuration change an operator makes deliberately, and making it cheap would
have meant paying the lookup on every routing decision instead.

## What this does not decide

It does not decide what a HANDLE is, how one is derived from a role name, or
which names yield no handle at all — that is `internal/org`'s own validation,
and it refuses a role whose name yields nothing rather than deriving an id from
an empty string.

It does not apply to anything a person creates at runtime: a task, a page, a
channel and a sandbox run all carry minted uuids on their own rows, because
none of them has to be computable by a node that has never heard of it.

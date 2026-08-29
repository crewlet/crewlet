# d-203 — The secret store stays node-local

**Status:** decided. The alternative is buildable and was measured against;
what it costs is a new API route carrying credentials, which is a product
call rather than a correctness one.

**Applies to:** `internal/store/secretvalues.go`, `cmd/crewlet/secrets.go`,
`internal/store/lock.go`.

## The question

Six kinds of fleet-shared state moved onto the coordination KV (d-201): seat
leases, the activation pointer, the completion ledger, delivery dedupe, the
credential cooldowns and the token counter. Each moved for the same reason —
a node's own database is invisible to its peers, so anything the COMPANY has
to agree on cannot live there.

`secret_values` did not move, and a reader comparing the two lists will
reasonably ask why. A rotated credential is exactly the sort of thing a fleet
would want to agree on: `crewlet secrets set` writes the rows of the one node
whose Tier A file it was pointed at, so on three nodes an operator runs it
three times or the rotation half-lands.

## What was decided

**It stays in the node's own store, and the gesture is a rolling one.** Stop a
node, set the value, start it, move on — or supply the value through the
process environment, which every node's resolver falls back to and which needs
no downtime at all.

## Why the obvious move does not work here

The KV is reachable from a second process only when the topology has an
external or clustered broker. On the DEFAULT topology — `stream.type:
embedded`, one node — the JetStream server runs inside the engine's own
process and, in the solo case, opens no socket at all. A second process cannot
dial it, and cannot start its own server over the same `store_dir` either:
that has the identical one-file-one-process hazard the store has, measured in
d-002 and enforced since by `internal/store/lock.go`.

So moving the rows to the KV does not, on its own, give the CLI a way to write
them. It forces a second change: the CLI has to stop writing storage directly
and start asking the ENGINE to write, over the API. Which is coherent — and on
a fleet it is strictly better than what we have, because one call would then
reach every node with no restart —

**but it means a route that accepts plaintext credentials.** Everything else
the API carries is either already redacted on the way out (`GET /config`) or
was never a secret; a `${VAR}` pointer in a company document is a *name*, not a
value. Adding a route whose body is the value itself widens the surface that an
auth bypass, a proxy log or a misconfigured ingress would expose, on a
subsystem whose whole purpose is that credentials are encrypted at rest and
never printed.

That trade is a product decision about the deployment's threat model, not a
correctness question with a right answer. It is recorded here rather than made
here.

## What was fixed instead

The two things that were genuinely wrong, both of which the propagation
question was obscuring:

- **The docs claimed propagation that does not happen.**
  `docs/concepts/secret-store.md` said "**every** node re-reads the store as it
  converges on the new activation epoch" — true only of each node's own rows.
  The activation epoch propagates; the value it re-resolves does not. Corrected
  there, and `crewlet secrets set` now says so after every write, because a
  rotation that works on the node an operator tested and nowhere else is
  otherwise a silent failure.
- **The CLI corrupted a running engine's database.** It opened the engine's
  live file from a second OS process — the exact thing the store's package doc
  forbids — and on the certified `sqlite` fallback driver that SUCCEEDS, with
  no error, until a write collides. `internal/store/lock.go` now refuses it.

## What would change this

A deployment that needs credential rotation without touching nodes, and whose
operator has weighed the route against their own ingress. If that is asked
for, the shape is settled: `coord.Secrets` beside the other six buckets, an
authenticated `PUT /secrets/{name}` on the API, and `crewlet secrets` becoming
a client of it — the CLI stops opening storage entirely, which also makes the
hazard above structurally impossible rather than merely locked out.

# q — the Go credential pool has no fleet-wide cooldown seam

Status: **OPEN — a known gap, recorded rather than discovered later**
Found by: reviewing `internal/providers/credential` against
`src/crewlet/providers/credential.py` and `src/crewlet/db/credentials.py`.

## The gap

The Go pool's cooldowns are entirely process-local. The Python pool takes an
optional `cooldown_sync` (`crewlet.db.credentials.CooldownSync`) and does two
things with it that the Go pool cannot do at all:

- **Publishes** a cooldown when it cools a key, so peers learn about a
  rate-limit one node discovered.
- **Force-re-reads** peer cooldowns at the one moment it matters — when every
  local key looks cooled and the caller is about to fall through to a different
  MODEL for this seat. The Python comment is explicit that this read is worth
  its cost there: "a peer may have recorded a shorter cooldown, or ours may be
  stale."

This is not an optional nicety in this system. `CLAUDE.md`'s package layout
names fleet-wide credential cooldowns as one of THE SHARED COUNTERS in `db/`,
alongside budget usage and webhook dedupe — the state that must be shared
because every node acts on it.

## What process-local costs

- **N nodes each pay their own 429** to learn what one of them already knows,
  on a key the provider has already told the fleet to back off.
- **A node keeps using a key a peer knows is cooled**, which is the direction
  that extends a rate limit rather than waiting it out.
- Both get worse with fleet size, which is the opposite of what a fleet is for.

## Why it is not fixed in this commit

The seam it needs is a store-backed counter, and the store integration is
phased separately. Adding a `CooldownSync` interface with no implementation
would be a stub, which this project forbids; adding the implementation means
reaching into `internal/store` from the provider layer, which is a layering
decision rather than a mechanical port.

## What must happen before the fleet topology ships

The pool grows a narrow optional seam — publish-on-cool and force-read-when-all-
cooled, exactly the two calls Python makes — with an in-memory twin and the
store-backed implementation certified by one contract suite, per the pattern
already proven three times here (`coordtest`, `queuetest`, `scheduletest`).

Until then this is a SOLO-TOPOLOGY-ONLY pool, and that is the honest label for
it. It is correct for one node and progressively wrong for more.

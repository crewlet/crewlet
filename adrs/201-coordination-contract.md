# adr-201 — The coordination contract, and how it lands on a KV

Status: **Accepted**

The contract is `internal/coord/coord.go`. This note records what a reader
should not have to re-derive, and how the SQL semantics map onto a
compare-and-swap KV (JetStream KV in v1).

## 1. The tri-state is the return signature

Distinguishing "definitely not yours" from "cannot tell" needs a third answer,
and `(value, error)` already is one — so the contract states it rather than
inventing an error type to carry it: `(lease, nil)` held,
`(nil, nil)` definitively not held, `(nil, err)` unknown. **Any** non-nil error
means unknown; no error is worth special-casing into a definite answer.

Conflating unknown with lost tears a healthy company down over a two-second
store blip. Conflating unknown with refusal makes a node stop refreshing its own
presence during exactly the outage it should ride out. Both shipped, once.

## 2. Ownership and epoch are TWO keys

The single hardest mapping. Postgres kept one row and expired it in place, so
the epoch survived release. A KV with per-key TTL deletes the key on expiry —
which would reset the epoch, handing the next owner a token a zombie from the
previous tenure is still fencing with.

So — and this shape is **measured**, not inferred (`internal/coord/kv/behavior_test.go`):

- **`crewlet_leases` bucket, `TTL` (bucket MaxAge) = the lease TTL.** One key
  per resource carrying owner, epoch, preferred, protocol and meta as JSON.
  Every write refreshes that entry's age, so `Update` at the current revision
  IS the renew, an unrenewed key expires server-side, and a peer's `Create`
  succeeds afterwards. The store's own expiry is the arbiter clock — the role
  Postgres `now()` played, and nodes never compare their own wall clocks.
- **`crewlet_epochs` bucket, NO TTL.** One persistent counter per resource,
  only ever CAS-incremented. Measured: it survives the lease key's expiry,
  which is the whole point. Gaps are harmless; resets are not.

**Per-key TTL (`KeyTTL`) cannot be used for this.** It is create-only by
design — "the TTL is set when the key is created and cannot be changed later"
— and `Update` clears it. Measured: a lease renewed through `Update` became
**immortal**, which would mean a dead node's seat could never be reclaimed.
The bucket-MaxAge form is the renewable one.

**Consequence: one TTL per bucket, so the backend takes its TTL at
construction** and rejects a per-call TTL that disagrees. That is honest
rather than limiting — seats, singleton duties and node presence all run on
the same 45 s TTL, and a backend that silently accepted a different one would
be lying about when a lease expires.

### Acquire

1. `Get(lease.<r>)`. Present ⇒ live (server-side TTL guarantees it).
   - Owner is us ⇒ renew: `Update` at the read revision, **keeping the epoch**
     (only an unbroken hold keeps it).
   - Owner is someone else ⇒ `(nil, nil)`.
2. Absent ⇒ CAS-increment `epoch.<r>` to get a fresh epoch, then `Create`
   `lease.<r>` (create-only, so a concurrent claimer loses). Create collision ⇒
   re-read and re-evaluate; the burned epoch number is a harmless gap.

Bumping the epoch *before* creating ownership is deliberate: the other order
leaves a window where ownership exists at a stale epoch, which is the exact
state fencing exists to prevent.

### Renew / Release

Renew is `Update` at the current revision with the same epoch and a fresh TTL —
a definite `false` when the value's owner/epoch no longer match. Release
**writes a tombstone value with a 1-second TTL** rather than deleting: it must
expire in place. The epoch key is untouched, which is what makes release safe.

## 3. The protocol gate: check → claim → re-check → release

Postgres evaluated "no live lease at a lower protocol" as a subquery *inside*
the claim statement, because a read-then-claim loses the race it exists to
prevent. A KV cannot express a cross-key predicate inside a CAS.

The Go design is check → claim → **re-check → release on violation**. The
window shrinks to the interval between our check and our claim, and its
consequence changes from *silent mixed-protocol operation* to *a claim we
immediately give back*. Combined with the gate's existing asymmetry (only newer
nodes wait; older ones were never gated), that is a faithful degradation.

This is deliberate and recorded, not an oversight. If it ever proves
insufficient, the fix is a fleet-protocol key
maintained at presence registration, read as a single Get inside the claim path.

## 4. Everything else is a single-key CAS

The remaining coordination stores were already single-row conditional upserts,
which is why they move to a KV without redesign:

| Store | KV shape |
|---|---|
| Completion ledger | create-only key per (seat, trigger event id); first writer wins |
| Budget usage | CAS increment with the cap in the read-compare-write loop |
| Webhook dedupe | create-or-TTL-reclaim key |
| Rate valve | fixed-window counter key, compared in app code |
| Credential cooldowns | max-merge on a deadline value |
| Config activations | append-only stream; **epoch = stream sequence** |
| Per-node apply status | one key per node, re-put every tick, TTL-fresh — and the LIVE view only: the same outcome also goes into each node's event log as `config_revision_applied`, because a bucket whose age makes a dead node vanish in a minute is the wrong place to look for the node that died |
| A2A channel record | one key per channel; create-only open, CAS read-modify-write for the close and the message count |
| Scheduled-fire claim | create-only key per dispatch identity; first writer wins |
| Detached sandbox run | one key per kick-off turn, value OPAQUE; every conditional flip is a CAS on the record's revision |

Two need care. **Budget spend** charges agent and org together and Postgres did
it in one transaction so a seat-refused turn never charges the org. A KV has no
transaction, so one of the two is charged first and compensated if the other
refuses — and the order is not free. This decision originally said agent first;
building it proved that wrong. A seat-first compensation is UNREACHABLE: the
org counter is charged only after the seat's own charge succeeded, so the
branch that would refund the seat never runs, and a mutation deleting it passed
the whole suite. **Charge the org first and compensate it when the seat
refuses**, and keep the refusal naming its own scope — "the company is out" and
"this seat is out" send an operator to different places, and a bare refusal
sends them to neither. See `internal/coord/fleet.go`.

**A2A channels** are the late addition, and the one whose per-node home broke a
feature outright rather than skewing a count. The record authorizes an ANSWER,
and the answer is published from whichever node owns the target's seat — never
the node that opened the channel. On the node's own database that read found
nothing, so a cross-node ask woke its target, spent a turn on an answer, and
dropped it as "no such channel"; two seats that happened to land together
worked, which is why it looked healthy on one node. Its bucket is the third
with NO age, and for a reason neither of the other two has: an age cannot tell
an open channel from a closed one, so a TTL would reap the record of an ask
still waiting. Closing an idle channel and deleting a closed one stay decisions
taken by the `maintenance` singleton duty.

**Detached sandbox runs** are the one slot whose VALUE coordination does not
understand, and the departure is deliberate. The record has twenty-five fields
and every decision taken on them — the at-most-once tail claim, the epoch
fence, the conditional pause expiry — belongs to internal/sandbox, beside the
contract suite that certifies it. So the KV holds bytes and a revision, and
what was a mutex inside one process becomes a read-decide-write across a fleet:
a writer that loses the revision RE-READS and RE-DECIDES rather than replaying
what it computed, because the condition it evaluated is a field the winner may
have changed.

**The fleet store is always the KV.** Only the LEASE store follows
`coordination.type`. The two answer different questions and only one is about
peers: a lease asks "who runs this seat", and a single node has nobody to fence
against and re-claims at boot — but the fleet store holds RECORDS that must
outlive the PROCESS, on one node as much as on four. While it followed the
lease slot, the default single-node topology kept the token counter in memory
(so a company's spend reset on every restart, against a bucket documented as
having no retention at all), lost turn completions across a restart, and
forgot detached sandbox runs — billed boxes — entirely.

**Config activation** must append the epoch atomically with the revision flip,
or a crash leaves the fleet converged on a revision nobody asked for — on JetStream the append IS the
commit, so the flip is derived from the stream rather than written separately.

**The revision PAYLOAD goes with the pointer**, which this decision originally
left out and which made the whole plane a fleet of one anyway. A revision is
stored in the database of whichever node served the write; every other node
meets it when the pointer names it, and a peer with no copy has nothing to
apply. It read nothing and reported "no such revision" once per reconcile tick,
for the life of the deployment. So Activate is two writes in a fixed order —
the sealed body, then the pointer — and a node that fetches a body ADOPTS it
into its own `company_config`, which is where its history and its revert
targets are read from. Only the CURRENT body is kept: a node behind the fleet
needs the revision the pointer names and never an older one, and a per-revision
history in an ageless bucket would grow without bound for rows nothing reads.

## 5. Fail-open vs fail-closed is per store, and deliberate

Each polarity is chosen per contract, and a change that "harmonises" them is
wrong. **Open** (unreachable ⇒ proceed): completion ledger in both directions
(the safe answer is the pre-ledger one — run it), webhook dedupe (a duplicate is
recoverable, a dropped delivery is lost work), rate valve, cooldown read.
**Closed** (unknown ⇒ refuse or hold): budget spend; the A2A channel read, which
must RAISE rather than answer "no such channel" — collapsing the two turns a
two-second broker blip into a company where every agent has stopped replying to
every other one; lease renew ambiguity
(keep the seat, quiesce admission); the onboarding pass claim; the secret store,
loudly — an empty string there becomes an empty Bearer token discovered hours
later.

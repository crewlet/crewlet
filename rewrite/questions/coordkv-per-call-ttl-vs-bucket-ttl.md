# q — coordtest passes three TTLs; a one-TTL-per-bucket backend can accept one

Status: **resolved in `internal/coord/kv/` without changing any contract** —
recorded here because the resolution is a real semantic addition and the owner
of d-201 should rule on it. Raised implementing `internal/coord/kv/`.

## The collision

d-201 §2 and the task brief both say, correctly, that JetStream KV's renewable
TTL is the BUCKET's `MaxAge`, and therefore:

> the backend takes its TTL at construction and rejects a per-call TTL that
> disagrees.

`coordtest` — the suite that certifies the backend — passes **three** different
TTLs, and the difference between them is load-bearing:

| constant | value | what it is for |
|---|---|---|
| `coordtest.LongTTL` | 5 min | anything that must survive a lapse |
| `coordtest.ShortTTL` | 100 ms | the lease a case intends to lapse |
| `churnTTL` (concurrency) | 2 ms | leases that really lapse mid-stress |

`harness.lapse()` outlasts a lease by sleeping `ShortTTL + 50 ms` on a backend
whose clock it cannot move — which a real broker's is. So for the suite to
pass, a `ShortTTL` lease must be lapsed 150 ms in **while a `LongTTL` lease
taken in the same case is still held**. A backend with one TTL per bucket
cannot produce that from one bucket, at any bucket TTL: below 150 ms both
lapse, above it neither does.

The two instructions are individually right and jointly unsatisfiable. There is
no bucket TTL that both honours and does not honour a per-call TTL.

## What the backend does

`Config.TTL` is the bucket's `MaxAge` and the **maximum** honourable lease TTL.

- `ttl > Config.TTL` is **refused with an error** — not `(nil, nil)`, which
  would claim a peer holds the resource. The bucket would reap the record
  before that deadline, so reporting it would be a lie about when the lease
  ends. This is the "rejects a per-call TTL that disagrees" rule, narrowed to
  the disagreement that cannot be honoured.
- `ttl == Config.TTL` — every production caller, since seats, singleton duties
  and node presence all run on one heartbeat — is **exactly the measured
  design**. The record's own disappearance is its expiry, the broker is the
  arbiter, and no clock is read at all.
- `ttl < Config.TTL` is honoured by a deadline carried in the record, evaluated
  against `Store.storeNow` — the leases stream's own `StreamInfo` timestamp.
  Never `time.Now`: a fleet where each node compares its local wall clock to a
  store-assigned deadline hands two nodes the same seat the first time an NTP
  step separates them, and that is the property this whole layer exists to
  keep.

`storeNow` is read only when a record in the snapshot actually carries a
shorter TTL, so the production path pays nothing for the affordance: zero extra
round trips, and the code path that consults a clock is never entered.

## What that costs, honestly

1. **A second expiry mechanism exists.** Two ways for a lease to end (the
   broker reaps the key; the record's own deadline passes) is more surface than
   d-201 intended. They coincide in production and the shorter one is only
   reachable by a caller that deliberately asks for less.
2. **`storeNow` is read after the records, not before**, so a read can be
   momentarily conservative — a record renewed in between reads as lapsed. It
   cannot cost exclusivity, because every takeover is a CAS at the revision the
   record was read at and a renew in that window fails it. The liveness
   judgement decides what to ATTEMPT; the CAS decides what happens.
3. **One extra API round trip** per operation that sees a short-TTL record.

## The alternative, if this is not wanted

Change `coordtest` so lapse is expressed as one TTL plus a wait, rather than as
two TTLs — e.g. a single `coordtest.TTL` with `LongTTL` claims re-heartbeated
across the lapse the way a real node does. That would let the KV backend reject
every TTL but its own and delete the second mechanism entirely. It is a change
to a contract owned by another agent, so it was not made here.

## Also worth ruling on: the claiming state

Not the same question, but discovered by the same suite and worth a look, since
it is a deviation from d-201's stated write order.

`one_winner_under_a_claim_stampede` asserts the winner of a 32-way race holds
**epoch 1**. Advancing the counter before writing ownership — d-201 §2's order —
means every LOSER of the ownership CAS has already advanced it, so the winner
holds whatever it happened to reserve. Measured: the case failed with "the
winner holds epoch 2, want 1", intermittently.

d-201's reason for that order stands and is not negotiable: the other order
leaves ownership at a token the counter has not committed, and a zombie from
that tenure fences straight through the next one after the lease key expires.

So the claim is three writes, and the order carries the invariant:

1. win the record in a **claiming state** — owner set, `epoch: 0`. This CAS is
   the exclusivity point, so exactly one claimant proceeds. Epoch 0 is not a
   fencing token (a conditional write predicated on it matches an unset
   column), and `TryAcquire` has not returned, so nothing can be written under
   it;
2. advance the counter — uncontended, because we hold the record;
3. commit the token into the record we already hold.

Ownership therefore never exists at an uncommitted token, and only the eventual
owner advances the counter — so the stampede winner holds epoch 1
deterministically, and epochs are no longer burned by losers. The cost is one
extra write on a fresh claim (renewals are unchanged at one) and a second
liveness notion inside the backend: a claiming record BLOCKS a peer's claim but
is never handed out as a `coord.Lease`.

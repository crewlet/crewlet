# d-503 — The inbound webhook edge

**Status:** DECIDED · Applies to: `internal/api/webhooks/`

Seven endpoints, four signature schemes, one pipeline. This records the rules
the pipeline enforces and the four places the Go edge deliberately does
something the Python one did not.

## Why the edge verifies at all

Every `/webhooks/` route is exempt from the API's bearer token. That exemption is
granted on the stated grounds that these routes authenticate by *provider*
credential instead — so the provider check is not defence in depth here, it is
the only authentication the route has.

The Python edge lost that property twice, and both failures were silent:

- **Slack skipped verification entirely when no signing secret was configured.**
  The check sat inside `if signing_secrets:`. A deployment with no Slack secret
  answered `200` to an unsigned POST, so anyone who could reach the port could
  publish a `raw_webhook` addressed at any seat — and the engine treats that as
  a message from Slack: it wakes the agent and drives a turn.
- **Jira and Confluence verified only inside their transports.** By the time the
  transport ran, the payload had already been written to the event store and fanned
  out to every connected dashboard socket. No agent woke, which is exactly why
  nobody noticed: the pollution was invisible from the org's behaviour.

Both were ordering bugs in code that *looked* right, so the Go edge makes the
ordering structural rather than conventional:

```go
type verified struct{ source string }              // unexported, no exported constructor
func (r *Receiver) accept(..., v verified, ...)    // the only way to reach the queue
```

`accept` — which claims, republishes, records and answers — cannot be called
without a `verified`, and only the two guards mint one. That is a compile error
rather than a review comment. `ordering_guard_test.go` covers the one thing the
compiler cannot: a new handler minting its own `verified{}` to get past the
signature.

## Nothing unverified is decoded

The raw body is buffered before the gate and there is no avoiding it — the
signature is over that body, so it cannot be read afterwards — and the cost is
bounded by `MaxBodyBytes`. **Decoding** it before the gate was avoidable, and
was not avoided: five routes ran `parseBody` first, handing an unauthenticated
caller a JSON unmarshal and a `map[string]any` several times the size of what
they sent, on every request, for nothing. Not one of them read a body field
before its gate.

They now authenticate first. Two routes genuinely cannot: Slack keys its
readiness answer on `type` and answers the `url_verification` handshake without
a signature at all (deliberately — the response is a pure echo of the caller's
own challenge), and Forge keys the same readiness answer on `eventType`. Both
say so at the line.

The observable is the STATUS on a delivery that is both unsigned and
unparseable: `401` means the gate ran first, `400` means the parser did. That
is also the better answer on its own merits — an unauthenticated caller learns
nothing about how their body was read — and it is what
`TestAnUnverifiedDeliveryIsNeverParsed` pins.

## The response vocabulary

Three refusals, and the difference between them is the whole design:

| situation | answer | Retry-After |
|---|---|---|
| no company revision active on this node | `503 unconfigured` | 15 s |
| the route has no secret / no Forge app id | `503 no_webhook_secret` | 300 s |
| a credential was presented and did not match | `401` | — |
| the delivery was verified and could not be queued | `503 queue_unavailable` | 15 s |

**5xx, never 4xx, for the first two.** A 4xx tells the sender its request was
malformed and should be discarded — and the request is fine; what is missing is
on this side. A 4xx there makes the sender drop a delivery nobody holds another
copy of: silent, unretried, unrecoverable loss. 503 is the honest answer —
nothing crashed, this node cannot serve this delivery *yet* — and it is what
every provider's retry schedule exists for.

The two `Retry-After` values are different on purpose. The unconfigured case
resolves itself on the next control-plane poll; the missing-secret case waits on
a human editing config, and a sender hammering every 15 s in the meantime buys
nothing.

## Four deliberate departures from the Python edge

**1. The readiness check runs BEFORE the signature check, uniformly.** Python
put it after verification on six routes and before it on Slack. Before is
correct for a reason that only shows up in the answer: a node with no active
revision has no secrets either, so verifying first answers every delivery with
the *no-secret* 503 and its five-minute wait — telling a sender to wait out a
human edit when what it is actually waiting for is a poll fifteen seconds away.
Nothing is persisted by answering there, so verify-before-persistence is
untouched.

**2. The body is bounded.** `MaxBodyBytes = 25 MiB`, GitHub's own documented
ceiling and the largest of the seven providers', so no legitimate delivery is
refused. The body must be read before the signature can be checked — the
signature is over the body — so without a bound an unauthenticated caller picks
this process's allocation size. Over the cap is `413`.

**3. Slack is deduped at the edge.** Python claimed deliveries for GitHub and
GitLab only and left Slack to a per-process ring inside the transport, which
answers correctly for one node and wrongly for two: the same delivery retried to
a different node is a fresh delivery *to that node*, so the agent wakes twice
and answers twice. Slack's envelope carries an `event_id` that is stable across
its retries, which is exactly the identity this needs.

Plane, Jira, Confluence and Forge are still **not** deduped at the edge, and
that is a decision rather than an omission: they send no delivery id, so "the
same delivery" is payload coordinates the transport derives with the routing
context that makes them correct. Re-deriving that here would be a second
definition of identity with less information.

**4. A republish that fails releases the claim.** The claim is taken *before* the
republish, because two concurrent retries must not both wake the seat. Python
stopped there — so a publish failure left the delivery claimed and unhandled,
and the provider's retry, the only other copy, was refused by a row nothing
clears for the TTL. The Go edge releases and answers 503.

Ordering inside `accept` follows from the same reasoning: claim, republish,
*then* record. The republish is the only step that has to happen; the store row
and the live push are observability and must not be able to swallow a wake.
Recording first would leave the opposite failure — a feed row saying the webhook
arrived, an agent that never heard about it, and a retry the claim refuses.

## Scheme notes

- Every scheme **decodes** the presented signature and compares bytes with
  `hmac.Equal`. Comparing the hex or base64 text would be case- and
  padding-sensitive — so an arithmetically correct uppercase digest would be
  refused — and text comparison in Go is not constant-time.
- Python's `_PLANE_SIGNATURE_RE` and `_ATLASSIAN_SIGNATURE_RE` shape prefilters
  exist because `hmac.compare_digest` raises on non-ASCII `str` operands, which
  would turn an unauthenticated request into a 500. Go has no such hazard —
  `hex.DecodeString` failing *is* the prefilter — so the regexes are not ported.
- Jira and Confluence share one function because they are one scheme. Two copies
  of a signature check is how they come to disagree, and the disagreement is
  silent because each half stays self-consistent.
- GitLab's replay window and Slack's are both 5 minutes, which is what each
  provider specifies. A **future** timestamp is refused as well as an old one:
  it is what a replay looks like against a node whose clock ran slow.
- Forge pins `RS256`. Without pinning, a token names its own algorithm: `none`
  verifies everything, and `HS256` has the library check the RSA *public* key as
  an HMAC secret — which anyone holding a published key can forge against. `exp`
  is required rather than merely honoured when present, or a captured token
  replays until Atlassian rotates.

## The JWKS cache

Forge verification fetches Atlassian's published keys, and that fetch sits on the
path of an unauthenticated request — the token is checked *with* the key, so the
key is fetched before the caller is known. Three numbers bound it:

- `jwksTTL = 1h` — a staleness budget, not a schedule; Atlassian publishes no
  rotation cadence. 24 fetches a day, with a firm ceiling on how long a
  withdrawn key stays accepted.
- `jwksRefreshFloor = 1m` — rate-limits the refetch an *unknown* key id
  triggers. Without it, a caller spraying random kids turns every forgery into an
  outbound HTTPS request from this process.
- `jwksFetchTimeout = 10s` — deliveries arrive against a provider's own delivery
  deadline, and a fetch that outlived it would burn the retry it was blocking.

A fetch that fails while a cached key is held serves the cached key: it is stale,
not wrong, and refusing every Cloud delivery because a CDN blinked is an outage
caused by someone else's availability.

## The dedupe table

`webhook_deliveries` (schema `0006`) is a first-claim-wins registry keyed
`(source, delivery_key)`. The claim is one statement — `INSERT … ON CONFLICT DO
UPDATE … WHERE seen_at < ?` — so there is no read-then-write window for a peer to
slip through, and the `WHERE` on the `DO UPDATE` is what keeps the claim from
being *permanent*: without it an operator replaying a webhook ten minutes later
watches it vanish into a row nothing will ever clear.

Reported through `RowsAffected` rather than `RETURNING`, which is a newer SQLite
feature and outside the dialect intersection the two certified drivers share
(d-002). The conditional upsert is exercised against **both** drivers by the
`storetest` contract suite, because that is precisely the kind of statement one
driver accepts and the other does not.

**Fail open in every direction.** No store, no key, or a store that cannot be
reached all yield "handle it". A duplicate is recoverable noise; a delivery
dropped because a database blinked is a message nobody ever answers.

## What stays out

- `/otlp/{token}/v1/{signal}` is not a webhook and is not in this package. It
  belongs with the sandbox receiver whose per-run token is its credential.
- `raw_webhook` is deliberately absent from the event log's admission list. The
  edge already writes its own `webhook:*` row carrying the provider's exact
  bytes; admitting the queue envelope too would store every delivery twice.

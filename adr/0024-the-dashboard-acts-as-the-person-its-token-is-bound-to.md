# ADR-0024 — The dashboard acts as the person its token is bound to, and nobody else acts through it

- **Status:** accepted
- **Authority:** `internal/api/operator`
- **Enforced-by:** `internal/api/operator.TestAnActWriteRecordsTheTokenAsAuthorAndTheBoundSeat`, `internal/api/operator.TestAnUnboundOrAnonymousCallerCannotAct`
- **Measured:** the largest legal page is 512 KiB of text, and a page of nothing but quotes, backslashes and newlines is 1 MiB once `JSON.stringify` has escaped it — so the act body is capped at twice the page plus 64 KiB for the envelope (`operator.MaxActBody`, 1 114 112 bytes), and a cap at the page's own size refused a page the store accepts.
- **Cost-when-tried:** the dashboard wrote nothing, on the argument that a browser form would write as "the dashboard", an actor no audit can ask why. Every change was a pre-filled tool call a person copied into an assistant connected to `/operator/mcp` — a gesture of four steps for "mark this read", checked by a test that each call named a real tool and never by anybody pressing it; the Inbox, the priorities list and the board each showed state no control on screen could change.
- **Tag-status:** unreleased

## The decision

A person changes the company from the dashboard **as themself**: every write
is `POST /operator/act/{tool}`, one tool of the operator catalogue per request,
admitted only when the presented token is bound to a human seat by
`contact.crewlet_operator_id`. A disabled guard's caller and a token no seat
binds are refused `unbound` (403); they keep `/operator/mcp`, where a
credential acting as itself is ordinary. The attribution is the one every
operator write already carries — the token as author, kind `operator`, the
bound seat as the actor's seat — so a dashboard write and the same person's
assistant's read identically in an audit. The request names its own
`request_id`, a UUID the client mints per gesture and repeats on a retry, and
every id the write derives — each record's operation, a created object's own
id, a comment's — is derived from it and from what the call sent, scoped to
the token so an id names only that credential's own writes. A retry after an
`unknown` is therefore the first attempt's operations, never new ones.

The socket stays read-only. `viewer.acts` names the tools the transport would
serve this caller — empty unless bound — so a screen enables exactly the
controls a press of would be served, and disables the rest with the reason.

## Why the obvious alternative is wrong

The obvious alternative is to keep the dashboard read-only and route every
change through an assistant. It keeps the audit honest by making the product
unusable: the rule it protects — every write is attributed to somebody who can
be asked why — is kept equally well by a button that writes as the person the
token names, because that person is exactly who pressed it.

The second is to let any valid token act, bound or not. A browser session
holding a CI token would then write through the dashboard as the pipeline,
and a disabled guard would let anybody who reached the listener write as
`anonymous` — the one caller the chart's refusal of the reserved id
(`org.ReservedOperatorID`) exists to keep from being mistaken for a person.

The third is to write over the socket. A frame sent into a socket that drops
has no answer at all; a request that loses its answer is retried under the same
request id, as the same operations as whatever landed.

## What this does not decide

It does not decide what the dashboard renders for a write in flight, how it
confirms one, or which queries it refetches — that is the dashboard's, over the
outcome and position the answer carries. It does not move `/config`,
`/secrets`, `/setup` or `/backup`, which stay credential-scoped surfaces of
their own. And it does not change who may do what once admitted: every
authority rule is the tool's, exactly as on `/operator/mcp`.

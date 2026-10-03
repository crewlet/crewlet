# ADR-0024 — The dashboard acts as the principal its session resolves to

- **Status:** accepted
- **Authority:** `internal/api/operator`
- **Enforced-by:** `internal/api/operator.TestAnActWriteIsMadeAsItsPrincipal`, `internal/api/operator.TestACallIsMadeAndAuditedAsThePrincipalItWasAdmittedAs`, `internal/api/operator.TestAnyResolvedPrincipalMayAct`, `internal/api/operator.TestARetriedActIsOneWrite`
- **Measured:** the largest legal page is 512 KiB of text, and a page of nothing but quotes, backslashes and newlines is 1 MiB once `JSON.stringify` has escaped it — so the act body is capped at twice the page plus 64 KiB for the envelope (`operator.MaxActBody`, 1 114 112 bytes), and a cap at the page's own size refused a page the store accepts.
- **Cost-when-tried:** the dashboard wrote nothing, on the argument that a browser form would write as "the dashboard", an actor no audit can ask why. Every change was a pre-filled tool call a person copied into an assistant connected to `/operator/mcp` — a gesture of four steps for "mark this read", checked by a test that each call named a real tool and never by anybody pressing it; the Inbox, the priorities list and the board each showed state no control on screen could change.
- **Tag-status:** unreleased

## The decision

A person changes the company from the dashboard **as the principal the request
guard resolved them to**: every write is `POST /operator/act/{tool}`, one tool
of the operator catalogue per request, made through the ONE dispatch that
`/operator/mcp` and the human write surface's tool-backed routes call too. The
attribution is `iam.ActorFor`'s and nobody else's — a person signed in, or a
token the identity directory binds to a seat, writes as that seat, with kind
`human`; and a credential nobody is bound through writes under its own whole
login, with kind `operator`. The
credential the call came through rides beside each of them as the operator id
(`pat:<id>` for a machine token, `session:<lineage>` for a browser session, the
login otherwise), so a dashboard write and the same person's assistant's read
identically in an audit, and an audit can tell the two credentials apart.

**Nobody is refused for being unbound.** Every principal the guard resolved may
call the route, and every tool is decided by the authority table exactly as on
every other surface — the table already says what a credential acting as
itself may do, and a write attributed to it is one an audit can still ask
about.

**One call is made by one principal.** The principal is resolved once, by the
guard, and is a value on the request's context; the dispatch, the tool's actor
and the `operator_acted` record all read that one value. A directory rebind or
a chart rename landing while a call runs changes the next request, never this
one, so a call cannot be admitted as one person, written as another and
audited as a third.

**The request names its operation.** The key travels in the `Idempotency-Key`
header (`internal/api/opkey`), a UUIDv7 the client mints per gesture and
repeats on a retry; on this route it is REQUIRED, and an absent or malformed
key is `400 op_id_invalid` naming the header. It is SCOPED BY THE PRINCIPAL'S
ID — never a login, a token label or a session lineage, all of which a rename
or a step-up moves — so a key names only that principal's own writes, and
nobody collapses or suppresses somebody else's write by sending its key first.
Every id the write derives — each record's operation, a created object's own
id, a comment's — is derived from it, so a retry after an `unknown` is the
first attempt's operations rather than new ones; an `op_id` argument beside
the header is refused, since it would be a second answer to "which operation
is this", one of them the caller's pick.

**The socket stays read-only.** `viewer.acts` names the tools the act route
would serve this caller — the catalogue's non-read tools the authority table
could admit them to before any object is named — so a screen enables exactly
the controls a press of could be served, and disables the rest with the reason.

## Why the obvious alternative is wrong

The obvious alternative is to keep the dashboard read-only and route every
change through an assistant. It keeps the audit honest by making the product
unusable: the rule it protects — every write is attributed to somebody who can
be asked why — is kept equally well by a button that writes as the principal
the request resolved to, because that principal is exactly who pressed it.

The second is to admit only a credential bound to a seat, and refuse the rest.
That is a second authority rule standing beside the table, deciding from a
fact the table does not ask: a Tier A token the table admits on
`fleet:operate`, or an administrator signed in through it to repair a locked
directory, would be shown a dashboard whose every control is refused, while
the same credential's assistant does the same thing over `/operator/mcp`. A
refusal keyed on a binding is also a refusal keyed on whatever holds the
binding, and the binding is the identity directory's — the same read the
attribution already makes, so the refusal would add a second reading of it
that could disagree with the first.

The third is to write over the socket. A frame sent into a socket that drops
has no answer at all; a request that loses its answer is retried under the same
key, as the same operations as whatever landed.

## What this does not decide

It does not decide what the dashboard renders for a write in flight, how it
confirms one, or which queries it refetches — that is the dashboard's, over the
outcome and position the answer carries. It does not move `/config`,
`/secrets`, `/setup` or `/backup`, which stay credential-scoped surfaces of
their own, nor the verbs no seat is given and so no tool serves — a work item's
purge, a page's rename, trash, restore and purge, a comment's take-down,
rewriting one's own remark — which stay routes of the human write surface. And
it does not change who may do what once admitted: every authority rule is the
tool's, exactly as on `/operator/mcp`.

# ADR-0031 — A key names its role, and every surface names the reach it serves

- **Status:** accepted
- **Authority:** `internal/api/auth`
- **Enforced-by:** `internal/api.TestEveryRouteDeclaresTheReachItServes`, `internal/api/queries.TestARegistrationWithoutAReachPanics`, `internal/config.TestAKeyWithoutARoleIsRefused`, and the `auth.Router` signature — a route mounted without a reach does not compile
- **Cost-when-tried:** every accepted token was an operator. The scope the access screen called `person` was everything `operator` reached plus `/operator/act`, so a key handed to a teammate for their inbox revealed the secret store's values, rewrote the company document and read every agent's transcript; and a Tier A switch that opened every read, on by default, served transcripts, diaries and the whole event stream to anybody who could reach the port, its one guard a warning at startup on a bind that was not loopback. What a read needed was decided in two places — a prefix list beside the switch, and a per-question operator flag the route rule could not see — and each looked complete from inside.
- **Tag-status:** unreleased

## The decision

Every key in Tier A's `api.auth.tokens` names its **role**, `member` or
`admin`, with no default in either direction, and `api.auth.anonymous` says
what a caller with no key reaches: `public` (the default — the company's name,
mission and chart) or `none`. The guard resolves every request to one
principal carrying a **reach**, ordered `open` < `public` < `member` < `admin`,
and every surface names the reach it serves **where it is declared**: a route
at its mount and a question at its registration. A question's REST route takes
its reach from the registry, so the two cannot disagree. A caller below a
surface's reach is refused before the surface runs — 401 when no key was
accepted, 403 `forbidden` when the key was accepted and reaches less, because
"sign in" sent to somebody already signed in is a circle.

One rule draws the line between the two keyed reaches, and it is stated here
once. **Members read what the company published** — its work, its pages, its
chart, who is working on what, and the decisions put to them — and act as the
person their key is linked to. **Admins also read what the machine processed**
— prompts, responses, tool arguments and results, narration, diaries, events,
traces and spend — **and how it is run**: nodes, leases, keys, secrets,
configuration, retention and backups. A record about a person is read by that
person and by the leads whose line they are in, whatever the key's role: an
admin key is reach over the engine, not over somebody else's inbox.

## Why the obvious alternative is wrong

The obvious alternative is the one this replaced: one kind of key, a switch
that opens reads, and a list of the surfaces that stay closed. Its cost is in
the line above, and the root of it is that "which reads are safe" was answered
by a list somebody had to remember to extend, with the default at open — so
every new read was public until somebody noticed what it carried. Declaring the
reach at the declaration inverts that: a route that names none does not
compile, a question that names none panics at registration, and there is no
default to fall through to.

The second is a third role between the two — an owner who edits the company
document but cannot read the secrets. The engine cannot keep that boundary:
whoever writes the document can repoint a provider's endpoint at a server of
their own while keeping the `${VAR}` its key is sent under, the concession
ADR-0030 already makes about a document writer. Nor is a lead a role: being
somebody's lead is a relation the org chart states, and the chart answers it.

The third is a role that defaults — to `admin`, which is the posture this
ended, or to `member`, which quietly locks an operator's pipeline out of the
configuration it writes on the first restart.

## What this does not decide

It does not decide who may act as a person: the act transport admits a key
linked to a human seat, member or admin, as ADR-0024 records. It does not
decide which admin keys may change a managed company document — that is
`api.auth.company_writers`, drift control among admin keys (ADR-0030). It does
not decide what a tool permits once a caller reaches it; every authority rule
is the tool's. And it does not make Tier A agree across a fleet: keys and their
roles are per node, like the rest of `api.auth`, and every node serving the API
should carry the same ones.

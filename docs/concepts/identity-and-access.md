# Identity and Access

Every row this engine keeps — a work item's comment, a page revision, an audit
event — records **who did it**. Every route, query and tool it serves decides
**whether you may**. This page is the vocabulary both of those use: what a
*principal* is, which *grants* exist and what each one opens, and — the part
that matters when something goes wrong — the difference between the engine
knowing you are nobody and the engine not being able to tell.

> The vocabulary lives in `internal/iam`. It is a deliberately small package:
> it names things and classifies them, and it reads nothing, writes nothing
> and stores nothing.

---

## A principal is who is acting

A principal is one actor the engine can name. There are four kinds, and the
kind is not a label — it decides how the actor's work is attributed and what
it is allowed to be.

| Kind | Who that is | Recorded as |
|---|---|---|
| `person` | A human being — the founder at the dashboard, a teammate with a login | `human` if they hold a seat, `operator` if not |
| `seat` | An agent seat acting inside its own turn | `agent` |
| `machine` | A token with nobody behind it: CI, a pipeline, an automation | `operator` |
| `engine` | The engine itself — a duty's repair, a chart apply, a retention trim | `system` |

Those four recorded values (`agent`, `human`, `operator`, `system`) are the
author kinds already stored beside every tracker commit and every page
revision, and they are what an audit filter selects on.

### A person with a seat acts as themselves

A person is the one kind that splits. Bind a teammate's login to their seat in
the company document and their work lands under **their own seat handle**:

```yaml
roles:
  - name: "Jane Founder"
    kind: human
    handle: founder
    contact:
      crewlet_operator_id: founder      # an id from api.auth.tokens
```

Without that binding they act as the credential, under its own login. Both are
ordinary — an operator who is not in the org chart, a pipeline, an automation
each act as themselves and are never refused for it. What you cannot do, in
either direction, is *choose* a seat to act as: a tracker whose author field is
picked by the writer is not an audit trail.

### Names can never collide

Three kinds of name land in one author column, with nothing but the kind beside
them, so the engine keeps them apart by their **shape**:

| What | Shape | Example |
|---|---|---|
| A seat handle | one segment, lowercase letters, digits and hyphens | `backend-lead` |
| A person's login | segments joined by **dots** | `jane.doe` |
| A machine's handle | segments joined by a **colon** | `ci:release` |

A seat handle can carry neither a dot nor a colon, and the other two each
require a different one — so no login can be read as a seat, no machine handle
as a login, and an audit feed filtered on a name matches everything that name
did and nothing else. A login without a dot, or a machine handle without a
colon, is refused when it is written rather than discovered later.

---

## Grants: the ten things there are to allow

A grant is one capability. There are ten, each covering a surface the engine
actually serves, and each is either a **read** or a **write** — the classes
`api.auth.allow_anonymous_read` distinguishes, because that setting opens reads
and only reads.

### Reads

| Grant | What it opens |
|---|---|
| `state:read` | What the company is doing: the board, the pages, the roster, the org chart, the fleet, budgets, schedules — the ordinary dashboard read |
| `transcripts:read` | What a turn actually *said*: `/events`, `/agents/{id}/memory` and the turn frames on `/ws/stream` carry full prompts, tool arguments and diary entries |
| `config:read` | The company document — the org chart, every integration, and the *names* of every credential the company holds or has not set yet |
| `secrets:read` | Revealing a stored credential's value (the one `/secrets` route that returns one, which needs an explicit `?reveal=true` and logs the access) |

`state:read` and `transcripts:read` are separate on purpose: showing somebody
the board and showing them every prompt an agent was ever given are not one
decision.

`config:read` also covers the `/setup` and `/secrets` *listings*. They carry no
values, but the list of which credentials a company has **not** configured is a
map of what to attack, which is why those surfaces are guarded even for reads.

### Writes

| Grant | What it opens |
|---|---|
| `work:write` | Filing and moving work: create, update, comment, merge, and the project facets a writer may declare |
| `knowledge:write` | Authoring the company's own pages: write, save, comment |
| `config:write` | Changing the company — `PATCH /config` and the epoch activation that rebuilds every seat's tools, providers and MCP children |
| `secrets:write` | Sealing, rotating, deleting and re-keying the fleet's credentials |
| `fleet:operate` | The deployment's own controls: `POST /backup`, the retention floor, the capacity window, the maintenance gestures, evict and readmit, `POST /budgets/reset`, a work item's purge |
| `sandbox:run` | Starting a detached coding run, and holding the per-run credential its MCP bridge mints |

**Connecting an integration has no grant of its own.** `/setup` performs no
write of its own — a credential goes through the store `/secrets` serves, and
the `${VAR}` pointer through the merge and compare-and-set `/config` performs —
so connecting a vendor is exactly `config:write` plus `secrets:write`. A third
grant beside them would be a second answer to one question.

Two splits are worth knowing because they follow a real deployment shape:

- `secrets:read` is separate from `secrets:write` so an automation that
  reseals keys on a schedule can hold the write and never the read.
- `sandbox:run` is separate from `work:write` because a coding run puts
  generated code on a machine and hands it a seat's whole tool surface, which
  is a different risk from filing a ticket.

### A grant this build has never heard of

During a rolling upgrade a newer node writes capability strings an older one
cannot read. The older node **ignores that one string and keeps the rest** — it
cannot open a door it has never heard of, and it does not need to in order to
open the doors it has. It does not refuse the principal, because refusing would
log everybody out for the length of the deploy. If you see a node logging
ignored capabilities for longer than a deploy takes, that is a node nobody
finished upgrading.

---

## Stages: whether a principal may act at all

Before any grant is consulted, a principal has to be *enrolled*.

| Stage | May act | Means |
|---|---|---|
| `invited` | no | Created, has proved nothing yet |
| `enrolling` | no | Mid-proof — setting a credential, completing a second factor |
| `active` | **yes** | Enrolled |
| `suspended` | no | Enrolled and blocked, reversibly, with the record kept |
| `retired` | no | Has left; the record is kept so their audit rows still resolve to a name |

Only `active` may act, and that is an allowlist rather than a blocklist: a
stage this build does not recognise may not act either.

Seats and the engine are always `active` — neither enrols, and neither can be
suspended by anything but the org chart and the process.

---

## Anonymous is not unknown

This is the distinction to remember when a request is refused.

```mermaid
flowchart LR
    REQ["a request arrives"] --> R{"can the engine<br/>say who this is?"}
    R -->|"yes"| RES["<b>resolved</b><br/>a principal, with grants"]
    R -->|"no, definitively:<br/>nothing was presented"| ANON["<b>anonymous</b><br/>a fact about the request"]
    R -->|"could not tell:<br/>the store was unreachable,<br/>nothing resolved it"| UNK["<b>unknown</b><br/>not an answer at all"]
    ANON --> A2["served if the posture<br/>allows anonymous reads;<br/>otherwise refused as<br/><i>you presented nothing</i>"]
    UNK --> U2["refused as <i>ask again</i>,<br/>never as <i>you are not<br/>who you say</i>"]
```

**Anonymous** is a finding: the resolver ran, and nobody presented a
credential. On an open read posture that is a perfectly good answer and the
read is served.

**Unknown** is the absence of a finding: the identity store could not be
reached, a session lookup failed, or nothing resolved the request at all.
Collapsing it into anonymous is the mistake this engine refuses to make
anywhere — treating "could not tell" as "definitely not" is what tears a
healthy company down over a two-second store blip, and on this surface it
would answer *invalid credential* to somebody holding a perfectly good one and
keep answering it for as long as the outage lasted, teaching everybody to go
and reset a password that was never wrong.

So: **if you are refused and the reason says the engine could not determine who
you are, check the engine's health before you check your token.** A silence —
a request nothing resolved at all — is read as unknown too, never as
anonymous: the absence of an answer is not evidence of absence.

---

## Sessions go stale, and an unset deadline is stale

Every principal carries the instant after which its proof of identity no longer
holds, and **an unset deadline is treated as already stale, not as eternal**.
The two readings are one keystroke apart and only one of them fails safe: a
deadline nobody set is a session nobody bounded, and reading it as "never
expires" turns a field somebody forgot to fill in into a credential that
outlives the company.

A seat binding carries a timestamp for the same reason. The org chart is edited
live, so a binding read once at sign-in is stale the moment a revision moves —
and a stale binding is somebody still writing as a seat the chart no longer
gives them.

---

## The authority table: one function decides

A grant says what a principal *carries*. It does not say whether they may do a
particular thing to a particular object — because most of the interesting
answers are about a **relation**: your own inbox, the project you lead, the
comment you wrote. One function takes that decision, over one table, and every
surface asks it: an HTTP route, a seat's own tool, the operator's assistant.

That matters because there used to be no such function. Three structs carried
pre-resolved booleans into the tracker, each filled by a different surface from
a different lookup, and two of them were filled *wrong* in ways that only
showed up as a support question:

- one flag existed purely because an operator's actor was a token label, which
  is never a handle in the chart — so the lead check was false for every
  operator by construction, and a founder re-ordering an agent's queue was
  refused by the only tool that offered it;
- the project lead lookup answered `false` both for "you do not lead this" and
  for "this node holds no company yet". A node that is booting, installing a
  revision or behind the chart log therefore told every lead in the company
  that they lead nothing — while reporting itself healthy.

### The ten rules

| Rule | Covers | Decided by |
|---|---|---|
| **Read** | The board, pages, the org chart, the roster, the fleet, spend | `state:read` |
| **Self** | The caller's own diary, episodes, skills and onboarding marker | The caller, and **nobody else** — not even the admin grant |
| **Colleague write** | Filing, commenting, updating, ranking; authoring a page; asking a colleague | `work:write` for work, `knowledge:write` for pages |
| **Own record** | Marking an inbox, pinned views | The owner, or `fleet:operate` |
| **Own or lead** | Priorities, a person's day, reading their queue | The owner, whoever leads them, or `fleet:operate` |
| **Container** | A project's fields, default assignee, routing unit, tag renames | The project's lead, or `fleet:operate` |
| **Destructive** | Removing and restoring a task; trashing and restoring a page | The **container's** lead, or `fleet:operate` |
| **Purge** | Anything beyond recovery | `fleet:operate` **and** a principal that is not an agent |
| **Authored** | Editing and removing a page comment | Whoever wrote it, or `fleet:operate` |
| **Operator** | Configuration, secrets, integrations, the node itself | The grant the verb names |

A lead may re-order what their report works on; marking somebody's mail read is
a different gesture and nobody asked a lead to make it. A colleague may file
work in a project and may not take it out again. An agent holding the
deployment's own grant still cannot purge — an irreversible delete decided
inside a turn is not something a model reaches for.

**Self has no admin path, and that is why it is its own rule.** A seat's memory
tools take no handle at all, because an agent recalling another's episodes
would make the per-seat memory a shared one. An operator *reading* a seat's
memory is a different surface with a grant of its own (`transcripts:read` over
`/agents/{id}/memory`), and admitting it here too would be a second answer to
one question.

**Asking a colleague is a write.** It files no row, which is why it looks like
it belongs nowhere — but it spends somebody else's turn and somebody else's
budget, so a credential with no write capability at all must not be able to
make every seat in the company think.

**A verb with no row is refused.** Not defaulted, not passed through: a default
is how a new verb ships ungated, and it ships looking correct. A walk over the
table fails the build if a verb resolves to no rule, if a rule is declared and
no verb uses it, or if a grant the vocabulary declares opens nothing at all —
that last one is what caught `state:read` being asked for by nothing, back when
reads were "any authenticated principal".

Those three walks hold the table against **itself**, which is not enough on its
own: a verb removed from the engine leaves behind a row that is perfectly
consistent and decides nothing. So a fourth walk holds it against the tools the
build actually registers, over both surfaces — a seat's own and the operator's
assistant — in both directions. It earned its place twice on its first run: two
rows had outlived the goal verbs being removed from the tracker, and seven
tools a seat uses every turn had no rule at all.

### Cannot-tell is not no

Every rule that asks about the chart can fail to get an answer, and that
outcome is its own: the decision reports **unknown**, and a surface answers
`503 try again` rather than `403 forbidden`. Telling somebody they may not do
something sends them to go and ask for an authority they already hold.

The admin grant is checked *before* the chart on every rule that has one, so an
operator is never told "I cannot tell" by a node that is merely lagging.

### Route policy travels with the route

An HTTP route declares its verb where it is **mounted**, not in a middleware
that inspects the request. A middleware runs before the router matches, so it
has no route identity to decide from — anything it decided from would be a
second router that has to agree with the real one, which is the divergence that
made `/work/items/` and `/work/items/{key}/purge` one gate. A route mounted
without a policy, or naming a verb with no rule, fails where it is written.

One authority concern *does* live outside: a request whose path is not already
canonical is refused, because `/work/items/../config` matches one pattern and
reads to a person as another. It has to be outside, because Go's router cleans
the path and redirects before it matches at all.

---

## What is configured today

The identity vocabulary above is what the engine *names*. What an operator sets
today is smaller, and lives in two places:

- **Tier A, `api.auth.tokens`** — the credentials that may act at all, each
  with an id recorded as the author of anything written with it. Plus
  `api.auth.allow_anonymous_read`, which opens reads (and only reads) to
  callers with no credential, and `api.auth.disabled`, which turns the guard
  off entirely and stamps every write `anonymous`. See
  [Configuration](configuration.md).
- **The company document, `contact.crewlet_operator_id`** — which of those
  token ids is a *person*, and which seat they hold. The binding is written
  here rather than on the token because Tier A is the root of trust and may
  never read Tier B. See [Humans in the Org Chart](humans-in-the-org.md).

Which surfaces always need a credential — reads included — is covered in
[Configuration § Auth](configuration.md), and the operator MCP surface an
assistant reaches the company through is in
[Tools and MCP](../guides/tools-and-mcp.md).

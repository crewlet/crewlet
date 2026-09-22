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

A seat binding carries a **chart position** rather than a timestamp, and the
difference is what makes the seat lookup three-valued. The org chart is a
separate log with its own applier, so a seat missing from a node's view means
one of two opposite things — the seat is *gone*, or this node has *not applied
the hire yet* — and those answers are 403 and 503. The binding records the
chart position the decision was made at, so any node can compare its own
position against it and tell them apart. Comparing two nodes' wall clocks is
what the coordination layer states it never does, and a node merely behind on
the chart would otherwise tell everybody it has not caught up with that their
seat does not exist.

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

## Where identity is kept: the fifth state-log domain

The vocabulary above is what the engine *names*. Where it is **stored** is a
replicated state machine of its own — the state log's fifth domain, beside the
work tracker, the vectors, the knowledge base and the [org
chart](chart-domain.md). Every change is one record on one ordered stream
(`CREWLET_IAM_LOG`), applied by a deterministic applier into an identical set
of SQL tables on every node, with the checkpoint committed in the same
transaction as the rows.

> **What is in place today** is the estate itself: the tables, the subject
> grammar, the scope and the Tier A ceiling below. The applier, the sign-in
> routes and the directory arrive with it; until they do, what an operator
> configures is still the token list under [§ What is configured
> today](#what-is-configured-today).

### Uniqueness is arbitrated, not indexed

This is the one decision the whole schema is built around, and it is the
opposite of what an identity database normally does.

The replicated estate **forbids a `UNIQUE` index outside a primary key**. A
constraint violation inside an apply transaction aborts it *deterministically
on every node at once* — so a rare, cosmetic anomaly becomes a fleet-wide
stalled log, and in this domain a stalled log is every login in the company.

So nothing here is unique, and the uniqueness an identity estate obviously
needs is enforced somewhere else entirely: **at the broker, by the subject a
claim arbitrates on.**

| What is claimed | Subject | How it is taken |
|---|---|---|
| An email address | `crewlet.iam.log.email.<blind>` | create-only, expectation zero |
| A login | `crewlet.iam.log.login.<login>` | create-only, expectation zero |
| A seat binding | `crewlet.iam.log.seat.<seat handle>` | create-only, expectation zero |
| A session | `crewlet.iam.log.session.<lineage>` | create-only, expectation zero |
| A person's own content | `crewlet.iam.log.person.<id>` | conditional on the row's version |
| The first-person bootstrap | `crewlet.iam.log.bootstrap` | one object for the whole company |
| Ending every session at once | `crewlet.iam.log.invalidation` | one object for the whole company |

Two administrators enrolling one address publish to the same subject at the
same expectation, the broker accepts exactly one, and the loser is told which
person holds it. Two administrators enrolling *different* addresses never
contend at all.

Because a record has exactly one subject, **an enrolment is a sequence**: take
the address, take the login, then write the person. A sequence that stops
halfway leaves a claimed address with no person — a legal, named state that a
duty reports and a sweep collects, rather than a person holding an address
somebody else also holds.

> **If you are reading the schema and reaching for a unique index as a
> backstop: don't.** A duplicate cannot arise from ordinary traffic, and it
> *can* arise from a restore or a reanchor. Three **non-unique, partial**
> indexes over the address blind, the login and the seat id are what a duty
> reads to *report* one. A unique index would convert an anomaly an operator
> can repair into an outage nobody can.

### What is in the clear, and what is not

A person's **name** and **email address** are sealed under that person's own
data encryption key, with their id as additional authenticated data, *before*
the record is published. They are ciphertext on the broker, in every node's
database, in every snapshot and in every backup — and destroying the key is
what removing a person actually does. The row and the id outlive it, because
the audit trail names them.

Sealing happens at the **writer** rather than on each node, which is what keeps
the fleet's byte-for-byte identity claim meaningful: every node writes the same
ciphertext.

A **login** is in the clear, and the asymmetry is deliberate — a login is a
name the company chose, printed beside every change an operator reads. Blinding
a value the dashboard renders on every row would cost you the ability to read
your own audit trail for nothing.

An address is looked up by its **blind**: a keyed hash, so a node holding the
key can compute it from an address and nobody else can go the other way.
Signing in opens nothing.

### The eight tables

| Table | What it holds |
|---|---|
| `iam_people` | One person or machine, and the three claims denormalised onto their row so a duplicate can be *reported* |
| `iam_credentials` | The **verifier** for each way somebody proves themselves — a password digest, an identity-provider subject, a machine token's hash. Never a secret that could be presented to anything |
| `iam_invites` | An address spoken for by somebody who has no person yet, and the grants redeeming it confers |
| `iam_bootstrap_codes` | How a company with nobody in it acquires its first administrator |
| `iam_sessions` | One row per session **lineage**. Rotations are not rows — a rotation id is derived — so this grows with sign-ins, not with requests |
| `iam_revocation_epochs` | One person's **revocation epoch**, in its own table because every request compares against it |
| `iam_session_generation` | The **fleet-wide generation** every bearer carries: one row, about nobody, that `crewlet iam invalidate-all` moves to end every session in the company at once |
| `iam_history` | The authentication trail: who did what to whom, and why |

Beside those sit three **gate** tables — `iam_evictions`, `iam_log_generations`
and `iam_removed` — which are not objects a record writes but the state an
apply reads *before* it writes anything. They outlive the records that made
them: a removal below the trim floor has no record left on the log to prove it
happened, and `iam_removed` is what still says so.

Beside them sit the log's own three machinery tables — the operation ledger,
the deferred records and their scope index — which are local to each node,
excluded from the identity claim, and scrubbed out of any snapshot a node
donates to a joining peer.

### Two counters, and why there are two

Ending a session is a **counter moving forward**, never a row being deleted and
never two nodes comparing clocks. There are two of them, and they end different
sets of sessions:

- **A person's revocation epoch** (`iam_revocation_epochs`) ends every session
  *that person* holds. Signing out everywhere, a password change and detected
  token reuse all move it, and no capability is needed to move your own:
  gating it would make the fastest response to a stolen cookie the one that
  needs an administrator.
- **The fleet-wide session generation** (`iam_session_generation`) ends *every*
  session in the company. One row, about nobody, moved by
  `crewlet iam invalidate-all`, and it requires the administrative grant.

The second is not the first at a larger scale, and the difference is what a
**restore** does. Restoring rolls the identity estate back to the instant the
backup was taken, so a revocation performed after that instant is rolled back
with it and the session somebody revoked comes back. The fleet-wide generation
is the only number that can be pushed *forward* without knowing which sessions
were affected — every cookie in existence carries a value below the new one —
which is why bumping it is the restore runbook's last step.

Both are stated on the record rather than incremented by the applier. An
applier that did `+ 1` would fold over an arrival order, and two nodes at one
checkpoint have seen the same set of records in a different order.

### Two retention horizons, and one

`iam_history` is the only table here with **two** horizons, separated by a
`class` column the engine derives from the operation rather than taking from
the record:

- a **change** — who suspended this person, who granted this access — is an
  audit somebody asks a year later, and in several jurisdictions one they must
  be able to ask;
- a **session** — who signed in on Tuesday — answers an investigation that is
  days old, and is a record of a person's working hours for as long as it is
  kept.

Keeping the second as long as the first would be storing more about people than
there is a reason to. The operation ledger has **one** horizon, which is not a
contradiction: it answers "did my write land", and is measured against the
longest a client will retry.

The sweep that enforces them is a **record on the log**, not a local delete,
and it names a *position range* the publisher resolves once — so two nodes with
skewed clocks delete identical rows. It runs per **bucket**: the estate is
divided into 64 partitions by a hash of the person's id, so one horizon's worth
of deletions is 64 bounded transactions rather than one unbounded one.

### Removing somebody destroys a key, not a row

Deleting a person's row removes them from every node's current database and
from nothing else. The backups still hold them, the donated snapshots still
hold them, and the log — which is the only copy of what no node has applied
yet — holds the record that wrote them, byte for byte, for as long as
retention says. A removal that only deleted rows would be a promise the estate
cannot keep.

So each person's name and address are sealed under a key that is **theirs
alone**, kept in the company's sealed secret store, and removing them
**destroys that key**. Every copy of the ciphertext, wherever it already is,
becomes unreadable at once — nothing has to be found or rewritten.

The row and the id survive on purpose. The authentication trail names them, and
a history whose authors evaporate is not an audit trail: "who suspended this
person, and when" has to keep answering after they have gone.

Two consequences worth knowing before you see them:

- **A removal is durable before the key is destroyed.** The rows commit first;
  the key goes afterwards. The other order would destroy a key for a removal
  that then rolled back, and nothing could put it back. If the key deletion
  fails — a coordination outage — the person is removed everywhere and their
  key lives on, which is a state a duty finds and retries.
- **A value that will not decrypt is not the same as an outage.** A removed
  person's row reports itself as *shredded*; a decryption failure on somebody
  who has not been removed is a key-store problem. Rendering the second as the
  first would tell you somebody had been off-boarded who had not.

### A stalled identity log does not stop your agents

This is the first strictly-ordered domain whose health does **not** gate seat
admission, and the reason is worth knowing before you see the alarm.

An agent seat never reads this domain. A seat's principal is its own handle,
its authority is decided from the [org chart](chart-domain.md), and its work
arrives on its mailbox. So a node whose identity applier has stalled runs every
seat it holds exactly as correctly as a node that is current — and shedding a
company's seats because a human cannot sign in would be an outage caused by the
wrong subsystem, at the moment you most need the company still working while
you fix it.

What a stall gates instead is the request path, one request at a time: a node
that has not yet applied a revocation is designed to answer a session with
**503**, never 401. A 401 tells a browser to sign in again, and one stalled
applier would stampede your identity provider.

**A seats-only node does not run this domain at all.** It is the first domain
whose participation narrows on `node.roles`: ingress runs it because it serves
requests, workers runs it because it sweeps, and a satellite that only holds
seats runs neither — so it does not pay the disk, the applier or a share of
the stream budget to hold a directory of people it authenticates nobody
against. The corollary matters when you read a satellite's tables: `iam_people`
there is empty because the domain is not running, **not** because the company
has nobody, and every reader in this estate is three-valued for that reason.

### Sizing `stream.iam_log_max_bytes`

Like the org chart's, this ceiling is **not derived from your disk** — what it
grows with is your headcount and how often people sign in, and your volume has
nothing to say about either. Unlike the org chart's, it genuinely grows.

Sessions are what size it. People, credentials and invitations are hundreds of
records a year; a session writes one record when it opens and one when it
closes and nothing in between. Unset, it takes a flat **512 MiB**, which is
roughly eighteen months of a *completely blocked* trim at a pessimistic rate
for a company of a few hundred people, and about five years at a realistic one.

Set it down toward its **64 MiB** floor for a small company — the broker
reserves a stream's whole ceiling when it creates it, so this number is free
space a node needs before it can boot at all — and up for a large one. See
[Configuration](configuration.md) and [Retention](../guides/retention.md).

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

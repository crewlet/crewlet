# adr-505 — What `/config` may show, and what it must accept back

Status: **Accepted**
Related: `406` (the store is authoritative; the file is a seed),
`203` (there is no leader on the config path), `docs/concepts/secret-store.md`.

## The surface is guarded in full, reads included

Every other read on this API follows `allow_anonymous_read`. `/config` never
does. Reading it exposes the whole company document — the org chart, which
integrations are wired, and the shape of every credential — and writing it
changes the company. The auth package makes it the one prefix that is never
eligible.

## A write does not apply anything

`PUT /config` stores a revision and moves the activation pointer. It does not
touch the running epoch, not even on the node that served the request. Every
node, including that one, applies it on its own reconcile tick.

That is what makes a write on one node reach the whole fleet. The failure the
control plane exists to remove was a config change that only the process
handling the request ever saw; a write path that applied locally would put half
of it back.

## Redaction is driven by a struct tag, not a path list

A credential field carries `secret:"true"`, and the read surface masks by
walking the config with reflect.

A hand-maintained list of secret PATHS is maintained by whoever remembers it
exists. The day somebody adds `integrations.newthing.token`, the surface starts
publishing it and nothing fails. A tag lives on the field it describes, and a
guard test walks the config type failing on any field whose *name* says
credential and whose tag does not — with the exemptions (token *counts*, scope
*names*, a path on disk) written down rather than assumed. That guard found a
real one on its first run: the sandbox provider's API key was unmasked.

**A `${VAR}` reference is not masked.** It names a credential rather than being
one, the value it points at is never in this document — Tier B keeps references
verbatim and resolves them where a provider is constructed — and it is the half
an operator edits.

**An empty credential stays empty.** "No credential" and "a credential I could
not see" are opposite facts, and erasing the difference would let a round trip
turn the first into the second.

## Four collections are addressable on their own

`PUT /config/{roles|units|llm-providers|mcp-servers}/{id}`, and a
`config_entities` query for the read half. Not because CRUD is nice to have —
Python had a much larger version of this and none of it was ported — but
because the whole-document write makes every edit a company-wide one. A
founder changing one seat's goal sends back a document carrying every other
seat, every provider and every integration, and a concurrent edit anywhere in
it is theirs to lose. Editing one entity narrows what a write CLAIMS to have
changed, which is what makes the revision summary worth reading.

**It is the same write underneath**, and that is the part that matters: open
the active revision, splice the entity in, restore masks against that same
revision, validate the WHOLE document, store. A per-entity surface that
validated only the entity would be a machine for introducing exactly one bug —
a seat naming a provider that no longer exists — because the caller never sees
the rest of the document.

**It never creates.** An id nothing carries is a 404. Naming one that is not
there is far more often a typo than an intent to add a seat, and adding
through this route would grow the company without the caller ever seeing the
document they changed.

**It never renames**, and that one is load-bearing in a way the shape of the
route hides. A body whose own identity disagrees with the path is a 400. The
id here is not a label: a seat's durable id is a UUIDv5 over (company name,
handle), so a handle that changes under an operator re-derives the agent id
and strands that seat's diary, its onboarding marker and its counterparty
profiles behind an id nothing derives any more — its inbox subject with them.
A unit's or an MCP server's name is referenced by every `manages:`, `lead:`,
`unit:` and per-seat credential block naming it. None of that travels with a
splice, and the URL is left addressing something that is gone.

For a role the comparison is on the DERIVED handle, not the declared one,
because a body that omits `handle` derives it from `name` — which made
editing a seat's display name a rename nobody asked for, and it shipped that
way. Refused rather than coerced back to the path's id: keeping the old
identity silently would land every other edit in the body and leave the caller
believing the rename took, which is the same surprise one revision later.

Four collections rather than a general grammar, because these are the four the
dashboard's Config room edits. The room was written against them and shipped
calling a `config_entities` query nothing answered — every list came back
`unknown_query` and the editor was dead from the first day. Both sides' tests
passed. What links them now is a sweep that reads the rooms' own source for
`query("…")` calls and fails on any name this build does not register.

## Everything else is one PATCH, not one route per section

The four collections earn routes because their members have identities. Every
other section — the turn engine, learning, budgets, the notification knobs, the
integration blocks, mission and vision — is a singleton object, and Python
answered that with a route apiece: `identity`, `embeddings`, `turn-engine`,
`learning`, `budgets`, `integrations/{kind}`. None were ported, and the
decision here is that none will be.

`PATCH /config` is the general narrower form. It is an RFC 7396 merge patch
whose shape IS the shape of the document, so one route covers every section and
a section added to the config needs nothing added here — where a list of
per-section routes is a list that falls behind the models, silently, because
nothing fails when a new section has no route.

**The replace semantics are not what is lost with them.** A merge patch nulls
individual keys and sets others in the same object, so "replace this section,
dropping these keys" is one request, and `{"learning": null}` resets a whole
section — an absent section gets the same defaults as an authored one, on both
readers, which is what makes deleting it a real operation rather than a way to
zero the company. What a merge patch genuinely cannot express is a *blind*
replace: "make section X exactly this, without my having to know what is in
it". That is the lost-update pattern the compare-and-set below exists to
remove, scoped to one section. Not having it is the point.

## Why not a patch format that CAN address a list member

The argument above — arrays replace, so editing one seat needs a route — is an
argument about RFC 7396 specifically, and it does not survive the obvious
reply: *then use a patch format that addresses list members.* Two exist, and
this record did not evaluate either, so it does so here.

**RFC 6902 (JSON Patch)** addresses array elements, but positionally only. RFC
6901 §4 admits digits and the append sentinel `-` and nothing else — "if an
array is referenced with a non-numeric token, an error condition will be
raised." Its `test` op supplies the guard a positional path needs, and a failed
op aborts the whole document, so a stale index fails rather than corrupting.

**Strategic merge patch** (Kubernetes) is the format built for exactly this: a
`patchMergeKey` tells the server to merge a list by a member's key rather than
replacing it. It is not a standard, it needs schema metadata published to
clients, it grew four in-band directives to express deletion, ordering,
primitive lists and unions, and Kubernetes has since superseded it with
server-side apply.

**Neither replaces the entity routes, and the reason is not about formats.**
A patch document addresses by STRUCTURE. This config addresses a seat by
IDENTITY, and the two are deliberately different namespaces. In the shipped
Nimbus example the eight seats sit at eight different structural paths —
`/roles/0`, `/units/0/children/0/roles/0`, `/units/2/children/0/roles/2` — and
every one of them is `PUT /config/roles/{handle}`, because `eachRole` walks the
whole tree and a handle is unique across it. A seat's durable id is a UUIDv5
over (company name, handle); WHERE it sits in the chart is not part of who it
is, and an operator editing "the CTO" does not know or care which unit chain
it hangs from. JSON Patch would make the caller write the chain and re-derive
it whenever the chart is reorganised; strategic merge patch would let the
caller key each LEVEL by name but still walk `units → children → roles` to
reach the seat. Both turn a flat namespace into a structural one, which is the
capability the entity route exists to provide rather than an ergonomic
preference.

**What the comparison genuinely costs us**, recorded because it is the half an
argument like this usually omits:

- `llm-providers` is a MAP, so `PATCH /config` already addresses one provider
  by key today, deep-merging it. That route earns its place on replace
  semantics and on being uniform with the other three, not on addressability —
  it is the one of the four a redesign could drop.
- A patch sends only the fields it changes, so it never sends a mask back and
  needs no mask restore at all. The entity routes re-send a whole entity, which
  is why `RestoreRedacted` exists — and that mechanism is where a reorder was
  found handing each seat its neighbour's credentials. A narrower write is a
  smaller attack surface for that entire class of fault.
- One route covers every future collection; four routes are four, and a fifth
  collection needs a fifth.

The call is to keep both, for the reason above rather than the one this record
originally gave: the entity routes provide identity addressing over a nested
document, which no patch format provides at any price, and `PATCH /config`
covers everything whose shape IS its address.

## The surface answers to HTTP's own vocabulary

Four gaps between what this surface did and what a caller who knows HTTP would
expect, each closed against the specification rather than against taste.

**An entity path serves GET.** The four paths took a write and answered 405 to
a read, so the entity a caller was expected to send back had to be fetched from
`/query/config_entities` — a different URI space, answering a `{kind, id,
entity}` envelope that `PUT` does not accept. The published loop bridged the
two with `jq '.entity'`. A URI that can be written and not read is not a
resource, and the read half living somewhere else is what made the round trip
need a translation step. `GET /config/{kind}/{id}` now answers the entity
itself, redacted, so `GET | PUT` round-trips. The query keeps its envelope:
there the kind and id are the answer to a question, not the resource.

**Reads carry an entity-tag.** `If-Match` was accepted long before anything
emitted a validator, so the only way to learn the token was to read a DIFFERENT
resource — `/config/revisions`. A conditional request whose precondition cannot
be discovered from the representation it guards is a feature nobody uses
correctly. The tag is the revision id, quoted, and strong (RFC 9110 §8.8.3): a
revision is immutable, so there is nothing weak about the correspondence.
`If-None-Match` on a read is a 304.

**The preconditions mean what RFC 9110 §13.1 says.** `If-Match: *` was compared
to the revision id as a literal, so the wildcard could never equal it and the
ordinary way to say "only if something is there" answered 409. `If-Match: none`
is the mirror: this record's own documentation named it as the unconfigured
case and nothing implemented it, so it fell into the no-revision branch and
answered 412 on exactly the node it was meant to permit. `*` and the standard
`If-None-Match: *` now behave as specified, and `none` is honoured as the
pre-tag spelling because the documentation promised it.

**A patch format this resource does not speak is refused as a format.** RFC
5789 makes the patch document negotiable — 415 for one the server cannot read,
`Accept-Patch` for what it can. An RFC 6902 document is a LIST of operations,
and a merge patch that is not an object replaces the target outright, so one
arriving here was refused as a MALFORMED MERGE PATCH: the caller was told their
shape was wrong when their format was. `OPTIONS /config` advertises
`application/merge-patch+json`, an unsupported type is 415 carrying the same
header, and `application/json` and an absent type stay acceptable because every
example this project has published sends one of those.

The bare revision id is still accepted wherever an entity-tag is. Breaking
every script that reads an id out of a write response, to add two quotes, would
be a cost with nothing on the other side.

## The masked document must be able to come back

Python's answer was to document that the read is not round-trippable and point
at a CLI export. A document that cannot be sent back is a document nobody can
edit through the API at all — and the failure mode is severe: fetch the config,
change one line, send it back, and every credential in the company becomes the
literal mask. Silently, discovered when each integration stops authenticating.

So `PUT` resolves masks against the previous revision before validating:

- A field the caller **actually changed** keeps their value — rotation through
  this surface has to work.
- A field they **cleared** stays cleared — removing a credential is a real
  operation.
- A field still holding the mask is filled from the revision it was read from.

Validation runs **after** that resolution. Validating first would reject a
document for carrying `__redacted__` in a field the operator never touched.

## A reshaped list is refused rather than guessed

Lists correspond by position and nothing else. A caller who added or removed an
API key has changed which slot means what, so resolving masks by position would
write one credential into another's place — and the result authenticates as the
*wrong account* rather than failing.

The restore refuses. What was missing until mutation testing pointed at it: the
refusal then has to be *reported*. A document still holding the marker now fails
validation with a message saying what happened, so the caller gets a 400 instead
of a stored config that hands a provider the literal string `__redacted__` and
fails hours later with an error naming nothing.

## The read must be accepted by the writer

Found the same way: `GET /config` emitted `providers.llm_order` and `PUT`
refused it as an unknown setting. The round trip was broken for every config
with providers, which is all of them.

`llm_order` is the declaration order of a Go map. It exists in the stored form
precisely because Go marshals a map with **sorted** keys, so the serialized
document says `alpha, mike, zulu` while the company means `zulu, alpha, mike` —
and per-phase resolution's last resort is "the first provider configured". It
is now a readable field, an explicitly stated order wins over the document's key
order, and both readers accept it.

The general rule this is an instance of: **a read surface that its own writer
rejects is not a surface.**

## Two readers, deliberately

- `ParseCompany` — the authored form. Fails closed on an unknown field, because
  a typo in a document a person wrote is a mistake to catch at the door. This is
  what `PUT` uses.
- `DecodeCompany` — the stored form. Lenient about unknown fields, because an
  unrecognised key in a stored revision is a peer running a newer build, and
  rejecting it makes a mixed-version fleet an outage in the older direction.

`ParseCompanyDocument` is the authored reader with validation deferred, and it
exists for exactly one caller: the write path, which cannot validate until the
masks are resolved.

## The diff compares documents, not lines

A line diff is the wrong tool here: the stored form is JSON produced by
marshalling a struct, so re-ordering a map or adding a defaulted field rewrites
lines that mean nothing. The question an operator asks is "what changed about
the company", which is answered by paths and values.

- Both sides are **redacted before comparison**. Diffing raw documents would put
  the old *and* the new value of a rotated credential in one response — strictly
  worse than the read this surface already refuses to serve.
- A subtree that exists on one side only is **one change**, not a change per
  leaf, so a first import reads as "providers: added" rather than several
  hundred lines saying it.
- Lists correspond **by position**, with changes reported inside the element:
  `roles[0].handle`, not "roles[0] replaced".
- A field that carries `omitempty` and was turned off reads as **removed**, with
  its previous value on the `from` side. Surprising and correct — the stored
  document genuinely does not contain the key — and pinned by a test so it stays
  a decision rather than something later "fixed" by diffing structs and losing
  the property that every path is a field the operator wrote.
- The change list is **capped and says so**. A wholesale rewrite produces one
  change per leaf, and a diff that quietly stopped would read as "that is all
  that changed".

## Revert writes forward

`POST /config/revisions/{id}/revert` creates a NEW revision carrying the old
document. The pointer never moves backwards: the history stays append-only, so
"we reverted at 04:12" is a fact somebody can find later, and the epoch keeps
advancing, which is what makes every node reconcile onto it.

It **opens** the target rather than copying its bytes. A revision sealed under a
key no longer in the keyring cannot be reverted to, and finding that out now
beats activating a document every node will fail to read.

## The activation is a compare-and-set, and `If-Match` is the early half

There is no leader on this surface — any node's API may write, and the
coordination KV is the shared truth (`adr-203`). So two operators editing one
company at the same moment is a real race, and it used to be a last-writer-wins
race that silently discarded one of them: the pointer moved with a plain put,
both callers got a 201, and the later write won. A lost edit with a success in
the loser's hand and nothing anywhere to find it by.

Every write here already reads the active revision, derives from it, and names
it as the new revision's parent. That parent is now the **expectation the
pointer flip compares against**, so a write that lost is refused with `409
revision_advanced` naming what won — **whether or not the caller sent
`If-Match`**, because the server knows what it read. The KV does the
serializing; there is still no elected process, and none is needed.

`If-Match` remains, optional, and still worth sending: it is checked when the
request arrives, before a document is built, masks are resolved, validation
runs and a revision is stored, so a caller editing a revision that has already
moved is told so without the work. An `If-Match` sent when there is no active
revision is a 412 rather than a silent success, because a client should not
believe it won a race that was never run.

**The loser's revision is kept.** By the time the pointer is compared it has
already been stored — so it stays, valid and inert, named in the 409 as
`stored_revision_id`, and the node that stored it adopts the winner at its next
reconcile. Unwinding would mean a second write that can itself fail, on the
path where something has already gone wrong.

**An unset pointer is not a race.** The comparison is "if a pointer exists it
must still name this"; with none there is no winner to have lost to. Refusing
there looks defensible and breaks a state nodes reach constantly — one seeded
from a file holds a locally-active revision before it has published anything,
and every config write on it would 409 until it did. The boot publish is
unconditional for the same reason: a node offering the revision it holds is
asserting, not editing, and two nodes booting at once are both legitimate.

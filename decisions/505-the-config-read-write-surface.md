# d-505 — What `/config` may show, and what it must accept back

**Status:** decided
Related: `406` (the store is authoritative; the file is a seed),
`docs/concepts/secret-store.md`.

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

Four collections rather than a general grammar, because these are the four the
dashboard's Config room edits. The room was written against them and shipped
calling a `config_entities` query nothing answered — every list came back
`unknown_query` and the editor was dead from the first day. Both sides' tests
passed. What links them now is a sweep that reads the rooms' own source for
`query("…")` calls and fails on any name this build does not register.

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

## If-Match

Two operators editing one company through a full-document PUT is a
last-writer-wins race that silently discards one of them. `If-Match` turns it
into a 409 naming the revision to re-read. It is optional, because a first
import has nothing to match and a script that owns the config outright has no
race to lose — and an `If-Match` sent when there is no active revision is a 412
rather than a silent success, because a client should not believe it won a race
that was never run.

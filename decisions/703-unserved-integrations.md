# d-703 — An integration this build cannot serve is refused

Status: **decided by the project owner, implemented, and now fully unwound.**
One row has since been dropped rather than served — see
[`d-705`](705-dropping-plane.md), which removes Plane and with it the
Confluence-XOR-Plane single-homing rule this document records coming back.
Every vendor it refused is served, so the mechanism it describes — the
`unservedIntegrations` table, `refuseUnserved`, the `ErrUnimplemented`
sentinel and the `js:"unimplemented"` schema directive — is **deleted**.
What outlives it is one rule, stated at the bottom.
Implementation: none, deliberately · Answers the question
[`d-701`](701-vendor-order.md) left open · Kept because the shape it
describes is the one to rebuild if a vendor is ever ahead of its parser
again.

## The question d-701 left open

d-701 chose the vendor order and closed with: *"What this does not decide:
whether a SaaS vendor ships in v1 at all. That is a product call and stays
open."*

It has been answered twice, and the second answer is the one that stands.

**First:** they do not ship, and the config says so — which is what the rest
of this document describes and what the code did.

**Then the project owner reversed it.** All four had working implementations
in the Python engine that this build replaced, and dropping them was a
regression rather than a scoping decision. So they shipped, one commit each,
and this document became the record of an interim state.

## The vendors, and what shipped

| Vendor | State |
|---|---|
| `integrations.jira` | **Served.** Parser, prompt, client, seat-identity resolution, lead map and `crewlet jira provision`. `integrations.forge_app_id` came back with it: Jira Cloud is what rides the Forge route. `role.integrations.jira` and `unit.integrations.jira` are consulted again — they are the lead-fallback map. |
| `integrations.slack` | **Served.** Parser, prompt, transport, per-seat app identities, thread follows, a text-carrying working indicator and `crewlet slack provision` — manifest CRUD, the OAuth install, and the app ledger. `role.integrations.slack` is a seat's own app again, and both its credentials are now required together. |
| `integrations.confluence` | **Served.** Parser, prompt, client, a `knowledge.Searcher` over live CQL, the tool-skill codec and walk, and `crewlet confluence import`. `knowledge.confluence_spaces` is its read scope again, and `role`/`unit.integrations.confluence` its write and routing home. The single-homing rule came back with it: Confluence XOR Plane. Page-subscription routing came back later and differently — see [d-704](704-confluence-page-subscriptions.md), which also records how its absence went unnoticed under this row. |
| `integrations.github` | **Served.** Parser, prompt, client, participant fan-out, derived seat logins and `crewlet github provision` — organization or per-repository webhooks, and a report of which account each seat's own credential authenticates as. `url` is optional, because github.com serves its API from a different host and needs no address. |

## What the state actually was

The engine wired exactly three vendors. `startNotifications` built its
parser and prompt lists from `integrations.mattermost`, `integrations.gitlab`
and `integrations.plane`, and from nothing else.

The other four — `jira`, `confluence`, `slack`, `github` — had config models,
validation, generated schema, inbound webhook routes and verifiers, and no
parser, transport or searcher behind any of them. A company naming one got a
block that validated, appeared on the dashboard's Integrations room beside
the working ones, and routed nothing.

Silently, which is the part that mattered. `integrations.confluence` is how
an operator says where the company's knowledge lives; the answer was an empty
`## Relevant knowledge` section on every Plan phase, with nothing anywhere
saying why. `integrations.github` bought a webhook endpoint that verified
GitHub's signatures correctly and woke nobody.

## The decision, as it stood

Config validation refused each block by name, with an `ErrUnimplemented`
sentinel and a message saying what served that role instead. The per-seat and
per-unit rows were refused alongside the org-level blocks — not because they
are credentials, but because they are **where a seat or unit files work and
where its deliveries route**. An operator writes `confluence: {space: ENG}`
to say this unit owns ENG, and the identity was recorded, rendered on the
dashboard, and never consulted. Refusing only the org-level block would have
left the same silence one level down, in a smaller place.

That is also why they came back **with** their vendor rather than after it:
`jira: {project: ENG}` is read on every unrouted issue, so it had to be live
in the commit that made it matter.

`ErrUnimplemented` was its own sentinel rather than `ErrUnknownValue` or
`ErrConflict`, because it was the one failure that is **not the operator's
mistake**: the config is well-formed and names a real capability, and the
engine simply has no code behind it. An error that blamed the author would
have been the wrong story.

A **disabled** block was untouched throughout. `github: {enabled: false}` is
an operator who already agrees, and failing their config would be pedantry.
That rule survives the refusal in a different form: a disabled block still
skips its own required-field checks, because those are requirements of
*running* an integration.

## Three consequences that had to be handled

**The generated schema refused them too — and only where the validator did.**
A JSON Schema that accepts what the validator rejects is worse than no
schema: an editor blesses a config the engine will not boot on. But the
converse is worse still, because it is the direction an author *sees*. A
schema that red-underlines a working file teaches them to ignore it, and
then it catches nothing at all.

So `js:"unimplemented"` did not emit a blanket refusal of the key. It read
the field's own notion of "on", the same way the validators do: a block
carrying an `enabled` flag was refused only when that flag was true, any
other block by its mere presence, and a scalar or a list when it held
something.

It did not, first time out. The generator emitted `{"not": {}}` for every
tagged field while the validator keyed on `Enabled`, so the two disagreed
about `github: {enabled: false}` in both directions at once — and the parity
table, hand-written on both sides, had no case for it. What held them
together afterwards was a sweep that DERIVED the field list from the models:
a field tagged `unimplemented` with no pair of documents behind it failed the
suite, and a shape the generator had no rule for failed generation rather
than guessing. That sweep is what announced the end: with the last vendor
served it found nothing to certify, and said so rather than passing
vacuously.

**The webhook routes stayed, and failed closed.** They remained registered,
and with no config able to supply a secret they answered 503 — the same
answer the edge gives any route with nothing to verify with. Deleting them
would have thrown away correct, tested work (the Forge JWT verification
against Atlassian's published keys is real engineering) that comes straight
back when a vendor ships.

That prediction is what actually happened, four times, and it is the
strongest argument for the shape:

- `POST /webhooks/jira` and `POST /webhooks/forge` came alive in the commit
  that shipped the Jira parser, with no change to either route beyond the
  delivery-id key Jira had been sending all along.
- `POST /webhooks/slack/{handle}` and its OAuth landing came alive with **no
  change to the route at all** — including the `url_verification` exemption,
  written and tested against a vendor nothing could yet route.
- `POST /webhooks/confluence` came alive with no change whatsoever.
- `POST /webhooks/github` came alive with no change whatsoever: the HMAC
  verifier, the `X-GitHub-Delivery` dedupe key and the captured
  `X-GitHub-Event` header were all already correct, and the last of those is
  load-bearing — GitHub puts the event name in a header and only the action
  in the body, so a parser reading the body alone cannot tell an issue
  comment from a review comment.

**MCP was untouched.** Agents reached Jira, Confluence, GitHub and Slack
through their MCP servers throughout, which is a different surface entirely:
`mcp_servers` plus each seat's `mcp_env`. What was refused was the
inbound-routing config, and the integration pages said so, because "GitHub is
refused" would otherwise read as "agents cannot use GitHub".

## What the mechanism cost, and why it is gone

The table cost four commits' worth of test fixtures written around a refusal
and rewritten when it lifted, plus a schema directive, a sentinel, and a
derived sweep to hold the two layers together. That was the right price while
four vendors were ahead of their parsers.

It is gone now because **an empty table is not a dormant capability, it is
dead code**: a loop over nothing, a directive no field sets, a sentinel
nothing returns, and a sweep certifying an empty set. Each would have to be
read and re-understood by every future contributor to conclude, correctly,
that it does nothing. This document is the cheaper form of the same
knowledge, and `git log` holds the implementation for whoever needs it back.

If a vendor is ever ahead of its parser again, rebuild this shape rather than
inventing another: refuse the block by name in `Integrations.validate` with
its own sentinel, mirror the refusal in the generated schema keyed on the
field's own notion of "on", leave the webhook route registered and failing
closed, and delete the entry in the commit that ships the parser.

## The rule that outlives it

**A config field ships with the code that reads it.**

Not before — because a field the engine accepts and never reads is exactly
the silence this decision exists to end, and it is invisible from every
operator surface there is: the config validates, the dashboard renders it,
the webhook verifies, and nothing happens.

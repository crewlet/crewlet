# d-703 — An integration this build cannot serve is refused

Status: **decided by the project owner, implemented — and being unwound,
vendor by vendor.** The refusal was never the destination: each entry is
deleted by the change that ships that vendor. Jira, Slack and Confluence are
done; GitHub is the last one left.
Implementation: `internal/config/integrations.go`, `internal/config/roles.go`,
`internal/config/company.go`, `internal/config/schema.go` ·
Answers the question [`d-701`](701-vendor-order.md) left open.

## The question d-701 left open

d-701 chose the vendor order and closed with: *"What this does not decide:
whether a SaaS vendor ships in v1 at all. That is a product call and stays
open."*

It has been answered twice, and the second answer is the one that stands.

**First:** they do not ship, and the config says so — which is what the rest
of this document describes and what the code did.

**Then the project owner reversed it.** All four had working implementations
in the Python engine that this build replaced, and dropping them was a
regression rather than a scoping decision. So they ship, and this document
becomes the record of an interim state and of the ONE rule that outlives it:
**a config field ships with the code that reads it.** A vendor's row leaves
`unservedIntegrations` in the same commit that gives it a parser — not
before, because a field the engine accepts and never reads is exactly the
silence this decision exists to end.

### What has shipped since

| Vendor | State |
|---|---|
| `integrations.jira` | **Served.** Parser, prompt, client, seat-identity resolution, lead map and `crewlet jira provision`. `integrations.forge_app_id` came back with it: Jira Cloud is what rides the Forge route. `role.integrations.jira` and `unit.integrations.jira` are consulted again — they are the lead-fallback map. |
| `integrations.slack` | **Served.** Parser, prompt, transport, per-seat app identities, thread follows, a text-carrying working indicator and `crewlet slack provision` — manifest CRUD, the OAuth install, and the app ledger. `role.integrations.slack` is a seat's own app again, and both its credentials are now required together. |
| `integrations.confluence` | **Served.** Parser, prompt, client, a `knowledge.Searcher` over live CQL, the tool-skill codec and walk, and `crewlet confluence import`. `knowledge.confluence_spaces` is its read scope again, and `role`/`unit.integrations.confluence` its write and routing home. The single-homing rule came back with it: Confluence XOR Plane. |
| `integrations.github` | Refused when enabled. |

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

## The decision

Config validation refuses each of them by name, with `ErrUnimplemented` and a
message saying what serves that role instead:

| Refused | Because | Instead |
|---|---|---|
| ~~`integrations.jira`~~ | *served — see the table above* | — |
| ~~`integrations.confluence`~~ | *served — see the table above* | — |
| ~~`integrations.slack`~~ | *served — see the table above* | — |
| `integrations.github` (enabled) | no parser | `integrations.gitlab` |
| ~~`integrations.forge_app_id`~~ | *served — Jira Cloud rides it* | — |
| ~~`role.integrations.slack`~~ | *served — a seat's own app* | — |
| ~~`role.integrations.jira`, `unit.integrations.jira`~~ | *served — the lead-fallback map* | — |
| ~~`role.integrations.confluence`, `unit.integrations.confluence`~~ | *served — where a team writes* | — |
| ~~`knowledge.confluence_spaces`~~ | *served — the read scope* | — |

The per-seat and per-unit rows are not credentials — they are WHERE a seat or
unit files work and where its deliveries route. That is precisely why they
could not be left standing: an operator writes `confluence: {space: ENG}` to
say this unit owns ENG, and the identity is recorded, rendered on the
dashboard, and never consulted. Refusing only the org-level block would have
left the same silence one level down, in a smaller place. And it is why they
come back with their vendor rather than after it: `jira: {project: ENG}` is
now read on every unrouted issue.

`ErrUnimplemented` is its own sentinel rather than reusing `ErrUnknownValue`
or `ErrConflict`, because it is the one failure that is **not the operator's
mistake**: the config is well-formed and names a real capability, and the
engine simply has no code behind it. An error that blamed the author would be
the wrong story.

A **disabled** block is untouched. `github: {enabled: false}` is an operator
who already agrees, and failing their config would be pedantry.

## Three consequences that had to be handled

**The generated schema refuses them too — and only where the validator does.**
A JSON Schema that accepted what the validator rejects is worse than no
schema: an editor blesses a config the engine will not boot on. But the
converse is worse still, because it is the direction an author *sees*. A
schema that red-underlines a working file teaches them to ignore it, and
then it catches nothing at all.

So `js:"unimplemented"` does not emit a blanket refusal of the key. It reads
the field's own notion of "on", the same way the validators do: a block
carrying an `enabled` flag is refused only when that flag is true, any other
block is refused by its mere presence, and a scalar or a list is refused when
it holds something. `integrations.github: {enabled: false}` and
`knowledge.confluence_spaces: []` are configs the engine runs, so the schema
accepts them.

It did not, first time out. The generator emitted `{"not": {}}` for every
tagged field while the validator keyed on `Enabled`, so the two disagreed
about `github: {enabled: false}` in both directions at once — and the parity
table, hand-written on both sides, had no case for it. What holds them
together now is a sweep that DERIVES the field list from the models: a field
tagged `unimplemented` with no pair of documents behind it fails the suite,
and a shape the generator has no rule for fails generation rather than
guessing.

**The webhook routes stay, and fail closed.** They are still registered, and
with no config able to supply a secret they answer 503 — the same answer the
edge already gives a route with nothing to verify with, for the same reason.
Deleting them would throw away correct, tested work (the Forge JWT
verification against Atlassian's published keys is real engineering) that
comes straight back when a vendor ships. They are marked inert in the API
reference so nobody reads a live endpoint into them.

That prediction is what actually happened, twice, and it is the strongest
argument for the shape. `POST /webhooks/jira` and `POST /webhooks/forge` came
alive in the commit that shipped the Jira parser, with no change to either
route beyond the delivery-id key Jira had been sending all along. `POST
/webhooks/slack/{handle}` and its OAuth landing came alive in the Slack
commit with **no change to the route at all** — including the
url_verification exemption, which had been written and tested against a
vendor nothing could yet route.

**MCP is untouched.** Agents still reach Jira, Confluence, GitHub and Slack
through their MCP servers, which is a different surface entirely: `mcp_servers`
plus each seat's `mcp_env`. What is refused is the inbound-routing config, and
the integration pages say so, because "GitHub is refused" would otherwise read
as "agents cannot use GitHub".

## What undoes an entry

The change that ships that vendor's parser, transport and — for Confluence —
searcher, deletes its row from `unservedIntegrations` in the same commit. A
config field ships with the code that reads it, which is the rule this
document exists to enforce and the one it was written because nobody had.

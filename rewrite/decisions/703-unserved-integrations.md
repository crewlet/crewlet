# d-703 — An integration this build cannot serve is refused

Status: **decided by the project owner, implemented.** Phase: 7 ·
Implementation: `internal/config/integrations.go`, `internal/config/roles.go`,
`internal/config/company.go`, `internal/config/schema.go` ·
Answers the question [`d-701`](701-vendor-order.md) left open.

## The question d-701 left open

d-701 chose the vendor order and closed with: *"What this does not decide:
whether a SaaS vendor ships in v1 at all. That is a product call and stays
open."*

It has been answered: they do not, and the config says so.

## What the state actually was

The engine wires exactly three vendors. `startNotifications` builds its
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
| `integrations.jira` | no parser | `integrations.plane` |
| `integrations.confluence` | no parser, no searcher | `integrations.plane` |
| `integrations.slack` | no parser | `integrations.mattermost` |
| `integrations.github` (enabled) | no parser | `integrations.gitlab` |
| `integrations.forge_app_id` | carries Jira and Confluence Cloud only | — |
| `role.integrations.slack` | no parser | `role.integrations.mattermost` |
| `role.integrations.jira`, `unit.integrations.jira` | no parser | `…integrations.plane` |
| `role.integrations.confluence`, `unit.integrations.confluence` | no parser, no searcher | `…integrations.plane` |
| `knowledge.confluence_spaces` | a scope for a backend with no searcher | `knowledge.plane_projects` |

The last three rows are not credentials — they are WHERE a seat or unit files
work and where its deliveries route. That is precisely why they could not be
left standing: an operator writes `jira: {project: ENG}` to say this unit owns
ENG, and the identity is recorded, rendered on the dashboard, and never
consulted. Refusing only the org-level block would have left the same silence
one level down, in a smaller place.

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

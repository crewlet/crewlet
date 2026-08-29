# d-705 — Plane is dropped, and the knowledge seam is not

**Status:** decided and implemented · Supersedes the vendor half of
[`d-701`](701-vendor-order.md) · Removes the last row
[`d-703`](703-unserved-integrations.md) tracked

## The question

Plane is gone. What that costs, and — the part worth writing down — what it
must NOT be allowed to simplify.

## Why the removal was structural rather than mechanical

Every other vendor fills one role. Plane filled **two**, which is exactly what
[`d-701`](701-vendor-order.md) chose it for: it was the tracker AND the
knowledge base, and the engine was built around that pair.

So `internal/plane` was not a leaf. It supplied:

| What | Who consumed it |
|---|---|
| A webhook parser and prompt | the inbound notification service |
| One of two `knowledge.Searcher` implementations | the Plan-phase `## Relevant knowledge` prefetch, the onboarding hint, `CanSearch` |
| The tool-skill page source and its live reindex | `agent/skills`, on a page webhook |
| A `learning.PromotionWriter` | the cross-agent skill-promotion pass |
| A `crewlet plane provision \| import \| resync` CLI | operators |

Deleting the package therefore forced a decision at four seams at once, and
three of them had been written as *two-armed switches* — a shape that only
exists because there were two backends.

## What collapsed, and what deliberately did not

**Collapsed.** `Engine.Knowledge` was a two-branch lookup and is now one.
`Engine.SkillsProject` was a precedence-free "whichever backend is wired" and
is now the Confluence field. `promotionBackend` was a three-valued enum
(`none` / `confluence` / `plane`) whose whole job was picking which of a unit's
two identity fields held the draft container; with one backend the question
"which vendor" disappears and only "is a knowledge base wired at all" remains,
so it is a bool. `companyRules()` lost its only expressible cross-field JSON
Schema rule and now returns nothing.

**Not collapsed: `knowledge.Searcher` stays an interface.** It has exactly one
implementation and that is not a reason to inline it. The interface is declared
by its CONSUMERS — the prefetch, the onboarding hint, the promotion pass — so a
second backend is a new implementation rather than a rewrite of everything that
searches. A seam collapsed into its last backend is what makes the next one a
rewrite, and the cost of keeping it is one indirection nobody has to think
about.

The same reasoning keeps `Engine.SyncSkills` taking rendered pages rather than
a backend client, and keeps `learning.PromotionWriter` an interface with one
implementation. Both are how the tool-skill and promotion subsystems stay
ignorant of which product answered.

## The single-homing rule, restated

`d-703` recorded the rule coming back with Confluence: **the knowledge base is
Confluence XOR Plane**, because two searchers would make an agent's answer to
"what do we already know about this" depend on which one was asked, and neither
would be wrong.

With one backend that rule is **structural rather than enforced** — there is no
second searcher to conflict with — so the validator no longer refuses a pair.
What it still refuses is a *read scope naming a backend the config does not
configure*: `knowledge.confluence_spaces` with no `integrations.confluence`
reads as a working narrowing and narrows nothing, which is the same silence the
rule has always existed to end, one level down from the block itself.

## The removal is a clean break, by decision

An `integrations.plane:` block now fails with the ordinary unknown-field error
that strict decoding gives any misspelling (`config.Load` runs the YAML decoder
with `KnownFields(true)`). No named refusal was left behind.

That is the opposite of the shape `d-703` prescribes, and deliberately: that
shape exists for a vendor whose config is **ahead of its parser** — a field the
engine accepts and never reads, which is invisible from every operator surface.
This is the reverse case. The capability is gone, not pending, so the honest
answer is "that is not a setting" and the honest cost of a permanent named stub
is a piece of Plane knowledge in `internal/config` that nothing else reads.

## What outlives the vendor

- **[`d-503`](503-webhook-edge.md)'s payload dedupe.** Plane's per-*attempt*
  `X-Plane-Delivery` is the case that motivated hashing the raw body, and that
  passage stays: the rule still governs the Forge relay and the Atlassian Data
  Center routes whose identifier header moved between versions. A decision does
  not stop being the reason for the code when its motivating example leaves.
- **The bundled tool skills.** They were authored Plane-shaped and are now
  Atlassian-shaped; what did not change is that the engine ships **no** skill
  prose and the pages are the knowledge base's, not the binary's.
- **The rule `d-703` ends on.** A config field ships with the code that reads
  it. Its corollary is what this decision does: a field whose code is deleted
  goes in the same commit.

## What the reference company lost

`examples/nimbus.company.yaml` moves to Jira + Confluence, keeping Mattermost
and GitLab. Every subsystem it demonstrates still runs — routing, knowledge
search, tool-skill sync, skill promotion — but the **fully-local loop does
not**: Plane was the only tracker and knowledge base this repository could
stand up in `docker-compose.yml`, and Atlassian is not something a compose file
can create. The compose stack is now `gitlab` + `mattermost`, and the example's
tracker and wiki halves need an Atlassian site.

That is the real price of this removal, and it is worth naming rather than
discovering: a contributor who wants to exercise the knowledge path end to end
locally no longer can from this repository alone. Restoring it means either a
self-hostable knowledge backend behind the seam above — which is what keeping
the seam buys — or accepting that the knowledge half is tested against fakes
and a real instance, never a local one.

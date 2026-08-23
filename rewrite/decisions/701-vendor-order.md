# d-701 — Vendor order for v1

**Status:** DECIDED. Answerable from the repository; no operator input needed.

## The question

REWRITE_PLAN §12 leaves the Phase-7 vendor order open, with the constraint
"whatever the driving deployment uses goes first (the operator's chat + tracker
+ knowledge + code host)".

## What the repository already says

Three independent signals in the tree agree, and they name the same three
vendors:

1. **`examples/nimbus.company.yaml`** — the one worked example org — enables
   exactly `plane`, `mattermost` and `gitlab` under `integrations:`. Nothing
   else. Slack, Jira, Confluence and GitHub have config models and transports,
   but the example company does not use them.

2. **`docker-compose.yml`** ships profile-gated local services for exactly
   those three: `[gitlab]`, `[mattermost]`, `[plane]`. No other vendor has one,
   because no other vendor can be self-hosted for a local loop.

3. **`scripts/`** carries a bootstrap for each: `gitlab-dev-bootstrap.sh`,
   `plane-dev-bootstrap.sh`, `mattermost-dev-bootstrap.sh`. A vendor with a
   bootstrap script is one somebody stands up repeatedly.

The four the plan asks for map onto three vendors, because Plane fills two:

| Role         | Vendor     |
| ------------ | ---------- |
| chat         | Mattermost |
| tracker      | Plane      |
| knowledge    | Plane      |
| code host    | GitLab     |

Slack / Jira / Confluence / GitHub are the SaaS alternates for the same four
roles. They are not dropped — the seams are backend-neutral by construction —
they are simply not what the driving deployment runs, which is the plan's own
tie-breaker.

## The order

1. **The backend-neutral spine.** Everything the plan lists as "the 30% that
   carries over": the raw-webhook envelope and its single fleet-wide inbound
   group, org-derived party resolution, the self-action and own-app guards, the
   per-agent rate valve, the thread-follow model and its `MentionGrammar`,
   conversation coalescing with per-source supersede rules, the
   `WorkingStatusDriver`, the notification-prompt registry, and the
   `KnowledgeSearcher` contract.

   First because every vendor lands on it, and because a vendor built before
   the spine is a vendor whose shape then defines the spine — which is how a
   "backend-neutral" seam ends up with one backend's assumptions welded in.

2. **The three vendors, in parallel, parser before transport.** They share no
   code beyond the spine, so ordering them against each other buys nothing.
   Within each: the webhook parser first (a pure function with a rich suite —
   the plan's own note that these are highly delegable is right), then the
   transport, then the prompt.

3. **Provisioning CLIs and `doctor` trail the transports**, as the plan says:
   they are operator tooling, and an operator can provision by hand for as long
   as it takes.

## What this does not decide

Whether a SaaS vendor ships in v1 at all. That is a product call and stays open
— but it is not on the critical path, because a vendor added after the spine is
a parser, a transport and a prompt, which is the shape Phase 7 is built to
absorb.

# d-701 — Vendor order for v1

**Status:** decided; **partly superseded by [`d-705`](705-dropping-plane.md)**,
which drops Plane. The order below is the record of how v1 was built and why;
the vendor it was built around is no longer served. The reasoning stands
unedited — what a decision cost is not something a later reversal unmakes.

## The question

Which vendors v1 serves, and in what order, under the constraint that whatever
the driving deployment uses goes first — the operator's chat, tracker,
knowledge and code host.

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

Those four roles map onto three vendors, because Plane fills two:

| Role         | Vendor     |
| ------------ | ---------- |
| chat         | Mattermost |
| tracker      | Plane      |
| knowledge    | Plane      |
| code host    | GitLab     |

Slack / Jira / Confluence / GitHub are the SaaS alternates for the same four
roles. They are not dropped — the seams are backend-neutral by construction —
they are simply not what the driving deployment runs, which is the tie-breaker.

## The order

1. **The backend-neutral spine.** Everything no single vendor owns: the
   raw-webhook envelope and its single fleet-wide inbound group, org-derived
   party resolution, the self-action and own-app guards, the per-agent rate
   valve, the thread-follow model and its `MentionGrammar`, conversation
   coalescing with per-source supersede rules, the `WorkingStatusDriver`, the
   notification-prompt registry, and the `KnowledgeSearcher` contract.

   First because every vendor lands on it, and because a vendor built before
   the spine is a vendor whose shape then defines the spine — which is how a
   "backend-neutral" seam ends up with one backend's assumptions welded in.

2. **The three vendors, in parallel, parser before transport.** They share no
   code beyond the spine, so ordering them against each other buys nothing.
   Within each: the webhook parser first (a pure function with a rich suite,
   and the most delegable piece of the work), then the transport, then the
   prompt.

3. **Provisioning CLIs and `doctor` trail the transports**: they are operator
   tooling, and an operator can provision by hand for as long as it takes.

## What this does not decide

Whether a SaaS vendor ships in v1 at all. That was a product call, and
[`d-703`](703-unserved-integrations.md) carries its history: first they did
not, and config refused their blocks by name rather than accepting them and
routing nothing; then the project owner reversed it, and they ship one at a
time. What this decision settled — which vendor the engine is BUILT AROUND —
is unaffected either way. Plane is the tracker the knowledge base and the
skill sync are built on; Jira is a tracker the engine also routes.

It was never on the critical path, because a vendor added after the spine is a
parser, a transport and a prompt, which is the shape the vendor layer is built
to absorb — and shipping one also deletes its row from `unservedIntegrations`,
in the same commit.

---
key: skill:retrieval_research
trigger:
  any_of:
    - tool: query_episodes
    - mcp_server: plane
    - tool: refresh_memory
phases: [plan]
required: false
title: Re-querying memory & knowledge after recon
summary: |
  After recon (chat thread, tracker work item, counterparty lookup)
  changes the picture, re-query memory / knowledge / episodes.
---

The `## Personal memory`, `## Relevant knowledge`, and `## Similar prior work` blocks above were derived from the triggering message as it stood at turn start — before you had done any recon. After any tool call that materially changed your understanding of what this task is actually about (reading a chat thread, fetching a tracker work item, looking up a counterparty), re-query with a focused query — **even if a block already had entries** — because the turn-start search could not see what you have since learned. Skip the re-query only when the trigger was already self-contained and you did no further recon.

- `query_episodes` — re-search your own past turns (`## Similar prior work`); recovering a past plan is cheaper than re-deriving it.
- Your Plane page-search tools — re-search team docs (`## Relevant knowledge`: playbooks, runbooks, ADRs, conventions) with a focused keyword query; open a full page body with your page-read tools.
- `refresh_memory(context_hint=...)` — re-filter your `## Personal memory` with a 1-2 sentence summary of the enriched framing; repeated calls with the same hint are cached, distinct calls capped per turn.

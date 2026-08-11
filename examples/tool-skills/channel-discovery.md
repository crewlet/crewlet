---
key: skill:channel_discovery
trigger:
  any_of:
    - tool: slack_channels_list
    - tool: slack_conversations_add_message
    - tool: slack_users_search
phases: [plan, execute]
title: Choosing the right Slack channel / surface
summary: |
  Team channel from identity prompt for team-wide; DM individuals
  for direct asks; Plane work items / pages for cross-team when you
  lack Slack channel access. Discover via `slack_channels_list`.
---

# Default surfaces

| Audience | Surface |
|---|---|
| Your own team — announcement, blocker, status | Your team's Slack channel (in your identity prompt) |
| A specific individual (peer or manager) | DM them — resolve their handle via `slack_users_search` if you don't already know it |
| Work that already has a durable home (work item / page) | Comment on the Plane work item (or update the relevant page) — these don't require Slack channel membership |
| Tight-loop mechanical sync between agents | `a2a_ask` |

# Finding the right channel

Don't guess channel names — call `slack_channels_list` and read the results. The Slack MCP returns `name`, `topic`, and `purpose` for each channel your bot can see; **pick by topic / purpose, not name**, because channel names are often abbreviated, dated, or repurposed.

Typical patterns:

- `*-team`, `*-eng`, `*-pod` — team-scoped discussion
- `proj-*` — time-bounded project channel
- `inc-*` — incident response
- `#announcements`, `#standups` — read-only or low-traffic; don't post asks here

# When you can't post to a channel

Your bot can only post to channels it has been added to.  If `slack_channels_list` returns a channel but your subsequent `slack_conversations_add_message` call returns a permission error:

1. **DM the channel's owner / team lead instead.**  Use `slack_users_search` to find them by name or email.
2. **Comment on the relevant Plane work item.**  Work items have their own permission model and usually allow cross-team comments.
3. **Ask in your own team's channel for an introduction** — let a teammate who *is* in the target channel relay your question.

Do **not** broadcast in your own channel hoping the other team will see it — they almost certainly won't, and you'll spam your own teammates.

# Asking for ongoing access

If you genuinely need ongoing access to another team's channel (e.g. you're embedded on a cross-team project), that's a workspace-admin task — not something you can resolve at runtime.  Hand off to the founder via your usual manager-handoff surface (see the `getting-unstuck` skill) with a one-line ask: "please add me to #<channel> for the <project-name> work".

# Caveat — tool availability

The discovery flow above assumes the Slack server's `channels_list` and `users_search` tools are enabled in the operator's `SLACK_MCP_ENABLED_TOOLS` (that env var uses the server's **raw** names; the engine exposes them to you prefixed, as `slack_channels_list` / `slack_users_search`).  If they aren't enabled (or your MCP catalogue doesn't list them), skip discovery and stick to:

- known channels from your identity prompt,
- handles you've already resolved via `lookup_colleague`,
- Plane work items / pages as the cross-team fallback.

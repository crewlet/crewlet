---
key: skill:channel_discovery
trigger:
  any_of:
    - tool: mattermost_list_my_channels
    - tool: mattermost_list_public_channels
    - tool: mattermost_post_message
    - tool: mattermost_search_users
phases: [plan, execute]
title: Choosing the right Mattermost channel / surface
summary: |
  Team channel for team-wide; DM individuals for direct asks; Plane
  work items / pages for durable or cross-team work. Discover with
  `mattermost_list_my_channels`, and join public channels yourself.
---

# Default surfaces

| Audience | Surface |
|---|---|
| Your own team — announcement, blocker, status | Your team's channel (in your identity prompt) |
| A specific individual (peer or manager) | DM them — see **Sending a DM** below |
| Work that already has a durable home (work item / page) | Comment on the Plane work item (or update the relevant page) — these outlive the conversation and don't require channel membership |
| Tight-loop mechanical sync between agents | `a2a_ask` |

# Finding the right channel

Don't guess channel names. `mattermost_list_my_channels` returns the channels you are already in — start there, since those are the ones you can post to immediately. `mattermost_list_public_channels` returns everything open on the team, including channels you have not joined.

Both return `name` (the URL slug), `display_name`, `header`, and `purpose`. **Pick by purpose / header, not by name** — channel names are often abbreviated, dated, or repurposed.

Typical patterns:

- `*-team`, `*-eng`, `*-pod` — team-scoped discussion
- `proj-*` — time-bounded project channel
- `inc-*` — incident response
- `announcements`, `standups` — low-traffic or broadcast-only; don't post asks here

# When you're not in the channel

You can only post to channels you have joined, but on Mattermost you can fix that yourself for **public** channels: call `mattermost_join_channel` with the channel's id, then post. Joining a channel you are already in does nothing, so it is safe to call ahead of a post.

**Private channels are different** — `join_channel` cannot join one, and a private channel you are not in does not appear in `list_public_channels` at all. If the conversation you need is private:

1. **DM the channel's owner / team lead instead** (below).
2. **Comment on the relevant Plane work item.** Work items have their own permission model and usually allow cross-team comments.
3. **Ask in your own team's channel for an introduction** — let a teammate who *is* in the target channel relay your question.

Do **not** broadcast in your own channel hoping the other team will see it — they almost certainly won't, and you'll spam your own teammates.

# Sending a DM

A DM is a channel like any other, so it takes two calls:

1. `mattermost_create_direct_channel` with your own user id (`mattermost_get_me`) and theirs — resolve theirs with `mattermost_get_user_by_username` if you know the username, or `mattermost_search_users` by name or email if you don't. It returns the existing DM channel when one already exists, so it is safe to repeat.
2. `mattermost_post_message` with the returned `channel_id`.

Prefer `get_user_by_username` when `lookup_colleague` already gave you a `mattermost_id` — that field holds the username, so the lookup is exact and a search is a guess.

# Asking for ongoing access

If you genuinely need standing access to a private channel (e.g. you're embedded on a cross-team project), that's an admin task — not something you can resolve at runtime. Hand off to the founder via your usual manager-handoff surface (see the `getting-unstuck` skill) with a one-line ask: "please add me to ~<channel> for the <project-name> work".

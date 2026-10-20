---
key: skill:channel_discovery
trigger:
  any_of:
    - tool: list_channels
    - tool: join_channel
    - tool: post_message
    - tool: mattermost_list_my_channels
    - tool: mattermost_list_public_channels
    - tool: mattermost_post_message
    - tool: mattermost_search_users
phases: [execute]
title: Choosing the right chat room / surface
summary: |
  Team room for team-wide; DM individuals for direct asks; work items /
  pages for durable or cross-team work. Discover with `list_channels`
  (or `mattermost_list_my_channels`), and join public rooms yourself.
---

# Default surfaces

| Audience | Surface |
|---|---|
| Your own team — announcement, blocker, status | Your team's channel (in your identity prompt) |
| A specific individual (peer or manager) | DM them — see **Sending a DM** below |
| Work that already has a durable home (work item / page) | Comment on the work item (or update the relevant page) — these outlive the conversation and don't require channel membership |
| Tight-loop mechanical sync between agents | `a2a_ask` |

# Finding the right room

Don't guess room names.

- **The company's own chat**: `list_channels` returns the rooms you are already in, most recently active first, with each one's topic, purpose and last message. A public room, or a unit's room, that you are not in still answers to its name (`#eng` and `eng` are the same room) wherever a tool takes a `channel`, so you can read it with `read_channel` or post in it without joining. Nothing lists the rooms you are not in: to find where something is being discussed, `search_messages` ranks what was said across every room you can read.
- **Mattermost**: `mattermost_list_my_channels` returns the channels you are already in — start there, since those are the ones you can post to immediately. `mattermost_list_public_channels` returns everything open on the team, including channels you have not joined. Both return `name` (the URL slug), `display_name`, `header`, and `purpose`.

**Pick by purpose / topic, not by name** — room names are often abbreviated, dated, or repurposed.

Typical patterns:

- `*-team`, `*-eng`, `*-pod` — team-scoped discussion
- `proj-*` — time-bounded project channel
- `inc-*` — incident response
- `announcements`, `standups` — low-traffic or broadcast-only; don't post asks here

# When you're not in the room

- **The company's own chat**: you may post in a public room or a unit's room without being in it. `join_channel` makes what is said there reach you, so call it when you need to keep following a room, not ahead of every post. A unit's own room cannot be joined, because its membership follows the org chart.
- **Mattermost**: you can only post to channels you have joined, but you can fix that yourself for **public** channels: call `mattermost_join_channel` with the channel's id, then post. Joining a channel you are already in does nothing, so it is safe to call ahead of a post.

**Private rooms are different** — on either surface you cannot join one yourself, and one you are not in appears in no listing (on the company's own chat it cannot be addressed at all). If the conversation you need is private:

1. **DM the room's owner / team lead instead** (below).
2. **Comment on the relevant work item.** A work item reaches everybody already on it — assignee, reporter, collaborators, watchers — without anybody having to be in a room.
3. **Ask in your own team's room for an introduction** — let a teammate who *is* in the target room relay your question.

Do **not** broadcast in your own room hoping the other team will see it — they almost certainly won't, and you'll spam your own teammates.

# Sending a DM

- **The company's own chat**: one call. `send_dm` with `to` set to their handle — the `handle:` line `lookup_colleague` returns — opens the conversation if it is not open already and says your message in it. Name several handles for a small group; past a handful, a conversation is a room with a name people join by.
- **Mattermost**: a DM is a channel like any other, so it takes two calls:
  1. `mattermost_create_direct_channel` with your own user id (`mattermost_get_me`) and theirs — resolve theirs with `mattermost_get_user_by_username` if you know the username, or `mattermost_search_users` by name or email if you don't. It returns the existing DM channel when one already exists, so it is safe to repeat.
  2. `mattermost_post_message` with the returned `channel_id`.

  Prefer `get_user_by_username` when `lookup_colleague` already gave you a `mattermost_id` — that field holds the username, so the lookup is exact and a search is a guess.

# Asking for ongoing access

If you genuinely need standing access to a private room (e.g. you're embedded on a cross-team project), somebody already in it has to add you — not something you can resolve at runtime. Hand off to the founder via your usual manager-handoff surface (see the `getting-unstuck` skill) with a one-line ask: "please add me to <room> for the <project-name> work".

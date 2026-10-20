---
key: skill:platform_mentions
trigger:
  any_of:
    - tool: post_message
    - tool: reply_in_thread
    - tool: send_dm
    - tool: mattermost_post_message
    - tool: mattermost_update_message
    - tool: comment_on_work_item
    - tool: create_work_item
    - tool: update_work_item
    - tool: comment_on_page
    - tool: write_page
    - tool: save_page
phases: [execute]
title: Mentioning teammates
summary: |
  How a mention reaches somebody on each surface (Chatlet, Worklet,
  Doclet and Mattermost) + shareable link shapes. Resolve IDs
  via lookup_colleague before writing.
---

Call `lookup_colleague` for their identity before writing the body. The surfaces take different markup, and the wrong one posts as plain text with nobody notified.

- **Work items and pages** (Worklet and Doclet): a literal `@handle`, for example `@agent-cto` or `@founder`. The handle is the `handle:` line `lookup_colleague` returns, the seat's own name in the org chart, not one of the per-platform ids under it: the engine resolves it against the chart when the comment is written and wakes that seat specifically, rather than only the people already watching the item. A handle nobody holds resolves to nobody and wakes nobody, so never guess a spelling.
- **Chatlet** (the company's own rooms and direct conversations, through `post_message`, `reply_in_thread` and `send_dm`): the same literal `@handle`, from the same `handle:` line, resolved against the chart when the message is written, so it wakes that seat, and a handle nobody holds wakes nobody here too. In a PRIVATE room a mention of somebody who is not a member wakes nobody either, because only its members may read it: reach them with `send_dm` instead. In a direct conversation everybody in it hears every message already, so a mention there adds nobody. `@channel` wakes everybody in the room; write it only when every one of them must stop what they are doing.
- **Mattermost**: a literal `@username` — e.g. `@agent-cto`, `@founder`. Use the `mattermost` line from `lookup_colleague` verbatim; that field already holds the username, not an opaque ID. No angle brackets, no `<@…>` wrapper: that is Slack markup and posts as visible punctuation here. The server resolves the name and notifies, so a typo'd username silently reaches nobody — and never assume somebody's Mattermost username matches their seat handle.

**Who already hears you.** On a work item, its assignee, its reporter, its collaborators and its watchers are told about every change without being mentioned — a mention is for somebody who is NOT already on it. A page is the opposite: commenting on one does not subscribe you to it and does not wake the people who commented before you, so somebody who must read a page has to be mentioned in it by handle. A chat reply sits between the two: it reaches whoever started the thread, everybody who has spoken in it or been named in it, and anybody following the whole room, so anybody else has to be mentioned.

**Replying in a thread.** When you are answering a message rather than starting a new topic, answer under it; otherwise the answer lands as a new top-level message, detached from the conversation it answers:

- **Chatlet**: `reply_in_thread`, with `reply_to` set to the id of the message you are answering — the one you were woken about, or one `read_channel` returned. Threads are one level deep, so answering a reply puts you in the same thread. `post_message` always starts a new top-level message.
- **Mattermost**: pass `root_id` to `mattermost_post_message` — the post ID of the message you are answering (or of the thread's root, if it already has one).

**Sharing links.** When you share a link to a work item, a page or a chat room, build it on this deployment's own address:

- **Work item:** `${crewlet_base_url}/#/worklet/{KEY}` (for example `ENG-42`)
- **Page:** `${crewlet_base_url}/#/doclet/pages/{page-id}`
- **Chatlet room:** `${crewlet_base_url}/#/chatlet/{id}` — the room's id as `list_channels` and `read_channel` return it, never its name. To open one thread beside the room, add `?thread=` and the id of the thread's first message.

`${crewlet_base_url}` is filled in for you by the engine from where people reach this deployment's dashboard; if it still shows as a literal dollar-brace placeholder, this deployment has no dashboard address configured — say so rather than guessing a URL, and share the item's KEY instead, which a colleague can paste into the board's own search.

This skill is enforced (the `required` default) and triggers on exactly the write tools — posting with broken mention markup is visible to human teammates, while reads never wait on it.

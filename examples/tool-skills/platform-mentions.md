---
key: skill:platform_mentions
trigger:
  any_of:
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
  How a mention reaches somebody on each surface (the company's own
  tracker and pages, and Mattermost) + shareable link shapes. Resolve IDs
  via lookup_colleague before writing.
---

Call `lookup_colleague` for their identity before writing the body. The surfaces take different markup, and the wrong one posts as plain text with nobody notified.

- **Work items and pages** (the company's own tracker and knowledge base): a literal `@handle` — e.g. `@agent-cto`, `@founder`. The handle is the `handle:` line `lookup_colleague` returns — the seat's own name in the org chart, not one of the per-platform ids under it: the engine resolves it against the chart when the comment is written and wakes that seat specifically, rather than only the people already watching the item. A handle nobody holds resolves to nobody and wakes nobody, so never guess a spelling.
- **Mattermost**: a literal `@username` — e.g. `@agent-cto`, `@founder`. Use the `mattermost` line from `lookup_colleague` verbatim; that field already holds the username, not an opaque ID. No angle brackets, no `<@…>` wrapper: that is Slack markup and posts as visible punctuation here. The server resolves the name and notifies, so a typo'd username silently reaches nobody — and never assume somebody's Mattermost username matches their seat handle.

**Who already hears you.** On a work item, its assignee, its reporter, its collaborators and its watchers are told about every change without being mentioned — a mention is for somebody who is NOT already on it. A page is the opposite: commenting on one does not subscribe you to it and does not wake the people who commented before you, so somebody who must read a page has to be mentioned in it by handle.

**Replying in a thread.** When you are answering a message rather than starting a new topic, pass `root_id` to `mattermost_post_message` — the post ID of the message you are answering (or of the thread's root, if it already has one). A reply posted without `root_id` lands as a new top-level channel message, detached from the conversation it answers.

**Sharing links.** When you share a link to a work item or a page, build it on this deployment's own address:

- **Work item:** `${crewlet_base_url}/#/work/{KEY}` — e.g. `ENG-42`
- **Page:** `${crewlet_base_url}/#/pages/{page-id}`

`${crewlet_base_url}` is filled in for you by the engine from where this deployment answers; if it still shows as a literal dollar-brace placeholder, this deployment has no public address configured — say so rather than guessing a URL, and share the item's KEY instead, which a colleague can paste into the board's own search.

This skill is enforced (the `required` default) and triggers on exactly the write tools — posting with broken mention markup is visible to human teammates, while reads never wait on it.

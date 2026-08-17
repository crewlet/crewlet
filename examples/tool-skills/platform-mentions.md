---
key: skill:platform_mentions
trigger:
  any_of:
    - tool: mattermost_post_message
    - tool: mattermost_update_message
    - tool: create_work_item_comment
    - tool: update_work_item_comment
    - tool: create_work_item
    - tool: update_work_item
    - tool: create_page
phases: [plan, execute]
title: Mentioning teammates
summary: |
  Per-platform mention markup (Plane / Mattermost) + shareable link shapes.
  Resolve IDs via lookup_colleague before writing.
---

Call `lookup_colleague` for their per-surface ID before writing the body. The two platforms take different markup, and the wrong one posts as plain text with nobody notified.

- **Plane** (work-item comments ONLY): `<mention-component id="<uuid4>" entity_identifier="<plane_user_id>" entity_name="user_mention"></mention-component>` — generate a fresh random UUID4 for `id` per mention node; `entity_identifier` is the target's `plane_user_id` from `lookup_colleague`; `entity_name` stays the literal `user_mention`. The markup survives the editor's sanitization and notifies exactly like a mention typed in the web app. Page bodies and work-item *descriptions* do NOT route mentions — when someone must be notified about a page, mention them in a comment on the related work item.
- **Mattermost**: a literal `@username` — e.g. `@agent-cto`, `@founder`. Use the `mattermost_id` from `lookup_colleague` verbatim; that field already holds the username, not an opaque ID. No angle brackets, no `<@…>` wrapper: that is Slack markup and posts as visible punctuation here. The server resolves the name and notifies, so a typo'd username silently reaches nobody — never guess a spelling, and never assume someone's Mattermost username matches their Plane display name.

**Replying in a thread.** When you are answering a message rather than starting a new topic, pass `root_id` to `mattermost_post_message` — the post ID of the message you are answering (or of the thread's root, if it already has one). A reply posted without `root_id` lands as a new top-level channel message, detached from the conversation it answers.

**Sharing links.** When you share a link to a work item or page, build it from the workspace's human-readable base:

- **Work item:** `${plane_base_url}/${plane_workspace_slug}/projects/{project-uuid}/issues/{work-item-uuid}`
- **Page:** `${plane_base_url}/${plane_workspace_slug}/projects/{project-uuid}/pages/{page-id}`

The `{project-uuid}` segment is the project **UUID** your Plane tools return — never the `ENG`-style identifier, which does not resolve in the web app.

The base URL and workspace slug above are filled in for you from the company's `skill_variables`; if either still shows as a literal dollar-brace placeholder, it is not configured — ask your operator to set it, and do not guess a URL.

This skill is enforced (the `required` default) and triggers on exactly the write tools — posting with broken mention markup is visible to human teammates, while Plane / Mattermost *reads* never wait on it.

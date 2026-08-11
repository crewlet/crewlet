---
key: skill:platform_mentions
trigger:
  any_of:
    - tool: slack_conversations_add_message
    - tool: create_work_item_comment
    - tool: update_work_item_comment
    - tool: create_work_item
    - tool: update_work_item
    - tool: create_page
phases: [plan, execute]
title: Mentioning teammates
summary: |
  Per-platform mention markup (Plane / Slack) + shareable link shapes.
  Resolve IDs via lookup_colleague before writing.
---

Call `lookup_colleague` for their per-surface ID before writing the body. A bare `@handle` is Slack shorthand and posts as plain text on Plane (no notify).

- **Plane** (work-item comments ONLY): `<mention-component id="<uuid4>" entity_identifier="<plane_user_id>" entity_name="user_mention"></mention-component>` — generate a fresh random UUID4 for `id` per mention node; `entity_identifier` is the target's `plane_user_id` from `lookup_colleague`; `entity_name` stays the literal `user_mention`. The markup survives the editor's sanitization and notifies exactly like a mention typed in the web app. Page bodies and work-item *descriptions* do NOT route mentions — when someone must be notified about a page, mention them in a comment on the related work item.
- **Slack**: `<@<slack_bot_id>>` — use the bot user ID, not the channel ID under `slack_id`.

**Sharing links.** When you share a link to a work item or page, build it from the workspace's human-readable base:

- **Work item:** `${plane_base_url}/${plane_workspace_slug}/projects/{project-uuid}/issues/{work-item-uuid}`
- **Page:** `${plane_base_url}/${plane_workspace_slug}/projects/{project-uuid}/pages/{page-id}`

The `{project-uuid}` segment is the project **UUID** your Plane tools return — never the `ENG`-style identifier, which does not resolve in the web app.

The base URL and workspace slug above are filled in for you from the company's `skill_variables`; if either still shows as a literal dollar-brace placeholder, it is not configured — ask your operator to set it, and do not guess a URL.

This skill is enforced (the `required` default) and triggers on exactly the write tools — posting with broken mention markup is visible to human teammates, while Plane / Slack *reads* never wait on it.

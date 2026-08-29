---
key: skill:platform_mentions
trigger:
  any_of:
    - tool: mattermost_post_message
    - tool: mattermost_update_message
    - tool: jira_add_comment
    - tool: jira_create_issue
    - tool: jira_update_issue
    - tool: confluence_add_comment
    - tool: confluence_create_page
    - tool: confluence_update_page
phases: [plan, execute]
title: Mentioning teammates
summary: |
  Per-platform mention markup (Jira / Confluence / Mattermost) + shareable
  link shapes. Resolve IDs via lookup_colleague before writing.
---

Call `lookup_colleague` for their per-surface ID before writing the body. The platforms take different markup, and the wrong one posts as plain text with nobody notified.

- **Jira** (issue comments and descriptions): `[~accountid:<atlassian-account-id>]` — the id is the `jira` entry `lookup_colleague` returns, and it is the same id Confluence uses. Jira renders it as a mention chip and notifies the account, exactly like a mention typed in the web app. Do not write `@name`: Jira does not resolve display names, so it posts as visible punctuation and reaches nobody.
- **Confluence** (page bodies AND footer comments): `<ac:link><ri:user ri:account-id="<atlassian-account-id>" /></ac:link>` — storage format, so it survives the editor and notifies. Unlike Jira, a Confluence *page body* does route mentions, so someone who must read a page can be mentioned in the page itself.
- **Mattermost**: a literal `@username` — e.g. `@agent-cto`, `@founder`. Use the `mattermost` id from `lookup_colleague` verbatim; that field already holds the username, not an opaque ID. No angle brackets, no `<@…>` wrapper: that is Slack markup and posts as visible punctuation here. The server resolves the name and notifies, so a typo'd username silently reaches nobody — never guess a spelling, and never assume someone's Mattermost username matches their Atlassian display name.

**Replying in a thread.** When you are answering a message rather than starting a new topic, pass `root_id` to `mattermost_post_message` — the post ID of the message you are answering (or of the thread's root, if it already has one). A reply posted without `root_id` lands as a new top-level channel message, detached from the conversation it answers.

**Sharing links.** When you share a link to a work item or page, build it from the site's human-readable base — never from a tool result's `self` URL, which is a REST endpoint, nor from an `api.atlassian.com/ex/...` gateway address, which a colleague cannot open:

- **Work item:** `${jira_base_url}/browse/{ISSUE-KEY}` — e.g. `ENG-42`
- **Page:** `${confluence_base_url}/spaces/{SPACE}/pages/{page-id}`

The base URLs above are filled in for you from the company's `skill_variables`; if either still shows as a literal dollar-brace placeholder, it is not configured — ask your operator to set it, and do not guess a URL.

This skill is enforced (the `required` default) and triggers on exactly the write tools — posting with broken mention markup is visible to human teammates, while Jira / Confluence / Mattermost *reads* never wait on it.

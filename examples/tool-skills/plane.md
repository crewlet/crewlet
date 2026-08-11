---
key: mcp:plane
trigger:
  mcp_server: plane
phases: [plan, execute]
title: Plane tools
summary: |
  Read / search / create / update work items; comment; read + publish
  pages. Authoring code happens in the sandbox, not via these tools.
---

You have access to Plane via MCP. Capabilities:

- Read, search, create, and update work items; manage assignees, states,
  and labels; track work and report status back to the requester.
- Comment on work items — comments are where thread discussion (and
  @-mentions) happen.
- Read and publish pages (the team's shared knowledge base).

**Writing code is a sandbox job, not a Plane-tool job.** When a task needs
you to *implement or modify code*, call the `run_sandbox` tool (see the
`skill:code_runtime` skill) — a coding agent does the work in an isolated
checkout and opens the MR under your own code-host identity. Use the Plane
tools here to read the work item, update its state, comment, and follow up
— not to author the change.

You act as your own Plane service account: humans assign work items to you,
subscribe you to threads, and @-mention you. When a work item is assigned
to you, report back to whoever asked for the work in the originating
channel once you're done.

**Never set `is_draft` on a work item.** In Plane, `is_draft: true` means
a *private unsubmitted draft*: it is excluded from every project view and
API read — invisible to teammates, unopenable by the person who asked,
and un-editable even by you after creation. When someone asks for a
"draft ticket", they mean a normal work item in the Backlog state to be
refined — create exactly that.

**Create work items only in your org's projects** (the ones your role
context names). A fresh Plane workspace also contains a seeded demo
project named after the workspace itself — you are not a member of it,
writes there fail with 403, and nothing of the org's lives in it.

This skill triggers on the whole `plane` MCP server, so under the
`required` default it loads once per session before any Plane call. That is
why this body is deliberately a one-pager: the per-session cost stays near
zero. Keep it that way — detailed practices belong in tool-specific skills.

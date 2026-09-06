---
key: mcp:gitlab
trigger:
  mcp_server: gitlab
phases: [execute]
title: GitLab tools
summary: |
  Read / search code + issues + MRs; review, comment, and track work.
  Authoring code happens in the sandbox (run_sandbox), not via these tools.
---

You have access to GitLab via MCP. Capabilities:

- Read and search code, files, and projects.
- Create and manage issues and merge requests; comment on and review MRs
  (approve, comment on the diff, resolve threads).
- Track work and report status back to the requester.

**Writing code is a sandbox job, not a GitLab-tool job.** When a task needs
you to *implement or modify code*, call the `run_sandbox` tool
(see the `skill:code_runtime` skill) — a coding agent does the work in an
isolated checkout and opens the MR under your own GitLab identity. Use the
GitLab tools here to read diffs, review, comment, and follow up on that MR —
not to author the change.

You act as your own GitLab service account: humans assign issues/MRs to you,
request your review, and @-mention you by your username. When a merge request
is assigned to you or your review is requested, report back to whoever asked
for the work in the originating channel once you've reviewed.

This skill triggers on the whole `gitlab` MCP server, so under the `required`
default it loads once per session before any GitLab call. That is why this
body is deliberately a one-pager: the per-session cost stays near zero. Keep
it that way — detailed practices belong in tool-specific skills.

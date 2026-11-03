---
key: mcp:gitlab
trigger:
  mcp_server: gitlab
phases: [execute]
title: GitLab tools
summary: |
  Read / search code + issues + MRs; review, comment, and track work.
  Author code in your own checkout with your shell and git, not via these tools.
---

You have access to GitLab via MCP. Capabilities:

- Read and search code, files, and projects.
- Create and manage issues and merge requests; comment on and review MRs
  (approve, comment on the diff, resolve threads).
- Track work and report status back to the requester.

**Writing code is your checkout's job, not a GitLab-tool job.** You run as
a harness with your own shell and git in your box: to *implement or
modify code*, clone the project there, make the change, run its tests,
push a branch and open the MR under your own GitLab identity. Use the
GitLab tools here to read diffs, review, comment, and follow up on that
MR — not to author the change.

You act as your own GitLab service account: humans assign issues/MRs to you,
request your review, and @-mention you by your username. When a merge request
is assigned to you or your review is requested, report back to whoever asked
for the work in the originating channel once you've reviewed.

This skill triggers on the whole `gitlab` MCP server, so under the `required`
default it loads once per session before any GitLab call. That is why this
body is deliberately a one-pager: the per-session cost stays near zero. Keep
it that way — detailed practices belong in tool-specific skills.

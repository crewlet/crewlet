---
key: mcp:github
trigger:
  mcp_server: github
phases: [execute]
title: GitHub tools
summary: |
  Read / search code + issues + PRs; review, comment, and track work.
  Author code in your own checkout with your shell and git, not via these tools.
---

You have access to GitHub via MCP. Capabilities:

- Read and search code, files, and repositories.
- Create and manage issues and pull requests; comment on and review PRs.
- Track work and report status back to the requester.

**Writing code is your checkout's job, not a GitHub-tool job.** You run as
a harness with your own shell and git in your box: to *implement or
modify code*, clone the repository there, make the change, run its tests,
push a branch and open the PR. Use the GitHub tools here to read diffs,
review, comment, and follow up on that PR — not to author the change.

This skill triggers on the whole `github` MCP server, so under the
`required` default it loads once per session before any GitHub call. That is
why this body is deliberately a one-pager: the per-session cost stays near
zero. Keep it that way — detailed practices belong in tool-specific skills.

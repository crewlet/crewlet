---
key: mcp:github
trigger:
  mcp_server: github
phases: [execute]
title: GitHub tools
summary: |
  Read / search code + issues + PRs; review, comment, and track work.
  Authoring code happens in the sandbox (run_sandbox), not via these tools.
---

You have access to GitHub via MCP. Capabilities:

- Read and search code, files, and repositories.
- Create and manage issues and pull requests; comment on and review PRs.
- Track work and report status back to the requester.

**Writing code is a sandbox job, not a GitHub-tool job.** When a task needs
you to *implement or modify code*, call the `run_sandbox` tool
(see the `skill:code_runtime` skill) — a coding agent does the work in an
isolated checkout and opens the PR. Use the GitHub tools here to read diffs,
review, comment, and follow up on that PR — not to author the change.

This skill triggers on the whole `github` MCP server, so under the
`required` default it loads once per session before any GitHub call. That is
why this body is deliberately a one-pager: the per-session cost stays near
zero. Keep it that way — detailed practices belong in tool-specific skills.

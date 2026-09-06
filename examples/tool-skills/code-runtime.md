---
key: skill:code_runtime
trigger:
  mcp_server: gitlab
phases: [execute]
required: false
title: Running code work in the sandbox
summary: |
  For real code work, call the run_sandbox tool: a coding agent
  implements in an isolated checkout and opens an MR. Write a clear
  brief; report the MR back with your native tools.
---

When a task needs you to **implement or modify code, run tests, reproduce a
bug, or run a script**, and your role is sandbox-enabled, call `run_sandbox`
with a concrete `brief`.

How to brief it well:

- Name the repository and the concrete change in the `brief`. The coding
  agent has its own shell, git, and code-host token — it clones, edits, runs
  tests, pushes a branch, and opens the MR **as your own identity**.
- Say what DONE means inside the brief itself: the coding agent and the
  reviewer measure against the same bar, so "endpoint returns 200 and tests
  pass" beats "do the thing".

What the sandbox delivers and doesn't:

- It delivers the **code change** (the MR). It does **not** post back to the
  channel that triggered you. The run is detached: your turn suspends at the
  `run_sandbox` call and resumes automatically with the coding agent's
  report spliced in as the tool result — you keep full context, so finish
  the job yourself: report the MR link via the originating channel's reply
  tool, update the work item, or call `run_sandbox` again to fix what the
  report flags.
- If the coding agent gets blocked on a human decision, it asks via
  `crewlet-ask`; the engine routes the question to you/your team, and the
  work resumes on the answer — you don't need to babysit it.

Keep your native tools for everything else (replying, updating a work item,
reading docs, answering a question) — the sandbox is for authoring code,
not chat.

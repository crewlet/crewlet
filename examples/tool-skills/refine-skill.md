---
key: tool:refine_skill
trigger:
  tool: refine_skill
phases: [plan]
title: Skill refinement
summary: |
  Patch a loaded synthesized skill when you find it stale or wrong.
  Don't wait to be asked.
---

After a task that took 5+ tool calls, fixed a tricky failure, or established a repeatable workflow, a later reflection pass may propose a skill. When using a loaded skill and finding it outdated or wrong, call `refine_skill` — do not wait to be asked.

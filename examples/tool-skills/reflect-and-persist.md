---
key: tool:reflect_and_persist
trigger:
  tool: reflect_and_persist
phases: [plan]
title: Persisting durable facts
summary: |
  Persist declarative facts (not instructions). Call after learning
  something worth carrying across turns.
---

Persist durable facts that reduce future steering. Write **declarative facts, not instructions to yourself**. "Stakeholder X prefers weekly digests" ✓ — "Always send weekly digests" ✗. Call `reflect_and_persist` when you learn something worth carrying across turns.

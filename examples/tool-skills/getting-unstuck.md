---
key: skill:getting_unstuck
trigger:
  any_of:
    - tool: mattermost_post_message
    - mcp_server: atlassian
    - tool: a2a_ask
phases: [execute, review]
required: false
title: Getting unstuck — manager handoff
summary: |
  When stuck after trying, hand off to your manager on the surface
  where the problem lives. Include what you tried, options seen,
  recommendation, and urgency. Never hand a naked problem.
---

# When to use this skill

You can't complete the task and you've already exhausted what you can do alone:

- **Authority** — the decision needs someone with permissions you don't have (deploy, budget, hire, policy change).
- **Resources** — you need access to a system / repo / channel / dataset you can't reach.
- **Expertise** — the question is genuinely outside your domain and a colleague would resolve it in minutes.
- **Stuck** — you've tried, gathered evidence, and don't see a path forward without input.

If you're just *uncertain*, prefer `self_iterate` first — re-plan with the uncertainty as the next thing to resolve.

# How to pick the surface

Hand off on the surface where the problem already lives. The audit trail belongs next to the work.

| Where the problem lives | Where to hand off | How |
|---|---|---|
| Jira work item | Comment on the work item | Jira comment tool + @-mention your manager via the `platform_mentions` skill |
| Mattermost thread | Reply in the thread | `mattermost_post_message` with `root_id` set to the thread root + `@manager-username` |
| Confluence page review | Comment on the page | Confluence footer-comment tool + mention markup |
| Merge request | Comment on the MR / request review | Your code-host MCP comment tool + mention |
| Cross-team coordination | Their team's channel | See `channel-discovery` skill |
| Tight-loop mechanical sync | A2A | `a2a_ask` (only when the other side is also an agent and the question is short / mechanical) |

Your manager's handle is in your identity prompt under **Your manager**.  If your role has no defined manager, hand off in your team's channel (also in the identity prompt) so any peer can pick it up.

# Required content

A handoff message must include all four:

1. **What you tried** — concrete tool calls / approaches, not "I tried but it didn't work".  Two to four bullets.
2. **Options you see** — two to four alternatives.  Even one alternative is better than asking "what should I do".
3. **Your recommendation** — which option you'd take and why.  Don't dodge; the manager will override if they disagree.
4. **Urgency** — is anyone blocked?  Is there a deadline?  One short sentence.

# Anti-patterns

- ❌ Asking for help without saying what you tried.
- ❌ Asking the same person on multiple surfaces (Mattermost DM + work-item comment) — pick one.
- ❌ Pinging the team channel for a question only one person can answer — DM that person.
- ❌ Using A2A for a complex / strategic question — the conversation disappears from any human surface.
- ❌ Reaching for a colleague because you don't want to think harder.  Re-plan first.

# Worked example (Jira work-item comment)

```
@John I'm blocked on AUTH-1432 (rate-limit middleware refactor).

What I tried:
- read the existing middleware (src/auth/throttle.py) and the three callers
- prototyped a replacement using token-bucket; tests pass locally
- tried wiring it through the existing IThrottleStrategy interface but the
  signature change breaks two downstream services

Options:
1. Add a v2 method to IThrottleStrategy alongside v1 (backwards compatible
   but two methods to maintain)
2. Bump IThrottleStrategy version with a breaking change + migrate the two
   downstream services (clean but ~3 MRs across teams)
3. Wrap my new implementation in an adapter that satisfies v1 (clean for
   me, hides the bucket semantics from callers)

I recommend (2) — the v1 interface predates the rate-limit redesign and
the downstream services are both ours.  Migration is mechanical.

Not blocking anything urgently but the rate-limit incidents from last week
are still open, so this should land this sprint.
```

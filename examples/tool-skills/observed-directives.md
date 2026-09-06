---
key: skill:observed_directives
trigger:
  any_of:
    - tool: mattermost_post_message
phases: [execute]
title: Sharing observed directives
summary: |
  If a stakeholder told you a standing team rule (not a one-off
  preference), share it in your team channel or hand it to your lead.
---

If a stakeholder told you something during this turn that looks like a STANDING RULE for the team or org (not just a one-off preference about how they personally work with you), consider sharing it: post a brief note in your team's channel via `mattermost_post_message`, or hand it to your team lead on the surface where the work lives (see the `getting-unstuck` skill). Personal observations stay in your own counterparty profile automatically; cross-agent application requires the rule to reach docs / channels other agents can read. Use judgement — do this for plausibly team-relevant directives, not for every interaction.

---
key: skill:observed_directives
trigger:
  any_of:
    - tool: post_message
    - tool: reply_in_thread
    - tool: send_dm
    - tool: mattermost_post_message
    - tool: slack_conversations_add_message
phases: [execute]
title: Sharing observed directives
summary: |
  If a stakeholder told you a standing team rule (not a one-off
  preference), share it in your team channel or hand it to your lead.
---

If a stakeholder told you something during this turn that looks like a STANDING RULE for the team or org (not just a one-off preference about how they personally work with you), consider sharing it: post a brief note in your team's channel (`post_message` on the company's own chat, `mattermost_post_message` on Mattermost, `slack_conversations_add_message` on Slack), or hand it to your team lead on the surface where the work lives (see the `getting-unstuck` skill). Personal observations stay in your own counterparty profile automatically; cross-agent application requires the rule to reach docs / channels other agents can read. Use judgement — do this for plausibly team-relevant directives, not for every interaction.

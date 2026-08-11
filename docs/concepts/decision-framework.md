# Decision Framework (DACI)

Multi-agent systems deadlock when agents disagree and no resolution mechanism exists. Crewlet uses the **DACI framework** (Driver / Approver / Contributor / Informed) as behavioral guidance — decisions happen naturally in Slack using each agent's existing MCP tools.

---

## DACI Roles

- **Driver** — the agent responsible for driving to resolution (typically the task owner)
- **Approver** — the agent who signs off (typically the driver's manager)
- **Contributors** — agents whose input is solicited before the decision
- **Informed** — agents notified of the outcome after resolution

These roles are implied by the org hierarchy. The driver's manager is the natural approver. Contributors are peers on the same team or in related teams.

---

## How It Works

There is no decision engine or special tooling. Agents use their team's **Slack channel** and their existing **Slack MCP tools** to discuss and decide — just like real employees:

1. **Driver posts in the team Slack channel** — states the decision topic, context, and options
2. **Contributors reply** — share their perspectives in the thread
3. **Driver synthesizes** — proposes an outcome based on contributions
4. **Approver approves or rejects** — replies with the final call
5. **Informed parties see the thread** — visibility is automatic

The team Slack channel is configured on the OrgUnit:

```yaml
units:
  - name: Core Engineering
    type: team
    lead: CTO
    slack_channel: C0123456789    # team's Slack channel
    roles:
      - name: CTO
        integrations:                                  # per-agent transport identity
          slack:
            bot_token: "${SLACK_BOT_TOKEN_CTO}"
            signing_secret: "${SLACK_SIGNING_SECRET_CTO}"
        mcp_env:
          slack:
            SLACK_MCP_XOXB_TOKEN: "${SLACK_BOT_TOKEN_CTO}"   # same token, the Slack MCP
      - name: Engineer
        integrations:
          slack:
            bot_token: "${SLACK_BOT_TOKEN_ENG}"
            signing_secret: "${SLACK_SIGNING_SECRET_ENG}"
        mcp_env:
          slack:
            SLACK_MCP_XOXB_TOKEN: "${SLACK_BOT_TOKEN_ENG}"
```

Each agent's system prompt includes their team channel, with guidance to use it for team discussions and decisions.

---

## Why No Decision Engine?

Agents already have Slack MCP tools for posting messages, threading, reading channels, and reacting. The org hierarchy already defines who reports to whom. Adding a separate decision engine with structured tools, internal state machines, and formatted messages would duplicate what Slack and the org chart already provide.

The simpler approach: tell agents their team channel and let them communicate naturally. DACI is a behavioral pattern, not a programmatic one.

---

## Example

```
#core-engineering channel:

Engineer: "Need to decide on auth strategy: JWT vs session tokens.
  @CTO @PM — would appreciate your input.
  Options: 1) JWT for statelessness  2) Sessions for easy revocation"

PM (thread reply): "Sessions — revocation is critical for our compliance needs"

Engineer (thread reply): "Proposal: JWT with short expiry + refresh tokens + token blacklist.
  Gets us statelessness with revocation capability.
  @CTO — what do you think?"

CTO (thread reply): "Approved. Add the blacklist to the MVP scope.
  Good synthesis of the tradeoffs."

Engineer proceeds with approved approach.
```

---

## Slack Channel Inheritance

The `slack_channel` field on OrgUnit is inherited by child units that don't set their own, just like `lead` inheritance. A department-level channel cascades to all teams unless a team specifies its own.

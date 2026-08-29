# Decision Framework (DACI)

Multi-agent systems deadlock when agents disagree and no resolution mechanism exists. Crewlet uses the **DACI framework** (Driver / Approver / Contributor / Informed) as behavioral guidance — decisions happen naturally in the company's chat, using each agent's existing MCP tools.

---

## DACI Roles

- **Driver** — the agent responsible for driving to resolution (typically the task owner)
- **Approver** — the agent who signs off (typically the driver's manager)
- **Contributors** — agents whose input is solicited before the decision
- **Informed** — agents notified of the outcome after resolution

These roles are implied by the org hierarchy. The driver's manager is the natural approver. Contributors are peers on the same team or in related teams.

---

## How It Works

There is no decision engine or special tooling. Agents use their team's **channel** and their existing **chat MCP tools** to discuss and decide — just like real employees:

1. **Driver posts in the team channel** — states the decision topic, context, and options
2. **Contributors reply** — share their perspectives in the thread
3. **Driver synthesizes** — proposes an outcome based on contributions
4. **Approver approves or rejects** — replies with the final call
5. **Informed parties see the thread** — visibility is automatic

The team channel is configured on the OrgUnit, and it is vendor-neutral: the same field serves whichever chat backend the company runs. A seat's transport identity is its own bot — a [Mattermost](../integrations/mattermost.md) bot token, or a [Slack](../integrations/slack.md) app:

```yaml
units:
  - name: Core Engineering
    type: team
    lead: CTO
    channel: core-engineering    # the team's channel on the chat surface
    roles:
      - name: CTO
        integrations:                                  # per-agent transport identity
          mattermost:
            bot_token: "${MATTERMOST_BOT_TOKEN_CTO}"
        mcp_env:
          mattermost:
            MATTERMOST_TOKEN: "${MATTERMOST_BOT_TOKEN_CTO}"  # same token, the chat MCP
      - name: Engineer
        integrations:
          mattermost:
            bot_token: "${MATTERMOST_BOT_TOKEN_ENG}"
        mcp_env:
          mattermost:
            MATTERMOST_TOKEN: "${MATTERMOST_BOT_TOKEN_ENG}"
```

One credential per seat, not two: a Mattermost bot's personal access token covers the websocket, the REST calls and the MCP server, and there is no inbound webhook to verify.

Each agent's system prompt includes their team channel, with guidance to use it for team discussions and decisions.

---

## Why No Decision Engine?

Agents already have chat MCP tools for posting messages, threading, reading channels, and reacting. The org hierarchy already defines who reports to whom. Adding a separate decision engine with structured tools, internal state machines, and formatted messages would duplicate what the chat surface and the org chart already provide.

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

## Channel Inheritance

The `channel` field on OrgUnit is inherited by child units that don't set their own, just like `lead` inheritance. A department-level channel cascades to all teams unless a team specifies its own.

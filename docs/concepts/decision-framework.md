# Decision Framework (DACI)

Multi-agent systems deadlock when agents disagree and no resolution mechanism exists. Crewlet uses the **DACI framework** (Driver / Approver / Contributor / Informed) as behavioral guidance — agents discuss in the company's chat with their own MCP tools, and when a decision needs a specific person to choose, the driver puts it to them as a **structured ask** on a work item.

The engine records that ask and enforces three things about it: that it is well formed, that an answer names one of its options, and that an outcome the driver promised to post in a channel is posted. It runs **no workflow, no approval chain and no state machine** — how a company decides stays a matter of the guidance its agents are given.

---

## DACI Roles

- **Driver** — the agent responsible for driving to resolution (typically the task owner). It is the one who asks.
- **Approver** — the agent or person who signs off (typically the driver's manager). Asked with `role: approver`, their answer **is** the decision.
- **Contributors** — agents or people whose input is solicited before the decision. Asked with `role: contributor`, their answer is an input the driver weighs.
- **Informed** — who hears the outcome after resolution: the team channel, which a structured ask can name as its `inform` channel.

These roles are implied by the org hierarchy. The driver's manager is the natural approver. Contributors are peers on the same team or in related teams. Nothing in the engine assigns them: the driver chooses whom to ask, and in which role.

---

## How It Works

A decision has two halves, and they live in different places.

**The discussion happens in chat.** Agents use their team's **channel** and their existing **chat MCP tools** to discuss — just like real employees:

1. **Driver posts in the team channel** — states the decision topic, context, and options
2. **Contributors reply** — share their perspectives in the thread
3. **Driver synthesizes** — proposes an outcome based on contributions

**The decision itself is a structured ask.** When the driver needs somebody to choose, it asks them on the work item the decision belongs to — `comment_on_work_item` with `ask` and `decision`, or `create_work_item` with the same two to file the question as an item of its own:

4. **Driver asks the approver** — the question in one sentence, two to six options, the option it recommends and why, the evidence it looked at, and the role the person is asked in
5. **Approver chooses** — answers with `choice`, the id of one option, and says why in the body if they like. A person answers with the same call through the [operator surface](../reference/api-endpoints.md#operatoract--the-dashboards-write-surface) — an assistant on `/operator/mcp`, or the dashboard, which writes through `/operator/act` as the person its token is bound to; an agent is woken with the options and the answering call written out whole
6. **Driver is woken with the choice** — by the option's label, and continues from it
7. **Informed parties see the outcome** — when the ask named an `inform` channel, the driver posts it there, and its turn is not finished until it has

The rules for each part of a decision, the caps, and the tools' arguments are in [Asking for a decision](../guides/work-tracker.md#asking-for-a-decision).

The team channel is configured on the unit (`channel`), and it is integration-neutral: the same field serves whichever chat backend the company runs. A seat's transport identity is its own bot: a [Mattermost](../integrations/mattermost.md) bot token, or a [Slack](../integrations/slack.md) app:

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

## When the outcome is promised to a channel

A structured ask on a work item (see [Asking for a decision](../guides/work-tracker.md#asking-for-a-decision))
can name the channel the asker will report the outcome in — `inform:
{surface, channel}`. That is the **Informed** role made concrete, and it is one
of the two things the engine enforces about a decision (the other is that a
choice names an option of the ask it answers):

- **Only an agent seat may promise it.** The engine keeps the promise by
  holding the asker's turn, and a person asking from the dashboard or through
  an assistant has no turn to hold.
- **The channel is the chart's.** The surface must be one the company runs and
  the asking seat holds a bot on, and the channel one a unit declares with
  `channel`.
- **The answer owes the post.** The asker is woken by the answer owing that
  chat surface, and its turn is not finished until a tool there has posted — a
  comment on the work item does not discharge it.

The person answering sees the consequence on the card: *"&lt;asker&gt; is woken
with your answer and posts it to #&lt;channel&gt;"*, or, with no inform,
*"&lt;asker&gt; continues from your answer"*.

---

## What the engine enforces, and what it does not

A structured ask is a comment on a work item, so it lives where every other comment does — in the tracker, identical on every node — and the engine holds it to exactly three rules:

- **Its shape.** Two to six options, each with an id typed back in `choice`; a recommendation that is one of them; evidence that resolves (a cited task is stored by id, a cited page must exist); a role. The decision rides only the comment that asks and is never edited afterwards, because an answer names an option by id and options changed under it would change what that answer meant.
- **Its answer.** A `choice` must name an option **of the ask it answers**, checked against that ask's own record, and an ask is answered once — a second answer is refused naming who answered first, and when.
- **Its promise.** An `inform` channel is kept, as described above.

What it deliberately does **not** do:

- **No approval chain.** One ask goes to one person, who answers it once. A decision that needs two sign-offs is two asks, and the driver decides what to do with the answers.
- **No quorum, voting or state machine.** `role` tells the person whether their answer is the decision or an input to one; it gates nothing and routes nothing.
- **No escalation timer.** An unanswered ask stays open and visible — in the asked person's `my_work`, their Inbox and the item's thread — and chasing it is the driver's behavior, not a timeout the engine fires.
- **No routing of its own.** The driver names whom to ask. The engine does not look up an approver from the org chart.

Each of those would make one company's way of deciding the engine's, and each would be a policy that today is a sentence in an agent's guidance — one a founder changes by editing a prompt rather than by running a migration. The org hierarchy already says who reports to whom, chat already carries the discussion, and the tracker already carries a question somebody owes an answer to; the structured ask adds only what none of them could: options a person can choose between at a glance, and an answer the driver does not have to interpret.

---

## Example

The discussion, in the team channel:

```
#core-engineering channel:

Engineer: "Need to decide on auth strategy: JWT vs session tokens.
  @PM — would appreciate your input.
  Options: 1) JWT for statelessness  2) Sessions for easy revocation"

PM (thread reply): "Sessions — revocation is critical for our compliance needs"

Engineer (thread reply): "Proposal: JWT with short expiry + refresh tokens + a
  revocation list. Gets us statelessness with revocation capability.
  Putting it to @CTO on ENG-42."
```

The decision, put to the approver on the work item — the engineer's
`comment_on_work_item` call:

```json
{
  "item": "ENG-42",
  "body": "Summary of the #core-engineering thread: PM needs revocation for compliance; JWT alone cannot revoke.",
  "ask": "cto",
  "decision": {
    "question": "Which auth strategy do we build for the MVP?",
    "options": [
      {"id": "jwt", "label": "JWT only", "detail": "Stateless, no revocation"},
      {"id": "sessions", "label": "Server sessions", "detail": "Easy revocation, sticky state"},
      {"id": "jwt-revocable", "label": "Short-lived JWT + revocation list", "detail": "Both, one extra table"}
    ],
    "recommended": "jwt-revocable",
    "rationale": "Keeps the API stateless and meets the compliance requirement the PM raised.",
    "role": "approver",
    "inform": {"surface": "mattermost", "channel": "core-engineering"}
  }
}
```

The CTO is woken with the three options and the answering call, and answers:

```json
{"item": "ENG-42", "answers": "<the ask's comment id>", "choice": "jwt-revocable", "body": "Approved. Add the revocation list to the MVP scope."}
```

The engineer is woken with *Chose “Short-lived JWT + revocation list”: Approved…*,
posts the outcome in `#core-engineering` — its turn is held until it does — and
proceeds with the approved approach.

---

## Channel Inheritance

The `channel` field on a unit is inherited by child units that don't set their own, just like `lead` inheritance (`org.Organization.Normalize`). A department-level channel cascades to all teams unless a team specifies its own.

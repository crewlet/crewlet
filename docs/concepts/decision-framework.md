# Decision Framework (DACI)

Multi-agent systems deadlock when agents disagree and nothing resolves it. Crewlet uses the **DACI framework** (Driver / Approver / Contributor / Informed) as behavioral guidance: agents discuss in the company's chat with their own MCP tools, and when a specific person must choose, the driver puts it to them as a **structured ask** on a work item.

The engine records the ask and enforces three things: it is well formed, an answer names one of its options, and an outcome the driver promised to post in a channel is posted. It runs **no workflow, approval chain or state machine**; how a company decides stays in its agents' guidance.

---

## DACI Roles

- **Driver**: drives to resolution (typically the task owner). It is the one who asks.
- **Approver**: signs off (typically the driver's manager). Asked with `role: approver`, their answer **is** the decision.
- **Contributors**: agents or people whose input is solicited. Asked with `role: contributor`, their answer is an input the driver weighs.
- **Informed**: who hears the outcome: the team channel, which an ask can name as its `inform` channel.

The roles are implied by the org hierarchy (the driver's manager is the natural approver; contributors are peers), but the engine assigns none: the driver chooses whom to ask, and in which role.

---

## How It Works

A decision has two halves in different places.

**The discussion happens in chat.** Agents use their team's **channel**, with the engine's chat tools (`post_message`, `reply_in_thread`) on [Chatlet](chatlet.md), or their **chat MCP tools** on a vendor's:

1. **Driver posts in the team channel**: the decision topic, context and options
2. **Contributors reply**: their perspectives, in the thread
3. **Driver synthesizes**: proposes an outcome from the contributions

**The decision itself is a structured ask.** The driver asks on the work item the decision belongs to: `comment_on_work_item` with `ask` and `decision`, or `create_work_item` with the same two to file the question as an item of its own:

4. **Driver asks the approver**: the question in one sentence, two to four options, the recommended option and why, the evidence looked at, and the role the person is asked in
5. **Approver chooses**: answers with `choice`, the id of one option, and may say why in the body. A person answers with the same call through the [operator surface](../reference/api-endpoints.md#operatoract--the-dashboards-write-surface): an assistant on `/operator/mcp`, or the dashboard, which writes through `/operator/act` as the principal the person signed in as (the seat they hold, which every person does). An agent is woken with the options and the answering call written out whole
6. **Driver is woken with the choice**, by the option's label, and continues from it
7. **Informed parties see the outcome**: when the ask named an `inform` channel, the driver posts it there, and its turn is not finished until it has

Rules for each part, the caps and the tools' arguments are in [Asking for a decision](../guides/worklet.md#asking-for-a-decision).

**An executor asks in the same shape.** A seat's executor is its harness in a box; a question it cannot go on without parks it. It calls `ask_and_wait` with the question, context, and the same options, recommendation, rationale, role and evidence a decision takes ([Mid-run clarification](code-sandbox.md#mid-run-clarification-ask_and_wait), [ADR-0068](../../adr/0068-a-run-asks-a-person-in-the-shape-a-decision-is-asked.md)). The person reads both in one layout in the Inbox and answers both with one press: a work item's ask with `comment_on_work_item{answers, choice}`, an executor's with `answer_executor{turn_id, choice}`, which carries the executor on with the option chosen. The executor's ask lives on its own record, because its turn need not be on a work item and a company on another tracker has none. It carries no `inform`: its answer resumes the executor itself, not a turn that could be held to a promise.

The team channel is set on the unit (`channel`) and is integration-neutral. On the engine's [own chat](chatlet.md) it opens the unit's room and needs nothing else; every agent seat posts there as itself. On a vendor's, a seat's transport identity is its own bot: a [Mattermost](../integrations/mattermost.md) bot token, or a [Slack](../integrations/slack.md) app:

```yaml
units:
  - name: Core Engineering
    id: core-engineering         # the unit's key; `name` above is display
    type: team
    lead: cto                    # the lead seat's handle
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

Each agent's system prompt includes its team channel, with guidance to use it for team discussions and decisions.

---

## When the outcome is promised to a channel

An ask (see [Asking for a decision](../guides/worklet.md#asking-for-a-decision)) can name the channel the asker will report the outcome in, `inform: {surface, channel}`. That is the **Informed** role made concrete, and one of the two things enforced about a decision (the other: a choice names an option of the ask it answers):

- **Only an agent seat may promise it.** The engine keeps the promise by holding the asker's turn; a person asking from the dashboard or an assistant has no turn to hold.
- **The channel is the chart's.** The surface must be one the company runs and the asking seat can post on (a bot it holds on Mattermost or Slack, or the seat itself on the engine's own chat, `chat`), and the channel one a unit declares with `channel`.
- **The answer owes the post.** The asker is woken by the answer owing that chat surface, and its turn is not finished until a tool there has posted; a comment on the work item does not discharge it.

The person answering sees the consequence on the card: *"&lt;asker&gt; is woken with your answer and posts it to #&lt;channel&gt;"*, or, with no inform, *"&lt;asker&gt; continues from your answer"*.

---

## What the engine enforces, and what it does not

A structured ask is a comment on a work item, so it lives in the tracker, identical on every data node. The engine holds it to exactly three rules:

- **Its shape.** Two to four options, each with an id typed back in `choice`; a recommendation that is one of them, and why; evidence that resolves (a cited task is stored by id, a cited page must exist); a role. An agent seat's question to a person is always one: asked in words, it is refused before anything is posted, because a person answers a decision with one press and has nothing to press for prose ([ADR-0100](../../adr/0100-a-question-an-agent-puts-to-a-person-offers-options-and-recommends-one.md)). The decision rides only the comment that asks and is never edited, because an answer names an option by id and changed options would change what the answer meant.
- **Its answer.** A `choice` must name an option **of the ask it answers**, checked against that ask's own record, and an ask is answered once: a second answer is refused naming who answered first, and when.
- **Its promise.** An `inform` channel is kept, as above.

What it deliberately does **not** do:

- **No approval chain.** One ask goes to one person, who answers once. A decision needing two sign-offs is two asks; the driver decides what to do with the answers.
- **No quorum, voting or state machine.** `role` tells the person whether their answer is the decision or an input; it gates and routes nothing.
- **No rewording.** Options never change once asked. An agent whose question no longer fits (its access changed, or the work moved on) withdraws it with `withdraw_ask`, saying why in its thread and naming what now covers it where something does (`replaced_by`, shown on the closed card in the person's Inbox), and asks again. A question asked before a change to the agent's access is noted as such in the Inbox until its review of the change ends ([An agent whose access changed reviews its open work](agent-runtime.md#an-agent-whose-access-changed-reviews-its-open-work)).
- **No escalation timer.** An unanswered ask stays open and visible, in the asked person's `my_work`, their Inbox and the item's thread; chasing it is the driver's behavior, not a timeout the engine fires.
- **No routing of its own.** The driver names whom to ask; the engine does not look up an approver from the org chart.

Each would make one company's way of deciding the engine's; today each is a sentence in an agent's guidance, which a founder changes by editing a prompt, not by running a migration. The structured ask adds only what the org hierarchy, chat and tracker could not: options a person can choose between at a glance, and an answer the driver need not interpret.

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

The decision, put to the approver on the work item, is the engineer's `comment_on_work_item` call:

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

The engineer is woken with *Chose “Short-lived JWT + revocation list”: Approved…*, posts the outcome in `#core-engineering` (its turn is held until it does) and proceeds.

---

## Channel Inheritance

A unit's `channel` is inherited by child units that don't set their own, like `lead` inheritance (`org.Organization.Normalize`). A department-level channel cascades to all teams unless a team sets its own.

# Delegated Decisions

Agents ask people to approve what they cannot do alone: a command on a
server today, a merge or a deploy as more kinds are added. When a person
accepts the agent's recommendation almost every time, the Inbox fills with
requests that only need a look. **Delegated decisions** let an agent seat you
choose, a *delegate*, decide those requests in a person's place, within
rules Crewlet enforces itself, and put in front of people only what it cannot
decide or judges risky.

It is off unless you turn it on, and one setting turns it off again.

## What a request is for

Every request that waits on a person carries its **act**: what approving it
does. Crewlet sets it from the structured data that made the request, never
from an agent's words.

| Act | What it is |
|---|---|
| `ssh_operate` | An agent's command on a server as its operate user ([SSH](../integrations/ssh.md#the-agents-tools)), or a decision `ask_to_operate` composed for one exact command |
| `other` | Every other request. Never delegated |

## Turning it on

```yaml
delegated_decisions:
  enabled: true
  rules:
    - kinds: [ssh_operate]
      delegates: [security-engineer]
      escalate_to: [ops-lead]
      sample_one_in: 10        # optional, 1 to 1000, default 10
```

| Field | Meaning |
|---|---|
| `enabled` | Turns delegation on. Off puts every request to people at once and keeps the rules. |
| `rules[].kinds` | The acts the rule delegates. A kind is in at most one rule; `other` is never delegated. |
| `rules[].delegates` | One agent seat, or two for a **panel**: both must allow, and any disagreement goes to people. A panel's two seats must run on different first `llm` entries, so one model's blind spot is not both. |
| `rules[].escalate_to` | The people an escalation is put to, beside administrators. At least one. They read the digest and spot-checks, and may approve the rule's requests as approvers do. |
| `rules[].sample_one_in` | One decision in this many is shown to `escalate_to` after the fact, for a spot-check. |

A delegate is an agent seat, never a person, and it can never request the
kind it decides: a delegate for `ssh_operate` holds no SSH access of its own.
The document is refused otherwise, and refused with a rule that names nobody
to escalate to.

## What a delegate does

A request of a delegated kind goes to its delegates first, as a turn of its
own. The turn's brief shows the request whole (for a command: the server, the
command exactly, the standard input in full with its size and SHA-256, the
reason, the decision it carries out and the work item) with the requesting
agent's words fenced as data. Nothing inside a request is an instruction to
the delegate. Until it is escalated, the request is in nobody's Inbox.

The delegate answers with `review_delegated`:

```json
{"request": "<id>", "verdict": "allow", "confidence": "high",
 "reason": "Restarts the API after the config change the item describes.",
 "findings": [], "digest": "<the request's digest from the brief>"}
```

- **`allow`** carries out what a person's approval would. For a command, that
  is the answer a person's Allow once gives: a five-minute certificate bound to
  exactly that command, run once on the server as shown, and refused if the
  server's address, port, operate user or keys changed meanwhile.
- **`escalate`** puts the request to people, with the delegate's verdict and
  findings shown first on its card.
- **`deny`** refuses it and tells the requesting agent the reason.

`digest` binds the verdict to the exact request the delegate was shown; a
verdict naming another digest is refused.

## What Crewlet enforces, whatever the delegate says

| Rule | What happens |
|---|---|
| Low confidence | Goes to people. |
| A panel that disagrees | Goes to people. A panel allows only when both allow. |
| The delegate's own request | A request the delegate made, asked for or handed on, or one from a seat it leads, goes to people. |
| A sensitive item | A request on a work item labelled `security`, `privacy` or `legal` goes to people. |
| Always-escalate rules | A request that touches access grants, budgets or spend, data deletion, secrets or the company's own document goes to people. For a command, that is the [always-escalate list](../integrations/ssh.md#what-a-delegate-never-allows). |
| Hourly caps | At most 30 allows per kind and delegate, and 10 commands per server, in one clock hour (UTC), counted across the fleet. Past either, requests go to people. A count Crewlet cannot take also goes to people. |
| Time | A request no delegate answered in 15 minutes goes to people. |
| Turning it off | `enabled: false`, removing a rule, or changing its delegates shows its waiting requests to people on the next read, and records why within a minute. |

Every verdict, and every escalation Crewlet makes itself, is recorded in the
access log **before** it acts. A verdict whose record did not land carries
nothing out; the request reaches people at its deadline.

## What you still see

- **Escalations**, in the Inbox and on Home, with each delegate's verdict, confidence, reason and findings above the request, and why Crewlet put it to you.
- **Decided under delegation**: every answer a delegate gave is marked with its rule wherever the answer is shown: the run's page, the turn, the event and the live event log.
- **The digest**: what delegates decided for you on the last company day, on Home and in **Settings › Delegated decisions**.
- **Spot-checks**: one decision in `sample_one_in`, shown after the fact, with nothing to answer unless it was wrong.
- **The audit**: `GET /setup/integrations/ssh/audit?kind=verdict` for every verdict on a command, and `GET /delegated-decisions` for the rules, the digest and the spot-checks.

## Approve once at the decision

An agent can ask before it calls: `ask_to_operate {item, ask, host, command,
stdin, reason}` puts one exact command to a person as a decision on the work
item. Crewlet composes it, quoting the server, the command and the standard
input's SHA-256, and stamps the ask with its act. Choosing **Run it once** is
an SSH approval: only an administrator or a person `integrations.ssh.approvers`
or an `ssh_operate` rule's `escalate_to` names may choose it, signed in again
within the step-up window. In the turn that answer starts, the agent's
`ssh_operate` call that matches the act byte for byte runs without a second
prompt, once; any difference is asked again, and the audit records the
command as approved by that person through that decision.

## What is not supported yet

- Merges and deploys as delegated kinds.
- Editing rules from the Settings view, which reads them; change them in the
  company document.

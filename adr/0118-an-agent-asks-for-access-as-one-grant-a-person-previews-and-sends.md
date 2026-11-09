# ADR-0118: An agent asks for access as one grant a person previews and sends

- **Status:** accepted
- **Authority:** `internal/advisor`
- **Enforced-by:** `internal/advisor.TestEveryGrantChangesOnlyWhatItNames`, `internal/advisor.TestAGrantThatCannotBeMadeIsRefused`, `internal/advisor.TestEveryAccessKeyIsAnOptionID`, `internal/advisor.TestTheDashboardKnowsAGrantOptionByTheEnginesPrefix`, `internal/agent/builtin.TestAnAccessRequestIsADecisionWhoseOptionIsTheGrant`, `internal/agent/builtin.TestAnAccessRequestNobodyCouldGrantIsRefused`
- **Tag-status:** unreleased

## The decision

**An agent asks for more access with `request_access`, naming one grant from a
closed catalogue, and the grant is made only by a person who previewed it.**

- **The catalogue is the engine's.** Each grant is a key (`grant-health`,
  `grant-github-pull-requests`, and the other GitHub App write permissions)
  with what it lets an agent do and the fields of the agent's own role entity
  that grant it. An agent cannot ask for anything the catalogue does not name,
  for a grant it holds, or for one it has nothing to add to.
- **The request is a decision (ADR-0100).** The tool posts it through
  `comment_on_work_item` on the work item that needs the access, asked of a
  person, with two options: the grant, whose id is the key, and declining.
  The question and the options are the engine's words; the agent's reason is
  the rationale.
- **The change is built for the asking agent alone, at read time.** The option
  carries only the key. `GET /advisor/access` (administrators) builds the
  entity change from the redacted company document for the seat that asked,
  so nothing an agent writes reaches the change.
- **Choosing the grant previews it; only Apply saves it; only then is the
  question answered.** The dashboard draws the change from the entity route's
  dry run (ADR-0115), sends it from the person's session under the entity's
  own tag, and answers the question with the grant once the write is saved.
  The write is `config.write`, so it asks for the person's step-up as every
  configuration change does. Closing the preview answers nothing; declining
  answers at once. One request is one grant, never batched.

## Why the obvious alternatives are wrong

**Carrying the change in the decision.** The agent would then write the change
the person approves, and a prompt injection in its work could widen it. The key
is all the record holds; the engine builds the rest.

**Answering the question and granting in the background.** A person who chose
the grant would then have approved a change they never saw, and a grant that
failed would leave the agent told yes with nothing changed.

**A free-text request.** An agent asking for "admin" in words is a question no
person can answer with one press, and the access an agent most often lacks has
an exact field.

## What this does not decide

Grants beyond the catalogue (a new key is added with its fields and its words),
who in a company should approve access (the agent names a person; the write is
refused to anyone the configuration surface refuses), and how the agent learns
the grant landed (ADR-0104 tells it on its next turn).

# ADR-0118: An agent asks for access as one grant a person previews and sends

- **Status:** accepted
- **Authority:** `internal/advisor`
- **Enforced-by:** `internal/advisor.TestEveryGrantChangesOnlyWhatItNames`, `internal/advisor.TestAGrantThatCannotBeMadeIsRefused`, `internal/advisor.TestEveryAccessKeyIsAnOptionID`, `internal/advisor.TestTheDashboardKnowsAGrantOptionByTheEnginesPrefix`, `internal/agent/builtin.TestAnAccessRequestIsADecisionWhoseOptionIsTheGrant`, `internal/agent/builtin.TestAnAccessRequestNobodyCouldGrantIsRefused`, `internal/agent/builtin.TestNoAuthorsDecisionMayOfferAGrant`, `internal/agent/builtin.TestAGrantIsAnsweredOnlyOnceItsAskerHoldsIt`, `internal/advisor.TestTheAccessCatalogueIsWhatASeatCanHold`, `internal/advisor.TestAGrantHoldsAtItsLevelAndBringsWhatItNeeds`, `internal/github.TestEveryNeedIsAPermissionASeatCanHold`, `internal/agent/builtin.TestRequestAccessOffersWhatASeatCanHoldAndWhereItIsSet`
- **Tag-status:** unreleased

## The decision

**An agent asks for more access with `request_access`, naming one grant from a
closed catalogue, and the grant is made only by a person who previewed it.**

- **The catalogue is the engine's, derived from what a seat can hold.** Each
  grant is a key with what it lets an agent do and the fields of the agent's
  own role entity that grant it: `grant-health`, and one key per level of
  every permission a seat's GitHub App may hold (`github.SeatPermissions`),
  `grant-github-<permission>-<read|write>`, never a permission the seat
  catalogue denies. The keys are derived from that catalogue rather than
  listed, so every GitHub permission the Agents tab offers an agent can ask
  for, and a permission added there is askable the same day. An agent cannot
  ask for anything the catalogue does not name, for a grant it holds (the
  permission at that level or higher), or for one it has nothing to add to
  (no GitHub App).
- **A level carries what it needs.** Where GitHub makes one permission's
  level useless without another (`github.Needs`: Workflows at Read and write
  needs Contents at Read and write, since a workflow change is a push), the
  grant merges both and says so, so a granted request is one the document
  admits. The rule lives in one place, which the document's validator reads
  too.
- **An integration's levels are not in the catalogue.** A level at Jira,
  Datadog, AWS or any other vendor is a role that replaces the agent's role
  there, not one more permission beside it. Administering is not the reason:
  the catalogue holds two administration grants, Contents and Workflows at
  Read and write, which the config entity write applies behind the same
  step-up the Agents tab's save takes for an administration role. An agent
  that needs a level asks in words, and the tool tells it where an
  administrator sets it: Settings, Integrations, then the integration's Agents
  tab.
- **The request is a decision (ADR-0100).** The tool posts it through
  `comment_on_work_item` on the work item that needs the access, asked of a
  person, with two options: the grant, whose id is the key, and declining.
  The question and the options are the engine's words; the agent's reason is
  the rationale.
- **Only the engine offers a grant.** An option id beginning with `grant-` is
  reserved: every author's decision that names one is refused, and the
  request's own decision is admitted on an internal path no argument reaches.
  The dashboard draws a grant's button in the words of the change the engine
  builds, never the decision's label.
- **The change is built for the asking agent alone, at read time.** The option
  carries only the key. `GET /advisor/access` (administrators) builds the
  entity change from the redacted company document for the seat that asked,
  so nothing an agent writes reaches the change.
- **Choosing the grant previews it; only Apply saves it; only then is the
  question answered.** The dashboard draws the change from the entity route's
  dry run (ADR-0115), sends it from the person's session under the entity's
  own tag, and answers the question with the grant once the write is saved.
  A GitHub grant is its one permission merged into the entity as the preview
  read it (with the permission its level needs, where it needs one), so no
  other field is rewritten. The engine accepts an answer
  choosing the grant only once the fleet's active revision gives the agent
  that access, whoever sends it.
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

**A catalogue kept by hand.** It named four GitHub permissions at write while
the Agents tab offered seven, so an agent lacking Workflows had no grant to
ask for, wrote its own instructions, and pointed them at the vendor's app
settings rather than at its grant in Crewlet. A list derived from the seat
catalogue cannot fall behind it.

## What this does not decide

Grants beyond the catalogue (a grant that is not a GitHub permission is added
as a key with its fields and its words), who in a company should approve access (the agent names a person; the write is
refused to anyone the configuration surface refuses), and how the agent learns
the grant landed (ADR-0104 tells it on its next turn).

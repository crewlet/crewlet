# ADR-0118: An agent asks for access as one grant a person previews and sends

- **Status:** accepted
- **Authority:** `internal/advisor`
- **Enforced-by:**
  - `internal/agent/builtin.TestAPermissionRequestIsTheEnginesDecision`
  - `internal/agent/builtin.TestAPermissionIsAnsweredOnlyOnceItsAgentHoldsIt`
  - `internal/advisor.TestEveryGrantChangesOnlyWhatItNames`
  - `internal/advisor.TestAGrantThatCannotBeMadeIsRefused`
  - `internal/advisor.TestEveryAccessKeyIsAnOptionID`
  - `internal/advisor.TestTheDashboardKnowsAGrantOptionByTheEnginesPrefix`
  - `internal/agent/builtin.TestAnAccessRequestIsADecisionWhoseOptionIsTheGrant`
  - `internal/agent/builtin.TestAnAccessRequestNobodyCouldGrantIsRefused`
  - `internal/agent/builtin.TestNoAuthorsDecisionMayOfferAGrant`
  - `internal/agent/builtin.TestAGrantIsAnsweredOnlyOnceItsAskerHoldsIt`
  - `internal/advisor.TestTheAccessCatalogueIsWhatASeatCanHold`
  - `internal/advisor.TestAGrantHoldsAtItsLevelAndBringsWhatItNeeds`
  - `internal/github.TestEveryNeedIsAPermissionASeatCanHold`
  - `internal/agent/builtin.TestRequestAccessOffersWhatASeatCanHoldAndWhereItIsSet`
  - `internal/agent/builtin.TestChoosingARequestsGrantSavesItBeforeTheAnswer`
  - `internal/agent/builtin.TestARequestsGrantIsChosenOnlyAtTheAccessWritesProof`
  - `internal/api/setupapi.TestChoosingARequestsGrantSavesTheGrantItAsked`
  - `internal/api/setupapi.TestARequestsGrantThatCannotBeSavedWritesNothing`
  - `internal/engine.TestARequestsGrantIsSavedThroughTheInstalledSaver`
  - `internal/assistant/catalog.TestAGrantRequestsAnswerIsDrawnInTheEnginesWords`
  - `internal/api.TestAnAgentsWordsNeverApproveItsGrant`
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

**Amended: one permission at an integration, beside the level (ADR-0150).**
The tool takes a second form, `integration`, `permission`, optional `scope`
and `hours`, for one permission from that integration's live catalogue (a
Cloudflare permission group) held beside the agent's level rather than
replacing it. The engine checks it against the live catalogue, stores the
request and posts a decision of three options, its grant options carrying
the request's id under the reserved prefix (`grant-r<id>-t` for the
request's hours, recommended, `grant-r<id>-p` for good) and declining. The
person previews the grant built from the stored request, saves it on the
integration's Agents tab, and only then is the question answered; the engine
accepts the answer only once the active revision holds a live grant naming
that request. A level is still never asked for this way.

**Amended: choosing the request's grant option is the approval, in one
engine operation.** Saving the grant on the Agents tab and then answering the
question were two writes the client made in order. A failure between them left
the grant saved and the agent unanswered. A rollout that had not reached the
node answering refused the answer. And Ask Crewlet could not approve the request
at all, since it would have had to propose two dependent writes as one. Now a
person answers the request's question with `grant-r<id>-t` or `grant-r<id>-p`
on the answer's own route (`POST /worklet/items/{key}/comments`, or
`comment_on_work_item` on the act route), and the engine runs the approval as
one operation. Every step happens in this order, or none does:

- **The proof is the access write's, not the comment's.** A comment asks no
  step-up, so the choice is decided as `setup.access.write`: an
  administrator signed in within the step-up window. Where the grant raises
  (a permission that changes things at the vendor, or one the live catalogue
  cannot vouch for, `seataccess.RaisesGrant`), it is also decided as
  `setup.access.raise`, which needs a person present, so an access token
  is refused it (`token_refused`). A stale proof is answered
  `step_up_required`, and the dashboard's step-up ceremony sends the same
  answer again. Nobody else may choose the option, and a seat never may
  (both verbs are human-only), whatever its words or its apps.
- **The grant is the stored request's.** The engine builds it from the request
  it stored when the agent asked: the permission, the scope, the hours and
  the reason. The option decides only whether it lapses after those hours or
  is for good. The request must still be open and be the asker's, and the
  grant is checked against the live catalogue as a save from the tab is.
- **Saved, then answered.** The grant is written as one revision attributed to
  the person, recorded as granted, its request settled, and the integration's
  sync started. Only then is the answer posted, and the existing check that
  the active revision holds the grant passes. An answer sent again after its
  grant landed finds the grant held and saves nothing twice. A grant refused
  (stale proof, lapsed request, a catalogue that no longer offers it) posts
  nothing.
- **Ask Crewlet proposes it as one card.** `answer_decision` choosing a grant
  option prepares one proposal whose card draws the grant in the engine's
  words (the permission by the catalogue's name, where it applies, when it
  lapses, and whether it raises) and asks the person to confirm it is them.
  The model never approves. The person's approval of that one card is the
  choice, and the approved request runs on the act route at the same proof
  (ADR-0051).

Declining is unchanged. The Agents tab still adds and removes grants as
before, and its save of a grant answering a request still settles that
request; the dashboard's Inbox sends the answer alone.

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

**Keeping the grant and the answer as two writes the client makes.** Each
caller had to know the order, and only the dashboard did: a person answering
through any other surface was refused until somebody saved the grant
elsewhere, and two proposals for one approval let the second be approved
without the first. One operation makes the order the engine's.

**A route of its own that grants and then answers.** The question is already
answered on one route by every surface, the Inbox and Ask Crewlet alike, and a
second route for one kind of option would leave the first refusing that option
for ever. Deciding the choice at the access write's own proof keeps the
answer's route the one way a question is answered.

**Saving the grant when the agent asks, held until somebody answers.** The
document would then hold access nobody approved, and a sync could apply it
before the answer.

## What this does not decide

Grants beyond the catalogue (a grant that is not a GitHub permission is added
as a key with its fields and its words), who in a company should approve access (the agent names a person; the write is
refused to anyone the configuration surface refuses), and how the agent learns
the grant landed (ADR-0104 tells it on its next turn).

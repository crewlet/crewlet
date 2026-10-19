# ADR-0060: A caveat or a refusal is worded for each reader, and a person is never shown a model's

- **Status:** accepted
- **Authority:** `internal/caveat`
- **Enforced-by:** `internal/caveat.TestEveryCaveatAPersonCanBeShownIsWordedForAPerson`, `internal/caveat.TestTheWalkCoversEveryConstructorAndCode`, `internal/caveat.TestEveryCodeStatesWhatItAsksOfAPerson`, `internal/caveat.TestTheDashboardReadsWhatACaveatAsks`, `internal/agent/builtin.TestOnlyTheCaveatHelperWritesAToolsWarnings`, `internal/api/operator.TestAnActAnswersCaveatsInAPersonsWordsAndNeverTheModels`, `internal/caveat.TestEveryRefusalAPersonCanBeShownIsWordedForAPerson`, `internal/caveat.TestTheCheckRefusesAModelsRefusal`, `internal/caveat.TestARefusalsWordsRideItsError`, `internal/api/operator.TestEveryRefusalTheActRouteAnswersIsWordedForAPerson`, `internal/api/operator.TestAnActRefusalIsAPersonsWordsOrItsCodes`, `internal/estate.TestARefusalsPersonWordsCrossTheRouter`, `internal/caveat.TestNoTextBecomesAPersonsWordsByBeingDecoded`, `internal/caveat.TestOnlyTheEstateRouterRebuildsAPeersWords`, `internal/caveat.TestACaveatCrossesTheEstateRouterWhole`, `internal/api/operator.TestEveryAnswerTheActRouteGivesIsWordedForAPerson`, `internal/api/workapi.TestTheWorkSurfaceAnswersAPersonInAPersonsWords`, `internal/agent/builtin.TestEveryAnswerAPersonCanBeGivenIsWordedForAPerson`, `internal/api.TestARefusedAssistedWriteTellsTheModelAndNeverTheAnswer`, `internal/caveat.TestEveryRemedyNamesAKnownSetting`, `internal/caveat.TestARefusalCrossesToAPeerWithItsRemedy`, `internal/caveat.TestTheDashboardKnowsEverySettingARemedyNames`, `internal/caveat.TestTheDashboardReadsARemedyAsTheEngineWritesIt`, `internal/assistant.TestASpentBudgetNamesTheCeilingToRaise`, `internal/api/assistantapi.TestABudgetRefusalIsATooManyRequestsWithItsReset`, `internal/api/operator.TestASpentBudgetAnswersBudgetExhausted`, and the compile errors a caveat or a refusal phrasing built outside its package meets (their fields are unexported), and a free sentence handed to `operator.Fail` meets (it takes a `caveat.Refusal`)
- **Cost-when-tried:** answering a decision in the dashboard Inbox left a warning toast that never went away, telling a person "backend-engineer is woken by this but is not asked to answer it. Use ask: backend-engineer, or @-mention them, if you are expecting a reply." The sentence was `comment_on_work_item`'s `warnings`, written for a model, and `useAct` showed every receipt warning verbatim as a sticky toast. The same path carried a project's default-assignee advice naming `write_project` and `default_assignee`, internal task ids from a retried create, and a near-duplicate tag sentence appended into the `labels_created` list of slugs. PR #408 fixed the one branch that fired on an answer, and the class stayed open. Refusals took the same route: `operator.Fail` put the tool's sentence in `detail` and the dashboard's `RefusalNote` drew it, so a decision answered with a choice the question did not offer was refused with "comment_on_work_item was refused: … change the argument this names (`answers`, or `choice`) and comment again".
- **Tag-status:** unreleased

## The decision

**A caveat (something a write that landed tells its caller, and was not
refused for) is one value carrying a model's phrasing and a person's.** Both
are written together, by one constructor per code in `internal/caveat`, from
the same facts. Nothing outside the package can construct one, and none
decodes from JSON: the one other source is a peer's catalog, whose caveats the
estate router alone rebuilds from the answer it routed
(`caveat.RebuildFromPeer`, held to `internal/estate` by a gate), so text from
anywhere else, a tool's output or a request body, can never become words a
person is shown. Each code also
states what it asks of a person: `required` (something stays broken until
somebody acts) or `none` (something to know).

**A partial outcome is a caveat too.** A write that landed part of what it
was asked (an item filed whose dependencies were not recorded, a move or a
removal whose walk stopped, a datastore verdict not applied here yet) keeps
the model's sentence under its own key, written only by `builtin.partial`,
and says the same to a person as a caveat of its own code
(`dependencies_not_recorded`, `move_unfinished` or `move_waiting`,
`subtree_unfinished`, `datastore_pending`). Those keys are model-only
(`builtin.ModelOnlyKeys`), and every surface answering a person takes them off
the receipt (`builtin.ForPerson`), as it takes off `warnings`.

**Each surface reads its own phrasing.** A builtin tool's answer carries the
model's words under one top-level `warnings` list, written only by
`builtin.caveated`, and the caveats themselves on `mcp.Result.Caveats`, which
is never rendered to a model (`Cause`'s rule). `POST /operator/act` answers
`caveats: [{code, message, action}]` in the person's words and takes
`warnings` off the receipt, so no screen reading the receipt can show a person
advice written for a model. The dashboard shows a press's caveats as a toast
that stays until dismissed when any is `required`, and as one that goes by
itself otherwise, by the kit Toaster's own variant defaults.

**A refusal is answered to a person in a person's words, or in its code's.**
A tool's refusal keeps its model sentence as `Output`, untouched. Beside it,
`mcp.Result.PersonRefusal` carries a `caveat.Refusal`, a person's phrasing
constructed only in `internal/caveat`'s refusal catalog (or rebuilt from a
peer's by the estate router, `caveat.SaidFromPeer`, under the same gate as a
caveat), and never rendered to a model. A domain writer that refuses with a
person's phrasing puts it beside its error (`caveat.Saying`, or a typed
refusal whose chain carries a `caveat.Said`), it crosses the estate router as
a typed value, and
`builtin.refusedBy` lifts it onto the result, so no tool answering that error
has to remember it. `operator.Fail`, the one refusal mapping every person's
surface answers through (the act route, the human write surface's tool-backed
routes and its tool-less verbs), takes that typed value and nothing else: a
refusal with a phrasing answers it as `detail`, and one without answers **no**
`detail`, so its code's own sentence (httpjson's per-code `message`, written
for people) stands alone. That is the floor: a refusal nobody has worded for a
person tells them less, and never tells them a model's words. The same rule
holds for every other sentence a person's surface used to answer from a
writer or a tool: a writer's error text, a condition's words, the sentence a
route's own authority decision named the verb in, and the tool's sentence on
an unknown outcome are all gone from the answer, which names what a client
acts on as values (`reason`, `grants`, `fields`, `action`, `op_id`). The act
route's own refusals (a tool it does not serve, a read, a body it cannot
take) answer their code and `tool`, and log what a developer needs.

**A refusal a configurable limit caused names where the limit is changed.**
The words say what happened; a person who can raise the limit then still has to
find it. So such a refusal carries a **remedy** beside its words: a value naming
the setting (`caveat.Setting`: the company's `token_budget`, a seat's, or
`assistant.token_budget`) and, for a seat's, the seat. It rides the
`caveat.Refusal` it belongs to (constructed by the catalog's
`TokenBudgetSpent`, and carried across the estate router with the words), and
`operator.Fail` answers it as `remedy` beside `detail`. The Dashboard
Assistant's own refusals answer it too, with no `caveat.Refusal` of their own:
the turn route's `429 budget_exhausted` and a turn's `budget_exhausted` error
frame name the ceiling the meter refused by, through one mapping from the
meter's scope (`org` or `person`) to the setting. The remedy is a value, not a
sentence and not an address: the dashboard's link table (`lib/remedy.ts`)
resolves each setting to the screen and the section that edit it, and draws the
way there behind the same write gate as the editor it opens, so a reader who
may not change the setting is told who can and is never handed a link to a
screen whose controls are locked for them.

The person phrasings this record ships are for the refusals a person meets
most on the dashboard: a work item with no title or a text over its cap
(title, description, comment), an answer to a question that asks nothing, was
removed or was asked of somebody else, a choice the decision does not offer or
made where no question is open, and an artifact version the policy refuses,
the storage limit stops or the trash holds.

## Why the obvious alternatives are wrong

**Rewording the sentences** is what #408 did, and it recurs: every caveat is
authored at the tool, beside the model contract, by the rule that serves a
model ("say what to call next"), and nothing in a list of strings says who it
is for. The next caveat written well for a model is the next one a person
cannot act on.

**A code and parameters, worded by each surface** puts every caveat's sentence
in two languages with nothing holding them to the same codes, and a code the
dashboard has not learned renders as nothing. Here the dashboard renders text
it never had to write, and a test walks the one catalog.

**A sticky toast for every caveat** made each one demand a click, including a
date stored in its canonical form. Whether a person must act is a property of
the caveat, so it is stated with the code rather than guessed by a renderer.

**Rewording each refusal's model sentence for both readers** fails as
rewording caveats did, and twice over: about 500 refusal sites are prompt text
tuned against model behaviour ("call get_work_item again"), and a sentence
that serves a person drops exactly what the model acts on.

**An address in the refusal** (a URL, or a screen and an anchor written by the
engine) puts the dashboard's routes in the engine, where nothing moves them
with the router; a screen renamed there would leave every refusal pointing at
Not Found. **Grants in the remedy** would be a second statement of who may
change the setting beside the gate the editor is drawn by, and the two would
disagree for exactly the readers that matter: a lead inside their own unit, and
an administrator of a document another system manages.

**Showing the floor in the dashboard only** (its own sentence for every code,
the engine's detail ignored) would hold one client and leave every other
reader of the act route, a person's assistant and a script on a person's
credential, with the model's words. The engine is where the guarantee is
kept, so every reader inherits it.

## What this does not decide

**Per-site person phrasings for the remaining refusals** are a separate
change (PR 2): every refusal site not listed above answers the floor until it
gains a constructor in the refusal catalog. By area: the work tools' own
argument refusals (`work.go`, `workperson.go`, `workplace.go`,
`workrelate.go`, `workmerge.go`, `workmove.go`, `worktrash.go`,
`workfiles.go`, `workprojects.go`, `workviews.go`, `worksearch.go`,
`workcatalogue.go`, and asking a decision in `workdecision.go`); Worklet's
domain refusals (`invalid` and `forbidden` sentences in its checklist,
catalogue, coercion, project, tag, dependency and move code, and
`writeFailure`'s tag clash, full tag set, reassignment budget and full inbox
arms); the page tools and Doclet's refusals, the tool-less page verbs among
them; the artifact tools' remaining arms (an edit that does not apply, content
and upload refusals, a turn's publish quota, sources, folders, sharing) and
the artifact data tools; the run, pause, steer and knowledge-answer tools;
Chatlet's tools and a person's chat gestures (an argument past its grammar or
its cap, a gesture the room's rule refuses its caller, an archived room, a
message carrying all the emoji it may, a name another room holds, a room this
node cannot serve yet, and the read-state flush's mute cap and a room both
muted and unmuted); and memory, delegation and sandbox tools a person's
assistant can reach. The
authority table's refusal already answers values a screen words itself
(`reason`, `grants`) and needs none.

**Compatibility starts at the first tag that carries this record.** No tag
has shipped any of these surfaces (the only release, v0.1.0, holds no Go
code), so a node keeps no compatibility with an unreleased build: the
estate's `Caveats` replace the list of sentences an earlier unreleased build
sent with no reader of the old shape, and the act answer's `caveats` and
person-worded `detail` replace what came before outright. From the first tag
that ships this record on, its wire shapes (a caveat's `{code, agent,
person}`, a refusal phrasing on `caveat.Said`, the act answer's `caveats`) are
a released surface, and changing them is owed what RELEASING.md says a
shipped surface is owed.

**Remedies for the limits a person cannot change from the dashboard** are not
part of this record. The artifact and datastore storage quotas
(`ArtifactQuotaReached`, `data_quota`) and the per-node write, read and check
budgets are Tier A, set in each node's own file by whoever runs the nodes, and
no dashboard screen edits them, so a link would open a page with nothing to
change on it. Their refusals keep their words; a remedy is added with the
setting's first editor.

The operation key's own refusals (`op_id_invalid`) and an unknown outcome's
`detail` (`opkey.UnknownDetail`) are a protocol's rule for a client sending
the `Idempotency-Key` header, shared by every write surface; the dashboard
never causes the first and draws the second in its own words, and both are
outside this class. Configuration warnings (`config.Warning`) and artifact
lint findings on the artifacts routes are structured, located and worded for
the person editing the document, and are outside it too.

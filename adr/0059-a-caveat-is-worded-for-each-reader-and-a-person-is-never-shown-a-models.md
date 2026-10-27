# ADR-0059: A caveat is worded for each reader, and a person is never shown a model's

- **Status:** accepted
- **Authority:** `internal/caveat`
- **Enforced-by:** `internal/caveat.TestEveryCaveatAPersonCanBeShownIsWordedForAPerson`, `internal/caveat.TestTheWalkCoversEveryConstructorAndCode`, `internal/caveat.TestEveryCodeStatesWhatItAsksOfAPerson`, `internal/caveat.TestTheDashboardReadsWhatACaveatAsks`, `internal/agent/builtin.TestOnlyTheCaveatHelperWritesAToolsWarnings`, `internal/api/operator.TestAnActAnswersCaveatsInAPersonsWordsAndNeverTheModels`, and the compile error a caveat built outside its package meets (its fields are unexported)
- **Cost-when-tried:** answering a decision in the dashboard Inbox left a warning toast that never went away, telling a person "backend-engineer is woken by this but is not asked to answer it. Use ask: backend-engineer, or @-mention them, if you are expecting a reply." The sentence was `comment_on_work_item`'s `warnings`, written for a model, and `useAct` showed every receipt warning verbatim as a sticky toast. The same path carried a project's default-assignee advice naming `write_project` and `default_assignee`, internal task ids from a retried create, and a near-duplicate tag sentence appended into the `labels_created` list of slugs. PR #408 fixed the one branch that fired on an answer, and the class stayed open.
- **Tag-status:** unreleased

## The decision

**A caveat (something a write that landed tells its caller, and was not
refused for) is one value carrying a model's phrasing and a person's.** Both
are written together, by one constructor per code in `internal/caveat`, from
the same facts; nothing outside the package can construct one. Each code also
states what it asks of a person: `required` (something stays broken until
somebody acts) or `none` (something to know).

**Each surface reads its own phrasing.** A builtin tool's answer carries the
model's words under one top-level `warnings` list, written only by
`builtin.caveated`, and the caveats themselves on `mcp.Result.Caveats`, which
is never rendered to a model (`Cause`'s rule). `POST /operator/act` answers
`caveats: [{code, message, action}]` in the person's words and takes
`warnings` off the receipt, so no screen reading the receipt can show a person
advice written for a model. The dashboard shows a press's caveats as a toast
that stays until dismissed when any is `required`, and as one that goes by
itself otherwise, by the kit Toaster's own variant defaults.

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

## What this does not decide

Refusal sentences reach a person through the same act route and are written
for a model too (`detail` is the tool's sentence). They carry a structured
class already (`mcp.Result.Cause`); giving them a person's phrasing is the
same mechanism applied to `Fail`, and is decided separately. Configuration
warnings (`config.Warning`) and artifact lint findings on the artifacts routes
are structured, located and worded for the person editing the document, and
are outside this class.

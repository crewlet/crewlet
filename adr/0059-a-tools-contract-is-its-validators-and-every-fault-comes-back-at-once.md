# ADR-0059: A tool's contract is its validator's, every fault comes back at once, and a refusal loop is stopped in words

- **Status:** accepted
- **Authority:** `internal/jsonform`
- **Enforced-by:** `internal/jsonform.TestEveryFaultComesBackInOnePass`, `internal/jsonform.TestAKeyIsMatchedExactly`, `internal/artifacts/brandformat.TestTheReferenceCoversEveryKeyOfTheIndex`, `internal/artifacts/brandformat.TestSixFaultsComeBackInOneRefusal`, `internal/artifacts/brandformat.TestTheBrandsOwnDomainIsItsName`, `internal/artifactupload.TestTheGP38KitGetsEveryFaultInOneRefusalAndLandsNext`, `internal/artifactupload.TestAKitWrittenFromTheFormatReferenceLandsFirstTime`, `internal/agent/builtin.TestEveryValidatedKindsFormatIsOneReadAway`, `internal/agent/builtin.TestAMissedToolSkillNamesTheKeysThatExist`, `internal/agent/runner.TestTheGP38LoopIsStoppedAndCounted`, `internal/tools.TestARunOfPolicyRefusalsIsStoppedInWordsAndCountedOnce`, `internal/tools.TestASuccessEndsTheRun`, `internal/tools.TestARunIsCountedPerTarget`, `internal/tools.TestParallelRefusalsCountEachCallOnce`, `internal/agent/builtin.TestAWriteNamesTheTargetItsRefusalsAreCountedOn`, `internal/jsonform.TestTheKeysAreEncodingJSONs`, `internal/jsonform.TestAnOwnReadersRefusalIsAFaultAtItsPath`, `internal/artifacts/canvasformat.TestACanvasStoredWithAStrayBracketStillReads`, `internal/jsonform.TestOnlyThePublishReadsAVersionStrictly`, `internal/jsonform.TestThePublishReadRefusesWhatTheStoredReadAdmits`, `internal/artifacts/brandformat.TestTheBrandIndexAdmitsTheKeysItAlwaysDid`, `internal/artifactupload.TestOnlyAnIndexThePublishWritesIsHeldToTheExactKeys`, `internal/tools/argval.TestEachReadIsItsHelpers`, `internal/tools.TestEachLossyValueIsAMisreadAtItsPath`, `internal/tools.TestEachLosslessValueIsNoFinding`, `internal/tools.TestOutsideAndUnknownAreNamedAndLeftToTheTool`, `internal/tools.TestAMisreadIsRefusedBeforeTheToolRuns`, `internal/tools.TestTheToolsAnswerCarriesEveryFinding`, `internal/agent/builtin.TestEveryBuiltinSchemaIsOneTheCheckKeeps`, `internal/agent/builtin.TestNoArgumentIsReadAsTextAgainstItsType`, `internal/agent/structured.TestEveryFaultOfASubmissionIsNamedAtOnce`, `internal/decision.TestEveryFaultOfADecisionIsNamedAtOnce`, `internal/assistant/catalog.TestEveryAssistantToolNamesEveryFaultAtOnce`, `internal/assistant/catalog.TestEveryFaultOfACallNamingRecordsIsOneRefusal`, `internal/assistant/catalog.TestEveryFaultOfAMembershipIsOneRefusal`, `internal/assistant/catalog.TestAReplaceNeverRemovesByOmission`, `internal/assistant/wire.TestEveryFaultOfAnActionsArgumentsIsNamedAtOnce`, `cmd/crewlet.TestTheShippedAssistantEvalSuitePasses`, `internal/tools.TestEveryToolIsCalledThroughInvoke`, `internal/api/mcpbridge.TestABridgedCallIsHeldToItsToolsSchema`, `internal/api/operator.TestAnOperatorsMisreadIsRefusedBeforeTheToolRuns`, `internal/agent/builtin.TestASetIsNeverReadAsNoList`, `internal/agent/builtin.TestIncludeIsNeverReadAsNoList`, `internal/agent/builtin.TestNoArgumentIsReadByARuleOfItsOwn`, `internal/tools.TestANoteNeverTouchesTheToolsOutput`, `internal/api/operator.TestTheArgumentsNoteIsTheModelsAndNeverAPersons`, `internal/api/operator.TestTheActRouteTellsTheModelTheNoteAndNeverThePerson`, `internal/api/workapi.TestTheWorkAPITellsTheModelTheNoteAndNeverThePerson`, `internal/tools/argval.TestAWholeNumberIsReadWithinItsRange`, `internal/tools.TestAClosedSetIsReadByOneRule`, `internal/tools.TestEveryCounterRecordsTheCataloguesLabels`, `internal/tools.TestAFindingIsANamedValue`, `internal/agent/builtin.TestWhatTheCheckAdmitsIsWhatTheToolReads`, `internal/agent/builtin.TestADeclaredNumberIsReadByItsTypesReader`, `internal/tools.TestARetrySpeltAnotherWayIsTheSameWrite`, `internal/tools.TestALoopSpeltSeveralWaysIsStillOneLoop`, `internal/api/operator.TestAnOperatorsRetrySpeltAnotherWayIsTheSameWrite`, `internal/agent/builtin.TestANullSetIsAbsent`, `internal/tools.TestTheInvokeGateSeesEveryShape`, `internal/agent/builtin.TestTheArgumentGateSeesEveryShape`, `internal/agent/runner.TestAResumedRunsRepeatSpeltAnotherWayIsTheSameWrite`, `internal/agent/runner.TestASeededSubmissionIsReadByTheSurfacesUniverse`
- **Measured:** GP-38 (brand kit): 14 refusals, rounds 11 to 24 and beyond, more than eight minutes of one turn, 12 distinct structural faults learned one per attempt. The brief (#407): 4 of 4 briefs ended `brief_invalid` at 7 steps. Replayed after this change: the GP-38 version gets all 14 of its findings in one refusal and its fixed version lands on the next attempt; a kit written from the format reference lands on the first.
- **Cost-when-tried:** the brief fix (#407) repaired one tool: its caps went into its schema and its validator started naming every fault. Two days later the brand kit failed the same way through a different tool, because nothing had changed for any other tool.
- **Tag-status:** unreleased

## The decision

**A tool that validates structured input shows the model the contract its
validator enforces, generated from the same source, before the model writes.**
For a closed artifact format the source is the Go type the format decodes into:
`jsonform.Of[T]()` derives the form (every key, every type), the strict decode
reads with that form, and the format's `Reference()` renders that form with the
rules the format checks, from the constants it checks against.
`get_artifact_format(kind)` serves it. A test holds each reference to its form
in both directions, and a reference's example is read back through the reader.

**A validator names every fault in one pass, with the path and the shape it
expected.** `jsonform.Decode` reports every repeated key, every unknown key
(with the keys its object takes) and every wrong type (with the shape read
there), decodes every well-formed field, and lets the format's own field rules
run over those in the same pass (`Decoded.Dropped` keeps a rule off a value
the author never wrote). A publish refused for an asset is answered with the
index's findings beside it. A tool answer lists the first 20 findings and
counts the rest.

**The stricter read is the publish's; a stored version reads as it did.**
`jsonform.Decode` matches keys exactly and refuses a key written twice, and
runs only where a publish writes an index (`slideformat.Check`,
`docindex.Check`, `canvasformat.Check`, `dashspec.Check`, and `Parse` and
`ParseIndex` beside them). Every read of a version already stored (a render,
`get_artifact`, a dashboard's data, a compare, a publish that carries an index
it does not rewrite) goes through the format's `Read`, which is
`jsonform.Stored`: ACCEPTANCE IS THE FORMAT'S OLD CALL, encoding/json with
DisallowUnknownFields and that format's own trailing rule (the next token
io.EOF for the deck, doc and dashboard; More false for the canvas, which
accepted a stray `}` or `]` after its value), so a version stored then reads
now, to the same value, by construction. Only when that call refuses does the
walk run, to name every fault. A fuzz holds the two halves together (accepted
exactly when the old call accepts, and at least one fault named otherwise), and
a walk of the tree refuses a strict read anywhere but the publish pipeline and
the formats.
`brand.json` needs neither: it was matched exactly and refused repeats before
this record, and its key tree is held to the one it had then. So no stored
version needs a migration.

**A wrong key is answered with the keys that exist.** `load_tool_skill` answers
a miss with the near keys and the catalogue, and a description names a skill
by its key.

**A write tool refused on its policy again and again is stopped in words, at
the tool-call layer.** `tools.Surface` counts runs of `invalid` refusals IN A
ROW, per tool not annotated read-only and per target the call acts on (an
artifact, a work item, through `tools.Targeted`; the whole tool where it names
none): a success on that target ends the run, and a refusal of another class
leaves it as it was, so refusals across four artifacts are four first attempts.
The count and the record are one step under the surface's lock, so calls
answered side by side cannot read the same count. From the fourth refusal in a
row (`tools.PolicyRefusalLimit`) the answer tells the model to stop retrying,
raise a blocker on its work item if it has one or tell whoever asked, and end
the branch; the run is logged (`tool_policy_refusal_loop`) and counted
(`crewlet.tools.policy_refusal_loops`) once, when it reaches the limit. The run
is a phase's: a resumed phase counts again. The call still runs: never a
silent cap.

**Every first-party tool's call is held to the schema it advertises, whoever
makes it.** Every caller calls a tool through `tools.Invoke`: a phase's surface
(a seat's executor run, over the tool bridge in `internal/api/mcpbridge`; every
other phase, over its tool session; a worker), the operator surface (an
operator's assistant over `/operator/mcp`, the work API's dispatch) and a
replay of a run's submission. A test walks the tree and refuses a tool's own
call made anywhere else, so a caller added later inherits the check by calling
a tool at all. `tools.CheckArgs` checks the call against the tool's
`Parameters`, every finding in one pass with its path and the shape read there,
and `tools.Invoke` acts on three kinds of finding. Third-party MCP servers hold
their own schemas and are not checked.

THE CHECK IS ON THE CALL PATH, NOT ON ONE CALLER: its first version sat in
`tools.Surface`, and the operator surface, which calls the same builtins
through `operator.Server.run`, skipped it.

- **A misread is refused before the tool runs.** A misread is a value the
  tool's own read would change without saying so, which writes wrong data
  that a note after the write cannot undo:
  - a non-boolean where a boolean belongs (`"true"` was read as false);
  - an object or a list where a string belongs (read as `""`);
  - a fraction where a whole number belongs (2.5 was truncated to 2);
  - trailing text after an integer (`"5 days"` was read as 5);
  - a number spelt outside decimal notation (`"0x10"`, `"1_000"`), which
    one reader read as 16 or 1000 and another as 0 or 1;
  - a non-finite number;
  - anything the read replaced with its default;
  - a value of the wrong kind where a list or an object belongs.
- **Outside the schema, the tool decides.** A missing required argument, a
  value outside an enum, a number past its bound or a list too long is left
  to the tool, and its answer carries every finding: a refusal lists them
  all, so one retry answers them.
- **An unknown argument is noted, never refused here.** At the top level,
  the builtin authority gate already refuses an argument the tool does not
  declare (`builtin.ErrUndeclaredArgument`), and that refusal now carries
  every finding. Inside an object argument, an unknown key was ignored. A
  call the tool answered successfully while outside its schema (an unknown
  key it ignored, a value it tolerated) carries a note naming each one in
  `Result.Note`, never in `Output`, which a caller may decode as a JSON
  receipt. The note is worded for the model, so the model and an MCP client
  read it after the answer, and the act and work API answers, which a person
  reads, never carry it (ADR-0060: a person is never shown a model's words);
  they tell it in process to an assistant's model that made the call
  (`modelnote`), as they tell a refusal's model sentence. A misread refused
  before the tool runs answers a person in its own words
  (`caveat.ValueInWrongForm`: the request was malformed, so it asks for the
  change to be sent again by the assistant or made by the person). It is logged (`tool_args_outside_schema`) and counted
  (`crewlet.tools.args_outside_schema`). That count is the drift signal for a
  later decision, per tool, to refuse before the tool runs.
- **The lossless leniencies stay accepted, exactly as the tools read them.**
  They are not findings at all:
  - a number or a boolean where a string belongs, rendered as JSON writes
    it (the JavaScript rule: digits below 1e21 and down to 1e-6, the
    exponent form outside them, the shortest spelling that reads back as
    the same number). This one is a deliberate change from the helpers'
    old `%g`, which wrote 1000000 as `1e+06`;
  - one string, or a comma-separated one, where a list of strings belongs;
  - a number written as a decimal string where a number belongs (`"90"`,
    `"5.0"`, `"1e3"`), read as exactly the number it spells.

**The canonical arguments are made once, at the entry.** Each caller's entry
(`tools.Surface`, `operator.Server.run`, the runner's replay of a submission)
makes the call's canonical arguments before anything reads them
(`tools.ArgsOf`): a closed set's member spelt as its set spells it (`" Draft"`
is `"draft"`). Every step after that reads only this form, as the distinct
type `toolargs.Args`, which the run's call log (`CallLog.Admit`,
`CallLog.Record`), the skill guard, `tools.Invoke` and the tool all require:
the repeat count an operation id carries, the refusal-loop target, the check,
the ledger and the metrics. A collected, continued or reopened executor run
seeds its call log with the turn's closed rounds and the calls its bridge
recorded by the same form, made through the surface it is served
(`tools.Surface.ArgsOf`, required by `turnctx.SeededCall`), so the tools the
surface adds are read by one universe too, while its records keep the
spelling the model sent.
The form was first made in the middle, inside
`tools.Invoke`, so the call log recorded a retry spelt `"Draft"` as sent while
the tool counted it as `"draft"`: the retry took a fresh repeat count and a
fresh operation id, and wrote twice.

**One reader per declared type, shared.** For each type a schema declares
(string, list of strings, integer, number, boolean, a closed set) there is
exactly one function in `internal/tools/argval`, which both decides whether a
value is one and returns the value read (`String`, `Strings`, `Whole`,
`Number`, `Bool`, `Choice`). The check calls it to decide and the tool's
helper (`argString`, `argStrings`, `argInt`, `argIntValue`, `argFloat`,
`argFloatValue`, `argBool`) calls the same function to read, so a value the
check admits is read as exactly what it spells. Null is absent to both,
`{"set": null}` included (`argval.Object`). The one exception is a custom
field's value in `fields`, where null is the value that clears the field by
its declared meaning, and is passed on as null. Two readers for one type is
the root cause this closes: a set written as `"GP-12"` passed the check as a
list and the tool read no list, and `"1e3"` passed as an integer and the tool
read it as 1. A property test holds the check and every helper to the exact
value over a corpus of awkward spellings, and a walk of the builtins refuses a
number parsed by any other function or a key read as a type its schema does
not declare. The
inventory the rule was set from covered the 51 builtins registered for a
seat:

| Reading | Call sites | What it did with a lossy value | Now |
|---|---|---|---|
| `argString` | 212 | a number or boolean rendered; an object or list read as `""` | the object or list is a misread |
| `argBool` | 20 | anything but true or false read as false | a misread |
| `argInt` | 24 | 2.5 truncated, `"5 days"` read as 5, junk read as the fallback | a misread |
| `argIntValue` | 9 | refused a fraction or junk itself | a misread, refused before it runs |
| `argFloat` | 2 | junk read as 0 | a misread |
| `argFloatValue` | 7 | refused non-finite values and junk itself | a misread, refused before it runs |
| `argStrings` | 15 | one string or a comma-separated one split; anything else read as no list | the split is kept; anything else is a misread |
| an unknown argument | every builtin | refused at the top level by the authority gate; ignored inside an object | the refusal carries every finding; inside an object, noted, logged and counted |

Two drift tests hold the advertised schemas to what the check and the tools
read:
- every builtin's schema uses only the keywords and types the check
  implements;
- no argument a schema declares as a number, a boolean or a list is read by
  any tool as free text.

The structured submissions (`structured.Remarshal`), a work item's
`decision`, the assistant catalogue's `Tool.Check` and a page action's
`ActionArgs.Check` keep exactly what they accepted, and now name every fault
of a refused call at once. An assistant call that names a seat, a unit, a
project or a space by its name is checked and resolved in one pass
(`Resolver.Prepare`): every name it cannot place, in every argument and every
entry of a list, is named by its path beside the schema's faults, each
directory's choices once, and a membership's and a replace's previews name
every fault of theirs at once.

## The class, and why a per-tool patch recurs

Both incidents are one failure with six causes, and each cause lives in a
layer every tool shares, not in the tool that happened to fail:

1. **The contract is not visible before the model acts.** The brief's schema
   advertised its caps in bytes at four times the real cap; `brand.json`'s
   shape was served nowhere at all, so the model probed it with keys it knew
   to be wrong (`voice.x`, `logos.zzz`).
2. **The validator stops at the first fault.** `encoding/json` stops at its
   first wrong type, and `DisallowUnknownFields` at its first unknown key, so
   N faults cost N attempts. The brief's retry hit a cap its refusal never
   named.
3. **The refusal does not state the expected shape.** "type.body is a JSON
   string, and the format reads brandformat.Face there" names a Go type the
   model cannot see.
4. **Discovery fails silently.** "Load the artifacts tool skill" sent the model
   to `load_tool_skill("artifacts")`; the key is `skill:artifacts`, and the miss
   named no key that exists.
5. **Nothing notices a loop.** Fourteen refusals of one tool in one turn looked
   like fourteen ordinary failed calls.
6. **Each retry is expensive.** A retry replays the whole conversation, so on
   a large context the loop costs more per round as it goes.

A fix inside one tool repairs one of these for one tool, and the next tool
that validates structured input fails the same way. So each is fixed where
every tool meets it: the form and the decode in `internal/jsonform`, the
reference beside each format, the refusal format in the tool answer, the key
list in the skill lookup, and the loop guard in the surface every call
passes.

## Where each validating tool stands

| Tool input | Contract shown before writing | Every fault in one pass |
|---|---|---|
| A brand kit (`brand.json`, its logos and faces) | `get_artifact_format`, generated | yes, here |
| A deck's `deck.json` | `get_artifact_format`, generated | yes, here; slide HTML already listed every finding |
| A doc's `doc.json` | `get_artifact_format`, generated | yes, here |
| A design's `canvas.json` | `get_artifact_format`, generated | yes, here |
| A dashboard's `dashboard.json` | `get_artifact_format`, generated | yes, here; an engine argument or a calc operand is read by its own reader, which names its own fault |
| A page (`html`) | the design contract in `skill:artifacts` | yes, the linter already listed every finding |
| `answer_brief` | the catalogue schema, which is also its validator (#407) | yes (#407) |
| Other assistant catalogue tools, and a page action's arguments | the catalogue schema, which is also its validator | yes: `Tool.Check` and `ActionArgs.Check` name every problem, and `Resolver.Prepare` names every name a call cannot place beside them |
| `submit_work`, `submit_review`, a worker's submit (`structured.Remarshal`) | the hand-written schema, held to its struct by a walk, and checked through `tools.Invoke`, whoever calls | yes: accepted as before, every fault named |
| A `decision` on a work item (`comment_on_work_item`, `create_work_item`) | the tool's schema, checked through `tools.Invoke`, whoever calls | yes: every rule's fault and every missing reference named |
| `create_work_item`, `update_work_item`, `write_work_catalogue`, `set_artifact_schema`, `write_artifact_data` and the other first-party seat tools | the advertised schema, checked through `tools.Invoke`, whoever calls | yes: a misread refused before the tool runs; every other finding on its answer |
| Configuration written through a tool (an assistant proposal) | the route's own check (ADR-0055) | yes, the config validator collects every problem |

## What this does not decide

Refusing an unknown argument, or any finding the tool tolerates, before the
tool runs is a later decision per tool, made when that tool's
`crewlet.tools.args_outside_schema` count reads zero; this record decides
that the contract and the validator share one source, that every fault comes
back at once, and that a misread is refused before it is written. It does not decide a
turn's round budget, which the loop guard leaves alone: the guard speaks to
the model, and the round cap still ends the run.

The brand's own name is allowed in its text as written and as a domain
(`GoPhrase.me` for the brand `GoPhrase`). The company record's name is not
consulted: an adoption reads the kit again from its snapshot, and a rule that
read a value held outside the bytes it judges would let a rename turn a kit
already published into one its own reader refuses.

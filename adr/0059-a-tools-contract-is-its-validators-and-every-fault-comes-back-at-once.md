# ADR-0059: A tool's contract is its validator's, every fault comes back at once, and a refusal loop is stopped in words

- **Status:** accepted
- **Authority:** `internal/jsonform`
- **Enforced-by:** `internal/jsonform.TestEveryFaultComesBackInOnePass`, `internal/jsonform.TestAKeyIsMatchedExactly`, `internal/artifacts/brandformat.TestTheReferenceCoversEveryKeyOfTheIndex`, `internal/artifacts/brandformat.TestSixFaultsComeBackInOneRefusal`, `internal/artifacts/brandformat.TestTheBrandsOwnDomainIsItsName`, `internal/artifactupload.TestTheGP38KitGetsEveryFaultInOneRefusalAndLandsNext`, `internal/artifactupload.TestAKitWrittenFromTheFormatReferenceLandsFirstTime`, `internal/agent/builtin.TestEveryValidatedKindsFormatIsOneReadAway`, `internal/agent/builtin.TestAMissedToolSkillNamesTheKeysThatExist`, `internal/agent/runner.TestTheGP38LoopIsStoppedAndCounted`, `internal/tools.TestARunOfPolicyRefusalsIsStoppedInWordsAndCountedOnce`, `internal/tools.TestASuccessEndsTheRun`, `internal/tools.TestARunIsCountedPerTarget`, `internal/tools.TestParallelRefusalsCountEachCallOnce`, `internal/agent/builtin.TestAWriteNamesTheTargetItsRefusalsAreCountedOn`, `internal/jsonform.TestTheKeysAreEncodingJSONs`, `internal/jsonform.TestAnOwnReadersRefusalIsAFaultAtItsPath`, `internal/artifacts/canvasformat.TestACanvasStoredWithAStrayBracketStillReads`, `internal/jsonform.TestOnlyThePublishReadsAVersionStrictly`, `internal/jsonform.TestThePublishReadRefusesWhatTheStoredReadAdmits`, `internal/artifacts/brandformat.TestTheBrandIndexAdmitsTheKeysItAlwaysDid`, `internal/artifactupload.TestOnlyAnIndexThePublishWritesIsHeldToTheExactKeys`
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
| Other assistant catalogue tools | the catalogue schema, which is also its validator | no: `Tool.Check` answers the first problem; the next change |
| `submit_work`, `submit_review`, a worker's submit (`structured.Remarshal`) | the hand-written schema, held to its struct by a walk | no: `DisallowUnknownFields`; the next change |
| A `decision` on a work item (`comment_on_work_item`, `create_work_item`) | the tool's schema | no: `DisallowUnknownFields`, then the first rule; the next change |
| `create_work_item`, `update_work_item`, `write_work_catalogue`, `set_artifact_schema`, `write_artifact_data` and the other first-party seat tools | a hand-written JSON schema nothing enforces | no: each tool answers its first fault; the next change checks every first-party tool's input against the schema it advertises, at the surface, in one pass |
| Configuration written through a tool (an assistant proposal) | the route's own check (ADR-0055) | yes, the config validator collects every problem |

## What this does not decide

The next change in this series decides how every first-party tool's arguments
are checked against the schema it advertises; this record decides that the
contract and the validator share one source and that every fault comes back
at once, and lands that for the closed artifact formats. It does not decide a
turn's round budget, which the loop guard leaves alone: the guard speaks to
the model, and the round cap still ends the run.

The brand's own name is allowed in its text as written and as a domain
(`GoPhrase.me` for the brand `GoPhrase`). The company record's name is not
consulted: an adoption reads the kit again from its snapshot, and a rule that
read a value held outside the bytes it judges would let a rename turn a kit
already published into one its own reader refuses.

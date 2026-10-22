# ADR-0040: Content a model reads carries its provenance

- **Status:** accepted
- **Authority:** `internal/provenance`
- **Enforced-by:** `internal/provenance.TestAFenceCannotBeClosedByItsContent`, `internal/agent/builtin.TestEveryUntrustedReadIsFenced`, `internal/agent/builtin.TestEveryArtifactToolResultIsFenced`, `internal/engine.TestEveryWakeSurfaceFencesWhatSomebodyWrote`, `internal/engine.TestEveryPromptBuilderIsAWakeSurface`, `internal/engine.TestEveryWakeTypeFencesSomebodyElsesWords`, `internal/engine.TestTheTurnsAskIsFencedAsItsTriggerIs`, `internal/agent/prefetch.TestTheTurnStartJudgementsReadSomebodyElsesWordsFenced`, `internal/knowledge.TestRenderFencesEveryHitUnderItsBackendsLabel`, `internal/estate.TestAKnowledgeHitsLabelCrossesTheWire`, `internal/compact.TestARewriteReadsItsTextFencedUnderATokenOfItsOwn`, `internal/learning.TestAPastTurnsAskIsFencedAsWhoeverWroteIt`, `internal/agent/prefetch.TestAPastTurnsAskIsFencedWhereverItIsRead`, `internal/agent/builtin.TestQueryEpisodesFencesWhatAPastTurnWasAskedAsWhoeverWroteIt`, `internal/learning.TestEveryPostTurnWorkerReadsWhatItsTurnWasToldFenced`, `internal/agent/prefetch.TestEveryBlockDefusesWhatItReads`, `internal/agent/builtin.TestWhatASeatRemembersReachesItDefused`, `internal/agent/builtin.TestAnAnswerReadsItsSourcesAsTheirWritersWords`, `internal/agent/extension.TestTheJudgeReadsForeignWordsUnderItsOwnTokenAndItsReasonIsDefused`, `internal/agent/runner.TestTheJudgeIsToldTheTurnsTokenAndItsReasonReachesTheSeatDefused`, `internal/provenance.TestEveryModelCallIsAccountedFor`, `internal/provenance.TestTextOnAHostedSurfaceIsTrustedByOneRule`, `internal/provenance.TestEveryTrustDecisionIsAccountedFor`, `internal/engine.TestEveryWakeSurfaceReadsAnUnnamedWriterByWhoHostsIt`, `internal/agent/prefetch.TestAThreadsUnnamedSenderIsReadByWhoHostsIt`, `internal/agent/builtin.TestAReadNamingNoWriterIsAColleagues`, `internal/notify.TestAHostedSurfaceVouchesForAWriterItsRecordDoesNotName`
- **Cost-when-tried:** before this record, a page body, a work item's comments, a project file and the chat thread handed to a turn all reached the model as plain text in a tool answer or a prompt block, with no boundary the text could not also write and nothing saying who wrote it or that it was data. Anybody who could write a page, comment on an item or speak in a thread a seat reads could address the seat in the voice of its instructions.
- **Tag-status:** unreleased

## The decision

**Text a model reads that somebody other than the reading seat wrote is data,
never an instruction, and every read result says so.** One package,
`internal/provenance`, is the only implementation:

- a **fence** around the foreign text, bounded by a token drawn per turn from
  a cryptographic source and carried on the turn's context, so content cannot
  close the fence by guessing its marker;
- **labels** on the fence: the source (a page body, a work item comment, a
  project file, a chat message, a chat room's own topic or purpose, an artifact
  file, a datastore row, the adopted brand's voice, a note somebody wrote about
  a change they made, a monitor's alert, a notification from an integration
  that names no surface of its own, a colleague's ask or answer on an agent-to-agent channel, and what the Dashboard Assistant reads as
  the person it acts for: a route's answer, what that person's open screen says
  about itself and a deep-work run's account of what it did), who wrote it (by
  kind and seat handle),
  and how far the reader may trust it (its own words, a colleague's, an outside
  party's, a page anybody may edit, or what an administrator adopted for the
  company, which governs tone and wording and never the task, the tools, the
  recipients or access);
- for a tool whose answer is structured, an **envelope** carrying the same
  trust and authors beside the payload;
- one **standing sentence** in every seat's prompt, owned by the package,
  saying what a fence means.

It applies to every read that hands a model somebody else's words: page bodies,
work item comments, a work search's snippets, project files, the chat thread block a turn is given, the
body every wake hands a seat on every surface (a message, a comment, a
description, a change's note, an alert, an unprompted integration's text), a
colleague's ask and answer, and
what the chat tools read (a room's messages, a thread, a search's snippets, a
room's topic and purpose), the knowledge base's search hits (the turn-start
block and `search_knowledge`, each page's title and snippet, labelled by the
backend that found them and carried with the hit wherever it is rendered),
and, from their first release, artifact files and datastore rows. A wake's trigger is built before the turn that reads it exists,
so it is fenced under a token minted for the wake, which the turn moves onto its
own; the notification service fences the body before the surface's prompt
renders it, so a prompt is never handed the words bare. A wake names a body's
writer only where its payload says one person wrote every word, and otherwise
names nobody. What nobody named earns depends on whose surface the words are
on, and there is ONE RULE for a surface the engine hosts — its own tracker,
knowledge base, chat, artifacts, project files and agent-to-agent channels —
whichever door the text comes through (a wake, a read tool, a search hit, the
thread handed to a turn): every write there is authenticated as a company
principal, so text whose record names every writer earns what those writers
earn, and text whose record names some or none of them is never `own` and,
with nobody named, a colleague's (`provenance.TrustForHosted`); a writer such a
record carries no handle and no kind for is one it could not name, never an
outside party. On a vendor's surface, which the company shares with the rest
of the world, nobody named is `external`. Before the rule one Doclet page
reached a seat as `external` through its wake and as a colleague's through a
search, so a text's trust was decided by the door it came through; now every
door that decides a trust is found by a walk and held to the rule its surface
takes, the adopted brand's voice (the one hosted text an administrator vouches
for) to `company` alone, and what a page's script reports it applied (which
the engine cannot verify) to `page` alone. A chat reply that answers a coding run
parked on a question is recorded as what was written, and the run it carries
on reads it fenced as the trigger labels the same body, under its own turn's
token: a fence is a model's, and what a person reads back as their reply
carries none. The same words reach the models that
judge for a turn before it starts — the memory filter, the knowledge-query
writer and the episode summary, each handed what the turn was asked — fenced as
the trigger labels them, under a token minted for that call rather than the
turn's, beside the standing sentence; what those calls answer decides what the
seat is shown, and the briefing the summary writes is defused before it reaches
the seat's prompt. The label each of those words was read under is a value
(`provenance.Label`) decided once, where the delivery is in hand, and carried
on the turn's record of what it was asked — each inbound body's, and the ask's
merged — across a suspension too, and onto the episode the turn writes, so
every reader after the turn fences the same words the same way rather than
deciding again from what it can no longer see: a seat recalling a turn a month
on (the turn-start block of similar prior work, the summary that briefs it,
`query_episodes`) reads what that turn was asked fenced under the label it was
first read with, a stranger's message as a stranger's and never as its own
memory, and one rendering of a past turn serves all three. The models that
learn from a turn after it ends — the ones that write a fact, a trait, a skill
or a practice note into the seat's memory — read what the turn was told the
same way, each message under the label the turn read it under and the ask
under the turn's record of it, under a token minted for that call and beside
the standing sentence, because what they write the seat later reads as its
own — and what they wrote, wherever it is handed back to a seat (a memory, a
trait, a skill, a past turn's account), is defused under the turn's token,
since a model that read a fence can echo it. So does every other model call
that reads somebody else's words on the seat's or a person's behalf: an
answer written from the company's pages, and the judge that decides whether
an exhausted onboarding pass gets more rounds, which reads the fences of the
turn's ask moved onto its own call's token and whose reason reaches the seat
defused. Every
function in the tree that builds a model's input, a provider request or the
launch of a coding CLI's run (every seat's executor, and the Dashboard
Assistant's deep work), names the test that holds its fences, or why it reads
nobody's words, and a walk refuses one that does not. Two things reach a
seat's run unfenced on purpose, defused: an answer given by turn and a note a
person sends the turn. Each is the instruction of a person the engine let
direct that turn (the run's requester, its seat's lead or an administrator
answers; a note is sent on the authority decided on the seat's sealed
anchor), sealed on the run's record with who gave it
([ADR-0031](0031-every-state-log-record-is-signed.md)'s amendments,
[ADR-0067](0067-a-note-to-a-coding-run-waits-sealed-on-its-record.md),
[ADR-0068](0068-a-run-asks-a-person-in-the-shape-a-decision-is-asked.md)), and
a fence would tell the run not to follow the instruction it waited for. A model that rewrites a text to fit a budget reads it the
same way, fenced under a token minted for that rewrite and labelled as its
caller says, and its rewrite is defused before the caller places it. A tool that returns such text and does not
go through the package fails a walk, and so does a wake surface or a kind of
wake whose foreign text does not reach the turn fenced, and a type anywhere in
the tree that builds a wake's trigger without being one of those surfaces. A schedule's task is
the company's own instruction to the seat, not somebody else's words, and is
not fenced.

**Fencing reduces prompt injection and does not remove it.** What holds whatever
a model does is structural, and stays where it is: the requester ceiling on
every artifact tool, human-only rows and `humans_only` fields, the rule that an
agent may tighten a datastore rule and never loosen one, and the rule that an
agent never widens a share to the whole company
([ADR-0034](0034-an-artifact-is-shared-and-a-share-is-a-relation.md)).

## Why the obvious alternatives are wrong

**Labelling artifact reads only**, where the new risk arrives, leaves the same
gap open on every surface that already had it, and gives the engine two ways
to present foreign text, which is the drift
[ADR-0008](0008-a-shared-rule-gets-one-implementation.md) exists to prevent.

**A fixed marker**, the same delimiter on every turn, is a marker the content
can contain: a page that writes the closing delimiter and then an instruction
has stepped outside the fence. A per-turn random boundary cannot be guessed by
text written before the turn began.

**Stripping or escaping instructions out of the content** needs a grammar of
instructions, which natural language does not have, and silently changes what
the seat reads, so a seat summarising a page would summarise something its
author never wrote.

## What this does not decide

It does not decide **what a seat may do**: every write still passes the
authority table, so a seat a fence failed to protect can do no more than its
relations and the requester's ceiling allow. It does not decide **the text an
artifact trigger carries**, which is engine-composed and arrives with the phase
that builds triggers. And it does not decide **what a person sees**: the
dashboard renders foreign text with its own element-constructing renderer, and
a fence is a model's concern.

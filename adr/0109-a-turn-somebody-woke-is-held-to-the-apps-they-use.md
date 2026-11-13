# ADR-0109: A turn somebody woke is held to the apps they use

- **Status:** accepted
- **Authority:** `internal/authz`
- **Enforced-by:** `internal/authz.TestARequesterWithoutTheAppRefusesItsVerbs`, `internal/authz.TestTheAskedArmAdmitsTheRequesterTheAskNames`, `internal/authz.TestAStrangerHoldsNoApp`, `internal/authz.TestARequesterWhoMayNotActHoldsNoApp`, `internal/authz.TestARowNamingNoAppIsNotBoundedByTheApps`, `internal/authz.TestAnAgentsOwnEditsAreSteeredAndItsReadsAreNot`, `internal/agent/builtin.TestEveryAppVerbIsHeldToTheRequestersApps`, `internal/agent/builtin.TestAnInToolDecisionIsHeldToTheCeiling`, `internal/agent/builtin.TestAPlainBuiltinStillAnswersOutsideATurn`, `internal/agent/builtin.TestOneCallComposesItsRequesterOnce`, `internal/tools.TestASeatsCallTellsATurnBoundToolItsTurn`, `internal/engine.TestTheRequesterCarriesTheirApps`, `internal/estate.TestARequesterCrossesAsOneSeatsAnswer`, `internal/estate.TestTheRequesterReadCarriesGrantsAndNothingElse`, `internal/estate.TestARetiredIdentityQuestionIsLeftUnserved`, `internal/iamdomain.TestTheRequesterIsTheSeatHolderWithTheirGrants`, `internal/agent/turnctx.TestASeatHoldsEveryAppAndNeverAdmin`, `internal/agent/builtin.TestATokensWorkUsesTheAppsItHolds`, `internal/agent/builtin.TestACredentialIsBoundByTheNarrowerOfItsWriteAndThisNode`, `internal/engine.TestACredentialIsPlacedByItsLoginsNamespaceAlone`, `internal/engine.TestACredentialsTurnUsesOnlyWhatEveryOneOfItsWritesCarried`, `internal/engine.TestAFirstPartyNoticeIsFromWhoeverWroteIt`, `internal/notify.TestACredentialsWriterCarriesOnlyTheAppsItsRecordNames`, `internal/iam.TestActorAppsAreACredentialsAlone`, `internal/notify.TestOneTurnWorksForOneWriter`, `internal/notify.TestEveryWriterIsAPartitionOfItsOwn`, `internal/engine.TestAPersonAndAStrangerInOneThreadAreTwoTurns`, `internal/agent/prefetch.TestAnotherWritersWaitingMessageIsOwedItsOwnTurn`, `internal/agent/builtin.TestWhoAHandOnWorksForIsTheHandingTurnsOwnCeiling`, `internal/agent/builtin.TestAHandedOnTurnIsHeldToWhoeverItCarried`, `internal/agent/builtin.TestAColleaguesAskCarriesWhoeverTheAskingTurnWorksFor`, `internal/engine.TestAHandOnWakesATurnWorkingForWhoeverItCarried`, `internal/engine.TestAnAnswerIsWorkedForWhoeverItsAskWasWorkedFor`, `internal/events/types.TestAnAgentsWriteStatesWhomItWasMadeFor`, `internal/events/types.TestWhomAWriteWasMadeForCrossesTheWireAsStated`, `internal/agent/builtin.TestAWriteCarriesWhoeverTheWritingTurnWorksFor`, `internal/agent/builtin.TestAWriteWhoseArgumentsNameSomebodyGetsNothingWider`, `internal/agent/builtin.TestAWriteWhoseRequesterCannotBeComposedWritesNothing`, `internal/worklet.TestAnAgentsWriteThatWakesSomebodyCarriesWhomItWasWrittenFor`, `internal/doclet.TestAnAgentsPageWriteThatWakesSomebodyCarriesWhomItWasWrittenFor`, `internal/chatlet.TestAnAgentsMessageThatWakesSomebodyCarriesWhomItWasWrittenFor`, `internal/doclet.TestAPageActorCrossesTheWireAsItWasStated`, `internal/chatlet.TestAChatActorCrossesTheWireAsItWasStated`, `internal/estate.TestATrackerActorCrossesTheWireAsItWasStated`, `internal/estate.TestAPlainlyEncodedTypeWritesItsPlainEncoding`, `internal/estate.TestAnActorThatCannotWriteIsRefusedBeforeAnythingIsAsked`, `internal/estate.TestEveryOperationHoldingAPageOrChatActorRefusesOneThatCannotWrite`, `internal/estate.TestAWriteWhoseActorCannotWriteIsRefusedBeforeAnyPeerRunsIt`, `internal/estate.TestAnAgentsWriteFromABuildWithoutTheCarrierRunsAsItsSeatsOwn`, `internal/engine.TestAnAgentsTrackerWriteFromABuildWithoutTheCarrierRunsForNobody`, `internal/estate.TestAnUnknownMemberInsideARoutedActorIsRefusedAtItsPath`, `internal/worklet.TestAHistoryRowKeepsWhomAnAgentsWriteWasMadeFor`, `internal/chatlet.TestAPostKeepsWhomAnAgentsMessageWasWrittenFor`, `internal/worklet.TestARecordALaterTurnReadsCarriesWhomItWasWrittenFor`, `internal/worklet.TestACredentialsWriteCarriesTheAppsItUsedHere`, `internal/doclet.TestAPageWriteCarriesWhomItWasWrittenForWhereItMayWakeAnAgent`, `internal/chatlet.TestAMessageCarriesWhomItWasWrittenForWhereItRoutesToSomebody`, `internal/estate.TestAWriteForSomebodyMovesOffADataNodeWithoutTheCarrier`, `internal/notify.TestAnAgentsWriterCarriesWhomItWroteFor`, `internal/notify.TestEveryWhomAnAgentWroteForIsAPartitionOfItsOwn`, `internal/engine.TestATurnAWriteWakesIsHeldToWhoeverTheWriteWasMadeFor`, `internal/engine.TestARequesterRidesEveryHopOfWhatSeatsWrite`, `internal/engine.TestOneSeatsWritesForTwoRequestersWakeItsColleagueTwice`, `internal/engine.TestAWriteWakesATurnWorkingForWhoeverItWasWrittenFor`, `internal/engine.TestATurnAWriteWokeIsStillItsRequestersWhenItResumes`, `internal/engine.TestAQueuedCloseIsWrittenAsItsEnqueuer`, `internal/chatlet.TestWhomAMessageWasWrittenForStaysOffItsReads`, `internal/chatlet.TestAWritesAnswerHandsItsMessageOverWithoutWhomItWasWrittenFor`, `internal/worklet.TestACredentialsAssignmentCarriesItsAppsOnlyWhereItMovesTheAssignee`, `internal/worklet.TestAPickupWorksForWhomItsAssignmentWasWrittenFor`, `internal/worklet.TestWorkASeatFiledForSomebodyIsTheirs`, `internal/worklet.TestATakeMadeForSomebodyIsWorkedForThem`, `internal/worklet.TestWorkHandedBackToTheSeatItWasDoneForIsItsOwn`, `internal/agent/prefetch.TestAWaitingMessageWrittenForAnotherRequesterIsOwedItsOwnTurn`, `internal/agent/prefetch.TestAWaitingMessageIsMatchedToWhomTheTurnWorksForNotToItsTriggersPost`, `internal/chatlet.TestTheTurnStartThreadReadHandsTheEngineWhomEachMessageWasWrittenFor`, `internal/estate.TestOnlyTheEngineReadsWhomAMessageWasWrittenFor`, `internal/estate.TestOnlyTheEngineReadsWhomAQueuedChangeWasEnqueuedFor`, `internal/estate.TestATurnThreadNoDataNodeServesFallsBackToThePlainThreadAndSaysSo`, `internal/sandbox.TestAReplyAnswersOnlyARunWorkingForWhomItWasWrittenFor`, `internal/agent/builtin.TestEveryCodeWriteStatesWhomItIsMadeFor`, `internal/agent/builtin.TestACodeWriteWhoseArgumentsNameSomebodyGetsNothingWider`, `internal/gitlet.TestACodeWriteWhoseCarrierAndCeilingDisagreeIsRefused`, `internal/gitlet/host.TestAnEngineWriteLandsCarryingWhomItWasMadeFor`, `internal/estate.TestEveryCodeWriteRefusesAnAgentsRequestThatStatesNobody`, `internal/estate.TestATypeThatDecodesItselfAndWritesNoEncodingIsWalkedInto`, `internal/estate.TestACodeWriteFromABuildWithoutTheCarrierIsMadeForNobody`, `internal/estate.TestACodeWriteForSomebodyMovesOffANodeWithoutTheCarrier`, `internal/estate.TestACodeWriteForACredentialMovesOffANodeThatDoesNotKnowItsApp`, `internal/estate.TestACodeWriteMovesOffANodeThatDoesNotKnowItsCarriersKind`, `internal/estate.TestAnInProcessCommitterAnswersAStoredCarriersUnknownAppAsAnotherBuild`, `internal/estate.TestAnInProcessCommitterAnswersAStoredCarriersUnknownKindAsAnotherBuild`, `internal/estate.TestAnInProcessGitletNodeAnswersACarriersUnknownAppAsAnotherBuild`, `internal/engine.TestARunsOpenThatRanNowhereComesBackNotOpenedAskedOnce`, `internal/gitlet.TestACodeRecordCarriesWhomItsAgentWroteItForWhereItWakesASeat`, `internal/gitlet.TestAnOpenAPushAMergeAndAnEnqueueCarryWhomTheyWereMadeForWhateverTheyWake`, `internal/gitlet.TestACodeRowKeepsWhomItsWriteWasMadeFor`, `internal/gitlet.TestARevisionsFactsWakeForWhomItsRevisionWasPushed`, `internal/gitlet.TestAFailedCheckWakesForWhomItsHeadWasPushed`, `internal/gitlet.TestALandingTheHomeFoundWakesForWhomItsMergingPushWasMade`, `internal/gitlet.TestAStoredCarrierThisBuildCannotReadIsAStrangers`, `internal/gitlet.TestACodeWakeIsWrittenForWhomItsAgentWroteIt`, `internal/gitlet.TestAQueuesLandingWakesForWhomItsEntryWasEnqueued`, `internal/gitlet.TestAnEjectionAndAnUnmadeCloseWakeForWhomTheirEnqueueWasMade`, `internal/gitlet.TestTheHomesQueueWorkSaysWhomEachEntryWasEnqueuedFor`, `internal/gitlet/home.TestTheHomeClosesAnEntryForWhomItWasEnqueued`, `internal/engine.TestAQueuedCloseIsDecidedWithinTheAppsOfWhomItsEnqueueWasMadeFor`, `internal/engine.TestAnEngineRecordAboutAnAgentsActIsWorkedForWhomTheActWasMadeFor`, `internal/estate.TestAHomesQueueReadIsReadWholeOrRefused`, `internal/gitlet/host.TestABoxPushCarriesWhomItsRunWorksFor`, `internal/gitlet/host.TestABoxACredentialsTurnLaunchedPushesOnlyItsSeatsOwnWork`
- **Measured:** `iamdomain.BenchmarkRequester`: one seat's requester, its holder and the holder's grants, is about 75 µs a question at people=100 and at people=10000 alike (beside as many removal tombstones), against about 64 µs for the seat's holder alone before. The document's decode adds about a fifth to the seeks, which still dominate. So the grants are read from the document the row already carries, and no column holds them apart.
- **Cost-when-tried:** the ceiling before this gave every person every app (`internal/engine/requesters.go`: "holding every app and never admin … and never the grants"). A person the company kept off Worklet, Doclet, Chatlet or authoring artifacts could message any agent and have it use the app for them. ADR-0034's "writes for them only what they could write" was false for the authoring app. Worklet, Doclet and Chatlet tools were never decided for the requester at all.
- **Tag-status:** unreleased

## The decision

**Inside a turn somebody woke, a seat uses only the apps that somebody uses.**
- Every verb whose authority row names an app, taken by a seat in a turn a principal woke, is admitted only when the requester holds that app.
- For a `ClassAnyApp` row, any one of its set admits them.
- For a row that admits without the app (`ClassAppOrAsked`: an open ask names the requester), the same terms admit them that admit them asking for themselves.
- It holds for Worklet, Doclet, Chatlet and the artifacts app, and for every app `iam.Apps` gains.
- A refusal is `ReasonCeiling`. It names the app the requester lacks in `Decision.RequesterGrants`, never in `Grants`: the agent holds the app, and what changes the answer is an administrator giving it to the requester. The agent is told so, with the app named.

**Who the requester is** is what woke the turn (`builtin.ceilingOf`).
- **An agent seat** holds every app and never admin (ADR-0033).
- **A person** holds the apps their grants name, through `iam.Expand`. Admin is never part of a ceiling, a grant this build does not know grants nothing, and a person whose stage may not act holds none.
- **A credential acting as no seat** (a Tier A token, or a service account bound to no seat) that woke the turn with a change on a work item or a page holds the apps its own grants name, never admin, narrowed twice:
  - by what the node that took its write resolved for it, which a record a later turn reads carries beside the credential's login (`iam.ActorApps`, Worklet's and Doclet's `MutationRecord.ActorApps` at record version 2, `NotificationWriter.Login` and `Apps` on the wake): in Worklet an item's creation and every move of its assignee, whatever they wake, since the work pickup reads both, and any other record only where its wake proves an agent other than the writer may start a turn (the rule the carrier rides by, below); in Doclet a record that wakes somebody. A login, never the token's value. A record that carries none stays at the base version, and a gate never carries any, so a gate stays readable by every build.
  - and by what the node running the call resolves the login to now (`builtin.Requesters.Credential`), per call as a person is read.
  - A `token:` login resolves from that node's own Tier A and nothing else, and any other login from the identity directory and nothing else. A login it cannot place holds none, as a stranger does: one its Tier A does not declare, a service account removed, suspended or bound to a seat. So does a write that carries no apps, which an older build wrote. A credential acting as a person is that person.
  - Nobody is asked anything for a credential: a question its turn's executor puts to "the requester" goes to the seat's lead chain, as one in a turn nobody woke does.
- **A human seat nobody holds in the directory** holds none: a permission is a person's (ADR-0033), and a seat the chart alone speaks for has no person. To have agents act for somebody, an administrator creates them on their seat with the apps they use. The create lands them active, and they need never sign in.
- **Somebody the company does not know** holds none, as they open no artifact.
- **A turn nobody woke** is bounded by nothing beyond the seat's own authority.

**The app is decided before anything else is decided for the requester.**
- `authz.DecideWithinApps` reads the same row `Decide` reads (`rule.app`, `rule.anyOf`), never a table of its own.
- `authz.DecideWithin` asks it before its steerable-and-owned spare. So the spare, which lets anybody steer an agent's own work, decides relations among the app's users, never whether somebody uses the app.
- No other relation of a row is decided for the requester here. Their own record, their lead and the container stay the seat's own. The families that hold a requester's relation (artifacts, the datastore, code) go on holding it as their records say.

**One composition per call, at the one gate.**
- Every builtin a seat surface calls passes the gate with the turn: the turn's own loop, a delegate worker and the executor's bridged calls all invoke as the seat. A read-only builtin is told its turn through `tools.TurnBound`, and a seat-scoped one through `CallForTurn`.
- The gate binds the turn's ceiling to that call. It composes the requester at most once, and only when a decision of the call first names an app or decides a relation for them.
- The one Authorizer decides every action of the call against it: the tool's own row and every row the tool asks inside itself.
- A seat's decision with nothing bound is refused as undecidable, the closed end as `CeilingKind`'s zero is: only a path that skipped the gate reaches the authority unbound, and reading it as a turn nobody woke would grant every app in silence. Every call a turn makes passes a gate, which binds it, and a caller deciding a seat's call outside any turn on purpose says so (`builtin.OutsideAnyTurn`): the offering question asked before any call (`builtin.Admits`, and `builtin.Withheld` through it) does.
- A builtin called outside a turn (the operator's surface, the HTTP write surface, the Dashboard Assistant acting as the person, ADR-0050) is decided for its caller alone, as before: a principal acting through their own credential is no deputy.

**The person's grants are read per call, from one seat's row.**
- `identity.requester` answers one seat's holder with the person's grants, read in one snapshot on a data node, and nothing else of the person: never a credential, a verifier or a sealed field.
- It costs the seeks the seat's holder cost, plus one decode of one document (Measured), and does not grow with the people the company has hired or removed.
- It is never cached across calls: an administrator's revocation applies at the turn's next call.

**A node that cannot read the requester refuses,** with a sentence that says to try again.
- A node that does not know the read leaves it unserved, and an asker no node answers treats it as unknown.
- An older build's answer is never read as "holds no app" or as "holds every app": the read takes a new name rather than reshaping the old one's answer, and a later change to its meaning takes another.

**The bound is the turn's, and every turn it hands on to.** A colleague the seat asks with `a2a_ask` answers in a turn worked for the same requester, and so does a colleague its write wakes on the engine's own surfaces (the two amendments below). What the agent already knows, it may go on to say: the ceiling bounds what a turn does for its requester.

## Why the obvious alternatives are wrong

**Leaving the ceiling app-blind** kept every app's permission a door with a side entrance. ADR-0033 says what keeps a capability from a person is that it belongs to a permission. That held at every route, and every agent the person could message undid it.

**Holding every relation of every row for the requester**, by deciding each family's verbs with the full `DecideWithin`, reads as the uniform answer. It refuses an agent its own inbox, its own priorities and its own record in every turn a person woke, because those rows are decided on the agent's record, which the requester is not. The relation families that need the requester's relation (an artifact's shares, a repository's grant) already decide it, with spares written for them.

**Reading a credential's wake as nobody's** (no ceiling) would keep token-filed work working with no new read. It would also let every narrow token have agents use every app for it: the deputy this record closes, for the credentials a company hands its pipelines. **Reading it as a stranger's** lets the agent use no app for it, so a pipeline could file work no agent can act on.

**Trusting either resolution of a credential alone.** Tier A is per node, and a write's record outlives the grants it was made under. The carried apps alone let a write recorded before its token was narrowed act on the old grants, and let a token the running node narrowed act on the writing node's wider ones. The running node's alone let a token act on wider grants than the node it wrote through gave it. Only the narrower of the two holds both.

**Caching the requester once a turn** saves a read per call. But it keeps acting for a person an administrator shut out mid-turn, which ADR-0088 already refused for code.

**Reshaping `identity.seat_holder`'s answer in place** saves an op name. But an older node's answer would then decode with no grants, which reads as "holds no app" and sends the agent the wrong refusal, unless every caller learns a presence flag. A new name is unserved where it is unknown, which is the closed end by construction.

## What this does not decide

- **What an agent may already know and say.** That is the provenance fences' and the model's, as before (ADR-0040).
- **Tools that are not apps.** A vendor's MCP server acts with the seat's own credential at that vendor, which the vendor bounds.
- **Surfaces that carry no requester.** What a seat writes on a vendor's surface wakes its seat in a turn worked for a seat rather than for whom that write was made for: the amendment "A requester rides on what a seat writes" states the edges.

## Amendment: one turn works for one writer

A turn works for the one principal who woke it, so the messages that wake a seat are grouped into
turns by writer as well as by conversation. The partition function splits a thread's stamped key by
the writer of each message (internal/notify's `KeyOf`): one person's burst is one turn, and a second
person's message in the same thread, or a stranger's, is a turn of its own, held to its own writer.
The stamped key stays the thread's, since a merged event carries it and a parked run's batch is
matched against it. A turn records as worked through only its own writer's waiting messages,
written for the same whom (internal/agent/prefetch's `waitingMessages`): another writer's message,
or one written for another requester, is owed a turn of its own.

**An intersection was refused**, a turn holding two writers to the apps both of them use: every
family's ceiling holds one principal, and a requester composed from two would be a principal nobody
is.

Who may answer a parked question in the conversation is
[ADR-0077](0077-a-tool-call-that-needs-approval-parks-its-run-in-the-shape-a-runs-ask-does.md)'s
amendment.

## Amendment: a requester crosses every ask

A turn somebody woke hands that somebody on to every colleague it asks. The asking turn's ceiling
names who it works for, and the engine composes the carrier from that ceiling where it builds the
ask (`types.WorkingFor` on `types.A2ARequest`): the requester's seat for a person or a colleague
seat, the credential at the apps the ceiling holds it to, or a stranger. It never takes the carrier
from the ask's arguments or anything the model wrote. The colleague's turn reads it before anything
else and composes it again on its own node: a seat as the directory holds it now, so a revocation
since the ask applies; a credential narrowed by that node's resolution, never widened; a stranger as
one. The answer carries it back (`types.A2AMessage`), so the asker's turn the answer wakes works for
them too, and an ask that carried nobody answers for the asking seat itself, whose own work it was.
A chain of asks carries the requester end to end. The stranger-only flag this replaces is gone.

During a rolling upgrade an older node ignores the carrier and works an ask for the asking seat
until the roll completes. The window is minutes, no tag has shipped the earlier rule, and it is
accepted as ADR-0110's was.

## Amendment: a requester rides on what a seat writes

What an agent seat writes on the engine's own surfaces in a turn somebody woke carries that
somebody, so the turn its write wakes works for them too, never for the writing seat, which holds
every app. It covers every write of Worklet, Doclet, Chatlet and Gitlet: an item filed, assigned or
moved, a comment, a page or a comment on one, a message; a change opened, edited, closed or
reopened, an ask to review, a review, a comment on a change, a finding's answer, a push, a merge, an
enqueue and a dequeue, the pushes of the seat's executor's box and the change its run opens
among them (crewlet#798's piece 4c-i;
[ADR-0093](0093-a-box-reaches-git-with-its-runs-credential.md)'s amendment). So does what the
engine records on Gitlet about an agent's earlier push or enqueue: a revision's facts, a failed
check of its head and a landing the change's home found carry whom that push was made for, and the
merge queue's landing, its ejection and a close it could not make carry whom the enqueue was made
for; and the merge queue's close of a landed change's work item, the enqueuing turn's write made
late, is written for them and decided within the apps they use (crewlet#798's piece 4c-ii). A chain
of writes carries the requester end to end, as a chain of asks does.

**Composed by the engine, from the bound ceiling alone.** The carrier is the ask's
(`types.WorkingFor`), composed where a seat's actor is built (internal/agent/builtin's
`writtenForTurn`: `ceilingOf`, then `workingForOf`), never from a tool's arguments or anything the
model wrote: a code tool's request takes it from its caller's actor, and a run's checkout composes
it from the same binding for the run's box and the change its run opens, which the run's row keeps.
A turn whose ceiling cannot be composed writes nothing, refused as unavailable: a write carrying
nobody would wake its seat for the writer with every app.

**Every actor built for a turn states it, and the type refuses one that does not.** The actor's
field is `types.WrittenFor`, whose zero value states nothing: Worklet's writer, Doclet's store and
Chatlet's store refuse an agent's write that states nothing (`types.WrittenFor.For`), and a carrier
on the write of a person, a credential or the engine, who write for nobody but themselves. A seat's
code write states it on the request it forms, since a code record names the node that decided it
as its actor: `WrittenFor` on each of the six (`ChangeProposal`, `ConversationWrite`, `EngineWrite`,
`QueueProposal`, `MergeProposal` and `Proposal`), which every node deciding one holds to its asker
by the same rule and, on the four that carry the ceiling the write is decided under (a change's
lifecycle and its conversation, an engine write and a dequeue), to that ceiling: a colleague's
carrier comes with that seat's ceiling, a credential's with the ceiling of nobody, and a person's
seat with no ceiling (internal/gitlet's `writtenForInvalid` and `writtenForUnder`). A turn's write
states `types.ForTurn` (nobody, for a turn that works for its own seat), and so does a write the
engine makes for a turn after it ended: the merge queue's close of a landed change's item, written
`ForTurn` of whom the entry's enqueue was made for (below); the engine's own write made as a seat
states `types.ForNobody` and says why where it does: the charge of a turn's spend, which wakes
nobody. A
node also refuses to ASK such a write anywhere, in ADR-0025's own idiom for a value a request may not
carry: the router checks the actor before the write is asked of any node, its own copy in process
included, so one answer comes in process and routed alike (internal/estate's `actorWrites`: through
`op.refusedBeforeAsking` on every page and chat write whose arguments hold the actor, a chat reach
being a read, which a gate over the operation registry holds every such operation to, and in
`callAs` for every operation that runs as an actor, the tracker's and `tracker.view_prior` among
them; and its `codeWrites`, through `op.refusedBeforeAsking` on every code write whose arguments
hold a code principal and its carrier, which a gate of its own holds every such operation to). The
actors' codecs carry no policy. What a data node decodes as
nobody is therefore what its sender stated, or an older node's write, and each actor's own decoder
(`UnmarshalJSONFrom`) states it, on the actor and whatever members arrived: internal/estate's `Actor`
whether or not a provenance did, since an older node sends an agent's provenance that carries
nothing as no member at all, and each of the six code requests states its carrier received the
same way. So a data node never refuses a write for a key its sender left out
because it carried nobody. And the actors and the six code requests cross under ADR-0025's rule for
what an operation takes, as every other member of a request does: each has no encoder, so its plain
encoding under the request's options is what crosses, and it and `types.WrittenFor` decode through
the request's own decoder (`encoding/json/v2`'s `UnmarshalJSONFrom`, never an `UnmarshalJSON`, which
starts again from `encoding/json`'s defaults, and returning the decoder's own error unwrapped, so
the wire reads the member's path as the decoder recorded it), so a field at its zero is left off
inside an actor, a request and its carrier, and a member the serving build does not declare there
is refused at its path. So, on a code request, is a credential's app or a carrier's kind the
serving build does not know, which only a newer build composes (the roll windows below).

**On every record a later turn reads, and no other.** `MutationRecord.WorkingFor` rides an agent's
record that a later turn reads to learn whom it works for, a wake now or a pickup later: Worklet at
record version 13, Doclet at 4, Chatlet at 3. Gitlet's code records carry it at version 2, on the
routing and the payload that name their agent, since a code record's actor is the node that
decided it. Worklet carries it on an item's own creation (a create of
kind `created`, never the alias a move claims for the item's old key, which nothing reads) and on
every record whose patch moves the item's assignee, whatever kind it ends with and whatever they
wake, a status move that takes the item included, because the work pickup reads an item's
creation and the rows that moved its assignee (ADR-0112). A reassertion to the seat that already
holds the item moves nothing and no read asks its carrier, so it carries one only as any other
record does. Any other record carries it only where its decide proves from the record's own Notify
that an agent other than the writer may start a turn: in Worklet, some candidate the record offers,
ordinary or the fallback chain's, that hears it and whose notice starts a turn, since the parser's
live roster wakes the lead when the one candidate it heard has left; in Doclet, a change that
offers its container's lead, whom only the parser's chart names, or one that offers anybody but the
writer, an agent's comment that asks nothing only where its thread awaits an asker; in Chatlet, a
record that routes to somebody (`Notify.Recipients`, never whether the message was silent); on
Gitlet, a routing that may wake a seat, beside the agent's actor it names
(`ChangeRouting.WorkingFor`): some concern hears it that is not a person, the repository lead's
fallback counted, since a routing that tells only persons wakes no turn (internal/gitlet's
`wakesASeat`, the one test the code feed relays a record by too). Gitlet also carries it on an
open, a reopen, a push's entry, a merge and an enqueue whatever they wake, since the engine's later
records about them and the merge queue's close read it back from the rows those fill. A
record that carries none, a gate, an erase or a prune always among them, stays at the version every
build reads, and Worklet's `ActorApps` follow the same rule. Worklet's history row (`tracker_history.working_for`, migration
0160) and Chatlet's message (`Message.WorkingFor`) keep it from the carrier's first build on,
applied on every node from the record, for the reads crewlet#798's piece 4b-ii brings: a replicated
migration runs on each node when it opens and not at a log position, so a column or a field a later
build began to fill would hold nothing for the records a node applied before it upgraded and the
carrier for the same records on a node that applied them after. Gitlet's rows keep it the same way
(`working_for` on `code_revisions`, `code_pushes` and `code_queue`, the replicated migration
`a_code_row_keeps_whom_an_agents_write_was_made_for`), for the engine's records about an agent's
earlier write, which carry it beside the actor they name, on the routing that may wake a seat: a
revision's facts and a failed check of a change's head read the revision's row, and a landing the
change's home found the row of the push it names as the merge, each in the statement that reads its
actor (internal/gitlet's `revisionPusher` and `landingPusher`); and the merge queue's records about
an entry read the entry's row their decide reads: its landing carries it beside the enqueuer it
names, and its ejection and a close it could not make beside no actor (below). A landing's push rows
take theirs from its entries' rows, which every node filled from the same enqueue. One read answer
hands it out: the home's work on its queue (`code.queue_due`, read whole,
`gitlet.QueueWorkEntry.WorkingFor`), the read the repository's home works its queue from, whose
close of a landed change's item is made for them; every listing of the queue leaves it out, and
the engine-class guard holds every package that serves a person or a model off that read, as it
holds them off Chatlet's turn-start thread read below.
Chatlet's stays on the stored document
alone: no read view of a message and no write's answer hands it out (`Message.forReaders`), since
a room's reader needs no requester's seat, login or apps, so the engine's turn-start thread read
alone hands it over (`chat.turn_thread`, through the engine-only facade `estate.TurnThreads`), never
a reader's view.
Doclet's carrier stays on its record alone.

**On the wake, and in the partition.** `notify.FirstPartyWriter(kind, actor, apps, workingFor)`
puts it on the wake's writer (`NotificationWriter.WorkingFor`), for an agent's write (Gitlet's
parser hands it the routing's carrier beside the routing's agent actor, internal/gitlet's
`writerOf`), and Chatlet's inbound now states its writer as the other families' do, so its
partition keys on the seat (`seat:`) rather than the actor. The partition function appends the
whole carrier to an agent writer's key, escaped (`notify.ForSeparator`), a credential's apps
included: a turn works for one
writer and for one whom, so one seat's writes for two requesters are two turns. One deliberate
exception keeps the thread as its key: ADR-0120's seen-through mark (`notify.KeyOfAll`, the thread and
never the thread split by writer). A completed turn absorbs a later wake at or below its mark that
obliges the seat to nothing, whatever that wake's writer or carrier. Absorption only drops a wake and
runs nothing for anybody, so nothing widens; but two requesters are two turns only for wakes that
oblige the seat. The woken turn reads
the carrier off the first constituent (internal/engine's `workingForIn`), which the partition makes
every constituent's, and composes it again on its own node as an ask's carrier is. Whom it replies
to stays the writing seat (`requesterOf`).

**The writer rule's one exception: the engine's record about an agent's earlier enqueue.** A
person, a credential and the engine write for nobody but themselves, and the stores refuse a
carrier on their writes. But the merge queue's ejection and a close it could not make, settled
`not_closed`, are records no principal's act made, which name no actor and tell the entry's
enqueuer and its change's authors about that enqueue: worked for the told seat's own, the turn would
fix the conflict and queue the change again, or close the item its requester could not, with every
app the seat holds, one hop after the engine's own close was held to them. So each carries whom the
enqueue was made for beside no actor, where its routing may wake a seat (internal/gitlet's
`concerning.enqueuedFor`), Gitlet's parser hands it to the engine's writer for those two ops alone
(`writerOf`), `notify.FirstPartyWriter` keeps it for the engine as for an agent, the partition keys
it after the engine's own key (`system for seat:ada`), so the engine's records for two requesters
are two turns, and the woken turn reads it as an agent's (internal/engine's `writtenForIn`). No
other engine record carries one: a mergeability and a broken landing name no earlier act made for
anybody.

**The roll windows**, accepted as the ask's was, with no protocol gating; each lasts the minutes of
a roll:
- an older applier retains a record carrying the field at its position, the established deferral;
- an older change feed defers that record's wake to a newer peer rather than dropping it
  (internal/changefeed, crewlet#842);
- an older notify parser cannot read that record either, so it passes the record's wake back to
  the inbound group for a node of the newer build rather than acknowledging it as a payload it
  cannot parse (internal/notify, crewlet#993); a wake no such node took within the group's
  deliveries is kept with the group's unhandled deliveries, and a node of the newer build replays
  it — one already running within a poll of the keep, the next to start otherwise — so the wake
  is late rather than acknowledged away, save the losses internal/notify's Deferral names, and
  never delivered by a node that could not read its carrier;
- an older data node refuses a routed write whose actor carries a requester, naming the key
  (`actor.provenance.WrittenFor` for Worklet, `args.Actor.WrittenFor` for Doclet and Chatlet), and
  runs none of it (ADR-0025's amendment), and an older data node or Gitlet node refuses a code
  write whose request carries one the same way (`args.written_for`). The router asks the next data
  node, or for a code write the repository's next committer or the next Gitlet node, so a node of
  the newer build serves it, and a box's push refused so is pushed again by its harness. Where none
  does, the agent's tool answers `fleet_builds_differ` and nothing
  is written: `save_work_view`'s prior read is refused alike, and `code_merge_change` merges and
  leaves its item open, saying so. A run whose collection meets no committer of the newer build
  comes back with its commits pushed and its change not opened, its report saying that nothing of
  the open ran, and the seat opens it again with `code_open_change`: the collection does not ask
  again, since it holds the run's seat until the turn resumes and the roll may outlast it
  (internal/engine's `ranNowhere`). A write that carries nobody (a turn for its own seat,
  `ForNobody`, a person's, a credential's or the engine's) crosses without the key, and either build
  serves it. Upgrading the data nodes first avoids the refusal, and crewlet-operator rolls the data
  pods first (its `docs/concepts/runtime.md`, "The seat pods follow the data pods"). An older seat
  node composes no carrier, so a newer data node reads its actor as stated nobody, and that write
  wakes its colleague for the writing seat until the roll completes; so does a code write an older
  seat node or Gitlet node sends, which a newer node decides by the ceiling it carries alone, as
  before, and records at code record version 1;
- a node whose build does not know an app a credential's carrier names, or the carrier's kind,
  answers a code write carrying it as it answers a member it does not declare
  (`args.written_for.apps[]`, `args.written_for.kind`), never as malformed, since only a newer build
  composes one: the walk asks the next node until one that knows it serves the write, or answers
  `fleet_builds_differ`. One place decides what a build knows (internal/gitlet's
  `unknownToThisBuild`), and a node's own copy asked in process answers as its wire would, before
  any other rule of the write and whatever else the write breaks, since this build cannot judge it,
  so a landing, and a carrier a run's row stored, reach a newer committer where one serves. A carrier of
  no kind, or a credential's naming no app, stays malformed, since no build composes one. A Gitlet
  node whose build does not know the kind a run's row names answers that run's box
  `503 carrier_unknown`: a node of a newer build judges it, and the harness's push again reaches one;
- an older node deciding a revision's facts, a failed check of a change's head, a landing the
  change's home found, or the merge queue's landing, ejection or settled close reads no carrier back
  from the rows, so the record it writes carries nobody and the turn it wakes works for the pusher's
  or the enqueuer's seat, or the told seat's own, as before the carrier, until the roll completes;
  the same record a node of the newer build decides carries the stored carrier on its routing at
  code record version 2, which the older build reads. A carrier stored under a kind the deciding
  node's build does not know rides its record as the row kept it, never rewritten, and a seat node
  whose build does not know the kind either composes it as a stranger's, the closed end
  (internal/gitlet's `storedCarrier`);
- an older seat node reads no carrier off the engine's writer and keys the engine's wakes as one,
  so an ejection or a settled close such a node takes wakes its seat for the told seat's own, until
  the roll completes;
- the merge queue's home reads its queue work whole or not at all, through an operation of its own
  (internal/estate's `code.queue_due`), since it lands, ejects, closes and settles each entry as it
  read it, and would carry an entry on without any field its build dropped. A home of an older build
  asks that read as a kind of `code.read`, which a data node of the newer build refuses naming
  `args.queue_due`, and a home of the newer build asks an operation an older data node does not
  know, which that node answers unserved; either walk moves on to a data node of the asking home's
  build, and where none is left the home's queue waits, stepping on nothing, until a home and a data
  node of one build meet. crewlet-operator rolls the data pods first, so an older home's queue waits
  for the rest of the roll, and only a roll that upgrades the homes first meets the newer home's
  wait. So a home of the newer build closes each landed change's item for whom the entry's row
  stored its enqueue as made for, never without it; a home of a build that reads no carrier, which
  only a data node of its own build answers, closes for the enqueuer alone, as before the carrier,
  until the roll completes. A carrier whose kind the closing home's build does not know is decided
  not at all: the close stays owed, logged at WARN as the fleet's builds differing, for a home of
  the build that knows the kind (internal/engine's queue close);
- a run launched by a build before the carrier carries nobody on its row, so until the run ends its
  box's pushes and the change its run opens wake whoever they reach for the seat itself, and a
  credential's box keeps the seat's bound alone. Its grants were taken for nobody, and a run granted
  for nobody is never used for somebody, whichever build wrote its row: carried on by a node of the
  newer build in a turn that works for somebody, the run is refused every checkout as a conflict, of
  a repository it holds already as much as of another, and its seat is told that a turn working for
  somebody starts a new run. That fails closed: the run keeps what it was given and pushes it for
  nobody. No field tells its row from one granted for nobody, and none is added, since a node keeps
  no compatibility with an unreleased build: no arm adopts the first carrier a checkout names, and
  no row marks the build that wrote it (internal/sandbox's `sameRequester`). A Gitlet node of such a
  build admits a newer run's box by its colleague's ceiling and ignores the carrier until it
  upgrades ([ADR-0093](0093-a-box-reaches-git-with-its-runs-credential.md)'s amendment).

**The edges.** A vendor's surface (Slack, Mattermost, Jira, GitHub, GitLab, Confluence) carries no
requester: the agent writes through the vendor's MCP server and the wake comes back by webhook with
no field of the engine's, so the turn it wakes works for the writing seat. So does a GitHub pull
request a task waits on reach the task (ADR-0121): internal/engine's `settleChange`, crewlet#921,
settles it in a turnless write the node makes as the engine, which carries nobody because the
vendor's event carries no requester, so the turn it wakes in the item's holder works for the
holder's own seat. A company that wants the
bound to hold keeps agent-to-agent work on the engine's own surfaces: Worklet, Doclet, Chatlet,
Gitlet and `a2a_ask`. On Gitlet, what the engine records about no earlier write carries nobody: a
mergeability and a broken landing wake their seats for themselves. What it records about an agent's
earlier push carries whom that push was made for, as the push's rows keep it: a revision's facts, a
failed check of a change's head and a landing its home found. What the merge queue records about an
agent's enqueue carries whom the enqueue was made for, as the entry's row keeps it: its landing
beside the enqueuer, and its ejection and a close it could not make beside no actor (the writer
rule's one exception, above); and its close of a landed change's work item is written for them and
decided within the apps they use, composed again on the closing node, so a requester who has left
the organisation, or who does not use Worklet, has the close settled `not_closed`, naming them. A
person's push, a repository's import and a pull mirror's sync carry nobody
either, since a person writes for nobody but themselves and the other two wake nobody, and a row
applied before the carrier's build holds nobody, as a record below its version carries nobody (the
legacy edge below), so the facts of a revision pushed then, and the landing of a push made then,
wake their seats for the pusher's seat. The three reads that keyed
on the writer alone take whom a write was made for too (the amendment "a later read takes whom a
write was made for" below). ADR-0120's seen-through mark is not one of them: it stays keyed on the
thread by design (the partition above).

**An optional field was refused**: a site that forgot it would write for nobody in silence, the
side entrance this record closes. **A carrier read from a tool's arguments** would let a model name
anybody. **A carrier on every record** would stamp gates and quiet records at a version older builds
retain for a field nothing reads.

## Amendment: a later read takes whom a write was made for

Three reads that named only a write's writer now take whom it was made for (crewlet#798's piece
4b-ii):
- **The work pickup** (ADR-0112) works for whom the item's newest admitted assignment, else its
  admitted creation, was written for, read off the history row's `working_for`. An agent's row
  written for somebody is worked for them, and any other row for its writer, as before. The seat's
  own row counts only as a creation or a take made in a turn for somebody: its uncarried moves and
  its moves away from itself are never read, so a take-back made on its own initiative reads through
  to the newest move by somebody else. A take made for a wider person, or for an agent seat, works
  for them, by this record's chain rule. A carrier this build cannot read is a stranger's, and one
  naming the seat is its own work.
- **The waiting messages** a turn records as worked through are only its writer's written for the
  same whom, compared with the turn's own carrier as the partition keys it (`notify.WorkingForKey`).
  The carrier reaches the engine through its turn-start thread read alone (`chat.turn_thread`,
  through the engine-only facade `estate.TurnThreads`), and every reader's view keeps stripping it.
  A read that could not say whom its messages were written for marks none.
- **A parked run's answer** in its conversation (ADR-0077's amendment) must be written for whom the
  run works for. A reply written for nobody else qualifies; any other must name the run's principal,
  read as the ceiling reads it before it asks the directory.

**Legacy and the nil edge.** A record below the carrier's version, and every write made for nobody
else, carries nobody, and each read takes it as its writer's, as before: the pickup reads the
writer, a waiting message written for nobody is worked through only by a turn for nobody, and a
reply carrying nobody answers. The nil edge is sound because a record skips its carrier only where
its decide proves that no agent but the writer may start a turn, and a wake held back delivers
nothing.

**The roll windows**, none of them gated:
- **The pickup.** A data node of an older build answers as before: the writer of the last
  assignment by somebody else, else of a creation by somebody else, else the seat's own work. So
  during the roll the seat's own carried creation is worked with its own every-app ceiling, and its
  own take reads the earlier assigner or filer.
- **The thread read.** A newer seat node that reads through older data nodes finds the turn's read
  unserved, reads the plain thread, and marks nothing, the safe direction. An older seat node still
  marks by sender alone, and the completion ledger is the fleet's, so until the last seat node
  upgrades its marks bind every node, for any redelivery within the ledger's retention, even for a
  message written for another requester.
- **The reply.** A seat whose deliveries land on an older node matches by its writer alone, and a
  reply written on a seat node of a build before the carrier carries nobody and matches as the nil
  edge, until the roll completes.

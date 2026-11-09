# ADR-0109: A turn somebody woke is held to the apps they use

- **Status:** accepted
- **Authority:** `internal/authz`
- **Enforced-by:** `internal/authz.TestARequesterWithoutTheAppRefusesItsVerbs`, `internal/authz.TestTheAskedArmAdmitsTheRequesterTheAskNames`, `internal/authz.TestAStrangerHoldsNoApp`, `internal/authz.TestARequesterWhoMayNotActHoldsNoApp`, `internal/authz.TestARowNamingNoAppIsNotBoundedByTheApps`, `internal/authz.TestAnAgentsOwnEditsAreSteeredAndItsReadsAreNot`, `internal/agent/builtin.TestEveryAppVerbIsHeldToTheRequestersApps`, `internal/agent/builtin.TestAnInToolDecisionIsHeldToTheCeiling`, `internal/agent/builtin.TestAPlainBuiltinStillAnswersOutsideATurn`, `internal/agent/builtin.TestOneCallComposesItsRequesterOnce`, `internal/tools.TestASeatsCallTellsATurnBoundToolItsTurn`, `internal/engine.TestTheRequesterCarriesTheirApps`, `internal/estate.TestARequesterCrossesAsOneSeatsAnswer`, `internal/estate.TestTheRequesterReadCarriesGrantsAndNothingElse`, `internal/estate.TestARetiredIdentityQuestionIsLeftUnserved`, `internal/iamdomain.TestTheRequesterIsTheSeatHolderWithTheirGrants`, `internal/agent/turnctx.TestASeatHoldsEveryAppAndNeverAdmin`, `internal/agent/builtin.TestATokensWorkUsesTheAppsItHolds`, `internal/agent/builtin.TestACredentialIsBoundByTheNarrowerOfItsWriteAndThisNode`, `internal/engine.TestACredentialIsPlacedByItsLoginsNamespaceAlone`, `internal/engine.TestACredentialsTurnUsesOnlyWhatEveryOneOfItsWritesCarried`, `internal/engine.TestAFirstPartyNoticeIsFromWhoeverWroteIt`, `internal/notify.TestACredentialsWriterCarriesOnlyTheAppsItsRecordNames`, `internal/iam.TestActorAppsAreACredentialsAlone`, `internal/notify.TestOneTurnWorksForOneWriter`, `internal/notify.TestEveryWriterIsAPartitionOfItsOwn`, `internal/engine.TestAPersonAndAStrangerInOneThreadAreTwoTurns`, `internal/agent/prefetch.TestAnotherWritersWaitingMessageIsOwedItsOwnTurn`, `internal/agent/builtin.TestWhoAHandOnWorksForIsTheHandingTurnsOwnCeiling`, `internal/agent/builtin.TestAHandedOnTurnIsHeldToWhoeverItCarried`, `internal/agent/builtin.TestAColleaguesAskCarriesWhoeverTheAskingTurnWorksFor`, `internal/engine.TestAHandOnWakesATurnWorkingForWhoeverItCarried`, `internal/engine.TestAnAnswerIsWorkedForWhoeverItsAskWasWorkedFor`
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
  - by what the node that took its write resolved for it, which a record that wakes somebody carries beside the credential's login (`iam.ActorApps`, Worklet's and Doclet's `MutationRecord.ActorApps` at record version 2, `NotificationWriter.Login` and `Apps` on the wake). A login, never the token's value. A record that wakes nobody carries none and stays at the base version, so a gate stays readable by every build.
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

**The bound is the turn's.** A colleague the seat asks with `a2a_ask` answers in a turn the seat woke, decided for the seat, which holds every app. That hop is the ceiling's current edge, and crewlet#798's second piece closes it by carrying the requester on the ask. What the agent already knows, it may go on to say: the ceiling bounds what a turn does for its requester.

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
- **The transitive bound.** A requester carried through the asks a turn sets off is crewlet#798's second piece, its own record.
- **Gitlet's grant.** Piece 5 of crewlet#733 makes Gitlet an app, and this record then holds it like the rest.

## Amendment: one turn works for one writer

A turn works for the one principal who woke it, so the messages that wake a seat are grouped into
turns by writer as well as by conversation. The partition function splits a thread's stamped key by
the writer of each message (internal/notify's `KeyOf`): one person's burst is one turn, and a second
person's message in the same thread, or a stranger's, is a turn of its own, held to its own writer.
The stamped key stays the thread's, since a merged event carries it and a parked run's batch is
matched against it. A turn records as worked through only its own writer's waiting messages
(internal/agent/prefetch's `waitingMessages`): another writer's message is owed a turn of its own.

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

**The edge that remains** is a write that wakes another seat: an item filed or assigned, a work-item
comment, a page or a mention in it, a Chatlet message. The turn each wakes still works for the seat
that wrote it. crewlet#798's fourth piece carries the requester through those: the record says whom
it was written for, as a credential's apps do, its wake carries it, and the woken turn reads it.

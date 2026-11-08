# ADR-0103: A turn somebody woke is held to the apps they use

- **Status:** accepted
- **Authority:** `internal/authz`
- **Enforced-by:** `internal/authz.TestARequesterWithoutTheAppRefusesItsVerbs`, `internal/authz.TestTheAskedArmAdmitsTheRequesterTheAskNames`, `internal/authz.TestAStrangerHoldsNoApp`, `internal/authz.TestARequesterWhoMayNotActHoldsNoApp`, `internal/authz.TestARowNamingNoAppIsNotBoundedByTheApps`, `internal/authz.TestAnAgentsOwnEditsAreSteeredAndItsReadsAreNot`, `internal/agent/builtin.TestEveryAppVerbIsHeldToTheRequestersApps`, `internal/agent/builtin.TestAnInToolDecisionIsHeldToTheCeiling`, `internal/agent/builtin.TestAPlainBuiltinStillAnswersOutsideATurn`, `internal/agent/builtin.TestOneCallComposesItsRequesterOnce`, `internal/tools.TestASeatsCallTellsATurnBoundToolItsTurn`, `internal/engine.TestTheRequesterCarriesTheirApps`, `internal/estate.TestARequesterCrossesAsOneSeatsAnswer`, `internal/estate.TestTheRequesterReadCarriesGrantsAndNothingElse`, `internal/estate.TestARetiredIdentityQuestionIsLeftUnserved`, `internal/iamdomain.TestTheRequesterIsTheSeatHolderWithTheirGrants`, `internal/agent/turnctx.TestASeatHoldsEveryAppAndNeverAdmin`
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

**Who the requester is** is unchanged: what woke the turn (`builtin.ceilingOf`).
- **An agent seat** holds every app and never admin (ADR-0033).
- **A person** holds the apps their grants name, through `iam.Expand`. Admin is never part of a ceiling, a grant this build does not know grants nothing, and a person whose stage may not act holds none.
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
- A decision with nothing bound is one no gate made, outside any turn, as `ceilingOf` answers a turn nobody can name. Every call a turn makes passes a gate, which binds it.
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

**Caching the requester once a turn** saves a read per call. But it keeps acting for a person an administrator shut out mid-turn, which ADR-0088 already refused for code.

**Reshaping `identity.seat_holder`'s answer in place** saves an op name. But an older node's answer would then decode with no grants, which reads as "holds no app" and sends the agent the wrong refusal, unless every caller learns a presence flag. A new name is unserved where it is unknown, which is the closed end by construction.

## What this does not decide

- **What an agent may already know and say.** That is the provenance fences' and the model's, as before (ADR-0040).
- **Tools that are not apps.** A vendor's MCP server acts with the seat's own credential at that vendor, which the vendor bounds.
- **The transitive bound.** A requester carried through the asks a turn sets off is crewlet#798's second piece, its own record.
- **Gitlet's grant.** Piece 5 of crewlet#733 makes Gitlet an app, and this record then holds it like the rest.

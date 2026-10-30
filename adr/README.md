# Architecture decision records

A decision that binds **more than one package** is recorded here. A decision
that lives inside one package is recorded in that package's own doc comment,
where `go doc` puts it beside the code — and it does not get a record here.

That single criterion is the whole of the filing rule, and it exists because
this repository has already paid for the alternative. `internal/textcut`,
`internal/whsec`, `internal/jsprovision`, `internal/httpx`,
`internal/api/httpjson`, `internal/tokens`, `internal/procgroup`,
`internal/runtoken`, `internal/clientsource`, `internal/hostbox` and
`internal/solo` all exist for one reason: a rule was written down twice, the
copies disagreed, and nothing compared them. `internal/jsprovision`'s own doc
names the failure exactly — *"five decisions spelled separately, each doc
comment asserting it matched the other with nothing enforcing that it did."*
A tree of records that restated package docs would be that failure with a
directory of its own. So an ADR here never restates its authority; it names it.

## Why these exist at all, when the package docs are this good

They do not exist because the reasoning was missing. Before the first record
below was written, the rule it carries was already stated correctly in
`internal/statelog/doc.go`, in `docs/guides/replication.md`, in
`docs/concepts/architecture.md` and in `CLAUDE.md`. It was still not taken into
account, and the reason is visible in the tree: **every rule here that has a
gate has never been re-litigated, and every rule held by prose alone has
decayed.**

The table below is the evidence, **as it stood when this directory was
created**. It is deliberately a historical record rather than a live one — two
of its rows acquired a gate in the same change that wrote it, and a table
edited to keep up would lose the only thing it is for, which is that the two
columns correlate.

| Rule | Held by, then | What had happened |
|---|---|---|
| No statement names both estates | a static walk | never broken |
| One store driver in the binary | a test | never broken |
| One outbound HTTP transport | a static walk | never broken |
| One writer of a live stream's config | a static walk | never broken |
| A skip is not a pass | `internal/skipgate` | never broken since |
| Every commit is signed off | a script | 61 of 361 commits missed it before the script |
| Fleet-agreement state belongs in coordination | prose | eight tables, four repair migrations — and a ninth found while writing this |
| The replicated estate is written only by an applier | prose | three bypasses, none reviewed as one |
| Every node-local table is swept on every node | prose, at the field that implements it | six of seven sweeps took the wrong default, including the audit log's |
| The Makefile matches `ci.yml` | prose | unchecked, and nothing would notice |

Every rule in the top half was still true. Every rule in the bottom half had
decayed, and the last of them is the sharpest: the rule was written out in full
at the definition of the very field that carries it, and six of the seven call
sites ignored it anyway. Reading is not enforcement.

So the load-bearing field of a record here is **`Enforced-by:`**, and the
record is the preface to the gate rather than a substitute for it. An ADR
whose `Enforced-by:` is `nothing` is allowed, and it must say so in that word,
and it is then listed in `adr.Unenforced` with a reason — two-sided,
so an entry that stops being true fails the build. Writing `nothing` is the
honest answer often enough that hiding it would be the worse outcome; what is
not allowed is leaving the question out.

## The fields, and how they differ from a generic template

A generic ADR template asks for Context, Options Considered, Decision and
Consequences. Four of those survive here under other names; three fields are
particular to this repository and one is missing on purpose.

| Field | What it is for |
|---|---|
| **Status** | `accepted` or `superseded`. A superseded record keeps its number and its text, and gains `Superseded-by:`. Numbers are never reused and a record is never edited into a different decision — the reasoning somebody acted on has to stay readable. |
| **Authority** | The ONE package or page whose own doc holds the detail. The record states the decision and why the obvious alternative is wrong; everything else stays at the authority, and the record is forbidden to restate it. This is the field that stops the tree becoming a second copy. |
| **Enforced-by** | A named test, a compile error, or the literal `nothing`. A test named here must exist. |
| **Measured** | The numbers the decision was settled on. This repository argues with measurements — 1.7 ms to create a consumer, 1 min 44 s against 50 ms for an index, K1 at 1.2, a delivery budget of 25 — and a Pros/Cons list discards the only thing that makes a decision hard to re-litigate later. Omit it only when there was genuinely nothing to measure. |
| **Cost-when-tried** | What the obvious alternative cost *when it was tried here*. Forensic rather than speculative, which is the difference from a generic "Considered Options" section: this repository has usually already run the experiment, and the incident is the argument. |
| **Tag-status** | `unreleased`, or the tag that first shipped the decision. A `v*` tag is the only compatibility boundary this project has, so a surface no tag has shipped can be changed outright — see `CONTRIBUTING.md`. Today every record reads `unreleased`; re-check with `git tag --merged` rather than trusting that sentence. |

There is no Consequences section. What a decision costs belongs in the same
paragraph as the decision, not in a list underneath it that nobody updates.

## Writing one

Copy `0000-template.md`, take the next free number, and name the file
`NNNN-a-short-imperative-title.md`. Then add the record's id to its authority's
doc — a package doc comment for a package, the page itself for a page — because
the anchor is checked in both directions:

- a record naming an authority whose doc does not carry the record's id fails;
- a doc carrying an id that no record declares fails.

That is what puts the decision in front of the person about to violate it.
`go doc ./internal/statelog` shows `ADR-0002` on its first screen, and the
record it names is a page long rather than the framework's whole argument.

The gate is `internal/adr`, it runs inside `make check`, and it is the thing
without which this directory becomes `docs/reference/design-decisions.md` —
which was this repository's ADR register under another name, carried fifteen
decisions, was referenced by nothing, was read by nobody, and had drifted into
six false statements by the time anybody checked.

## The records

| # | Decision | Enforced by |
|---|---|---|
| [0001](0001-one-broker-carries-the-stream-and-the-coordination-store.md) | One broker carries both the stream and the coordination store, and every connection a node cannot run without is watched | `TestNoPackageBuildsASubjectByHand`, `queuetest`, `TestEveryConnectionANodeDependsOnIsWatched`, `TestTheCoordinationConnectionIsWatched` |
| [0002](0002-the-stream-is-the-write-ahead-log.md) | The stream is the write-ahead log; the SQL estate is derived | `TestOnlyTheApplierWritesTheReplicatedEstate` |
| [0003](0003-fleet-agreement-state-lives-in-coordination.md) | Fleet-agreement state lives in coordination, never a node's own file | `TestEveryNodeTableSaysWhoHasToAgreeOnIt` |
| [0004](0004-a-node-keeps-two-database-files.md) | A node keeps two database files, and nothing spans them | `TestNoStatementSpansTwoFiles` |
| [0005](0005-ownership-is-three-valued.md) | "Do I hold this?" has three answers, never two | the `(value, error)` signature |
| [0006](0006-event-evolution-is-additive-only.md) | Event evolution is additive-only and unknown types round-trip | `TestAnUnknownTypesLargeIntegersSurviveARoundTrip` |
| [0007](0007-turso-is-the-only-store-driver.md) | Turso is the only store driver, and it bounds the release matrix | `TestTursoIsTheOnlyDriverInTheBinary` |
| [0008](0008-a-shared-rule-gets-one-implementation.md) | A rule more than one package needs gets exactly one implementation | `TestNoClientSitsOnTheProcessGlobalPool` and its siblings |
| [0009](0009-one-activation-pointer-applied-per-node.md) | Which revision is current is fleet-wide; applying it is per node | nothing — declared |
| [0010](0010-tracing-is-configured-by-the-standard-otel-environment.md) | Tracing is configured by the standard OTel environment, not by Tier A | nothing — declared |
| [0011](0011-tier-a-is-the-root-of-trust.md) | Tier A resolves from the environment and nothing else | `TestTierAIsNeverResolvedFromTheSecretStore` |
| [0012](0012-a-wake-is-derived-not-published.md) | A wake is derived from a durable record, never published by the writer | `TestAnUnreachableClaimStorePublishesAnyway` |
| [0013](0013-a-seats-identity-is-derived.md) | A seat's identity is derived, never looked up, and a handle is immutable | `TestDeriveAgentIDIsStable` |
| [0014](0014-a-compacted-changelog-is-the-fourth-answer.md) | A compacted changelog is the fourth answer to "who has to agree on it?" | `TestASeatsMemoryCrossesToANodeThatHasNeverRunIt` |
| [0015](0015-an-alarm-borrows-its-threshold.md) | An alarm fires at a threshold another decision already made | `TestTheBackupAlarmFiresAtTheAgeThePolicyNames` |
| [0016](0016-a-protocol-bump-refuses-where-an-envelope-round-trips.md) | A coordination protocol bump refuses where an event envelope round-trips | nothing — declared |
| [0017](0017-a-turn-id-names-one-run.md) | A turn id names one RUN; the work key names the unit of work | `TestARedeliveredTriggerRunsUnderItsOwnIdentity` |
| [0018](0018-the-company-has-one-clock.md) | The company has one clock | `TestTheCompanysClockIsTheZoneItNames` |
| [0019](0019-a-token-budget-is-a-periodic-window.md) | A token budget is a periodic window, and the window's turnover is its only reset | `TestAWindowRollsAtItsBoundary` |
| [0020](0020-a-nodes-own-day-is-a-compacted-domain.md) | A node's own day is a compacted domain, and every node writes only its own | `TestADepartedNodesSpendIsStillAnswered` |
| [0021](0021-fleet-turn-detail-is-a-scatter.md) | The fleet's turn-level detail is read from every node at query time, and every answer says which nodes it covers | `TestEventReadsGoThroughTheFleet`, `queries.FleetEvents` |
| [0022](0022-a-turn-is-charged-to-one-work-item.md) | A turn is charged to one work item, per segment, and never split | `TestTaskSpendNeverExceedsTheRollup` |
| [0023](0023-a-decision-is-a-structured-ask.md) | A decision is a structured ask, and the engine enforces only its shape, its answer and its promise | `TestADecisionRidesOnlyTheAskAndNeverChanges`, `TestADecisionIsValidatedField`, `TestAChoiceMustNameAnOptionOfTheAskItAnswers`, `TestAnInformedAnswerOwesTheChatSurface` |
| [0024](0024-the-dashboard-acts-as-the-principal-its-session-resolves-to.md) | The dashboard acts as the principal its session resolves to: a person or a bound token as its seat, any other credential under its own login, and nobody refused for being unbound | `TestAnActWriteIsMadeAsItsPrincipal`, `TestACallIsMadeAndAuditedAsThePrincipalItWasAdmittedAs`, `TestAnyResolvedPrincipalMayAct` |
| [0025](0025-a-node-without-data-reaches-the-estate-through-one-that-holds-it.md) | A node without data reaches the estate through a node that holds it | `TestASeatOnAStatelessNodeWritesThroughADataNode` |
| [0026](0026-the-estate-names-an-object-and-a-store-keeps-its-bytes.md) | The estate names an object, and a store the fleet shares keeps its bytes | `TestEveryTableThatNamesAnObjectIsDeclared` |
| [0027](0027-a-deletion-from-the-shared-store-needs-no-lock.md) | A deletion from the shared store needs no lock | `TestAPutNamingAnOldKeyIsRefused`, `TestTheGraceOutlastsTheLongestUnrecordedUpload`, `TestEveryPutMintsAKeyNeverWrittenBefore`, `TestAnObjectIsJudgedByBothItsInstants`, `TestAnIncompleteEstateDeletesNothing` |
| [0028](0028-the-semantic-first-stage-is-an-index.md) | The semantic first stage is an index | `TestIVFRecallMeetsTheFloorCurve`, `TestEveryHolderBuildsTheSameIndex`, `TestAPartialRepublicationStillFilesEveryRow`, `TestTheIndexWaitsForTheLog` |
| [0029](0029-a-derived-id-older-than-the-ledger-is-rebased.md) | A derived operation id older than the ledger is rebased, by every attempt, onto an instant the fleet records | `TestAnAttemptRebasesOnlyPastTheHorizon`, `TestALaterAttemptIsJudgedAtItsOwnClock`, `TestALaterAttemptInheritsTheRebaseWhileItCan`, `TestADispatchPastTheHorizonMintsAtItsAttempt`, `TestAResumePastTheHorizonMintsAtItsAttemptOrItsHalfsRebase`, `TestTheRetentionsOutlastWhatTheyCover` |
| [0030](0030-a-managed-company-document-names-its-writers.md) | A managed company document names its writers, and every other credential is refused a change | `TestAManagedDocumentRefusesEveryOtherCredential`, `TestTheProgrammaticWritesAreJudgedByTheirAuthor`, `TestAReloadOfAManagedDocumentIsOpenToEveryCredential`, `TestAManagedDocumentRefusesAConnectBeforeSealing`, `TestAManagedDocumentRefusesAGitHubAppBeforeGitHubIsAsked`, `TestAManagedDocumentRefusesTheOfflineWrites` |
| [0031](0031-every-state-log-record-is-signed.md) | Every state-log record is signed, and the framework verifies before a domain decodes | `TestATamperedRecordIsRefusedPermanently`, `TestEnvelopeDomainsNeverVerifyAsEachOther` |
| [0032](0032-every-person-holds-a-human-seat.md) | Every person holds a human seat from the record that creates them to the one that removes them: it is named at a create and on every invitation, moved and never cleared, held by an open invitation, and always a human seat | `TestAPersonEnrolsOnlyOntoAHumanSeat`, `TestAPersonsSeatIsChangedButNeverCleared`, `TestAnOpenInvitationHoldsItsSeat`, `TestACreatedPersonGetsAFirstPasswordLink`, `TestAPersonIsNeverCreatedOrInvitedWithoutASeat`, `TestAnInvitedSeatCannotBeTakenOutOfTheCompany` |
| [0033](0033-access-is-admin-and-one-permission-per-app.md) | Access is admin and one permission per app: admin runs the company and includes every app, each app is one permission, everything else is everybody's, and a relation is asked beside the app | `TestEveryAppIsAGrantAndAdminIsNot`, `TestExpandAddsEveryAppExactlyWhenAdminIsHeld`, `TestARefusingNodeKeepsEveryApp`, `TestEveryAppIsNamedByARow`, `TestEveryAppRowAdmitsAHolderAndRefusesANonHolder`, `TestSeatOnlyAndHumanOnlyAreCheckedBeforeTheClass`, `TestEveryDottedVerbIsAskedByANonTestCallSite`, `TestAnAdministrativeRecordIsNeverWrittenThroughAMachineToken`, `TestASeatHoldsEveryAppAndNeverAdmin` |
| [0034](0034-an-artifact-is-shared-and-a-share-is-a-relation.md) | An artifact is shared, and a share is a relation: the `artifacts` app gates authoring only, one class decides every artifact row on a five-role ladder, the lead arm opens only an agent owner's artifacts, and a caller with no relation is told the artifact does not exist | `TestASharedRowAdmitsTheRoleItNamesAndEveryRoleAbove`, `TestACallerWithNoRelationIsToldTheArtifactIsNotFound`, `TestTheLeadArmOpensOnlyAnAgentOwnersArtifacts`, `TestTheAudienceHoldsExactlyTheAppsThePrincipalCan` |
| [0035](0035-an-artifact-is-served-from-an-origin-that-holds-no-credential.md) | An artifact is served from an origin that holds no credential: a listener of its own, a real origin only on a site of its own, documents re-authorized on every request, and a stale node fails closed | `TestTheArtifactListenerServesOnlyTheServingRoutes`, `TestTheArtifactListenerReadsNoCookie`, `TestAnArtifactGetsAnOriginOnlyOnASiteOfItsOwn`, `TestEveryServedDocumentCarriesAFreshNonce`, `TestARevokedShareStopsTheNextDocumentRequest`, `TestAStaleServingNodeAnswersUnavailableRatherThanServe`, `TestTheAPIOriginNeverRendersAnArtifact` |
| [0036](0036-bytes-are-reserved-before-they-move.md) | Bytes are reserved before they move, and a carried object is named again only by a record that finds it named | `TestNoByteIsAcceptedBeforeItsReservation`, `TestFinalizeRefusesBytesThatDoNotMatchTheirDigest`, `TestUnnamedBytesNeverExceedTheReservationBudget`, `TestACarriedObjectMustBeNamedWhereItIsCarriedFrom`, `TestNoCarryCrossesAnArtifact` |
| [0037](0037-a-datastore-write-is-decided-where-it-is-applied.md) | A datastore write is decided where it is applied, from authority stamped at the API: one sequenced log per store, and an applier that reads no other domain | `TestAConditionalWriteIsSkippedIdenticallyOnEveryNode`, `TestTheWriterReadsItsVerdictFromItsOwnRows`, `TestTheApplierDecidesFromTheStampedAuthorityAlone`, `TestTheApplierReadsNoOtherDomain` |
| [0038](0038-presence-is-told-never-stored.md) | Presence is told, never stored: rosters in memory, an ephemeral sealed broadcast between nodes, and expiry after three missed heartbeats | `TestPresenceWritesNoRowAnywhere`, `TestAPresenceFrameTheFleetDidNotSealShowsNobody`, `TestPresenceExpiresAfterThreeMissedHeartbeats`, `TestABroadcastIsNeverRetained` |
| [0039](0039-a-cold-store-lives-in-object-storage-and-returns-through-the-log.md) | A cold store will live in object storage and return through the log; the hot store keeps what that needs from its first release | `TestTheStateDigestIsTheSumOfItsDocuments`, `TestEveryHistoryRowCarriesTheDigestAfterIt` |
| [0040](0040-content-a-model-reads-carries-its-provenance.md) | Content a model reads carries its provenance: one fence, bounded per turn, around every word somebody else wrote | `TestAFenceCannotBeClosedByItsContent`, `TestEveryUntrustedReadIsFenced`, `TestEveryArtifactToolResultIsFenced` |
| [0042](0042-a-kind-is-native-or-framed.md) | A kind is native or framed, and one table decides every rule by kind: a native kind carries no author code and is drawn by the dashboard from a closed format, a kind enters the table only when it works end to end, a kind is fixed at create, covers are drawn and never captured, and each kind's comment anchor is fixed now | `TestEveryKindAnswersEveryFacet`, `TestOnlyAFramedKindRunsAPage`, `TestEveryEntryRuleCanBeMet`, `TestAnEntryInfersExactlyOneKind`, `TestADeckDeclaresNoCapability`, `TestTheDashboardDrawsEachKindAsTheKindTableSays` |
| [0043](0043-a-dashboards-live-data-is-read-as-its-viewer.md) | A dashboard's live data is read as its viewer: every engine dataset is the question its screens ask, on the viewer's own request, and a board carries no row | `TestTwoViewersOfOneBoardEachSeeOnlyTheirOwnRows`, `TestABindingToAPerRecordQuestionIsRefused` |
| [0044](0044-a-page-is-edited-in-place-and-saved-as-the-markdown-it-was.md) | A page is edited in place, and saved as the Markdown it was: one grammar, a no-edit save changes no byte, no markup parsed through a sink, and the editor fetched only to edit | `TestTheDashboardFitsItsBudget`, and the dashboard's round-trip and Trusted Types suites |
| [0045](0045-every-model-is-reached-through-its-vendors-cli.md) | Every model is reached through its vendor's coding CLI: no HTTP client to a model vendor, and the OpenAI SDK only for embeddings | `TestNoModelIsReachedOverAVendorsHTTPAPI` |
| [0047](0047-an-artifact-lives-in-one-folder-and-a-folder-grants-nothing.md) | An artifact lives in one folder, and a folder grants nothing: one tree on one subject, a folder seen through its top-level folder, filing asks the artifact and the folder, a removal moves what it held up, and every count shares the listing's predicate | `TestAFolderGrantsNothingToItsContents`, `TestFilingNeverMovesTheAccessEpoch`, `TestAFolderCountHoldsOnlyWhatTheReaderMaySee`, `TestABreadcrumbNeverNamesAHiddenAncestor`, `TestTheTotalEqualsTheRowsTheViewerMaySee`, `TestAnInvisibleFolderIsNotFoundLikeAnUnknownOne`, `TestFilingAsksTheFolderAndTheArtifact`, `TestTheAdminPathListsNoPrivateFolder` and their siblings |
| [0048](0048-the-adopted-brand-is-the-companys-public-face.md) | The adopted brand is the company's public face, snapshotted by an administrator and served by its bytes: an adoption copies one kit version, everybody reads it, the listener serves only its hashes, and a framed page pins the stylesheet adopted when it was published | `TestTheAdoptedBrandOutlivesItsKitAndIgnoresItsDrafts`, `TestTheBrandRouteServesOnlyAdoptedBytes`, `TestASupersededAdoptionIsServedWhileAVersionPinsIt`, `TestNoValueLeavesItsDeclaration` and the rest the record names |
| [0049](0049-a-domains-fault-takes-only-its-own-copy-out-of-service.md) | A domain's fault takes only its own copy out of service | `TestEveryDomainsCopyIsJudged`, `TestAFaultInADomainThatGatesNoSeatShedsNone`, `TestAFaultInOneDomainRoutesOnlyThatDomainsOperations`, `TestADutyDeclinesWhileACopyItDecidesFromIsOutOfService` |
| [0050](0050-an-anchor-below-the-trim-floor-is-removed-by-a-record.md) | An anchor below the trim floor is removed by a record every node applies | `TrimmedAnchorsGoAndAZeroWriteIsFenced` on every domain, `TestATrimmedRecordRemovesOnlyTheAnchorsBelowItsFloor`, `TestEveryLogThatWritesAnchorsHasATrimmedRecord` |
| [0051](0051-a-retired-reference-keeps-its-object-for-a-grace.md) | A retired reference keeps its object for a grace, a derived one for as long as it is named; only a required one is audited and accounted for by a backup, and a store move copies a required or a derived one | `TestADeclarationStatesItsStanding`, `TestAWalkReadsOnlyTheStandingsItsPassIsOwed`, `TestEveryTableThatNamesAnObjectIsDeclared`, `TestEveryDeclarationStatesItsStanding`, `TestARetiredTableIsSweptAndOutlivesItsLongestRead`, `TestARetiredReferenceKeepsItsObjectFromCollection`, `TestTheAuditAsksOnlyAfterRequiredReferences`, `TestADerivedReferenceIsKeptAndNeverAudited`, `TestAStoreMoveWalksTheRequiredAndDerivedTablesAlone`, `TestAStoreMoveCopiesADerivedObjectVerified`, `TestADerivedObjectTheSourceCannotGiveBackHoldsNothingUp`, `TestABackupCarriesNoRetiredReference`, `TestABackupCarriesNoDerivedObject`, `TestABackupRefusesADeclarationThatStatesNoStanding` |
| [0052](0052-a-backup-carries-every-object-the-log-it-replays-names.md) | A backup carries every object the log its restore replays names, and records lost only what the restored rows name | `TestABackupCarriesAnObjectNamedAfterItsCopy`, `TestTheObjectStreamIsSnapshottedAfterEveryOtherStream`, `TestABackupRefusesALogRecordItCannotRead`, `TestEveryRequiredTableSaysHowItsLogNamesObjects`, `TestEveryRequiredTableNamesItsDomainsLog`, `TestAFileRecordNamesTheObjectItsRowNames` |
| [0053](0053-a-streams-identity-is-the-instant-its-creator-stamped.md) | A stream's identity is the instant its creator stamped, and one function reads it | `TestOnlyStreamCreatedAtReadsAStreamsCreationInstant`, `TestTheStreamInstantWalkJudgesAKnownTree`, `TestEveryCreatedFieldOfTheNATSPackagesIsClassified`, `TestAStreamsIdentityGoesWhereItsConfigurationGoes` |
| [0054](0054-a-repositorys-refs-move-only-by-records-and-a-cache-is-derived.md) | A repository's refs and packs move only by records on its own subject, and a bare repository is a cache of them | `TestReceivePackNeverRunsInACache`, `TestAnOkIsAnsweredOnlyFromAnAppliedRow`, `TestNothingButSyncWritesACache`, `TestTheSuiteCatchesAnOkOnPending`, `TestTheSuiteCatchesAWriteIntoACache` |
| [0055](0055-a-person-reaches-git-with-their-own-bound-token.md) | A person reaches git with their own token, and git is one more way a person acts | `TestGitAccessIsTheOneClassifier`, `TestAnUnboundTokenCannotPush`, `TestEveryPushIsAuditedAsItsPerson`, `TestGitRoutesAreNotPublished` |
| [0056](0056-a-node-local-derived-file-is-declared.md) | A node-local derived file is declared, lives in its incarnation, and the cache writes none it has not declared | `TestEveryDerivedFileIsDeclared`, `TestTheSweepKeepsExactlyTheDeclaredImage`, `TestNothingButSyncWritesACache`, `TestTheConceptPageNamesEveryDerivedFile` |
| [0057](0057-a-git-node-runs-git-2-55-from-one-pin.md) | A git node runs git 2.55 or newer from one pin, and a client needs 2.40 | `TestAGitNodeRefusesAGitBelowItsFloor`, `TestErrTooOldNamesItsFloor`, `TestThePinsMeetTheFloors`, `TestTheNodeFloorWritesALayerOverANamedBase`, `TestEveryPackReuseModeSendsOverlappingPacksWhole`, `TestTheFormatTableIsWhatGitSays`, `TestARequestCannotNameTheDamage`, `TestEveryNodeGitClaimRunsOnBothEnds`, `TestTheFloorJudgementIsTheFloors`, `TestTheGitFloorsPassTheSuite`, `TestAGitNodeBootsOnlyAtItsFloor`, `TestTheCLIRefusesAGitBelowTheClientFloor`, the image's own build step |
| [0058](0058-a-repository-grant-is-derived-from-the-chart.md) | A repository grant is derived from the chart, never stored, a seat's own branches live under `refs/heads/seats/<handle>/`, a seat's push is held to its grant at the decide and a seat outside the owning unit fills at most half of a repository, a seat's read in a turn somebody woke is held to their grant too, and a protection binds every seat, a unit's lead included: only a person removes or loosens one — a merge gate weighed field by field, and the path owners where the gate makes them gating | `TestGrantsFollowTheChart`, `TestRefRuleTable`, `TestASeatsNamespaceIsUnderTheSeatsRoot`, `TestAnUnboundTokenCannotPush`, `TestAUnitRepositoryRefusesASeatOutsideIt`, `TestANodeBehindTheConfigurationDecidesNoGrant`, `TestTheDecideRefusesWhatTheGrantRefuses`, `TestASeatOutsideTheUnitFillsAtMostHalfARepository`, `TestAReadMadeForAColleagueIsHeldToTheColleaguesGrant`, `TestACodeReadIsDecidedAgainForWhoeverWokeTheTurn`, `TestAnAgentCannotLoosenAProtection`, `TestAnAgentCannotLoosenAGate`, `TestAnAgentCannotRemoveAPathOwnerUnderOwnersApprove` |
| [0059](0059-a-repositorys-git-work-is-asked-of-its-home-git-node.md) | A repository's git work — its reads by object id and its engine writes — is asked of its home git node — the first of its rendezvous order over the git nodes, a preference and never ownership — one node a walk fills a copy, and a write is asked again of the next git node as the same push | `TestAGitOperationIsAskedOfItsRepositorysHome`, `TestAGitNodeThatIsSyncingSendsTheAskerToTheRunnerUp`, `TestOnlyTheEffectiveHomeIsAskedToFill`, `TestANodeThatHoldsNoCopyFillsOnlyAsHome`, `TestEveryMemberRoutesAGitReadToOneHome`, `TestTheGitHalfIsServedOnceAnApplyBuildsTheCodeHost`, `TestAnEngineWriteThatRanNothingMovesOnWithTheSameWrite`, `TestAGitWriteIsHeldToItsOwnAttempt` |
| [0060](0060-a-compaction-merges-packs-without-judging-reachability.md) | A compaction merges packs without judging reachability — every object of its inputs in its outputs, decided alone by the repository's committer — and a retired pack keeps its object past its longest read, leaving every copy by a swap | `TestTheHostPassesTheSuite`, `TestTheSuiteCatchesARetirementWithNoGrace`, `TestARetiredTableIsSweptAndOutlivesItsLongestRead`, `TestASwapKeepsEveryPackAReaderHolds`, `TestAMergeCompactionKeepsTheCommitGraph`, `TestACompactionKeepsEveryObjectOfItsInputs`, `TestACompactionOfAPackNoLongerLiveIsStale` |
| [0061](0061-a-derived-object-is-published-kept-and-never-audited.md) | A derived object any git node can rebuild is published once by its repository's maintainer — the first git node of its order whose advertised budget holds it — kept from collection and never audited, and every git node installs a layer only over its own packs, matching its row, in chain order and read through its bitmap first, serving from the prefix below otherwise | `TestEveryDeclarationStatesItsStanding`, `TestADerivedReferenceIsKeptAndNeverAudited`, `TestABackupCarriesNoDerivedObject`, `TestALayerIsInstalledOnlyOverItsOwnPacks`, `TestTheCanaryRefusesAChainOverTheWrongBase`, `TestAPublicationIsBuiltInAScratchNamingExactlyItsBase`, `TestAPublicationIsNeverBuiltOverABaseTheStoreLost`, `TestTheHostPassesTheSuite`, `TestTheSuiteCatchesALayerOverAPackTheNodeLacks` |
| [0062](0062-an-engine-built-commit-is-the-acting-principals-and-lands-as-a-push.md) | An engine-built commit is the acting principal's, committed by the engine at its operation's instant — its committer date never before its parent's — built over the trees the change touches and landed as a push through the one landing, its push id derived from the caller's operation so a retry is the same push and the same commit | `TestARetriedEngineWriteIsOnePush`, `TestEveryPushTakesTheOneLanding`, `TestAnEngineCommitReadsNoWholeTree`, `TestAnEngineCommitIsTheActingPrincipals`, `TestACommitIsNeverDatedBeforeItsParent`, `TestAnEngineWriteIsHeldToTheRelaysFsck`, `TestOnlyCommitTreeTakesAnIdentity`, `TestTheSuiteCatchesAWriteAnsweredOkWithoutARow` |
| [0063](0063-a-box-reaches-git-with-its-runs-credential.md) | A coding box reaches git on `/box/git/` with its run's own credential, admitted by its run's row and given only what its run was given — resolved at the launch for the seat and for whoever woke its turn — and the engine configures the box's git through the launch's environment; a person's stop refuses the box's pushes and its run's bridged writes alike | `TestARunTokenOfAnEndedRunIsRefused`, `TestABoxIsGivenOnlyItsGrants`, `TestABoxIsShownNothingItsFloorDoesNotHold`, `TestEveryMountedRouteIsOnTheListenerItsPathNames`, `TestABoxRouteIsAdmittedByItsRunNotTheGuard`, `TestABoxIsGivenOnlyItsGrantsAtLaunch`, `TestABoxIsConfiguredByEnvironment`, `TestTheEnginesGitVariablesAreReserved`, `TestARunsRepositoriesAreReadForWhoeverWokeItsTurn`, `TestTheGitFloorsPassTheSuite`, `TestNeitherTransportTakesTheOthersCredential`, `TestAStoppedSeatsBoxPushesNothing`, `TestAStoppedSeatsBridgedWritesAreRefused`, `TestTheEngineStopsOneWayForBoxAndBridge`, `TestBoxesHoldAtMostHalfTheTransferLane`, `TestNothingButTheHookSpellsATrailerKey` |
| [0065](0065-a-nodes-alarm-transition-is-the-record-its-wake-derives-from.md) | A node's alarm transition is the record its wake derives from: the tracker hands each one on with a derived id, the node publishes it from an outbox until the broker takes it, and the seats whose role carries `health_tools` are woken from the delivery, once each | `TestATrackerHandsOnEveryTransitionOnce`, `TestAnAlarmTransitionIsPublishedUntilTheBrokerTakesIt`, `TestAnAlarmTransitionWakesEachGrantedSeatOnce` |
| [0066](0066-a-protected-branch-moves-only-by-a-merge-whose-preconditions-one-snapshot-proves.md) | A protected branch moves only by a merge whose preconditions one snapshot proves: one record its repository's committer decides — the change open at the head it was built from, the target at the tip it was built onto, the gate met by the record's principal or overridden by a person — merges the change and moves its target as a push; a person's own push under `direct_push: persons` is their recorded override, and the gate counts review objects, never ADR-0023's decisions | `TestAMergeIsDecidedFromOneSnapshot`, `TestAMergeMovesTargetDeletesSourceInOneRecord`, `TestAnOverrideIsAPersons`, `TestAProtectedRefMovesOnlyByAGatedRecord`, `TestTheGateReadsNoDecision` |

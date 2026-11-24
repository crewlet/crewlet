# ADR-0175: A coordination read is given the time its bytes take to cross

- **Status:** accepted
- **Authority:** `internal/coord`
- **Enforced-by:** `internal/coord/kv.TestAPointerReadAcrossALeafsLinkCarriesNoBody`, `internal/coord/kv.TestABodyAtItsBoundIsReadAcrossALeafsLinkAtItsFirstAsk`, `internal/coord/kv.TestABodysReadIsGivenTheArrivalOfItsRecordedSize`, `internal/coord/kv.TestAFlipLandsWithinHalfTheBodysRetentionOfItsWrite`, `internal/coord/kv.TestAFlipIsGivenTwiceTheBodysWriteAtItsBound`, `internal/coord/kv.TestABodysCrossingIsHeldToWhatAKeyHolds`, `internal/maintenance.TestTheBodySweepRunsUnderTheDutyAtItsOwnHorizon`, `internal/engine.TestAPostureAcrossASlowLinkIsReadNotAssumed`, `internal/engine.TestAReconcilerAcrossASlowLinkAppliesARevisionAtItsBound`, `internal/coord/kv.TestEveryReadIsGivenTheCrossingOfItsClassBound`, `internal/coord/kv.TestASmallClassCrossesTheFloorInsideTheReadTerm`, `internal/coord/kv.TestASettledWriteIsHeldToItsClassBound`, `internal/coord/kv.TestARunsListingAcrossALeafsLinkDeliversItsRecordsInItsPass`, `internal/coord/kv.TestAWatchAcrossASlowLinkDeliversARecordLongerThanTwoHeartbeatsToCross`, `internal/coord/kv.TestTheMarkerSweepAcrossALinkCarriesHeadersOnly`, `internal/coord/kv.TestNothingWatchesASettledBucketThroughTheClient`, `internal/coord/kv.TestEveryClientWatchOfASettledBucketIsRefused`, `internal/coord/kv.TestAPassOpensNoConsumerUntilItsKeyIndexAnswers`, `internal/coord/kv.TestARunsStandingCrossesALeafsLinkWithoutItsRecords`, `internal/coord/kv.TestAStandingLandsInTheMessageOfItsRecord`, `internal/coord/kv.TestTheWidestStandingIsCountedInKvHeaderBytes`, `internal/coord/kv.TestACoordinationValueIsHeldToWhatItsLeadersReadCarriesBackAcrossALink`, `internal/sandbox.TestEveryWriteOfARunStatesItsStanding`, `internal/sandbox.TestTheEpochFloorReadsNoStatedRecordWhole`, `internal/sandbox.TestASeatsListingReadsOnlyItsOwnRecordsWhole`, `internal/engine.TestALeafBootsBesideRunsAtTheirBound`, `internal/engine.TestAnMCPCheckOfEveryNodeAtItsWidestIsRecordedWithinItsBound`, `internal/engine.TestADeviceSignInsDetailIsCutToFitItsRecord`, `internal/seataccess.TestASeatWhoseAccessOutgrowsItsLedgerRecordIsNotSynced`, `internal/coord.TestEveryPositionsClassesWidestRecordFitsItsBound`, `internal/coord/kv.TestEveryPositionsClassAtItsBoundIsReadAcrossALeafsLinkAtItsFirstAsk`, `internal/coord.TestEveryPositionsFamilysWidestRecordFitsItsClass`, `internal/coord.TestASentenceIsCutToWhatItTakesAsStored`, `internal/coord/kv.TestEveryPositionsFamilyIsDeclaredAtTheBoundItsWidestRecordFits`, `internal/coord/kv.TestFleetContract`, `internal/coord/kv.TestEveryPositionsClassPastASmallValueIsReadAcrossALeafsLinkAtItsFirstAsk`, `internal/statelog.TestAJournalDropsItsOldestResolvedRecordsAndNeverAnUnresolvedOne`, `internal/statelog.TestARetirementsEvidenceIsCutToItsRecordCeilingFirst`, `internal/engine.TestAWindowPastWhatItsRecordHoldsIsBlockedForAPersonAndSendsNothing`, `internal/engine.TestEveryLogThisBuildRunsFitsWhatAPositionsRecordNames`, `internal/seataccess.TestARequestPastWhatItsRecordKeepsIsRefusedNamingWhatToNarrow`, `internal/engine.TestAFleetScaledDownOpensItsWindowOnceItsGoneIDsAreRetired`, `internal/coord.TestARetirementStandsOnlyUntilItsNodeWritesAgain`, `internal/statelog.TestTheSealLiftsABlockThatHeldARequestBack`, `internal/engine.TestAGrantTheFleetCouldNotKeepIsRefusedBeforeAnybodyConsents`, `internal/coord.TestAnEmptyFieldIsRefusedAsMissingNotAsPastItsBound`, `internal/engine.TestTheMCPBeatReadsDefinitionsOnTheLoopsContext`, `internal/engine.TestTheAtlasReadsTheFleetsListingsFromTheWatchedCopy`, `internal/seataccess.TestALaneIsSizedInItsVendorsWords`, `internal/seataccess.TestAHeldLaneHoldsOnlyItself`, `internal/seataccess.TestAVendorsLongRefusalIsKeptAtItsHead`, `internal/seataccess.TestARecordPastItsClassFailsItsSeatNotThePass`, `internal/engine.TestOneSeatsAccessRecordIsReadByItsKey`, `internal/coord/kv.TestTheAccessReviewsRevisionsCrossALinkAsHeadersAlone`, `internal/coord/kv.TestEveryUploadsClassAtItsBoundIsReadAcrossALeafsLinkAtItsFirstAsk`, `internal/artifactupload.TestTheWidestUploadRecordFitsItsBound`, `internal/artifactupload.TestTheWidestReservationLedgerFitsItsBound`, `internal/sandbox.TestASeatsWidestSaveLedgerFitsItsBound`, `internal/artifactupload.TestAnUploadRecordPastItsBoundIsRefusedByItsWriter`, `internal/artifactupload.TestARefusedPublishDropsTheGeneratedFilesItDeleted`, `internal/artifactupload.TestAVersionPastItsFileCountIsRefusedBeforeAByteMoves`, `internal/artifactupload.TestAPublishsWarningsAreCutToWhatItsRecordKeeps`, `internal/artifactupload.TestAnUploadRecordsTheTypeAFileIsServedAs`, `internal/artifactupload.TestALedgerChangeReadsABoundedNumberOfRecords`, `internal/artifactupload.TestTheWidestOutcomeFitsWhatARecordKeepsForIt`, `internal/artifactupload.TestAStagedRecordAtItsBoundClosesWithItsOutcome`, `internal/artifactupload.TestASizeRefusalUndoesNoEarlierAttemptsObjects`, `internal/artifactupload.TestARetryRefusedAsMovedLeavesWhatAnEarlierAttemptLanded`, `internal/artifactupload.TestAnAbandonedUploadIsReleasedBehindHoldsThatCannotBe`, `internal/artifactupload.TestARefusedReservationKeepsTheReleaseItMade`, `internal/artifactupload.TestARefusedReservationIsAnsweredHoweverItsReleasesWriteEnds`, `internal/artifactupload.TestAReleaseLeavesAHoldARetriedIntentTookUp`, `internal/artifactupload.TestARetriedIntentTakesUpTheHoldItsFirstAttemptLeft`, `internal/artifactupload.TestAnEarlierAttemptsBytesStayReservedThroughARetrysRefusal`, `internal/artifactupload.TestATakenUpHoldIsNeverWeakened`, `internal/artifactupload.TestAReleaseDropsAHoldAnotherNodeOnlyRechecked`, `internal/artifactupload.TestAReleaseDropsWhatAnotherReleaseLeftOfItsHold`, `internal/artifactupload.TestAHoldMadeForAReleasedUploadIsNeverTakenForTheReleasedOne`, `internal/coord.TestTheWidestChatReadRecordFitsItsBound`, `internal/coord.TestAChatReadRecordPastItsBoundIsRefusedWhereItIsEncoded`, `internal/coord.TestAFlushNamingAnIDChatletDidNotMintIsRefusedNamingItsField`, `internal/coord/kv.TestEveryChatReadAtItsBoundIsReadAcrossALeafsLinkAtItsFirstAsk`, `internal/agent/builtin.TestAMarkNamingAnIDChatletDidNotMintIsRefusedNamingItsArgument`, `internal/coord/kv.TestEveryAtlasClassAtItsBoundIsReadAcrossALeafsLinkAtItsFirstAsk`, `internal/atlas/collect.TestTheWidestAtlasStandingFitsItsBound`, `internal/atlas/collect.TestAStandingPastItsBoundIsCutDetailFirstKeepingEveryCount`, `internal/atlas/collect.TestTheWidestAtlasMissesFitTheirBound`, `internal/atlas/collect.TestOnlyAVisitReadsItsSourcesMissesByItsKey`, `internal/atlas/collect.TestAScopeWhoseIDIsNoHostsIsWithheld`, `internal/engine.TestAnAtlasSourceReportReadsNoSourcesMisses`, `internal/atlas/codescan.TestTheWidestCodeScanStandingFitsItsBound`, `internal/atlas/codescan.TestAForgesStampIsKeptAsItsDigest`, `internal/atlas/codescan.TestAPassReadsItsOwnStandingAndTheListingReadsThemOnce`, `internal/coord/kv.TestAnAtlasListingCarriesNoRecordOutsideItsFilterAcrossALink`, `internal/atlas/collect.TestAVisitWhoseMissesCannotBeReadReadsNothing`, `internal/atlas/collect.TestAVisitWhoseMissesCannotBeWrittenCountsOnlyThoseRecorded`, `internal/atlas/codescan.TestAForgesTipIsACommitIDOrNoRead`, `internal/coord/kv.TestEverySecretAtItsBoundIsReadAcrossALeafsLinkAtItsFirstAsk`, `internal/fleetsecrets.TestTheWidestSecretRecordFitsItsBound`, `internal/fleetsecrets.TestAProvenanceNoRecordKeepsIsRefusedByEveryWrite`, `cmd/crewlet.TestAnAuthorNoRecordKeepsIsRefusedNamingItsVariable`, `internal/fleetsecrets.TestANamePastItsBoundIsRefusedByEveryWriteAndNamesNoRow`, `internal/secrets.TestANameIsHeldToTheLengthItsRecordKeeps`, `internal/api/secretsapi.TestANamePastItsBoundIsRefusedAsAnInvalidName`, `internal/api/secretsapi.TestAListingReadsTheBucketOnce`, `cmd/crewlet.TestAListingAndADryRunReadTheStoreOnce`, `internal/engine.TestTheAlarmBeatReadsNoSealedValueFromTheStore`, `internal/engine.TestLocalModelsDateTheValueTheAlarmJudges`, `internal/api/queries.TestAModelsExpiryIsTheValueThisNodeResolves`, `internal/coord/kv.TestEveryIntegrationsClassAtItsBoundIsReadAcrossALeafsLinkAtItsFirstAsk`, `internal/integration.TestTheWidestStatusFitsItsBound`, `internal/integration.TestAPassesFindingsAreCutToTheirRecordFirstFindingWhole`, `internal/integration.TestAStatusReadReadsNoFindings`, `internal/integration.TestFindingsTheirStatusDoesNotNameReadAsUnread`, `internal/integration.TestAFoldThatFoundNothingRemovesTheFindingsItsStatusNamed`, `internal/engine.TestTheMintJudgementReadsNoFindingsOnItsCallersPath`, `internal/engine.TestTheAtlasReadsTheIntegrationsStatusesAlone`, `internal/api/queries.TestTheAdvisorSignatureMovesWithTheFindingsDigest`, `internal/config.TestAnAddressAStatusKeepsExactIsBoundedWhereTheSchemaSaysSo`, `internal/integration.TestAPassThatFitsItsRecordIsKeptWhole`, `internal/integration.TestAFailedFindingsPassReadsTheFindingsUnread`, `internal/integration.TestWhatARowShortensIsLoggedWhole`, `internal/integration.TestTheRollupTable`, `internal/engine.TestAMintIsNeverJudgedFromFindingsARecordCutAway`, `internal/api/setupapi.TestASeatIsNeverSatisfiedOnFindingsNotInHand`, `internal/advisor.TestIntegrationFindingsNotInHandLeaveTheIntegrationsUnread`, `internal/advisor.TestACutSurfaceHoldsOnlyItsOwnFindingsOpen`, `internal/coord.TestEveryNodeStatusRowsWidestRecordFitsItsBound`, `internal/coord.TestAnMCPRowsUnlistedKeysAreKeptWholeAndCountedPastTheRow`, `internal/coord.TestARowsTextIsHeldToItsBoundAsStored`, `internal/coord/kv.TestAnUndecodableStatusRowIsSkippedNotRaised`, `internal/engine.TestTheStatusRecordFollowsChangesAndOutlivesNoReconcile`, `internal/coord.TestANodeReadAcrossPassesIsCountedWholeOrNotAtAll`, `internal/engine.TestARowWhoseWriteLostItsAnswerIsRemovedOnceDropped`, `internal/api/queries.TestACutListOfUnlistedKeysIsCountedPastWhatItNames`, `internal/api/queries.TestTheAdvisorsNoticesReadEachLiveNodesGrowthRow`, `internal/coord/kv.TestEveryFleetRecordsWidestFitsItsClass`, `internal/coord/kv.TestEveryKeyClassThePackageWritesWithIsDeclaredForItsBucket`, `internal/coord/kv.TestContract`, `internal/coord/kv.TestNodeStatusRecordContract`, `internal/config.TestAnObjectStoresIdentityFitsTheFleetsRecordOfIt`, `internal/coord.TestASessionClaimHoldsEverySessionIDABoxMayName`, `internal/notify.TestEveryFollowReasonFitsWhatAFollowHolds`, `internal/engine.TestTheWidestCollectionReportIsCutToItsRecordKeepingEveryCount`, `internal/trashreap.TestTheWidestReportFitsItsRecord`, `internal/engine.TestEveryLeaseThisBuildTakesNamesItselfWithinItsRecord`, `internal/engine.TestAGrowthActsLeaseIsNamedAtOneWidthWhateverItsAccount`
- **Measured:** at the slowest link pace allowed (30 s a MiB), a leader read's reply is a third larger than the value, so no record above 47.1 KiB is read inside a probe's 1.875 s and above 127.1 KiB every read without a deadline fails at the five-second read term; a company at its bound made the activation pointer an 8 MiB reply, 240 s. The pointer at its 8 KiB bound is a 12,404-byte reply, 0.35 s; a typical one about 0.06 s. A class read at the term is at most 16 KiB (`coord.SmallValueBytes`): a 23,328-byte reply, 0.67 s. A run's standing is 196 bytes of header at its widest, so the most header a write carries is 342 bytes; listed beside its message's other headers it is about 1.3 KB a run across the link (counted, not measured), 37 ms at the slowest pace, so a boot's 300 s ceiling fits the floor of about 8,000 runs, where three records at their bound read whole take nine minutes (counted). A pass's quiet and a watch's heartbeat are 5 s plus the raw crossing of the largest value under the filter: 185.0 s for a run's value across a link, 5 s in process, and the same for the runs' standing, whose headers wait behind any record on the link; the marker sweep's headers-only consumer beats at 5 s. Measured 2026-11-22 under `-race` (Go 1.27.2, the build that added the bucket's watcher and the index-first pass): the client's KV watch, at its fixed 5 s heartbeat, never delivered a record taking 20 s or 25 s to cross (3.92 and 3.95 copies carried in four times its crossing); the bucket's own watcher delivered each once, at 20.5 s and 25.6 s.
- **Cost-when-tried:** the company document inside the activation pointer (#1149): every probe, heartbeat and tick on every node read it whole, so on a leaf a large company failed `/ready`, every reconcile tick and the boot, and above about 50 KB the pointer reads alone filled a floor link.
- **Tag-status:** unreleased

## The decision

**A coordination read is given the time the bytes it may carry take to cross
the node's link at the slowest pace allowed.** Three clauses:

1. Every key class declares the most its value may be, and a write past it is
   refused.
2. Every way the class is read is given the crossing of that bound at the
   node's reach: a key's leader read, a listing's pass and its certifying
   reads, a watch's consumer.
3. A reader with a reason of its own that cannot wait that long (a probe, a
   boot, a hot listing) reads a small record, and the large bytes are a class
   of their own, named on it by size and digest. Only the reader that needs
   them pays their crossing, sized to the size recorded.

Clauses 1 and 2 are declared where each bucket opens (`internal/coord/kv`'s
holdings): a class names its keys, its bound and how its reads wait. A run's
value is read with its reply's crossing and no handling, since a writer waits
on it; a body with its arrival; the pointer and every record of a few KiB at
the term. A pass and a watch over a filter wait the raw crossing of the
largest class under it, and a watch is the bucket's own consumer, whose
heartbeat is that wait: the client's resets a delivery longer than two of its
five-second beats and never lands it. The marker sweep's consumer carries
headers alone and beats at five seconds. A pass reads its key index before its
consumer opens, since the index's answer would cross behind its deliveries.
Every crossing is counted for one value on the link at a time, as a write's is.

The activation pointer is the first case of clause 3. It is at most 8 KiB
(`coord.MaxPointerBytes`), its summary at most 4 KiB as it encodes
(`coord.MaxSummaryBytes`, refused past it, never cut), and it names the sealed
document, `body.<sha256>` in the same bucket, held to what one key holds. A
write puts the body, then flips the pointer within half of
`coord.BodyRetention` of that write; a node reads the body only when it lacks
the revision, given the arrival of the recorded size, and refuses one whose
length or digest differs. A sweep of its own removes a body no pointer names
once it is `coord.BodyRetention` (an hour) old, through the revision its pass
saw: half of it covers a body's write at its bound across a floor link and the
flip behind it, the other half clock skew. Each activation's body is kept that
long past the next activation, plus a 15-minute tick, so a company at its bound
activating N times an hour holds up to about 1.25 × N × 6.29 MB more per
replica. The
posture's grace (`configplane.LagGrace`) grows by the body's crossing. ADR-0009
stands: the pointer is still append-only and its revision is the epoch.

The runs' standing is the second case. The boot's epoch floor and a seat's
listings (its busy check, its recovery, a retirement) select on three fields of
a run's record: its seat, its handle and its lease epoch. The record's writer
states them as three headers (`Crewlet-Run-Seat`, `Crewlet-Run-Handle-Sha256`,
`Crewlet-Run-Owner-Epoch`) in the same conditional message as the value, so the
two land at one revision or not at all, and those readers list them over a
certified pass of headers alone, which keeps the records' quiet since its
headers wait behind any record its link carries. A record is read whole only
where the pass missed it (a race, or a pass ended quiet), or where it states
no standing, as an older build's write does; that one is judged on its value,
never skipped, since skipping it would hide a live run from a newer node's
floor and recovery while builds are mixed. The handle rides as its SHA-256, because a handle has no
bound and every header has a widest form, counted in the most header a write
carries. No status rides: every status a record holds is one the listings keep.
The executor sessions' retention, which asks only which turns still have a
record, lists the same pass.

**The positions register declares its classes**, each at the bound its writer
holds a record to, a read deciding a write waiting its reply's crossing and
one deciding none its arrival (the reply at the floor in brackets):

| Class | Bound | Reads |
|---|---|---|
| `mcp_listing` | `coord.MaxMCPListingBytes`, 896 KiB | carrying (35.0 s) |
| `mcp_definition` | `coord.MaxMCPDefinitionBytes`, both of its bounds and 512 bytes | carrying (5.1 s) |
| `mcp_check` | `coord.MaxMCPCheckBytes`, 64 KiB | deciding (2.5 s) |
| `access_review` | `coord.MaxAccessReviewBytes`, 40 KiB | deciding (1.6 s) |
| `seat_access` | `coord.MaxSeatAccessBytes`, 256 KiB | deciding (10.0 s) |
| `home_board` | `coord.MaxHomeBoardRecordBytes`, the board and 256 bytes | deciding (20.1 s) |
| `llm_check` | `coord.MaxModelCheckBytes`, its answer and 4 KiB | deciding (2.7 s) |
| `mcp_oauth`, `integration_grant` | `coord.MaxOAuthGrantBytes`, 200 KiB | deciding (7.9 s) |
| `mcp_sign_in`, `integration_sign_in` | `coord.MaxOAuthSignInBytes`, its sealed half and 8 KiB | deciding (2.2 s) |
| `atlas_wake`, `llm_device_sign_in`, `grant_request` and every other family | `coord.SmallValueBytes` | the term |

Every other family is a small value: a node's positions (`node`), a trim
hold, floor and backup point, a capacity window (`maintenance`), its
admissions and acknowledgements, a retired node id (`retired`), a feed mark, a seat's pause, a box reading,
a conversation hold, a model retry, an MCP tool's acceptance and restart, an
SSH test, an issue report, an insight record and a Google identity. Each
record's widest is its fields' widest as stored, every byte of free text one
JSON escapes six-fold: each field is bounded where its record is validated,
an engine-written sentence cut to what it takes as stored (`coord.FitText`),
a value a person or the configuration gives refused naming its field, and a
grant request held whole to its class where it is filed. A capacity window,
which every data node's boot reads at the term, names at most 16
participants (`coord.MaxMaintenanceParticipants`), never counting a node id
an operator retired for good that has not run since, records at most 32
(`coord.MaxMaintenanceRecorded`), those retired included, and keeps its 8
newest journal records, dropping the oldest resolved ones and never an
unresolved one; a fleet past it opens no window, and a window whose fleet
outgrew either bound, or whose ninth request would be unresolved, is blocked
for a person and sends nothing. Every key the store writes to the register matches a declared
class, so its rest holds none.

Three writers cut to fit rather than refuse: an MCP check halves its
sentences, then its server's identity, keeping every node and stage; a device
sign-in halves its detail; an atlas wake names each change once. A seat's
access record holds every sentence and name a vendor or a step writes to a
kilobyte as encoded, and drops a link past two; a lane whose access would
outgrow the record, in its vendor's names and whichever way its steps end,
takes no step and is told to narrow it, while a lane going off is never held.
Every class a boot reads (a node's positions, a floor, a capacity window, an
admission) is small, read at the term, so no boot window moves. Two readers that cannot wait a crossing read less: the atlas
lists the MCP listings from the node's watched copy, never the store, and one
seat's access record (a turn's, a Google Cloud token's, a Slack install
link's) is read by its key, never its integration's ledger. The MCP beat reads the definitions a moved
catalogue names on its loop's context, each under its own read's crossing,
since a few at their bound outlast the beat's 15 s.

**The uploads bucket declares its classes**, each at the bound its writer
holds a record to. Every read of each decides a compare-and-set a caller
waits on, so each is deciding:

| Class | Bound | Reads |
|---|---|---|
| the reservation ledger, `ledger` | `coord.MaxUploadLedgerBytes`, 72 KiB | deciding (2.9 s) |
| an upload's record, `upload.<id>` | `coord.MaxUploadRecordBytes`, 2.5 MiB | deciding (100.0 s) |
| a seat's save ledger, `save.<seat>` | `coord.MaxSaveLedgerBytes`, 64 KiB | deciding (2.5 s) |

The two ledgers fit by their counts: 256 holds at most, and 512 saves. An
upload's record holds an intent's files and carried paths and a publish's
generated files, every path at its bound written as two bytes a byte at
most, since the record is stored without HTML escaping; its widest is
2,549,551 bytes. So a publish names at most a version's files before a byte
moves, and its warnings are cut to 50, each to 512 bytes, with one counting
the rest, in its first answer as in a retry's. Each file's type is the one it
is served as, never the client's. The outcome is written after the version
has landed, where a refusal could undo nothing, so every write before it
leaves room for the widest outcome (262,713 bytes) and close always fits. A
record past that, which only retries ending unknown that record other bytes
can reach, is refused by its writer before the retry stores a byte.

A refusal deletes and drops only the generated files its own attempt staged.
A generated file an earlier attempt recorded is dropped from the record
without being deleted, since that attempt's outcome is unknown and a version
it landed may name it: the object collector alone judges it, and its bytes
stay reserved, kept as a count on the record, until the hold lapses, when the
collector could have taken it. A record keeping such bytes is never released
early. The files an upload transferred stay held on the upload the refusal
reopens, so its next publish's close, or its early release where it keeps no
such bytes, still deletes those a landed version names (#1986).

A change of the ledger reads at most four records across its rounds to
release abandoned uploads, since each read is a record's crossing inside a
request, and a hold it read and could not release waits 15 minutes to be
read again, so the holds behind it are read. A change refused for its budget
still writes the releases and the waits it decided, and answers its refusal
however that write ends. A later round drops a released hold only while it
is what remains of the one released: the same attempt's, no more bytes (a
release lowers them) and no later lapse, whatever its window. Each hold names
the intent attempt it reserves for; only a retried intent that found its
record released takes the hold up, as a new attempt, never weakening it
(direct where either attempt is, the later window, lapse and bytes), and a
hold the releasing change makes for that upload draws an attempt of its own.
No boot or probe reads the bucket, so no boot window moves.

**The chat reads bucket declares its class**, a seat's read record, held to
the bound its writer keeps it within. Every read of it decides a flush a
person waits on, so it is deciding:

| Class | Bound | Reads |
|---|---|---|
| a seat's read record, `chatread.<handle>` | `coord.MaxChatReadBytes`, 144 KiB | deciding (5.7 s) |

Its sets are capped at 256 each; what gives the record a widest form
is that every room, message and thread root id it keeps is the UUID Chatlet
minted for it, 36 bytes JSON writes as itself. So a flush naming any other id
is refused, by the `mark_chat_read` tool naming its argument and by the merge
both backends share, and both store the record through one encoding that
refuses it past the bound. Its widest, every set at its cap, every position at
19 digits and every instant to the nanosecond, is 137,953 bytes. A pass or the
watch every node serving sockets keeps waits its raw crossing, 9.3 s at the
floor. No boot, probe or tick reads the bucket, so no boot window moves.

**The secrets bucket declares its class**, a credential's record, one key
per name. Its reads decide writes a caller waits on (a put keeping a
write-only mark, a create's lost round, a rekey's re-read, a grant's
renewal, a delete's existence read), so it is deciding:

| Class | Bound | Reads |
|---|---|---|
| a credential's record, under its name | `coord.MaxSecretRecordBytes`, 96 KiB | deciding (3.8 s) |

Every field the record keeps is bounded where it is written: the value is
sealed from at most 64 KiB under a key id of at most 255 bytes, the
provenance is a login's 64 bytes and an operation id's 128, refused past
them by every write of the store, and the name,
which is also the key, is at most 255 bytes (`secrets.MaxNameBytes`),
refused past it as an invalid name by every write, an operator's and the
engine's. Both backends store the record through one encoding that refuses
it past the bound. Its widest, every field at its bound in the
characters JSON spells longest, is 89,931 bytes. A pass waits its raw
crossing, 7.9 s at the floor. The one boot read, the secret snapshot, runs
after the node has recovered, where no probe bounds it, so no boot window
moves. The readers that read more than they need read less: the alarm beat,
every 10 s on every node, judges a token's expiry from the instant the
node's snapshot holds beside its value, never listing the store, as
Settings › Models dates it, and
`GET /secrets`, `crewlet secrets list` and its rekey's dry run answer their
names and the engine's count from one pass.

**The atlas buckets declare their classes**, each at the bound its writer
holds a record to:

| Class | Bound | Reads |
|---|---|---|
| a source's standing, `standing.<kind>` | `coord.MaxAtlasSourceStandingBytes`, 64 KiB | deciding (2.5 s) |
| a source's misses, `missing.<kind>` | `coord.MaxAtlasSourceMissesBytes`, 768 KiB | carrying (31.0 s) |
| a refresh ask; a code scan standing, `repo.<host>.<repo>` and `host.<host>` | `coord.SmallValueBytes` | the term |

A source's standing was one record holding the ids of every entry the source
stopped seeing, up to 1.1 MiB, and the collect duty's tick and every report
of the sources (a person's question, the dashboard's poll, the
`atlas_refresh` tool) listed every source's whole: at the floor a delivery
past about 169 KiB outlasts a pass's quiet. Clause 3 splits it. The standing
is what those readers need, the misses a count on it, and its read decides a
person's ask for a read. Its widest plain form is 53,043 bytes, 200 scopes
(one per server a company may declare, each id a host's) beside 16 refused
listings, and its writer cuts a vendor's text that escaping widens past the
bound: each listing's detail, then the last fault, then each listing's name,
keeping every count and scope. The misses, 8,192 entry ids at most, 770,092
bytes at the widest, are read by their key by the duty's visit of the one
source alone, and written before the standing that counts them: a visit
that cannot read them asks the vendor nothing, and one that cannot write
them leaves the standing counting those still recorded. A code scan
standing's widest is 13,763 bytes, a forge listing's stamp kept as its digest
and a forge's tip taken only as a commit id; each is filed under the host it
is read from, so a repository's pass reads its own by its key and a forge
lists its own repositories alone, the broker filtering each listing. No boot
or probe reads the buckets, so no boot window moves.

**The integrations bucket declares its classes**, and is a case of
clause 3: a surface's row was one value whose findings grow with the seats
(one per seat and lane, up to about 8,400 at 200 seats), listed whole by a
turn's atlas, the duty's tick and the beat's credential judgement. It is two
records now:

| Class | Bound | Reads |
|---|---|---|
| a surface's status, `status.<kind>` | `coord.MaxIntegrationStatusBytes`, 16 KiB | the term |
| its findings, `findings.<kind>` | `coord.MaxIntegrationFindingsBytes`, 64 KiB | carrying (2.6 s) |

The status names its findings by size and SHA-256, counts what a person owes,
what names a subject and the expiring credentials, and what the cut left out.
Every reader that keeps a window reads statuses alone: the duty's tick and a
turn's atlas their pass, every write's re-read under the surface's lease its
key. A pass writes its findings, then the status naming them; a reader whose
findings differ from the name reads them as unread, a failed findings pass
reads them all unread under statuses that stand, and a fold that found
nothing purges the surface's findings whatever its status named. The writers
cut rather than refuse: the status halves its fault, then its report's
sentence, and drops its link, keeping exact the address and the
registration's name it compares, whose settings are bounded instead (2,048
and 255 characters, as written); the findings keep each text to 500 bytes,
then, only while past the bound and from the tail, cut every word before any
name and what nobody owes before what a person does: agents' own findings
lose their words, then findings no person owes, then those a person owes;
findings that can name no seat go, nobody's first; subject lists are cut (the
reported finding's last); the rest go, nobody's first. The reported finding
stays first, never dropped and never without its words. Each finding is
counted once, by what the cut left of it, and dropped ones a person owes
apart; a cut may still hide a seat's name, counted apart again: nothing
bounds a company's seats, and 500 to 1,000 wordless findings naming one fill
the record. Its widest status is 14,801 bytes stored. 64 KiB, not 256: the
dashboard reads every surface's findings in one pass, about 46 s at the floor
for 24 at the bound, where 256 KiB would take three minutes.

**Findings not in hand are never none.** Where they cannot be read, or the
record cut one that may name a seat, a reader judging a seat says it cannot
tell: the credential judgement holds the child on that integration, saying
why, unless what is in hand shows a stop or an earlier judgement found one,
and the setup roster confirms no seat they may name. A cut that hides no seat
(findings about nothing, an advisory's list) changes neither. The Advisor
leaves the integrations unread only for findings it could not read, and reads
a cut record as a partial reading of what that record may have dropped alone
(a finding a person owes only where it dropped one a person owes), a finding
kept without its words keeping the words it had; the dashboard says it could
not read them, and how many it shows without their words; and the tools'
roll-up raises an expiring credential from the status's count.

The credential judgement (the beat's MCP rows, a boot's shared servers, a
seat's children, a run's launch) reads each minting integration's status by
its key and judges from it alone unless a person owes a finding that names
something; then from the findings the engine holds under the status's
digest, a newer record read on the engine's own context, never the caller's,
that reading judging the integration as it cannot say. So no boot window
moves. The Advisor duty's signature reads the digest, never the findings.

**The node status bucket declares its classes**, and is a case of clause 3:
a node's status was one record, written whole every reconcile, with a row per
configured MCP server and per model entry and nothing bounding the whole. It
is rows now, each a key of its own and each held to
`coord.MaxNodeStatusRowBytes` (16 KiB), read at the term:

| Class | Key | Widest |
|---|---|---|
| the node's own row: epoch, instant, runner cells, infra, runs waiting for an operator | `node_status.<node>` | 7,882 bytes |
| one configured MCP server | `mcp_status.<node>.<server>` | 6,105 bytes, and its unlisted keys |
| one model entry | `model_status.<node>.<entry>` | 4,725 bytes |
| the node's growth signals | `growth_status.<node>` | 15,230 bytes |

Growth is a row of its own: two lists of 32 signals take most of the bound
alone (35 of each would not fit). Each width is every text at exactly its
bound as stored, which plain text past it reaches and escaped text never
passes. Every text is held to its bound as stored, so a byte JSON writes as
six cannot pass it sixfold, and the harness's words a row names (a route, a
window, a kind, a state) are held to `coord.MaxModelWordBytes`. An MCP row's
unlisted tool_annotations keys are kept whole, sorted, as many as the row
holds beside its other parts, and counted where not all fit: a reader
intersecting them across nodes under-reports, never names a key a node
listed, and the answer says how many the list may stand for
(`unlisted_annotations_count`), so a cut list never reads as whole. Every row carries the epoch it was written under, and a reader counts
a node's rows only at the epoch of its own row and of its live presence, so a
row an earlier incarnation left, or one whose own row is gone, is never read.
A reader lists the own rows and the classes it needs, never the bucket: the
retry, the credential page and the Models page the model rows; the MCP pages
the MCP rows; the atlas both, and the alarm beat both where each is
configured; the Advisor's notices the growth rows; the runners page and the
resume alarm the own rows alone. It reads the own rows again after the other
classes and counts a node whole or not at all: one whose own row went or
moved epoch in between, or with a row at a later epoch than its own, is not
counted on that read. The writer writes each row that moved or whose
reconcile is due, the own row last, a write never landing over a later
epoch's row; it removes the rows of a server or entry no longer configured
up to its epoch, a row whose write's answer was lost among them, and a clean
stop removes every row up to it, its own first.

**Every other bucket holds small values, and no key falls to a rest.** A
bucket of one class states it as its rest (a credential's record, a small
value); one of several declares a class for every family it
holds, and the store writes no key outside them, so its rest, a small value,
holds none. Each record below is a small value read at the term, its widest
its fields' widest as stored, every text bounded where it is written: an
identifier or a word the engine chose refused past its bound before anything
is sent (a cut would name nothing), a person's or the configuration's
refused naming its field, a report's lists and errors cut to fit.

| Bucket | Its records at their widest |
|---|---|
| leases, duties | a lease: resource, owner and preferred node each `coord.MaxLeaseNameBytes` (256 bytes) beside its 8 KiB Meta, 9,190 bytes; the widest a writer names is a person's control of a browser, 118 bytes, and a growth act's lease is its account's SHA-256, whatever the account |
| epochs | a resource's epoch and preferred node, 571 bytes |
| sandbox runs | a run's record and each of its three parts, each a class of its own at a run's value; a session's claim, each part `coord.MaxSessionClaimPartBytes` (128 bytes, a box's session id's own bound), 420 bytes |
| config-apply status | three revisions and a status of `coord.MaxApplyNameBytes` (64 bytes) and an error cut to 2,000, 12,460 bytes |
| ledger | a completion's detail of `coord.MaxLedgerDetailBytes` (128), or a seen-through mark of `coord.MaxSeenMarkBytes` (128) |
| rate, counter and token windows | a count, or three slots each a label `period.At` cut, 356 bytes |
| claims, fires, rebases, custody | one or two instants and a node id (`coord.MaxNodeIDBytes`) |
| channels, follows, mailboxes | seat handles (`iam.MaxLogin`), a follow reason of `coord.MaxFollowReasonBytes` (64) |
| objects | the agreed store, `store.objects.s3`'s endpoint, bucket and prefix held to 2,048, 255 and 1,024 characters, 13,292 bytes; the collector's report, naming its store by the identity's SHA-256, each sentence cut to 1 KiB and each file's name to 512 bytes as stored, its unreadable files the longest opening of their list that fits, every count whole; the trash reaper's, at most 16 sources each named in 64 bytes, each source's error cut to 512 |

Two gates hold every key to a declared class. The contract suites record
every put as the broker carries it and fail a key falling to the rest of a
bucket of several classes, a value past its class's bound, or a declared
class nothing wrote. A reading of the package's source fails a class
constant, a key function built on one, or an entryClass's filter that no
holding of the bucket its declaration touches declares, so a write no case
reaches is held too. No boot window moves: every class a boot reads is a
small value at the term.

## Why the obvious alternatives are wrong

**A longer deadline for every read.** A probe's budget is the orchestrator's,
not ours: no deadline makes an 8 MiB reply cross a floor link in 1.875 s.

**Cutting the document smaller.** The bound on a company is what one key
holds; the pointer is small only if the document is not in it.

**Keying the body by revision, or one shared body key.** Two writers' bodies
then meet on one key, and a flip can name a body the other overwrote, which is
why the body once moved into the pointer. Content addressing gives each set of
bytes its own key.

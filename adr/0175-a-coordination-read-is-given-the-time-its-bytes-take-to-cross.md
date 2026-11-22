# ADR-0175: A coordination read is given the time its bytes take to cross

- **Status:** accepted
- **Authority:** `internal/coord`
- **Enforced-by:** `internal/coord/kv.TestAPointerReadAcrossALeafsLinkCarriesNoBody`, `internal/coord/kv.TestABodyAtItsBoundIsReadAcrossALeafsLinkAtItsFirstAsk`, `internal/coord/kv.TestABodysReadIsGivenTheArrivalOfItsRecordedSize`, `internal/coord/kv.TestAFlipLandsWithinHalfTheBodysRetentionOfItsWrite`, `internal/coord/kv.TestAFlipIsGivenTwiceTheBodysWriteAtItsBound`, `internal/coord/kv.TestABodysCrossingIsHeldToWhatAKeyHolds`, `internal/maintenance.TestTheBodySweepRunsUnderTheDutyAtItsOwnHorizon`, `internal/engine.TestAPostureAcrossASlowLinkIsReadNotAssumed`, `internal/engine.TestAReconcilerAcrossASlowLinkAppliesARevisionAtItsBound`, `internal/coord/kv.TestEveryReadIsGivenTheCrossingOfItsClassBound`, `internal/coord/kv.TestASmallClassCrossesTheFloorInsideTheReadTerm`, `internal/coord/kv.TestASettledWriteIsHeldToItsClassBound`, `internal/coord/kv.TestARunsListingAcrossALeafsLinkDeliversItsRecordsInItsPass`, `internal/coord/kv.TestAWatchAcrossASlowLinkDeliversARecordLongerThanTwoHeartbeatsToCross`, `internal/coord/kv.TestTheMarkerSweepAcrossALinkCarriesHeadersOnly`, `internal/coord/kv.TestNothingWatchesASettledBucketThroughTheClient`, `internal/coord/kv.TestEveryClientWatchOfASettledBucketIsRefused`, `internal/coord/kv.TestAPassOpensNoConsumerUntilItsKeyIndexAnswers`, `internal/coord/kv.TestARunsStandingCrossesALeafsLinkWithoutItsRecords`, `internal/coord/kv.TestAStandingLandsInTheMessageOfItsRecord`, `internal/coord/kv.TestTheWidestStandingIsCountedInKvHeaderBytes`, `internal/coord/kv.TestACoordinationValueIsHeldToWhatItsLeadersReadCarriesBackAcrossALink`, `internal/sandbox.TestEveryWriteOfARunStatesItsStanding`, `internal/sandbox.TestTheEpochFloorReadsNoStatedRecordWhole`, `internal/sandbox.TestASeatsListingReadsOnlyItsOwnRecordsWhole`, `internal/engine.TestALeafBootsBesideRunsAtTheirBound`, `internal/engine.TestAnMCPCheckOfEveryNodeAtItsWidestIsRecordedWithinItsBound`, `internal/engine.TestADeviceSignInsDetailIsCutToFitItsRecord`, `internal/seataccess.TestASeatWhoseAccessOutgrowsItsLedgerRecordIsNotSynced`, `internal/coord.TestEveryPositionsClassesWidestRecordFitsItsBound`, `internal/coord/kv.TestEveryPositionsClassAtItsBoundIsReadAcrossALeafsLinkAtItsFirstAsk`, `internal/engine.TestTheMCPBeatReadsDefinitionsOnTheLoopsContext`, `internal/engine.TestTheAtlasReadsTheFleetsListingsFromTheWatchedCopy`
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
| `atlas_wake`, `llm_device_sign_in` | `coord.SmallValueBytes` | the term |

Three writers cut to fit rather than refuse: an MCP check halves its
sentences, then its server's identity, keeping every node and stage; a device
sign-in halves its detail; an atlas wake names each change once. A seat whose
access would outgrow its record is not synced, and is told to narrow its
scope, before any vendor step. Every other key, every class a boot lists among
them, stays undeclared, so no boot window moves. Two readers that cannot wait
a crossing read less: the atlas lists the MCP listings from the node's watched
copy, never the store, and a turn reads its seat's access record by its key,
never its integration's ledger. The MCP beat reads the definitions a moved
catalogue names on its loop's context, each under its own read's crossing,
since a few at their bound outlast the beat's 15 s.

## Why the obvious alternatives are wrong

**A longer deadline for every read.** A probe's budget is the orchestrator's,
not ours: no deadline makes an 8 MiB reply cross a floor link in 1.875 s.

**Cutting the document smaller.** The bound on a company is what one key
holds; the pointer is small only if the document is not in it.

**Keying the body by revision, or one shared body key.** Two writers' bodies
then meet on one key, and a flip can name a body the other overwrote, which is
why the body once moved into the pointer. Content addressing gives each set of
bytes its own key.

## What this does not decide

Every bucket but the runs, the config and the positions register's classes
above declares no class yet: its values are held to `coord.MaxValueBytes` and
read at the term, which only its records' own fields keep small. The rest of
#1814 declares them: the other buckets and the positions register's rest, then
a default class of `coord.SmallValueBytes`, with a gate holding every key to a
declared class.

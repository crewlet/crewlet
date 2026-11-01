package metrics

// THE INSTRUMENT NAMES, as constants.
//
// # Why a name is a constant and not a literal at its call site
//
// Every instrument is named in at least two places — the catalogue that
// declares it, which is what docs/reference/metrics.md is generated from, and
// the code that records into it — and several are named in a third, the code
// that reads a percentile back to evaluate an alarm. A recorder rejects a name
// it has no instrument for, so a typo at a WRITE site is caught immediately;
// a typo at a READ site is not caught at all. It silently reads no series,
// which renders as a healthy zero.
//
// So the name exists once and every site refers to it. The catalogue below is
// the declaration; these are its identifiers.
const (
	AlarmActive                      = "crewlet.alarm.active"
	AlarmTransitions                 = "crewlet.alarm.transitions"
	AlarmTransitionsDropped          = "crewlet.alarm.transitions.dropped"
	AuthAttemptsFailed               = "crewlet.auth.attempts.failed"
	BackupAge                        = "crewlet.backup.age"
	BackupDuration                   = "crewlet.backup.duration"
	BackupHolds                      = "crewlet.backup.holds"
	BrokerPeerRefusals               = "crewlet.broker.peer.refusals"
	CodeBoxInflightRefused           = "crewlet.code.box.inflight_refused"
	CodeBoxResyncs                   = "crewlet.code.box.resyncs"
	CodeCacheAdoptions               = "crewlet.code.cache.adoptions"
	CodeCacheBytes                   = "crewlet.code.cache.bytes"
	CodeCacheChained                 = "crewlet.code.cache.chained"
	CodeCacheCheckDuration           = "crewlet.code.cache.check.duration"
	CodeCacheChecks                  = "crewlet.code.cache.checks"
	CodeCacheDroppingBytes           = "crewlet.code.cache.dropping.bytes"
	CodeCacheFullRounds              = "crewlet.code.cache.full_rounds"
	CodeCacheHintHashes              = "crewlet.code.cache.hint.hashes"
	CodeCacheHints                   = "crewlet.code.cache.hints"
	CodeCacheLayerInstall            = "crewlet.code.cache.layer.install"
	CodeCacheLayers                  = "crewlet.code.cache.layers"
	CodeCacheMaintenanceRefusals     = "crewlet.code.cache.maintenance.refusals"
	CodeCacheMaintenanceRefused      = "crewlet.code.cache.maintenance.refused"
	CodeCacheMemoBytes               = "crewlet.code.cache.memo.bytes"
	CodeCacheMemoLookups             = "crewlet.code.cache.memo.lookups"
	CodeCacheProvenanceBackfilled    = "crewlet.code.cache.provenance.backfilled"
	CodeCacheProvenanceBehind        = "crewlet.code.cache.provenance.behind"
	CodeCacheReadPageDuration        = "crewlet.code.cache.read.page.duration"
	CodeCacheRebuildRefusals         = "crewlet.code.cache.rebuild.refusals"
	CodeCacheRebuilds                = "crewlet.code.cache.rebuilds"
	CodeCacheReports                 = "crewlet.code.cache.reports"
	CodeCacheRepos                   = "crewlet.code.cache.repos"
	CodeCacheSwaps                   = "crewlet.code.cache.swaps"
	CodeCacheSyncDuration            = "crewlet.code.cache.sync.duration"
	CodeChangeProposals              = "crewlet.code.change.proposals"
	CodeChecksClaims                 = "crewlet.code.checks.claims"
	CodeChecksDuration               = "crewlet.code.checks.duration"
	CodeChecksFinished               = "crewlet.code.checks.finished"
	CodeChecksQueued                 = "crewlet.code.checks.queued"
	CodeChecksRunning                = "crewlet.code.checks.running"
	CodeCommitBatchSize              = "crewlet.code.commit.batch.size"
	CodeCommitBusy                   = "crewlet.code.commit.busy"
	CodeCompactBehind                = "crewlet.code.compact.behind"
	CodeCompactDuration              = "crewlet.code.compact.duration"
	CodeCompactRefusals              = "crewlet.code.compact.refusals"
	CodeCompactRuns                  = "crewlet.code.compact.runs"
	CodeConversationWrites           = "crewlet.code.conversation.writes"
	CodeExpungeDroppedBytes          = "crewlet.code.expunge.dropped_bytes"
	CodeExpungeDuration              = "crewlet.code.expunge.duration"
	CodeExpungeRuns                  = "crewlet.code.expunge.runs"
	CodeFetchDuration                = "crewlet.code.fetch.duration"
	CodeGitCPU                       = "crewlet.code.git.cpu"
	CodeGitLentCores                 = "crewlet.code.git.lent_cores"
	CodeGitProcesses                 = "crewlet.code.git.processes"
	CodeGitShare                     = "crewlet.code.git.share"
	CodeGitShareWait                 = "crewlet.code.git.share_wait"
	CodeGitWait                      = "crewlet.code.git.wait"
	CodeHomeFactsDuration            = "crewlet.code.home.facts.duration"
	CodeHomeMergeDuration            = "crewlet.code.home.merge.duration"
	CodeHomeNudges                   = "crewlet.code.home.nudges"
	CodeHomePasses                   = "crewlet.code.home.passes"
	CodeHostSpoolWait                = "crewlet.code.host.spool.wait"
	CodeHostSpoolWaits               = "crewlet.code.host.spool.waits"
	CodeHostWriteDuration            = "crewlet.code.host.write.duration"
	CodeHostWrites                   = "crewlet.code.host.writes"
	CodeIndexBuilds                  = "crewlet.code.index.builds"
	CodeIndexBytes                   = "crewlet.code.index.bytes"
	CodeSearchQueries                = "crewlet.code.search.queries"
	CodeSearchDuration               = "crewlet.code.search.duration"
	CodeSearchCoverage               = "crewlet.code.search.coverage"
	CodePublishBytes                 = "crewlet.code.publish.bytes"
	CodeQueueProposals               = "crewlet.code.queue.proposals"
	CodeMergeOutcomes                = "crewlet.code.merge.outcomes"
	CodePublishDuration              = "crewlet.code.publish.duration"
	CodePublishRefusals              = "crewlet.code.publish.refusals"
	CodePublishRuns                  = "crewlet.code.publish.runs"
	CodePushBytes                    = "crewlet.code.push.bytes"
	CodePushDuration                 = "crewlet.code.push.duration"
	CodeSeatworkHeld                 = "crewlet.code.seatwork.held"
	CodeSeatworkWaits                = "crewlet.code.seatwork.waits"
	HistoryAnswers                   = "crewlet.history.answers"
	ObjectsS3Attempts                = "crewlet.objects.s3.attempts"
	ObjectsS3GateLimit               = "crewlet.objects.s3.gate.limit"
	ObjectsS3GateWaiting             = "crewlet.objects.s3.gate.waiting"
	ObjectsS3RequestAttempts         = "crewlet.objects.s3.request.attempts"
	StatelogAnchorsSwept             = "crewlet.statelog.anchors.swept"
	StatelogAppliedThrough           = "crewlet.statelog.applied_through"
	StatelogApplyBatchRows           = "crewlet.statelog.apply.batch.rows"
	StatelogApplyLagSeconds          = "crewlet.statelog.apply.lag.seconds"
	StatelogApplyLagSeq              = "crewlet.statelog.apply.lag.seq"
	StatelogApplyLatency             = "crewlet.statelog.apply.latency"
	StatelogApplyRecordDuration      = "crewlet.statelog.apply.record.duration"
	StatelogApplyRecords             = "crewlet.statelog.apply.records"
	StatelogApplyRetries             = "crewlet.statelog.apply.retries"
	StatelogApplyTxAborts            = "crewlet.statelog.apply.tx.aborts"
	StatelogApplyTxDuration          = "crewlet.statelog.apply.tx.duration"
	StatelogBarrierAppends           = "crewlet.statelog.barrier.appends"
	StatelogBarriersApplied          = "crewlet.statelog.barrier.applied"
	StatelogBarrierDuration          = "crewlet.statelog.barrier.duration"
	StatelogDeferredCount            = "crewlet.statelog.deferred.count"
	StatelogDeferredOldestAgeSeconds = "crewlet.statelog.deferred.oldest_age_seconds"
	StatelogDrainCommitsPerSecond    = "crewlet.statelog.drain.commits_per_second"
	StatelogDrainRowsPerSecond       = "crewlet.statelog.drain.rows_per_second"
	StatelogLingerYields             = "crewlet.statelog.linger.yields"
	StatelogLogBytes                 = "crewlet.statelog.log.bytes"
	StatelogLogBytesPerDay           = "crewlet.statelog.log.bytes_per_day"
	StatelogLogHeadroomFraction      = "crewlet.statelog.log.headroom_fraction"
	StatelogLogMaxBytes              = "crewlet.statelog.log.max_bytes"
	StatelogPublishConflicts         = "crewlet.statelog.publish.conflicts"
	StatelogPublishDuration          = "crewlet.statelog.publish.duration"
	StatelogPublishOutcomes          = "crewlet.statelog.publish.outcomes"
	StatelogPublishRefusals          = "crewlet.statelog.publish.refusals"
	StatelogPublishRejections        = "crewlet.statelog.publish.rejections"
	StatelogPublishRounds            = "crewlet.statelog.publish.rounds"
	StatelogReadRefusals             = "crewlet.statelog.read.refusals"
	StatelogReadServed               = "crewlet.statelog.read.served"
	StatelogReadWait                 = "crewlet.statelog.read.wait"
	StatelogRecordsGated             = "crewlet.statelog.records_gated"
	StatelogRecordsTampered          = "crewlet.statelog.records_tampered"
	StatelogRecordsUnverifiable      = "crewlet.statelog.records_unverifiable"
	StatelogTrimBlockedSeconds       = "crewlet.statelog.trim.blocked_seconds"
	StatelogWaiters                  = "crewlet.statelog.waiters"
	StatelogWriteSessionWait         = "crewlet.statelog.write.session_wait"
	StoreBytes                       = "crewlet.store.bytes"
	StorePoolWait                    = "crewlet.store.pool.wait"
	StoreWalBytes                    = "crewlet.store.wal.bytes"
	TrackerBulkApplySeconds          = "crewlet.tracker.bulk.apply_seconds"
	TrackerBulkCalls                 = "crewlet.tracker.bulk.calls"
	TrackerFeedUnreadable            = "crewlet.tracker.feed.unreadable"
	TrackerSearchAnswers             = "crewlet.tracker.search.answers"
	TrackerSearchConcurrency         = "crewlet.tracker.search.concurrency"
	TrackerSearchScanDuration        = "crewlet.tracker.search.scan.duration"
	TrackerVectorCoverage            = "crewlet.tracker.vector.coverage"
)

// THE ARTIFACTS INSTRUMENTS, named here before their recording sites exist so
// every package that records one and every reader that alarms on one spells it
// the same. Each is CATALOGUED by the package that records it, beside its
// first recording site, because a catalogued instrument with no recording site
// fails the build (an instrument nobody records reads as a healthy zero). Until
// then a name here is declared and published nowhere.
const (
	// The object store's own ceiling, recorded by internal/objstore.
	ObjectsBucketBytes    = "crewlet.objects.bucket.bytes"
	ObjectsBucketMaxBytes = "crewlet.objects.bucket.max_bytes"

	// Uploads and publishes, recorded by internal/artifactupload.
	ArtifactsUploadBytes         = "crewlet.artifacts.upload.bytes"
	ArtifactsUploadReservedBytes = "crewlet.artifacts.upload.reserved_bytes"
	ArtifactsPublishDuration     = "crewlet.artifacts.publish.duration"
	ArtifactsPublishRefusals     = "crewlet.artifacts.publish.refusals"

	// The artifact origin, recorded by internal/artifactserve.
	ArtifactsServeRequests   = "crewlet.artifacts.serve.requests"
	ArtifactsServeDuration   = "crewlet.artifacts.serve.duration"
	ArtifactsServeBytes      = "crewlet.artifacts.serve.bytes"
	ArtifactsTicketRefusals  = "crewlet.artifacts.ticket.refusals"
	ArtifactsAuthzPeerChecks = "crewlet.artifacts.authz.peer_checks"

	// The datastore, recorded by internal/artifactdata.
	ArtifactsDataWriteDuration  = "crewlet.artifacts.data.write.duration"
	ArtifactsDataWrites         = "crewlet.artifacts.data.writes"
	ArtifactsDataFootprintBytes = "crewlet.artifacts.data.footprint.bytes"

	// Presence and pushes, recorded by internal/presence and
	// internal/api/stream.
	PresenceFrames   = "crewlet.presence.frames"
	PresenceRooms    = "crewlet.presence.rooms"
	ArtifactsPushLag = "crewlet.artifacts.push.lag"

	// The browser-reported time from a ticket to a ready frame, and every
	// service objective's good and bad events, recorded by
	// internal/objectives.
	ArtifactsReadyDuration = "crewlet.artifacts.ready.duration"
	ObjectivesEvents       = "crewlet.objectives.events"
)

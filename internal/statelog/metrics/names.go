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
	BackupAge                        = "crewlet.backup.age"
	BackupDuration                   = "crewlet.backup.duration"
	BackupHolds                      = "crewlet.backup.holds"
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
	StatelogBarrierDuration          = "crewlet.statelog.barrier.duration"
	StatelogDeferredCount            = "crewlet.statelog.deferred.count"
	StatelogDeferredOldestAgeSeconds = "crewlet.statelog.deferred.oldest_age_seconds"
	StatelogDrainCommitsPerSecond    = "crewlet.statelog.drain.commits_per_second"
	StatelogDrainRowsPerSecond       = "crewlet.statelog.drain.rows_per_second"
	StatelogLingerYields             = "crewlet.statelog.linger.yields"
	StatelogLogBytes                 = "crewlet.statelog.log.bytes"
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
	StatelogTrimBlockedSeconds       = "crewlet.statelog.trim.blocked_seconds"
	StatelogWaiters                  = "crewlet.statelog.waiters"
	StatelogWriteSessionWait         = "crewlet.statelog.write.session_wait"
	StoreBytes                       = "crewlet.store.bytes"
	StorePoolWait                    = "crewlet.store.pool.wait"
	StoreWalBytes                    = "crewlet.store.wal.bytes"
	TrackerBulkApplySeconds          = "crewlet.tracker.bulk.apply_seconds"
	TrackerBulkCalls                 = "crewlet.tracker.bulk.calls"
	TrackerSearchAnswers             = "crewlet.tracker.search.answers"
	TrackerSearchConcurrency         = "crewlet.tracker.search.concurrency"
	TrackerSearchScanDuration        = "crewlet.tracker.search.scan.duration"
	TrackerVectorCoverage            = "crewlet.tracker.vector.coverage"
)

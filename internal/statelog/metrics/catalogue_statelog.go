package metrics

// statelogInstruments is the state log's instruments: its write and read paths, the applier, position, retention, the
// backup, the embedded broker, the fleet's history and the store.
func statelogInstruments() []Instrument {
	return []Instrument{
		// ---- the write path -------------------------------------------
		{
			Name: StatelogPublishDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"domain", "outcome"},
			Shows: "A write path slowing down before it starts refusing. The " +
				"outcome dimension separates the three answers a write has, " +
				"so a rise in `pending` reads as an applier falling behind " +
				"rather than as a broker getting slower.",
		},
		{
			Name: StatelogPublishRounds, Kind: KindHistogram, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "Contention on one subject, which the compare-and-set round " +
				"cap bounds and nothing measured. A distribution creeping " +
				"toward the cap is a hot object; reaching it is a refusal a " +
				"model reads as a colleague editing the same thing.",
		},
		{
			Name: StatelogPublishOutcomes, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain", "outcome"},
			Shows: "The three-valued write outcome, counted, and only the " +
				"three — a refusal is on `publish.refusals` instead, because " +
				"it says the write never happened at all. `unknown` is the " +
				"one that matters most and had no counter: a broker flapping " +
				"into ambiguity was visible only to the model that received " +
				"the answer.",
		},
		{
			Name: StatelogPublishConflicts, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain", "subject_kind"},
			Shows: "Writes that spent their whole round budget losing races " +
				"on one subject, BY KIND. The refusal counter beside it " +
				"says a conflict happened and not what it was about, and " +
				"the remedy differs entirely: one contended object is a " +
				"design question and a contended kind is a hot subject.",
		},
		{
			Name: StatelogPublishRejections, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain", "subject_kind"},
			Shows: "How often a write loses a race, per kind of subject. It is " +
				"what says whether a counter, a rank order or an ordinary " +
				"object is the contended one.",
		},
		{
			Name: StatelogPublishRefusals, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain", "reason"},
			Shows: "Writes refused before or instead of an append, by `reason`, and " +
				"EVERY value it carries is here, each with its remedy. The node: " +
				"`evicted` (this node is removed from the fleet — run the write " +
				"on another), `eviction_unknown` (its own eviction state could " +
				"not be read — clears when it is read again), `deferred` (it " +
				"holds a record it cannot decode — a " +
				"newer build serves it), `behind` (it has not applied a position " +
				"the write needs — clears on its own), `below_floor` (it is below " +
				"the log and must adopt a snapshot — another node writes " +
				"meanwhile), `floor_unknown` (the domain's floor or the log's " +
				"ends could not be read — clears when coordination answers, or " +
				"once a floor row that does not read as its floor is removed), " +
				"`usage_unknown` (the log's usage could not be read before an " +
				"ordinary append, so nothing was appended; clears when the broker " +
				"answers again, typically once an election ends). " +
				"The log: " +
				"`log_full` (at its ceiling — raise it or unblock the trim), " +
				"`record_too_large` (the record is larger than the broker takes " +
				"in one message — split the change; the detail names which " +
				"limit refused it), `broker_refused` (a refusal this build has " +
				"no remedy for — the detail carries the broker's own words), " +
				"`skew` (a store and a stream restored out of step — restore " +
				"both from one backup), `wrong_stream` (not the log this node's " +
				"rows came from: rebuilt, ending below its checkpoint, holding " +
				"another record there, or re-anchored past it by a peer — " +
				"re-anchor, or adopt where a peer holds the generation), " +
				"`log_truncated` (it lost records a peer's rows hold — settle " +
				"which history the fleet keeps). The object: `deleted` (a " +
				"permanent deletion marker — nothing recreates it). The " +
				"operation: `op_reused` (its id names a record on another " +
				"object) and `superseded` (its record landed and a later one " +
				"undid it) — both answered by a NEW operation under a fresh " +
				"id, never by a retry. A record that landed and a gate dropped " +
				"is counted under the gate that dropped it — `evicted`, " +
				"`deleted`, `abandoned` (written in a generation a reanchor abandoned) or " +
				"`overtaken` (written after a restored reanchor, below its " +
				"generation) — and is never re-decided, because republishing " +
				"makes another record nothing applies. Such a record holds its " +
				"operation id for the log's duplicate window, so the same id sent " +
				"inside it, by any node, is collapsed onto the record and refused " +
				"the same way: the write is retried under a fresh id, or under the " +
				"same one once the window has passed. Beside the refusals: " +
				"`conflict` for a write that lost every round, `exists` for a " +
				"create whose object already exists, and `error` for a write " +
				"that failed before it could answer. A refusal is not one of " +
				"the three outcomes: it says the write never happened, and each " +
				"reason has a different remedy, so one counter with an outcome " +
				"dimension would hide them all.",
		},
		{
			Name: StatelogWriteSessionWait, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"domain"},
			Shows: "The wait a write pays for its own previous write to apply. " +
				"Zero when the caller is caught up, which is the common case, " +
				"and the shape of the load that is not.",
		},

		// ---- the read path --------------------------------------------
		{
			Name: StatelogBarrierDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"domain"},
			Shows: "The broker round trip under every linearizable read, and " +
				"the first number a drifting fsync or a degrading quorum " +
				"moves. It was a benchmark's p50 on an idle loopback cluster " +
				"and nothing in production.",
		},
		{
			Name: StatelogReadWait, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"domain", "level"},
			Shows: "How much of the read budget a barrier or session wait " +
				"actually spends. A p95 approaching the budget is reads about " +
				"to start refusing, which is the warning the refusal itself " +
				"is too late to be. What a read waits for is this node " +
				"reaching the position: every record up to it that writes a " +
				"row committed, every barrier decoded — never a barrier's " +
				"own commit, so a disk's stalled fsync shows here only " +
				"behind a commit that writes a row.",
		},
		{
			Name: StatelogReadRefusals, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain", "level", "code"},
			Shows: "Every refusal code, counted. The codes, each with its own " +
				"remedy, had no counter between them, so an operator had no " +
				"rejection rate for any of them.",
		},
		{
			Name: StatelogReadServed, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain", "level"},
			Shows: "Reads answered per level, which is the denominator every " +
				"refusal fraction needs and the check on the assumed read " +
				"rate the log's own size is derived from.",
		},
		{
			Name: StatelogBarrierAppends, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "Barrier records appended. Against reads served it is the " +
				"single-flight ratio, which says whether coalescing is doing " +
				"anything at all.",
		},
		{
			Name: StatelogBarriersApplied, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain", "stream"},
			Shows: "Barrier records applied from each log — every node's " +
				"linearizable reads on it, since every data node applies every " +
				"record. It is the rate `census_drift` holds against the " +
				"census, per log: a node's own appends are only its share of " +
				"the fleet's reads, and on a fleet of several serving nodes " +
				"they describe a company that many times quieter than it is. " +
				"The alarm's 24-hour window files each barrier at the hour the " +
				"broker stored it, so a node replaying a backlog does not read " +
				"days of reads as one; this cumulative series counts them as " +
				"they are applied, so it jumps by a backlog's barriers when " +
				"one is replayed.",
		},
		{
			Name: StatelogLingerYields, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "How often a waiter cut a batch short. It is the batching " +
				"the applier gives up to answer a read promptly, and without " +
				"it that trade is invisible.",
		},

		// ---- the applier ----------------------------------------------
		{
			Name: StatelogApplyLatency, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"domain"},
			Shows: "THE COMMIT-TO-APPLY GAP: from the broker's own timestamp " +
				"on a record to this node committing it. Every read level is " +
				"a policy about this quantity and nothing measured it.",
		},
		{
			Name: StatelogApplyTxDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"domain", "bound_by"},
			Shows: "How long one apply transaction holds the store's writer, " +
				"and which budget ended it. A transaction with rows is what " +
				"every waiter behind it pays, and rows were only ever a " +
				"proxy for the duration. A run of nothing but barriers " +
				"commits beside the apply loop, one transaction at a time " +
				"carrying every barrier since the last: no reader waits on " +
				"it, and a transaction with rows waits for it to return.",
		},
		{
			Name: StatelogApplyRecordDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"domain", "kind"},
			Shows: "One record's apply. A single record past the time budget " +
				"is still one transaction, so this is the real ceiling on how " +
				"long a read can be delayed — a sentence in a design document " +
				"until it was measured.",
		},
		{
			Name: StatelogApplyBatchRows, Kind: KindHistogram, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "Rows per apply transaction, which is what the row budget " +
				"bounds and what the drain rate divides.",
		},
		{
			Name: StatelogApplyRecords, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain", "result"},
			Shows: "Records consumed, by what happened to them: applied, " +
				"retained, gated, skipped, or reprocessed by a build that could " +
				"read what an earlier one retained — a retained record a gate " +
				"drops when it is reprocessed is counted `gated`, as it would " +
				"have been live, since it wrote no row. A node applying nothing " +
				"while its position advances is healthy on lag alone.",
		},
		{
			Name: StatelogApplyRetries, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "Attempts the apply loop retried in place after a failure " +
				"that was not a stop — a fetch the broker did not answer, a " +
				"transaction the disk refused, an applier that errored. A rate " +
				"that stays up past the retry budget is a node whose rows have " +
				"stopped moving, and its health says so.",
		},
		{
			Name: StatelogApplyTxAborts, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "Apply transactions whose body the store ran more than " +
				"once, because an attempt failed transiently after it began. " +
				"It reads zero by construction: this database detects write " +
				"conflicts per file, so every write transaction takes the " +
				"file's lock at its BEGIN and queues for it, and no commit " +
				"elsewhere in the file can abort an apply. A non-zero count " +
				"on the operator's own hardware means the retry budget is " +
				"being spent rather than held in reserve.",
			// Not apply.retries: that counts the apply loop retrying a
			// failed attempt; this counts the store re-running one
			// transaction's body.
		},
		{
			Name: StatelogRecordsGated, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"gate", "subject_kind"},
			Shows: "Records an apply gate dropped, by the gate that dropped " +
				"each — the domain's own (`evicted`, `deleted`) or " +
				"the framework's (`abandoned`, `overtaken`). " +
				"Each node counts a record once, where its own applier drops it, " +
				"when the transaction that drops it commits — never once per " +
				"attempt at that transaction. A write refused because its " +
				"record — or another node's copy it was collapsed onto — was " +
				"dropped is a refusal, counted under " +
				"`crewlet.statelog.publish.refusals`, and not a second drop. " +
				"A dropped commit is recoverable by nothing, and this is the " +
				"only place anyone would see that it happened.",
		},
		{
			Name: StatelogRecordsTampered, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "Records whose signature failed under a key this node " +
				"holds, which means something that is not this fleet wrote " +
				"to the broker. The applier stops on the first one, so this " +
				"counts at most one per domain per stop, and any value above " +
				"zero is an incident rather than a rate.",
		},
		{
			Name: StatelogRecordsUnverifiable, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "Records signed under a key this node does not hold, " +
				"retained until it does. Expected and transient during a " +
				"keyring rotation; sustained, it means a key reached one " +
				"node's secrets.keys and not this one's.",
		},
		{
			Name: StatelogDrainRowsPerSecond, Kind: KindGauge, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "The applier's observed drain, which every retry hint " +
				"divides by. Seeded from a benchmark and then measured, so a " +
				"hint on real hardware stops being an extrapolation from " +
				"somebody else's.",
		},
		{
			Name: StatelogDrainCommitsPerSecond, Kind: KindGauge, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "Commits per second, which is the fsync rate under " +
				"`synchronous = FULL` and the number a device budget is " +
				"spent by.",
		},

		// ---- position and health --------------------------------------
		{
			Name: StatelogApplyLagSeq, Kind: KindGauge, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows:      "How many records this node is behind the log's head.",
		},
		{
			Name: StatelogApplyLagSeconds, Kind: KindGauge, Unit: UnitSeconds,
			Attributes: []string{"domain"},
			Shows: "How OLD the oldest unapplied record is. Seconds are what " +
				"a stall grace, a pending outcome and a copy taken out of " +
				"service all turn on; sequences are not, and a lag of 4 000 " +
				"says nothing about whether anything is wrong.",
		},
		{
			Name: StatelogAppliedThrough, Kind: KindGauge, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "The prefix this node has actually applied, which is lower " +
				"than its checkpoint whenever a record was retained.",
		},
		{
			Name: StatelogDeferredCount, Kind: KindGauge, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "Records this build could not read and kept. Non-zero is a " +
				"rolling upgrade in progress; non-zero and not falling is one " +
				"that stopped.",
		},
		{
			Name: StatelogDeferredOldestAgeSeconds, Kind: KindGauge, Unit: UnitSeconds,
			Attributes: []string{"domain"},
			Shows: "How long the oldest retained record has been retained, " +
				"which is what decides whether this node's copy stays in service.",
		},
		{
			Name: StatelogWaiters, Kind: KindGauge, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "Callers blocked on the applier right now. It is the depth " +
				"of the queue a slow apply is making.",
		},

		// ---- retention and capacity -----------------------------------
		{
			Name: StatelogLogBytes, Kind: KindGauge, Unit: UnitBytes,
			Attributes: []string{"domain"},
			Shows:      "What the log actually holds, against its ceiling below.",
		},
		{
			Name: StatelogLogBytesPerDay, Kind: KindGauge, Unit: UnitBytesPerDay,
			Attributes: []string{"domain"},
			Shows: "What the log took in over the trailing day, measured from " +
				"its own records at each trim tick. The ceiling has to hold " +
				"`min_age` of this, because the trim never removes a record " +
				"younger than that — a log whose ceiling is smaller fills with " +
				"every trim term satisfied, which is what `log_ceiling_short` " +
				"fires on. Absent for a compacted log and for one in its first " +
				"two days, whose trailing day would contain the day it was " +
				"imported into; a log that could not be read keeps its last " +
				"measurement, as the report does.",
		},
		{
			Name: StatelogLogMaxBytes, Kind: KindGauge, Unit: UnitBytes,
			Attributes: []string{"domain"},
			Shows: "The ceiling, read from the running stream rather than " +
				"from this node's own configuration — the two differ, and the " +
				"running one is what refuses the append.",
		},
		{
			Name: StatelogLogHeadroomFraction, Kind: KindGauge, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "How much of the ceiling ordinary writes are held to is " +
				"left on every log that claims identity (Worklet's, Doclet's, " +
				"the identity estate's, the artifacts log's, the artifact " +
				"datastore's, Ask Crewlet's, Chatlet's, the executor log's and " +
				"the code log's): the ceiling " +
				"less the gate reserve kept above it for evictions. At zero the log " +
				"refuses every write AND every linearizable read, and the " +
				"remedy is a fleet-wide maintenance cycle, so this is the one " +
				"number worth alarming on long before it is small.",
		},
		{
			Name: StatelogTrimBlockedSeconds, Kind: KindGauge, Unit: UnitSeconds,
			Attributes: []string{"domain", "term"},
			Shows: "How long one retention term has held the trim, named. A " +
				"trim blocked for weeks is a log walking toward its ceiling " +
				"with a cause an operator can act on.",
		},
		{
			Name: StatelogAnchorsSwept, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "Arbitration anchors this node's applier removed under the " +
				"trim's `trimmed` records (ADR-0080), counted once the " +
				"transaction that removed them commits. Every node applies " +
				"every record, so every data node counts the same removals. " +
				"Flat is the normal reading of a log whose purged objects all " +
				"wrote again later — their anchors moved above the trim and " +
				"nothing is below it to remove; a sweep that is failing shows " +
				"as a `retention_anchor_sweep_failed` line from the trim's " +
				"holder, and as anchors below the log's first record that keep " +
				"growing while this stays flat.",
		},

		// ---- the backup -----------------------------------------------
		{
			Name: BackupAge, Kind: KindGauge, Unit: UnitSeconds,
			Attributes: nil,
			Shows: "How long ago the newest COMPLETE backup THIS NODE took " +
				"finished, read from the manifests in the directories it " +
				"announced rather than from the fleet's register. The " +
				"register records that a copy was announced; the disk " +
				"records that it still exists, and they differ in exactly " +
				"the cases worth catching — a copy deleted, a volume never " +
				"mounted, a schedule pointing at a path nobody ships from. A " +
				"node holding no manifest of its own reports NO SERIES " +
				"rather than a value: zero is the freshest backup imaginable " +
				"and every other number is an age nobody measured, so " +
				"neither can stand for an absence, and an `absent()` rule " +
				"over this gauge is what a collector alerts on. It is NOT " +
				"what the `backup_age` alarm reads: the alarm ages, per log " +
				"and fleet-wide, the newest verified point the trim counts " +
				"in the register (under `backup_floor: operator`, the " +
				"operator's acknowledgement of that log), so the two answer " +
				"different questions and can disagree — a copy deleted " +
				"after it was announced still counts for the alarm and the " +
				"trim, and leaves this gauge.",
		},
		{
			Name: BackupDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: nil,
			Shows: "How long a backup took, which is the window the trim " +
				"hold covers and the I/O the copy spends competing with the " +
				"applier's own commits. It is what turns the retention " +
				"guide's worked example into a number for THIS hardware.",
		},
		{
			Name: BackupHolds, Kind: KindGauge, Unit: UnitCount,
			Attributes: nil,
			Shows: "Trim holds in the register: a backup copying the log, or " +
				"a joining node fetching a snapshot and replaying its tail. A " +
				"holder renews its pin every fifteen seconds and removes it " +
				"when it is done, and the trim ignores one nobody has renewed " +
				"for a minute; nothing else removes it, so a count that does " +
				"not return to zero is a holder that crashed — a backup " +
				"mid-copy, or a join before its node published a position.",
		},

		// ---- the embedded broker ---------------------------------------
		{
			Name: BrokerPeerRefusals, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"listener", "presented"},
			Shows: "A connection to this node's embedded broker refused for its " +
				"credential, by the listener it reached (`route`, `leaf`, " +
				"`client`) and what it presented (`none`; `unknown`: a key " +
				"this node does not hold; `invalid`: a held key whose proof " +
				"failed). `unknown` rising during a keyring rotation is a " +
				"node whose keyring differs from this one's; `none` is a " +
				"process that is not a node of this fleet, or one on an " +
				"older build. Every refusal is counted; the " +
				"`broker_peer_refused` line says which key, from where, once " +
				"a minute a class. No alarm reads it: an operator alarms on " +
				"any rise.",
		},

		// ---- the fleet's history ---------------------------------------
		{
			Name: HistoryAnswers, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"question", "coverage"},
			Shows: "What each fleet history read covered: `complete` when " +
				"every live data node answered inside the fleet read budget, " +
				"`partial` when one did not. Turn-level detail lives only " +
				"on the data node that published it or keeps it in custody, " +
				"so a partial answer is missing that node's rows — and the " +
				"history_partial alarm is a fraction of this counter.",
		},

		// ---- the store ------------------------------------------------
		{
			Name: StorePoolWait, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"file"},
			Shows: "How long a reader waited for a connection. It is what " +
				"says the reader pool is too small on this node, which " +
				"nothing could say before.",
		},
		{
			Name: StoreWalBytes, Kind: KindGauge, Unit: UnitBytes,
			Attributes: []string{"file"},
			Shows: "A write-ahead log a checkpoint cannot pass grows, and " +
				"this is the only way to see it before the volume fills.",
		},
		{
			Name: StoreBytes, Kind: KindGauge, Unit: UnitBytes,
			Attributes: []string{"file"},
			Shows: "The store's size on disk, which the snapshot's free-space " +
				"precondition and the provisioning rule are both derived from.",
		},
	}
}

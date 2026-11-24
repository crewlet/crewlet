package metrics

// serviceInstruments is the instruments of the services beside the state log: search, the change feed, the
// notification service's inbound group, the work pickup, the object store's S3 requests and the code sandbox's
// session spools.
func serviceInstruments() []Instrument {
	return []Instrument{
		// ---- search ---------------------------------------------------
		{
			Name: TrackerSearchScanDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"path", "rung"},
			Shows: "One search's scan, recorded by the node that coordinated " +
				"it, split by what it ran for (`path`: `prefetch` for a turn's " +
				"knowledge prefetch, which the turn waits on before its first " +
				"token, `interactive` for somebody's deliberate search) and by " +
				"the ranking it actually served (`rung`: hybrid, keyword, " +
				"semantic, or none). Each path has its own budget — half a " +
				"second for a prefetch (`prefetch_slow`), a second for an " +
				"interactive search (`search_slow`) — and read as one series " +
				"the quicker one hides the other; a keyword scan is a fraction " +
				"of a fused one's cost, so reading rungs as one series hides a " +
				"slow meaning half.",
		},
		{
			Name: TrackerSearchConcurrency, Kind: KindGauge, Unit: UnitCount,
			Attributes: nil,
			Shows: "Searches in flight, which is the row of the " +
				"supported-corpus table this node is actually on: inside the " +
				"one-second budget, about 345 000 sources with one running " +
				"and about 136 000 with eight through the full scan, and " +
				"about 545 000 and 183 000 through an index probing half " +
				"its lists.",
		},
		{
			Name: TrackerSearchAnswers, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"coverage", "semantic"},
			Shows: "What each answer actually covered: whether every bucket " +
				"of the corpus was scanned, and whether the semantic half " +
				"ran (`full`), was asked for and lost (`skipped`), or was " +
				"never asked for (`off` — a keyword search, or a company " +
				"with no embeddings). Both alarms below it are a FRACTION " +
				"of this counter, and a short answer is indistinguishable " +
				"from a short corpus without it.",
		},
		{
			Name: TrackerVectorCoverage, Kind: KindGauge, Unit: UnitCount,
			Attributes: nil,
			Shows: "The fraction of sources carrying a current vector. It is " +
				"how a stalled embedding backlog is reported, since it never " +
				"drops a seat. Absent where the company configures no " +
				"embeddings and while the corpus cannot be measured — an " +
				"unreadable corpus is neither covered nor uncovered.",
		},

		// ---- the change feed ------------------------------------------
		{
			Name: TrackerFeedUnreadable, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"source"},
			Shows: "Change records this build could not translate into a " +
				"wake, once per delivery. Such a record is handed back for a node " +
				"that can read it, with a backoff, and dead-lettered once it is an " +
				"hour old, so its wake waits until then and has no other symptom; " +
				"`feed_unreadable` fires on it. Upgrade the node past it.",
		},
		{
			Name: TrackerFeedDeferred, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"source", "reason"},
			Shows: "Change records handed back for a peer that can read them, " +
				"once per delivery: a version (`newer_record`) or an envelope " +
				"(`newer_envelope`) a newer build writes, or a frame sealed under " +
				"a key this node does not hold (`unknown_key`). A rolling upgrade's " +
				"or a key rotation's ordinary traffic for its minutes; one that " +
				"keeps rising after the roll is a node left on the older build or " +
				"the old keyring, whose records are dead-lettered after an hour.",
		},
		{
			Name: TrackerFeedDeadLettered, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"source", "reason"},
			Shows: "Change records a feed stopped handing back after an hour " +
				"and kept as a dead letter in CREWLET_DLQ, frame and all, by why " +
				"(the deferral reasons, or `untranslatable`). Each is a wake " +
				"nobody has had until a node whose feed starts able to read it " +
				"replays it; `feed_dead_lettered` is raised from the first. " +
				"Counted once across the fleet, by whichever data node takes the " +
				"record's dead-letter claim, whichever of its keeps stored the " +
				"entry; a node whose claim the coordination store could not " +
				"decide counts the record anyway.",
		},
		{
			Name: TrackerFeedDeadLetterExpired, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"source"},
			Shows: "Dead letters the dead-letter stream's retention " +
				"(stream.event_retention_hours) let go before any node replayed " +
				"them: wakes lost for good. Counted once across the fleet, by the " +
				"first data node whose alarm reading saw the entry within a beat " +
				"of its expiry and then gone, through a claim a replay or a discard " +
				"that drops an entry takes first, so a replayed or discarded entry " +
				"is never counted. An expiry across a restart of every data node is " +
				"not counted: a node starts with no sight of what it had seen.",
		},
		{
			Name: TrackerFeedDeadLetterDiscarded, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"source"},
			Shows: "Dead letters an operator discarded (`crewlet feeds discard`, " +
				"`POST /feeds/dead-letters/{id}/discard`): wakes given up for good " +
				"by somebody's decision rather than lost to the retention. Counted " +
				"once across the fleet, by the node that served the discard, " +
				"through the claim a replay and the expiry count take, so an entry " +
				"a replay, its expiry or another discard took first is not counted; " +
				"each is a `changefeed_dead_letter_discarded` line naming who " +
				"pressed it.",
		},

		// ---- the notification service's inbound group ------------------
		{
			Name: NotifyInboundDeadLetterExpired, Kind: KindCounter, Unit: UnitCount,
			Shows: "Deliveries the notification service's inbound group never " +
				"handled — a newer record's wake an older node passed until its " +
				"budget was spent, or one a node that shed or stopped handed back " +
				"last — that the dead-letter stream's retention " +
				"(stream.event_retention_hours) let go before any node's replay " +
				"delivered them: wakes lost for good. Counted once across the fleet, " +
				"by the first data node whose alarm reading saw the entry within a " +
				"beat of its expiry and then gone, through a claim a replay that " +
				"drops an entry takes first, so a replayed entry is never counted. " +
				"`inbound_dead_lettered` is raised while any waits.",
		},

		// ---- the work pickup ------------------------------------------
		{
			Name: WorkPickups, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "What each work pickup decided for an idle agent seat this " +
				"node holds (ADR-0112): `woken` when it handed the seat its " +
				"next ready item (an urgent or due one also to a seat that " +
				"was running a turn or had mail waiting, ADR-0159 and " +
				"ADR-0162), " +
				"`nothing_ready` when the seat holds no " +
				"ready work, `all_offered` when it was handed every ready " +
				"item at its current version already, `cooldown` and " +
				"`claimed` when the fleet's claims held it back, and " +
				"`unknown` when a read, a claim or the publish failed; the " +
				"`work_pickup_skipped` line names the error. Ready work that " +
				"sits while this counts only `cooldown` or `all_offered` is " +
				"seats declining it.",
		},

		// ---- a busy seat's router ------------------------------------
		{
			Name: SeatWakesRouted, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome", "rule"},
			Shows: "What the router of a seat whose executor run holds it " +
				"decided for each wake reaching the seat, once per wake and job " +
				"(ADR-0167, ADR-0168, ADR-0169): `joined` when it handed the " +
				"wake to the run (a lead's steer as their note), `absorbed` when " +
				"its triage found it noise owing nothing, `refused` when the " +
				"run's record took no more, and `left_queued` when it waits for " +
				"a turn of its own; and below the seat's cap, `waited` for each " +
				"wake set aside until the run it relates to leaves (whatever " +
				"else was counted of it), and `started` for each related to no " +
				"run, which starts a turn beside them. `rule` names what decided: " +
				"the requester rule's `conversation`, `work_item` or `wrote` (a " +
				"record the run created or wrote); the triage's " +
				"`triage_input`, `triage_steer`, `triage_request`, " +
				"`triage_noise`, or `triage_failed` for a triage that decided " +
				"nothing; another writer's request put to the people who may " +
				"let it join, `foldin_asked` (`left_queued` while it waits), " +
				"then `foldin_approved` (`joined` as the approver's note), " +
				"`foldin_refused`, or `foldin_expired` when its job ended " +
				"unanswered; `run` for a wake waiting on a run other than by its " +
				"conversation; or `none`. `triage_failed` climbing is a seat whose " +
				"triage chain is failing, and `foldin_expired` beside few " +
				"answers is approvers not reached. The `seat_wake_left_queued`, " +
				"`seat_wake_triaged`, `seat_wake_foldin_*` and `seat_wake_aside` " +
				"lines name the reason.",
		},
		{
			Name: SeatRunCalls, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"tool", "outcome"},
			Shows: "What became of each call an executor's run made to the tools " +
				"for the runs in flight around it (ADR-0176), by `tool` (the " +
				"run tool called, or `wait_answered` for the engine's answer " +
				"to a wait): `listed`, `sent`, `waited`, " +
				"`resumed` when a wait's run left its seat and the wait was " +
				"answered, `failed` when a store or the publish failed, or " +
				"`refused_` and the reason (`self`, `unknown_run`, `requester`, " +
				"`not_working`, `harness`, `bound`, `cycle`, `caller_done`, " +
				"`invalid`, `unreachable` for another seat's run the caller's may not ask). " +
				"`refused_requester` climbing is runs of one seat working for " +
				"different people on the same work; `refused_bound` runs talking " +
				"past what a handoff needs. The `seat_run_*` lines name the runs.",
		},

		// ---- the object store's S3 requests ---------------------------
		{
			Name: ObjectsS3Attempts, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"operation", "outcome"},
			Shows: "Every attempt this node's S3 client sent its bucket, by S3 " +
				"`operation` (the API's own name: `HeadObject`, " +
				"`GetObject`, `PutObject`, `CreateMultipartUpload`, " +
				"`UploadPart`, `CompleteMultipartUpload`, " +
				"`AbortMultipartUpload`, `DeleteObject`, `DeleteObjects`, " +
				"`ListObjectsV2`, `ListMultipartUploads`, and S3 Express's " +
				"`CreateSession`) and what the bucket made of it (`outcome`): " +
				"`answered` — served, a success or a refusal such as a missing " +
				"key; `throttled` — asked to slow down (`503 SlowDown`, any " +
				"`429`, MinIO's `SlowDownRead` and `SlowDownWrite`), which cuts " +
				"the node's gate; `failed` — any other answer: a server error " +
				"that is no throttle, or a status no request expects, such as a " +
				"redirect to another region; `unreached` — no answer at all: a " +
				"connection refused or reset, a name that did not resolve, a TLS " +
				"failure, or an attempt never sent because its S3 Express " +
				"session could not be had; `stalled` — the bucket stopped " +
				"moving the attempt for a minute: took no more of its body, " +
				"acknowledged none of it at the node's socket and sent none of " +
				"its answer, so the request was given up; `ended` — the " +
				"attempt's own deadline or its caller ended it in flight. An " +
				"operation's RETRIES are " +
				"this counter's total for it, less the count of " +
				"`crewlet.objects.s3.request.attempts` for it, plus that " +
				"histogram's `unsent` count for it. `throttled` rising is the " +
				"fleet over the bucket's rate for the prefix (S3 serves 5 500 " +
				"reads and 3 500 writes a second a prefix); `failed` the " +
				"bucket's own fault; `unreached` the network or the endpoint.",
		},
		{
			Name: ObjectsS3GateLimit, Kind: KindGauge, Unit: UnitCount,
			Shows: "How many attempts this node's S3 client lets its bucket " +
				"hold at once while a throttle holds it back — seven tenths of " +
				"the most the node had out at once when the bucket last " +
				"throttled it, one more each round of answers — and zero while " +
				"nothing does: the gate lifts a minute after the bucket's last " +
				"throttle. Above zero for long is a fleet held over the " +
				"prefix's rate; every request of the node — git syncs, the " +
				"collector, backups, people's files — shares what it allows.",
		},
		{
			Name: ObjectsS3GateWaiting, Kind: KindGauge, Unit: UnitCount,
			Shows: "Attempts of this node's S3 client waiting in line for the " +
				"gate, first come first served: what a throttle is costing the " +
				"node now. High while `crewlet.objects.s3.gate.limit` stays low " +
				"is a node sending more than the prefix will take from it — its " +
				"requests wait rather than fail, until their own deadlines.",
		},
		{
			Name: ObjectsS3RequestAttempts, Kind: KindHistogram, Unit: UnitCount,
			Attributes: []string{"operation", "outcome"},
			Shows: "How many attempts each request of this node's S3 client " +
				"took, observed once as it ends, by `operation` and how it " +
				"ended (`outcome`): `answered` — the bucket's final answer, a " +
				"success or a refusal; `given_up` — the bucket answered nothing " +
				"but failures (server errors that are no throttle, or no answer " +
				"at all) from the first to a bound drawn for it between forty " +
				"seconds and a minute after it, or stopped moving one of its " +
				"attempts for a minute (`stalled` on the attempts counter), so " +
				"the request was not asked again and its caller was told the " +
				"bucket's last answer; " +
				"`ended` — its deadline or its caller ended it after at least " +
				"one attempt, one still throttled among them; `unsent` — it " +
				"ended before its first attempt, observed at zero: its deadline " +
				"or its caller ended it while it waited at the gate (the gate " +
				"starving the node's requests), or it was refused before " +
				"sending; `failed` — a failure asking again cannot change, such " +
				"as a certificate the node does not trust, or an S3 Express " +
				"session that could not be had for it. Nothing counts a " +
				"request's attempts but its deadline and that bound, so the " +
				"tail is how long the bucket made requests wait: a p95 above " +
				"one is a bucket throttling or failing, and `given_up` above " +
				"zero is a bucket that failed every attempt for most of a " +
				"minute, or held one for a whole minute without moving it.",
		},

		// ---- the code sandbox -----------------------------------------
		{
			Name: ExecutorSpoolWait, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"outcome"},
			Shows: "How long each final take of an executor run's session " +
				"waited for room to spool it on this engine process, before " +
				"it held anything else, by `outcome`: `taken`, it got its " +
				"room (at once unless other final takes held it: saves never " +
				"wait, are not counted here, and leave a final take its " +
				"room); `refused`, the room stayed full for the whole " +
				"minute, so the take kept nothing, stamped its capture " +
				"refused as `no_spool_room`, and the turn's next launch " +
				"continues its newest kept capture and says so " +
				"(`crewlet.executor.session.not_continued`), logged as " +
				"`executor_session_not_kept` with `waited_seconds`, `bytes`, " +
				"`room_bytes` and `others_held_bytes` (a standby's refusal " +
				"counts too, logged at info, and costs nothing unless the " +
				"read it stands by for fails); `ended`, the take's " +
				"own time ran out first. `refused` above zero beside short " +
				"`taken` waits is a room (481.17 MiB, five takes' spools at the " +
				"sealed bound) too small for the takes that end at once on " +
				"the node; beside a `taken` tail of tens of seconds, a store " +
				"uploading too slowly for the minute. Both are engine " +
				"constants, not settings: report it with both readings.",
		},
		{
			Name: ExecutorSpoolHeldBytes, Kind: KindGauge, Unit: UnitBytes,
			Attributes: nil,
			Shows: "Bytes this engine process's executor-session spools hold " +
				"of the room they share (481.17 MiB): each save's and final " +
				"take's, from before its pack until its upload ends, its " +
				"sealed spool's at the sealed bound while it packs and its " +
				"archive's counted size after, beside its scrub's spool's " +
				"until the scrub ends. Near the room for long is a node whose takes outrun " +
				"its uploads, where `crewlet.executor.spool.wait` starts " +
				"counting `refused`; above zero with no run ending or saving " +
				"on the node is a spool never given back.",
		},
		{
			Name: ExecutorSessionNotContinued, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"reason", "instead"},
			Shows: "Collected executor launches that did not continue the " +
				"newest session their turn or conversation left, counted " +
				"once per launch where its record is published, by " +
				"`reason` (the loss: `too_large`, `keep_failed`, " +
				"`no_spool_room`, `store_refused`, `log_refused`, " +
				"`capture_late`, `release_floor`, `box_refused`, " +
				"`restore_failed`, `box_lost`, and the deliberate " +
				"`context_exhausted`, `request_refused`, `harness_changed`, " +
				"`budget_floor`, `entry_changed`) and `instead` (`fresh`, a session of its " +
				"own; `older`, an older one). Every launch that had a session " +
				"to continue is also an event of the `session_carry` " +
				"objective, bad for every reason but the deliberate five. A " +
				"rising fault is read beside the launch's " +
				"`executor_session_not_continued` and the capture's " +
				"`executor_session_not_kept` lines, which name it.",
		},

		// ---- the node's answer budget ------------------------------------
		{
			Name: QueueAnswerWait, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"answer", "outcome"},
			Shows: "How long each payload answer this engine process built " +
				"for a peer waited for its weight of the node's answer budget " +
				"(256 MiB, every payload answer on the node sharing it, in " +
				"arrival order), and every collection of a run's box, by " +
				"`answer` (`box_read`, `history`, `memory`, `replay`, " +
				"`live_relay`, `bridge`, `tail`, `codesearch`, " +
				"`search_slice`, `mcp_check`, `estate`) and `outcome`: `admitted`, at " +
				"once or after waiting; `ended`, its asker's wait or its own " +
				"ran out first, so it answered that the budget was full (or " +
				"nothing). A p95 climbing beside `held_bytes` near the budget " +
				"is a node answering more payload at once than its memory " +
				"keeps for answers; `ended` above zero is answers refused for it. " +
				"The budget is an engine constant: report it with both readings.",
		},
		{
			Name: QueueAnswerHeldBytes, Kind: KindGauge, Unit: UnitBytes,
			Attributes: nil,
			Shows: "Bytes of the node's answer budget (256 MiB) its payload " +
				"answers and box collections hold now, each at its weight " +
				"from before its read until its reply is written. Near the " +
				"budget for long is a node whose answers wait " +
				"(`crewlet.queue.answers.wait`); above zero with nothing " +
				"answered or collected is a hold never given back.",
		},
	}
}

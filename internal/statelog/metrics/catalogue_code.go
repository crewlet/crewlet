package metrics

// codeInstruments is the code subsystem's instruments.
func codeInstruments() []Instrument {
	return []Instrument{
		// ---- code -----------------------------------------------------
		{
			Name: CodeCommitBatchSize, Kind: KindHistogram, Unit: UnitCount,
			Shows: "Pushes per code record: group commit working, or not. The " +
				"committer takes everything already queued behind an in-flight " +
				"record — without lingering for more — so an idle repository's " +
				"records stay at one push and a busy one's grow, up to 64. A busy " +
				"repository whose batches stay at one is one whose pushes arrive " +
				"more slowly than a record commits.",
		},
		{
			Name: CodeChangeProposals, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"op", "outcome"},
			Shows: "Writes of a change's lifecycle this node's committer was " +
				"asked to decide, by `op` (`change_open`, `change_edit`, " +
				"`change_draft`, `change_retarget`, `change_close`, " +
				"`change_reopen`, and `change_landed`, a landing its " +
				"repository's home Gitlet node found) and what became of each, by " +
				"`outcome`: " +
				"`applied`, its record landed and its change's row says so; " +
				"`unchanged`, it asked for what the change already said, and " +
				"nothing was appended; `pending` and `unknown`, its record was " +
				"appended and is not yet known here to have applied; a " +
				"refusal's own code — `not_permitted`, `stale`, `change_exists`, " +
				"`no_source`, `no_target`, `same_ref`, `no_difference`, " +
				"`change_closed`, `change_merged`, `change_not_found`, " +
				"`repo_archived`, `repo_importing`, `repo_mirrored`, " +
				"`repo_deleted`, `busy`, `not_applied`, " +
				"`unavailable` — which decided nothing; `handed_on`, one this " +
				"node decided nothing of for a reason about itself (stopping, " +
				"no chart to judge a seat by, behind the company's " +
				"configuration, a build that does not know a value whom the " +
				"write is made for names), which the estate asks the " +
				"repository's runner-up; and `failed`, any other error. " +
				"`stale` climbing is " +
				"askers writing against a change pushes keep moving; " +
				"`handed_on` on one node and `applied` on another, a node that " +
				"cannot judge its seats.",
		},
		{
			Name: CodeQueueProposals, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"op", "outcome"},
			Shows: "Writes of a target's merge queue this node's committer was " +
				"asked to decide, by `op` (`enqueue`, a change put in its " +
				"target's queue through its merge gate, and `dequeue`, an " +
				"entry taken out at a principal's ask; and the queue's own " +
				"records its home asks: `candidate`, a train built onto the " +
				"target's tip, `eject`, entries put out, `land`, entries " +
				"landed with every one's gate proved again, `green` and " +
				"`broken`, a landing its tip checks passed or failed) and what " +
				"became of each, by `outcome`: `applied`, its record landed and " +
				"the rows it wrote say so; `pending` and `unknown`, its record " +
				"was appended and is not yet known here to have applied; a " +
				"refusal's own code — `gate_unmet`, `already_queued`, " +
				"`queue_full`, `no_queue`, `head_moved`, " +
				"`stale`, `target_moved`, `change_not_open`, `change_draft`, " +
				"`change_not_found`, `not_queued`, `not_permitted`, " +
				"`not_green`, `not_red`, `no_landing`, `record_full`, " +
				"`upload_expired`, `repo_size`, `compaction_behind`, " +
				"`repo_archived`, `repo_importing`, `repo_mirrored`, " +
				"`expunging`, `repo_deleted`, `busy`, `not_applied`, " +
				"`unavailable` — which decided nothing; `handed_on`, one this " +
				"node decided nothing of for a reason about itself, which the " +
				"estate asks the repository's runner-up; and `failed`, any " +
				"other error. `gate_unmet` climbing is askers queueing changes " +
				"before their gate is met, or, on `land`, gates that stop " +
				"holding between an enqueue and its landing; `queue_full`, a " +
				"queue landing slower than it is fed; `target_moved` on " +
				"`candidate` and `land`, a branch moving under its queue.",
		},
		{
			Name: CodeQueueRounds, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "Trains of a target's merge queue that ended on the Gitlet node " +
				"that is its repository's home, by how, `outcome`: `landed`, " +
				"its longest green prefix landed in one land record, applied " +
				"or pending; " +
				"`bisected`, its first candidate red and bisected down to the " +
				"culprit, which was ejected `check_failed`; `ejected`, entries " +
				"its landing would not land (a gate no longer met, a head " +
				"moved) put out, which ends it; and `abandoned`, a train this " +
				"node built that ended any other way — a person's merge or " +
				"push moving its target, a dequeue, a newer train. `bisected` " +
				"climbing against `landed` is changes queued red, each batch " +
				"paying a bisect; `abandoned`, a target pushed to under its " +
				"queue. A train counts once, where it ended; a home new to a " +
				"queue counts the ones it saw end.",
		},
		{
			Name: CodeQueueLanded, Kind: KindCounter, Unit: UnitCount,
			Shows: "Changes a merge queue landed: the entries of each land " +
				"record the repository's home appended, applied or pending " +
				"(durable, which every node applies), every one merged onto " +
				"its target as its enqueuer. Against " +
				"`crewlet.code.queue.proposals{op=\"enqueue\"}` it is the " +
				"queue's throughput: enqueues climbing past landings is a " +
				"queue fed faster than it lands, which " +
				"`code_merge_rate_over_capacity` raises once it outruns the " +
				"queue's own capacity.",
		},
		{
			Name: CodeQueueEjected, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"reason"},
			Shows: "Entries a merge queue's home put out of their queue, one " +
				"for each ejection of an eject record it appended, by " +
				"`reason`: `check_failed`, a bisect's culprit; " +
				"`checks_undefined`, every entry of a train whose base's checks " +
				"file defines a required check in no form that runs; `conflict` and " +
				"`not_fast_forward`, a merge its chain could not build; " +
				"`gate_unmet` and `head_moved`, an entry its landing would not " +
				"land; `repo_mirrored`, the entries of a pull mirror's queue. " +
				"`check_failed` climbing is changes enqueued before their " +
				"checks pass on the target; `conflict`, changes queued " +
				"together that touch the same lines.",
		},
		{
			Name: CodeQueueRoundDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Shows: "One round of a merge queue that landed, on its home: from " +
				"the candidate record that built its train applying to the " +
				"land record that landed its green prefix appended, applied " +
				"or pending — the same " +
				"instants `code_merge_rate_over_capacity` prices a queue's " +
				"capacity at. Only a train this node built is timed, so a " +
				"home new to a queue records the rounds it builds. It is the " +
				"queue's checks' wall clock plus its records': rising, a " +
				"queue whose checks slowed or whose runners are busy, which " +
				"lowers the rate it can land at.",
		},
		{
			Name: CodeMergeOutcomes, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "Merges of changes this node's committer was asked to decide " +
				"— each one record that merges the change and moves its target, " +
				"its merge gate proved from one snapshot or a person's override " +
				"recorded — by what became of each, `outcome`: `applied`, its " +
				"record landed and the change's row says so; `pending` and " +
				"`unknown`, its record was appended and is not yet known here to " +
				"have applied; a refusal's own code — `gate_unmet`, " +
				"`target_moved`, `head_moved`, `stale`, `queue_target`, " +
				"`not_person`, `source_refused`, `upload_expired`, `repo_size`, " +
				"`compaction_behind`, `change_not_open`, `change_draft`, " +
				"`change_not_found`, `repo_archived`, `repo_importing`, " +
				"`repo_mirrored`, `expunging`, `repo_deleted`, `busy`, " +
				"`not_applied`, `unavailable` — which decided nothing; " +
				"`handed_on`, one this node decided nothing of for a reason " +
				"about itself, which the estate asks the repository's runner-up; " +
				"and `failed`, any other error. `target_moved` climbing is a " +
				"branch moving faster than its merges are built onto it; " +
				"`gate_unmet`, askers merging changes before their gate is met.",
		},
		{
			Name: CodeMergeRecomputes, Kind: KindHistogram, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "How many times each merge of a change this Gitlet node built " +
				"was built again onto a target that moved under it, observed " +
				"once a merge as it ends — from 0, a merge its first build " +
				"landed, to 4 (`gitlet.MergeRecomputes`), the most it is ever " +
				"rebuilt — by how it ended, `outcome`: `merged`, its record " +
				"applied or pending, or found already landed; `queued`, its " +
				"target's queue took it, built nothing; a refusal's own code " +
				"— `target_moving` where its rebuilds, or the one budget of git " +
				"a merge is given, ran out before its target held still, " +
				"`gate_unmet`, `conflict` and the rest; `unknown`, its record " +
				"was appended and is not yet known here to have applied; " +
				"`unready`, this node could not answer and another Gitlet node " +
				"was asked, which records its own; `refused`, an argument the " +
				"build found invalid or an object it named missing; and " +
				"`failed`, the node's own failure. Read its sum over its count, " +
				"the mean rebuilds a merge took: above 0 is a branch moving " +
				"while its merges are built (the count boundaries put 0 and 1 " +
				"in one bucket, and 3 and 4 in another, so the buckets alone " +
				"cannot say it); `target_moving` counted, merges it outran — " +
				"land them through its queue, or at a quieter moment.",
		},
		{
			Name: CodeChecksQueued, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"purpose"},
			Shows: "Attempts of checks this node's checks writer queued — each by a " +
				"record on its repository's checks subject that applied here (a " +
				"head's dispatch record carries all of its own), counted once, by " +
				"the node that published it — by `purpose`: " +
				"`head`, a change's head; `candidate`, a merge queue's candidate; " +
				"`tip`, a protected branch's tip; `bisect`, a landing between a " +
				"green tip and a red one. A head recorded skipped at its dispatch " +
				"is counted under crewlet.code.checks.finished alone.",
		},
		{
			Name: CodeChecksFinished, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"state", "placement", "replaced"},
			Shows: "Endings of attempts of checks, as their rows keep the ending " +
				"— each counted once, by the node whose checks writer published " +
				"it — by `state` (`passed`, `failed`, `errored`, `cancelled`, " +
				"`skipped`), `placement` (`container`, a runner's container on " +
				"a Gitlet node; `external`, a person's own CI; or `none` for one " +
				"no runner claimed) and `replaced`: `none` for an " +
				"attempt's first ending, and otherwise the ending this one " +
				"REPLACED on its row — a measurement after an infrastructure " +
				"ending, a slow runner's exit after its attempt was errored " +
				"`runner_lost` — so the attempts that ended are the count under " +
				"`replaced=none` and the rest are corrections of them, never " +
				"second endings. An ending the row did not keep — a later " +
				"infrastructure ending beside a measurement — is not counted. " +
				"`errored` climbing under `container` is the Gitlet nodes' boxes " +
				"failing the checks they run; `replaced=errored` climbing is " +
				"runners declared lost that were only slow.",
		},
		{
			Name: CodeChecksClaims, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "Claims of queued attempts this node's checks writer decided, " +
				"by `outcome`: `won`, the claim landed and its runner holds the " +
				"attempt; `claimed`, refused before it appended — another runner " +
				"holds the attempt, or it ended; `lost`, the claim landed and " +
				"the row keeps another runner's, which a second node publishing " +
				"for the repository landed first; `unchanged`, the holder's own " +
				"claim sent again; `no_such_check`, `repo_deleted`, `busy`, " +
				"`not_applied` and `unavailable`, which decided nothing; " +
				"`pending` and `unknown`, appended and not yet known here to " +
				"have applied; `handed_on`, one this node decided nothing of for " +
				"a reason about itself, which the estate asks the repository's " +
				"runner-up; and `failed`, any other error. `claimed` is Gitlet " +
				"nodes racing for one attempt, each losing one box it never " +
				"made; `lost` above zero is two nodes publishing one " +
				"repository's checks at once.",
		},
		{
			Name: CodeChecksRunning, Kind: KindGauge, Unit: UnitCount,
			Shows: "Checks this Gitlet node's runner is running now — each an " +
				"attempt it claimed, from its claim to its ending, in a " +
				"container on this node — at most `gitlet.max_checks`. Pinned " +
				"there while code_checks_backlog stands is a fleet whose Gitlet " +
				"nodes run too few containers for its checks.",
		},
		{
			Name: CodeChecksWithheld, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"piece", "reason"},
			Shows: "Pieces of a failed check's output this Gitlet node's runner did " +
				"not keep or show, by `piece` (`log`, the log kept in the object " +
				"store; `tail`, its summary's end) and `reason`, the reasons " +
				"`crewlet.executor.collect.withheld` gives (`changed`, the output " +
				"was cut, grown or written over while it was read; `refused`, the " +
				"redaction stream refused its text; `unsettled`, no line of its " +
				"end could be redacted as the whole output is; `too_large`, its " +
				"last line, redacted, is past what a log keeps). The check's " +
				"summary says each. `changed` above zero is something in a box " +
				"still writing after its check's command ended.",
		},
		{
			Name: CodeChecksDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Shows: "How long the checks this Gitlet node's runner ran took, from " +
				"the claim to the ending it recorded — the container's making " +
				"and setup, the command and its results read. A check's time is " +
				"its own definition's to bound (`timeout_seconds`, held to " +
				"`code.checks.max_timeout_seconds`); a p95 climbing with no " +
				"change to the checks is this node's containers slower to make " +
				"or to set up.",
		},
		{
			Name: CodeConversationWrites, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"op", "outcome"},
			Shows: "Writes of a change's conversation this node's change writer " +
				"was asked to decide — each a record on the change's own " +
				"subject, never in the repository's committer queue — by `op` " +
				"(`reviewers`, `comment`, `thread_resolve`, `review`, " +
				"`finding_resolve`, and three its repository's home Gitlet node " +
				"asks: `revise`, a revision's facts it read, " +
				"`mergeability`, whether the change's head revision merges " +
				"into its target's tip, and `asks`, the change's asks for " +
				"review settled with its lifecycle — ended once it closed or " +
				"merged, asked again once it is reopened) and what became of " +
				"each, by " +
				"`outcome`: " +
				"`applied`, its record landed and the change's conversation " +
				"holds it; `unchanged`, it asked for what the conversation " +
				"already said, and nothing was appended; `pending` and " +
				"`unknown`, its record was appended and is not yet known here " +
				"to have applied; a refusal's own code — `not_permitted`, " +
				"`own_change`, `no_reviewer`, `too_many_reviewers`, `unknown_revision`, " +
				"`change_closed`, `change_merged`, `thread_gone`, " +
				"`not_author`, `not_raiser`, `finding_gone`, " +
				"`too_many_findings`, `change_not_found`, `repo_archived`, " +
				"`repo_deleted`, `busy`, `not_applied`, `unavailable`, and the " +
				"home's `superseded` — facts or a merge of a revision, a head or " +
				"a target the change has moved from; facts no later than the " +
				"facts it holds, or matched against path owners or human paths changed since; " +
				"a merge into a tip before the one its mergeability is of, or " +
				"into the same tip once decided; a settling of asks for a " +
				"change that moved since its home read it, or holds no such " +
				"ask — which decided nothing; `handed_on`, one this node " +
				"decided nothing of " +
				"for a reason about itself (stopping, no chart to judge a seat " +
				"or a reviewer by, behind the company's configuration, a build " +
				"that does not know a value whom the write is made for names), " +
				"which the estate asks the repository's runner-up; and " +
				"`failed`, any " +
				"other error. `busy` climbing is a change written faster than " +
				"one node writes it — its writes wait their turn one at a time " +
				"— or one the change kept moving under for every round.",
		},
		{
			Name: CodeCommitBusy, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"kind", "reason"},
			Shows: "Proposals refused `busy`, undecided, by `kind` and `reason`. " +
				"`kind`: `push`, whose client runs `git push` again; " +
				"`compaction`, which the Gitlet node that built it plans again at " +
				"its next pass; `expunge`, a record of an expunge of a " +
				"repository's unreachable objects — a person's purge request, " +
				"which they ask for again, or the begin or the commit its " +
				"maintainer built, which it plans again at its next pass; " +
				"`change`, a write of a change's lifecycle, which its asker " +
				"sends again; `merge_queue`, an enqueue or a dequeue of a " +
				"target's merge queue, which its asker sends again; `merge`, " +
				"a change's merge, which its builder asks again; `import`, a " +
				"chunk or the end of a repository's import from another host, " +
				"which its home reads the repository and proposes again at its " +
				"next round; and `mirror_sync`, a pull mirror's sync, which its " +
				"home fetches and builds again at its next round. `reason` is " +
				"one of five causes, each pointing at " +
				"a different remedy. Three are a BACKLOG, which raises " +
				"`code_commit_busy` on the committer's node: `queue`, a " +
				"repository taking pushes faster than one node's committer " +
				"commits them (its queue of 1 024 proposals, or sixteen " +
				"records' bytes, is full); `node`, the node holding " +
				"sixty-four records' bytes of proposals across its " +
				"repositories; and `waited`, the queue's TIME bound, the one a " +
				"committer slower than its pushes meets before its count: a " +
				"proposal that queued with more of its asker's attempt left " +
				"than the 30-second commit budget and a second for the " +
				"answer, and lost it waiting behind earlier ones — asked with " +
				"no deadline, one that waited that budget whole. `late` is a " +
				"proposal that ARRIVED with no more than that left — the " +
				"attempt spent before it reached the committer, waiting for " +
				"the node to take it (its proposal slots full), for the node " +
				"to reach the asker's own writes, or under a caller deadline " +
				"shorter than 31 seconds. `contended` is a record that " +
				"lost every compare-and-set round it may take: the repository " +
				"kept changing, a second committer deciding it during a " +
				"handoff. Neither of the last two says this committer turns " +
				"proposals away, so neither raises an alarm. A `compaction` " +
				"busy is a repository whose pushes keep its committer's queue " +
				"full, and whose pack count grows meanwhile; an `expunge` busy, " +
				"one whose unreachable objects stay in the store meanwhile. No " +
				"repository is an attribute — which one is hot is a question " +
				"for its rows and the node's `code_commit_busy` lines, a " +
				"repository a minute, not a time series a repository.",
		},
		{
			Name: CodeHomePasses, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"worker", "outcome"},
			Shows: "Turns this node took as the HOME Gitlet node of a repository — " +
				"one worker's work in one repository, scheduled by a nudge from " +
				"the code log or by the minute's backstop pass — by `worker` " +
				"(`changes`: its live changes' facts and mergeability recorded — " +
				"a moved target's merges again, at most sixty-four a repository " +
				"a minute — or a change whose head its target holds landed; " +
				"`check_runners`: its running checks whose runner left its git " +
				"order, or ran past its check's timeout and a setup's grace, " +
				"ended `errored runner_lost`; `mirrors`: its import or its pull " +
				"mirror's sync STARTED, on a goroutine of its own, since a fetch " +
				"from another host outlives a turn — `crewlet.code.import.chunks` " +
				"and `crewlet.code.mirror.runs` count what each came to; " +
				"`atlas_code`: its deploy and dependency files read for the atlas " +
				"where its default branch moved, at most fifty files a turn, or " +
				"what they declared withdrawn where it is no longer scanned) and " +
				"`outcome`: `worked`, its due " +
				"work done; `failed`, a read or a record that decided nothing for " +
				"a reason of the node's or the log's (`code_home_turn_failed` " +
				"says which), the rest worked and the repository listed again by " +
				"the next pass; and `cut`, more work than one turn's minute, the " +
				"rest listed again by the next pass — work the minute's end " +
				"stopped, never a turn that finished its work a little past it, " +
				"which is `worked`. A refusal that the rows moved " +
				"since — a revision's facts superseded, a landing stale — is the " +
				"rows answering, and the turn `worked`. `cut` climbing on one node " +
				"is a home with more changes than its two turns keep up with.",
		},
		{
			Name: CodeHomeNudges, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "The code log's word to this node's home loop, one a record " +
				"on a repository's subject, by `outcome`: `scheduled`, this node " +
				"is the repository's home and its work was queued at the " +
				"record's position; `elsewhere`, another Gitlet node is its home; " +
				"`unknown`, who serves git could not be read, and the backstop " +
				"lists the repository within a minute; `ignored`, a subject that " +
				"names no repository; and `gap`, the watch may have missed records " +
				"— its consumer lost, its connection back — and a pass was run at " +
				"once at the position it reaches. Every Gitlet node watches every " +
				"repository, so `elsewhere` is most of every node's count but its " +
				"homes'; `gap` climbing is a watch that keeps losing its consumer.",
		},
		{
			Name: CodeHomeFactsDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"outcome"},
			Shows: "One revision's facts as this node's home loop asked them of " +
				"the repository's Gitlet nodes — its merge base, the files it touches " +
				"and its patch-id against its target's tip — by `outcome`: " +
				"`answered` or `failed`. Read by the home itself unless its copy " +
				"cannot answer, the runner-up then: a 1 MiB patch takes some 20 " +
				"ms and one at the 256 MiB bound some 2.3 s on two cores. " +
				"Durations near the read's ten seconds of git are changes past " +
				"what a home identifies — their patch-ids omitted — or a home " +
				"short of the CPU its reads run on.",
		},
		{
			Name: CodeHomeMergeDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"work", "outcome"},
			Shows: "One merge of a change's head into its target's tip on this " +
				"Gitlet node — `git merge-tree --write-tree` in a scratch of its " +
				"own, never in a cache — by `work`: `mergeability`, the home's " +
				"own, its ancestry asked first and its merge given thirty " +
				"seconds of the maintenance lane, timed from the copy's sync to " +
				"the ancestry's or the merge's answer — never the record of it " +
				"on the change or the landing, which are the change writer's " +
				"and the committer's; or `preview`, a comparison's merge " +
				"preview a person or a seat asked, given a read's ten seconds " +
				"of the read lane under a unit of the node's seat git-work " +
				"share, its merge-tree alone timed: a preview refused before " +
				"it (not held, not permitted, a commit absent), or answered " +
				"from the node's memo of the same two commits, is no merge, and " +
				"is not observed. `outcome`: `clean`, `conflict`, `unknown` — " +
				"the merge ran out of its time and decided nothing, which a " +
				"change's home records and asks again ten minutes on — " +
				"`landed`, the home's ancestry finding the head inside its " +
				"target, the change landed with no merge run, and `failed`, a " +
				"merge that could not run (a copy that would not sync, a busy " +
				"lane, git failing) or a preview's merge its asker's attempt " +
				"ended first. `unknown` climbing is changes past what a merge " +
				"decides in its time; `failed` climbing on the `mergeability` " +
				"work, a home whose maintenance lane is full.",
		},
		{
			Name: CodeCompactRuns, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "Compactions this node made as the MAINTAINER of their " +
				"repositories — the first Gitlet node in each repository's order " +
				"whose background budget holds its publication, the home on a " +
				"fleet of alike nodes — each a plan over a repository's live " +
				"packs that no published layer names, and what became of it, by " +
				"`outcome`. `applied`: its record landed and " +
				"its outputs replaced its inputs. `pending`, `unknown`: its record " +
				"was appended and is not yet known to have applied — the next poll " +
				"reads the rows. `not_due`: the plan found nothing worth merging, " +
				"and the repository is not planned again until its row moves. " +
				"`stale`: another compaction, a publication or a purge moved an " +
				"input first. `busy`: no maintenance slot or no memory room was " +
				"free, a publication of the repository was running on the node, " +
				"or the repository's committer was busy. `refused`: by the committer or " +
				"before it (`crewlet.code.compact.refusals` says why). `failed`: " +
				"the build, an upload or the answer failed " +
				"(`code_compaction_failed` names which). Every outcome but " +
				"`applied` is tried again at a later poll. `applied` keeping pace " +
				"with the pushes is a node keeping its repositories' pack counts " +
				"bounded; `failed` rising is its disk, its git or its object store.",
		},
		{
			Name: CodeCompactDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"outcome"},
			Shows: "One compaction from its plan to its answer, by `outcome` (as " +
				"`crewlet.code.compact.runs` counts them): the sync, the plan, the " +
				"build, every output's upload and the committer's decide. A merge " +
				"of a few small packs takes under a second; one of several " +
				"gigabytes, minutes of its build and of the store's pace. " +
				"`applied` durations rising while the pushes hold steady are a " +
				"maintaining node short of the CPU its builds run on, or of the " +
				"bandwidth to its object store.",
		},
		{
			Name: CodeCompactRefusals, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"reason"},
			Shows: "Compactions refused before or at their decide, by `reason`. " +
				"This node's own: `memory`, the build's charge past what the node " +
				"can give its background work (`code_cache_compaction_refused` " +
				"names it, and nothing is built); `store_full`, an object store " +
				"with no room for an output (the repository is held back until " +
				"the store shows the room — on a store that reserves nothing, " +
				"tried again once an hour, as the collector frees room); " +
				"`outputs`, a build that wrote more packs than one compaction may " +
				"record, and `bundle_too_large`, an output's bundle past the bound " +
				"even at a smaller cut — either uploads nothing and halves what " +
				"the repository's next plan merges. The committer's: `stale`, " +
				"`busy`, `repo_deleted`, `upload_expired`, `pack_too_large`, " +
				"`not_applied` and `unavailable`, as for a push. " +
				"`upload_expired` is a build and its uploads longer than the " +
				"twelve hours a key may wait to be named, `pack_too_large` the " +
				"builder's bug: each is logged by name.",
		},
		{
			Name: CodeCompactBehind, Kind: KindGauge, Unit: UnitCount,
			Shows: "Repositories this node maintains that its last pass found at " +
				"the most live packs a repository may hold, whose pushes carrying " +
				"objects are refused `compaction_behind` until a compaction lands: " +
				"`code_compaction_behind` fires on any. Zero on a node that " +
				"maintains no such repository.",
		},
		{
			Name: CodePublishRuns, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "Publications this node made as the MAINTAINER of their " +
				"repositories — each a plan of the repository's next published " +
				"multi-pack-index layer, its bitmap and its commit-graph layer " +
				"over every live pack its base does not cover, which every Gitlet " +
				"node installs — and what became of it, by `outcome`. `applied`: " +
				"its record landed and its layer is the top of the chain. " +
				"`pending`, `unknown`: its record was appended and is not yet " +
				"known to have applied. `not_due`: nothing lay outside the chain " +
				"to publish, and the repository is not planned again until its row " +
				"moves. `waiting`: a layer of the chain was still at this node's " +
				"installer, so the plan waited for it rather than replace it. " +
				"`stale`: another publication, a compaction, an expunge or a purge " +
				"moved the chain or a pack first. `busy`: no maintenance slot or no memory " +
				"room was free, a compaction of the repository was running on the " +
				"node, or its committer was busy. `refused`: by the committer or " +
				"before it (`crewlet.code.publish.refusals` says why). `failed`: " +
				"the build, an upload or the answer failed " +
				"(`code_publication_failed` names which). Every outcome but " +
				"`applied` is tried again at a later poll; `failed` rising is the " +
				"node's disk, its git or its object store.",
		},
		{
			Name: CodePublishDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"outcome"},
			Shows: "One publication from its plan to its answer, by `outcome` (as " +
				"`crewlet.code.publish.runs` counts them): the sync, the plan, the " +
				"repack, the layer's bitmap — a walk of the repository's whole " +
				"history, seconds at a gigabyte and minutes at the largest — every " +
				"part's and output's upload, and the committer's decide. Its " +
				"build is what paces the repository's next publication: at most a " +
				"tenth of the node's time.",
		},
		{
			Name: CodePublishRefusals, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"reason"},
			Shows: "Publications refused before or at their decide, by `reason`. " +
				"This node's own: `memory`, the build's charge — 600 bytes an " +
				"object of the repository for the bitmap, with room beside it for a " +
				"clone — past what the node can give its background work " +
				"(`code_cache_maintenance_refused` names it with step `publish`, " +
				"and the repository counts toward code_maintenance_refused for as " +
				"long; asked again at every pass, and a Gitlet node with the memory " +
				"takes it over); `store_full`, an object store with no room for a " +
				"part or an output (held back until the store shows the room); " +
				"`outputs` and `bundle_too_large`, its repack's, as a " +
				"compaction's; `no_bitmap`, a layer git wrote without a bitmap — no " +
				"pack it covers holds a commit — even over one more layer; " +
				"`layer_too_large`, a layer past what sixteen parts of a gibibyte " +
				"hold. The committer's: `stale`, `busy`, `repo_deleted`, " +
				"`upload_expired`, `pack_too_large`, `not_applied` and " +
				"`unavailable`, as for a compaction.",
		},
		{
			Name: CodePublishBytes, Kind: KindCounter, Unit: UnitBytes,
			Attributes: []string{"kind"},
			Shows: "Bytes this node stored for its publications, by `kind`: " +
				"`layer`, a layer's three files — its multi-pack-index, its bitmap " +
				"and its commit-graph layer — stored as parts of a gibibyte; " +
				"`outputs`, its repack's packs. A layer is about 37 bytes an " +
				"object and 69 a commit of what it covers, so a repository's root " +
				"layer is the one large write and every later one is its tail's.",
		},
		{
			Name: CodeExpungeRuns, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "Expunges this node ran as the MAINTAINER of their " +
				"repositories — a person's purge served, a repository its weekly " +
				"listing names judged, or an expunge left open finished from its " +
				"rows — each one cycle: a scan of what a ref " +
				"reaches, a build sorting some live packs into kept and dropping " +
				"ones, their begin, a rescue of what a push revived meanwhile and " +
				"their commit — by `outcome`. `applied`: its commit landed, and " +
				"what no ref reached left the repository's live packs. `pending`, " +
				"`unknown`: a record was appended and is not yet known to have " +
				"applied — the next pass reads the rows and finishes it. " +
				"`not_due`: nothing was left to rewrite — a purge already served, " +
				"every live pack arrived within the grace the weekly expunge " +
				"keeps whole, or what no ref reaches is too little to be due — " +
				"no more than its share of the live bytes, or under the least an " +
				"expunge frees — each but a served purge leaving the repository " +
				"unscanned on this node until the weekly schedule lists it " +
				"again. `stale`: a compaction, a " +
				"publication or a purge moved an input first, or the expunge was " +
				"finished elsewhere. `busy`: no maintenance slot or memory room " +
				"was free, another build of the repository was running on the " +
				"node, its copy's objects/ still held a begin's inputs, or its " +
				"committer was busy. `refused`: by the committer or before it " +
				"(`code_expunge_refused` says why: this node's memory, an object " +
				"store with no room, a build past what one record carries). " +
				"`failed`: a sync, a build, an upload or an answer failed " +
				"(`code_expunge_failed` names the step). Every outcome but " +
				"`applied` is tried again at a later pass, and a purge whose " +
				"repository holds more than one expunge rewrites at once is " +
				"served by several `applied`.",
		},
		{
			Name: CodeExpungeDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"stage"},
			Shows: "One stage of an expunge, by `stage`: `scan`, its sync and " +
				"its reads of what it keeps; `build`, its kept and dropping " +
				"packs' builds — a walk of the repository's whole history from " +
				"every tip it keeps, minutes at the largest; `upload`, every " +
				"output's upload at once; `begin`, its begin's decide and the " +
				"adoption of its kept packs; `rescue`, the sync past the begin " +
				"and the rescue's build and upload; `commit`, its commit's decide. " +
				"`build` and `rescue` climbing with the repository's history are " +
				"the walks; `upload` alone climbing is the object store.",
		},
		{
			Name: CodeExpungeDroppedBytes, Kind: KindCounter, Unit: UnitBytes,
			Shows: "Bytes the expunges this node ran took out of their " +
				"repositories' packs: each commit's dropping packs less the " +
				"rescue it kept of them. The store keeps them for gitlet's " +
				"RetireGrace (a day) after the commit, so a reader already on one " +
				"finishes, and the collector deletes them at its next pass after " +
				"that.",
		},
	}
}

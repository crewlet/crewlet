package gitlet

import (
	"math/bits"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/usage"
)

// The domain's bounds. Those a configuration document is also validated
// against are config's (internal/config/code.go), aliased here — one value,
// never a copy — since gitlet imports config and not the reverse.

// Limits are the epoch's code.limits with the defaults applied: what a push
// is held to. They cross the estate wire in [RepoDetail], which an import
// chunks its pushes to.
type Limits struct {
	MaxPushBytes int64 `json:"max_push_bytes"`
	MaxBlobBytes int64 `json:"max_blob_bytes"`
	RepoMaxBytes int64 `json:"repo_max_bytes"`
	// PushesPerSeatDay (code.limits.pushes_per_seat_day) bounds the pushes one
	// SEAT lands in a company day across every repository: a runaway bound on
	// what a seat costs the code log, held by the push decide
	// ([RefusalPushQuota]) to seats only, never to a person.
	PushesPerSeatDay int `json:"pushes_per_seat_day"`
	// MergeByFloor is code.merge_by_floor: the least authority a merge into ANY
	// protected ref needs. A merge is held to the stricter of it and every
	// matching rule's merge_by ([MergeBy.Stricter]); a rule with no gate reads
	// as writers.
	MergeByFloor MergeBy `json:"merge_by_floor"`
}

// DefaultLimits are the limits of a company that sets none.
func DefaultLimits() Limits { return LimitsOf(config.Code{}) }

// LimitsOf is a company's code.limits with the defaults applied, and its
// code.merge_by_floor. It CONVERTS config's own readings
// ([config.CodeLimits.Resolved], [config.CodeMergeBy.Resolved]) rather than
// resolving the defaults again, so `crewlet validate` and the committer's
// decide read one document one way (TestLimitsOfIsConfigsReading).
func LimitsOf(c config.Code) Limits {
	r := c.Limits.Resolved()
	return Limits{MaxPushBytes: r.MaxPushBytes, MaxBlobBytes: r.MaxBlobBytes, RepoMaxBytes: r.RepoMaxBytes,
		PushesPerSeatDay: r.PushesPerSeatDay, MergeByFloor: MergeBy(c.MergeByFloor.Resolved())}
}

// OutsideSeatShare divides what seats OUTSIDE a repository's owning unit
// (below write, pushing only into their own namespaces, [SeatNamespace]) may
// fill of its live refs ([MaxRefsPerRepo]) and packed bytes
// (code.limits.repo_max_bytes): 1/OutsideSeatShare of each, judged on the
// repository's whole counts. Without it one full_access seat that merely
// reads a repository could fill it and refuse its owners every push there.
//
// HALF, the engine's rule for an untrusted class beside a trusted one (as
// [GitShare]), a product judgment. No per-seat bound inside that half: refs
// could be counted per seat (code_refs.seat), but compacted packs' bytes
// belong to no seat, so a per-seat bound would hold refs and never bytes;
// one shared half keeps the two bounds alike.
const OutsideSeatShare = 2

// BarrierRecordBytes is what one code-log barrier costs its stream as the
// broker counts it: the encoded record ([EncodeBarrier]) in its signed frame
// (ADR-0031), its subject and the per-message overhead.
//
// 176 BYTES, MEASURED: 156 at the first generation, 173 at the last (a
// generation prints up to ten digits), so it bounds every barrier and
// overstates the commonest by an eighth, noise beside census_drift's factor
// of two (TestABarrierCostsTheCodeLogWhatTheCensusSays). It is also a
// mutation-log barrier's cost from its second generation to its tenth, so
// the forecast prices both logs at one figure. Forty bytes are the frame,
// with a 32-byte MAC and the signing key's id at the suites' two bytes; a
// longer key id adds a byte a character, under 3 % of a barrier.
const BarrierRecordBytes = 176

// GitReadsShare is how much of the code log's byte ceiling the census lets
// its git clients' barriers hold over the trim's min_age: one part in TEN.
//
// AN ALLOWANCE, not the split the ceiling was sized by: that ratio (config's
// DerivedCodeLogNumerator, behind [CodeLogMaxBytes]) already counts the
// reference company's git barriers beside its pushes: 2.4–5.0 MB a day,
// 17–35 MB over the default seven days — under a thirtieth of the tenth of a
// derived 10⅔ GiB ceiling, and about a third of the tenth of even the 1 GiB
// floor. A JUDGEMENT: smaller fires census_drift on a busy healthy git host,
// larger lets barriers crowd the log's pushes before anybody is told. The
// share follows the ceiling the stream holds, so a company that outgrows it
// resizes the log (`crewlet retention set-capacity`).
const GitReadsShare = 10

// GitReadsPerDay is the linearizable git reads a day the code log's census
// allows its git clients (statelog.Census.Clients): barriers filling
// 1/GitReadsShare of ceiling over minAge, 87 154 a day at the 1 GiB floor
// and seven days. They are the readers no seat count predicts, a person's or
// their CI's; a seat's reads of the log are the seats' census term, held to
// it on each node by [NewResyncs], and a check box's by [CheckKey.Resyncs].
//
// ceiling is the one the stream HOLDS, read off the broker, never Tier A's
// stream.code_log_max_bytes, which sizes only a log not yet created: raising
// it would silence the alarm without resizing. The whole minAge counts (a
// barrier stays for all of it), at least a day, in 128-bit arithmetic: the
// ceiling times a day in nanoseconds overflows 64 bits past about 208 KiB.
func GitReadsPerDay(ceiling int64, minAge time.Duration) int {
	if ceiling <= 0 {
		return 0
	}
	const day = uint64(24 * time.Hour)
	span := uint64(max(minAge, 24*time.Hour))
	// ceiling × day ÷ span. The product's high word is below 2^46 (a ceiling
	// below 2^63 times a day below 2^47), and a span of a day or more is
	// above 2^46, so the quotient fits in 64 bits and Div64 cannot panic.
	hi, lo := bits.Mul64(uint64(ceiling), day)
	scaled, _ := bits.Div64(hi, lo, span)
	return int(scaled / GitReadsShare / BarrierRecordBytes)
}

const (
	// CodeLogMaxBytes is the ceiling the suites provision this log at (a node sizes its own by
	// Tier A): TWO THIRDS of the tracker's 16 GiB, what an unset stream.code_log_max_bytes
	// derives (TestTheCodeLogIsDeclaredAtTierAsRatioToTheTrackers), weighed against the logs
	// beside it since every node reserves this one. A day prices 3 000 pushes at the MEASURED
	// 1 667 bytes a seat's coding box's push alone in its record (BenchmarkPushLogBytes),
	// 5.0 MB, the compactions that merge their packs and the sweeps of what those retired at
	// the MEASURED 108 and 44 bytes a push (internal/gitlet/compact's steady arm at the floor),
	// 0.5 MB, the publications of their layers, 0.18–3.51 MB, its git clients' barriers at
	// BarrierRecordBytes, 2.4–5.0 MB, and its changes' records, 14.7–86.6 MB
	// (TestAChangesRecordsCostTheLogWhatTheCodeLogsDayPricesThemAt), against the tracker's
	// measured 9.9 MB of records and 2.2 MB of barriers. A company whose seats open changes
	// sets its own ratio (config's DerivedCodeLogNumerator).
	CodeLogMaxBytes = (16 << 30) * config.DerivedCodeLogNumerator / config.DerivedCodeLogDenominator

	// CodeLogDuplicates is the window the broker collapses a repeated operation
	// id in, the tracker's and the knowledge base's: an optimisation only — the
	// operation ledger and a push's own id make a retry idempotent.
	CodeLogDuplicates = 2 * time.Minute

	// MaxPushesPerRecord is the most pushes one record carries: it bounds the
	// work one lost compare-and-set round re-decides, and matches the tracker's
	// bulk writes. Group commit pays in bursts (a CI fan-out of tag pushes): a
	// record of sixty-four holds the write lane 10–12 ms applied and committed,
	// 0.16–0.18 ms a push against 3.4–4.8 ms alone, far inside
	// statelog.ApplyTxTimeBudget, and a burst runs 2 050–2 090 pushes a second
	// against 86–89 for a lone push (doc.go's "What a push costs, measured").
	// Wider records are NOT measured and would not pay: every node applies every
	// record on its one lane, so width trades round trips for a longer hold
	// every other write waits behind.
	MaxPushesPerRecord = 64

	// CodeLogMaxRecordBytes is the largest record the code log admits
	// ([statelog.StreamSpec.MaxRecordBytes]): the transport's largest
	// ([statelog.MaxTransportRecordBytes]), as the tracker's is. It sizes the
	// node's appends in flight on the log ([statelog.StreamSpec.MaxAppendBytes],
	// [statelog.NewReserve]) to eight records of [MaxRecordBytes], and the gate
	// reserve's fleet term, which at the 1 GiB floor a sixteenth of the ceiling
	// outweighs (64 MiB against 57). A record THIS build forms is bounded by
	// [MaxRecordBytes]; a newer build's may reach this, which the apply already
	// assumes ([Applier]'s tombstone backstop).
	CodeLogMaxRecordBytes = statelog.MaxTransportRecordBytes

	// MaxRecordBytes is the largest record this domain's committer forms,
	// 1 044 480 bytes: an EIGHTH of the node's appends in flight on the code log,
	// each counted with its append overhead, so eight maximal records are out at
	// once and a committer serving many repositories never serialises behind
	// one. A create, settings change or restore carrying a repository's settings
	// whole fits by the settings' own bounds, the path owners' bytes among them
	// (TestTheLargestSettingsFitTheirRecordAndTheDetail).
	MaxRecordBytes = (CodeLogMaxRecordBytes+appendOverheadBytes)/8 - appendOverheadBytes

	// appendOverheadBytes is what the log's reserve counts an append at beyond
	// its payload (signature frame, store framing, subject, headers): the
	// transport's largest message less its largest record, statelog's own
	// definition, so the two cannot disagree (TestTheCodeDomainsDeclaration).
	appendOverheadBytes = queue.MaxPayloadBytes - statelog.MaxTransportRecordBytes

	// MaxRecordRows is the most rows one record's apply may write or delete: the
	// framework's per-transaction budget, checked only at record boundaries, so
	// a larger record would hold the shared write lane past it. A push record's
	// tombstone sweep (past RefTombstoneHorizon or MaxTombstonesPerRepo) and then
	// its removal of drops past PruneGrace fill what its own rows leave. Its
	// notices count too (noticeWrites): at their widest, 3 904, they hold the
	// lane 143 ms (doc.go's measured section). Its change tallies and push
	// activity get what an older build's rows leave (record.go).
	MaxRecordRows = statelog.ApplyTxRowBudget

	// MaxCommandsPerPush is the most ref updates one push carries; its apply then
	// writes at most 2 005 rows. A record counts each command by the rows it may
	// touch (rowBound: a deletion three, a forced update two, any other one), so
	// three maximal fast-forward pushes fit one record's row budget, one of
	// forced updates or deletions, and two its byte budget (about 410 KiB each at
	// MaxRefNameBytes). An import chunks its refs at this count, or fewer where
	// their bytes reach EntryRoom first (ImportProposal.Fitted): refs JSON spells
	// long (`&`, `<` and `>` take six bytes) reach it at some six hundred, and are
	// refused by the receiver before its work (Proposal.ValidateCommands).
	MaxCommandsPerPush = 1_000

	// MaxRefsPerRepo is the most LIVE refs a repository holds; past it a
	// repository has a namespace problem, refused (repo_refs) rather than slowly
	// degraded. A protocol-v0 advertisement costs about 100 bytes a ref, so about
	// 10 MB on every v0 fetch and every cache sync's packed-refs rewrite. At the
	// bound a push measured about three seconds over a gigabyte of history
	// (internal/gitlet/host's measured section). A deleted ref's tombstone never
	// counts against it: MaxTombstonesPerRepo bounds those, and refuses nobody.
	MaxRefsPerRepo = 100_000

	// MaxTombstonesPerRepo is the most TOMBSTONES (refs deleted within
	// RefTombstoneHorizon, kept so a delta-syncing cache learns of each) a
	// repository keeps beside its live refs, so its refs table never passes
	// MaxRefsPerRepo + MaxTombstonesPerRepo rows. Past it the applier forgets the
	// oldest, or removes outright the rows its sweep cannot make room for
	// (apply_push.go); a deletion is never refused. Equal to MaxRefsPerRepo: a
	// delta is answered only while it has no more rows than the full answer
	// (reader.go's deltaOutweighs), passes it in bytes only by tombstones in
	// place of packs (TestADeltaPassesTheWidestFullAnswerOnlyByTombstonesInPlaceOfPacks),
	// a full repository's clean-up keeps its 48-hour horizon, and the large
	// company's 80 000 branch deletions in 48 hours on one repository fit.
	MaxTombstonesPerRepo = MaxRefsPerRepo

	// MaxIntroduced is the most introduced commit ids a push row lists (43
	// bytes each); IntroducedCount carries the rest.
	MaxIntroduced = 1_000

	// MaxProtectionsPerRepo is config.CodeMaxProtections: every matching
	// rule is asked about every command in the decide.
	MaxProtectionsPerRepo = config.CodeMaxProtections

	// MaxPathOwners is config.CodeMaxPathOwners: one code_path_owners row each,
	// so the final purge deletes them inside MaxRecordRows (the limits test holds
	// the sum) and a settings record replacing them all touches twice this many.
	MaxPathOwners = config.CodeMaxPathOwners

	// MaxPathOwnerPaths is config.CodeMaxPathOwnerPaths: the globs one path
	// owner names, packed into its one row.
	MaxPathOwnerPaths = config.CodeMaxPathOwnerPaths

	// MaxPathOwnersBytes is config.CodeMaxPathOwnersBytes: their JSON as a record
	// carries it, the bound the counts alone do not give that holds every create,
	// settings change and restore to MaxRecordBytes beside the widest rules
	// (TestTheLargestSettingsFitTheirRecordAndTheDetail).
	MaxPathOwnersBytes = config.CodeMaxPathOwnersBytes

	// MaxSnapshotItems is the most rows one snapshot or listing page carries:
	// about 1–2 MB of JSON for typical refs, 4 MB at MaxRefNameBytes, under the
	// estate reply's ceiling with room. MaxPageBytes may end a page first.
	MaxSnapshotItems = 10_000

	// MaxPageBytes is the encoded rows one page may carry WHATEVER its row limit
	// says: half the estate reply ceiling (queue.MaxPayloadBytes), leaving room
	// for the envelope and the repository row, since a row count alone would put
	// two hundred maximal push rows, 88 MB, in one reply. A page always serves at
	// least one row, so it always advances.
	MaxPageBytes = 4 << 20

	// DefaultMaxBlobBytes is an unset code.limits.max_blob_bytes, which
	// config validates against and defines (config.DefaultCodeMaxBlobBytes).
	DefaultMaxBlobBytes = config.DefaultCodeMaxBlobBytes

	// DefaultRepoMaxBytes is an unset code.limits.repo_max_bytes, which
	// config validates against and defines (config.DefaultCodeRepoMaxBytes).
	DefaultRepoMaxBytes = config.DefaultCodeRepoMaxBytes

	// DefaultDeletedRetention is an unset code.deleted_retention_days: the
	// trim's default min_age, so a deletion is undone from the same window
	// the log can replay.
	DefaultDeletedRetention = config.DefaultCodeDeletedRetentionDays * 24 * time.Hour

	// PushHistory is how long a push row is kept: usage.History, so "what
	// happened over the last half year" has one answer across the engine.
	PushHistory = usage.History

	// RefTombstoneHorizon is how long a deleted ref is kept as a TOMBSTONE (a row
	// with an empty oid) so a delta-syncing cache learns of the deletion, or less,
	// oldest first, once MaxTombstonesPerRepo is reached. A cache synced before a
	// swept deletion takes a full answer (code_repos.tombstones_through), which
	// re-reads rows but reuses every pack, so the horizon trades rows kept against
	// an idle cache's full listing, never a download. Two days covers a cache
	// fetched on any working day across a one-day gap.
	RefTombstoneHorizon = 48 * time.Hour

	// PruneGrace is how long a ref's DROP (a code_ref_drops row: the object a
	// forced update or deletion moved a ref off) keeps what only it reaches
	// reachable to the weekly expunge, so force-pushed-away history stays
	// fetchable by id, unless a person purges unreachable objects, which keeps no
	// grace (ADR-0099). FOURTEEN DAYS, git's own gc.pruneExpire default, the grace
	// anyone running git expects. Every push record removes its repository's
	// older drops by its broker instant (apply_push.go); a reader judging by its
	// own clock is within twice objstore.AssumedClockSkew of it while no node
	// raises `clock_skew`, a policy's edge, where RetireGrace's margin guards a
	// read in flight.
	PruneGrace = 14 * 24 * time.Hour

	// ExpungeInterval is how often a repository's maintainer ASKS whether an
	// expunge is due: every active or archived repository, scratch included, last
	// judged this long ago or never ([ReadQuery.ScannedBefore]), so one an expunge
	// committed in or a scan found not due ([ExpungeScan]) is left out of every
	// node's listing this long (internal/gitlet/compact). SEVEN DAYS, half of
	// [PruneGrace]: a drop's object goes within a week of its grace ending, and
	// the scan, which reads every object a ref reaches, walks even a hot
	// repository at most weekly.
	ExpungeInterval = 7 * 24 * time.Hour

	// ExpungeShare makes an expunge due on the maintainer's schedule once a
	// repository's unreachable bytes (reached by no ref, no drop within the grace
	// and no row in flight) are MORE than 1/ExpungeShare of its live pack bytes.
	// FOUR, A JUDGMENT: an expunge makes every Gitlet node holding the repository
	// download the kept packs again, so below a quarter each downloads over three
	// kept bytes per byte freed. Unreachable bytes count against
	// code.limits.repo_max_bytes (refused repo_size), so a quarter is also the most of the quota
	// garbage holds before it is taken back.
	ExpungeShare = 4

	// ExpungeMinBytes is the least unreachable bytes that make an expunge due,
	// whatever their share, and the live bytes under which a repository is judged
	// clean from its row alone, never synced or walked.
	//
	// 64 MiB, PROVISIONAL: BenchmarkExpunge (internal/gitlet/compact) places it
	// where a cycle stops being dominated by its walk of the reachable history.
	// Its one run put that between 24 and 48 MiB for a 100 MB repository, while
	// its 1 GB pair, on a loaded host, could not; kept above both until the 1 GB
	// arms run on an unloaded host.
	ExpungeMinBytes = 64 << 20

	// RefExpiry is a SEAT BRANCH'S IDLE WINDOW: how long a branch a seat set in
	// its own namespace (code_refs.seat, [SeatNamespace]) goes unset before the
	// REF LIFECYCLE deletes it, unless a live turn holds it (retention_expiry.go;
	// the hold is recorded past [RefHoldRecordAfter]). TWO WEEKS, Q4 as approved:
	// past the trim's default week and a person's holiday, and short enough that
	// a busy seat holds no hundreds of dead branches against MaxRefsPerRepo. A
	// deletion is restorable from its push row's Old for [PushHistory] while the
	// commit survives: PruneGrace, or no grace on a person's purge (ADR-0099). It
	// is also code_refs_near_limit's window ([NearLimit]): live refs plus those
	// set within it reaching MaxRefsPerRepo raise the alarm while deleting refs
	// can still prevent a repo_refs refusal.
	RefExpiry = 14 * 24 * time.Hour

	// MaxDescriptionBytes is config.CodeMaxDescriptionBytes: a repository
	// description is one paragraph, riding every settings record and every
	// listing row.
	MaxDescriptionBytes = config.CodeMaxDescriptionBytes

	// MaxRemoteURLBytes is config.CodeMaxRemoteURLBytes: the longest URL a
	// repository's mirror or its import names, which every record of its
	// settings carries as it carries the description.
	MaxRemoteURLBytes = config.CodeMaxRemoteURLBytes

	// MaxCredentialRefBytes is config.CodeMaxCredentialRefBytes: the longest
	// ${NAME} reference a mirror or an import names its credential by — all
	// a record carries of one.
	MaxCredentialRefBytes = config.CodeMaxCredentialRefBytes

	// MaxMirrorRefs is config.CodeMaxMirrorRefs: the most ref patterns a push
	// mirror names, as many as a repository carries protection rules.
	MaxMirrorRefs = config.CodeMaxMirrorRefs

	// MaxChangeBodyBytes is the longest body (description) a change carries: a
	// Worklet task body's, which it most often carries over. It outgrows a page,
	// so it is a row of its own (code_change_bodies, replicated 0094), not a
	// column of the row every head move rewrites; weighed by its change's
	// body_bytes, it counts toward PurgePartBytes.
	MaxChangeBodyBytes = 32 << 10

	// MaxChangeTitleBytes is the longest change title: ONE LINE a listing shows
	// whole, twice the commit subject git's tools wrap at, and short enough that
	// code_changes, which every head move rewrites, stays inside its page
	// (replicated 0094).
	MaxChangeTitleBytes = 256

	// MaxCloseReasonBytes is the longest reason a close of a change gives, kept
	// on the change's row, which replicated 0094 sizes by it as by the title.
	MaxCloseReasonBytes = 1 << 10

	// MaxOverrideReasonBytes is the longest reason a person gives to merge past
	// the target's merge gate ([Merge.Override]), audited on the merge's record
	// and the change's row. The close reason's bound: the row holds one or the
	// other (a reopen clears the close reason), so it stays the width replicated
	// 0094 sized.
	MaxOverrideReasonBytes = MaxCloseReasonBytes

	// MaxReviewersPerChange is the most reviewers a change asks at once: the
	// tracker's MaxMentions, since a request is a mention awaiting an answer, and
	// the readiness readers read each one's latest verdict. Only an ask still
	// WAITING counts ([ReviewerState]).
	MaxReviewersPerChange = 32

	// MaxReviewNoteBytes is the longest note a review request or an answer to a
	// finding carries, kept on the reviewer's or the finding's narrow row as a
	// close reason is on a change's.
	MaxReviewNoteBytes = 1 << 10

	// MaxOwnedPathsBytes is the most a path owner's ask's globs weigh as JSON
	// (code_reviewers.owned_paths), the rest counted: a note's bound, since the
	// row holds one or the other, keeping the width replicated 0100 sized.
	MaxOwnedPathsBytes = MaxReviewNoteBytes

	// MaxCommentBytes is the longest comment on a change: as long as its
	// description (MaxChangeBodyBytes) and a Worklet task comment, which a review
	// most often quotes. Weighed into its conversation's text_bytes, which a
	// purge part counts toward PurgePartBytes.
	MaxCommentBytes = 32 << 10

	// MaxReviewSummaryBytes is the longest summary a review gives — a
	// paragraph or two of what the reviewer found, kept on the review's row
	// beside its verdict and weighed into its conversation's text_bytes.
	MaxReviewSummaryBytes = 4 << 10

	// MaxFindingsPerReview is the most findings one review raises: one careful
	// pass line by line, few enough that a review fits one record beside the
	// room it keeps ([MaxConversationBytes]; past it a review is refused, "split
	// into two reviews of the same revision", which supersede nothing of each
	// other).
	MaxFindingsPerReview = 64

	// MaxFindingTitleBytes bounds a finding's title: one line a listing shows
	// whole. It and the next two bounds are weighed into the conversation's
	// text_bytes and kept on a row of their own (code_finding_texts), off the
	// narrow row resolutions and supersessions rewrite.
	MaxFindingTitleBytes = 200
	// MaxFindingBodyBytes bounds a finding's body: a paragraph or two of why.
	MaxFindingBodyBytes = 4 << 10
	// MaxSuggestionBytes bounds a finding's suggestion: the lines it would
	// put in place of the ones it is anchored to.
	MaxSuggestionBytes = 16 << 10

	// MaxStandingFindings is the most findings one reviewer keeps STANDING
	// (open, resolved, won't fix or disputed) on one change: sixteen reviews'
	// worth, reached only by reviewing one revision again and again, since a
	// later revision's review supersedes what it does not carry. Past it a review
	// is refused too_many_findings. It keeps a superseding review one record,
	// 1 024 narrow rows beside its own, a third of [MaxRecordRows], costing 24 ms
	// of decide and apply at the bound (BenchmarkTheWidestReview).
	MaxStandingFindings = 1 << 10

	// MaxRoutingBytes is the room every record of a change keeps for the snapshot
	// of whom it concerns (owner, authors, reviewers, path owners, participants,
	// mentions, work item), so a write's text never takes the room its wakes
	// need. Each list is bounded (MaxMentions, MaxReviewersPerChange,
	// MaxRoutedParticipants) and a record fills only the roles its op routes by
	// (concerns.go): the widest, a comment's, is some 15 KiB at iam.MaxLogin
	// handles, so the room is four times that, kept for roles a later op adds.
	MaxRoutingBytes = 64 << 10

	// MaxCarrierBytes is the most whom one code write was made for takes,
	// encoded, wherever kept (a record's routing and payload; a revision's,
	// push's or queue entry's working_for, ADR-0109): a seat's handle, or a
	// credential's login with its turn's apps (writtenForInvalid). The widest,
	// iam.MaxLogin with every iam.Apps app, is 163 bytes; 200 leaves room for
	// apps a later build adds (TestAPushMeasuredForItsRecordCountsWhomItWasMadeFor).
	// Nothing refuses at it: it counts a landing's push rows' bytes
	// (HorizonBytesPerRecord).
	MaxCarrierBytes = 200

	// MaxMentions is the most handles one write of a change mentions that its
	// record routes: the tracker's own MaxMentions. Past it the write is refused,
	// naming the bound, rather than a mention silently going unheard.
	MaxMentions = 32

	// MaxRoutedParticipants is the most handles one role of a change's routing
	// names (authors, watchers, a thread's participants, the raisers an answer
	// replies to): as many as a revise asks to look again (MaxReReviewers). Past
	// it a wake is not the way to reach them, and they read their listings; each
	// role is read in its key's order, so every writer names the same ones.
	MaxRoutedParticipants = 64

	// MaxConversationBytes is the most one write of a change's conversation may
	// encode to: [MaxRecordBytes] less the routing's room and 4 KiB of envelope.
	// A review of [MaxFindingsPerReview] findings with suggestions at their bound
	// can pass it, and is refused, "split into two reviews of the same revision".
	MaxConversationBytes = MaxRecordBytes - MaxRoutingBytes - 4<<10

	// HorizonDeletesPerRecord is how many expired PUSH ROWS one push record
	// removes from its repository: no fewer than a record adds
	// (MaxPushesPerRecord), so removal keeps pace, and fixed, so the committer
	// counts it before the record exists; HorizonBytesPerRecord bounds the same
	// removal by weight. It does NOT bound the TOMBSTONES a record sweeps, which
	// a bulk deleter makes faster than any fixed share: the sweep takes what the
	// record leaves of MaxRecordRows (rowBound reserves one per deletion), then
	// the drops past PruneGrace, so a record writes and removes at most
	// MaxRecordRows.
	HorizonDeletesPerRecord = 64

	// HorizonBytesPerRecord is how many BYTES of expired push rows one push
	// record removes beside HorizonDeletesPerRecord's count, by row_bytes, oldest
	// first, passing it by at most one row. MaxRecordBytes, the most push-row
	// bytes a record can ADD (a landing's rows keep more than its record carries,
	// under a quarter more: pushRowBytes,
	// TestAPushMeasuredForItsRecordCountsWhomItWasMadeFor), so a removal stopped
	// by either bound has taken the oldest record's whole rows (one instant, so
	// first and together) and keeps pace. NO MORE: push rows reach 461 000 bytes
	// and freeing overflow pages costs about 2.4 ms a megabyte, so a count alone
	// would remove 29 MB, 60–82 ms more of every node's lane, in the largest push
	// record (doc.go's measured section).
	HorizonBytesPerRecord = MaxRecordBytes

	// PurgePartRows is how many rows one purge step deletes: under MaxRecordRows
	// with room for the record's bookkeeping and the final purge's (every
	// protection rule, the name claim, the repository's row and the marker),
	// which the limits test pins. A change's notices count by
	// code_changes.notices, which every node agrees on and none holds fewer
	// rows than (apply_notices.go). PurgePartBytes bounds a part's wide rows by weight.
	PurgePartRows = 3_000

	// PurgePartBytes is how many BYTES of wide rows one purge step deletes beside
	// PurgePartRows, in one budget: push rows first by row_bytes, oldest first,
	// then changes' bodies by body_bytes, always at least one row and at most
	// one past it. A part cut by it deletes nothing else, and the final purge,
	// whose statements are unbounded, forms only once what is left fits both
	// bounds. Rows alone bound only narrow rows' time: freeing a wide row's
	// overflow pages costs about 2.4 ms a megabyte, and 1 000 rows of 460 KB held
	// the lane over a second in a record the budget cannot split. THIRTY-TWO MEBIBYTES is a third
	// of statelog.ApplyTxTimeBudget, 76–103 ms at the widest mixes measured
	// (doc.go's measured section).
	PurgePartBytes = 32 << 20

	// RetentionInterval is the code-retention pass's tick: fifteen minutes, as
	// the fleet's other housekeeping (the trim, the maintenance sweep). A purge
	// runs days after a removal by design, so the tick is noise beside it. Its
	// own constant: this domain does not import maintenance, which reaches the
	// sandbox, the scheduler and the agent ledger, and the sandbox will read
	// this package's types.
	RetentionInterval = 15 * time.Minute

	// RetentionLaneShare is the most of the estate's one write lane a
	// code-retention pass takes: one pacer over every step (sweeps, purges, name
	// releases, scratch expiry) waits (1 − share) / share times the step's
	// duration, twice at a third, before the next. A step's duration is its apply
	// plus whatever the lane applied ahead of it, so a busy lane slows the pass.
	// A THIRD, because every data node applies every record and the holder sees
	// only its own lane: unpaced, a pass held it 82% and a node 1.2 times slower
	// fell behind; at a third a node up to three times slower keeps up, the share
	// PurgePartBytes sizes a part to. Paced, 33–37% measured
	// (TestAPassHoldsAtMostAThirdOfTheWriteLane). A sweep holds other nodes less:
	// 1.5 ms of apply (BenchmarkApplyARetireSweep) of its 4.9 ms on the whole path
	// (BenchmarkRetireSweepStep).
	RetentionLaneShare = 1.0 / 3

	// RetentionPassBudget is how long one code-retention pass takes steps for,
	// ONE BUDGET and one pacer over retire sweeps past RetireGrace, then checks
	// sweeps past CheckHistory, then purges, then name releases and scratch
	// expiry, leaving the rest to the next pass. statelog.StallGrace, A MINUTE: a
	// node k times slower than the holder queues (k·share − 1) of each second, so
	// one up to six times slower ends a pass at most StallGrace behind and catches
	// up before the next. Of a pass's twenty seconds of steps, design-point retire
	// sweeps (some 170 at 4.9 ms) take 0.8 s, checks sweeps (about 104 at 3.5 ms,
	// BenchmarkChecksSweepStep) 0.4 s, and purges the rest, 205–240 parts, so ten
	// thousand removed repositories purge within two days. A backlog of every
	// repository's sweeps (84 s) clears by the fifth pass, a week of retirements
	// within twelve (doc.go's measured section).
	RetentionPassBudget = statelog.StallGrace

	// RetireGrace is how long a retired pack's row (code_packs_retired, ADR-0081)
	// keeps its object from the collector before retire_sweep removes it, so a
	// retired pack outlives its longest read (ADR-0090). A DAY: a reader that
	// began before the retirement may read the largest bundle for
	// objstore.ReadBudget(MaxPackBytes), 21 h 21 min, judged by the duty
	// holder's clock against the broker's retired_at, two clocks up to twice
	// objstore.AssumedClockSkew apart while no node raises `clock_skew`: 23 h 21
	// min, under a day by 39 min (internal/objstore/references holds it). Equal
	// to the collector's PendingGrace by the same reason, not by reference, since
	// this package does not import the collector.
	RetireGrace = 24 * time.Hour

	// RetireSweepRows is the most retired pack rows one retire_sweep record
	// removes: MaxRecordRows less the repository's own row, which it also moves.
	// More is stepped record by record, as a purge is; a full record holds the
	// lane 40 ms (BenchmarkApplyARetireSweep), under the third of
	// statelog.ApplyTxTimeBudget a purge part is sized to.
	RetireSweepRows = MaxRecordRows - 1

	// MaxFullRetirements is the most retirements a FULL snapshot serves beside
	// its rows (those above StateQuery.RetiredSince, all or none), and so where a
	// delta's retirements start to weigh against the full answer on its side
	// alone (reader.go's choose); the largest answer is a full repository and one
	// page. ONE PAGE (MaxSnapshotItems): served, some 2.4 MB under MaxPageBytes;
	// not served, a cache drops its commit-graph over the pack files no row
	// names, costly past some 36 000 objects; and a page is some 9 400 pushes to
	// one repository within RetireGrace, past which the full answer is the
	// cheaper catch-up. Not the full answer's own row count: a repository of few
	// refs may be a monorepo, and compaction keeps live packs few, so that served
	// nothing just where a graph is dearest.
	MaxFullRetirements = MaxSnapshotItems

	// CommitterInbox is how many proposals one repository's committer queues on
	// one node: SIXTEEN FULL RECORDS of the smallest pushes, so a burst (a CI
	// fan-out of tag pushes) queues whole behind an in-flight record. Past it a
	// push is refused busy AT ONCE and the client runs git push again, rather than a dying
	// process holding an unbounded queue; CommitterInboxBytes bounds it by
	// weight. The wait is bounded in TIME by the committer (committer.go): busy
	// once too little of the asker's attempt is left, or after a whole
	// CommitBudget with no deadline, counted by code_commit_busy (busy.go's
	// `waited`). A ROUTED burst meets this bound too, never the estate's proposal
	// registration (internal/estate's proposalAnswers, CommitterNodeProposals),
	// past which it would wait unseen.
	CommitterInbox = 1_024

	// CommitterInboxBytes is the most one repository's queue may weigh, by the
	// bytes each proposal's entry can reach in its record: SIXTEEN RECORDS'
	// BYTES, making CommitterInbox's reason exact for large pushes too (a count
	// alone admits about a gibibyte at EntryRoom each). The limits test holds it
	// to at least two records, a full one queued behind one in flight, and over
	// CommitterInbox minimal pushes, so small pushes meet the count and only
	// large ones the bytes.
	CommitterInboxBytes = 16 * MaxRecordBytes

	// CommitterNodeBytes is the most the proposals one node's committer HOLDS may
	// weigh together across every repository, each from queueing to its answer:
	// FOUR REPOSITORIES' CommitterInboxBytes, sixty-four records, past which a
	// push is refused busy at once. Per-repository queues bound nothing together:
	// a thousand of the largest pushes from as many repositories would hold about
	// a gibibyte. The largest entry is EntryRoom (1 043 456 bytes, some six
	// hundred `&` refs; Proposal.ValidateCommands), a plain one at most 451 222.
	// A JUDGEMENT of a burst's reach against a data node's memory: 64 MiB, room
	// for four bursts whole; small pushes meet CommitterInbox long before, and an
	// import's chunks are refused busy past it and retried.
	CommitterNodeBytes = 4 * CommitterInboxBytes

	// CommitterNodeProposals is the most proposals one node's committer HOLDS at
	// once by COUNT, as CommitterNodeBytes counts them: ONE REPOSITORY'S FULL
	// QUEUE AND THE BATCH AHEAD OF IT, 1 088, past which a proposal is refused
	// busy at once. Small pushes meet the bytes only past some forty thousand,
	// so without a count two repositories' bursts overflow the estate's proposal
	// lane (internal/estate's proposalAnswers) and wait unseen in the broker's
	// buffer. The estate sizes that lane as this share beside
	// ChangeWriterNodeWrites and ChecksWriterNodeWrites, so a burst meets the
	// committer's own refusal.
	CommitterNodeProposals = CommitterInbox + MaxPushesPerRecord

	// CommitterAnnouncements is the most landed records' announcements one
	// node's committer holds unpublished (announce.go): CommitterNodeProposals.
	// Every landed record answers at least one held proposal (a push batch hands
	// over at most two announcements), so one is dropped only once the announcer
	// is a whole node's held work behind a stream that acknowledges nothing
	// (checkEventBudget each). A drop is logged, costing what a refused
	// announcement already does (its day one count short, its rows whole), never
	// a wait before the next push. Each holds what its record named, never rows.
	CommitterAnnouncements = CommitterNodeProposals

	// MaxChangeWritesWaiting is the most writes of ONE change's conversation one
	// node's change writer holds at once, publishing and waiting
	// ([ChangeWriter]): SIXTY-FOUR, everybody it concerns writing at once
	// (MaxReviewersPerChange and as many again). A healthy single flight lands a
	// write every 4.3 ms (BenchmarkAChangesWritesInTurn), draining them in 0.3 s
	// against a wait of up to CommitBudget. Past it the change outpaces its
	// flight or its publish is held, and a waiting write would only hold a
	// proposal-lane slot until refused, so it is refused busy AT ONCE,
	// undecided. It keeps one hot change under a sixteenth of
	// ChangeWriterNodeWrites.
	MaxChangeWritesWaiting = 64

	// ChangeWriterNodeWrites is the most writes of changes' conversations one
	// node's change writer holds across every change: SIXTEEN CHANGES' widest
	// bursts, as many as a committer queue holds pushes (CommitterInbox). Past
	// it a write is refused busy at once. It is the writer's SHARE of the
	// estate's proposal lane (internal/estate's proposalAnswers sums them), so
	// comments never hold the slots a routed push needs.
	ChangeWriterNodeWrites = 1_024

	// ChangeWriterNodeBytes is the most the writes one node's change writer holds
	// may weigh, each by its record's bytes ([ConversationWrite.Validate]):
	// SIXTY-FOUR OF THE WIDEST (a review at MaxConversationBytes); ordinary
	// comments meet ChangeWriterNodeWrites first. The committer's judgment of a
	// node's memory (CommitterNodeBytes), for the same exposure: a full share of
	// maximal reviews would hold about a gibibyte decoded. Past it a write is
	// refused busy at once.
	ChangeWriterNodeBytes = 64 * MaxConversationBytes

	// MaxCheckWritesWaiting is the most writes of ONE repository's checks one
	// node's checks writer holds at once ([ChecksWriter]): FOUR CHECKS FILES'
	// WORTH (MaxChecksPerFile), 256. A repository's widest burst is a commit's
	// every check claimed, named and ended, three writes each, beside the next
	// commit's dispatches, and it drains in a fraction of a write's wait. Past it
	// a write is refused busy at once.
	MaxCheckWritesWaiting = 4 * MaxChecksPerFile

	// ChecksWriterNodeWrites is the most writes of repositories' checks one
	// node's checks writer holds at once, across every repository: 1 024, as
	// the change writer's (ChangeWriterNodeWrites) — four repositories'
	// widest bursts. It is the checks writer's share of the estate's proposal
	// lane, and past it a write is refused busy at once.
	ChecksWriterNodeWrites = 1_024

	// ChecksWriterNodeBytes is the most the writes one node's checks writer
	// holds may weigh together, each by the bytes its record carries: the
	// change writer's share of a node's memory (ChangeWriterNodeBytes) — some
	// seven hundred of a check's largest endings (its summary, its output's
	// tail and its failures at their bounds), while a claim or a dispatch of
	// a kilobyte or two meets ChecksWriterNodeWrites first.
	ChecksWriterNodeBytes = ChangeWriterNodeBytes

	// CommitterIdle is how long a repository's committer goroutine waits with
	// nothing queued before it exits. A goroutine costs a few kilobytes and a
	// restart microseconds, so it bounds only how many repositories a node keeps
	// one for, while a burst's pushes, seconds apart, keep theirs.
	CommitterIdle = 30 * time.Second

	// CommitBudget bounds ONE batch's publish and the read that answers it: HALF
	// the estate router's sixty-second write attempt. A committer must answer
	// within the one attempt its asker gives it, or the asker hands the push id
	// to the runner-up and two committers contend. So a push with a deadline
	// starts a batch only while a budget and ReplyMargin are left, else is
	// answered busy undecided; one without may queue under a budget; one whose
	// caller left is never decided. internal/estate refuses a router whose
	// attempt cannot hold both. The worst decide a founder's rules make costs
	// about 2.4 s over sixteen rounds, its verdicts formed once a batch and kept
	// while the rules stand (decide.go, BenchmarkDecideABatch); thirty seconds is
	// then a broker election with room, past which the publisher answers
	// unknown, naming the operation.
	CommitBudget = 30 * time.Second

	// ReplyMargin is what a serving node leaves between the end of each wait (a
	// committer's batch; a routed request's floor, barrier or record) and the
	// end of its asker's wait, counted from the request's arrival on its own
	// clock (signedask.Due), so running out of time is said while the asker
	// listens (statelog.WithReplyMargin, set by internal/estate). It covers the
	// request's trip there and the answer's back, milliseconds each, and no
	// clock's skew. ONE SECOND is two orders of magnitude over, and costs a
	// routed push one second of the twenty-odd it may queue.
	ReplyMargin = time.Second

	// GitShareWait is the longest a seat's git work waits for a unit of its
	// node's share ([GitShare]) before the node answers
	// [ErrGitUnready]{[UnreadyShare]} and the asker's walk moves on, rather than
	// queueing behind work that may take a whole read deadline. TWO SECONDS,
	// PROVISIONAL: max(2 s, the slowest warm read kind's p95 rounded up to a
	// second), under gitproc.ReadDeadline/4 (2.5 s). internal/gitlet/query's
	// BenchmarkCodeRead measured that p95 at 231 ms (a stat across 10 000
	// files); its 1 GB arm (CREWLET_BENCH_LARGE) can move it. Declared here,
	// beside [GitShare.Take], because every bound on a git read's attempt is
	// read from the package every party to it imports.
	GitShareWait = 2 * time.Second

	// MaxPackBytes is the largest OBJECT one push stores: its pack bundle
	// (bundle.go), the stored pack then its index and reverse index. FIVE
	// QUARTERS of config.CodeMaxPushBytesCeiling: receive.maxInputSize bounds the
	// input, but index-pack --fix-thin appends every base a thin pack names, and
	// the indexes add 2.5 %; a bundle past it is refused pack_too_large before
	// upload. Its put fits UploadDeadline (10 h 41 min) under
	// objstore.RecordWithin (12 h), and it stays BELOW 2 GiB as a RULE: past that
	// an index grows a 64-bit offset table, so with compaction's
	// --max-pack-size, CheckBundle's section sizes stay exact functions of the
	// object count (TestTheBoundsFitTheReadAndPurgeBudgets).
	MaxPackBytes = config.CodeMaxPushBytesCeiling + config.CodeMaxPushBytesCeiling/4
)

// The compaction's bounds: how many packs a repository may hold, which a
// compaction merges, and how large its record may be. A compaction is a
// record of its own on the repository's subject (OpCompact, [Compaction]),
// committed by the repository's committer and built by its home.
const (
	// MaxPacksPerRepo is the most LIVE packs a repository holds: a push that
	// would carry one more is answered compaction_behind, undecided (the push
	// decide's [working.behind], and the receiver before upload), until a
	// compaction merges its packs. 2 048 keeps a cold sync of every live pack at
	// FetchConcurrency and S3's slow 200 ms first byte within 27 s, half of
	// Gitlet's 55 s spool wait, the rest for the round's index and peel.
	// MEASURED by internal/gitlet/cache's BenchmarkColdSync
	// (bucket=fake/firstbyte=200ms/packs=2048/fetch=16): 26.4 s to download,
	// 26.6 s the round, 28.1 s with a published layer. Downloads past 27 s would
	// lower it; a round past the spool wait with its downloads inside is a
	// finding against MidxRoundPacks, never this.
	MaxPacksPerRepo = 2_048

	// CompactFactor is the geometric progression a home keeps a repository's
	// packs in: every pack at least twice the objects of all the lighter
	// ones together, git's own `repack --geometric=2` (gc's), so each object
	// is rewritten O(log n) times over a repository's life.
	CompactFactor = 2

	// CompactMinMerge is the fewest packs a compaction merges while the
	// repository holds no more than CompactTargetPacks. A merge costs a PUT and a
	// GET on every other Gitlet node holding the repository and saves later syncs
	// and lookups, so the floor decides what a QUIET repository's pushes cost.
	// SIXTEEN, MEASURED (internal/gitlet/compact's BenchmarkCompact,
	// steady/pushes=1200/rate=1/min, eight nodes): 527 GETs and PUTs a thousand
	// pushes against 1 081 from 8 and 2 212 from 4, for a cold sync of 20 live
	// packs at most rather than 13, with fewer bytes (713 MB against 873), rows
	// (1.05 retired a push against 1.13) and record bytes (108 a push against
	// 184). The largest floor measured (M3-O32's 4, 8, 16): a higher one is
	// bounded by CompactTargetPacks, which a quiet repository stays under only
	// while the floor leaves the progression room.
	CompactMinMerge = 16

	// CompactTargetPacks is the live packs a repository is held near: git's
	// gc.autoPackLimit, and the approved design's "about fifty". Past it the
	// maintainer compacts whatever its roll merges, two packs at the least, so
	// a repository past the target shrinks at every poll.
	CompactTargetPacks = 50

	// CompactPackBytes is the --max-pack-size every compaction output is cut
	// at: the largest pack a push may send (config.CodeMaxPushBytesCeiling,
	// 1 GiB), so a full pack is as large as any pack a push makes, and under
	// MaxPackBytes with the room a bundle's two indexes take beside it.
	CompactPackBytes = config.CodeMaxPushBytesCeiling

	// FullPackBytes is the pack section past which a pack is FULL, merged by no
	// later compaction ([PackRow.Full]). pack-objects closes an output before the
	// object that would pass --max-pack-size, so every output but a cut's last is
	// within an object of it: seven eighths marks those while objects are under
	// an eighth of the cut (max_blob_bytes' default is). Larger blobs cost only a
	// pack rewritten once more than needed, never a wrong one.
	FullPackBytes = CompactPackBytes / 8 * 7

	// FullBundleBytes is the BUNDLE size past which a pack is full, beside its
	// pack section. A dense roll's output, whose indexes (32 bytes an object)
	// make the builder scale its cut down, has a small pack section but a bundle
	// near CompactPackBytes; without this it would roll again at every poll.
	FullBundleBytes = FullPackBytes

	// MaxCompactInputs is the most packs one compaction merges. Its apply writes
	// two rows an input and one an output: 2 065 at the bounds
	// (compactDecidedRows), about half of MaxRecordRows, in a record of some
	// 52 KiB. A PUBLICATION's repack takes the same bound and is the op's widest,
	// 2 609 rows plus its chain and live-pack reads. MEASURED against
	// MaxPacksPerRepo packs (BenchmarkApplyACompaction): 62 ms at inputs=1024,
	// 68 ms for the widest publication, both under the third of
	// statelog.ApplyTxTimeBudget (83 ms) a compaction is held to (doc.go's
	// measured section); a measurement past it halves this, or the publication's
	// own bound. It bounds this build's committer ([CompactionProposal.Validate]);
	// the apply takes whatever a record carries.
	MaxCompactInputs = 1_024

	// MaxCompactOutputs is the most outputs one compaction writes, all uploaded
	// AT ONCE so each key, minted as its upload begins, lands within
	// objstore.ReadBase + objstore.PaceFor(MaxPackBytes), 10 h 41 min, under
	// objstore.RecordWithin (12 h); uploaded in turn, the later uploads would age
	// the first key past it. Sixteen PUTs hold 128 MiB of S3 part buffers (8 MiB
	// each). A proposal of more is refused ([CompactionProposal.Validate]), as
	// is one of more outputs than inputs, which would undo itself.
	MaxCompactOutputs = 16
)

// THE PUBLISHED LAYERS' BOUNDS: how long a repository's published chain may
// grow, and how its bytes are cut into stored objects. A layer is published as
// a compaction carrying it ([Compaction.Layer], [LayerRef]); these bound what
// such a record names and writes.
const (
	// MaxPublishedLayers is the most layers a repository's published chain
	// holds; a publication leaving more is never formed
	// ([CompactionProposal.Validate], and the decide's count). THIRTY-TWO, A
	// JUDGMENT: a geometric publisher (git's repack.midxSplitFactor) keeps about
	// log₂(history ÷ publication) layers, eleven to twenty-one for the largest
	// default repository, and past the bound merges the top whatever their sizes.
	// A deeper chain slows every lookup; CONFIRMED by internal/gitlet/cache's
	// BenchmarkPackLookup (layers={1,8,21,32,64}): 5.5 µs an id through one
	// layer, 5.8 through thirty-two, 6.2 through sixty-four, so thirty-two cost
	// 1.06 times one layer's, under the twice past which the bound would come
	// down.
	MaxPublishedLayers = 32

	// LayerPartBytes is the most bytes one stored object of a layer holds: a
	// layer is cut into parts of exactly this, the last at most it
	// ([LayerRef.Parts]). ONE GIBIBYTE (config.CodeMaxPushBytesCeiling): every
	// part uploads AT ONCE, its key minted as it begins, so each lands within
	// objstore.ReadBase + objstore.PaceFor(LayerPartBytes), about 8 h 33 min,
	// under objstore.RecordWithin (12 h), which internal/objstore/references
	// pins. The largest default repository's layer is some 2.6 GB, which one
	// object could not hold within that bound.
	LayerPartBytes = config.CodeMaxPushBytesCeiling

	// MaxLayerParts is the most parts one layer is cut into: 16 GiB, some 300
	// million objects, five times the largest default repository and more than
	// any Gitlet node this release sizes can build. Sixteen PUTs at once hold
	// 128 MiB of S3 part buffers, as MaxCompactOutputs bounds a compaction's.
	MaxLayerParts = 16
)

// compactDecidedRows is the most rows a compaction of n inputs into m outputs
// makes its apply touch, the bound the committer holds its decision to (as
// [withinRecord] a push batch's): each input's retired row and deletion, each
// output's row and the repository's row once; for a PUBLICATION (l its
// layer), also the layer's row and parts and every replaced layer's row with
// up to [MaxLayerParts] parts, which the proposal does not carry. At every
// bound, 2 609 rows, about two thirds of MaxRecordRows.
func compactDecidedRows(n, m int, l *LayerRef) int {
	rows := 2*n + m + 1
	if l != nil {
		rows += 1 + len(l.Parts) + len(l.Replaces)*(1+MaxLayerParts)
	}
	return rows
}

// THE EXPUNGE'S BOUNDS: how many packs one expunge rewrites, and how many it
// writes. An expunge is two records of their own on the repository's subject
// — its begin and its commit ([ExpungeBegin], [ExpungeCommit]) — decided by
// the repository's committer like a compaction, and built by the repository's
// maintainer.
const (
	// MaxExpungeInputs is the most packs one expunge's begin rewrites:
	// MaxCompactInputs, two rows an input as a compaction's, so a begin at every
	// bound is expungeBeginRows of them, 2 625, the widest record the code domain
	// forms, beside a publication's chain and recount reads. MEASURED as its own
	// shape (BenchmarkApplyACompaction's expunge/begin arm, every bound at once):
	// 75 ms, under the third of statelog.ApplyTxTimeBudget (83 ms) a compaction
	// is held to (doc.go's measured section); a measurement past it halves this.
	// It bounds this build's committer ([ExpungeProposal.Validate]); the apply
	// takes whatever a record carries.
	MaxExpungeInputs = MaxCompactInputs

	// MaxExpungeOutputs is the most packs one expunge's begin writes, kept and
	// dropping together, and the most its commit's rescue writes. THIRTY-TWO:
	// config.DefaultCodeRepoMaxBytes over CompactPackBytes, twenty, rounded up to
	// a power of two, so a repository at the default ceiling is rewritten in a
	// cycle or two. Every output uploads AT ONCE, as [MaxCompactOutputs] do, so the
	// last lands within objstore.RecordWithin of the first; thirty-two PUTs hold
	// 256 MiB of S3 part buffers on the maintainer.
	MaxExpungeOutputs = 32

	// MaxImportRefs is the most refs one import chunk sets and one mirror sync
	// moves (M6-D19's thousand): MaxCommandsPerPush, at most three rows each
	// ([commandRows]), so with MaxImportPacks packs a chunk stays under
	// MaxRecordRows. It bounds ROWS; long-spelled refs reach EntryRoom first, and
	// a home cuts each record by both (ImportProposal.Fitted,
	// MirrorSyncProposal.Fitted).
	MaxImportRefs = MaxCommandsPerPush

	// MaxImportPacks is the most packs one import chunk or mirror sync lands:
	// MaxExpungeOutputs, for its reason (simultaneous uploads within
	// objstore.RecordWithin, 256 MiB of S3 part buffers on the home).
	MaxImportPacks = MaxExpungeOutputs

	// MirrorInterval is how often a home syncs a PULL MIRROR from its source
	// (internal/gitlet/mirror): FIFTEEN MINUTES, the refresh P8's decisions
	// promise (M6-D16), a constant of its own rather than the retention's, which
	// answers another question. `mirrors_due` lists a mirror once its last sync
	// is this old. A sync finding its source level is a record too
	// ([MirrorSync.Current]), under a kilobyte, so the listing can stop at the
	// first recently synced mirror and a home that forgot an attempt does not
	// refetch every quiet mirror at once.
	MirrorInterval = 15 * time.Minute
)

// expungeBeginRows is the most rows a begin of n inputs into m outputs (kept
// and dropping together), dropping layers published layers, makes its apply
// touch, the bound the committer holds it to (as [withinRecord]): each
// input's retired row and deletion, each output's row, each dropped layer's
// row with up to [MaxLayerParts] parts, which the proposal does not carry,
// and the repository's row once. At every bound, 2 625 rows.
func expungeBeginRows(n, m, layers int) int { return 2*n + m + layers*(1+MaxLayerParts) + 1 }

// expungeCommitRows is the most rows a commit retiring n dropping packs and
// writing m rescue packs makes its record's apply touch: each dropping pack's
// retired row and its deletion, each rescue pack's row, and the repository's
// row — 97 at every bound, since a repository holds no more dropping packs than
// one begin wrote.
func expungeCommitRows(n, m int) int { return 2*n + m + 1 }

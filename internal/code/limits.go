package code

import (
	"math/bits"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/usage"
)

// THE DOMAIN'S BOUNDS, each with the reason it is the value it is. The
// constants a configuration document is also validated against are config's
// (internal/config/code.go) and named again here as aliases — one value, never
// a copy — because code imports config and the reverse cannot hold.

// Limits are the epoch's code.limits as one value, with the defaults applied:
// what a push is held to. They cross the estate wire in [RepoDetail], which is
// what an import chunks its pushes to.
type Limits struct {
	MaxPushBytes int64 `json:"max_push_bytes"`
	MaxBlobBytes int64 `json:"max_blob_bytes"`
	RepoMaxBytes int64 `json:"repo_max_bytes"`
	// PushesPerSeatDay is how many pushes one SEAT may land in one company
	// day, every repository together (code.limits.pushes_per_seat_day): a
	// runaway bound on what a seat costs the code log, which the push decide
	// holds every seat's push to ([RefusalPushQuota]) and no person's.
	PushesPerSeatDay int `json:"pushes_per_seat_day"`
}

// DefaultLimits are the limits of a company that sets none.
func DefaultLimits() Limits { return LimitsOf(config.Code{}) }

// LimitsOf is a company's code.limits with the defaults applied — what a push
// is held to on the deciding node's epoch. A CONVERSION of config's own
// reading ([config.CodeLimits.Resolved]), never a second resolution of the
// defaults, so `crewlet validate`'s order rules and the committer's decide
// read one document one way (TestLimitsOfIsConfigsReading).
func LimitsOf(c config.Code) Limits {
	r := c.Limits.Resolved()
	return Limits{MaxPushBytes: r.MaxPushBytes, MaxBlobBytes: r.MaxBlobBytes, RepoMaxBytes: r.RepoMaxBytes,
		PushesPerSeatDay: r.PushesPerSeatDay}
}

// OutsideSeatShare divides what a seat OUTSIDE a repository's owning unit may
// fill of it — a seat whose relation to the repository is below write, which
// pushes only into its own namespace there ([SeatNamespace]): the
// repository's live refs ([MaxRefsPerRepo]) and its packed bytes
// (code.limits.repo_max_bytes), each to 1/OutsideSeatShare, judged on the
// repository's whole counts. Every full_access seat owns a namespace on every
// repository it may read, and every object a ref reaches stays in the
// repository's packs — its own branches' among them — so without a share one
// seat from another unit could fill a repository it merely reads — its refs
// in a hundred pushes of MaxCommandsPerPush creates, its bytes in a score of
// the largest pushes — and refuse its owners every push there until somebody
// removed what it pushed.
//
// TWO — HALF — is the engine's rule for an untrusted class beside a trusted
// one (a git node's seat git work holds half its read lane, [GitShare]): the
// owners always keep half, and the outside seats share the other half among
// themselves. A JUDGMENT CALL, taken as the product's: half, and no per-seat
// bound within it — one outside seat can take the outside half from the
// others, which a per-seat count would stop at the cost of a counter table
// this build does not keep. A quarter would keep owners more and turn
// cross-unit pushes away sooner.
const OutsideSeatShare = 2

// BarrierRecordBytes is what one code-log barrier costs its stream, as the
// broker counts it against the log's ceiling: the encoded record
// ([EncodeBarrier]) in the signed frame every record is appended in
// (ADR-0031), its subject and the broker's per-message overhead.
//
// 176 BYTES, MEASURED: a barrier at the first generation, whose envelope
// carries no generation, costs 156; one at the last costs 173, since a
// generation printed whole is up to ten digits and its key. So 176 bounds every
// barrier the log can hold, overstates the commonest by an eighth — noise
// beside census_drift's factor of two
// (TestABarrierCostsTheCodeLogWhatTheCensusSays holds both arms between half
// the figure and all of it) — and is exactly what a mutation-log barrier
// costs from that log's second generation to its tenth, so the storage
// forecast prices both logs' barriers at one figure.
//
// FORTY OF THOSE BYTES ARE THE FRAME: its magic, its version, the key id's
// length, the key id and a 32-byte MAC. It was 136 before every record was
// signed, which now understates a barrier by 15 to 27 %, and a census priced
// at it would let the git clients' barriers past the tenth they are allowed
// before census_drift said anything. The frame carries the SIGNING KEY'S ID,
// measured here at the suites' two bytes, so a deployment whose active key has
// a longer id pays a byte a character more on every record — three for
// `crewlet secrets keygen`'s `key-1`, five for the examples' `2026-01`: under
// 3 % of a barrier, which census_drift's factor of two does not notice.
const BarrierRecordBytes = 176

// GitReadsShare is how much of the code log's byte ceiling the census lets its
// git clients' barriers hold over the trim's min_age: one part in TEN.
//
// AN ALLOWANCE, NOT THE SPLIT THE CEILING WAS SIZED BY. The ceiling's own
// ratio (config's DerivedCodeLogNumerator, which [CodeLogMaxBytes] below is
// spelled from) already counts the reference company's git barriers beside its
// pushes: 2.4–5.0 MB a day, 17–35 MB over the default seven days — under a
// thirtieth of the tenth of a derived 10⅔ GiB ceiling, and about a third of
// the tenth of even the 1 GiB floor. So the tenth is not what git was expected
// to take: it is room that git traffic well past that estimate grows into, and
// census_drift, at twice the census, says when even that is outgrown and every
// sizing decision under the log is stale. A JUDGEMENT: smaller fires
// census_drift on a busy but healthy git host, larger lets barriers crowd the
// pushes, settings and purges the log is there for before anybody is told; the
// census is a share of the ceiling the log's stream HOLDS, so a company that
// outgrows the tenth resizes the log (`crewlet retention set-capacity`) and the
// census moves with it.
const GitReadsShare = 10

// GitReadsPerDay is the linearizable git reads a day the code log's census
// allows: the census term of its git clients (statelog.Census.Clients) — the
// barriers that fill 1/GitReadsShare of ceiling over the trim's min_age.
//
// THIS TERM IS THE READERS NO SEAT COUNT PREDICTS: a person's or their CI's —
// an advertisement, a push's sync, a fetch whose want a cache lacks, a `/code/`
// read at that level. Held to the census's 125 a day per agent seat, a
// five-seat company's code log alarmed past 1 300 git reads a day, which one
// CI polling ten repositories a minute exceeds tenfold. A SEAT's reads of the
// code log — the refs a call names resolved (the estate's code.resolve_ref,
// one a call however many it names) and a read by object id's one re-sync at
// the head, when its home's copy lacks an id it names (code.git_read) — are
// the seats' term instead, which the census gives
// every log whole, so this term does not count them. At the 1 GiB floor and the
// default seven days it is 87 154 a day — about 60 a minute fleet-wide: that
// CI, thirty developers fetching every three minutes and 3 000 pushes a day,
// three times over.
//
// IT FOLLOWS THE CEILING THE STREAM HOLDS, which the caller reads off the
// broker: the one every node reads alike, and the one a resize moves. Never
// the Tier A `stream.code_log_max_bytes`, which only sizes a log not yet
// created — raised under a log that exists, it would raise the census over a
// log that holds no more than it did, which is the alarm silenced rather than
// the deployment resized.
//
// OVER THE WHOLE min_age, a fraction of a day included: a barrier stays on the
// log for all of it, so 36 hours divided as one day would let the barriers
// fill half again the tenth they were sized for. A min_age under a day counts
// as one. The arithmetic is EXACT, in 128 bits (bits.Mul64, then bits.Div64):
// a day is 8.64e13 nanoseconds, so the ceiling times a day overflows 64 bits
// once the ceiling passes 2^64 ÷ 8.64e13 bytes, about 208 KiB — which every
// ceiling a log can have does, the 1 GiB floor included — and a ceiling read
// off the broker is whatever the stream declares.
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
	// CodeLogMaxBytes is the ceiling this domain declares for its log, which
	// is what the framework's own suites provision the stream with. A node
	// creates the stream at the ceiling its own Tier A sizes
	// (stream.code_log_max_bytes), as every domain's.
	//
	// TWO THIRDS OF THE TRACKER'S sixteen gibibytes, 10⅔ GiB: the ceiling
	// an unset stream.code_log_max_bytes derives beside a mutation log of
	// the tracker's declared size (TestTheCodeLogIsDeclaredAtTierAsRatioToTheTrackers),
	// so the log the suites provision and the log a node creates are sized
	// by one rule — config's DerivedCodeLogNumerator, whose arithmetic is
	// what this log takes a day at the reference company once seats push,
	// each push priced as a coding box's, the most a seat's push takes:
	// 3 000 pushes at the MEASURED 1 555 bytes a seat's coding box's push
	// alone in its record (BenchmarkPushLogBytes), 4.7 MB, the compactions
	// that merge their packs and the sweeps of what those retired at the
	// MEASURED 108 and 44 bytes a push (internal/code/compact's steady arm
	// at the floor), 0.5 MB, and its git clients' barriers at BarrierRecordBytes,
	// 2.4–5.0 MB, against the tracker's measured 9.9 MB of records and
	// 2.2 MB of barriers, every record in its signed frame.
	// It was the tracker's whole sixteen, on an estimate of 2 KiB a push and
	// no barriers, which put the two logs on "the same order" rather than at
	// the ratio a blocked trim fills both in the same time.
	CodeLogMaxBytes = (16 << 30) * config.DerivedCodeLogNumerator / config.DerivedCodeLogDenominator

	// CodeLogDuplicates is the window the broker collapses a repeated
	// operation id in: the tracker's and the knowledge base's two minutes.
	// An optimisation, never the mechanism — the operation ledger and a
	// push's own id are what make a retry idempotent.
	CodeLogDuplicates = 2 * time.Minute

	// MaxPushesPerRecord is the most pushes one record carries — the bound
	// on the work one lost compare-and-set round re-decides (64 pushes at a
	// few indexed reads each), and the per-record ceiling the tracker's bulk
	// writes use. Group commit's gain is in bursts — a CI fan-out of tag
	// pushes — where it turns 64 appends into one. SIXTY-FOUR for what such
	// a record holds the write lane for: applied and committed, 10–12 ms,
	// 0.16–0.18 ms a push against 3.4–4.8 ms for one push on its own, far
	// inside statelog.ApplyTxTimeBudget (doc.go's measured section).
	// MEASURED end to end on three members (doc.go's "What a push costs,
	// measured"): a burst past the bound fills its records to sixty-four,
	// and its rate still rises with its size — 2 050–2 090 pushes a second
	// at sixty-four at once, 2 720–2 830 at 256 and 3 010–3 090 at 1 024 —
	// against 86–89 for a lone push. What records WIDER than sixty-four
	// would buy a burst is NOT measured, since the bound is what forms them;
	// it is bounded by the write lane, not by the round trips they would
	// save — every node applies every record on its one lane, and a record's
	// hold grows with its width, so a wider one trades round trips for a
	// longer hold every other write of the log waits behind.
	MaxPushesPerRecord = 64

	// CodeLogMaxRecordBytes is the largest record the code log admits
	// ([statelog.StreamSpec.MaxRecordBytes]): THE LARGEST THE TRANSPORT
	// CARRIES ([statelog.MaxTransportRecordBytes]), as the tracker's is.
	//
	// The declaration sizes two things. The node's ordinary appends in flight
	// on the log are one append of it ([statelog.StreamSpec.MaxAppendBytes],
	// [statelog.NewReserve]), which is what lets this node's committers hold
	// eight records of [MaxRecordBytes] out at once (below). And the gate
	// reserve's fleet term is seven peers' such appends; at the code log's
	// gibibyte Tier A floor a sixteenth of the ceiling is the larger term
	// (64 MiB against 57), so the reserve is what any declaration would give
	// it. What bounds a record THIS build forms is [MaxRecordBytes]; a newer
	// build's may be as large as this, which the apply's own bounds already
	// assume of a record it did not form ([Applier]'s tombstone backstop).
	CodeLogMaxRecordBytes = statelog.MaxTransportRecordBytes

	// MaxRecordBytes is the largest record this domain's committer forms: an
	// EIGHTH of the node's appends in flight on the code log, each append
	// counted with what a stored record carries beside its payload, so eight
	// maximal code records are out at once on one node and a committer
	// serving many repositories is never serialised behind one large record.
	// 1 044 480 bytes.
	MaxRecordBytes = (CodeLogMaxRecordBytes+appendOverheadBytes)/8 - appendOverheadBytes

	// appendOverheadBytes is what the log's reserve counts one append at
	// beyond its payload — the signature frame, the store's framing, the
	// subject and headers: the transport's largest message less the largest
	// record it carries, which is how statelog defines the latter, so the
	// two readings cannot disagree (TestTheCodeDomainsDeclaration holds
	// eight maximal records to the log's own [statelog.StreamSpec.MaxAppendBytes]).
	appendOverheadBytes = queue.MaxPayloadBytes - statelog.MaxTransportRecordBytes

	// MaxRecordRows is the most rows one record's apply may write or delete:
	// the framework's per-transaction budget, which is checked only at record
	// boundaries — a larger record would hold the shared write lane past it.
	// A push record's tombstone sweep, and then its removal of the ref drops
	// past PruneGrace, fill what the record's own rows leave of it, so a push
	// record reaches it exactly whenever enough of either is due —
	// tombstones past RefTombstoneHorizon or past MaxTombstonesPerRepo,
	// drops past PruneGrace.
	MaxRecordRows = statelog.ApplyTxRowBudget

	// MaxCommandsPerPush is the most ref updates one push carries. A push's
	// apply then writes at most 2 003 rows — its refs, the object each forced
	// update or deletion left (code_ref_drops), its pack, its push row and the
	// counters — and its record's row bound counts each command by the rows
	// it may make the record touch (rowBound: a deletion three, for its
	// tombstone, the old tombstone the sweep may take for it and its drop; a
	// forced update two; any other one), so three maximal pushes of
	// fast-forward updates fit one record's row budget, one of forced updates
	// or of deletions, and two of either its byte budget (about 410 KiB each
	// with plain refs at MaxRefNameBytes); an import chunks its refs at this
	// size.
	// Refs JSON spells long — `&`, `<` and `>` take six bytes each — reach
	// EntryRoom first, at some six hundred of the longest, and such a push
	// is refused by its receiver before its work (Proposal.ValidateCommands).
	MaxCommandsPerPush = 1_000

	// MaxRefsPerRepo is the most LIVE refs a repository holds. A protocol-v0
	// advertisement costs about 100 bytes a ref, so this is about 10 MB on
	// every v0 fetch and a 10 MB packed-refs rewrite on every cache sync;
	// past it a repository is a namespace problem, refused (repo_refs)
	// rather than slowly degraded. At the bound a push is still about two
	// seconds as a person waits for it — measured over a gigabyte of
	// history, its request 303 ms against 59–63 at one ref, another node's
	// delta sync 42 ms, the advertisement 136 ms (internal/code/host's
	// measured section). A
	// deleted ref never counts against it: its tombstone is bounded apart, by
	// MaxTombstonesPerRepo, and refuses nobody.
	MaxRefsPerRepo = 100_000

	// MaxTombstonesPerRepo is the most TOMBSTONES — refs deleted within
	// RefTombstoneHorizon, kept so a cache syncing a delta learns of each
	// deletion — a repository's refs table holds beside its live refs. Past
	// it the applier forgets the oldest, and a record whose deletions its
	// sweep cannot make room for removes those refs' rows outright
	// (apply_push.go); a deletion is never refused for it.
	//
	// MaxRefsPerRepo, a hundred thousand, because:
	//   - the reader gains no new worst case in ROWS whatever the tombstones
	//     hold: it answers a delta only while the delta carries no more rows
	//     than the full answer would (reader.go's deltaOutweighs) — a delta
	//     reads every ref row moved above its floor, tombstones included, and
	//     a full answer the live refs and every pack — so a cache synced
	//     before a clean-up larger than what the repository keeps reads what
	//     is left rather than every deletion;
	//   - in BYTES, every row at its widest, a delta passes the full answer of
	//     a repository of as many refs and packs only by tombstones carried in
	//     place of packs — its live refs and packs are rows that answer
	//     carries too, and a live ref is wider than its own tombstone — so by
	//     at most a tombstone less a pack row a pack, for as many packs as
	//     this bound has tombstones: nothing for names JSON writes as
	//     themselves, whose tombstone at MaxRefNameBytes (347 bytes) is
	//     narrower than any pack row (459 at the narrowest), and 1 108 bytes
	//     a pack for names JSON spells longest (a tombstone of 1 567), 22 MB
	//     beside the 171 MB a full repository of 20 000 packs answers whole at
	//     those names
	//     (TestADeltaPassesTheWidestFullAnswerOnlyByTombstonesInPlaceOfPacks);
	//   - a clean-up of a full repository — every live ref deleted at once —
	//     keeps its whole 48-hour horizon;
	//   - the large company's some 40 000 changes a day, every one landing on
	//     ONE repository with its branch deleted on merge, make about 80 000
	//     tombstones in 48 hours, inside it.
	//
	// So a repository's refs table holds at most MaxRefsPerRepo +
	// MaxTombstonesPerRepo rows, exactly, after every record.
	MaxTombstonesPerRepo = MaxRefsPerRepo

	// MaxIntroduced is the most introduced commit ids a push row lists (43
	// bytes each); IntroducedCount carries the rest.
	MaxIntroduced = 1_000

	// MaxProtectionsPerRepo is config.CodeMaxProtections: every matching
	// rule is asked about every command in the decide.
	MaxProtectionsPerRepo = config.CodeMaxProtections

	// MaxPathOwners is config.CodeMaxPathOwners: the path owners one
	// repository's settings name, each one row of code_path_owners — so the
	// final purge deletes them beside its rules, as it deletes those, inside
	// MaxRecordRows (the limits test holds the sum), and a settings record
	// replacing them all touches twice this many rows.
	MaxPathOwners = config.CodeMaxPathOwners

	// MaxPathOwnerPaths is config.CodeMaxPathOwnerPaths: the globs one path
	// owner names, packed into its one row.
	MaxPathOwnerPaths = config.CodeMaxPathOwnerPaths

	// MaxSnapshotItems is the most rows one snapshot or listing page
	// carries: about 1–2 MB of JSON for typical refs and about 4 MB at
	// worst (every ref at MaxRefNameBytes), under the estate reply's
	// ceiling with room; MaxRefsPerRepo refs is ten pages of rows, and more
	// where MaxPageBytes ends a page first (names JSON spells long).
	MaxSnapshotItems = 10_000

	// MaxPageBytes is the encoded rows one page may carry WHATEVER its row
	// limit says: half the estate reply ceiling (queue.MaxPayloadBytes),
	// leaving the envelope and the repository row room. A push row carries
	// up to MaxCommandsPerPush commands, so a row count alone would put two
	// hundred maximal pushes — 88 MB — in one reply. A page always serves
	// at least one row, so it always advances.
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

	// RefTombstoneHorizon is how long a deleted ref is kept as a TOMBSTONE —
	// a row whose oid is empty — so a cache syncing a delta learns of the
	// deletion: 48 hours, or sooner — oldest first — once the repository
	// holds MaxTombstonesPerRepo. A record whose deletions its sweep cannot
	// make room for removes those refs' rows outright. A cache synced before
	// a swept or removed deletion takes a full answer
	// (code_repos.tombstones_through), which re-reads the rows and reuses
	// every pack the cache holds — so the horizon trades tombstone rows kept
	// against how often an idle-but-warm cache pays a full listing, never a
	// download. Two days covers a cache fetched on any working day across a
	// one-day gap.
	RefTombstoneHorizon = 48 * time.Hour

	// PruneGrace is how long a ref's DROP is kept — the object a forced
	// update or a deletion moved a ref off, a code_ref_drops row — and so how
	// long what only that object reached still counts as reachable to
	// whatever drops a repository's unreachable objects: history force-pushed
	// away or deleted stays fetchable by its id for that long after the drop.
	//
	// FOURTEEN DAYS, git's own gc.pruneExpire default (2.weeks.ago): the
	// grace anybody who runs git expects a repository's unreachable objects
	// to have before they may go, so what a mistaken force-push left is
	// recoverable from the server for as long as git itself keeps it in a
	// clone. Every push record removes its own repository's drops older than
	// this by its broker instant (apply_push.go). A reader judging the grace
	// by its own clock stands at most twice objstore.AssumedClockSkew from
	// that instant while no node raises `clock_skew`: two hours either way
	// at the edge of fourteen days — a policy's edge, where RetireGrace's
	// margin guards a read in flight.
	PruneGrace = 14 * 24 * time.Hour

	// MaxDescriptionBytes is config.CodeMaxDescriptionBytes: a repository
	// description is one paragraph, riding every settings record and every
	// listing row.
	MaxDescriptionBytes = config.CodeMaxDescriptionBytes

	// MaxChangeBodyBytes is the longest body — a change's description — a
	// change carries: as long as a task's body in the engine's own tracker,
	// the text a change most often carries over from the work it is for.
	// The one value of a change that outgrows a page, so it is a row of its
	// own (code_change_bodies, replicated 0066) rather than a column of the
	// row every head move reads and rewrites, weighed by its change's
	// body_bytes, and a purge part counts it toward PurgePartBytes.
	MaxChangeBodyBytes = 32 << 10

	// MaxChangeTitleBytes is the longest title a change carries: ONE LINE a
	// listing shows whole, as long again as a commit subject git's own
	// tools wrap at, and short enough beside the refs, the oids and the
	// handles that code_changes stays inside its page (replicated 0066),
	// which every head move reads and rewrites.
	MaxChangeTitleBytes = 256

	// MaxCloseReasonBytes is the longest reason a close of a change gives —
	// a sentence or two of why, kept on the change's row beside its title,
	// which replicated 0066 sizes the row by as it does by the title.
	MaxCloseReasonBytes = 1 << 10

	// MaxReviewersPerChange is the most reviewers a change asks at once: the
	// tracker's own bound on the people one item mentions (its MaxMentions),
	// because a request is a mention that waits for an answer — and the
	// readers who weigh a change's readiness read each requested reviewer's
	// latest verdict, so the set they read is bounded by it too. A reviewer
	// withdrawn no longer counts against it.
	MaxReviewersPerChange = 32

	// MaxReviewNoteBytes is the longest note a request for review carries —
	// a sentence or two of what to look at, kept on each requested
	// reviewer's narrow row as a change's close reason is on its own — and
	// the longest an answer to a finding carries, kept on the finding's own
	// narrow row for the same reason.
	MaxReviewNoteBytes = 1 << 10

	// MaxCommentBytes is the longest comment on a change: as long as the
	// change's own description (MaxChangeBodyBytes) and a comment on a task
	// in the engine's own tracker, the text a review's discussion most often
	// quotes from. Weighed into its conversation's text_bytes, which a purge
	// part counts toward PurgePartBytes as it does a change's body.
	MaxCommentBytes = 32 << 10

	// MaxReviewSummaryBytes is the longest summary a review gives — a
	// paragraph or two of what the reviewer found, kept on the review's row
	// beside its verdict and weighed into its conversation's text_bytes.
	MaxReviewSummaryBytes = 4 << 10

	// MaxFindingsPerReview is the most findings one review raises: what one
	// careful pass over a change says line by line, and what keeps a review
	// with every finding at its bounds inside one record beside the room the
	// record keeps ([MaxConversationBytes] — a review over it is refused,
	// "split into two reviews of the same revision", which supersede
	// nothing of each other).
	MaxFindingsPerReview = 64

	// MaxFindingTitleBytes, MaxFindingBodyBytes and MaxSuggestionBytes bound a
	// finding's text: a title one line a listing shows whole, a body a
	// paragraph or two of why, and a suggestion the lines it would put in
	// place of the ones it is anchored to — every one weighed into its
	// conversation's text_bytes and kept on a row of its own
	// (code_finding_texts), off the narrow row a finding's resolution and a
	// later review's supersession rewrite.
	MaxFindingTitleBytes = 200
	MaxFindingBodyBytes  = 4 << 10
	MaxSuggestionBytes   = 16 << 10

	// MaxStandingFindings is the most findings one reviewer keeps STANDING —
	// open, resolved, won't fix or disputed — on one change: sixteen
	// reviews' worth at [MaxFindingsPerReview], which a reviewer reaches only
	// by reviewing one revision again and again, since a review of a later
	// revision supersedes every standing finding of its reviewer it does
	// not carry. Past it a review is refused too_many_findings. It bounds
	// what one review's record rewrites when it supersedes them — 1 024
	// narrow rows beside its own 64 findings and their texts, a third of
	// [MaxRecordRows] — so a review is always one record however long a
	// reviewer has followed a change.
	MaxStandingFindings = 1 << 10

	// MaxRoutingBytes is the room every record of a change keeps for the
	// snapshot of whom the change concerns — its owner, authors, reviewers,
	// path owners, participants, mentions and work item: about two hundred
	// handles at seventy bytes, the work item and the title — so the text a
	// write carries never takes the room its record needs for the wakes it
	// causes.
	MaxRoutingBytes = 64 << 10

	// MaxConversationBytes is the most one write of a change's conversation
	// may encode to: a record's bytes ([MaxRecordBytes]) less the routing's
	// room and four kibibytes of the record's own envelope. A review of
	// [MaxFindingsPerReview] findings with suggestions at their bound can
	// pass it, and is refused, "split into two reviews of the same
	// revision".
	MaxConversationBytes = MaxRecordBytes - MaxRoutingBytes - 4<<10

	// HorizonDeletesPerRecord is how many expired PUSH ROWS one push record
	// removes from its repository: no fewer than the push rows a record can
	// add (MaxPushesPerRecord), so the push history's removal keeps pace on
	// an active repository, and a fixed share, so the committer can count it
	// in a record's rows before the record exists. HorizonBytesPerRecord
	// bounds the same removal by what the rows weigh.
	//
	// It does NOT bound the TOMBSTONES a record sweeps. A record can make
	// over a thousand of those, which a share of sixty-four fell behind
	// by about 936 a record on a repository that deletes refs in bulk; the
	// sweep takes every row the record leaves of MaxRecordRows instead —
	// 3 995 behind a one-command push, at least one for each of its own
	// deletions behind any record the committer forms (rowBound reserves
	// one for every deletion), and none behind one it filled to the row
	// bound with creations — and the ref drops past PruneGrace whatever the
	// tombstones leave, so what a record writes and removes together is
	// MaxRecordRows at most.
	HorizonDeletesPerRecord = 64

	// HorizonBytesPerRecord is how many BYTES of expired push rows one push
	// record removes beside HorizonDeletesPerRecord's count — each row
	// weighed by its row_bytes, the commands, introduced ids and seat's run
	// it carries — taking rows oldest first until their bytes reach it, so a
	// removal passes it by at most one row.
	//
	// MaxRecordBytes, the most push-row bytes one record can ADD: a row's
	// commands, introduced ids and run are its record's own, stored with
	// less (no refusal sentence, the ids space-separated rather than quoted,
	// each run field its bytes with no key and no escape). A
	// removal that stops at either bound has therefore taken at least the
	// oldest record's whole contribution — its rows share one instant and
	// one position, so they are first in the order and together — and the
	// history's removal keeps pace with its growth in rows and in bytes
	// alike.
	//
	// NO MORE, because this is the push history, the widest rows the domain
	// keeps (a push row reaches 461 000 bytes), and deleting a row frees its
	// overflow pages: about 2.4 ms a megabyte measured, which a count of
	// rows does not see. Bounded by HorizonDeletesPerRecord alone, a record
	// removed sixty-four maximal rows — 29 MB, 60–82 ms more of the write
	// lane on every node inside the largest push record; bounded here it
	// removes three, 1.4 MB, and the largest record with them is no slower
	// than without (doc.go's measured section).
	HorizonBytesPerRecord = MaxRecordBytes

	// PurgePartRows is how many rows one purge step deletes: under
	// MaxRecordRows with room for the record's own bookkeeping — and for
	// the final purge's, which also removes every protection rule
	// (MaxProtectionsPerRepo), the name claim, the repository's row and
	// writes the marker — which the limits test pins. PurgePartBytes bounds
	// a part's wide rows — its push rows and its changes' bodies — by what
	// they weigh as well.
	PurgePartRows = 3_000

	// PurgePartBytes is how many BYTES of wide rows one purge step deletes
	// beside PurgePartRows' count, in one budget: its push rows first, each
	// weighed by its row_bytes and taken oldest first until their bytes
	// reach it — always at least one, and at most one row past it — and
	// then its changes' bodies, each weighed by its change's body_bytes, a
	// change taken whole only while the part weighs under it: the first
	// whenever the pushes left room, and at most one past it. A part whose
	// pushes were cut by it deletes nothing else, so the refs and the packs
	// wait for a part of their own, and a part whose wide rows reached it
	// takes no change; and the final purge, whose statements are unbounded,
	// is formed only once the pushes and the bodies left fit it together as
	// well as the rows do.
	//
	// A ROW COUNT BOUNDS A PART'S TIME ONLY FOR NARROW ROWS. The domain keeps
	// two wide ones — a push row, up to 461 000 bytes of commands and
	// introduced ids, and a change's body, up to MaxChangeBodyBytes — and
	// deleting either frees its overflow pages: measured, a part of 3 000
	// push rows of a kilobyte holds the write lane 56–60 ms, and bounded by
	// rows alone one of 1 000 rows of 460 KB held it 1.05–1.10 s, about
	// 2.4 ms a megabyte — four times statelog.ApplyTxTimeBudget in a record
	// the budget cannot split, on every data node, with every other domain's
	// applier waiting behind it. Bodies weighed by their rows alone took a
	// part some 32 MB past the bound, on top of push rows just short of it:
	// 112 ms against 64 ms for the same rows without bodies, in the run of
	// the review that weighed them. THIRTY-TWO MEBIBYTES is a third of the
	// budget: 81–88 ms for a part of the widest rows (seventy-three of
	// them), and 76–103 ms for the most a part can take of push rows and
	// narrow refs, rows just short of it and refs for the rest of
	// PurgePartRows; and the most it can take of push rows and bodies, both
	// bounds at once, held the lane 53 ms in a run where that rows-and-refs
	// part held it 56 ms (doc.go's measured section).
	PurgePartBytes = 32 << 20

	// RetentionInterval is the code-retention pass's tick: a quarter of an
	// hour, the cadence of the fleet's other housekeeping duties (the trim,
	// the maintenance sweep). A purge is days late by design — it runs
	// deleted_retention_days after a removal — so the tick only bounds how
	// long a purgeable repository waits, and fifteen minutes against seven
	// days is noise. Its own constant rather than the maintenance package's,
	// which this domain does not import: that package reaches the sandbox,
	// the scheduler and the agent ledger, and the sandbox will read this
	// package's types.
	RetentionInterval = 15 * time.Minute

	// RetentionLaneShare is the most of the replicated estate's one write
	// lane a code-retention pass takes: after each step — a sweep's of
	// retired packs or a purge's, one pacer over both — the pass waits
	// (1 − share) / share times as long as that step took — twice, at a
	// third — before the next. A step is decided, appended and APPLIED on the
	// deciding node before it answers, so its duration is its own apply plus
	// whatever the lane applied ahead of it, and a busy lane slows the pass
	// by itself.
	//
	// A THIRD, because every data node applies every record and the holder
	// sees only its own lane: closed-loop, a pass held the holder's lane
	// about 82% of the time (68 ms of every 83 ms step, over parts of
	// PurgePartRows narrow rows), and any node applying the parts even 1.2
	// times slower than the holder queued a growing backlog in front of its
	// own pushes. At a third a node up to three times slower than the holder
	// keeps up with no queue at all, and the other two thirds are left for
	// the pushes, settings and reads the lane is for — the share
	// PurgePartBytes already sizes a part to: a third of
	// statelog.ApplyTxTimeBudget. Measured on the whole path, a paced pass's
	// records held 33–37% of its wall clock over five steps (five of the
	// thirteen step-lengths five steps and their four waits take, a third
	// over a longer pass), against all of it unpaced
	// (TestAPassHoldsAtMostAThirdOfTheWriteLane). A sweep's step holds the
	// other nodes' lanes for less than its third, because most of a small one
	// is what only the holder pays: of the 4.9 ms a sweep of eight rows takes
	// on the whole path (BenchmarkRetireSweepStep), its apply is 1.5 ms
	// (BenchmarkApplyARetireSweep) and the rest the decide's read and the
	// append's round trip.
	RetentionLaneShare = 1.0 / 3

	// RetentionPassBudget is how long one code-retention pass takes steps for
	// before it stops, leaving what is due to the next pass, a
	// RetentionInterval later: ONE BUDGET, and one pacer, over both kinds of
	// step — the sweeps of retired packs past RetireGrace first, then the
	// purges with whatever the sweeps leave of it. Running out is the pass's
	// designed stop.
	//
	// statelog.StallGrace, A MINUTE, because a node that applies the pass's
	// records more than 1/RetentionLaneShare times slower than the holder
	// still queues, (k·share − 1) of every second the pass runs: bounded to a
	// minute, a node up to six times slower ends a pass at most StallGrace
	// behind — where a push it commits starts to be answered pending rather
	// than ok — and catches up within the quarter hour before the next; a
	// node slower still is one the lag alarm already names.
	//
	// What it costs is the drain, out of twenty seconds of steps a pass (a
	// minute at a third). THE SWEEPS TAKE THEIRS FIRST, AND LITTLE: a pass
	// steps each repository holding rows past the grace once — again only
	// for every RetireSweepRows rows past the first step's — and a step of a
	// few rows takes 4.9 ms on the whole path (BenchmarkRetireSweepStep,
	// doc.go's measured section), about a seventeenth of the measured 83 ms
	// purge part, so a pass that sweeps S repositories still has room for
	// about 240 − S/17 parts. S is how many repositories had packs retired in
	// the matching quarter hour a day earlier: at the code host's design
	// point, 250 000 pushes a day (docs/concepts/scaling.md), each adding at
	// most one pack, and a compaction merging CompactMinMerge packs or more,
	// nearly always into one, at most about 17 000 records a day retire
	// packs — some 170 a pass, or 520 were all the day's pushes made in eight
	// hours: 0.8 s of a pass's twenty, 2.6 s at the busiest, and under a
	// twentieth of a day's steps. THE PURGES HAVE THE REST: about 240 parts a
	// pass with no sweep due and 210 beside the busiest sweeps, some 22 000 a
	// day — so ten thousand repositories (CodeMaxRepos) removed at once, four
	// parts each, are purged within two days of falling due, against a
	// retention of a week by default.
	//
	// A BACKLOG OF SWEEPS holds the purges back by passes, not days. Every
	// repository due at once — a fleet's compactions in one quarter hour, or
	// the passes after the duty was declined — is CodeMaxRepos steps of a few
	// rows, 49 s of steps, and the purges step again in the third pass; and
	// every row past a step's few adds 16 µs (93 µs on a loaded disk: the
	// fullest step, RetireSweepRows rows, took 69 ms and 378 ms), so even a
	// week of the design point's retirements — every pack pushed or
	// compacted retired once, two million rows — is swept within twelve
	// passes, three hours. A purge is days late by design; a node's pushes
	// are not.
	RetentionPassBudget = statelog.StallGrace

	// RetireGrace is how long a retired pack's row keeps its object from the
	// collector (code_packs_retired, ADR-0051) before the code-retention
	// duty's retire_sweep records remove it: a retired pack outlives its
	// longest read (ADR-0060).
	//
	// A DAY, because a reader that began on a pack before it was retired — a
	// cache that has not synced past the retirement, a clone that started on
	// the old pack list — may legitimately read the largest bundle a row can
	// name for objstore.ReadBudget(MaxPackBytes), 21 h 21 min, and the clock
	// judging the grace is the duty holder's against an instant the broker
	// stamped (retired_at): two machines — the duty holder and the broker
	// member leading this log — each held within objstore.AssumedClockSkew of
	// the fleet's clock, the median of the data nodes' clocks (its doc says
	// which machines `clock_skew` measures), so up to twice it apart while no
	// node raises it: 23 h 21 min in all, under a day by 39 min. The margin
	// past the longest read alone, RetireGrace − ReadBudget(MaxPackBytes) =
	// 2 h 39 min, is what a duty holder's clock may run ahead of the stamping
	// member's before a pack can be deleted under such a read; a node raises
	// `clock_skew` once its own clock, or the clock of the broker member
	// leading the position register, stands more than the assumed hour from
	// the fleet's, and NTP on every machine keeps them within milliseconds
	// (docs/guides/deployment.md), as the collector's own grace already
	// needs. internal/objstore/references holds the inequality.
	//
	// Equal to the collector's PendingGrace by one reason, not by reference —
	// a day outlives every legal read — since this package does not import
	// the collector.
	RetireGrace = 24 * time.Hour

	// RetireSweepRows is the most retired pack rows one retire_sweep record
	// removes: every row a record's apply may touch (MaxRecordRows) but the
	// repository's own, whose mark and version the record moves too. A
	// repository holding more rows past the grace than one record removes is
	// stepped record by record, each its own transaction, as a purge is; a
	// retired row is a pack row's width, narrower than the push rows a purge
	// part removes PurgePartRows of, and a record of all of them holds the
	// write lane 40 ms (BenchmarkApplyARetireSweep), under the third of
	// statelog.ApplyTxTimeBudget a purge part is sized to.
	RetireSweepRows = MaxRecordRows - 1

	// MaxFullRetirements is the most retirements a FULL snapshot serves beside
	// its own rows — those above its caller's StateQuery.RetiredSince, every
	// one or none — and so the point past which a delta's retirements weigh
	// against the full answer on the delta's side alone (reader.go's choose):
	// up to it the full answer would serve the same ones, and they cancel. So
	// the largest answer the reader serves is a full snapshot of a full
	// repository and one page.
	//
	// ONE PAGE OF ROWS, MaxSnapshotItems, because it weighs what a git node's
	// cache holds for its round against what serving none costs every git
	// node holding the repository:
	//   - served, they are at most a page more of the answer — some 2.4 MB of
	//     JSON at a retirement's 240 bytes or so, under MaxPageBytes — read
	//     once on the data node and held about as large by the cache;
	//   - not served, the cache treats every pack file no row names as one no
	//     row accounts for and drops the repository's commit-graph with it —
	//     rewritten from the whole history at 56 bytes an object of memory
	//     (the cache's GraphBytesPerObject), and until then a walk that would
	//     stop on its generation numbers walks by date (a push dated before
	//     its parent: 6.9 s over a gigabyte, against 0.37 s with the graph,
	//     docs/concepts/code-host.md) — and a graph of some 43 000 objects, a
	//     repository of about fifteen megabytes, already holds the page's
	//     memory;
	//   - a page is some 9 400 pushes to ONE repository since a cache last
	//     synced it (at most 16/15 retirements a push, a compaction below
	//     CompactTargetPacks merging CompactMinMerge packs or more into one;
	//     internal/code/compact's steady arms measured 1.05 for a repository
	//     pushed once a minute and 1.003 for one pushed three hundred times a
	//     minute, a minute's roll taking three hundred), all inside the
	//     RetireGrace a retirement's row lives: a cache further behind on so
	//     busy a repository takes the full answer — no more rows than the
	//     repository holds — rather than every retirement, so the catch-up of
	//     a cache a thousand compactions behind, some sixteen thousand
	//     retirements beside a repository of fifty packs, stays full.
	//
	// NOT the full answer's own rows, refs plus live packs, which it replaced:
	// those say nothing of what a graph costs — a repository of five refs may
	// be a monorepo — and compaction keeps the live packs few, so bounded by
	// them a full answer served nothing to a cache a few dozen pushes behind
	// on a repository of a few refs, nor to any cache after a compaction that
	// retired more packs than the repository has refs and packs left, each
	// such answer costing every git node its commit-graph to save a few dozen
	// rows.
	MaxFullRetirements = MaxSnapshotItems

	// CommitterInbox is how many proposals one repository's committer holds
	// queued on one node: SIXTEEN FULL RECORDS of the smallest pushes
	// (MaxPushesPerRecord each), so a burst — a CI fan-out of tag pushes —
	// queues whole behind an in-flight record and is committed in a few
	// appends. Past it a push is refused busy AT ONCE and the client runs
	// git push again, rather than the queue growing without bound in a
	// process that may die holding it. CommitterInboxBytes bounds the same
	// queue by what it weighs. NEITHER BOUNDS HOW LONG a push waits — a full
	// queue of healthy batches drains in a third of a second (1 024 pushes at
	// once on three members: 0.33–0.34 s in seventeen records, the last
	// answered within 0.35–0.38 s; doc.go's "What a push costs, measured"),
	// sixteen slow ones in minutes — so the queue is bounded in TIME too: a
	// push is answered busy
	// rather than decided once too little of its asker's attempt is left for
	// its own batch, and one asked with no deadline once it has waited a
	// whole CommitBudget (committer.go).
	//
	// A ROUTED burst reaches it too: the estate answers a node's proposals
	// on a registration of their own whose concurrency is this queue and
	// the batch ahead of it (internal/estate's proposalAnswers), so the
	// bound a burst meets is this one, refused busy at once, never the
	// registration's — past which a push would wait unseen in the broker's
	// buffer for a slot.
	CommitterInbox = 1_024

	// CommitterInboxBytes is the most one repository's queue may weigh, by
	// the bytes each proposal's entry can reach in its record: SIXTEEN
	// RECORDS' BYTES, which makes CommitterInbox's own reason exact for
	// large pushes as well as small ones. A count alone held 1 024 maximal
	// proposals — about a gibibyte for ONE repository, at EntryRoom each —
	// where a burst of large
	// pushes (an import, a fan-out of wide tag pushes) now queues sixteen
	// records' worth and is refused busy past it. The limits test holds it
	// to at least two records — a full record always queues behind an
	// in-flight one — and holds a queue of CommitterInbox minimal pushes
	// under it, so small pushes meet the count and only large ones meet the
	// bytes.
	CommitterInboxBytes = 16 * MaxRecordBytes

	// CommitterNodeBytes is the most the proposals one node's committer
	// HOLDS may weigh together, across every repository, by the same
	// measure — each from the moment it is queued until its caller has its
	// answer, its batch included: FOUR REPOSITORIES' CommitterInboxBytes,
	// sixty-four records. Past it a push is refused busy at once.
	//
	// Each repository's queue is bounded on its own, and nothing else bound
	// them together: a node is the committer of every repository rendezvous
	// ranks it first for, and the estate answers its proposals on a
	// registration of their own as wide as one repository's whole queue
	// (CommitterInbox), so a thousand of the largest pushes at once — each
	// one a receive-pack some receiving node holds open, from as many
	// repositories — held about a gibibyte of entries on the committer, and
	// as much again decoded. The largest entry this domain admits is
	// EntryRoom, 1 043 456 bytes: a push of refs JSON spells long, some six
	// hundred `&` refs at MaxRefNameBytes (Proposal.ValidateCommands). One of
	// plain refs is at most 451 222 bytes (MaxCommandsPerPush commands at
	// MaxRefNameBytes, then MaxIntroduced ids, measured). So four
	// repositories' queues is room for four bursts taken whole at once:
	// sixty-four of the largest entries, or about 150 of the largest plain
	// ones. A JUDGEMENT, of a burst's reach against a node's memory: 64 MiB
	// of entries, on a node that holds the whole estate, so that four
	// repositories' bursts queue whole at once. That is also the most the
	// estate's one shared registration could hold before proposals had a
	// registration of their own: sixty-four proposals a node, 64 MiB at the
	// largest entry and about 28 MiB at the largest plain one. Small pushes
	// never reach it — a thousand at a kilobyte or two meet CommitterInbox
	// long before — while large ones, an import's thousand-ref chunks, queue
	// four repositories' worth at once and are refused busy past it, and the
	// client runs git push again.
	CommitterNodeBytes = 4 * CommitterInboxBytes

	// CommitterIdle is how long a repository's committer goroutine waits
	// with nothing queued before it exits and forgets the repository. A
	// goroutine and its queue cost a few kilobytes and restarting one costs
	// microseconds, so the value bounds only how many repositories a node
	// keeps one for — those pushed to in the last half minute — while a
	// burst, whose pushes arrive seconds apart, keeps the one it started.
	CommitterIdle = 30 * time.Second

	// CommitBudget bounds ONE batch's publish and the read that answers it:
	// HALF the estate router's write attempt (sixty seconds). A healthy batch
	// takes milliseconds.
	//
	// A committer must answer inside the ONE attempt its asker gives it, or
	// the asker hands the same push id to the runner-up and two committers
	// contend on the repository's subject. So a push whose caller carries a
	// DEADLINE — the router's attempt, on a routed proposal — starts a batch
	// only while a whole budget and ReplyMargin are left before it, and is
	// answered busy undecided the moment they are not, whether it is still
	// queued or just reaching its batch; one whose caller carries none — the
	// in-process path, where no runner-up is asked while it runs — may wait
	// in the queue for less than one budget. One whose caller has gone is
	// never decided. internal/estate refuses to build a router whose write
	// attempt, less the serving node's floor wait, holds no more than a
	// budget and ReplyMargin, so a routed push can be decided at all (estate
	// imports this package, never the reverse) — and a write attempt can be
	// scaled no lower than that while this budget stands.
	//
	// WHAT A BATCH'S DECIDE CAN COST, against it. The committer forms the
	// batch's protection verdicts once, before its publish, and the
	// publisher then runs the decide once a compare-and-set round, up to
	// sixteen rounds (decide.go). At rules of the shape a team writes the
	// formation is 0.6–1.0 ms for a group commit of MaxPushesPerRecord
	// one-command pushes and 8–12 ms for three pushes of
	// MaxCommandsPerPush, and a round 2.6–3.8 ms and 10–11 ms. The rules a
	// founder CAN write make the formation longer, because asking a rule
	// about a ref is the one cost of a decide the rows do not bound:
	// 1.8–2.0 s for the most refs of 255 bytes a record carries (2 567)
	// against MaxProtectionsPerRepo rules of gitref's costliest shape,
	// 11–12 µs a pair, whether no rule matches or every command is refused
	// (BenchmarkDecideABatch's worst arms, a 2.1 GHz Xeon). Formed every
	// round, sixteen of those were 28–30 s — the whole budget — and a round
	// whose every command a rule refused took 3.4 s, twice that, while
	// naming the refusing rule took a second walk over the rules. So a
	// refusal names its rule from the one walk that found it, and the
	// verdicts are formed once a batch and kept across its rounds while each
	// round's snapshot holds the rules they were formed under: a round costs
	// 13–22 ms at that worst, the first as much as any, and a publish's
	// formation and sixteen rounds 2.0 s + 16 × 22 ms, about 2.4 s, a
	// twelfth of the budget.
	// A round forms the verdicts again, inside its snapshot, whenever its
	// rules differ from those the verdicts were last formed under — a
	// settings record changing THIS repository's protections, which only an
	// activation writes, landing before the round; and a flip back to rules
	// an earlier round read is a difference too — at another 1.8–2.1 s each
	// at that worst. Thirty seconds is then a broker election and its
	// rounds with room to spare, and past it the publisher's own resolution
	// answers unknown, naming the operation.
	CommitBudget = 30 * time.Second

	// ReplyMargin is what a serving node leaves between the end of its work
	// and its asker's deadline: a committer between the end of a push's
	// batch and the deadline, and every node answering a routed request
	// between the end of each wait it makes — for the asker's floor, a
	// read's barrier and its position, a write's own record — and the
	// deadline, so a node that runs out of time says so while its asker is
	// still listening (internal/estate marks every request it answers with
	// it, statelog.WithReplyMargin). It is the answer's trip back — its
	// encoding and one hop of the broker's request/reply, milliseconds on a
	// healthy fleet — and the skew between the asker's clock, which stamped
	// the deadline, and this node's, which reads it, which NTP holds to
	// milliseconds as well. ONE SECOND is two orders of magnitude over both,
	// and costs a routed push one second of the twenty-odd it may queue.
	ReplyMargin = time.Second

	// GitShareWait is the longest a seat's git work waits for a unit of its
	// node's share ([GitShare]) before the node answers it
	// [ErrGitUnready]{[UnreadyShare]} and the asker's walk moves to another
	// git node — a node whose seats' git work already holds half its read
	// lane sends its askers on rather than queueing them behind work that
	// may take a whole read deadline.
	//
	// TWO SECONDS, PROVISIONAL, by its rule: max(2 s, the slowest warm read
	// kind's p95 rounded up to a second), held under gitproc.ReadDeadline/4
	// (2.5 s), so a read waiting behind one other read of the slowest kind
	// still gets its unit while a saturated node gives its askers up well
	// inside what one read may take. MEASURED by internal/code/query's
	// BenchmarkCodeRead over its 32 MB history — gittest.History's 44 files
	// and 14 080 commits — under a 10 000-file tree (five reads of each kind,
	// -benchtime 5x, one count, git 2.56.0 on two E-cores of a Core Ultra 7
	// 265U): the slowest warm kind is the stat of an edit across all 10 000
	// files, 231 ms at its p95, beside 26 ms for the whole tree's listing at
	// any of its pages, 22 ms for that edit's stat filtered to one directory
	// and 14 ms for a comparison over half the history's commits — so the
	// slowest p95 rounds up to one second and the floor holds. Its 1 GB arm
	// (CREWLET_BENCH_LARGE: 455 680 commits, thirty-two times the commits a
	// comparison walks, under a 100 000-file tree, ten times the files a
	// listing lists) is the one that can move it.
	// Declared here, beside the share that waits it ([GitShare.Take]),
	// because everything that bounds a git read's whole attempt reads it from
	// the package every party to a read imports.
	GitShareWait = 2 * time.Second

	// MaxPackBytes is the largest OBJECT one push stores — its pack bundle
	// (bundle.go): the pack receive-pack stored, then its index and its
	// reverse index.
	//
	// FIVE QUARTERS of config.CodeMaxPushBytesCeiling. The client's input is
	// bounded by receive.maxInputSize at code.limits.max_push_bytes (at most
	// that ceiling), but the pack receive-pack STORES is that input
	// completed against the repository — index-pack --fix-thin appends,
	// whole, every base a thin pack's deltas name — and the bundle adds the
	// index (2.2 % of the pack) and the reverse index (0.3 %); a quarter is
	// room for both on any push a person makes, and a bundle past it is
	// refused pack_too_large by the receiver before anything is uploaded.
	//
	// Bounded above by the object store's write bound — the put of a
	// 1.25 GiB bundle is given UploadDeadline's 10 h 41 min (a minute of
	// objstore.ReadBase and objstore.PaceFor's 10 h 40 min), under
	// objstore.RecordWithin (12 h), which internal/objstore/references pins —
	// and BELOW 2 GiB, which is a RULE rather than an accident: an index
	// grows a 64-bit offset table only for a pack past 2 GiB, and no writer of
	// a stored pack may ever produce one — a push is held here, and a later
	// compaction caps what it writes with pack-objects --max-pack-size — so
	// CheckBundle's section sizes are exact functions of the object count for
	// every bundle the store will ever hold.
	// TestTheBoundsFitTheReadAndPurgeBudgets holds both inequalities.
	MaxPackBytes = config.CodeMaxPushBytesCeiling + config.CodeMaxPushBytesCeiling/4
)

// THE COMPACTION'S BOUNDS: how many packs a repository may hold, which of them
// a compaction merges, and how large one compaction's record may be. A
// compaction is a record of its own on the repository's subject (OpCompact,
// [Compaction]); its committer is the one its pushes meet, and the git node
// that builds it is the repository's home.
const (
	// MaxPacksPerRepo is the most LIVE packs a repository holds: a push that
	// would carry one more is answered compaction_behind, undecided — by the
	// push decide ([working.behind]), and by the receiver before it uploads
	// the push's bundle — until a compaction has merged the repository's
	// packs.
	//
	// 2 048, from a cold sync of every live pack at ONE sync's width (the
	// cache's FetchConcurrency, sixteen GETs in flight) at S3's slow first
	// byte, 200 ms: its downloads held to 27 s, half of the code host's spool
	// wait (55 s, a minute of the balancer's quiet less a keepalive), which
	// leaves the other half for the round's index and its peel before the
	// first byte of the request that waited for them. MEASURED, and kept:
	// internal/code/cache's BenchmarkColdSync, its
	// bucket=fake/firstbyte=200ms/packs=2048/fetch=16 arm — the S3 fake
	// through aws-sdk-go-v2/service/s3 v1.114.3, pushes of 150 KB, git
	// 2.56.0, Go 1.27.2, one bounded run (-benchtime 1x, one count) on two
	// E-cores of an Intel Core Ultra 7 265U, which has SHA instructions,
	// 2026-10-28 — downloaded the 2 048 in 26.4 s, 77 bundles a second at
	// 204 ms a GET, and ended its whole round at 26.6 s. Downloads past 27 s
	// would make this the largest count their rate downloads in 27 s; a round
	// past the spool wait with its downloads inside them is a finding against
	// the round's index (internal/code/cache's MidxRoundPacks), never this.
	MaxPacksPerRepo = 2_048

	// CompactFactor is the geometric progression a home keeps a repository's
	// packs in: every pack at least twice the objects of all the lighter
	// ones together, git's own `repack --geometric=2` (gc's), so each object
	// is rewritten O(log n) times over a repository's life.
	CompactFactor = 2

	// CompactMinMerge is the fewest packs a compaction merges while the
	// repository holds no more than CompactTargetPacks. A merge costs one PUT
	// of its output and one GET of it on every other git node that holds the
	// repository, and saves each later cold sync k−1 GETs and each request
	// k−1 pack lookups — so what the floor decides is how many merges a QUIET
	// repository's pushes cost, one whose home's poll finds a few new packs
	// at a time: a busy one's minute of pushes passes any floor.
	//
	// SIXTEEN, MEASURED by internal/code/compact's BenchmarkCompact, its
	// steady/pushes=1200/rate=1/min arms: a repository pushed once a minute
	// for twenty simulated hours, a pass every minute, its requests counted
	// on a fleet of eight git nodes (git 2.56.0 over the in-memory store, Go
	// 1.27.2; one bounded run, -benchtime 1x and one count, on two E-cores of
	// an Intel Core Ultra 7 265U, which has SHA instructions, 2026-10-28).
	// Merging from 16 packs cost 527 GETs and PUTs a thousand pushes — 64
	// PUTs, 449 GETs of the outputs and 13 of a cold sync of what was left —
	// against 1 081 from 8 and 2 212 from 4: half the requests, each merge's
	// PUT and GETs bought by twice the pushes, for a cold sync of 20 live
	// packs at the most rather than 13. It costs fewer bytes too (713 MB of
	// outputs stored and downloaded a thousand pushes, against 873), fewer
	// rows (1.05 packs retired a push, against 1.13) and fewer records (108
	// bytes a push on the code log, against 184); and every merge drops every
	// git node's bitmap of the repository, so half the merges are half that
	// churn. The 8 this replaced was a judgement, kept unless an arm saved a
	// tenth of the requests.
	//
	// THE LARGEST FLOOR MEASURED, not a minimum the arms bracket: M3-decisions
	// M3-O32 sets them at 4, 8 and 16, and each doubling halved the requests,
	// so sixteen is the best of the three. What bounds a higher floor is
	// CompactTargetPacks, which a quiet repository's packs below the floor and
	// the progression of its merged ones stay under only while the floor
	// leaves the progression room: past the target the home merges at every
	// poll, two packs at the least — as often as it merges anything.
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

	// FullPackBytes is the pack section past which a pack is FULL — merged by
	// no later compaction ([PackRow.Full]). pack-objects closes an output
	// before the object that would take it past --max-pack-size, so every
	// output but the last of a cut is within an object's size of the cut:
	// seven eighths marks those, and no pack a push of ordinary size makes.
	// It holds while an object is smaller than an eighth of the cut —
	// code.limits.max_blob_bytes' default (50 MiB) is; its ceiling (1 GiB) is
	// not, and a repository whose blobs approach the cut writes outputs short
	// of seven eighths that are the last of their cut, read as not full and
	// merged again: a pack rewritten once more than it needed, never a pack
	// that is wrong.
	FullPackBytes = CompactPackBytes / 8 * 7

	// FullBundleBytes is the BUNDLE past which a pack is full, beside its pack
	// section: FullPackBytes. An output of a cut the builder scales down for
	// a dense roll — one of many small objects, whose indexes, at 32 bytes an
	// object, would take its bundle past MaxPackBytes at the nominal cut —
	// has a pack section under seven eighths of the nominal cut and a bundle
	// near CompactPackBytes, so it is full by its bundle; a threshold it never
	// reaches would roll it again at every poll.
	FullBundleBytes = FullPackBytes

	// MaxCompactInputs is the most packs one compaction merges. Its record's
	// apply writes two rows an input — its retired row and its deletion from
	// code_packs — and one an output, so a compaction of MaxCompactInputs
	// packs into MaxCompactOutputs is compactDecidedRows of them, 2 065, about
	// half of MaxRecordRows; the record itself is about 45 bytes an input and
	// 330 an output, some 52 KiB at the most, far under MaxRecordBytes. A
	// PUBLICATION's repack takes the same bound, and its record is the op's
	// widest: beside those rows a layer of MaxLayerParts parts replacing
	// MaxPublishedLayers − 1 layers of as many, 2 609 rows in all, about two
	// thirds of MaxRecordRows, and the reads no other compaction's apply
	// makes — the chain whole before, up to every live pack's checksum, and
	// the live packs again for the recount after.
	//
	// MEASURED, on one machine, both against a repository of MaxPacksPerRepo
	// packs: BenchmarkApplyACompaction's inputs=1024 arm applies in 62 ms,
	// and its publication/inputs=1024 arm — the widest publication, every
	// bound at once — in 68 ms, where its rows alone at the first's rate
	// would have priced it at 78; both under the third of
	// statelog.ApplyTxTimeBudget (83 ms) a compaction's apply is held to
	// (doc.go's measured section). A measurement of inputs=1024 past that
	// third halves this until it is under; one of the publication's alone
	// bounds a publication's repack below this instead, halved until that
	// arm is under, since its layer's rows are what it adds. It bounds what
	// this build's committer takes ([CompactionProposal.Validate]); the
	// apply takes whatever a record carries, as a push's does.
	MaxCompactInputs = 1_024

	// MaxCompactOutputs is the most outputs one compaction writes, all of
	// them uploaded AT ONCE: each output's key is minted as its upload
	// begins, so the last lands at most objstore.ReadBase +
	// objstore.PaceFor(MaxPackBytes), 10 h 41 min, after the first at the
	// store's pacing floor — every output a bundle of at most MaxPackBytes —
	// under objstore.RecordWithin (12 h) however many there are, where outputs
	// uploaded one after another aged the first key by every later upload,
	// and two of 1 GiB at the floor passed it. Sixteen bounds the uploads'
	// transient memory too: sixteen PUTs, each holding one first buffer of an
	// S3 part (8 MiB), 128 MiB. A proposal of more is refused
	// ([CompactionProposal.Validate]), and never more outputs than inputs: a
	// merge that wrote more packs than it replaced would undo itself.
	MaxCompactOutputs = 16
)

// THE PUBLISHED LAYERS' BOUNDS: how long a repository's published chain may
// grow, and how its bytes are cut into stored objects. A layer is published as
// a compaction carrying it ([Compaction.Layer], [LayerRef]); these bound what
// such a record names and writes.
const (
	// MaxPublishedLayers is the most layers a repository's published chain
	// holds: a publication that would leave more is no record a committer
	// forms ([CompactionProposal.Validate], and the decide's own count).
	//
	// THIRTY-TWO, A JUDGMENT CALL. A publisher that merges the chain's top
	// layers geometrically — each layer at least twice the objects of the
	// layers above it together, git's own repack.midxSplitFactor — keeps at
	// most about log₂(history ÷ one publication) of them: eleven for the
	// largest repository the defaults admit (some 60 million objects,
	// DefaultRepoMaxBytes) published 32 000 objects at a time, and
	// twenty-one were each publication as small as thirty-two objects —
	// under thirty-two either way. What the bound costs is a lookup's: an
	// object is looked for in every layer above the one that holds it, so a
	// deeper chain is a slower answer on every git node; past the bound a
	// publisher merges the top layers whatever their sizes.
	//
	// PROVISIONAL (M3-decisions M3-O39): nothing yet measures what a chain's
	// depth costs a lookup — no arm of BenchmarkPackLookup serves through a
	// chain. The layers={1,8,32,64} arm the layer measurements add to it
	// confirms the bound or moves it: thirty-two stays unless a lookup
	// through thirty-two layers costs more than twice one through a single
	// layer, and otherwise becomes the longest chain that does not.
	MaxPublishedLayers = 32

	// LayerPartBytes is the most bytes one stored object of a layer holds:
	// a layer's bytes are cut into parts of exactly this, the last at most
	// it ([LayerRef.Parts]), each a stored object of its own.
	//
	// ONE GIBIBYTE, the push ceiling (config.CodeMaxPushBytesCeiling),
	// because every part of a layer is uploaded AT ONCE and its key is
	// minted as its upload begins: a part at the store's pacing floor lands
	// within objstore.ReadBase + objstore.PaceFor(LayerPartBytes), about
	// 8 h 33 min, under objstore.RecordWithin (12 h) however many parts
	// there are — which internal/objstore/references pins beside the
	// largest object every other declaring domain stores. A layer of the
	// largest repository the defaults admit is some 2.6 GB (37 bytes an
	// object for its multi-pack-index and bitmap, 69 a commit for its
	// commit-graph layer, measured on git 2.56.0), which one object could
	// not hold within that bound.
	LayerPartBytes = config.CodeMaxPushBytesCeiling

	// MaxLayerParts is the most parts one layer is cut into: 16 GiB of
	// layer, some 300 million objects at those two figures — five times the
	// largest repository the defaults admit — whose publication no git
	// node this release sizes has the memory to build. Sixteen bounds the
	// uploads' transient memory too: sixteen PUTs at once, each holding one
	// first buffer of an S3 part (8 MiB), 128 MiB, as MaxCompactOutputs
	// bounds a compaction's.
	MaxLayerParts = 16
)

// compactDecidedRows is the most rows a compaction of n inputs into m outputs
// makes its record's apply touch: each input's retired row and its deletion,
// each output's row, and the repository's counters — and, for a PUBLICATION
// (l is its layer), the layer's row and each of its parts, and every replaced
// layer's row with up to [MaxLayerParts] parts, which the proposal does not
// carry — the bound the committer holds a compaction's decision to, as
// [withinRecord] holds a push batch's. The repository's row is written once
// either way: a publication's recount rides the counters' update. At every
// bound — MaxCompactInputs inputs into MaxCompactOutputs outputs, a layer of
// MaxLayerParts parts replacing MaxPublishedLayers − 1 layers — that is 2 609
// rows, about two thirds of MaxRecordRows.
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
	// MaxCompactInputs. The begin's apply writes two rows an input, its
	// retired row and its deletion from code_packs, as a compaction's does,
	// and the record names each by its id, so a begin at every bound is
	// expungeBeginRows of them, 2 625, about two thirds of MaxRecordRows —
	// the widest record the code domain forms — beside the reads only a
	// publication's apply makes too: the chain whole before, and the live
	// packs again for the recount after.
	//
	// MEASURED AS ITS OWN SHAPE, not a compaction's: BenchmarkApplyACompaction's
	// expunge/begin arm — every bound at once, a chain of MaxPublishedLayers
	// layers of MaxLayerParts parts dropped whole, against MaxPacksPerRepo
	// live packs — applies in 75 ms (one bounded run, -benchtime 5x and one
	// count, on two E-cores of a Core Ultra 7 265U at a load average of
	// 3.4–4.7), under the third of statelog.ApplyTxTimeBudget (83 ms) a
	// compaction's apply is held to, by 8 ms (doc.go's measured section). A
	// measurement of that arm past the third halves this below
	// MaxCompactInputs until it is under. It bounds what this build's
	// committer takes ([ExpungeProposal.Validate]); the apply takes whatever
	// a record carries, as a compaction's does.
	MaxExpungeInputs = MaxCompactInputs

	// MaxExpungeOutputs is the most packs one expunge's begin writes — the
	// packs it keeps live and the packs it leaves dropping, together — and
	// the most its commit's rescue writes.
	//
	// THIRTY-TWO: config.DefaultCodeRepoMaxBytes over CompactPackBytes, the
	// cut every output is written at — twenty — rounded up to a power of two,
	// so a repository at the default ceiling is rewritten in a cycle or two,
	// whatever of it is reachable. Every output is uploaded AT ONCE, as a
	// compaction's are ([MaxCompactOutputs]): each key is minted as its
	// upload begins, so the last lands at most objstore.ReadBase +
	// objstore.PaceFor(MaxPackBytes), 10 h 41 min, after the first at the
	// store's pacing floor, under objstore.RecordWithin (12 h) however many
	// there are; and thirty-two PUTs at once hold one first buffer of an S3
	// part each (8 MiB), 256 MiB, on the maintainer that builds them.
	MaxExpungeOutputs = 32
)

// expungeBeginRows is the most rows a begin of n inputs into m outputs — its
// kept and its dropping packs together — dropping layers published layers
// makes its record's apply touch: each input's retired row and its deletion,
// each output's row, each dropped layer's row with up to [MaxLayerParts]
// parts, which the proposal does not carry, and the repository's row, whose
// counters and published recount ride one update — the bound the committer
// holds a begin's decision to, as [withinRecord] holds a push batch's. At
// every bound — MaxExpungeInputs inputs into MaxExpungeOutputs outputs,
// MaxPublishedLayers layers dropped — that is 2 625 rows, about two thirds of
// MaxRecordRows.
func expungeBeginRows(n, m, layers int) int { return 2*n + m + layers*(1+MaxLayerParts) + 1 }

// expungeCommitRows is the most rows a commit retiring n dropping packs and
// writing m rescue packs makes its record's apply touch: each dropping pack's
// retired row and its deletion, each rescue pack's row, and the repository's
// row — 97 at every bound, since a repository holds no more dropping packs than
// one begin wrote.
func expungeCommitRows(n, m int) int { return 2*n + m + 1 }

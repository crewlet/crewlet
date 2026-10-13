package code

import (
	"math/bits"
	"time"

	"github.com/crewlet/crewlet/internal/config"
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
	return Limits{MaxPushBytes: r.MaxPushBytes, MaxBlobBytes: r.MaxBlobBytes, RepoMaxBytes: r.RepoMaxBytes}
}

// BarrierRecordBytes is what one code-log barrier costs its stream, as the
// broker counts it against the log's ceiling: the encoded record
// ([EncodeBarrier]), its subject and the broker's per-message overhead.
//
// 136 BYTES, MEASURED: a barrier at the first generation, whose envelope
// carries no generation, costs 116; one at the last costs 133, since a
// generation printed whole is up to ten digits and its key. So 136 bounds every
// barrier the log can hold, and overstates the commonest by under a fifth —
// noise beside census_drift's factor of two
// (TestABarrierCostsTheCodeLogWhatTheCensusSays holds both arms between half
// the figure and all of it). The 160 first proposed was an estimate from the
// tracker's figure in docs/guides/retention.md and overstated a real barrier
// by 38 %, which sized the git census that much smaller.
const BarrierRecordBytes = 136

// GitReadsShare is how much of the code log's byte ceiling its git clients'
// barriers are sized to hold over the trim's min_age: one part in TEN. The rest
// is what [CodeLogMaxBytes] was sized for — pushes, settings, purges — and a
// tenth is room a company's git traffic grows into without crowding them. A
// JUDGEMENT: smaller fires census_drift on a busy but healthy git host, larger
// lets barriers crowd the records the ceiling was sized for before anybody is
// told; the census is a share of the ceiling the log's stream HOLDS, so a
// company that outgrows the tenth resizes the log (`crewlet retention
// set-capacity`) and the census moves with it.
const GitReadsShare = 10

// GitReadsPerDay is the linearizable git reads a day the code log was sized
// for: the census term of its git clients (statelog.Census.Clients) — the
// barriers that fill 1/GitReadsShare of ceiling over the trim's min_age.
//
// EVERY linearizable read of the code log in this release is a person's or
// their CI's — an advertisement, a push's sync, a fetch whose want a cache
// lacks, a `/code/` read at that level — and none is a seat's, so the census's
// 125 a day per agent seat says nothing about them: a five-seat company's code
// log alarmed past 1 300 git reads a day, which one CI polling ten repositories
// a minute exceeds tenfold. At the 1 GiB floor and the default seven days this
// is 112 788 a day — about 78 a minute fleet-wide: that CI, thirty developers
// fetching every three minutes, and every clone and push, several times over.
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
	// SIXTEEN GIBIBYTES, the tracker's: at the reference company once seats
	// push the code log grows at the mutation log's order — about 3 000
	// pushes a day at about 2 KiB a record, 6 MB a day before checks,
	// against the tracker's 9 MB — so a blocked trim fills both on the same
	// order of time.
	CodeLogMaxBytes = 16 << 30

	// CodeLogDuplicates is the window the broker collapses a repeated
	// operation id in: the tracker's and the knowledge base's two minutes.
	// An optimisation, never the mechanism — the operation ledger and a
	// push's own id are what make a retry idempotent.
	CodeLogDuplicates = 2 * time.Minute

	// MaxPushesPerRecord is the most pushes one record carries — the bound
	// on the work one lost compare-and-set round re-decides (64 pushes at a
	// few indexed reads each), and the per-record ceiling the tracker's bulk
	// writes use. Group commit's gain is in bursts — a CI fan-out of tag
	// pushes — where it turns 64 appends into one: applied and committed,
	// such a record holds the write lane 10–12 ms, 0.16–0.19 ms a push
	// against 3.0–4.0 ms for one push on its own, far inside
	// statelog.ApplyTxTimeBudget (doc.go's measured section).
	MaxPushesPerRecord = 64

	// MaxRecordBytes is the largest record this domain appends: an eighth of
	// what one append may carry, so eight maximal code records fit one
	// node's per-log reservation at once and a committer serving many
	// repositories is never serialised behind one large record.
	MaxRecordBytes = statelog.MaxAppendBytes / 8

	// MaxRecordRows is the most rows one record's apply may write or delete:
	// the framework's per-transaction budget, which is checked only at record
	// boundaries — a larger record would hold the shared write lane past it.
	// A push record's tombstone sweep fills what the record's own rows leave
	// of it, so a push record reaches it exactly whenever expired tombstones
	// are waiting.
	MaxRecordRows = statelog.ApplyTxRowBudget

	// MaxCommandsPerPush is the most ref updates one push carries. A push's
	// apply then writes at most 1 003 rows, so three maximal pushes fit one
	// record's row budget and two its byte budget (about 410 KiB each with
	// refs at MaxRefNameBytes); an import chunks its refs at this size.
	MaxCommandsPerPush = 1_000

	// MaxRefsPerRepo is the most live refs a repository holds. A protocol-v0
	// advertisement costs about 100 bytes a ref, so this is about 10 MB on
	// every v0 fetch and a 10 MB packed-refs rewrite on every cache sync;
	// past it a repository is a namespace problem, refused (repo_refs)
	// rather than slowly degraded.
	//
	// The rows a repository's refs table holds are its live refs AND its
	// tombstones, which the applier counts beside them (code_repos.refs and
	// code_repos.tombstones), so the decide that judges a push against this
	// bound can judge the table rather than the live refs alone: a push that
	// creates refs on a repository whose deletions have not yet aged past
	// RefTombstoneHorizon adds rows the sweep cannot take yet.
	MaxRefsPerRepo = 100_000

	// MaxIntroduced is the most introduced commit ids a push row lists (43
	// bytes each); IntroducedCount carries the rest.
	MaxIntroduced = 1_000

	// MaxProtectionsPerRepo is config.CodeMaxProtections: every matching
	// rule is asked about every command in the decide.
	MaxProtectionsPerRepo = config.CodeMaxProtections

	// MaxSnapshotItems is the most rows one snapshot or listing page
	// carries: about 1–2 MB of JSON for typical refs and about 4 MB at
	// worst (every ref at MaxRefNameBytes), under the estate reply's
	// ceiling with room; MaxRefsPerRepo refs is ten pages.
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
	// deletion. A cache synced within it gets a delta; an older one a full
	// answer (code_repos.tombstones_through), which re-reads the rows and
	// reuses every pack the cache holds — so the horizon trades tombstone
	// rows kept against how often an idle-but-warm cache pays a full
	// listing, never a download. Two days covers a cache fetched on any
	// working day across a one-day gap.
	RefTombstoneHorizon = 48 * time.Hour

	// MaxDescriptionBytes is config.CodeMaxDescriptionBytes: a repository
	// description is one paragraph, riding every settings record and every
	// listing row.
	MaxDescriptionBytes = config.CodeMaxDescriptionBytes

	// HorizonDeletesPerRecord is how many expired PUSH ROWS one push record
	// removes from its repository: no fewer than the push rows a record can
	// add (MaxPushesPerRecord), so the push history's removal keeps pace on
	// an active repository, and a fixed share, so the committer can count it
	// in a record's rows before the record exists. HorizonBytesPerRecord
	// bounds the same removal by what the rows weigh.
	//
	// It does NOT bound the TOMBSTONES a record sweeps. A record can make
	// three thousand of those, which a share of sixty-four fell behind by
	// about 936 a record on a repository that deletes refs in bulk; the
	// sweep takes every row the record leaves of MaxRecordRows instead —
	// 3 995 behind a one-command push, 992 behind the largest record — so
	// what a record writes and removes together is MaxRecordRows at most.
	HorizonDeletesPerRecord = 64

	// HorizonBytesPerRecord is how many BYTES of expired push rows one push
	// record removes beside HorizonDeletesPerRecord's count — each row
	// weighed by its row_bytes, the commands and introduced ids it carries —
	// taking rows oldest first until their bytes reach it, so a removal
	// passes it by at most one row.
	//
	// MaxRecordBytes, the most push-row bytes one record can ADD: a row's
	// commands and introduced ids are its record's own, stored with less
	// (no refusal sentence, the ids space-separated rather than quoted). A
	// removal that stops at either bound has therefore taken at least the
	// oldest record's whole contribution — its rows share one instant and
	// one position, so they are first in the order and together — and the
	// history's removal keeps pace with its growth in rows and in bytes
	// alike.
	//
	// NO MORE, because this is the push history, the one wide table in the
	// estate (a push row reaches 461 000 bytes), and deleting a row frees its
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
	// a part's push rows by what they weigh as well.
	PurgePartRows = 3_000

	// PurgePartBytes is how many BYTES of push rows one purge step deletes
	// beside PurgePartRows' count — each row weighed by its row_bytes —
	// taking them oldest first until their bytes reach it: always at least
	// one, and at most one row past it. A part whose pushes were cut by it
	// deletes nothing else, so the refs and the packs wait for a part of
	// their own; and the final purge, whose statements are unbounded, is
	// formed only once the pushes left fit it as well as the rows do.
	//
	// A ROW COUNT BOUNDS A PART'S TIME ONLY FOR NARROW ROWS. A push row is
	// the estate's one wide row — up to 461 000 bytes of commands and
	// introduced ids — and deleting one frees its overflow pages: measured,
	// a part of 3 000 push rows of a kilobyte holds the write lane 56–60 ms,
	// and bounded by rows alone one of 1 000 rows of 460 KB held it
	// 1.05–1.10 s, about 2.4 ms a megabyte — four times
	// statelog.ApplyTxTimeBudget in a record the budget cannot split, on
	// every data node, with every other domain's applier waiting behind it.
	// THIRTY-TWO MEBIBYTES is a third of the budget: 81–88 ms for a part of
	// the widest rows (seventy-three of them), and 76–103 ms for the most a
	// part can take, rows just short of it and narrow refs for the rest of
	// PurgePartRows (doc.go's measured section).
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
	// lane a code-retention pass takes: after each purge step the pass waits
	// (1 − share) / share times as long as that step took — twice, at a
	// third — before the next. A step is decided, appended and APPLIED on the
	// deciding node before it answers, so its duration is its own apply plus
	// whatever the lane applied ahead of it, and a busy lane slows the purge
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
	// (TestAPassHoldsAtMostAThirdOfTheWriteLane).
	RetentionLaneShare = 1.0 / 3

	// RetentionPassBudget is how long one code-retention pass takes steps for
	// before it stops, leaving what is due to the next pass, a
	// RetentionInterval later. Running out is the pass's designed stop.
	//
	// statelog.StallGrace, A MINUTE, because a node that applies the purge
	// more than 1/RetentionLaneShare times slower than the holder still
	// queues, (k·share − 1) of every second the pass runs: bounded to a
	// minute, a node up to six times slower ends a pass at most StallGrace
	// behind — where a push it commits starts to be answered pending rather
	// than ok — and catches up within the quarter hour before the next; a
	// node slower still is one the lag alarm already names. What it costs is
	// the drain: a minute at a third is twenty seconds of steps a pass, about
	// 240 of the measured 83 ms parts — some 23 000 a day — so ten thousand
	// repositories (CodeMaxRepos) removed at once, four parts each, are purged
	// within two days of falling due, against a retention of a week by
	// default. A purge is days late by design; a node's pushes are not.
	RetentionPassBudget = statelog.StallGrace

	// CommitterInbox is how many proposals one repository's committer holds
	// queued on one node: SIXTEEN FULL RECORDS of the smallest pushes
	// (MaxPushesPerRecord each), so a burst — a CI fan-out of tag pushes —
	// queues whole behind an in-flight record and is committed in a few
	// appends. Past it a push is refused busy AT ONCE and the client runs
	// git push again, rather than the queue growing without bound in a
	// process that may die holding it. CommitterInboxBytes bounds the same
	// queue by what it weighs. NEITHER BOUNDS HOW LONG a push waits — sixteen
	// healthy batches drain in well under a second, sixteen slow ones in
	// minutes — so the queue is bounded in TIME too: a push is answered busy
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
	// proposals — about 400 MiB for ONE repository — where a burst of large
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
	// repositories — held over 400 MiB of entries on the committer, and as
	// much again decoded. The largest entry this domain admits is about
	// 440 KiB (MaxCommandsPerPush commands at MaxRefNameBytes, then
	// MaxIntroduced ids: 451 222 bytes measured), so four repositories'
	// queues is room for four bursts taken whole at once, or about 150 of the
	// largest pushes — where the estate's one shared registration held at
	// most sixty-four proposals a node, about 28 MiB of them, before
	// proposals had a registration of their own. A JUDGEMENT, of a burst's
	// reach against a node's memory: about 64 MiB of entries, some two and
	// a third times what the shared registration could hold, on a node that
	// holds the whole estate, so that four repositories' bursts queue whole
	// at once. Small pushes
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
	// Bounded above by the object store's write bound —
	// objstore.PaceFor(1.25 GiB) is 10 h 40 min, under objstore.RecordWithin
	// (12 h), which internal/objstore/references pins — and BELOW 2 GiB,
	// which is a RULE rather than an accident: an index grows a 64-bit offset
	// table only for a pack past 2 GiB, and no writer of a stored pack may
	// ever produce one — a push is held here, and a later compaction caps
	// what it writes with pack-objects --max-pack-size — so CheckBundle's
	// section sizes are exact functions of the object count for every bundle
	// the store will ever hold. TestTheBoundsFitTheReadAndPurgeBudgets holds
	// both inequalities.
	MaxPackBytes = config.CodeMaxPushBytesCeiling + config.CodeMaxPushBytesCeiling/4
)

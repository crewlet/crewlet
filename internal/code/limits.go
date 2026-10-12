package code

import (
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
func DefaultLimits() Limits {
	return Limits{
		MaxPushBytes: config.DefaultCodeMaxPushBytes,
		MaxBlobBytes: DefaultMaxBlobBytes,
		RepoMaxBytes: DefaultRepoMaxBytes,
	}
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

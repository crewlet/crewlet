package artifacts

import "time"

// THE ONE LIMITS TABLE. Every bound an artifact, a version, an upload, a
// datastore or a presence room is held to is named here, with its reason, and
// every surface cites the constant rather than restating the number. A
// refusal names the value it refused at; nothing is ever silently cut.

const (
	kiB = int64(1) << 10
	miB = int64(1) << 20
	giB = int64(1) << 30
	tiB = int64(1) << 40
)

// Limits are the plan quotas a company is held to: Tier A, because a tenant
// administrator must not be able to raise their own quota, and in a hosted
// deployment the platform renders them from the plan.
//
// STAMPED, NEVER READ AT APPLY. Tier A is per node and differs across nodes
// during a rollout, so the node that decides a write stamps the limits in force
// into the record, and the applier enforces the stamp. Two nodes then apply one
// record identically whatever each was configured with.
type Limits struct {
	// CompanyBytes bounds the bytes every artifact's retained versions and
	// assets name, counted once per distinct object.
	CompanyBytes int64 `json:"company_bytes"`

	// Artifacts bounds how many artifacts a company holds, trashed ones
	// included until they are purged.
	Artifacts int64 `json:"artifacts"`

	// DataBytes bounds the datastore footprint of every store together:
	// the bytes its rows cost on every data node (document rows and their
	// field index, and hot history), as internal/artifactdata's meter
	// counts them.
	DataBytes int64 `json:"data_bytes"`

	// DataStoreBytes bounds one artifact's datastore footprint.
	DataStoreBytes int64 `json:"data_store_bytes"`

	// PendingBytes bounds the bytes one principal may hold reserved and not
	// yet named by a record; a company may hold [CompanyPendingFactor] times
	// it.
	PendingBytes int64 `json:"pending_bytes"`
}

// The plan quota defaults, and the bounds Tier A accepts for each.
const (
	// DefaultCompanyBytes is 10 GiB: on the default nats object backend every
	// broker member holds every object, so this is 30 GiB of disk at three
	// replicas, inside the bucket reservation of a modest volume. An
	// operator on s3 raises it.
	DefaultCompanyBytes = 10 * giB
	MinCompanyBytes     = 1 * giB
	MaxCompanyBytes     = 16 * tiB

	// DefaultArtifacts is 50 000: rows are small, and the bound keeps a
	// gallery query and a usage scan inside an index's reach.
	DefaultArtifacts = int64(50_000)
	MinArtifacts     = int64(100)
	MaxArtifacts     = int64(10_000_000)

	// DefaultDataBytes is 1 GiB: datastore rows live in every data node's
	// replicated file and ride every snapshot and backup until tiering
	// moves idle stores to object storage.
	DefaultDataBytes = 1 * giB
	MinDataBytes     = 64 * miB
	MaxDataBytes     = 1 * tiB

	// DefaultDataStoreBytes is 32 MiB per store, in the bytes its rows cost
	// on disk: spike DS-2 measured a twelve-column review sheet at about
	// 3.2 KiB a row with a week of history, so a sheet of a few thousand
	// rows takes a few MiB to ten, and the default holds about ten thousand
	// before a sheet is really a database.
	DefaultDataStoreBytes = 32 * miB
	MinDataStoreBytes     = 1 * miB
	MaxDataStoreBytes     = 1 * giB

	// DefaultPendingBytes is 512 MiB: eight full publishes in flight for one
	// principal, which no honest client exceeds, while a looping agent or a
	// stale client is refused before it fills the object store with bytes
	// nothing will ever name.
	DefaultPendingBytes = 512 * miB
	MinPendingBytes     = 64 * miB
	MaxPendingBytes     = 16 * giB

	// CompanyPendingFactor is how many principals' worth of reservations a
	// company may hold at once.
	CompanyPendingFactor = int64(4)
)

// Parity limits: the published contract an artifact author already knows, so
// a page that fits elsewhere fits here.
const (
	MaxEntryBytes        = 16 * miB  // the served HTML or Markdown entry
	MaxTextFileBytes     = 16 * miB  // any other text file
	MaxBinaryFileBytes   = 15 * miB  // any binary file
	MaxFilesPerPublish   = 255       // new bytes in one publish; carried entries move none
	MaxPublishBytes      = 64 * miB  // new bytes in one publish; bounded at 32 minutes at the objstore pace, inside RecordWithin
	MaxFilesPerVersion   = 511       // bounds a manifest and so the artifacts log's largest record
	MaxVersionBytes      = 256 * miB // the sum of a version's manifest sizes
	MaxPathBytes         = 1024      // one published path
	MaxTitleBytes        = 120       // a tab title and a gallery card line
	MaxDescriptionBytes  = 1000      // one sentence for the card's subtitle
	MaxIconBytes         = 40        // one short word
	MaxLabelBytes        = 60        // a version label
	MaxSourceKeyBytes    = 256       // the republish-in-place address
	MaxPreflightBytes    = 8 * kiB   // the reserved preflight.js at the root
	MaxSearchTextBytes   = 64 * kiB  // text extracted at publish for the gallery index
	MaxConflictEntryText = 1 * miB   // the live entry's text a version_moved answer inlines
)

// Asset store bounds.
const (
	MaxAssetBinaryBytes      = 15 * miB // one publish's worth per binary asset
	MaxAssetTextBytes        = 16 * miB // and per text asset
	MaxAssetsPerArtifact     = 1000     // keeps the asset store from becoming a file share
	MaxAssetBytesPerArtifact = 1 * giB
	MaxAssetsPerCall         = 25 // one upload call
)

// Retention.
const (
	// VersionsKeptUnlabelled is how many unlabelled versions an artifact
	// keeps: a self-saving page or an agent republishing every turn would
	// otherwise grow one manifest per save for ever. Labelled versions are
	// exempt.
	VersionsKeptUnlabelled = 100

	// MaxLabels bounds the labelled versions that exemption keeps.
	MaxLabels = 100

	// TrashRetention is how long a trashed artifact (or page) is
	// recoverable before the trash reaper purges it.
	TrashRetention = 30 * 24 * time.Hour

	// HistoryRetention is how long an artifact's change record keeps a row.
	HistoryRetention = 365 * 24 * time.Hour

	// OpenedDebounce bounds "recently opened" to human browsing: at most one
	// record per seat and artifact in this window.
	OpenedDebounce = 15 * time.Minute
)

// Agent guardrails, per turn segment, and the tool surface's own bounds.
const (
	// MaxInlineBytes is the decoded inline content one tool call may carry:
	// twice a model's largest output, so it never refuses honest output,
	// while anything larger belongs in a sandbox or a project file.
	MaxInlineBytes = 1 * miB

	MaxEditsPerPublish       = 64        // old_str/new_str pairs in one publish
	ToolReadPageBytes        = 48 * kiB  // one page of a file a read tool returns, inside a tool answer's budget
	TurnPublishes            = 20        // publishes per turn segment
	TurnPublishesPerArtifact = 10        // of which on one artifact
	TurnBytesWritten         = 128 * miB // bytes written per turn segment
	TurnDataOps              = 1000      // datastore ops per turn segment
)

// Datastore bounds.
const (
	MaxCollections         = 64       // per store
	MaxColumns             = 128      // per collection
	MaxDocsPerStore        = 50_000   // where a full client snapshot stops fitting a phone
	MaxDocBytes            = 64 * kiB // one document's canonical JSON
	MaxBatchOps            = 100      // ops in one write
	MaxBatchBytes          = 1 * miB  // one write's encoded ops
	MaxIndexedPaths        = 64       // per document, under the store's rows-per-insert cap
	MaxCollectionNameBytes = 48
	MaxDocIDBytes          = 128
	MaxFieldNameBytes      = 64
	MaxQueryLimit          = 500 // rows one query page returns
	MaxQueryInValues       = 100 // values one `in` filter names
	MaxHistoryValueBytes   = 4 * kiB
	MaxMigrationDocs       = 1000 // documents one schema migration step rewrites in one record

	// DataHistoryHotWindow is how long per-write history stays on the data
	// nodes. Older history is pruned in the write's own transaction.
	DataHistoryHotWindow = 7 * 24 * time.Hour

	// MaxHistoryBytesPerStore bounds one store's hot history: a quarter of
	// the default store footprint, so history never crowds out the data.
	MaxHistoryBytesPerStore = 8 * miB

	// Cost-weighted write limits, per node, in cost units of sixteen applied
	// rows (internal/artifactdata's Cost): they protect this node from one
	// client and are not a fleet quota.
	//
	// A PRINCIPAL ON A STORE takes 20 units a second, about ten cell edits on
	// a twelve-column row (two units each), with bursts of 100: more than a
	// person clicks, and an agent's pasted block of a hundred rows (about two
	// hundred units, weighed at a whole bucket) once every five seconds.
	DataWritesPerPrincipalPerSecond = 20
	DataWritesBurst                 = 100

	// A STORE takes 40 units a second (about 640 applied rows) with bursts
	// of 100. Spike DS-1 measured the three-member fleet's ceiling at about
	// 1,160 applied rows a second on a development machine, and the store
	// guard sits near half of it, so one store's writers cannot saturate the
	// one replicated writer every other domain shares; the thirty-person
	// review at its peak (15 cell edits a second, 30 units, in bursts of 50
	// edits, 100 units) fits under both bounds. The reference run
	// (docs/reference/artifact-capacity.md) confirms or moves both.
	DataWritesPerStorePerSecond = 40
	DataWritesStoreBurst        = 100
)

// Serving bounds.
const (
	// TicketLifetime is a view ticket's life, capped by the credential's own
	// absolute deadline: one working hour, after which the runtime asks the
	// host for a fresh one.
	TicketLifetime = time.Hour

	// TicketExpiredGrace is how long past its claims' expiry a ticket is
	// still recognised as one this fleet minted, so it is answered "expired,
	// mint another" rather than "unknown".
	TicketExpiredGrace = 10 * time.Minute

	// SubresourceVerdictAge is the oldest authorization verdict a
	// subresource may be served on. Documents are re-authorized on every
	// request.
	SubresourceVerdictAge = 30 * time.Second

	// PushFrameMaxBytes is the largest data push sent inline; past it a push
	// names the documents and the client re-reads.
	PushFrameMaxBytes = 64 * kiB
)

// Presence timings.
const (
	PresenceExpiry        = 45 * time.Second // three missed rebroadcasts
	PresenceRebroadcast   = 15 * time.Second
	PresenceFrameValidity = 60 * time.Second // inside the seal
	PresenceClientRate    = 10               // client frames a second per socket
	PresenceRoomTop       = 50               // members a large room presents individually
	PresenceRoomFull      = 40               // below this a large room returns to full rosters

	// PresenceCoalesceRate is how many times a second a node fans one
	// room out to its sockets, and PresenceBroadcastRate how many times a
	// second it tells the fleet what its own members did there. A cursor
	// crossing nodes waits one broadcast window on its own node and one
	// fan-out window on the viewer's: at 20 and 10 that is at most 150 ms
	// beside the broker's transit, the bound presence is held to, where
	// one window of 200 ms on each side (5 a second for both) measured a
	// median near 240 ms in spike S6. Ten frames a second is also what a
	// socket may send ([PresenceClientRate]).
	PresenceCoalesceRate  = 10
	PresenceBroadcastRate = 20

	// PresenceFrameMembers bounds the members one fan-out re-presents to a
	// socket: the most recently active first, the rest in the frames that
	// follow. A large room presents its fifty most recent members, and
	// when everybody moves, the fifty change constantly; spike S6 measured
	// about 50 KiB a second a socket at 200 viewers moving ten times a
	// second, each frame naming most of the fifty again. Ten members a
	// frame at ten frames a second bounds that near 20 KiB, and a room
	// whose members are still settles within a second.
	PresenceFrameMembers = 10
)

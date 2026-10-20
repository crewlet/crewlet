package artifacts

import "time"

// THE ONE LIMITS TABLE. Every bound an artifact, a version, an upload or a
// datastore is held to is named here, with its reason, and every surface cites
// the constant rather than restating the number. A presence room's timings are
// internal/presence's, which every kind of room shares. A
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
	MaxWorkItemBytes     = 64        // a tracker key a write is made for or a link names
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

// Folder bounds (ADR-0047: an artifact lives in one folder, and a folder grants
// nothing). Each is decided by the writer from its snapshot of the one tree
// subject, and the depth is also STAMPED into every folder record
// ([MaxFolderDepth]), so the applier enforces the bound a record was decided
// under rather than its own build's constant.
const (
	// MaxFolderDepth is how deep a folder may sit, a top-level folder at
	// depth one: four levels is a trail that still fits a 1024-pixel header
	// and a move picker that still fits one screen, and every walk of the
	// chain (a cycle check, a breadcrumb) is bounded by it. RAISE-ONLY: a
	// lower value in a later build would refuse moves under folders an older
	// build already made, while the stamp keeps old records applying the
	// bound they were decided under.
	MaxFolderDepth = 4

	// MaxFolders bounds the live folders one company holds: the whole tree
	// is one answer the dashboard reads at once (about 200 KiB at the
	// bound), and one visibility pass over it is the set the gallery's
	// folder filter is computed from.
	MaxFolders = 2000

	// MaxFolderChildren bounds the live folders directly in one folder: a
	// folder page lists its subfolders before its artifacts, unpaged, and a
	// thousand rows is past what anybody files by hand.
	MaxFolderChildren = 1000

	// MaxFolderNameBytes bounds one folder name in UTF-8 bytes: a tile line
	// and one segment of a trail. Shorter than a title, because a path of
	// four of them is what a move picker row shows.
	MaxFolderNameBytes = 80

	// TurnFolderCreates bounds the folder records one agent turn segment
	// writes (each may create a short chain, up to [MaxFolderDepth]): an
	// agent filing its work needs a folder or two, and a loop that makes a
	// folder per artifact is stopped before it buries the tree.
	TurnFolderCreates = 5

	// MaxMovesPerCall bounds the artifacts one move call files, each its own
	// record and its own decision: the tool's batch and the dashboard's bulk
	// move alike, the same figure as an upload call's assets.
	MaxMovesPerCall = 25
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

// Dashboard bounds: what one dashboard spec may declare and what one dataset
// may answer (internal/artifacts/dashspec).
const (
	// MaxDashboardWidgets bounds a board's widgets, a group's children
	// counted: twice what a screen shows without scrolling, so a board is a
	// dashboard rather than a report.
	MaxDashboardWidgets = 64

	// MaxDashboardDatasets bounds a spec's datasets: every one may be
	// fetched when the board opens, so this is also the most live reads one
	// open costs.
	MaxDashboardDatasets = 32

	// MaxDashboardParams bounds the filters a board offers, and
	// MaxParamOptions the choices one offers: a filter is a closed set, so
	// a value a viewer sends is always one the author wrote.
	MaxDashboardParams = 8
	MaxParamOptions    = 50

	// MaxCalcSteps bounds one calculated dataset's steps, and MaxCalcDepth
	// how many calculated datasets one may stand on (a join counted): a
	// calculation is a few declared steps, never a program.
	MaxCalcSteps = 16
	MaxCalcDepth = 4

	// MaxDatasetRows bounds the rows one dataset answers, a file's rows
	// included: a widget draws at most a few hundred, and five thousand is
	// what a browser holds and exports without a stall.
	MaxDatasetRows = 5000

	// MaxFileDatasetBytes bounds one CSV or JSON file a dataset reads: the
	// published contract an author already knows (an attached data file of
	// at most 8 MB), and read whole at publish and at each first read.
	MaxFileDatasetBytes = 8 * miB

	// DashboardColumns is the grid every board lays out on, the Home
	// board's; a widget is 1 to 12 columns wide and MinWidgetRows to
	// MaxWidgetRows grid rows tall, or as tall as its content.
	DashboardColumns = 12
	MinWidgetRows    = 2
	MaxWidgetRows    = 60

	// What one widget draws: a chart's series, a share's parts, a list's
	// bars and a table's columns and rows, each past what a reader takes in
	// at a glance, and a note's and a unit's text.
	MaxChartSeries   = 6
	MaxShareParts    = 8
	MaxBarRows       = 50
	DefaultBarRows   = 10
	MaxTableColumns  = 16
	MaxTableRows     = 500
	DefaultTableRows = 100
	MaxNoteBytes     = 2000
	MaxUnitBytes     = 16

	// MinRefreshSeconds is the shortest interval an engine dataset may
	// declare (D63): a minute keeps a board of thirty-two datasets inside
	// the per-viewer read budget with room for a person's own refresh.
	// MaxRefreshSeconds is a day.
	MinRefreshSeconds = 60
	MaxRefreshSeconds = 24 * 60 * 60

	// DatasetCacheTTL is how long a node keeps one viewer's answer to one
	// live dataset (ADR-0043): long enough that a board opened in three
	// tabs, or reopened, costs one domain read, and short against the
	// minute an interval may refresh at. Keyed by the viewer and their
	// grants, so no answer ever reaches a second principal.
	DatasetCacheTTL = 30 * time.Second

	// DatasetReadsPerViewerPerSecond and DatasetReadsViewerBurst are one
	// viewer's live dataset reads on one artifact, per node: a whole board
	// opens in one burst (every dataset at once, twice over for a second
	// tab), and then refreshes at most once a minute per dataset, about half
	// a read a second at the bound.
	DatasetReadsPerViewerPerSecond = 2
	DatasetReadsViewerBurst        = 2 * MaxDashboardDatasets

	// DatasetReadsPerArtifactPerSecond and DatasetReadsArtifactBurst are
	// every viewer together on one artifact, per node: a board thirty
	// people have open refreshing every dataset each minute is sixteen
	// reads a second, and the cache answers the repeats.
	DatasetReadsPerArtifactPerSecond = 20
	DatasetReadsArtifactBurst        = 10 * MaxDashboardDatasets
)

// Serving bounds.
const (
	// TicketLifetime is a view ticket's life, capped by the credential's own
	// absolute deadline: one working hour, after which the runtime asks the
	// host for a fresh one.
	TicketLifetime = time.Hour

	// PreviewTicketLifetime bounds a preview's ticket ([TicketPurposePreview]):
	// a gallery card mounts its frame as it scrolls into view and mints a new
	// one when it comes back, so a preview needs only the minutes its page
	// takes to load and settle, and a leaked one opens nothing for long.
	PreviewTicketLifetime = 5 * time.Minute

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

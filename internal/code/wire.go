package code

import (
	"encoding/base64"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE READ WIRE: the queries a reader answers and the answers it gives. They
// cross the estate wire — a node without the code copy asks one that has it —
// so every field is plain JSON, and the estate's wire gate holds them.

// ResolveQuery names a repository by its name.
type ResolveQuery struct {
	Name  string             `json:"name"`
	Fresh statelog.Freshness `json:"fresh"`
}

// RepoRef is a repository as one read found it.
type RepoRef struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Status    RepoStatus `json:"status"`
	ManagedBy ManagedBy  `json:"managed_by"`
	Owner     Owner      `json:"owner"`
	// Visibility is who besides persons reads it — with Owner, all a grant
	// reads of the repository ([GrantOf]).
	Visibility    Visibility   `json:"visibility"`
	DefaultBranch string       `json:"default_branch"`
	Description   string       `json:"description,omitempty"`
	Protections   []Protection `json:"protections,omitempty"`
	DeletedAt     time.Time    `json:"deleted_at,omitzero"`
	Refs          int64        `json:"refs"`
	LivePacks     int64        `json:"live_packs"`
	LiveBytes     int64        `json:"live_bytes"`
	// LiveObjects is how many objects the live packs hold, kept on the row
	// beside LiveBytes: the most a fetch request to the repository can
	// legitimately name, which the code host bounds a request it spools by.
	LiveObjects int64 `json:"live_objects"`
	// Position is the checkpoint the rows were read at — read in the same
	// transaction as them.
	Position statelog.Position `json:"position"`
}

// StateQuery asks for a repository's refs and packs — the snapshot a git
// node's cache converges to.
type StateQuery struct {
	RepoID string `json:"repo_id"`
	// Since is the cache's synced position: zero asks for everything. READ
	// ON THE FIRST PAGE ONLY — a later page's delta floor is the cursor's.
	Since statelog.Position  `json:"since"`
	Fresh statelog.Freshness `json:"fresh"`
	// Cursor is nil for the first page, and the Next a page answered after.
	Cursor *StateCursor `json:"cursor,omitempty"`
	// Pushes asks a DELTA for its pushes phase: every push of the repository
	// applied in (Since, the snapshot's position], in log order — what a git
	// node's cache folds into its index of which push introduced each commit
	// before the round answers, so the index is level with the copy. Ignored
	// by a full answer, whose pushes that index reads with the `introduced`
	// read instead. READ ON THE FIRST PAGE ONLY, as Since is: a later page
	// carries it in its cursor.
	Pushes bool `json:"pushes,omitempty"`
	// RetiredSince asks a FULL answer for the retirements above it — the
	// caller's own synced position, which a full answer stands in for a
	// delta from: every pack a record retired in (RetiredSince, the
	// snapshot's position], so a full round can tell a pack file a
	// compaction retired from one no row accounts for. Served only when the
	// reader can serve all of them (Snapshot: on this generation, at or past
	// the repository's retired mark, and at most MaxFullRetirements of them),
	// and the answer's RetiredFrom says whether it did. A
	// delta's retirements are always those above its own Since, but page 1
	// weighs it against the full answer with what each would serve, so asked
	// from Since itself — the position the caller holds — the retirements
	// both carry cancel rather than counting against the delta alone. Zero
	// asks for none. READ ON THE FIRST PAGE ONLY, as Since is: a later page
	// carries the floor it decided in its cursor.
	RetiredSince statelog.Position `json:"retired_since,omitzero"`
}

// The snapshot's phases, in the order a snapshot serves them. The LAYERS
// phase comes first and is served whole on page 1 alone, so no cursor names
// it: a cursor that does is refused, since nothing resumes it.
const (
	phaseLayers  = "layers"
	phasePacks   = "packs"
	phaseRefs    = "refs"
	phasePushes  = "pushes"
	phaseRetired = "retired"
	phaseTail    = "tail"
)

// StateCursor is where a snapshot resumes. OPAQUE to its caller, which hands
// back the Next it was given; every field is checked on the page it comes back
// on ([ErrBadCursor] otherwise).
type StateCursor struct {
	Full bool `json:"full"`
	// Phase is "packs", then "refs", then — on a delta whose first page
	// asked for it — "pushes", then — on a delta, and on a full answer that
	// serves retirements — "retired", then — on a snapshot of more than one
	// page — "tail".
	Phase string `json:"phase"`
	// Pushes is page 1's StateQuery.Pushes on a delta, carried to every
	// later page; false on a full answer, which serves no pushes phase.
	Pushes bool `json:"pushes,omitempty"`
	// From is page 1's checkpoint: the position the WHOLE snapshot answers
	// as of.
	From statelog.Position `json:"from"`
	// Since is page 1's q.Since: what a delta is a delta from.
	Since statelog.Position `json:"since"`
	// RetiredFrom is the floor of the snapshot's retired phase, decided on
	// page 1 and carried to every later page: Since on a delta, the
	// StateQuery.RetiredSince a full answer honoured, and zero on a full
	// answer that serves no retirements.
	RetiredFrom statelog.Position `json:"retired_from,omitzero"`
	// Seen is the highest checkpoint a page of this snapshot was read at;
	// the next page reads at session with Seen as its floor, so pages read
	// on whatever node answers are never read behind one another.
	Seen statelog.Position `json:"seen"`
	// Stream is page 1's checkpoint's stream identity: a later page keyed to
	// another stream answers ErrSnapshotMoved.
	Stream time.Time `json:"stream"`
	// After is the last key served in this phase.
	After string `json:"after"`
}

// RepoSnapshot is one page of a snapshot.
type RepoSnapshot struct {
	// Repo is the repository's row as THIS page read it.
	Repo RepoRef `json:"repo"`
	// Position is ALWAYS page 1's checkpoint: the position the whole
	// snapshot is complete as of, which a cache records once it has applied
	// every page.
	Position statelog.Position `json:"position"`
	// StreamCreated is page 1's checkpoint's stream identity, on every page.
	StreamCreated time.Time `json:"stream_created"`
	Full          bool      `json:"full"`
	// Layers is the repository's WHOLE published chain as of Position, root
	// first, served on page 1 when LayersServed — the snapshot's first phase,
	// read in page 1's own transaction, never continued on a later page: a
	// chain is at most MaxPublishedLayers rows, which fit beside a page of
	// packs. Every pack a layer names is live at Position, since nothing
	// retires a published pack but the record that replaces its layer; a
	// publication after Position is the next delta's to serve.
	Layers []LayerRow `json:"layers,omitempty"`
	// LayersServed reports that Layers is the whole chain — on every full
	// answer, an empty chain included, and on a delta exactly when the chain
	// changed above its floor. A cache replaces the chain it holds only when
	// it is set.
	LayersServed bool      `json:"layers_served,omitempty"`
	Packs        []PackRow `json:"packs,omitempty"`
	Refs         []RefRow  `json:"refs,omitempty"`
	// Pushes is the pushes phase's rows on a delta that asked for them:
	// every push applied in (Since, Position], in log order, each once
	// across the snapshot's pages — none above Position, which the next
	// delta serves.
	Pushes []IntroducedRow `json:"pushes,omitempty"`
	// Retired is the retired phase's rows: every pack a record retired in
	// (RetiredFrom, Position], in (position, pack id, part) order, each once
	// across the snapshot's pages — none above Position, whose retirement
	// the next delta serves. It names what a cache holding the retired pack
	// drops, and a delta serves a pack added above its floor and retired by
	// Position too, which its packs never served: a cache that never held
	// one has nothing to drop.
	Retired []RetiredRow `json:"retired,omitempty"`
	// RetiredFrom is the floor Retired is complete above, the same on every
	// page: Since on a delta; on a full answer the StateQuery.RetiredSince
	// it honoured, or ZERO when it served no retirements — a cache then
	// reads every pack file no row names as one no row accounts for.
	RetiredFrom statelog.Position `json:"retired_from,omitzero"`
	Next        *StateCursor      `json:"next,omitempty"`
}

// PackRow is one pack bundle a repository's rows name.
type PackRow struct {
	PackID       string            `json:"pack_id"`
	Part         string            `json:"part"`
	Object       objstore.Key      `json:"object"`
	Hash         objstore.Hash     `json:"hash"`
	Size         int64             `json:"size"`
	IndexBytes   int64             `json:"index_bytes"`
	ReverseBytes int64             `json:"reverse_bytes"`
	GitChecksum  string            `json:"git_checksum"`
	Objects      int64             `json:"objects"`
	Kind         string            `json:"kind"`
	State        string            `json:"state"`
	Added        statelog.Position `json:"added"`
}

// Ref is the PackRef a cache installs and [CheckBundle] reads.
func (r PackRow) Ref() PackRef {
	return PackRef{
		PackID: r.PackID, Object: r.Object, Hash: r.Hash, Size: r.Size,
		IndexBytes: r.IndexBytes, ReverseBytes: r.ReverseBytes,
		GitChecksum: r.GitChecksum, Objects: r.Objects,
	}
}

// Full reports a pack no compaction merges again: its pack section at least
// [FullPackBytes], or its whole bundle at least [FullBundleBytes].
//
// THE ONE PREDICATE, read by every party that judges it — the committer's
// decide of a compaction, which refuses a full input stale, and the plan a
// compaction is built from — so a plan and the decide that judges it never
// disagree about a pack. By its bundle as well as its pack section, because
// an output of a cut scaled down for a dense roll is full by its bundle with
// its pack section well short of seven eighths of the nominal cut.
func (r PackRow) Full() bool {
	return r.Ref().PackBytes() >= FullPackBytes || r.Size >= FullBundleBytes
}

// LayerRow is one published layer as the rows hold it: its place in the chain
// (Seq, 1.. root first and contiguous), the layer — Replaces empty, since a
// row is a layer rather than a replacement — and the position of the record
// that added it.
type LayerRow struct {
	Seq   int               `json:"seq"`
	Layer LayerRef          `json:"layer"`
	Added statelog.Position `json:"added"`
}

// RefRow is one ref, or — in a delta — a tombstone (an empty OID).
type RefRow struct {
	Ref      string            `json:"ref"`
	OID      string            `json:"oid,omitempty"`
	Position statelog.Position `json:"position"`
}

// RetiredRow is one pack a record retired, as a snapshot's retired phase
// serves it: which pack — its id and part, the table's key and the phase's
// keyset — the git checksum its files are named by, which is what a cache
// drops, and the instant and the position of the record that retired it.
type RetiredRow struct {
	PackID      string            `json:"pack_id"`
	Part        string            `json:"part"`
	GitChecksum string            `json:"git_checksum"`
	RetiredAt   time.Time         `json:"retired_at"`
	Position    statelog.Position `json:"position"`
}

// OutcomeQuery asks what became of one push.
type OutcomeQuery struct {
	RepoID string `json:"repo_id"`
	PushID string `json:"push_id"`
	// At is the record's position: "not found" is final only at or past it.
	At statelog.Position `json:"at"`
}

// PushOutcome is what became of one push.
type PushOutcome struct {
	Found    bool              `json:"found"`
	Push     PushRow           `json:"push,omitzero"`
	Position statelog.Position `json:"position"`
}

// PushRow is one push as its repository's history keeps it.
type PushRow struct {
	PushID          string            `json:"push_id"`
	RepoID          string            `json:"repo_id"`
	Position        statelog.Position `json:"position"`
	At              time.Time         `json:"at"`
	Actor           string            `json:"actor"`
	ActorKind       string            `json:"actor_kind"`
	ActorSeat       string            `json:"actor_seat,omitempty"`
	Node            string            `json:"node,omitempty"`
	Atomic          bool              `json:"atomic,omitempty"`
	Commands        []Command         `json:"commands"`
	PackID          string            `json:"pack_id,omitempty"`
	PackBytes       int64             `json:"pack_bytes,omitempty"`
	Introduced      []string          `json:"introduced,omitempty"`
	IntroducedCount int               `json:"introduced_count,omitempty"`
	ReceivedAt      time.Time         `json:"received_at"`
}

// PushVia is how a push's pack was built. Every push this build lands is
// received by git, so it is [ViaGit] for every row — DERIVED, never stored:
// no column records it, and no record carries it.
type PushVia string

// ViaGit is a push a client's git sent and receive-pack received.
const ViaGit PushVia = "git"

// PushVias is the closed set.
func PushVias() []PushVia { return []PushVia{ViaGit} }

// Valid reports a way this build knows.
func (v PushVia) Valid() bool { return slices.Contains(PushVias(), v) }

// PushedBy is the push that INTRODUCED a commit, as a git node's index of
// which push introduced each commit holds it: which push, where it landed on
// the log, when, by whom and how.
type PushedBy struct {
	PushID    string            `json:"push_id"`
	Position  statelog.Position `json:"position"`
	At        time.Time         `json:"at"`
	Principal Principal         `json:"principal"`
	Via       PushVia           `json:"via"`
}

// RefMove is one ref update a push LANDED — an ok command, as its row records
// it.
type RefMove struct {
	Ref    string `json:"ref"`
	Old    string `json:"old"`
	New    string `json:"new"`
	Forced bool   `json:"forced,omitempty"`
}

// IntroducedRow is one push as that index is built from it: who made it, the
// refs it moved, and the commits it introduced — at most [MaxIntroduced] of
// them listed, Count the whole.
type IntroducedRow struct {
	PushedBy
	Moves      []RefMove `json:"moves,omitempty"`
	Introduced []string  `json:"introduced,omitempty"`
	Count      int       `json:"count,omitempty"`
}

// introducedOf is a push row as the index is built from it: its principal
// from the row's actor columns, and its ok commands as the refs it moved.
func introducedOf(row PushRow) IntroducedRow {
	in := IntroducedRow{
		PushedBy: PushedBy{PushID: row.PushID, Position: row.Position, At: row.At,
			Principal: Principal{Kind: PrincipalKind(row.ActorKind), OperatorID: row.Actor, Seat: row.ActorSeat},
			Via:       ViaGit},
		Introduced: row.Introduced, Count: row.IntroducedCount,
	}
	for _, c := range row.Commands {
		if c.Result == ResultOK {
			in.Moves = append(in.Moves, RefMove{Ref: c.Ref, Old: c.Old, New: c.New, Forced: c.Forced})
		}
	}
	return in
}

// IntroducedQuery is one page of a repository's pushes in LOG ORDER: (After,
// Through] ascending — Through zero meaning the read's own checkpoint — or,
// Descending, everything below Before, newest first, down to the start of the
// push history the rows keep.
type IntroducedQuery struct {
	RepoID     string            `json:"repo_id"`
	After      statelog.Position `json:"after,omitzero"`
	Through    statelog.Position `json:"through,omitzero"`
	Before     statelog.Position `json:"before,omitzero"`
	Descending bool              `json:"descending,omitempty"`
}

// RefQuery resolves one ref of one repository.
type RefQuery struct {
	// Repo is the repository's NAME.
	Repo string `json:"repo"`
	// Ref is a whole ref (refs/heads/main), or a branch or a tag by its
	// short name — a branch first, then a tag.
	Ref string `json:"ref"`
	// Fresh is REQUIRED, as on every code point read: whoever names a ref
	// names the level it must be read at.
	Fresh statelog.Freshness `json:"fresh"`
	// Asker is who resolves it, REQUIRED and held to its grant on the
	// repository by the node that answers, as a [ReadQuery]'s is.
	Asker Principal `json:"asker"`
}

// RefAnswer is the ref a [RefQuery] resolved to.
type RefAnswer struct {
	Repo RepoRef `json:"repo"`
	// Ref is the whole ref it resolved to.
	Ref string `json:"ref"`
	OID string `json:"oid"`
	// Position is the checkpoint the ref was read at, read in the read's
	// own transaction.
	Position statelog.Position `json:"position"`
}

// ReadKind is what a [ReadQuery] lists.
type ReadKind string

const (
	// ReadRepos lists repositories, by name.
	ReadRepos ReadKind = "repos"
	// ReadRepo is one repository in detail.
	ReadRepo ReadKind = "repo"
	// ReadPushes is one repository's push history, newest first in log
	// order.
	ReadPushes ReadKind = "pushes"
	// ReadRefs is one repository's live refs under a prefix, by name; its
	// first page carries the repository in detail too.
	ReadRefs ReadKind = "refs"
	// ReadIntroduced is one repository's pushes in log order, as a git
	// node's index of which push introduced each commit reads them: the
	// engine's own read, which no principal makes.
	ReadIntroduced ReadKind = "introduced"
	// ReadCompactionDue is the repositories holding at least
	// [ReadQuery.MinPacks] live packs, MOST FIRST, each with the version
	// its row was read at: the listing a git node's compactor polls for
	// the repositories a compaction may be due in — the engine's own read,
	// which no principal makes, as `introduced` is.
	ReadCompactionDue ReadKind = "compaction_due"
	// ReadPublicationDue is the repositories holding a live pack no
	// published layer covers, LEAST RECENTLY PUBLISHED FIRST — one never
	// published before any other — each with what a publication is due by:
	// the listing a repository's maintainer polls for the repositories a
	// publication may be due in. The engine's own read, as `compaction_due`
	// is.
	ReadPublicationDue ReadKind = "publication_due"
)

// ReadKinds is the closed set.
func ReadKinds() []ReadKind {
	return []ReadKind{ReadRepos, ReadRepo, ReadPushes, ReadRefs, ReadIntroduced, ReadCompactionDue,
		ReadPublicationDue}
}

// Valid reports a kind this build knows.
func (k ReadKind) Valid() bool { return slices.Contains(ReadKinds(), k) }

// ReadQuery is a person's, a seat's or a surface's read of the code host.
type ReadQuery struct {
	Kind ReadKind `json:"kind"`
	// Asker is who reads, REQUIRED for every kind a principal reads — a
	// zero or invalid one is [ErrInvalid] — and the answer is held to its
	// grant on the node that serves it: a listing leaves out every
	// repository the asker may not read, and a read of one answers
	// [ErrNotPermitted]. Required rather than defaulted, because a default
	// would be a reader nobody named, and an optional asker is a hole any
	// caller that forgot it opens. REFUSED on `introduced`, on
	// `compaction_due` and on `publication_due`, which no principal makes:
	// an asker there would be judged by nothing.
	Asker Principal `json:"asker"`
	// Repo is the repository's NAME, for repo, pushes and refs.
	Repo string `json:"repo,omitempty"`
	// Status filters a repos listing; empty is active and archived.
	Status []RepoStatus `json:"status,omitempty"`
	// Prefix narrows a refs listing to the refs under it: "refs/heads/"
	// (the default) or "refs/tags/", or a deeper prefix ending in "/" — a
	// seat's own namespace.
	Prefix string `json:"prefix,omitempty"`
	// Introduced is an `introduced` read's range: REQUIRED for that kind,
	// refused on every other.
	Introduced *IntroducedQuery `json:"introduced,omitempty"`
	// MinPacks is a `compaction_due` read's floor — the repositories
	// holding at least this many live packs — REQUIRED, at least 1, for that
	// kind and refused on every other.
	MinPacks int    `json:"min_packs,omitempty"`
	Cursor   string `json:"cursor,omitempty"`
	// Limit is repos 1..500 (0 = 100), pushes 1..200 (0 = 50), refs
	// 1..1 000 (0 = 200) and introduced 1..MaxSnapshotItems (0 = the most);
	// a `compaction_due` page and a `publication_due` page are
	// MaxSnapshotItems and name none.
	Limit int                `json:"limit,omitempty"`
	Fresh statelog.Freshness `json:"fresh"`
}

// ReadAnswer is a [ReadQuery]'s answer.
type ReadAnswer struct {
	Repos      []RepoSummary          `json:"repos,omitempty"`
	Repo       *RepoDetail            `json:"repo,omitempty"`
	Pushes     []PushRow              `json:"pushes,omitempty"`
	Refs       []RefRow               `json:"refs,omitempty"`
	Introduced []IntroducedRow        `json:"introduced,omitempty"`
	Due        []CompactionCandidate  `json:"due,omitempty"`
	Publish    []PublicationCandidate `json:"publish,omitempty"`
	Next       string                 `json:"next,omitempty"`
	Position   statelog.Position      `json:"position"`
	Level      statelog.ReadLevel     `json:"level"`
}

// CompactionCandidate is one repository a `compaction_due` read lists: its id
// and name, how many live packs it holds and the objects they hold — what a
// compaction's memory charge is stated by before anything is synced — and the
// version its row was read at — what a compactor keys its memory of a plan
// found not due by, so a repository whose row has not moved since is not
// planned again.
type CompactionCandidate struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	LivePacks   int64             `json:"live_packs"`
	LiveObjects int64             `json:"live_objects"`
	Version     statelog.Position `json:"version"`
}

// PublicationCandidate is one repository a `publication_due` read lists: what
// a publication's due rule reads before anything is synced — the live packs
// and objects beside the published ones, so the tail no layer covers is their
// difference, the last push and the last publication with how long its build
// took, which paces the next — and the version its row was read at.
type PublicationCandidate struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	LivePacks        int64  `json:"live_packs"`
	PublishedPacks   int64  `json:"published_packs"`
	LiveObjects      int64  `json:"live_objects"`
	PublishedObjects int64  `json:"published_objects"`
	// LastPushAt is zero for a repository nobody pushed to, and PublishedAt
	// for one never published.
	LastPushAt  time.Time `json:"last_push_at,omitzero"`
	PublishedAt time.Time `json:"published_at,omitzero"`
	// PublishTook is the last publication's build.
	PublishTook time.Duration     `json:"publish_took"`
	Version     statelog.Position `json:"version"`
}

// CompactionProposal is what the node that built a compaction asks the
// repository's committer to decide ([Committer.ProposeCompaction]): the
// compaction whole. Its CompactionID is the record's operation id and its
// Seen the publish's session floor, so it carries nothing beside them.
type CompactionProposal struct {
	Compaction Compaction `json:"compaction"`
}

// CompactResult is what became of one compaction.
type CompactResult struct {
	// Outcome is applied, pending or unknown for a compaction whose record
	// was appended, and empty for one refused with none. Applied is answered
	// only from the output rows the record applied, never from the decision.
	Outcome statelog.Outcome `json:"outcome,omitempty"`
	// Refusal is why it was refused with no record — stale, repo_deleted,
	// upload_expired, pack_too_large, busy, not_applied or unavailable —
	// and Detail its one sentence.
	Refusal Refusal `json:"refusal,omitempty"`
	Detail  string  `json:"detail,omitempty"`
	// Position is where its record landed — the outputs' rows' own for an
	// applied one — and, for a not_applied or repo_deleted refusal the log
	// made after the append, where that record lies applying nowhere.
	Position statelog.Position `json:"position,omitzero"`
}

// RepoSummary is a repository as a listing renders it.
type RepoSummary struct {
	RepoRef
	// Access is the asker's standing on it ([Grant.Access] by name: read,
	// review, write or maintain) — never none, since a repository the asker
	// may not read is not listed at all.
	Access string `json:"access,omitempty"`
	// Head is the default branch's oid; empty for an empty repository.
	Head string `json:"head,omitempty"`
	// Pushes is pushes EVER.
	Pushes     int64     `json:"pushes"`
	LastPushAt time.Time `json:"last_push_at,omitzero"`
}

// RepoDetail is one repository in detail.
type RepoDetail struct {
	RepoSummary
	// Branches and Tags are the live refs under refs/heads/ and refs/tags/.
	Branches int64 `json:"branches"`
	Tags     int64 `json:"tags"`
	// Tombstones is the deleted refs the repository still holds as rows, so
	// a cache syncing a delta learns of each deletion: what its refs hold
	// beyond Refs, until a push sweeps them RefTombstoneHorizon after the
	// deletion, or sooner, oldest first, past MaxTombstonesPerRepo.
	Tombstones int64     `json:"tombstones"`
	CreatedAt  time.Time `json:"created_at"`
	// Limits are the SERVING node's epoch's: what an import chunks to.
	Limits Limits `json:"limits"`
}

// THE READS BY OBJECT ID: what a git node answers about a repository's content
// — a tree, a file's lines, a diff, a comparison — over its own cache
// (internal/code/query), never over the rows. They cross the estate wire like
// every read here, so each is plain JSON and every field is the asker's or the
// answer's.
//
// EVERY REVISION IS AN OBJECT ID, whole or abbreviated ([ValidRev]), and never
// a ref, a `^`, a `~` or a `:`: an id names the same object on every node at
// every position, so a read by one needs no level — what a ref points at is a
// row, which ResolveQuery and its kin answer — and none of git's revision
// grammar reaches the git node, whose argument lists carry validated ids and
// fixed options alone. A PATH is data, never an argument: it reaches git only
// on a batch command's standard input (internal/code/query's rule).
//
// EVERY ANSWER IS ONE PAGE, at most [MaxGitAnswerBytes] of JSON, with a cursor
// (GitAnswer.Next) where the next page starts; one item that cannot fit a page
// is refused [ErrSliceTooLarge] naming the narrowing, never cut.

// MaxGitAnswerBytes is the most one GitAnswer's JSON carries: the 64 KiB a
// seat's tool answers in (internal/agent/builtin's ToolAnswerBytes) less 8
// KiB for what a tool wraps around a page — the ids it resolved, the position,
// the cursor, at their longest — so a page is always one tool answer. Spelled
// here rather than read from builtin, which imports this package.
const MaxGitAnswerBytes = 56 << 10

// MaxGitPathBytes is the longest path a read by oid names: 4 KiB, PATH_MAX on
// Linux and far past any path a source tree holds — a bound on what the asker
// can make a git node look up, not a rule about the repository.
const MaxGitPathBytes = 4 << 10

// MaxDiffPaths is the most paths one diff's stat is filtered by: a person's or
// a model's list of the files and directories it cares about, never a
// repository's listing.
const MaxDiffPaths = 64

// MinAbbrevDigits is the fewest hex digits an abbreviated object id may have:
// git's own default abbreviation (core.abbrev), the length of every id a
// person or a model copies out of a log or a message.
const MinAbbrevDigits = 7

// maxGitCursorBytes bounds a cursor the asker hands back: every cursor a git
// node issues is a kind's letter and up to two decimal numbers.
const maxGitCursorBytes = 64

// GitReadKind is what a [GitQuery] asks. A closed set ([GitReadKinds]).
type GitReadKind string

const (
	// GitTree lists a tree: a commit's root or one of its directories, or a
	// tree named by its own id; recursively when asked.
	GitTree GitReadKind = "tree"
	// GitBlob reads a file's lines, by a commit and a path or by the blob's
	// own id; a path naming a directory answers its listing (Kind tree), so
	// one read serves both.
	GitBlob GitReadKind = "blob"
	// GitDiff is the per-file stat of the difference between two commits
	// or trees, or one file's patch.
	GitDiff GitReadKind = "diff"
	// GitCompare is how two commits relate: their merge base, which
	// contains which, the commits each has that the other lacks, and the
	// first page of what head changed since the merge base.
	GitCompare GitReadKind = "compare"
	// GitLandedIn is whether a commit is in a ref's history, and the push
	// that put it there and kept it there: the one question about history a
	// seat asks the server rather than a clone.
	GitLandedIn GitReadKind = "landed_in"
)

// GitReadKinds is the closed set.
func GitReadKinds() []GitReadKind {
	return []GitReadKind{GitTree, GitBlob, GitDiff, GitCompare, GitLandedIn}
}

// Valid reports a kind this build answers.
func (k GitReadKind) Valid() bool { return slices.Contains(GitReadKinds(), k) }

// GitQuery is one read of a repository by object id: exactly the sub-query of
// its Kind is set.
type GitQuery struct {
	Kind GitReadKind `json:"kind"`
	// RepoID is the repository's canonical id.
	RepoID string `json:"repo_id"`
	// Asker is who reads, REQUIRED — a zero or invalid one is [ErrInvalid] —
	// and held to the repository's read grant by the git node that answers,
	// from its own epoch's chart ([GrantOf]): a seat by its relation to the
	// repository, a person or a token by ADR-0055's rules, which read no
	// chart. Never trusted for anything but the grant.
	Asker Principal `json:"asker"`
	// Fresh is the asker's floor on the code log, where it holds one: the
	// answering node's copy is brought to at least it before anything is
	// read. With no floor the copy is read as it stands — an object id
	// names the same object at every position — and an id the copy lacks
	// is looked for once more at the log's head before it is answered
	// [ErrObjectNotFound]. A floor on another log is [ErrInvalid].
	Fresh statelog.Freshness `json:"fresh"`
	// Cursor is a page's Next, handed back whole; empty for the first page.
	// Opaque to the asker, and refused [ErrBadCursor] where it is not one the
	// kind issues.
	Cursor string `json:"cursor,omitempty"`
	// Home is set on the one attempt of a walk asked of its EFFECTIVE HOME —
	// the first git node of the repository's order once the walk's demotions
	// are applied (ADR-0059) — and on no other: the one node a walk lets FILL
	// a copy it does not hold. A git node holding no copy of the repository
	// answers a read without it [ErrGitUnready]{[UnreadyNotHeld]} at once,
	// having started nothing, because a cold round runs on under the cache's
	// own lifetime after its read stops waiting: with every node free to
	// fill, one read of a repository nobody held — the first after its
	// home's death — began a whole download of it on every git node the walk
	// reached. Stamped by the walk on every attempt it makes, over whatever
	// its caller set, so no caller chooses it. An estate request, not a
	// record: a build that knows no `home` reads every read as one it may
	// fill, which is this build's answer without the rule.
	Home bool `json:"home,omitempty"`

	Tree     *TreeQuery     `json:"tree,omitempty"`
	Blob     *BlobQuery     `json:"blob,omitempty"`
	Diff     *DiffQuery     `json:"diff,omitempty"`
	Compare  *CompareQuery  `json:"compare,omitempty"`
	LandedIn *LandedInQuery `json:"landed_in,omitempty"`
}

// TreeQuery lists one tree.
type TreeQuery struct {
	// Rev is a commit's id, a tag's or a tree's, whole or abbreviated.
	Rev string `json:"rev"`
	// Path is the directory under Rev's root tree to list; empty lists the
	// root. PathB64 is the same path as its exact bytes in standard base64,
	// for a path that is not valid UTF-8, which JSON cannot carry: at most
	// one of the two is set.
	Path    string `json:"path,omitempty"`
	PathB64 string `json:"path_b64,omitempty"`
	// Recursive lists every file below the directory rather than its
	// entries.
	Recursive bool `json:"recursive,omitempty"`
}

// BlobQuery reads one file's lines.
type BlobQuery struct {
	// Rev is a commit's id (or a tag's) with Path naming the file in it, or
	// a blob's own id with no Path; whole or abbreviated. A commit with no
	// Path reads its root, as a listing.
	Rev     string `json:"rev"`
	Path    string `json:"path,omitempty"`
	PathB64 string `json:"path_b64,omitempty"`
	// StartLine and EndLine are the lines to read, 1-based and inclusive;
	// zero is the file's first line and its last. A page ends where the
	// answer is full, and its Next resumes inside the range.
	StartLine int `json:"start_line,omitempty"`
	EndLine   int `json:"end_line,omitempty"`
}

// DiffQuery is the difference between two commits or trees.
type DiffQuery struct {
	// Base and Head are commit, tag or tree ids, whole or abbreviated.
	Base string `json:"base"`
	Head string `json:"head"`
	// Paths filters the stat to files equal to one of them or under one as
	// a directory, judged by the git node over git's whole answer — never
	// handed to git — at most MaxDiffPaths. Only the files they admit are
	// counted, so naming paths is what narrows a stat of a wide diff.
	Paths []string `json:"paths,omitempty"`
	// File asks for one file's patch instead of the stat: its path at Head,
	// or at Base for a file Head deleted. FileB64 is the same path as exact
	// bytes; at most one of the two is set, and neither beside Paths.
	File    string `json:"file,omitempty"`
	FileB64 string `json:"file_b64,omitempty"`
	// Renames detects a file moved between Base and Head as one renamed
	// file rather than one deleted and one added.
	Renames bool `json:"renames,omitempty"`
}

// CompareQuery is how two commits relate.
type CompareQuery struct {
	// Base and Head are commit or tag ids, whole or abbreviated.
	Base string `json:"base"`
	Head string `json:"head"`
}

// LandedInQuery asks whether a commit is in a ref's history, and which push
// landed it there.
//
// THE TARGET IS THE ONE THE ASKER WAS SHOWN: the tip it resolved, at the
// position it read it at, so the git node judges that tip and pays no barrier
// of its own — and an answer names what it was judged against, never a tip
// the asker cannot place.
type LandedInQuery struct {
	// Commit is the commit asked about, whole or abbreviated.
	Commit string `json:"commit"`
	// Target is the whole ref asked about (refs/heads/main).
	Target string `json:"target"`
	// TargetOID is the commit Target named when the asker resolved it — a
	// whole id — and At the position that resolution read it at: the git
	// node's copy is brought to At before it judges, so the moves of Target
	// up to that tip are in the index it searches.
	TargetOID string            `json:"target_oid"`
	At        statelog.Position `json:"at"`
}

// ValidRev reports whether rev names an object as a read by oid takes it: a
// whole object id (40 lowercase hex digits, not the zero id), or an
// ABBREVIATION of one, MinAbbrevDigits to 39 lowercase hex digits. Never a ref,
// a `^`, a `~` or a `:` — no revision grammar reaches the git node — and never
// an abbreviation shorter than git's own default, which would name dozens of
// objects of any large repository.
func ValidRev(rev string) bool {
	if gitref.ValidOID(rev) {
		return rev != ZeroOID
	}
	if len(rev) < MinAbbrevDigits || len(rev) >= gitref.OIDHexLen {
		return false
	}
	for i := range len(rev) {
		if c := rev[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// GitPath is the path a read names, from its Path and its PathB64 — the exact
// bytes, decoded — or [ErrInvalid] naming field: both set, base64 that does
// not decode, or a path no tree entry can be named by ([validGitPath]). root
// admits the empty path, a tree's root.
func GitPath(field, p, b64 string, root bool) (string, error) {
	if b64 != "" {
		if p != "" {
			return "", fmt.Errorf("%w: %s: give the path as %s or as %s_b64, not both", ErrInvalid, field,
				field, field)
		}
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return "", fmt.Errorf("%w: %s_b64: not standard base64: %v", ErrInvalid, field, err)
		}
		if len(raw) == 0 {
			return "", fmt.Errorf("%w: %s_b64: an empty path is given by leaving the path out", ErrInvalid, field)
		}
		p = string(raw)
	}
	if why := validGitPath(p, root); why != "" {
		return "", fmt.Errorf("%w: %s: %s", ErrInvalid, field, why)
	}
	return p, nil
}

// validGitPath is why p is no path a read by oid may name, or "": relative and
// clean — path.Clean's own form, so no leading slash, no trailing one, no
// empty, `.` or `..` component — at most MaxGitPathBytes, and no NUL, which
// would end the record git reads it in. The clean form is also what keeps
// git's object-name grammar out of a path: `<commit>:<path>` reads a path that
// begins `./` or `../` as one relative to a work tree, which a bare repository
// has none of, and git then dies over the whole batch.
func validGitPath(p string, root bool) string {
	switch {
	case p == "":
		if root {
			return ""
		}
		return "a path is required"
	case len(p) > MaxGitPathBytes:
		return fmt.Sprintf("%d bytes, past the %d a path may have", len(p), MaxGitPathBytes)
	case strings.IndexByte(p, 0) >= 0:
		return "a path holds no NUL"
	case path.Clean(p) != p || p == "." || p == ".." || strings.HasPrefix(p, "/") ||
		strings.HasPrefix(p, "../"):
		return fmt.Sprintf("%q is not a clean relative path: no leading or trailing slash, and no empty, "+
			"\".\" or \"..\" component", p)
	}
	return ""
}

// WirePath is p as an answer carries it: itself where it is valid UTF-8, and
// otherwise as much of it as JSON can carry — every invalid byte U+FFFD —
// beside the exact bytes in standard base64, the form a later read names it
// back by (a PathB64).
func WirePath(p string) (text, b64 string) {
	if utf8.ValidString(p) {
		return p, ""
	}
	return strings.ToValidUTF8(p, string(utf8.RuneError)), base64.StdEncoding.EncodeToString([]byte(p))
}

// Validate refuses a read the git node cannot answer as asked ([ErrInvalid],
// naming the field): a kind this build does not know, a repository id that is
// not canonical, an asker no principal is, a sub-query other than its kind's
// or none, a revision [ValidRev] refuses, a path [GitPath] refuses, a line
// range that runs backwards, and a diff naming both a file and paths. A
// cursor longer than any a git node issues is [ErrBadCursor]; whether a
// shorter one is the kind's own is the git node's to judge.
func (q GitQuery) Validate() error {
	if !q.Kind.Valid() {
		return fmt.Errorf("%w: kind: %q is not one of %v", ErrInvalid, q.Kind, GitReadKinds())
	}
	if id, err := uuid.Parse(q.RepoID); err != nil || id.String() != q.RepoID {
		return fmt.Errorf("%w: repo_id: %q is not a repository's id", ErrInvalid, q.RepoID)
	}
	// WHO ASKS IS NAMED, or nothing is read: the answer is held to the
	// asker's grant, and an absent asker would be judged as somebody nobody
	// named.
	if why := q.Asker.invalid(); why != "" {
		return fmt.Errorf("%w: asker: %s", ErrInvalid, why)
	}
	if len(q.Cursor) > maxGitCursorBytes {
		return fmt.Errorf("%w: cursor: %d bytes is no cursor a git read issues", ErrBadCursor, len(q.Cursor))
	}
	// THE ASKER'S FLOOR IS ON THE CODE LOG, the one log a copy is brought
	// along: a position on another names nothing a copy could reach.
	if err := q.Fresh.OnlyOn(topics.CodeLogStream); err != nil {
		return fmt.Errorf("%w: fresh: %w", ErrInvalid, err)
	}
	for _, s := range []struct {
		kind    GitReadKind
		present bool
	}{{GitTree, q.Tree != nil}, {GitBlob, q.Blob != nil}, {GitDiff, q.Diff != nil},
		{GitCompare, q.Compare != nil}, {GitLandedIn, q.LandedIn != nil}} {
		switch {
		case s.kind == q.Kind && !s.present:
			return fmt.Errorf("%w: %s: a %s read names what it reads", ErrInvalid, s.kind, s.kind)
		case s.kind != q.Kind && s.present:
			return fmt.Errorf("%w: %s: a %s read carries no %s query", ErrInvalid, s.kind, q.Kind, s.kind)
		}
	}
	switch q.Kind {
	case GitTree:
		return q.Tree.validate()
	case GitBlob:
		return q.Blob.validate()
	case GitDiff:
		return q.Diff.validate()
	case GitLandedIn:
		return q.LandedIn.validate()
	}
	return q.Compare.validate()
}

// validRev is ValidRev as a refusal naming field.
func validRev(field, rev string) error {
	if !ValidRev(rev) {
		return fmt.Errorf("%w: %s: %q is not an object id — %d to 40 lowercase hex digits, never a ref "+
			"or a revision expression; a ref is resolved to its id first", ErrInvalid, field, rev,
			MinAbbrevDigits)
	}
	return nil
}

func (q TreeQuery) validate() error {
	if err := validRev("tree.rev", q.Rev); err != nil {
		return err
	}
	_, err := GitPath("tree.path", q.Path, q.PathB64, true)
	return err
}

func (q BlobQuery) validate() error {
	if err := validRev("blob.rev", q.Rev); err != nil {
		return err
	}
	if _, err := GitPath("blob.path", q.Path, q.PathB64, true); err != nil {
		return err
	}
	switch {
	case q.StartLine < 0:
		return fmt.Errorf("%w: blob.start_line: %d; lines count from 1", ErrInvalid, q.StartLine)
	case q.EndLine < 0:
		return fmt.Errorf("%w: blob.end_line: %d; lines count from 1", ErrInvalid, q.EndLine)
	case q.EndLine > 0 && q.EndLine < max(q.StartLine, 1):
		return fmt.Errorf("%w: blob.end_line: %d is before start_line %d", ErrInvalid, q.EndLine,
			max(q.StartLine, 1))
	}
	return nil
}

func (q DiffQuery) validate() error {
	if err := validRev("diff.base", q.Base); err != nil {
		return err
	}
	if err := validRev("diff.head", q.Head); err != nil {
		return err
	}
	if len(q.Paths) > MaxDiffPaths {
		return fmt.Errorf("%w: diff.paths: %d paths, past the %d one diff is filtered by", ErrInvalid,
			len(q.Paths), MaxDiffPaths)
	}
	for i, p := range q.Paths {
		if why := validGitPath(p, false); why != "" {
			return fmt.Errorf("%w: diff.paths[%d]: %s", ErrInvalid, i, why)
		}
	}
	if q.File == "" && q.FileB64 == "" {
		return nil
	}
	if len(q.Paths) > 0 {
		return fmt.Errorf("%w: diff.file: a patch names its one file and paths filter a stat — give one "+
			"or the other", ErrInvalid)
	}
	_, err := GitPath("diff.file", q.File, q.FileB64, false)
	return err
}

func (q CompareQuery) validate() error {
	if err := validRev("compare.base", q.Base); err != nil {
		return err
	}
	return validRev("compare.head", q.Head)
}

func (q LandedInQuery) validate() error {
	if err := validRev("landed_in.commit", q.Commit); err != nil {
		return err
	}
	switch {
	case !strings.HasPrefix(q.Target, "refs/") || !ValidRef(q.Target):
		return fmt.Errorf("%w: landed_in.target: %q is not a whole ref (refs/heads/<branch>, refs/tags/<tag>): a "+
			"branch's short name is resolved to its ref first", ErrInvalid, q.Target)
	case !gitref.ValidOID(q.TargetOID) || q.TargetOID == ZeroOID:
		return fmt.Errorf("%w: landed_in.target_oid: %q is not the whole id of the tip the target was resolved to",
			ErrInvalid, q.TargetOID)
	case q.At.IsZero() || q.At.Stream != topics.CodeLogStream:
		return fmt.Errorf("%w: landed_in.at: %s is not the code log position the target was resolved at",
			ErrInvalid, q.At)
	}
	return nil
}

// GitAnswer answers a [GitQuery]: exactly the field of its Kind is set — a
// file read whose path names a directory answers Kind tree. Its JSON is at
// most MaxGitAnswerBytes.
type GitAnswer struct {
	Kind   GitReadKind `json:"kind"`
	RepoID string      `json:"repo_id"`
	// Rev is the object a tree or a file read was asked of, as its whole id
	// — a tag's is the commit or tree it names. A diff and a comparison name
	// both of theirs in their own answer.
	Rev string `json:"rev,omitempty"`
	// Position is the answering node's copy's: every row up to it is in what
	// it read, so an asker holding a later floor knows the answer may
	// predate a push it made — never that an object it names is wrong.
	Position statelog.Position `json:"position"`
	// Next is where the next page starts, handed back as the query's Cursor
	// with the query otherwise unchanged; empty on the last page.
	Next string `json:"next,omitempty"`
	// PushedBy is the push that INTRODUCED each commit the answer names —
	// its Rev, a diff's or a comparison's commits — by the commit's whole id,
	// where the answering node's index of which push introduced each commit
	// holds it. A commit it does not name was introduced outside Coverage, or
	// by a push that listed more than MaxIntroduced commits.
	PushedBy map[string]PushedBy `json:"pushed_by,omitempty"`
	// Coverage is what that index holds, on every answer of a node that keeps
	// one: an absent PushedBy means nothing beyond it.
	Coverage *Coverage `json:"coverage,omitempty"`

	Tree     *TreeAnswer     `json:"tree,omitempty"`
	Blob     *BlobAnswer     `json:"blob,omitempty"`
	Diff     *DiffAnswer     `json:"diff,omitempty"`
	Compare  *CompareAnswer  `json:"compare,omitempty"`
	LandedIn *LandedInAnswer `json:"landed_in,omitempty"`
}

// Coverage is what a git node's index of which push introduced each commit
// holds: every push with a position in (From, Through] but those in (GapFrom,
// GapThrough], a range a full read of the repository passed over and the
// index has not yet read. Complete is From being the start of the push history
// the rows keep (PushHistory), so a commit with no push inside complete
// coverage was introduced by a push that listed more than MaxIntroduced
// commits, or before that history.
type Coverage struct {
	From       statelog.Position `json:"from"`
	Through    statelog.Position `json:"through"`
	Complete   bool              `json:"complete,omitempty"`
	GapFrom    statelog.Position `json:"gap_from,omitzero"`
	GapThrough statelog.Position `json:"gap_through,omitzero"`
}

// LandedInAnswer is whether Commit is in Target's history, and the push that
// landed it.
type LandedInAnswer struct {
	// Commit is the commit asked about, whole.
	Commit string `json:"commit"`
	// Target is the whole ref, TargetOID the tip it was judged against and
	// At the position that tip was resolved at — the query's own.
	Target    string            `json:"target"`
	TargetOID string            `json:"target_oid"`
	At        statelog.Position `json:"at"`
	// Landed is Commit reachable from TargetOID.
	Landed bool `json:"landed"`
	// LandedBy is the push that made Commit reachable from Target and kept it
	// so — the first of the target's moves after which every move kept it
	// reachable, which is never one Commit was reachable from before — with
	// the move it made (Move); nil where the index this node keeps does not
	// hold that push, which Detail says.
	LandedBy *PushedBy `json:"landed_by,omitempty"`
	Move     *RefMove  `json:"move,omitempty"`
	// IntroducedBy is the push that introduced Commit itself, which may be
	// another ref's (a branch merged later).
	IntroducedBy *PushedBy `json:"introduced_by,omitempty"`
	// Probes is the ancestry checks the search for the landing ran.
	Probes int `json:"probes"`
	// Detail says why LandedBy is nil: the landing predates what the index
	// holds, falls in a range it has not read yet, or lies past the bound of
	// the search's checks (internal/code/query's MaxLandedProbes).
	Detail string `json:"detail,omitempty"`
}

// TreeAnswer is one page of a tree's listing, in git's own order.
type TreeAnswer struct {
	// Tree is the id of the tree listed.
	Tree string `json:"tree"`
	// Path is the directory listed, from the repository's root; empty for
	// the root (PathB64 per [WirePath]).
	Path    string      `json:"path,omitempty"`
	PathB64 string      `json:"path_b64,omitempty"`
	Entries []TreeEntry `json:"entries"`
	// Total is how many entries the whole listing holds, every page.
	Total int `json:"total"`
}

// TreeEntry is one entry of a listing.
type TreeEntry struct {
	// Mode is git's: 040000 a directory, 100644 a file, 100755 an
	// executable, 120000 a symbolic link, 160000 a submodule.
	Mode string `json:"mode"`
	// Type is tree, blob or commit (a submodule's).
	Type string `json:"type"`
	OID  string `json:"oid"`
	// Path is the entry's, from the repository's root (PathB64 per
	// [WirePath]).
	Path    string `json:"path"`
	PathB64 string `json:"path_b64,omitempty"`
	// Size is a blob's bytes; a directory and a submodule carry none, and an
	// empty file reads as none too.
	Size int64 `json:"size,omitempty"`
}

// BlobAnswer is one page of a file's lines.
type BlobAnswer struct {
	// Path is the file's, from the repository's root, where the read named
	// one (PathB64 per [WirePath]).
	Path    string `json:"path,omitempty"`
	PathB64 string `json:"path_b64,omitempty"`
	OID     string `json:"oid"`
	Size    int64  `json:"size"`
	// Binary is a file git calls binary — a NUL in its first 8 000 bytes,
	// git's own test — answered by its size, with no text.
	Binary bool `json:"binary,omitempty"`
	// Lines is how many lines the whole file has.
	Lines int `json:"lines"`
	// StartLine and EndLine are the lines Text holds, whole but where a page
	// begins or ends inside a line longer than a page: Text then begins
	// StartByte bytes into StartLine, and Continues says it ends inside
	// EndLine.
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
	StartByte int    `json:"start_byte,omitempty"`
	Text      string `json:"text,omitempty"`
	// Continues is set when Text ends inside line EndLine — a line longer
	// than a page, served in pieces at rune boundaries — and Next resumes
	// it. Never a cut: the rest is the next page's.
	Continues bool `json:"continues,omitempty"`
}

// DiffFile is one file of a diff's stat.
type DiffFile struct {
	// Path is the file's at head, or at base for a deleted file (PathB64
	// per [WirePath]); OldPath is where a renamed file was.
	Path       string `json:"path"`
	PathB64    string `json:"path_b64,omitempty"`
	OldPath    string `json:"old_path,omitempty"`
	OldPathB64 string `json:"old_path_b64,omitempty"`
	// Status is git's own letter: A added, D deleted, M modified, R
	// renamed, T its type changed (a file made a link, say).
	Status string `json:"status"`
	// Similarity is a rename's, in percent.
	Similarity int `json:"similarity,omitempty"`
	// OldMode and NewMode are git's modes on each side, empty on the side a
	// file is absent from: a mode alone changing — a script made executable
	// — is a file modified with no line touched, which only they tell.
	OldMode string `json:"old_mode,omitempty"`
	NewMode string `json:"new_mode,omitempty"`
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
	// Binary is a file git calls binary: no line counts, and no patch.
	Binary bool `json:"binary,omitempty"`
	// OldOID and NewOID are the blobs on each side, the side a file is
	// absent from carrying none.
	OldOID string `json:"old_oid,omitempty"`
	NewOID string `json:"new_oid,omitempty"`
}

// DiffAnswer is one page of a diff's stat, or of one file's patch.
type DiffAnswer struct {
	// Base and Head are the commits (or trees) compared, as whole ids.
	Base string `json:"base"`
	Head string `json:"head"`
	// Files is a page of the files changed, in git's order (by path); a
	// patch's answer holds its one file on every page.
	Files []DiffFile `json:"files"`
	// Total, Added and Deleted are over the whole diff the query names —
	// every file Paths admits, on every page.
	Total   int `json:"total"`
	Added   int `json:"added"`
	Deleted int `json:"deleted"`
	// Patch is a page of one file's patch (DiffQuery.File), cut at line
	// boundaries; its header lines name the paths, as git's own do.
	Patch string `json:"patch,omitempty"`
}

// CompareAnswer is how two commits relate.
type CompareAnswer struct {
	// Base and Head are the commits compared, as whole ids; MergeBase is
	// their best common ancestor, empty for histories that share none.
	Base      string `json:"base"`
	Head      string `json:"head"`
	MergeBase string `json:"merge_base,omitempty"`
	// BaseIsAncestor is head containing base — a fast-forward from base to
	// head — and HeadIsAncestor the reverse.
	BaseIsAncestor bool `json:"base_is_ancestor"`
	HeadIsAncestor bool `json:"head_is_ancestor"`
	// Ahead is the commits head has that base lacks, Behind the reverse.
	Ahead  int `json:"ahead"`
	Behind int `json:"behind"`
	// Files is the FIRST page of what head changed since the merge base —
	// the three-dot reading every code host shows; a diff of the merge base
	// and head reads the rest, and Total, Added and Deleted are over all of
	// it. Histories that share no commit have no such change, and none is
	// listed.
	Files   []DiffFile `json:"files"`
	Total   int        `json:"total"`
	Added   int        `json:"added"`
	Deleted int        `json:"deleted"`
}

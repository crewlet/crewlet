package code

import (
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
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
}

// The snapshot's phases, in the order a snapshot serves them.
const (
	phasePacks  = "packs"
	phaseRefs   = "refs"
	phasePushes = "pushes"
	phaseTail   = "tail"
)

// StateCursor is where a snapshot resumes. OPAQUE to its caller, which hands
// back the Next it was given; every field is checked on the page it comes back
// on ([ErrBadCursor] otherwise).
type StateCursor struct {
	Full bool `json:"full"`
	// Phase is "packs", then "refs", then — on a delta whose first page
	// asked for it — "pushes", then — on a snapshot of more than one page —
	// "tail".
	Phase string `json:"phase"`
	// Pushes is page 1's StateQuery.Pushes on a delta, carried to every
	// later page; false on a full answer, which serves no pushes phase.
	Pushes bool `json:"pushes,omitempty"`
	// From is page 1's checkpoint: the position the WHOLE snapshot answers
	// as of.
	From statelog.Position `json:"from"`
	// Since is page 1's q.Since: what a delta is a delta from.
	Since statelog.Position `json:"since"`
	// Seen is the highest checkpoint a page of this snapshot was read at;
	// the next page reads at session with MinPosition = Seen, so pages read
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
	Packs         []PackRow `json:"packs,omitempty"`
	Refs          []RefRow  `json:"refs,omitempty"`
	// Pushes is the pushes phase's rows on a delta that asked for them:
	// every push applied in (Since, Position], in log order, each once
	// across the snapshot's pages — none above Position, which the next
	// delta serves.
	Pushes []IntroducedRow `json:"pushes,omitempty"`
	Next   *StateCursor    `json:"next,omitempty"`
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

// RefRow is one ref, or — in a delta — a tombstone (an empty OID).
type RefRow struct {
	Ref      string            `json:"ref"`
	OID      string            `json:"oid,omitempty"`
	Position statelog.Position `json:"position"`
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
)

// ReadKinds is the closed set.
func ReadKinds() []ReadKind {
	return []ReadKind{ReadRepos, ReadRepo, ReadPushes, ReadRefs, ReadIntroduced}
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
	// caller that forgot it opens. REFUSED on `introduced`, which no
	// principal makes: an asker there would be judged by nothing.
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
	Cursor     string           `json:"cursor,omitempty"`
	// Limit is repos 1..500 (0 = 100), pushes 1..200 (0 = 50), refs
	// 1..1 000 (0 = 200) and introduced 1..MaxSnapshotItems (0 = the most).
	Limit int                `json:"limit,omitempty"`
	Fresh statelog.Freshness `json:"fresh"`
}

// ReadAnswer is a [ReadQuery]'s answer.
type ReadAnswer struct {
	Repos      []RepoSummary      `json:"repos,omitempty"`
	Repo       *RepoDetail        `json:"repo,omitempty"`
	Pushes     []PushRow          `json:"pushes,omitempty"`
	Refs       []RefRow           `json:"refs,omitempty"`
	Introduced []IntroducedRow    `json:"introduced,omitempty"`
	Next       string             `json:"next,omitempty"`
	Position   statelog.Position  `json:"position"`
	Level      statelog.ReadLevel `json:"level"`
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

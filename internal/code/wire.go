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
	ID            string       `json:"id"`
	Name          string       `json:"name"`
	Status        RepoStatus   `json:"status"`
	ManagedBy     ManagedBy    `json:"managed_by"`
	Owner         Owner        `json:"owner"`
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
}

// The snapshot's phases, in the order a snapshot serves them.
const (
	phasePacks = "packs"
	phaseRefs  = "refs"
	phaseTail  = "tail"
)

// StateCursor is where a snapshot resumes. OPAQUE to its caller, which hands
// back the Next it was given; every field is checked on the page it comes back
// on ([ErrBadCursor] otherwise).
type StateCursor struct {
	Full bool `json:"full"`
	// Phase is "packs", then "refs", then — on a snapshot of more than one
	// page — "tail".
	Phase string `json:"phase"`
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
	StreamCreated time.Time    `json:"stream_created"`
	Full          bool         `json:"full"`
	Packs         []PackRow    `json:"packs,omitempty"`
	Refs          []RefRow     `json:"refs,omitempty"`
	Next          *StateCursor `json:"next,omitempty"`
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

// ReadKind is what a [ReadQuery] lists.
type ReadKind string

const (
	// ReadRepos lists repositories, by name.
	ReadRepos ReadKind = "repos"
	// ReadRepo is one repository in detail.
	ReadRepo ReadKind = "repo"
	// ReadPushes is one repository's push history, newest first.
	ReadPushes ReadKind = "pushes"
)

// ReadKinds is the closed set.
func ReadKinds() []ReadKind { return []ReadKind{ReadRepos, ReadRepo, ReadPushes} }

// Valid reports a kind this build knows.
func (k ReadKind) Valid() bool { return slices.Contains(ReadKinds(), k) }

// ReadQuery is a person's or a surface's read of the code host.
type ReadQuery struct {
	Kind ReadKind `json:"kind"`
	// Repo is the repository's NAME, for repo and pushes.
	Repo string `json:"repo,omitempty"`
	// Status filters a repos listing; empty is active and archived.
	Status []RepoStatus `json:"status,omitempty"`
	Cursor string       `json:"cursor,omitempty"`
	// Limit is repos 1..500 (0 = 100) and pushes 1..200 (0 = 50).
	Limit int                `json:"limit,omitempty"`
	Fresh statelog.Freshness `json:"fresh"`
}

// ReadAnswer is a [ReadQuery]'s answer.
type ReadAnswer struct {
	Repos    []RepoSummary      `json:"repos,omitempty"`
	Repo     *RepoDetail        `json:"repo,omitempty"`
	Pushes   []PushRow          `json:"pushes,omitempty"`
	Next     string             `json:"next,omitempty"`
	Position statelog.Position  `json:"position"`
	Level    statelog.ReadLevel `json:"level"`
}

// RepoSummary is a repository as a listing renders it.
type RepoSummary struct {
	RepoRef
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
	// deletion.
	Tombstones int64     `json:"tombstones"`
	CreatedAt  time.Time `json:"created_at"`
	// Limits are the SERVING node's epoch's: what an import chunks to.
	Limits Limits `json:"limits"`
}

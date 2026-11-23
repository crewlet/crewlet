package gitlet

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"path"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/deployfiles"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/routing"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
)

// THE READ WIRE: the queries a reader answers and the answers it gives. They
// cross the estate wire (a node without the code copy asks one that has it), so
// every field is plain JSON, held by the estate's wire gate.

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
	// Kind is what the repository is for: a project, or a seat's scratch
	// repository ([RepoKind]).
	Kind  RepoKind `json:"kind"`
	Owner Owner    `json:"owner"`
	// Visibility is who besides persons reads it — with Owner, all a grant
	// reads of the repository ([GrantOf]).
	Visibility Visibility `json:"visibility"`
	// Search is how code search reads it ([SearchMode]).
	Search SearchMode `json:"search"`
	// Mirrored is a PULL MIRROR ([RepoSettings.MirrorFrom]): only its syncs
	// move it, so admission and an engine write's landing refuse a push or
	// change write repo_mirrored. Never the mirror's URL or credential, which
	// only [RepoDetail.Remotes] serves: every push and fetch reads this row.
	// An answer's field, so no versionedFields row.
	Mirrored      bool   `json:"mirrored,omitempty"`
	DefaultBranch string `json:"default_branch"`
	// Description is what the repository says about itself, a line whoever
	// created or last changed it chose: somebody's words (ADR-0040).
	Description string `json:"description,omitempty" untrusted:"name"`
	// Protections are its rules AS A PUSH IS JUDGED BY THEM ([PushRule]),
	// sorted by pattern, never a merge gate: every row read here is on the hot
	// path and gates run to half a MiB of JSON per repository at the domain's
	// bounds ([ReadQuery.Gates], [RepoDetail.Gates]).
	Protections []PushRule `json:"protections,omitempty"`
	DeletedAt   time.Time  `json:"deleted_at,omitzero"`
	// LastActivityAt is the broker's instant of the last push, rename,
	// settings change or restore: the IDLE CLOCK a scratch repository expires
	// by ([Retention]) and a turn's hold records by ([HoldRecordAfter]). Never
	// a read or a hold; a stale session read only makes a hold record needlessly.
	LastActivityAt time.Time `json:"last_activity_at,omitzero"`
	Refs           int64     `json:"refs"`
	LivePacks      int64     `json:"live_packs"`
	LiveBytes      int64     `json:"live_bytes"`
	// LiveObjects is how many objects the live packs hold: the most a fetch
	// request can legitimately name, which bounds a request Gitlet spools.
	LiveObjects int64 `json:"live_objects"`
	// DropEpoch counts the expunges begun ([ExpungeBegin]). A push carries the
	// epoch of the row its receiver checked; the committer refuses it
	// `expunging` once it has moved.
	DropEpoch int64 `json:"drop_epoch,omitempty"`
	// Redirect is the current name where asked by an OLD name, redirecting
	// until RedirectUntil after a rename ([RedirectWindow]). It ends with the
	// release removing the old name's row, so nodes at one position answer
	// alike, which comparing node clocks would not.
	Redirect      string    `json:"redirect,omitempty"`
	RedirectUntil time.Time `json:"redirect_until,omitzero"`
	// Position is the checkpoint the rows were read at, in the same
	// transaction.
	Position statelog.Position `json:"position"`
}

// StateQuery asks for a repository's refs and packs — the snapshot a Gitlet
// node's cache converges to.
type StateQuery struct {
	RepoID string `json:"repo_id"`
	// Since is the cache's synced position: zero asks for everything. READ
	// ON THE FIRST PAGE ONLY — a later page's delta floor is the cursor's.
	Since statelog.Position  `json:"since"`
	Fresh statelog.Freshness `json:"fresh"`
	// Cursor is nil for the first page, and the Next a page answered after.
	Cursor *StateCursor `json:"cursor,omitempty"`
	// Pushes asks a DELTA for its pushes phase: every push applied in (Since,
	// snapshot position], in log order, which the cache folds into its index of
	// which push introduced each commit. A full answer ignores it (that index
	// reads `introduced`). READ ON THE FIRST PAGE ONLY; the cursor carries it.
	Pushes bool `json:"pushes,omitempty"`
	// RetiredSince, the caller's synced position, asks a FULL answer for every
	// pack a record retired in (RetiredSince, snapshot position], so a full
	// round can tell a compaction-retired pack file from one no row accounts
	// for. Served only when the reader can serve all of them (this generation,
	// at or past the retired mark, at most MaxFullRetirements); the answer's
	// RetiredFrom says whether it did. Page 1 weighs delta against full by what
	// each would serve, so asked from Since the shared retirements cancel.
	// Zero asks for none. READ ON THE FIRST PAGE ONLY; the cursor carries it.
	RetiredSince statelog.Position `json:"retired_since,omitzero"`
}

// The snapshot's phases, in serving order. LAYERS is served whole on page 1, so
// no cursor names it; one that does is refused.
const (
	phaseLayers  = "layers"
	phasePacks   = "packs"
	phaseRefs    = "refs"
	phasePushes  = "pushes"
	phaseRetired = "retired"
	phaseTail    = "tail"
)

// StateCursor is where a snapshot resumes. OPAQUE to its caller, which hands
// back the Next it was given; every field is checked on its return
// ([ErrBadCursor] otherwise).
type StateCursor struct {
	Full bool `json:"full"`
	// Phase runs "packs", "refs", "pushes" (a delta that asked), "retired" (a
	// delta, or a full answer serving retirements), then "tail".
	Phase string `json:"phase"`
	// Pushes is page 1's StateQuery.Pushes on a delta; false on a full answer.
	Pushes bool `json:"pushes,omitempty"`
	// From is page 1's checkpoint: the position the WHOLE snapshot answers as of.
	From statelog.Position `json:"from"`
	// Since is page 1's q.Since: what a delta is from.
	Since statelog.Position `json:"since"`
	// RetiredFrom is the retired phase's floor, decided on page 1: Since on a
	// delta, the RetiredSince a full answer honoured, else zero.
	RetiredFrom statelog.Position `json:"retired_from,omitzero"`
	// Seen is the highest checkpoint a page was read at; the next page reads
	// at session with it as floor, so pages are never read behind each other
	// whatever node answers.
	Seen statelog.Position `json:"seen"`
	// Stream is page 1's stream identity: another stream answers ErrSnapshotMoved.
	Stream time.Time `json:"stream"`
	// DropEpoch is the drop epoch page 1 read; another answers
	// ErrSnapshotMoved, so every page is read at the epoch its Repo carries,
	// which decides a push checked against the synced copy ([Proposal.DropEpoch]).
	DropEpoch int64 `json:"drop_epoch,omitempty"`
	// After is the last key served in this phase.
	After string `json:"after"`
}

// RepoSnapshot is one page of a snapshot.
type RepoSnapshot struct {
	// Repo is the repository's row as THIS page read it.
	Repo RepoRef `json:"repo"`
	// Position is ALWAYS page 1's checkpoint: the position the whole snapshot
	// is complete as of, which a cache records once it has applied every page.
	Position statelog.Position `json:"position"`
	// StreamCreated is page 1's stream identity, on every page.
	StreamCreated time.Time `json:"stream_created"`
	Full          bool      `json:"full"`
	// Layers is the WHOLE published chain as of Position, root first, served
	// on page 1 when LayersServed and never continued: at most
	// MaxPublishedLayers rows fit beside a page of packs. Every pack a layer
	// names is live at Position, since only the record replacing its layer
	// retires a published pack.
	Layers []LayerRow `json:"layers,omitempty"`
	// LayersServed reports that Layers is the whole chain: on every full
	// answer (an empty chain included) and on a delta exactly when the chain
	// changed above its floor. A cache replaces its chain only when set.
	LayersServed bool      `json:"layers_served,omitempty"`
	Packs        []PackRow `json:"packs,omitempty"`
	Refs         []RefRow  `json:"refs,omitempty"`
	// Pushes is the pushes phase on a delta that asked: every push applied in
	// (Since, Position], in log order, each once across the pages.
	Pushes []IntroducedRow `json:"pushes,omitempty"`
	// Retired is the retired phase: every pack a record retired in
	// (RetiredFrom, Position], in (position, pack id, part) order, each once.
	// It names what a cache drops; a delta also serves a pack added above its
	// floor and retired by Position, which its packs never served.
	Retired []RetiredRow `json:"retired,omitempty"`
	// RetiredFrom is the floor Retired is complete above, the same on every
	// page: Since on a delta; on a full answer the RetiredSince it honoured, or
	// ZERO when it served none, and a cache then treats every pack file no row
	// names as unaccounted for.
	RetiredFrom statelog.Position `json:"retired_from,omitzero"`
	Next        *StateCursor      `json:"next,omitempty"`
}

// PackRow is one pack bundle a repository's rows name.
type PackRow struct {
	PackID       string        `json:"pack_id"`
	Part         string        `json:"part"`
	Object       objstore.Key  `json:"object"`
	Hash         objstore.Hash `json:"hash"`
	Size         int64         `json:"size"`
	IndexBytes   int64         `json:"index_bytes"`
	ReverseBytes int64         `json:"reverse_bytes"`
	GitChecksum  string        `json:"git_checksum"`
	Objects      int64         `json:"objects"`
	// Kind is the record that added it, and State whether the repository
	// holds it live or an expunge's begin has left it dropping
	// ([PackDropping]).
	Kind  PackKind          `json:"kind"`
	State PackState         `json:"state"`
	Added statelog.Position `json:"added"`
	// Content is the position the pack's CONTENT dates from for a purge:
	// nothing in it reached the repository, or was last judged by a purge's
	// scan, before it. A push's pack dates from its own position; a
	// compaction's outputs and an expunge's rescue or kept remainder (of a ref
	// drop within a grace) from the oldest pack replaced, since moving content
	// judges none of it; a purge's kept and dropping packs from its scan
	// ([ExpungeBegin.Seen]). A purge request is SERVED once no pack dates from
	// before it, and a cycle serving one takes the live packs that do, whenever
	// their rows were added (a rewrite moves those past the request without
	// judging anything). A read's answer: no versionedFields row.
	Content statelog.Position `json:"content"`
	// Arrived is the broker instant the pack's NEWEST content arrived: a push's
	// or merge's pack at its record; a rewrite's output at the latest pack it
	// replaced. An expunge on its maintainer's schedule keeps whole a pack that
	// arrived within [PruneGrace]. Zero where unknown (a pack a cache adopted
	// before a round served its row). A read's answer: no versionedFields row.
	Arrived time.Time `json:"arrived,omitzero"`
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
// [FullPackBytes], or its whole bundle at least [FullBundleBytes] (FullAt at
// [CompactPackBytes]).
//
// THE ONE PREDICATE, read by the committer's decide of a compaction (which
// refuses a full input stale) and by its plan (internal/gitlet/compact), so
// they never disagree. By bundle too: an output of a cut scaled down for a
// dense roll is full by bundle with its pack section well short of 7/8 of cut.
func (r PackRow) Full() bool { return r.FullAt(CompactPackBytes) }

// FullAt is [PackRow.Full] at cut: both bounds scaled by cut ÷
// CompactPackBytes. A suite compacts over megabytes, and its outputs must be
// full by the same rule or each would be rolled again. Defined for cut in
// (0, CompactPackBytes].
func (r PackRow) FullAt(cut int64) bool {
	return r.Ref().PackBytes() >= fullScaled(FullPackBytes, cut) || r.Size >= fullScaled(FullBundleBytes, cut)
}

// fullScaled is bound at a cut of cut, in whole bytes; both bounds are at most
// CompactPackBytes, so the product is under 2^60.
func fullScaled(bound, cut int64) int64 { return bound * cut / CompactPackBytes }

// LayerRow is one published layer: its place in the chain (Seq, 1.. root first,
// contiguous), the layer (Replaces empty) and the adding record's position.
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

// RetiredRow is one pack a record retired, as the retired phase serves it: id
// and part (the phase's keyset), the git checksum naming its files, and the
// retiring record's instant and position.
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
	// Change asks for the change a push opened ([PushOutcome.Opened]). A
	// request's field landed with the domain: no versionedFields row.
	Change bool `json:"change,omitempty"`
}

// PushOutcome is what became of one push.
type PushOutcome struct {
	Found    bool              `json:"found"`
	Push     PushRow           `json:"push,omitzero"`
	Position statelog.Position `json:"position"`
	// Opened is the number of the change the push opened ([PushChangeID]), read
	// in its row's snapshot where the query asked; 0 otherwise.
	Opened int64 `json:"opened,omitempty"`
}

// PushRow is one push as its repository's history keeps it.
type PushRow struct {
	PushID   string            `json:"push_id"`
	RepoID   string            `json:"repo_id"`
	Position statelog.Position `json:"position"`
	At       time.Time         `json:"at"`
	// Actor is who pushed: an operator's credential or a seat's handle;
	// ActorKind the principal's kind, ActorSeat the seat (human or itself).
	Actor     string `json:"actor"`
	ActorKind string `json:"actor_kind"`
	ActorSeat string `json:"actor_seat,omitempty"`
	// Via is how the pack was built; Run, for a seat's push, the turn, work item
	// and model (a coding box adds run and harness), stamped by the engine, never
	// read from a commit.
	Via      PushVia       `json:"via"`
	Run      RunProvenance `json:"run,omitzero"`
	Node     string        `json:"node,omitempty"`
	Atomic   bool          `json:"atomic,omitempty"`
	Commands []Command     `json:"commands"`
	// Change is the change this push's own record merged on a branch one of its
	// commands set (a merge's or queue landing's, else the lowest-numbered);
	// ChangesMore counts the others. Answered on push history rows
	// ([Reader.pushes]); nil where none merged.
	Change      *ChangeName `json:"change,omitempty"`
	ChangesMore int         `json:"changes_more,omitempty"`
	// Omitted counts by outcome the commands a row read IN BRIEF
	// ([ReadQuery.Brief]) leaves out of Commands, so a reader says what it was
	// not shown. Absent when nothing is left out.
	Omitted         CommandCounts `json:"omitted,omitzero"`
	PackID          string        `json:"pack_id,omitempty"`
	PackBytes       int64         `json:"pack_bytes,omitempty"`
	Introduced      []string      `json:"introduced,omitempty"`
	IntroducedCount int           `json:"introduced_count,omitempty"`
	// Offered is an attempt's commits offered ([PushEntry.Offered]); absent on
	// other rows, uncounted attempts and rows before [OfferedVersion].
	Offered    int       `json:"offered,omitempty"`
	ReceivedAt time.Time `json:"received_at"`
}

// CommandCounts counts a push's commands by outcome: made, moved, moved by
// force, removed, unchanged (asked for the value it had) or refused. Each is
// exactly one, decided in that order ([CommandCounts.add]), as the dashboard's
// commandWord does.
type CommandCounts struct {
	Created   int `json:"created,omitempty"`
	Moved     int `json:"moved,omitempty"`
	Forced    int `json:"forced,omitempty"`
	Deleted   int `json:"deleted,omitempty"`
	Unchanged int `json:"unchanged,omitempty"`
	Refused   int `json:"refused,omitempty"`
}

// add counts one command by what became of it.
func (n *CommandCounts) add(c Command) {
	switch {
	case c.Result == ResultNG:
		n.Refused++
	case c.Result == ResultNoop:
		n.Unchanged++
	case c.Old == ZeroOID:
		n.Created++
	case c.New == ZeroOID:
		n.Deleted++
	case c.Forced:
		n.Forced++
	default:
		n.Moved++
	}
}

// brief is a push row as a history read IN BRIEF serves it ([ReadQuery.Brief]):
// introduced commits counted, not listed, and of a push of more than
// BriefCommands commands, that many (refused first with reasons, then the rest,
// each in client order), the rest counted in Omitted.
//
// REFUSALS FIRST: what did not happen is what a reader acts on; cut in send
// order, one refused tag among a thousand would be hidden.
func (row PushRow) brief() PushRow {
	row.Introduced = nil
	if len(row.Commands) <= BriefCommands {
		return row
	}
	refusals := 0
	for _, c := range row.Commands {
		if c.Result == ResultNG {
			refusals++
		}
	}
	keepRefused := min(refusals, BriefCommands)
	// brief is the refused kept, then the rest, in the room left.
	brief := make([]Command, 0, BriefCommands)
	rest := make([]Command, 0, BriefCommands-keepRefused)
	for _, c := range row.Commands {
		switch {
		case c.Result == ResultNG && len(brief) < keepRefused:
			brief = append(brief, c)
		case c.Result != ResultNG && len(rest) < BriefCommands-keepRefused:
			rest = append(rest, c)
		default:
			row.Omitted.add(c)
		}
	}
	brief = append(brief, rest...)
	row.Commands = brief
	return row
}

// PushVia is how a push's pack was built: STAMPED by its builder, carried by
// the proposal and record entry, stored on the push row, never derived from
// commits, whose committer header any client may write. The row's column has
// no domain default and every reader refuses an unknown way (scanPush), so an
// apply that forgot the column cannot read as one way or another.
type PushVia string

const (
	// ViaGit is a push a client's git sent and receive-pack received.
	ViaGit PushVia = "git"
	// ViaEngine is a push whose commit and pack the engine built from a tool's
	// call: exactly one ref moved, never --atomic ([Proposal.Validate]).
	ViaEngine PushVia = "engine"
	// ViaMerge is the push a MERGE RECORD lands ([Merge]): target onto the merge
	// commit (source deleted where the merge deletes it), written by the merge's
	// apply so history names every branch move. No proposal names it: a merge is
	// decided through its gate ([Committer.ProposeMerge]). BASE FORMAT, as
	// merge.go's head says.
	ViaMerge PushVia = "merge"
	// ViaExpiry is the ref lifecycle's deletion of seats' idle branches
	// (retention_expiry.go): a push by [PrincipalSystem], the one kind it pairs
	// with, that only deletes refs in seats' namespaces with no pack
	// ([Proposal.Validate]), each Old the commit its row keeps for a restore.
	// Its decide keeps whatever is no longer idle (rule 4b, [ExpiryRule]); its
	// apply moves no activity clock (apply_push.go). BASE FORMAT.
	ViaExpiry PushVia = "expiry"
	// ViaQueue is a push a LAND RECORD lands ([Land]), one per change a merge
	// queue landed: target from what the prior change left onto the queue's
	// commit (source deleted where it deletes), naming the ENQUEUER, since the
	// queue never acts on its own account. No proposal names it. BASE FORMAT, as
	// queuerecords.go's head says.
	ViaQueue PushVia = "queue"
	// ViaImport is the push an IMPORT CHUNK lands ([ImportChunk]): refs set onto
	// objects the home fetched from the source host, by [PrincipalSystem]. No
	// proposal names it. BASE FORMAT, as [OpImportChunk] is.
	ViaImport PushVia = "import"
	// ViaMirror is the push a PULL MIRROR'S SYNC lands ([MirrorSync]): refs moved
	// onto its source's, by the engine's own account. No proposal names it. BASE
	// FORMAT, as [OpMirrorSync] is.
	ViaMirror PushVia = "mirror"
)

// PushVias is the closed set, in its documented order.
func PushVias() []PushVia {
	return []PushVia{ViaGit, ViaEngine, ViaMerge, ViaExpiry, ViaQueue, ViaImport, ViaMirror}
}

// Valid reports a way this build knows.
func (v PushVia) Valid() bool { return slices.Contains(PushVias(), v) }

// proposedVias is the ways a push PROPOSAL may name: merge and landing pushes
// are written by their records alone.
func proposedVias() []PushVia { return []PushVia{ViaGit, ViaEngine, ViaExpiry} }

// proposed reports a way a push proposal may name ([proposedVias]).
func (v PushVia) proposed() bool { return slices.Contains(proposedVias(), v) }

// PushedBy is the push that INTRODUCED a commit, as a Gitlet node's introduced
// index holds it: which push, its log position, when, by whom and how.
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
// refs it moved, and the commits it introduced, at most [MaxIntroduced] listed,
// Count the whole.
type IntroducedRow struct {
	PushedBy
	Moves      []RefMove `json:"moves,omitempty"`
	Introduced []string  `json:"introduced,omitempty"`
	Count      int       `json:"count,omitempty"`
}

// introducedOf is a push row as the index is built from it, its ok commands as
// the refs it moved.
func introducedOf(row PushRow) IntroducedRow {
	in := IntroducedRow{
		PushedBy: PushedBy{PushID: row.PushID, Position: row.Position, At: row.At,
			Principal: row.principal(), Via: row.Via},
		Introduced: row.Introduced, Count: row.IntroducedCount,
	}
	for _, c := range row.Commands {
		if c.Result == ResultOK {
			in.Moves = append(in.Moves, RefMove{Ref: c.Ref, Old: c.Old, New: c.New, Forced: c.Forced})
		}
	}
	return in
}

// principal inverts the push apply's [Principal.Actor]: an operator's
// credential and its bound seat, or a seat by its handle with its push's run.
func (row PushRow) principal() Principal {
	if PrincipalKind(row.ActorKind) == PrincipalAgent {
		return Principal{Kind: PrincipalAgent, Seat: row.ActorSeat, Run: row.Run}
	}
	return Principal{Kind: PrincipalKind(row.ActorKind), OperatorID: row.Actor, Seat: row.ActorSeat}
}

// IntroducedQuery is one page of a repository's pushes in LOG ORDER: (After,
// Through] ascending, Through zero meaning the read's own checkpoint; or,
// Descending, everything below Before, newest first, down to the start of the
// kept history.
type IntroducedQuery struct {
	RepoID     string            `json:"repo_id"`
	After      statelog.Position `json:"after,omitzero"`
	Through    statelog.Position `json:"through,omitzero"`
	Before     statelog.Position `json:"before,omitzero"`
	Descending bool              `json:"descending,omitempty"`
}

// RunPushesQuery names the executor a `run_pushes` read lists the pushes of:
// the run by launch id ([RunProvenance.RunID]), the repositories it was given
// by id, and the code log position its launch read them at, above which its
// box's pushes applied.
//
// THE READ SCANS each repository's pushes above After over
// code_pushes_repo_idx, keeping the run's, in pages held to what they READ
// (limit and MaxPageBytes, weighed by the index before a row is touched): a
// long run costs more pages, never a bigger one. An index on the run would hold
// an entry per box push for 181 days for one read a run (BenchmarkRunPushes).
type RunPushesQuery struct {
	Run   string            `json:"run"`
	Repos []string          `json:"repos"`
	After statelog.Position `json:"after"`
}

// BranchChangesQuery is a `branch_changes` read: the repositories a collected
// executor was given, one to [MaxRunRepos], each by id once, each with the
// branches (whole refs under refs/heads/, once, [MaxRefsPerQuery] in all)
// whose live changes the read answers, by which the engine names each
// delivered ref's change. One seek of code_changes_open_idx a branch, so the
// read costs its branches, never the repositories' changes.
//
// EVERY REPOSITORY IN ONE READ: one transaction at one checkpoint, one round
// trip from a node without the data role.
type BranchChangesQuery struct {
	Repos []RepoBranches `json:"repos"`
}

// RepoBranches is one repository of a [BranchChangesQuery]: its id, and the
// branches of it whose live changes the read answers.
type RepoBranches struct {
	RepoID string   `json:"repo_id"`
	Refs   []string `json:"refs"`
}

// BranchChange is the live change one branch of a `branch_changes` read heads.
type BranchChange struct {
	RepoID   string `json:"repo_id"`
	Ref      string `json:"ref"`
	ChangeID string `json:"change_id"`
	Number   int64  `json:"number"`
}

// ChangesDueQuery is a `changes_due` read: the repositories a home owes change
// work in, or ONE repository's due change work, each judged against the
// instant the home's merge recheck is paced by.
type ChangesDueQuery struct {
	// RepoID is "" for the listing of repositories, else the repository whose
	// due changes the read pages.
	RepoID string `json:"repo_id,omitempty"`
	// RecheckBefore is REQUIRED: a change whose mergeability was recorded
	// undecided, or against a tip its target has since moved from, is due again
	// once that record's broker instant is at or before it (the home's clock
	// less home's MergeabilityRefresh), so a target moving on every push is
	// rechecked at the home's pace, never its own.
	RecheckBefore time.Time `json:"recheck_before"`
	// Rechecks is, with a repository, the most changes due a recheck the read
	// names on its first page ([ReadAnswer.Rechecks]), oldest first, 0 to
	// MaxSnapshotItems; none past the first page or without a repository.
	Rechecks int `json:"rechecks,omitempty"`
	// ChartEpoch is the activation of the asking home's chart
	// (configplane.ActivationStamp), 0 for none: a change whose readiness an
	// EARLIER chart judged ([Mergeability.ChartEpoch]) is due a refresh, so a
	// chart apply re-judges every live change once, the one readiness input no
	// record moves. Never negative. BASE FORMAT, as [OpMergeability] is.
	ChartEpoch int64 `json:"chart_epoch,omitempty"`
}

// ChangeDue is one live change a `changes_due` read names, targeting a live
// branch, and what its home owes it: its head revision's FACTS where it holds
// none against the target, its MERGE where it holds no mergeability of that
// revision against that target, or, among rechecks, a mergeability recorded at
// Checked that is undecided or that its target has moved past. It carries all
// the home needs, from one snapshot: id, number, head revision and head, the
// source branch (the listing's order and cursor), and the target with the rows'
// tip and the position of the record that moved it. A `checks_due` read names
// changes in the same shape, owing neither facts nor a merge.
type ChangeDue struct {
	ID          string            `json:"id"`
	Number      int64             `json:"number"`
	Revision    int64             `json:"revision"`
	Head        string            `json:"head"`
	SourceRef   string            `json:"source_ref"`
	TargetRef   string            `json:"target_ref"`
	Tip         string            `json:"tip"`
	TipPosition statelog.Position `json:"tip_position"`
	// FactsDue, MergeDue and RefreshDue say what the home owes a due change of
	// a `changes_due` page: head revision's facts, merge, and a refresh of its
	// mergeability's readiness ([Mergeability.Refresh]). A recheck owes its merge
	// alone and says none; a `checks_due` change says what it owes in Checks.
	// RefreshDue is an estate answer's field: no versionedFields row.
	FactsDue   bool `json:"facts_due,omitempty"`
	MergeDue   bool `json:"merge_due,omitempty"`
	RefreshDue bool `json:"refresh_due,omitempty"`
	// Checked is a recheck's: the instant its mergeability was recorded, which
	// orders rechecks; zero otherwise.
	Checked time.Time `json:"checked,omitzero"`
	// Checks is what a `checks_due` change owes its checks ([CheckWork]); nil on
	// `changes_due` reads.
	Checks *CheckWork `json:"checks,omitempty"`
}

// AskDue is one change whose review asks its home owes a settling
// ([AskSettle]): the change, and the state its asks move to (ended for a closed
// or merged change's waiting asks, requested for a reopened change's).
type AskDue struct {
	ChangeID string        `json:"change_id"`
	State    ReviewerState `json:"state"`
}

// MaxAskSettlesPerRead is the most changes one `changes_due` first page names
// whose asks are due a settling ([ReadAnswer.AskSettles]): one home pass's
// worth. SIXTY-FOUR, as a pass's merges are held to (home's
// MergeabilityPerPass); the rest stay due for the next pass.
const MaxAskSettlesPerRead = 64

// MaxRunRepos is the most repositories one [RunPushesQuery] names: the most an
// executor is given (each costs a clone inside its setup budget). The granting
// bound (sandbox.MaxRunRepos) may not pass it: a run given more could not be
// read whole. internal/engine's
// TestARunGivenItsMostRepositoriesHasItsPushesReadWhole holds the two together.
const MaxRunRepos = 8

// RefQuery resolves refs of one repository, EVERY ONE IN ONE READ: one
// transaction at one checkpoint, so a linearizable resolve of a comparison's
// two sides pays one barrier and compares them at the same moment. Each name
// is answered on its own ([RefAnswer.Refs]).
type RefQuery struct {
	// Repo is the repository's NAME.
	Repo string `json:"repo"`
	// Refs is every ref the read resolves, one to [MaxRefsPerQuery]: a whole
	// ref (refs/heads/main), a short name (a branch first, then a tag), or
	// [HeadRef], the default branch read from the row in the read's own
	// transaction.
	//
	// HEAD IS RESOLVED BY THE READ, NEVER BY ITS ASKER: reading the row first
	// costs a second router hop for the commonest question and names a branch a
	// rename or settings change may have moved in between. No versionedFields
	// row: a query's meaning is no record's.
	Refs []string `json:"refs"`
	// Fresh is REQUIRED, as on every code point read.
	Fresh statelog.Freshness `json:"fresh"`
	// Asker is who resolves them, REQUIRED and held to its grant by the
	// answering node, as a [ReadQuery]'s is.
	Asker Principal `json:"asker"`
	// CeilingSeat is the seat the asking turn acts for, whose grant bounds the
	// resolve as [ReadQuery.CeilingSeat] bounds a read.
	CeilingSeat string `json:"ceiling_seat,omitempty"`
}

// HeadRef is the name a [RefQuery] gives the default branch: resolved to
// refs/heads/<default branch>, never to a tag.
const HeadRef = "HEAD"

// MaxRefsPerQuery is the most refs one [RefQuery] resolves: one page of the
// refs listing ([MaxRefsPerPage]). A name costs at most two point reads and
// answers at most two refs of [MaxRefNameBytes] and a sentence, so a full
// query answers in about a megabyte, inside [MaxPageBytes].
const MaxRefsPerQuery = MaxRefsPerPage

// RefAnswer is what a [RefQuery]'s refs resolved to.
type RefAnswer struct {
	Repo RepoRef `json:"repo"`
	// Refs answers the query's Refs, one each in order, a name no live ref
	// holds answered as such.
	Refs []ResolvedRef `json:"refs"`
	// Position is the checkpoint every ref was read at.
	Position statelog.Position `json:"position"`
}

// ResolvedRef is one name of a [RefQuery], as the read found it.
type ResolvedRef struct {
	// Name is the name as the query named it.
	Name string `json:"name"`
	// Ref is the whole ref it resolved to and OID the object it names; both
	// empty where no live ref holds the name (Missing).
	Ref string `json:"ref,omitempty"`
	OID string `json:"oid,omitempty"`
	// Moved is the position of the record that last moved the ref, at or before
	// the answer's Position: WHAT A READ OF OID IS FLOORED AT, since OID's
	// objects landed by then. Never the answer's Position, which a linearizable
	// read takes past its own barrier: a copy current to every push still sits
	// below it, and a floor there would make a Gitlet node sync for nothing.
	Moved statelog.Position `json:"moved,omitzero"`
	// At and Seat are the ref row's broker instant of its last move and the
	// seat that set it inside its own namespace ("" for none, [seatOf]), read
	// with OID: what a turn's hold on a seat's branch decides from whether to
	// record itself ([RefHoldRecordAfter]).
	//
	// Estate answer's fields, not a record's: no versionedFields row
	// ([RecordVersion]).
	At   time.Time `json:"at,omitzero"`
	Seat string    `json:"seat,omitempty"`
	// Missing is, for a name no live ref holds, the sentence naming every ref
	// tried, which [RefAnswer.Err] refuses with.
	Missing string `json:"missing,omitempty"`
}

// Err refuses the names no live ref holds ([ErrRefNotFound], each one's
// sentence in query order), or is nil where every name resolved: for a caller
// needing every ref it named.
func (a RefAnswer) Err() error {
	var missing []string
	for _, r := range a.Refs {
		if r.Missing != "" {
			missing = append(missing, r.Missing)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrRefNotFound, strings.Join(missing, "; "))
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
	// ReadRunPushes is ONE EXECUTOR'S PUSHES, oldest first: every push row
	// naming the run ([RunProvenance.RunID]) in the repositories it was given,
	// above its launch position ([RunPushesQuery]). The engine's own read, as
	// `introduced` is, made at collection, every page to the last: a page held
	// to the pushes it READ may answer none of the run's and a cursor past them.
	ReadRunPushes ReadKind = "run_pushes"
	// ReadRefs is one repository's live refs under a prefix, by name; its
	// first page carries the repository in detail too.
	ReadRefs ReadKind = "refs"
	// ReadChanges lists changes, NEWEST FIRST: one repository's in number
	// order, or every readable repository's in the order their opening records
	// landed, narrowed by state, owner and work item ([ReadQuery.State]), each
	// listing walking the one index its filters lead.
	ReadChanges ReadKind = "changes"
	// ReadChange is one change by number (web!42): row, description and authors
	// in detail, and its revisions newest first, a page at a time (later pages
	// carry revisions alone).
	ReadChange ReadKind = "change"
	// ReadChangeRouting is WHOM ONE CHANGE'S RECORDS REACHED (routedread.go):
	// the records about the change that reached anybody, newest first, each
	// with every recipient its notices name (code_notifications), held to the
	// grant as a `change` read is.
	ReadChangeRouting ReadKind = "change_routing"
	// ReadReviews is ONE REVIEWER'S REVIEWS (conversationread.go): those
	// [ReadQuery.Reviewer] gave, newest first, across every repository the
	// asker may read, each naming its change ([ReviewRow.Change]).
	ReadReviews ReadKind = "reviews"
	// ReadIntroduced is one repository's pushes in log order, as a Gitlet
	// node's introduced index reads them: the engine's own read.
	ReadIntroduced ReadKind = "introduced"
	// ReadCompactionDue is the repositories holding at least
	// [ReadQuery.MinPacks] live packs, MOST FIRST, each with its row's version:
	// what a Gitlet node's compactor polls. The engine's own read.
	ReadCompactionDue ReadKind = "compaction_due"
	// ReadPublicationDue is the repositories whose live packs are past those
	// their last publication's recount found a layer covering (a live pack no
	// layer covers, or only a pack pushed since that is byte-identical to a
	// published one: [PublicationCandidate]), LEAST RECENTLY PUBLISHED FIRST,
	// never-published first, each with what a publication is due by. None with
	// an expunge open, whose publication rule 6g refuses until its commit. The
	// engine's own read; the maintainer polls it.
	ReadPublicationDue ReadKind = "publication_due"
	// ReadExpungeDue is the active and archived repositories with an expunge
	// open or a purge requested, by id, each with what its maintainer finishes
	// or serves it by; then, asked with [ReadQuery.ScannedBefore], every other
	// one last JUDGED (its last expunge's commit or clean scan's record,
	// [ExpungeScan], whichever later) at or before that instant or never, least
	// recently judged first, for the maintainer to decide whether an expunge is
	// due. The engine's own read; the maintainer polls it.
	ReadExpungeDue ReadKind = "expunge_due"
	// ReadExpungeTips is the objects one repository's expunge keeps beside its
	// refs ([ReadQuery.ExpungeTips]), in phases: the object each forced or
	// deleting command left since an instant (code_ref_drops, in key order),
	// which the grace keeps; then, whatever the instant, what rows hold IN
	// FLIGHT: every revision head of a draft or open change, every live queue
	// entry's head, every candidate's tip and every green row, so a purge keeps
	// the commit a landing moves onto and the revisions reviewers compare. The
	// engine's own read, at the maintainer's scan and rescue.
	ReadExpungeTips ReadKind = "expunge_tips"
	// ReadImportsDue is the repositories being imported, by id, each with its
	// source and landed chunk count ([ImportCandidate]): what a home polls.
	// The engine's own read.
	ReadImportsDue ReadKind = "imports_due"
	// ReadMirrorsDue is the repositories with a mirror ([MirrorCandidate]):
	// every push mirror, by id, then the active pull mirrors LEAST RECENTLY
	// SYNCED FIRST, so a home syncing every MirrorInterval reads pages until
	// one is synced within it, every sync (even one finding its source level,
	// [MirrorSync.Current]) moving its mirror's instant. What a home polls. The
	// engine's own read.
	ReadMirrorsDue ReadKind = "mirrors_due"
	// ReadMirrorPush is ONE repository's push mirror as its home pushes it
	// ([MirrorPushQuery]): its current push mirror, as the one candidate of
	// [ReadAnswer.Mirrors], and every green row ([ReadAnswer.Green]) in ONE
	// snapshot, so a push's target and the green commits it publishes are one
	// version's. The engine's own read: the home judges the mirror's seat
	// before each push ([MirrorSeatRefusal]), never as a read's asker.
	ReadMirrorPush ReadKind = "mirror_push"
	// ReadBranchChanges is the LIVE CHANGE each named branch heads
	// ([BranchChangesQuery]), in the query's order, a branch heading none left
	// out. The engine's own read, held to no grant: how it names the change
	// each ref a collected executor moved delivered to, and whether a change
	// the run asked to open is open already.
	ReadBranchChanges ReadKind = "branch_changes"
	// ReadChangesDue is the change work a repository's HOME Gitlet node owes
	// ([ChangesDueQuery]): with no repository, by id, the active repositories
	// holding a LIVE change whose head revision lacks facts or mergeability
	// against its target, or whose mergeability is due again; with one, the
	// first two kinds by source branch, each with its target's tip
	// ([ChangeDue]), the path owners where a change owes its facts, and the
	// third oldest first. The engine's own read: its home polls and is nudged
	// into reading it.
	ReadChangesDue ReadKind = "changes_due"
	// ReadIndexed is the active and archived repositories searched by index
	// ([SearchIndexed]), BY NAME, each by id and name, a page of
	// MaxSnapshotItems after [ReadQuery.Cursor] (the last name served): what a
	// Gitlet node's indexer polls. The engine's own read.
	ReadIndexed ReadKind = "indexed"
	// ReadDefaultTips is the active project repositories, BY NAME, each with
	// id, name, readers and default branch's tip ("" for an empty one), a page
	// of MaxSnapshotItems after [ReadQuery.Cursor]: what the atlas's scan
	// compares with the tips it last read. The engine's own read.
	ReadDefaultTips ReadKind = "default_tips"
	// ReadChecks lists check attempts ([CheckRow]), held to the asker's grant:
	// one repository's, newest ended first and the unended after; one change's
	// or commit's, by commit, name and attempt ([CheckQuery]); or, naming no
	// repository, the WINDOW: every readable repository's attempts ended at or
	// after [CheckQuery.Since], newest ended first.
	ReadChecks ReadKind = "checks"
	// ReadCheck is one attempt read whole ([CheckDetail]): row, definition,
	// tail, ending failures and kept log, held to the asker's grant.
	ReadCheck ReadKind = "check"
	// ReadChecksDue is the check work a repository's HOME Gitlet node owes
	// ([ChecksDueQuery]): with no repository, by id, the active repositories
	// holding a LIVE change whose head no dispatch for its target names its
	// target row's checks file, or whose head's latest attempt of a check ended
	// by infrastructure below [AutoRequeueAttempts], or a target due a
	// re-reading of its checks file; with one, those changes by source branch,
	// each with its target's tip ([ChangeDue]) and what it owes ([CheckWork]),
	// and on the first page the targets due a reading ([CheckTargetDue]). The
	// engine's own read (checksdispatch.go).
	ReadChecksDue ReadKind = "checks_due"
	// ReadCheckBox is the ONE attempt a check box's token names, by key
	// ([ReadQuery.CheckBox]): its row, or none, in the answer's Checks. THE
	// ENGINE'S OWN: the box's admission (internal/gitlet/host) reads it on every
	// request, and a box is no principal (the row is its authority), so it names
	// no asker and has no grant. Its own kind rather than a `check` read naming
	// no asker, so a principal's read that forgot its asker is refused, not
	// answered ungated.
	ReadCheckBox ReadKind = "check_box"
	// ReadChecksQueued is every attempt waiting for a runner, OLDEST QUEUED
	// FIRST, each with its definition and repository name ([RunnableCheck]): a
	// Gitlet node's CLAIM WALK (internal/gitlet/checkrun). The engine's own,
	// held to no grant, and asking no question but its page.
	ReadChecksQueued ReadKind = "checks_queued"
	// ReadChecksRunning is the attempts runners hold ([ChecksRunningQuery]):
	// with no repository, the repositories holding one, by id; with one, its
	// running attempts ([CheckRow]). The engine's own: a HOME sweeps it for a
	// runner that is gone.
	ReadChecksRunning ReadKind = "checks_running"
	// ReadQueue lists the LIVE merge queue entries ([QueueRow]), held to the
	// asker's grant: one repository's in target and queue order (one target's
	// where [ReadQuery.Target] names it) beside the candidates of the trains
	// testing them ([CandidateRow]); or, naming no repository, every readable
	// one's. A HOME's queue work is not this read: it carries what it reads
	// into its next records, so it reads whole or not at all ([Reader.QueueDue]).
	ReadQueue ReadKind = "queue"
	// ReadLandings is one protected target's landings above its green
	// ([LandingRow]) in landing order, beside its green row ([GreenRow]), held
	// to the asker's grant.
	ReadLandings ReadKind = "landings"
	// ReadGreen is one repository's green rows ([GreenRow]), each protected
	// target's newest commit whose required checks passed, or one target's,
	// held to the asker's grant.
	ReadGreen ReadKind = "green"
	// ReadCommit is ONE COMMIT'S CHANGES (commitread.go): the repository in
	// detail and every change [ReadQuery.Commit] belongs to (whose merge it is
	// or whose revision it heads), in one snapshot, held to the asker's grant.
	ReadCommit ReadKind = "commit"
	// ReadInbox is ONE PERSON'S CODE INBOX (inboxread.go): their notices, newest
	// first on the code log, filtered in the scan by their own marks and the
	// query's narrowing, each marked and carrying what its change says, held to
	// the asker's grant on each notice's repository.
	ReadInbox ReadKind = "inbox"
	// ReadNoticeCards is WHAT AN INBOX PAGE'S CARDS DRAW (inboxread.go): each
	// change [ReadQuery.NoticeCards] names, as its notice's card reads it (row,
	// description opening, open agent findings and, while live, where its merge
	// stands for the asker: [NoticeChange]), held to the asker's grant. Asked
	// after an inbox page is composed, only by a read that draws them.
	ReadNoticeCards ReadKind = "notice_cards"
	// ReadCommitMarks is what a history page draws beside its commits
	// (commitmarks.go): each commit's checks per branch and the changes it
	// landed with, in one snapshot, held to the asker's grant.
	ReadCommitMarks ReadKind = "commit_marks"
	// ReadCounts is a scope's counts (counts.go): the parts [ReadQuery.Counts]
	// asks, of one repository or of every one in use the asker reads, in one
	// snapshot.
	ReadCounts ReadKind = "counts"
)

// ReadKinds is the closed set.
//
// A KIND IS NEW TO A BUILD THAT PREDATES IT, which refuses it whole
// ([ErrInvalid]), never answering it as another, so a field only newer kinds
// read takes no versionedFields row: State, Owner, Work (`changes`, `change`);
// Change (`change`, `change_routing`); Reviewer (`changes`, `reviews`); Target
// (`queue`, `landings`, `green`); and RunPushes, ExpungeTips, BranchChanges,
// CheckBox, ChecksRunning, Commit, Inbox, NoticeCards, CommitMarks and Counts,
// each read by its own kind alone ([ReadQuery]).
func ReadKinds() []ReadKind {
	return []ReadKind{ReadRepos, ReadRepo, ReadPushes, ReadRunPushes, ReadRefs, ReadChanges, ReadChange,
		ReadChangeRouting, ReadReviews, ReadCommit,
		ReadIntroduced, ReadCompactionDue, ReadPublicationDue, ReadExpungeDue, ReadExpungeTips, ReadImportsDue,
		ReadMirrorsDue, ReadMirrorPush, ReadBranchChanges, ReadChangesDue, ReadIndexed, ReadDefaultTips,
		ReadChecks, ReadCheck, ReadChecksDue, ReadCheckBox, ReadChecksQueued, ReadChecksRunning, ReadQueue,
		ReadLandings, ReadGreen, ReadInbox, ReadNoticeCards, ReadCommitMarks, ReadCounts}
}

// Valid reports a kind this build knows.
func (k ReadKind) Valid() bool { return slices.Contains(ReadKinds(), k) }

// ReadQuery is a person's, a seat's or a surface's read of Gitlet. Every
// field is a request field: an older build refuses a key it does not know
// unread (ErrUnknownKeys) and the router asks the next node, so a newer field
// takes no versionedFields row ([RecordVersion]).
type ReadQuery struct {
	Kind ReadKind `json:"kind"`
	// Asker is who reads, REQUIRED for every kind a principal reads
	// ([ErrInvalid] when zero or invalid), and the serving node holds the
	// answer to its grant: a listing leaves out what it may not read, a read of
	// one answers [ErrNotPermitted]. Never defaulted: an optional asker is a
	// hole any caller that forgot it opens. REFUSED on `introduced`,
	// `run_pushes` and the duty listings (`compaction_due`, `publication_due`,
	// `expunge_due`, `changes_due`, `checks_due`, `indexed`), which no principal
	// makes.
	Asker Principal `json:"asker"`
	// CeilingSeat is the handle of the seat the asking turn acts FOR (whose ask
	// woke it), "" for its asker alone: the REQUESTER CEILING
	// (authz.DecideWithin's) on a repository's grant. The answering node judges
	// it beside the asker from the same chart and row ([JudgeRead]) and serves
	// only what BOTH may read; beyond it is [ErrAboveCeiling]. Judged where
	// answered (ADR-0088), never where the turn runs, which would keep
	// admitting a seat a later activation shut out.
	//
	// ONLY A SEAT, BESIDE A SEAT: a person's or token's read is its own (ADR-0085).
	// Refused on the reads no principal makes. No versionedFields row.
	CeilingSeat string `json:"ceiling_seat,omitempty"`
	// Repo is the repository's NAME: required for repo, pushes, refs and
	// change; for changes, the one repository listed, or none for every
	// readable one.
	Repo string `json:"repo,omitempty"`
	// Status filters a repos listing; empty is active and archived.
	Status []RepoStatus `json:"status,omitempty"`
	// Protected narrows a repos listing to the repositories a rule protects
	// (code_repos.protected), walked by code_repos_protected_idx
	// ([protectedReposKey]), so a page costs what the protected ones cost
	// however many are unprotected (filtering after the read never showed one
	// past the first page). Refused on every other kind, before any kind's own
	// rules. No versionedFields row.
	Protected bool `json:"protected,omitempty"`
	// Prefix narrows a refs listing to the refs under it: "refs/heads/" (the
	// default), "refs/tags/" or a deeper prefix ending in "/" (a seat's
	// namespace). It narrows a repositories listing to the names it begins,
	// read as one range of the name index so a page costs what it holds.
	Prefix string `json:"prefix,omitempty"`
	// RefListing orders, narrows and details a refs listing; nil is the plain
	// listing. An older data node refuses it (ErrUnknownKeys) and the router
	// asks the next.
	RefListing *RefListingQuery `json:"ref_listing,omitempty"`
	// Brief asks a push history for its rows IN BRIEF ([PushRow.brief]): a
	// screen draws a push a few lines tall, while an import's push of
	// MaxCommandsPerPush refs is 140 to 460 KB a poll; [ReadQuery.Push] reads
	// one whole.
	//
	// It asks a `change` read's first page for the row alone
	// ([ReadAnswer.ChangeBrief]) beside its revisions, no detail or readiness
	// (six statements and a chart): for a read needing only state, revision,
	// head and branches. Beside DetailOnly it answers the row ALONE (one seek of
	// code_changes_number_idx). Refused beside a part or cursor and on other
	// kinds. No versionedFields row.
	Brief bool `json:"brief,omitempty"`
	// Push narrows a push history to ONE push by id, read whole: every command
	// and introduced commit. A push the history does not hold (another
	// repository's, or past PushHistory) is an answer with no row. Pushes only;
	// refused beside a cursor, a limit or Brief. No versionedFields row.
	Push string `json:"push,omitempty"`
	// PushFilter narrows a push history to one pusher's pushes in a window of
	// instants ([PushFilter]); refused on every other kind and beside Push.
	PushFilter *PushFilter `json:"push_filter,omitempty"`
	// Commit is the whole commit id a `commit` read names the changes of;
	// refused on every other kind ([ReadKinds]).
	Commit string `json:"commit,omitempty"`
	// PathOwners asks a `repo` read for the repository's PATH OWNERS beside its
	// detail ([RepoDetail.PathOwners]). A read of who owns its paths sets it; a
	// read that RESOLVES a repository or reads its row does not (every seat
	// tool's once a turn, a refs listing's first page, a header polled every
	// minute): at their bound the path owners are a quarter MiB of JSON each,
	// encoded, shipped and decoded to be dropped. Refused on every other kind.
	// No versionedFields row.
	PathOwners bool `json:"path_owners,omitempty"`
	// Gates asks a read answering one repository's DETAIL (a `repo` read, or a
	// refs listing's first page) for its rules' merge gates
	// ([RepoDetail.Gates]). A read presenting a gate sets it; a read resolving a
	// repository does not, for the path owners' reason: gates reach half a MiB
	// of JSON. Refused on every other read and on a refs listing's later pages.
	// No versionedFields row.
	Gates bool `json:"gates,omitempty"`
	// Cards asks a `changes` listing for each change's card
	// ([ChangeSummary.Card]); refused on every other kind.
	Cards bool `json:"cards,omitempty"`
	// Change is a `change` or `change_routing` read's number on the repository
	// Repo names (web!42's 42): REQUIRED there, at least 1, refused on every
	// other kind.
	Change int64 `json:"change,omitempty"`
	// State, Owner and Work narrow a `changes` listing, each refused on every
	// other kind: one state; the changes one principal opened, by the handle
	// their rows name; the changes carrying one work item, `<backend>:<id>`
	// ([types.WorkItem.Ref]). Repo, where set, narrows to one repository.
	//
	// A LISTING NAMES A REPOSITORY, A WORK ITEM, A STATE OR A WINDOW (what its
	// indexes lead with, beside reviewer and need below) and is refused naming
	// none. It walks the ONE index its filters lead (changeread.go) and tests
	// every other filter on the rows it read, so a page is the rows it READ and
	// may be short or empty with a cursor.
	State ChangeState `json:"state,omitempty"`
	Owner string      `json:"owner,omitempty"`
	Work  string      `json:"work,omitempty"`
	// Reviewer and Needs narrow a `changes` listing by what it asks of somebody
	// (conversationread.go), refused on other kinds: Reviewer, the changes
	// whose ask of that reviewer stands; Needs, the LIVE changes waiting on whom
	// it names ([Need]): a review (review), their owner's fix (fix), the asker
	// either way (me), or a person (person, naming no reviewer). It walks the
	// ONE index of who changes wait on. The domain landed with these, so no
	// versionedFields row. Reviewer is also a `reviews` listing's REQUIRED
	// reviewer.
	Reviewer string `json:"reviewer,omitempty"`
	Needs    Need   `json:"needs,omitempty"`
	// Window narrows a `changes` listing to the changes that moved within it
	// ([ChangeWindow], [ChangeSummary.UpdatedAt]), refused on every other kind.
	// A listing naming nothing else an index leads by walks the two indexes of
	// last move (changewindow.go), newest first, testing a terminal state and an
	// owner on the rows; one naming any of those walks its index in its order
	// and tests the window on each row. A live state sorted by move
	// ([ChangeSortUpdated]) walks by move and reads no change that moved before
	// the window.
	Window ChangeWindow `json:"window,omitempty"`
	// ChangeSort orders a `changes` listing ([ChangeSort]); "" is its walk's
	// own order.
	ChangeSort ChangeSort `json:"change_sort,omitempty"`
	// Readiness narrows a `changes` listing to the open changes in one bucket
	// ([Readiness.Bucket]); refused on every other kind.
	Readiness ReadinessBucket `json:"readiness,omitempty"`
	// Text narrows a `changes` listing to the changes whose title or
	// description holds every word of it (changetext.go); refused on every
	// other kind.
	Text string `json:"text,omitempty"`
	// Part is the list a `change` read pages beside the change: revisions (""
	// too), reviewers, comments, reviews, findings or the asker's own standing
	// findings ([ChangeParts]); refused on every other kind. The detail rides
	// only a first page of revisions; every page of another part carries that
	// part's rows, held to the grant again.
	Part ChangePart `json:"part,omitempty"`
	// DetailOnly asks a `change` read for the change's DETAIL ALONE (row,
	// description, authors, conversation, mergeability, readiness) with no page
	// of revisions. A read judging or acting on a change sets it (a merge's or
	// rebase's build, a seat's write tool resolving its change); a read
	// presenting revisions does not: a first page is up to defaultRevisionLimit
	// (200) revisions with up to MaxTouchedPaths each, up to MaxPageBytes of
	// JSON shipped to be dropped. Beside Brief it answers the row alone
	// ([ReadQuery.Brief]). Refused on every other kind and beside a part,
	// cursor or limit. No versionedFields row.
	DetailOnly bool `json:"detail_only,omitempty"`
	// Resolve asks a read that names its repository BY NAME and judges the
	// grant on it (a `change` read, a `changes` or `checks` listing of one
	// repository) for the repository the name RESOLVED to
	// ([ReadAnswer.Resolved]): its row as a `repo` read resolves it, in the
	// answer's snapshot, refused as a `repo` read is by a record this node
	// deferred on it. A call that has not yet resolved its repository sets it on
	// its first read: ONE linearizable read and one code log barrier where a
	// `repo` read then the call's own were two. Refused on every other read and
	// on a listing naming no repository. No versionedFields row.
	Resolve bool `json:"resolve,omitempty"`
	// FindingIDs narrows a `change` read's findings part to the 1 to
	// [MaxFindingsPerReview] findings it names, each once, served by one seek
	// per id, never a walk. Findings page by id and ids are time-ordered, so a
	// write's findings (the newest review's) sit on the part's LAST page:
	// unnarrowed, every such write read the whole review history. An id the
	// change lacks is no row. Rows come in id order, bounded by the page's
	// bytes, so a cursor resumes them. Refused beside a limit and on every
	// other part and kind. No versionedFields row.
	FindingIDs []string `json:"finding_ids,omitempty"`
	// FindingPaths asks a `change` read for its findings counted by path, state
	// and revision ([ReadAnswer.FindingPaths]), paged by Limit and Cursor;
	// refused on every other kind and beside a part, detail_only, brief or
	// finding_ids.
	FindingPaths bool `json:"finding_paths,omitempty"`
	// Viewed asks a `change` read for the asker's own viewed marks on its files
	// ([ReadAnswer.Viewed]), paged by Limit and Cursor in digest order; refused
	// on every other kind and beside a part, detail_only, brief, finding_ids or
	// finding_paths.
	Viewed bool `json:"viewed,omitempty"`
	// Introduced is an `introduced` read's range: REQUIRED for that kind,
	// refused on every other.
	Introduced *IntroducedQuery `json:"introduced,omitempty"`
	// RunPushes is a `run_pushes` read's run: REQUIRED there, refused on every
	// other kind. A build predating the kind refuses it (ErrInvalid), never
	// answering as none: no versionedFields row. The same holds for each
	// kind-specific query below (ChangesDue, Checks, ChecksDue, CheckBox,
	// ChecksRunning, ExpungeTips, MirrorPush, BranchChanges, Target,
	// NoticeCards, CommitMarks): REQUIRED for its kind, refused on others.
	RunPushes *RunPushesQuery `json:"run_pushes,omitempty"`
	// ChangesDue is a `changes_due` read's own ([ChangesDueQuery]).
	ChangesDue *ChangesDueQuery `json:"changes_due,omitempty"`
	// Checks narrows a `checks` read to a change's or commit's attempts, or
	// bounds its window, and names the one attempt a `check` read is of.
	// ChecksDue is a `checks_due` read's question.
	Checks    *CheckQuery     `json:"checks,omitempty"`
	ChecksDue *ChecksDueQuery `json:"checks_due,omitempty"`
	// CheckBox is the attempt a `check_box` read is of, by KEY (its repository
	// by id, as the token names it): a build predating the kind refuses it
	// (ErrInvalid), the box refused, never admitted by a row read as none.
	CheckBox *CheckKey `json:"check_box,omitempty"`
	// ChecksRunning is a `checks_running` read's question (the repositories
	// holding a running attempt, or one repository's by id).
	ChecksRunning *ChecksRunningQuery `json:"checks_running,omitempty"`
	// DefaultTips narrows a `default_tips` read to ONE repository by id, no row
	// where it is not an active project repository; nil lists every one.
	// Refused on every other kind. No versionedFields row.
	DefaultTips *DefaultTipsQuery `json:"default_tips,omitempty"`
	// ExpungeTips is an `expunge_tips` read's repository and instant.
	ExpungeTips *ExpungeTipsQuery `json:"expunge_tips,omitempty"`
	// MirrorPush is a `mirror_push` read's repository.
	MirrorPush *MirrorPushQuery `json:"mirror_push,omitempty"`
	// ScannedBefore asks an `expunge_due` read for its WEEKLY ARM beside its
	// open one ([ReadExpungeDue]): every other active or archived repository
	// last judged at or before it ([ExpungeScan]) or never, least recently
	// judged first; the maintainer asks from its clock less [ExpungeInterval].
	// Zero asks the open arm alone. Refused on every other kind.
	ScannedBefore time.Time `json:"scanned_before,omitzero"`
	// BranchChanges is a `branch_changes` read's repositories and branches.
	BranchChanges *BranchChangesQuery `json:"branch_changes,omitempty"`
	// Target narrows a `queue` read of a repository to one branch's queue, and
	// names the protected branch a `landings` read lists (REQUIRED there) and a
	// `green` read answers, beside Repo; refused on every other kind.
	Target string `json:"target,omitempty"`
	// Trains asks a `queue` read for its trains and ejections across every
	// repository it lists, and any queue read for EjectedTotal
	// ([ReadAnswer.EjectedTotal]); refused on every other kind.
	Trains bool `json:"trains,omitempty"`

	// Inbox is an `inbox` read's question: whose inbox, and how it is
	// narrowed ([InboxQuery]). Only that kind reads it.
	Inbox *InboxQuery `json:"inbox,omitempty"`
	// NoticeCards is a `notice_cards` read's change ids, each once, at most
	// [MaxNoticeCards] (what one composed inbox page can name).
	NoticeCards []string `json:"notice_cards,omitempty"`
	// CommitMarks is a `commit_marks` read's commits: 1 to [MaxCommitMarks]
	// whole ids, once each.
	CommitMarks []string `json:"commit_marks,omitempty"`
	// DayBounds is the asker's day boundaries, a day between each two: on a
	// push history, and with RepoFacts on a repositories listing or one
	// repository's read ([ReadQuery.validateDayBounds]); refused on every other
	// kind.
	DayBounds []time.Time `json:"day_bounds,omitempty"`
	// Counts is a `counts` read's parts, each a key of its own so an older
	// build refuses one it lacks unread; required there, refused on every other
	// kind.
	Counts *CountsQuery `json:"counts,omitempty"`
	// RepoFacts asks a repositories listing or one repository's read for each
	// one's facts ([RepoSummary.ChangeCounts], [RepoSummary.Card]) and a first
	// page for the total ([ReadAnswer.ReposTotal]); refused on every other
	// kind.
	RepoFacts bool `json:"repo_facts,omitempty"`
	// RepoKind narrows a `repos` listing to one kind ([RepoKind]); "" is every
	// kind. Refused on every other kind.
	RepoKind RepoKind `json:"repo_kind,omitempty"`
	// RepoSort orders a `repos` listing ([RepoSort]); "" is by name. Refused on
	// every other kind.
	RepoSort RepoSort `json:"repo_sort,omitempty"`
	// MinPacks is a `compaction_due` read's floor of live packs: REQUIRED
	// there, at least 1, refused on every other kind.
	MinPacks int    `json:"min_packs,omitempty"`
	Cursor   string `json:"cursor,omitempty"`
	// Limit, per kind (min..max, with its zero default):
	//   - repos 1..500 (100); pushes 1..200 (50); refs 1..1 000 (200)
	//   - changes 1..200 (50), a change's revisions and reviewers 1..1 000 (200),
	//     its comments, reviews and findings, its routing's records and a
	//     reviewer's reviews 1..200 (50); checks 1..200 (50)
	//   - introduced, run_pushes and checks_queued 1..MaxSnapshotItems (the most)
	//   - a duty listing and expunge_tips: MaxSnapshotItems, naming none
	// Each counts the rows a page READS, whatever its filters keep, except a
	// push history narrowed by PushFilter: the pushes a page KEEPS, reading at
	// most MaxPushesScanned pushes and MaxPageBytes of their rows.
	Limit int                `json:"limit,omitempty"`
	Fresh statelog.Freshness `json:"fresh"`
}

// ReadAnswer is a [ReadQuery]'s answer: Changes a `changes` page; Change a
// `change` read's detail, on its first revisions page alone, ChangeBrief its
// row instead when read in brief ([ReadQuery.Brief]); Revisions a page of its
// revisions, newest first; Reviewers, Comments, Reviews and Findings a page of
// the part a `change` read names ([ReadQuery.Part]), Reviews also a `reviews`
// page, each naming its change; Routed a `change_routing` page; Counts a
// `counts` read's parts; ReposTotal a first repositories page's total.
type ReadAnswer struct {
	Repos []RepoSummary `json:"repos,omitempty"`
	Repo  *RepoDetail   `json:"repo,omitempty"`
	// Pushes is a page of a repository's history, newest first, or of one
	// executor's pushes, oldest first (each repository in the query's order,
	// its pushes in log order).
	Pushes []PushRow `json:"pushes,omitempty"`
	// PushScan is on every page of a narrowed push history ([ReadQuery.PushFilter]), and nil on every other.
	PushScan *PushScan `json:"push_scan,omitempty"`
	// PushDays is the repository's pushes on each of the asker's days, oldest
	// first, zeros included: on a push history asked with
	// [ReadQuery.DayBounds], whatever the page holds.
	PushDays []int64  `json:"push_days,omitempty"`
	Refs     []RefRow `json:"refs,omitempty"`
	// RefsDetail is a refs page read in detail, in place of Refs;
	// ActiveBranches a first page's count of the branches moved since
	// [RefListingQuery.ActiveSince].
	RefsDetail     []RefListing    `json:"refs_detail,omitempty"`
	ActiveBranches *BranchCount    `json:"active_branches,omitempty"`
	Changes        []ChangeSummary `json:"changes,omitempty"`
	Change         *ChangeDetail   `json:"change,omitempty"`
	// ChangeBrief is a `change` read's change IN BRIEF ([ReadQuery.Brief]): its
	// row, its repository's name and the asker's access, with no readiness
	// judged (never meaning "no longer live", which a nil readiness says
	// elsewhere). An answer's field: no versionedFields row.
	ChangeBrief *ChangeSummary `json:"change_brief,omitempty"`
	// NarrowedTo is, on every page of a `changes` listing narrowed by
	// [ReadQuery.Readiness], its bucket: a page without it was not so narrowed.
	NarrowedTo ReadinessBucket `json:"narrowed_to,omitempty"`
	// ChangesText is on every page of a `changes` listing narrowed by
	// [ReadQuery.Text], `{}` where the search was whole: a page without it was
	// not so narrowed.
	ChangesText *ChangesText `json:"changes_text,omitempty"`
	// Resolved is the repository the read's name resolved to, when asked
	// ([ReadQuery.Resolve]): its row as a `repo` read resolves it, at the
	// answer's checkpoint, naming the old name where renamed
	// ([RepoRef.Redirect]). An answer's field: no versionedFields row.
	Resolved  *RepoRef      `json:"resolved,omitempty"`
	Revisions []RevisionRow `json:"revisions,omitempty"`
	Reviewers []ReviewerRow `json:"reviewers,omitempty"`
	// Moves is a page of a change's moves ([PartMoves]).
	Moves []ChangeMove `json:"moves,omitempty"`
	// QueueEntries is a page of a change's queue entries ([PartQueue]).
	QueueEntries []ChangeQueueEntry `json:"queue_entries,omitempty"`
	Comments     []CommentRow       `json:"comments,omitempty"`
	Reviews      []ReviewRow        `json:"reviews,omitempty"`
	Findings     []FindingRow       `json:"findings,omitempty"`
	// FindingPaths is a `finding_paths` page of a change ([ReadQuery.FindingPaths]).
	FindingPaths []FindingPathRow `json:"finding_paths,omitempty"`
	// Viewed is a `viewed` page of a change ([ReadQuery.Viewed]): the asker's own marks.
	Viewed []ViewedRow `json:"viewed,omitempty"`
	// ViewedKey is the repository's viewed key, beside a Viewed page holding a
	// mark: what the asker's screen digests its files under, to match the page.
	ViewedKey  ViewedKey              `json:"viewed_key,omitempty"`
	Routed     *RoutedPage            `json:"routed,omitempty"`
	Introduced []IntroducedRow        `json:"introduced,omitempty"`
	Due        []CompactionCandidate  `json:"due,omitempty"`
	Publish    []PublicationCandidate `json:"publish,omitempty"`
	Expunge    []ExpungeCandidate     `json:"expunge,omitempty"`
	// Imports is an `imports_due` read's page, Mirrors a `mirrors_due`
	// read's.
	Imports []ImportCandidate `json:"imports,omitempty"`
	Mirrors []MirrorCandidate `json:"mirrors,omitempty"`
	// DueRepos is a `changes_due` listing's page of repositories, by id;
	// DueChanges one repository's due changes by source branch, beside
	// PathOwners (matched against touched paths, so only beside a change owing
	// facts) and, on its first page, Rechecks: changes whose merge is due again,
	// oldest first ([ChangesDueQuery.Rechecks]). A `checks_due` read answers in
	// the same two fields (DueChanges each with [ChangeDue.Checks]) and, on a
	// repository's first page, DueTargets (targets due a re-reading of their
	// checks file) and DueTips (protected targets owing tip work, [TipDue],
	// tipwork.go), paged ahead of its heads ([ReadAnswer.TipsNext]).
	DueRepos []string `json:"due_repos,omitempty"`
	// Indexed is an `indexed` listing's page of repositories, by name.
	Indexed []IndexedRepo `json:"indexed,omitempty"`
	// DefaultTips is a `default_tips` listing's page of repositories, by name.
	DefaultTips []DefaultTip     `json:"default_tips,omitempty"`
	DueChanges  []ChangeDue      `json:"due_changes,omitempty"`
	DueTargets  []CheckTargetDue `json:"due_targets,omitempty"`
	DueTips     []TipDue         `json:"due_tips,omitempty"`
	Rechecks    []ChangeDue      `json:"rechecks,omitempty"`
	PathOwners  []PathOwner      `json:"path_owners,omitempty"`
	// Gates is, beside PathOwners on the same pages, the repository's rules'
	// merge gates, each named by its rule's pattern ([RuleGate]): what the home
	// matches touched paths against for the human paths each change's target is
	// held to ([MatchTargetHumanPaths]). An answer's field: no versionedFields row.
	Gates []RuleGate `json:"gates,omitempty"`
	// AskSettles is, on the first page of one repository's `changes_due` read,
	// its changes whose review asks no longer match their state (an ended
	// change's asks still waiting, a reopened change's asks its end left), at
	// most [MaxAskSettlesPerRead], each with the state its asks move to
	// ([AskSettle]). An answer's field.
	AskSettles []AskDue `json:"ask_settles,omitempty"`
	// Checks is a page of a `checks` read, or a `check_box` read's one attempt
	// (or none); Check a `check` read's one attempt whole. A `checks_running`
	// read of one repository answers its running attempts in Checks, and of
	// none the repositories holding one in DueRepos.
	Checks []CheckRow   `json:"checks,omitempty"`
	Check  *CheckDetail `json:"check,omitempty"`
	// StandingCounts is a standing listing's first page's attempts by state ([CheckQuery.Standing]).
	StandingCounts *StandingCounts `json:"standing_counts,omitempty"`
	// Runnable is a page of a `checks_queued` read: the attempts waiting for
	// a runner, oldest queued first.
	Runnable []RunnableCheck `json:"runnable,omitempty"`
	// Tips is a page of an `expunge_tips` read: object ids, each once on the
	// page, in the order of the rows naming them (drops, revisions, queue
	// entries, candidates, green rows); a later page may name one again, which
	// its reader keeps once.
	Tips []string `json:"tips,omitempty"`
	// BranchChanges is a `branch_changes` read's answer: each branch's live
	// change in the query's order, a branch heading none left out.
	BranchChanges []BranchChange `json:"branch_changes,omitempty"`
	// Queue is a page of a `queue` read's live entries, in queue order; Trains
	// the candidates of the trains testing the targets its page reaches (a
	// named repository's always, every repository's where [ReadQuery.Trains]
	// asks); Ejected, on a first page, the entries put out lately
	// ([EjectedRow]), newest first; Landings a `landings` page; Green the
	// target's green row a `landings` read starts above, or a `green` read's rows.
	Queue    []QueueRow     `json:"queue,omitempty"`
	Trains   []CandidateRow `json:"trains,omitempty"`
	Ejected  []EjectedRow   `json:"ejected,omitempty"`
	Landings []LandingRow   `json:"landings,omitempty"`
	Green    []GreenRow     `json:"green,omitempty"`
	// EjectedTotal counts what Ejected is the newest of, on a first page asked
	// with [ReadQuery.Trains]; nil where not asked.
	EjectedTotal *EjectedTotal `json:"ejected_total,omitempty"`
	// Commit is a `commit` read's changes of its commit, beside Repo.
	Commit *CommitChanges `json:"commit,omitempty"`
	// Inbox is an `inbox` read's page: the person's notices and their marks.
	Inbox *InboxPage `json:"inbox,omitempty"`
	// NoticeCards is a `notice_cards` read's answer: what each named change
	// says, by id ([NoticeChange]); none for a change no row holds or on a
	// repository the asker may not read.
	NoticeCards map[string]*NoticeChange `json:"notice_cards,omitempty"`
	// CommitMarks is a `commit_marks` read's answer, one mark a commit in the
	// query's order.
	CommitMarks []CommitMark `json:"commit_marks,omitempty"`
	// Counts is a `counts` read's answer, a part for each it asked.
	Counts *Counts `json:"counts,omitempty"`
	// ReposTotal is a first page of repositories' whole listing, where [ReadQuery.RepoFacts] asks.
	ReposTotal *ReposTotal `json:"repos_total,omitempty"`
	Next       string      `json:"next,omitempty"`
	// TipsNext is a `checks_due` page of one repository's tip work cut at its
	// bound: the target it ended at, which the next page continues after
	// ([ChecksDueQuery.TipsAfter]) before any head, so targets whose windows
	// fill a page are all answered within one turn.
	TipsNext string             `json:"tips_next,omitempty"`
	Position statelog.Position  `json:"position"`
	Level    statelog.ReadLevel `json:"level"`
}

// IndexedRepo is one repository an `indexed` read lists: its id (keying its git
// order and its copy) and its name (the listing's cursor).
type IndexedRepo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// DefaultTipsQuery is the one repository a `default_tips` read is narrowed to.
type DefaultTipsQuery struct {
	RepoID string `json:"repo_id"`
}

// DefaultTip is one repository a `default_tips` read lists: its default
// branch's tip, empty for a repository whose default branch holds no commit.
type DefaultTip struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Visibility Visibility `json:"visibility"`
	Head       string     `json:"head,omitempty"`
}

// ExpungeCandidate is one repository an `expunge_due` read lists: its open
// expunge and requested purge, whichever it holds (what the maintainer
// finishes or serves first), or neither for the weekly arm; when it was last
// expunged; its live packs, bytes and objects (a cycle's inputs are cut from
// them, and the objects place its maintenance by charge on the node that
// compacts and publishes it); and its row's version.
type ExpungeCandidate struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Open is the expunge begun and not committed, by id, "" for none: what the
	// maintainer finishes before beginning another.
	Open string `json:"open,omitempty"`
	// Request is the purge requested and not served, by id, and Requested its
	// record's position: a cycle serving it rewrites the live packs whose
	// content dates from before that ([PackRow.Content]). "" and zero for none.
	Request   string            `json:"request,omitempty"`
	Requested statelog.Position `json:"requested,omitzero"`
	// ExpungedAt is the last commit's broker instant, zero for a repository
	// never expunged.
	ExpungedAt  time.Time         `json:"expunged_at,omitzero"`
	LivePacks   int64             `json:"live_packs"`
	LiveBytes   int64             `json:"live_bytes"`
	LiveObjects int64             `json:"live_objects"`
	Version     statelog.Position `json:"version"`
}

// ImportCandidate is one repository an `imports_due` read lists: its source (a
// URL and a ${VAR} reference, never a secret), how many chunks have landed (the
// next is Seq+1) and its row's version.
type ImportCandidate struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Source  ImportSource      `json:"source"`
	Seq     int64             `json:"seq"`
	Version statelog.Position `json:"version"`
}

// MirrorCandidate is one repository a `mirrors_due` read lists: its pull
// mirror's source (From) or push mirror's target (To), a repository with both
// listed once in each arm; and, for a pull mirror, when its last sync landed
// (zero for never) and its row's version: the floor its home syncs its copy at
// before fetching, since a copy synced at no floor could still be the one its
// import left, importing. Answer fields: no versionedFields row.
type MirrorCandidate struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	From     *MirrorSource     `json:"from,omitempty"`
	To       *MirrorTarget     `json:"to,omitempty"`
	SyncedAt time.Time         `json:"synced_at,omitzero"`
	Version  statelog.Position `json:"version,omitzero"`
}

// MirrorPushQuery is a `mirror_push` read's repository, by id
// ([ReadMirrorPush]).
type MirrorPushQuery struct {
	RepoID string `json:"repo_id"`
}

// ExpungeTipsQuery is an `expunge_tips` read's repository, by id, and the
// instant from which its drops were left: every code_ref_drops row whose
// record's broker instant is at or after Since. An expunge keeping a grace asks
// from its scan's instant less the grace; one serving a purge, which keeps
// nothing a drop left, from its scan's instant. What rows hold in flight is
// listed whatever Since is ([ReadExpungeTips]).
type ExpungeTipsQuery struct {
	RepoID string    `json:"repo_id"`
	Since  time.Time `json:"since"`
}

// CompactionCandidate is one repository a `compaction_due` read lists: its live
// packs and objects (a compaction's memory charge is stated by them before
// anything syncs) and its row's version, which keys a compactor's memory of a
// not-due plan so an unmoved row is not planned again.
type CompactionCandidate struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	LivePacks   int64             `json:"live_packs"`
	LiveObjects int64             `json:"live_objects"`
	Version     statelog.Position `json:"version"`
}

// PublicationCandidate is one repository a `publication_due` read lists: what a
// publication's due rule reads before anything syncs (live and published packs
// and objects, last push, last publication and its build time) and its row's
// version.
//
// THE LIVE LESS THE PUBLISHED NEVER FALLS SHORT OF THE PACKS NO LAYER COVERS,
// AND MAY EXCEED THEM: a pack pushed since the last recount that is
// byte-identical to a published one counts until the next recount. A
// candidate can thus have nothing to publish, which only a plan over its packs
// tells; a due rule reading these counts alone would plan it at every poll.
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
// compaction whole, whose CompactionID is the record's operation id and Seen
// the publish's session floor.
type CompactionProposal struct {
	Compaction Compaction `json:"compaction"`
}

// CompactResult is what became of one compaction.
type CompactResult struct {
	// Outcome is applied, pending or unknown for a compaction whose record was
	// appended, empty for one refused with none. Applied is answered only from
	// the output rows the record applied, never from the decision.
	Outcome statelog.Outcome `json:"outcome,omitempty"`
	// Refusal is why it was refused with no record (stale, repo_deleted,
	// upload_expired, pack_too_large, busy, not_applied, unavailable); Detail
	// is its one sentence.
	Refusal Refusal `json:"refusal,omitempty"`
	Detail  string  `json:"detail,omitempty"`
	// Position is where its record landed and, for a not_applied or
	// repo_deleted refusal made after the append, where that record lies
	// applying nowhere.
	Position statelog.Position `json:"position,omitzero"`
}

// ExpungeProposal is what an expunge's asker hands the repository's committer
// ([Committer.ProposeExpunge]): EXACTLY ONE of a person's request, a
// maintainer's begin, its commit and its clean scan, each decided alone as a
// record of its own op. The set field's own id (RequestID, ExpungeID, CommitID,
// ScanID) is the record's operation id, and a begin's, commit's or scan's Seen
// its publish's session floor.
type ExpungeProposal struct {
	Request *ExpungeRequest `json:"request,omitempty"`
	Begin   *ExpungeBegin   `json:"begin,omitempty"`
	Commit  *ExpungeCommit  `json:"commit,omitempty"`
	Scan    *ExpungeScan    `json:"scan,omitempty"`
}

// ExpungeResult is what became of one expunge proposal.
type ExpungeResult struct {
	// Outcome is applied, pending or unknown for a proposal whose record was
	// appended, empty for one refused with none. A begin's and a commit's
	// applied is answered only from the rows of the packs the record added,
	// never from the decision.
	Outcome statelog.Outcome `json:"outcome,omitempty"`
	// Refusal is why it was refused with no record (stale, repo_deleted,
	// repo_importing, upload_expired, pack_too_large, busy, not_applied,
	// unavailable); Detail is its one sentence.
	Refusal Refusal `json:"refusal,omitempty"`
	Detail  string  `json:"detail,omitempty"`
	// Position is where its record landed (asked again under its operation id,
	// the record the ledger found) and, for a not_applied or repo_deleted
	// refusal made after the append, where that record lies applying nowhere.
	Position statelog.Position `json:"position,omitzero"`
	// Request is the purge a request's answer is about: its own id, the
	// repository's open request from its record on until a later request widens
	// it or a commit serves it. Empty for a begin and a commit.
	Request string `json:"request,omitempty"`
}

// ImportProposal is what a repository's home hands its committer to land one
// step of an import ([Committer.ProposeImport]): EXACTLY ONE of a chunk and
// the import's end, each decided alone as a record of its own op.
type ImportProposal struct {
	Chunk *ImportChunk `json:"chunk,omitempty"`
	Done  *ImportDone  `json:"done,omitempty"`
}

// MirrorSyncProposal is what a pull mirror's home hands its committer to land
// one sync ([Committer.ProposeMirrorSync]).
type MirrorSyncProposal struct {
	Sync MirrorSync `json:"sync"`
}

// SyncResult is what became of one import chunk, one import's end or one
// mirror sync.
type SyncResult struct {
	// Outcome is applied, pending or unknown for a proposal whose record was
	// appended, empty for one refused with none. A chunk's and a sync's applied
	// is answered only from the push row the record applied, never from the
	// decision; a sync moving nothing ([MirrorSync.Current]) writes no push row
	// and is answered at its record's position, as an end is.
	Outcome statelog.Outcome `json:"outcome,omitempty"`
	// Refusal is why it was refused with no record (stale, repo_deleted,
	// repo_importing, repo_archived, expunging, ref_conflict, repo_refs,
	// repo_size, upload_expired, pack_too_large, busy, not_applied,
	// unavailable); Detail is its one sentence.
	Refusal Refusal `json:"refusal,omitempty"`
	Detail  string  `json:"detail,omitempty"`
	// Position is where its record landed; see [CompactResult.Position].
	Position statelog.Position `json:"position,omitzero"`
}

// ChangeProposal is one write of a change's lifecycle as its asker hands it to
// the repository's committer ([Committer.ProposeChange]): EXACTLY ONE of an
// open, an edit, a draft's state, a retarget, a close and a reopen (each its
// op's payload, [ChangeOps]), decided alone as a record of its own. OpID is
// the record's operation id, Seen the publish's session floor. What no asker
// sends (who wrote it, an open's and a reopen's head) the committer stamps.
type ChangeProposal struct {
	// OpID is the record's operation id, minted once by the asker
	// (statelog.CheckCallerOpID), so the write asked again under it (by the
	// runner-up, or after a lost answer) is the same write, collapsed by the
	// operation ledger; an open's change id derives from it ([OpenChangeID]).
	OpID string `json:"op_id"`
	// Asker is a person through a credential bound to their human seat, a seat
	// by handle, or a token bound to nobody, which the decide refuses every
	// write. A SEAT'S IS JUDGED FROM THE CHART of the deciding node's epoch
	// ([CommitterOptions.SeatRules]), so a node holding none, or confirmed
	// behind the configuration, decides none of a seat's writes. It names no
	// run: a change keeps its opening turn and run in its own fields. A LANDING
	// NAMES NONE: it is the home's own write, which no chart judges.
	Asker Principal `json:"asker"`
	// Seen is the code log position the asker read at, zero for none. The
	// committer DECIDES AT OR PAST IT, or refuses `behind` so the runner-up
	// decides: from below it, a write formed against what the asker was shown
	// would be refused (an edit stale, an open's branch missing).
	Seen statelog.Position `json:"seen,omitzero"`
	// ExpectedVersion is the change's version the write was formed against
	// (code_changes.version, from 1 at its open); a change no longer at it is
	// refused stale. Every write names one but an open (the change does not
	// exist yet) and a landing, guarded by what its home read (head, target and
	// target's tip), none of which a title edit in between changes.
	ExpectedVersion int64 `json:"expected_version,omitempty"`
	// Ceiling is whom a SEAT'S write inside a turn somebody woke is made for
	// ([WriteCeiling]), nil for its seat alone or a person's; never a landing's.
	// The committer judges it once the seat's own rules are met
	// (decide_change.go's head): a colleague held at the least access the write
	// needs, nobody refused, but for the seat's own work, which whoever woke it
	// steers. A proposal is no record: no versionedFields row.
	Ceiling *WriteCeiling `json:"ceiling,omitempty"`
	// WrittenFor is whom a SEAT'S write is made for (ADR-0109): whoever the
	// asking turn works for, from the call's bound ceiling
	// (internal/agent/builtin), never a tool's arguments; nobody for its seat
	// alone; a person's, token's and landing's state nothing. Held to asker and
	// ceiling by [writtenForUnder]: Validate, and the deciding committer's chart.
	// The record carries it ([ChangeRouting.WorkingFor], [ChangeOpen.WorkingFor]):
	// no versionedFields row. A build lacking it refuses one carrying somebody,
	// naming args.written_for ([unknownToThisBuild]), so the walk asks one
	// reading it; one for nobody sends no member, and an older build's arrives
	// stated nobody ([ChangeProposal.UnmarshalJSONFrom]), by its ceiling.
	WrittenFor types.WrittenFor `json:"written_for,omitzero"`

	Open     *ChangeOpen     `json:"open,omitempty"`
	Edit     *ChangeEdit     `json:"edit,omitempty"`
	Draft    *ChangeDraft    `json:"draft,omitempty"`
	Retarget *ChangeRetarget `json:"retarget,omitempty"`
	Close    *ChangeClose    `json:"close,omitempty"`
	Reopen   *ChangeReopen   `json:"reopen,omitempty"`
	// Landed is a landing the home Gitlet node asks, with NO Asker: the
	// engine's own write ([ChangeLanded]).
	Landed *ChangeLanded `json:"landed,omitempty"`
}

// ChangeResult is what became of one change proposal.
type ChangeResult struct {
	// Outcome is applied, pending or unknown for a write whose record was
	// appended (or applied and Unchanged, answered from the row with no
	// record), empty for one refused. Applied is answered only from the
	// change's row the record wrote, never from the decision.
	Outcome statelog.Outcome `json:"outcome,omitempty"`
	// ChangeID is the change the write is about (an open's derived id).
	ChangeID string `json:"change_id,omitempty"`
	// Number is the change's number (web!42), read from its row: an open's is
	// the repository's next_change at its apply, which no proposal can name.
	Number int64 `json:"number,omitempty"`
	// Version is the change's version once the write applied (the asker's next
	// expectation), 0 where a later record has moved the change since: it is
	// then read again, never answered with writes its asker never saw.
	Version int64 `json:"version,omitempty"`
	// Position is where the write's record landed (for Unchanged, the row's
	// own); see [CompactResult.Position].
	Position statelog.Position `json:"position,omitzero"`
	// Unchanged is a write asking for what the change already says (an edit to
	// its own values, a close of a closed change, a reopen of a live one),
	// answered from its row with no record, at its version.
	Unchanged bool `json:"unchanged,omitempty"`
	// Refusal is why it was refused with no record; Detail is its sentence.
	Refusal ChangeRefusal `json:"refusal,omitempty"`
	Detail  string        `json:"detail,omitempty"`
}

// ConversationWrite is one write of a change's conversation as its asker hands
// it to the repository's committing data node ([ChangeWriter.Converse]):
// EXACTLY ONE of an ask for review, a comment, a thread's resolution, a
// comment's edit or delete, a review, an answer to findings and a revision's
// facts (each its op's payload, [ConversationOps]), decided as a record of its
// own on the change's subject. OpID is the record's operation id, Seen the
// publish's session floor. What no asker sends (who wrote it, a reviewer's
// kind, ids and thread) the writer stamps.
type ConversationWrite struct {
	// OpID is the record's operation id, as [ChangeProposal.OpID] is; a
	// comment's id derives from it ([CommentID]).
	OpID string `json:"op_id"`
	// Asker is a person through a credential bound to their human seat, or a
	// seat by handle; a token bound to nobody writes nothing, and a revision's
	// facts (the home's own) name none. A SEAT'S, and every reviewer an ask
	// names whoever asks ([ConversationWrite.JudgesChart]), IS JUDGED FROM THE
	// CHART of the deciding node's epoch ([ChangeWriterOptions.SeatRules]): a
	// node holding none, or confirmed behind the configuration, decides none of
	// those. It names no run: a comment keeps its turn in its own field.
	Asker Principal `json:"asker"`
	// Seen is the code log position the asker read the change at, zero for
	// none. The writer DECIDES AT OR PAST IT, or refuses `behind` so the
	// runner-up decides; see [ChangeProposal.Seen].
	Seen statelog.Position `json:"seen,omitzero"`
	// Ceiling is whom a SEAT'S write inside a turn somebody woke is made for
	// ([WriteCeiling]), nil for its seat alone or a person; never a revision's
	// facts' or mergeability's. The writer judges it beside the seat
	// (decide_conversation.go's head): a colleague held at the least access the
	// write needs (review for a review, read otherwise), nobody refused, but
	// for the seat's own work. A write is no record: no versionedFields row.
	Ceiling *WriteCeiling `json:"ceiling,omitempty"`
	// WrittenFor is whom a SEAT'S write is made for (ADR-0109), as
	// [ChangeProposal.WrittenFor] is; a person's states nothing, and the home's
	// own writes carry nobody. Held to the asker and ceiling by
	// [writtenForUnder]: by Validate, and by the deciding writer against its
	// chart. The record carries it on its routing ([ChangeRouting.WorkingFor]);
	// an older build refuses or reads it as [ChangeProposal.WrittenFor] says
	// ([ConversationWrite.UnmarshalJSONFrom]).
	WrittenFor types.WrittenFor `json:"written_for,omitzero"`

	Reviewers      *ReviewerRequest `json:"reviewers,omitempty"`
	Comment        *Comment         `json:"comment,omitempty"`
	ThreadResolve  *ThreadResolve   `json:"thread_resolve,omitempty"`
	CommentEdit    *CommentEdit     `json:"comment_edit,omitempty"`
	CommentDelete  *CommentDelete   `json:"comment_delete,omitempty"`
	Review         *Review          `json:"review,omitempty"`
	FindingResolve *FindingResolve  `json:"finding_resolve,omitempty"`
	// Revise is a revision's facts, and Mergeability whether the change's head
	// revision merges into its target's tip: the home Gitlet node's own writes,
	// with NO Asker ([Revise], [Mergeability]).
	Revise       *Revise       `json:"revise,omitempty"`
	Mergeability *Mergeability `json:"mergeability,omitempty"`
	// Asks settles the change's asks with its lifecycle, which the home asks
	// with NO Asker too ([AskSettle]). It rides the code log as the [OpAsks]
	// record, in the base format.
	Asks *AskSettle `json:"asks,omitempty"`
}

// ConversationResult is what became of one write of a change's conversation.
type ConversationResult struct {
	// Outcome is applied, pending or unknown for a write whose record was
	// appended (or applied and Unchanged, answered from the rows with no
	// record), empty for one refused. Applied is answered only once the
	// conversation holds the record's rows, never from the decision.
	Outcome statelog.Outcome `json:"outcome,omitempty"`
	// ID is what the write made that is named apart from its change (a
	// comment's id, [CommentID]; a review's, [ReviewID]), "" for none;
	// FindingIDs is a review's findings' ids in the order raised ([FindingID]).
	ID         string   `json:"id,omitempty"`
	FindingIDs []string `json:"finding_ids,omitempty"`
	// Position is where the write's record landed (for Unchanged, the
	// conversation's last record); see [CompactResult.Position].
	Position statelog.Position `json:"position,omitzero"`
	// Unchanged is a write asking for what the conversation already says (an
	// ask of reviewers already asked, a thread's current state), answered from
	// the rows with no record.
	Unchanged bool `json:"unchanged,omitempty"`
	// Refusal is why it was refused with no record; Detail is its sentence.
	Refusal ConversationRefusal `json:"refusal,omitempty"`
	Detail  string              `json:"detail,omitempty"`
}

// ChecksRequest is one write of a repository's checks handed to its committing
// data node ([ChecksWriter.Publish]): EXACTLY ONE of an attempt queued (or
// recorded skipped), a claim or its holder's naming of its box, and an ending,
// decided as a record of its own on the checks subject. The engine's own: the
// home queues and requeues, a runner claims and ends, none naming a principal.
//
// A CHECKS SWEEP IS NO REQUEST: [Store.ChecksSweep] publishes it on the
// retention duty's node, deciding only "rows past the history are held", which
// no other write races. The record format is the checks subject's rows
// ([KindChecks]): no versionedFields row.
type ChecksRequest struct {
	// OpID is the record's operation id, as [ChangeProposal.OpID] is.
	OpID string `json:"op_id"`
	// Seen is the code log position the asker read the attempt at (a runner's
	// own claim; the home's read, for a requeue), zero for none. The writer
	// DECIDES AT OR PAST IT, or refuses `behind`, as [ChangeProposal.Seen].
	Seen statelog.Position `json:"seen,omitzero"`

	Queued   *CheckQueued   `json:"queued,omitempty"`
	Started  *CheckStarted  `json:"started,omitempty"`
	Finished *CheckFinished `json:"finished,omitempty"`
	// Dispatched is the home's record of a target's checks file read at its tip
	// and the heads it dispatched under it (checksdispatch.go).
	Dispatched *CheckDispatched `json:"dispatched,omitempty"`
	// Asker, Rerun and Report are a PRINCIPAL'S write (a rerun a seat or person
	// asks for, a person's CI's report), decided into one of the records above
	// by the asker's standing (checkasks.go): a seat, or a person through a
	// credential bound to their human seat; zero on every record the engine asks
	// for itself.
	Asker  Principal      `json:"asker,omitzero"`
	Rerun  *CheckRerun    `json:"rerun,omitempty"`
	Report *CheckReported `json:"report,omitempty"`
}

// ChecksResult is what became of one write of a repository's checks.
type ChecksResult struct {
	// Outcome is applied, pending or unknown for a write whose record was
	// appended (or applied and Unchanged, answered from the rows with no
	// record), empty for one refused. Applied is answered only once the
	// attempt's row holds what the record carried, never from the decision.
	Outcome statelog.Outcome `json:"outcome,omitempty"`
	// Unchanged is a write asking for what the attempt's row already says (a
	// claim its holder resends, an ending the row's own is kept over),
	// answered from the row with no record.
	Unchanged bool `json:"unchanged,omitempty"`
	// Refusal is why it was refused with no record; Detail is its sentence.
	Refusal CheckRefusal `json:"refusal,omitempty"`
	Detail  string       `json:"detail,omitempty"`
	// Holder is the runner holding the attempt as the rows say: on a claim
	// refused `claimed`, the one that does; on a claim applied, the one the
	// join kept (the claim's own runner unless another's claim landed with a
	// lower (StartedAt, Runner)); "" for none. A runner whose claim answered
	// another holder starts no box.
	Holder string `json:"holder,omitempty"`
	// Row is the attempt as the rows hold it once the write applied, or as the
	// refusal or unchanged answer read it; nil where the repository holds none.
	Row *CheckRow `json:"row,omitempty"`
	// Position is where the write's record landed (for Unchanged, the row's
	// last record); see [CompactResult.Position].
	Position statelog.Position `json:"position,omitzero"`
}

// CheckRow is one attempt of one check of one commit as code_checks holds it:
// its key, state, and what its queue, run and ending say; every column but the
// wide ones (definition, tail, failures) a listing leaves to a read of the one
// attempt ([CheckDetail]).
type CheckRow struct {
	CheckKey
	// Repo is its repository's name, named by every person's or seat's read; a
	// box admission's read ([ReadCheckBox]) addresses the row by id and leaves
	// it out. An answer's field: no versionedFields row.
	Repo    string       `json:"repo,omitempty"`
	State   CheckState   `json:"state"`
	Purpose CheckPurpose `json:"purpose,omitempty"`
	// ChangeID and Revision are a head check's change and revision;
	// CandidateID and EntryID a candidate's queue entry.
	ChangeID    string `json:"change_id,omitempty"`
	Revision    int    `json:"revision,omitempty"`
	CandidateID string `json:"candidate_id,omitempty"`
	EntryID     string `json:"entry_id,omitempty"`
	// Change, SourceRef and ChangeHead are the change the attempt judges, on a
	// standing listing alone: the live change at a head's commit, or a
	// candidate's entry's, at its enqueued head.
	Change     *ChangeName `json:"change,omitempty"`
	SourceRef  string      `json:"source_ref,omitempty"`
	ChangeHead string      `json:"change_head,omitempty"`
	// Base is what its diff is read against; DefsOID the checks file its
	// definition came from, of the branch it is checked for (its key's Target).
	Base    string `json:"base,omitempty"`
	DefsOID string `json:"defs_oid,omitempty"`
	// Required reports a check the target's protection requires;
	// TimeoutSeconds is its definition's.
	Required       bool `json:"required,omitempty"`
	TimeoutSeconds int  `json:"timeout_seconds,omitempty"`
	// Reason is why its ending ended it, "" before one.
	Reason CheckReason `json:"reason,omitempty"`
	// Runner, Placement and Box are its claim's; Reporter is the human seat of
	// the person whose CI reported its ending.
	Runner    string         `json:"runner,omitempty"`
	Placement CheckPlacement `json:"placement,omitempty"`
	Box       string         `json:"box,omitempty"`
	Reporter  string         `json:"reporter,omitempty"`
	// QueuedAt, StartedAt and FinishedAt are its queue's, its claim's and
	// its ending's instants, each zero before it.
	QueuedAt   time.Time `json:"queued_at,omitzero"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
	DurationMs int64     `json:"duration_ms,omitempty"`
	ExitCode   *int      `json:"exit_code,omitempty"`
	// Summary is what a reader is told first; FailuresOmitted how many
	// failures its ending counted past the ones it names; URL a reported
	// run's own page.
	Summary         string `json:"summary,omitempty"`
	FailuresOmitted int    `json:"failures_omitted,omitempty"`
	URL             string `json:"url,omitempty"`
	// Queued and Finished are the positions of the records whose queue and
	// whose ending the row keeps: zero before either.
	Queued   statelog.Position `json:"queued,omitzero"`
	Finished statelog.Position `json:"finished,omitzero"`
}

// CheckDetail is one attempt read whole ([ReadCheck]): its row, its definition,
// its output's tail, its ending's failures, a failed run's kept log, its test
// counts and its output's lines, each nil or empty where it carries none.
type CheckDetail struct {
	CheckRow
	Def  *CheckDef `json:"def,omitempty"`
	Tail string    `json:"tail,omitempty"`
	// Tests, LogLines and TailCut are its ending's ([CheckFinished]);
	// TailFirstLine is Tail's first line's number in the whole output
	// ([TailFirstLine]), 0 where lines were not counted. Each is absent from an
	// ending before [CheckCountsVersion].
	Tests         *CheckTests    `json:"tests,omitempty"`
	LogLines      int            `json:"log_lines,omitempty"`
	TailFirstLine int            `json:"tail_first_line,omitempty"`
	TailCut       bool           `json:"tail_cut,omitempty"`
	Failures      []CheckFailure `json:"failures,omitempty"`
	Log           *CheckLog      `json:"log,omitempty"`
}

// CheckQuery narrows a `checks` read to one change's attempts or one
// commit's, or names the one attempt a `check` read is of — each on the
// repository the read names ([ReadQuery.Repo]).
type CheckQuery struct {
	// Change is a change's number: the attempts judging it (its head's, checked
	// for its target), or beside OID that commit's checked for its target. A
	// `checks` read's alone.
	Change int64 `json:"change,omitempty"`
	// OID is a commit's whole id: its attempts (checked for every target, the
	// target of Change's change, or the Target beside it), and the commit a
	// `check` read's attempt is of.
	OID string `json:"oid,omitempty"`
	// Target, Name and Attempt are the one attempt a `check` read is of:
	// REQUIRED there. A `checks` read takes a Target only beside an OID and no
	// Change (a range of code_checks' key), refuses an Attempt, and takes a
	// Name as a NARROWING to that check's attempts.
	Target  string `json:"target,omitempty"`
	Name    string `json:"name,omitempty"`
	Attempt int    `json:"attempt,omitempty"`
	// Latest answers each check's LATEST attempt per branch of a change's or
	// commit's attempts: what it STANDS AT, which a merge gate, a queue's meter
	// and an Inbox card read; rebuilt from a page-cut history, a rerun across a
	// page's end would draw its superseded attempt. Refused on a repository's
	// listing and the window and on a `check` read. ONE WALK OF code_checks'
	// KEY, keeping a row only where no later attempt follows, so a page is at
	// most one row a check and target: at most [MaxChecksPerFile], an unlimited
	// Latest listing's page ([latestCheckLimit]). No versionedFields row.
	Latest bool `json:"latest,omitempty"`
	// State narrows a `checks` read to one state; refused on a `check` read and
	// on the window where not an ending ([CheckState.Ended]): the window lists
	// ENDED attempts.
	//
	// THE STATEMENT'S, never a filter of a page's rows, so a page costs what it
	// answers. A repository's listing and the window SEEK an index led by the
	// narrowing: the state, or the name then state ([narrowedPlaces]). The
	// repository's two move with each of an attempt's three records, as
	// code_checks_finished_idx does; the window's two are PARTIAL on ended rows,
	// as code_checks_recent_idx is. A change's and a commit's walks test it
	// inside. No versionedFields row.
	State CheckState `json:"state,omitempty"`
	// Since bounds a `checks` read of EVERY repository (the window) to the
	// attempts ended at or after it; zero is all the rows keep
	// ([CheckHistory]). Refused beside a repository.
	Since time.Time `json:"since,omitzero"`
	// Until is the window's END: the attempts that ended BEFORE it, so a past
	// interval is a seek of code_checks_recent_idx from its end down and its
	// first page is that interval's newest ending. Zero is no end. Refused
	// beside a repository, at or before Since, and on a `check` read.
	Until time.Time `json:"until,omitzero"`
	// Standing lists where each check stands, in its order ([StandingOrder]):
	// each key's latest attempt of a live change's head, its newest queue entry
	// or a branch's tip, running ones included; "" is the listing above.
	Standing StandingOrder `json:"standing,omitempty"`
}

// ChecksDueQuery is a `checks_due` read's question, REQUIRED as a `changes_due`
// read's is: the instant a target's checks file re-reading is due by, and, for
// ONE repository's due check work, the repository by id.
type ChecksDueQuery struct {
	RepoID string `json:"repo_id,omitempty"`
	// RefreshBefore is the home's clock less its refresh: a target whose branch
	// moved past the tip its checks file was read at is due a re-reading once
	// that reading was recorded at or before it, so a target pushed on every
	// landing is read at the home's pace, never its own.
	RefreshBefore time.Time `json:"refresh_before"`
	// TipsAfter is, for one repository's due work with no cursor, the protected
	// target its tip work continues after: the previous [ReadAnswer.TipsNext].
	TipsAfter string `json:"tips_after,omitempty"`
}

// ChecksRunningQuery is a `checks_running` read's question: the repository
// whose running attempts it reads, by id in its canonical spelling, or "" for
// the repositories holding one.
type ChecksRunningQuery struct {
	RepoID string `json:"repo_id,omitempty"`
}

// RunnableCheck is one attempt a `checks_queued` read lists: its row, its
// repository's name (what its box, clone directory and token name it by) and
// the definition it was queued under, which its box runs whatever the target's
// file says now.
type RunnableCheck struct {
	CheckRow
	Repo string    `json:"repo"`
	Def  *CheckDef `json:"def,omitempty"`
}

// CheckTargetDue is a target branch whose checks file its home owes a reading
// at its tip ([ReadAnswer.DueTargets]).
type CheckTargetDue struct {
	TargetRef   string            `json:"target_ref"`
	Tip         string            `json:"tip"`
	TipPosition statelog.Position `json:"tip_position"`
}

// RepoSummary is a repository as a listing renders it.
type RepoSummary struct {
	RepoRef
	// Access is the asker's standing ([Grant.Access] by name), never none: an
	// unreadable repository is not listed.
	Access string `json:"access,omitempty"`
	// Head is the default branch's oid; empty for an empty repository.
	Head string `json:"head,omitempty"`
	// Pushes is pushes EVER.
	Pushes     int64     `json:"pushes"`
	LastPushAt time.Time `json:"last_push_at,omitzero"`
	// Spark is its pushes on each of the asker's days, oldest first, zeros
	// included: Spark[i] those landed in [DayBounds[i], DayBounds[i+1]).
	// Answered only where RepoFacts asks with DayBounds.
	Spark []int64 `json:"spark,omitempty"`
	// ChangeCounts is its changes by state, answered only where [ReadQuery.RepoFacts] asks.
	ChangeCounts *ChangeCounts `json:"change_counts,omitempty"`
	// Card is what a listing's row draws of it, where [ReadQuery.RepoFacts]
	// asks; each part absent where unknown or none.
	Card *RepoCard `json:"card,omitempty"`
}

// RepoDetail is one repository in detail.
type RepoDetail struct {
	RepoSummary
	// Branches and Tags are the live refs under refs/heads/ and refs/tags/.
	Branches int64 `json:"branches"`
	Tags     int64 `json:"tags"`
	// Tombstones is the deleted refs still held as rows so a cache syncing a
	// delta learns of each deletion, until a push sweeps them
	// RefTombstoneHorizon after the deletion (or sooner, oldest first, past
	// MaxTombstonesPerRepo).
	Tombstones int64     `json:"tombstones"`
	CreatedAt  time.Time `json:"created_at"`
	// Limits are the SERVING node's epoch's: what an import chunks to.
	Limits Limits `json:"limits"`
	// PathOwners are who owns which paths ([RepoSettings.PathOwners]): on the
	// detail alone, never the [RepoRef] every push, fetch and listing reads, and
	// only to a read that asks ([ReadQuery.PathOwners]). At their bound
	// ([MaxPathOwnersBytes]) the detail still fits beside a page in the estate's
	// reply ([MaxPageBytes]; the limits test holds it).
	PathOwners []PathOwner `json:"path_owners,omitempty"`
	// Gates are the merge gates of its rules that carry one, each named by its
	// rule's pattern, in pattern order ([RuleGate]); [RepoRef.Protections] never
	// carry them. Only to a read that asks ([ReadQuery.Gates]), for the path
	// owners' reason.
	Gates []RuleGate `json:"gates,omitempty"`
	// RuleSources are who set each rule and when ([RuleSource]), every rule's
	// in pattern order, to the read asking for gates alone, read in the
	// transaction of the row's rules so every source names a rule of the row.
	RuleSources []RuleSource `json:"rule_sources,omitempty"`
	// Pushers is who pushed on the UTC days beginning within the asker's day
	// bounds, most first: answered only where RepoFacts asks with DayBounds
	// ([Pushers]).
	Pushers *Pushers `json:"pushers,omitempty"`
	// Expunge is where the repository's expunges stand ([ExpungeState]), read
	// from its own row, so a person who asked for a purge reads its progress.
	Expunge ExpungeState `json:"expunge,omitzero"`
	// Remotes is where it stands with other hosts ([RemoteState]), on the
	// detail alone, never on the [RepoRef] every push and fetch reads, which
	// needs only [RepoRef.Mirrored].
	Remotes RemoteState `json:"remotes,omitzero"`
	// Version is the position of the record that last moved its row
	// (code_repos.version): a read listing what its refs hold floors there.
	Version statelog.Position `json:"version,omitzero"`
}

// RuleSource is who set one of a repository's rules and when, named by its
// pattern: the decider of the record that last CHANGED the rule (its push
// controls or gate) and that record's instant (code_protections). A record
// carrying the rule unchanged, as every settings change carries every rule,
// leaves its source ([rulesSourced]).
//
// A READ'S ANSWER, derived from every record's envelope decider and broker
// instant, so no versionedFields row.
type RuleSource struct {
	Pattern string `json:"pattern"`
	// SetBy is the decider as the record names it: a seat's handle (SetByKind
	// agent), a person's human seat or an unbound credential's login (operator,
	// iam.ActorFor), "" where the configuration set it (config).
	SetBy     string    `json:"set_by,omitempty"`
	SetByKind ActorKind `json:"set_by_kind"`
	SetAt     time.Time `json:"set_at"`
}

// RemoteState is a repository's other hosts as its row holds them (code_repos):
// zero for one with no mirror, never imported. A read's answer, so no
// versionedFields row.
//
// EVERY CREDENTIAL HERE IS A REFERENCE, one whole ${VAR} the domain refuses any
// other value for, so a reader is served a variable's name, never a token.
type RemoteState struct {
	// MirrorFrom and MirrorTo are the repository's pull and push mirrors as
	// its settings carry them ([RepoSettings.MirrorFrom]), nil for none.
	MirrorFrom *MirrorSource `json:"mirror_from,omitempty"`
	MirrorTo   *MirrorTarget `json:"mirror_to,omitempty"`
	// Import is the import its create filled it from (its own, or a pull
	// mirror's first fetch) and how far it went; nil for none.
	Import *ImportState `json:"import,omitempty"`
	// MirrorSyncedAt is a pull mirror's last sync's broker instant, zero for
	// never and for a repository that is no pull mirror.
	MirrorSyncedAt time.Time `json:"mirror_synced_at,omitzero"`
}

// ImportState is one repository's import as its row holds it.
type ImportState struct {
	// Source is what the create imported from, fixed then
	// ([CreateRepo.Import]).
	Source ImportSource `json:"source"`
	// Chunks is how many of its chunks have landed ([ImportChunk.Seq]).
	Chunks int64 `json:"chunks"`
	// DoneAt is the broker instant of the record that ended it, zero while it
	// runs; Error why it ended short, "" if it finished.
	DoneAt time.Time `json:"done_at,omitzero"`
	Error  string    `json:"error,omitempty"`
}

// ExpungeState is a repository's expunges as its row holds them (code_repos,
// written by the expunge's three records alone): zero for one never expunged,
// with none open and no purge asked.
type ExpungeState struct {
	// Open is the expunge begun and not committed, by id, "" for none: while
	// open, objects no ref reached at its scan are still served, and no new
	// push may rely on them.
	Open string `json:"open,omitempty"`
	// Request is a person's unserved purge, by id, and Requested its record's
	// position: served once no pack holds content from before it. "" and zero
	// for none.
	Request   string            `json:"request,omitempty"`
	Requested statelog.Position `json:"requested,omitzero"`
	// ExpungedAt is the last commit's broker instant, zero for never;
	// DroppedObjects and DroppedBytes what that commit took out of the live
	// packs.
	ExpungedAt     time.Time `json:"expunged_at,omitzero"`
	DroppedObjects int64     `json:"dropped_objects,omitempty"`
	DroppedBytes   int64     `json:"dropped_bytes,omitempty"`
}

// ChangeRow is a change as code_changes holds it: every column, as every read
// of one serves it (a listing's [ChangeSummary], a detail's [ChangeDetail]).
type ChangeRow struct {
	ID     string `json:"id"`
	RepoID string `json:"repo_id"`
	// Number is the change's on its repository (web!42's 42): the repository's
	// next_change at the apply that opened it.
	Number int64 `json:"number"`
	// Title is the line it was opened with, or chosen by whoever edited it
	// since: somebody's words (ADR-0040).
	Title string      `json:"title" untrusted:"name"`
	State ChangeState `json:"state"`
	// SourceRef is the branch whose head the change proposes, and TargetRef
	// the branch it proposes it into, each in full (refs/heads/feature).
	SourceRef string `json:"source_ref"`
	TargetRef string `json:"target_ref"`
	// Head is the source branch's head at the latest revision; Revision that
	// revision's number, which is how many it has.
	Head     string `json:"head"`
	Revision int64  `json:"revision"`
	// Owner is who opened it (a seat's handle, or a person's human seat) and
	// OwnerKind which; OwnerOperatorID is the credential a person opened it
	// with (the change's own attribution), "" for a seat's.
	Owner           string    `json:"owner"`
	OwnerKind       PartyKind `json:"owner_kind"`
	OwnerOperatorID string    `json:"owner_operator_id,omitempty"`
	// WorkItem is the work item the change carries, nil for none.
	WorkItem *types.WorkItem `json:"work_item,omitempty"`
	// CreatedTurn and CreatedRun are the turn and executor launch that opened
	// it, "" where none (a person's open has neither).
	CreatedTurn string `json:"created_turn,omitempty"`
	CreatedRun  string `json:"created_run,omitempty"`
	// OpenedAt is the opening record's broker instant and OpenedPosition its
	// code log position: what a cross-repository listing orders by.
	OpenedAt       time.Time         `json:"opened_at"`
	OpenedPosition statelog.Position `json:"opened_position"`
	// ClosedAt is set exactly while the change is closed, with CloseReason
	// (SourceDeleted for one a push closed by deleting its source); MergedAt
	// exactly while merged, with MergedVia and the commit its target was merged
	// at (MergeOID).
	ClosedAt    time.Time `json:"closed_at,omitzero"`
	CloseReason string    `json:"close_reason,omitempty" untrusted:"name"`
	// ClosedBy is who closed it, while closed: the close's principal or the
	// pusher who deleted its source (SourceDeleted), ClosedByKind which kind;
	// each "" for a push nobody made (an import's, a mirror's).
	ClosedBy     string    `json:"closed_by,omitempty"`
	ClosedByKind PartyKind `json:"closed_by_kind,omitempty"`
	MergedAt     time.Time `json:"merged_at,omitzero"`
	MergedVia    MergedVia `json:"merged_via,omitempty"`
	MergeOID     string    `json:"merge_oid,omitempty"`
	// MergedBy is who merged it through its gate, a merge record's principal
	// ([Merge]): a seat's handle or a person's human seat, MergedByKind which;
	// MergedOperatorID the credential a person used; OverrideReason a person's
	// reason for merging past the gate. Each "" but on a change a merge record
	// merged: a push that merged one names its pusher on its push row.
	MergedBy         string    `json:"merged_by,omitempty"`
	MergedByKind     PartyKind `json:"merged_by_kind,omitempty"`
	MergedOperatorID string    `json:"merged_operator_id,omitempty"`
	OverrideReason   string    `json:"override_reason,omitempty"`
	// BodyBytes is its description's length, 0 for none: a listing says a
	// change has one without carrying it ([ChangeDetail.Body]).
	BodyBytes int64 `json:"body_bytes,omitempty"`
	// Version counts the records that changed the row (1 at its open), what a
	// lifecycle write names as its expectation
	// ([ChangeProposal.ExpectedVersion]); Position is where the last landed.
	Version  int64             `json:"version"`
	Position statelog.Position `json:"position"`
}

// ChangeSummary is a change as a listing renders it: its row, its repository's
// name and its reference (web!42), and the asker's standing on the repository.
type ChangeSummary struct {
	ChangeRow
	Repo string `json:"repo"`
	// Ref is [ChangeRef] of the repository's name and the change's number.
	Ref string `json:"ref"`
	// Access is the asker's access to the change's repository ([Grant.Access]
	// by name), never none: a change of an unreadable repository is not listed.
	Access string `json:"access,omitempty"`
	// Readiness is whether the change may merge, judged in the read's one
	// transaction against its one chart ([ReadinessOf]), never the stored
	// answer ([MergeabilityRow.Ready]); nil for a change no longer live. A
	// change read in brief judges none ([ReadAnswer.ChangeBrief]).
	Readiness *Readiness `json:"readiness,omitempty"`
	// OpenFindings is how many findings stand open, answered by none of its
	// authors (code_conversations.open_findings); 0 for an unreviewed change.
	OpenFindings int64 `json:"open_findings"`
	// UpdatedAt is when the change last moved: the later of the last record on
	// its repository's subject changing its row (open, edit, close, merge, a
	// push moving its head) and the last somebody wrote about it on its own
	// subject (an ask, a comment, a review, an answer to a finding), never the
	// home's facts. What a window listing ([ReadQuery.Window]) keeps. A change
	// read in brief carries its lifecycle's last move alone and no findings
	// count. Answer fields: no versionedFields row.
	UpdatedAt time.Time `json:"updated_at"`
	// Card is what a listing's row draws beside its readiness, where
	// [ReadQuery.Cards] asks; nil is not answered, never "nothing to say".
	Card *ChangeCard `json:"card,omitempty"`
	// WaitsFor is, on a needs=me listing, the half that listed the change
	// (review or fix); "" on every other listing.
	WaitsFor Need `json:"waits_for,omitempty"`
}

// ChangeDetail is one change in detail: its summary, its description and its
// authors. Its revisions are paged beside it ([ReadAnswer.Revisions]).
type ChangeDetail struct {
	ChangeSummary
	// Body is its description, "" for none.
	Body string `json:"body,omitempty" untrusted:"fence"`
	// Authors is everyone who wrote it, by handle: its opener and everyone
	// whose push moved its head.
	Authors []ChangeAuthor `json:"authors"`
	// Conversation is its conversation's counters, nil for an undiscussed
	// change; its rows are read a part at a time ([ReadQuery.Part]).
	Conversation *ConversationRow `json:"conversation,omitempty"`
	// Mergeability is whether its latest merged revision merges into its
	// target, as its home recorded it; nil before the home merged any. One of
	// an earlier revision or into another branch is the home's to merge again.
	Mergeability *MergeabilityRow `json:"mergeability,omitempty"`
	// TargetTip is its target branch's tip as read, "" with no live branch: what
	// a stale Mergeability.TargetOID is checked against and a fast-forward needs
	// its head to hold.
	TargetTip string `json:"target_tip,omitempty"`
	// Participants is who takes part in its conversation (code_participants),
	// by handle, at most MaxDetailParticipants; nil where not answered.
	Participants *ParticipantList `json:"participants,omitempty"`
	// RevertedBy is the latest change opened to revert this merged one
	// (code_changes.reverts), nil for none.
	RevertedBy *ChangeName `json:"reverted_by,omitempty"`
}

// ConversationRow is a change's conversation's counters (code_conversations):
// its comments and reviews; its OPEN findings, unanswered by its authors; its
// STANDING BLOCKERS, which hold it back whatever its authors answered until
// their raiser's later review or a dismissal clears them (S40); how many
// reviewers' latest verdict asks for changes; the version and position of the
// last record on the change's subject (the floor a read of its parts is asked
// at); and when somebody last wrote on it.
type ConversationRow struct {
	Comments         int64             `json:"comments"`
	Reviews          int64             `json:"reviews"`
	OpenFindings     int64             `json:"open_findings"`
	OpenBlockers     int64             `json:"open_blockers"`
	ChangesRequested int64             `json:"changes_requested"`
	Version          int64             `json:"version"`
	Position         statelog.Position `json:"position"`
	// SpokenAt is the broker instant of the last record somebody wrote on the
	// change's subject (an ask, a comment, a thread resolved, a review, an
	// answer to a finding; code_conversations.updated_at), zero where every
	// record was the home's or the engine's. Unlike Version, which every record
	// moves (a revision's facts, every mergeability), it moves only with what
	// reviews and comments are rows of, so a reader holding pages of those asks
	// again only when it moves.
	SpokenAt time.Time `json:"spoken_at,omitzero"`
}

// ReviewRow is one review of a change (code_reviews): the revision judged, the
// reviewer (a seat, or a person with the credential used), verdict and summary,
// findings raised, the reviewing turn and model, and its record's position and
// broker instant.
type ReviewRow struct {
	ID           string            `json:"id"`
	Revision     int64             `json:"revision"`
	Reviewer     string            `json:"reviewer"`
	ReviewerKind PartyKind         `json:"reviewer_kind"`
	OperatorID   string            `json:"operator_id,omitempty"`
	Verdict      Verdict           `json:"verdict"`
	Summary      string            `json:"summary,omitempty" untrusted:"fence"`
	Findings     int64             `json:"findings"`
	TurnID       string            `json:"turn_id,omitempty"`
	Model        string            `json:"model,omitempty"`
	Position     statelog.Position `json:"position"`
	At           time.Time         `json:"at"`
	// Change is the change it judged, on a `reviews` listing's rows, which span
	// changes; nil on a `change` read's.
	Change *ChangeName `json:"change,omitempty"`
}

// ChangeName is a change as a row read apart from it names it (a review a
// `reviews` listing found, a `change_routing` page's change): its repository by
// name, its reference (web!42), title and state, as the read found its row.
type ChangeName struct {
	Repo  string      `json:"repo"`
	Ref   string      `json:"ref"`
	Title string      `json:"title"`
	State ChangeState `json:"state"`
}

// RoutedPage is a `change_routing` read's page (routedread.go): the change, a
// page of the records about it that reached anybody, newest first, each with
// every recipient, and what the read can say of whom its records reached where
// it lists none.
type RoutedPage struct {
	Change ChangeName `json:"change"`
	// OpenedAt is the opening record's broker instant: the oldest record about
	// the change, which its Delivery is dated by.
	OpenedAt time.Time      `json:"opened_at"`
	Records  []RoutedRecord `json:"records"`
	// Delivery is what the read can say of whom the change's records reached
	// ([routing.DeliveryOf]): `reached` where a record is listed (and on every
	// later page); where none is, `nobody` for a change opened since
	// RetainedFrom, all of whose notices would still be kept, and `swept` for
	// one opened before. Every record says whom it concerns, so `quiet` and
	// `unknown` are never this read's.
	Delivery routing.Delivery `json:"delivery"`
	// RetainedFrom is the instant from which every notice is kept: the read's
	// instant less [SeatNoticeHorizon], the shorter horizon (a person's,
	// inbox.retention_days, is never under thirty days). An older record lists
	// only the persons it reached: a seat's notice went with the turn detail it
	// named.
	RetainedFrom time.Time `json:"retained_from"`
}

// RoutedRecord is one record about a change and whom it reached: its operation
// id (code_notifications.record_id), code log position and broker instant, what
// its notices kept of what it says (an open's description, a comment's body, a
// review's summary, or nothing), and every recipient its notices name,
// addressed first, a fallback last, each group by handle.
type RoutedRecord struct {
	RecordID   string            `json:"record_id"`
	Position   statelog.Position `json:"position"`
	At         time.Time         `json:"at"`
	Excerpt    string            `json:"excerpt,omitempty"`
	Recipients []RoutedRecipient `json:"recipients"`
}

// RoutedRecipient is one handle a record reached: a seat's, or a person's human
// seat ([PartyKind] says which, and so which horizon keeps its notice), the ONE
// reason it heard under ([Reasons]' precedence), whether that asks something of
// it, and, for a recipient reached only because nobody better was found, which
// substitute it was.
type RoutedRecipient struct {
	Handle       string    `json:"handle"`
	Kind         PartyKind `json:"kind"`
	Reason       Reason    `json:"reason"`
	Addressed    bool      `json:"addressed,omitempty"`
	Fallback     bool      `json:"fallback,omitempty"`
	FallbackRank int       `json:"fallback_rank,omitempty"`
}

// FindingRow is one finding on a change (code_findings, its text in
// code_finding_texts): its review and raiser, the revision it was raised on,
// where it points (path, diff side, lines, and the commit and blob they were
// read in), its weight, subject and words, the replacement lines it suggests,
// its STATE, and its last answer (the note, who gave it, the commit it names
// and the record that gave it).
type FindingRow struct {
	ID               string            `json:"id"`
	ReviewID         string            `json:"review_id"`
	Raiser           string            `json:"raiser"`
	RaiserKind       PartyKind         `json:"raiser_kind"`
	Revision         int64             `json:"revision"`
	CommitOID        string            `json:"commit_oid"`
	Path             string            `json:"path"`
	BlobOID          string            `json:"blob_oid"`
	Side             Side              `json:"side"`
	LineStart        int               `json:"line_start"`
	LineEnd          int               `json:"line_end"`
	Severity         Severity          `json:"severity"`
	Category         Category          `json:"category"`
	Title            string            `json:"title" untrusted:"name"`
	Body             string            `json:"body,omitempty" untrusted:"fence"`
	Suggestion       *string           `json:"suggestion,omitempty"`
	State            FindingState      `json:"state"`
	ResolutionNote   string            `json:"resolution_note,omitempty" untrusted:"name"`
	ResolvedBy       string            `json:"resolved_by,omitempty"`
	ResolvedByKind   PartyKind         `json:"resolved_by_kind,omitempty"`
	ResolvedCommit   string            `json:"resolved_commit,omitempty"`
	ResolvedPosition statelog.Position `json:"resolved_position,omitzero"`
	Position         statelog.Position `json:"position"`
	// RaisedAt is the broker instant of the review that raised it (code_reviews.at).
	RaisedAt time.Time `json:"raised_at,omitzero"`
}

// ReviewerRow is one reviewer a change has asked (code_reviewers): a seat's
// handle or a person's human seat, why asked, whether the ask stands, who asked
// with what note, and the record that last moved it.
type ReviewerRow struct {
	Reviewer        string            `json:"reviewer"`
	Kind            PartyKind         `json:"kind"`
	Reason          ReviewerReason    `json:"reason"`
	State           ReviewerState     `json:"state"`
	RequestedBy     string            `json:"requested_by"`
	RequestedByKind PartyKind         `json:"requested_by_kind"`
	Note            string            `json:"note,omitempty"`
	Position        statelog.Position `json:"position"`
	// At is when the record at Position landed, read from one of its notices;
	// zero where none is kept (past the notices' horizon, or a record telling
	// nobody).
	At time.Time `json:"at,omitzero"`
	// OwnedPaths is the globs a path owner's ask is for (OwnerAsk.Paths),
	// OwnedPathsOmitted how many more; none on an ask by name or before
	// [OwnedPathsVersion].
	OwnedPaths        []string `json:"owned_paths,omitempty"`
	OwnedPathsOmitted int      `json:"owned_paths_omitted,omitempty"`
}

// CommentRow is one comment on a change (code_comments): its thread (its own id
// for a thread's first comment, a finding's for a reply to one), what it
// replies to, its author, the line a thread's first comment points at, whether
// and by whom its thread is resolved, the turn that said it, its words, its
// record's position and instant, and any edit or deletion (which clears its
// words and keeps its place).
type CommentRow struct {
	ID               string            `json:"id"`
	ThreadID         string            `json:"thread_id"`
	ParentID         string            `json:"parent_id,omitempty"`
	FindingID        string            `json:"finding_id,omitempty"`
	Author           string            `json:"author"`
	AuthorKind       PartyKind         `json:"author_kind"`
	AuthorOperatorID string            `json:"author_operator_id,omitempty"`
	Anchor           *CommentAnchor    `json:"anchor,omitempty"`
	Resolved         bool              `json:"resolved,omitempty"`
	ResolvedBy       string            `json:"resolved_by,omitempty"`
	ResolvedByKind   PartyKind         `json:"resolved_by_kind,omitempty"`
	TurnID           string            `json:"turn_id,omitempty"`
	Body             string            `json:"body" untrusted:"fence"`
	Position         statelog.Position `json:"position"`
	At               time.Time         `json:"at"`
	EditedAt         time.Time         `json:"edited_at,omitzero"`
	DeletedAt        time.Time         `json:"deleted_at,omitzero"`
	DeletedBy        string            `json:"deleted_by,omitempty"`
	DeletedByKind    PartyKind         `json:"deleted_by_kind,omitempty"`
}

// ChangeAuthor is one author of a change (code_change_authors): a seat's handle
// or a person's human seat, from the revision they first wrote. An author's
// approval of their own change counts for nothing.
type ChangeAuthor struct {
	Author        string            `json:"author"`
	Kind          PartyKind         `json:"kind"`
	FirstRevision int64             `json:"first_revision"`
	Position      statelog.Position `json:"position"`
}

// RevisionRow is one revision of a change (code_revisions): a move of its
// source head while live (new head, who moved it, the push that did, empty for
// an open or reopen), its record's position and instant, and the facts the home
// recorded of it, nil before any.
type RevisionRow struct {
	N          int64             `json:"n"`
	Head       string            `json:"head"`
	Pusher     string            `json:"pusher"`
	PusherKind PartyKind         `json:"pusher_kind"`
	PushID     string            `json:"push_id,omitempty"`
	Position   statelog.Position `json:"position"`
	At         time.Time         `json:"at"`
	// Via is how the push that made it was built ("" for one no push made) and
	// Forced whether it rewrote the source branch: a force-push or, through the
	// engine, a rebase onto its target ([WriteRebaseChange]).
	Via    PushVia           `json:"via,omitempty"`
	Forced bool              `json:"forced,omitempty"`
	Facts  *RevisionFactsRow `json:"facts,omitempty"`
}

// RevisionFactsRow is a revision's facts as the change's home recorded them
// (code_revision_facts, [Revise]): the target branch and tip read against, the
// base, base and head trees, patch id (or that none was taken), the files
// touched (or that it touches all), their change classes, the recording node,
// the path-owner match and the writing record. A revision whose head its target
// holds has none: its change is merged and its home lands it. Two revisions
// naming the same trees hold the same code ([ChangeClassNoCodeChange]).
type RevisionFactsRow struct {
	TargetRef      string            `json:"target_ref"`
	TargetOID      string            `json:"target_oid"`
	TargetPosition statelog.Position `json:"target_position"`
	BaseOID        string            `json:"base_oid,omitempty"`
	BaseTree       string            `json:"base_tree"`
	HeadTree       string            `json:"head_tree"`
	PatchID        string            `json:"patch_id,omitempty"`
	PatchIDOmitted bool              `json:"patch_id_omitted,omitempty"`
	Touched        []string          `json:"touched"`
	TouchesAll     bool              `json:"touches_all,omitempty"`
	Classes        RevisionClasses   `json:"classes,omitzero"`
	Node           string            `json:"node"`
	// MeasuredAt is when its home read them, by its clock; zero for facts
	// recorded before [OwnedPathsVersion].
	MeasuredAt      time.Time         `json:"measured_at,omitzero"`
	OwnersDigest    string            `json:"owners_digest"`
	RequestedOwners []Owner           `json:"requested_owners,omitempty"`
	SkippedOwners   []SkippedOwner    `json:"skipped_owners,omitempty"`
	Position        statelog.Position `json:"position"`
}

// THE READS BY OBJECT ID: what a Gitlet node answers about content (a tree, a
// file's lines, a diff, a comparison) from its cache (internal/gitlet/query),
// never the rows.
//
// EVERY REVISION IS AN OBJECT ID, whole or abbreviated ([ValidRev]), never a
// ref, `^`, `~` or `:`: an id names one object everywhere, needing no level,
// and no revision grammar reaches git. A PATH is data, never an option: on
// stdin, or the one argument after `--`, a log's as a literal pathspec.
//
// EVERY ANSWER IS ONE PAGE of at most [MaxGitAnswerBytes] JSON with a cursor
// (GitAnswer.Next); an item too large is refused [ErrSliceTooLarge], never cut.

// MaxGitAnswerBytes is the most one GitAnswer's JSON carries: the 64 KiB a
// seat's tool answers in (internal/agent/builtin's ToolAnswerBytes) less 8 KiB
// for what a tool wraps around a page, so a page is always one tool answer.
// Spelled here because builtin imports this package.
const MaxGitAnswerBytes = 56 << 10

// MaxGitPathBytes is the longest path a read by oid names: Linux's PATH_MAX, a
// bound on what the asker can make a Gitlet node look up.
const MaxGitPathBytes = 4 << 10

// MaxDiffPaths is the most paths one diff's stat is filtered by: a list of
// what the asker cares about, never a repository's listing.
const MaxDiffPaths = 64

// MinAbbrevDigits is the fewest hex digits an abbreviated object id may have:
// git's default core.abbrev.
const MinAbbrevDigits = 7

// maxGitCursorBytes bounds a returned cursor: every cursor a Gitlet node issues
// is a kind's letter and up to two decimal numbers, or a tags page's date and
// tag name, 349 bytes at the widest.
const maxGitCursorBytes = 384

// MaxTouchedPaths is the most paths a revision's facts list as touched
// ([RevisionFactsAnswer.Touched]). Past it the revision TOUCHES ALL, with no
// list: the safe side for every reader (an interdiff shows every differing
// file, every path rule matches), where a cut list would leave files out.
//
// A THOUSAND: far more than a change reviewed file by file touches, so only
// changes nobody reads that way (a formatter run, a vendored tree) touch all,
// and few enough that matching rules on every revision stays cheap.
// [MaxTouchedBytes] bounds the bytes beside it, whichever comes first.
const MaxTouchedPaths = 1000

// MaxTouchedBytes is the most the touched list may take AS JSON (every path
// quoted and escaped as encoding/json writes it) in a revision's facts and
// whatever carries them; past it the revision touches all.
//
// 48 KiB, SO A FACTS ANSWER IS ALWAYS ONE PAGE: an answer is at most
// [MaxGitAnswerBytes], and the facts' ids, flags and stamps fit in the rest. As
// JSON, because a path of `<` costs six bytes there. At the bound it is about a
// twentieth of MaxRecordBytes, so a record carrying the facts fits beside the
// rest.
const MaxTouchedBytes = 48 << 10

// MaxReanchors is the most anchors one re-anchoring read places
// ([ReanchorQuery]): each is two lines of the read's one check and at most one
// pair of its one diff process, so sixty-four (one review's findings at most)
// keep the check and the answer far inside their bounds.
const MaxReanchors = 64

// MaxAnchorLine is the last line an anchor's range may name: a line a byte at
// the largest blob code.limits.max_blob_bytes allows
// (config.CodeMaxPushBytesCeiling, 1 GiB). Placing moves a range by at most its
// new file's lines, so a range within it moves within an int rather than
// wrapping.
const MaxAnchorLine = int(config.CodeMaxPushBytesCeiling)

// maxAnchorIDBytes bounds an anchor's id, the opaque name the caller reads its
// answer back by (a finding's UUID is 36 characters). MaxReanchors ids, every
// byte encoded in six, are under 30 KiB at the widest.
const maxAnchorIDBytes = 64

// THE BOUNDS OF A LOG AND A BLAME. Every field git leaves unbounded (a commit's
// subject, author name and address, parents, a blamed line's text) is
// DESCRIBED, NEVER CUT, past its bound: answered absent beside its size, so a
// page never carries half a field read as whole. Each bound is of the ENCODED
// size (its JSON, every control or non-UTF-8 byte six, with the escape a code
// tool's fence writes into it), so the widest commit and blamed line always fit
// one page: 4 KiB of control bytes would encode to 24 KiB.

// MaxLogLimit is the most commits one log page lists, DefaultLogLimit how many
// a read naming none lists. A page also ends before the commit whose encoding
// would pass MaxGitAnswerBytes, so a hundred 4 KiB subjects take several pages,
// never one too large. TWENTY-FIVE by default: the screen a "when did this
// change" question reads first.
const (
	MaxLogLimit     = 100
	DefaultLogLimit = 25
)

// MaxLogGrepBytes is the longest literal a log is kept to: a few words of a
// message, short enough that the streaming matcher's table is nothing beside
// the page.
const MaxLogGrepBytes = 256

// MaxLogSubjectBytes is the most a commit's SUBJECT (git's %s) may encode to
// and be answered: 4 KiB holds any human subject, and a page at its bound is
// still a dozen commits; past it, described by size (LogCommit.SubjectBytes).
const MaxLogSubjectBytes = 4 << 10

// MaxLogPersonBytes is the most a commit's author's or committer's name or
// address may encode to and be answered. A JUDGMENT: git and fsck bound
// neither, RFC 5321 bounds an address at 254 octets, and 1 KiB holds every real
// one; past it each is described by its size.
const MaxLogPersonBytes = 1 << 10

// MaxLogParents is the most parents a commit's answer lists: an octopus merge's
// 64 ids at 43 encoded bytes each. Past it, described by count
// (LogCommit.ParentCount).
const MaxLogParents = 64

// MaxBlameLines is the most lines one blame names. A blame's cost is the walk
// of the history of the lines it names, so the range is bounded rather than the
// file, within the read's one git budget: a range that runs it out is refused
// naming a narrower one.
const MaxBlameLines = 500

// MaxBlameLineBytes is the most a blamed line's text may encode to and be
// answered: twice a subject's bound, since generated or minified lines run
// long; past it, described by size (BlameLine.TextBytes).
const MaxBlameLineBytes = 8 << 10

// GitReadKind is what a [GitQuery] asks. A closed set ([GitReadKinds]).
type GitReadKind string

const (
	// GitTree lists a tree: a commit's root or one of its directories, or a
	// tree named by its own id; recursively when asked.
	GitTree GitReadKind = "tree"
	// GitBlob reads a file's lines, by a commit and path or by the blob's id; a
	// path naming a directory answers its listing (Kind tree), so one read
	// serves both.
	GitBlob GitReadKind = "blob"
	// GitDiff is the per-file stat of the difference between two commits
	// or trees, or one file's patch.
	GitDiff GitReadKind = "diff"
	// GitCompare is how two commits relate: their merge base, which contains
	// which, the commits each lacks of the other, and the first page of what
	// head changed since the base.
	GitCompare GitReadKind = "compare"
	// GitTips is many branch heads and tags at once: what each peels to, an
	// annotated tag's facts, its commit, and its distance from a base
	// (tips.go).
	GitTips GitReadKind = "tips"
	// GitTags is the copy's tags newest first by their own date, read from its
	// tag index without git (tags.go).
	GitTags GitReadKind = "tags"
	// GitLandedIn is whether a commit is in a ref's history, and the push that
	// put and kept it there: the one history question a seat asks the server
	// rather than a clone.
	GitLandedIn GitReadKind = "landed_in"
	// GitRevisionFacts is a change's revision against its target's tip: their
	// merge base, the change's patch-id and touched files, and whether its head
	// is in the target already, which the home records for each revision or
	// lands it by. THE ENGINE'S OWN READ ([GitReadKind.EnginesOwn]): every later
	// reader reads the rows, never git.
	GitRevisionFacts GitReadKind = "revision_facts"
	// GitReanchor is where lines anchored in one revision's files (a finding's,
	// a comment's) are in another's: moved by changed lines above them,
	// outdated where their own lines changed, gone with their file.
	GitReanchor GitReadKind = "reanchor"
	// GitInterdiff is what a change changed between two of its revisions: the
	// diff of their heads kept to the files either revision's change touches,
	// answered as a diff is (Kind interdiff, its Diff part).
	GitInterdiff GitReadKind = "interdiff"
	// GitLog is the history of a path, or of every path, from a commit: the
	// commits that changed it, newest first by generation, a page at a time,
	// optionally only those whose message holds a literal.
	GitLog GitReadKind = "log"
	// GitBlame is which commit last changed each line of a file at a commit,
	// and the line (and path, where another) it had there: at most
	// MaxBlameLines lines a read, a page at a time.
	GitBlame GitReadKind = "blame"
	// GitTreeCommits is the commit that last changed each entry of a directory
	// at a commit, as `git log -1 -- <entry>` names it, a page at a time
	// (treecommits.go).
	GitTreeCommits GitReadKind = "tree_commits"
	// GitChecksFile is the checks file a commit holds ([ChecksFilePath]): its
	// object's id and type and, within [MaxChecksFileBytes], its bytes WHOLE,
	// which a home reads a change's check definitions from at its target's tip.
	// THE ENGINE'S OWN READ ([GitReadKind.EnginesOwn]), always one answer: a
	// file too large to parse is answered by its size, and the bytes are exact,
	// never paged lines or JSON text replacing an invalid byte, since the parse
	// must see them as git holds them.
	GitChecksFile GitReadKind = "checks_file"
	// GitCodeFiles is a commit's deploy and dependency files on the closed list
	// (internal/deployfiles), each by path, blob and size, or ONE of them
	// whole. THE ENGINE'S OWN READ ([GitReadKind.EnginesOwn]), made by the
	// atlas's scan of a default branch's tip; a path off the list is refused.
	GitCodeFiles GitReadKind = "code_files"
	// GitSearch is the lines of a commit a code search matches (the default
	// branch's tip through the code index where its node keeps one, git grep
	// otherwise), at most MaxSearchHits, in one answer with no next page
	// (search.go).
	GitSearch GitReadKind = "search"
	// GitFind is the files of a commit's tree whose path holds a query's
	// characters in order, case-folded, best first, with the match count
	// (find.go): read over the tree's memoized listing, so typing runs no git
	// after the first query.
	GitFind GitReadKind = "find"
	// GitExcerpts is the lines around ranges of lines, each read in a blob a
	// change's revision holds, marked as that revision's diff changes them
	// (excerpts.go).
	GitExcerpts GitReadKind = "excerpts"
)

// GitReadKinds is the closed set.
//
// A KIND IS A REQUEST'S, NEVER A RECORD'S: no code log record names one, so a
// new kind moves no RecordVersion and takes no versionedFields row.
func GitReadKinds() []GitReadKind {
	return []GitReadKind{GitTree, GitBlob, GitDiff, GitCompare, GitTips, GitTags, GitLandedIn,
		GitRevisionFacts, GitReanchor, GitInterdiff, GitLog, GitBlame, GitTreeCommits, GitChecksFile, GitCodeFiles,
		GitSearch, GitFind, GitExcerpts}
}

// Valid reports a kind this build answers.
func (k GitReadKind) Valid() bool { return slices.Contains(GitReadKinds(), k) }

// EnginesOwn reports a kind THE ENGINE ALONE reads: a revision's facts, a
// commit's deploy and dependency files and its checks file. Such a read names
// NO asker and no seat it reads for, is held to no grant and recorded as
// nobody's: an asker would be judged for a read it never made, and a
// `visibility: unit` repository's readers told the engine read it. As the
// rows' duty listings ([ReadQuery]'s `compaction_due`), for the same reason.
func (k GitReadKind) EnginesOwn() bool {
	return k == GitRevisionFacts || k == GitChecksFile || k == GitCodeFiles
}

// Unstampable reports a kind whose answer is a PAGE of content, which a read
// may ask cut with no stamp ([GitQuery.Unstamped]). A find is a function of the
// tree, query, limit and build, so a browser may keep one by whole ids for a
// typing session. Never a landed-in read, whose answer IS the node's index of
// pushes; nor a revision's facts, re-anchoring or checks file, which only the
// engine and a change's own reads ask, never keeping a page.
func (k GitReadKind) Unstampable() bool {
	switch k {
	case GitTree, GitBlob, GitDiff, GitCompare, GitTips, GitInterdiff, GitLog, GitBlame, GitTreeCommits,
		GitFind:
		return true
	}
	return false
}

// EnginesOwn reports a read THE ENGINE makes on its own account: a kind only it
// reads ([GitReadKind.EnginesOwn]), or one FOR A CHECK ([GitQuery.Check]): the
// Gitlet node's one test of both.
func (q GitQuery) EnginesOwn() bool { return q.Kind.EnginesOwn() || q.Check != nil }

// SeatWork is the unit of the answering node's seat git-work share q's git
// takes ([GitShare.Take]).
func (q GitQuery) SeatWork() SeatWork {
	if q.EnginesOwn() {
		return SeatWorkEngine
	}
	return SeatWorkRead
}

// ReadsIndex reports a read whose page depends on the node's index of pushes,
// not the ids alone ([LogQuery.PushedBy]): never Unstamped, memoized or
// answered Immutable.
func (q GitQuery) ReadsIndex() bool { return q.Kind == GitLog && q.Log != nil && q.Log.PushedBy != "" }

// GitQuery is one read of a repository by object id: exactly the sub-query of
// its Kind is set.
type GitQuery struct {
	Kind GitReadKind `json:"kind"`
	// RepoID is the repository's canonical id.
	RepoID string `json:"repo_id"`
	// Asker is who reads, REQUIRED ([ErrInvalid] when zero or invalid), held to
	// the read grant by the answering Gitlet node from its own epoch's chart
	// ([GrantOf]): a seat by its relation to the repository, a person or token
	// by ADR-0085's rules. Trusted for nothing but the grant. REFUSED on the
	// engine's own read ([GitQuery.EnginesOwn]).
	Asker Principal `json:"asker"`
	// Check is the check attempt a read is made FOR: a check's box reading its
	// commit through the API (/box/gitlet/), admitted by the attempt's row and
	// token (internal/gitlet/host's AdmitCheck). THE ENGINE'S OWN READ
	// ([GitQuery.EnginesOwn]): a box is no principal and the row is its
	// authority. It reads THE CHECK'S COMMIT ALONE (a tree or file at it, or a
	// diff whose head is it), which the node enforces in validation whatever the
	// asker set; a diff's base is the row's, set by the admission.
	Check *CheckKey `json:"check,omitempty"`
	// CeilingSeat is the seat the asking turn acts for, whose grant the Gitlet
	// node judges beside the asker's from the same chart and its copy's row, as
	// [ReadQuery.CeilingSeat] says.
	CeilingSeat string `json:"ceiling_seat,omitempty"`
	// Turn is the turn a SEAT'S read is made in (turnctx.Turn.RunID): the
	// principal its one second look at the log's head is charged to, on its
	// seat's bucket of the answering node ([NewResyncs]); "" outside a turn.
	// Set only beside a seat's asker, and trusted only for whose bucket pays.
	Turn string `json:"turn,omitempty"`
	// Fresh is the asker's code log floor, where it holds one: the copy is
	// brought to at least it before anything is read. With none the copy is
	// read as it stands (an id names one object at every position), and an id
	// the copy lacks is looked for once more at the log's head before
	// [ErrObjectNotFound]. A floor on another log is [ErrInvalid].
	Fresh statelog.Freshness `json:"fresh"`
	// Head is the code log's head AS THE ASKER READ IT after asking (its own
	// linearizable read of the rows), where it holds one: the read's ONE SECOND
	// LOOK at the head (a refusal judged again on a maybe-behind copy, a ref or
	// object the copy lacks) brings the copy to it at session instead of its
	// own barrier. A many-repository search then pays one barrier, not one per
	// repository. Every write the asker could have seen is at or before it, so
	// the look stays linearizable for the asker; a seat's look is still charged
	// to its turn. Zero takes its own barrier; another log's position is
	// [ErrInvalid].
	Head statelog.Position `json:"head,omitzero"`
	// Cursor is a page's Next, handed back whole; empty for the first page.
	// Opaque; [ErrBadCursor] where it is not one the kind issues.
	Cursor string `json:"cursor,omitempty"`
	// Home is set on the walk's attempt of the ONE node it lets FILL a copy it
	// lacks (ADR-0089): its EFFECTIVE HOME, the first node of the repository's
	// order after the walk's demotions, or, only where that node answered no
	// serving copy is coming ([UnreadyReason.PassesFill]), the next one; never
	// two. A node holding no copy answers a read without it
	// [ErrGitUnready]{[UnreadyNotHeld]} at once, because a cold round outlives
	// its read's wait: with every node free to fill, one read of an unheld
	// repository began a whole download on every node the walk reached. Stamped
	// by the walk, so no caller chooses it. A build not knowing `home` reads
	// every read as one it may fill.
	Home bool `json:"home,omitempty"`
	// Unstamped asks for THE PAGE ALONE (Kind, Rev, Next and the kind's part)
	// without the node's stamps (the repository, the copy's position, Memo,
	// each commit's introducing push and the index's coverage, [GitAnswer]),
	// CUT AGAINST A ROOM NONE OF THEM TAKES. A stamped page varies with the
	// node's index and position; an unstamped page depends only on its ids,
	// arguments and the build, so a question may answer it as never changing
	// (internal/api/queries' Immutable). A node memoizes the two apart.
	Unstamped bool `json:"unstamped,omitempty"`

	Tree          *TreeQuery          `json:"tree,omitempty"`
	Blob          *BlobQuery          `json:"blob,omitempty"`
	Diff          *DiffQuery          `json:"diff,omitempty"`
	Compare       *CompareQuery       `json:"compare,omitempty"`
	Tips          *TipsQuery          `json:"tips,omitempty"`
	Tags          *TagsQuery          `json:"tags,omitempty"`
	LandedIn      *LandedInQuery      `json:"landed_in,omitempty"`
	RevisionFacts *RevisionFactsQuery `json:"revision_facts,omitempty"`
	Reanchor      *ReanchorQuery      `json:"reanchor,omitempty"`
	Interdiff     *InterdiffQuery     `json:"interdiff,omitempty"`
	Log           *LogQuery           `json:"log,omitempty"`
	Blame         *BlameQuery         `json:"blame,omitempty"`
	TreeCommits   *TreeCommitsQuery   `json:"tree_commits,omitempty"`
	ChecksFile    *ChecksFileQuery    `json:"checks_file,omitempty"`
	CodeFiles     *CodeFilesQuery     `json:"code_files,omitempty"`
	Search        *SearchQuery        `json:"search,omitempty"`
	Find          *FindQuery          `json:"find,omitempty"`
	Excerpts      *ExcerptsQuery      `json:"excerpts,omitempty"`
}

// CodeFilesQuery reads a commit's deploy and dependency files ([GitCodeFiles]).
type CodeFilesQuery struct {
	// Commit is the whole id read at: a default branch's tip, as the scan read
	// it from the rows.
	Commit string `json:"commit"`
	// File is the one file to read whole, by its root path; empty lists the
	// commit's files. A path the closed list does not admit as a read file
	// ([deployfiles.Format.Parsed]) is refused.
	File string `json:"file,omitempty"`
}

func (q *CodeFilesQuery) validate() error {
	if err := wholeID("code_files.commit", q.Commit, false); err != nil {
		return err
	}
	if q.File == "" {
		return nil
	}
	if _, err := GitPath("code_files.file", q.File, "", false); err != nil {
		return err
	}
	if f, ok := deployfiles.Classify(q.File); !ok || !f.Parsed() {
		return refusedf("%w: code_files.file: not a deploy or dependency file the engine reads: a file off "+
			"the closed list is never read", ErrInvalid)
	}
	return nil
}

// ChecksFileQuery reads the checks file one commit holds ([GitChecksFile]). A
// build predating it refuses the read (ErrInvalid) and its asker asks the next
// node of the order.
type ChecksFileQuery struct {
	// Commit is the whole id read at: a change's target's tip, as read from the
	// rows.
	Commit string `json:"commit"`
}

// TreeQuery lists one tree.
type TreeQuery struct {
	// Rev is a commit's id, a tag's or a tree's, whole or abbreviated.
	Rev string `json:"rev"`
	// Path is the directory under Rev's root tree to list; empty lists the
	// root. PathB64 is the same path's exact bytes in standard base64, for a
	// path that is not valid UTF-8, which JSON cannot carry; at most one is
	// set.
	Path    string `json:"path,omitempty"`
	PathB64 string `json:"path_b64,omitempty"`
	// Recursive lists every file below the directory rather than its
	// entries.
	Recursive bool `json:"recursive,omitempty"`
}

// BlobQuery reads one file's lines.
type BlobQuery struct {
	// Rev is a commit's (or tag's) id with Path naming the file, or a blob's
	// own id with no Path; whole or abbreviated. A commit with no Path reads
	// its root, as a listing.
	Rev     string `json:"rev"`
	Path    string `json:"path,omitempty"`
	PathB64 string `json:"path_b64,omitempty"`
	// StartLine and EndLine are the lines to read, 1-based and inclusive; zero
	// is the first or last. A page ends where the answer is full, and its Next
	// resumes inside the range.
	StartLine int `json:"start_line,omitempty"`
	EndLine   int `json:"end_line,omitempty"`
}

// DiffQuery is the difference between two commits or trees.
type DiffQuery struct {
	// Base and Head are commit, tag or tree ids, whole or abbreviated.
	Base string `json:"base"`
	Head string `json:"head"`
	// Paths filters the stat to files equal to or under one of them, judged by
	// the Gitlet node over git's whole answer (never handed to git), at most
	// MaxDiffPaths.
	Paths []string `json:"paths,omitempty"`
	// File asks for one file's patch instead of the stat: its path at Head, or
	// at Base for a file Head deleted. FileB64 is its exact bytes; at most one
	// is set, and neither beside Paths.
	File    string `json:"file,omitempty"`
	FileB64 string `json:"file_b64,omitempty"`
	// Renames detects a file moved between Base and Head as one rename rather
	// than a delete and an add.
	Renames bool `json:"renames,omitempty"`
	// Patches asks for every file's patch instead of the stat, a page at a time
	// in the stat's order ([DiffAnswer.Patches]); refused beside File, kept to
	// Paths where named.
	Patches bool `json:"patches,omitempty"`
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
// position it read it at, so the node judges that tip with no barrier of its
// own, and an answer names what it was judged against.
type LandedInQuery struct {
	// Commit is the commit asked about, whole or abbreviated.
	Commit string `json:"commit"`
	// Target is the whole ref asked about (refs/heads/main).
	Target string `json:"target"`
	// TargetOID is the whole id Target named when the asker resolved it, and At
	// the position of the record that moved Target there ([ResolvedRef.Moved]):
	// the copy is brought to At before judging, so every move of Target up to
	// that tip is in the index it searches.
	TargetOID string            `json:"target_oid"`
	At        statelog.Position `json:"at"`
}

// THE READS OF A CHANGE'S REVISIONS name every revision by the WHOLE ids its
// rows recorded, never an abbreviation: an interdiff compares two bases as ids
// without reading either.

// RevisionFactsQuery asks what a change's revision is against its target:
// the facts of the commit Head against the commit Target.
type RevisionFactsQuery struct {
	// Head is the revision's head commit and Target the change's target tip as
	// the asker read it, both whole ids. The facts are a function of the two
	// commits alone.
	Head   string `json:"head"`
	Target string `json:"target"`
}

// ReanchorQuery asks where ranges of lines, each read in a blob a revision
// held, are in another revision of the same change: an anchor is placed in the
// file at its path in Head where it is on the diff's new side, in Base where on
// the old.
type ReanchorQuery struct {
	// Base and Head are the revision the anchors are placed in: its base (the
	// merge base its facts record) and head commit, whole. Base is required only
	// to place an anchor on the old side.
	Base string `json:"base,omitempty"`
	Head string `json:"head"`
	// Anchors are the ranges to place, 1 to MaxReanchors of them, each id
	// once.
	Anchors []LineAnchor `json:"anchors"`
}

// Side is which side of a change's diff a line is on. A closed set
// ([Sides]).
type Side string

const (
	// SideOld is the base's file: the lines the change deletes and those it
	// keeps.
	SideOld Side = "old"
	// SideNew is the head's file: the lines the change adds, and those it
	// keeps.
	SideNew Side = "new"
)

// Sides is the closed set.
func Sides() []Side { return []Side{SideOld, SideNew} }

// Valid reports a side this build knows.
func (s Side) Valid() bool { return slices.Contains(Sides(), s) }

// LineAnchor is a range of lines read in one blob: what a finding or a
// comment was written against.
type LineAnchor struct {
	// ID names the anchor in the answer: the caller's own, at most 64 bytes
	// of UTF-8.
	ID string `json:"id"`
	// Path is the file's path in the revision the anchor is placed in (PathB64
	// its exact bytes, as [GitPath] reads them), and Side which of its two
	// commits holds it.
	Path    string `json:"path,omitempty"`
	PathB64 string `json:"path_b64,omitempty"`
	Side    Side   `json:"side"`
	// BlobOID is the whole blob the lines were read in; Start and End the
	// range, 1-based and inclusive, ending at MaxAnchorLine at most.
	BlobOID string `json:"blob_oid"`
	Start   int    `json:"start"`
	End     int    `json:"end"`
}

// InterdiffQuery asks what a change changed between two of its revisions: the
// diff of their heads kept to the files either revision's change touches, so
// files a rebase brought in from the target stay out, unless either revision
// touches all, when it is the whole diff.
type InterdiffQuery struct {
	// FromBase and FromHead are the earlier revision's base and head as its
	// facts record them, ToBase and ToHead the later one's: whole ids, a base
	// empty where its facts name no merge base. The bases are compared, never
	// read: two revisions from one base differ only in files one touches.
	FromBase string `json:"from_base,omitempty"`
	FromHead string `json:"from_head"`
	ToBase   string `json:"to_base,omitempty"`
	ToHead   string `json:"to_head"`
	// FromTouched and ToTouched are each revision's touched paths as its facts
	// list them; FromTouchesAll and ToTouchesAll whether it touches all, with
	// no list.
	FromTouched    []string `json:"from_touched,omitempty"`
	FromTouchesAll bool     `json:"from_touches_all,omitempty"`
	ToTouched      []string `json:"to_touched,omitempty"`
	ToTouchesAll   bool     `json:"to_touches_all,omitempty"`
	// File asks for one file's patch instead of the stat: its path at ToHead,
	// or at FromHead for a file ToHead deleted (FileB64 its exact bytes), as
	// [DiffQuery.File] is.
	File    string `json:"file,omitempty"`
	FileB64 string `json:"file_b64,omitempty"`
	// Patches asks for every file's patch of the interdiff, as [DiffQuery.Patches] does.
	Patches bool `json:"patches,omitempty"`
}

// LogQuery is the history of a path from a commit, or of every path.
type LogQuery struct {
	// Rev is the commit the history is read from (or a tag naming one),
	// whole or abbreviated.
	Rev string `json:"rev"`
	// Path is the file or directory whose history is read, from the root; none
	// reads every commit. PathB64 is its exact bytes in standard base64, for a
	// path not valid UTF-8; at most one is set. A path the commit lacks still
	// has a history: a deleted file was changed by the commits before its
	// deletion.
	Path    string `json:"path,omitempty"`
	PathB64 string `json:"path_b64,omitempty"`
	// Grep keeps the commits whose whole message holds it: a LITERAL, matched
	// case-folded in the Gitlet node's own code as each message streams by,
	// never handed to git: a regular expression is an unbounded runtime, and a
	// read by oid carries no text on a command line. Valid UTF-8, no NUL, at
	// most MaxLogGrepBytes.
	Grep string `json:"grep,omitempty"`
	// Since keeps commits committed at or after it, as git's --since does: the
	// walk stops at an older commit on each line of history, so a newer commit
	// whose every line runs through an older one is not listed. Zero keeps
	// every one.
	Since time.Time `json:"since,omitzero"`
	// Until keeps commits committed BEFORE it: a filter of each commit reached,
	// not a stop as Since is. Zero keeps every one; otherwise after the epoch,
	// after Since, and no further ahead than Since may be.
	Until time.Time `json:"until,omitzero"`
	// PushedBy, a seat's handle, keeps the commits whose introducing push in
	// the node's index holds that seat ([Principal.Seat]); a commit the index
	// cannot judge counts in [LogAnswer.Unjudged].
	PushedBy string `json:"pushed_by,omitempty"`
	// Limit is the most commits a page lists, 1 to MaxLogLimit; zero is
	// DefaultLogLimit. A page past the answer's bytes holds fewer.
	Limit int `json:"limit,omitempty"`
}

// BlameQuery is which commit last changed each line of a range of a file.
type BlameQuery struct {
	// Rev is the commit the file is read at (or a tag naming one), whole or
	// abbreviated, and Path the file in it (PathB64 its exact bytes, as a
	// TreeQuery's): REQUIRED, and a file there.
	Rev     string `json:"rev"`
	Path    string `json:"path,omitempty"`
	PathB64 string `json:"path_b64,omitempty"`
	// StartLine and EndLine are the range, 1-based and inclusive, at most
	// MaxBlameLines lines: zero is the file's first line, and MaxBlameLines − 1
	// past StartLine or the file's last, whichever is sooner. A page ends where
	// the answer is full, and its Next resumes inside the range.
	StartLine int `json:"start_line,omitempty"`
	EndLine   int `json:"end_line,omitempty"`
}

// Range is the blame's range as git is asked it: the default end MaxBlameLines
// − 1 past the first, or the file's last line where sooner, which git clamps it
// to.
func (q BlameQuery) Range() (start, end int) {
	start = max(q.StartLine, 1)
	end = q.EndLine
	if end == 0 {
		// MaxBlameLines − 1 past the first, capped at MaxInt: a range that far
		// names no line, so git's end of file refuses it, never an overflow.
		end = start + min(MaxBlameLines-1, math.MaxInt-start)
	}
	return start, end
}

// ValidRev reports whether rev names an object as a read by oid takes it: a
// whole object id (40 lowercase hex digits, not the zero id), or an
// ABBREVIATION of MinAbbrevDigits to 39 lowercase hex digits. Never a ref,
// `^`, `~` or `:`, and never shorter than git's default, which would name
// dozens of objects in a large repository.
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

// GitPath is the path a read names, from its Path or its PathB64 (the exact
// bytes, decoded), or [ErrInvalid] naming field: both set, base64 that does not
// decode, or a path no tree entry can have ([validGitPath]). root admits the
// empty path, a tree's root.
func GitPath(field, p, b64 string, root bool) (string, error) {
	if b64 != "" {
		if p != "" {
			return "", refusedf("%w: %s: give the path as %s or as %s_b64, not both", ErrInvalid, field,
				field, field)
		}
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return "", fmt.Errorf("%w: %s_b64: not standard base64: %w", ErrInvalid, field, err)
		}
		if len(raw) == 0 {
			return "", fmt.Errorf("%w: %s_b64: an empty path is given by leaving the path out", ErrInvalid, field)
		}
		p = string(raw)
	}
	if why := validGitPath(p, root); why != "" {
		return "", refusedf("%w: %s: %s", ErrInvalid, field, why)
	}
	return p, nil
}

// validGitPath is why p is no path a read by oid may name, or "": relative and
// clean (path.Clean's form: no leading or trailing slash, no empty, `.` or `..`
// component), at most MaxGitPathBytes, and no NUL, which would end git's input
// record. The clean form also keeps git's object-name grammar out:
// `<commit>:<path>` reads a `./` or `../` path as relative to a work tree,
// which a bare repository lacks, and git dies over the whole batch.
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

// WirePath is p as an answer carries it: itself where valid UTF-8, otherwise
// with every invalid byte U+FFFD beside its exact bytes in standard base64,
// which a later read names it back by (a PathB64).
func WirePath(p string) (text, b64 string) {
	if utf8.ValidString(p) {
		return p, ""
	}
	return strings.ToValidUTF8(p, string(utf8.RuneError)), base64.StdEncoding.EncodeToString([]byte(p))
}

// Validate refuses, [ErrInvalid] naming the field, a read the Gitlet node
// cannot answer as asked: a malformed kind, repository id, asker, turn, or
// sub-query (other than its kind's, or none); a revision, tip, base, path,
// line range, log literal, limit or date its kind refuses; a diff naming
// both a file and paths; a check's read that names an asker or reads beyond its
// check's commit. A cursor longer than any a node issues is [ErrBadCursor];
// whether a shorter one is the kind's own is the node's to judge.
func (q GitQuery) Validate() error {
	if !q.Kind.Valid() {
		return refusedf("%w: kind: %q is not one of %s", ErrInvalid, q.Kind, words.Or(GitReadKinds()))
	}
	if id, err := uuid.Parse(q.RepoID); err != nil || id.String() != q.RepoID {
		return refusedf("%w: repo_id: %q is not a repository's id", ErrInvalid, q.RepoID)
	}
	// WHO ASKS IS NAMED, or nothing is read: the answer is held to the asker's
	// grant. The engine's own read names nobody.
	switch {
	case q.Check != nil:
		if q.Asker != (Principal{}) || q.CeilingSeat != "" {
			return refusedf("%w: asker: a read made for a check is the engine's own, on the check's "+
				"behalf — a check's box is no principal — which no principal makes", ErrInvalid)
		}
		if err := q.Check.validateAs("check."); err != nil {
			return err
		}
	case q.Kind.EnginesOwn():
		if q.Asker != (Principal{}) || q.CeilingSeat != "" {
			return refusedf("%w: asker: a %s read is the engine's own — its repository's home Gitlet node's — "+
				"which no principal makes", ErrInvalid, q.Kind)
		}
	default:
		if why := q.Asker.invalid(); why != "" {
			return refusedf("%w: asker: %s", ErrInvalid, why)
		}
		if why := ceilingInvalid(q.Asker, q.CeilingSeat); why != "" {
			return fmt.Errorf("%w: %s", ErrInvalid, why)
		}
	}
	if why := turnInvalid(q.Asker, q.Turn); why != "" {
		return refusedf("%w: turn: %s", ErrInvalid, why)
	}
	if len(q.Cursor) > maxGitCursorBytes {
		return fmt.Errorf("%w: cursor: %d bytes is no cursor a git read issues", ErrBadCursor, len(q.Cursor))
	}
	// THE FLOOR IS ON THE CODE LOG, the one log a copy follows.
	if err := q.Fresh.OnlyOn(topics.CodeLogStream); err != nil {
		return fmt.Errorf("%w: fresh: %w", ErrInvalid, err)
	}
	if err := q.Head.On(topics.CodeLogStream); err != nil {
		return fmt.Errorf("%w: head: %w", ErrInvalid, err)
	}
	for _, s := range []struct {
		kind    GitReadKind
		present bool
	}{{GitTree, q.Tree != nil}, {GitBlob, q.Blob != nil}, {GitDiff, q.Diff != nil},
		{GitCompare, q.Compare != nil}, {GitTips, q.Tips != nil}, {GitTags, q.Tags != nil},
		{GitLandedIn, q.LandedIn != nil}, {GitRevisionFacts, q.RevisionFacts != nil}, {GitReanchor, q.Reanchor != nil},
		{GitInterdiff, q.Interdiff != nil}, {GitLog, q.Log != nil}, {GitBlame, q.Blame != nil},
		{GitTreeCommits, q.TreeCommits != nil},
		{GitChecksFile, q.ChecksFile != nil}, {GitCodeFiles, q.CodeFiles != nil},
		{GitSearch, q.Search != nil}, {GitFind, q.Find != nil}, {GitExcerpts, q.Excerpts != nil}} {
		switch {
		case s.kind == q.Kind && !s.present:
			return refusedf("%w: %s: a %s read names what it reads", ErrInvalid, s.kind, s.kind)
		case s.kind != q.Kind && s.present:
			return refusedf("%w: %s: a %s read carries no %s query", ErrInvalid, s.kind, q.Kind, s.kind)
		}
	}
	if q.Check != nil {
		if err := q.atItsCheck(); err != nil {
			return err
		}
	}
	if q.Unstamped && !q.Kind.Unstampable() {
		return refusedf("%w: unstamped: a %s read answers no page to cut unstamped", ErrInvalid, q.Kind)
	}
	if q.Unstamped && q.ReadsIndex() {
		return refusedf("%w: unstamped: a history narrowed by who pushed it reads this node's index of "+
			"pushes, which an unstamped page names nothing of", ErrInvalid)
	}
	switch q.Kind {
	case GitTree:
		return q.Tree.validate()
	case GitBlob:
		return q.Blob.validate()
	case GitDiff:
		return q.Diff.validate()
	case GitTips:
		return q.Tips.validate()
	case GitTags:
		return q.Tags.validate()
	case GitLandedIn:
		return q.LandedIn.validate()
	case GitRevisionFacts:
		return q.RevisionFacts.validate()
	case GitReanchor:
		return q.Reanchor.validate()
	case GitInterdiff:
		return q.Interdiff.validate()
	case GitLog:
		return q.Log.validate()
	case GitBlame:
		return q.Blame.validate()
	case GitTreeCommits:
		return q.TreeCommits.validate()
	case GitChecksFile:
		return wholeID("checks_file.commit", q.ChecksFile.Commit, false)
	case GitCodeFiles:
		return q.CodeFiles.validate()
	case GitSearch:
		return q.Search.validate()
	case GitFind:
		return q.Find.validate()
	case GitExcerpts:
		return q.Excerpts.validate()
	}
	return q.Compare.validate()
}

// atItsCheck refuses a check's read ([GitQuery.Check]) of anything but the
// check's commit (a tree or file at it, or a diff whose head is it) by its
// whole id: the one commit its box's token admits, enforced by the Gitlet node
// whatever the asking node built.
func (q GitQuery) atItsCheck() error {
	var field, rev string
	switch q.Kind {
	case GitTree:
		field, rev = "tree.rev", q.Tree.Rev
	case GitBlob:
		field, rev = "blob.rev", q.Blob.Rev
	case GitDiff:
		field, rev = "diff.head", q.Diff.Head
	default:
		return refusedf("%w: kind: a read made for a check is a tree, a file or a diff of its commit, "+
			"never a %s read", ErrInvalid, q.Kind)
	}
	if rev != q.Check.OID {
		return refusedf("%w: %s: %q is not the check's commit %s: a read made for a check reads that "+
			"commit alone", ErrInvalid, field, rev, q.Check.OID)
	}
	return nil
}

// ceilingInvalid is why a read may not carry seat as its ceiling beside asker
// ([ReadQuery.CeilingSeat]), or "" where it may: none, or a seat's handle
// beside a seat's asker. Every principal's query states its ceiling by it.
func ceilingInvalid(asker Principal, seat string) string {
	switch {
	case seat == "":
		return ""
	case asker.Kind != PrincipalAgent:
		return "ceiling_seat: only a seat's read is made for another seat — a person's or a token's " +
			"read is its own, which no ceiling bounds"
	case !org.ValidHandle(seat):
		return fmt.Sprintf("ceiling_seat: %q is not a seat's handle", seat)
	}
	return ""
}

// turnInvalid is why turn is no turn asker's read may name, or "": a turn on a
// non-seat asker (a person's or token's read is charged to no turn), or one
// past MaxProvenanceBytes, not UTF-8, or holding a control character: a push's
// turn's bounds.
func turnInvalid(asker Principal, turn string) string {
	switch {
	case turn == "":
		return ""
	case asker.Kind != PrincipalAgent:
		return "only a seat's read is made in a turn whose re-syncs it pays for — a person's or a token's is " +
			"its own"
	case len(turn) > MaxProvenanceBytes:
		return fmt.Sprintf("%d bytes, past the %d a turn id may hold (MaxProvenanceBytes)", len(turn),
			MaxProvenanceBytes)
	case !utf8.ValidString(turn):
		return "not valid UTF-8"
	case strings.ContainsFunc(turn, unicode.IsControl):
		return "holds a control character"
	}
	return ""
}

// wholeID refuses, naming field, anything but a whole object id as a read of a
// change's revisions takes it (no abbreviation, not the zero id); empty passes
// where empty says.
func wholeID(field, id string, empty bool) error {
	switch {
	case id == "" && empty:
		return nil
	case !gitref.ValidOID(id) || id == ZeroOID:
		return refusedf("%w: %s: %q is not a whole object id: a change's revisions are read by the 40-digit "+
			"ids their rows record", ErrInvalid, field, id)
	}
	return nil
}

// validRev is ValidRev as a refusal naming field.
func validRev(field, rev string) error {
	if !ValidRev(rev) {
		return refusedf("%w: %s: %q is not an object id — %d to 40 lowercase hex digits, never a ref "+
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
		return refusedf("%w: blob.start_line: %d; lines count from 1", ErrInvalid, q.StartLine)
	case q.EndLine < 0:
		return refusedf("%w: blob.end_line: %d; lines count from 1", ErrInvalid, q.EndLine)
	case q.EndLine > 0 && q.EndLine < max(q.StartLine, 1):
		return refusedf("%w: blob.end_line: %d is before start_line %d", ErrInvalid, q.EndLine,
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
		return refusedf("%w: diff.paths: %d paths, past the %d one diff is filtered by", ErrInvalid,
			len(q.Paths), MaxDiffPaths)
	}
	for i, p := range q.Paths {
		if why := validGitPath(p, false); why != "" {
			return fmt.Errorf("%w: diff.paths[%d]: %s", ErrInvalid, i, why)
		}
	}
	if q.Patches && (q.File != "" || q.FileB64 != "") {
		return refusedf("%w: diff.patches: every file's patch names no one file: give file or patches",
			ErrInvalid)
	}
	if q.File == "" && q.FileB64 == "" {
		return nil
	}
	if len(q.Paths) > 0 {
		return refusedf("%w: diff.file: a patch names its one file and paths filter a stat — give one "+
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
		return refusedf("%w: landed_in.target: %q is not a whole ref (refs/heads/<branch>, refs/tags/<tag>): a "+
			"branch's short name is resolved to its ref first", ErrInvalid, q.Target)
	case !gitref.ValidOID(q.TargetOID) || q.TargetOID == ZeroOID:
		return refusedf("%w: landed_in.target_oid: %q is not the whole id of the tip the target was resolved to",
			ErrInvalid, q.TargetOID)
	case q.At.IsZero() || q.At.Stream != topics.CodeLogStream:
		return refusedf("%w: landed_in.at: %s is not the code log position the target last moved at",
			ErrInvalid, q.At)
	}
	return nil
}

func (q RevisionFactsQuery) validate() error {
	if err := wholeID("revision_facts.head", q.Head, false); err != nil {
		return err
	}
	return wholeID("revision_facts.target", q.Target, false)
}

func (q ReanchorQuery) validate() error {
	if err := wholeID("reanchor.base", q.Base, true); err != nil {
		return err
	}
	if err := wholeID("reanchor.head", q.Head, false); err != nil {
		return err
	}
	if n := len(q.Anchors); n == 0 || n > MaxReanchors {
		return refusedf("%w: reanchor.anchors: %d anchors; a read places 1 to %d", ErrInvalid, n, MaxReanchors)
	}
	ids := make(map[string]bool, len(q.Anchors))
	for i, a := range q.Anchors {
		field := fmt.Sprintf("reanchor.anchors[%d]", i)
		switch {
		case a.ID == "" || len(a.ID) > maxAnchorIDBytes || !utf8.ValidString(a.ID):
			return fmt.Errorf("%w: %s.id: an anchor is named by 1 to %d bytes of UTF-8, which its answer carries "+
				"back", ErrInvalid, field, maxAnchorIDBytes)
		case ids[a.ID]:
			return fmt.Errorf("%w: %s.id: %q names two anchors, whose answers would read as one", ErrInvalid,
				field, a.ID)
		case !a.Side.Valid():
			return fmt.Errorf("%w: %s.side: %q is not one of %s", ErrInvalid, field, a.Side, words.Or(Sides()))
		case a.Side == SideOld && q.Base == "":
			return fmt.Errorf("%w: %s.side: an anchor on the old side is placed in the revision's base, which "+
				"reanchor.base names", ErrInvalid, field)
		case a.Start < 1 || a.End < a.Start || a.End > MaxAnchorLine:
			return refusedf("%w: %s: lines %d to %d are no range: lines count from 1, end is at or after "+
				"start, and no file holds a line past %d", ErrInvalid, field, a.Start, a.End, MaxAnchorLine)
		}
		ids[a.ID] = true
		if _, err := GitPath(field+".path", a.Path, a.PathB64, false); err != nil {
			return err
		}
		if err := wholeID(field+".blob_oid", a.BlobOID, false); err != nil {
			return err
		}
	}
	return nil
}

func (q InterdiffQuery) validate() error {
	for _, id := range []struct {
		field, id string
		empty     bool
	}{{"interdiff.from_base", q.FromBase, true}, {"interdiff.from_head", q.FromHead, false},
		{"interdiff.to_base", q.ToBase, true}, {"interdiff.to_head", q.ToHead, false}} {
		if err := wholeID(id.field, id.id, id.empty); err != nil {
			return err
		}
	}
	for _, side := range []struct {
		field   string
		touched []string
		all     bool
	}{{"interdiff.from_touched", q.FromTouched, q.FromTouchesAll},
		{"interdiff.to_touched", q.ToTouched, q.ToTouchesAll}} {
		if err := validTouched(side.field, side.touched, side.all); err != nil {
			return err
		}
	}
	if q.Patches && (q.File != "" || q.FileB64 != "") {
		return refusedf("%w: interdiff.patches: every file's patch names no one file: give file or patches",
			ErrInvalid)
	}
	if q.File == "" && q.FileB64 == "" {
		return nil
	}
	_, err := GitPath("interdiff.file", q.File, q.FileB64, false)
	return err
}

// logSinceAhead is how far past this node's clock a log's Since may be: a day,
// the most two nodes' clocks may honestly differ by. Past it the date is a
// mistyped year, and the asker is told so rather than answered an empty history.
const logSinceAhead = 24 * time.Hour

func (q LogQuery) validate() error {
	if err := validRev("log.rev", q.Rev); err != nil {
		return err
	}
	if _, err := GitPath("log.path", q.Path, q.PathB64, true); err != nil {
		return err
	}
	switch {
	case len(q.Grep) > MaxLogGrepBytes:
		return refusedf("%w: log.grep: %d bytes, past the %d a log's literal may have", ErrInvalid,
			len(q.Grep), MaxLogGrepBytes)
	case !utf8.ValidString(q.Grep) || strings.IndexByte(q.Grep, 0) >= 0:
		return refusedf("%w: log.grep: a literal is valid UTF-8 holding no NUL", ErrInvalid)
	case q.Limit < 0 || q.Limit > MaxLogLimit:
		return refusedf("%w: log.limit: %d; a page lists 1 to %d commits, %d when left out", ErrInvalid,
			q.Limit, MaxLogLimit, DefaultLogLimit)
	case q.Since.IsZero():
	case q.Since.Before(time.Unix(0, 0)):
		return refusedf("%w: log.since: %s is before 1970, the first instant a commit's date can name",
			ErrInvalid, q.Since.UTC().Format(time.RFC3339))
	case q.Since.After(time.Now().Add(logSinceAhead)):
		return refusedf("%w: log.since: %s is not yet: give a date in the past", ErrInvalid,
			q.Since.UTC().Format(time.RFC3339))
	}
	// Separate from Since's switch, which ends at its first holding arm.
	switch {
	case q.Until.IsZero():
	case !q.Until.After(time.Unix(0, 0)):
		// At the epoch the second before it is -1, which git reads as no bound.
		return refusedf("%w: log.until: %s is not after 1970-01-01T00:00:00Z: a commit is kept only before an "+
			"instant after the epoch", ErrInvalid, q.Until.UTC().Format(time.RFC3339Nano))
	case q.Until.After(time.Now().Add(logSinceAhead)):
		return refusedf("%w: log.until: %s is not yet: give a date no further than a day ahead", ErrInvalid,
			q.Until.UTC().Format(time.RFC3339))
	case !q.Since.IsZero() && !q.Until.After(q.Since):
		return refusedf("%w: log.until: %s is not after since %s: the window holds no instant", ErrInvalid,
			q.Until.UTC().Format(time.RFC3339Nano), q.Since.UTC().Format(time.RFC3339Nano))
	}
	if q.PushedBy != "" && !org.ValidHandle(q.PushedBy) {
		return refusedf("%w: log.pushed_by: %q is not a seat's handle", ErrInvalid, q.PushedBy)
	}
	return nil
}

func (q BlameQuery) validate() error {
	if err := validRev("blame.rev", q.Rev); err != nil {
		return err
	}
	if _, err := GitPath("blame.path", q.Path, q.PathB64, false); err != nil {
		return err
	}
	start, end := q.Range()
	switch {
	case q.StartLine < 0:
		return refusedf("%w: blame.start_line: %d; lines count from 1", ErrInvalid, q.StartLine)
	case q.EndLine < 0:
		return refusedf("%w: blame.end_line: %d; lines count from 1", ErrInvalid, q.EndLine)
	case end < start:
		return refusedf("%w: blame.end_line: %d is before start_line %d", ErrInvalid, end, start)
	case end-start >= MaxBlameLines:
		return refusedf("%w: blame.end_line: lines %d to %d are %d lines; a blame names at most %d",
			ErrInvalid, start, end, end-start+1, MaxBlameLines)
	}
	return nil
}

// validTouched refuses a revision's touched paths as an interdiff's asker gives
// them back: a list beside touches-all, a list past either facts bound, or a
// path no tree entry can have.
func validTouched(field string, touched []string, all bool) error {
	switch cost := TouchedCost(touched); {
	case all && len(touched) > 0:
		return refusedf("%w: %s: a revision that touches all lists no paths", ErrInvalid, field)
	case len(touched) > MaxTouchedPaths:
		return refusedf("%w: %s: %d paths, past the %d a revision's facts list", ErrInvalid, field,
			len(touched), MaxTouchedPaths)
	case cost > MaxTouchedBytes:
		return refusedf("%w: %s: the list is %d bytes of JSON, past the %d a revision's facts list",
			ErrInvalid, field, cost, MaxTouchedBytes)
	}
	for i, p := range touched {
		if why := validGitPath(p, false); why != "" {
			return fmt.Errorf("%w: %s[%d]: %s", ErrInvalid, field, i, why)
		}
	}
	return nil
}

// TouchedCost is a touched list's JSON encoding length, as [MaxTouchedBytes]
// bounds it (an empty list its two brackets).
func TouchedCost(touched []string) int {
	if touched == nil {
		touched = []string{}
	}
	out, err := json.Marshal(touched)
	if err != nil {
		// A list of strings always encodes: reaching here is a broken encoder.
		panic(fmt.Sprintf("gitlet: encoding a touched list: %v", err))
	}
	return len(out)
}

// GitAnswer answers a [GitQuery]: exactly its Kind's field is set (a file read
// naming a directory answers Kind tree, an interdiff sets Diff, a find sets
// Find). Its JSON is at most MaxGitAnswerBytes.
type GitAnswer struct {
	Kind   GitReadKind `json:"kind"`
	RepoID string      `json:"repo_id"`
	// Rev is the whole id of the object a tree, file or find read was asked of
	// (a tag's is the commit or tree it names). A diff, comparison, interdiff
	// and revision's facts name their commits in their own answer.
	Rev string `json:"rev,omitempty"`
	// Position is the answering copy's: every row up to it is in what it read,
	// so an asker holding a later floor knows the answer may predate its push,
	// never that a named object is wrong.
	Position statelog.Position `json:"position"`
	// Memo is set on a page the node kept from an earlier cut rather than cut
	// again: the same page, since a page depends only on its ids and arguments,
	// answered with no git run.
	Memo bool `json:"memo,omitempty"`
	// Next is where the next page starts, handed back as the query's Cursor
	// with the query otherwise unchanged; empty on the last page.
	Next string `json:"next,omitempty"`
	// PushedBy is the push that INTRODUCED each commit the answer names, by
	// whole id, where the node's introduced index holds it. A commit it does not
	// name was introduced outside Coverage, or by a push listing more than
	// MaxIntroduced commits.
	PushedBy map[string]PushedBy `json:"pushed_by,omitempty"`
	// Coverage is what that index holds, on every answer of a node keeping one:
	// an absent PushedBy means nothing beyond it.
	Coverage *Coverage `json:"coverage,omitempty"`
	// Tagged is the tags naming each commit a history page names, from the
	// node's tag index; absent where it keeps none it can read, which says
	// nothing of any commit's tags.
	Tagged *TagMarks `json:"tagged,omitempty"`

	Tree          *TreeAnswer          `json:"tree,omitempty"`
	Blob          *BlobAnswer          `json:"blob,omitempty"`
	Diff          *DiffAnswer          `json:"diff,omitempty"`
	Compare       *CompareAnswer       `json:"compare,omitempty"`
	Tips          *TipsAnswer          `json:"tips,omitempty"`
	Tags          *TagsAnswer          `json:"tags,omitempty"`
	LandedIn      *LandedInAnswer      `json:"landed_in,omitempty"`
	RevisionFacts *RevisionFactsAnswer `json:"revision_facts,omitempty"`
	Reanchor      *ReanchorAnswer      `json:"reanchor,omitempty"`
	Log           *LogAnswer           `json:"log,omitempty"`
	Blame         *BlameAnswer         `json:"blame,omitempty"`
	TreeCommits   *TreeCommitsAnswer   `json:"tree_commits,omitempty"`
	ChecksFile    *ChecksFileAnswer    `json:"checks_file,omitempty"`
	CodeFiles     *CodeFilesAnswer     `json:"code_files,omitempty"`
	Search        *SearchAnswer        `json:"search,omitempty"`
	Find          *FindAnswer          `json:"find,omitempty"`
	Excerpts      *ExcerptsAnswer      `json:"excerpts,omitempty"`
}

// CodeFilesAnswer is a commit's deploy and dependency files: its LISTING, at
// most [deployfiles.MaxFiles] by path with the rest counted, or ONE file
// ([CodeFilesQuery.File]). The engine's, never held to [MaxGitAnswerBytes]: a
// listing at its bound is some 64 KiB of JSON and a file some 88 KiB, far
// inside the estate's reply ([MaxPageBytes]).
type CodeFilesAnswer struct {
	// Commit is the commit read at, its whole id.
	Commit string `json:"commit"`
	// Files is a listing's files in path order; More is how many past the bound
	// the commit holds, none read.
	Files []CodeFile `json:"files,omitempty"`
	More  int        `json:"more,omitempty"`
	// File is the one file asked for: absent (no OID) where the commit holds no
	// regular file there. Raw is its bytes whole where at most
	// [deployfiles.MaxFileBytes]; nil otherwise and for an empty file.
	File *CodeFile `json:"file,omitempty"`
	Raw  []byte    `json:"raw,omitempty"`
}

// CodeFile is one deploy or dependency file of a commit.
type CodeFile struct {
	Path string `json:"path"`
	OID  string `json:"oid,omitempty"`
	Size int64  `json:"size,omitempty"`
}

// ChecksFileAnswer is what a commit holds at [ChecksFilePath]: nothing (no
// OID), something that is no file (Type tree, or commit for a submodule), or a
// file: its id and size, and its bytes whole where at most
// [MaxChecksFileBytes]; past that none, its size being what [ParseChecks]
// refuses it by. At most some 88 KiB of base64 JSON, far inside the estate's
// reply ([MaxPageBytes]); never held to [MaxGitAnswerBytes], since no seat
// reads it.
type ChecksFileAnswer struct {
	// Commit is the commit read at, its whole id.
	Commit string `json:"commit"`
	// OID is the object at the path and Type its type (blob, tree or commit);
	// both empty where the commit holds nothing there.
	OID  string `json:"oid,omitempty"`
	Type string `json:"type,omitempty"`
	// Size is a blob's bytes.
	Size int64 `json:"size,omitempty"`
	// Raw is a blob's bytes whole where Size is at most MaxChecksFileBytes; nil
	// otherwise and for an empty file.
	Raw []byte `json:"raw,omitempty"`
}

// LogAnswer is one page of a history: the commits that changed the path (or
// any), newest first by generation (git's `log --topo-order`, every parent
// after every child), as many as the limit and bytes hold. Each one's
// introducing push is the answer's PushedBy.
type LogAnswer struct {
	// Path is the history's path, empty for a whole history (PathB64 per
	// [WirePath]).
	Path    string `json:"path,omitempty"`
	PathB64 string `json:"path_b64,omitempty"`
	// Commits is the page, in the walk's order.
	Commits []LogCommit `json:"commits"`
	// Scanned is how many commits of the history this page covers, those a Grep
	// dropped included, so the next page begins that many further on.
	Scanned int `json:"scanned"`
	// Complete is set where the walk reached the history's end: no next page. A
	// page the git budget ended is never complete (never a claim nothing more
	// matches); nor is a Grep's page holding its limit, which ends at its last
	// match, so the page after may list nothing and be the complete one.
	Complete bool `json:"complete,omitempty"`
	// Unjudged is, on a page asked with [LogQuery.PushedBy], how many commits
	// every other narrowing kept that the node's index could not attribute:
	// set, zero included, on every such page; nil otherwise.
	Unjudged *int `json:"unjudged,omitempty"`
}

// LogCommit is one commit of a history. Every field git leaves unbounded is
// described, never cut, past its bound (MaxLogSubjectBytes and its kin).
type LogCommit struct {
	OID string `json:"oid"`
	// Parents are the commit's parents' ids, none for a root; past
	// MaxLogParents they are absent and ParentCount is how many there are.
	Parents     []string `json:"parents,omitempty"`
	ParentCount int      `json:"parent_count,omitempty"`
	// Author and AuthorEmail are the author as the commit states them, never
	// `.mailmap`-renamed (which would make an answer depend on a file at some
	// branch), each past MaxLogPersonBytes absent with its size in AuthorBytes
	// or AuthorEmailBytes.
	Author           string `json:"author,omitempty"`
	AuthorBytes      int    `json:"author_bytes,omitempty"`
	AuthorEmail      string `json:"author_email,omitempty"`
	AuthorEmailBytes int    `json:"author_email_bytes,omitempty"`
	// Committer and CommitterEmail are the committer as the commit states them,
	// each past MaxLogPersonBytes absent with its size in CommitterBytes or
	// CommitterEmailBytes.
	Committer           string `json:"committer,omitempty"`
	CommitterBytes      int    `json:"committer_bytes,omitempty"`
	CommitterEmail      string `json:"committer_email,omitempty"`
	CommitterEmailBytes int    `json:"committer_email_bytes,omitempty"`
	// EngineCommitted says the committer spells the engine's convention
	// ([EngineCommitted]), which authenticates nothing. Nil from a build naming
	// no committer.
	EngineCommitted *bool `json:"engine_committed,omitempty"`
	// AuthoredAt and CommittedAt are the commit's two dates, to the second.
	AuthoredAt  time.Time `json:"authored_at"`
	CommittedAt time.Time `json:"committed_at"`
	// Subject is the message's first paragraph, joined as git's %s joins it;
	// past MaxLogSubjectBytes, absent with SubjectBytes its size.
	Subject      string `json:"subject,omitempty"`
	SubjectBytes int    `json:"subject_bytes,omitempty"`
}

// BlameAnswer is one page of a blame: each line of the page's part of the range
// with the commit that last changed it, and each such commit once. Each
// commit's introducing push is the answer's PushedBy.
type BlameAnswer struct {
	// Path is the file blamed (PathB64 per [WirePath]).
	Path    string `json:"path"`
	PathB64 string `json:"path_b64,omitempty"`
	// StartLine and EndLine are the first and last line THIS page attributes;
	// both zero for an empty file.
	StartLine int         `json:"start_line"`
	EndLine   int         `json:"end_line"`
	Lines     []BlameLine `json:"lines"`
	// Commits is every commit a line of the page names, by its whole id,
	// once.
	Commits map[string]BlameCommit `json:"commits"`
}

// BlameLine is one line of a blame.
type BlameLine struct {
	// Line is its number in the blamed file, and Commit the whole id of the
	// commit that last changed it.
	Line   int    `json:"line"`
	Commit string `json:"commit"`
	// OrigLine is its number in Commit's version of the file, and OrigPath that
	// file's path where it was another, a file since renamed (PathB64 per
	// [WirePath]).
	OrigLine    int    `json:"orig_line"`
	OrigPath    string `json:"orig_path,omitempty"`
	OrigPathB64 string `json:"orig_path_b64,omitempty"`
	// Text is the line without its line feed; past MaxBlameLineBytes, absent
	// with TextBytes its size.
	Text      string `json:"text,omitempty"`
	TextBytes int    `json:"text_bytes,omitempty"`
}

// BlameCommit is a commit a blame's lines name: its author and committer as it
// states them (never a `.mailmap`'s), its authored date and its subject, each
// described past its bound as a LogCommit's is.
type BlameCommit struct {
	Author           string `json:"author,omitempty"`
	AuthorBytes      int    `json:"author_bytes,omitempty"`
	AuthorEmail      string `json:"author_email,omitempty"`
	AuthorEmailBytes int    `json:"author_email_bytes,omitempty"`
	// Committer, CommitterEmail and their sizes, and EngineCommitted, are
	// as a LogCommit's.
	Committer           string    `json:"committer,omitempty"`
	CommitterBytes      int       `json:"committer_bytes,omitempty"`
	CommitterEmail      string    `json:"committer_email,omitempty"`
	CommitterEmailBytes int       `json:"committer_email_bytes,omitempty"`
	EngineCommitted     *bool     `json:"engine_committed,omitempty"`
	AuthoredAt          time.Time `json:"authored_at"`
	// Subject is the message's first LINE, as git's blame names it (not a log
	// subject's first paragraph), absent past MaxLogSubjectBytes with
	// SubjectBytes its size, empty for an empty message.
	Subject      string `json:"subject,omitempty"`
	SubjectBytes int    `json:"subject_bytes,omitempty"`
}

// RevisionFactsAnswer is what a revision's head is against its target's tip:
// the change it makes, as a function of the two commits alone.
type RevisionFactsAnswer struct {
	// Head and Target are the query's commits, whole.
	Head   string `json:"head"`
	Target string `json:"target"`
	// MergeBase is their best common ancestor, the BASE the change is measured
	// from, or empty for unrelated histories, measured from the empty tree.
	MergeBase string `json:"merge_base,omitempty"`
	// BaseTree and HeadTree are the trees of the base (the empty tree's id
	// where none) and of Head: two revisions holding the same pair hold the
	// same code, byte for byte, which an approval carries across whatever their
	// patch ids say ([ChangeClassNoCodeChange]). Empty only where HeadInTarget.
	BaseTree string `json:"base_tree,omitempty"`
	HeadTree string `json:"head_tree,omitempty"`
	// PatchID is `git patch-id --verbatim` of the cumulative diff from the base
	// to Head: the same for two revisions changing the same lines with the same
	// context wherever the base put them, another for any changed byte,
	// whitespace included. Empty for a revision changing nothing, and where
	// PatchIDOmission says why none was taken: an empty id equals nothing.
	PatchID string `json:"patch_id,omitempty"`
	// PatchIDOmission is why the node took no id of a non-empty diff, "" if it
	// did: past the bound on a diff it identifies (OmittedPastBound, final), or
	// out of the git budget (OmittedOutOfTime, not final,
	// [RevisionFactsAnswer.Final]). An answer's field: no versionedFields row
	// (the record carries [Revise.PatchIDOmitted], the final omission alone).
	PatchIDOmission FactsOmission `json:"patch_id_omission,omitempty"`
	// Touched is every path the diff from the base to Head changes, in git's
	// order, unless TouchesAll: more than a list holds ([MaxTouchedPaths],
	// [MaxTouchedBytes]) or a path JSON cannot carry exactly (not UTF-8), when
	// Touched is empty, read as every path.
	Touched    []string `json:"touched"`
	TouchesAll bool     `json:"touches_all,omitempty"`
	// Classes is the path classes of the touched files' changes, each path by
	// its place in Touched ([RevisionClasses]): each Go file changed in place,
	// both versions lexed ([ClassifyGo]) within [MaxClassedBytes] and the git
	// budget; incomplete past either, and where the revision touches all.
	Classes RevisionClasses `json:"classes,omitzero"`
	// ClassesOmission is why Classes are incomplete, "" where complete, as
	// PatchIDOmission is (OmittedPastBound also for a touches-all revision).
	ClassesOmission FactsOmission `json:"classes_omission,omitempty"`
	// HeadInTarget is Head reachable from Target: the change is in its target
	// already and changes nothing: no id, no path, no tree.
	HeadInTarget bool `json:"head_in_target,omitempty"`
}

// Final reports facts that depend on their two commits alone, as every Gitlet
// node answers them: nothing left out for want of time. An omission past a
// bound is final, alike on every node; one the git budget ran out before
// measures the node at that moment, so such facts are RECORDED BY NOBODY and
// asked again, never kept as though the commits were unidentifiable, which
// would cost every approval carried by the id a re-review exactly when the
// fleet is busiest.
func (f *RevisionFactsAnswer) Final() bool {
	return f.PatchIDOmission != OmittedOutOfTime && f.ClassesOmission != OmittedOutOfTime
}

// FactsOmission is why a revision's facts left out a part they may (its patch
// id, or classes of its paths), "" for none. A closed set ([FactsOmissions]).
type FactsOmission string

const (
	// OmittedPastBound is a part past its own bound (a diff past the patch-id
	// bound, Go past the classification bound, a touches-all revision): the
	// bound measures the two commits, so every node omits it alike: FINAL.
	OmittedPastBound FactsOmission = "past_bound"
	// OmittedOutOfTime is a part the read's git budget ran out before: it
	// depends on the node's load, so NOT FINAL ([RevisionFactsAnswer.Final]).
	OmittedOutOfTime FactsOmission = "out_of_time"
)

// FactsOmissions is the closed set.
func FactsOmissions() []FactsOmission { return []FactsOmission{OmittedPastBound, OmittedOutOfTime} }

// Valid reports an omission this build knows.
func (o FactsOmission) Valid() bool { return slices.Contains(FactsOmissions(), o) }

// ReanchorAnswer places every anchor of the query, in its order.
type ReanchorAnswer struct {
	Anchors []Reanchored `json:"anchors"`
}

// Reanchored is one anchor placed. Gone: its revision holds no file at its path
// (deleted, or now a directory or submodule). Otherwise Start and End are where
// its lines are now, moved by lines added or removed above them, and Outdated
// says they are not the lines it was written against (one changed or deleted,
// lines inserted between two of them, or the file binary on either side); Start
// and End are then where the range would be had only the changes above it moved
// it.
type Reanchored struct {
	ID       string `json:"id"`
	Start    int    `json:"start,omitempty"`
	End      int    `json:"end,omitempty"`
	Outdated bool   `json:"outdated,omitempty"`
	Gone     bool   `json:"gone,omitempty"`
}

// Coverage is what a Gitlet node's introduced index holds: every push in
// (From, Through] but those in (GapFrom, GapThrough], a range a full read
// passed over and the index has not yet read. Complete is From being the start
// of the kept push history (PushHistory), so a commit with no push inside
// complete coverage was introduced by a push listing more than MaxIntroduced
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
	// Target, TargetOID and At are the query's own.
	Target    string            `json:"target"`
	TargetOID string            `json:"target_oid"`
	At        statelog.Position `json:"at"`
	// Landed is Commit reachable from TargetOID.
	Landed bool `json:"landed"`
	// LandedBy is the push that made Commit reachable from Target and kept it
	// so (the first of the target's moves after which every move kept it
	// reachable), with its move (Move); nil where this node's index lacks that
	// push, which Detail says.
	LandedBy *PushedBy `json:"landed_by,omitempty"`
	Move     *RefMove  `json:"move,omitempty"`
	// IntroducedBy is the push that introduced Commit itself, maybe to another
	// ref (a branch merged later).
	IntroducedBy *PushedBy `json:"introduced_by,omitempty"`
	// Probes is the ancestry checks the search for the landing ran.
	Probes int `json:"probes"`
	// Detail says why LandedBy is nil: the landing predates the index, falls in
	// a range not yet read, or lies past what the search checked
	// (internal/gitlet/query's MaxLandedProbes, the git budget, the answer
	// falling due, or a read lane busy past gitproc.SlotWait), naming the oldest
	// push the search knows kept the commit.
	Detail string `json:"detail,omitempty"`
}

// TreeAnswer is one page of a tree's listing, in git's own order.
type TreeAnswer struct {
	// Tree is the id of the tree listed.
	Tree string `json:"tree"`
	// Path is the directory listed, from the root; empty for the root (PathB64
	// per [WirePath]).
	Path    string      `json:"path,omitempty"`
	PathB64 string      `json:"path_b64,omitempty"`
	Entries []TreeEntry `json:"entries"`
	// Total is how many entries the whole listing holds, every page.
	Total int `json:"total"`
}

// TreeEntry is one entry of a listing.
type TreeEntry struct {
	// Mode is git's: 040000 a directory, 100644 a file, 100755 an executable,
	// 120000 a symbolic link, 160000 a submodule.
	Mode string `json:"mode"`
	// Type is tree, blob or commit (a submodule's).
	Type string `json:"type"`
	OID  string `json:"oid"`
	// Path is the entry's, from the repository's root (PathB64 per
	// [WirePath]).
	Path    string `json:"path"`
	PathB64 string `json:"path_b64,omitempty"`
	// Size is a blob's bytes; a directory and a submodule carry none, and an
	// empty file reads as none.
	Size int64 `json:"size,omitempty"`
}

// BlobAnswer is one page of a file's lines.
type BlobAnswer struct {
	// Path is the file's, from the root, where the read named one (PathB64 per
	// [WirePath]).
	Path    string `json:"path,omitempty"`
	PathB64 string `json:"path_b64,omitempty"`
	OID     string `json:"oid"`
	Size    int64  `json:"size"`
	// Binary is a file git calls binary (a NUL in its first 8 000 bytes, git's
	// own test), answered by its size with no text.
	Binary bool `json:"binary,omitempty"`
	// Lines is how many lines the whole file has.
	Lines int `json:"lines"`
	// StartLine and EndLine are the lines Text holds, whole unless a page
	// begins or ends inside a line longer than a page: Text then begins
	// StartByte bytes into StartLine.
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
	StartByte int    `json:"start_byte,omitempty"`
	Text      string `json:"text,omitempty"`
	// Continues is set when Text ends inside line EndLine (served in pieces at
	// rune boundaries), and Next resumes it. Never a cut.
	Continues bool `json:"continues,omitempty"`
}

// DiffFile is one file of a diff's stat.
type DiffFile struct {
	// Path is the file's at head, or at base for a deleted file (PathB64 per
	// [WirePath]); OldPath is where a renamed file was.
	Path       string `json:"path"`
	PathB64    string `json:"path_b64,omitempty"`
	OldPath    string `json:"old_path,omitempty"`
	OldPathB64 string `json:"old_path_b64,omitempty"`
	// Status is git's letter: A added, D deleted, M modified, R renamed, T type
	// changed (a file made a link, say).
	Status string `json:"status"`
	// Similarity is a rename's, in percent.
	Similarity int `json:"similarity,omitempty"`
	// OldMode and NewMode are git's modes on each side, empty on a side the
	// file is absent from: only they tell a mode-only change.
	OldMode string `json:"old_mode,omitempty"`
	NewMode string `json:"new_mode,omitempty"`
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
	// Binary is a file git calls binary: no line counts, and no patch.
	Binary bool `json:"binary,omitempty"`
	// OldOID and NewOID are the blobs on each side, none on a side the file is
	// absent from.
	OldOID string `json:"old_oid,omitempty"`
	NewOID string `json:"new_oid,omitempty"`
}

// DiffAnswer is one page of a diff's stat, of one file's patch, or of every file's patch.
type DiffAnswer struct {
	// Base and Head are the commits (or trees) compared, as whole ids.
	Base string `json:"base"`
	Head string `json:"head"`
	// Files is a page of the changed files, in git's order (by path); a patch's
	// answer holds its one file on every page.
	Files []DiffFile `json:"files"`
	// Total, Added and Deleted are over the whole diff the query names (every
	// file Paths admits), on every page.
	Total   int `json:"total"`
	Added   int `json:"added"`
	Deleted int `json:"deleted"`
	// Patch is a page of one file's patch (DiffQuery.File), cut at line
	// boundaries; its header lines name the paths, as git's do.
	Patch string `json:"patch,omitempty"`
	// Patches is a page of every file's patch (DiffQuery.Patches), cut at line
	// boundaries; Files is empty on such a page, and the totals are the stat's.
	// A diff of no file answers one page with no patches member.
	Patches []FilePatch `json:"patches,omitempty"`
}

// FilePatch is one file's patch, or the page's part of it: File its place among
// the files the diff admits, from 0 in stat order, its path and status as the
// stat names them, and Continues where the next page goes on with it.
type FilePatch struct {
	File      int    `json:"file"`
	Path      string `json:"path"`
	PathB64   string `json:"path_b64,omitempty"`
	Status    string `json:"status"`
	Patch     string `json:"patch"`
	Continues bool   `json:"continues,omitempty"`
	// RestOmitted is a patch stopped before a line longer than one answer
	// holds: the rest of the file's patch is left out, and the pages continue
	// at the next file.
	RestOmitted bool `json:"rest_omitted,omitempty"`
}

// CompareAnswer is how two commits relate.
type CompareAnswer struct {
	// Base and Head are the whole ids compared; MergeBase their best common
	// ancestor, empty for unrelated histories.
	Base      string `json:"base"`
	Head      string `json:"head"`
	MergeBase string `json:"merge_base,omitempty"`
	// BaseIsAncestor is head containing base (a fast-forward from base to
	// head); HeadIsAncestor the reverse.
	BaseIsAncestor bool `json:"base_is_ancestor"`
	HeadIsAncestor bool `json:"head_is_ancestor"`
	// Ahead is the commits head has that base lacks, Behind the reverse.
	Ahead  int `json:"ahead"`
	Behind int `json:"behind"`
	// Files is the FIRST page of what head changed since the merge base (the
	// three-dot reading); a diff of the merge base and head reads the rest, and
	// Total, Added and Deleted are over all of it. Unrelated histories list none.
	Files   []DiffFile `json:"files"`
	Total   int        `json:"total"`
	Added   int        `json:"added"`
	Deleted int        `json:"deleted"`
}

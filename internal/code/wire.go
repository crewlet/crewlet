package code

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

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/routing"
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
	// Kind is what the repository is for: a project, or a seat's scratch
	// repository ([RepoKind]).
	Kind  RepoKind `json:"kind"`
	Owner Owner    `json:"owner"`
	// Visibility is who besides persons reads it — with Owner, all a grant
	// reads of the repository ([GrantOf]).
	Visibility Visibility `json:"visibility"`
	// Search is how code search reads it ([SearchMode]).
	Search SearchMode `json:"search"`
	// Mirrored is a PULL MIRROR ([RepoSettings.MirrorFrom]): it moves only
	// by its syncs, so a push and a change's every write are refused
	// repo_mirrored — what the code host's admission and an engine write's
	// landing read of it before the committer is asked. Never the mirror's
	// URL or credential, which no row a read serves carries.
	//
	// AN ANSWER'S FIELD, NOT A RECORD'S, so it takes no versionedFields row:
	// it is read from the row a record wrote. AS A STATED CONDITION, as the
	// mirror's own fields are (mirror.go): no build that carries the code
	// domain has run without it — none of the domain is on main — so every
	// node a code host asks answers it; were one without it ever to run
	// first, a host reading its answer would take a pull mirror's push to the
	// committer, which refuses it there, recorded, from the rows.
	Mirrored      bool   `json:"mirrored,omitempty"`
	DefaultBranch string `json:"default_branch"`
	Description   string `json:"description,omitempty"`
	// Protections are its rules AS A PUSH IS JUDGED BY THEM ([PushRule]),
	// sorted by pattern — never a rule's merge gate. This row is what the
	// admission of every git request reads, every page of a git node's sync
	// carries and its cache's state file keeps, and none of them reads a
	// gate: at the domain's bounds a repository's gates are half a mebibyte
	// of JSON, which each would decode, ship and write to disk for nothing.
	// One repository's detail answers them to a read that asks
	// ([ReadQuery.Gates], [RepoDetail.Gates]).
	Protections []PushRule `json:"protections,omitempty"`
	DeletedAt   time.Time  `json:"deleted_at,omitzero"`
	// LastActivityAt is the broker's instant of the repository's last push,
	// rename, settings change or restore — its IDLE CLOCK, which a scratch
	// repository expires by ([Retention]) and a turn's hold decides whether to
	// record itself by ([HoldRecordAfter]). Never a read, and never a hold. A
	// session read may be behind and show an older one, which can only make a
	// hold record itself where it did not need to.
	LastActivityAt time.Time `json:"last_activity_at,omitzero"`
	Refs           int64     `json:"refs"`
	LivePacks      int64     `json:"live_packs"`
	LiveBytes      int64     `json:"live_bytes"`
	// LiveObjects is how many objects the live packs hold, kept on the row
	// beside LiveBytes: the most a fetch request to the repository can
	// legitimately name, which the code host bounds a request it spools by.
	LiveObjects int64 `json:"live_objects"`
	// DropEpoch is how many expunges have begun on the repository
	// ([ExpungeBegin]): what a push carries of the row its receiver checked
	// it against, and the committer refuses it `expunging` where the
	// repository's has moved since. Omitted at zero, a repository never
	// expunged.
	DropEpoch int64 `json:"drop_epoch,omitempty"`
	// Redirect is the repository's current name where the name it was asked
	// by is an OLD one, still redirecting to it until RedirectUntil after a
	// rename ([RedirectWindow]) — "" where it was asked by its own. The
	// redirect lasts exactly as long as the old name's row: the release that
	// removes it is what ends it, so every node at one position answers one
	// way, which a comparison with each node's clock would not.
	Redirect      string    `json:"redirect,omitempty"`
	RedirectUntil time.Time `json:"redirect_until,omitzero"`
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
	// DropEpoch is the repository's drop epoch as page 1 read it: a later
	// page reading another answers ErrSnapshotMoved, so every page of one
	// snapshot is read at one epoch — the one its Repo carries, which a
	// push checked against the copy the snapshot synced is decided by
	// ([Proposal.DropEpoch]).
	DropEpoch int64 `json:"drop_epoch,omitempty"`
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
	// Content is the position the pack's CONTENT dates from as a purge
	// reads it: nothing in it reached the repository, or was last judged by
	// a purge's scan, before it. A push's pack dates from its own position;
	// a compaction's outputs, an expunge's that keeps what a ref drop left
	// within a grace and an expunge's rescue from the oldest pack they
	// replaced, since moving content judges none of it; and a purge's kept
	// and dropping packs from its scan ([ExpungeBegin.Seen]), where it
	// judged them. So a purge request is SERVED once no pack of its
	// repository dates from before it, and a cycle serving one takes the
	// live packs that do — whenever their rows were added, which a rewrite
	// moves past the request without judging anything. A read's answer,
	// never a record's field: no versionedFields row.
	Content statelog.Position `json:"content"`
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
// [FullPackBytes], or its whole bundle at least [FullBundleBytes] — FullAt at
// the cut every compaction makes, [CompactPackBytes].
//
// THE ONE PREDICATE, read by every party that judges it — the committer's
// decide of a compaction, which refuses a full input stale, and the plan a
// compaction is built from (internal/code/compact) — so a plan and the decide
// that judges it never disagree about a pack. By its bundle as well as its
// pack section, because an output of a cut scaled down for a dense roll is
// full by its bundle with its pack section well short of seven eighths of the
// nominal cut.
func (r PackRow) Full() bool { return r.FullAt(CompactPackBytes) }

// FullAt is [PackRow.Full] for compactions that cut their outputs at cut
// rather than at [CompactPackBytes]: both bounds, [FullPackBytes] and
// [FullBundleBytes], scaled by cut ÷ CompactPackBytes — exactly the two
// constants at the production cut, which is the one cut anything but a suite
// makes. A suite builds its compactions over megabytes rather than gibibytes
// (internal/code/compact's test knob), and an output of its cut has to be
// full by the same rule, scaled, or every one would be rolled again at the
// next plan. Defined for a cut in (0, CompactPackBytes], the only cuts a
// compaction is built at.
func (r PackRow) FullAt(cut int64) bool {
	return r.Ref().PackBytes() >= fullScaled(FullPackBytes, cut) || r.Size >= fullScaled(FullBundleBytes, cut)
}

// fullScaled is bound — one of the full bounds at the production cut — at a
// cut of cut. In whole bytes, without overflow: both are at most
// CompactPackBytes, so the product is under 2^60.
func fullScaled(bound, cut int64) int64 { return bound * cut / CompactPackBytes }

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
	// Change asks, of a push that asked for a change ([Proposal.Change]),
	// for the change it opened as well ([PushOutcome.Opened]). A field a
	// build without it ignores, answering no change: the receiver then says
	// it does not know what became of it, which is true.
	Change bool `json:"change,omitempty"`
}

// PushOutcome is what became of one push.
type PushOutcome struct {
	Found    bool              `json:"found"`
	Push     PushRow           `json:"push,omitzero"`
	Position statelog.Position `json:"position"`
	// Opened is the number of the change the push opened from its options
	// ([PushChangeID]), read in the snapshot its row was, where the query
	// asked and the push opened one; 0 otherwise.
	Opened int64 `json:"opened,omitempty"`
}

// PushRow is one push as its repository's history keeps it.
type PushRow struct {
	PushID   string            `json:"push_id"`
	RepoID   string            `json:"repo_id"`
	Position statelog.Position `json:"position"`
	At       time.Time         `json:"at"`
	// Actor is who pushed, as the history names it: an operator's credential
	// — the person's own attribution, as every record a write leaves names
	// it — or a seat's handle; ActorKind is the principal's kind, and
	// ActorSeat the seat: the human seat an operator's principal holds, or
	// the seat itself.
	Actor     string `json:"actor"`
	ActorKind string `json:"actor_kind"`
	ActorSeat string `json:"actor_seat,omitempty"`
	// Via is how the push's pack was built, and Run, for a seat's push, the
	// turn, the work item and the model behind it — and for a coding box's,
	// the run that pushed and its coding agent — each stamped by the engine
	// from the call or the run that made the push, never read from a commit.
	Via      PushVia       `json:"via"`
	Run      RunProvenance `json:"run,omitzero"`
	Node     string        `json:"node,omitempty"`
	Atomic   bool          `json:"atomic,omitempty"`
	Commands []Command     `json:"commands"`
	// Omitted is the commands a row read IN BRIEF ([ReadQuery.Brief]) leaves
	// out of Commands, counted by what became of each — so Commands and
	// Omitted together are every command the push carried, and a reader says
	// what it was not shown without asking for it. Absent wherever nothing
	// is left out: a row read whole, and a brief row of a push of at most
	// [BriefCommands] commands.
	Omitted         CommandCounts `json:"omitted,omitzero"`
	PackID          string        `json:"pack_id,omitempty"`
	PackBytes       int64         `json:"pack_bytes,omitempty"`
	Introduced      []string      `json:"introduced,omitempty"`
	IntroducedCount int           `json:"introduced_count,omitempty"`
	ReceivedAt      time.Time     `json:"received_at"`
}

// CommandCounts is commands of a push counted by what became of each, in the
// words a reader is told them in: a ref made, moved, moved by force or
// removed; asked for the value it already had; or refused. Every command is
// exactly one, decided in that order ([CommandCounts.add]) — a refusal is a
// refusal whatever it asked, and an unchanged ref moved nowhere — the order
// the dashboard's commandWord says one command in. Each count is omitted at
// zero.
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

// brief is a push row as a history read IN BRIEF serves it
// ([ReadQuery.Brief]): its introduced commits counted and not listed, and, of
// a push of more than BriefCommands commands, BriefCommands of them — its
// refused ones first, each with its reason, then the rest, each part in the
// order the client sent it — with every command left out counted in Omitted.
//
// THE REFUSALS FIRST because what did not happen is what a reader of a push
// has to act on: a push of a thousand tags with one refused, cut in the order
// it was sent, would show five that landed and hide the one that did not.
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
	refused := make([]Command, 0, BriefCommands)
	rest := make([]Command, 0, BriefCommands-keepRefused)
	for _, c := range row.Commands {
		switch {
		case c.Result == ResultNG && len(refused) < keepRefused:
			refused = append(refused, c)
		case c.Result != ResultNG && len(rest) < BriefCommands-keepRefused:
			rest = append(rest, c)
		default:
			row.Omitted.add(c)
		}
	}
	row.Commands = append(refused, rest...)
	return row
}

// PushVia is how a push's pack was built: STAMPED by whoever built it, carried
// by its proposal and its record's entry, and stored on its push row — never
// derived from the commits, whose committer header any client may write.
//
// THE ROW'S COLUMN HAS NO DOMAIN VALUE AS ITS DEFAULT (the empty string, in
// replicated 0063), and a row naming no way this build knows is refused by
// every reader of it (scanPush): an apply that forgot the column would
// otherwise read as one way or the other.
type PushVia string

const (
	// ViaGit is a push a client's git sent and receive-pack received.
	ViaGit PushVia = "git"
	// ViaEngine is a push whose commit and pack the engine built itself, from
	// a tool's call, rather than one git received — exactly one ref moved,
	// never --atomic ([Proposal.Validate]).
	ViaEngine PushVia = "engine"
	// ViaMerge is the push a MERGE RECORD lands ([Merge]): its target moved
	// onto the commit the merge built — and, where it deletes its source, the
	// source branch deleted — written by the merge's own apply, so the push
	// history names every move of a branch and who made it. No proposal of a
	// push names it ([Proposal.Validate]): a merge is decided through its
	// gate ([Committer.ProposeMerge]), never as a push.
	//
	// BASE FORMAT, under the condition merge.go's head states: no build that
	// carries the code domain has run without it.
	ViaMerge PushVia = "merge"
	// ViaExpiry is the ref lifecycle's deletion of seats' idle branches
	// (retention_expiry.go): a push by the engine's own account
	// ([PrincipalSystem]) — the one kind it pairs with, both ways — that only
	// deletes refs in seats' namespaces, carrying no pack
	// ([Proposal.Validate]), each command's Old the commit its push row keeps
	// to restore the branch at. Its decide keeps whatever is no longer idle
	// (rule 4b, [ExpiryRule]); its apply moves no activity clock
	// (apply_push.go).
	//
	// BASE FORMAT, AS A STATED CONDITION: no build that carries the code
	// domain has run without it — none of the domain is on main, so no image
	// and no release holds it. Once a build without it has run, an entry
	// carrying it takes {Name: "PushEntry.Via expiry", Since: 2, Op: "push",
	// Path: []string{"mutation", "pushes", "via"}, Equals: string(ViaExpiry)}
	// in versionedFields, with RecordVersion at 2, its line under "What each
	// version added" and a Carrying record in the statelogtest candidate:
	// such a build would apply the deletion as a push that moved the
	// repository's idle clock, and refuse its row's via as a way it does not
	// know (scanPush).
	ViaExpiry PushVia = "expiry"
	// ViaQueue is a push a LAND RECORD lands ([Land]), one for each change a
	// merge queue landed: its target moved from what the change before it
	// left onto the commit the queue built of it — and, where it deletes its
	// source, the source deleted — naming the change's ENQUEUER, since the
	// queue never acts on its own account. No proposal of a push names it: a
	// landing is decided through each change's gate.
	//
	// BASE FORMAT, under the condition queuerecords.go's head states.
	ViaQueue PushVia = "queue"
	// ViaImport is the push an IMPORT CHUNK lands ([ImportChunk]): the refs
	// it sets, onto objects its home fetched from the host the repository is
	// imported from, by the engine's own account ([PrincipalSystem]) —
	// written by the chunk's own apply. No proposal of a push names it.
	//
	// BASE FORMAT, under the condition [OpImportChunk] states.
	ViaImport PushVia = "import"
	// ViaMirror is the push a PULL MIRROR'S SYNC lands ([MirrorSync]): the
	// refs it moves onto its source's, by the engine's own account —
	// written by the sync's own apply. No proposal of a push names it.
	//
	// BASE FORMAT, under the condition [OpMirrorSync] states.
	ViaMirror PushVia = "mirror"
)

// PushVias is the closed set, in its documented order.
func PushVias() []PushVia {
	return []PushVia{ViaGit, ViaEngine, ViaMerge, ViaExpiry, ViaQueue, ViaImport, ViaMirror}
}

// Valid reports a way this build knows.
func (v PushVia) Valid() bool { return slices.Contains(PushVias(), v) }

// proposedVias is the ways a push PROPOSAL may name — git's, the engine's and
// the ref expiry's — in [PushVias]' order: a merge's push and a landing's are
// written by their records alone ([ViaMerge], [ViaQueue]).
func proposedVias() []PushVia { return []PushVia{ViaGit, ViaEngine, ViaExpiry} }

// proposed reports a way a push proposal may name ([proposedVias]).
func (v PushVia) proposed() bool { return slices.Contains(proposedVias(), v) }

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
// from the row's actor columns, how it was built from its via, and its ok
// commands as the refs it moved.
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

// principal is who made the push as its row's actor columns name it — the
// inverse of the push apply's [Principal.Actor]: an operator's credential
// and the seat it is bound to, or a seat by its handle with the run its push
// carried.
func (row PushRow) principal() Principal {
	if PrincipalKind(row.ActorKind) == PrincipalAgent {
		return Principal{Kind: PrincipalAgent, Seat: row.ActorSeat, Run: row.Run}
	}
	return Principal{Kind: PrincipalKind(row.ActorKind), OperatorID: row.Actor, Seat: row.ActorSeat}
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

// RunPushesQuery is the coding run a `run_pushes` read lists the pushes of:
// the run by its launch id, the id its pushes' rows name it by
// ([RunProvenance.RunID]); the repositories its launch was given, by id — the
// run could push nowhere else, and the read lists them in this order — and
// the position on the code log the launch read them at, above which every
// push the run's box made applied.
//
// THE READ SCANS INSTEAD OF SEEKING AN INDEX OF ITS OWN: each repository's
// pushes above After, over code_pushes_repo_idx in log order, keeping the
// run's own — every push made to the run's repositories DURING the run, read
// once, when the engine collects it, in pages each held to what it READS: at
// most the query's limit of those pushes and MaxPageBytes of what their rows
// weigh, as the index says before a row is touched, so a long run or a wide
// push beside it costs the read more pages and never one page more than
// that. An index on the run would hold an entry for every box push on every
// data node for the history's 181 days to serve that one read a run
// (replicated 0070 says what it would cost); BenchmarkRunPushes measures what
// the scan costs instead.
type RunPushesQuery struct {
	Run   string            `json:"run"`
	Repos []string          `json:"repos"`
	After statelog.Position `json:"after"`
}

// BranchChangesQuery is a `branch_changes` read: one repository by its id and
// the branches whose live changes the read answers, each a whole ref under
// refs/heads/, one to [MaxRefsPerQuery] of them, each once — a collected
// coding run's branches, which the engine names each delivered ref's change
// by. One seek of code_changes_open_idx a branch, the index partial on the
// live states that holds at most one change a branch, so the read costs its
// branches and never the repository's changes.
type BranchChangesQuery struct {
	RepoID string   `json:"repo_id"`
	Refs   []string `json:"refs"`
}

// BranchChange is the live change one branch of a `branch_changes` read heads:
// the branch as the query named it, the change's id and its number on its
// repository.
type BranchChange struct {
	Ref      string `json:"ref"`
	ChangeID string `json:"change_id"`
	Number   int64  `json:"number"`
}

// ChangesDueQuery is a `changes_due` read: the repositories a home owes
// change work in, or ONE repository's due change work — the repository by its
// id, which its home was nudged about or found listed — each judged against
// the instant the home's recheck of a merge is paced by.
type ChangesDueQuery struct {
	// RepoID is "" for the listing of repositories, and otherwise the
	// repository whose due changes the read pages.
	RepoID string `json:"repo_id,omitempty"`
	// RecheckBefore is REQUIRED: a change whose mergeability was recorded
	// against a tip its target has moved from since, or recorded undecided,
	// is due again once that record's broker instant is at or before it —
	// its home's clock less its refresh (internal/code/home's
	// MergeabilityRefresh) — so a target that moves on every push asks its
	// changes' merges again at the home's pace, never at its own.
	RecheckBefore time.Time `json:"recheck_before"`
	// Rechecks is, with a repository, the most changes due such a recheck
	// the read names on its first page ([ReadAnswer.Rechecks]), the change
	// whose mergeability is oldest first, 0 to MaxSnapshotItems; none past
	// its first page, and none without a repository.
	Rechecks int `json:"rechecks,omitempty"`
}

// ChangeDue is one live change a `changes_due` read names, its target a live
// branch of its repository, and what its home owes it: its head revision's
// FACTS where it holds none against the branch it merges into, and its head
// revision's MERGE where the change holds no mergeability of that revision
// against that branch — or, named among a read's rechecks, a mergeability its
// target has moved past, or one undecided, recorded at Checked. The change by
// its id and number, its head revision and head, its source branch — the
// listing's order and cursor — and its target with the tip the rows hold for
// it and the position of the record that moved it there: everything its home
// reads the revision's facts and merges it against and records them with,
// from the one snapshot the read was answered from. A `checks_due` read names
// its changes in the same shape, its tip the one whose checks file its home
// dispatches by, and owes them neither facts nor a merge.
type ChangeDue struct {
	ID          string            `json:"id"`
	Number      int64             `json:"number"`
	Revision    int64             `json:"revision"`
	Head        string            `json:"head"`
	SourceRef   string            `json:"source_ref"`
	TargetRef   string            `json:"target_ref"`
	Tip         string            `json:"tip"`
	TipPosition statelog.Position `json:"tip_position"`
	// FactsDue, MergeDue and RefreshDue say what the home owes a change of a
	// page of a `changes_due` read's due changes — its head revision's facts,
	// its merge, and a refresh of the readiness its mergeability holds
	// ([Mergeability.Refresh]); a recheck owes its merge alone, and says
	// none, and so does a `checks_due` read's change, which says what it owes
	// in Checks.
	//
	// RefreshDue is AN ESTATE ANSWER'S FIELD, NOT A RECORD'S, so it takes no
	// versionedFields row. AS A STATED CONDITION, as [ReadQuery.Push]'s: no
	// build that carries the code domain has run without it — none of the
	// domain is on main — and a home of a build that predates it would never
	// refresh a stored readiness, whose change's next merge records it again.
	FactsDue   bool `json:"facts_due,omitempty"`
	MergeDue   bool `json:"merge_due,omitempty"`
	RefreshDue bool `json:"refresh_due,omitempty"`
	// Checked is a recheck's: the broker instant its mergeability was
	// recorded at, which orders the rechecks; zero for every other.
	Checked time.Time `json:"checked,omitzero"`
	// Checks is what a `checks_due` read's change owes its checks
	// ([CheckWork]); nil on every `changes_due` read's.
	Checks *CheckWork `json:"checks,omitempty"`
}

// MaxRunRepos is the most repositories one [RunPushesQuery] names: the most a
// coding run is given — each costs a clone before its coding agent starts,
// inside its setup budget, and a run that needs more is a task to split — so a
// read of a run's pushes scans at most this many repositories' pushes above its
// launch. The bound a run's repositories are held to where they are granted
// may not pass it: a run given more could not be read whole.
const MaxRunRepos = 8

// RefQuery resolves refs of one repository — EVERY ONE IN ONE READ: one
// transaction at one checkpoint, so a linearizable resolve of a comparison's
// two sides pays one barrier and one round trip rather than one each, and
// compares the two as they stood at the same moment. Each name is answered on
// its own ([RefAnswer.Refs]).
type RefQuery struct {
	// Repo is the repository's NAME.
	Repo string `json:"repo"`
	// Refs is every ref the read resolves, one to [MaxRefsPerQuery] of
	// them: each a whole ref (refs/heads/main), or a branch or a tag by its
	// short name — a branch first, then a tag.
	Refs []string `json:"refs"`
	// Fresh is REQUIRED, as on every code point read: whoever names a ref
	// names the level it must be read at.
	Fresh statelog.Freshness `json:"fresh"`
	// Asker is who resolves them, REQUIRED and held to its grant on the
	// repository by the node that answers, as a [ReadQuery]'s is.
	Asker Principal `json:"asker"`
	// CeilingSeat is the seat the asking seat's turn is acting for, whose
	// grant bounds the resolve as [ReadQuery.CeilingSeat] bounds a read.
	CeilingSeat string `json:"ceiling_seat,omitempty"`
}

// MaxRefsPerQuery is the most refs one [RefQuery] resolves: what one page of
// the refs listing answers ([MaxRefsPerPage]), the reader's one bound on the
// refs a read serves. A name costs at most two point reads of code_refs' key
// and answers at most two refs of [MaxRefNameBytes] and a sentence, so a full
// query answers in about a megabyte, inside [MaxPageBytes]. A code tool's call
// names two at most — a comparison's sides, a commit and its target.
const MaxRefsPerQuery = MaxRefsPerPage

// RefAnswer is what a [RefQuery]'s refs resolved to.
type RefAnswer struct {
	Repo RepoRef `json:"repo"`
	// Refs answers the query's Refs, one each and in their order — a name
	// no live ref holds answered as such beside the rest.
	Refs []ResolvedRef `json:"refs"`
	// Position is the checkpoint every ref was read at, read in the read's
	// own transaction.
	Position statelog.Position `json:"position"`
}

// ResolvedRef is one name of a [RefQuery], as the read found it.
type ResolvedRef struct {
	// Name is the name as the query named it.
	Name string `json:"name"`
	// Ref is the whole ref it resolved to and OID the object that ref names
	// — both empty where no live ref holds the name (Missing).
	Ref string `json:"ref,omitempty"`
	OID string `json:"oid,omitempty"`
	// Moved is the position of the record that last moved the ref — the
	// push that set it to OID, its row's own position — at or before the
	// answer's Position. WHAT A READ OF OID IS FLOORED AT: OID's objects
	// landed in that record or an earlier one, so every copy of the
	// repository at or past Moved holds them, and the target's moves up to
	// its tip are every move up to Moved. Never the answer's Position, which
	// a linearizable read takes past its own barrier, so a copy current to
	// every push still sits below it and a floor there would make a git
	// node sync for nothing.
	Moved statelog.Position `json:"moved,omitzero"`
	// At and Seat are the ref's row's broker instant of its last move and the
	// seat that set it inside its own namespace ("" for none, [seatOf]), read
	// in the one lookup that answers OID: what a turn's hold on a seat's
	// branch decides from whether to record itself ([RefHoldRecordAfter]),
	// from the same read whose position the hold is decided at.
	//
	// AN ESTATE ANSWER'S FIELDS, NOT A RECORD'S, so they take no
	// versionedFields row. AS A STATED CONDITION, as [LifecycleResult.Rule]'s:
	// no build that carries the code domain has run without them — none of
	// the domain is on main. A build that predated them would answer every
	// ref seat-less, and a hold decided from that answer would record
	// nothing; so were such a build ever to run beside this one, a hold would
	// read the ref's row in its own decide rather than ride fields such a
	// build leaves out.
	At   time.Time `json:"at,omitzero"`
	Seat string    `json:"seat,omitempty"`
	// Missing is, for a name no live ref holds, the sentence that says so,
	// naming every ref tried: what [RefAnswer.Err] refuses it with.
	Missing string `json:"missing,omitempty"`
}

// Err is the refusal of the names no live ref holds — [ErrRefNotFound] in the
// sentence of each, in the query's order — or nil where every name resolved:
// what a caller that needs every ref it named refuses its call with.
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
	// ReadRunPushes is ONE CODING RUN'S PUSHES, oldest first — what its box
	// delivered to the code host: every push row naming the run
	// ([RunProvenance.RunID]) among those of the repositories the run was
	// given, above the position it was launched at ([RunPushesQuery]). The
	// engine's own read, which no principal makes, as `introduced` is: the
	// engine reads it when it collects the run, for the seat that launched it,
	// every page to the last — a page held to the pushes it READ may answer
	// none of the run's and a cursor past them.
	ReadRunPushes ReadKind = "run_pushes"
	// ReadRefs is one repository's live refs under a prefix, by name; its
	// first page carries the repository in detail too.
	ReadRefs ReadKind = "refs"
	// ReadChanges lists changes, NEWEST FIRST: one repository's in number
	// order, or those of every repository the asker may read in the order
	// the records that opened them landed — narrowed by state, by owner and
	// by work item ([ReadQuery.State]), each listing walking the one index
	// its filters lead.
	ReadChanges ReadKind = "changes"
	// ReadChange is one change by its number on its repository (web!42):
	// its row, its description and its authors in detail, and its revisions
	// newest first, a page at a time — a later page its revisions alone.
	ReadChange ReadKind = "change"
	// ReadChangeRouting is WHOM ONE CHANGE'S RECORDS REACHED (routedread.go):
	// the change by its number on its repository, as a `change` read names
	// it, and the records about it that reached anybody, newest first, a
	// page of whole records at a time — each with every recipient its
	// notices name (code_notifications, by the change) — held to the grant
	// on its repository as a `change` read is.
	ReadChangeRouting ReadKind = "change_routing"
	// ReadReviews is ONE REVIEWER'S REVIEWS (conversationread.go): the
	// reviews the reviewer [ReadQuery.Reviewer] names gave, newest first, of
	// every change of the repositories the asker may read, each naming the
	// change it judged ([ReviewRow.Change]).
	ReadReviews ReadKind = "reviews"
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
	// ReadPublicationDue is the repositories whose live packs are past the
	// ones their last publication's recount found a layer covering — every
	// one holding a live pack no published layer covers, and one holding
	// only a pack pushed since that recount that is byte-identical to a
	// published one, which has nothing to publish ([PublicationCandidate])
	// — LEAST RECENTLY PUBLISHED FIRST, one never published before any
	// other, each with what a publication is due by: the listing a
	// repository's maintainer polls for the repositories a publication may
	// be due in. None with an expunge open, whose publication rule 6g
	// refuses until its commit. The engine's own read, as `compaction_due`
	// is.
	ReadPublicationDue ReadKind = "publication_due"
	// ReadExpungeDue is the active and archived repositories with an
	// expunge open or a purge requested, by id, each with what its
	// maintainer finishes or serves it by: the listing a repository's
	// maintainer polls for the expunges it owes. The engine's own read, as
	// `compaction_due` is.
	ReadExpungeDue ReadKind = "expunge_due"
	// ReadExpungeTips is the objects one repository's expunge keeps beside
	// its refs ([ReadQuery.ExpungeTips]): the object each forced or deleting
	// command left since an instant (code_ref_drops) — what the expunge's
	// grace keeps reachable — in the order the drops' key holds them. The
	// engine's own read, which no principal makes: a repository's maintainer
	// reads it at its scan and at its rescue (internal/code/compact).
	ReadExpungeTips ReadKind = "expunge_tips"
	// ReadImportsDue is the repositories being imported, by id, each with
	// its source and how many of its chunks have landed
	// ([ImportCandidate]): the listing a repository's home polls for the
	// imports it owes. The engine's own read, as `compaction_due` is.
	ReadImportsDue ReadKind = "imports_due"
	// ReadMirrorsDue is the repositories with a mirror ([MirrorCandidate]):
	// every push mirror, by id, then the active pull mirrors LEAST RECENTLY
	// SYNCED FIRST — so a home that syncs every MirrorInterval reads pages
	// until one is synced within it, and no further — the listing a
	// repository's home polls for the mirrors it keeps. The engine's own
	// read, as `compaction_due` is.
	ReadMirrorsDue ReadKind = "mirrors_due"
	// ReadBranchChanges is the LIVE CHANGE each of one repository's branches
	// heads ([BranchChangesQuery]), in the order the query names the
	// branches, a branch heading none left out. The engine's own read, as
	// `run_pushes` is, which no principal makes and no grant holds: what it
	// names each ref a collected coding run moved by — the change the run
	// delivered to — and what tells it whether the change a run asked to
	// open at its collection is open already.
	ReadBranchChanges ReadKind = "branch_changes"
	// ReadChangesDue is the change work a repository's HOME git node owes
	// ([ChangesDueQuery]): with no repository, the active repositories
	// holding a LIVE change whose head revision has no facts or no
	// mergeability against the branch it merges into, or whose mergeability
	// is due again, by id; with one, the first two kinds of that repository,
	// by source branch, each with its target's tip as the rows hold it
	// ([ChangeDue]), beside the repository's path owners, and the third
	// oldest first. The engine's own read, as `compaction_due` is: what its
	// home polls and is nudged into reading.
	ReadChangesDue ReadKind = "changes_due"
	// ReadIndexed is the active and archived repositories searched by index
	// ([SearchIndexed]), BY NAME, each by its id and name, a page of
	// MaxSnapshotItems after the [ReadQuery.Cursor] — the last name a page
	// served, "" from the start: the listing a git node's indexer polls for
	// the repositories whose code index it may keep (M6-D12). The engine's
	// own read, as `compaction_due` is.
	ReadIndexed ReadKind = "indexed"
	// ReadChecks lists attempts of checks ([CheckRow]), held to the asker's
	// grant: one repository's, newest ended first and every attempt not yet
	// ended after them; one change's of it, or one commit's, by commit, name
	// and attempt ([CheckQuery]); or, naming no repository, the WINDOW —
	// every repository's the asker may read whose attempts ended at or after
	// [CheckQuery.Since], newest ended first.
	ReadChecks ReadKind = "checks"
	// ReadCheck is one attempt read whole ([CheckDetail]): its row, its
	// definition, its tail, the failures its ending named and its kept log —
	// held to the asker's grant on its repository.
	ReadCheck ReadKind = "check"
	// ReadChecksDue is the check work a repository's HOME git node owes
	// ([ChecksDueQuery]): with no repository, the active repositories holding
	// a LIVE change whose head no dispatch for its target names the checks
	// file its target's row holds, or whose head's latest attempt of a check
	// its infrastructure ended below [AutoRequeueAttempts], or a target due a
	// reading of its checks file again, by id; with one, those changes of
	// that repository, by source branch, each with its target's tip as the
	// rows hold it ([ChangeDue]) — the tip whose checks file defines what the
	// home dispatches — and what it owes ([CheckWork]), and on its first page
	// the targets due a reading ([CheckTargetDue]). The engine's own read, as
	// `changes_due` is (checksdispatch.go).
	ReadChecksDue ReadKind = "checks_due"
	// ReadCheckBox is the ONE attempt a check's box's token names, by its key
	// ([ReadQuery.CheckBox]): its row as the rows hold it, in the answer's
	// Checks — one row, or none. THE ENGINE'S OWN, as `checks_due` is: the
	// box's admission to its repository (internal/code/host) reads it on
	// every request, and a box is no principal — the row it reads is its
	// authority — so it names no asker and is held to no grant. A kind of its
	// own rather than a `check` read naming none, so that a principal's read
	// that forgot its asker is refused rather than answered ungated.
	ReadCheckBox ReadKind = "check_box"
	// ReadChecksQueued is the attempts waiting for a runner, OLDEST QUEUED
	// FIRST, each with its definition and its repository's name
	// ([RunnableCheck]): a git node's CLAIM WALK (internal/code/checkrun),
	// every repository's whose repository a runner may still claim in. The
	// engine's own, as `check_box` is: no principal makes it, and it is held
	// to no grant.
	ReadChecksQueued ReadKind = "checks_queued"
	// ReadChecksRunning is the attempts runners hold ([ChecksRunningQuery]):
	// with no repository, the repositories holding a running attempt, by id;
	// with one, its running attempts ([CheckRow]). The engine's own: what a
	// repository's HOME sweeps for a runner that is gone.
	ReadChecksRunning ReadKind = "checks_running"
	// ReadQueue lists the LIVE entries of merge queues ([QueueRow]), held to
	// the asker's grant: one repository's in target and queue order — one of
	// its targets', where [ReadQuery.Target] names one — beside the
	// candidates of the trains testing its targets ([CandidateRow]); or,
	// naming no repository, every repository's the asker may read.
	ReadQueue ReadKind = "queue"
	// ReadQueueDue is the queue work a repository's HOME git node owes
	// ([QueueDueQuery]): with no repository, the active repositories holding
	// a live entry, by id; with one, its live entries in queue order. The
	// engine's own read, as `changes_due` is.
	ReadQueueDue ReadKind = "queue_due"
	// ReadLandings is one protected target's landings above its green
	// ([LandingRow]), in the order they landed, beside its green row
	// ([GreenRow]) — held to the asker's grant on its repository.
	ReadLandings ReadKind = "landings"
	// ReadGreen is one repository's green rows ([GreenRow]) — each protected
	// target's newest commit whose required checks passed — or one target's,
	// held to the asker's grant.
	ReadGreen ReadKind = "green"
	// ReadCommit is ONE COMMIT'S CHANGES (commitread.go): the repository in
	// detail, as a `repo` read answers it, and every change the commit
	// [ReadQuery.Commit] names belongs to — each whose merge it is and each
	// revision it heads — read in the same snapshot and held to the asker's
	// grant on the repository.
	ReadCommit ReadKind = "commit"
	// ReadInbox is ONE PERSON'S CODE INBOX (inboxread.go): their notices,
	// newest first on the code log, filtered in the scan by their own marks
	// and the query's narrowing, each marked and carrying what its change
	// says — held to the asker's grant on each notice's repository.
	ReadInbox ReadKind = "inbox"
)

// ReadKinds is the closed set.
//
// A KIND IS NEW TO A BUILD THAT PREDATES IT, and such a build refuses it as a
// kind it does not know ([ErrInvalid]) — never answers it as another — so the
// fields only a newer kind reads ride a request that build refuses whole, and
// take no versionedFields row: `changes` and `change` are the only kinds that
// read State, Owner and Work, `change` and `change_routing` the only ones that
// read [ReadQuery.Change], `changes` and `reviews` the only ones that read
// [ReadQuery.Reviewer], `run_pushes` the only one
// that reads [ReadQuery.RunPushes], `expunge_tips` the only one that reads
// [ReadQuery.ExpungeTips], `branch_changes` the only one that reads
// [ReadQuery.BranchChanges], `check_box` the only one that reads
// [ReadQuery.CheckBox], `checks_running` the only one that reads
// [ReadQuery.ChecksRunning], `queue`, `landings` and `green` the only ones
// that read [ReadQuery.Target], `queue_due` the only one that reads
// [ReadQuery.QueueDue], `commit` the only one that reads
// [ReadQuery.Commit], and `inbox` the only one that reads
// [ReadQuery.Inbox].
func ReadKinds() []ReadKind {
	return []ReadKind{ReadRepos, ReadRepo, ReadPushes, ReadRunPushes, ReadRefs, ReadChanges, ReadChange,
		ReadChangeRouting, ReadReviews, ReadCommit,
		ReadIntroduced, ReadCompactionDue, ReadPublicationDue, ReadExpungeDue, ReadExpungeTips, ReadImportsDue,
		ReadMirrorsDue, ReadBranchChanges, ReadChangesDue, ReadIndexed,
		ReadChecks, ReadCheck, ReadChecksDue, ReadCheckBox, ReadChecksQueued, ReadChecksRunning, ReadQueue,
		ReadQueueDue, ReadLandings, ReadGreen, ReadInbox}
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
	// caller that forgot it opens. REFUSED on `introduced`, on `run_pushes`
	// and on the duty listings `compaction_due`, `publication_due`,
	// `expunge_due`, `changes_due`, `checks_due` and `indexed`, which no
	// principal makes: an asker there would be judged by nothing.
	Asker Principal `json:"asker"`
	// CeilingSeat is the handle of the seat the asking seat's turn is acting
	// FOR — the colleague whose ask woke it — and "" for a read made for its
	// asker alone: the REQUESTER CEILING (authz.DecideWithin's) applied to a
	// repository's grant. The node that answers judges that seat's grant
	// beside the asker's, from the same chart and the same row ([JudgeRead]),
	// and serves only what BOTH may read: a listing leaves out what either
	// may not, and a read the asker's own grant covers and the ceiling's
	// does not is [ErrAboveCeiling]. Judged where the read is answered, as
	// the asker's grant is (ADR-0058), and never where the turn runs: a
	// ceiling judged against the repository a turn resolved once would keep
	// admitting a seat a later activation shut out of it for the rest of
	// the turn, while the asker's own grant was judged afresh at every read.
	//
	// ONLY A SEAT, AND ONLY BESIDE A SEAT: a person's or a token's read is
	// its own, which no ceiling bounds, and a seat is the one requester
	// whose grant reads the chart — a person maintains every repository
	// (ADR-0055) and a stranger reads none, both settled where the turn runs
	// before any read is asked. Refused on `introduced`, on `run_pushes` and
	// on the duty listings `compaction_due`, `publication_due` and
	// `expunge_due`, which no principal makes.
	//
	// AN ESTATE REQUEST'S FIELD, NOT A RECORD'S, so it takes no
	// versionedFields row — and one a build that predates it would decode
	// and ignore (the estate reads arguments leniently), serving the read to
	// the asker's grant alone: a ceiling dropped, never a read refused. AS A
	// STATED CONDITION: no build that carries the code domain has run
	// without this field — none of the domain is on main, so no image and
	// no release serves a code read — and while that is so, every node that
	// answers a read made for a seat judges that seat. Were a build without
	// it ever to run first, a read made for a seat would move to operations
	// of its own, which such a build refuses as operations it does not know,
	// rather than ride a field it ignores.
	CeilingSeat string `json:"ceiling_seat,omitempty"`
	// Repo is the repository's NAME: required for repo, pushes, refs and
	// change, and for changes the one repository the listing is of — where
	// it names none, every repository the asker may read.
	Repo string `json:"repo,omitempty"`
	// Status filters a repos listing; empty is active and archived.
	Status []RepoStatus `json:"status,omitempty"`
	// Prefix narrows a refs listing to the refs under it: "refs/heads/"
	// (the default) or "refs/tags/", or a deeper prefix ending in "/" — a
	// seat's own namespace.
	Prefix string `json:"prefix,omitempty"`
	// Brief asks a push history for its rows IN BRIEF: the commits each push
	// introduced counted and not listed, and the commands of a push of more
	// than [BriefCommands] cut to that many — its refused ones first, each
	// with its reason, then the rest in the order its client sent them — with
	// every command left out counted by what became of it
	// ([PushRow.Omitted]). A screen draws a push a few lines tall, and an
	// import's or a mirror's push of up to MaxCommandsPerPush refs, with its
	// introduced ids, is 140 to 460 kilobytes of row a listing never shows,
	// sent again on every poll of the page; [ReadQuery.Push] reads one push
	// whole. Pushes only, refused on every other kind.
	//
	// AN ESTATE REQUEST'S FIELD, NOT A RECORD'S, so it takes no
	// versionedFields row: a build that predates it decodes and ignores it,
	// and answers the rows whole — a heavier answer, never a wrong one: a
	// whole row's commands are every one it carried, and it omits none.
	Brief bool `json:"brief,omitempty"`
	// Push narrows a push history to ONE push, by its id, read whole: every
	// command it carried and every commit it introduced. A push the
	// repository's history does not hold — another repository's, or one past
	// PushHistory — is an answer with no row, as a listing narrowed to it
	// holds none. Pushes only, and refused beside a cursor, a limit or Brief,
	// which page or cut a listing it is not.
	//
	// AN ESTATE REQUEST'S FIELD, NOT A RECORD'S, so it takes no
	// versionedFields row — and one a build that predates it would decode and
	// ignore, answering the history's first page in its place. AS A STATED
	// CONDITION, as [ReadQuery.CeilingSeat]'s: no build that carries the code
	// domain has run without this field — none of the domain is on main, so
	// no image and no release serves a code read. Were a build without it
	// ever to run first, a read of one push would move to a kind of its own,
	// which such a build refuses as a kind it does not know, rather than ride
	// a field it ignores.
	Push string `json:"push,omitempty"`
	// Commit is the commit a `commit` read names the changes of, its whole
	// id; refused on every other kind. A field only that kind reads, which a
	// build that predates the kind refuses whole ([ReadKinds]).
	Commit string `json:"commit,omitempty"`
	// PathOwners asks a `repo` read for the repository's PATH OWNERS beside
	// its detail ([RepoDetail.PathOwners]), and nothing else answers them: a
	// read of who owns which of its paths sets it — the API's question of one
	// repository does — and a read that RESOLVES a repository does not:
	// every seat tool's once a turn, a refs listing's first page, the status
	// and readers questions'. None of those reads a path owner, and at their
	// bound the path owners are a quarter of a mebibyte of JSON on each — one
	// more statement on the serving node, and the bytes encoded there,
	// shipped and decoded to be dropped. Refused on every other kind.
	//
	// AN ESTATE REQUEST'S FIELD, NOT A RECORD'S, so it takes no
	// versionedFields row. AS A STATED CONDITION, as [ReadQuery.Push]'s: no
	// build that carries the code domain has run without this field — none
	// of the domain is on main — and a build that predates it, which decodes
	// and ignores it, answers every detail with the path owners: a heavier
	// answer, never a wrong one.
	PathOwners bool `json:"path_owners,omitempty"`
	// Gates asks a read that answers one repository's DETAIL — a `repo`
	// read, or a refs listing's first page, which carries it — for the merge
	// gates of its rules ([RepoDetail.Gates]), and nothing else answers
	// them: a read that presents a rule's gate sets it — the API's question
	// of one repository, code_get_repo — and a read that resolves a
	// repository does not, for the path owners' reason: none of those reads
	// a gate, and at their bound a repository's gates are half a mebibyte of
	// JSON on each. Refused on every other read, and on a refs listing's
	// later pages, which carry no detail.
	//
	// AN ESTATE REQUEST'S FIELD, NOT A RECORD'S, so it takes no
	// versionedFields row. AS A STATED CONDITION, as [ReadQuery.Push]'s: no
	// build that carries the code domain has run without this field — none
	// of the domain is on main, so no image and no release serves a code
	// read. A build that predated it would decode and ignore it, and answer
	// a detail with no gates to a read that asked for them, every gated rule
	// read as ungated; so were a build without it ever to run first, a read
	// of the gates would move to a kind of its own, which such a build
	// refuses as a kind it does not know, rather than ride a field it
	// ignores.
	Gates bool `json:"gates,omitempty"`
	// Change is a `change` read's number, and a `change_routing` read's: the
	// change's on the repository Repo names, web!42's 42 — REQUIRED there,
	// at least 1, and refused on every other kind.
	Change int64 `json:"change,omitempty"`
	// State, Owner and Work narrow a `changes` listing, each refused on every
	// other kind: the changes in one state; the changes one principal opened,
	// by the handle their rows name it by — a seat's own, or the human seat a
	// person holds; and the changes carrying one work item, by its identity
	// across trackers, `<backend>:<id>` ([types.WorkItem.Ref]). Repo, where
	// set, narrows the listing to one repository.
	//
	// A LISTING NAMES A REPOSITORY, A WORK ITEM OR A STATE — what the indexes
	// it walks lead with — and is refused naming none: there is no index of
	// every change the company holds, and an owner's changes are kept by
	// state. It walks the ONE index its filters lead (changeread.go): a work
	// item's changes; an owner's in a state; a repository's in a state; a
	// repository's; the company's in a state — and tests every filter that
	// index does not seek on the rows the page read, so a page is the rows
	// it READ, never the rows it kept, and may be short, or empty with a
	// cursor, as a listing of repositories a seat reads little of is.
	State ChangeState `json:"state,omitempty"`
	Owner string      `json:"owner,omitempty"`
	Work  string      `json:"work,omitempty"`
	// Reviewer and Needs narrow a `changes` listing by what its changes ask
	// of somebody, each refused on every other kind (conversationread.go):
	// Reviewer, the changes whose ask of that reviewer stands — a seat by
	// its handle, or a person by the human seat they hold; and Needs, the
	// LIVE changes waiting on whom it names ([Need]): on a reviewer's review
	// (review), on their owner's fix (fix), on the asker either way (me), or
	// on a person (person) — the last names no reviewer, whoever asks. A
	// listing either names walks the ONE index of who the changes wait on —
	// a reviewer's standing asks, an owner's changes in a state, the changes
	// whose stored readiness waits on a person — and tests every other
	// filter on the rows it read, as every listing does.
	//
	// THE CHANGES KINDS CAME WITH THIS BUILD'S LINE: no build that carries
	// the code domain has run without these two — none of it is on main —
	// so no build ignores them and answers the wider listing they narrow.
	//
	// Reviewer is also a `reviews` listing's REQUIRED reviewer — whose
	// reviews it lists, a seat by its handle or a person by the human seat
	// they hold — which no build that predates the kind reads, since it
	// refuses the kind whole.
	Reviewer string `json:"reviewer,omitempty"`
	Needs    Need   `json:"needs,omitempty"`
	// Part is the list a `change` read pages beside the change — its
	// revisions, which "" names too, its reviewers, its comments, its
	// reviews or its findings ([ChangeParts]) — refused on every other kind.
	// The change's detail rides a first page of its revisions alone; every
	// page of another part carries that part's rows, held to the grant again
	// as every page of a change is. Under the same condition as Reviewer: no
	// build ignores it.
	Part ChangePart `json:"part,omitempty"`
	// Introduced is an `introduced` read's range: REQUIRED for that kind,
	// refused on every other.
	Introduced *IntroducedQuery `json:"introduced,omitempty"`
	// RunPushes is a `run_pushes` read's run: REQUIRED for that kind,
	// refused on every other. An estate request's field and no record's, so
	// it takes no versionedFields row: a node of a build that predates the
	// kind refuses the read as a kind it does not know (ErrInvalid) — the
	// run's pushes unread, never answered as none.
	RunPushes *RunPushesQuery `json:"run_pushes,omitempty"`
	// ChangesDue is a `changes_due` read's own: REQUIRED for that kind and
	// refused on every other ([ChangesDueQuery]). An estate request's field
	// and no record's, so it takes no versionedFields row: a node of a build
	// that predates the kind refuses the read as a kind it does not know
	// (ErrInvalid) — the home's work waits, never answered as none.
	ChangesDue *ChangesDueQuery `json:"changes_due,omitempty"`
	// Checks narrows a `checks` read to a change's attempts or a commit's,
	// or bounds its window; and names the one attempt a `check` read is of,
	// REQUIRED there. Refused on every other kind. ChecksDue is a
	// `checks_due` read's question, REQUIRED there as ChangesDue is a
	// `changes_due` read's, and refused on every other kind. Estate request fields and no
	// record's, so they take no versionedFields row: a node of a build that
	// predates the kinds refuses the read as a kind it does not know.
	Checks    *CheckQuery     `json:"checks,omitempty"`
	ChecksDue *ChecksDueQuery `json:"checks_due,omitempty"`
	// CheckBox is the attempt a `check_box` read is of, by its KEY — its
	// repository by id, as the token that names it does: REQUIRED for that
	// kind, refused on every other. An estate request's field and no
	// record's, so it takes no versionedFields row: a node of a build that
	// predates the kind refuses the read as a kind it does not know
	// (ErrInvalid) — the box refused, never admitted by a row read as none.
	CheckBox *CheckKey `json:"check_box,omitempty"`
	// ChecksRunning is a `checks_running` read's question, REQUIRED for that
	// kind and refused on every other: the repositories holding a running
	// attempt, or one repository's running attempts by its id. An estate
	// request's field and no record's, so it takes no versionedFields row: a
	// node of a build that predates the kind refuses the read as a kind it
	// does not know (ErrInvalid) — the sweep waits, never answered as none.
	ChecksRunning *ChecksRunningQuery `json:"checks_running,omitempty"`
	// ExpungeTips is an `expunge_tips` read's repository and instant:
	// REQUIRED for that kind, refused on every other. An estate request's
	// field and no record's, so it takes no versionedFields row: a node of a
	// build that predates the kind refuses the read as a kind it does not
	// know (ErrInvalid) — the scan unread, never answered as no tips.
	ExpungeTips *ExpungeTipsQuery `json:"expunge_tips,omitempty"`
	// BranchChanges is a `branch_changes` read's repository and branches:
	// REQUIRED for that kind, refused on every other. An estate request's
	// field and no record's, so it takes no versionedFields row: a node of a
	// build that predates the kind refuses the read as a kind it does not
	// know (ErrInvalid) — the run's delivered refs named by no change, never
	// answered as heading none.
	BranchChanges *BranchChangesQuery `json:"branch_changes,omitempty"`
	// Target narrows a `queue` read of a repository to one of its branches'
	// queues, and names the protected branch a `landings` read lists and a
	// `green` read answers — REQUIRED for landings — a branch a change may
	// name, beside the repository Repo names; refused on every other kind.
	// QueueDue is a `queue_due` read's own: REQUIRED for that kind and
	// refused on every other ([QueueDueQuery]). Estate request fields and no
	// record's, so they take no versionedFields row: a node of a build that
	// predates the kinds refuses the read as a kind it does not know.
	Target   string         `json:"target,omitempty"`
	QueueDue *QueueDueQuery `json:"queue_due,omitempty"`

	// Inbox is an `inbox` read's question: whose inbox, and how it is
	// narrowed ([InboxQuery]). Only that kind reads it.
	Inbox *InboxQuery `json:"inbox,omitempty"`
	// MinPacks is a `compaction_due` read's floor — the repositories
	// holding at least this many live packs — REQUIRED, at least 1, for that
	// kind and refused on every other.
	MinPacks int    `json:"min_packs,omitempty"`
	Cursor   string `json:"cursor,omitempty"`
	// Limit is repos 1..500 (0 = 100), pushes 1..200 (0 = 50), refs
	// 1..1 000 (0 = 200), changes 1..200 (0 = 50) — the rows a page READS,
	// whatever its filters keep of them — a change's revisions and its
	// reviewers 1..1 000 (0 = 200), its comments, its reviews and its
	// findings 1..200 (0 = 50), the records of a change's routing 1..200
	// (0 = 50), a reviewer's reviews 1..200 (0 = 50) — the rows a page READS,
	// as a listing of changes — and the engine's own two reads of pushes in
	// log order, introduced and run_pushes, 1..MaxSnapshotItems (0 = the
	// most) — run_pushes' the pushes of its run's repositories a page READS,
	// whatever few of them name the run; checks 1..200 (0 = 50), the rows a
	// page READS, as a listing of changes; checks_queued 1..MaxSnapshotItems
	// (0 = the most), the attempts a runner's claim walk reads a page; a page
	// of a duty listing —
	// `compaction_due`, `publication_due`, `expunge_due`, `changes_due`,
	// `checks_due` — and of an `expunge_tips` read is MaxSnapshotItems and
	// names none.
	Limit int                `json:"limit,omitempty"`
	Fresh statelog.Freshness `json:"fresh"`
}

// ReadAnswer is a [ReadQuery]'s answer: Changes is a `changes` listing's page;
// Change a `change` read's detail, on the first page of its revisions alone;
// Revisions a page of that change's revisions, newest first, on every page of
// them; Reviewers, Comments, Reviews and Findings a page of the part of its
// conversation a `change` read names ([ReadQuery.Part]) — Reviews a `reviews`
// listing's page too, each naming its change; and Routed a `change_routing`
// read's page.
type ReadAnswer struct {
	Repos []RepoSummary `json:"repos,omitempty"`
	Repo  *RepoDetail   `json:"repo,omitempty"`
	// Pushes is a page of a repository's history, newest first, or of one
	// coding run's pushes, oldest first — each repository the run was given
	// in its query's order, and each one's pushes in log order.
	Pushes     []PushRow              `json:"pushes,omitempty"`
	Refs       []RefRow               `json:"refs,omitempty"`
	Changes    []ChangeSummary        `json:"changes,omitempty"`
	Change     *ChangeDetail          `json:"change,omitempty"`
	Revisions  []RevisionRow          `json:"revisions,omitempty"`
	Reviewers  []ReviewerRow          `json:"reviewers,omitempty"`
	Comments   []CommentRow           `json:"comments,omitempty"`
	Reviews    []ReviewRow            `json:"reviews,omitempty"`
	Findings   []FindingRow           `json:"findings,omitempty"`
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
	// DueChanges a page of one repository's due changes, by source branch,
	// beside PathOwners, its path owners as its settings carry them, read in
	// the same snapshot — what its home matches the touched paths against —
	// and, on its first page, Rechecks: the changes whose merge is due again,
	// the oldest recorded first ([ChangesDueQuery.Rechecks]). A `checks_due`
	// read answers in the same two fields — its repositories in DueRepos, one
	// repository's heads owing their checks work in DueChanges, each with what
	// it owes ([ChangeDue.Checks]), with no owners and no rechecks — and, on a
	// repository's first page, DueTargets: its target branches due a reading
	// of their checks file again, and DueTips: its protected targets owing
	// tip work ([TipDue], tipwork.go).
	DueRepos []string `json:"due_repos,omitempty"`
	// Indexed is an `indexed` listing's page of repositories, by name.
	Indexed    []IndexedRepo    `json:"indexed,omitempty"`
	DueChanges []ChangeDue      `json:"due_changes,omitempty"`
	DueTargets []CheckTargetDue `json:"due_targets,omitempty"`
	DueTips    []TipDue         `json:"due_tips,omitempty"`
	Rechecks   []ChangeDue      `json:"rechecks,omitempty"`
	PathOwners []PathOwner      `json:"path_owners,omitempty"`
	// Checks is a page of a `checks` read — and a `check_box` read's one
	// attempt as its row stands, or none where the rows hold no such attempt
	// — and Check a `check` read's one attempt whole. A `checks_running`
	// read of one repository answers its running attempts in Checks, and of
	// none the repositories holding one in DueRepos.
	Checks []CheckRow   `json:"checks,omitempty"`
	Check  *CheckDetail `json:"check,omitempty"`
	// Runnable is a page of a `checks_queued` read: the attempts waiting for
	// a runner, oldest queued first.
	Runnable []RunnableCheck `json:"runnable,omitempty"`
	// Tips is a page of an `expunge_tips` read: object ids, each once on the
	// page, in the order of the drops that name them — an object a later
	// page names again is one more drop left, which its reader keeps once.
	Tips []string `json:"tips,omitempty"`
	// BranchChanges is a `branch_changes` read's answer: each branch's live
	// change, in the query's order, a branch heading none left out.
	BranchChanges []BranchChange `json:"branch_changes,omitempty"`
	// Queue is a page of a `queue` or a `queue_due` read's live entries, in
	// queue order; Trains a `queue` read's candidates of the trains testing
	// the targets its page reaches; Landings a page of a `landings` read; and
	// Green the target's green row a `landings` read starts above, or a
	// `green` read's rows.
	Queue    []QueueRow     `json:"queue,omitempty"`
	Trains   []CandidateRow `json:"trains,omitempty"`
	Landings []LandingRow   `json:"landings,omitempty"`
	Green    []GreenRow     `json:"green,omitempty"`
	// Commit is a `commit` read's changes of its commit, beside Repo.
	Commit *CommitChanges `json:"commit,omitempty"`
	// Inbox is an `inbox` read's page: the person's notices and their marks.
	Inbox    *InboxPage         `json:"inbox,omitempty"`
	Next     string             `json:"next,omitempty"`
	Position statelog.Position  `json:"position"`
	Level    statelog.ReadLevel `json:"level"`
}

// IndexedRepo is one repository an `indexed` read lists: its id, which its
// git order and its copy are keyed by, and its name, which the listing's
// cursor is.
type IndexedRepo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ExpungeCandidate is one repository an `expunge_due` read lists: the expunge
// open on it and the purge requested of it, whichever it holds — what its
// maintainer finishes or serves first — when it was last expunged, its live
// packs, bytes and objects — which a cycle's inputs are cut from, and the last
// of which places the repository's maintenance on the node that compacts and
// publishes it too, by its maintenance's charge (internal/code/compact) — and
// the version its row was read at.
type ExpungeCandidate struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Open is the expunge begun and not yet committed — its id — and ""
	// for none: what the maintainer finishes, from the rows, before it
	// begins another.
	Open string `json:"open,omitempty"`
	// Request is the purge requested and not yet served — its id — and
	// Requested the position its record landed at: a cycle serving it
	// rewrites the live packs whose content dates from before that position
	// ([PackRow.Content]), whenever their rows were added. "" and zero for
	// none.
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

// ImportCandidate is one repository an `imports_due` read lists: a repository
// being imported, its source — a URL and a ${VAR} reference, never a secret —
// how many of its chunks have landed (the next is Seq+1) and the version its
// row was read at.
type ImportCandidate struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Source  ImportSource      `json:"source"`
	Seq     int64             `json:"seq"`
	Version statelog.Position `json:"version"`
}

// MirrorCandidate is one repository a `mirrors_due` read lists: its pull
// mirror's source (From) or its push mirror's target (To) — a repository
// with both is listed once in each arm, each naming its own — and, for a pull
// mirror, when its last sync landed, zero for never.
type MirrorCandidate struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	From     *MirrorSource `json:"from,omitempty"`
	To       *MirrorTarget `json:"to,omitempty"`
	SyncedAt time.Time     `json:"synced_at,omitzero"`
}

// ExpungeTipsQuery is an `expunge_tips` read's repository, by id, and the
// instant from which the drops it lists were left: every code_ref_drops row
// of the repository whose record's broker instant is at or after Since. An
// expunge keeping a grace asks from its scan's instant less the grace; one
// serving a purge, which keeps nothing a drop left, from its scan's instant.
type ExpungeTipsQuery struct {
	RepoID string    `json:"repo_id"`
	Since  time.Time `json:"since"`
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
// and objects beside the published ones, the last push and the last
// publication with how long its build took, which paces the next — and the
// version its row was read at.
//
// THEIR DIFFERENCE NEVER FALLS SHORT OF THE PACKS NO LAYER COVERS, AND MAY
// EXCEED THEM: PublishedPacks and PublishedObjects are the live packs a layer
// covered, and their objects, as the last publication recounted them, and a
// push since moves only the live counts — so a pack pushed since then that is
// byte-identical to a published one (its git checksum one a layer names)
// counts in the difference until the next publication recounts it. A
// candidate can therefore have nothing to publish, which only a plan over its
// packs can tell: a due rule that reads these counts alone would sync and plan
// it again at every poll.
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

// ExpungeProposal is what an expunge's asker hands the repository's committer
// to decide ([Committer.ProposeExpunge]): EXACTLY ONE of a person's request, a
// maintainer's begin and its commit, each decided alone as a record of its
// own op. The set field's own id is the record's operation id — a request's
// RequestID, a begin's ExpungeID, a commit's CommitID — and a begin's or a
// commit's Seen its publish's session floor, so it carries nothing beside it.
type ExpungeProposal struct {
	Request *ExpungeRequest `json:"request,omitempty"`
	Begin   *ExpungeBegin   `json:"begin,omitempty"`
	Commit  *ExpungeCommit  `json:"commit,omitempty"`
}

// ExpungeResult is what became of one expunge proposal.
type ExpungeResult struct {
	// Outcome is applied, pending or unknown for a proposal whose record was
	// appended, and empty for one refused with none. A begin's and a
	// commit's applied is answered only from the rows of the packs the record
	// added, never from the decision.
	Outcome statelog.Outcome `json:"outcome,omitempty"`
	// Refusal is why it was refused with no record — stale, repo_deleted,
	// repo_importing, upload_expired, pack_too_large, busy, not_applied or
	// unavailable — and Detail its one sentence.
	Refusal Refusal `json:"refusal,omitempty"`
	Detail  string  `json:"detail,omitempty"`
	// Position is where its record landed — for one asked again under its
	// own operation id, the record the ledger found — and, for a not_applied
	// or repo_deleted refusal the log made after the append, where that
	// record lies applying nowhere.
	Position statelog.Position `json:"position,omitzero"`
	// Request is the purge a request's answer is about: its own id, the
	// repository's open request from its record on — until a later request
	// widens it, or a commit serves it. Empty for a begin and a commit.
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
	// appended, and empty for one refused with none. A chunk's and a sync's
	// applied is answered only from the rows the record applied — its push
	// row — never from the decision.
	Outcome statelog.Outcome `json:"outcome,omitempty"`
	// Refusal is why it was refused with no record — stale (a sync of a
	// repository that follows no source among its causes), repo_deleted,
	// repo_importing (a sync of a mirror not yet imported), repo_archived,
	// expunging, ref_conflict, repo_refs, repo_size, upload_expired,
	// pack_too_large, busy, not_applied or unavailable — and Detail its one
	// sentence.
	Refusal Refusal `json:"refusal,omitempty"`
	Detail  string  `json:"detail,omitempty"`
	// Position is where its record landed and, for a not_applied or
	// repo_deleted refusal the log made after the append, where that record
	// lies applying nowhere.
	Position statelog.Position `json:"position,omitzero"`
}

// ChangeProposal is one write of a change's lifecycle as its asker hands it to
// the repository's committer ([Committer.ProposeChange]): EXACTLY ONE of the
// six records it may be — an open, an edit, a draft's state, a retarget, a
// close and a reopen, each its op's payload ([ChangeOps]) — decided alone as a
// record of its own, beside who asks, the floor they read at and the version
// they formed it against. Its OpID is the record's operation id, its Seen the
// publish's session floor. What no asker sends — who wrote it, and an open's
// and a reopen's head — the committer stamps into the record from its asker
// and its snapshot.
type ChangeProposal struct {
	// OpID is the record's operation id, minted once by whoever asks — an
	// operation id this engine accepts (statelog.CheckCallerOpID) — so the
	// write asked again under it, of the repository's runner-up after a
	// committer went silent or by an asker whose answer was lost, is the same
	// write, collapsed by the operation ledger; an open's change id is
	// derived from it ([OpenChangeID]).
	OpID string `json:"op_id"`
	// Asker is who asks: a person through a credential bound to their human
	// seat, a seat by its handle, or a token bound to nobody, which the
	// decide refuses every write. A SEAT'S IS JUDGED FROM THE CHART of the
	// deciding node's epoch ([CommitterOptions.SeatRules]), so a node that
	// holds none, or one confirmed behind the company's configuration,
	// decides none of a seat's — the estate declares the operation to judge
	// a grant where its asker is a seat. It names no run: a change keeps the
	// turn and the run that opened it in its own fields. A LANDING NAMES
	// NONE: it is the repository's home git node's own write, which no
	// principal asks and no chart judges.
	Asker Principal `json:"asker"`
	// Seen is the code log's position the asker read the repository and the
	// change at — zero for one that read none. The committer DECIDES AT OR
	// PAST IT, or refuses `behind` about itself, so the runner-up decides:
	// from below it the decide would refuse a write formed against what the
	// asker was shown — an edit stale, an open's branch missing.
	Seen statelog.Position `json:"seen,omitzero"`
	// ExpectedVersion is the change's version the write was formed against
	// (code_changes.version, its records counted from 1 at its open): every
	// write but an open and a landing names one, and one the change is no
	// longer at is refused stale — another write, an edit or a push that
	// moved its head, landed first. An open names none: the change does not
	// exist yet. A landing names none either: it is guarded by what its home
	// read — the change's head and target and the target's tip — and an edit
	// of its title between the read and the landing changes none of those.
	ExpectedVersion int64 `json:"expected_version,omitempty"`
	// Ceiling is whom a SEAT'S write is made for inside a turn somebody woke
	// ([WriteCeiling]), nil for one made for its seat alone or for a person;
	// never a landing's, which no principal asks. The committer judges it
	// once the seat's own rules are met (decide_change.go's head): a
	// colleague held at the least access the write needs, nobody refused —
	// but for the seat's own work, which whoever woke it steers.
	//
	// A proposal is no record — the record is what the committer stamps from
	// it — so this field takes no versionedFields row; it crosses the
	// estate's wire to whichever data node decides it under the code domain's
	// stated condition — no build carrying the domain has run without it,
	// none of the domain being on main — so every node asked it reads it.
	Ceiling *WriteCeiling `json:"ceiling,omitempty"`

	Open     *ChangeOpen     `json:"open,omitempty"`
	Edit     *ChangeEdit     `json:"edit,omitempty"`
	Draft    *ChangeDraft    `json:"draft,omitempty"`
	Retarget *ChangeRetarget `json:"retarget,omitempty"`
	Close    *ChangeClose    `json:"close,omitempty"`
	Reopen   *ChangeReopen   `json:"reopen,omitempty"`
	// Landed is a landing the repository's home git node asks, with NO
	// Asker: the engine's own write ([ChangeLanded]).
	Landed *ChangeLanded `json:"landed,omitempty"`
}

// ChangeResult is what became of one change proposal.
type ChangeResult struct {
	// Outcome is applied, pending or unknown for a write whose record was
	// appended — or, answered from the change's row with no record, applied
	// and Unchanged — and empty for one refused. Applied is answered only
	// from the change's row the record wrote, never from the decision.
	Outcome statelog.Outcome `json:"outcome,omitempty"`
	// ChangeID is the change the write is about — an open's, its derived id.
	ChangeID string `json:"change_id,omitempty"`
	// Number is the change's number on its repository (web!42), read from its
	// row: an open's is the repository's next_change at the apply that opened
	// it, which no proposal can name before its record lands.
	Number int64 `json:"number,omitempty"`
	// Version is the change's version once the write applied — what the
	// asker's next write names as its expectation — and 0 where a later
	// record has moved the change since this one: the change is then read
	// again before it is written, rather than answered with a version that
	// carries writes its asker never saw.
	Version int64 `json:"version,omitempty"`
	// Position is where the write's record landed — for one asked again under
	// its own operation id, the record the ledger found; for an Unchanged
	// answer, the change's row's own — and, for a not_applied or
	// repo_deleted refusal the log made after the append, where that record
	// lies applying nowhere.
	Position statelog.Position `json:"position,omitzero"`
	// Unchanged is a write that asked for what the change already says — an
	// edit to its own values, a draft's state it is in, its own target, a
	// close of a closed change and a reopen of a live one — answered from its
	// row with no record, at its version.
	Unchanged bool `json:"unchanged,omitempty"`
	// Refusal is why it was refused with no record, and Detail its one
	// sentence.
	Refusal ChangeRefusal `json:"refusal,omitempty"`
	Detail  string        `json:"detail,omitempty"`
}

// ConversationWrite is one write of a change's conversation as its asker hands
// it to the repository's committing data node ([ChangeWriter.Converse]):
// EXACTLY ONE of the records it may be — an ask for review, a comment, a
// thread's resolution, a review, an answer to findings and a revision's facts,
// each its op's payload ([ConversationOps]) — decided as a record of its own
// on the change's subject, beside who asks and the floor they read at. Its
// OpID is the record's operation id, its Seen the publish's session floor.
// What no asker sends — who wrote it, a reviewer's kind, a comment's id and
// thread, a review's id and its findings' — the writer stamps into the record
// from its asker, the chart and its snapshot.
type ConversationWrite struct {
	// OpID is the record's operation id, minted once by whoever asks — an
	// operation id this engine accepts (statelog.CheckCallerOpID) — so the
	// write asked again under it, of the repository's runner-up after a
	// writer went silent or by an asker whose answer was lost, is the same
	// write, collapsed by the operation ledger; a comment's id is derived
	// from it ([CommentID]).
	OpID string `json:"op_id"`
	// Asker is who asks: a person through a credential bound to their human
	// seat, or a seat by its handle; a token bound to nobody writes nothing,
	// and a revision's facts, the repository's home git node's own, name
	// none. A SEAT'S IS JUDGED FROM THE CHART of the deciding node's epoch
	// ([ChangeWriterOptions.SeatRules]), and so is every reviewer an ask
	// names, whoever asks ([ConversationWrite.JudgesChart]): a node that holds
	// no chart, or one confirmed behind the company's configuration, decides
	// none of those. It names no run: a comment keeps the turn that said it
	// in its own field.
	Asker Principal `json:"asker"`
	// Seen is the code log's position the asker read the change at — zero
	// for one that read none. The writer DECIDES AT OR PAST IT, or refuses
	// `behind` about itself, so the runner-up decides: from below it the
	// decide would refuse a write formed against what the asker was shown —
	// a change it was shown open as missing, a revision it read as unknown.
	Seen statelog.Position `json:"seen,omitzero"`
	// Ceiling is whom a SEAT'S write is made for inside a turn somebody woke
	// ([WriteCeiling]), nil for one made for its seat alone or for a person;
	// never a revision's facts' or a mergeability's, which no principal asks.
	// The writer judges it beside the seat (decide_conversation.go's head): a
	// colleague held at the least access the write needs — review for a
	// review, read for every other — nobody refused, but for the seat's own
	// work, which whoever woke it steers.
	//
	// A write is no record — the record is what the writer stamps from it —
	// so this field takes no versionedFields row; it crosses the estate's
	// wire to whichever data node decides it under the code domain's stated
	// condition — no build carrying the domain has run without it, none of
	// the domain being on main — so every node asked it reads it.
	Ceiling *WriteCeiling `json:"ceiling,omitempty"`

	Reviewers      *ReviewerRequest `json:"reviewers,omitempty"`
	Comment        *Comment         `json:"comment,omitempty"`
	ThreadResolve  *ThreadResolve   `json:"thread_resolve,omitempty"`
	Review         *Review          `json:"review,omitempty"`
	FindingResolve *FindingResolve  `json:"finding_resolve,omitempty"`
	// Revise is a revision's facts, and Mergeability whether the change's head
	// revision merges into its target's tip, each of which the repository's
	// home git node asks with NO Asker: the engine's own writes ([Revise],
	// [Mergeability]).
	Revise       *Revise       `json:"revise,omitempty"`
	Mergeability *Mergeability `json:"mergeability,omitempty"`
}

// ConversationResult is what became of one write of a change's conversation.
type ConversationResult struct {
	// Outcome is applied, pending or unknown for a write whose record was
	// appended — or, answered from the rows with no record, applied and
	// Unchanged — and empty for one refused. Applied is answered only once
	// the change's conversation holds the record's rows, never from the
	// decision.
	Outcome statelog.Outcome `json:"outcome,omitempty"`
	// ID is what the write made that is named apart from its change — a
	// comment's id ([CommentID]), a review's ([ReviewID]) — and "" for a
	// write that made none; FindingIDs is a review's findings' ids, in the
	// order it raised them ([FindingID]).
	ID         string   `json:"id,omitempty"`
	FindingIDs []string `json:"finding_ids,omitempty"`
	// Position is where the write's record landed — for one asked again under
	// its own operation id, the record the ledger found; for an Unchanged
	// answer, the conversation's last record — and, for a not_applied or
	// repo_deleted refusal the log made after the append, where that record
	// lies applying nowhere.
	Position statelog.Position `json:"position,omitzero"`
	// Unchanged is a write that asked for what the conversation already
	// says — an ask of reviewers already asked and of withdrawals of none
	// who are, a thread's state it is in — answered from the rows with no
	// record.
	Unchanged bool `json:"unchanged,omitempty"`
	// Refusal is why it was refused with no record, and Detail its one
	// sentence.
	Refusal ConversationRefusal `json:"refusal,omitempty"`
	Detail  string              `json:"detail,omitempty"`
}

// ChecksRequest is one write of a repository's checks as its asker hands it to
// the repository's committing data node ([ChecksWriter.Publish]): EXACTLY ONE
// of the records it may be — an attempt queued (or recorded skipped), a claim
// of one or its holder's naming of the box it made, and an attempt's ending —
// decided as a record of its own on the repository's checks subject, beside
// the operation id it is asked under and the floor its asker read at. The
// engine's own: the home queues and requeues, a runner claims and ends, and
// none of those names a principal.
//
// A CHECKS SWEEP IS NO REQUEST: the code-retention duty's [Store.ChecksSweep]
// publishes it on the node that holds the duty, deciding only "rows past the
// history are held", which no other write of the subject races — so the
// writer that takes a repository's checks one at a time has nothing to order
// it by.
//
// THE CODE DOMAIN'S RECORD FORMAT IS UNTOUCHED: an estate request's type and
// no record's — what it carries rides the code log as the payloads it names,
// whose rows are the checks subject's ([KindChecks]) — so it takes no
// versionedFields row.
type ChecksRequest struct {
	// OpID is the record's operation id, minted once by whoever asks — an
	// operation id this engine accepts (statelog.CheckCallerOpID) — so the
	// write asked again under it, of the repository's runner-up after a
	// writer went silent or by an asker whose answer was lost, is the same
	// write, collapsed by the operation ledger.
	OpID string `json:"op_id"`
	// Seen is the code log's position the asker read the attempt at — a
	// runner's own claim, for the record naming its box; the home's read, for
	// a requeue — and zero for one that read none. The writer DECIDES AT OR
	// PAST IT, or refuses `behind` about itself, so the runner-up decides:
	// from below it the decide would answer a claim it was shown as no
	// attempt at all.
	Seen statelog.Position `json:"seen,omitzero"`

	Queued   *CheckQueued   `json:"queued,omitempty"`
	Started  *CheckStarted  `json:"started,omitempty"`
	Finished *CheckFinished `json:"finished,omitempty"`
	// Dispatched is the home's record of a target's checks file read at its
	// tip and the heads it dispatched under it (checksdispatch.go). An
	// estate request's field, no record's: the record it asks for takes its
	// op's versionedFields row, under the subject's stated condition.
	Dispatched *CheckDispatched `json:"dispatched,omitempty"`
}

// ChecksResult is what became of one write of a repository's checks.
type ChecksResult struct {
	// Outcome is applied, pending or unknown for a write whose record was
	// appended — or, answered from the rows with no record, applied and
	// Unchanged — and empty for one refused. Applied is answered only once
	// the attempt's row holds what the record carried, never from the
	// decision.
	Outcome statelog.Outcome `json:"outcome,omitempty"`
	// Unchanged is a write that asked for what the attempt's row already
	// says — an attempt queued that is, a claim its holder sends again, an
	// ending the row's own ending is kept over — answered from the row with
	// no record.
	Unchanged bool `json:"unchanged,omitempty"`
	// Refusal is why it was refused with no record, and Detail its one
	// sentence.
	Refusal CheckRefusal `json:"refusal,omitempty"`
	Detail  string       `json:"detail,omitempty"`
	// Holder is the runner that holds the attempt as the rows say — on a
	// claim refused `claimed`, the one that does; on a claim applied, the one
	// the join kept, which is the claim's own runner unless a claim another
	// writer published landed with a lower (StartedAt, Runner) — and "" for
	// an attempt no runner holds. A runner whose claim answered another
	// holder starts no box.
	Holder string `json:"holder,omitempty"`
	// Row is the attempt as the rows hold it once the write applied, or as
	// the refusal or the unchanged answer read it — nil where the repository
	// holds none.
	Row *CheckRow `json:"row,omitempty"`
	// Position is where the write's record landed — for one asked again under
	// its own operation id, the record the ledger found; for an Unchanged
	// answer, the row's last record — and, for a not_applied or repo_deleted
	// refusal the log made after the append, where that record lies applying
	// nowhere.
	Position statelog.Position `json:"position,omitzero"`
}

// CheckRow is one attempt of one check of one commit as its row holds it
// (code_checks): its key and state, and what its queue, its run and its
// ending say — every column but the wide ones a listing leaves to a read of
// the one attempt ([CheckDetail]): its definition, its tail and its failures.
type CheckRow struct {
	CheckKey
	State   CheckState   `json:"state"`
	Purpose CheckPurpose `json:"purpose,omitempty"`
	// ChangeID and Revision are a head check's change and revision;
	// CandidateID and EntryID a candidate's queue entry.
	ChangeID    string `json:"change_id,omitempty"`
	Revision    int    `json:"revision,omitempty"`
	CandidateID string `json:"candidate_id,omitempty"`
	EntryID     string `json:"entry_id,omitempty"`
	// Base is what its diff is read against, and DefsOID the checks file its
	// definition came from — the file of the branch it is checked for, its
	// key's Target.
	Base    string `json:"base,omitempty"`
	DefsOID string `json:"defs_oid,omitempty"`
	// Required reports a check the target's protection requires; RunIn and
	// TimeoutSeconds are its definition's.
	Required       bool      `json:"required,omitempty"`
	RunIn          CheckCell `json:"run_in,omitempty"`
	TimeoutSeconds int       `json:"timeout_seconds,omitempty"`
	// Reason is why its ending ended it, "" before one.
	Reason CheckReason `json:"reason,omitempty"`
	// Runner, Placement, Box and Borrowed are its claim's; Reporter is the
	// human seat of the person whose CI reported its ending.
	Runner    string         `json:"runner,omitempty"`
	Placement CheckPlacement `json:"placement,omitempty"`
	Box       string         `json:"box,omitempty"`
	Borrowed  bool           `json:"borrowed,omitempty"`
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

// CheckDetail is one attempt read whole ([ReadCheck]): its row, the
// definition it ran under, its output's tail, the failures its ending named,
// and the log a failed run kept — each nil or empty where the attempt carries
// none.
type CheckDetail struct {
	CheckRow
	Def      *CheckDef      `json:"def,omitempty"`
	Tail     string         `json:"tail,omitempty"`
	Failures []CheckFailure `json:"failures,omitempty"`
	Log      *CheckLog      `json:"log,omitempty"`
}

// CheckQuery narrows a `checks` read to one change's attempts or one
// commit's, or names the one attempt a `check` read is of — each on the
// repository the read names ([ReadQuery.Repo]).
type CheckQuery struct {
	// Change is a change's number on the repository: the attempts that judge
	// it — its head's, checked for its target — or, beside OID, that commit's
	// checked for its target. A `checks` read's alone.
	Change int64 `json:"change,omitempty"`
	// OID is a commit's whole id: its attempts — checked for every target,
	// or for the target of the change Change names — and the commit a
	// `check` read's attempt is of.
	OID string `json:"oid,omitempty"`
	// Target, Name and Attempt are the one attempt a `check` read is of —
	// the branch it was checked for, the check and the attempt — REQUIRED
	// there. A `checks` read refuses a Target and an Attempt, and takes a
	// Name as a NARROWING: the attempts of the check of that name alone.
	Target  string `json:"target,omitempty"`
	Name    string `json:"name,omitempty"`
	Attempt int    `json:"attempt,omitempty"`
	// State narrows a `checks` read to the attempts in one state; refused on
	// a `check` read, which names its one attempt whatever its state.
	//
	// A NARROWING TESTED ON THE ROWS A PAGE READ, as Name's is, and never a
	// seek: every shape of the listing walks the one index its own order
	// leads (code_checks' key, code_checks_finished_idx,
	// code_checks_recent_idx), and an index per state or per name would be
	// one more write on every check's every transition for a filter a person
	// applies to a page. So a narrowed page is the rows it READ, never the
	// rows it kept, and may be short — or empty with a cursor — as a window
	// a reader reads little of is.
	//
	// AN ESTATE REQUEST'S FIELD, NOT A RECORD'S, so it takes no
	// versionedFields row — and neither does Name's use as a narrowing. AS A
	// STATED CONDITION (M3-O84 §2 item 5): no build that carries the code
	// domain has run without them — none of the domain is on main, so no
	// image and no release serves a code read. A build that predated them
	// would refuse a listing naming a Name and decode and ignore a State,
	// answering the wider listing; so were such a build ever to run first,
	// a narrowed listing would move to a kind of its own, which such a build
	// refuses as a kind it does not know, rather than ride a field it
	// ignores.
	State CheckState `json:"state,omitempty"`
	// Since bounds a `checks` read of EVERY repository — the window, which
	// names no repository — to the attempts that ended at or after it; zero
	// is the whole of what the rows keep ([CheckHistory]). Refused beside a
	// repository, whose listing is its own attempts newest ended first.
	Since time.Time `json:"since,omitzero"`
}

// keeps reports whether row is one a `checks` listing narrowed by c keeps:
// of the check c names, where it names one, and in the state it names, where
// it names one.
func (c CheckQuery) keeps(row CheckRow) bool {
	return (c.Name == "" || row.Name == c.Name) && (c.State == "" || row.State == c.State)
}

// ChecksDueQuery is a `checks_due` read's question, REQUIRED as a
// `changes_due` read's is: the instant a target's reading of its checks file
// is due again by, and — for ONE repository's due check work — the repository
// by its id, which its home was nudged about or found listed.
type ChecksDueQuery struct {
	RepoID string `json:"repo_id,omitempty"`
	// RefreshBefore is the home's clock less its refresh: a target whose
	// branch moved past the tip its checks file was read at is due a reading
	// again once that reading was recorded at or before it — so a target
	// pushed to on every landing is read at the home's pace, never at its
	// own.
	RefreshBefore time.Time `json:"refresh_before"`
}

// ChecksRunningQuery is a `checks_running` read's question: the repository
// whose running attempts it reads, by id in its canonical spelling, or "" for
// the repositories holding one — the listing a repository's home pages for
// the attempts it sweeps.
type ChecksRunningQuery struct {
	RepoID string `json:"repo_id,omitempty"`
}

// RunnableCheck is one attempt a `checks_queued` read lists: its row, the
// name of its repository — the name its box is given the repository under,
// its clone's directory and its token's — and the definition it was queued
// under, which is what its box runs, whatever its target's file says now.
type RunnableCheck struct {
	CheckRow
	Repo string    `json:"repo"`
	Def  *CheckDef `json:"def,omitempty"`
}

// CheckTargetDue is a target branch whose checks file its home owes a reading
// at the branch's tip ([ReadAnswer.DueTargets]): the branch, its tip and the
// position of the record that moved it there.
type CheckTargetDue struct {
	TargetRef   string            `json:"target_ref"`
	Tip         string            `json:"tip"`
	TipPosition statelog.Position `json:"tip_position"`
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
	// PathOwners are who owns which of its paths, as its settings carry them
	// ([RepoSettings.PathOwners]): on the detail alone, never on the
	// [RepoRef] beside it, which every push, fetch and listing reads and
	// which no path owner changes a thing about — and only to a read that
	// asks for them ([ReadQuery.PathOwners]), since every resolution of a
	// repository reads this detail for its row. At their bound
	// ([MaxPathOwnersBytes]) the detail still fits the room a page beside it
	// leaves the estate's reply ([MaxPageBytes]; the limits test holds it).
	PathOwners []PathOwner `json:"path_owners,omitempty"`
	// Gates are the merge gates of its rules that carry one, each named by
	// its rule's pattern and in pattern order ([RuleGate]) — what its row's
	// rules ([RepoRef.Protections]) never carry — and, as the path owners
	// are, only to a read that asks ([ReadQuery.Gates]): every resolution of
	// a repository reads this detail for its row, and at their bound a
	// repository's gates are half a mebibyte of JSON. A read that asked and
	// finds none answers none.
	Gates []RuleGate `json:"gates,omitempty"`
}

// ChangeRow is a change as its row holds it (code_changes): every column, as
// every read of one serves it — what a listing renders ([ChangeSummary]) and
// a read of one change details ([ChangeDetail]).
type ChangeRow struct {
	ID     string `json:"id"`
	RepoID string `json:"repo_id"`
	// Number is the change's on its repository — web!42's 42 — the
	// repository's next_change at the apply that opened it.
	Number int64       `json:"number"`
	Title  string      `json:"title"`
	State  ChangeState `json:"state"`
	// SourceRef is the branch whose head the change proposes, and TargetRef
	// the branch it proposes it into, each in full (refs/heads/feature).
	SourceRef string `json:"source_ref"`
	TargetRef string `json:"target_ref"`
	// Head is the source branch's head at the change's latest revision, and
	// Revision that revision's number: how many revisions it has.
	Head     string `json:"head"`
	Revision int64  `json:"revision"`
	// Owner is who opened it — a seat by its handle, or a person by the
	// human seat they hold — and OwnerKind which; OwnerOperatorID is the
	// credential a person opened it with, the change's own attribution as
	// every record a write leaves names it, and "" for a seat's.
	Owner           string    `json:"owner"`
	OwnerKind       PartyKind `json:"owner_kind"`
	OwnerOperatorID string    `json:"owner_operator_id,omitempty"`
	// WorkItem is the work item the change carries, nil for none.
	WorkItem *types.WorkItem `json:"work_item,omitempty"`
	// CreatedTurn and CreatedRun are the turn and the sandbox run that opened
	// it, each "" where there was none — a person's open has neither.
	CreatedTurn string `json:"created_turn,omitempty"`
	CreatedRun  string `json:"created_run,omitempty"`
	// OpenedAt is the broker instant of the record that opened it, and
	// OpenedPosition where on the code log that record landed: what a listing
	// across repositories orders its changes by.
	OpenedAt       time.Time         `json:"opened_at"`
	OpenedPosition statelog.Position `json:"opened_position"`
	// ClosedAt is set exactly while the change is closed, with CloseReason —
	// SourceDeleted for one a push closed by deleting its source — and
	// MergedAt exactly while it is merged, with how (MergedVia) and the commit
	// its target was merged at (MergeOID).
	ClosedAt    time.Time `json:"closed_at,omitzero"`
	CloseReason string    `json:"close_reason,omitempty"`
	MergedAt    time.Time `json:"merged_at,omitzero"`
	MergedVia   MergedVia `json:"merged_via,omitempty"`
	MergeOID    string    `json:"merge_oid,omitempty"`
	// MergedBy is who merged it through its gate — a merge record's
	// principal ([Merge]): a seat by its handle, or a person by the human
	// seat they hold — and MergedByKind which; MergedOperatorID the
	// credential a person merged with; and OverrideReason a person's reason
	// for merging past the gate. Each "" but on a change a merge record
	// merged: a push that merged a change names its pusher on its push row.
	MergedBy         string    `json:"merged_by,omitempty"`
	MergedByKind     PartyKind `json:"merged_by_kind,omitempty"`
	MergedOperatorID string    `json:"merged_operator_id,omitempty"`
	OverrideReason   string    `json:"override_reason,omitempty"`
	// BodyBytes is how long its description is, 0 for none: a listing says a
	// change has one without carrying it ([ChangeDetail.Body]).
	BodyBytes int64 `json:"body_bytes,omitempty"`
	// Version counts the records that changed the row — 1 at its open — and
	// is what a write of the change's lifecycle names as its expectation
	// ([ChangeProposal.ExpectedVersion]); Position is where the last of them
	// landed.
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
	// by name: read, review, write or maintain) — never none, since a change
	// of a repository the asker may not read is not listed at all.
	Access string `json:"access,omitempty"`
	// Readiness is whether the change may merge, judged in the read's one
	// transaction against its one chart ([ReadinessOf]) — every row of a
	// listing and one change's detail alike, never the answer its last
	// mergeability stored ([MergeabilityRow.Ready]) — and nil for a change
	// that is no longer live, which merges nothing.
	Readiness *Readiness `json:"readiness,omitempty"`
}

// ChangeDetail is one change in detail: its summary, its description and its
// authors. Its revisions are paged beside it ([ReadAnswer.Revisions]).
type ChangeDetail struct {
	ChangeSummary
	// Body is its description, "" for none.
	Body string `json:"body,omitempty"`
	// Authors is everyone who wrote it — whoever opened it, and everyone whose
	// push moved its head — by handle.
	Authors []ChangeAuthor `json:"authors"`
	// Conversation is what its conversation counts, nil for a change nobody
	// has discussed; its rows are read a part at a time ([ReadQuery.Part]).
	Conversation *ConversationRow `json:"conversation,omitempty"`
	// Mergeability is whether its latest merged revision merges into its
	// target, as its home recorded it — nil for a change its home has merged
	// no revision of yet. One of an earlier revision than the change's head,
	// or into another branch, is the home's to merge again.
	Mergeability *MergeabilityRow `json:"mergeability,omitempty"`
}

// ConversationRow is a change's conversation's counters
// (code_conversations): its comments and its reviews; its OPEN findings, which
// none of its authors has answered; its STANDING BLOCKERS — the blockers that
// hold it back whatever its authors answered, until their raiser's later
// review or a dismissal clears them (S40); how many reviewers' latest verdict
// asks for changes; and the version and position of the last record on the
// change's subject — the floor a read of its parts is asked at.
type ConversationRow struct {
	Comments         int64             `json:"comments"`
	Reviews          int64             `json:"reviews"`
	OpenFindings     int64             `json:"open_findings"`
	OpenBlockers     int64             `json:"open_blockers"`
	ChangesRequested int64             `json:"changes_requested"`
	Version          int64             `json:"version"`
	Position         statelog.Position `json:"position"`
}

// ReviewRow is one review of a change (code_reviews): the revision it judged,
// who reviewed — a seat, or a person with the credential they used — its
// verdict and summary, how many findings it raised, the turn and the model
// that reviewed, and its record's position and broker instant.
type ReviewRow struct {
	ID           string            `json:"id"`
	Revision     int64             `json:"revision"`
	Reviewer     string            `json:"reviewer"`
	ReviewerKind PartyKind         `json:"reviewer_kind"`
	OperatorID   string            `json:"operator_id,omitempty"`
	Verdict      Verdict           `json:"verdict"`
	Summary      string            `json:"summary,omitempty"`
	Findings     int64             `json:"findings"`
	TurnID       string            `json:"turn_id,omitempty"`
	Model        string            `json:"model,omitempty"`
	Position     statelog.Position `json:"position"`
	At           time.Time         `json:"at"`
	// Change is the change it judged, on a `reviews` listing's rows, which
	// span changes; nil on a `change` read's, which names its change.
	Change *ChangeName `json:"change,omitempty"`
}

// ChangeName is a change as a row about it names it where the row is read
// apart from the change — a review a `reviews` listing found, the change a
// `change_routing` page is of: its repository by name, its reference
// (web!42), its title and its state, as the read found its row.
type ChangeName struct {
	Repo  string      `json:"repo"`
	Ref   string      `json:"ref"`
	Title string      `json:"title"`
	State ChangeState `json:"state"`
}

// RoutedPage is a `change_routing` read's page (routedread.go): the change,
// and a page of the records about it that reached anybody, newest first,
// each with every recipient it reached — and what the read can say about
// whom its records reached where it lists none.
type RoutedPage struct {
	Change ChangeName `json:"change"`
	// OpenedAt is the broker instant of the record that opened the change:
	// the oldest record about it, which its Delivery is dated by.
	OpenedAt time.Time      `json:"opened_at"`
	Records  []RoutedRecord `json:"records"`
	// Delivery is what the read can say about whom the change's records
	// reached ([routing.DeliveryOf]): `reached` where a record is listed —
	// and on every later page, since its first listed one — and, where
	// none is, `nobody` for a change opened since RetainedFrom, every one of
	// whose notices would still be kept, and `swept` for one opened before
	// it, whose notices a sweep was entitled to take and the applier never
	// to write. Every record about a change carries its routing, so its
	// records always announced: `quiet` and `unknown` are never this read's.
	Delivery routing.Delivery `json:"delivery"`
	// RetainedFrom is the instant from which every notice is kept, a seat's
	// and a person's alike: the read's instant less [SeatNoticeHorizon], the
	// shorter of the two horizons, since a person's — the company's
	// inbox.retention_days — is never less than thirty days. A record older
	// than it lists only the persons it reached: a seat's notice of it went
	// with the turn detail it named.
	RetainedFrom time.Time `json:"retained_from"`
}

// RoutedRecord is one record about a change and whom it reached: its
// operation id (code_notifications.record_id), where it landed on the code
// log and its broker instant, what its notices kept of what it says — an
// open's description, a comment's body, a review's summary, nothing for a
// record that says nothing of its own — and every recipient its notices name,
// addressed first, a fallback last, each group by handle.
type RoutedRecord struct {
	RecordID   string            `json:"record_id"`
	Position   statelog.Position `json:"position"`
	At         time.Time         `json:"at"`
	Excerpt    string            `json:"excerpt,omitempty"`
	Recipients []RoutedRecipient `json:"recipients"`
}

// RoutedRecipient is one handle a record reached: a seat's, or the human seat
// a person holds ([PartyKind] says which, and so which horizon keeps its
// notice), the ONE reason it heard under ([Reasons]' precedence), whether that
// asks something of it, and — for a recipient reached only because nobody
// better was found — which substitute it was.
type RoutedRecipient struct {
	Handle       string    `json:"handle"`
	Kind         PartyKind `json:"kind"`
	Reason       Reason    `json:"reason"`
	Addressed    bool      `json:"addressed,omitempty"`
	Fallback     bool      `json:"fallback,omitempty"`
	FallbackRank int       `json:"fallback_rank,omitempty"`
}

// FindingRow is one finding on a change (code_findings and its text,
// code_finding_texts): the review that raised it and who — its raiser — the
// revision it was raised on, where it points — the path, the side of that
// revision's diff, its lines, and the commit and the blob they were read in —
// how much it weighs, what it is about and what it says, the lines it would
// put in their place where it suggests some, its STATE, and its last answer:
// the note, who gave it, the commit it names and the record that gave it.
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
	Title            string            `json:"title"`
	Body             string            `json:"body,omitempty"`
	Suggestion       *string           `json:"suggestion,omitempty"`
	State            FindingState      `json:"state"`
	ResolutionNote   string            `json:"resolution_note,omitempty"`
	ResolvedBy       string            `json:"resolved_by,omitempty"`
	ResolvedByKind   PartyKind         `json:"resolved_by_kind,omitempty"`
	ResolvedCommit   string            `json:"resolved_commit,omitempty"`
	ResolvedPosition statelog.Position `json:"resolved_position,omitzero"`
	Position         statelog.Position `json:"position"`
}

// ReviewerRow is one reviewer a change has asked (code_reviewers): a seat by
// its handle or a person by the human seat they hold, why they were asked,
// whether the ask stands, who asked with what note, and the record that last
// moved it.
type ReviewerRow struct {
	Reviewer        string            `json:"reviewer"`
	Kind            PartyKind         `json:"kind"`
	Reason          ReviewerReason    `json:"reason"`
	State           ReviewerState     `json:"state"`
	RequestedBy     string            `json:"requested_by"`
	RequestedByKind PartyKind         `json:"requested_by_kind"`
	Note            string            `json:"note,omitempty"`
	Position        statelog.Position `json:"position"`
}

// CommentRow is one comment on a change (code_comments): its thread — its own
// id for a thread's first comment, a finding's for a reply to one — the root
// or the finding it replies to, who said it — a seat, or a person with the
// credential they used — the line a thread's first comment points at, whether
// its thread is resolved and by whom, the turn that said it, what it says, and
// its record's position and broker instant.
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
	Body             string            `json:"body"`
	Position         statelog.Position `json:"position"`
	At               time.Time         `json:"at"`
}

// ChangeAuthor is one author of a change (code_change_authors): a seat by its
// handle or a person by the human seat they hold, from the revision they
// first wrote. An author's approval of their own change counts for nothing.
type ChangeAuthor struct {
	Author        string            `json:"author"`
	Kind          PartyKind         `json:"kind"`
	FirstRevision int64             `json:"first_revision"`
	Position      statelog.Position `json:"position"`
}

// RevisionRow is one revision of a change (code_revisions): a move of its
// source branch's head while it was live — the new head, who moved it and
// the push that did, empty for a revision an open or a reopen made — where
// its record landed and the record's broker instant, and the facts the
// repository's home git node recorded of it, nil for a revision it has
// recorded none of yet.
type RevisionRow struct {
	N          int64             `json:"n"`
	Head       string            `json:"head"`
	Pusher     string            `json:"pusher"`
	PusherKind PartyKind         `json:"pusher_kind"`
	PushID     string            `json:"push_id,omitempty"`
	Position   statelog.Position `json:"position"`
	At         time.Time         `json:"at"`
	Facts      *RevisionFactsRow `json:"facts,omitempty"`
}

// RevisionFactsRow is a revision's facts as the change's home recorded them
// (code_revision_facts, [Revise]): the target branch and the tip they were
// read against with the position that moved it there, the base the change is
// measured from, the trees of the base and of the head, the id of the change
// it makes — or that none was taken — the files it touches or that it touches
// all, the classes of their changes, the git node that recorded them, the
// path-owner match — the digest of the owners matched against, those matched
// and those asked no review of — and the record that wrote them. A revision
// whose head its target holds has none: its change is merged, and its home
// lands it. Two revisions whose rows name the same trees hold the same code
// ([ChangeClassNoCodeChange]).
type RevisionFactsRow struct {
	TargetRef       string            `json:"target_ref"`
	TargetOID       string            `json:"target_oid"`
	TargetPosition  statelog.Position `json:"target_position"`
	BaseOID         string            `json:"base_oid,omitempty"`
	BaseTree        string            `json:"base_tree"`
	HeadTree        string            `json:"head_tree"`
	PatchID         string            `json:"patch_id,omitempty"`
	PatchIDOmitted  bool              `json:"patch_id_omitted,omitempty"`
	Touched         []string          `json:"touched"`
	TouchesAll      bool              `json:"touches_all,omitempty"`
	Classes         RevisionClasses   `json:"classes,omitzero"`
	Node            string            `json:"node"`
	OwnersDigest    string            `json:"owners_digest"`
	RequestedOwners []Owner           `json:"requested_owners,omitempty"`
	SkippedOwners   []SkippedOwner    `json:"skipped_owners,omitempty"`
	Position        statelog.Position `json:"position"`
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
// fixed options alone. A PATH is data, never an option: it reaches git on a
// batch command's standard input, or — for a log and a blame, which read no
// path there — as the one argument after `--`, a log's as a literal pathspec
// (internal/code/query's rule).
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

// MaxTouchedPaths is the most paths a revision's facts list as the files its
// change touches ([RevisionFactsAnswer.Touched]). Past it the facts say the
// revision TOUCHES ALL, with no list: the safe side for everything that reads
// one — an interdiff then shows every file its two heads differ in, and
// anything matched against the paths matches — where a list cut at its bound
// would leave the files past the cut out of all of it.
//
// A THOUSAND: far more than a change a reviewer reads file by file touches,
// so only the changes nobody reviews that way — a formatter's run, a vendored
// tree, a move of a whole directory — touch all; and few enough that matching
// rules against the list on every revision stays cheap. The list's bytes are
// bounded beside it ([MaxTouchedBytes]), whichever is reached first.
const MaxTouchedPaths = 1000

// MaxTouchedBytes is the most the touched list may take AS JSON — its
// brackets, every path quoted and escaped as encoding/json writes it, the
// commas between — in a revision's facts and in whatever carries them on:
// past it, as past [MaxTouchedPaths], the revision touches all.
//
// 48 KiB, SO A FACTS ANSWER IS ALWAYS ONE PAGE: an answer is at most
// [MaxGitAnswerBytes], 56 KiB, and the facts' ids, flags and the stamps every
// answer carries — its position, and the push that introduced each commit it
// names — take the rest with room to spare. Measured as JSON rather than as
// the paths' own bytes because a path of `<` costs six bytes there: a bound on
// raw bytes would let such a list run past the page it must fit. A list at
// the bound is about a twentieth of a record's bytes (MaxRecordBytes), so a
// record that carries the facts on carries it beside everything else.
const MaxTouchedBytes = 48 << 10

// MaxReanchors is the most anchors one re-anchoring read places
// ([ReanchorQuery]). Each is two lines of the read's one check of what it
// names and at most one pair of its one diff process, and its answer at most
// a few hundred bytes, so sixty-four keep the read's short check far inside
// its bound and its answer far inside a page: the findings one review raises
// at most, placed by one read.
const MaxReanchors = 64

// maxAnchorIDBytes bounds an anchor's id: an opaque name the caller gives
// each anchor and reads its answer back by — a finding's, a UUID's 36
// characters. 64 bytes leave room for another naming and keep a
// re-anchoring's answer inside a page whatever its ids hold: MaxReanchors of
// them, every byte one JSON writes in six, are 24 KiB of ids, and the whole
// list, every anchor's numbers at their widest, under 30 KiB — about half a
// page.
const maxAnchorIDBytes = 64

// THE BOUNDS OF A LOG AND A BLAME. Every field git leaves unbounded — a
// commit's subject, its author's name and address, its parents, a blamed
// line's text — is DESCRIBED, NEVER CUT, past its bound: answered absent
// beside its size, so a page never carries half a field read as the whole.
// Each bound is of the field's ENCODED size — its JSON, every control byte
// six and every byte that is not UTF-8 six, with the escape the fence a code
// tool puts it in or beside writes into it — so the widest commit and the
// widest blamed line with its commit always fit one page with room to spare,
// however the text is spelled: a subject of 4 KiB of control bytes would
// otherwise encode to 24 KiB.

// MaxLogLimit is the most commits one log page lists, and DefaultLogLimit how
// many a read naming none lists. A HUNDRED, a long screen of history, and a
// page past it is bounded in bytes as well: a page ends before the commit
// whose encoding — its subject's fence included — would take the answer past
// MaxGitAnswerBytes, so a hundred subjects of 4 KiB is several pages, never
// one answer too large to send. TWENTY-FIVE by default: the screen a "when
// did this change" question reads first.
const (
	MaxLogLimit     = 100
	DefaultLogLimit = 25
)

// MaxLogGrepBytes is the longest literal a log is kept to: a few words of a
// message, which a model looks for — never a document — and short enough that
// the streaming matcher's table is nothing beside the page.
const MaxLogGrepBytes = 256

// MaxLogSubjectBytes is the most a commit's SUBJECT may encode to and be
// answered: the message's first paragraph, as git's %s joins it. 4 KiB holds
// any subject a person writes, and a page of them at its bound is still a
// dozen commits; past it the subject is described by its size
// (LogCommit.SubjectBytes).
const MaxLogSubjectBytes = 4 << 10

// MaxLogPersonBytes is the most a commit's author's name, or address, may
// encode to and be answered. A JUDGMENT: git and fsck bound neither, RFC 5321
// bounds an address at 254 octets, and 1 KiB holds every real name and
// address with room; past it each is described by its size.
const MaxLogPersonBytes = 1 << 10

// MaxLogParents is the most parents a commit's answer lists: an octopus
// merge's, 64 ids of 43 bytes encoded. Past it the parents are described by
// their count (LogCommit.ParentCount).
const MaxLogParents = 64

// MaxBlameLines is the most lines one blame names, its whole range: a few
// screens of a file, which its answer pages within. A blame's cost is the
// walk of the file's history for the lines it names, so a range is bounded
// rather than the file, and that walk is the read's own git, within its one
// budget like any other: a range whose walk runs it out is refused naming a
// narrower one.
const MaxBlameLines = 500

// MaxBlameLineBytes is the most a blamed line's text may encode to and be
// answered: twice a subject's bound, since a source line of generated code or
// minified text runs long; past it the line is described by its size
// (BlameLine.TextBytes).
const MaxBlameLineBytes = 8 << 10

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
	// GitRevisionFacts is what a change's revision is against its target's
	// tip: their merge base, the change's patch-id and the files it touches,
	// and whether its head is in the target already — what the repository's
	// home git node records on the change for each revision, or lands it by.
	// THE ENGINE'S OWN READ ([GitReadKind.EnginesOwn]): every reader of a
	// revision's facts after the home reads the rows, never git.
	GitRevisionFacts GitReadKind = "revision_facts"
	// GitReanchor is where lines anchored in one revision's files — a
	// finding's, a comment's — are in another revision's: moved by the
	// lines changed above them, outdated where their own lines changed, gone
	// with their file.
	GitReanchor GitReadKind = "reanchor"
	// GitInterdiff is what a change changed between two of its revisions:
	// the diff of their heads kept to the files either revision's change
	// touches, its stat or one file's patch, answered as a diff is (Kind
	// interdiff, its Diff part).
	GitInterdiff GitReadKind = "interdiff"
	// GitLog is the history of a path — or of every path — from a commit:
	// the commits that changed it, newest first by generation, a page at a
	// time, each with its parents, its author, its two dates and its
	// subject, and optionally only those whose message holds a literal.
	GitLog GitReadKind = "log"
	// GitBlame is which commit last changed each line of a file at a commit,
	// and the line — and the path, where it was another — it had there: at
	// most MaxBlameLines lines a read, a page at a time.
	GitBlame GitReadKind = "blame"
	// GitChecksFile is the checks file a commit holds — [ChecksFilePath] in
	// its tree — its object's id and type and, for a file within
	// [MaxChecksFileBytes], its bytes WHOLE: what a repository's home reads a
	// change's checks' definitions from, at its target's tip (M5-D15). THE
	// ENGINE'S OWN READ ([GitReadKind.EnginesOwn]), and one answer always: a
	// file the parse refuses by its size alone is answered by its size, and
	// the bytes are exact — never lines cut into pages, nor text a JSON
	// string would replace an invalid byte of, which the parse must see as
	// git holds it.
	GitChecksFile GitReadKind = "checks_file"
	// GitSearch is the lines of a commit a code search matches — the
	// default branch's tip through the repository's code index where its git
	// node keeps one, and otherwise git grep — at most MaxSearchHits of
	// them, in one answer with no next page (search.go).
	GitSearch GitReadKind = "search"
)

// GitReadKinds is the closed set.
//
// A KIND IS AN ESTATE REQUEST'S, NEVER A RECORD'S: a GitQuery crosses the
// estate wire and is answered by a git node from its cache, and no record of
// the code log names one, so a kind added here moves no RecordVersion and
// takes no versionedFields row.
func GitReadKinds() []GitReadKind {
	return []GitReadKind{GitTree, GitBlob, GitDiff, GitCompare, GitLandedIn, GitRevisionFacts, GitReanchor,
		GitInterdiff, GitLog, GitBlame, GitChecksFile, GitSearch}
}

// Valid reports a kind this build answers.
func (k GitReadKind) Valid() bool { return slices.Contains(GitReadKinds(), k) }

// EnginesOwn reports a kind THE ENGINE ALONE reads — a revision's facts, which
// the repository's home git node reads to record them on the change, or to
// land it, and a commit's checks file, which it reads a change's checks'
// definitions from at its target's tip — so a read of it names NO asker and
// no seat it reads for, is held to no grant and is recorded as nobody's read:
// an asker there would be judged for a read it never made, and a
// `visibility: unit` repository's readers would be told the engine read it.
// As the duty listings of the rows are ([ReadQuery]'s `compaction_due`), and
// for the same reason.
func (k GitReadKind) EnginesOwn() bool { return k == GitRevisionFacts || k == GitChecksFile }

// Unstampable reports a kind whose answer is a PAGE of the repository's
// content — a listing, a file's lines, a diff, a comparison, an interdiff, a
// history or a blame — which a read may ask cut and answered with no stamp
// ([GitQuery.Unstamped]). Never a landed-in read, whose answer IS what the
// node's index says of a push; nor a revision's facts, a re-anchoring or a
// checks file, which the engine and a change's own reads ask, never a
// question that keeps a page.
func (k GitReadKind) Unstampable() bool {
	switch k {
	case GitTree, GitBlob, GitDiff, GitCompare, GitInterdiff, GitLog, GitBlame:
		return true
	}
	return false
}

// EnginesOwn reports a read THE ENGINE makes on its own account: one of a kind
// only it reads ([GitReadKind.EnginesOwn]), or one it makes FOR A CHECK
// ([GitQuery.Check]). Such a read names no asker and no seat it reads for, is
// held to no grant and is recorded as nobody's read — the git node's one test
// of all three.
func (q GitQuery) EnginesOwn() bool { return q.Kind.EnginesOwn() || q.Check != nil }

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
	// chart. Never trusted for anything but the grant. REFUSED on a read the
	// engine makes on its own account ([GitQuery.EnginesOwn]), which names
	// none.
	Asker Principal `json:"asker"`
	// Check is the attempt of a check a read is made FOR: a check's box
	// reading its commit through the API (/box/code/), admitted by the
	// attempt's row and its token (internal/code/host's AdmitCheck). THE
	// ENGINE'S OWN READ on the check's behalf ([GitQuery.EnginesOwn]): a
	// check's box is no principal and the attempt's row is its authority, so
	// it names no asker, is held to no grant and is recorded as nobody's read
	// — the check's row is its record. And it reads THE CHECK'S COMMIT ALONE:
	// a tree or a file at it, or a diff whose head is it — the one commit the
	// box's token admits it to — which the git node holds it to as it
	// validates it, whatever its asker set; a diff's base is the attempt's
	// row's, which the admission sets. An estate request's field and no
	// record's, so it takes no versionedFields row: a git node of a build that
	// predates it reads such a read as one naming no asker, and refuses it
	// (ErrInvalid) — the box's read refused, never served as another's.
	Check *CheckKey `json:"check,omitempty"`
	// CeilingSeat is the seat the asking seat's turn is acting for, whose
	// grant the git node judges beside the asker's, from the same chart and
	// the row its copy holds, as [ReadQuery.CeilingSeat] says.
	CeilingSeat string `json:"ceiling_seat,omitempty"`
	// Turn is the turn a SEAT'S read is made in (turnctx.Turn.RunID): the
	// principal its one second look at the log's head is charged to, on its
	// seat's bucket of the answering git node ([NewResyncs]) — "" for a read
	// outside a turn, which the seat's bucket alone bounds then. Set only
	// beside a seat's asker, and never trusted for anything but whose bucket
	// pays: the asker is judged by who it is, never by what it is doing. An
	// estate request's field and no record's, so it takes no versionedFields
	// row: a git node of a build that predates it charges nothing, as that
	// build did.
	Turn string `json:"turn,omitempty"`
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
	// Home is set on the attempt a walk makes of the ONE node it lets FILL a
	// copy it does not hold (ADR-0059): its EFFECTIVE HOME — the first git
	// node of the repository's order once the walk's demotions are applied —
	// and, only where that node answered that no copy serving the read is
	// coming there ([UnreadyReason.PassesFill]: behind its configuration, a
	// failure of its own, no code host), the next node of the order in its
	// place; never two at once. A git node holding no copy of the repository
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
	// Unstamped asks for THE PAGE ALONE: its Kind, Rev, Next and the kind's
	// own part, with none of the fields a git node stamps on an answer — the
	// repository, the copy's position, Memo, the push that introduced each
	// commit and the coverage of the index that says so ([GitAnswer]) — and
	// CUT AGAINST A ROOM NONE OF THEM TAKES. A stamped page is cut against
	// the room its stamps leave, so the same page cut where the node's index
	// holds more of its commits, or at a longer position, holds fewer items
	// and another Next; an unstamped page is a function of the object ids it
	// names, its arguments and the build that cuts it, and of nothing a node
	// or a moment holds — what a question may answer as never changing
	// (internal/api/queries' Immutable), which a stamped page cannot be. A
	// node memoizes the two apart, since they are two cuts of one read.
	//
	// AN ESTATE REQUEST'S FIELD, NOT A RECORD'S, so it takes no
	// versionedFields row. AS A STATED CONDITION: no build that carries the
	// code domain has run without it — none of the domain is on main — so
	// every git node that answers a read honours it.
	Unstamped bool `json:"unstamped,omitempty"`

	Tree          *TreeQuery          `json:"tree,omitempty"`
	Blob          *BlobQuery          `json:"blob,omitempty"`
	Diff          *DiffQuery          `json:"diff,omitempty"`
	Compare       *CompareQuery       `json:"compare,omitempty"`
	LandedIn      *LandedInQuery      `json:"landed_in,omitempty"`
	RevisionFacts *RevisionFactsQuery `json:"revision_facts,omitempty"`
	Reanchor      *ReanchorQuery      `json:"reanchor,omitempty"`
	Interdiff     *InterdiffQuery     `json:"interdiff,omitempty"`
	Log           *LogQuery           `json:"log,omitempty"`
	Blame         *BlameQuery         `json:"blame,omitempty"`
	ChecksFile    *ChecksFileQuery    `json:"checks_file,omitempty"`
	Search        *SearchQuery        `json:"search,omitempty"`
}

// ChecksFileQuery reads the checks file one commit holds ([GitChecksFile]).
// An estate request's, as every kind's query is ([GitReadKinds]): a git node of
// a build that predates it refuses the read as one naming no query of its kind
// (ErrInvalid), and its asker asks the next git node of the order.
type ChecksFileQuery struct {
	// Commit is the commit read at, its whole id — a change's target's tip,
	// as its home read it from the rows.
	Commit string `json:"commit"`
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
	// whole id — and At the position of the record that moved Target there
	// ([ResolvedRef.Moved]): the git node's copy is brought to At before it
	// judges, so the moves of Target up to that tip — every move it had when
	// it was resolved — are in the index it searches.
	TargetOID string            `json:"target_oid"`
	At        statelog.Position `json:"at"`
}

// THE READS OF A CHANGE'S REVISIONS name every revision by the WHOLE ids its
// rows recorded — a change's revision heads, its target's tip, a revision's
// base — never an abbreviation: their asker read them from rows, and an
// interdiff compares two bases as ids without reading either.

// RevisionFactsQuery asks what a change's revision is against its target:
// the facts of the commit Head against the commit Target.
type RevisionFactsQuery struct {
	// Head is the revision's head commit, and Target the tip of the change's
	// target branch as the asker read it — both whole ids. The facts are a
	// function of the two commits alone, recorded against them.
	Head   string `json:"head"`
	Target string `json:"target"`
}

// ReanchorQuery asks where ranges of lines, each read in a blob a revision
// held, are in another revision of the same change: each anchor is placed in
// the file at its path in Head where it is on the new side of the change's
// diff, and in Base where it is on the old side.
type ReanchorQuery struct {
	// Base and Head are the revision the anchors are placed in: its base
	// (the merge base its facts record) and its head commit, whole. Base is
	// required only of a read placing an anchor on the old side.
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
	// SideOld is the base's file: the lines the change deletes, and those
	// it keeps as they were.
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
	// Path is the file's path in the revision the anchor is placed in
	// (PathB64 its exact bytes, as [GitPath] reads them), and Side which of
	// that revision's two commits holds it.
	Path    string `json:"path,omitempty"`
	PathB64 string `json:"path_b64,omitempty"`
	Side    Side   `json:"side"`
	// BlobOID is the blob the lines were read in, whole, and Start and End
	// the range, 1-based and inclusive.
	BlobOID string `json:"blob_oid"`
	Start   int    `json:"start"`
	End     int    `json:"end"`
}

// InterdiffQuery asks what a change changed between two of its revisions:
// the diff of their heads, kept to the files either revision's change touches
// — so the files a rebase between them brought in from the target, which
// neither revision touches, stay out of it — unless either revision touches
// all, when it is the heads' whole diff.
type InterdiffQuery struct {
	// FromBase and FromHead are the earlier revision's base and head as its
	// facts record them, and ToBase and ToHead the later one's — whole ids, a
	// base empty where its facts name no merge base. The bases are compared,
	// never read: two revisions measured from one base differ only in files
	// one of them touches, so their heads' diff needs no keeping to them.
	FromBase string `json:"from_base,omitempty"`
	FromHead string `json:"from_head"`
	ToBase   string `json:"to_base,omitempty"`
	ToHead   string `json:"to_head"`
	// FromTouched and ToTouched are each revision's touched paths as its
	// facts list them, and FromTouchesAll and ToTouchesAll whether its facts
	// say it touches all, beside which it gives no list.
	FromTouched    []string `json:"from_touched,omitempty"`
	FromTouchesAll bool     `json:"from_touches_all,omitempty"`
	ToTouched      []string `json:"to_touched,omitempty"`
	ToTouchesAll   bool     `json:"to_touches_all,omitempty"`
	// File asks for one file's patch instead of the stat: its path at
	// ToHead, or at FromHead for a file ToHead deleted (FileB64 its exact
	// bytes), as [DiffQuery.File] is.
	File    string `json:"file,omitempty"`
	FileB64 string `json:"file_b64,omitempty"`
}

// LogQuery is the history of a path from a commit, or of every path.
type LogQuery struct {
	// Rev is the commit the history is read from (or a tag naming one),
	// whole or abbreviated.
	Rev string `json:"rev"`
	// Path is the file or the directory whose history is read, from the
	// repository's root — its commits are those that changed it, as git's
	// history simplification keeps them — and none reads every commit.
	// PathB64 is the same path as its exact bytes in standard base64, for a
	// path that is not valid UTF-8: at most one of the two is set. A path
	// the commit does not hold is a history all the same: a file deleted
	// since was changed by the commits before its deletion.
	Path    string `json:"path,omitempty"`
	PathB64 string `json:"path_b64,omitempty"`
	// Grep keeps the commits whose whole message holds it: a LITERAL,
	// matched case-folded in the git node's own code over each message as it
	// streams by, never handed to git — a regular expression from a model is
	// a runtime nobody bounded, and text on a command line is what a read by
	// oid never carries. Valid UTF-8, no NUL, at most MaxLogGrepBytes.
	Grep string `json:"grep,omitempty"`
	// Since keeps the commits committed at or after it, as git's --since
	// does: the walk stops at a commit committed before it on each line of
	// history it reaches, so a commit dated after it whose every line runs
	// through an older one is not listed. Zero keeps every one.
	Since time.Time `json:"since,omitzero"`
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
	// MaxBlameLines lines: zero is the file's first line, and the line
	// MaxBlameLines − 1 past StartLine, or the file's last where that is
	// sooner. A page ends where the answer is full, and its Next resumes
	// inside the range.
	StartLine int `json:"start_line,omitempty"`
	EndLine   int `json:"end_line,omitempty"`
}

// Range is the blame's range as its git is asked it: its first line and its
// last, the default end MaxBlameLines − 1 past the first — the file's last
// line where that is sooner, which git clamps it to.
func (q BlameQuery) Range() (start, end int) {
	start = max(q.StartLine, 1)
	end = q.EndLine
	if end == 0 {
		// The line MaxBlameLines − 1 past the first, or the last an int
		// counts to: a range that far out names no line of any file, and is
		// refused by git's own end of the file rather than by an overflow.
		end = start + min(MaxBlameLines-1, math.MaxInt-start)
	}
	return start, end
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
// not canonical, an asker no principal is, a turn beside an asker that is no
// seat or past a turn id's bounds, a sub-query other than its kind's
// or none, a revision [ValidRev] refuses, a path [GitPath] refuses — or none
// where a blame names its file — a line range that runs backwards or past a
// blame's MaxBlameLines, a diff naming both a file and paths, a log whose
// literal, limit or date no log takes, and a read made for a check that names
// an asker or reads anything but its check's commit. A cursor longer than any
// a git node issues is [ErrBadCursor]; whether a shorter one is the kind's own
// is the git node's to judge.
func (q GitQuery) Validate() error {
	if !q.Kind.Valid() {
		return fmt.Errorf("%w: kind: %q is not one of %v", ErrInvalid, q.Kind, GitReadKinds())
	}
	if id, err := uuid.Parse(q.RepoID); err != nil || id.String() != q.RepoID {
		return fmt.Errorf("%w: repo_id: %q is not a repository's id", ErrInvalid, q.RepoID)
	}
	// WHO ASKS IS NAMED, or nothing is read: the answer is held to the
	// asker's grant, and an absent asker would be judged as somebody nobody
	// named — but for a read the engine makes on its own account, which names
	// nobody, an asker there being judged for a read it never made.
	switch {
	case q.Check != nil:
		if q.Asker != (Principal{}) || q.CeilingSeat != "" {
			return fmt.Errorf("%w: asker: a read made for a check is the engine's own, on the check's "+
				"behalf — a check's box is no principal — which no principal makes", ErrInvalid)
		}
		if err := q.Check.validateAs("check."); err != nil {
			return err
		}
	case q.Kind.EnginesOwn():
		if q.Asker != (Principal{}) || q.CeilingSeat != "" {
			return fmt.Errorf("%w: asker: a %s read is the engine's own — its repository's home git node's — "+
				"which no principal makes", ErrInvalid, q.Kind)
		}
	default:
		if why := q.Asker.invalid(); why != "" {
			return fmt.Errorf("%w: asker: %s", ErrInvalid, why)
		}
		if why := ceilingInvalid(q.Asker, q.CeilingSeat); why != "" {
			return fmt.Errorf("%w: %s", ErrInvalid, why)
		}
	}
	if why := turnInvalid(q.Asker, q.Turn); why != "" {
		return fmt.Errorf("%w: turn: %s", ErrInvalid, why)
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
		{GitCompare, q.Compare != nil}, {GitLandedIn, q.LandedIn != nil},
		{GitRevisionFacts, q.RevisionFacts != nil}, {GitReanchor, q.Reanchor != nil},
		{GitInterdiff, q.Interdiff != nil}, {GitLog, q.Log != nil}, {GitBlame, q.Blame != nil},
		{GitChecksFile, q.ChecksFile != nil}, {GitSearch, q.Search != nil}} {
		switch {
		case s.kind == q.Kind && !s.present:
			return fmt.Errorf("%w: %s: a %s read names what it reads", ErrInvalid, s.kind, s.kind)
		case s.kind != q.Kind && s.present:
			return fmt.Errorf("%w: %s: a %s read carries no %s query", ErrInvalid, s.kind, q.Kind, s.kind)
		}
	}
	if q.Check != nil {
		if err := q.atItsCheck(); err != nil {
			return err
		}
	}
	if q.Unstamped && !q.Kind.Unstampable() {
		return fmt.Errorf("%w: unstamped: a %s read answers no page to cut unstamped", ErrInvalid, q.Kind)
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
	case GitChecksFile:
		return wholeID("checks_file.commit", q.ChecksFile.Commit, false)
	case GitSearch:
		return q.Search.validate()
	}
	return q.Compare.validate()
}

// atItsCheck refuses a read made for a check ([GitQuery.Check]) that reads
// anything but the check's commit — a tree or a file at it, or a diff whose
// head is it, by its whole id as the attempt's row names it: the one commit a
// check's box's token admits it to, which the git node holds the read to
// whatever the node that asked it built.
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
		return fmt.Errorf("%w: kind: a read made for a check is a tree, a file or a diff of its commit, "+
			"never a %s read", ErrInvalid, q.Kind)
	}
	if rev != q.Check.OID {
		return fmt.Errorf("%w: %s: %q is not the check's commit %s: a read made for a check reads that "+
			"commit alone", ErrInvalid, field, rev, q.Check.OID)
	}
	return nil
}

// ceilingInvalid is why a read may not carry seat as its ceiling beside asker
// ([ReadQuery.CeilingSeat]), or "" where it may: none at all, or a seat's
// handle beside a seat's asker. Every query a principal reads by — a read, a
// resolve, a read by object id — states its ceiling by this one rule.
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

// turnInvalid is why turn is no turn asker's read may name, or "": a turn on
// any asker but a seat's — a person's or a token's read is charged to no
// turn — or one past MaxProvenanceBytes, not UTF-8, or holding a control
// character, the bounds a push's turn is held to.
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

// wholeID is a whole object id as a read of a change's revisions takes it —
// never an abbreviation, never the zero id — or a refusal naming field; one
// empty passes where empty says.
func wholeID(field, id string, empty bool) error {
	switch {
	case id == "" && empty:
		return nil
	case !gitref.ValidOID(id) || id == ZeroOID:
		return fmt.Errorf("%w: %s: %q is not a whole object id: a change's revisions are read by the 40-digit "+
			"ids their rows record", ErrInvalid, field, id)
	}
	return nil
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
		return fmt.Errorf("%w: landed_in.at: %s is not the code log position the target last moved at",
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
		return fmt.Errorf("%w: reanchor.anchors: %d anchors; a read places 1 to %d", ErrInvalid, n, MaxReanchors)
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
			return fmt.Errorf("%w: %s.side: %q is not one of %v", ErrInvalid, field, a.Side, Sides())
		case a.Side == SideOld && q.Base == "":
			return fmt.Errorf("%w: %s.side: an anchor on the old side is placed in the revision's base, which "+
				"reanchor.base names", ErrInvalid, field)
		case a.Start < 1 || a.End < a.Start:
			return fmt.Errorf("%w: %s: lines %d to %d are no range: lines count from 1, and end is at or after "+
				"start", ErrInvalid, field, a.Start, a.End)
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
	if q.File == "" && q.FileB64 == "" {
		return nil
	}
	_, err := GitPath("interdiff.file", q.File, q.FileB64, false)
	return err
}

// logSinceAhead is how far past this node's clock a log's Since may be: a day,
// the most two nodes' clocks or an asker's idea of today may honestly differ
// by. Past it the asker named a date that is not yet — a year typed wrong —
// and is told so rather than answered an empty history.
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
		return fmt.Errorf("%w: log.grep: %d bytes, past the %d a log's literal may have", ErrInvalid,
			len(q.Grep), MaxLogGrepBytes)
	case !utf8.ValidString(q.Grep) || strings.IndexByte(q.Grep, 0) >= 0:
		return fmt.Errorf("%w: log.grep: a literal is valid UTF-8 holding no NUL", ErrInvalid)
	case q.Limit < 0 || q.Limit > MaxLogLimit:
		return fmt.Errorf("%w: log.limit: %d; a page lists 1 to %d commits, %d when left out", ErrInvalid,
			q.Limit, MaxLogLimit, DefaultLogLimit)
	case q.Since.IsZero():
	case q.Since.Before(time.Unix(0, 0)):
		return fmt.Errorf("%w: log.since: %s is before 1970, the first instant a commit's date can name",
			ErrInvalid, q.Since.UTC().Format(time.RFC3339))
	case q.Since.After(time.Now().Add(logSinceAhead)):
		return fmt.Errorf("%w: log.since: %s is not yet: give a date in the past", ErrInvalid,
			q.Since.UTC().Format(time.RFC3339))
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
		return fmt.Errorf("%w: blame.start_line: %d; lines count from 1", ErrInvalid, q.StartLine)
	case q.EndLine < 0:
		return fmt.Errorf("%w: blame.end_line: %d; lines count from 1", ErrInvalid, q.EndLine)
	case end < start:
		return fmt.Errorf("%w: blame.end_line: %d is before start_line %d", ErrInvalid, end, start)
	case end-start >= MaxBlameLines:
		return fmt.Errorf("%w: blame.end_line: lines %d to %d are %d lines; a blame names at most %d",
			ErrInvalid, start, end, end-start+1, MaxBlameLines)
	}
	return nil
}

// validTouched refuses a revision's touched paths as an interdiff's asker
// gives them back: a list beside the word that it touches all, a list past
// either bound its facts hold one to, or a path no tree entry is named by.
func validTouched(field string, touched []string, all bool) error {
	switch cost := TouchedCost(touched); {
	case all && len(touched) > 0:
		return fmt.Errorf("%w: %s: a revision that touches all lists no paths", ErrInvalid, field)
	case len(touched) > MaxTouchedPaths:
		return fmt.Errorf("%w: %s: %d paths, past the %d a revision's facts list", ErrInvalid, field,
			len(touched), MaxTouchedPaths)
	case cost > MaxTouchedBytes:
		return fmt.Errorf("%w: %s: the list is %d bytes of JSON, past the %d a revision's facts list",
			ErrInvalid, field, cost, MaxTouchedBytes)
	}
	for i, p := range touched {
		if why := validGitPath(p, false); why != "" {
			return fmt.Errorf("%w: %s[%d]: %s", ErrInvalid, field, i, why)
		}
	}
	return nil
}

// TouchedCost is what a touched list takes as JSON, as [MaxTouchedBytes]
// bounds it: the list's own encoding — an empty list its two brackets.
func TouchedCost(touched []string) int {
	if touched == nil {
		touched = []string{}
	}
	out, err := json.Marshal(touched)
	if err != nil {
		// A list of strings always encodes; reaching here is a broken
		// encoder, against which no list could be measured.
		panic(fmt.Sprintf("code: encoding a touched list: %v", err))
	}
	return len(out)
}

// GitAnswer answers a [GitQuery]: exactly the field of its Kind is set — a
// file read whose path names a directory answers Kind tree, and an interdiff
// (Kind interdiff) sets Diff, being one. Its JSON is at most
// MaxGitAnswerBytes.
type GitAnswer struct {
	Kind   GitReadKind `json:"kind"`
	RepoID string      `json:"repo_id"`
	// Rev is the object a tree or a file read was asked of, as its whole id
	// — a tag's is the commit or tree it names. A diff, a comparison, an
	// interdiff and a revision's facts name their commits in their own
	// answer, and a re-anchoring names none.
	Rev string `json:"rev,omitempty"`
	// Position is the answering node's copy's: every row up to it is in what
	// it read, so an asker holding a later floor knows the answer may
	// predate a push it made — never that an object it names is wrong.
	Position statelog.Position `json:"position"`
	// Memo is set on a page the answering node kept from a read that cut it
	// before (its cache's memo) rather than cut again: the same page, since a
	// page is a function of the object ids it names and the read's arguments
	// alone, answered with no git run.
	Memo bool `json:"memo,omitempty"`
	// Next is where the next page starts, handed back as the query's Cursor
	// with the query otherwise unchanged; empty on the last page.
	Next string `json:"next,omitempty"`
	// PushedBy is the push that INTRODUCED each commit the answer names —
	// its Rev, a diff's or a comparison's commits, every commit a log page
	// lists or a blame page's lines name — by the commit's whole id,
	// where the answering node's index of which push introduced each commit
	// holds it. A commit it does not name was introduced outside Coverage, or
	// by a push that listed more than MaxIntroduced commits.
	PushedBy map[string]PushedBy `json:"pushed_by,omitempty"`
	// Coverage is what that index holds, on every answer of a node that keeps
	// one: an absent PushedBy means nothing beyond it.
	Coverage *Coverage `json:"coverage,omitempty"`

	Tree          *TreeAnswer          `json:"tree,omitempty"`
	Blob          *BlobAnswer          `json:"blob,omitempty"`
	Diff          *DiffAnswer          `json:"diff,omitempty"`
	Compare       *CompareAnswer       `json:"compare,omitempty"`
	LandedIn      *LandedInAnswer      `json:"landed_in,omitempty"`
	RevisionFacts *RevisionFactsAnswer `json:"revision_facts,omitempty"`
	Reanchor      *ReanchorAnswer      `json:"reanchor,omitempty"`
	Log           *LogAnswer           `json:"log,omitempty"`
	Blame         *BlameAnswer         `json:"blame,omitempty"`
	ChecksFile    *ChecksFileAnswer    `json:"checks_file,omitempty"`
	Search        *SearchAnswer        `json:"search,omitempty"`
}

// ChecksFileAnswer is what a commit holds at [ChecksFilePath]: nothing (no
// OID), something that is no file (Type tree, or commit for a submodule), or a
// file — its id and size, and its bytes whole where it is at most
// [MaxChecksFileBytes]; a file past that carries none, its size being what
// [ParseChecks] refuses it by. So at most MaxChecksFileBytes of bytes, which
// the answer's JSON carries in base64 — some 88 KiB, far inside the estate's
// reply ([MaxPageBytes]) — and never held to [MaxGitAnswerBytes], the bound a
// seat's tool answer is cut to, which no seat reads this one through.
type ChecksFileAnswer struct {
	// Commit is the commit read at, its whole id.
	Commit string `json:"commit"`
	// OID is the object at the path, and Type its type: blob, tree or
	// commit. Both are empty where the commit holds nothing there.
	OID  string `json:"oid,omitempty"`
	Type string `json:"type,omitempty"`
	// Size is a blob's bytes.
	Size int64 `json:"size,omitempty"`
	// Raw is a blob's bytes, whole, where Size is at most
	// MaxChecksFileBytes; nil otherwise, and for an empty file.
	Raw []byte `json:"raw,omitempty"`
}

// LogAnswer is one page of a history: the commits that changed the path (or
// any path), newest first by generation — git's `log --topo-order`, every
// parent after every child — as many as the page's limit and its bytes hold.
// The push that introduced each is the answer's PushedBy.
type LogAnswer struct {
	// Path is the path whose history it is, empty for a whole history
	// (PathB64 per [WirePath]).
	Path    string `json:"path,omitempty"`
	PathB64 string `json:"path_b64,omitempty"`
	// Commits is the page, in the walk's order.
	Commits []LogCommit `json:"commits"`
	// Scanned is how many commits of the history this page covers — every
	// one a Grep did not keep included — so the next page begins that many
	// commits further on.
	Scanned int `json:"scanned"`
	// Complete is set where the walk reached the history's end: there is no
	// next page. A page the read's budget of git ended is never complete,
	// and carries what it found and a Next: never a claim that nothing more
	// matches. Nor is a Grep's page that holds its limit, which ends at its
	// last match rather than walking on to find one more, so the page after
	// it may list nothing and be the complete one.
	Complete bool `json:"complete,omitempty"`
}

// LogCommit is one commit of a history. Every field git leaves unbounded is
// described, never cut, past its bound (see MaxLogSubjectBytes and its kin).
type LogCommit struct {
	OID string `json:"oid"`
	// Parents are the commit's parents' ids, none for a root; past
	// MaxLogParents they are absent and ParentCount is how many there are.
	Parents     []string `json:"parents,omitempty"`
	ParentCount int      `json:"parent_count,omitempty"`
	// Author and AuthorEmail are the commit's author as it states them —
	// never as a `.mailmap` would rename them, which would make an answer a
	// function of a file at some branch rather than of the commit — each past
	// MaxLogPersonBytes absent with its size in AuthorBytes or
	// AuthorEmailBytes.
	Author           string `json:"author,omitempty"`
	AuthorBytes      int    `json:"author_bytes,omitempty"`
	AuthorEmail      string `json:"author_email,omitempty"`
	AuthorEmailBytes int    `json:"author_email_bytes,omitempty"`
	// AuthoredAt and CommittedAt are the commit's two dates, to the second.
	AuthoredAt  time.Time `json:"authored_at"`
	CommittedAt time.Time `json:"committed_at"`
	// Subject is the message's first paragraph, its lines joined as git's
	// %s joins them — or, past MaxLogSubjectBytes, absent with SubjectBytes
	// its size.
	Subject      string `json:"subject,omitempty"`
	SubjectBytes int    `json:"subject_bytes,omitempty"`
}

// BlameAnswer is one page of a blame: each line of the page's part of the
// range, with the commit that last changed it, and each of those commits
// once. The push that introduced each commit is the answer's PushedBy.
type BlameAnswer struct {
	// Path is the file blamed (PathB64 per [WirePath]).
	Path    string `json:"path"`
	PathB64 string `json:"path_b64,omitempty"`
	// StartLine and EndLine are the first and the last line THIS page
	// attributes — both zero for an empty file, which has none.
	StartLine int         `json:"start_line"`
	EndLine   int         `json:"end_line"`
	Lines     []BlameLine `json:"lines"`
	// Commits is every commit a line of the page names, by its whole id,
	// once.
	Commits map[string]BlameCommit `json:"commits"`
}

// BlameLine is one line of a blame.
type BlameLine struct {
	// Line is its number in the file blamed, and Commit the commit that last
	// changed it, whole.
	Line   int    `json:"line"`
	Commit string `json:"commit"`
	// OrigLine is its number in Commit's version of the file, and OrigPath
	// that file's path where it was another — a file renamed since (PathB64
	// per [WirePath]).
	OrigLine    int    `json:"orig_line"`
	OrigPath    string `json:"orig_path,omitempty"`
	OrigPathB64 string `json:"orig_path_b64,omitempty"`
	// Text is the line, without its line feed — or, past
	// MaxBlameLineBytes, absent with TextBytes its size.
	Text      string `json:"text,omitempty"`
	TextBytes int    `json:"text_bytes,omitempty"`
}

// BlameCommit is a commit a blame's lines name: its author as it states them
// (never a `.mailmap`'s), the date it was authored and its subject, each
// described past its bound as a LogCommit's is.
type BlameCommit struct {
	Author           string    `json:"author,omitempty"`
	AuthorBytes      int       `json:"author_bytes,omitempty"`
	AuthorEmail      string    `json:"author_email,omitempty"`
	AuthorEmailBytes int       `json:"author_email_bytes,omitempty"`
	AuthoredAt       time.Time `json:"authored_at"`
	// Subject is the message's first LINE, as git's blame names it — not
	// the first paragraph a log's subject is — absent past
	// MaxLogSubjectBytes with SubjectBytes its size, and empty for a
	// commit whose message is.
	Subject      string `json:"subject,omitempty"`
	SubjectBytes int    `json:"subject_bytes,omitempty"`
}

// RevisionFactsAnswer is what a revision's head is against its target's tip:
// the change it makes, as a function of the two commits alone.
type RevisionFactsAnswer struct {
	// Head and Target are the query's commits, whole.
	Head   string `json:"head"`
	Target string `json:"target"`
	// MergeBase is their best common ancestor — the BASE the change is
	// measured from, on the target's side — or empty for histories that
	// share none, whose change is measured from the empty tree: every file of
	// Head.
	MergeBase string `json:"merge_base,omitempty"`
	// BaseTree and HeadTree are the trees of the base — the empty tree's id
	// where there is none — and of Head: two revisions holding the same pair
	// hold the same code, byte for byte, which an approval carries across
	// whatever their patch ids say ([ChangeClassNoCodeChange]). Empty only
	// where HeadInTarget, which measures nothing.
	BaseTree string `json:"base_tree,omitempty"`
	HeadTree string `json:"head_tree,omitempty"`
	// PatchID is `git patch-id --verbatim` of the cumulative diff from the
	// base to Head: the same for two revisions whose changes are the same
	// lines with the same lines around them, wherever the base put them — a
	// rebase that moved nothing near them keeps it — and another for any
	// changed byte, whitespace included. Empty for a revision that changes
	// nothing, and where PatchIDOmitted: an empty id equals nothing.
	PatchID string `json:"patch_id,omitempty"`
	// PatchIDOmitted is a change whose diff the git node did not identify:
	// one past the bound on a diff it identifies, or one whose diff ran out
	// what was left of the read's budget of git.
	PatchIDOmitted bool `json:"patch_id_omitted,omitempty"`
	// Touched is every file the change touches — every path the diff from
	// the base to Head changes, in git's order — unless TouchesAll: the
	// change touches more than a list holds ([MaxTouchedPaths],
	// [MaxTouchedBytes]) or a path JSON cannot carry exactly (bytes that are
	// not UTF-8), and Touched is empty, to be read as every path.
	Touched    []string `json:"touched"`
	TouchesAll bool     `json:"touches_all,omitempty"`
	// Classes is the path classes of the touched files' changes, each path
	// named by its place in Touched ([RevisionClasses]): each touched Go file
	// changed in place, both of its versions lexed ([ClassifyGo]) within the
	// bounds a revision's classification keeps to ([MaxClassedBytes]) and
	// what the read's budget of git left — incomplete past either, and where
	// the revision touches all.
	Classes RevisionClasses `json:"classes,omitzero"`
	// HeadInTarget is Head reachable from Target: the change is in its
	// target already, merged by whatever moved the target there. Its change
	// is then nothing: no id, no path, no tree.
	HeadInTarget bool `json:"head_in_target,omitempty"`
}

// ReanchorAnswer places every anchor of the query, in its order.
type ReanchorAnswer struct {
	Anchors []Reanchored `json:"anchors"`
}

// Reanchored is one anchor placed. Gone: its revision holds no file at its
// path (deleted, or a directory or a submodule now), so it has no lines
// there. Otherwise Start and End are where its lines are now — moved by the
// lines the change added or removed above them — and Outdated says they are
// not the lines it was written against: one of them was changed or deleted,
// or lines were inserted between two of them, or the file is binary on
// either side; Start and End are then where the range would begin and end had
// only the changes above it moved it.
type Reanchored struct {
	ID       string `json:"id"`
	Start    int    `json:"start,omitempty"`
	End      int    `json:"end,omitempty"`
	Outdated bool   `json:"outdated,omitempty"`
	Gone     bool   `json:"gone,omitempty"`
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
	// At the position of the record that moved the target there — the
	// query's own.
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
	// holds, falls in a range it has not read yet, or lies past what the
	// search checked — its bound of checks (internal/code/query's
	// MaxLandedProbes) or the read's budget of git — named with the oldest
	// push the search knows kept the commit.
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

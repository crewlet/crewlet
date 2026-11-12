package gitlet

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
)

// A CHAIN OF MERGES, BUILT IN ONE SCRATCH ON A REPOSITORY'S HOME (M5-D12,
// M5-D13; internal/gitlet/host's Server.BuildChain): the queue's candidates are
// not built a merge at a time, each its own scratch, pack and upload, but as
// ONE CHAIN — each entry merged by a merge_change's own build onto the commit
// the entry before it made, the first onto the target's tip the chain was
// asked against — so a candidate's train is one scratch, one pack and one
// stored bundle, as a group commit is one record (PLAN: "chains merge-tree
// results in one scratch").
//
// # What a chain is built from
//
// A chain is a pure function of what it is asked ([ChainBuild]) and of the
// objects its copy holds: the target's tip it builds onto, each entry's head
// and method, the change's words its commit carries ([ChainChange]), and who
// enqueued it — the merger a merge commit is authored by (U43 a) — and each
// entry's own instant, its author date: the instant its enqueue was minted at
// ([QueueEntryID] keeps it), so a chain built again from the same rows is the
// same chain, byte for byte, whichever Gitlet node builds it and however often
// (a bisect builds the same prefix twice). Nothing it carries is read from a
// row by the build: the caller formed it from the rows it decided on, and its
// record's decide holds those rows again.
//
// # What a chain answers
//
// One [ChainCommit] an entry it reached, in order: the commit the entry
// became — a fast-forward's is its own head, which the copy already holds —
// or the refusal that skipped it, the chain building on without it: a
// conflict with its paths, a fast-forward the target's history refuses, a
// head the chain already holds, a merge past a merge_change's own budget, or
// whose objects pack past a merge_change's own bound or hold more than
// [MaxChainObjectBytes] uncompressed (too_large), or an object git's own
// check refuses (rejected_object). A skipped entry leaves
// nothing behind it in the scratch. Then the chain's one pack, stored as one
// bundle — nil where no entry made an object — and its tip. The pack enters
// the building node's cache only once the record naming it has applied
// ([ChainResult.Settle]): a pack linked in before it would be objects no row
// names.
//
// # Never refused whole
//
// Every refusal names the ONE entry it is about, so a chain always decides
// at least its first entry and its caller always has an entry to eject or a
// prefix to land: a chain refused whole would be built again, entry for
// entry, by the next turn, and refused again, wedging its target's queue.
// What could sink a chain whole is bounded per entry instead: each entry's
// objects to a merge_change's own [MaxWritePackBytes] packed and to
// [MaxChainObjectBytes] uncompressed, the chain's objects together to
// [MaxChainObjectBytes] (so its pack is made within [ChainPackDeadline]), an
// object git's check refuses to the entry that wrote it, and the chain's time
// to [ChainBuildDeadline]. The entries a chain did not reach within its
// bytes or its time are left out of its answer — a prefix of the entries,
// neither built nor skipped — for the next chain.
//
// # In process, never a record
//
// These are the home's call of its own Gitlet node's builder, through the
// consumer interface the engine wires: no field here is ever encoded onto a
// log, so none is held to the code domain's record format. The record the
// chain's caller publishes — its candidate — names its commits and its pack,
// and carries that format's condition itself.

// MaxChainEntries is the most entries one chain merges: SIXTY-FOUR,
// [MaxPushesPerRecord]. A candidate's green prefix is landed by ONE record
// that names one push row an entry (the landing's `via queue` rows), so a
// chain longer than one record could hold could never land whole; and a
// candidate's train, at the queue's greatest depth and batch (8 × 8), is the
// same sixty-four.
const MaxChainEntries = MaxPushesPerRecord

// MaxChainObjectBytes is the most bytes, UNCOMPRESSED, the objects one chain
// writes may hold — every entry's merge commit, the trees above the paths
// both sides changed and the blobs merge-tree merged — and so the most its
// one pack deflates, deltas and checks: SIXTEEN MIB, four
// [MaxWritePackBytes]. What a pack costs follows the bytes it is made of,
// not the bytes it stores, and it was MEASURED (internal/gitlet/host's
// BenchmarkBuildChain, its doc.go's WHAT A CHAIN COSTS): 314 MiB of
// sixty-four merged files of random text took 51 s to pack and check, 6.2
// MiB a second, while sixty-four merges whose objects hold just under
// sixteen took 2.1 s, inside [ChainPackDeadline] 4.8 times over. An entry
// whose merge holds more
// alone is skipped too_large; one that would take the chain past it is left
// for the next chain, as an entry its time cannot hold is
// (internal/gitlet/host's chain.go). So the bound shortens the trains of
// merges that rewrite large files and leaves the common train — sixty-four
// merges of a few kilobytes each — whole.
const MaxChainObjectBytes = 4 * MaxWritePackBytes

// ChainPackPerObjectByte is how many bytes of pack a chain may store for
// each byte its objects hold uncompressed: TWO, BY CONSTRUCTION — a pack
// holds each object deflated, or as a delta no larger, and adds to it a
// header and deflate's framing of some twenty bytes, against at least the
// twenty-eight of the tree entry that names it in the merge that wrote it (a
// commit, the one object no tree names, holds more than a hundred). The
// builder checks its pack against it all the same, and answers a pack past
// it as an error of its own, never a refusal of the chain.
const ChainPackPerObjectByte = 2

// MaxChainPackBytes bounds the one pack a chain stores:
// [ChainPackPerObjectByte] × [MaxChainObjectBytes], 32 MiB.
const MaxChainPackBytes = ChainPackPerObjectByte * MaxChainObjectBytes

// ChainPackDeadline is the git run time a chain's one pack is given — its
// `pack-objects` and its `index-pack --strict` together, and the pack made
// again after an entry whose object git's check refused is dropped: TEN
// SECONDS. MEASURED (internal/gitlet/host's BenchmarkBuildChain, its doc.go's
// WHAT A CHAIN COSTS): the largest pack a chain makes, of sixty-four merges
// whose objects hold just under [MaxChainObjectBytes], took 2.1 s, so ten
// hold it 4.8 times over — the margin for a node whose two cores are shared
// and for a second pack after an entry git's check refused. A pack past it
// is the node failing (unready), never a chain too large. PROVISIONAL as
// every build deadline is: what moves it is a production pack whose p99
// nears it.
const ChainPackDeadline = 10 * time.Second

// ChainBuildDeadline is a chain's whole git: its merges, given ONE merge's
// [MergeBuildDeadline] together — the first entry it reaches all of it, as a
// merge_change of it alone is given, so every chain decides at least one
// entry; each later one what is left of it, in run time and in time since
// the merges began, whichever is less, so a chain whose git waits for its
// slots stops as surely as one whose git runs slowly — and its pack
// [ChainPackDeadline]: FORTY SECONDS. A chain is built in its home's turn,
// one internal/gitlet/home Backstop (a minute; its TestATurnHoldsAWholeChain
// holds the two together), and the twenty seconds the turn has past the
// chain are its reads before it, the bundle's store and the candidate record
// after it. An entry the merges' share cannot hold is left for the next
// chain, never a reason to refuse this one.
const ChainBuildDeadline = MergeBuildDeadline + ChainPackDeadline

// ChainBuild is one chain a repository's home asks its Gitlet node to build:
// Entries merged in order onto Base, the tip of TargetRef the caller read at
// Seen.
type ChainBuild struct {
	RepoID string
	// ChainID names the chain's scratch and its stored bundle: a canonical
	// UUIDv7, derived by the caller from what the chain is — its
	// candidate's id — so a chain built again is the same scratch name and
	// the same pack id.
	ChainID   string
	TargetRef string
	// Base is the target's tip the first entry is merged onto.
	Base string
	// Seen is the code log's position the caller read the queue's rows at:
	// the copy is synced to it before anything is built, so every head and
	// the base are in it.
	Seen    statelog.Position
	Entries []ChainEntry
}

// ChainEntry is one change a chain merges, as its queue entry holds it.
type ChainEntry struct {
	// EntryID is the queue entry ([QueueEntryID]): its instant — the enqueue's
	// own — is the commit's author date.
	EntryID string
	Change  ChainChange
	// Head is the change's head the entry was enqueued at; Method how it
	// lands.
	Head   string
	Method MergeMethod
	// Merger is who enqueued it: a merge commit's author (U43 a).
	Merger Principal
}

// ChainChange is what of a change its chain's commit is written from: its
// number and title in a merge commit's subject, its source branch in its
// body, and — for a squash — its description and the authors it names, the
// owner first.
type ChainChange struct {
	ID        string
	Number    int64
	Title     string
	Body      string
	SourceRef string
	Owner     string
	// Authors is every author of the change by handle, in the order they
	// first wrote it ([ChangeAuthor]); the owner among them or not.
	Authors []string
}

// ChainChangeOf is d as a chain's commit of it is written.
func ChainChangeOf(d ChangeDetail) ChainChange {
	authors := make([]string, len(d.Authors))
	for i, a := range d.Authors {
		authors[i] = a.Author
	}
	return ChainChange{ID: d.ID, Number: d.Number, Title: d.Title, Body: d.Body, SourceRef: d.SourceRef,
		Owner: d.Owner, Authors: authors}
}

// ChainCommit is what one entry of a chain became: Commit, the commit the
// target moves to with it — "" for an entry the chain skipped — or Skipped,
// why it was skipped, with its sentence and, for a conflict, the paths as a
// merge_change's conflict answers them.
type ChainCommit struct {
	EntryID          string
	Commit           string
	Skipped          WriteRefusal
	Detail           string
	Conflicts        []Conflict
	ConflictsOmitted int
}

// ChainResult is a built chain: one [ChainCommit] an entry it reached, in
// order — a PREFIX of its entries, at least the first: an entry its
// [ChainBuildDeadline] or its [MaxChainObjectBytes] could not hold and every
// entry after it, and every entry after one whose object git's check
// refused, are left out, neither built nor skipped, for its
// caller to leave queued; Tip the commit the last entry that was not skipped
// became — Base where every entry was — and Pack the one stored bundle of
// every object the chain made, nil where it made none (every entry a
// fast-forward, or skipped). A chain is never refused whole for what it
// built. Or Status, nothing built and nothing stored: the repository's row,
// as the copy the chain was to be built over holds it, refuses every merge —
// repo_deleted, repo_archived, repo_importing or repo_mirrored, in the
// domain's one sentence for the state (Detail) — which the candidate's decide
// would refuse from the same row once its scratch, its share unit and its
// pack were spent.
//
// Settle ends the chain's hold on its scratch, once: applied — the record
// naming the pack has applied, as its caller read it — adopts the pack into
// the building node's cache first. It is never nil on a chain that answers no
// error, and its every call after the first does nothing. A chain never
// settled holds its scratch until the node's next boot empties scratch/.
//
// DropEpoch is the repository's drop epoch as the copy the chain was built
// from held it (internal/gitlet/cache's Synced.DropEpoch): the epoch the
// candidate record naming its pack states ([QueueProposal.DropEpoch]), so
// a chain built over objects an expunge begun since left dropping is refused
// `expunging` and built again.
type ChainResult struct {
	Commits   []ChainCommit
	Pack      *PackRef
	Tip       string
	DropEpoch int64
	Status    MergeRefusal
	Detail    string
	Settle    func(ctx context.Context, applied bool) error
}

// Validate refuses a chain no Gitlet node may build ([ErrInvalid], naming the
// field): the one rule the home that asks it and the builder both hold it to.
func (b ChainBuild) Validate() error {
	if id, err := uuid.Parse(b.RepoID); err != nil || id.String() != b.RepoID {
		return fmt.Errorf("%w: repo_id: %q is not a repository's id", ErrInvalid, b.RepoID)
	}
	if id, err := uuid.Parse(b.ChainID); err != nil || id.String() != b.ChainID || id.Version() != 7 {
		return fmt.Errorf("%w: chain_id: %q is not a canonical UUIDv7, which names the chain's scratch and "+
			"its pack", ErrInvalid, b.ChainID)
	}
	if branch, ok := strings.CutPrefix(b.TargetRef, "refs/heads/"); !ok || !gitref.ValidBranch(branch) {
		return fmt.Errorf("%w: target_ref: %q is not a branch", ErrInvalid, b.TargetRef)
	}
	if !gitref.ValidOID(b.Base) || b.Base == ZeroOID {
		return fmt.Errorf("%w: base: %q is not a whole commit id", ErrInvalid, b.Base)
	}
	if b.Seen.IsZero() {
		return fmt.Errorf("%w: seen: a chain is built from rows read at a position of the %s log", ErrInvalid,
			topics.CodeLogStream)
	}
	if n := len(b.Entries); n == 0 || n > MaxChainEntries {
		return fmt.Errorf("%w: entries: %d, where a chain merges 1 to %d", ErrInvalid, n, MaxChainEntries)
	}
	seen := make(map[string]bool, len(b.Entries))
	for i, e := range b.Entries {
		if err := e.validate(); err != nil {
			return fmt.Errorf("%w: entries[%d].%w", ErrInvalid, i, err)
		}
		if seen[e.EntryID] {
			return fmt.Errorf("%w: entries[%d].entry_id: %s is in the chain twice", ErrInvalid, i, e.EntryID)
		}
		seen[e.EntryID] = true
	}
	return nil
}

// validate is why e is no entry a chain merges, as an error naming its field.
func (e ChainEntry) validate() error {
	if at, ok := statelog.OpMintedAt(e.EntryID); !ok || at.IsZero() {
		return fmt.Errorf("entry_id: %q carries no instant, which is its commit's author date", e.EntryID)
	}
	switch {
	case e.Change.Number < 1:
		return fmt.Errorf("change.number: %d, where a change is numbered from 1", e.Change.Number)
	case strings.IndexByte(e.Change.Title, 0) >= 0 || strings.IndexByte(e.Change.Body, 0) >= 0:
		return fmt.Errorf("change: its title or body holds a NUL byte, which git's own check of a commit refuses")
	case !gitref.ValidOID(e.Head) || e.Head == ZeroOID:
		return fmt.Errorf("head: %q is not a whole commit id", e.Head)
	case !e.Method.Valid():
		return fmt.Errorf("method: %q is not one of %s", e.Method, words.Or(MergeMethods()))
	case e.Method == MergeMethodSquash && e.Change.Owner == "":
		return fmt.Errorf("change.owner: a squash is authored by its change's owner, and this one names none")
	}
	if why := e.Merger.invalid(); why != "" {
		return fmt.Errorf("merger: %s", why)
	}
	return nil
}

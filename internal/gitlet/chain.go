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

// A CHAIN OF MERGES, built in one scratch on a repository's home
// (internal/gitlet/host's Server.BuildChain): a queue candidate's entries are
// merged in order, each by a merge_change's own build onto the commit the one
// before it made, so a train is one scratch, one pack and one stored bundle,
// as a group commit is one record. A chain is a pure function of [ChainBuild]
// and its copy's objects (an entry's author date is its enqueue's instant,
// [QueueEntryID]), so a rebuild on any Gitlet node, or a bisect's second
// build of a prefix, is the same chain byte for byte. The build reads no row:
// the caller formed it from the rows it decided on, and its record's decide
// holds them again. These types are in-process only, never encoded onto a
// log; the candidate record its caller publishes carries the code domain's
// format.

// A chain is never refused whole: every refusal names ONE entry, so it always
// decides at least its first and its caller has an entry to eject or a prefix
// to land, where a whole refusal would be rebuilt and refused by every turn,
// wedging the target's queue. What could sink it whole is bounded per entry
// instead ([MaxWritePackBytes] packed, [MaxChainObjectBytes] uncompressed, an
// object git's check refuses, each skipping that entry) and per chain
// ([MaxChainObjectBytes], [ChainBuildDeadline]), entries past those left out
// for the next chain ([ChainResult]). A skipped entry leaves nothing in the
// scratch, and the pack enters the building node's cache only once the record
// naming it has applied ([ChainResult.Settle]): before, it would be objects no
// row names.

// MaxChainEntries is the most entries one chain merges: [MaxPushesPerRecord],
// sixty-four. A candidate's green prefix lands by ONE record naming a push row
// an entry, so a longer chain could never land whole; the queue's greatest
// depth and batch (8 × 8) is the same sixty-four.
const MaxChainEntries = MaxPushesPerRecord

// MaxChainObjectBytes is the most bytes, UNCOMPRESSED, the objects one chain
// writes may hold (merge commits, the trees above paths both sides changed,
// merged blobs), and so the most its one pack deflates, deltas and checks:
// 16 MiB, four [MaxWritePackBytes]. A pack's cost follows its raw bytes, not
// its stored ones. MEASURED (internal/gitlet/host's BenchmarkBuildChain, its
// doc.go's WHAT A CHAIN COSTS): 314 MiB took 51 s to pack and check, while
// sixty-four merges just under this bound took 2.1 s, inside
// [ChainPackDeadline] 4.8 times over. An entry past it alone is skipped
// too_large; one that would take the chain past it waits for the next chain.
// The common train, sixty-four merges of a few kilobytes, stays whole.
const MaxChainObjectBytes = 4 * MaxWritePackBytes

// ChainPackPerObjectByte is how many bytes of pack a chain may store for each
// byte its objects hold uncompressed: TWO, BY CONSTRUCTION. A pack holds each
// object deflated or as a delta no larger, plus some twenty bytes of header
// and framing, against at least the twenty-eight of the tree entry naming it
// (a commit, which no tree names, holds over a hundred). The builder checks
// its pack against it anyway, a pack past it being its own error, never a
// refusal of the chain.
const ChainPackPerObjectByte = 2

// MaxChainPackBytes bounds the one pack a chain stores:
// [ChainPackPerObjectByte] × [MaxChainObjectBytes], 32 MiB.
const MaxChainPackBytes = ChainPackPerObjectByte * MaxChainObjectBytes

// ChainPackDeadline is the git run time a chain's one pack is given (its
// `pack-objects` and `index-pack --strict`, and the repack after an entry
// git's check refused is dropped): TEN SECONDS. MEASURED (as
// [MaxChainObjectBytes]): the largest pack took 2.1 s, and the 4.8× margin
// covers a node's two shared cores and that repack. A pack past it is the
// node failing (unready), never a chain too large. PROVISIONAL as every build
// deadline is: a production pack whose p99 nears it moves it.
const ChainPackDeadline = 10 * time.Second

// ChainBuildDeadline is a chain's whole git: FORTY SECONDS, ONE merge's
// [MergeBuildDeadline] shared by its merges plus [ChainPackDeadline]. The
// first entry gets all of the merges' share, as a lone merge_change would, so
// every chain decides one entry; each later one what is left, by run time or
// by time since the merges began, whichever is less, so a chain waiting on
// git slots stops as surely as a slow one. It fits one internal/gitlet/home
// Backstop (a minute; TestATurnHoldsAWholeChain), leaving twenty seconds for
// the reads before and the bundle store and candidate record after. An entry
// the share cannot hold is left for the next chain, never refuses this one.
const ChainBuildDeadline = MergeBuildDeadline + ChainPackDeadline

// ChainBuild is one chain a repository's home asks its Gitlet node to build:
// Entries merged in order onto Base, the tip of TargetRef the caller read at
// Seen.
type ChainBuild struct {
	RepoID string
	// ChainID names the chain's scratch and its stored bundle: a canonical
	// UUIDv7 the caller derives from its candidate's id, so a rebuild has the
	// same scratch name and pack id.
	ChainID   string
	TargetRef string
	// Base is the target's tip the first entry is merged onto.
	Base string
	// Seen is the code log position the caller read the queue's rows at; the
	// copy syncs to it before building, so every head and the base are in it.
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

// ChainResult is a built chain: Commits, one an entry it reached in order, a
// PREFIX of at least the first (every entry from one its [ChainBuildDeadline]
// or [MaxChainObjectBytes] could not hold, or after one whose object git's
// check refused, is neither built nor skipped, left queued); Tip, the last
// unskipped entry's commit, or Base; Pack, the one stored bundle, nil where no
// entry made an object. Or Status (Detail the domain's sentence), nothing
// built or stored: the copy's repository row refuses every merge
// (repo_deleted, repo_archived, repo_importing or repo_mirrored), as the
// candidate's decide would once the scratch, share unit and pack were spent.
type ChainResult struct {
	Commits []ChainCommit
	Pack    *PackRef
	Tip     string
	// DropEpoch is the repository's drop epoch in the copy the chain was
	// built from (internal/gitlet/cache's Synced.DropEpoch), which the
	// candidate record states ([QueueProposal.DropEpoch]), so a chain built
	// over objects an expunge has since begun dropping is refused `expunging`.
	DropEpoch int64
	Status    MergeRefusal
	Detail    string
	// Settle ends the chain's hold on its scratch; applied (the record naming
	// the pack has applied) adopts the pack into the building node's cache
	// first. Never nil without an error; calls after the first do nothing. An
	// unsettled chain holds its scratch until the next boot empties scratch/.
	Settle func(ctx context.Context, applied bool) error
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

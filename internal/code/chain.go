package code

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A CHAIN OF MERGES, BUILT IN ONE SCRATCH ON A REPOSITORY'S HOME (M5-D12,
// M5-D13; internal/code/host's Server.BuildChain): the queue's candidates are
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
// same chain, byte for byte, whichever git node builds it and however often
// (a bisect builds the same prefix twice). Nothing it carries is read from a
// row by the build: the caller formed it from the rows it decided on, and its
// record's decide holds those rows again.
//
// # What a chain answers
//
// One [ChainCommit] an entry, in order: the commit the entry became — a
// fast-forward's is its own head, which the copy already holds — or the
// refusal that skipped it, the chain building on without it: a conflict with
// its paths, a fast-forward the target's history refuses, a head the chain
// already holds, or a merge past its build's budget. A skipped entry leaves
// nothing behind it in the scratch. Then the chain's one pack, stored as one
// bundle — nil where no entry made an object — and its tip. The pack enters
// the building node's cache only once the record naming it has applied
// ([ChainResult.Settle]): a pack linked in before it would be objects no row
// names.
//
// # In process, never a record
//
// These are the home's call of its own git node's builder, through the
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

// MaxChainPackBytes bounds the one pack a chain stores: [MaxChainEntries] ×
// [MaxWritePackBytes], 256 MiB. Each entry is a merge a merge_change could
// build and pack alone under [MaxWritePackBytes] — its new objects a commit,
// the trees above the paths both sides changed and the blobs merge-tree
// merged — so a chain of them packs under their sum, smaller in practice as
// one pack deltas what neighbouring entries rewrite alike. A chain past it is
// refused too_large whole, its caller's to build in smaller parts.
const MaxChainPackBytes = MaxChainEntries * MaxWritePackBytes

// ChainBuild is one chain a repository's home asks its git node to build:
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

// ChainResult is a built chain: one [ChainCommit] an entry, in order; Tip the
// commit the last entry that was not skipped became — Base where every entry
// was — and Pack the one stored bundle of every object the chain made, nil
// where it made none (every entry a fast-forward, or skipped). Or Refusal,
// with nothing stored: the chain whole refused — too_large past
// [MaxChainPackBytes], rejected_object where git's own check refuses an
// object it made.
//
// Settle ends the chain's hold on its scratch, once: applied — the record
// naming the pack has applied, as its caller read it — adopts the pack into
// the building node's cache first. It is never nil on a chain that answers no
// error, and its every call after the first does nothing. A chain never
// settled holds its scratch until the node's next boot empties scratch/.
type ChainResult struct {
	Commits []ChainCommit
	Pack    *PackRef
	Tip     string
	Refusal WriteRefusal
	Detail  string
	Settle  func(ctx context.Context, applied bool) error
}

// Validate refuses a chain no git node may build ([ErrInvalid], naming the
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
		return fmt.Errorf("method: %q is not one of %v", e.Method, MergeMethods())
	case e.Method == MergeMethodSquash && e.Change.Owner == "":
		return fmt.Errorf("change.owner: a squash is authored by its change's owner, and this one names none")
	}
	if why := e.Merger.invalid(); why != "" {
		return fmt.Errorf("merger: %s", why)
	}
	return nil
}

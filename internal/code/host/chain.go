package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/toolname"
)

// A CHAIN OF MERGES, BUILT IN ONE SCRATCH (M5-D12, M5-D13; code.ChainBuild):
// the queue's candidates, asked of the repository's home Gitlet node by the
// home's own worker — one scratch named by the chain's id, each
// entry merged onto the commit the entry before it made by the very build a
// merge_change runs (merge.go's mergeOnto: its ancestry, its one merge-tree,
// its one commit-tree, U43 (a)'s identity), one pack of every object the
// chain made, checked by `index-pack --strict` as every engine write's is,
// and one bundle stored. No record is proposed here: the chain's caller
// proposes the candidate that names its commits and its pack,
// and SETTLES the chain once that record's outcome is known — adopting the
// pack into this node's cache only once the record has applied
// (code.ChainResult.Settle), since a pack linked in before would be objects
// no row names.
//
// # Each entry a merge_change's build, and held to its bounds
//
// An entry is merged as a merge_change merges its change (mergeOnto), onto
// the chain's tip so far — the target's tip the caller read for the first —
// and held to what a merge_change of it is held to, so the queue lands no
// merge a merge_change would refuse: its git to a merge's budget,
// code.MergeBuildDeadline, and the objects it wrote to a merge_change's pack
// bound, code.MaxWritePackBytes — an entry past either is skipped too_large
// in a merge_change's own words. The objects an entry wrote are the scratch's
// loose objects it added; their loose files are each object deflated whole,
// as a pack stores one at the most (a pack deflates at least as hard and
// takes a delta only where it is smaller), so an entry under the bound in
// loose bytes packs under it, and only one past it in loose bytes is packed —
// its objects alone, counted and never kept — to be measured as a
// merge_change's pack is.
//
// What a pack COSTS follows the bytes it deflates, deltas and checks, which
// its compressed size hides, so the chain holds those too: its entries'
// objects together to code.MaxChainObjectBytes UNCOMPRESSED, each object's
// size read from its loose header (looseSize). An entry holding more alone
// is skipped too_large — past what any chain may pack — and one that would
// take the chain past it is NOT REACHED (below). So no chain's pack is
// larger than the one measured to be made well within
// code.ChainPackDeadline (doc.go's WHAT A CHAIN COSTS), and every one is
// under code.MaxChainPackBytes by construction.
//
// An entry's author date is its ENTRY's
// instant — its enqueue's own (code.QueueEntryID keeps it) — and its
// committer date that or its later parent's (commitDates), so a chain built
// again from the same rows, on any Gitlet node, is the same commits byte for
// byte.
//
// # A skipped entry leaves nothing
//
// An entry that conflicts, a fast-forward its target's history refuses, a
// head the chain already holds and a merge past any bound are SKIPPED: the
// chain answers why (code.ChainCommit.Skipped, a conflict with its paths) and
// builds the next entry onto the same tip. What the skipped merge wrote into
// the scratch — a conflict's tree and its marked files — is pruned before the
// next entry (building.prune), so the pack holds exactly the commits the chain
// made and what they need.
//
// # One budget for the chain, and a prefix where it runs out
//
// The chain is built in its home's turn, one internal/code/home Backstop, so
// its git has ONE budget, code.ChainBuildDeadline: its merges share one
// merge's code.MergeBuildDeadline, and its pack has code.ChainPackDeadline of
// its own. The FIRST entry is given a merge's whole budget, as a merge_change
// of it alone is — so every chain decides at least one entry, built or
// skipped, and a queue whose head entry is slow still moves. Each later entry
// is given what is left of the merges' share, by the run time its merges
// were charged and by the time since they began, whichever is less: a node
// whose git waits on its slots charges nothing for the waits, and the turn's
// minute passes all the same. An entry not started for want of it, or cut by
// it short of a merge's whole budget, is NOT REACHED — pruned like a skipped
// one, left out of the answer with every entry after it, and left queued by
// the caller for the next chain. Only an entry that ran out a merge's whole
// budget is too_large, as a merge_change of it would be. So is an entry the
// chain's objects cannot hold — under code.MaxChainObjectBytes alone, past it
// with the entries built before it — not reached.
//
// # One pack, checked once, an entry's object refused alone
//
// The pack is made once every reached entry has been merged. An object
// `index-pack --strict` refuses is the one entry's that wrote it: that entry
// is skipped rejected_object in git's own words, it and every entry after it
// are pruned — the entries after it were merged onto its commit — the entries
// after it are not reached, and the pack is made again, within what is left of
// code.ChainPackDeadline. A pack that runs that out is the NODE failing —
// code.ChainPackDeadline holds the largest pack a chain can make 4.8 times
// over (doc.go's WHAT A CHAIN COSTS) — so it is unready node_failed,
// never a refusal of the chain: no answer here refuses a chain whole, since
// its caller would only build the same chain again and be refused again,
// wedging its target's queue.
//
// The whole build holds ONE unit of the node's
// seat git-work share (code.SeatWorkWrite): the chain is the queue's batching
// of the merges its enqueuers asked for, each of which would hold a unit for
// its own build — so one unit for the batch, never one for each entry, and
// the chain waits for it as a merge does, unready past code.GitShareWait.

// BuildChain builds b in one scratch on this node: see this file's head. It
// answers the chain — every reached entry's commit or why it was skipped, its
// tip and its stored pack, with its Settle — or refused by its repository's
// row before anything is built (code.ChainResult.Status,
// [Server.mergeStatusRefusal]); or an error about this node or
// its store: code.ErrNotServing for a host that is stopping,
// code.ErrGitUnready for a copy it could not sync, a share it could not take
// or a pack it could not make within its budget, code.ErrRepoNotFound for a
// repository the rows hold no live row of, and the store's own failure — an
// *objstore.Full among them — for a bundle it could not store, which nothing
// names.
func (s *Server) BuildChain(ctx context.Context, b code.ChainBuild) (code.ChainResult, error) {
	if err := b.Validate(); err != nil {
		return code.ChainResult{}, err
	}
	ctx, end, ok := s.enter(ctx)
	if !ok {
		return code.ChainResult{}, s.writesStopping()
	}
	defer end()
	started := s.now()
	// A CHAIN IS A RUN OF MERGES, and its writing is a merge_change's as far
	// as its build reads one: its repository, its target, the budget its
	// kind is given and the words a merge past it is refused in.
	wr := &writing{s: s, pushID: b.ChainID, started: started,
		w: code.EngineWrite{Kind: code.WriteMergeChange, RepoID: b.RepoID, Ref: b.TargetRef}}
	defer func() { wr.synced.Release() }()
	gone, err := wr.sync(ctx, statelog.FloorAt(statelog.ReadSession, b.Seen))
	switch {
	case err != nil:
		return code.ChainResult{}, err
	case gone:
		return code.ChainResult{}, fmt.Errorf("%w: chain %s: repository %s has no live row at %v",
			code.ErrRepoNotFound, b.ChainID, b.RepoID, b.Seen)
	}
	// THE REPOSITORY'S STATUS, BEFORE ANYTHING IS BUILT: a chain is a run of
	// merges, which a repository being imported, a pull mirror, an archived
	// one and one being deleted take none of — its candidate is refused from
	// the same row — so it is refused here, as a merge is, with no scratch, no
	// share unit and no stored pack.
	if refusal, words := s.mergeStatusRefusal(wr.synced.Repo); refusal != "" {
		return code.ChainResult{Status: refusal, Detail: words,
			Settle: func(context.Context, bool) error { return nil }}, nil
	}
	sc, err := s.o.Cache.NewScratch(ctx, wr.synced, b.ChainID)
	if err != nil {
		if errors.Is(err, cache.ErrStopped) {
			return code.ChainResult{}, s.writesStopping()
		}
		return code.ChainResult{}, fmt.Errorf("host: a scratch for chain %s: %w", b.ChainID, err)
	}
	var once sync.Once
	settle := func(ctx context.Context, adopt *code.PackRef) (err error) {
		once.Do(func() {
			if adopt != nil {
				err = s.o.Cache.Adopt(ctx, b.RepoID, sc, *adopt)
			}
			sc.Remove()
		})
		return err
	}
	res, err := wr.buildChain(ctx, sc, b)
	if err != nil {
		_ = settle(ctx, nil)
		return code.ChainResult{}, err
	}
	if err = s.fault(ctx, codefault.ChainAfterBuild, 0); err != nil {
		_ = settle(ctx, nil)
		return code.ChainResult{}, fmt.Errorf("host: chain %s after its build: %w", b.ChainID, err)
	}
	if res.Pack, err = s.storeChain(ctx, sc, b); err != nil {
		_ = settle(ctx, nil)
		return code.ChainResult{}, err
	}
	pack := res.Pack
	// THE EPOCH OF THE COPY the chain was built from, which the candidate
	// record naming its pack states (code.QueueProposal.DropEpoch): its
	// committer refuses it `expunging` once an expunge has begun since.
	res.DropEpoch = wr.synced.DropEpoch
	res.Settle = func(ctx context.Context, applied bool) error {
		if !applied || pack == nil {
			return settle(ctx, nil)
		}
		return settle(ctx, pack)
	}
	skipped := 0
	for _, c := range res.Commits {
		if c.Skipped != "" {
			skipped++
		}
	}
	var packBytes int64
	if pack != nil {
		packBytes = pack.Size
	}
	s.log.Info("code_chain_built", "repo", wr.repoName(), "chain", b.ChainID, "target", b.TargetRef,
		"entries", len(b.Entries), "skipped", skipped, "not_reached", len(b.Entries)-len(res.Commits),
		"tip", res.Tip, "pack_bytes", packBytes, "took", s.now().Sub(started))
	return res, nil
}

// chainMade is what the chain had made before one entry of its answer: the
// scratch's loose objects and the tip the entry was merged onto, and the
// objects the entry added to them, sorted — none for a skipped entry.
type chainMade struct {
	kept  []string
	tip   string
	added []string
}

// buildChain is the chain's build in sc under one unit of the node's seat
// git-work share: each entry merged onto the tip so far within the chain's
// budget — skipped, and what it wrote pruned, where its merge was refused;
// the chain stopped where its budget cannot hold the next — then its one
// pack (packChain).
func (wr *writing) buildChain(ctx context.Context, sc *cache.Scratch, b code.ChainBuild) (code.ChainResult,
	error) {

	s := wr.s
	release, err := s.o.Share.Take(ctx, code.SeatWorkWrite)
	if err != nil {
		return code.ChainResult{}, err
	}
	defer release()
	bd, err := wr.newBuilding(sc)
	if err != nil {
		return code.ChainResult{}, err
	}
	objects := filepath.Join(sc.Dir, "objects")
	kept, err := looseObjects(objects)
	if err != nil {
		return code.ChainResult{}, err
	}
	out := code.ChainResult{Tip: b.Base, Commits: make([]code.ChainCommit, 0, len(b.Entries))}
	made := make([]chainMade, 0, len(b.Entries))
	began, ran, wrote := s.now(), time.Duration(0), int64(0)
	for i, e := range b.Entries {
		budget, whole := s.buildBudget(code.WriteMergeChange), true
		if i > 0 {
			left := s.chainMerges - max(ran, s.now().Sub(began))
			if left <= 0 {
				break
			}
			if left < budget {
				budget, whole = left, false
			}
		}
		at, _ := statelog.OpMintedAt(e.EntryID)
		wr.metered, wr.left = true, budget
		commit, refused, done, err := bd.mergeOnto(ctx, mergeOf{change: e.Change, target: b.TargetRef,
			tip: out.Tip, head: e.Head, method: e.Method, merger: e.Merger, instant: at})
		var (
			after, added []string
			raw          int64
		)
		if err == nil && !done {
			after, added, raw, refused, done, err = bd.entryObjects(ctx, objects, kept, s.chainObjects)
		}
		wr.metered = false
		ran += budget - wr.left
		if errors.Is(err, errBuildSpent) {
			if !whole {
				// THE CHAIN'S TIME RAN OUT, NOT THE ENTRY'S: it had less
				// than a merge is given, so it is no larger for it — it and
				// the rest are the next chain's.
				if err = bd.prune(kept); err != nil {
					return code.ChainResult{}, err
				}
				break
			}
			refused, done, err = wr.spent(), true, nil
		}
		if err != nil {
			return code.ChainResult{}, err
		}
		if done {
			if err := bd.prune(kept); err != nil {
				return code.ChainResult{}, err
			}
			made = append(made, chainMade{kept: kept, tip: out.Tip})
			out.Commits = append(out.Commits, code.ChainCommit{EntryID: e.EntryID, Skipped: refused.Refusal,
				Detail: refused.Detail, Conflicts: refused.Conflicts, ConflictsOmitted: refused.ConflictsOmitted})
			continue
		}
		if wrote+raw > s.chainObjects {
			// THE CHAIN'S PACK IS FULL, NOT THE ENTRY'S: the entry is under
			// the bound alone (entryObjects), so it is never the first, and
			// it and the rest are the next chain's.
			if err := bd.prune(kept); err != nil {
				return code.ChainResult{}, err
			}
			break
		}
		wrote += raw
		made = append(made, chainMade{kept: kept, tip: out.Tip, added: added})
		out.Commits = append(out.Commits, code.ChainCommit{EntryID: e.EntryID, Commit: commit})
		out.Tip, kept = commit, after
	}
	return wr.packChain(ctx, bd, b, out, made)
}

// entryObjects is what the entry just merged added to the scratch's loose
// objects, kept before it: every loose object now and the ones that are new,
// both sorted, and the bytes the new ones hold uncompressed — or too_large,
// where they pack past a merge_change's own bound (this file's head: measured
// by their loose bytes, and packed to be measured only past it), or hold more
// than bound uncompressed, the most one chain's pack is given to pack and
// check (code.MaxChainObjectBytes).
func (b *building) entryObjects(ctx context.Context, objects string, kept []string, bound int64) (after,
	added []string, raw int64, refused code.EngineWriteResult, done bool, err error) {

	if after, err = looseObjects(objects); err != nil {
		return nil, nil, 0, code.EngineWriteResult{}, true, err
	}
	var loose int64
	for _, id := range after {
		if _, found := slices.BinarySearch(kept, id); found {
			continue
		}
		added = append(added, id)
		path := filepath.Join(objects, id[:2], id[2:])
		info, err := os.Stat(path)
		if err != nil {
			return nil, nil, 0, code.EngineWriteResult{}, true, err
		}
		loose += info.Size()
		size, err := looseSize(path)
		if err != nil {
			return nil, nil, 0, code.EngineWriteResult{}, true, err
		}
		raw += size
	}
	if loose > code.MaxWritePackBytes {
		var size counted
		if err := b.packObjects(ctx, added, &size, "pack of a chain entry's objects"); err != nil {
			return nil, nil, 0, code.EngineWriteResult{}, true, err
		}
		if int64(size) > code.MaxWritePackBytes {
			return nil, nil, 0, b.wr.changeRefused(code.WriteTooLarge, writePackTooLarge(int64(size))), true, nil
		}
	}
	if raw > bound {
		return nil, nil, 0, b.wr.changeRefused(code.WriteTooLarge, fmt.Sprintf("this change's merge writes %s "+
			"of objects uncompressed, past the %s one train of the merge queue packs and checks in its time "+
			"(code.MaxChainObjectBytes); merge its target into its branch in a checkout of the repository ("+
			toolname.CodeCheckout+"), so "+
			"its merge writes no file of its own", humanBytes(raw), humanBytes(bound))), true, nil
	}
	return after, added, raw, code.EngineWriteResult{}, false, nil
}

// packChain is the chain's one pack, of out's entries, within
// code.ChainPackDeadline: an object git's check refuses skips the entry of
// made that wrote it — the entries after it not reached — and the pack is
// made again; a pack its budget cannot hold is the node's failure.
func (wr *writing) packChain(ctx context.Context, bd *building, b code.ChainBuild, out code.ChainResult,
	made []chainMade) (code.ChainResult, error) {

	s := wr.s
	wr.metered, wr.left = true, s.chainPack
	defer func() { wr.metered = false }()
	// THE PACK'S BOUND FOLLOWS ITS OBJECTS' (code.MaxChainPackBytes at the
	// default), so a suite that scales one scales both.
	bound := code.ChainPackPerObjectByte * s.chainObjects
	for {
		refused, object, done, err := bd.packWithin(ctx, bound, func(size int64) string {
			return fmt.Sprintf("chain %s's pack is %s, past the %s (code.MaxChainPackBytes) a chain's objects "+
				"of at most %s may pack to", b.ChainID, humanBytes(size), humanBytes(bound),
				humanBytes(s.chainObjects))
		})
		switch {
		case errors.Is(err, errBuildSpent):
			return code.ChainResult{}, wr.unready(code.UnreadyNodeFailed, fmt.Sprintf("%s's pack of chain %s "+
				"over repository %s ran the whole %s of git a chain's pack is given (code.ChainPackDeadline), "+
				"several times what the largest pack a chain makes takes; the next turn builds it again",
				s.o.NodeID, b.ChainID, wr.repoName(), s.chainPack))
		case err != nil:
			return code.ChainResult{}, err
		case !done:
			return out, nil
		case refused.Refusal != code.WriteRejectedObject:
			// UNREACHABLE BY CONSTRUCTION (code.MaxChainPackBytes): an
			// invariant broken, so this node's error, never the chain's.
			return code.ChainResult{}, fmt.Errorf("host: %s", refused.Detail)
		}
		k := slices.IndexFunc(made, func(m chainMade) bool {
			_, found := slices.BinarySearch(m.added, object)
			return found
		})
		if k < 0 {
			return code.ChainResult{}, fmt.Errorf("host: git's check refused object %q of chain %s, which no "+
				"entry of it wrote: %s", object, b.ChainID, refused.Detail)
		}
		if err := bd.prune(made[k].kept); err != nil {
			return code.ChainResult{}, err
		}
		out.Tip = made[k].tip
		out.Commits = append(out.Commits[:k], code.ChainCommit{EntryID: out.Commits[k].EntryID,
			Skipped: code.WriteRejectedObject, Detail: refused.Detail})
		made = made[:k]
	}
}

// storeChain stores the chain's one pack, as one bundle under the chain's id,
// and answers it — nil for a chain that made no object. Nothing names it
// until its caller's record does; a bundle stored for a record that never
// applies is the collector's to remove, as a refused push's is. A bundle its
// own seal refused is logged here, naming the chain.
func (s *Server) storeChain(ctx context.Context, sc *cache.Scratch, b code.ChainBuild) (*code.PackRef, error) {
	packPath, checksum, held, err := sc.Pack()
	if err != nil || !held {
		return nil, err
	}
	ref, err := code.LayBundle(b.ChainID, packPath, checksum)
	if err != nil {
		return nil, err
	}
	if err := code.StoreBundle(ctx, s.o.Objects, packPath, ref, nil); err != nil {
		var bad *code.BadBundle
		if errors.As(err, &bad) {
			s.log.Error("code_chain_pack_rejected", "repo_id", b.RepoID, "chain", b.ChainID,
				"object", bad.Object.String(), "error", bad.Err)
		}
		return nil, fmt.Errorf("host: storing chain %s's pack: %w", b.ChainID, err)
	}
	return ref, nil
}

package host

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A CHAIN OF MERGES, BUILT IN ONE SCRATCH (M5-D12, M5-D13; code.ChainBuild):
// the queue's candidates, asked of the repository's home git node by the
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
// # Each entry a merge_change's build
//
// An entry is merged as a merge_change merges its change (mergeOnto), onto
// the chain's tip so far — the target's tip the caller read for the first —
// under ITS OWN budget of git run time, code.MergeBuildDeadline: each entry
// is the one merge-tree a merge_change of it would run, so it is given what
// that merge is given, and an entry past it is skipped too_large rather than
// sinking the entries after it. Its author date is its ENTRY's instant — its
// enqueue's own (code.QueueEntryID keeps it) — and its committer date that or
// its later parent's (commitDates), so a chain built again from the same
// rows, on any git node, is the same commits byte for byte.
//
// # A skipped entry leaves nothing
//
// An entry that conflicts, a fast-forward its target's history refuses, a
// head the chain already holds and a merge past its budget are SKIPPED: the
// chain answers why (code.ChainCommit.Skipped, a conflict with its paths) and
// builds the next entry onto the same tip. What the skipped merge wrote into
// the scratch — a conflict's tree and its marked files — is pruned before the
// next entry (building.prune), so the pack holds exactly the commits the chain
// made and what they need.
//
// # One pack, one budget for it
//
// The pack is made once every entry has been merged, under one budget of its
// own, a merge's (code.MergeBuildDeadline): it packs and checks the commits
// and trees of at most code.MaxChainEntries merges, which takes a fraction of
// a second, and a pack whose making runs it out is too_large whole, as is one
// past code.MaxChainPackBytes. The whole build holds ONE unit of the node's
// seat git-work share (code.SeatWorkWrite): the chain is the queue's batching
// of the merges its enqueuers asked for, each of which would hold a unit for
// its own build — so one unit for the batch, never one for each entry, and
// the chain waits for it as a merge does, unready past code.GitShareWait.

// BuildChain builds b in one scratch on this node: see this file's head. It
// answers the chain — every entry's commit or why it was skipped, its tip and
// its stored pack, with its Settle — or the chain refused whole
// (code.ChainResult.Refusal), nothing stored; or an error about this node or
// its store: code.ErrNotServing for a host that is stopping,
// code.ErrGitUnready for a copy it could not sync or a share it could not
// take, code.ErrRepoNotFound for a repository the rows hold no live row of,
// and the store's own failure — an *objstore.Full among them — for a bundle
// it could not store, which nothing names.
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
	if err != nil || res.Refusal != "" {
		_ = settle(ctx, nil)
		res.Settle = func(context.Context, bool) error { return nil }
		return res, err
	}
	if err := s.fault(ctx, codefault.ChainAfterBuild, 0); err != nil {
		_ = settle(ctx, nil)
		return code.ChainResult{}, fmt.Errorf("host: chain %s after its build: %w", b.ChainID, err)
	}
	if res.Pack, err = s.storeChain(ctx, sc, b); err != nil {
		_ = settle(ctx, nil)
		return code.ChainResult{}, err
	}
	pack := res.Pack
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
		"entries", len(b.Entries), "skipped", skipped, "tip", res.Tip, "pack_bytes", packBytes,
		"took", s.now().Sub(started))
	return res, nil
}

// buildChain is the chain's build in sc under one unit of the node's seat
// git-work share: each entry merged onto the tip so far under its own budget
// — skipped, and what it wrote pruned, where its merge was refused — then
// the one pack under a budget of its own.
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
	out := code.ChainResult{Tip: b.Base, Commits: make([]code.ChainCommit, 0, len(b.Entries))}
	for _, e := range b.Entries {
		kept, err := looseObjects(filepath.Join(sc.Dir, "objects"))
		if err != nil {
			return code.ChainResult{}, err
		}
		at, _ := statelog.OpMintedAt(e.EntryID)
		wr.metered, wr.left = true, s.buildBudget(code.WriteMergeChange)
		commit, refused, done, err := bd.mergeOnto(ctx, mergeOf{change: e.Change, target: b.TargetRef,
			tip: out.Tip, head: e.Head, method: e.Method, merger: e.Merger, instant: at})
		wr.metered = false
		if errors.Is(err, errBuildSpent) {
			refused, done, err = wr.spent(), true, nil
		}
		if err != nil {
			return code.ChainResult{}, err
		}
		if done {
			if err := bd.prune(kept); err != nil {
				return code.ChainResult{}, err
			}
			out.Commits = append(out.Commits, code.ChainCommit{EntryID: e.EntryID, Skipped: refused.Refusal,
				Detail: refused.Detail, Conflicts: refused.Conflicts, ConflictsOmitted: refused.ConflictsOmitted})
			continue
		}
		out.Commits = append(out.Commits, code.ChainCommit{EntryID: e.EntryID, Commit: commit})
		out.Tip = commit
	}
	wr.metered, wr.left = true, s.buildBudget(code.WriteMergeChange)
	refused, done, err := bd.packWithin(ctx, code.MaxChainPackBytes, func(size int64) string {
		return fmt.Sprintf("this chain's pack is %s, past the %s one chain may store (code.MaxChainPackBytes); "+
			"build it in shorter chains", humanBytes(size), humanBytes(code.MaxChainPackBytes))
	})
	wr.metered = false
	if errors.Is(err, errBuildSpent) {
		refused, done, err = wr.changeRefused(code.WriteTooLarge, fmt.Sprintf("packing this chain ran the "+
			"whole %s of git it is given on %s; build it in shorter chains", s.buildBudget(code.WriteMergeChange),
			s.o.NodeID)), true, nil
	}
	switch {
	case err != nil:
		return code.ChainResult{}, err
	case done:
		return code.ChainResult{Commits: out.Commits, Tip: out.Tip, Refusal: refused.Refusal,
			Detail: refused.Detail}, nil
	}
	return out, nil
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

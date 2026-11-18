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

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitletfault"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/toolname"
)

// A CHAIN OF MERGES, BUILT IN ONE SCRATCH (M5-D12, M5-D13; gitlet.ChainBuild):
// the queue's candidates, built on the repository's home Gitlet node by its own
// worker in one turn (an internal/gitlet/home Backstop), so its git has one
// budget, gitlet.ChainBuildDeadline. Each entry is merged onto the commit the entry before made by a
// merge_change's own build (merge.go's mergeOnto) and held to a merge_change's
// bounds, so the queue lands no merge a merge_change would refuse. The chain
// makes one pack, checked by `index-pack --strict`, and stores one bundle. No
// record is proposed here: the caller proposes the candidate and settles the
// chain (gitlet.ChainResult.Settle), which adopts the pack into this node's
// cache only once the record has applied, since a pack linked in before is
// objects no row names. No answer refuses a chain whole: its caller would build
// the same chain again and be refused again, wedging its target's queue.

// BuildChain builds b in one scratch on this node (this file's head). It
// answers every reached entry's commit or why it was skipped, the tip, the
// stored pack and its Settle; or a refusal by the repository's row before
// anything is built (gitlet.ChainResult.Status, [Server.mergeStatusRefusal]);
// or an error about this node or its store: gitlet.ErrNotServing while
// stopping, gitlet.ErrGitUnready for a copy not synced, a share not taken or a
// pack not made within its budget, gitlet.ErrRepoNotFound for a repository with
// no live row, and the store's own failure (an *objstore.Full among them) for a
// bundle not stored, which nothing names.
func (s *Server) BuildChain(ctx context.Context, b gitlet.ChainBuild) (gitlet.ChainResult, error) {
	if err := b.Validate(); err != nil {
		return gitlet.ChainResult{}, err
	}
	ctx, end, ok := s.enter(ctx)
	if !ok {
		return gitlet.ChainResult{}, s.writesStopping()
	}
	defer end()
	started := s.now()
	// A chain is a run of merges: its build reads a merge_change's repository,
	// target, budget and refusal words off its writing.
	wr := &writing{s: s, pushID: b.ChainID, started: started,
		w: gitlet.EngineWrite{Kind: gitlet.WriteMergeChange, RepoID: b.RepoID, Ref: b.TargetRef}}
	defer func() { wr.synced.Release() }()
	gone, err := wr.sync(ctx, statelog.FloorAt(statelog.ReadSession, b.Seen))
	switch {
	case err != nil:
		return gitlet.ChainResult{}, err
	case gone:
		return gitlet.ChainResult{}, fmt.Errorf("%w: chain %s: repository %s has no live row at %v",
			gitlet.ErrRepoNotFound, b.ChainID, b.RepoID, b.Seen)
	}
	// Refused by the repository's status before anything is built, as a merge
	// is (its candidate would be refused from the same row): no scratch, no
	// share unit, no stored pack.
	if refusal, words := s.mergeStatusRefusal(wr.synced.Repo); refusal != "" {
		return gitlet.ChainResult{Status: refusal, Detail: words,
			Settle: func(context.Context, bool) error { return nil }}, nil
	}
	sc, err := s.o.Cache.NewScratch(ctx, wr.synced, b.ChainID)
	if err != nil {
		if errors.Is(err, cache.ErrStopped) {
			return gitlet.ChainResult{}, s.writesStopping()
		}
		return gitlet.ChainResult{}, fmt.Errorf("host: a scratch for chain %s: %w", b.ChainID, err)
	}
	var once sync.Once
	settle := func(ctx context.Context, adopt *gitlet.PackRef) (err error) {
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
		return gitlet.ChainResult{}, err
	}
	if err = s.fault(ctx, gitletfault.ChainAfterBuild, 0); err != nil {
		_ = settle(ctx, nil)
		return gitlet.ChainResult{}, fmt.Errorf("host: chain %s after its build: %w", b.ChainID, err)
	}
	if res.Pack, err = s.storeChain(ctx, sc, b); err != nil {
		_ = settle(ctx, nil)
		return gitlet.ChainResult{}, err
	}
	pack := res.Pack
	// The candidate states the epoch of the copy built from
	// (gitlet.QueueProposal.DropEpoch); its committer refuses it `expunging`
	// once an expunge has begun since.
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
// scratch's loose objects, the tip the entry was merged onto, and the objects
// the entry added, sorted (none for a skipped entry).
type chainMade struct {
	kept  []string
	tip   string
	added []string
}

// buildChain merges b's entries in sc, then packs them (packChain), under ONE
// unit of the seat git-work share (gitlet.SeatWorkWrite) for the batch, as one
// merge holds, unready past gitlet.GitShareWait. A skipped entry is pruned
// (building.prune) and the next merged onto the same tip, so the pack holds
// only the chain's commits. The merges share one gitlet.MergeBuildDeadline of
// gitlet.ChainBuildDeadline: the first gets a merge's whole budget, so every
// chain decides at least one entry; each later one what is left, by the run
// time charged or the time since the merges began, whichever is less (git
// waiting on its slots charges nothing). An entry cut short of a merge's whole
// budget, or that would take the chain past its object bound, is NOT REACHED:
// pruned and left queued, with every later entry, for the next chain. Only an
// entry that ran out a merge's whole budget is too_large.
func (wr *writing) buildChain(ctx context.Context, sc *cache.Scratch, b gitlet.ChainBuild) (gitlet.ChainResult,
	error) {

	s := wr.s
	release, err := s.o.Share.Take(ctx, gitlet.SeatWorkWrite)
	if err != nil {
		return gitlet.ChainResult{}, err
	}
	defer release()
	bd, err := wr.newBuilding(sc)
	if err != nil {
		return gitlet.ChainResult{}, err
	}
	objects := filepath.Join(sc.Dir, "objects")
	kept, err := looseObjects(objects)
	if err != nil {
		return gitlet.ChainResult{}, err
	}
	out := gitlet.ChainResult{Tip: b.Base, Commits: make([]gitlet.ChainCommit, 0, len(b.Entries))}
	made := make([]chainMade, 0, len(b.Entries))
	began, ran, wrote := s.now(), time.Duration(0), int64(0)
	for i, e := range b.Entries {
		budget, whole := s.buildBudget(gitlet.WriteMergeChange), true
		if i > 0 {
			left := s.chainMerges - max(ran, s.now().Sub(began))
			if left <= 0 {
				break
			}
			if left < budget {
				budget, whole = left, false
			}
		}
		// Dated by its entry's instant (commitDates), never the build's, so a
		// chain rebuilt from the same rows on any node is the same bytes.
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
				// The chain's time ran out, not the entry's: it and the rest
				// are the next chain's.
				if err = bd.prune(kept); err != nil {
					return gitlet.ChainResult{}, err
				}
				break
			}
			refused, done, err = wr.spent(), true, nil
		}
		if err != nil {
			return gitlet.ChainResult{}, err
		}
		if done {
			if err := bd.prune(kept); err != nil {
				return gitlet.ChainResult{}, err
			}
			made = append(made, chainMade{kept: kept, tip: out.Tip})
			out.Commits = append(out.Commits, gitlet.ChainCommit{EntryID: e.EntryID, Skipped: refused.Refusal,
				Detail: refused.Detail, Conflicts: refused.Conflicts, ConflictsOmitted: refused.ConflictsOmitted})
			continue
		}
		if wrote+raw > s.chainObjects {
			// The chain is full, not the entry (under the bound alone, so never
			// the first): it and the rest are the next chain's.
			if err := bd.prune(kept); err != nil {
				return gitlet.ChainResult{}, err
			}
			break
		}
		wrote += raw
		made = append(made, chainMade{kept: kept, tip: out.Tip, added: added})
		out.Commits = append(out.Commits, gitlet.ChainCommit{EntryID: e.EntryID, Commit: commit})
		out.Tip, kept = commit, after
	}
	return wr.packChain(ctx, bd, b, out, made)
}

// entryObjects is what the entry just merged added to the scratch's loose
// objects (kept before it): every loose object now and the new ones, both
// sorted, and the new ones' bytes uncompressed. It refuses too_large where they
// pack past gitlet.MaxWritePackBytes, a merge_change's bound: a loose file is
// its object deflated whole, which a pack never exceeds, so only an entry past
// the bound in loose bytes is packed, alone and into no file, to be measured.
// It refuses too_large too where they hold more than bound uncompressed
// (gitlet.MaxChainObjectBytes): a pack's cost follows the bytes it deflates,
// deltas and checks, which its size hides, so no chain packs more than was
// measured to pack within gitlet.ChainPackDeadline (package doc, "Measured").
func (b *building) entryObjects(ctx context.Context, objects string, kept []string, bound int64) (after,
	added []string, raw int64, refused gitlet.EngineWriteResult, done bool, err error) {

	if after, err = looseObjects(objects); err != nil {
		return nil, nil, 0, gitlet.EngineWriteResult{}, true, err
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
			return nil, nil, 0, gitlet.EngineWriteResult{}, true, err
		}
		loose += info.Size()
		size, err := looseSize(path)
		if err != nil {
			return nil, nil, 0, gitlet.EngineWriteResult{}, true, err
		}
		raw += size
	}
	if loose > gitlet.MaxWritePackBytes {
		var size counted
		if err := b.packObjects(ctx, added, &size, "pack of a chain entry's objects"); err != nil {
			return nil, nil, 0, gitlet.EngineWriteResult{}, true, err
		}
		if int64(size) > gitlet.MaxWritePackBytes {
			return nil, nil, 0, b.wr.changeRefused(gitlet.WriteTooLarge, writePackTooLarge(int64(size))), true, nil
		}
	}
	if raw > bound {
		return nil, nil, 0, b.wr.changeRefused(gitlet.WriteTooLarge, fmt.Sprintf("this change's merge writes %s "+
			"of objects uncompressed, past the %s one train of the merge queue packs and checks in its time "+
			"(gitlet.MaxChainObjectBytes); merge its target into its branch in a checkout of the repository ("+
			toolname.CodeCheckout+"), so "+
			"its merge writes no file of its own", humanBytes(raw), humanBytes(bound))), true, nil
	}
	return after, added, raw, gitlet.EngineWriteResult{}, false, nil
}

// packChain makes the chain's one pack, of out's entries, within
// gitlet.ChainPackDeadline. An object `index-pack --strict` refuses skips the
// entry of made that wrote it rejected_object, in git's words; it and every
// later entry (merged onto its commit) are pruned, the later ones not reached,
// and the pack is made again in what is left. A pack that runs the deadline
// out is the NODE failing, unready node_failed, never the chain's refusal: the
// deadline holds the largest pack a chain makes 4.8 times over (package doc,
// "Measured").
func (wr *writing) packChain(ctx context.Context, bd *building, b gitlet.ChainBuild, out gitlet.ChainResult,
	made []chainMade) (gitlet.ChainResult, error) {

	s := wr.s
	wr.metered, wr.left = true, s.chainPack
	defer func() { wr.metered = false }()
	// The pack's bound follows its objects' (gitlet.MaxChainPackBytes at the
	// default), so a suite that scales one scales both.
	bound := gitlet.ChainPackPerObjectByte * s.chainObjects
	for {
		refused, object, done, err := bd.packWithin(ctx, bound, func(size int64) string {
			return fmt.Sprintf("chain %s's pack is %s, past the %s (gitlet.MaxChainPackBytes) a chain's objects "+
				"of at most %s may pack to", b.ChainID, humanBytes(size), humanBytes(bound),
				humanBytes(s.chainObjects))
		})
		switch {
		case errors.Is(err, errBuildSpent):
			return gitlet.ChainResult{}, wr.unready(gitlet.UnreadyNodeFailed, fmt.Sprintf("%s's pack of chain %s "+
				"over repository %s ran the whole %s of git a chain's pack is given (gitlet.ChainPackDeadline), "+
				"several times what the largest pack a chain makes takes; the next turn builds it again",
				s.o.NodeID, b.ChainID, wr.repoName(), s.chainPack))
		case err != nil:
			return gitlet.ChainResult{}, err
		case !done:
			return out, nil
		case refused.Refusal != gitlet.WriteRejectedObject:
			// Unreachable by construction (gitlet.MaxChainPackBytes): a broken
			// invariant is this node's error, never the chain's.
			return gitlet.ChainResult{}, fmt.Errorf("host: %s", refused.Detail)
		}
		k := slices.IndexFunc(made, func(m chainMade) bool {
			_, found := slices.BinarySearch(m.added, object)
			return found
		})
		if k < 0 {
			return gitlet.ChainResult{}, fmt.Errorf("host: git's check refused object %q of chain %s, which no "+
				"entry of it wrote: %s", object, b.ChainID, refused.Detail)
		}
		if err := bd.prune(made[k].kept); err != nil {
			return gitlet.ChainResult{}, err
		}
		out.Tip = made[k].tip
		out.Commits = append(out.Commits[:k], gitlet.ChainCommit{EntryID: out.Commits[k].EntryID,
			Skipped: gitlet.WriteRejectedObject, Detail: refused.Detail})
		made = made[:k]
	}
}

// storeChain stores the chain's one pack as one bundle under the chain's id
// and answers it, nil for a chain that made no object. Nothing names it until
// its caller's record does; one whose record never applies is the collector's
// to remove, as a refused push's is. A bundle its seal refused is logged here,
// naming the chain.
func (s *Server) storeChain(ctx context.Context, sc *cache.Scratch, b gitlet.ChainBuild) (*gitlet.PackRef, error) {
	packPath, checksum, held, err := sc.Pack()
	if err != nil || !held {
		return nil, err
	}
	ref, err := gitlet.LayBundle(b.ChainID, packPath, checksum)
	if err != nil {
		return nil, err
	}
	if err := gitlet.StoreBundle(ctx, s.o.Objects, packPath, ref, nil); err != nil {
		var bad *gitlet.BadBundle
		if errors.As(err, &bad) {
			s.log.Error("code_chain_pack_rejected", "repo_id", b.RepoID, "chain", b.ChainID,
				"object", bad.Object.String(), "error", bad.Err)
		}
		return nil, fmt.Errorf("host: storing chain %s's pack: %w", b.ChainID, err)
	}
	return ref, nil
}

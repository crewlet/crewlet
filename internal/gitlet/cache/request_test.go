package cache_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// A REQUEST'S PACK OR OBJECT DAMAGE IS REPAIRED IN PLACE FIRST (rebuild.go):
// Cache.Rebuild runs the forced round (fetching again a pack whose index files
// fail their checksums, vouched or not), then, if it found nothing, the
// request's scrub (fetching again a pack whose BODY fails its own), and only
// past both the whole rebuild, seeded with every pack the scrub found whole.

// failureOf is the failure a git run over s's directory meets, as the host
// reads it (cache.DamageReason), failing t when the run succeeds or names no
// damage the cache repairs. The run names s.Repairer, as the host's does, so
// the case, as the request, asks the repair itself.
func failureOf(t *testing.T, w *syncWorld, s cache.Synced, args ...string) cache.Failure {
	t.Helper()
	_, err := w.pool.Run(t.Context(), gitproc.Cmd{Dir: s.Dir, Args: args, Repairer: s.Repairer,
		Class: gitproc.ClassRead, Deadline: time.Minute})
	if err == nil {
		t.Fatalf("git %v over the damaged copy succeeded: the case damaged nothing it reads", args)
	}
	f, ok := cache.DamageReason(err)
	if !ok {
		t.Fatalf("git %v failed with %v, which names no damage the cache repairs", args, err)
	}
	return f
}

// rebuilds is the cache's crewlet.code.cache.rebuilds for reason.
func rebuilds(w *syncWorld, reason cache.RebuildReason) uint64 {
	return counter(w.rec, metrics.CodeCacheRebuilds, "reason", string(reason))
}

// checks is the cache's crewlet.code.cache.checks of file with outcome and
// trigger.
func checks(w *syncWorld, file, outcome, trigger string) uint64 {
	return counterOf(w.rec, metrics.CodeCacheChecks, map[string]string{"file": file, "outcome": outcome,
		"trigger": trigger})
}

// A REQUEST'S PACK DAMAGE IS REPAIRED IN PLACE FIRST, in ONE Rebuild call, each
// arm's finder named by code_cache_pack_reinstalled's found and trigger: an
// index truncated (B: the round sees its size, one GET); every index damaged IN
// PLACE, identity kept after it was vouched for (M: only the forced checksums
// see it, two GETs); a pack's body (G: the request's scrub fails its trailer,
// one GET); a pack LOST (reinstalled, found=absent, one GET); a SOUND copy
// (damage in the rows: the rebuild, SEEDED by the scrub, downloads nothing).
//
// Mutations: no forced check (trigger `request`); no escalation (G's git
// fails); no scrub step, or the whole rebuild first (a new incarnation); an
// unseeded rebuild (the sound arm downloads); a lost pack's install no repair
// (no found=absent).
func TestARequestsPackDamageIsRepairedInPlaceFirst(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		damage func(t *testing.T, dir string, h history)
		read   []string // the git run that meets it, over the tip
		// gets is the packs fetched again, each found as found by the check
		// of trigger.
		gets           int
		found, trigger string
	}{
		{"B an index file truncated", func(t *testing.T, dir string, h history) {
			gittest.Damage(t, dir, gittest.IdxTruncated, h.commits[3])
		}, []string{"rev-list", "--objects"}, 1, "size", "forced"},
		{"M every index file damaged in place", func(t *testing.T, dir string, _ history) {
			gittest.DamageInPlace(t, dir, gittest.IdxOffsetsPastPack)
		}, []string{"rev-list", "--objects"}, 2, "trailer", "forced"},
		{"G a pack's body", func(t *testing.T, dir string, h history) {
			gittest.DamageInPlace(t, dir, gittest.EntryBytes, h.commits[3])
		}, []string{"rev-list"}, 1, "trailer", "request"},
		{"a pack the copy lost", func(t *testing.T, dir string, h history) {
			for _, ext := range []string{".pack", ".idx", ".rev"} {
				name := filepath.Join(dir, "objects", "pack", "pack-"+h.newer.GitChecksum+ext)
				if err := os.Remove(name); err != nil {
					t.Fatal(err)
				}
			}
		}, []string{"rev-list", "--objects"}, 1, "absent", "forced"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newSyncWorld(t, onMemory)
			h := w.measured(t)
			c := w.open(t)
			s := w.sync(t, c, h.repo)
			tc.damage(t, s.Dir, h)
			f := failureOf(t, w, s, append(tc.read, h.commits[3])...)
			gets := w.objects.total()

			r, err := c.Rebuild(t.Context(), s, f)
			if err != nil {
				t.Fatalf("Rebuild(%s): %v", f.Reason, err)
			}
			defer r.Release()
			if n := cache.Incarnation(r); n != cache.Incarnation(s) {
				t.Errorf("the repair answered incarnation %d, want %d repaired in place", n, cache.Incarnation(s))
			}
			if got := w.objects.total() - gets; got != tc.gets {
				t.Errorf("the repair downloaded %d bundles, want %d", got, tc.gets)
			}
			lines := w.logs.lines(t, "code_cache_pack_reinstalled")
			for _, l := range lines {
				if l["found"] != tc.found || l["trigger"] != tc.trigger {
					t.Errorf("code_cache_pack_reinstalled %v, want found=%s by trigger=%s", l, tc.found, tc.trigger)
				}
			}
			if len(lines) != tc.gets {
				t.Errorf("code_cache_pack_reinstalled was logged %d times, want %d", len(lines), tc.gets)
			}
			if got := rebuilds(w, cache.RebuildStored); got != 1 {
				t.Errorf("rebuilds{reason=stored} = %d, want the one round that fetched again", got)
			}
			if got := rebuilds(w, f.Reason); got != 0 {
				t.Errorf("rebuilds{reason=%s} = %d, want no whole rebuild", f.Reason, got)
			}
			if _, err := runGit(t, w.pool, r.Dir, "", "rev-list", "--objects", h.commits[3]); err != nil {
				t.Errorf("the repaired copy still fails: %v", err)
			}
		})
	}

	t.Run("a sound copy", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		h := w.measured(t)
		c := w.open(t)
		s := w.sync(t, c, h.repo)
		gets := w.objects.total()
		r, err := c.Rebuild(t.Context(), s, cache.Failure{Reason: cache.RebuildMissing, Began: time.Now()})
		if err != nil {
			t.Fatalf("Rebuild: %v", err)
		}
		defer r.Release()
		if n := cache.Incarnation(r); n != cache.Incarnation(s)+1 {
			t.Errorf("the repair answered incarnation %d, want the whole rebuild's %d", n, cache.Incarnation(s)+1)
		}
		if got := w.objects.total() - gets; got != 0 {
			t.Errorf("the whole rebuild downloaded %d bundles, want none: the scrub found every pack whole", got)
		}
		if got := checks(w, "pack", "sound", "request"); got != 2 {
			t.Errorf("the request's scrub held %d packs sound, want both", got)
		}
		if got := rebuilds(w, cache.RebuildMissing); got != 1 {
			t.Errorf("rebuilds{reason=missing} = %d, want the one whole rebuild", got)
		}
		if got := rebuilds(w, cache.RebuildStored); got != 0 {
			t.Errorf("rebuilds{reason=stored} = %d, want none: nothing was fetched again in place", got)
		}
		lines := w.logs.lines(t, "code_cache_rebuild_seeded")
		if len(lines) != 1 || lines[0]["whole"] != float64(2) || lines[0]["packs"] != float64(2) {
			t.Errorf("code_cache_rebuild_seeded = %v, want one rebuild seeded with both packs", lines)
		}
	})

	// TEN FAILURES ON ONE SOUND COPY COST ONE SCRUB AND ONE WHOLE REBUILD:
	// the in-place rounds they ask are joined, the scrub runs for the first
	// to escalate and its verdict answers the rest (a scrub of the copy that
	// found nothing, nothing written since), and every one waits on the one
	// rebuild.
	t.Run("ten failures on a sound copy", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		h := w.measured(t)
		c := w.open(t)
		s := w.sync(t, c, h.repo)
		defer s.Release()
		answers := make([]cache.Synced, 10)
		errs := make([]error, 10)
		var wg sync.WaitGroup
		for i := range 10 {
			wg.Go(func() {
				answers[i], errs[i] = c.Rebuild(context.Background(), s,
					cache.Failure{Reason: cache.RebuildMissing, Began: time.Now()})
			})
		}
		wg.Wait()
		for i := range 10 {
			if errs[i] != nil {
				t.Fatalf("Rebuild %d: %v", i, errs[i])
			}
			defer answers[i].Release()
			if cache.Incarnation(answers[i]) != 2 {
				t.Errorf("Rebuild %d answered incarnation %d, want the one rebuilt incarnation", i,
					cache.Incarnation(answers[i]))
			}
		}
		if lines := w.logs.lines(t, "code_cache_rebuilt"); len(lines) != 1 {
			t.Errorf("code_cache_rebuilt = %v, want one whole rebuild", lines)
		}
		if got := checks(w, "pack", "sound", "request"); got != 2 {
			t.Errorf("the requests' scrubs held %d packs sound, want both once: one scrub", got)
		}
	})
}

// servedHere is the measured history synced into a fresh cache on clk and
// served by this process: its maintainer's multi-pack-index, bitmap and
// commit-graph written over the two packs — files a repair can drop.
func servedHere(t *testing.T, clk *clock, opts ...func(*cache.Options)) (*syncWorld, history, *cache.Cache,
	cache.Synced) {

	t.Helper()
	w := newSyncWorld(t, onMemory)
	h := w.measured(t)
	c := w.open(t, append([]func(*cache.Options){clk.on}, opts...)...)
	s := w.sync(t, c, h.repo)
	clk.Add(cache.MidxDebounce)
	if ran := cache.MaintainerPass(c); len(ran) != 1 {
		t.Fatalf("the maintainer ran %v, want %s", ran, h.repo)
	}
	if _, _, graph := cache.Indexed(c, h.repo); !graph {
		t.Fatal("the maintainer wrote no commit-graph: the case needs a derived file a repair drops")
	}
	return w, h, c, s
}

// droppedGraph has a run report s's commit-graph damaged and waits until the
// repair round has dropped it: a REPAIR written into the copy (a derived file
// dropped, which a lookup that went wrong through it no longer reads).
func droppedGraph(t *testing.T, c *cache.Cache, repo string, s cache.Synced) {
	t.Helper()
	graph := filepath.Join(s.Dir, "objects", "info", "commit-graph")
	cache.Report(c, derivedReport(s.Dir, gitproc.PartCommitGraph, time.Now()))
	eventually(t, "the reported commit-graph dropped", func() bool {
		_, err := os.Lstat(graph)
		return os.IsNotExist(err) && cache.Settled(c, repo)
	})
}

// A FAILURE THAT PREDATES A REPAIR IS NOT ESCALATED (rebuild.go's guard). A
// request whose git began before this process last wrote a repair into the
// copy — here a derived file a report had dropped — may have failed on what
// the repair replaced: it is answered from the copy as it stands (Kind
// SyncNone), counted reports{part,outcome=stale}, and runs once more over it,
// with no in-place round, no scrub and no rebuild. The same failure begun
// after the repair is repaired.
//
// Mutation: no guard — the in-place round runs (checks{trigger=forced}), the
// scrub runs and the whole rebuild is asked of a sound copy.
func TestAFailureThatPredatesARepairIsNotEscalated(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		reason cache.RebuildReason
		part   string
	}{
		{cache.RebuildCorrupt, "pack"},
		{cache.RebuildMissing, "object"},
	} {
		t.Run(string(tc.reason), func(t *testing.T) {
			t.Parallel()
			w, h, c, s := servedHere(t, newClock())
			defer s.Release()
			began := time.Now()
			droppedGraph(t, c, h.repo, s)
			repairs := counterOf(w.rec, metrics.CodeCacheRebuilds, nil)

			r, err := c.Rebuild(t.Context(), s, cache.Failure{Reason: tc.reason, Began: began})
			if err != nil {
				t.Fatalf("Rebuild: %v", err)
			}
			r.Release()
			if r.Kind != cache.SyncNone || cache.Incarnation(r) != cache.Incarnation(s) {
				t.Errorf("a failure that began before a repair was answered %s from incarnation %d, want %s "+
					"from %d as it stands", r.Kind, cache.Incarnation(r), cache.SyncNone, cache.Incarnation(s))
			}
			if got := reports(w, tc.part, "stale"); got != 1 {
				t.Errorf("reports{part=%s,outcome=stale} = %d, want the one failure answered as it stands",
					tc.part, got)
			}
			forced := counterOf(w.rec, metrics.CodeCacheChecks, map[string]string{"trigger": "forced"})
			scrubbed := counterOf(w.rec, metrics.CodeCacheChecks, map[string]string{"trigger": "request"})
			rebuilt := counterOf(w.rec, metrics.CodeCacheRebuilds, nil) - repairs
			if forced != 0 || scrubbed != 0 || rebuilt != 0 {
				t.Errorf("the failure answered as it stands checked %d files in place and scrubbed %d, and "+
					"counted %d repairs; want none", forced, scrubbed, rebuilt)
			}

			// Control: begun after the repair, the same failure is repaired.
			after, err := c.Rebuild(t.Context(), s, cache.Failure{Reason: tc.reason, Began: time.Now()})
			if err != nil {
				t.Fatalf("Rebuild after the repair: %v", err)
			}
			after.Release()
			if got := counterOf(w.rec, metrics.CodeCacheChecks, map[string]string{"trigger": "forced"}); got == 0 {
				t.Error("a failure that began after the repair ran no in-place round")
			}
		})
	}
}

// A PUSH IS NO REPAIR OF A FAILING REQUEST. A pack this process installs or
// adopts for a push the copy had not yet read leaves whatever failed a request
// as it was, so a request whose git began before it is repaired all the same:
// its in-place round runs, forced, and nothing is answered `stale`. Taken for
// a repair, damage on a repository taking pushes is never repaired: both of a
// request's tries are answered as it stands.
//
// Mutation: stamp a push's install or an adoption as a repair — the request
// is answered as it stands, with no in-place round.
func TestAPushIsNoRepairOfAFailingRequest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		land func(t *testing.T, w *syncWorld, c *cache.Cache, repo string, s cache.Synced)
	}{
		{"installed by a sync", func(t *testing.T, w *syncWorld, c *cache.Cache, repo string, _ cache.Synced) {
			w.push(t, repo, "refs/heads/main", nil)
			w.sync(t, c, repo).Release()
		}},
		{"adopted", func(t *testing.T, w *syncWorld, c *cache.Cache, repo string, s cache.Synced) {
			pushID := uuid.Must(uuid.NewV7()).String()
			scratch, err := c.NewScratch(t.Context(), s, pushID)
			if err != nil {
				t.Fatalf("NewScratch: %v", err)
			}
			defer scratch.Remove()
			tip, cs := w.packInto(t, repo, scratch.Dir, nil)
			row := w.storeBundle(t, w.bundle(cs), pushID)
			row.Added = w.log.write(repo, []gitlet.PackRow{row}, map[string]string{"refs/heads/adopted": tip})
			if err := c.Adopt(t.Context(), repo, scratch, row.Ref()); err != nil {
				t.Fatalf("Adopt: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newSyncWorld(t, onMemory)
			h := w.measured(t)
			c := w.open(t)
			s := w.sync(t, c, h.repo)
			defer s.Release()
			began := time.Now()
			tc.land(t, w, c, h.repo, s)

			r, err := c.Rebuild(t.Context(), s, cache.Failure{Reason: cache.RebuildCorrupt, Began: began})
			if err != nil {
				t.Fatalf("Rebuild: %v", err)
			}
			r.Release()
			if got := counterOf(w.rec, metrics.CodeCacheChecks, map[string]string{"trigger": "forced"}); got == 0 {
				t.Error("no in-place round ran: the push was taken for a repair the failure predates")
			}
			if got := reports(w, "pack", "stale"); got != 0 {
				t.Errorf("reports{part=pack,outcome=stale} = %d, want none: a push repairs nothing", got)
			}
		})
	}
}

// A LOST PACK ANY ROUND PUTS BACK IS A REPAIR. A pack the copy had read and
// lost (files gone, rows naming it at a position read) fails a request's git,
// and any round reading the copy whole (here one its shape marked) that puts
// it back ends that failure, so writes a REPAIR (incarnation.repairWrite),
// counted `stored` and logged found=absent: a request whose git began before
// it is answered as it stands, not escalated to a scrub and a whole rebuild of
// a copy nothing is wrong with. A pack a log gone back names is no lost pack:
// the log, restored from an older copy, gives positions the copy had read to
// pushes it never held.
//
// Mutations: a lost pack counted only by a forced round (the request is
// escalated); lost packs read against a gone-back log (a push found=absent).
func TestALostPackAnyRoundPutsBackIsARepair(t *testing.T) {
	t.Parallel()
	t.Run("a round its shape marked", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		h := w.measured(t)
		c := w.open(t)
		s := w.sync(t, c, h.repo)
		for _, ext := range []string{".pack", ".idx", ".rev"} {
			if err := os.Remove(filepath.Join(s.Dir, "objects", "pack", "pack-"+h.newer.GitChecksum+ext)); err != nil {
				t.Fatal(err)
			}
		}
		// The request's git meets the loss, and asks its repair only once a
		// round of nobody's has put the pack back.
		f := failureOf(t, w, s, "rev-list", "--objects", h.commits[3])
		cache.MarkWhole(c, h.repo, cache.RebuildShape, 0)
		w.sync(t, c, h.repo)
		if got := rebuilds(w, cache.RebuildStored); got != 1 {
			t.Errorf("rebuilds{reason=stored} = %d after the round that put the lost pack back, want 1", got)
		}
		lines := w.logs.lines(t, "code_cache_pack_reinstalled")
		if len(lines) != 1 || lines[0]["found"] != "absent" || lines[0]["pack_id"] != h.newer.PackID {
			t.Errorf("code_cache_pack_reinstalled = %v, want the lost pack put back, found=absent", lines)
		}

		r, err := c.Rebuild(t.Context(), s, f)
		if err != nil {
			t.Fatalf("Rebuild: %v", err)
		}
		defer r.Release()
		if r.Kind != cache.SyncNone || cache.Incarnation(r) != cache.Incarnation(s) {
			t.Errorf("the request was answered %s from incarnation %d, want %s from %d as it stands", r.Kind,
				cache.Incarnation(r), cache.SyncNone, cache.Incarnation(s))
		}
		if got := reports(w, "object", "stale"); got != 1 {
			t.Errorf("reports{part=object,outcome=stale} = %d, want the request answered as it stands", got)
		}
		forced := counterOf(w.rec, metrics.CodeCacheChecks, map[string]string{"trigger": "forced"})
		scrubbed := counterOf(w.rec, metrics.CodeCacheChecks, map[string]string{"trigger": "request"})
		if forced != 0 || scrubbed != 0 {
			t.Errorf("the request's repair checked %d files in place and scrubbed %d, want none: the pack it "+
				"failed on was put back after it began", forced, scrubbed)
		}
		if _, err := runGit(t, w.pool, r.Dir, "", "rev-list", "--objects", h.commits[3]); err != nil {
			t.Errorf("the copy the request runs again over still fails: %v", err)
		}
	})

	t.Run("a log gone back", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		repo := w.repo(t, 1)
		f := w.repos[repo]
		w.push(t, repo, "refs/heads/main", nil)
		earlier, head, tips := w.log.state(), f.head, slices.Clone(f.tips)
		w.push(t, repo, "refs/heads/main", nil)
		w.push(t, repo, "refs/heads/main", nil)
		c := w.open(t)
		ahead := w.sync(t, c, repo).Position

		// Brought back from the earlier copy, the log takes a push of the
		// history it holds at a position the copy had already read.
		w.log.restore(earlier)
		w.client.Run(t, f.work, "reset", "--hard", head)
		f.head, f.tips = head, tips
		_, row := w.push(t, repo, "refs/heads/main", nil)
		if newer, _ := ahead.Before(row.Added); newer {
			t.Fatalf("the push after the restore sits at %s, past the copy's %s: the case tests nothing", row.Added,
				ahead)
		}
		w.sync(t, c, repo)
		if len(w.logs.lines(t, "code_cache_log_rewound")) == 0 {
			t.Fatal("the round did not read the log as gone back: the case tests nothing")
		}
		if got := rebuilds(w, cache.RebuildStored); got != 0 {
			t.Errorf("rebuilds{reason=stored} = %d, want none: the copy never held the push", got)
		}
		if lines := w.logs.lines(t, "code_cache_pack_reinstalled"); len(lines) != 0 {
			t.Errorf("code_cache_pack_reinstalled = %v, want nothing put back", lines)
		}
	})
}

// A CONFIRMED HINT REINSTALLS ONE PACK. A git that names the pack holding a
// corrupt object (2.55's packed-object row, 2.56's bare "%s" sentence) has it
// held to its row (Cache.ConfirmHint); one that is not its row's is named to
// the repair (Failure.Packs), and the in-place round fetches THAT pack again
// (one GET, same incarnation, found=hint) with no second hash and no scrub.
// The damage is eight bytes in place in the newer pack (C); a hint naming the
// sound pack beside it asks no repair.
//
// Mutation: a confirmed hint's repair without its pack (RebuildCorrupt alone)
// — the scrub, not the hint, finds the pack (found=trailer, a scrub's checks).
func TestAConfirmedHintReinstallsOnePack(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	h := w.measured(t)
	c := w.open(t)
	s := w.sync(t, c, h.repo)
	defer s.Release()
	gittest.DamageInPlace(t, s.Dir, gittest.PackFlipped, h.commits[3])

	if f, ok := c.ConfirmHint(t.Context(), s, gitproc.Hint{Object: h.side, Pack: h.older.GitChecksum}); ok {
		t.Fatalf("a hint naming the sound pack asked %+v", f)
	}
	f, ok := c.ConfirmHint(t.Context(), s, gitproc.Hint{Object: h.blob, Pack: h.newer.GitChecksum})
	if !ok || f.Reason != cache.RebuildCorrupt || !slices.Equal(f.Packs, []string{h.newer.GitChecksum}) {
		t.Fatalf("a hint naming the damaged pack = %+v, %v; want a corrupt repair naming it", f, ok)
	}
	f.Began = time.Now()
	gets := w.objects.total()
	r, err := c.Rebuild(t.Context(), s, f)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	defer r.Release()
	if n := cache.Incarnation(r); n != cache.Incarnation(s) {
		t.Errorf("the repair answered incarnation %d, want %d in place", n, cache.Incarnation(s))
	}
	if got := w.objects.total() - gets; got != 1 {
		t.Errorf("the repair downloaded %d bundles, want the one pack the hint named", got)
	}
	lines := w.logs.lines(t, "code_cache_pack_reinstalled")
	if len(lines) != 1 || lines[0]["found"] != "hint" || lines[0]["trigger"] != "request" {
		t.Errorf("code_cache_pack_reinstalled = %v, want the one pack found by its hint", lines)
	}
	if got := counterOf(w.rec, metrics.CodeCacheChecks, map[string]string{"file": "pack"}); got != 0 {
		t.Errorf("the repair hashed %d packs, want none: the hint's confirmation already did", got)
	}
	if got := rebuilds(w, cache.RebuildStored); got != 1 {
		t.Errorf("rebuilds{reason=stored} = %d, want one", got)
	}
	if _, err := runGit(t, w.pool, r.Dir, "", "cat-file", "-p", h.blob); err != nil {
		t.Errorf("the repaired copy still fails: %v", err)
	}
}

// A REQUEST ON A COPY A REBUILD REPLACED IS ANSWERED BY THE REBUILD. A whole
// rebuild already running replaces the incarnation a request failed on before
// its in-place round runs, so the request is answered from the rebuilt copy,
// never escalated to a scrub and rebuild of a copy that never failed it, which
// the rebuild just read and would refuse ("unchanged").
//
// Mutation: escalate whatever incarnation the round read (no
// waiter.failedOn) — the second request is refused.
func TestARequestOnACopyARebuildReplacedIsAnsweredByTheRebuild(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	h := w.measured(t)
	c := w.open(t)
	s := w.sync(t, c, h.repo)
	defer s.Release()

	// The second whole read is the whole rebuild's (the first, the first
	// request's in-place round): held until the second request has queued.
	var reads sync.Mutex
	wholeReads := 0
	reached, release := make(chan struct{}), make(chan struct{})
	w.log.set(func(f *fakeLog) {
		f.hook = func(call fakeCall) error {
			if call.Page != 1 || !call.Since.IsZero() {
				return nil
			}
			reads.Lock()
			wholeReads++
			n := wholeReads
			reads.Unlock()
			if n == 2 {
				close(reached)
				<-release
			}
			return nil
		}
	})
	type answer struct {
		s   cache.Synced
		err error
	}
	first, second := make(chan answer, 1), make(chan answer, 1)
	go func() {
		r, err := c.Rebuild(context.Background(), s, cache.Failure{Reason: cache.RebuildMissing, Began: time.Now()})
		first <- answer{r, err}
	}()
	select {
	case <-reached:
	case <-time.After(hangDetector): //fixedwait:liveness
		t.Fatal("the first request's whole rebuild never began")
	}
	go func() {
		r, err := c.Rebuild(context.Background(), s, cache.Failure{Reason: cache.RebuildMissing, Began: time.Now()})
		second <- answer{r, err}
	}()
	eventually(t, "the second request's round queues behind the rebuild", func() bool {
		return cache.Waiting(c, h.repo) == 2
	})
	close(release)
	for _, tc := range []struct {
		name string
		ch   chan answer
	}{{"first", first}, {"second", second}} {
		a := <-tc.ch
		if a.err != nil {
			t.Errorf("the %s request: %v, want the rebuilt copy", tc.name, a.err)
			continue
		}
		if n := cache.Incarnation(a.s); n != 2 {
			t.Errorf("the %s request answered incarnation %d, want the rebuilt 2", tc.name, n)
		}
		a.s.Release()
	}
}

// A REPAIR WHILE A REQUEST'S REPAIR RUNS ANSWERS IT AS IT STANDS; A PUSH DOES
// NOT. The guard (rebuild.go) is asked again before the scrub and before the
// rebuild, since the copy is written into meanwhile, and only a write that can
// repair counts: a push the in-place round installs repairs nothing (no
// found=absent, no `stored`), so the request still goes to its scrub and the
// whole rebuild; a REPAIR landing during the scrub (here a reported derived
// file dropped, standing in for the scrubber's own repair) answers it as it
// stands, never with a rebuild of a copy the repair may have mended.
//
// Mutations: a push's pack counted as the repair (the first arm `stored`,
// found=absent); a push's install taken for a repair (the first arm never
// scrubbed); no guard after the scrub (the second arm gets a new incarnation).
func TestARepairWhileARequestIsRepairedAnswersItAsItStands(t *testing.T) {
	t.Parallel()
	t.Run("the in-place round installs a push", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		h := w.measured(t)
		c := w.open(t)
		s := w.sync(t, c, h.repo)
		defer s.Release()
		began := time.Now()
		w.push(t, h.repo, "refs/heads/main", nil)

		r, err := c.Rebuild(t.Context(), s, cache.Failure{Reason: cache.RebuildCorrupt, Began: began})
		if err != nil {
			t.Fatalf("Rebuild: %v", err)
		}
		defer r.Release()
		if got := counterOf(w.rec, metrics.CodeCacheChecks, map[string]string{"trigger": "forced"}); got == 0 {
			t.Error("no in-place round ran")
		}
		if got := reports(w, "pack", "stale"); got != 0 {
			t.Errorf("reports{part=pack,outcome=stale} = %d, want none: the push repaired nothing", got)
		}
		if got := counterOf(w.rec, metrics.CodeCacheChecks, map[string]string{"trigger": "request"}); got == 0 {
			t.Error("the request's scrub never ran: the push the in-place round installed was taken for a repair")
		}
		if got := rebuilds(w, cache.RebuildStored); got != 0 {
			t.Errorf("the push the round installed counted %d in-place repairs, want none", got)
		}
		if lines := w.logs.lines(t, "code_cache_pack_reinstalled"); len(lines) != 0 {
			t.Errorf("code_cache_pack_reinstalled = %v, want none: a push is no pack the copy lost", lines)
		}
		if n := cache.Incarnation(r); n != cache.Incarnation(s)+1 || rebuilds(w, cache.RebuildCorrupt) != 1 {
			t.Errorf("the request was answered from incarnation %d (rebuilds{reason=corrupt} %d), want the "+
				"whole rebuild's %d: in place and the scrub found nothing", n, rebuilds(w, cache.RebuildCorrupt),
				cache.Incarnation(s)+1)
		}
	})

	t.Run("a repair lands while the request's scrub runs", func(t *testing.T) {
		t.Parallel()
		var (
			c     *cache.Cache
			s     cache.Synced
			repo  string
			armed atomic.Bool
			once  sync.Once
			wrote atomic.Bool
		)
		w, h, cc, ss := servedHere(t, newClock(), func(o *cache.Options) {
			cache.WhileHashing(o, func(path string) {
				if !armed.Load() || !strings.HasSuffix(path, ".pack") {
					return
				}
				// The request's scrub — the one hasher of a .pack here —
				// has listed the copy: a repair lands beside it.
				once.Do(func() {
					droppedGraph(t, c, repo, s)
					wrote.Store(true)
				})
			})
		})
		c, s, repo = cc, ss, h.repo
		defer s.Release()
		began := time.Now()
		armed.Store(true)

		r, err := c.Rebuild(t.Context(), s, cache.Failure{Reason: cache.RebuildCorrupt, Began: began})
		if err != nil {
			t.Fatalf("Rebuild: %v", err)
		}
		defer r.Release()
		if !wrote.Load() {
			t.Fatal("no repair landed during the request's scrub")
		}
		if r.Kind != cache.SyncNone || cache.Incarnation(r) != cache.Incarnation(s) {
			t.Errorf("the request was answered %s from incarnation %d, want %s from %d as it stands",
				r.Kind, cache.Incarnation(r), cache.SyncNone, cache.Incarnation(s))
		}
		if got := checks(w, "pack", "sound", "request"); got != 2 {
			t.Errorf("the request's scrub held %d packs sound, want both: it ran, and found nothing", got)
		}
		if got := reports(w, "pack", "stale"); got != 1 {
			t.Errorf("reports{part=pack,outcome=stale} = %d, want the one request answered as it stands", got)
		}
		if got := rebuilds(w, cache.RebuildCorrupt); got != 0 {
			t.Errorf("rebuilds{reason=corrupt} = %d, want no whole rebuild of a copy written into since the "+
				"request failed", got)
		}
		if lines := w.logs.lines(t, "code_cache_rebuilt"); len(lines) != 0 {
			t.Errorf("code_cache_rebuilt = %v, want none", lines)
		}
	})
}

// A REQUEST THE GUARD ANSWERS AS IT STANDS WAITS FOR THE ROUND STILL WRITING
// THE REPAIR. A repairing round writes in steps (derived files dropped, then a
// pack's files renamed over), and its first write already guards an earlier
// failure; pinned at once, such a request would get a half-repaired copy and
// fail again, so it joins the round queued behind the running one. Joined is
// read off cache.Waiting, never waited out. The case cannot rule out a later
// answer once a wait on the cache's own clock passes: it runs on the machine's
// clock, since the guard compares the repair's stamp with an instant read
// before it, which a synctest bubble would make one instant.
//
// Mutation: answer a guarded request at once (asItStands pinning while the
// round runs) — the request returns while the repairing round is held.
func TestAGuardedRequestWaitsForTheRoundStillRepairing(t *testing.T) {
	t.Parallel()
	var (
		armed   atomic.Bool
		reached = make(chan struct{})
		release = make(chan struct{})
	)
	w := newSyncWorld(t, onMemory)
	h := w.measured(t)
	c := w.open(t, func(o *cache.Options) {
		cache.BeforeRoundEnds(o, func(string) {
			if armed.CompareAndSwap(true, false) {
				close(reached)
				<-release
			}
		})
	})
	s := w.sync(t, c, h.repo)
	defer s.Release()
	gittest.Damage(t, s.Dir, gittest.IdxTruncated, h.commits[3])
	early := time.Now()
	f := failureOf(t, w, s, "rev-list", "--objects", h.commits[3])

	// The first request's in-place round reinstalls the pack — a repair —
	// and is held before it lets the repository go.
	armed.Store(true)
	type answer struct {
		s   cache.Synced
		err error
	}
	first := make(chan answer, 1)
	go func() {
		r, err := c.Rebuild(context.Background(), s, f)
		first <- answer{r, err}
	}()
	select {
	case <-reached:
	case <-time.After(hangDetector): //fixedwait:liveness
		t.Fatal("the in-place round never reached its end")
	}
	if got := rebuilds(w, cache.RebuildStored); got != 1 {
		t.Fatalf("rebuilds{reason=stored} = %d before the held round ends, want its reinstall counted", got)
	}

	// A request whose git began before that reinstall: guarded. It waits
	// beside the held round's own caller — on the round queued behind it —
	// or it was answered at once.
	second := make(chan answer, 1)
	go func() {
		r, err := c.Rebuild(context.Background(), s, cache.Failure{Reason: cache.RebuildCorrupt, Began: early})
		second <- answer{r, err}
	}()
	var (
		premature answer
		answered  bool
	)
	eventually(t, "the guarded request waiting on a round, or answered", func() bool {
		select {
		case premature = <-second:
			answered = true
			return true
		default:
		}
		return cache.Waiting(c, h.repo) == 2
	})
	if answered {
		premature.s.Release()
		t.Fatalf("the guarded request was answered (%s, %v) while the round repairing the copy still ran",
			premature.s.Kind, premature.err)
	}
	close(release)
	for _, tc := range []struct {
		name string
		ch   chan answer
		kind cache.SyncKind
	}{{"the repaired", first, ""}, {"the guarded", second, cache.SyncNone}} {
		select {
		case a := <-tc.ch:
			if a.err != nil {
				t.Errorf("%s request: %v", tc.name, a.err)
				continue
			}
			if tc.kind != "" && a.s.Kind != tc.kind {
				t.Errorf("%s request was answered %s, want %s: as it stands", tc.name, a.s.Kind, tc.kind)
			}
			if n := cache.Incarnation(a.s); n != cache.Incarnation(s) {
				t.Errorf("%s request was answered from incarnation %d, want %d in place", tc.name, n,
					cache.Incarnation(s))
			}
			if _, err := runGit(t, w.pool, a.s.Dir, "", "rev-list", "--objects", h.commits[3]); err != nil {
				t.Errorf("%s request's copy still fails: %v", tc.name, err)
			}
			a.s.Release()
		case <-time.After(hangDetector): //fixedwait:liveness
			t.Fatalf("%s request was never answered", tc.name)
		}
	}
	if got := reports(w, "pack", "stale"); got != 1 {
		t.Errorf("reports{part=pack,outcome=stale} = %d, want the guarded request", got)
	}
}

// A FORCED ROUND THAT REPAIRED NOTHING ANSWERS THE NEXT HOUR'S REQUESTS. When
// the damage is in the rows (a sound copy, its whole rebuild made and refused
// again), a forced round per request would read the rows whole at linearizable
// and hash every index file (about a quarter of the pack bytes), holding every
// reader, only to reach the scrub's cached verdict and the refusal. So while
// no repair is written, one forced round per ScrubCooldown: later requests go
// straight to the verdict and the refusal, and past it get a forced round.
//
// Mutation: no bound on step 2 — every request hashes every index file
// again (checks{trigger=forced} grows).
func TestAForcedRoundThatFoundNothingIsNotRepeatedForAnHour(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	h := w.measured(t)
	c := w.open(t, clk.on)
	s := w.sync(t, c, h.repo)
	defer s.Release()
	fail := func(on cache.Synced) (cache.Synced, error) {
		return c.Rebuild(t.Context(), on, cache.Failure{Reason: cache.RebuildMissing, Began: time.Now()})
	}
	forced := func() uint64 {
		return counterOf(w.rec, metrics.CodeCacheChecks, map[string]string{"trigger": "forced"})
	}
	scrubbed := func() uint64 {
		return counterOf(w.rec, metrics.CodeCacheChecks, map[string]string{"trigger": "request"})
	}

	// The first failure rebuilds the sound copy whole; the second, on the
	// rebuilt one, is refused — it holds exactly what that rebuild read.
	rebuilt, err := fail(s)
	if err != nil {
		t.Fatalf("the first failure: %v", err)
	}
	defer rebuilt.Release()
	if _, err := fail(rebuilt); !errors.Is(err, cache.ErrRebuildRefused) {
		t.Fatalf("the second failure = %v, want the whole rebuild refused", err)
	}
	beforeForced, beforeScrub := forced(), scrubbed()
	if beforeForced == 0 {
		t.Fatal("no forced round has run: the case tests nothing")
	}

	for i := range 3 {
		clk.Add(time.Minute)
		if _, err := fail(rebuilt); !errors.Is(err, cache.ErrRebuildRefused) {
			t.Fatalf("failure %d within the hour = %v, want the refusal", i, err)
		}
	}
	if got := forced() - beforeForced; got != 0 {
		t.Errorf("three failures within the hour hashed %d index files in forced rounds, want none", got)
	}
	if got := scrubbed() - beforeScrub; got != 0 {
		t.Errorf("three failures within the hour scrubbed %d files, want none: the scrub's verdict stands", got)
	}

	clk.Add(cache.ScrubCooldown)
	if _, err := fail(rebuilt); !errors.Is(err, cache.ErrRebuildRefused) {
		t.Fatalf("the failure past the hour = %v, want the refusal", err)
	}
	if forced() == beforeForced {
		t.Error("a failure past the hour ran no forced round: the bound is for ever")
	}
}

// A FORCED ROUND THAT FAILS AFTER ITS CHECK STILL BOUNDS THE NEXT. Its check
// leaves what it found on the repair mark whatever fails after it (here its
// install, on a push the object store will not hand over), so a forced round
// within ScrubCooldown with nothing written since would learn nothing new: the
// next request goes to the scrub's answer, as after a round that converged
// repairing nothing, and past the hour gets a forced round again. Both checks:
// one that found nothing, and one that found an index damaged in place, whose
// GET refusePush holds until the round has failed, so no repair is written.
//
// Mutation: record the verdict only for a round that got past its install —
// the request within the hour runs a forced round.
func TestAForcedRoundThatFailsAfterItsCheckStillBoundsTheNext(t *testing.T) {
	t.Parallel()
	for _, damaged := range []bool{false, true} {
		t.Run(fmt.Sprintf("damaged=%t", damaged), func(t *testing.T) {
			t.Parallel()
			w := newSyncWorld(t, onMemory)
			clk := newClock()
			h := w.measured(t)
			c := w.open(t, clk.on)
			s := w.sync(t, c, h.repo)
			if damaged {
				gittest.DamageInPlace(t, s.Dir, gittest.IdxTrailer, h.commits[3])
			}
			w.refusePush(t, h)
			forced := func() uint64 {
				return counterOf(w.rec, metrics.CodeCacheChecks, map[string]string{"trigger": "forced"})
			}
			fail := func(what string) {
				t.Helper()
				_, err := c.Rebuild(t.Context(), s, cache.Failure{Reason: cache.RebuildMissing, Began: time.Now()})
				if !errors.Is(err, errPushRefused) {
					t.Fatalf("%s: %v, want the store's refusal of the push", what, err)
				}
			}

			fail("the first request")
			first := forced()
			if first == 0 {
				t.Fatal("the first request ran no forced round: the case tests nothing")
			}
			clk.Add(time.Minute)
			fail("a request within the hour")
			if got := forced() - first; got != 0 {
				t.Errorf("a request within the hour of a forced round that failed at its install held %d index "+
					"files to their trailers in a forced round, want none", got)
			}
			clk.Add(cache.ScrubCooldown)
			fail("a request past the hour")
			if forced() == first {
				t.Error("a request past the hour ran no forced round: the bound is for ever")
			}
		})
	}
}

package cache_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE CHECK: nothing an index file says is served from a file this process
// did not write or check. Every case here runs on the measured history
// (gittest.NewFixture's, plus a side branch) in a sync world: commits one to
// three and the side commit in an OLDER pack, the fourth — a 256 KiB file —
// in a NEWER one; main at the fourth.

// history is the measured history as a sync world holds it.
type history struct {
	repo         string
	commits      [4]string
	side, blob   string
	older, newer code.PackRow
}

// measured writes the measured history to a new repository of w's log, each
// of its two packs one push.
func (w *syncWorld) measured(t testing.TB) history {
	t.Helper()
	repo := w.repo(t, 1)
	f := w.repos[repo]
	h := history{repo: repo}
	h.commits[0] = w.client.Commit(t, f.work, map[string]string{"a.txt": "one\n"}, "one")
	h.commits[1] = w.client.Commit(t, f.work, map[string]string{"a.txt": "two\n"}, "two")
	h.commits[2] = w.client.Commit(t, f.work, map[string]string{"b.txt": "three\n"}, "three")
	// The side branch leaves main at its first commit: main's commit-graph
	// says it is no ancestor of main, which a parent read wrong denies.
	h.side = strings.TrimSpace(w.client.Run(t, f.work, "commit-tree", h.commits[0]+"^{tree}", "-p",
		h.commits[0], "-m", "side"))
	h.older = w.packTips(t, f, h.commits[2], h.side)
	h.older.Added = w.log.write(repo, []code.PackRow{h.older},
		map[string]string{"refs/heads/main": h.commits[2], "refs/heads/side": h.side})
	h.commits[3] = w.client.Commit(t, f.work, map[string]string{"big.txt": gittest.BigFile()}, "four")
	h.blob = strings.TrimSpace(w.client.Run(t, f.work, "rev-parse", h.commits[3]+":big.txt"))
	h.newer = w.packTips(t, f, h.commits[3])
	h.newer.Added = w.log.write(repo, []code.PackRow{h.newer}, map[string]string{"refs/heads/main": h.commits[3]})
	f.head = h.commits[3]
	return h
}

// packTips is pack over several tips: the objects they reach that no earlier
// push did, one bundle in the store.
func (w *syncWorld) packTips(t testing.TB, f *fixture, tips ...string) code.PackRow {
	t.Helper()
	bare := gittest.Empty(t)
	revs := slices.Clone(tips)
	for _, old := range f.tips {
		revs = append(revs, "^"+old)
	}
	cs := gittest.Pack(t, w.client, bare, f.work, revs...)
	f.tips = append(f.tips, tips...)
	b := gittest.Bundle(t, bare, cs)
	w.mu.Lock()
	w.bundles[cs] = b
	w.mu.Unlock()
	return w.storeBundle(t, b, "")
}

// served syncs h into a cache on clk, has its maintainer write the
// multi-pack-index with its bitmap and the commit-graph — the copy a previous
// process served clones from — and stops it, answering the incarnation's
// directory.
func (w *syncWorld) served(t testing.TB, h history, clk *clock) string {
	t.Helper()
	c := w.open(t, clk.on)
	w.sync(t, c, h.repo)
	clk.Add(cache.MidxDebounce)
	if ran := cache.MaintainerPass(c); len(ran) != 1 {
		t.Fatalf("the maintainer ran %v, want %s", ran, h.repo)
	}
	if packs, bitmap, graph := cache.Indexed(c, h.repo); packs != 2 || !bitmap || !graph {
		t.Fatalf("the maintainer wrote an index over %d packs (bitmap %t) and a graph %t, want two packs, a "+
			"bitmap and a graph", packs, bitmap, graph)
	}
	dir := w.dir(t, c, h.repo)
	c.Stop(context.Background())
	return dir
}

// serveGit runs git in dir through w's pool under c's serve configuration,
// as the host does, answering its output and its exit status.
func serveGit(t testing.TB, w *syncWorld, c *cache.Cache, dir string, args ...string) (string, int) {
	t.Helper()
	res, err := w.pool.Run(t.Context(), gitproc.Cmd{Dir: dir, Args: args, Config: c.ServeConfig(),
		Class: gitproc.ClassRead, Deadline: time.Minute})
	var ee *gitproc.ExitError
	switch {
	case errors.As(err, &ee):
		return string(res.Stdout), ee.Result.Exit
	case err != nil:
		t.Fatalf("git %s in %s: %v", strings.Join(args, " "), dir, err)
	}
	return string(res.Stdout), 0
}

// cloneLacks fetches every object tip reaches from the copy at dir through
// upload-pack, as a clone does, under c's serve configuration — so through
// the multi-pack-index, its bitmap and the commit-graph — and answers what
// is wrong with what arrived: "" when the client indexes the pack it was sent
// and fsck finds everything tip reaches whole.
func cloneLacks(t testing.TB, w *syncWorld, c *cache.Cache, dir, tip string) string {
	t.Helper()
	pkt := func(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }
	req := pkt("want "+tip+" ofs-delta\n") + "0000" + pkt("done\n")
	var out bytes.Buffer
	if _, err := w.pool.Run(t.Context(), gitproc.Cmd{Dir: dir, Args: []string{"upload-pack", "--stateless-rpc", dir},
		Config: c.ServeConfig(), Class: gitproc.ClassRead, Stdin: strings.NewReader(req), Stdout: &out,
		Deadline: time.Minute}); err != nil {
		return "upload-pack: " + err.Error()
	}
	const nak = "0008NAK\n"
	if !bytes.HasPrefix(out.Bytes(), []byte(nak)) {
		return fmt.Sprintf("upload-pack answered %q", out.Bytes()[:min(out.Len(), 64)])
	}
	bare := gittest.Empty(t)
	if _, stderr, err := w.client.Try(t, bare, bytes.NewReader(out.Bytes()[len(nak):]), "index-pack", "--stdin",
		"--strict"); err != nil {
		return "index-pack: " + strings.TrimSpace(stderr)
	}
	gittest.SetRefs(t, bare, map[string]string{"refs/heads/main": tip})
	if _, stderr, err := w.client.Try(t, bare, nil, "fsck", "--strict", "--no-dangling"); err != nil {
		return "fsck: " + strings.TrimSpace(stderr)
	}
	return ""
}

// wrong is every way the copy at dir answers h wrong, under c's serve
// configuration: main's commits counted, the first commit and the side
// commit each asked whether it is main's ancestor, and a clone of main.
func wrong(t testing.TB, w *syncWorld, c *cache.Cache, dir string, h history) []string {
	t.Helper()
	var out []string
	if n, exit := serveGit(t, w, c, dir, "rev-list", "--count", "refs/heads/main"); exit != 0 ||
		strings.TrimSpace(n) != "4" {
		out = append(out, fmt.Sprintf("rev-list --count main = %q (exit %d), want 4", strings.TrimSpace(n), exit))
	}
	if _, exit := serveGit(t, w, c, dir, "merge-base", "--is-ancestor", h.commits[0], "refs/heads/main"); exit != 0 {
		out = append(out, fmt.Sprintf("merge-base --is-ancestor <first> main exits %d, want 0", exit))
	}
	if _, exit := serveGit(t, w, c, dir, "merge-base", "--is-ancestor", h.side, "refs/heads/main"); exit != 1 {
		out = append(out, fmt.Sprintf("merge-base --is-ancestor side main exits %d, want 1", exit))
	}
	if lacks := cloneLacks(t, w, c, dir, h.commits[3]); lacks != "" {
		out = append(out, "a clone of main: "+lacks)
	}
	return out
}

// derivedOf is the derived files of part present in the incarnation at dir,
// by their names.
func derivedOf(t testing.TB, dir string, part gitproc.Part) []string {
	t.Helper()
	var out []string
	if part&gitproc.PartCommitGraph != 0 && statOK(t, filepath.Join(dir, "objects", "info", "commit-graph")) {
		out = append(out, "commit-graph")
	}
	if part&gitproc.PartMultiPackIndex != 0 {
		for _, name := range names(t, filepath.Join(dir, "objects", "pack")) {
			if strings.HasPrefix(name, "multi-pack-index") {
				out = append(out, name)
			}
		}
	}
	return out
}

// checkArm is one damage a previous process left: the case, the objects it
// names, and what it damaged — a derived part, or how many packs' stored
// files.
type checkArm struct {
	damage gittest.DamageCase
	ids    func(h history) []string
	part   gitproc.Part
	packs  int
}

// tip and tipBlob name the newer pack's objects to a case.
func tip(h history) []string     { return []string{h.commits[3]} }
func tipBlob(h history) []string { return []string{h.commits[3], h.blob} }

// THE FIRST FULL ROUND OF A PROCESS HOLDS EVERY INDEX FILE IT DID NOT WRITE TO
// ITS TRAILER, AND REPAIRS WHAT FAILS IN PLACE BEFORE IT ANSWERS. A previous
// process served the copy — the maintainer's index, bitmap and graph over its
// two packs — and left it damaged, one case an arm; this process's first
// round finds it. A derived file failing its trailer is DROPPED and written
// again at once — the graph graph-first, the index as a repair — never
// written over (git keeps a damaged bitmap whose index has not changed, and
// cannot write an index over offsets it cannot read). A pack whose .idx or
// .rev fails, or that is at the wrong size, is REINSTALLED with one GET a
// damaged pack, renamed over its files in the same incarnation. Every arm
// then answers main whole and right: four commits, the first an ancestor, the
// side commit not, and a clone with every object. A sound copy downloads
// nothing, drops nothing, and counts every file sound.
//
// Mutations: no check — the I9, side and IO arms answer wrong; derived files
// only — the stored arms' clone and counts fail; a stored file dropped rather
// than reinstalled — its objects missing; the wrong size alone — the R2 arm
// (a .rev of the right size with the wrong bytes) answers wrong.
func TestAFirstRoundChecksEveryIndexFileItDidNotWrite(t *testing.T) {
	t.Parallel()
	graph, midx := gitproc.PartCommitGraph, gitproc.PartMultiPackIndex
	none := func(history) []string { return nil }
	arms := []checkArm{
		{gittest.GraphSignature, none, graph, 0},
		{gittest.GraphParentSelf, tip, graph, 0},
		{gittest.GraphParentOther, func(h history) []string { return []string{h.commits[3], h.side} }, graph, 0},
		{gittest.GraphDate, tip, graph, 0},
		{gittest.GraphTrailer, none, graph, 0},
		{gittest.GraphFanout, none, graph, 0},
		{gittest.MidxTruncated, none, midx, 0},
		{gittest.MidxHashVersion, none, midx, 0},
		{gittest.MidxFirstChunkZeroed, none, midx, 0},
		{gittest.MidxChunkPastEnd, none, midx, 0},
		{gittest.MidxFanout, none, midx, 0},
		{gittest.MidxRidx, none, midx, 0},
		{gittest.MidxOffset, tipBlob, midx, 0},
		{gittest.MidxTrailer, none, midx, 0},
		{gittest.BitmapSignature, none, midx, 0},
		{gittest.BitmapTruncated, none, midx, 0},
		{gittest.BitmapVersion, none, midx, 0},
		{gittest.BitmapBody, none, midx, 0},
		{gittest.BitmapTrailer, none, midx, 0},
		// Every pack's .rev, and every pack's .idx offsets: two GETs.
		{gittest.RevTruncated, none, 0, 2},
		{gittest.RevSignature, none, 0, 2},
		{gittest.RevVersion, none, 0, 2},
		{gittest.RevHashID, none, 0, 2},
		{gittest.RevAppended, none, 0, 2},
		{gittest.IdxOffsetsPastPack, none, 0, 2},
		{gittest.RevSwapped, tip, 0, 1},
		{gittest.RevOverwritten, tip, 0, 1},
		{gittest.RevTrailer, tip, 0, 1},
		{gittest.IdxTruncated, tip, 0, 1},
		{gittest.IdxOffset, tipBlob, 0, 1},
		{gittest.IdxTrailer, tip, 0, 1},
	}
	for _, arm := range arms {
		t.Run(string(arm.damage), func(t *testing.T) {
			t.Parallel()
			w := newSyncWorld(t, onMemory)
			clk := newClock()
			h := w.measured(t)
			dir := w.served(t, h, clk)
			gittest.Damage(t, dir, arm.damage, arm.ids(h)...)

			c := w.open(t, clk.on)
			before, gets := cache.Repos(c)[h.repo], w.objects.total()
			s := w.sync(t, c, h.repo)
			if n := cache.Incarnation(s); n != before {
				t.Errorf("the repair answered incarnation %d, want %d in place", n, before)
			}
			got := w.objects.total() - gets
			switch {
			case arm.part != 0:
				if got != 0 {
					t.Errorf("the round downloaded %d packs for a derived file, want none", got)
				}
				if left := derivedOf(t, s.Dir, arm.part); len(left) != 0 {
					t.Errorf("the round left %v, which failed its trailer", left)
				}
				if lines := w.logs.lines(t, "code_cache_check_damaged"); len(lines) == 0 ||
					lines[0]["action"] != "dropped" || lines[0]["trigger"] != "first" {
					t.Errorf("code_cache_check_damaged = %v, want the file dropped by the first check", lines)
				}
			default:
				if got != arm.packs {
					t.Errorf("the round downloaded %d packs, want %d: one GET a damaged pack", got, arm.packs)
				}
				if lines := w.logs.lines(t, "code_cache_pack_reinstalled"); len(lines) != arm.packs {
					t.Errorf("code_cache_pack_reinstalled = %v, want %d", lines, arm.packs)
				}
			}
			// Written again at once: the graph graph-first, the index as a
			// repair — the clock not moved, so neither the debounce nor the
			// interval would let an ordinary run start.
			if ran := cache.MaintainerPass(c); arm.part != 0 && len(ran) != 1 {
				t.Errorf("the maintainer ran %v after the drop, want %s at once", ran, h.repo)
			}
			if packs, bitmap, graph := cache.Indexed(c, h.repo); packs != 2 || !bitmap || !graph {
				t.Errorf("after the repair the index covers %d packs (bitmap %t), graph %t; want both written "+
					"again", packs, bitmap, graph)
			}
			if cache.IndexRepair(c, h.repo) {
				t.Errorf("the index repair is still due once the index is written")
			}
			for _, bad := range wrong(t, w, c, s.Dir, h) {
				t.Errorf("after the repair: %s", bad)
			}
		})
	}

	t.Run("sound", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		clk := newClock()
		h := w.measured(t)
		dir := w.served(t, h, clk)
		files := 4 + len(derivedOf(t, dir, graph|midx)) // each pack's .idx and .rev, and the derived files
		ids := map[string][4]int64{}
		for _, name := range derivedOf(t, dir, graph|midx) {
			path := filepath.Join(dir, "objects", "pack", name)
			if name == "commit-graph" {
				path = filepath.Join(dir, "objects", "info", name)
			}
			ids[name] = gittest.FileIdentity(t, path)
		}

		c := w.open(t, clk.on)
		gets := w.objects.total()
		s := w.sync(t, c, h.repo)
		if got := w.objects.total() - gets; got != 0 {
			t.Errorf("a sound copy's first round downloaded %d packs, want none", got)
		}
		if got := counter(w.rec, metrics.CodeCacheChecks, "outcome", "sound"); got != uint64(files) {
			t.Errorf("%s{outcome=sound} = %d, want every one of the %d files", metrics.CodeCacheChecks, got, files)
		}
		if got := counter(w.rec, metrics.CodeCacheChecks, "outcome", "damaged"); got != 0 {
			t.Errorf("%s{outcome=damaged} = %d over a sound copy", metrics.CodeCacheChecks, got)
		}
		for name, id := range ids {
			path := filepath.Join(s.Dir, "objects", "pack", name)
			if name == "commit-graph" {
				path = filepath.Join(s.Dir, "objects", "info", name)
			}
			if got := gittest.FileIdentity(t, path); got != id {
				t.Errorf("the round replaced %s, which was sound", name)
			}
		}
		if got := len(cache.Vouched(c, h.repo)); got != files {
			t.Errorf("the round vouched for %d files, want the %d it checked", got, files)
		}
		for _, bad := range wrong(t, w, c, s.Dir, h) {
			t.Errorf("a sound copy: %s", bad)
		}
	})
}

// A FILE THIS PROCESS CHECKED IS NOT HASHED AGAIN — UNTIL IT IS ANOTHER FILE. A
// vouch is kept by the file's identity (its device, inode, size and
// modification time), so a later full round of the same process hashes
// nothing it checked, and a file renamed in over a checked one — a damaged
// copy — is another identity, hashed and caught.
//
// Mutations: vouch by path alone — the damaged copy renamed over the graph is
// passed; vouch nothing — the second round hashes every file.
func TestACheckedFileIsNotHashedAgain(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	h := w.measured(t)
	w.served(t, h, clk)

	var mu sync.Mutex
	var hashed []string
	c := w.open(t, clk.on, func(o *cache.Options) {
		cache.WhileHashing(o, func(path string) {
			mu.Lock()
			defer mu.Unlock()
			hashed = append(hashed, filepath.Base(path))
		})
	})
	s := w.sync(t, c, h.repo)
	mu.Lock()
	first := len(hashed)
	hashed = nil
	mu.Unlock()
	if first == 0 {
		t.Fatalf("the first round of the process hashed nothing")
	}

	// A second full round of the same process: every file vouched for.
	cache.Reread(c, cache.RebuildStream)
	w.sync(t, c, h.repo)
	mu.Lock()
	if len(hashed) != 0 {
		t.Errorf("a later full round hashed %v, every one of them checked already", hashed)
	}
	hashed = nil
	mu.Unlock()

	// A damaged copy of the graph renamed in over it: another file.
	graph := filepath.Join(s.Dir, "objects", "info", "commit-graph")
	raw, err := os.ReadFile(graph)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)/2] ^= 0xff
	staged := filepath.Join(t.TempDir(), "commit-graph")
	if err := os.WriteFile(staged, raw, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staged, graph); err != nil {
		t.Fatal(err)
	}
	cache.Reread(c, cache.RebuildStream)
	w.sync(t, c, h.repo)
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(hashed, []string{"commit-graph"}) {
		t.Errorf("the round after the graph was replaced hashed %v, want the graph alone", hashed)
	}
	if statOK(t, graph) {
		t.Errorf("the damaged graph renamed in over a checked one survived the round")
	}
}

// A BITMAP GIT KEPT IS NOT VOUCHED FOR BY THE RUN THAT KEPT IT. The
// maintainer's index step over an index whose packs have not changed keeps
// the bitmap beside it as it is — damaged or not — and exits 0, so a run
// vouches only for what it changed: a damaged bitmap put in under a cache
// whose index covers the same packs is still found by the next full round,
// and dropped.
//
// Mutation: vouch every derived file present after a run — the damaged bitmap
// is skipped by the round, and survives it.
func TestABitmapGitKeptIsNotVouchedByTheRunThatKeptIt(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	h := w.measured(t)
	w.served(t, h, clk)
	c := w.open(t, clk.on)
	s := w.sync(t, c, h.repo)
	packDir := filepath.Join(s.Dir, "objects", "pack")
	bitmap := bitmapOf(t, packDir)

	// Damaged under the cache — another identity, as a file copied in is.
	gittest.Damage(t, s.Dir, gittest.BitmapBody)
	kept := gittest.FileIdentity(t, bitmap)
	cache.WantIndex(c, h.repo)
	if ran := cache.MaintainerPass(c); len(ran) != 1 {
		t.Fatalf("the maintainer ran %v, want %s's index", ran, h.repo)
	}
	if got := gittest.FileIdentity(t, bitmap); got != kept {
		t.Fatalf("git wrote the bitmap again over an unchanged index; the case needs one it keeps")
	}
	if slices.Contains(cache.Vouched(c, h.repo), filepath.Join("objects", "pack", filepath.Base(bitmap))) {
		t.Errorf("the run vouched for the bitmap it kept")
	}
	cache.Reread(c, cache.RebuildStream)
	w.sync(t, c, h.repo)
	if statOK(t, bitmap) {
		t.Errorf("the next full round kept the damaged bitmap")
	}
}

// A PACK AT THE WRONG SIZE IS INSTALLED AGAIN BY RENAMING OVER ITS FILES, NEVER
// REMOVED FIRST: while its download runs, a git process over the copy still
// finds every object the pack holds where it was. Removed first, its objects
// were missing for the whole download.
//
// Mutation: move the pack's files into trash/ before the download — the read
// made while the download is held finds no such object.
func TestAPackAtTheWrongSizeIsReinstalledOverItsFiles(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	h := w.measured(t)
	dir := w.served(t, h, clk)
	gittest.Damage(t, dir, gittest.RevAppended)

	held := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	w.objects.mu.Lock()
	w.objects.hold = func(objstore.Key) <-chan struct{} {
		once.Do(func() { close(held) })
		return release
	}
	w.objects.mu.Unlock()
	c := w.open(t, clk.on)
	done := make(chan error, 1)
	go func() {
		s, err := c.Sync(context.Background(), h.repo, statelog.Freshness{Level: statelog.ReadLinearizable})
		if err == nil {
			s.Release()
		}
		done <- err
	}()
	select {
	case <-held:
	case err := <-done:
		t.Fatalf("the round ended without a download: %v", err)
	}
	for _, oid := range []string{h.commits[3], h.blob, h.commits[0]} {
		if out, err := runGit(t, w.pool, dir, "", "cat-file", "-t", oid); err != nil {
			t.Errorf("cat-file -t %s while the pack's download is held: %v (%s)", oid, err, out)
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("the round: %v", err)
	}
	lines := w.logs.lines(t, "code_cache_pack_reinstalled")
	if len(lines) != 2 || lines[0]["found"] != "size" {
		t.Errorf("code_cache_pack_reinstalled = %v, want both packs found at the wrong size", lines)
	}
}

// A CHECK HOLDS ONE READ SLOT A PIECE, AND NONE BETWEEN PIECES OR WHILE THE
// ROUND RUNS GIT. Hashing is a request's core, so it is a slot of the read
// lane — taken for one piece and given back before the next, so a read queued
// meanwhile is admitted between two pieces (the lane is first come, first
// served: gitproc's TestHoldReadIsAReadSlot) — and none is held while the
// round's own git runs, which waits in the same lane. A check that cannot have
// a slot in SlotWait fails the round busy, and the next round checks.
//
// Mutations: no slot — a piece sees every slot free; the slot held for a
// whole file or the whole check — a piece's release sees one still taken; the
// slot held across the peel — the peel's hook sees one taken (in a lane of one
// slot, the peel waits on the check); a refused slot read as a verdict — the
// busy round answers; a refused slot counted `unreadable`.
func TestTheCheckTakesAReadSlotAPieceAndNoneForTheRoundsGit(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	h := w.measured(t)
	w.served(t, h, clk)

	var (
		mu     sync.Mutex
		bad    []string
		pieces int
	)
	wrongly := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		bad = append(bad, fmt.Sprintf(format, args...))
	}
	c := w.open(t, clk.on, func(o *cache.Options) {
		pool := o.Git
		slots := pool.Slots(gitproc.ClassRead)
		cache.WhileHashing(o, func(path string) {
			if free := pool.Free(gitproc.ClassRead); free != slots {
				wrongly("%d of %d read slots free before %s is hashed", free, slots, filepath.Base(path))
			}
		})
		cache.CheckPiecesOf(o, 64, func(path string, held bool) {
			want := slots
			if held {
				want = slots - 1
				mu.Lock()
				pieces++
				mu.Unlock()
			}
			if free := pool.Free(gitproc.ClassRead); free != want {
				wrongly("%d of %d read slots free as a piece of %s is hashed (held %t), want %d", free, slots,
					filepath.Base(path), held, want)
			}
		})
		cache.BeforePeel(o, func(string) {
			if free := pool.Free(gitproc.ClassRead); free != slots {
				wrongly("%d of %d read slots free as the round's peel runs", free, slots)
			}
		})
	})

	// Every read slot held elsewhere: the check cannot start, and the round
	// is refused busy rather than answered.
	var holds []func()
	for range w.pool.Slots(gitproc.ClassRead) {
		release, err := w.pool.HoldRead(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		holds = append(holds, release)
	}
	if _, err := c.Sync(t.Context(), h.repo, statelog.Freshness{Level: statelog.ReadLinearizable}); !errors.Is(err,
		gitproc.ErrBusy) {
		t.Errorf("a round whose check found every read slot held answered %v, want %v", err, gitproc.ErrBusy)
	}
	// A slot refused says nothing of the file: no outcome, and never
	// `unreadable`, which an operator reads as a disk losing data.
	if got := counter(w.rec, metrics.CodeCacheChecks, "outcome", "unreadable"); got != 0 {
		t.Errorf("%s{outcome=unreadable} = %d after a refused slot, want none", metrics.CodeCacheChecks, got)
	}
	for _, release := range holds {
		release()
	}
	mu.Lock()
	bad = nil // what the hooks saw while the case held the lane is the case's own doing
	mu.Unlock()

	// The next round checks, a piece at a time.
	w.sync(t, c, h.repo)
	mu.Lock()
	defer mu.Unlock()
	for _, b := range bad {
		t.Error(b)
	}
	files := 4 + len(derivedOf(t, w.dir(t, c, h.repo), gitproc.PartCommitGraph|gitproc.PartMultiPackIndex))
	if pieces <= files {
		t.Errorf("the check hashed %d pieces of %d files, want the files in several pieces each", pieces, files)
	}
	if got := counter(w.rec, metrics.CodeCacheChecks, "trigger", "first"); got == 0 {
		t.Errorf("the round after the busy one checked nothing as the process's first")
	}
}

// A VERDICT HOLDS ONLY FOR THE FILE IT HASHED, AND A REPAIR ACTS ONLY ON THAT
// FILE. A file another is renamed in over while it is hashed gets no verdict —
// its successor is no file the hash read — and is neither dropped nor
// installed again; the next round hashes the successor. A file found damaged
// and put right before the repair acts — by the maintainer, by an earlier
// repair — is no finding any more, and the repair leaves its successor.
//
// Mutation: judge by path — the hash's verdict given whatever the path holds
// after it, or the repair acting on whatever is at the path — and the sound
// successor is dropped or downloaded again.
func TestAVerdictHoldsOnlyForTheFileItHashed(t *testing.T) {
	t.Parallel()
	graph := func(history) string { return filepath.Join("objects", "info", "commit-graph") }
	for name, tc := range map[string]struct {
		damage  gittest.DamageCase
		ids     func(history) []string
		file    func(history) string // the file damaged, relative to the incarnation
		swapAt  func(history) string // the file whose hash the swap is made at
		midRead bool                 // the swap is made while file's own first piece is hashed
	}{
		"a graph replaced while it is hashed": {
			damage: gittest.GraphTrailer, file: graph, midRead: true,
		},
		"a graph replaced before its drop": {
			damage: gittest.GraphTrailer, file: graph,
			swapAt: func(history) string { return filepath.Join("objects", "pack", "multi-pack-index") },
		},
		"an index replaced before its pack is installed again": {
			damage: gittest.IdxTrailer, ids: tip,
			file: func(h history) string {
				return filepath.Join("objects", "pack", "pack-"+h.newer.GitChecksum+".idx")
			},
			swapAt: func(h history) string {
				return filepath.Join("objects", "pack", "pack-"+h.newer.GitChecksum+".rev")
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := newSyncWorld(t, onMemory)
			clk := newClock()
			h := w.measured(t)
			dir := w.served(t, h, clk)
			file := filepath.Join(dir, tc.file(h))
			sound, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			if tc.ids != nil {
				ids = tc.ids(h)
			}
			gittest.Damage(t, dir, tc.damage, ids...)

			var once sync.Once
			swap := func() {
				once.Do(func() {
					staged := filepath.Join(w.path("tmp"), "sound")
					if err := os.WriteFile(staged, sound, 0o444); err != nil {
						t.Error(err)
						return
					}
					if err := os.Rename(staged, file); err != nil {
						t.Error(err)
					}
				})
			}
			c := w.open(t, clk.on, func(o *cache.Options) {
				if tc.midRead {
					cache.CheckPiecesOf(o, 64, func(path string, held bool) {
						if held && path == file {
							swap()
						}
					})
					return
				}
				cache.WhileHashing(o, func(path string) {
					if path == filepath.Join(dir, tc.swapAt(h)) {
						swap()
					}
				})
			})
			gets := w.objects.total()
			w.sync(t, c, h.repo)
			if got := w.objects.total() - gets; got != 0 {
				t.Errorf("the round downloaded %d packs for a file put right before it acted", got)
			}
			if !statOK(t, file) {
				t.Fatalf("the round removed %s, the sound file renamed in over the damaged one", tc.file(h))
			}
			if got, err := os.ReadFile(file); err != nil || !bytes.Equal(got, sound) {
				t.Errorf("%s is not the sound file renamed in: %v", tc.file(h), err)
			}
			replaced := counter(w.rec, metrics.CodeCacheChecks, "outcome", "replaced")
			if tc.midRead && replaced != 1 {
				t.Errorf("%s{outcome=replaced} = %d, want the one file replaced while it was hashed",
					metrics.CodeCacheChecks, replaced)
			}
			if slices.Contains(cache.Vouched(c, h.repo), tc.file(h)) {
				t.Errorf("the round vouched for %s, which no hash of it read", tc.file(h))
			}

			// The next round hashes the successor, and finds it sound.
			sounds := counter(w.rec, metrics.CodeCacheChecks, "outcome", "sound")
			cache.Reread(c, cache.RebuildStream)
			w.sync(t, c, h.repo)
			if got := counter(w.rec, metrics.CodeCacheChecks, "outcome", "sound") - sounds; got != 1 {
				t.Errorf("the next round found %d files sound, want the successor alone", got)
			}
			if !slices.Contains(cache.Vouched(c, h.repo), tc.file(h)) {
				t.Errorf("the next round did not vouch for the successor it hashed")
			}
		})
	}
}

// BenchmarkCheck is what the check costs a copy's first request after a
// restart: the repository BenchmarkMidx builds (64 packs of a source
// repository's history), its maintainer's index, bitmap and graph written,
// the cache restarted — so no file is vouched for — and one linearizable
// sync, whose full round holds every index file to its trailer before it
// answers.
//
//   - cache=page: the files in the page cache, as on a node whose last
//     process served them moments before;
//   - cache=cold: every file of the incarnation dropped from the page cache
//     first (posix_fadvise), so the hash reads at the volume's throughput.
//
// Each reports check-ms, the check's own time (crewlet.code.cache.check.
// duration), beside the whole first round's ns/op; checked-MB, the index
// files it read; and s/GB, the check's seconds per gigabyte of the copy's
// packs — the figure the package doc and the deployment guide carry.
func BenchmarkCheck(b *testing.B) {
	quiet(b)
	for _, arm := range []struct {
		name  string
		bytes int64
	}{{"100MB", 100_000_000}, {"1GB", 1_000_000_000}, {"5GB", 5_000_000_000}} {
		b.Run("size="+arm.name, func(b *testing.B) {
			if arm.bytes > 1_000_000_000 {
				gittest.Large(b, "a 5 GB history fast-imported, packed, stored and synced")
			}
			w := newSyncWorld(b, onMemory)
			w.processes = runtime.NumCPU()
			repo, src, _, _, total, _ := w.midxRepo(b, arm.bytes)
			if err := os.RemoveAll(src); err != nil {
				b.Fatal(err)
			}
			clk := newClock()
			quietly := func(o *cache.Options) { o.MaxBytes, o.Logger = 1<<40, nil }
			c := w.open(b, clk.on, quietly)
			w.sync(b, c, repo)
			clk.Add(cache.MidxDebounce)
			if ran := cache.MaintainerPass(c); len(ran) != 1 {
				b.Fatalf("the maintainer ran %v, want the repository", ran)
			}
			dir := w.dir(b, c, repo)
			checked := indexBytes(b, dir)
			for _, mode := range []string{"page", "cold"} {
				b.Run("cache="+mode, func(b *testing.B) {
					var checkSum float64
					var checks uint64
					for b.Loop() {
						b.StopTimer()
						c = w.reopen(b, c, clk.on, quietly)
						if mode == "cold" && !dropCached(b, dir) {
							b.Skip("no way to drop a file's cached pages on this platform")
						}
						before := checkDuration(w.rec)
						b.StartTimer()
						s, err := c.Sync(b.Context(), repo, statelog.Freshness{Level: statelog.ReadLinearizable})
						b.StopTimer()
						if err != nil {
							b.Fatal(err)
						}
						s.Release()
						after := checkDuration(w.rec)
						checkSum += after.Sum - before.Sum
						checks += after.Count - before.Count
						b.StartTimer()
					}
					if checks == 0 {
						b.Fatal("no first round checked anything")
					}
					ms := checkSum / float64(checks)
					b.ReportMetric(ms, "check-ms")
					b.ReportMetric(float64(checked)/1e6, "checked-MB")
					b.ReportMetric(ms/1e3/(float64(total)/1e9), "s/GB")
				})
			}
		})
	}
}

// indexBytes is the bytes of every file a first round's check reads in the
// incarnation at dir: each pack's .idx and .rev, and the derived files.
func indexBytes(b *testing.B, dir string) int64 {
	b.Helper()
	var n int64
	add := func(path string) {
		if info, err := os.Lstat(path); err == nil {
			n += info.Size()
		}
	}
	for _, name := range names(b, filepath.Join(dir, "objects", "pack")) {
		if !strings.HasSuffix(name, ".pack") {
			add(filepath.Join(dir, "objects", "pack", name))
		}
	}
	add(filepath.Join(dir, "objects", "info", "commit-graph"))
	return n
}

// checkDuration is crewlet.code.cache.check.duration as rec holds it: how
// many checks, and their milliseconds.
func checkDuration(rec *metrics.Recorder) metrics.Snapshot {
	var out metrics.Snapshot
	for _, s := range rec.Read() {
		if s.Name == metrics.CodeCacheCheckDuration {
			out.Count += s.Count
			out.Sum += s.Sum
		}
	}
	return out
}

// failOnce is a disk that cannot read one file once: the ReadFails knob,
// answering EIO for the first piece read of the first file whose path ends
// with suffix, once armed.
type failOnce struct {
	suffix atomic.Value // string; "" disarmed
	failed atomic.Bool
}

func (f *failOnce) at(path string) error {
	if s, _ := f.suffix.Load().(string); s == "" || !strings.HasSuffix(path, s) || !f.failed.CompareAndSwap(false, true) {
		return nil
	}
	return &os.PathError{Op: "read", Path: path, Err: syscall.EIO}
}

// A FILE THE DISK CANNOT READ IS REPAIRED AS ONE THAT FAILED ITS CHECKSUM. A
// bad sector under a checkable file answers its read with an error, not a
// verdict — and a full round that failed over it left the repository unserved
// on the node for good: no round could ever pass the file, and the
// maintainer, whose rewrite would have replaced a derived one, runs only on a
// copy a round has converged. So the check, and the scrub, count it
// `unreadable` and take it as damaged at the identity it had: a pack's index
// is installed again over its files — one GET, a new inode where the bad
// sector was — a derived file dropped and written again, a pack's body
// fetched again by the scrub's repair round. Every arm then answers whole.
//
// Mutation: fail the round over an unreadable file (the arms' syncs fail, and
// keep failing); pass it over in the scrub (the pack is never fetched again).
func TestAFileTheDiskCannotReadIsRepaired(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, suffix, file string
		gets               int
		scrub              bool
	}{
		{"a pack's index, at the first round's check", ".idx", "idx", 1, false},
		{"a bitmap, at the first round's check", ".bitmap", "bitmap", 0, false},
		{"a pack, at a scrub", ".pack", "pack", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newSyncWorld(t, onMemory)
			clk := newClock()
			h := w.measured(t)
			w.served(t, h, clk)
			var disk failOnce
			disk.suffix.Store("")
			c := w.open(t, clk.on, func(o *cache.Options) { cache.ReadFails(o, disk.at) })
			trigger := "first"
			if tc.scrub {
				w.sync(t, c, h.repo)
				if done := cache.ScrubPass(c); len(done) != 1 {
					t.Fatalf("the copy's first scrub ran for %v", done)
				}
				clk.Add(cache.ScrubInterval)
				trigger = "scrub"
			}
			disk.suffix.Store("pack-" + h.newer.GitChecksum + tc.suffix)
			if tc.suffix == ".bitmap" {
				disk.suffix.Store(tc.suffix)
			}
			before, gets := cache.Repos(c)[h.repo], w.objects.total()
			s := w.sync(t, c, h.repo)
			if tc.scrub {
				if done := cache.ScrubPass(c); len(done) != 1 {
					t.Fatalf("the used copy's scrub ran for %v", done)
				}
				eventually(t, "the scrub's findings repaired", func() bool {
					return cache.Settled(c, h.repo) && len(cache.ScrubFindings(c, h.repo)) == 0
				})
				s = w.sync(t, c, h.repo)
			}
			if !disk.failed.Load() {
				t.Fatal("no read of the file failed: the case tested nothing")
			}
			if got := checks(w, tc.file, "unreadable", trigger); got != 1 {
				t.Errorf("checks{file=%s,outcome=unreadable,trigger=%s} = %d, want 1", tc.file, trigger, got)
			}
			if got := w.objects.total() - gets; got != tc.gets {
				t.Errorf("the repair downloaded %d packs, want %d", got, tc.gets)
			}
			if n := cache.Incarnation(s); n != before {
				t.Errorf("the repair answered incarnation %d, want %d in place", n, before)
			}
			cache.MaintainerPass(c)
			if packs, bitmap, graph := cache.Indexed(c, h.repo); packs != 2 || !bitmap || !graph {
				t.Errorf("after the repair the index covers %d packs (bitmap %t), graph %t; want all written",
					packs, bitmap, graph)
			}
			for _, bad := range wrong(t, w, c, s.Dir, h) {
				t.Errorf("after the repair: %s", bad)
			}
		})
	}
}

// A PACK'S VOUCHES FIT IN A QUARTER OF A KILOBYTE. Every incarnation a node
// holds keeps a vouch for each of its packs' .idx and .rev, for as long as it
// holds the copy, and a pack is added by every push — so the vouches are kept
// as ONE entry a pack keyed by its checksum's twenty bytes, and none for the
// .pack, which the check never reads: measured on Go 1.27.2, 118 to 207 bytes
// a pack across the map's growth (168 at 300 000), where keyed by each file's
// sixty-byte path, a .pack's among them, they took 528.
//
// Not parallel: the heap is measured, and another case allocating beside it
// would be counted as the vouches'. The least of three readings is taken for
// the same reason.
//
// Mutation: key the vouches by each file's path (a pack costs 528 bytes).
func TestAPacksVouchesFitInAQuarterKilobyte(t *testing.T) {
	for _, packs := range []int{3_000, 300_000} {
		least := math.Inf(1)
		for range 3 {
			least = min(least, cache.VouchMemory(packs))
		}
		if least > 256 {
			t.Errorf("the vouches of %d packs cost %.0f bytes a pack, want at most 256", packs, least)
		}
	}
}

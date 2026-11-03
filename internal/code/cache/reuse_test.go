package cache_test

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
)

// BenchmarkPackReuse is what ServeConfig's pack.allowPackReuse costs a CLONE
// in each value git offers a multi-pack-index bitmap — `single`, the
// bitmap's preferred pack sent verbatim and every other object packed as
// pack-objects always packs one (a stored delta reused, a whole copy searched
// for a delta), and `multi`, verbatim from every pack the bitmap covers — in
// the two figures the value is decided on: the BYTES the clone is sent
// ("bytes", what the client's link paces the transfer slot by and every CI
// clone downloads) and the node's CPU for the clone's pack-objects
// ("server-cpu-ms", gitproc.Result.CPU). Everything else is the serve
// configuration as it stands.
//
// Every arm is a copy THE CACHE MADE: each push received as a person's git
// pushes it — a THIN pack, completed by receive-pack's index-pack --fix-thin,
// which APPENDS WHOLE every base the push's deltas borrowed from the packs
// before it, and kept as one pack (transfer.unpackLimit=1) — stored as its
// bundle and its row, installed by a cold round, and indexed by the cache's
// own maintainer, its bitmap written by the git that then serves it — so the
// bitmap prefers the pack git chooses for an index written with no
// --preferred-pack: the oldest .pack to the second, and among a second's
// packs, which a cold round's nearly all are, whichever git lists first,
// reported beside each arm (preferredShare). So an object a later push
// edited sits in two packs, a delta in its own push's and whole in the next
// one's, and the multi-pack-index keeps ONE copy, as git's midx_oid_compare
// orders them: the preferred pack's; else the copy in the pack whose .pack
// is newer to the second; and among one second's packs the first in the
// write's own listing of the pack directory, the filesystem's order, never
// the pushes' (an entry carried from an index already on disk counts as
// oldest of all). A cold round installs nearly every pack within one second,
// so on these copies the listing chooses which edited objects the index
// keeps whole and which as their deltas — and `multi` sends as they are
// stored the whole copies it keeps.
//
//   - history=tree is 200 pushes each ADDING fifty files of 3 000 random
//     bytes (code/host's BenchmarkCloneWarm tree): nothing is edited, so a
//     duplicate is only a tree a later push borrowed.
//   - history=edits is gittest.History's seeded history of edits (every
//     commit four files of one directory, each version rewriting a window of
//     its file) pushed as 200 pushes: every push edits files an earlier push
//     wrote, as a team's pushes do, and borrows each one whole.
//   - history=compacted is edits once all but its last 20 pushes are ONE pack
//     written by pack-objects — no base appended, every delta searched — the
//     layout geometric compaction leaves (M3's compactor), its tail still a
//     pack a push.
//
// On the node pin and on the node floor's own build (git=pin, git=floor),
// since what each value sends is the serving git's doing.
func BenchmarkPackReuse(b *testing.B) {
	quiet(b)
	w := newSyncWorld(b, onMemory)
	treeSrc := gittest.Empty(b)
	gittest.FastImport(b, w.client, treeSrc, reuseTreeStream(200, 50, 3_000))
	treeTips := commitsOf(b, w.client, treeSrc, "main")
	edits, editTips := reuseEdits(b, w)
	compacted := slices.Concat([]gittest.Bundled{reuseCompacted(b, w, edits.src, editTips[len(editTips)-21])},
		edits.bundles[len(edits.bundles)-20:])
	for i, h := range []struct {
		name    string
		bundles []gittest.Bundled
		tips    []string
	}{
		{"tree", received(b, w.client, treeSrc, treeTips), treeTips},
		{"edits", edits.bundles, editTips},
		{"compacted", compacted, editTips[len(editTips)-21:]},
	} {
		repo := w.logRepo(i + 1)
		for j, bundle := range h.bundles {
			row, err := w.putBundle(b.Context(), bundle, "")
			if err != nil {
				b.Fatal(err)
			}
			w.log.write(repo, []code.PackRow{row}, map[string]string{"refs/heads/main": h.tips[j]})
		}
		b.Run("history="+h.name, func(b *testing.B) {
			for k, git := range []struct {
				name string
				path func(testing.TB) string
			}{{"pin", gittest.RequireNode}, {"floor", gittest.NodeFloor}} {
				b.Run("git="+git.name, func(b *testing.B) {
					w.reuseArms(b, repo, git.path(b), i*2+k)
				})
			}
		})
	}
}

// reuseArms syncs repo cold into a cache of its own whose pool runs git, has
// its maintainer write the bitmap, and runs the reuse arms over that copy.
func (w *syncWorld) reuseArms(b *testing.B, repo, git string, n int) {
	b.Helper()
	clk := newClock()
	var pool *gitproc.Pool
	c, drop := w.coldCache(b, b.TempDir(), n, cache.FetchConcurrency, clk.on, func(o *cache.Options) {
		pool = newPoolOf(b, o.Layout, git, runtime.NumCPU(), 0, 1<<40)
		o.Git = pool
	})
	defer drop()
	s, err := c.Sync(b.Context(), repo, linearizable)
	if err != nil || s.Kind != cache.SyncFull {
		b.Fatalf("the cold round: a %s round, %v", s.Kind, err)
	}
	s.Release()
	clk.Add(cache.MidxDebounce)
	if ran := cache.MaintainerPass(c); len(ran) != 1 {
		b.Fatalf("the maintainer ran %v, want the repository", ran)
	}
	s = w.sync(b, c, repo)
	defer s.Release()
	if found, _ := filepath.Glob(filepath.Join(s.Dir, "objects", "pack", "multi-pack-index-*.bitmap")); len(found) == 0 {
		b.Fatal("the maintainer wrote no bitmap: there would be nothing to reuse")
	}
	preferred, rank := preferredShare(b, s.Dir)
	for _, reuse := range []string{"single", "multi"} {
		b.Run("reuse="+reuse, func(b *testing.B) {
			config := withReuse(c.ServeConfig(), reuse)
			var (
				sent int64
				cpu  time.Duration
			)
			for b.Loop() {
				out := &byteCount{}
				res, err := pool.Run(b.Context(), gitproc.Cmd{
					Dir:    s.Dir,
					Args:   []string{"pack-objects", "--revs", "--thin", "--stdout", "--delta-base-offset", "-q"},
					Config: config, Stdin: strings.NewReader("refs/heads/main\n"), Stdout: out,
					Class: gitproc.ClassRead, Deadline: time.Hour,
				})
				if err != nil {
					b.Fatalf("a clone's pack with pack.allowPackReuse=%s: %v", reuse, err)
				}
				sent, cpu = out.n, cpu+res.CPU
			}
			b.ReportMetric(float64(sent), "bytes")
			b.ReportMetric(float64(cpu.Microseconds())/1e3/float64(b.N), "server-cpu-ms")
			b.ReportMetric(preferred, "preferred-%objects")
			b.ReportMetric(float64(rank), "preferred-rank")
		})
	}
}

// preferredShare is what the copy at dir's bitmap PREFERS — the pack single
// reuse sends as it is stored, git's choice for the maintainer's index: its
// share of the objects every pack holds, in percent, and its rank among the
// packs by object count (1, the most). Reported beside each arm, because the
// choice is the oldest .pack to the second and, among a second's packs,
// whichever git lists first, so a copy installed cold prefers a pack no rule
// of the cache picked — and that pack is what single sends as it is stored.
func preferredShare(b *testing.B, dir string) (percent float64, rank int) {
	b.Helper()
	objects := func(idx string) int64 {
		head, err := os.ReadFile(idx)
		if err != nil || len(head) < 8+256*4 {
			b.Fatalf("the index %s: %v", idx, err)
		}
		return int64(binary.BigEndian.Uint32(head[8+255*4:]))
	}
	idxs, err := filepath.Glob(filepath.Join(dir, "objects", "pack", "pack-*.idx"))
	if err != nil || len(idxs) == 0 {
		b.Fatalf("the copy's indexes: %v", err)
	}
	mine := objects(gittest.PreferredPack(b, dir) + ".idx")
	var all int64
	rank = 1
	for _, idx := range idxs {
		n := objects(idx)
		all += n
		if n > mine {
			rank++
		}
	}
	return 100 * float64(mine) / float64(all), rank
}

// withReuse is config with pack.allowPackReuse set to value, and every other
// pair as it is — a serve configuration with no such pair fails the arm,
// since there would be nothing it measures.
func withReuse(config []gitproc.KV, value string) []gitproc.KV {
	out := slices.Clone(config)
	for i, kv := range out {
		if kv.Key == "pack.allowPackReuse" {
			out[i].Value = value
			return out
		}
	}
	panic("ServeConfig sets no pack.allowPackReuse")
}

// byteCount counts what it is given and keeps none of it: the pack a clone is
// sent, measured.
type byteCount struct{ n int64 }

func (c *byteCount) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// receivePack is the receive-pack a person's push reaches in received: one
// pack a push, never unpacked, as the code host's receive configuration keeps
// it (host/receive.go), and named on its command line because a local push
// runs it with none of the pushing process's configuration.
const receivePack = "git -c transfer.unpackLimit=1 -c receive.autogc=false -c pack.writeReverseIndex=true receive-pack"

// received is each of tips pushed in turn from src into a repository of its
// own, as a person's git pushes — thin, so receive-pack completes each pack
// with the whole bases it borrowed — read back as the bundle each push
// stores, in push order.
func received(b *testing.B, client *gittest.Client, src string, tips []string) []gittest.Bundled {
	b.Helper()
	recv := gittest.Empty(b)
	have := map[string]bool{}
	out := make([]gittest.Bundled, 0, len(tips))
	for _, tip := range tips {
		client.Run(b, src, "push", "--quiet", "--receive-pack="+receivePack, recv, tip+":refs/heads/main")
		packs, err := filepath.Glob(filepath.Join(recv, "objects", "pack", "pack-*.pack"))
		if err != nil {
			b.Fatal(err)
		}
		var fresh []string
		for _, p := range packs {
			cs := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), "pack-"), ".pack")
			if !have[cs] {
				have[cs] = true
				fresh = append(fresh, cs)
			}
		}
		if len(fresh) != 1 {
			b.Fatalf("the push of %s left %d new packs, want the one it received", tip, len(fresh))
		}
		bundle, err := gittest.ReadBundle(recv, fresh[0])
		if err != nil {
			b.Fatal(err)
		}
		out = append(out, bundle)
	}
	return out
}

// reuseTreeStream is the history=tree fast-import stream: pushes commits on
// main, each adding files new files of size seeded random bytes, file f in
// directory f mod pushes·files/100 — so every push touches every directory
// it writes into, as host's treeFixture does.
func reuseTreeStream(pushes, files int, size int64) func(*bufio.Writer) error {
	return func(w *bufio.Writer) error {
		data, _ := chacha(drawsReuse, 0)
		dirs := pushes * files / 100
		for i := range pushes {
			msg := fmt.Sprintf("push %d\n", i)
			fmt.Fprintf(w, "commit refs/heads/main\ncommitter %s %d +0000\ndata %d\n%s", benchIdent, benchEpoch+i, len(msg), msg)
			for j := range files {
				f := i*files + j
				fmt.Fprintf(w, "M 100644 inline d%04d/f%06d.bin\ndata %d\n", f%dirs, f, size)
				if _, err := io.CopyN(w, data, size); err != nil {
					return err
				}
				if err := w.WriteByte('\n'); err != nil {
					return err
				}
			}
		}
		return nil
	}
}

// editHistory is the history=edits source and what its pushes stored.
type editHistory struct {
	src     string
	bundles []gittest.Bundled
}

// reuseEdits is gittest.History's 20 MB history pushed as 200 pushes of
// consecutive commits, and each push's tip.
func reuseEdits(b *testing.B, w *syncWorld) (editHistory, []string) {
	b.Helper()
	src, commits := gittest.History(b, w.client, 20_000_000)
	pushes := splitPushes(commits, 200)
	tips := make([]string, len(pushes))
	for i, p := range pushes {
		tips[i] = p.tip
	}
	return editHistory{src: src, bundles: received(b, w.client, src, tips)}, tips
}

// reuseCompacted is ONE pack of every object tip reaches in src, written by
// pack-objects with its delta search — what a compaction of every push up to
// tip stores — read back as its bundle.
func reuseCompacted(b *testing.B, w *syncWorld, src, tip string) gittest.Bundled {
	b.Helper()
	dir := gittest.Empty(b)
	return gittest.Bundle(b, dir, gittest.Pack(b, w.client, dir, src, tip))
}

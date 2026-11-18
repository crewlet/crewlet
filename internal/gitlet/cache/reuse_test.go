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

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
)

// BenchmarkPackReuse measures each pack.allowPackReuse value a multi-pack-index
// bitmap offers (single: its preferred pack verbatim; multi: verbatim from every
// pack it covers) in the figures ServeConfig's value is decided on (serve.go):
// "bytes" a clone is sent and "server-cpu-ms" of its pack-objects. Every arm
// serves a copy THE CACHE MADE (thin pushes received as one pack each, a cold
// round, the maintainer's bitmap), since that layout decides which pack the
// bitmap prefers (preferredShare) and which copy of an edited object the index
// keeps. Histories: tree (200 pushes adding files, nothing edited), edits
// (gittest.History as 200 pushes, each editing earlier files) and compacted
// (edits with all but its last 20 pushes one pack-objects pack, as geometric
// compaction leaves it); each on git=pin and git=floor, since what a value
// sends is the serving git's doing.
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
			w.log.write(repo, []gitlet.PackRow{row}, map[string]string{"refs/heads/main": h.tips[j]})
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

// preferredShare is the share, in percent, of all objects held by the pack
// the copy at dir's bitmap PREFERS (what single sends as stored), and its rank
// by object count (1, the most). Reported beside each arm because git, not the
// cache, picks it: the oldest .pack to the second, then whichever git lists
// first.
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

// withReuse is config with pack.allowPackReuse set to value; a config with no
// such pair panics, since the arm would measure nothing.
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

// byteCount counts the bytes of a clone's pack and keeps none.
type byteCount struct{ n int64 }

func (c *byteCount) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// receivePack keeps one pack a push, never unpacked, as Gitlet's receive
// configuration does (host/receive.go); its configuration is on its command
// line because a local push runs it with none of the pusher's.
const receivePack = "git -c transfer.unpackLimit=1 -c receive.autogc=false -c pack.writeReverseIndex=true receive-pack"

// received pushes each of tips in turn from src into a fresh repository, thin
// as a person's git pushes (so receive-pack appends the whole bases each
// borrowed), and answers the bundle each push stores, in push order.
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
// directory f mod pushes·files/100, so every push touches every directory it
// writes into, as host's treeFixture does.
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

// reuseCompacted is ONE pack-objects pack (delta search on) of every object
// tip reaches in src, as a compaction up to tip stores it, read back as its
// bundle.
func reuseCompacted(b *testing.B, w *syncWorld, src, tip string) gittest.Bundled {
	b.Helper()
	dir := gittest.Empty(b)
	return gittest.Bundle(b, dir, gittest.Pack(b, w.client, dir, src, tip))
}

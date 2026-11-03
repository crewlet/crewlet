package cache_test

import (
	"bytes"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
)

// THE SERVE CONFIGURATION IS WHAT THE HOST RUNS: exactly these pairs, every
// key one the pool's allowlist admits — upload-pack runs with them — and none
// that would serve an object no advertised tip reaches, nor any the pool sets
// for every process itself. pack.allowPackReuse is single whatever the pool's
// git — the reuse that sends a cache's clones the fewest bytes
// (BenchmarkPackReuse) — and every git at the node floor serves it whole
// (gitproc's TestEveryPackReuseModeSendsOverlappingPacksWhole).
func TestTheServeConfigIsWhatTheHostRuns(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	tip, _ := w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	s := w.sync(t, c, repo)

	got := c.ServeConfig()
	want := []gitproc.KV{
		{Key: "core.multiPackIndex", Value: "true"},
		{Key: "pack.allowPackReuse", Value: "single"},
		{Key: "pack.windowMemory", Value: strconv.FormatInt(2*code.DefaultLimits().MaxBlobBytes, 10)},
		{Key: "pack.useBitmaps", Value: "true"},
		{Key: "pack.writeBitmapLookupTable", Value: "true"},
		{Key: "commitGraph.changedPathsVersion", Value: "2"},
		{Key: "uploadpack.allowFilter", Value: "true"},
		{Key: "uploadpackfilter.allow", Value: "false"},
		{Key: "uploadpackfilter.blob:none.allow", Value: "true"},
		{Key: "uploadpackfilter.blob:limit.allow", Value: "true"},
		{Key: "uploadpackfilter.tree.maxDepth", Value: "0"},
		{Key: "uploadpackfilter.object:type.allow", Value: "true"},
		{Key: "uploadpackfilter.combine.allow", Value: "true"},
		{Key: "uploadpack.keepAlive", Value: "5"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("ServeConfig =\n%v\nwant\n%v", got, want)
	}
	for _, kv := range got {
		switch strings.ToLower(kv.Key) {
		case "uploadpack.allowanysha1inwant", "uploadpack.allowreachablesha1inwant", "uploadpack.allowtipsha1inwant",
			"uploadpack.allowrefinwant",
			"transfer.advertiseobjectinfo", "uploadpack.advertisebundleuris",
			"protocol.allow", "gc.auto", "maintenance.auto", "pack.threads", "core.hookspath":
			t.Errorf("ServeConfig sets %s", kv.Key)
		}
	}

	res, err := w.pool.Run(t.Context(), gitproc.Cmd{
		Dir: s.Dir, Args: []string{"upload-pack", "--stateless-rpc", "--advertise-refs", s.Dir},
		Config: got, Class: gitproc.ClassTransfer, Share: suiteShare, Deadline: time.Minute,
	})
	if err != nil {
		t.Fatalf("upload-pack with the serve configuration: %v", err)
	}
	if !strings.Contains(string(res.Stdout), tip+" refs/heads/main") {
		t.Errorf("upload-pack's advertisement does not list main at %s:\n%s", tip, res.Stdout)
	}
}

// advertise is a push advertisement — receive-pack --stateless-rpc
// --advertise-refs — run in dir through pool with config, its output
// streamed (at code.MaxRefsPerRepo it outgrows the pool's capture).
func advertise(t testing.TB, pool *gitproc.Pool, dir, alternate string, config []gitproc.KV) string {
	t.Helper()
	var out bytes.Buffer
	_, err := pool.Run(t.Context(), gitproc.Cmd{
		Dir: dir, Alternate: alternate, Args: []string{"receive-pack", "--stateless-rpc", "--advertise-refs", dir},
		Config: config, Stdout: &out, Class: gitproc.ClassTransfer, Share: suiteShare, Deadline: time.Minute,
	})
	if err != nil {
		t.Fatalf("receive-pack --advertise-refs in %s: %v", dir, err)
	}
	return out.String()
}

// THE ADVERTISE CONFIGURATION IS WHAT A VIEW RUNS: one pair, a key the pool's
// allowlist admits, that keeps a view's receive-pack from listing the refs of
// its one alternate — the incarnation, whose refs ARE the view's own
// packed-refs, so the listing (a for-each-ref over the same file, as dear as
// the advertisement itself) printed nothing the view does not already say.
// The prefix it names can be no ref at all, so no repository's refs come back
// through it. The view advertises exactly what it did; that the alternate's
// refs went unread shows once the incarnation has moved past the view, when
// the view without the configuration prints the newer tip as a .have.
func TestTheAdvertiseConfigIsWhatAViewRuns(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	w.tag(t, repo, "v1", true)
	c := w.open(t)
	s := w.sync(t, c, repo)

	got := c.AdvertiseConfig()
	if len(got) != 1 || got[0].Key != "core.alternateRefsPrefixes" {
		t.Fatalf("AdvertiseConfig = %v, want core.alternateRefsPrefixes alone", got)
	}
	if prefix := got[0].Value; gitref.ValidRef(prefix) || gitref.ValidRef(prefix+"/main") {
		t.Errorf("AdvertiseConfig filters the alternate's refs to %q, a prefix a ref may begin with", prefix)
	}
	view, err := c.AdvertiseView(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Remove()
	filtered := advertise(t, w.pool, view.Dir, view.Alternate, got)
	if plain := advertise(t, w.pool, view.Dir, view.Alternate, nil); filtered != plain {
		t.Errorf("a view advertises, with the configuration:\n%s\nand without it:\n%s", filtered, plain)
	}

	tip, _ := w.push(t, repo, "refs/heads/dev", nil)
	w.sync(t, c, repo)
	if again := advertise(t, w.pool, view.Dir, view.Alternate, got); again != filtered {
		t.Errorf("a view advertised, once its incarnation moved:\n%s\nwant what it did:\n%s", again, filtered)
	}
	if plain := advertise(t, w.pool, view.Dir, view.Alternate, nil); !strings.Contains(plain, tip+" .have") {
		t.Fatalf("a view without the configuration does not list its alternate's newer tip %s:\n%s", tip, plain)
	}
}

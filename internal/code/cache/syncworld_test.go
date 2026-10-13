package cache_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/objstore/s3obj"
	"github.com/crewlet/crewlet/internal/objstore/s3obj/s3fake"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE REPOSITORIES A CASE SYNCS: real git history in a work tree per
// repository, each push ONE pack of its new objects stored as the bundle a
// receiving node's receive-pack would have stored (the pack, its .idx and its
// .rev), its row and its ref written to a fakeLog. A case then syncs a cache
// over that log and that store, and compares what it built against a cache
// built from nothing at the same position.

// syncWorld is a world whose cache reads a fakeLog and an object store.
type syncWorld struct {
	*world
	client  *gittest.Client
	log     *fakeLog
	backend objstore.Backend
	mem     *memobj.Backend // when the backend is the in-memory one
	s3      *s3fake.Server  // when it is the S3 fake
	store   *objstore.Store
	objects *countingObjects
	rec     *metrics.Recorder
	repos   map[string]*fixture

	mu sync.Mutex
	// bundles is every bundle pushed, by its pack's checksum.
	bundles map[string]gittest.Bundled
}

// fixture is one repository's work tree and what has been pushed of it.
type fixture struct {
	id   string
	work string
	// tips is every object a push or a tag made reachable, each excluded
	// from the next push's pack.
	tips []string
	// head is the newest commit.
	head string
	n    int
}

// The backends a sync world stores its packs in.
const (
	onMemory = "memory"
	onS3     = "s3"
)

// newSyncWorld is a sync world over a store of the given backend.
func newSyncWorld(t *testing.T, backend string) *syncWorld {
	t.Helper()
	w := &syncWorld{
		world: newWorld(t), client: gittest.NewClient(t), log: newFakeLog(), repos: map[string]*fixture{},
		bundles: map[string]gittest.Bundled{},
	}
	switch backend {
	case onMemory:
		w.mem = memobj.New()
		w.backend = w.mem
	case onS3:
		w.s3 = s3fake.Start(t, "files")
		b, err := s3obj.Open(t.Context(), s3obj.Config{
			Endpoint: w.s3.URL, Region: "us-east-1", Bucket: "files", Prefix: "acme/",
			PathStyle: true, AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "example-secret",
		})
		if err != nil {
			t.Fatalf("opening the S3 fake: %v", err)
		}
		w.backend = b
	default:
		t.Fatalf("no backend %q", backend)
	}
	store, err := objstore.NewStore(w.backend)
	if err != nil {
		t.Fatal(err)
	}
	w.store = store
	w.objects = &countingObjects{inner: store, opens: map[objstore.Key]int{}}
	if w.rec, err = metrics.New(); err != nil {
		t.Fatal(err)
	}
	return w
}

// wire points an Options value at the world's log, store and recorder.
func (w *syncWorld) wire(o *cache.Options) {
	o.Fetch, o.Objects, o.Metrics = w.log, w.objects, w.rec
}

// open boots a cache over the world — Prepare, a pool, Open — reading its log
// and its store; it is stopped when the case ends.
func (w *syncWorld) open(t *testing.T, mutate ...func(*cache.Options)) *cache.Cache {
	t.Helper()
	return w.world.open(t, append([]func(*cache.Options){w.wire}, mutate...)...)
}

// reopen is a restart: c stopped, and the same directory booted again.
func (w *syncWorld) reopen(t *testing.T, c *cache.Cache, mutate ...func(*cache.Options)) *cache.Cache {
	t.Helper()
	c.Stop(context.Background())
	return w.open(t, mutate...)
}

// repo makes a repository in the log with a work tree of its own; its id is
// the n'th of a fixed series, so a failing case names the same id each run.
func (w *syncWorld) repo(t *testing.T, n int) string {
	t.Helper()
	id := uuid.NewSHA1(uuid.NameSpaceOID, []byte("repo-"+strconv.Itoa(n))).String()
	w.log.create(id, "repo-"+strconv.Itoa(n))
	w.repos[id] = &fixture{id: id, work: w.client.Init(t)}
	return id
}

// push commits files on repo's work tree and stores the commit's new objects
// as ONE pack, written to the log with ref moved to the commit. It answers
// the commit and its row.
func (w *syncWorld) push(t *testing.T, repo, ref string, files map[string]string) (string, code.PackRow) {
	t.Helper()
	return w.pushAs(t, repo, ref, files, "")
}

// pushAs is push under the given pack id ("" mints one).
func (w *syncWorld) pushAs(t *testing.T, repo, ref string, files map[string]string, packID string) (string, code.PackRow) {
	t.Helper()
	oid, row := w.commit(t, repo, files, packID)
	row.Added = w.log.write(repo, []code.PackRow{row}, map[string]string{ref: oid})
	return oid, row
}

// commit is a push's commit and its stored pack, its row not yet written: a
// case writes it at the moment it chooses (w.log.write).
func (w *syncWorld) commit(t *testing.T, repo string, files map[string]string, packID string) (string, code.PackRow) {
	t.Helper()
	f := w.repos[repo]
	f.n++
	if files == nil {
		files = map[string]string{fmt.Sprintf("f%03d.txt", f.n): fmt.Sprintf("push %d of %s\n", f.n, repo)}
	}
	oid := w.client.Commit(t, f.work, files, fmt.Sprintf("push %d", f.n))
	row := w.pack(t, f, oid, packID)
	f.head = oid
	return oid, row
}

// pack packs the objects tip reaches that no earlier push did into a bundle
// in the store, and answers its row.
func (w *syncWorld) pack(t *testing.T, f *fixture, tip, packID string) code.PackRow {
	t.Helper()
	bare := gittest.Empty(t)
	revs := []string{tip}
	for _, old := range f.tips {
		revs = append(revs, "^"+old)
	}
	cs := gittest.Pack(t, w.client, bare, f.work, revs...)
	f.tips = append(f.tips, tip)
	b := gittest.Bundle(t, bare, cs)
	w.mu.Lock()
	w.bundles[cs] = b
	w.mu.Unlock()
	return w.storeBundle(t, b, packID)
}

// bundle is the bundle a push stored under checksum cs.
func (w *syncWorld) bundle(cs string) gittest.Bundled {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.bundles[cs]
}

// storeBundle stores a bundle's bytes and answers the row naming them.
func (w *syncWorld) storeBundle(t *testing.T, b gittest.Bundled, packID string) code.PackRow {
	t.Helper()
	obj, err := w.store.Put(t.Context(), bytes.NewReader(b.Bytes), int64(len(b.Bytes)),
		objstore.PutMeta{ContentType: code.BundleContentType})
	if err != nil {
		t.Fatalf("storing a bundle: %v", err)
	}
	if packID == "" {
		packID = uuid.Must(uuid.NewV7()).String()
	}
	return code.PackRow{
		PackID: packID, Part: "pack", Object: obj.Key, Hash: obj.Hash, Size: obj.Size,
		IndexBytes: b.IndexBytes, ReverseBytes: b.ReverseBytes, GitChecksum: b.Checksum, Objects: b.Objects,
		Kind: "push", State: "live",
	}
}

// tag tags repo's newest commit: annotated, a tag object pushed as its own
// pack; lightweight, a ref and no pack. It answers what the ref names.
func (w *syncWorld) tag(t *testing.T, repo, name string, annotated bool) string {
	t.Helper()
	f := w.repos[repo]
	ref := "refs/tags/" + name
	if !annotated {
		w.log.write(repo, nil, map[string]string{ref: f.head})
		return f.head
	}
	w.client.Run(t, f.work, "tag", "-a", name, "-m", "tag "+name, f.head)
	oid := strings.TrimSpace(w.client.Run(t, f.work, "rev-parse", ref))
	row := w.pack(t, f, oid, "")
	w.log.write(repo, []code.PackRow{row}, map[string]string{ref: oid})
	return oid
}

// sync is a linearizable sync that must succeed, released when the case ends.
func (w *syncWorld) sync(t *testing.T, c *cache.Cache, repo string) cache.Synced {
	t.Helper()
	s, err := c.Sync(t.Context(), repo, statelog.Freshness{Level: statelog.ReadLinearizable})
	if err != nil {
		t.Fatalf("sync of %s: %v", repo, err)
	}
	t.Cleanup(s.Release)
	return s
}

// dir is repo's current incarnation's directory in c.
func (w *syncWorld) dir(t *testing.T, c *cache.Cache, repo string) string {
	t.Helper()
	n, ok := cache.Repos(c)[repo]
	if !ok {
		t.Fatalf("the cache holds no %s", repo)
	}
	return filepath.Join(w.layout.Dir, "repos", repo+"."+strconv.Itoa(n)+".git")
}

// image is what an incarnation IS, as text a case compares: HEAD, config and
// packed-refs byte for byte, the names of its pack files, and every entry
// outside the image grammar. The maintainer's outputs — the multi-pack-index
// and the commit-graph — are left out: whether one is on disk is the
// maintainer's schedule, not the rows'.
func (w *syncWorld) image(t *testing.T, c *cache.Cache, repo string) string {
	t.Helper()
	return imageOf(t, w.dir(t, c, repo))
}

// rebuilt is the image of a FRESH cache — its own directory and pool —
// synced linearizably from nothing at the log's position now.
func (w *syncWorld) rebuilt(t *testing.T, repo string) string {
	t.Helper()
	layout, err := cache.Prepare(t.Context(), filepath.Join(t.TempDir(), "reference"))
	if err != nil {
		t.Fatal(err)
	}
	defer layout.Release()
	pool := newPool(t, layout)
	c, err := cache.Open(t.Context(), cache.Options{
		Layout: layout, MaxBytes: 1 << 40, Git: pool, Objects: w.store, Fetch: silent{w.log},
		Limits: code.DefaultLimits, HookCommand: []string{"/bin/true"}, Now: time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Stop(context.Background())
	s, err := c.Sync(t.Context(), repo, statelog.Freshness{Level: statelog.ReadLinearizable})
	if err != nil {
		t.Fatalf("a fresh cache's sync of %s: %v", repo, err)
	}
	defer s.Release()
	return imageOf(t, s.Dir)
}

// The image grammar, as the cases read it.
var (
	packFile   = regexp.MustCompile(`^pack-[0-9a-f]{40}\.(pack|idx|rev)$`)
	maintainer = regexp.MustCompile(`^multi-pack-index(-[0-9a-f]{40}\.(bitmap|rev))?$`)
)

func imageOf(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	for _, name := range []string{"HEAD", "config", "packed-refs"} {
		fmt.Fprintf(&b, "== %s\n%s", name, read(t, filepath.Join(dir, name)))
	}
	var packs, strays []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		parent, name := filepath.Dir(rel), d.Name()
		switch {
		case rel == ".":
		case parent == "." && (name == "refs" || name == "objects") && d.IsDir():
		case parent == "." && !d.IsDir() && slices.Contains([]string{"HEAD", "config", "packed-refs", "crewlet-state.json"}, name):
		case parent == "refs" && d.IsDir() && (name == "heads" || name == "tags"):
		case parent == "objects" && d.IsDir() && (name == "pack" || name == "info"):
		case rel == filepath.Join("objects", "info", "commit-graph"):
		case parent == filepath.Join("objects", "pack") && packFile.MatchString(name):
			packs = append(packs, name)
		case parent == filepath.Join("objects", "pack") && maintainer.MatchString(name):
		default:
			strays = append(strays, rel)
			if d.IsDir() {
				return fs.SkipDir
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	slices.Sort(packs)
	slices.Sort(strays)
	fmt.Fprintf(&b, "== packs\n%s\n== outside the image\n%s\n", strings.Join(packs, "\n"), strings.Join(strays, "\n"))
	return b.String()
}

// countingObjects is the store's read, counted: the opens of each object, in
// order, and the most open at once — and, when a case asks, held: every open
// until gate are open at once (or quiet passes), or one object's until the
// case lets it go.
type countingObjects struct {
	inner cache.Objects

	mu       sync.Mutex
	opens    map[objstore.Key]int
	order    []objstore.Key
	inFlight int
	most     int
	gateN    int
	gate     chan struct{}
	hold     func(objstore.Key) <-chan struct{}
}

func (o *countingObjects) Open(ctx context.Context, obj objstore.Object) (io.ReadCloser, error) {
	o.mu.Lock()
	o.opens[obj.Key]++
	o.order = append(o.order, obj.Key)
	o.inFlight++
	o.most = max(o.most, o.inFlight)
	gate, hold := o.gate, o.hold
	if gate != nil && o.inFlight >= o.gateN {
		close(gate)
		o.gate = nil
	}
	o.mu.Unlock()
	done := sync.OnceFunc(func() {
		o.mu.Lock()
		o.inFlight--
		o.mu.Unlock()
	})
	wait := func(ch <-chan struct{}) error {
		select {
		case <-ch:
			return nil
		case <-ctx.Done():
			done()
			return context.Cause(ctx)
		}
	}
	if gate != nil {
		if err := wait(gate); err != nil {
			return nil, err
		}
	}
	if hold != nil {
		if ch := hold(obj.Key); ch != nil {
			if err := wait(ch); err != nil {
				return nil, err
			}
		}
	}
	rc, err := o.inner.Open(ctx, obj)
	if err != nil {
		done()
		return nil, err
	}
	return &counted{ReadCloser: rc, done: done}, nil
}

// holdUntil holds every open until n are open at once, or quiet has passed.
func (o *countingObjects) holdUntil(n int, quiet time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	gate := make(chan struct{})
	o.gateN, o.gate = n, gate
	time.AfterFunc(quiet, func() {
		o.mu.Lock()
		defer o.mu.Unlock()
		if o.gate == gate {
			close(gate)
			o.gate = nil
		}
	})
}

// counts is the opens so far, the opening order and the most open at once.
func (o *countingObjects) counts() (map[objstore.Key]int, []objstore.Key, int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	opens := make(map[objstore.Key]int, len(o.opens))
	for k, n := range o.opens {
		opens[k] = n
	}
	return opens, slices.Clone(o.order), o.most
}

// total is how many opens there have been.
func (o *countingObjects) total() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.order)
}

// open is how many are open now.
func (o *countingObjects) open() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.inFlight
}

type counted struct {
	io.ReadCloser
	done func()
}

func (c *counted) Close() error {
	err := c.ReadCloser.Close()
	c.done()
	return err
}

// gauge is a gauge's value in rec.
func gauge(t testing.TB, rec *metrics.Recorder, name string) float64 {
	t.Helper()
	for _, s := range rec.Read() {
		if s.Name == name {
			return s.Value
		}
	}
	return 0
}

// counter is a counter's total in rec for one attribute value.
func counter(rec *metrics.Recorder, name, attr, value string) uint64 {
	var n uint64
	for _, s := range rec.Read() {
		if s.Name == name && s.Attrs[attr] == value {
			n += s.Total
		}
	}
	return n
}

// histogramCount is how many observations a histogram holds for one
// attribute value.
func histogramCount(rec *metrics.Recorder, name, attr, value string) uint64 {
	var n uint64
	for _, s := range rec.Read() {
		if s.Name == name && s.Attrs[attr] == value {
			n += s.Count
		}
	}
	return n
}

// packFiles is the pack files in an incarnation, sorted.
func packFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	for _, name := range names(t, filepath.Join(dir, "objects", "pack")) {
		if packFile.MatchString(name) {
			out = append(out, name)
		}
	}
	return out
}

// runGit runs one git process in dir through pool, as the host would.
func runGit(t *testing.T, pool *gitproc.Pool, dir string, stdin string, args ...string) (string, error) {
	t.Helper()
	cmd := gitproc.Cmd{Dir: dir, Args: args, Class: gitproc.ClassRead, Deadline: time.Minute}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	res, err := pool.Run(t.Context(), cmd)
	return string(res.Stdout), err
}

// tmpEmpty fails t when the cache's tmp/ holds anything.
func (w *syncWorld) tmpEmpty(t *testing.T) {
	t.Helper()
	if got := names(t, w.path("tmp")); len(got) != 0 {
		t.Errorf("tmp/ holds %v after the round, want nothing", got)
	}
}

// statOK reports whether path exists, failing t on any other error.
func statOK(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return true
	case os.IsNotExist(err):
		return false
	}
	t.Fatalf("stat %s: %v", path, err)
	return false
}

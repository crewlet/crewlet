package gitproc_test

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// THE VERSION LINE IS READ FOR ITS THREE NUMBERS, whatever a vendor or a build
// appends — and anything that is not git's own line is refused rather than
// read as some version.
func TestParseVersion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want gitproc.Version
	}{
		{"git version 2.43.0", gitproc.Version{Major: 2, Minor: 43}},
		{"git version 2.39.3 (Apple Git-145)", gitproc.Version{Major: 2, Minor: 39, Patch: 3}},
		{"git version 2.47.2\n", gitproc.Version{Major: 2, Minor: 47, Patch: 2}},
		{"git version 2.45.GIT", gitproc.Version{Major: 2, Minor: 45}},
		{"git version 2.48.0.rc1", gitproc.Version{Major: 2, Minor: 48}},
		{"git version 2.43.0.windows.1", gitproc.Version{Major: 2, Minor: 43}},
		{"hub version 2.14.2\ngit version 2.40.1", gitproc.Version{Major: 2, Minor: 40, Patch: 1}},
	} {
		got, err := gitproc.ParseVersion(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseVersion(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{"", "git version", "git version x.y", "version 2.43.0", "git version 99999999999999999999.1"} {
		if got, err := gitproc.ParseVersion(in); err == nil {
			t.Errorf("ParseVersion(%q) = %v, want an error", in, got)
		}
	}
}

// A GIT NODE REFUSES A GIT BELOW ITS FLOOR: New over a git one patch short of
// gitproc.NodeMinVersion answers an *ErrTooOld held to the NODE floor, naming
// the binary, what it is, the floor and its version — the client floor's 2.40
// is no longer enough for a pool — and the boundary itself builds a pool. A
// version git did not print is refused naming the text.
//
// Mutation: check ClientMinVersion in New, and 2.54.9 builds a pool — red.
func TestAGitNodeRefusesAGitBelowItsFloor(t *testing.T) {
	t.Parallel()
	old := gittest.Fake(t, "git version 2.54.9", "")
	_, err := gitproc.New(t.Context(), gitproc.Options{Path: old, Home: filepath.Join(t.TempDir(), "home")})
	var tooOld *gitproc.ErrTooOld
	if !errors.As(err, &tooOld) {
		t.Fatalf("New over git 2.54.9 = %v, want *ErrTooOld", err)
	}
	if tooOld.Path != old || tooOld.Found != (gitproc.Version{Major: 2, Minor: 54, Patch: 9}) ||
		tooOld.Floor != gitproc.FloorNode || tooOld.Want() != (gitproc.Version{Major: 2, Minor: 55}) {
		t.Fatalf("ErrTooOld = %+v (Want %v), want git 2.54.9 at %s held to the node floor, 2.55.0", tooOld, tooOld.Want(), old)
	}
	for _, want := range []string{old, "2.54.9", "2.55.0", "a git node", "gitproc.NodeMinVersion"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %q", err, want)
		}
	}

	newPool(t, gitproc.Options{Path: gittest.Fake(t, "git version 2.55.0", "")})

	_, err = gitproc.New(t.Context(), gitproc.Options{Path: gittest.Fake(t, "git version garbage", ""), Home: filepath.Join(t.TempDir(), "home")})
	if err == nil || !strings.Contains(err.Error(), "garbage") {
		t.Fatalf("New over a git printing garbage = %v, want an error naming the text", err)
	}
}

// AN ErrTooOld NAMES ITS FLOOR, in the words its reader acts on: a git node's
// operator is told the node floor and where a newer git comes from; a
// client's person the client floor and that Apple's git is too old for it.
// Neither names the engine image's own git version, which moves with the pin.
// A Floor nothing set is no floor, wants nothing, and says it is a call site's
// mistake rather than guess which floor was meant.
//
// Mutation: swap the two texts — red.
func TestErrTooOldNamesItsFloor(t *testing.T) {
	t.Parallel()
	found := gitproc.Version{Major: 2, Minor: 30}
	for _, tc := range []struct {
		floor gitproc.Floor
		min   gitproc.Version
		want  string
	}{
		{gitproc.FloorNode, gitproc.Version{Major: 2, Minor: 55},
			"gitproc: /usr/bin/git is git 2.30.0, and a git node needs git 2.55.0 or newer " +
				"(gitproc.NodeMinVersion, the node floor): install a newer git on this node's PATH — " +
				"the engine image builds the newest git release — or on macOS Homebrew's"},
		{gitproc.FloorClient, gitproc.Version{Major: 2, Minor: 40},
			"gitproc: /usr/bin/git is git 2.30.0, and a client of the code host needs git 2.40.0 or newer " +
				"(gitproc.ClientMinVersion, the client floor): install a newer git — on macOS, Homebrew's: " +
				"Apple's bundled git is 2.39"},
		{"", gitproc.Version{}, `gitproc: /usr/bin/git is git 2.30.0, held to no floor (ErrTooOld.Floor "")`},
		{"x", gitproc.Version{}, `gitproc: /usr/bin/git is git 2.30.0, held to no floor (ErrTooOld.Floor "x")`},
	} {
		e := &gitproc.ErrTooOld{Path: "/usr/bin/git", Found: found, Floor: tc.floor}
		if got := e.Error(); got != tc.want {
			t.Errorf("Floor %q: Error() = %q\nwant              %q", tc.floor, got, tc.want)
		}
		if e.Want() != tc.min || tc.floor.Min() != tc.min {
			t.Errorf("Floor %q: Want() %v, Min() %v; want %v", tc.floor, e.Want(), tc.floor.Min(), tc.min)
		}
		if valid := tc.min != (gitproc.Version{}); tc.floor.Valid() != valid {
			t.Errorf("Floor %q: Valid() = %v, want %v", tc.floor, tc.floor.Valid(), valid)
		}
	}
	if gitproc.FloorNode.Min() != gitproc.NodeMinVersion || gitproc.FloorClient.Min() != gitproc.ClientMinVersion {
		t.Fatalf("the floors' minimums are %v and %v, not NodeMinVersion %v and ClientMinVersion %v",
			gitproc.FloorNode.Min(), gitproc.FloorClient.Min(), gitproc.NodeMinVersion, gitproc.ClientMinVersion)
	}
}

// THE PINS MEET THE FLOORS. scripts/git-pin.env names the gits the engine is
// built and tested with — exactly six variables, a three-number release and a
// tarball's SHA-256 for each of the three pins — and the node pin, which every
// pool and client of the suite runs and the engine image builds, is at or
// above the node floor; the node-floor pin is of NodeMinVersion's own series
// and at or above it, and the client-floor pin of ClientMinVersion's, so the
// floors' arm runs the floors and not two newer gits under their names.
//
// Mutation: raise NodeMinVersion to 2.57.0, and the node pin is below it.
func TestThePinsMeetTheFloors(t *testing.T) {
	t.Parallel()
	pins := readPins(t, filepath.Join(sourcetree.Root(t), "scripts", "git-pin.env"))
	names := []string{"GIT_NODE_VERSION", "GIT_NODE_SHA256", "GIT_NODE_FLOOR_VERSION", "GIT_NODE_FLOOR_SHA256",
		"GIT_CLIENT_FLOOR_VERSION", "GIT_CLIENT_FLOOR_SHA256"}
	if got := slices.Sorted(maps.Keys(pins)); !slices.Equal(got, slices.Sorted(slices.Values(names))) {
		t.Fatalf("scripts/git-pin.env sets %q, want exactly %q", got, names)
	}
	release := regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	digest := regexp.MustCompile(`^[0-9a-f]{64}$`)
	version := func(name string) gitproc.Version {
		t.Helper()
		if !release.MatchString(pins[name]) {
			t.Fatalf("%s=%q is not a three-number release", name, pins[name])
		}
		v, err := gitproc.ParseVersion("git version " + pins[name])
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, name := range names {
		if strings.HasSuffix(name, "_SHA256") && !digest.MatchString(pins[name]) {
			t.Errorf("%s=%q is not 64 lowercase hex digits", name, pins[name])
		}
	}
	inSeries := func(v, floor gitproc.Version) bool {
		return v.Major == floor.Major && v.Minor == floor.Minor && v.AtLeast(floor)
	}
	if node := version("GIT_NODE_VERSION"); !node.AtLeast(gitproc.NodeMinVersion) {
		t.Errorf("the node pin, %s, is below the node floor, %s (gitproc.NodeMinVersion)", node, gitproc.NodeMinVersion)
	}
	if floor := version("GIT_NODE_FLOOR_VERSION"); !inSeries(floor, gitproc.NodeMinVersion) {
		t.Errorf("the node-floor pin, %s, is not of the node floor's series at or above %s", floor, gitproc.NodeMinVersion)
	}
	if floor := version("GIT_CLIENT_FLOOR_VERSION"); !inSeries(floor, gitproc.ClientMinVersion) {
		t.Errorf("the client-floor pin, %s, is not of the client floor's series at or above %s", floor, gitproc.ClientMinVersion)
	}
}

// readPins is the NAME=value lines of a pin file, its comments and blank
// lines skipped; a line that is neither, or a name set twice, fails t.
func readPins(t *testing.T, path string) map[string]string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pins := map[string]string{}
	for line := range strings.Lines(string(body)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("%s: %q is neither a comment nor NAME=value", path, line)
		}
		if _, dup := pins[name]; dup {
			t.Fatalf("%s sets %s twice", path, name)
		}
		pins[name] = value
	}
	return pins
}

// THE NODE FLOOR WRITES A LAYER OVER A NAMED BASE — the floor's stated reason,
// held where the floor lands, on the floor's own build (gittest.NodeFloor):
// over two packs, a bitmapped multi-pack-index layer written incrementally
// with no chain file and no base, then a second over the first BY ITS
// CHECKSUM, the chain written by hand (git resolves a named base through the
// chain on disk, so the first layer's chain is written before the second is
// asked for); the whole chain verifies, and a walk over every object loads
// its bitmap. That is how a repository's maintainer publishes the layers
// every git node installs. If a later part's commands fail on the floor, the
// FLOOR moves to the oldest release they pass on; the commands never bend to
// the floor.
//
// Mutation: drop --no-write-chain-file from the second write, and git
// refuses --base — red.
func TestTheNodeFloorWritesALayerOverANamedBase(t *testing.T) {
	t.Parallel()
	c := gittest.NewClientWith(t, gittest.NodeFloor(t))
	wt := c.Init(t)
	one := c.Commit(t, wt, map[string]string{"a.txt": "a\n"}, "one")
	two := c.Commit(t, wt, map[string]string{"b.txt": "b\n"}, "two")
	bare := gittest.Empty(t)
	first := gittest.Pack(t, c, bare, wt, one)
	second := gittest.Pack(t, c, bare, wt, two, "^"+one)
	gittest.SetRefs(t, bare, map[string]string{"refs/heads/main": two})

	chain := filepath.Join(bare, "objects", "pack", "multi-pack-index.d", "multi-pack-index-chain")
	layer := func(pack, base string) string {
		t.Helper()
		out, stderr, err := c.Try(t, bare, strings.NewReader("pack-"+pack+".idx\n"), "multi-pack-index", "write",
			"--incremental", "--bitmap", "--stdin-packs", "--no-write-chain-file", "--base="+base)
		if err != nil {
			t.Fatalf("git %s wrote no layer over --base=%s: %v\n%s", gitproc.NodeMinVersion, base, err, stderr)
		}
		checksum := strings.TrimSpace(out)
		if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(checksum) {
			t.Fatalf("the layer's write printed %q, not its checksum", out)
		}
		return checksum
	}
	writeChain := func(layers ...string) {
		t.Helper()
		if err := os.WriteFile(chain, []byte(strings.Join(layers, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	base := layer(first, "none")
	writeChain(base)
	tip := layer(second, base)
	writeChain(base, tip)

	if _, stderr, err := c.Try(t, bare, nil, "multi-pack-index", "verify"); err != nil {
		t.Fatalf("the chain %s, %s does not verify: %v\n%s", base, tip, err, stderr)
	}
	out, stderr, err := c.Try(t, bare, nil, "rev-list", "--test-bitmap", two)
	if err != nil || !strings.Contains(out+stderr, "OK!") {
		t.Fatalf("the tip layer's bitmap does not load and hold every object of %s: %v\n%s%s", two, err, out, stderr)
	}
	if count := strings.TrimSpace(c.Run(t, bare, "rev-list", "--objects", "--use-bitmap-index", "--count", "--all")); count != "6" {
		t.Fatalf("a walk through the bitmaps counts %s objects, want the 6 the two commits hold", count)
	}
}

// EVERY CLAIM ABOUT WHAT GIT DOES IN A POOL RUNS ON BOTH ENDS OF WHAT A POOL
// ADMITS. onNodeGits — which the format table, the request replay, the
// environment's cases and the trace's process tree are held through — runs
// its body on a pool over the node pin (the PATH's git) and on one over the
// node floor's own build (in gitproc.NodeMinVersion's series), so those arms
// cannot quietly shrink to the one git a make target puts on PATH, which is
// how the floor's expectations were wrong in three arms and nothing ran them.
// Where the floor's build is named (every make target names it), its arm must
// have run.
//
// Mutation: run onNodeGits's body on the pin alone, and this is red.
func TestEveryNodeGitClaimRunsOnBothEnds(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	ran := map[string]gitproc.Version{}
	t.Run("arms", func(t *testing.T) {
		onNodeGits(t, func(t *testing.T, git string) {
			v := poolOn(t, git, gitproc.Options{}).Version()
			mu.Lock()
			defer mu.Unlock()
			ran[path.Base(t.Name())] = v
		})
	})
	if pin := realPool(t, gitproc.Options{}).Version(); ran["node_pin"] != pin {
		t.Errorf("the node pin's arm ran git %s, want the PATH's %s (arms run: %v)", ran["node_pin"], pin, ran)
	}
	floor, ok := ran["node_floor"]
	switch {
	case !ok && os.Getenv("CREWLET_TEST_GIT_NODE_FLOOR") != "":
		t.Errorf("the node floor's build is named, and no arm ran it (arms run: %v)", ran)
	case ok && (floor.Major != gitproc.NodeMinVersion.Major || floor.Minor != gitproc.NodeMinVersion.Minor):
		t.Errorf("the node floor's arm ran git %s, not of the node floor's series %s", floor, gitproc.NodeMinVersion)
	}
}

// EVERY GIT A NODE MAY RUN SENDS A CACHE'S OVERLAPPING PACKS WHOLE under
// pack.allowPackReuse=multi, the value every cache serves with
// (cache.ServeConfig): every object once, and every delta with its base,
// when one object sits in many packs — which is a cache's every repository:
// each push is a pack of its own, and receive-pack completes a thin push with
// the bases it borrowed from the packs before it. Reuse reads the
// multi-pack-index bitmap over all of them, as a warm cache's clone does,
// the index and its bitmap written by the node's own git, as its maintainer
// writes them. git 2.44 to 2.47 accept the value and, reusing from several
// such packs, sent an object twice or a delta whose base they never sent, and
// the client's index-pack refused the whole pack ("REF_DELTA … already
// resolved", "pack has N unresolved deltas" — measured on 2.47.3, Debian
// trixie's git, which fails this case at forty pushes); 2.48.0 fixed it
// (tb/multi-pack-reuse-dupfix), and 2.43 refuses the value outright ("bad
// boolean config value 'multi'"). So nothing chooses the value per binary
// any more: it is safe because gitproc.NodeMinVersion is past 2.48, and this
// case runs on both ends of what a pool admits — the node pin and the node
// floor's own build (onNodeGits) — since the claim is about every git at the
// floor, not the one a make target puts first on PATH.
//
// Mutation: lower gitproc.NodeMinVersion to 2.47.0 and name a 2.47.3 build
// as CREWLET_TEST_GIT_NODE_FLOOR, and the floor's arm is red.
func TestMultiPackReuseSendsOverlappingPacksWhole(t *testing.T) {
	t.Parallel()
	c := gittest.NewClient(t)
	src := c.Init(t)
	const pushes, files = 40, 50
	gittest.FastImport(t, c, src, func(w *bufio.Writer) error {
		data := rand.NewChaCha8([32]byte{'r', 'e', 'u', 's', 'e'})
		for i := range pushes {
			fmt.Fprintf(w, "commit refs/heads/main\ncommitter Crewlet Test <test@example.com> %d +0000\ndata 0\n",
				1_767_225_600+i)
			for j := range files {
				// FILE f IN DIRECTORY f MOD twenty: every commit changes
				// every directory's tree, so each push's trees are deltas
				// against trees an earlier pack holds.
				f := i*files + j
				fmt.Fprintf(w, "M 100644 inline d%02d/f%05d\ndata 64\n", f%20, f)
				if _, err := io.CopyN(w, data, 64); err != nil {
					return err
				}
				if err := w.WriteByte('\n'); err != nil {
					return err
				}
			}
		}
		return nil
	})
	packs := gittest.Empty(t)
	for _, commit := range strings.Fields(c.Run(t, src, "rev-list", "--reverse", "main")) {
		// A PACK A PUSH, as the cache keeps them: never unpacked into loose
		// objects, never repacked. Named on receive-pack's own command line,
		// because a local push clears GIT_CONFIG_PARAMETERS for the process
		// it runs in another repository, and the repository's config is the
		// engine's.
		c.Run(t, src, "push", "--quiet",
			"--receive-pack=git -c transfer.unpackLimit=1 -c receive.autogc=false receive-pack",
			packs, commit+":refs/heads/main")
	}

	onNodeGits(t, func(t *testing.T, git string) {
		p := poolOn(t, git, gitproc.Options{})
		// The node's own copy of the packs, and its maintainer's index and
		// bitmap over them, written by the git that then serves them.
		bare := filepath.Join(t.TempDir(), "repo.git")
		if err := os.CopyFS(bare, os.DirFS(packs)); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Run(t.Context(), gitproc.Cmd{
			Dir: bare, Args: []string{"multi-pack-index", "write", "--bitmap", "--no-progress"},
			Config: []gitproc.KV{{Key: "pack.writeBitmapLookupTable", Value: "true"}},
			Class:  gitproc.ClassMaintenance, Deadline: gitproc.ReadDeadline,
		}); err != nil {
			t.Fatalf("git %s wrote no multi-pack-index bitmap over the %d packs: %v", p.Version(), pushes, err)
		}
		var pack bytes.Buffer
		// THE CLONE'S PACK AS upload-pack ASKS FOR IT.
		if _, err := p.Run(t.Context(), gitproc.Cmd{
			Dir:  bare,
			Args: []string{"pack-objects", "--revs", "--thin", "--stdout", "--delta-base-offset", "-q"},
			Config: []gitproc.KV{{Key: "core.multiPackIndex", Value: "true"}, {Key: "pack.useBitmaps", Value: "true"},
				{Key: "pack.allowPackReuse", Value: "multi"}},
			Stdin: strings.NewReader("refs/heads/main\n"), Stdout: &pack,
			Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline,
		}); err != nil {
			t.Fatalf("git %s: pack-objects of every object with pack.allowPackReuse=multi: %v", p.Version(), err)
		}
		clone := c.Init(t)
		if _, stderr, err := c.Try(t, clone, &pack, "index-pack", "--stdin"); err != nil {
			t.Fatalf("git %s with pack.allowPackReuse=multi sent a pack the client refuses: %v\n%s",
				p.Version(), err, stderr)
		}
	})
}

func TestAPathThatIsNotAbsoluteIsRefused(t *testing.T) {
	t.Parallel()
	_, err := gitproc.New(t.Context(), gitproc.Options{Path: "git", Home: filepath.Join(t.TempDir(), "home")})
	if err == nil || !strings.Contains(err.Error(), `"git"`) {
		t.Fatalf("New with a relative path = %v, want a refusal naming it", err)
	}
}

// THE LANES ARE SIZED HERE AND NOWHERE ELSE: 0 resolves to the cores (and four
// transfers a core, no more than one for every TransferShare of the node's
// memory), and a negative size names the setting an operator changes.
//
// The memory bound is what a container limited in memory and not in CPU
// needs: it sees its host's every core, and on 64 of them with 16 GiB four a
// core admitted 256 transfers that could hold 56 GiB — it runs 32, as the
// eight cores its memory backs would. A node of CoreMemory a core or more is
// sized by its cores as it always was, a node too small for one transfer
// still runs one, and an operator's own count is taken as given.
//
// Mutation: size the default by the cores alone, and the 64-core row fails.
func TestTheLaneSizesAreResolvedHere(t *testing.T) {
	t.Parallel()
	cores := runtime.GOMAXPROCS(0)
	const plenty = 1 << 40
	for _, tc := range []struct {
		o                      gitproc.Options
		read, maint, transfers int
	}{
		{gitproc.Options{Memory: plenty}, cores, (cores + 1) / 2, gitproc.TransfersPerProcess * cores},
		{gitproc.Options{MaxProcesses: 3, Memory: plenty}, 3, 2, 12},
		{gitproc.Options{MaxProcesses: 1, Memory: plenty}, 1, 1, 4},
		{gitproc.Options{MaxProcesses: 3, MaxTransfers: 5, Memory: plenty}, 3, 2, 5},
		{gitproc.Options{MaxProcesses: 4, Memory: 4 * gitproc.CoreMemory}, 4, 2, 16},
		{gitproc.Options{MaxProcesses: 64, Memory: 16 << 30}, 64, 32, 32},
		{gitproc.Options{MaxProcesses: 4, Memory: 1 << 30}, 4, 2, 2},
		{gitproc.Options{MaxProcesses: 4, Memory: 100 << 20}, 4, 2, 1},
		{gitproc.Options{MaxProcesses: 64, MaxTransfers: 300, Memory: 16 << 30}, 64, 32, 300},
	} {
		p := fakePool(t, "exit 0", tc.o)
		if r, m, x := p.Slots(gitproc.ClassRead), p.Slots(gitproc.ClassMaintenance), p.Slots(gitproc.ClassTransfer); r != tc.read || m != tc.maint || x != tc.transfers {
			t.Errorf("%+v: read %d, maintenance %d, transfer %d; want %d, %d, %d", tc.o, r, m, x, tc.read, tc.maint, tc.transfers)
		}
		if got := p.Slots("bogus"); got != 0 {
			t.Errorf("Slots(bogus) = %d, want 0", got)
		}
	}
	fake := gittest.Fake(t, fakeVersion, "exit 0")
	for _, tc := range []struct {
		o    gitproc.Options
		name string
	}{
		{gitproc.Options{MaxProcesses: -1}, "git.max_processes"},
		{gitproc.Options{MaxTransfers: -1}, "git.max_transfers"},
		{gitproc.Options{Memory: -1}, "Memory"},
	} {
		tc.o.Path, tc.o.Home = fake, filepath.Join(t.TempDir(), "home")
		if _, err := gitproc.New(t.Context(), tc.o); err == nil || !strings.Contains(err.Error(), tc.name) {
			t.Errorf("New(%+v) = %v, want a refusal naming %s", tc.o, err, tc.name)
		}
	}
}

// A GIT WRITING NO TRACE IS REFUSED. The code host classifies a damaged cache
// from the trace2 event stream alone, so a git built or patched without
// trace2 — one that answers `version` and writes nothing on the descriptor
// the pool gave it — would read every damage as an unclassified failure and
// never repair one. New refuses it, naming the path; the same version that
// writes its stream is a pool. A git BELOW THE FLOOR is refused for its
// version whatever stream it writes: one older than 2.22 writes no trace2 at
// all, and "built or patched without it" would send its operator after a
// fault it does not have, where the floor's refusal names the version found
// and the one wanted.
//
// Mutations: drop New's check, and the first arm is a pool — red; check the
// stream before the version, and the last arm is ErrNoTrace — red.
func TestAGitWritingNoTraceIsRefused(t *testing.T) {
	t.Parallel()
	silent := gittest.FakeWithoutTrace(t, fakeVersion, "")
	_, err := gitproc.New(t.Context(), gitproc.Options{Path: silent, Home: filepath.Join(t.TempDir(), "home")})
	if !errors.Is(err, gitproc.ErrNoTrace) || !strings.Contains(err.Error(), silent) {
		t.Fatalf("New over a git writing no trace = %v, want ErrNoTrace naming %s", err, silent)
	}
	newPool(t, gitproc.Options{Path: gittest.Fake(t, fakeVersion, "")})

	ancient := gittest.FakeWithoutTrace(t, "git version 2.20.1", "")
	_, err = gitproc.New(t.Context(), gitproc.Options{Path: ancient, Home: filepath.Join(t.TempDir(), "home")})
	var tooOld *gitproc.ErrTooOld
	if !errors.As(err, &tooOld) || errors.Is(err, gitproc.ErrNoTrace) {
		t.Fatalf("New over git 2.20.1, which writes no trace = %v, want *ErrTooOld and not ErrNoTrace", err)
	}
	if tooOld.Found != (gitproc.Version{Major: 2, Minor: 20, Patch: 1}) || tooOld.Floor != gitproc.FloorNode ||
		tooOld.Want() != gitproc.NodeMinVersion {
		t.Fatalf("ErrTooOld = %+v, want 2.20.1 found and the node floor wanted", tooOld)
	}
}

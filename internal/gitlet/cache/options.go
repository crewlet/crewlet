package cache

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitletfault"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// Fetcher is the rows' side of a cache. RepoState pages a repository's refs
// and packs at a position, a delta's with the pushes it covers, which a round
// folds into its index of pushes before it answers. Read pages its pushes in
// log order (gitlet.ReadIntroduced), for the backfill (backfill.go).
// estate.CodeSeam satisfies it; a single-node harness wires the in-process
// reader.
type Fetcher interface {
	RepoState(ctx context.Context, q gitlet.StateQuery) (gitlet.RepoSnapshot, error)
	Read(ctx context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error)
}

// Fleet is how many live nodes serve git and hold the estate, as this node's
// placements see them now, or why it cannot say: the backfill paces its reads
// by it (BackfillIdle). A node with no fleet answers zero of each, which the
// pace reads as one.
type Fleet func() (gitletNodes, dataNodes int, err error)

// Placement is where a `search: indexed` repository's code index is kept
// (M6-D12): the first gitlet.IndexHolders of Order, its rendezvous order over
// the live Gitlet nodes (estate.Router.GitOrder), keep one; Node is this
// node's id in that order. An Order that fails decides nothing: the indexer
// keeps what it holds and asks again next look.
type Placement struct {
	Node  string
	Order func(repoID string) ([]string, error)
}

// Objects is the object store's read of one pack bundle, verified against
// the row that names it: *objstore.Store satisfies it.
type Objects interface {
	Open(ctx context.Context, o objstore.Object) (io.ReadCloser, error)
}

// Options is how a [Cache] is opened. Every field but Faults, Logger and
// Metrics is required, and [Open] refuses a missing one by name
// ([ErrOptions]) before it touches the directory.
type Options struct {
	// Layout is what [Prepare] returned, unchanged: Open takes over its claim
	// on the directory and its reading of repos/, so a Layout opens once.
	Layout Layout

	// MaxBytes is the budget the cache is held to: Tier A
	// gitlet.cache_max_bytes, resolved by the engine. Above zero.
	MaxBytes int64

	// Git runs every git process in a cache, built over Layout.Home.
	Git *gitproc.Pool

	// Objects reads pack bundles from the object store.
	Objects Objects

	// Fetch reads a repository's rows.
	Fetch Fetcher

	// Fleet paces the backfill's reads, read again after every page.
	Fleet Fleet

	// Placement is where this node keeps code indexes: nil (a harness with no
	// fleet) keeps none; the engine sets it on every Gitlet node.
	Placement *Placement

	// Limits is the current epoch's code.limits.
	Limits func() gitlet.Limits

	// HookCommand is the argv hooks/proc-receive execs for every push: arg0
	// absolute, no argument holding a NUL (no shell word can). The engine
	// passes [<crewlet>, code, hook, proc-receive].
	HookCommand []string

	// Faults is nil in production (internal/gitlet/gitletfault).
	Faults gitletfault.Injector

	// Logger nil is the package's own (component gitlet.cache), never a
	// discarding one: a missing warning reads as a quiet node.
	Logger *slog.Logger

	// Metrics nil records nothing: a deployment without a recorder is legal.
	Metrics *metrics.Recorder

	// Now is the cache's clock: when a repository was last used.
	Now func() time.Time

	// knobs is the suite's reach into steps no option exposes
	// (export_test.go); nil outside a test.
	knobs *knobs
}

// knobs are the test suite's hooks into the cache's own steps, each a moment
// a case must hold still to be deterministic rather than lucky. A knob that
// replaces a value or a clock does so only when set.
type knobs struct {
	// hookStaged runs while the hook's staged file is open for writing, the
	// window in which a fork makes it unexecutable.
	hookStaged func(f *os.File)
	// removing runs before the janitor removes each entry of trash/.
	removing func(path string)
	// measuring runs as a janitor's measure is about to list dir (scratch/,
	// each directory under it, and each memo cohort of a copy no state
	// records): a case removes or replaces it, or retires its copy, as the
	// host or a purge would from under a measure that listed it.
	measuring func(dir string)
	// statingMemo is told each memo-cohort entry a measure is about to stat,
	// after the listing named it: a case removes it, replaces the cohort with a
	// file, or answers an error (a failing disk), which is the stat's. A change
	// to the disk reaches the stat only where ReadDir reports entry types
	// rather than stating as it lists, as every filesystem the suite uses does.
	statingMemo func(path string) error
	// trimming runs as the memo's trim is about to move the cohort at dir out
	// of a copy: a case retires the copy there, as a purge would.
	trimming func(dir string)
	// ticks replaces the janitor's EvictInterval ticker: a case ticks when it
	// chooses, or never, so no tick passes for a kick.
	ticks <-chan time.Time
	// peeling runs as a round's peel is about to read the incarnation at
	// dir.
	peeling func(dir string)
	// tagging runs as a round's tag step is about to read objects through git
	// in the incarnation at dir; its error fails the step.
	tagging func(ctx context.Context, dir string, reading int) error
	// roundEnding runs once a round over repo has converged, its write lock
	// given back but the round still the repository's: a case holds it there.
	roundEnding func(repo string)
	// swept runs once the sweep's first half has made the incarnation at dir
	// its image, before the pool's shape check: a case writes into it there,
	// as something outside the engine would.
	swept func(dir string)
	// maintaining runs before each step of a maintenance run, with the run's
	// own context: a case holds a run in flight by blocking here.
	maintaining func(ctx context.Context, step string)
	// maintTicks replaces the maintainer's MidxDebounce ticker.
	maintTicks <-chan time.Time
	indexTicks <-chan time.Time
	// indexReading is told how many blobs a code-index build reads through
	// its cat-file, before it starts it.
	indexReading func(blobs int)
	// indexSegmentBytes, above zero, replaces codeindex.SegmentSourceBytes, so
	// a case builds many segments from a few small files.
	indexSegmentBytes int64
	// indexMapped is told each segment a code-index build writes and the
	// bytes it reads it back through: a case holds them to a mapping of it.
	indexMapped func(path string, data []byte)
	// indexTipReading is told each copy's directory the indexer reads a
	// default branch's tip in, before it starts git.
	indexTipReading func(dir string)
	// indexPlaced is told the node's mappings each time a build has placed a
	// segment and merged: a case holds a build to the segments it still reads.
	indexPlaced func(mapped int)
	// installWake replaces the installer's timer for its earliest wait's end,
	// at on Options.Now's clock. It fires once that clock reaches at, and at
	// once if it already has, since a pass may arm after a case moved the
	// clock. A case wakes it by moving its clock.
	installWake func(at time.Time) <-chan time.Time
	// installSettling runs as an installer's task has tried repository id and
	// is about to settle it: a case lands a kick of the repository there.
	installSettling func(id string)
	// installReading is told each time the installer takes (held) or gives
	// back a share of the node's read lane, its canaries' where canary says
	// so, else its checks': a case counts how many of each it holds at once.
	installReading func(canary, held bool)
	// chainLines replaces the ids a written multi-pack-index chain file
	// lists: a case writes a chain over the wrong base for the canary (M16).
	chainLines func(ids []string) []string
	// canaryRunning runs as a placement's canary is about to read the chain at
	// dir (a swap's staged directory, or the incarnation's own), and the func
	// it returns once the canary's git ends: a case holds every read-lane slot
	// between the two, as requests filling it past SlotWait do.
	canaryRunning func(dir string) func()
	// verifyWaits runs as Verify is about to wait for the repository's
	// rounds before listing the incarnation at dir.
	verifyWaits func(dir string)
	// verifyListing runs while Verify lists the incarnation at dir, rounds
	// held off and no maintenance run let start.
	verifyListing func(dir string)
	// verifyFsck runs as Verify's fsck is about to ask for its maintenance
	// slot in the incarnation at dir.
	verifyFsck func(dir string)
	// checkBudget replaces CheckBudget's bound of a check pass over a copy of
	// bytes.
	checkBudget func(bytes int64) time.Duration
	// adopting runs while an adoption holds its share of the repository's
	// directory, before it links: a case holds adoptions there together.
	adopting func(repo string)
	// adoptSync replaces the directory sync that makes an adoption's links
	// durable: a case makes it fail.
	adoptSync func(dir string) error
	// roundPacks, above zero, replaces MidxRoundPacks (the packs a full round
	// leaves before it writes its own index), so a handful of pushes reach it.
	roundPacks int
	// midxPacks, above zero, replaces MidxPacks (the packs beside the
	// maintainer's index that make a repository due again), so a handful of
	// pushes reach a run over the maintainer's own index.
	midxPacks int
	// fileSync replaces (*os.File).Sync for a downloaded file: a case watches
	// which syncs overlap, and what is in objects/pack meanwhile.
	fileSync func(f *os.File) error
	// staged runs once a round has made a bundle's three files, as it hands
	// the pack to its worker: a case reads the order the round made them in.
	staged func(object objstore.Key)
	// stagedWrite answers every write into a bundle's staged files during
	// its download (nil writes): a case fails this node's disk under one.
	stagedWrite func(object objstore.Key) error
	// asking runs as a round's worker is about to ask for its GET, its
	// bundle's files made: a case holds one pack there while others ask.
	asking func(object objstore.Key)
	// roundIndexed is told the incarnation's directory each time a full round
	// writes its own multi-pack-index there: interleavings must reach it.
	roundIndexed func(dir string)
	// hashing runs before a check hashes the file at path: a case counts
	// what a round hashes, or swaps the file under the hash.
	hashing func(path string)
	// checkPiece replaces CheckPieceBytes, so a small file is several pieces;
	// pieceRead is told each piece's path once read, its slot still held, and
	// again once the slot is given back.
	checkPiece int64
	pieceRead  func(path string, held bool)
	// readFault is asked before each piece a hash reads, its error that
	// read's: a disk failing under a file.
	readFault func(path string) error
	// scrubWake replaces the scrubber's timer for the next interval's end,
	// next on the cache's clock, firing as installWake does: a case sees when
	// the scrubber means to wake, and wakes it by moving the clock there.
	scrubWake func(next time.Time) <-chan time.Time
	// derived replaces DerivedFiles(): a case proves the code reads the
	// declarations by opening a cache over changed ones.
	derived []Derived
	// stating is told the incarnation-relative path of every entry the cache
	// stats to count its bytes (image.sizes): a case counts what the end of a
	// maintenance run reads.
	stating func(rel string)
	// decodes counts every move a view of the index of pushes decodes
	// (MoveView.At): a case holds a landed-in search to the moves it probes.
	decodes *atomic.Int64
	// backfillWake replaces the backfill's timer of d (after a page, a
	// failure, or until the next horizon): a case sees the wait and wakes it.
	backfillWake func(d time.Duration) <-chan time.Time
	// folded is told what each round's fold of its delta's pushes took
	// (foldPushes): a benchmark weighs the fold against its round.
	folded func(took time.Duration)
	// mappedRuns, above zero, replaces the node's bound on mapped runs (the
	// process map count's share, runmaps.go), so a handful of copies reach it.
	mappedRuns int
	// stateWrite is asked before every write of a repository's state (round:
	// a round writes it), its error that write's: a case fails the backfill's
	// or the maintainer's write, which reach no fault point.
	stateWrite func(repo string, round bool) error
	// merged is told how many runs each stored merge of an index of pushes
	// merged (mergeSpan): a benchmark counts the merges a push costs.
	merged func(runs int)
	// link replaces os.Link for every hard link a swap makes (Cache.link): a
	// case fails one, as a filesystem out of links or room would.
	link func(oldname, newname string) error
	// syncing is told every directory a swap or a state's write is about to
	// sync (Cache.durableDir): a case counts what a swap's durability costs.
	syncing func(dir string)
	// dropping and dropped are told each derived file a drop moves into
	// trash/ (dropFor), incarnation-relative: dropping once moved and before
	// the incarnation's record changes, dropped after, the entry's lock let go
	// between them. A case holds a drop on either side of the change.
	dropping, dropped func(rel string)
}

// validate refuses an Options value naming the field and what to set,
// before anything is touched.
func (o Options) validate() error {
	refuse := func(format string, args ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrOptions}, args...)...)
	}
	switch {
	case o.Layout.boot == nil && o.Layout.Dir == "":
		return refuse("Layout is empty: open the Layout cache.Prepare returned")
	case o.Layout.boot == nil:
		return fmt.Errorf("%w: a Layout naming %s that cache.Prepare did not return — "+
			"prepare the directory, then open the Layout Prepare returned", ErrNotACache, o.Layout.Dir)
	case o.Layout.Dir != o.Layout.boot.dir:
		return refuse("Layout.Dir %q is not the directory cache.Prepare claimed (%s): open the Layout as Prepare returned it",
			o.Layout.Dir, o.Layout.boot.dir)
	case o.MaxBytes <= 0:
		return refuse("MaxBytes %d is not a budget: gitlet.cache_max_bytes is resolved by the engine and is above zero", o.MaxBytes)
	case o.Git == nil:
		return refuse("Git is nil: a cache runs its git through a gitproc.Pool built over Layout.Home")
	case o.Objects == nil:
		return refuse("Objects is nil: a cache reads its packs from the object store")
	case o.Fetch == nil:
		return refuse("Fetch is nil: a cache reads its repositories' rows through the code seam")
	case o.Fleet == nil:
		return refuse("Fleet is nil: the backfill paces its reads of the estate by the fleet's live git and " +
			"data nodes")
	case o.Placement != nil && (o.Placement.Node == "" || o.Placement.Order == nil):
		return refuse("Placement names no node or no order: a node keeping code indexes knows its own id " +
			"and its repositories' git order (estate.Router.GitOrder); leave Placement nil to keep none")
	case o.Limits == nil:
		return refuse("Limits is nil: a cache serves under the current epoch's code.limits")
	case o.Now == nil:
		return refuse("Now is nil: a cache needs a clock")
	case len(o.HookCommand) == 0:
		return refuse("HookCommand is empty: hooks/proc-receive execs it for every push")
	case !filepath.IsAbs(o.HookCommand[0]):
		return refuse("HookCommand[0] %q is not an absolute path: git runs the hook with the pool's PATH, not the engine's", o.HookCommand[0])
	}
	for i, arg := range o.HookCommand {
		if strings.ContainsRune(arg, 0) {
			return refuse("HookCommand[%d] holds a NUL, which no shell word can carry", i)
		}
	}
	return nil
}

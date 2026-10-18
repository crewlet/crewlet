package cache

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// Fetcher is the rows' side of a cache: one page of a repository's snapshot,
// its refs and packs at a position. estate.CodeSeam satisfies it on every
// node; a single-node harness wires the in-process reader.
type Fetcher interface {
	RepoState(ctx context.Context, q code.StateQuery) (code.RepoSnapshot, error)
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
	// Layout is what [Prepare] returned, as it returned it: Open takes over
	// its claim on the directory and its reading of repos/, so a Layout
	// opens once.
	Layout Layout

	// MaxBytes is the budget the cache is held to: Tier A
	// git.cache_max_bytes, resolved by the engine. Above zero.
	MaxBytes int64

	// Git runs every git process in a cache. Built over Layout.Home.
	Git *gitproc.Pool

	// Objects reads pack bundles from the object store.
	Objects Objects

	// Fetch reads a repository's rows.
	Fetch Fetcher

	// Limits is the current epoch's code.limits.
	Limits func() code.Limits

	// HookCommand is the argv hooks/proc-receive execs for every push's
	// receive-pack: arg0 absolute, no argument holding a NUL (a shell word
	// cannot carry one). The engine passes [<crewlet>, code, hook,
	// proc-receive].
	HookCommand []string

	// Faults is nil in production (internal/code/codefault).
	Faults codefault.Injector

	// Logger nil is the package's own (component code.cache) — never a
	// discarding one, because a missing warning reads as a quiet node.
	Logger *slog.Logger

	// Metrics nil records nothing: engine.Options.Metrics may be nil, and a
	// deployment without a recorder is a legal one.
	Metrics *metrics.Recorder

	// Now is the cache's clock: when a repository was last used.
	Now func() time.Time

	// knobs is the suite's reach into steps no option exposes
	// (export_test.go). Unexported, so nil everywhere but a test.
	knobs *knobs
}

// knobs are the test suite's hooks into the cache's own steps, each a
// moment a case must hold still to be deterministic rather than lucky.
type knobs struct {
	// hookStaged runs while the hook's staged file is still open for
	// writing — the window in which a fork makes it unexecutable.
	hookStaged func(f *os.File)
	// removing runs before the janitor removes each entry of trash/.
	removing func(path string)
	// ticks, when set, is where the janitor takes its ticks from, in place
	// of a ticker of EvictInterval: a case delivers a tick when it chooses
	// — or never, so that no tick can pass for a kick — rather than waiting
	// a minute for one.
	ticks <-chan time.Time
	// peeling runs as a round's peel is about to read the incarnation at
	// dir.
	peeling func(dir string)
	// roundEnding runs as a round over repo has converged and before it
	// lets the repository go: a case holds a round there, its write lock
	// given back and the round still the repository's.
	roundEnding func(repo string)
	// swept runs as the sweep's first half has made the incarnation at dir
	// its image, before what it left is held to the pool's shape check: a
	// case writes into it there, as something outside the engine would.
	swept func(dir string)
	// maintaining runs before each step of a maintenance run, with the run's
	// own context: a case holds a run in flight by blocking here.
	maintaining func(ctx context.Context, step string)
	// maintTicks, when set, is where the maintainer takes its ticks from, in
	// place of a ticker of MidxDebounce.
	maintTicks <-chan time.Time
	// verifyWaits runs as Verify is about to wait for the repository's
	// rounds before listing the incarnation at dir.
	verifyWaits func(dir string)
	// verifyListing runs while Verify lists the incarnation at dir — the
	// repository's rounds held off and no maintenance run let start on it.
	verifyListing func(dir string)
	// verifyFsck runs as Verify's fsck is about to ask for its maintenance
	// slot in the incarnation at dir.
	verifyFsck func(dir string)
	// checkBudget, when set, is the bound of a check pass over a copy holding
	// the bytes it is given, in place of CheckBudget's.
	checkBudget func(bytes int64) time.Duration
	// adopting runs while an adoption holds its share of the repository's
	// directory, before it links: a case holds adoptions there together.
	adopting func(repo string)
	// adoptSync, when set, is the directory sync an adoption's links are
	// made durable by, in place of the real one: a case makes it fail.
	adoptSync func(dir string) error
	// roundPacks, when above zero, is how many packs a full round leaves
	// before it writes its own index, in place of MidxRoundPacks: a case
	// exercises the round's index with a handful of pushes.
	roundPacks int
	// midxPacks, when above zero, is how many packs gathered beside the
	// maintainer's index make a repository due again, in place of
	// MidxPacks: an interleaving reaches a run over the maintainer's own
	// index with a handful of pushes.
	midxPacks int
	// fileSync, when set, makes a downloaded file durable in place of
	// (*os.File).Sync: a case watches which syncs are in flight together,
	// and what is already in objects/pack while they are.
	fileSync func(f *os.File) error
	// staged runs once a round has made a bundle's three files, as it hands
	// the pack to its worker: a case reads the order the round made them in.
	staged func(object objstore.Key)
	// stagedWrite, when set, is what every write into a bundle's staged
	// files answers while it is downloaded — nil to write: a case makes this
	// node's own disk fail under a download.
	stagedWrite func(object objstore.Key) error
	// asking runs as a round's worker is about to ask for its GET, its
	// bundle's files made: a case holds one pack there while the round's
	// others are free to ask before it.
	asking func(object objstore.Key)
	// roundIndexed, when set, is told the incarnation's directory every
	// time a full round writes its own multi-pack-index there: the
	// interleavings are held to reaching it.
	roundIndexed func(dir string)
	// hashing runs before a check hashes the file at path: a case counts
	// what a round hashes, or swaps the file under the hash.
	hashing func(path string)
	// checkPiece, when set, is the piece a hash reads in one slot in place
	// of CheckPieceBytes, so a case's small file is several pieces; and
	// pieceRead is told each piece's path once it is read, its slot still
	// held, and again once the slot is given back.
	checkPiece int64
	pieceRead  func(path string, held bool)
	// readFault, when set, is asked before each piece a hash reads, and an
	// error it answers is that read's: a case's disk failing under a file.
	readFault func(path string) error
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
		return refuse("MaxBytes %d is not a budget: git.cache_max_bytes is resolved by the engine and is above zero", o.MaxBytes)
	case o.Git == nil:
		return refuse("Git is nil: a cache runs its git through a gitproc.Pool built over Layout.Home")
	case o.Objects == nil:
		return refuse("Objects is nil: a cache reads its packs from the object store")
	case o.Fetch == nil:
		return refuse("Fetch is nil: a cache reads its repositories' rows through the code seam")
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

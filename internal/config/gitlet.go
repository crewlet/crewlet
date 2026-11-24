package config

import (
	"path/filepath"
	"strings"
)

// Gitlet is how this node serves the company's repositories when it has the gitlet
// role and the company sets code.backend: gitlet: where its disposable cache lives, how
// large it may grow, how many git processes it runs, and which hosts the git
// it runs may dial. Read once per process, like every Tier A block.
//
// TIER A, because every field is a fact about THIS NODE'S MACHINE — its disk,
// its cores, its memory, its network — rather than about the company: two Gitlet
// nodes of one company on different hardware size their caches and lanes
// differently, and a company document edited live has no business deciding
// how much of an operator's volume a cache may fill, or what of the node's
// network its git may reach.
type Gitlet struct {
	// CacheDir is where the node keeps its disposable copies of the
	// company's repositories (internal/gitlet/cache): relative to the store's
	// directory, like store.snapshot_dir; unset is <store dir>/git.
	CacheDir string `yaml:"cache_dir,omitempty" json:"cache_dir,omitempty" desc:"Where this node keeps its disposable copies of the company's repositories; relative to the store's directory; unset is <store dir>/git. A volume of its own is recommended: its own filesystem with its own space, not a subvolume or dataset drawing on the store's pool. Never backed up (it carries a CACHEDIR.TAG)."`

	// CacheMaxBytes is the cache's byte budget: what it evicts down to,
	// least recently used first. Unset derives it at boot (CacheBudget) from
	// what the engine measures of the cache's volume.
	CacheMaxBytes int64 `yaml:"cache_max_bytes,omitempty" json:"cache_max_bytes,omitempty" js:"min=1073741824;max=70368744177664" desc:"The cache's byte budget; unset is derived at boot from the room the cache can use: the volume's free space plus what the cache already holds, less what an embedded broker storing its streams on the same volume may still write under its limit (stream.store_max_bytes, or its own derivation), the request spools' allowance and the revision facts' patch spools (512 MiB). Half of that room when the volume also holds the store, the replicated estate, the snapshots or the broker's streams; seven eighths of a volume of its own. Never more, and never raised to a minimum: a volume that leaves no room at all refuses Gitlet on the node, naming what to change."`

	// MaxProcesses is the size of the git pool's READ lane — one core a
	// slot — and twice its MAINTENANCE lane's. Passed RAW to the pool,
	// which is the one place its default is resolved
	// (internal/gitlet/gitproc's Options.MaxProcesses).
	MaxProcesses int `yaml:"max_processes,omitempty" json:"max_processes,omitempty" js:"min=0;max=1024" desc:"Git processes this node runs at once to answer reads, one core each, and half as many again for its caches' background maintenance; 0 is GOMAXPROCS."`

	// MaxTransfers is the size of the pool's TRANSFER lane — the clones,
	// fetches and pushes in flight — passed raw like MaxProcesses.
	MaxTransfers int `yaml:"max_transfers,omitempty" json:"max_transfers,omitempty" js:"min=0;max=65536" desc:"Clones, fetches and pushes this node serves at once; 0 is four for each of max_processes, and no more than one for every 512 MiB of the memory the executor's pools leave. Each holds a few hundred MiB while it runs, so size it to memory rather than cores."`

	// ImportAllowPrivate lifts the public-address rule from the one outbound git
	// dial (internal/gitlet/gitproc's ResolveRemote): set, a remote's host is
	// pinned to whatever its name resolves to, a forge on this node's own network
	// among them. It governs every remote a Gitlet node dials (an import's source
	// and both kinds of mirror), though its name names only the first.
	//
	// TIER A, and off unset, because what it opens is THIS NODE'S network: a live
	// edited company document or an import's URL has no business deciding that git
	// may reach the node's metadata service or its neighbours. The zero value is
	// the refusal.
	ImportAllowPrivate bool `yaml:"import_allow_private,omitempty" json:"import_allow_private,omitempty" desc:"Let this node's imports and mirrors — pull and push alike, whatever the name says — dial a host that resolves to a loopback, link-local, private or otherwise non-public address, such as a forge on this node's own network. Off, a host any of whose addresses is not on the public internet is refused before git runs. A proxy the process names (HTTPS_PROXY, NO_PROXY) is dialed either way, and resolves the host itself."`

	// MaxChecks is how many of the company's checks this node runs at once
	// (internal/gitlet/checkrun), each a CONTAINER: a process tree on this
	// node's own machine — a build, a test suite — so what it may run beside
	// the node's own work is a fact about the node's cores.
	// A POINTER, because unset and 0 are two settings: unset is a quarter of
	// the node's cores and at least one ([Gitlet.MaxChecksFor]), and 0 is none,
	// a Gitlet node that serves the company's repositories and leaves its
	// checks to the fleet's other Gitlet nodes.
	MaxChecks *int `yaml:"max_checks,omitempty" json:"max_checks,omitempty" js:"min=0;max=1024" desc:"Checks this node runs at once, each a container on this machine running a repository's build or tests; unset is a quarter of the node's cores (GOMAXPROCS), at least one; 0 is none, leaving the company's checks to its other Gitlet nodes."`
}

const (
	// GitletCacheMaxBytesFloor is the smallest explicit cache budget: under a
	// gibibyte a Gitlet node cannot hold the working set of even a small
	// company, and a value that small is a byte count somebody meant in
	// GiB. The ceiling is [StoreMaxBytesCeiling]'s 64 TiB typo guard, the one
	// every byte setting of Tier A shares.
	GitletCacheMaxBytesFloor int64 = 1 << 30

	// GitletMaxProcessesCeiling is a TYPO GUARD, never policy: 1 024 cores is
	// past every machine a node runs on, so a value past it is a digit
	// somebody did not mean.
	GitletMaxProcessesCeiling = 1024

	// GitletMaxTransfersCeiling is a TYPO GUARD, never policy: 65 536
	// transfers at the few hundred MiB each holds is terabytes of memory.
	GitletMaxTransfersCeiling = 65536

	// GitletMaxChecksCeiling is a TYPO GUARD, never policy: 1 024 checks at
	// once is a container a core of a machine past any a node runs on.
	GitletMaxChecksCeiling = 1024

	// GitletCoresPerCheck is the cores of the node a container check is given
	// by an unset gitlet.max_checks: FOUR, so the node runs a quarter of
	// GOMAXPROCS checks at once ([Gitlet.MaxChecksFor]). A check is a build or a
	// test suite, which runs as many processes as it is let — `go test
	// ./...`, `make -j` — so a check a core would put each of them beside a
	// git process wanting that core; four cores a check leave the node's own
	// lanes (gitlet.max_processes, a core a slot) the three quarters Gitlet's reads
	// and maintenance use when busy, and give a parallel build room to be
	// parallel. A node whose checks are its main work sets the figure itself.
	GitletCoresPerCheck = 4

	// CacheVolumeDedicatedShare is the eighths of a DEDICATED volume's room the
	// derived cache budget takes: seven. The eighth left free is for what the
	// budget does not count — trash/ awaiting the janitor, and the
	// maintainer's new multi-pack-index and commit-graph, written beside the
	// old ones before they replace them.
	CacheVolumeDedicatedShare int64 = 7

	// CacheVolumeSharedShare is the eighths of a SHARED volume's room — what
	// the volume's bounded tenants leave of it — the derived cache budget
	// takes: four, HALF, the other half being the store's and its snapshots'
	// ([DerivedCacheMaxBytes]).
	CacheVolumeSharedShare int64 = 4

	// defaultGitCacheDirName is the cache's directory beside the store.
	defaultGitCacheDirName = "git"
)

// IsZero reports a block that sets nothing — [Bootstrap.Warnings]' predicate,
// and what lets an unset block drop out of a JSON round trip.
func (g Gitlet) IsZero() bool {
	return strings.TrimSpace(g.CacheDir) == "" && g.CacheMaxBytes == 0 &&
		g.MaxProcesses == 0 && g.MaxTransfers == 0 && g.MaxChecks == nil && !g.ImportAllowPrivate
}

// MaxChecksFor is how many checks this node runs at once in container cells
// on a machine of cores (GOMAXPROCS): gitlet.max_checks as written where it is
// set — 0 is none — and otherwise one for every [GitletCoresPerCheck] cores, AT
// LEAST ONE, so a two-core node runs one. THE ONE PLACE the default is
// resolved.
func (g Gitlet) MaxChecksFor(cores int) int {
	if g.MaxChecks != nil {
		return *g.MaxChecks
	}
	return max(1, cores/GitletCoresPerCheck)
}

// CacheDirFor is the cache's directory with the default applied and a
// relative value resolved against the store's own directory — the rule and
// the reason of [Store.SnapshotDirFor]: an engine's working directory is not
// something the person who wrote the config can see, so a path resolved
// against it would land somewhere different in a container and in a shell.
// storePath is the store's file (store.path).
func (g Gitlet) CacheDirFor(storePath string) string {
	dir := strings.TrimSpace(g.CacheDir)
	base := filepath.Dir(storePath)
	switch {
	case dir == "":
		return filepath.Join(base, defaultGitCacheDirName)
	case filepath.IsAbs(dir):
		return filepath.Clean(dir)
	}
	return filepath.Join(base, dir)
}

// CacheBudget is the explicit budget and false, or the derived one
// ([DerivedCacheMaxBytes]) and true; the engine logs the second return
// (code_cache_sized) to say where a budget came from.
//
// room is what only the engine can measure: the volume's free space PLUS what
// the cache already holds, LESS what the volume's other BOUNDED tenants may still
// write (an embedded broker up to its store limit; Gitlet's request spools and
// query.PatchSpoolAllowance). Free space alone excludes the cache's own files, so
// the budget shrank at every warm restart and evicted the difference. A broker's
// limit is itself derived from the same free space, so counting both counts the
// bytes twice. dedicated is whether the cache's volume is its own.
func (g Gitlet) CacheBudget(room int64, dedicated bool) (int64, bool) {
	if g.CacheMaxBytes > 0 {
		return g.CacheMaxBytes, false
	}
	return DerivedCacheMaxBytes(room, dedicated), true
}

// DerivedCacheMaxBytes is the budget an unset gitlet.cache_max_bytes derives from
// the room the cache can use ([Gitlet.CacheBudget]): SEVEN EIGHTHS of it on a
// DEDICATED volume ([CacheVolumeDedicatedShare]; every byte refused is a refetch
// later), HALF ([CacheVolumeSharedShare]) on a SHARED one, after the broker's
// limit is taken out. The cache (evicts to budget) and the store with its
// snapshots (cannot evict) have no ceiling of their own and divide the rest
// evenly, so the store keeps headroom for growth, a snapshot beside its
// predecessor and what the budget omits (trash/, a new index).
// NEVER MORE THAN THE SHARE, no floor: a cache works at any size, a floor above
// the share took the other tenants' last bytes. A share below
// [GitletCacheMaxBytesFloor] is warned at boot (code_cache_sized), not raised;
// no ceiling, as eviction holds it. No room, no cache: the engine refuses Gitlet.
func DerivedCacheMaxBytes(room int64, dedicated bool) int64 {
	if room <= 0 {
		return 0
	}
	share := CacheVolumeSharedShare
	if dedicated {
		share = CacheVolumeDedicatedShare
	}
	// THE QUOTIENT FIRST, then the remainder's share: room/8*share alone
	// drops up to seven bytes, and room*share/8 overflows past an exbibyte.
	return room/8*share + room%8*share/8
}

// validate refuses the gitlet block's settings this node cannot run. store is
// the node's store block, whose directory the cache's is resolved against and
// must stay out of.
func (g Gitlet) validate(path Path, store Store) error {
	var p problems
	switch {
	case g.CacheMaxBytes < 0:
		p.add(at(path, "cache_max_bytes"), ErrOutOfRange,
			"must be 0 or unset (which derives it from the cache's volume) or a "+
				"budget in bytes, got %d", g.CacheMaxBytes)
	default:
		bytesInRange(&p, path, "cache_max_bytes", g.CacheMaxBytes,
			GitletCacheMaxBytesFloor, StoreMaxBytesCeiling)
	}
	switch {
	case g.MaxProcesses < 0:
		p.add(at(path, "max_processes"), ErrOutOfRange,
			"must be 0 (one a core) or a positive number of processes, got %d",
			g.MaxProcesses)
	case g.MaxProcesses > GitletMaxProcessesCeiling:
		p.add(at(path, "max_processes"), ErrOutOfRange,
			"%d is past %d, which is more cores than any node has", g.MaxProcesses,
			GitletMaxProcessesCeiling)
	}
	switch {
	case g.MaxTransfers < 0:
		p.add(at(path, "max_transfers"), ErrOutOfRange,
			"must be 0 (four for each process slot) or a positive number of "+
				"transfers, got %d", g.MaxTransfers)
	case g.MaxTransfers > GitletMaxTransfersCeiling:
		p.add(at(path, "max_transfers"), ErrOutOfRange,
			"%d is past %d: each transfer holds a few hundred MiB while it runs, "+
				"so that many is terabytes of memory", g.MaxTransfers, GitletMaxTransfersCeiling)
	}
	if n := g.MaxChecks; n != nil && (*n < 0 || *n > GitletMaxChecksCeiling) {
		p.add(at(path, "max_checks"), ErrOutOfRange,
			"%d is outside 0..%d container checks at once (0 runs none on this node), or unset for a "+
				"quarter of the node's cores, at least one", *n, GitletMaxChecksCeiling)
	}
	if held, what := g.holdsStore(store); held != "" {
		p.add(at(path, "cache_dir"), ErrConflict,
			"resolves to %s, which holds %s: the cache removes what it finds under "+
				"its tmp/, scratch/ and trash/ at every boot: give it a directory of "+
				"its own (unset is <store dir>/%s)", held, what, defaultGitCacheDirName)
	}
	return p.err()
}

// holdsStore answers the resolved cache directory and what of the store's it
// holds ("the store" or "the snapshots") where it is that directory or an
// ancestor of it, and "" where it holds neither.
//
// REFUSED AT VALIDATE: the cache's boot guard (cache's Prepare refuses a non-empty
// untagged directory) is the second line and misses one case. The store and
// estate are opened first, so a cache directory holding either is refused then;
// but the SNAPSHOTS' directory is made after the cache is tagged, so it is
// adopted while empty and lives in a tree the cache empties at every boot.
// A store with no directory of its own (`:memory:`) has nothing to protect.
func (g Gitlet) holdsStore(store Store) (dir, what string) {
	storePath := strings.TrimSpace(store.Path)
	if storePath == "" || strings.HasPrefix(storePath, ":memory:") {
		return "", ""
	}
	cache, err := filepath.Abs(g.CacheDirFor(storePath))
	if err != nil {
		return "", ""
	}
	dirs := []struct{ dir, what string }{
		{filepath.Dir(storePath), "the store"},
		{store.SnapshotDirFor(), "the snapshots"},
	}
	// THE REPLICATED ESTATE where it lives apart from the store: the same
	// node's other database file, and the same hazard.
	if replicated := strings.TrimSpace(store.ReplicatedPath); replicated != "" &&
		!strings.HasPrefix(replicated, ":memory:") {
		dirs = append(dirs, struct{ dir, what string }{filepath.Dir(replicated), "the replicated estate"})
	}
	for _, held := range dirs {
		abs, err := filepath.Abs(held.dir)
		if err != nil {
			continue
		}
		if contains(cache, abs) {
			return cache, held.what
		}
	}
	return "", ""
}

// contains reports whether dir is ancestor or itself the directory of inner:
// both clean and absolute, compared by filepath.Rel, so `/a/b` holds `/a/b/c`
// and not `/a/bc`.
func contains(dir, inner string) bool {
	rel, err := filepath.Rel(dir, inner)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

package config

import (
	"path/filepath"
	"strings"
)

// Git is how this node serves the company's repositories when it has the git
// role and the company sets code.native: where its disposable cache lives, how
// large it may grow, how many git processes it runs, and which hosts the git
// it runs may dial. Read once per process, like every Tier A block.
//
// TIER A, because every field is a fact about THIS NODE'S MACHINE — its disk,
// its cores, its memory, its network — rather than about the company: two git
// nodes of one company on different hardware size their caches and lanes
// differently, and a company document edited live has no business deciding
// how much of an operator's volume a cache may fill, or what of the node's
// network its git may reach.
type Git struct {
	// CacheDir is where the node keeps its disposable copies of the
	// company's repositories (internal/code/cache): relative to the store's
	// directory, like store.snapshot_dir; unset is <store dir>/git.
	CacheDir string `yaml:"cache_dir,omitempty" json:"cache_dir,omitempty" desc:"Where this node keeps its disposable copies of the company's repositories; relative to the store's directory; unset is <store dir>/git. A volume of its own is recommended: its own filesystem with its own space, not a subvolume or dataset drawing on the store's pool. Never backed up (it carries a CACHEDIR.TAG)."`

	// CacheMaxBytes is the cache's byte budget: what it evicts down to,
	// least recently used first. Unset derives it at boot (CacheBudget) from
	// what the engine measures of the cache's volume.
	CacheMaxBytes int64 `yaml:"cache_max_bytes,omitempty" json:"cache_max_bytes,omitempty" js:"min=1073741824;max=70368744177664" desc:"The cache's byte budget; unset is derived at boot from the room the cache can use: the volume's free space plus what the cache already holds, less what an embedded broker storing its streams on the same volume may still write under its limit (stream.store_max_bytes, or its own derivation), the request spools' allowance and the revision facts' patch spools (512 MiB). Half of that room when the volume also holds the store, the replicated estate, the snapshots or the broker's streams; seven eighths of a volume of its own. Never more, and never raised to a minimum: a volume that leaves no room at all refuses the node's code host, naming what to change."`

	// MaxProcesses is the size of the git pool's READ lane — one core a
	// slot — and twice its MAINTENANCE lane's. Passed RAW to the pool,
	// which is the one place its default is resolved
	// (internal/code/gitproc's Options.MaxProcesses).
	MaxProcesses int `yaml:"max_processes,omitempty" json:"max_processes,omitempty" js:"min=0;max=1024" desc:"Git processes this node runs at once to answer reads, one core each, and half as many again for its caches' background maintenance; 0 is GOMAXPROCS."`

	// MaxTransfers is the size of the pool's TRANSFER lane — the clones,
	// fetches and pushes in flight — passed raw like MaxProcesses.
	MaxTransfers int `yaml:"max_transfers,omitempty" json:"max_transfers,omitempty" js:"min=0;max=65536" desc:"Clones, fetches and pushes this node serves at once; 0 is four for each of max_processes, and no more than one for every 512 MiB of the node's memory. Each holds a few hundred MiB while it runs, so size it to memory rather than cores."`

	// ImportAllowPrivate lifts the public-address rule from the one outbound
	// git dial (internal/code/gitproc's ResolveRemote): set, a remote's host
	// is pinned to whatever its name resolves to, a forge on this node's own
	// network among them. It governs every remote a git node dials — an
	// import's source and both kinds of mirror — though its name names
	// only the first of them.
	//
	// TIER A, and off unset, because what it opens is THIS NODE'S network:
	// the addresses a dial may reach are the ones its machine is wired to,
	// and a company document edited live — or a person naming an import's
	// URL — has no business deciding that a git process may reach the
	// node's metadata service or its neighbours. The zero value is the
	// refusal, so a block that never names it keeps the wall.
	ImportAllowPrivate bool `yaml:"import_allow_private,omitempty" json:"import_allow_private,omitempty" desc:"Let this node's imports and mirrors — pull and push alike, whatever the name says — dial a host that resolves to a loopback, link-local, private or otherwise non-public address, such as a forge on this node's own network. Off, a host any of whose addresses is not on the public internet is refused before git runs. A proxy the process names (HTTPS_PROXY, NO_PROXY) is dialed either way, and resolves the host itself."`

	// MaxChecks is how many of the company's checks this node runs at once
	// in CONTAINER cells (internal/code/checkrun): a container is a process
	// tree on this node's own machine — a build, a test suite — so what it
	// may run beside the node's own work is a fact about the node's cores.
	// A POINTER, because unset and 0 are two settings: unset is a quarter of
	// the node's cores and at least one ([Git.MaxChecksFor]), and 0 is none,
	// a git node that serves the company's repositories and leaves its
	// container checks to the fleet's other git nodes. A remote (e2b) check
	// runs on another machine and takes none of these.
	MaxChecks *int `yaml:"max_checks,omitempty" json:"max_checks,omitempty" js:"min=0;max=1024" desc:"Checks this node runs at once in container cells — each a container on this machine running a repository's build or tests; unset is a quarter of the node's cores (GOMAXPROCS), at least one; 0 is none, leaving the company's container checks to its other git nodes. A remote (e2b) check runs elsewhere and is not counted."`
}

const (
	// GitCacheMaxBytesFloor is the smallest explicit cache budget: under a
	// gibibyte a git node cannot hold the working set of even a small
	// company, and a value that small is a byte count somebody meant in
	// GiB. The ceiling is [StoreMaxBytesCeiling]'s 64 TiB typo guard, the one
	// every byte setting of Tier A shares.
	GitCacheMaxBytesFloor int64 = 1 << 30

	// GitMaxProcessesCeiling is a TYPO GUARD, never policy: 1 024 cores is
	// past every machine a node runs on, so a value past it is a digit
	// somebody did not mean.
	GitMaxProcessesCeiling = 1024

	// GitMaxTransfersCeiling is a TYPO GUARD, never policy: 65 536
	// transfers at the few hundred MiB each holds is terabytes of memory.
	GitMaxTransfersCeiling = 65536

	// GitMaxChecksCeiling is a TYPO GUARD, never policy: 1 024 checks at
	// once is a container a core of a machine past any a node runs on.
	GitMaxChecksCeiling = 1024

	// GitCoresPerCheck is the cores of the node a container check is given
	// by an unset git.max_checks: FOUR, so the node runs a quarter of
	// GOMAXPROCS checks at once ([Git.MaxChecksFor]). A check is a build or a
	// test suite, which runs as many processes as it is let — `go test
	// ./...`, `make -j` — so a check a core would put each of them beside a
	// git process wanting that core; four cores a check leave the node's own
	// lanes (git.max_processes, a core a slot) the three quarters a busy code
	// host's reads and maintenance use, and give a parallel build room to be
	// parallel. A node whose checks are its main work sets the figure itself.
	GitCoresPerCheck = 4

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
func (g Git) IsZero() bool {
	return strings.TrimSpace(g.CacheDir) == "" && g.CacheMaxBytes == 0 &&
		g.MaxProcesses == 0 && g.MaxTransfers == 0 && g.MaxChecks == nil && !g.ImportAllowPrivate
}

// MaxChecksFor is how many checks this node runs at once in container cells
// on a machine of cores (GOMAXPROCS): git.max_checks as written where it is
// set — 0 is none — and otherwise one for every [GitCoresPerCheck] cores, AT
// LEAST ONE, so a two-core node runs one. THE ONE PLACE the default is
// resolved.
func (g Git) MaxChecksFor(cores int) int {
	if g.MaxChecks != nil {
		return *g.MaxChecks
	}
	return max(1, cores/GitCoresPerCheck)
}

// CacheDirFor is the cache's directory with the default applied and a
// relative value resolved against the store's own directory — the rule and
// the reason of [Store.SnapshotDirFor]: an engine's working directory is not
// something the person who wrote the config can see, so a path resolved
// against it would land somewhere different in a container and in a shell.
// storePath is the store's file (store.path).
func (g Git) CacheDirFor(storePath string) string {
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
// ([DerivedCacheMaxBytes]) and true — the second return is what the engine logs
// (code_cache_sized), so a node can say where its budget came from.
//
// room is the space the cache can use, which only the engine can measure: the
// volume's free space PLUS what the cache already holds, LESS what the volume's
// other BOUNDED tenants may still write there — an embedded broker sharing the
// volume, up to the store limit its server enforces, and the code host's
// request spools, up to their allowance, and the revision facts' patch spools,
// up to theirs (query.PatchSpoolAllowance). Free space excludes the cache's own
// files, and a budget derived from it alone shrank at every restart of a warm
// node, whose first janitor pass then evicted the difference — warm
// repositories downloaded again, a GET a push, for no change in the disk. And
// a broker's limit is room IT may fill, itself derived from the same free
// space: a budget taken from that free space beside it claimed the same bytes
// twice. dedicated is whether the cache's volume is one of its own, which the
// engine decides by comparing filesystems.
func (g Git) CacheBudget(room int64, dedicated bool) (int64, bool) {
	if g.CacheMaxBytes > 0 {
		return g.CacheMaxBytes, false
	}
	return DerivedCacheMaxBytes(room, dedicated), true
}

// DerivedCacheMaxBytes is the budget an unset git.cache_max_bytes derives from
// the room the cache can use ([Git.CacheBudget]):
//
//   - DEDICATED — the volume holds nothing else of this node's — SEVEN EIGHTHS
//     ([CacheVolumeDedicatedShare]). A quarter of a volume nothing else uses
//     was three quarters wasted, and every byte the budget refuses is a GET a
//     push the next time an evicted repository is fetched.
//   - SHARED — the volume also holds the store, the replicated estate, the
//     snapshots or the broker's streams — HALF ([CacheVolumeSharedShare]) of
//     what the bounded tenants leave. The broker is in neither half: its limit
//     is taken out of the room before it is divided. What is left has two
//     tenants with no ceiling of their own — the cache, which evicts to its
//     budget, and the store with its snapshots, which cannot evict anything —
//     so they divide it evenly, and the store never has less headroom than
//     the disposable cache beside it: its own growth, a snapshot written
//     beside the one it replaces, and what the cache's budget does not count
//     (trash/ awaiting the janitor, a new index beside the old).
//
// NEVER MORE THAN THE SHARE, and no floor: a cache needs no minimum to work —
// it serves a repository larger than its budget and evicts it once idle —
// while a floor above the share is a budget the volume does not have, which on
// a shared volume took the store's, the broker's and the snapshots' last bytes
// before the cache evicted anything. A share below [GitCacheMaxBytesFloor] is
// said by the engine at boot (code_cache_sized, a warning) rather than raised
// to it. And no derived ceiling: the cache's own eviction holds it to whatever
// this answers, and a ceiling would waste exactly the dedicated volume this
// rule exists to use. A room of nothing — a volume its bounded tenants may
// fill — is a budget of nothing, which is no cache at all: the engine refuses
// to build a code host on it, naming the volume's figures and the settings
// that give it room, since any budget it chose there would be bytes the volume
// does not have.
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

// validate refuses the git block's settings this node cannot run. store is
// the node's store block, whose directory the cache's is resolved against and
// must stay out of.
func (g Git) validate(path Path, store Store) error {
	var p problems
	switch {
	case g.CacheMaxBytes < 0:
		p.add(at(path, "cache_max_bytes"), ErrOutOfRange,
			"must be 0 or unset — which derives it from the cache's volume — or a "+
				"budget in bytes, got %d", g.CacheMaxBytes)
	default:
		bytesInRange(&p, path, "cache_max_bytes", g.CacheMaxBytes,
			GitCacheMaxBytesFloor, StoreMaxBytesCeiling)
	}
	switch {
	case g.MaxProcesses < 0:
		p.add(at(path, "max_processes"), ErrOutOfRange,
			"must be 0 — one a core — or a positive number of processes, got %d",
			g.MaxProcesses)
	case g.MaxProcesses > GitMaxProcessesCeiling:
		p.add(at(path, "max_processes"), ErrOutOfRange,
			"%d is past %d, which is more cores than any node has", g.MaxProcesses,
			GitMaxProcessesCeiling)
	}
	switch {
	case g.MaxTransfers < 0:
		p.add(at(path, "max_transfers"), ErrOutOfRange,
			"must be 0 — four for each process slot — or a positive number of "+
				"transfers, got %d", g.MaxTransfers)
	case g.MaxTransfers > GitMaxTransfersCeiling:
		p.add(at(path, "max_transfers"), ErrOutOfRange,
			"%d is past %d: each transfer holds a few hundred MiB while it runs, "+
				"so that many is terabytes of memory", g.MaxTransfers, GitMaxTransfersCeiling)
	}
	if n := g.MaxChecks; n != nil && (*n < 0 || *n > GitMaxChecksCeiling) {
		p.add(at(path, "max_checks"), ErrOutOfRange,
			"%d is outside 0..%d container checks at once — 0 runs none on this node — or unset for a "+
				"quarter of the node's cores, at least one", *n, GitMaxChecksCeiling)
	}
	if held, what := g.holdsStore(store); held != "" {
		p.add(at(path, "cache_dir"), ErrConflict,
			"resolves to %s, which holds %s: the cache removes what it finds under "+
				"its tmp/, scratch/ and trash/ at every boot — give it a directory of "+
				"its own (unset is <store dir>/%s)", held, what, defaultGitCacheDirName)
	}
	return p.err()
}

// holdsStore answers the resolved cache directory and what of the store's it
// holds — "the store" or "the snapshots" — where it is that directory or an
// ancestor of it, and "" where it holds neither.
//
// REFUSED AT VALIDATE, before a node is started on the shape: the cache's own
// guard at boot (internal/code/cache's Prepare refuses a non-empty directory
// without its tag) is the second line, not the first, and it cannot see one
// case at all. The node's store and its replicated estate are opened before
// the code host is built, so a cache directory holding either is non-empty and
// untagged by then and Prepare refuses it; but the SNAPSHOTS' directory is
// made at the first snapshot, after the cache has been prepared and tagged, so
// a cache directory that holds it is adopted while still empty, and from then
// on the snapshots live in a tree the cache treats as its own (it empties its
// tmp/, scratch/ and trash/ at every boot). Here, and only here, is that shape
// refused before it happens — and `crewlet validate` names the other two
// before a node is ever started on them.
//
// A store with no directory of its own — `:memory:`, an empty path — has
// nothing on disk to protect, and is not compared.
func (g Git) holdsStore(store Store) (dir, what string) {
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

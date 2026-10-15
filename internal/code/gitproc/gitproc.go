// Package gitproc is THE ONE GIT SUBPROCESS WRAPPER. Every git process the
// engine starts — a clone's upload-pack, a push's receive-pack, the cache's
// multi-pack-index, the receive pipeline's reads — is started by [Pool.Run],
// and nothing else in the module but cmd/'s operator commands may start git
// or one of its git-* programs: TestOnlyGitprocExecsGit holds that, and
// ADR-0008 is why it is a gate.
//
// # Why one wrapper
//
// The code host runs real upstream git rather than a reimplementation,
// because pack negotiation, delta resolution and fsck are what a
// reimplementation gets subtly wrong and a client notices first. So a git
// process is the unit of work on every git node, and each one is a program
// with its own idea of where its configuration comes from, how long it may
// run and what it leaves running. Six decisions have to be made the same way
// for every one — the environment, the configuration, the repository it may
// trust, the slot, the end of its process tree, and what its failure means —
// and a second exec.Command is a place to forget one of them.
//
// # A git process is configured by the engine and by nothing else
//
// The environment is BUILT, never inherited: the engine's own holds the
// company's tokens and the store's keys, and git reads dozens of variables
// (GIT_EXEC_PATH, GIT_ALTERNATE_OBJECT_DIRECTORIES, GIT_CONFIG_PARAMETERS,
// GIT_SSH_COMMAND, GIT_TRACE…) any one of which changes what a process is.
// HOME is an empty directory the pool owns, and neither the system nor the
// global configuration is read — nor the system or global ATTRIBUTES, which
// the configuration switches do not reach: git reads /etc/gitattributes
// unless GIT_ATTR_NOSYSTEM is set and $HOME/.config/git/attributes unless
// core.attributesFile names another file, so the environment sets the one and
// the base configuration points the other at a path that cannot exist.
// pack-objects asks a path's `delta` attribute when it SEARCHES for a delta,
// so a `* -delta` an operator wrote on the host for something else would
// leave whole every object whose delta pack-objects has to compute: one
// stored whole, and every object of a pack written without reusing the
// stored deltas — which is what a compaction that recomputes deltas writes,
// and keeps. A clone or fetch of what the host stores reuses its stored
// deltas whatever the attribute says. Measured on git 2.43 over thirty
// revisions of one 100 KB file: 135 120 bytes either way from one pack and
// 79 987 either way from thirty pushes kept as their packs, but
// 135 304 → 1 691 584 (12.5×) with --no-reuse-delta, and 135 120 →
// 1 691 584 from loose objects.
//
// Configuration reaches a process only as GIT_CONFIG_COUNT pairs: a fixed
// BASE the pool owns (no transport, no automatic gc or maintenance, one
// thread, no hook unless the call site names a hooks directory, no global
// attributes file), then the call site's own pairs, each of which must be on
// an ALLOWLIST. An allowlist rather than a list of forbidden keys, for
// hostbox's reason: git has a dozen keys that run a command (alias.*,
// uploadpack.packObjectsHook, core.fsmonitor, core.alternateRefsCommand,
// include.path, …) and one that serves every object in a repository to
// anyone (uploadpack.allowAnySHA1InWant, which the code host never sets) — a
// denylist leaks whichever nobody named.
//
// A REPOSITORY CANNOT CONFIGURE ITS OWN PROCESS, and the obvious mechanism
// does not achieve it: command-line configuration outranks a repository's
// config file only for the keys it sets, and every other key is read from the
// file — a repository config naming uploadpack.allowAnySHA1InWant is honoured
// by every git that does not set it on its command line. So the pool refuses
// to run in a directory whose config is not [RepoConfig] byte for byte, or
// that holds a commondir file (which points git at ANOTHER repository's
// objects and refs, and which GIT_COMMON_DIR does not override), and it
// neutralises the two repository files that change what history IS —
// info/grafts and shallow — by pointing GIT_GRAFT_FILE and GIT_SHALLOW_FILE
// at a path that can never exist. Every repository the engine runs git in is
// one it wrote by hand: a cache, a push's scratch, an advertisement view.
//
// NOR DOES A REPOSITORY CHOOSE WHERE ITS OBJECTS COME FROM. Its objects
// directory must be its own — at its real path, never a link — and its one
// alternate is the call's ([Cmd.Alternate]: a push's scratch and an
// advertisement view the cache incarnation they lean on, everything else
// none), the alternates file exactly the one line naming it or absent: under
// protocol v2 every object either way reaches is fetchable by id through the
// repository, so a cache with a planted alternates file or a linked objects
// directory would serve another repository's objects. And the guard reaches
// ONE REPOSITORY FURTHER than the process's own, because git does: a
// repository with an alternate lists that alternate's refs by starting
// another git in it with every variable this pool sets for a repository
// CLEARED (git's local_repo_env — GIT_CONFIG_COUNT, GIT_GRAFT_FILE and
// GIT_SHALLOW_FILE among them). That process is configured by the alternate's
// own files and nothing else, so the alternate must pass the same shape
// check, and may name no alternate of its own — and since git runs it at the
// REAL path it resolves the line to, the alternate's objects directory being
// its own is what makes that path the alternate checked.
//
// The refusal is a *[ShapeError] naming its [ShapeRule] and the repository
// that broke it (errors.Is [ErrRepoShape]), a definite answer — a file absent,
// other bytes, a commondir present — and a read that FAILED is never one: it
// is returned as the read's own error, so a node out of descriptors is never
// told its healthy caches were tampered with. [CheckShape] is that check,
// exported for the git cache's sweep, which repairs a cache the pool refused
// and holds what it leaves to the pool's own predicate.
//
// # Slots: three lanes, each sized by what it spends
//
// A slot bounds the resource a process spends, and git's processes spend two
// different ones. A READ (rev-parse, cat-file, merge-base, rev-list) and a
// MAINTENANCE run (multi-pack-index, commit-graph, fsck) spend a CORE from
// start to end: every process is single-threaded (pack.threads=1 in the
// base), so the read lane holds [Options.MaxProcesses] slots and parallelism
// comes from concurrent requests, which is where a git host's load is. No
// node indexes a pack it downloads: a push stores its pack with the index
// and reverse index its receiving git wrote, so a cache installs one with
// renames and no git process at all.
//
// A TRANSFER (upload-pack, receive-pack) is paced by its CLIENT: a clone at
// the transfer floor holds its process for hours with the CPU idle, and a
// push holds receive-pack through the object-store upload and the wait for
// its record. Counted as cores, a handful of slow clients would refuse every
// clone on an idle node; so transfers have a lane of their own,
// [Options.MaxTransfers] wide, bounded by what each transfer holds in MEMORY
// rather than by the cores it rarely uses — by default four a core, and no
// more than the node's memory backs: one for every [TransferShare] of it,
// read at New (memory.go).
//
// The lanes are separate for a second reason that no sizing could fix: one
// process waits on others. A push's receive-pack holds its transfer slot
// while its proc-receive hook runs the receive pipeline's reads; in one pool,
// N concurrent pushes each wait for an N+1th slot that never frees. A read
// waits on nothing while it holds a slot, so a transfer waiting on reads can
// never deadlock. The rule every caller keeps: never wait for a slot while
// holding one in the same lane.
//
// Maintenance never waits, and its lane is half the node's cores, rounded
// up: background work no client waits on takes a free slot or none, and
// never more than half the cores the node's reads and transfers share. Any
// other wait longer than [SlotWait] is [ErrBusy].
//
// One transfer may use more than one core: a push's receive-pack — the one that
// receives a pack, never the push's advertisement (--advertise-refs), which
// runs no index-pack — is LENT up to [MaxLentCores] of the read lane's free
// cores, less the one its own thread runs on, as threads for the index-pack a
// person waits on — sized when it starts, so a loaded node lends nothing; never
// holding the read slots it was sized by, so no read waits for a loan and the
// push's own hook never queues behind its own receive-pack; and repaid as soon
// as the call site says the index-pack is done ([Cmd.Indexed], without which a
// push's receive-pack is refused), so a push's hook, upload and record wait
// hold no loan (lane.go says why each matters).
//
// # Every process ends, and so does everything it started
//
// Each process leads its own process group (internal/procgroup), so its
// children — upload-pack's pack-objects, receive-pack's index-pack — end with
// it. The caller's context, the call's deadline and the stall watchdog each
// end a run the same way: SIGTERM to the group, SIGKILL [KillGrace] later.
// Every signal is sent only after procgroup's Leader.Current confirms the
// group is still the one the pool started, and once the process is reaped
// the group is swept, so nothing a run started is running when Run returns.
//
// The pipes are the pool's, not os/exec's, and they end when the process tree
// does — never on a timer. os/exec's WaitDelay closes a pipe a fixed time
// after the process exits, which cuts the last pipe-full of a clone off when
// its client is slower than the timer; here every byte git wrote reaches the
// caller, and only a pipe held by a descendant that escaped the group, and
// left EMPTY, is closed from this side.
//
// # What every process cost is counted when it ends
//
// A lane counts processes, and a transfer's slot says nothing about the
// cores it burns: a clone paced by a slow client holds its slot for hours on
// almost no CPU, a burst of identical clones each runs pack-objects over the
// same history. So every run's [Result] carries its CPU — the user and system
// time wait4 reports at the reap, which the kernel sums over the process and
// every descendant it waited for (upload-pack's pack-objects, receive-pack's
// index-pack) — and every run is one observation of crewlet.code.git.cpu by
// class, as it ends. A run the node ENDS — its deadline, a stall, its caller
// gone — is read before it is signalled as well: the group dies as one and
// its leader reaps none of the rest, so the reap alone would count a clone
// abandoned at the end of its pack-objects as almost free; the group's own
// process records, read just before the signal (procgroup.GroupCPU), hold
// what every member spent, and the run counts the greater of the two (Linux;
// on darwin the reap's alone). Read from the reap rather than from this
// process's children's usage (getrusage), which sums every git the process
// ever reaped and cannot tell one run from another — nor the server's git
// from a test's client git running beside it.
//
// One observation is one run's whole cost, recorded when it ends: the cost
// a request or a lane is charged, never a share of the node over a window —
// a run's CPU lands in whichever window it ends in, all at once, however
// long it ran.
//
// # What went wrong is classified from git's own words
//
// Every stderr line is read as it arrives and classified by a table built
// from real damaged repositories (classify_test.go): a pack, index or
// multi-pack-index git could not read ([ErrCorrupt]), an object git could
// not find ([ErrMissingObject]), a want upload-pack does not serve
// ([ErrNotOurRef]) — never a rebuild trigger, since an absent want and a
// cache that lost a pack print the same line — or none of those
// ([ErrFailed]). LC_ALL=C, because the table matches git's English.
//
// # Measured
//
// On four vCPUs of a 2.8 GHz Xeon (no SHA extensions) shared with other
// builds, with git 2.43.0 and Go 1.27 (make bench, six counts, medians;
// bench_test.go says what each arm is): one process through the pool — slot,
// environment, fork and exec, identity, reap and sweep — costs 2.1 ms, no
// more than the same `git version` from a shell loop on that machine
// (2.3 ms), so the pool's own share is lost in the fork; 2.3 ms for a
// rev-parse that reads one object, and 2.5 ms for the same read in a
// scratch, through its alternate and the guard's second repository; 0.70 ms
// a process — about 1 400 a second — with every read slot busy; and 14 ms
// for a cat-file --batch-check --buffer of a thousand ids streamed through
// [Lines], against 22 ms without --buffer, which makes git write and flush
// each answer as it goes and wakes the reader for every line (both
// re-measured on four vCPUs of a 2.1 GHz Xeon, where the unbuffered figure
// came out unchanged) — so a caller whose next question never waits on an
// answer asks for --buffer. A clone of an 18 MiB pack of 2 178 objects
// streams through Run at 146 MiB/s, which is git's own rate for the request
// (the same upload-pack writing to /dev/null from a shell: about
// 140 MiB/s), so git and not the
// pool is what bounds a clone: the same pack through the pool with git taken
// out — a stand-in that cats it, its own process start included, so a lower
// bound on the copy path — moves 860 MiB/s, about 7 Gbit/s and six times
// git's rate. The index-pack every push runs inside receive-pack, measured
// in receive-pack's own form at one thread — the pack on stdin and written
// out, every object checked (--strict) and looked up in the repository —
// indexes at 7.3 MiB/s (0.14 s a mebibyte) when every pushed object is
// already in the repository and is read back and compared, the worst case
// and the figure [PackWorkPerMiB] is anchored to (its one second a mebibyte
// is seven times it), and at 8.8 MiB/s (0.11 s) when every object is new.
//
// git ≥ 2.40 ([MinVersion]); the engine image ships 2.47 (Debian trixie),
// and a git CVE is an engine CVE on a git node: the image tracks Debian's
// security updates through the Dependabot docker surface.
package gitproc

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// log is the package's logger, which a Pool uses when Options.Logger is nil.
var log = logging.Get("code.gitproc")

// Options is how a [Pool] is built: which git, how many processes of each
// kind, and where its HOME is.
type Options struct {
	// Path is the git binary; "" is exec.LookPath("git") on the engine's
	// PATH. It must resolve to an absolute path (a relative PATH entry is
	// refused, exec.ErrDot included).
	Path string

	// MaxProcesses is the size of the READ lane — one core a slot — and
	// twice the MAINTENANCE lane's, rounded up; 0 is runtime.GOMAXPROCS(0);
	// negative is refused. Passed RAW from configuration: this is the one
	// place its default is resolved, and [Pool.Slots] reports the result.
	MaxProcesses int

	// MaxTransfers is the size of the TRANSFER lane (upload-pack,
	// receive-pack); 0 is [TransfersPerProcess] × the resolved
	// MaxProcesses, and no more than one for every [TransferShare] of the
	// node's memory (memory.go); negative is refused. Passed RAW, like
	// MaxProcesses.
	MaxTransfers int

	// Memory is the node's memory in bytes, which the TRANSFER lane's
	// default is bounded by: 0 — the engine's setting — reads the node's
	// own at New, the least of its physical memory and its cgroups' limits
	// ([Pool.Memory] answers it, [Pool.ReadMemory] reads it again), and New
	// refuses a node whose memory cannot be read; above zero is that figure,
	// held for the pool's life, so a suite sizes a lane without the machine
	// it runs on; negative is refused.
	Memory int64

	// Home is HOME for every process: absolute; created when absent; its
	// mode is set to hostbox.DirMode; it must be EMPTY.
	Home string

	// Logger is where the pool logs; nil is the package's own
	// (component code.gitproc).
	Logger *slog.Logger

	// Metrics is where the pool records crewlet.code.git.wait (how long
	// each admission waited for its lane), crewlet.code.git.processes (the
	// processes running in each class), crewlet.code.git.lent_cores (the
	// read lane's cores lent to pushes' receive-packs right now) and
	// crewlet.code.git.cpu (the CPU each run's process tree spent, by its
	// class, recorded whole as the run ends); nil records nothing.
	Metrics *metrics.Recorder
}

// TransfersPerProcess is how many transfers the TRANSFER lane admits for
// every slot of the READ lane when Options.MaxTransfers is 0.
//
// FOUR, A MEMORY BOUND RATHER THAN A CPU ONE: a transfer spends a core only
// while pack-objects counts and compresses or index-pack resolves deltas, and
// otherwise waits on its client, on the object store or on its record. What
// it holds for its whole life is memory — the delta window (pack.windowMemory,
// twice the blob limit: 100 MiB at the defaults), the delta base cache
// (core.deltaBaseCacheLimit, git's default 96 MiB) and the object list
// ([TransferBytesPerObject] an object of a whole repository, MEASURED) —
// [TransferMemory], 224 MiB, for a repository of about 120 000 objects. Four a
// core is 896 MiB a core at that; a receive-pack lent cores ([MaxLentCores])
// raises its index-pack's base-cache cap by core.deltaBaseCacheLimit a lent
// thread (git caps it at the limit times its threads), and the loans out
// never reach the read lane's width. That is inside the 2 GiB a vCPU of even
// the compute-optimised instance shapes ([CoreMemory]; general-purpose ones
// give 4) with the store and the broker beside it; when every transfer IS
// CPU-bound (fast clients on the same network) the scheduler shares the cores
// four ways and a clone is slower, never refused. And because it is a memory
// bound, a node with less memory than that a core — a container limited in
// memory and not in CPU sees its host's every core — runs one transfer for
// every [TransferShare] it has rather than four a core (memory.go). A
// judgment call, and one the operator's own setting overrides.
//
// What it does NOT bound is a transfer of a repository of millions of
// objects, which holds [TransferBytesPerObject] an object whatever its
// slot: no lane knows which repository a slot will serve.
const TransfersPerProcess = 4

// Pool runs git processes. It holds no goroutine and needs no Close: every
// goroutine it starts is started and joined by one Run.
type Pool struct {
	// git is the binary. THE FIELD'S NAME MATTERS: the exec gate finds this
	// package's one exec of git by the word git in its first argument.
	git     string
	version Version
	reuse   string

	home    string // resolved
	ceiling string // filepath.Dir(home): GIT_CEILING_DIRECTORIES for a run with no repository
	baseEnv []string

	read, maint, transfer *lane

	// memory is the node's memory the lanes were sized by, and fixedMemory
	// whether Options.Memory gave it, so ReadMemory answers it again
	// rather than reading the node.
	memory      Memory
	fixedMemory bool

	// slotWait, killGrace and captureMax are SlotWait, KillGrace and
	// CaptureMax; fields so the suite can scale them (export_test.go).
	slotWait, killGrace time.Duration
	captureMax          int

	// gaugeMu orders every change a gauge reports — the processes running
	// in each class and the read lane's cores lent — so a later Set never
	// carries an older count.
	gaugeMu sync.Mutex
	running map[Class]int64

	log     *slog.Logger
	metrics *metrics.Recorder
}

// New builds a pool over the node's git, checks it is at least [MinVersion]
// (an [*ErrTooOld] otherwise), and resolves the lanes' sizes. Every refusal
// names what to change.
func New(ctx context.Context, o Options) (*Pool, error) {
	git := o.Path
	if git == "" {
		found, err := exec.LookPath("git")
		if err != nil {
			return nil, fmt.Errorf("gitproc: no git on this node's PATH (%w): the code host runs the node's own git, 2.40 or newer; the engine image ships 2.47", err)
		}
		git = found
	}
	if !filepath.IsAbs(git) {
		return nil, fmt.Errorf("gitproc: the git binary %q is not an absolute path", git)
	}

	if o.MaxProcesses < 0 {
		return nil, fmt.Errorf("gitproc: git.max_processes %d is negative: 0 is one a core", o.MaxProcesses)
	}
	if o.MaxTransfers < 0 {
		return nil, fmt.Errorf("gitproc: git.max_transfers %d is negative: 0 is %d for each process slot", o.MaxTransfers, TransfersPerProcess)
	}
	memory, err := resolveMemory(o, readNodeMemory)
	if err != nil {
		return nil, err
	}
	cpuSlots := int64(o.MaxProcesses)
	if cpuSlots == 0 {
		cpuSlots = int64(runtime.GOMAXPROCS(0))
	}
	transferSlots := int64(o.MaxTransfers)
	if transferSlots == 0 {
		transferSlots = defaultTransfers(cpuSlots, memory.Bytes)
	}

	home, err := prepareHome(o.Home)
	if err != nil {
		return nil, err
	}

	logger := o.Logger
	if logger == nil {
		logger = log
	}
	p := &Pool{
		git:     git,
		home:    home,
		ceiling: filepath.Dir(home),
		// The table every process starts from, in this order; Run adds
		// GIT_DIR or GIT_CEILING_DIRECTORIES, GIT_PROTOCOL, the
		// configuration and the call site's two variables.
		baseEnv: []string{
			// git runs its own subcommands from its exec-path; PATH is
			// what a hook's interpreter resolves by.
			"PATH=" + os.Getenv("PATH"),
			// An empty directory the pool owns. Nothing git would read
			// there is read even when something writes into it later:
			// the configuration by GIT_CONFIG_GLOBAL below, the
			// attributes by the base's core.attributesFile (cmd.go).
			"HOME=" + home,
			// /etc/gitconfig is the host's, not the engine's.
			"GIT_CONFIG_NOSYSTEM=1",
			// Overrides both ~/.gitconfig and $XDG_CONFIG_HOME/git/config.
			"GIT_CONFIG_GLOBAL=/dev/null",
			// /etc/gitattributes is the host's too, and the variable above
			// does not reach it (traced on git 2.43: a process with
			// GIT_CONFIG_NOSYSTEM still opens it). pack-objects' delta
			// search asks a path's `delta` attribute, so a host's
			// `* -delta`, written for something else entirely, would
			// write undeltified every delta a repack recomputes — 12.5
			// times the bytes, measured (the package doc) — though the
			// deltas a clone or fetch reuses are sent as stored.
			"GIT_ATTR_NOSYSTEM=1",
			// Nothing ever prompts.
			"GIT_TERMINAL_PROMPT=0",
			// A refs/replace/ ref can never change what an object id means.
			"GIT_NO_REPLACE_OBJECTS=1",
			// info/grafts rewrites parents and shallow truncates history;
			// a path that can never exist (ENOTDIR) disables each, without
			// the deprecation hint /dev/null itself prints. Shallow clones
			// still work: upload-pack and receive-pack pass their own
			// temporary shallow files.
			"GIT_GRAFT_FILE=/dev/null/grafts",
			"GIT_SHALLOW_FILE=/dev/null/shallow",
			// The classification table reads git's English.
			"LC_ALL=C",
		},
		memory:      memory,
		fixedMemory: o.Memory > 0,
		read:        newLane(ClassRead, cpuSlots),
		maint:       newLane(ClassMaintenance, (cpuSlots+1)/2),
		transfer:    newLane(ClassTransfer, transferSlots),
		slotWait:    SlotWait,
		killGrace:   KillGrace,
		captureMax:  CaptureMax,
		running:     make(map[Class]int64, len(Classes)),
		log:         logger,
		metrics:     o.Metrics,
	}
	for _, c := range Classes {
		p.running[c] = 0
	}

	res, err := p.Run(ctx, Cmd{Args: []string{"version"}, Class: ClassRead, Deadline: ReadDeadline})
	if err != nil {
		return nil, fmt.Errorf("gitproc: running %s version: %w", git, err)
	}
	found, err := ParseVersion(string(res.Stdout))
	if err != nil {
		return nil, fmt.Errorf("gitproc: reading the version of %s: %w", git, err)
	}
	if !found.AtLeast(MinVersion) {
		return nil, &ErrTooOld{Path: git, Found: found, Want: MinVersion}
	}
	p.version = found
	p.reuse = "true"
	if found.AtLeast(MultiPackReuse) {
		p.reuse = "multi"
	}
	p.log.Info("code_git_ready", "path", git, "version", found.String(),
		"read_slots", p.read.size, "maintenance_slots", p.maint.size, "transfer_slots", p.transfer.size,
		"pack_reuse", p.reuse, "home", home, "memory", memory.Bytes, "memory_from", memory.From)
	return p, nil
}

// prepareHome makes HOME an empty directory of the pool's mode and resolves
// it. A populated one is REFUSED, never cleared: the path is the engine's,
// but a refusal cannot destroy an operator's files if it ever is not.
func prepareHome(home string) (string, error) {
	if home == "" || !filepath.IsAbs(home) {
		return "", fmt.Errorf("gitproc: Home %q is not an absolute path: it is HOME for every git process this node runs", home)
	}
	if err := os.MkdirAll(home, hostbox.DirMode); err != nil {
		return "", fmt.Errorf("gitproc: making HOME %s: %w", home, err)
	}
	info, err := os.Stat(home)
	if err != nil {
		return "", fmt.Errorf("gitproc: reading HOME %s: %w", home, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: %s is not a directory", ErrHome, home)
	}
	if info.Mode().Perm() != hostbox.DirMode {
		if err = os.Chmod(home, hostbox.DirMode); err != nil {
			return "", fmt.Errorf("gitproc: setting HOME %s to %v: %w", home, hostbox.DirMode, err)
		}
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		return "", fmt.Errorf("gitproc: listing HOME %s: %w", home, err)
	}
	if len(entries) > 0 {
		names := make([]string, 0, 5)
		for _, e := range entries[:min(len(entries), 5)] {
			names = append(names, e.Name())
		}
		return "", fmt.Errorf("%w: %s holds %s: it is HOME for every git process this node runs and must stay empty — remove them",
			ErrHome, home, strings.Join(names, ", "))
	}
	resolved, err := filepath.EvalSymlinks(home)
	if err != nil {
		return "", fmt.Errorf("gitproc: resolving HOME %s: %w", home, err)
	}
	return resolved, nil
}

// Version is the git this pool runs.
func (p *Pool) Version() Version { return p.version }

// PackReuse is the pack.allowPackReuse value this binary accepts: "multi" at
// [MultiPackReuse] or newer, "true" before — chosen once, so the cache's serve
// configuration has one source.
func (p *Pool) PackReuse() string { return p.reuse }

// Slots is the size of the lane class c waits in (an unknown class: 0).
//
// A caller that runs several processes of one class at once runs at most this
// many: more would queue against ITSELF and fail ErrBusy with no other load on
// the node. It is also what the engine logs and reports as the node's
// resolved sizes, so no second package resolves a default.
func (p *Pool) Slots(c Class) int {
	if !c.Valid() {
		return 0
	}
	return int(p.lane(c).size)
}

// Free is how many of class c's slots a run could take right now WITHOUT
// WAITING: those neither held nor — in the READ lane — lent to a push's
// receive-pack (MaxLentCores), and none while a run waits in the lane (an
// unknown class: 0). A moment's reading, never a reservation: a run started
// on it may still find its slot taken by another and wait, as any run does.
//
// It is what a caller with several INDEPENDENT reads to make sizes their
// parallelism by — a push's force check asks one merge-base a branch it moves
// — so that a node with idle cores answers them at once while a loaded one
// asks them one after another, exactly as before, rather than a push taking
// every slot of the lane and queueing the other readers behind itself.
func (p *Pool) Free(c Class) int {
	if !c.Valid() {
		return 0
	}
	return int(p.lane(c).free())
}

// lane is the lane class c waits in; c must be valid.
func (p *Pool) lane(c Class) *lane {
	switch c {
	case ClassTransfer:
		return p.transfer
	case ClassMaintenance:
		return p.maint
	default:
		return p.read
	}
}

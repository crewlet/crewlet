// Package gitproc is THE ONE GIT SUBPROCESS WRAPPER. Every git process the
// engine starts — a clone's upload-pack, a push's receive-pack, the cache's
// multi-pack-index, the receive pipeline's reads — is started by [Pool.Run],
// and nothing else in the module but cmd/'s operator commands may start git
// or one of its git-* programs: TestOnlyGitprocExecsGit holds that, and
// ADR-0008 is why it is a gate.
//
// # Why one wrapper
//
// Gitlet runs real upstream git, not a reimplementation, which gets pack
// negotiation, delta resolution and fsck subtly wrong. Six decisions have to be made the same way for every git
// process — the environment, the configuration, the repository it may trust,
// the slot, the end of its process tree, and what its failure means — and a
// second exec.Command is a place to forget one of them.
//
// # A git process is configured by the engine and by nothing else
//
// The environment is BUILT, never inherited: the engine's own holds the
// company's tokens and the store's keys, and git reads dozens of variables
// (GIT_EXEC_PATH, GIT_CONFIG_PARAMETERS, GIT_SSH_COMMAND…) any one of which
// changes what a process is. HOME is an empty directory the pool
// owns, and neither the system nor the global configuration is read — nor the
// system or global ATTRIBUTES, which the configuration switches do not reach
// (GIT_ATTR_NOSYSTEM, and core.attributesFile at a path that cannot exist): a
// host-wide `* -delta` would leave whole every object whose delta a compaction
// computes, 12.5× the bytes.
//
// Configuration reaches a process only as GIT_CONFIG_COUNT pairs: a fixed
// BASE the pool owns (no transport, no automatic gc or maintenance, one
// thread, no hook unless the call site names a hooks directory, no global
// attributes file), then — for a run that dials a remote alone — the pairs
// the pool writes from its [Cmd.Remote] ("One outbound dial", below), then the
// call site's own pairs, each on an ALLOWLIST rather than a list of forbidden
// keys, for hostbox's reason: git has a dozen keys that run a command
// (alias.*, uploadpack.packObjectsHook, core.fsmonitor, include.path, …) and
// one that serves every object to anyone (uploadpack.allowAnySHA1InWant) — a
// denylist leaks whichever nobody named.
//
// A COMMIT IS ITS CALL'S. Left to its environment git finds a commit's author,
// committer and dates on the node, so the same commit-tree on two nodes would
// be two commits naming a machine. The built environment carries no identity;
// the one a process is given is its call's: [Cmd.Identity], required of a
// commit-tree and refused on every other subcommand, written as the six
// GIT_AUTHOR_* and GIT_COMMITTER_* variables, which Cmd.Env never names, so
// the commit is the same bytes on every node.
//
// A REPOSITORY CANNOT CONFIGURE ITS OWN PROCESS, and the obvious mechanism
// does not achieve it: command-line configuration outranks a repository's
// config file only for the keys it sets, and every other key is read from the
// file. So the pool refuses to run in a directory whose config is not
// [RepoConfig] byte for byte, or that holds a commondir file (which points git
// at ANOTHER repository's objects and refs, and which GIT_COMMON_DIR does not
// override), and it points GIT_GRAFT_FILE and GIT_SHALLOW_FILE, the two files
// that change what history IS, at a path that can never exist.
//
// NOR DOES A REPOSITORY CHOOSE WHERE ITS OBJECTS COME FROM. Its objects
// directory must be its own — at its real path, never a link — and its one
// alternate is the call's ([Cmd.Alternate]: a push's scratch and an
// advertisement view the cache incarnation they lean on, everything else
// none), the alternates file exactly the one line naming it or absent: under
// protocol v2 every object reachable is fetchable by id, so a planted
// alternates file or linked objects directory would serve another
// repository's objects. The guard reaches ONE REPOSITORY FURTHER, because git
// lists an alternate's refs by starting another git in it with every variable
// this pool sets CLEARED, so the alternate must pass the same shape check and
// name no alternate of its own.
//
// THE DROPPING VIEW IS A SERVING PROCESS'S ALONE. A cache incarnation keeps
// the packs an expunge has left dropping (internal/gitlet's PackDropping) in
// an object directory of its own, [DroppingDir], so a client still reading one
// of their objects is served it while no push may rely on one. A process that
// serves the incarnation names it ([Cmd.Dropping]) and reads it as
// GIT_ALTERNATE_OBJECT_DIRECTORIES, never as an alternates file (git follows
// that from every repository leaning on the incarnation); a process leaning on
// an alternate never names it, and runs with
// GIT_COMMIT_GRAPH_PARANOIA=1 when that alternate holds a dropping view, since
// git loads the FIRST commit-graph its object directories hold and a
// connectivity check would pass over a commit only a dropping pack holds. The
// view's directory is the repository's own too ([ShapeDropping]).
//
// The refusal is a *[ShapeError] (errors.Is [ErrRepoShape]); a read that
// FAILED is the read's own error, never a refusal. [CheckShape] is the check,
// exported for the git cache's sweep.
//
// # One outbound dial
//
// A git process reaches another host in exactly one shape: a [Cmd.Remote],
// REQUIRED of a fetch, an ls-remote and a push and refused on every other
// subcommand, which lifts the base's protocol.allow=never for https alone, for
// its run alone. A repository's import and both kinds of mirror take it, and a
// host a person or a company document names is dialed where the engine judged
// it, and nowhere else:
//
//   - THE NAME IS RESOLVED IN GO AND PINNED. git resolves a host itself,
//     through libcurl, after any judgment the engine could make, so a name
//     answering once with a public address and once with an internal one would
//     pass a check made in Go and be dialed at the second answer.
//     [ResolveRemote] holds every address to the engine's public-address rule
//     (internal/httpx; gitlet.import_allow_private lifts it), and the run hands
//     libcurl those addresses as the name's only answer (http.curloptResolve).
//   - THE URL GIT READS IS THE ONE THE RULE JUDGED. ResolveRemote writes the
//     URL back from what it parsed — https; a lowercase name or an address in
//     its one written form; no user, query or fragment — because two parsers
//     reading one URL differently (an @, a backslash, an octal IPv4 address, a
//     trailing dot, punycode) are how a check of one host becomes a dial of
//     another.
//   - ONE HOST, NEVER A SECOND. Nothing the remote answers sends git to a host
//     the pin does not cover: no redirect (http.followRedirects=false), bundle
//     URI (transfer.bundleURI=false), submodule (fetch.recurseSubmodules=false)
//     or packfile URI (fetch.uriProtocols is no key a call site may set). The
//     first argument that is no option must be Remote.URL, and no other
//     argument may name a URL.
//   - A CREDENTIAL IS A HEADER, NEVER AN ARGUMENT. [Remote.WithHeader]'s
//     header reaches git as a GIT_CONFIG_COUNT pair (http.extraHeader) in the
//     process's environment, which only its own user can read, never in its
//     arguments, which every user of the machine can; a URL naming a user is
//     refused, since git copies the URL into every process it starts. The
//     remote may echo the header back, so its value, what a Basic credential
//     encodes and a proxy's password are taken out of every line the run hands
//     back, and stderr and the damage then go through internal/redact.
//   - NOTHING PROMPTS (GIT_TERMINAL_PROMPT=0) and no credential helper is
//     configured, so a remote that asks for a password is refused at once.
//   - A REMOTE PACES ITS RUN as a client paces a transfer, so it waits in the
//     transfer lane, charged to its [Share], under [RemoteDeadline]. No stall
//     watchdog sees its network leg move, so a Cmd.Stall beside a remote is
//     refused; TCP's word on its PEER holds the leg instead: keepalive probes (http.keepAliveIdle,
//     http.keepAliveInterval, http.keepAliveCount) end a connection whose peer
//     stopped answering a minute ([TransferStall]) after the last thing it
//     heard, and a peer that answers is waited on to RemoteDeadline whatever it
//     sends. NEVER libcurl's low-speed check (http.lowSpeedLimit): its least
//     limit is what git's own keepalive moves, so it ended quietly working
//     remotes.
//   - THE PROCESS'S PROXY AND AUTHORITIES, which the built environment would
//     otherwise drop (internal/httpx): HTTPS_PROXY and NO_PROXY become git's
//     http.proxy, which resolves the remote; SSL_CERT_FILE and SSL_CERT_DIR
//     become http.sslCAInfo and http.sslCAPath. TLS is verified, and no key
//     that turns it off is one a call site may set.
//
// # Slots: three lanes, each sized by what it spends
//
// A slot bounds the resource a process spends. A READ (rev-parse, cat-file,
// merge-base, rev-list) and a MAINTENANCE run (multi-pack-index, commit-graph,
// fsck) spend a CORE from start to end: every process is single-threaded
// (pack.threads=1 in the base), so the read lane holds [Options.MaxProcesses]
// slots and parallelism comes from concurrent requests.
//
// A TRANSFER (upload-pack, receive-pack) is paced by its CLIENT: a clone at the
// transfer floor holds its process for hours with the CPU idle, and a push
// holds receive-pack through the object-store upload. Counted as cores, a
// handful of slow clients would refuse every clone on an idle node; so
// transfers have a lane of their own, [Options.MaxTransfers] wide, bounded by
// what each holds in MEMORY: four a core by default, and no more than one for
// every [TransferShare] of the node's memory.
//
// The transfer lane is SHARED BY WHO ASKED (share.go), where the other lanes
// grant in arrival order: every transfer carries [Cmd.Share] — its
// credential's class and principal — and a released slot goes to the class
// furthest below its weighted share of what the lane spent over the last
// [ShareWindow] (a person 4, a machine 2, a coding box 1), within it to the
// principal that spent least, each run charged the greater of its CPU and the
// slot-time it held, so a CI system's storm of clones waits behind every
// credential of its class that asked less. Coding boxes together HOLD at most
// half the lane ([ShareClass.Cap]), so a person's git is never queued behind
// their client-paced clones. The principal that spent most in its class can
// wait out the whole [SlotWait] and be answered [ErrBusy].
//
// The lanes are separate for a second reason no sizing could fix: one process
// waits on others. A push's receive-pack holds its transfer slot while its
// proc-receive hook runs the receive pipeline's reads; in one pool, N
// concurrent pushes each wait for an N+1th slot that never frees. A read waits
// on nothing while it holds a slot, so a transfer waiting on reads can never
// deadlock. The rule every caller keeps: never wait for a slot while holding
// one in the same lane. Maintenance never waits: its lane is half the node's
// cores, rounded up, and it takes a free slot or none.
//
// # Lending a push cores
//
// Every process runs at pack.threads=1, a slot being a core, except in the one
// phase that both threads and has a person waiting: a push's receive-pack
// running index-pack over the pack just pushed. A 37 MB pack indexes in 2.8 s
// on one thread, 1.9 on three and 1.8 on four, so [MaxLentCores] is 3. Never upload-pack:
// pack-objects holds pack.windowMemory PER THREAD, which [TransfersPerProcess]
// counts once. Never a push's advertisement (receive-pack --advertise-refs),
// which runs no index-pack; cmd.go's receivePackOptions tells the two apart.
// Each lent thread may hold up to 96 MiB (core.deltaBaseCacheLimit) more than
// TransfersPerProcess counts.
//
// Sized when the receive-pack starts: the READ lane's free cores LESS ONE for
// its own thread. Lent, never held: a loan holds only its COUNT, never the read
// slots, which would sit idle through the client's upload, close the read lane
// with the CPU idle and queue the push's own hook reads behind it.
//
// Repaid at [Cmd.Indexed]: pack.threads is fixed at process start, so unrepaid
// a loan would cover the whole run. Every push's receive-pack call site closes
// Cmd.Indexed when the proc-receive hook first reaches its relay (receive-pack
// starts the hook only after index-pack exits). A push's receive-pack without
// Indexed is refused on every node, even one with nothing to lend, where a
// forgetful call site's own tests would still pass.
//
// # Every process ends, and so does everything it started
//
// Each process leads its own process group (internal/procgroup), so its
// children — upload-pack's pack-objects, receive-pack's index-pack — end with
// it. The caller's context, the call's deadline and the stall watchdog each
// end a run the same way: SIGTERM to the group, SIGKILL [KillGrace] later,
// each signal only after procgroup's Leader.Current confirms the group is
// still the one the pool started; once the process is reaped the group is
// swept, so nothing a run started is running when Run returns.
//
// The pipes end with the process tree, never on a timer: os/exec's WaitDelay
// would cut off the last pipe-full of a clone whose client is slow.
//
// # What every process cost is counted when it ends
//
// A transfer slot says nothing of the cores it burns, so every run's
// [Result] carries its CPU — the user and system time wait4 reports at the
// reap, summed over the process and every descendant it waited
// for — and is one observation of crewlet.code.git.cpu by class. A run the
// node ENDS (deadline, stall, caller gone) is also read before it is signalled
// (procgroup.GroupCPU), since its leader reaps none of the group and the reap
// alone would count an abandoned clone as almost free; the run counts the
// greater of the two.
//
// # What went wrong is classified from git's own format strings
//
// Every process the pool runs — and everything it starts — writes git's trace2
// event stream to a pipe the pool gives it as fd 3 (GIT_TRACE2_EVENT=3), and
// every report git makes (die, error, warning, BUG) is an `error` event
// carrying its FORMAT: the constant git's source passed, beside the message. The run is classified by the
// formats its events carry, looked up whole (every printf conversion read as
// one, so a platform's PRIuMAX matches) in a table of git's own formats built
// from real damaged repositories and held, per git version, both ways
// (classify_test.go): a file git could not read ([ErrCorrupt], with the [Part]
// it named), an object it could not find ([ErrMissingObject]), a want
// upload-pack does not serve ([ErrNotOurRef] — never a rebuild trigger, since
// an absent want and a cache that lost a pack print the same line), or none of
// those ([ErrFailed]).
//
// NEVER BY THE MESSAGE, AND NEVER BY STDERR, because upload-pack copies the
// client's request text into the messages it dies with, so reading the words
// let any token that may fetch force a cache to be read whole again from the
// object store. The request's text lands only in an event's message; the
// formats a request reaches are constants of git's, none a row. stderr stays
// the human's message (Result.Stderr). A git that writes no stream is refused
// at [New] ([ErrNoTrace]). The one damage git 2.56 names only through a bare
// "%s" — a corrupt packed object — is a [Hint] naming a pack, which decides
// nothing until that pack's bytes are held to its row (the cache's
// ConfirmHint).
//
// WHAT A RUN NAMED IS REPORTED WHETHER OR NOT IT FAILED. git reads around most
// damage to a derived file — it warns, and answers from the packs — and a
// caller that got its answer never looks at the warning; so every run whose
// process started is handed to the pool's one observer ([Pool.Observe]) with
// its Result, and the cache repairs what the run named. [Result.Began] tells a
// report about a file since rewritten from one about the file now held;
// [Cmd.Repairer] names the caller repairing what its own run failed with.
//
// A CRASH IS REPORTED TOO, and is no kind: a pack-objects reusing entries by a
// damaged reverse index dies of SIGSEGV, and no format names the file.
// [Result.Crashed] is that a process of the run's tree died of a signal the
// POOL DID NOT SEND (never SIGPIPE); the cache hashes what it read, and takes
// no damage of a part from it.
//
// A run that names two kinds is BOTH: an [ExitError]'s Kind is the highest,
// and errors.Is reaches every kind the run named ([Trace.Kinds]), so a
// question about one is never answered by another that outranks it.
//
// # Formats left out of the table
//
// Deliberately not rows of formatTable (classify.go): `invalid commit
// position. commit-graph is likely corrupt` (every path a position reaches it
// by is first reported as `invalid parent position %u`, a row); fsck's findings
// (`missing %s %s`, `broken link from`), written to stdout, not reported; a BUG
// only beside a row and the run's crash (`could not find object in pack %s at
// offset %lu in MIDX`); and the formats a healthy repository prints (`object %s is a %s, not a %s`, `unable to parse object:
// %s`, `unable to find all commit-graph files`, `unable to find all multi-pack
// index files`; TestNoRowIsAFormatARequestWrites).
//
// # Measured
//
// A caller whose next question never waits asks cat-file for --buffer. The
// index-pack every push runs, at one thread, indexes at 7.3 MiB/s (0.14 s a
// mebibyte) when every pushed object is already present and is read back and
// compared: the worst case, and the figure [PackWorkPerMiB] is anchored to. A
// grant over 10 000 waiters and 1 000 principals costs 34 µs
// (BenchmarkTransferGrant), so a scan stays under a 100 µs budget.
//
// # Two floors, one pin (ADR-0087)
//
// A Gitlet node runs git 2.55 or newer ([NodeMinVersion], the node floor),
// checked at [New] and nowhere else, since every git a Gitlet node runs is a
// pool's, and refused there as an [*ErrTooOld] naming the floor. A git that
// only TALKS to a Gitlet node — a person's, a CI runner's, the operator CLI's,
// the suite's client — needs 2.40 ([ClientMinVersion], the client floor),
// checked where the engine meets such a git (gittest.Require, and the CLI
// before it runs one): a client's git is a laptop or a CI image the operator
// does not choose. The image
// BUILDS the newest git release from the tarball scripts/git-pin.env pins
// (scripts/git.sh, the recipe the suite's `make git` runs too) rather than
// install Debian's, which is below the node floor, so a git CVE is an engine
// CVE, and what reaches the image is a bump of that pin, which a weekly check
// (.github/workflows/pins.yml) asks for; each release's signature is verified
// before its digest is named, and a git security release is taken by running
// that check by hand. A row or an expectation only a git below the floor could
// meet is deleted. The suite runs that pin; every case about what git
// says or does in a pool runs on the node floor's build too, and one arm
// of Gitlet's suite runs it against the client floor's, so each floor is a
// measured claim.
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
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// log is the package's logger, which a Pool uses when Options.Logger is nil.
var log = logging.Get("gitlet.gitproc")

// Options is how a [Pool] is built: which git, how many processes of each
// kind, and where its HOME is.
type Options struct {
	// Path is the git binary; "" is exec.LookPath("git") on the engine's
	// PATH. It must resolve to an absolute path (a relative PATH entry is
	// refused, exec.ErrDot included).
	Path string

	// MaxProcesses is the size of the READ lane — one core a slot — and
	// twice the MAINTENANCE lane's, rounded up; 0 is runtime.GOMAXPROCS(0);
	// negative is refused. Passed RAW from configuration: its default is
	// resolved only here, and [Pool.Slots] reports the result.
	MaxProcesses int

	// MaxTransfers is the size of the TRANSFER lane (upload-pack,
	// receive-pack); 0 is [TransfersPerProcess] × the resolved
	// MaxProcesses, and no more than one for every [TransferShare] of the
	// node's memory (memory.go); negative is refused. Passed RAW, like
	// MaxProcesses.
	MaxTransfers int

	// Memory is the node's memory in bytes, which bounds the TRANSFER
	// lane's default: 0 (the engine's setting) has New read the node's own,
	// the least of its physical memory and its cgroups' limits ([Pool.Memory],
	// [Pool.ReadMemory]), refusing a node whose memory cannot be read; above
	// zero is held for the pool's life, so a suite sizes a lane independent
	// of its machine; negative is refused.
	Memory int64

	// Home is HOME for every process: absolute; created when absent; its
	// mode is set to hostbox.DirMode; it must be EMPTY.
	Home string

	// Logger is where the pool logs; nil is the package's own
	// (component gitlet.gitproc).
	Logger *slog.Logger

	// Metrics records crewlet.code.git.wait (each admission's wait for its
	// lane), crewlet.code.git.processes (running processes by class),
	// crewlet.code.git.lent_cores (read-lane cores lent to receive-packs now)
	// and crewlet.code.git.cpu (each run's process-tree CPU by class,
	// recorded whole as the run ends); nil records nothing.
	Metrics *metrics.Recorder
}

// TransfersPerProcess is how many transfers the TRANSFER lane admits for
// every slot of the READ lane when Options.MaxTransfers is 0.
//
// FOUR IS A MEMORY BOUND: a transfer spends a core only while it compresses
// or resolves deltas, but holds [TransferMemory] for its life. Four a core is
// 896 MiB a core, inside [CoreMemory] with the store and the broker beside
// it; a receive-pack's lent cores ([MaxLentCores]) add
// core.deltaBaseCacheLimit each, never past the read lane's width. CPU-bound
// transfers share the cores, slower but never refused; a node with less
// memory a core runs one per [TransferShare] (memory.go). A repository of
// millions of objects is not bounded ([TransferBytesPerObject]): no lane knows
// which repository a slot serves. The operator's setting overrides it.
const TransfersPerProcess = 4

// Pool runs git processes. It holds no goroutine and needs no Close: every
// goroutine it starts is started and joined by one Run.
type Pool struct {
	// git is the binary. KEEP ITS NAME: the exec gate (exec_gate_test.go)
	// finds this package's one exec of git by the word git in its first
	// argument.
	git     string
	version Version

	home    string // resolved
	ceiling string // filepath.Dir(home): GIT_CEILING_DIRECTORIES for a run with no repository
	baseEnv []string

	read, maint, transfer *lane

	// memory is the node's memory the lanes were sized by; fixedMemory says
	// Options.Memory gave it, so ReadMemory answers it rather than reading
	// the node.
	memory      Memory
	fixedMemory bool

	// slotWait, killGrace, remoteStall and captureMax are SlotWait,
	// KillGrace, TransferStall (Remote.config derives a remote run's TCP
	// keepalive from it) and CaptureMax, as fields so the suite can scale
	// them (export_test.go).
	slotWait, killGrace, remoteStall time.Duration
	captureMax                       int

	// now, grace and draining are the suite's seams (export_test.go), New
	// setting each to the machine's own: now is the clock of a run's Began
	// and Took; grace arms kill's wait between SIGTERM and SIGKILL, answering
	// the channel that ends it and the Stop that frees it; draining, nil
	// outside a case, is told each stream the drain begins to wait for. A
	// case uses them for what no run's report shows.
	now      func() time.Time
	grace    func(time.Duration) (<-chan time.Time, func() bool)
	draining func(stream string)

	// gaugeMu orders every change a gauge reports — the processes running
	// in each class and the read lane's cores lent — so a later Set never
	// carries an older count.
	gaugeMu sync.Mutex
	running map[Class]int64

	log     *slog.Logger
	metrics *metrics.Recorder

	// observing is the one observer every run reports to (Observe), or nil.
	observing atomic.Pointer[observer]
}

// observer is one Observe call's: its own pointer, so its unobserve removes
// it and never one installed after it.
type observer struct{ fn func(Ran) }

// Ran is one run as the pool reports it to its observer ([Pool.Observe]):
// where its process ran, what it loads, and how it ended. NEVER THE [Cmd]: an
// observer handed one could run it again, and the cache's writers gate holds
// that every Cmd reaching git is spelled by its call (a Cmd handed to a
// function value is a finding there).
type Ran struct {
	// Dir and Alternate are the run's Cmd.Dir and Cmd.Alternate: the
	// directory it ran in, and the one alternate it was handed, if any.
	Dir, Alternate string
	// Loads is the run's Cmd.Loads: the chunk-format files its process
	// loads, which narrow what its damage names (Damage.Narrow).
	Loads Part
	// Repairer is the run's Cmd.Repairer: the caller that repairs what it
	// failed with before its first byte, or zero for none.
	Repairer Repairer
	// Result is how it ended.
	Result Result
}

// Repairer is an opaque name for the caller that repairs what a run fails
// with before its first byte ([Cmd.Repairer]), minted by the pool's observer
// (the code cache names each reader it hands a directory to) and carried to
// it unread. Zero names nobody. It is carried, not guessed from the
// directory, because one directory has many readers at once, each repairing
// its own failure.
type Repairer uint64

// Observe installs fn as the pool's OBSERVER, called with what every run ran
// and how it ended ([Pool.Run] and [Reserved.Run], success or failure) once
// its process has run, and never for a call refused before a process started
// (the call, its shape or the lane's [ErrBusy]). It runs on the run's
// goroutine before the run returns, so it must never block.
//
// ONE OBSERVER, which every run reports to: a call site that had to hand its
// result to the cache would forget, and the processes that read around
// damage (git warns and answers from the packs) are the ones whose callers
// never look. A second Observe while one is installed is [ErrInvalid];
// unobserve removes this call's observer, once, waiting for no run (one past
// its report keeps the observer it read).
func (p *Pool) Observe(fn func(Ran)) (unobserve func(), err error) {
	o := &observer{fn: fn}
	if !p.observing.CompareAndSwap(nil, o) {
		return nil, fmt.Errorf("%w: this git pool reports its runs to one observer, and one is installed", ErrInvalid)
	}
	return sync.OnceFunc(func() { p.observing.CompareAndSwap(o, nil) }), nil
}

// report hands the observer where c ran and how it ended, when a process
// started.
func (p *Pool) report(c Cmd, res Result) {
	if res.Began.IsZero() {
		return
	}
	if o := p.observing.Load(); o != nil {
		o.fn(Ran{Dir: c.Dir, Alternate: c.Alternate, Loads: c.Loads, Repairer: c.Repairer, Result: res})
	}
}

// New builds a pool over the node's git, checks it is at least
// [NodeMinVersion] (an [*ErrTooOld] naming the node floor otherwise) and then
// that it writes the trace2 event stream ([ErrNoTrace] otherwise), and
// resolves the lanes' sizes. Every refusal names what to change.
func New(ctx context.Context, o Options) (*Pool, error) {
	git := o.Path
	if git == "" {
		found, err := exec.LookPath("git")
		if err != nil {
			return nil, fmt.Errorf("gitproc: no git on this node's PATH (%w): a Gitlet node runs the node's own git, %s or newer (gitproc.NodeMinVersion, the node floor); the engine image builds the newest git release", err, NodeMinVersion)
		}
		git = found
	}
	if !filepath.IsAbs(git) {
		return nil, fmt.Errorf("gitproc: the git binary %q is not an absolute path", git)
	}

	if o.MaxProcesses < 0 {
		return nil, fmt.Errorf("gitproc: gitlet.max_processes %d is negative: 0 is one a core", o.MaxProcesses)
	}
	if o.MaxTransfers < 0 {
		return nil, fmt.Errorf("gitproc: gitlet.max_transfers %d is negative: 0 is %d for each process slot", o.MaxTransfers, TransfersPerProcess)
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
		// The table every process starts from; Run adds GIT_DIR or
		// GIT_CEILING_DIRECTORIES, GIT_PROTOCOL, the configuration, the call
		// site's two variables and, for a commit-tree alone, Cmd.Identity. It
		// holds no GIT_AUTHOR_*, GIT_COMMITTER_* or EMAIL, so a commit's
		// names and dates are never the node's.
		baseEnv: []string{
			// git runs its own subcommands from its exec-path; PATH is
			// what a hook's interpreter resolves by.
			"PATH=" + os.Getenv("PATH"),
			// An empty directory the pool owns, never read even if written
			// later: GIT_CONFIG_GLOBAL below and the base's
			// core.attributesFile (cmd.go) point git elsewhere.
			"HOME=" + home,
			// The host's system configuration (`git var
			// GIT_CONFIG_SYSTEM`) is not the engine's.
			"GIT_CONFIG_NOSYSTEM=1",
			// Overrides both ~/.gitconfig and $XDG_CONFIG_HOME/git/config.
			"GIT_CONFIG_GLOBAL=/dev/null",
			// Nor its system attributes (`git var GIT_ATTR_SYSTEM`), which
			// GIT_CONFIG_NOSYSTEM does not reach: a host's `* -delta` would
			// have every repack write its deltas whole (package doc, "A git
			// process is configured by the engine and by nothing else").
			"GIT_ATTR_NOSYSTEM=1",
			// Nothing ever prompts.
			"GIT_TERMINAL_PROMPT=0",
			// A refs/replace/ ref can never change what an object id means.
			"GIT_NO_REPLACE_OBJECTS=1",
			// info/grafts rewrites parents and shallow truncates history; a
			// path that can never exist (ENOTDIR) disables each without the
			// deprecation hint /dev/null prints. Shallow clones still work:
			// upload-pack and receive-pack pass their own shallow files.
			"GIT_GRAFT_FILE=/dev/null/grafts",
			"GIT_SHALLOW_FILE=/dev/null/shallow",
			// The classification table matches git's format strings as its
			// source spells them: under C, gettext answers each with its msgid.
			"LC_ALL=C",
			// THE TRACE2 EVENT STREAM, on fd 3 (ExtraFiles[0], the pipe Run
			// reads: trace.go). Set for EVERY run, never a call site's
			// option: one that forgot it would turn a damaged cache into a
			// silent permanent failure. No file moves or enriches it: trace2
			// reads only the environment and the system and global
			// configuration (both off above), and the built environment
			// never sets GIT_TRACE2_CONFIG_PARAMS or GIT_TRACE2_ENV_VARS.
			"GIT_TRACE2_EVENT=3",
			// No time, file or line on an event: the classification reads
			// none of them, and they are 30% of the bytes.
			"GIT_TRACE2_EVENT_BRIEF=1",
			// Region and data events of the outermost regions only; an
			// error event is no region and is written at any depth.
			"GIT_TRACE2_EVENT_NESTING=1",
		},
		memory:      memory,
		fixedMemory: o.Memory > 0,
		read:        newLane(ClassRead, cpuSlots),
		maint:       newLane(ClassMaintenance, (cpuSlots+1)/2),
		// THE TRANSFER LANE GRANTS BY SHARE (share.go): a held slot is
		// charged the share of a core it stands for on this shape
		// (occupancyRate), and a coding box's class is capped at half
		// the lane (ShareClass.Cap).
		transfer: newShareLane(transferSlots, newShareBook(time.Now, int(transferSlots),
			occupancyRate(cpuSlots, transferSlots), o.Metrics)),
		slotWait:    SlotWait,
		killGrace:   KillGrace,
		remoteStall: TransferStall,
		captureMax:  CaptureMax,
		now:         time.Now,
		grace:       timer,
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
	if !found.AtLeast(NodeMinVersion) {
		return nil, &ErrTooOld{Path: git, Found: found, Floor: FloorNode}
	}
	// A git that writes no event stream classifies no failure, silently
	// answering every damaged cache as git's own failure, so it is refused.
	// Asked AFTER the floor: a git below 2.22 writes none either, and the
	// floor's refusal names what is actually wrong with it.
	if res.Trace.Version == "" {
		return nil, fmt.Errorf("%w: git %s (%s) wrote no trace2 event stream on the descriptor the pool gave it "+
			"(GIT_TRACE2_EVENT=3): Gitlet classifies a damaged cache from it, and every git since 2.22 "+
			"writes it, so this git was built or patched without it — install the engine image's git",
			ErrNoTrace, git, found)
	}
	p.version = found
	p.log.Info("code_git_ready", "path", git, "version", found.String(),
		"read_slots", p.read.size, "maintenance_slots", p.maint.size, "transfer_slots", p.transfer.size,
		"home", home, "memory", memory.Bytes, "memory_from", memory.From,
		"trace_evt", res.Trace.EventVersion)
	return p, nil
}

// prepareHome makes HOME an empty directory of the pool's mode and resolves
// it. A populated one is REFUSED, never cleared: should the path ever not be
// the engine's, a refusal destroys no operator's files.
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

// Slots is the size of the lane class c waits in (an unknown class: 0).
//
// A caller running several processes of one class at once runs at most this
// many: more would queue against ITSELF and fail ErrBusy on an idle node. The
// engine logs and reports it as the resolved sizes, so no second package
// resolves a default.
func (p *Pool) Slots(c Class) int {
	if !c.Valid() {
		return 0
	}
	return int(p.lane(c).size)
}

// Free is how many of class c's slots a run could take now WITHOUT WAITING:
// those neither held nor, in the READ lane, lent to a push's receive-pack
// (MaxLentCores), and none while a run waits in the lane (an unknown class:
// 0). A moment's reading, never a reservation: a run started on it may wait.
//
// A caller with several INDEPENDENT reads (a push's force check: one
// merge-base a moved branch) sizes its parallelism by it, so an idle node
// answers them at once and a loaded one in turn, rather than one push taking
// every slot and queueing the other readers behind it.
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

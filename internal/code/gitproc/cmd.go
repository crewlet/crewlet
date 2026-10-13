package gitproc

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/textcut"
)

// Class is which lane a process waits in and the attribute it is counted
// under. A closed set; an unknown one is refused, never defaulted.
type Class string

const (
	// ClassRead is a bounded local read that waits on nothing while it
	// holds its slot — rev-parse, cat-file, merge-base, show-index,
	// rev-list, for-each-ref, `git version` — and the one bounded local
	// WRITE a waiting request needs: a full cache round's multi-pack-index
	// without a bitmap, CPU work a request waits on and so never left to the
	// maintenance lane's refusal. The READ lane: a core a slot.
	ClassRead Class = "read"

	// ClassTransfer is upload-pack and receive-pack, and what they start.
	// The TRANSFER lane, sized by memory rather than cores, because a client
	// paces it; a receive-pack's hook runs reads, which is why reads have a
	// lane of their own.
	ClassTransfer Class = "transfer"

	// ClassMaintenance is multi-pack-index, commit-graph and fsck — local
	// work no client waits on. The MAINTENANCE lane: half the cores, rounded
	// up, and a run takes a free slot or is refused, never waiting.
	ClassMaintenance Class = "maintenance"
)

// Classes is every class, in the order a message lists them. There is no
// install class: a pack arrives with the index and reverse index the
// receiving git wrote, and a cache installs it with renames and no git.
var Classes = []Class{ClassRead, ClassTransfer, ClassMaintenance}

// Valid reports whether c is one of [Classes].
func (c Class) Valid() bool { return slices.Contains(Classes, c) }

// KV is one configuration pair or one environment variable a call site adds.
type KV struct{ Key, Value string }

// RepoConfig is the whole of the config file of every repository the engine
// runs git in — a cache, a push's scratch, an advertisement view. The code
// that creates one writes this constant, never a copy of its text, and Run
// refuses a directory whose config is anything else ([ErrRepoShape]).
const RepoConfig = "[core]\n\trepositoryformatversion = 0\n\tbare = true\n"

// The two variables a call site may add (Cmd.Env): where receive-pack's hook
// reaches the engine's relay, and the token it presents there.
const (
	EnvHookSocket = "CREWLET_HOOK_SOCKET"
	EnvHookToken  = "CREWLET_HOOK_TOKEN"
)

// Cmd is one git process, described entirely by the call site: nothing about
// it comes from the environment, the HOME, or a configuration file.
type Cmd struct {
	// Dir is the repository: absolute, filepath.Clean'ed, holding
	// RepoConfig and no commondir, its alternates (if any) naming only
	// repositories of the same shape. It is GIT_DIR and the working
	// directory. "" runs with NO repository: the working directory is the
	// pool's HOME and GIT_CEILING_DIRECTORIES is its parent, so git
	// discovers none.
	Dir string

	// Args follow "git". Args[0] is a SUBCOMMAND (^[a-z][a-z0-9-]*$): a
	// global option there (-c, --exec-path, --git-dir, --config-env) would
	// change what the process is. The engine passes no argument a client
	// wrote except validated object ids and refs — never a path or an
	// option; that rule is the call site's, since a path the engine built
	// itself is an ordinary argument. A receive-pack's options are the
	// pool's to read, so each is one of receivePackOptions, in full.
	Args []string

	// Config is the call site's configuration, after the pool's base. Each
	// key must be on the allowlist.
	Config []KV

	// Hooks is the hooks directory (absolute, an existing directory); ""
	// runs no hook at all.
	Hooks string

	// Env adds EnvHookSocket and/or EnvHookToken; any other name is ErrEnv.
	Env []KV

	// Protocol is GIT_PROTOCOL as the client sent it. One not matching
	// ^version=[0-9]+(:[a-z0-9=.-]+)*$ is DROPPED, and the run is then v0,
	// which every client speaks. "" sets none.
	Protocol string

	// Stdin is read to its end (or the process's) by a copy Run waits for.
	// CONTRACT: every Read returns within a bound of its own — a
	// bytes.Reader, a file, a paced request body — because Run returns only
	// after the copy does, and no goroutine Run starts outlives it.
	Stdin io.Reader

	// Stdout receives everything git writes — every byte, however long after
	// the process ended the caller takes it; nil captures up to CaptureMax
	// into Result.Stdout. Same contract as Stdin: every Write returns within
	// a bound of its own, and a Write error stops git (the pipe is closed
	// under it).
	Stdout io.Writer

	// Class picks the lane.
	Class Class

	// Deadline bounds the run from start to reap; 0 or negative is
	// ErrNoDeadline. Pick one from deadlines.go.
	Deadline time.Duration

	// Stall ends a run in which no byte of Stdin was consumed and no byte of
	// stdout or stderr produced for this long; 0 is not watched; negative is
	// ErrInvalid. Stderr counts: a receive-pack serving a client that did
	// not ask for side-band relays its hook's output — the engine's
	// keepalives among it — on its own stderr, and a process writing
	// anything has not stopped.
	Stall time.Duration

	// Answers are exit statuses that are the command's ANSWER rather than
	// its failure: merge-base --is-ancestor answers 1 for "not an ancestor",
	// check-ref-format 1 for "not a valid name". 0 always is. Each 1..255.
	Answers []int

	// Indexed is a push's receive-pack's, closed by the call site once the
	// receive-pack's index-pack has finished: the cores it was lent
	// (MaxLentCores) are repaid then rather than when it exits, so the hook,
	// the object-store upload and the wait for the push's record hold no
	// loan. The host closes it when the proc-receive hook first reaches its
	// relay, which receive-pack starts only once its index-pack has exited;
	// a push that never gets there (its pack refused) repays at its exit.
	// REQUIRED on a receive-pack that receives a pack — in the transfer
	// lane, and not a push's ADVERTISEMENT (--advertise-refs), which writes
	// the refs and exits with no index-pack and no hook — and ErrInvalid on
	// every other command, the advertisement included, since nothing else
	// is lent a core: a call site that left it nil would hold every push's
	// loan to the end, and nothing but the lent-cores gauge would ever say
	// so.
	Indexed <-chan struct{}
}

// Result is what a run did, returned beside every error once the process
// started.
type Result struct {
	Stdout []byte        // the capture, when Cmd.Stdout was nil
	Stderr string        // the last StderrTail bytes, whatever the outcome
	Exit   int           // the exit status; -1 when a signal ended the process
	Took   time.Duration // start to reap
	Waited time.Duration // in the lane, before start
}

// THE BASE CONFIGURATION, set first for every process, in this order. A call
// site may not set any of these keys: a rule every call site has to remember
// is the rule ADR-0008 says gets one implementation.
//
//   - protocol.allow=never: no engine git dials anything; alternates are not
//     a transport.
//   - gc.auto=0, maintenance.auto=false: only the cache's sync and its
//     maintainer change a cache, and an automatic gc or maintenance run
//     would detach a background process the sweep then kills.
//   - pack.threads: one, so no process uses more than one core — a READ or
//     MAINTENANCE slot IS one, and a CPU-bound transfer shares one. The one
//     exception is a receive-pack lent free read cores (lane.go), whose
//     index-pack is given a thread for each.
//   - core.hooksPath: Cmd.Hooks, or a path that cannot exist, so a
//     repository's own hooks/ never runs.
//   - core.attributesFile: a path that cannot exist (ENOTDIR, which git
//     reads as an absent file without a warning), because git otherwise
//     reads $HOME/.config/git/attributes — GIT_CONFIG_GLOBAL does not move
//     it — and a file written into the pool's HOME after New checked it was
//     empty would steer every process's attributes. Its system twin,
//     /etc/gitattributes, is the environment's GIT_ATTR_NOSYSTEM (gitproc.go).
const (
	keyThreads   = "pack.threads"
	keyHooksPath = "core.hooksPath"
	noHooks      = "/dev/null/no-hooks"
	noAttributes = "/dev/null/no-attributes"
)

// baseKeys is the base, in order; pack.threads' and core.hooksPath's values
// are per run.
var baseKeys = []KV{
	{"protocol.allow", "never"},
	{"gc.auto", "0"},
	{"maintenance.auto", "false"},
	{keyThreads, "1"},
	{keyHooksPath, noHooks},
	{"core.attributesFile", noAttributes},
}

// allowedConfig is every key a call site may set, keyed by its lowercase form
// (git's keys are case-insensitive), each with the call site that needs it.
//
// AN ALLOWLIST, NOT A DENYLIST, for hostbox's reason: git has a dozen keys that
// run a command (alias.*, uploadpack.packObjectsHook, core.fsmonitor,
// core.alternateRefsCommand, core.sshCommand, include.path, …) and one that
// serves every object in a repository to anyone who asks
// (uploadpack.allowAnySHA1InWant, which the code host never sets, with its
// two siblings) — a denylist leaks whichever nobody named. A key a call site
// needs is added here in the commit that adds the call site.
var allowedConfig = map[string]string{
	"core.multipackindex":       "the cache's serve configuration and its maintainer",
	"pack.allowpackreuse":       "the cache's serve configuration, valued Pool.PackReuse",
	"pack.windowmemory":         "the cache's serve configuration",
	"pack.usebitmaps":           "the cache's serve configuration",
	"uploadpack.allowfilter":    "the cache's serve configuration",
	"uploadpack.allowrefinwant": "the cache's serve configuration",
	"uploadpack.keepalive":      "the cache's serve configuration: the stall watchdog relies on it",
	// A prefix filter on the refs git lists of an alternate, passed to the
	// for-each-ref it runs as patterns: it names refs and runs nothing. Its
	// sibling core.alternateRefsCommand runs a command, and is not here.
	"core.alternaterefsprefixes":   "the cache's advertisement view (Cache.AdvertiseConfig)",
	"receive.procreceiverefs":      "the receive configuration",
	"receive.unpacklimit":          "the receive configuration",
	"receive.fsckobjects":          "the receive configuration",
	"receive.maxinputsize":         "the receive configuration",
	"receive.advertisepushoptions": "the receive configuration",
	"receive.autogc":               "the receive configuration",
	"receive.updateserverinfo":     "the receive configuration",
	// The .rev a stored bundle carries is written by receive-pack's own
	// index-pack, never by a second process; pinned rather than left to
	// git's default, which changed in 2.41.
	"pack.writereverseindex": "the receive configuration",
	// The index a stored bundle carries is version 2, which the bundle check
	// requires; pinned so a future default cannot store one no cache accepts.
	"pack.indexversion": "the receive configuration",
}

// subcommand is what Args[0] must be.
var subcommand = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// receivePackOptions is every option a receive-pack may be given, by the one
// spelling the pool reads, each with whether it makes the run a push's
// ADVERTISEMENT — the refs a push's GET is answered with — rather than the
// receive-pack of its POST, which receives a pack.
//
// The pool has to know which of the two a receive-pack is, EXACTLY: only the
// one that receives a pack runs an index-pack, so only it is lent cores and
// must carry Cmd.Indexed, and an advertisement taken for a push would hold a
// loan of three cores for as long as its client takes to read the refs while
// carrying an Indexed nothing ever closes (lane.go). A rule that looked for
// one spelling would guess, because git reads more than one: any unambiguous
// ABBREVIATION (--adv, --http — whose meaning moves the day git adds an option
// sharing the prefix), a NEGATION (--no-advertise-refs, which makes the run a
// push again), "--", after which an option is a directory, and an option after
// the directory as well as before it (all measured on git 2.43). So every
// argument of a receive-pack that begins with a dash must be a key here,
// spelled in full, and one is read wherever it stands, as git reads it. Both
// full names of the advertisement option are read: --advertise-refs is git's
// alias of http-backend-info-refs, the name git's own http-backend passes.
var receivePackOptions = map[string]bool{
	"--stateless-rpc":          false,
	"--advertise-refs":         true,
	"--http-backend-info-refs": true,
}

// checkReceivePack holds a receive-pack's options to receivePackOptions.
func checkReceivePack(args []string) error {
	if args[0] != "receive-pack" {
		return nil
	}
	for _, a := range args[1:] {
		if _, ok := receivePackOptions[a]; strings.HasPrefix(a, "-") && !ok {
			return fmt.Errorf("%w: git receive-pack is given %q, which is not an option the pool reads: a receive-pack takes only "+
				"--stateless-rpc and --advertise-refs (or its other name, --http-backend-info-refs), spelled in full, beside its directory, "+
				"because the pool must know exactly whether it receives a pack — git also reads abbreviations, negations and --",
				ErrInvalid, textcut.Ellipsis(a, 64))
		}
	}
	return nil
}

// advertises reports whether a receive-pack's arguments, held to
// receivePackOptions, make it a push's advertisement.
func advertises(args []string) bool {
	return slices.ContainsFunc(args[1:], func(a string) bool { return receivePackOptions[a] })
}

// protocolValue is a GIT_PROTOCOL a client may send: version=2, and the
// colon-separated key=value extensions git defines after it.
var protocolValue = regexp.MustCompile(`^version=[0-9]+(:[a-z0-9=.-]+)*$`)

// prepared is a Cmd checked and turned into a process's environment.
type prepared struct {
	env []string
	wd  string
	// threads is where in env pack.threads' value sits, which Run rewrites
	// for a receive-pack lent cores once it is admitted (lane.go).
	threads int
}

// prepare runs every check on c, in order, before any slot is taken: a Cmd
// refused here started nothing and waited for nothing.
func (p *Pool) prepare(c Cmd) (prepared, error) {
	if !c.Class.Valid() {
		return prepared{}, fmt.Errorf("%w: class %q is not one of read, transfer, maintenance", ErrInvalid, c.Class)
	}
	if len(c.Args) == 0 {
		return prepared{}, fmt.Errorf("%w: no subcommand: Cmd.Args[0] names what git runs", ErrInvalid)
	}
	if !subcommand.MatchString(c.Args[0]) {
		return prepared{}, fmt.Errorf("%w: %q is not a git subcommand: Cmd.Args[0] must be one, since a global option there would change what the process is",
			ErrInvalid, textcut.Ellipsis(c.Args[0], 64))
	}
	for _, a := range c.Args {
		if strings.IndexByte(a, 0) >= 0 {
			return prepared{}, fmt.Errorf("%w: git %s has an argument holding a NUL byte", ErrInvalid, c.Args[0])
		}
	}
	// Before the Indexed checks, which read a receive-pack's options.
	if err := checkReceivePack(c.Args); err != nil {
		return prepared{}, err
	}
	if c.Deadline <= 0 {
		return prepared{}, fmt.Errorf("%w: git %s has none: pick one from gitproc's deadlines.go", ErrNoDeadline, c.Args[0])
	}
	if c.Stall < 0 {
		return prepared{}, fmt.Errorf("%w: git %s has a negative Stall: 0 is not watched", ErrInvalid, c.Args[0])
	}
	for _, a := range c.Answers {
		if a < 1 || a > 255 {
			return prepared{}, fmt.Errorf("%w: git %s lists %d among its answers: an answer is an exit status from 1 to 255 (0 always is one)", ErrInvalid, c.Args[0], a)
		}
	}
	if c.Indexed != nil && !lendsCores(c) {
		what := "git " + c.Args[0] + " in the " + string(c.Class) + " lane"
		if c.Args[0] == "receive-pack" && advertises(c.Args) {
			what = "a push's advertisement (git receive-pack --advertise-refs), which runs no index-pack,"
		}
		return prepared{}, fmt.Errorf("%w: %s carries Cmd.Indexed, which belongs only to a receive-pack in the transfer lane that receives a pack: "+
			"nothing else is lent a core to repay", ErrInvalid, what)
	}
	// Refused whatever the node: a call site checked only on a node with no
	// core to lend would pass, and hold every loan to the end everywhere
	// else (lane.go).
	if c.Indexed == nil && lendsCores(c) {
		return prepared{}, fmt.Errorf("%w: git receive-pack carries no Cmd.Indexed: a receive-pack that receives a pack is lent free read cores for its index-pack, "+
			"and without it the loan is held through the hook, the object-store upload and the wait for the push's record — "+
			"close it once the index-pack is done, when the proc-receive hook first reaches its relay", ErrInvalid)
	}
	hooks := noHooks
	if c.Hooks != "" {
		if !filepath.IsAbs(c.Hooks) || filepath.Clean(c.Hooks) != c.Hooks {
			return prepared{}, fmt.Errorf("%w: Cmd.Hooks %q is not an absolute, clean path", ErrInvalid, c.Hooks)
		}
		info, err := os.Stat(c.Hooks)
		if err != nil || !info.IsDir() {
			return prepared{}, fmt.Errorf("%w: Cmd.Hooks %q is not a directory", ErrInvalid, c.Hooks)
		}
		hooks = c.Hooks
	}
	if c.Dir != "" {
		if !filepath.IsAbs(c.Dir) || filepath.Clean(c.Dir) != c.Dir {
			return prepared{}, fmt.Errorf("%w: Cmd.Dir %q is not an absolute, clean path", ErrInvalid, c.Dir)
		}
		if err := checkRepo(c.Dir); err != nil {
			return prepared{}, err
		}
		if err := checkAlternates(c.Dir, false); err != nil {
			return prepared{}, err
		}
	}
	if err := checkConfig(c.Config); err != nil {
		return prepared{}, err
	}
	if err := checkEnv(c.Env); err != nil {
		return prepared{}, err
	}

	env := make([]string, 0, len(p.baseEnv)+3+2*(len(baseKeys)+len(c.Config))+len(c.Env))
	env = append(env, p.baseEnv...)
	wd := c.Dir
	if c.Dir != "" {
		env = append(env, "GIT_DIR="+c.Dir)
	} else {
		env = append(env, "GIT_CEILING_DIRECTORIES="+p.ceiling)
		wd = p.home
	}
	if c.Protocol != "" {
		if protocolValue.MatchString(c.Protocol) {
			env = append(env, "GIT_PROTOCOL="+c.Protocol)
		} else {
			p.log.Debug("code_git_protocol_dropped", "protocol", textcut.Ellipsis(c.Protocol, 64))
		}
	}
	env = append(env, "GIT_CONFIG_COUNT="+strconv.Itoa(len(baseKeys)+len(c.Config)))
	threads := -1
	for i, kv := range baseKeys {
		value := kv.Value
		switch kv.Key {
		case keyHooksPath:
			value = hooks
		case keyThreads:
			threads = len(env) + 1 // the value, after its key
		}
		env = append(env, configKey(i, kv.Key), configValue(i, value))
	}
	for i, kv := range c.Config {
		env = append(env, configKey(len(baseKeys)+i, kv.Key), configValue(len(baseKeys)+i, kv.Value))
	}
	for _, kv := range c.Env {
		env = append(env, kv.Key+"="+kv.Value)
	}
	return prepared{env: env, wd: wd, threads: threads}, nil
}

func configKey(i int, key string) string {
	return "GIT_CONFIG_KEY_" + strconv.Itoa(i) + "=" + key
}

func configValue(i int, value string) string {
	return "GIT_CONFIG_VALUE_" + strconv.Itoa(i) + "=" + value
}

// checkConfig holds a call site's configuration to the allowlist.
func checkConfig(config []KV) error {
	seen := make(map[string]bool, len(config))
	for _, kv := range config {
		key := strings.ToLower(kv.Key)
		for _, base := range baseKeys {
			if key == strings.ToLower(base.Key) {
				if base.Key == keyHooksPath {
					return fmt.Errorf("%w: %s is the pool's: name the directory in Cmd.Hooks", ErrConfig, kv.Key)
				}
				return fmt.Errorf("%w: %s is the pool's: the pool sets it for every process", ErrConfig, kv.Key)
			}
		}
		if _, ok := allowedConfig[key]; !ok {
			return fmt.Errorf("%w: %s: add it to gitproc's allowlist with the call site that needs it", ErrConfig, textcut.Ellipsis(kv.Key, 120))
		}
		if seen[key] {
			return fmt.Errorf("%w: %s is set twice: one call site, one value", ErrConfig, kv.Key)
		}
		seen[key] = true
		if strings.IndexByte(kv.Value, 0) >= 0 {
			return fmt.Errorf("%w: %s's value holds a NUL byte", ErrConfig, kv.Key)
		}
	}
	return nil
}

// checkEnv holds a call site's variables to the two the pool admits.
func checkEnv(env []KV) error {
	seen := map[string]bool{}
	for _, kv := range env {
		if kv.Key != EnvHookSocket && kv.Key != EnvHookToken {
			return fmt.Errorf("%w: %s: a call site may add only %s and %s, because every other variable a git process sees is the pool's",
				ErrEnv, textcut.Ellipsis(kv.Key, 120), EnvHookSocket, EnvHookToken)
		}
		if seen[kv.Key] {
			return fmt.Errorf("%w: %s is set twice", ErrEnv, kv.Key)
		}
		seen[kv.Key] = true
		if strings.IndexByte(kv.Value, 0) >= 0 {
			return fmt.Errorf("%w: %s's value holds a NUL byte", ErrEnv, kv.Key)
		}
	}
	return nil
}

// A REPOSITORY CANNOT CONFIGURE ITS OWN PROCESS — and overriding dangerous
// keys on the command line does not achieve it.
//
// Command-line configuration (GIT_CONFIG_COUNT) outranks a repository's config
// file only for the keys it sets; every other key is read from the file, so a
// repository config naming uploadpack.allowAnySHA1InWant is honoured by every
// git that does not set it, and an include.path there reads any file at all. A
// planted commondir moves objects AND refs to another repository and has no
// override (GIT_COMMON_DIR does not beat it). The engine writes every
// repository it runs git in, by hand, so a check of their SHAPE costs nothing
// legitimate and turns "a repository can configure git" from a hope into a
// refusal.
//
// Not checked, deliberately: info/attributes and info/exclude (a
// repository's own attributes — that file, and in a bare repository its
// HEAD's .gitattributes, which git 2.43 reads too — steer only pack-objects'
// delta search for that one repository, a cost and never an answer; exclude
// is a work-tree file). The HOST's and the HOME's attributes are another
// matter and are never read (GIT_ATTR_NOSYSTEM, the base's
// core.attributesFile): written by whoever administers the machine, for
// something else, they would apply to every repository the node serves.
// Also not checked: hooks/ (never run: core.hooksPath is the pool's), the
// refs and packed-refs (content the engine wrote, not configuration), and the
// commit-graph and multi-pack-index (written by git itself under the
// maintainer; a damaged one is classified). info/grafts and shallow are
// neutralised by the environment rather than checked (GIT_GRAFT_FILE,
// GIT_SHALLOW_FILE).

// checkRepo holds dir to the shape of a repository the engine wrote.
//
// ErrRepoShape is a DEFINITE answer and only that — the config absent or
// holding other bytes, a commondir present. A read that FAILED (descriptors
// exhausted on a busy node, an I/O error, a directory where the file should
// be) says nothing about the shape, so it is returned as the read's own
// error, wrapped and without ErrRepoShape: collapsing "could not be read"
// into "definitely wrong" would tell a caller a healthy cache was tampered
// with on a transient failure, and the operator reading it the same.
func checkRepo(dir string) error {
	config, err := os.ReadFile(filepath.Join(dir, "config"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%w: %s holds no config: the engine writes one into every repository it runs git in", ErrRepoShape, dir)
	case err != nil:
		return fmt.Errorf("gitproc: reading %s's config: %w", dir, err)
	case string(config) != RepoConfig:
		return fmt.Errorf("%w: %s's config is not the one the engine writes: a repository's own config is read for every key the command line does not set",
			ErrRepoShape, dir)
	}
	_, err = os.Lstat(filepath.Join(dir, "commondir"))
	switch {
	case err == nil:
		return fmt.Errorf("%w: %s holds a commondir, which would point git at another repository's objects and refs", ErrRepoShape, dir)
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("gitproc: reading whether %s holds a commondir: %w", dir, err)
	}
	return nil
}

// checkAlternates holds every repository dir's alternates file names to the
// same shape — and refuses any alternate at all when nested.
//
// THE GUARD REACHES ONE REPOSITORY FURTHER THAN THE PROCESS'S OWN, because git
// does: a repository with an alternate lists that alternate's refs by running
// `git --git-dir=<alternate> for-each-ref` with every variable this pool sets
// for a repository CLEARED (git's local_repo_env: GIT_CONFIG_COUNT,
// GIT_GRAFT_FILE and GIT_SHALLOW_FILE among them). That process is configured
// by the alternate's own files and nothing else. Today it runs no command and
// reads no history — but the guard holds the SHAPE rather than a list of what
// today's subprocess happens to read, for the reason the configuration is an
// allowlist. A chained alternate is refused because its second hop would be
// read by the same unconfigured process, and the engine never builds one.
//
// THE REPOSITORY CHECKED IS THE ONE GIT RUNS IN, which is the alternate's
// REAL path: git resolves each line (strbuf_realpath in
// refs_from_alternate_cb), strips "/objects" and runs for-each-ref there. A
// line whose objects directory is a link into another repository is
// therefore configured by THAT repository's files, and checking the directory
// the line merely passes through would accept a config git never reads while
// git reads one nobody checked. So each line is resolved first, and what it
// resolves to must itself be an objects directory.
//
// As in checkRepo, ErrRepoShape is a definite answer, and a read that failed
// is the read's own error.
func checkAlternates(dir string, nested bool) error {
	body, err := os.ReadFile(filepath.Join(dir, "objects", "info", "alternates"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("gitproc: reading %s's alternates: %w", dir, err)
	}
	if nested {
		return fmt.Errorf("%w: %s names an alternate and is itself one: the engine never chains alternates, and git would read the second hop's files with no configuration of the pool's",
			ErrRepoShape, dir)
	}
	if len(body) == 0 || body[len(body)-1] != '\n' {
		return fmt.Errorf("%w: %s's alternates is not a list of whole lines: the engine writes one line, ending in a newline", ErrRepoShape, dir)
	}
	for _, line := range bytes.Split(body[:len(body)-1], []byte{'\n'}) {
		alt := string(line)
		// An absolute path also rules out the two first bytes git gives a
		// meaning of its own, '#' (a comment) and '"' (a quoted path): the
		// engine writes neither.
		if !filepath.IsAbs(alt) || filepath.Clean(alt) != alt || filepath.Base(alt) != "objects" {
			return fmt.Errorf("%w: %s's alternates names %q: each line must be the absolute, clean path of a repository's objects directory",
				ErrRepoShape, dir, textcut.Ellipsis(alt, 120))
		}
		resolved, err := filepath.EvalSymlinks(alt)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("%w: %s's alternates names %s, which does not exist: %w", ErrRepoShape, dir, alt, err)
		case err != nil:
			return fmt.Errorf("gitproc: resolving %s's alternate %s: %w", dir, alt, err)
		case filepath.Base(resolved) != "objects":
			return fmt.Errorf("%w: %s's alternate %s resolves to %s, which is no repository's objects directory", ErrRepoShape, dir, alt, resolved)
		}
		repo := filepath.Dir(resolved)
		if err := checkRepo(repo); err != nil {
			return err
		}
		if err := checkAlternates(repo, true); err != nil {
			return err
		}
	}
	return nil
}

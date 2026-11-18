package gitproc

import (
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

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/textcut"
	"github.com/crewlet/crewlet/internal/words"
)

// Class is which lane a process waits in and the attribute it is counted
// under. A closed set; an unknown one is refused, never defaulted.
type Class string

const (
	// ClassRead is a bounded local read that waits on nothing while it
	// holds its slot (rev-parse, cat-file, merge-base, rev-list, …), and the
	// one bounded local WRITE a waiting request needs: a cache round's
	// multi-pack-index without a bitmap, never left to the maintenance lane's
	// refusal. The READ lane: a core a slot.
	ClassRead Class = "read"

	// ClassTransfer is upload-pack and receive-pack, and what they start.
	// The TRANSFER lane, sized by memory rather than cores because a client
	// paces it; a receive-pack's hook runs reads, so reads have their own lane.
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

// DroppingDir is the name, in a cache incarnation's root, of the OBJECT
// DIRECTORY its dropping packs live in ([Cmd.Dropping]): beside objects/, never
// under it, so a process reading objects/ through an alternate reaches none of
// it, and git, which lists an alternate's refs in its path less "/objects",
// starts no process at it.
const DroppingDir = "objects-dropping"

// The two variables a call site may add (Cmd.Env): where receive-pack's hook
// reaches the engine's relay, and the token it presents there.
const (
	EnvHookSocket = "CREWLET_HOOK_SOCKET"
	EnvHookToken  = "CREWLET_HOOK_TOKEN"
)

// commitTree is the one subcommand a run may give an [Identity] to, and the
// one that must have one.
const commitTree = "commit-tree"

// identityVars is every variable git reads a commit's identity from, in the
// order the pool writes them from a [Cmd.Identity]: no call site spells one
// (checkEnv), and the built environment holds none of them otherwise.
var identityVars = []string{
	"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_AUTHOR_DATE",
	"GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "GIT_COMMITTER_DATE",
}

// Identity is a commit's author and committer and their dates, for
// commit-tree alone ([Cmd.Identity]): a git left to find one takes it from the
// node (package doc, "A git process is configured by the engine and by nothing
// else"). Each part must be one git writes as given, or the run is refused
// before it starts ([ErrInvalid]): git cuts line feeds, < and >, trims
// ident.c's crud() from either end, and rewrites as Latin-1 what is not UTF-8
// by its reading, which also excludes Unicode's 66 noncharacters (commit.c's
// ensure_utf8). The pool also refuses a NUL and an empty part (git writes an
// empty address as `<>`). Held against git by
// TestThePoolAdmitsAnIdentityPartExactlyWhereGitWritesItAsGiven. git holds a
// message to the same UTF-8 rule and the pool hands Stdin over unread, so a
// call site writing one holds it to gitref.CommitText.
type Identity struct {
	AuthorName, AuthorEmail       string
	CommitterName, CommitterEmail string
	AuthorWhen                    time.Time // GIT_AUTHOR_DATE, written "@<unix seconds> <its zone>"
	CommitterWhen                 time.Time // GIT_COMMITTER_DATE, the same form; never before AuthorWhen
}

// env is id as the six variables git reads it from, in identityVars' order.
func (id *Identity) env() []string {
	values := []string{
		id.AuthorName, id.AuthorEmail, gitDate(id.AuthorWhen),
		id.CommitterName, id.CommitterEmail, gitDate(id.CommitterWhen),
	}
	out := make([]string, len(identityVars))
	for i, name := range identityVars {
		out[i] = name + "=" + values[i]
	}
	return out
}

// gitDate is t as git writes it into a commit: `@<unix seconds>` (without the
// `@`, git reads eight digits or fewer as a calendar date) at t's own offset,
// in `-0700`'s form, so a rebased commit keeps its author's zone (U43 a).
func gitDate(t time.Time) string {
	return "@" + strconv.FormatInt(t.Unix(), 10) + " " + t.Format("-0700")
}

// checkIdentity holds a run's identity to its subcommand (commit-tree's alone,
// and required there) and every part to what git writes as given ([Identity]).
// Each date is whole seconds at or after the epoch, as gitDate writes it, a
// fraction refused rather than dropped; CommitterWhen is never before
// AuthorWhen (an engine-built commit's committer date is held to its parent's,
// its author date to when its principal acted).
func checkIdentity(sub string, id *Identity) error {
	switch {
	case sub != commitTree && id != nil:
		return fmt.Errorf("%w: git %s carries a Cmd.Identity: an identity is given to commit-tree alone, and "+
			"every other process runs with none", ErrInvalid, sub)
	case sub == commitTree && id == nil:
		return fmt.Errorf("%w: git commit-tree carries no Cmd.Identity: commit-tree takes the identity its call "+
			"names, because a git left to find one takes a name and an address from the node it runs on, and the "+
			"time from its clock, or refuses when it finds none", ErrInvalid)
	case id == nil:
		return nil
	}
	for _, part := range []struct{ field, value string }{
		{"AuthorName", id.AuthorName}, {"AuthorEmail", id.AuthorEmail},
		{"CommitterName", id.CommitterName}, {"CommitterEmail", id.CommitterEmail},
	} {
		if why := identityPart(part.value); why != "" {
			return fmt.Errorf("%w: Cmd.Identity.%s %q %s", ErrInvalid, part.field, textcut.Ellipsis(part.value, 64), why)
		}
	}
	for _, date := range []struct {
		field string
		when  time.Time
	}{{"AuthorWhen", id.AuthorWhen}, {"CommitterWhen", id.CommitterWhen}} {
		switch {
		case date.when.IsZero():
			return fmt.Errorf("%w: Cmd.Identity.%s is unset: a commit carries the second its call states", ErrInvalid, date.field)
		case date.when.Unix() < 0:
			return fmt.Errorf("%w: Cmd.Identity.%s %s is before the epoch: git writes a commit's dates as the seconds since it",
				ErrInvalid, date.field, date.when.UTC().Format(time.RFC3339))
		case date.when.Nanosecond() != 0:
			return fmt.Errorf("%w: Cmd.Identity.%s %s holds a fraction of a second, which git cannot write: "+
				"truncate it to the second it means (time.Truncate(time.Second))",
				ErrInvalid, date.field, date.when.UTC().Format(time.RFC3339Nano))
		}
	}
	if id.CommitterWhen.Before(id.AuthorWhen) {
		return fmt.Errorf("%w: Cmd.Identity.CommitterWhen %s is before its AuthorWhen %s: a commit is committed at "+
			"or after it is authored", ErrInvalid, id.CommitterWhen.UTC().Format(time.RFC3339),
			id.AuthorWhen.UTC().Format(time.RFC3339))
	}
	return nil
}

// IdentityRefusal is why git would not write s, a name or address of a
// commit's author or committer, as given, or why the pool refuses it (empty, a
// NUL), and "" when neither: [Identity]'s rules. Exported for whoever forms an
// identity from text it did not write: Gitlet asks it of a seat's role name and
// address from the company document, so it can sign with a part git writes as
// given rather than have the build refused after its work, from this one
// reading of git's rules.
func IdentityRefusal(s string) string { return identityPart(s) }

// identityPart is IdentityRefusal: its rules are [Identity]'s.
func identityPart(s string) string {
	switch notUTF8 := gitref.CommitText(s); {
	case s == "":
		return "is empty: a commit names its author and its committer, each by a name and an address"
	case strings.IndexByte(s, 0) >= 0:
		return "holds a NUL byte, which no process's environment can carry"
	case notUTF8 != "":
		return notUTF8
	case strings.ContainsAny(s, "\n<>"):
		return "holds a line feed, a < or a >: git cuts each out of an identity line"
	case identityCrud(s[0]) || identityCrud(s[len(s)-1]):
		return "begins or ends with a space, a control byte or one of , : ; \" \\ ': git trims each from " +
			"either end of an identity"
	}
	return ""
}

// identityCrud reports whether git trims b from either end of a name or an
// address it writes into an identity line: ident.c's crud(), every byte at or
// below the space and the eight marks , : ; < > " \ '. No byte of a multi-byte
// UTF-8 sequence is one.
func identityCrud(b byte) bool {
	return b <= ' ' || strings.IndexByte(`,:;<>"\'`, b) >= 0
}

// Cmd is one git process, described entirely by the call site: nothing about
// it comes from the environment, the HOME, or a configuration file.
type Cmd struct {
	// Dir is the repository: absolute, filepath.Clean'ed, of the shape
	// [CheckShape] holds it to; GIT_DIR and the working directory. "" runs
	// with NO repository: the working directory is the pool's HOME and
	// GIT_CEILING_DIRECTORIES its parent, so git discovers none.
	Dir string

	// Alternate is the one repository Dir reads objects from beside its own
	// (absolute, clean, never Dir), or "" for none: a push's scratch and an
	// advertisement view name the cache incarnation they lean on. Dir's
	// objects/info/alternates must be exactly the line naming it, or absent:
	// A REPOSITORY DOES NOT CHOOSE ITS OWN ALTERNATE ([ShapeAlternates]),
	// since under protocol v2 every object an alternate holds is fetchable by
	// id through the repository naming it.
	Alternate string

	// Dropping is the object directory of the DROPPING packs of the cache
	// incarnation Dir is, exactly Dir's [DroppingDir], or "" for none. A
	// process that SERVES the incarnation reads it beside its objects as
	// GIT_ALTERNATE_OBJECT_DIRECTORIES (never an alternates file, which git
	// follows from every repository naming Dir as its alternate), so an object
	// an expunge left dropping is still answered by id while no push relies on
	// it. Refused ([ErrInvalid]) with no Dir, beside an Alternate (that process
	// writes, and may not rely on a dropping object) and as any other path; a
	// missing or linked directory is [ShapeDropping].
	Dropping string

	// Loads, when set, is the chunk-format files the process loads, where
	// its caller knows it loads only one (a cat-file asked only about object
	// ids never parses a commit): the pool's observer narrows the run's damage
	// by it ([Damage.Narrow]), as the caller does. Zero says nothing.
	Loads Part

	// Repairer, when set, names the caller that repairs what this run fails
	// with before its first byte (Gitlet, for a process in a cache incarnation
	// it holds pinned); the pool hands it to its observer (Ran.Repairer), so
	// the run's report is held for that repair rather than acted on beside
	// it. Zero names nobody: the report is acted on as it arrives.
	Repairer Repairer

	// Args follow "git". Args[0] is a SUBCOMMAND (^[a-z][a-z0-9-]*$): a
	// global option there (-c, --exec-path, --git-dir, --config-env) would
	// change what the process is. An argument a client wrote is only ever a
	// validated object id or ref, never a path or option: the call site's
	// rule, since a path the engine built is an ordinary argument. A
	// receive-pack's options are each one of receivePackOptions, in full.
	Args []string

	// Config is the call site's configuration, after the pool's base. Each
	// key must be on the allowlist.
	Config []KV

	// Hooks is the hooks directory (absolute, an existing directory); ""
	// runs no hook at all.
	Hooks string

	// Env adds EnvHookSocket and/or EnvHookToken; any other name is ErrEnv.
	Env []KV

	// Identity is the author, committer and dates of the commit a
	// commit-tree writes: REQUIRED there and ErrInvalid on every other
	// subcommand, written as the six variables identityVars names, which Env
	// never may ([Identity]).
	Identity *Identity

	// Remote is the one host the run dials: REQUIRED of a fetch, an ls-remote
	// and a push, and ErrInvalid on every other subcommand, outside the
	// transfer lane, beside a Stall (libcurl watches a remote's leg, where the
	// pool's watchdog cannot see it), and where the arguments name another
	// remote: the first non-option argument (or the first after "--") must be
	// Remote.URL, and no other may name a URL. Built by [ResolveRemote] alone,
	// so the public-address rule judged every address git dials; the pool
	// writes its configuration after the base and takes its secrets out of
	// every line the run hands back (package doc, "One outbound dial").
	Remote *Remote

	// Protocol is GIT_PROTOCOL as the client sent it. One not matching
	// ^version=[0-9]+(:[a-z0-9=.-]+)*$ is DROPPED, and the run is then v0,
	// which every client speaks. "" sets none.
	Protocol string

	// Stdin is read to its end (or the process's) by a copy Run waits for.
	// CONTRACT: every Read returns within a bound of its own — a
	// bytes.Reader, a file, a paced request body — because Run returns only
	// after the copy does, and no goroutine Run starts outlives it.
	Stdin io.Reader

	// Stdout receives every byte git writes, however long after the process
	// ended the caller takes it; nil captures up to CaptureMax into
	// Result.Stdout. Stdin's contract: every Write returns within a bound of
	// its own, and a Write error stops git (its pipe is closed).
	Stdout io.Writer

	// Class picks the lane.
	Class Class

	// Share is whose transfer this is: the class and principal of the
	// credential that asked for it, by which the transfer lane grants released
	// slots and charges what the run spent (share.go). REQUIRED on a
	// ClassTransfer Cmd and refused on every other: a transfer without one
	// would be nobody's, and anything else carrying one would claim a share no
	// lane reads.
	Share Share

	// Deadline bounds the run from start to reap; 0 or negative is
	// ErrNoDeadline. Pick one from deadlines.go.
	Deadline time.Duration

	// Stall ends a run that consumed no Stdin byte and produced no stdout or
	// stderr byte for this long; 0 is not watched, negative is ErrInvalid.
	// Stderr counts: a receive-pack whose client asked for no side-band relays
	// its hook's output, the engine's keepalives among it, there.
	Stall time.Duration

	// Answers are exit statuses that are the command's ANSWER rather than
	// its failure: merge-base --is-ancestor answers 1 for "not an ancestor",
	// check-ref-format 1 for "not a valid name". 0 always is. Each 1..255.
	Answers []int

	// Indexed is closed by the call site once a push's receive-pack's
	// index-pack has finished, so the cores it was lent (MaxLentCores) are
	// repaid then rather than at exit, and the hook, the object-store upload
	// and the wait for the push's record hold no loan. The host closes it when
	// the proc-receive hook first reaches its relay, which receive-pack starts
	// only after its index-pack exits; a push that never gets there repays at
	// its exit. REQUIRED on a receive-pack in the transfer lane that receives a
	// pack (not --advertise-refs, which runs no index-pack) and ErrInvalid on
	// everything else: a nil one would hold every push's loan to the end,
	// which only the lent-cores gauge would show.
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
	// Began is when the process started (after its slot was granted), a
	// time.Now() reading, so a caller orders it against its own on the
	// MONOTONIC clock: the cache compares it with when it last rewrote a file
	// the run may have read, which a wall-clock step must not reorder. Zero
	// when no process started.
	Began time.Time
	// Damage is a [*Damage] of kind [ErrCorrupt] or [ErrMissingObject] when an
	// error event of the run's process tree names one (trace.go): the highest
	// kind, as [ExitError.Kind] is chosen, the parts named and the first such
	// event's words, about the repository the process read; nil when none
	// does, WHATEVER THE OUTCOME. git answers some questions over damage and
	// exits 0 (cat-file --batch-check calls an object `missing` past `index
	// file … is too small`), so a caller taking an answer as what the
	// repository holds asks this first. An error, not a *Damage field: a nil
	// pointer in an error is non-nil at every %w.
	Damage error
	// Trace is what the run's trace2 event stream said: every kind its
	// formats named (what an ExitError matches through errors.Is), the
	// formats themselves, the hints, and how much of it was read.
	Trace Trace
	// Crashed is that a process of the run's tree died of a signal the pool
	// did not send, in a run the pool did not end (deadline, stall, the
	// caller's context) and whose caller's own I/O did not fail: the leader, of
	// a signal other than SIGPIPE (a consumer that hung up), or a child the
	// tree reported dying (Trace.Crashes, which leave SIGPIPE out). A crash names
	// no part: a git dying on a damaged reverse index or bitmap says nothing
	// else, so the cache reads it as a reason to hash what the process read
	// (cache's scrub), never as damage.
	Crashed bool
	// CPU is the user and system time the whole process tree spent, as wait4
	// reports it at the reap (with every descendant waited for: upload-pack's
	// pack-objects, receive-pack's index-pack). For a run the node ended, at
	// least what its group's members had spent when signalled, read from
	// their own records first, since a leader killed with its group reaps
	// none of them (Linux; elsewhere the reap's figure alone).
	CPU time.Duration
}

// THE BASE CONFIGURATION, set first for every process, in this order; no call
// site may set these keys (ADR-0008: one implementation of each rule).
//   - protocol.allow=never: only a remote run's configuration lifts it, for
//     https and that run (package doc, "One outbound dial").
//   - gc.auto=0, maintenance.auto=false: only the cache's sync and maintainer
//     change a cache, and a detached gc would be killed by the sweep.
//   - pack.threads=1: a slot is a core, but for a receive-pack lent read
//     cores (lane.go), whose index-pack gets a thread for each.
//   - core.hooksPath: Cmd.Hooks or a path that cannot exist, never hooks/.
//   - core.attributesFile: a path that cannot exist (ENOTDIR, read as absent
//     silently), else git reads $HOME/.config/git/attributes, which a file
//     written after New checked HOME would steer; GIT_ATTR_NOSYSTEM the host's.
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

// allowedConfig is every key a call site may set, by its lowercase form (git's
// keys are case-insensitive), each with the call site that needs it, added in
// that call site's commit. An allowlist, since a denylist leaks whichever
// command-running or object-serving key nobody named (package doc, "A git
// process is configured by the engine and by nothing else"). No key that
// reaches a network is here: the pool writes those from [Cmd.Remote].
var allowedConfig = map[string]string{
	"core.multipackindex": "the cache's serve configuration and its maintainer",
	"pack.allowpackreuse": "the cache's serve configuration: `single` — the bitmap's preferred pack sent " +
		"verbatim and every other object packed as pack-objects packs any (a stored delta reused, a whole " +
		"copy searched for a delta), which sends a cache's clones fewer bytes than `multi`",
	// The delta window's memory, PER THREAD (pack.threads is the pool's 1); a
	// compaction's charge against the node's background budget counts it
	// (internal/gitlet/cache's CompactWindowBytes).
	"pack.windowmemory": "the cache's serve configuration, and a compaction's pack-objects in its scratch " +
		"(internal/gitlet/compact's PackConfig) and an import's or a pull mirror's cut in its own " +
		"(internal/gitlet/mirror)",
	"pack.usebitmaps": "the cache's serve configuration",
	// Read only by a bitmap write (the maintainer's multi-pack-index), so
	// it rides the serve configuration and changes no other process.
	"pack.writebitmaplookuptable": "the maintainer's bitmap write: a table of its commits, so a reader loads " +
		"only the bitmaps it asks for (git 2.38)",
	// The version of the changed-path filters a commit-graph write writes and
	// a reader reads: it names a format and runs nothing.
	"commitgraph.changedpathsversion": "the cache's serve configuration: its commit-graph writes write " +
		"version-2 changed-path filters, which hash a path alike on every build (correct past ASCII), and " +
		"read only those",
	"uploadpack.allowfilter": "the cache's serve configuration",
	// Which partial-clone filters upload-pack serves. A subsection (the
	// filter's name) is read case-sensitively, so another case names no
	// filter: it allows nothing, and uploadpackfilter.allow=false refuses it.
	"uploadpackfilter.allow":             "the cache's serve configuration: only the filters a bitmap answers",
	"uploadpackfilter.blob:none.allow":   "the cache's serve configuration: only the filters a bitmap answers",
	"uploadpackfilter.blob:limit.allow":  "the cache's serve configuration: only the filters a bitmap answers",
	"uploadpackfilter.tree.maxdepth":     "the cache's serve configuration: only the filters a bitmap answers",
	"uploadpackfilter.object:type.allow": "the cache's serve configuration: only the filters a bitmap answers",
	"uploadpackfilter.combine.allow":     "the cache's serve configuration: only the filters a bitmap answers",
	"uploadpack.keepalive":               "the cache's serve configuration: the stall watchdog relies on it",
	// Patterns for the refs git lists of an alternate: it runs nothing, unlike
	// its sibling core.alternateRefsCommand, which is not here.
	"core.alternaterefsprefixes":   "the cache's advertisement view (Cache.AdvertiseConfig)",
	"receive.procreceiverefs":      "the receive configuration",
	"receive.unpacklimit":          "the receive configuration",
	"receive.fsckobjects":          "the receive configuration",
	"receive.maxinputsize":         "the receive configuration",
	"receive.advertisepushoptions": "the receive configuration",
	"receive.autogc":               "the receive configuration",
	"receive.updateserverinfo":     "the receive configuration",
	// A fetch from another host checks every object as receive-pack checks a
	// push's, and keeps the pack it arrived in: the cut after it cannot name
	// loose objects.
	"transfer.fsckobjects": "an import's and a pull mirror's fetch into its scratch (internal/gitlet/mirror)",
	"fetch.unpacklimit":    "an import's and a pull mirror's fetch into its scratch (internal/gitlet/mirror)",
	// A stored bundle's .rev is written by the process that checked its pack
	// as it indexed it, never a later one: receive-pack's index-pack for a
	// push, the scratch's `index-pack --strict` for an engine write; or for a
	// compaction the pack-objects that wrote it from packs already stored and
	// checked (its bundle still held to its seal, gitlet.StoreBundle). Pinned
	// because git's default changed in 2.41.
	"pack.writereverseindex": "the receive configuration, an engine write's index-pack in its scratch, and a " +
		"compaction's pack-objects in its scratch (internal/gitlet/compact's PackConfig)",
	// Version 2, which the bundle check requires, pinned against a future
	// default; written by the same process as the .rev.
	"pack.indexversion": "the receive configuration, an engine write's index-pack in its scratch, and a " +
		"compaction's pack-objects in its scratch (internal/gitlet/compact's PackConfig)",
	// How git prints a path, and nothing else: off, a path is quoted only for
	// a quote, a backslash or a control byte, not every byte past ASCII.
	"core.quotepath": "a seat's read of one file's patch (internal/gitlet/query's diff-pairs): its header names " +
		"a UTF-8 path as it is, as the stat beside it does",
	// Where a bare repository's `.mailmap` is read from (HEAD's unless this
	// names a blob); it runs and dials nothing. EMPTY reads no mailmap.
	"mailmap.blob": "a seat's blame (internal/gitlet/query's blame.go): empty, so a commit's author is named as the " +
		"commit states it — git blame reads HEAD's .mailmap in a bare repository and has no --no-mailmap",
}

// subcommand is what Args[0] must be.
var subcommand = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// receivePackOptions is every option a receive-pack may be given, in the one
// spelling the pool reads, each with whether it makes the run a push's
// ADVERTISEMENT (its GET) rather than the receive-pack of its POST. The pool
// must know which EXACTLY: only the POST runs an index-pack, is lent
// cores and carries Cmd.Indexed, and a misread advertisement would hold a
// three-core loan until its client read the refs (package doc, "Lending a
// push cores"). git also reads abbreviations (--adv, whose meaning moves as
// git adds options), negations (--no-advertise-refs), "--" and options after
// the directory, so every dashed argument must be a key here, in full, read
// wherever it stands.
// --advertise-refs is git's alias of --http-backend-info-refs, which git's own
// http-backend passes; both are read.
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
	if c.Class == ClassTransfer && !c.Share.Valid() {
		return prepared{}, fmt.Errorf("%w: git %s in the transfer lane carries no valid Cmd.Share (class %q, a principal of %d bytes): "+
			"every transfer is charged to the credential that asked for it, one of the classes %s with a principal of 1 to %d bytes and no NUL",
			ErrInvalid, c.Args[0], textcut.Ellipsis(string(c.Share.Class), 64), len(c.Share.Principal), words.Or(ShareClasses()), maxSharePrincipal)
	}
	if c.Class != ClassTransfer && c.Share != (Share{}) {
		return prepared{}, fmt.Errorf("%w: git %s in the %s lane carries a Cmd.Share: only a transfer is scheduled by share",
			ErrInvalid, c.Args[0], c.Class)
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
	// else (package doc, "Lending a push cores").
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
		if c.Alternate != "" && (!filepath.IsAbs(c.Alternate) || filepath.Clean(c.Alternate) != c.Alternate ||
			c.Alternate == c.Dir) {
			return prepared{}, fmt.Errorf("%w: Cmd.Alternate %q is not an absolute, clean path of a repository other than Cmd.Dir",
				ErrInvalid, c.Alternate)
		}
		if err := CheckShape(c.Dir, c.Alternate); err != nil {
			return prepared{}, err
		}
	} else if c.Alternate != "" {
		return prepared{}, fmt.Errorf("%w: Cmd.Alternate %q names an alternate for no repository: Cmd.Dir is empty",
			ErrInvalid, c.Alternate)
	}
	if err := checkDroppingCall(c); err != nil {
		return prepared{}, err
	}
	if err := checkConfig(c.Config); err != nil {
		return prepared{}, err
	}
	if err := checkEnv(c.Env); err != nil {
		return prepared{}, err
	}
	if err := checkIdentity(c.Args[0], c.Identity); err != nil {
		return prepared{}, err
	}
	if err := checkRemote(c); err != nil {
		return prepared{}, err
	}
	var remote []KV
	if c.Remote != nil {
		remote = c.Remote.config(p.remoteStall)
	}

	// Five beside the tables: GIT_DIR or GIT_CEILING_DIRECTORIES,
	// GIT_PROTOCOL, GIT_CONFIG_COUNT, and the dropping view's two.
	env := make([]string, 0, len(p.baseEnv)+5+2*(len(baseKeys)+len(remote)+len(c.Config))+len(c.Env)+len(identityVars))
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
	if c.Dropping != "" {
		// A SERVING PROCESS'S VIEW of the dropping packs, and its children's
		// — upload-pack's pack-objects among them — which inherit it.
		env = append(env, "GIT_ALTERNATE_OBJECT_DIRECTORIES="+alternateEntry(c.Dropping))
	}
	if c.Alternate != "" && leansOnDropping(c.Alternate) {
		// The alternate's commit-graph may name a commit only a dropping pack
		// holds (leansOnDropping), which git answers from the graph alone
		// unless told to hold it to the objects.
		env = append(env, "GIT_COMMIT_GRAPH_PARANOIA=1")
	}
	env = append(env, "GIT_CONFIG_COUNT="+strconv.Itoa(len(baseKeys)+len(remote)+len(c.Config)))
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
	// A remote run's configuration, after the base and before the call
	// site's: in the environment, never the arguments, since it carries a
	// credential header.
	for i, kv := range remote {
		env = append(env, configKey(len(baseKeys)+i, kv.Key), configValue(len(baseKeys)+i, kv.Value))
	}
	for i, kv := range c.Config {
		at := len(baseKeys) + len(remote) + i
		env = append(env, configKey(at, kv.Key), configValue(at, kv.Value))
	}
	for _, kv := range c.Env {
		env = append(env, kv.Key+"="+kv.Value)
	}
	if c.Identity != nil {
		env = append(env, c.Identity.env()...)
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
		if slices.ContainsFunc(remoteKeys, func(k string) bool { return strings.EqualFold(k, key) }) {
			return fmt.Errorf("%w: %s is the pool's: it is written for a remote run from its Cmd.Remote, so name "+
				"the remote there (ResolveRemote, Remote.WithHeader)", ErrConfig, kv.Key)
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

// checkEnv holds a call site's variables to the two the pool admits; a
// commit's identity is named in [Cmd.Identity] instead.
func checkEnv(env []KV) error {
	seen := map[string]bool{}
	for _, kv := range env {
		if slices.Contains(identityVars, kv.Key) {
			return fmt.Errorf("%w: %s is a commit's identity, which the pool writes from Cmd.Identity for commit-tree alone: name it there",
				ErrEnv, kv.Key)
		}
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

// ShapeRule is one rule of the SHAPE the pool holds a repository to before it
// runs git there ([CheckShape]): a closed set, [ShapeRules], each the reason a
// [ShapeError] gives. Every rule is about a file that CONFIGURES the process
// (where it reads keys, objects and refs), never content the engine wrote
// (refs, packs), which git classifies itself.
type ShapeRule string

const (
	// ShapeConfig holds config to [RepoConfig] byte for byte, since every
	// key the command line does not set is read from the file.
	ShapeConfig ShapeRule = "config"
	// ShapeCommondir refuses a commondir, which would move objects AND refs
	// to another repository with no override.
	ShapeCommondir ShapeRule = "commondir"
	// ShapeObjects holds objects to a directory of the repository's OWN, at
	// its real path's "objects", never a link, which would read another
	// directory's objects as this repository's.
	ShapeObjects ShapeRule = "objects"
	// ShapeAlternates holds objects/info/alternates to exactly what the
	// call's [Cmd.Alternate] makes it (no file for none, else the one line
	// naming the alternate's objects directory), and the alternate to none
	// of its own.
	ShapeAlternates ShapeRule = "alternates"
	// ShapeDropping holds a [DroppingDir], where there is one, to a
	// directory of the repository's OWN at its real path (a link would serve
	// another directory's objects by id), and one a call names
	// ([Cmd.Dropping]) to being there.
	ShapeDropping ShapeRule = "dropping"
)

// ShapeRules is every [ShapeRule]: each holds the call's Dir, and the
// alternate it names, alike.
var ShapeRules = []ShapeRule{ShapeConfig, ShapeCommondir, ShapeObjects, ShapeAlternates, ShapeDropping}

// Valid reports whether r is one of [ShapeRules].
func (r ShapeRule) Valid() bool { return slices.Contains(ShapeRules, r) }

// ShapeError is a repository the pool refused for its shape: the [ShapeRule]
// it breaks and which repository breaks it. errors.Is(err, [ErrRepoShape]).
// A DEFINITE answer and only that: a file that could not be READ is never one.
type ShapeError struct {
	// Rule is the rule broken.
	Rule ShapeRule
	// Dir is the repository that breaks it: the call's Dir, or, when
	// InAlternate, the alternate it names at the real path git resolves. A
	// caller repairs the one named: a scratch's alternate is its cache.
	Dir         string
	InAlternate bool
	// found is what was found, as the error says it.
	found string
}

// Error is ErrRepoShape's text and what was found.
func (e *ShapeError) Error() string { return ErrRepoShape.Error() + ": " + e.found }

// Unwrap is ErrRepoShape.
func (e *ShapeError) Unwrap() error { return ErrRepoShape }

// misshapen is a refusal of dir under rule; alternate says dir is the
// alternate a call names.
func misshapen(rule ShapeRule, dir string, alternate bool, format string, args ...any) error {
	return &ShapeError{Rule: rule, Dir: dir, InAlternate: alternate, found: fmt.Sprintf(format, args...)}
}

// Why the shape is checked: package doc, "A git process is configured by the
// engine and by nothing else". Deliberately not checked: info/attributes (a
// bare repository's only attributes, as git reads no tree's there on the node
// floor; they steer only pack-objects' delta search and an archive's
// conversions, and a cache, whose image has no info/, holds none),
// info/exclude (a work-tree file), hooks/ (core.hooksPath is the pool's), refs
// and packed-refs (content the engine wrote), and the commit-graph and
// multi-pack-index (git writes them under the maintainer; damage is
// classified). info/grafts and shallow are neutralised by GIT_GRAFT_FILE and
// GIT_SHALLOW_FILE instead.

// CheckShape holds the repository at dir to every [ShapeRule], as a process
// whose [Cmd.Alternate] is alternate ("" for none): exactly Run's check of a
// Cmd's Dir. A refusal is a *[ShapeError]; a failed read is the read's own
// error. Exported for the git cache's sweep, which repairs a cache the pool
// refused, so what it leaves is held to the pool's own predicate, not a copy.
func CheckShape(dir, alternate string) error {
	if err := checkRepo(dir, false); err != nil {
		return err
	}
	return checkAlternates(dir, alternate)
}

// checkRepo holds dir to ShapeConfig, ShapeCommondir, ShapeObjects and
// ShapeDropping; alternate says dir is the alternate a call names.
// ErrRepoShape is a DEFINITE answer only: a read that FAILED (descriptors
// exhausted, an I/O error, a directory where the file should be) is returned
// as its own error, without ErrRepoShape, so a transient failure never reports
// a healthy cache as tampered with.
func checkRepo(dir string, alternate bool) error {
	config, err := os.ReadFile(filepath.Join(dir, "config"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return misshapen(ShapeConfig, dir, alternate,
			"%s holds no config: the engine writes one into every repository it runs git in", dir)
	case err != nil:
		return fmt.Errorf("gitproc: reading %s's config: %w", dir, err)
	case string(config) != RepoConfig:
		return misshapen(ShapeConfig, dir, alternate, "%s's config is not the one the engine writes: a "+
			"repository's own config is read for every key the command line does not set", dir)
	}
	_, err = os.Lstat(filepath.Join(dir, "commondir"))
	switch {
	case err == nil:
		return misshapen(ShapeCommondir, dir, alternate,
			"%s holds a commondir, which would point git at another repository's objects and refs", dir)
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("gitproc: reading whether %s holds a commondir: %w", dir, err)
	}
	if err := checkObjects(dir, alternate); err != nil {
		return err
	}
	return checkDropping(dir, alternate, false)
}

// checkDropping holds dir's [DroppingDir] to ShapeDropping: absent unless the
// call names it (named), else a directory of dir's own at its real path, for
// checkObjects' reason.
func checkDropping(dir string, alternate, named bool) error {
	view := filepath.Join(dir, DroppingDir)
	_, err := os.Lstat(view)
	switch {
	case errors.Is(err, fs.ErrNotExist) && !named:
		return nil
	case errors.Is(err, fs.ErrNotExist):
		return misshapen(ShapeDropping, dir, alternate, "%s holds no %s, and the call names it as the dropping view "+
			"it serves: the cache makes it with the first dropping pack it installs", dir, DroppingDir)
	case err != nil:
		return fmt.Errorf("gitproc: reading whether %s holds a %s: %w", dir, DroppingDir, err)
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("gitproc: resolving %s: %w", dir, err)
	}
	resolved, err := filepath.EvalSymlinks(view)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return misshapen(ShapeDropping, dir, alternate, "%s's %s is a link to nowhere: %v", dir, DroppingDir, err)
	case err != nil:
		return fmt.Errorf("gitproc: resolving %s: %w", view, err)
	case resolved != filepath.Join(real, DroppingDir):
		return misshapen(ShapeDropping, dir, alternate, "%s's %s is a link to %s: a repository's dropping packs are "+
			"its own, and the engine never links them", dir, DroppingDir, resolved)
	}
	info, err := os.Stat(resolved)
	switch {
	case err != nil:
		return fmt.Errorf("gitproc: reading %s: %w", resolved, err)
	case !info.IsDir():
		return misshapen(ShapeDropping, dir, alternate, "%s's %s is no directory", dir, DroppingDir)
	}
	return nil
}

// checkDroppingCall refuses a call that names a dropping view it may not
// ([Cmd.Dropping]), and holds the one it names to being there (ShapeDropping).
func checkDroppingCall(c Cmd) error {
	switch {
	case c.Dropping == "":
		return nil
	case c.Dir == "":
		return fmt.Errorf("%w: Cmd.Dropping %q names a dropping view for no repository: Cmd.Dir is empty",
			ErrInvalid, c.Dropping)
	case c.Alternate != "":
		return fmt.Errorf("%w: Cmd.Dropping %q beside Cmd.Alternate %q: a process leaning on an alternate writes into "+
			"a repository of its own, and nothing it writes may rely on an object an expunge has left dropping",
			ErrInvalid, c.Dropping, c.Alternate)
	case c.Dropping != filepath.Join(c.Dir, DroppingDir):
		return fmt.Errorf("%w: Cmd.Dropping %q is not %s: a dropping view is its own repository's %s",
			ErrInvalid, c.Dropping, filepath.Join(c.Dir, DroppingDir), DroppingDir)
	}
	return checkDropping(c.Dir, false, true)
}

// leansOnDropping reports whether alternate holds a dropping view: its
// commit-graph, which git loads for the process as its own, may then name a
// commit only a dropping pack holds. Anything at the name counts, an
// unreadable one included: the paranoia costs a lookup a commit, and missing
// it lets a push rely on an object an expunge drops.
func leansOnDropping(alternate string) bool {
	_, err := os.Lstat(filepath.Join(alternate, DroppingDir))
	return !errors.Is(err, fs.ErrNotExist)
}

// alternateEntry is path as one entry of GIT_ALTERNATE_OBJECT_DIRECTORIES: as
// it is, or C-quoted (object-file.c's parse_alt_odb_entry) where it holds the
// list separator, a quote, a backslash or a control byte, since an operator
// may name a cache directory anything.
func alternateEntry(path string) string {
	if !strings.ContainsAny(path, string(os.PathListSeparator)+`"\`) && !hasControl(path) {
		return path
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := range len(path) {
		switch c := path[i]; {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20 || c == 0x7f:
			fmt.Fprintf(&b, `\%03o`, c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// hasControl reports whether s holds an ASCII control byte.
func hasControl(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0
}

// checkObjects holds dir to ShapeObjects: its objects directory, resolved, is
// its real path's "objects" and a directory. A link there would serve
// another directory's objects by id, as an unnamed alternates file would.
func checkObjects(dir string, alternate bool) error {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("gitproc: resolving %s: %w", dir, err)
	}
	objects, err := filepath.EvalSymlinks(filepath.Join(dir, "objects"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return misshapen(ShapeObjects, dir, alternate, "%s has no objects directory: %v", dir, err)
	case err != nil:
		return fmt.Errorf("gitproc: resolving %s's objects directory: %w", dir, err)
	case objects != filepath.Join(real, "objects"):
		return misshapen(ShapeObjects, dir, alternate, "%s's objects is a link to %s: a repository reads its own "+
			"objects, and the engine never links them", dir, objects)
	}
	info, err := os.Stat(objects)
	switch {
	case err != nil:
		return fmt.Errorf("gitproc: reading %s's objects directory: %w", dir, err)
	case !info.IsDir():
		return misshapen(ShapeObjects, dir, alternate, "%s's objects is no directory", dir)
	}
	return nil
}

// checkAlternates holds dir's alternates file to the call's alternate (no
// file for none, else the one line naming its objects directory), and that
// alternate to checkRepo's shape with no alternate of its own: git lists an
// alternate's refs by running for-each-ref in it with every variable the pool
// sets CLEARED (local_repo_env), so its own files alone configure it. The
// SHAPE is held, not what that process reads today, for the allowlist's
// reason; a chain's second hop would be read the same way. The repository
// checked is the one git runs in: git resolves the line to its real path
// (refs_from_alternate_cb) and strips "/objects", and ShapeObjects makes that
// the alternate itself (package doc, "A git process is configured by the
// engine and by nothing else"). As in checkRepo, a failed read is its own
// error.
func checkAlternates(dir, alternate string) error {
	body, err := os.ReadFile(filepath.Join(dir, "objects", "info", "alternates"))
	switch {
	case errors.Is(err, fs.ErrNotExist) && alternate == "":
		return nil
	case errors.Is(err, fs.ErrNotExist):
		return misshapen(ShapeAlternates, dir, false, "%s names no alternate, and the call reads %s as its one: the "+
			"engine writes the line naming it", dir, alternate)
	case err != nil:
		return fmt.Errorf("gitproc: reading %s's alternates: %w", dir, err)
	case alternate == "":
		return misshapen(ShapeAlternates, dir, false, "%s holds an alternates file, and the call names no "+
			"alternate: a repository does not choose where git reads objects from, and under protocol v2 every "+
			"object an alternate holds is fetchable by id through it", dir)
	}
	// The one absolute, clean line ending in a newline that the engine
	// writes — which also rules out the two first bytes git gives a meaning
	// of its own, '#' (a comment) and '"' (a quoted path).
	if want := filepath.Join(alternate, "objects") + "\n"; string(body) != want {
		return misshapen(ShapeAlternates, dir, false, "%s's alternates reads %q, and the call's alternate is %s: "+
			"the engine writes the one line naming its objects directory", dir, textcut.Ellipsis(string(body), 120),
			alternate)
	}
	if err = checkRepo(alternate, true); err != nil {
		return err
	}
	_, err = os.Lstat(filepath.Join(alternate, "objects", "info", "alternates"))
	switch {
	case err == nil:
		return misshapen(ShapeAlternates, alternate, true, "%s names an alternate and is itself one: the engine "+
			"never chains alternates, and git would read the second hop's files with no configuration of the pool's",
			alternate)
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("gitproc: reading whether %s holds an alternates file: %w", alternate, err)
	}
	return nil
}

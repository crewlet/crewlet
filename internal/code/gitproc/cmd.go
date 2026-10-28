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
	"unicode/utf8"

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
// `commit-tree` and nothing else ([Cmd.Identity]): the pool builds every other
// process's environment with no identity at all, so a commit's names and
// dates are exactly what its call states — never the HOME's, never the
// host's, never the clock's. A git left to find an identity takes one from
// the node it runs on — its account's name, its mail name or host name, its
// clock — or refuses when it finds none, so the same commit-tree run on two
// nodes, or twice on one, would be two commits naming a machine.
//
// EVERY PART IS ONE GIT WRITES AS IT IS GIVEN, or the run is refused before
// any process starts ([ErrInvalid], naming the field). git builds an identity
// line by cutting every line feed, `<` and `>` out of a name or an address and
// trimming from either end of it every control byte but DEL, the space and
// every `,` `:` `;` `"` `\` `'` (ident.c's crud), and it rewrites every byte
// of a commit that is not UTF-8 AS GIT READS IT as though the byte were
// Latin-1 (commit.c's ensure_utf8): git's UTF-8 is unicode/utf8's less
// Unicode's 66 noncharacters — U+FDD0 to U+FDEF, and the last two code points
// of every plane, U+xxFFFE and U+xxFFFF — which Go takes as any other rune.
// A part git changes is a name the call never stated, in history for good,
// and a part made of nothing else git refuses itself, late, after the work
// that asked for the commit. A NUL is refused, since no environment variable
// can carry one, and so is an empty part — an empty name git refuses too, but
// an empty address it writes as `<>`, a commit that names no address. Those
// two are the pool's own rules; every other is git's, and
// TestThePoolAdmitsAnIdentityPartExactlyWhereGitWritesItAsGiven holds them
// both ways against git. git holds a commit's message to the same UTF-8 rule,
// since ensure_utf8 reads the whole commit, and the pool hands Stdin to git
// unread: a call site that writes a message holds it to that rule itself.
//
// THE DATES are written in the one form git reads as seconds whatever their
// digits, `@<unix seconds> +0000` — without the `@`, git reads a number of
// eight digits or fewer as a calendar date, as it reads 20070606, and refuses
// the epoch's 0 as one it cannot read — so each is held to WHOLE seconds at or
// after the epoch: a fraction of a second is refused rather than dropped, so
// the call states the second it means. CommitterWhen is never before
// AuthorWhen and may be after it: an engine-built commit's committer date is
// held to its parent's, so no child is dated before its parent, while its
// author date stays the instant its principal acted.
type Identity struct {
	AuthorName, AuthorEmail       string
	CommitterName, CommitterEmail string
	AuthorWhen                    time.Time // GIT_AUTHOR_DATE, written "@<unix seconds> +0000"
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

// gitDate is t as the timestamp git writes into a commit, seconds since the
// epoch at +0000, behind the `@` that makes git read it as one.
func gitDate(t time.Time) string {
	return "@" + strconv.FormatInt(t.Unix(), 10) + " +0000"
}

// checkIdentity holds a run's identity to its subcommand — commit-tree's
// alone, and required there — and every part of it to what git writes as it
// is given ([Identity]).
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

// identityPart is why git would not write s into a commit as it is given — or
// why the pool refuses it of its own (an empty part, a NUL) — and "" when it
// would. Its rules are [Identity]'s.
func identityPart(s string) string {
	switch notUTF8 := commitUTF8(s); {
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

// commitUTF8 is why s is not UTF-8 as git reads a commit, and "" when it is.
// git's UTF-8 is not Go's: its has_invalid_utf8 (commit.c) refuses, beside
// everything unicode/utf8 refuses, Unicode's noncharacters (noncharacter),
// which unicode/utf8 takes as it takes any other rune, and its ensure_utf8
// rewrites every byte of a sequence it refused as though the byte were
// Latin-1 — so a valid Go string ending in U+FFFE goes into history ending in
// the six bytes of "ï¿¾", while git prints a warning and exits 0. The rule is
// the whole commit's, not its identity's alone: ensure_utf8 reads every byte
// commit-tree writes, the message it reads from Stdin among them.
func commitUTF8(s string) string {
	if !utf8.ValidString(s) {
		return "is not UTF-8: git would rewrite the commit's bytes as though they were Latin-1"
	}
	for _, r := range s {
		if noncharacter(r) {
			return fmt.Sprintf("holds the Unicode noncharacter %U, which git reads as bytes that are not UTF-8 "+
				"and rewrites as though they were Latin-1", r)
		}
	}
	return ""
}

// noncharacter reports whether r is one of Unicode's 66 noncharacters, by
// has_invalid_utf8's own two tests: U+FDD0 to U+FDEF, and the last two code
// points of every plane, U+xxFFFE and U+xxFFFF, each a code point whose low
// sixteen bits are FFFE or FFFF. Unicode's stability policy fixes the set for
// good.
func noncharacter(r rune) bool {
	return (0xFDD0 <= r && r <= 0xFDEF) || r&0xFFFE == 0xFFFE
}

// identityCrud reports whether git trims b from either end of a name or an
// address it writes into an identity line: ident.c's crud(), every byte at or
// below the space — each control byte but DEL, and the space itself — and the
// eight marks , : ; < > " \ '. No byte of a multi-byte UTF-8 sequence is one.
func identityCrud(b byte) bool {
	return b <= ' ' || strings.IndexByte(`,:;<>"\'`, b) >= 0
}

// Cmd is one git process, described entirely by the call site: nothing about
// it comes from the environment, the HOME, or a configuration file.
type Cmd struct {
	// Dir is the repository: absolute, filepath.Clean'ed, of the shape
	// [CheckShape] holds it to — RepoConfig, no commondir, an objects
	// directory of its own, and the alternate Alternate names or none. It is
	// GIT_DIR and the working
	// directory. "" runs with NO repository: the working directory is the
	// pool's HOME and GIT_CEILING_DIRECTORIES is its parent, so git
	// discovers none.
	Dir string

	// Alternate is the one repository Dir reads objects from beside its
	// own — absolute and clean, never Dir — or "" for none. Dir's
	// objects/info/alternates must be exactly the one line naming
	// Alternate's objects directory, and with no Alternate there must be
	// no such file: A REPOSITORY DOES NOT CHOOSE ITS OWN ALTERNATE
	// ([ShapeAlternates]). Read from the file, an alternate is whatever a
	// disk or anything outside the engine wrote there — and under
	// protocol v2 every object an alternate holds is fetchable by id
	// through the repository naming it, so a cache with a planted
	// alternates file would serve another repository's objects. A push's
	// scratch and an advertisement view name the cache incarnation they
	// lean on; nothing else the engine runs git in has one.
	Alternate string

	// Loads, when set, is the chunk-format files the process loads, where
	// its caller knows it loads only one of them — a cat-file asked about
	// object ids alone reads objects through the multi-pack-index and never
	// parses a commit: the pool's observer reads the run's damage narrowed
	// by it ([Damage.Narrow]), as the caller does. Zero says nothing.
	Loads Part

	// Repairer, when set, names the caller that repairs what this run fails
	// with before its first byte — the code host, for a process it runs in
	// a cache incarnation it holds pinned — and the pool hands it to its
	// observer untouched (Ran.Repairer), so what the run reports is held
	// for that caller's own repair rather than acted on beside it. Zero
	// names nobody: the run's report is acted on as it arrives.
	Repairer Repairer

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

	// Identity is the author, committer and dates of the commit a
	// commit-tree writes — REQUIRED on a commit-tree and ErrInvalid on every
	// other subcommand — written into the process's environment as the six
	// variables git reads them from (GIT_AUTHOR_NAME, _EMAIL and _DATE, and
	// GIT_COMMITTER_NAME, _EMAIL and _DATE), which Env never names. See
	// [Identity] for why the identity is the call's and what a part may hold.
	Identity *Identity

	// Remote is the one host the run dials — REQUIRED of a fetch, an
	// ls-remote and a push, and ErrInvalid on every other subcommand, in
	// any lane but the transfer lane, beside a Stall (libcurl watches a
	// remote's leg, where the pool's watchdog cannot see it), and where its
	// arguments name any remote but this one: git reads the remote from the
	// first argument that is no option (or the first after "--"), which
	// must be Remote.URL, and no other argument may name a URL. Built by
	// [ResolveRemote] alone, so every address git connects to is one the
	// public-address rule judged; the pool turns it into the run's
	// configuration after the base (https alone, its host pinned to those
	// addresses, no redirect, its header as http.extraHeader), and takes
	// its secrets out of every line the run hands back. The package doc's
	// "One outbound dial" says why each is so.
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

	// Stdout receives everything git writes — every byte, however long after
	// the process ended the caller takes it; nil captures up to CaptureMax
	// into Result.Stdout. Same contract as Stdin: every Write returns within
	// a bound of its own, and a Write error stops git (the pipe is closed
	// under it).
	Stdout io.Writer

	// Class picks the lane.
	Class Class

	// Share is whose transfer this is: the class and principal of the
	// credential that asked for it, by which the transfer lane decides who is
	// granted a released slot and charges what the run spent (share.go).
	// REQUIRED on every ClassTransfer Cmd and refused on every other, as
	// Indexed is: a transfer without one would be scheduled as nobody's, and
	// a read or a maintenance run carrying one would claim a share no lane
	// reads.
	Share Share

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
	// Began is when the process started — after its slot was granted — a
	// time.Now() reading, so a caller orders it against a time.Now() of its
	// own on the MONOTONIC clock (Time.Before uses it when both carry one):
	// the cache compares it with when it last rewrote a file the run may
	// have read, and a wall-clock step must not make a report look newer
	// than the rewrite it predates. Zero when no process started.
	Began time.Time
	// Damage is a [*Damage] — of kind [ErrCorrupt] or [ErrMissingObject] —
	// when an error event of the run's process tree names one (trace.go):
	// the highest kind, as [ExitError.Kind] is chosen, the parts its events
	// named and the first such event's words; and nil when none does,
	// WHATEVER THE OUTCOME: git answers some questions over damage it
	// reports and exits 0 (cat-file --batch-check says an object in a pack
	// whose index is truncated is `missing`, past `index file … is too
	// small`), so a caller that takes an answer as what the repository holds
	// asks this first. Like the kind, it describes the repository the process
	// read. An error, never a *Damage field: a nil pointer in an error is a
	// non-nil error at every %w.
	Damage error
	// Trace is what the run's trace2 event stream said: every kind its
	// formats named (what an ExitError matches through errors.Is), the
	// formats themselves, the hints, and how much of it was read.
	Trace Trace
	// Crashed is that a process of the run's tree died of a signal the pool
	// did not send: the leader, with no kill of the pool's (a deadline, a
	// stall, the caller's context), no failure of the caller's own input or
	// output, and a signal other than SIGPIPE — a consumer that hung up — or
	// a child the tree reported dying (Trace.Crashes: git's own reports,
	// which leave SIGPIPE out). Never set for a run the pool ended. Nothing
	// a crash names is a part: a git that dies reading a damaged reverse
	// index or bitmap says nothing else, so the cache reads it as a reason
	// to hash what the process read (cache's scrub), never as damage.
	Crashed bool
	// CPU is the user and system time the process tree spent, the whole of
	// it, counted when the run ends: the process and every descendant it
	// waited for (upload-pack's pack-objects, receive-pack's index-pack), as
	// wait4 reports them at the reap — and for a run the node ended, at
	// least what its group's members had spent when it was signalled, read
	// from their own records first, since a leader killed with its group
	// reaps none of them (Linux; elsewhere the reap's figure alone).
	CPU time.Duration
}

// THE BASE CONFIGURATION, set first for every process, in this order. A call
// site may not set any of these keys: a rule every call site has to remember
// is the rule ADR-0008 says gets one implementation.
//
//   - protocol.allow=never: no engine git dials anything through git's
//     transports — alternates are not one — but a remote run, whose
//     configuration after the base lifts it for https alone, for that run
//     ([Cmd.Remote], remote.go's keys and the package doc's "One outbound
//     dial").
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
//     empty would steer every process's attributes. Its system twin, the
//     host's system attributes file (`git var GIT_ATTR_SYSTEM`), is the
//     environment's GIT_ATTR_NOSYSTEM (gitproc.go).
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
// needs is added here in the commit that adds the call site. No key of a
// remote run's configuration is here, nor any other that reaches a network:
// the pool writes those from the run's [Cmd.Remote], and a call site setting
// one is told to name the remote there.
var allowedConfig = map[string]string{
	"core.multipackindex": "the cache's serve configuration and its maintainer",
	"pack.allowpackreuse": "the cache's serve configuration: `single` — the bitmap's preferred pack sent " +
		"verbatim and every other object packed as pack-objects packs any (a stored delta reused, a whole " +
		"copy searched for a delta), which sends a cache's clones fewer bytes than `multi`",
	// The delta window's memory, PER THREAD (pack.threads is the pool's 1): a
	// cache's pack-objects for a clone, and a compaction's build, whose charge
	// against the node's background budget counts it (internal/code/cache's
	// CompactWindowBytes).
	"pack.windowmemory": "the cache's serve configuration, and a compaction's pack-objects in its scratch " +
		"(internal/code/compact's CompactConfig)",
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
	// Which partial-clone filters upload-pack serves. A subsection — the
	// filter's name — is the one part of a key git reads case-sensitively, so
	// another case of one of these names no filter git knows: it allows
	// nothing, and what it names is refused by uploadpackfilter.allow=false.
	"uploadpackfilter.allow":             "the cache's serve configuration: only the filters a bitmap answers",
	"uploadpackfilter.blob:none.allow":   "the cache's serve configuration: only the filters a bitmap answers",
	"uploadpackfilter.blob:limit.allow":  "the cache's serve configuration: only the filters a bitmap answers",
	"uploadpackfilter.tree.maxdepth":     "the cache's serve configuration: only the filters a bitmap answers",
	"uploadpackfilter.object:type.allow": "the cache's serve configuration: only the filters a bitmap answers",
	"uploadpackfilter.combine.allow":     "the cache's serve configuration: only the filters a bitmap answers",
	"uploadpack.keepalive":               "the cache's serve configuration: the stall watchdog relies on it",
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
	// The .rev a stored bundle carries is written by the process that wrote
	// or checked its pack: for content from outside — a push, an engine
	// write — the index-pack that checked the pack as it indexed it,
	// receive-pack's own for a push and for an engine write the
	// `index-pack --strict` of its scratch, the relay's own fsck, never
	// pack-objects or a second process after it; and for a COMPACTION the
	// pack-objects that wrote the output (internal/code/compact), since every
	// object it writes comes out of packs the code host already stored and
	// checked, and its bundle is held to its seal on the way to the store
	// like any other (code.StoreBundle). Pinned rather than left to git's
	// default, which changed in 2.41.
	"pack.writereverseindex": "the receive configuration, an engine write's index-pack in its scratch, and a " +
		"compaction's pack-objects in its scratch (internal/code/compact's CompactConfig)",
	// The index a stored bundle carries is version 2, which the bundle check
	// requires; pinned so a future default cannot store one no cache accepts.
	// Written by the same process as the .rev.
	"pack.indexversion": "the receive configuration, an engine write's index-pack in its scratch, and a " +
		"compaction's pack-objects in its scratch (internal/code/compact's CompactConfig)",
	// How git writes a path in what it prints, and nothing else: off, a path
	// is quoted only where a quote, a backslash or a control byte would break
	// its line, rather than every byte past ASCII written as an octal escape.
	"core.quotepath": "a seat's read of one file's patch (internal/code/query's diff-pairs): its header names " +
		"a UTF-8 path as it is, as the stat beside it does",
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
	if c.Class == ClassTransfer && !c.Share.Valid() {
		return prepared{}, fmt.Errorf("%w: git %s in the transfer lane carries no valid Cmd.Share (class %q, a principal of %d bytes): "+
			"every transfer is charged to the credential that asked for it, one of the classes %v with a principal of 1 to %d bytes and no NUL",
			ErrInvalid, c.Args[0], textcut.Ellipsis(string(c.Share.Class), 64), len(c.Share.Principal), ShareClasses(), maxSharePrincipal)
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

	env := make([]string, 0, len(p.baseEnv)+3+2*(len(baseKeys)+len(remote)+len(c.Config))+len(c.Env)+len(identityVars))
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
	// site's: written by the pool from Cmd.Remote, and so in the process's
	// environment, never its arguments — a credential header among it.
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

// checkEnv holds a call site's variables to the two the pool admits. A
// commit's identity is not among them: the pool writes its variables from
// [Cmd.Identity], for commit-tree alone, and a call site naming one is told
// to name it there.
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
// [ShapeError] gives. Every rule is about a file that CONFIGURES the process —
// what it reads its keys, its objects and its refs from — never about content
// the engine wrote (refs, packs), which git classifies itself.
type ShapeRule string

const (
	// ShapeConfig holds config to [RepoConfig] byte for byte, since every
	// key the command line does not set is read from the file.
	ShapeConfig ShapeRule = "config"
	// ShapeCommondir refuses a commondir, which would move objects AND refs
	// to another repository with no override.
	ShapeCommondir ShapeRule = "commondir"
	// ShapeObjects holds objects to a directory of the repository's OWN — at
	// its real path, the repository's real path and "objects" — never a
	// link, which would read another directory's objects as this
	// repository's.
	ShapeObjects ShapeRule = "objects"
	// ShapeAlternates holds objects/info/alternates to exactly what the
	// call's [Cmd.Alternate] makes it — no file for none, the one line
	// naming the alternate's objects directory for one — and the alternate
	// to holding none of its own.
	ShapeAlternates ShapeRule = "alternates"
)

// ShapeRules is every [ShapeRule]: each holds the call's Dir, and the
// alternate it names, alike.
var ShapeRules = []ShapeRule{ShapeConfig, ShapeCommondir, ShapeObjects, ShapeAlternates}

// Valid reports whether r is one of [ShapeRules].
func (r ShapeRule) Valid() bool { return slices.Contains(ShapeRules, r) }

// ShapeError is a repository the pool refused for its shape: the [ShapeRule]
// it breaks and which repository breaks it. errors.Is(err, [ErrRepoShape]).
// A DEFINITE answer and only that: a file that could not be READ is never one.
type ShapeError struct {
	// Rule is the rule broken.
	Rule ShapeRule
	// Dir is the repository that breaks it: the call's Dir, or — when
	// InAlternate — the alternate the call names, at the real path git
	// resolves it to. What a caller repairs is the one it names: a refusal
	// of a scratch's alternate is a refusal of the cache it leans on.
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

// A REPOSITORY CANNOT CONFIGURE ITS OWN PROCESS — and overriding dangerous
// keys on the command line does not achieve it.
//
// Command-line configuration (GIT_CONFIG_COUNT) outranks a repository's config
// file only for the keys it sets; every other key is read from the file, so a
// repository config naming uploadpack.allowAnySHA1InWant is honoured by every
// git that does not set it, and an include.path there reads any file at all. A
// planted commondir moves objects AND refs to another repository and has no
// override (GIT_COMMON_DIR does not beat it), and a planted alternates file
// adds another repository's objects to this one's — every one of them
// fetchable by id under protocol v2. The engine writes every repository it
// runs git in, by hand, so a check of their SHAPE costs nothing legitimate and
// turns "a repository can configure git" from a hope into a refusal.
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

// CheckShape holds the repository at dir to every [ShapeRule], as a process
// whose [Cmd.Alternate] is alternate ("" for none): exactly the check Run
// makes of a Cmd's Dir before it starts anything. A refusal is a *[ShapeError];
// a file that could not be read is the read's own error.
//
// Exported for the one caller that MAKES a directory this shape — the git
// cache's sweep, which repairs a cache the pool refused — so what it leaves is
// held to the pool's own predicate rather than to a copy of it, and the two
// can never disagree about what the pool will run in.
func CheckShape(dir, alternate string) error {
	if err := checkRepo(dir, false); err != nil {
		return err
	}
	return checkAlternates(dir, alternate)
}

// checkRepo holds dir to ShapeConfig, ShapeCommondir and ShapeObjects;
// alternate says dir is the alternate a call names.
//
// ErrRepoShape is a DEFINITE answer and only that — the config absent or
// holding other bytes, a commondir present, an objects directory that is a
// link or none at all. A read that FAILED (descriptors exhausted on a busy
// node, an I/O error, a directory where the file should be) says nothing
// about the shape, so it is returned as the read's own error, wrapped and
// without ErrRepoShape: collapsing "could not be read" into "definitely
// wrong" would tell a caller a healthy cache was tampered with on a transient
// failure, and the operator reading it the same.
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
	return checkObjects(dir, alternate)
}

// checkObjects holds dir to ShapeObjects: its objects directory, resolved,
// is its own — its real path's "objects" — and a directory. A link there
// reads ANOTHER directory's objects as this repository's, every one of them
// fetchable by id under protocol v2, which is exactly what an alternates file
// the call did not name would do; the engine never makes one.
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

// checkAlternates holds dir's alternates file to the call's alternate: no
// file for none, and for one the one line naming its objects directory — and
// holds that alternate to the same shape, with no alternate of its own.
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
// THE REPOSITORY CHECKED IS THE ONE GIT RUNS IN. git resolves the line to its
// real path (strbuf_realpath in refs_from_alternate_cb), strips "/objects" and
// runs for-each-ref there — so a line whose objects directory were a link into
// another repository would be configured by THAT repository's files, and
// checking the directory the line merely passes through would accept a config
// git never reads. ShapeObjects holds the alternate's objects directory to
// its own real path, which makes the repository git resolves the line to the
// alternate itself, and that is the one checked.
//
// As in checkRepo, ErrRepoShape is a definite answer, and a read that failed
// is the read's own error.
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

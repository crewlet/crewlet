// Package gitproc is THE ONE GIT SUBPROCESS WRAPPER. Every git process the
// engine starts — a clone's upload-pack, a push's receive-pack, the cache's
// multi-pack-index, the receive pipeline's reads — is started by [Pool.Run],
// and nothing else in the module but cmd/'s operator commands may start git
// or one of its git-* programs: TestOnlyGitprocExecsGit holds that, and
// ADR-0008 is why it is a gate.
//
// # Why one wrapper
//
// Gitlet, the engine's own git host, runs real upstream git rather than a
// reimplementation, because pack negotiation, delta resolution and fsck are
// what a reimplementation gets subtly wrong and a client notices first. So a git
// process is the unit of work on every Gitlet node, and each one is a program
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
// the configuration switches do not reach: git reads the system attributes
// file (`git var GIT_ATTR_SYSTEM`: /etc/gitattributes for the image's git, as
// for a distribution's) unless GIT_ATTR_NOSYSTEM is set, and
// $HOME/.config/git/attributes unless core.attributesFile names another file,
// so the environment sets the one and the base configuration points the other
// at a path that cannot exist.
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
// attributes file), then — for a run that dials a remote alone — the pairs
// the pool writes from its [Cmd.Remote] ("One outbound dial", below), then
// the call site's own pairs, each of which must be on an ALLOWLIST. An
// allowlist rather than a list of forbidden keys, for
// hostbox's reason: git has a dozen keys that run a command (alias.*,
// uploadpack.packObjectsHook, core.fsmonitor, core.alternateRefsCommand,
// include.path, …) and one that serves every object in a repository to
// anyone (uploadpack.allowAnySHA1InWant, which Gitlet never sets) — a
// denylist leaks whichever nobody named.
//
// A COMMIT IS ITS CALL'S. git writes a commit's author, committer and dates
// from its environment, and where it is given none it finds them on the node
// — its account's name, its mail name or host name, its clock — or refuses,
// so the same commit-tree run on two nodes, or twice on one, would be two
// commits naming a machine. The built environment carries no identity, and
// the one a process is given is its call's: [Cmd.Identity], required of a
// commit-tree and refused on every other subcommand, written as the six
// GIT_AUTHOR_* and GIT_COMMITTER_* variables, which Cmd.Env never names. Each
// part is held to what git writes as it is given, and each date to whole
// seconds at or after the epoch ([Identity]), so the commit a call builds is
// the same bytes on every node.
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
// THE DROPPING VIEW IS A SERVING PROCESS'S ALONE. A cache incarnation keeps
// the packs an expunge has left dropping (internal/gitlet's PackDropping) in an
// object directory of its own beside objects/, [DroppingDir], so a client
// still reading one of their objects is served it while no push may rely on
// one. A process that serves the incarnation names it ([Cmd.Dropping]) and
// reads it as GIT_ALTERNATE_OBJECT_DIRECTORIES — set in its environment by
// the pool, never as an alternates file, which git follows from every
// repository that names the incarnation as ITS alternate, and never named by
// a process that leans on an alternate, which the pool refuses. And because
// git loads the FIRST commit-graph its object directories hold, an
// alternate's included, a process leaning on an alternate that holds a
// dropping view — a push's scratch, an advertisement view, a build — would
// parse a commit only a dropping pack holds out of the alternate's graph
// without reading the object it cannot see, and its connectivity check would
// pass over it: such a process runs with GIT_COMMIT_GRAPH_PARANOIA=1, which
// has git hold every commit the graph names to the objects it can reach. The
// view's directory is the repository's own too ([ShapeDropping]): a link
// there would serve another directory's objects by id.
//
// The refusal is a *[ShapeError] naming its [ShapeRule] and the repository
// that broke it (errors.Is [ErrRepoShape]), a definite answer — a file absent,
// other bytes, a commondir present — and a read that FAILED is never one: it
// is returned as the read's own error, so a node out of descriptors is never
// told its healthy caches were tampered with. [CheckShape] is that check,
// exported for the git cache's sweep, which repairs a cache the pool refused
// and holds what it leaves to the pool's own predicate.
//
// # One outbound dial
//
// A git process reaches another host in exactly one shape: a [Cmd.Remote],
// REQUIRED of a fetch, an ls-remote and a push and refused on every other
// subcommand, which lifts the base's protocol.allow=never for https alone
// (protocol.https.allow=always), for its run alone. It is the engine's one
// outbound git dial — a repository's import and both kinds of mirror take it
// — and it is built so that a host a person or a company document names is
// dialed where the engine judged it, and nowhere else:
//
//   - THE NAME IS RESOLVED IN GO AND PINNED. git resolves a host itself,
//     through libcurl, after any judgment the engine could make of it, so a
//     name whose owner answers once with a public address and once with an
//     internal one would pass a check made in Go and be dialed at the
//     second answer. [ResolveRemote] resolves the host and holds every
//     address to the engine's public-address rule (internal/httpx, its
//     NAT64 prefixes discovered through the same resolver) — unless Tier A's
//     gitlet.import_allow_private lifts the rule — and the run hands libcurl
//     those addresses as the name's only answer (http.curloptResolve), so
//     the address judged is the address connected to.
//   - THE URL GIT READS IS THE ONE THE RULE JUDGED. ResolveRemote writes the
//     URL back from what it parsed — https; a lowercase name of letters,
//     digits and hyphens whose last label begins with a letter, or an
//     address in its one written form; no user, query or fragment — because
//     two parsers that read one URL differently (a host after an @, a
//     backslash, an IPv4 address in octal or in fewer than four parts, a
//     trailing dot, a name libcurl would turn into punycode) are how a check
//     of one host becomes a dial of another, or how libcurl looks a name up
//     by a spelling the pin is not keyed by and asks DNS instead.
//   - ONE HOST, NEVER A SECOND. Nothing the remote answers sends git to a
//     host the pin does not cover: no redirect is followed, nor a dumb
//     server's http-alternates, which git follows under the same switch
//     (http.followRedirects=false); no bundle URI is fetched
//     (transfer.bundleURI=false, written out against a default that may
//     change); no submodule is (fetch.recurseSubmodules=false, which also
//     spares a large fetch git's walk of every new tree for gitlinks); and
//     packfile URIs are never asked for, since fetch.uriProtocols is no key
//     a call site may set. The remote git dials is the run's: the first
//     argument that is no option must be Remote.URL, and no other argument
//     may name a URL.
//   - A CREDENTIAL IS A HEADER, NEVER AN ARGUMENT. [Remote.WithHeader]'s
//     header reaches git as a GIT_CONFIG_COUNT pair (http.extraHeader), in
//     the process's environment, which only its own user can read — never in
//     its arguments, which every user of the machine can (ps,
//     /proc/<pid>/cmdline) — and a URL naming a user is refused, since git
//     writes the URL into the arguments of every process it starts. git
//     never prints the header, but the remote, which holds it, may echo it
//     back: so the header's value, the credential in it, what a Basic
//     credential encodes and a proxy's password are taken out of every line
//     the run hands back — stderr's tail and stdout a line at a time as they
//     arrive, and the damage its trace named — and the stderr and the damage
//     then go through internal/redact.
//   - NOTHING PROMPTS (GIT_TERMINAL_PROMPT=0 is every run's) and no
//     credential helper is configured, so a remote that asks for a password
//     is refused at once rather than waited on.
//   - A REMOTE PACES ITS RUN as a client paces a transfer, so a remote run
//     waits in the transfer lane, charged to the [Share] it carries, under
//     [RemoteDeadline]. Its network leg is git-remote-https's, past the
//     pool's pipes, where no stall watchdog of the pool's sees it move — a
//     fetch or a push writes nothing on them without --progress until it
//     ends — so a Cmd.Stall beside a remote is refused rather than left to end
//     a transfer that is moving, and the leg is held to TCP's word on its
//     PEER instead: keepalive probes (http.keepAliveIdle,
//     http.keepAliveInterval, http.keepAliveCount) end a connection whose
//     peer stopped answering a minute ([TransferStall]) after the last thing
//     it heard, and a peer that answers is waited on to RemoteDeadline
//     whatever it sends, since no client tells a server working quietly from
//     one that has wedged. NEVER libcurl's low-speed check
//     (http.lowSpeedLimit): its least limit, a byte a second averaged over
//     five seconds, is exactly what git's own keepalive moves — five bytes
//     every five seconds — so a remote working quietly through a minute of
//     counting or checking was ended though it answered (measured, at
//     remoteKeepAliveProbes).
//   - THE PROCESS'S PROXY AND AUTHORITIES, which the built environment would
//     otherwise drop exactly as a hand-built HTTP transport drops a proxy
//     (internal/httpx). The proxy the process names for the URL
//     (HTTPS_PROXY and NO_PROXY, read as Go's clients read them) is git's
//     http.proxy, dialed as itself, and it resolves the remote — the wall is
//     the operator's there, as it is for the CDN fetch's dialer — and
//     SSL_CERT_FILE and SSL_CERT_DIR, where they are set, are git's
//     http.sslCAInfo and http.sslCAPath, so git trusts what the engine's own
//     clients trust; an SSL_CERT_DIR naming several directories is refused,
//     since libcurl reads one. TLS is verified, git's default, and no key
//     that turns it off is one a call site may set.
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
// The transfer lane is SHARED BY WHO ASKED (share.go), where the other lanes
// grant in arrival order: every transfer carries [Cmd.Share] — its
// credential's class and principal — and when every slot is held, a released
// one goes to the class furthest below its weighted share of what the lane
// has spent over the last [ShareWindow] (a person 4, a machine 2, a coding
// box 1), within it to the principal that spent least, each run charged the
// greater of its CPU and the slot-time it held, a slot-second at the share of
// a core one slot stands for on the lane's shape (a quarter at four a core, a
// whole core on a lane narrower than the cores). With persons and machines
// waiting, people take about two thirds of what the lane spends and
// credentials acting as nobody about one third: a CI system's storm of clones
// or lazy fetches waits behind every credential of its class that asked less,
// and while people have spent less lately than twice what the machines have,
// a person's clone takes the next slot released rather than the sixty-fifth —
// past that, it waits behind the machines' third. Work-conserving for persons
// and machines — a slot is never free while one of them waits, so either
// class alone has the whole lane — while coding boxes together HOLD at most
// half of it ([ShareClass.Cap]), so a person's git is never queued behind
// generated code's client-paced clones; and bounded as arrival order is,
// though not ordered as it is: no waiter waits longer than [SlotWait],
// whichever order it is granted in, but the principal that spent most in its
// class can wait out that whole bound and be answered [ErrBusy] where
// arrival order would have served it.
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
// # Lending a push cores
//
// Every process runs at pack.threads=1, a slot being a core, except in the one
// phase that both threads and has a person waiting: a push's receive-pack
// running index-pack over the pack just pushed. MEASURED with git 2.43 on four
// vCPUs: a 97 MB pack indexes in 12.1 s on one thread and 6.4 s on four; a
// 37 MB pack in 2.8 s on one, 2.2 s on two, 1.9 s on three and 1.8 s on four.
// The receiving phase before delta resolution is single-threaded whatever the
// setting, so the curve is flat past three and [MaxLentCores] is 3. Never
// upload-pack: pack-objects holds pack.windowMemory PER THREAD, which
// [TransfersPerProcess] counts once. Never a push's advertisement
// (receive-pack --advertise-refs): it runs no index-pack, so a loan would sit
// on up to three cores while its client reads the refs and the push's POST
// would be lent nothing; cmd.go's receivePackOptions tells the two apart
// exactly.
//
// Memory: index-pack held 12 MiB at one thread and 23 MiB at four on the 37 MB
// pack, 25 and 48 MiB on the 97 MB one. The bound is larger: index-pack caps
// its delta base cache at core.deltaBaseCacheLimit TIMES its threads, so each
// lent thread may hold up to 96 MiB (git's default) more than
// TransfersPerProcess counts. Loans never exceed the read lane's width, so a
// node's worst case grows by under 96 MiB a core (about 900 MiB a core rather
// than 800), as TransfersPerProcess's doc says.
//
// Sized when the receive-pack starts: the READ lane's free cores (those neither
// a held read nor an earlier loan claims) LESS ONE for the receive-pack's own
// thread, which resolves deltas too. Lent every free core, a push would run a
// thread more than there are free cores, raising the cache cap for no speed.
// An idle four-core node lends three, a two-core node one, a one-core or
// loaded node none.
//
// Lent, never held: a loan holds only its COUNT, which later loans are sized
// against, never the read slots. Held, those slots would sit idle through the
// client's upload (hours at the transfer floor) and the engine's hook, so a
// few slow pushes would close the read lane with the CPU idle, and the push's
// own hook reads would queue behind its own receive-pack. A read that takes a
// lent core shares it with the loan's threads; the overcommit is bounded by
// the loans out.
//
// Repaid at [Cmd.Indexed]: pack.threads is fixed at process start, so unrepaid
// a loan's count would cover the whole run. Every push's receive-pack call site
// closes Cmd.Indexed when the proc-receive hook first reaches its relay
// (receive-pack starts the hook only after index-pack exits), so the
// object-store upload (up to 1.25 GiB) and the wait for the record hold no
// loan. A push's receive-pack without Indexed is refused on every node, even
// one with nothing to lend: a call site that forgot it would park every loan
// through upload and record wait, so later pushes would index on one core,
// while its own tests, on a node with nothing to lend, still passed. The gauge
// crewlet.code.git.lent_cores shows the cores lent now, so pushes indexing on
// one core because loans are parked in slow uploads are seen.
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
// long it ran. (The transfer lane's share does read a window, from the same
// figure: a transfer's CPU is charged to its principal at its reap, and the
// slot-time it holds accrues while it runs, so a share reads a principal
// holding every slot with hour-long clones as spending all along.)
//
// # What went wrong is classified from git's own format strings
//
// Every process the pool runs — and everything it starts — writes git's
// trace2 event stream to a pipe the pool gives it as fd 3
// (GIT_TRACE2_EVENT=3, brief, nesting 1), and every report git makes (die,
// error, warning, BUG) is an `error` event carrying its FORMAT: the constant
// git's source passed, beside the message with its arguments filled in. The
// run is classified by the formats its events carry, looked up whole — every
// printf conversion read as one, so a platform's PRIuMAX matches — in a table
// of git's own formats built from real damaged repositories and held, per git
// version, both ways (classify_test.go): a pack, its index or reverse index, a
// multi-pack-index or its bitmap, a commit-graph, or a chunk-format file git
// could not read ([ErrCorrupt], with the [Part] it named), an object git could
// not find ([ErrMissingObject]), a want upload-pack does not serve
// ([ErrNotOurRef] — never a rebuild trigger, since an absent want and a cache
// that lost a pack print the same line), or none of those ([ErrFailed]).
//
// NEVER BY THE MESSAGE, AND NEVER BY STDERR, because upload-pack copies the
// client's request text into the messages it dies with — "unexpected line:
// '<arg>'", a filter spec, an unparsed want, newlines and all — so reading the
// words let any token that may fetch force a cache to be read whole again
// from the object store. The request's text lands only in an event's message;
// the 21 formats a request was measured to reach (74 forms on four gits) are
// constants of git's, none a row. stderr stays the human's message
// (Result.Stderr, LastLine). A line of the stream past [MaxLine] is skipped
// whole (an echo can be 390 KB; no damage event is near 1 KB), and a git that
// writes no stream is refused at [New] ([ErrNoTrace]). The one damage git 2.56
// names only through a bare "%s" — a corrupt packed object — is a [Hint]
// naming a pack, which decides nothing until that pack's bytes are held to its
// row (the cache's ConfirmHint); the same sentence is a row on 2.55,
// a pack's damage, and the pack its message names is recorded as a hint
// beside it, so the repair can fetch that pack again rather than look for it.
//
// WHAT A RUN NAMED IS REPORTED WHETHER OR NOT IT FAILED. git reads around
// most damage to a derived file — it warns, and answers from the packs — and
// a caller that got its answer never looks at the warning; so every run whose
// process started is handed to the pool's one observer ([Pool.Observe]) with
// its Result, a reservation's included, and the cache that installed it
// repairs what the run named. [Result.Began] says when the process started,
// on the monotonic clock, so a report about a file the cache has rewritten
// since is told from one about the file it holds now; and [Cmd.Repairer],
// carried to the observer as it was given, says which caller repairs what the
// run failed with itself, so the cache leaves that report to that caller's
// repair rather than repair the same damage a second time beside it.
//
// A CRASH IS REPORTED TOO, and is no kind. A pack-objects reusing entries by
// a damaged reverse index dies of SIGSEGV on every git, and its upload-pack
// says only that it died ("%s died of signal %d", run-command's report of any
// child; 2.56 also "pack-objects died of signal %d") — no format names the
// file. [Result.Crashed] is that a process of the run's tree died of a signal
// the POOL DID NOT SEND: the leader, never for a run the pool killed (its
// deadline, a stall, the caller's context) or one whose caller's own input or
// output failed first, and never of SIGPIPE, a consumer hanging up, which git
// itself leaves out of both reports; or a child, by those two formats
// ([Trace.Crashes]). The cache reads it as a reason to hash what the process
// read, never as damage of a part.
//
// A run that names two kinds is BOTH: an [ExitError]'s Kind is the highest,
// and errors.Is reaches every kind the run named ([Trace.Kinds]), so a
// question about one is never answered by another that outranks it — a force
// check's missing old commit beside a multi-pack-index git reads around is
// still a stale update. Beside damage to a pack, its index or its reverse
// index it is that damage's, since the commit may be in the pack git could
// not read ([Damage.Derived] tells the two; internal/gitlet/host decides).
//
// It costs git's write of each event and its read of /proc for its ancestry:
// MEASURED against the build before it, a warm fetch's four processes take
// 1.5 ms more of the node's git CPU — 10.5 → 12.0 ms, +15%, about 0.3–0.4 ms
// a process — and eight clients at once fetch 5% fewer a second
// (internal/gitlet/host's measured section, its one figure of git 2.47.3: the
// build without the stream is not this code, and every figure there of the
// node pin includes the stream), while a clone's
// share is lost in its own spread; the planning pass, timing single
// processes on 2.43, 2.47 and 2.56, had put it at 0.16–0.30 ms a process. The
// reader here costs 1.4 µs for a healthy fetch's stream, 5.2 µs for one
// carrying three error events and 40 µs to skip a 390 KB echo
// (BenchmarkTraceSink). That is the price of a classification no request can
// steer; "Measured" has the pool's own arms re-measured.
//
// # Formats left out of the table
//
// Formats read from git's source and deliberately not rows of formatTable
// (classify.go), each for its reason:
//
//   - `invalid commit position. commit-graph is likely corrupt`: every path a
//     commit position from a file reaches it by is checked first and reported
//     as `invalid parent position %u`, a row.
//   - `commit-graph overflow generation data is too small`: it needs a
//     generation-overflow chunk, which a test-sized repository never has, and
//     building one by hand is building the file.
//   - `unable to read index` (show-index's), `missing %s %s` and `broken link
//     from`: fsck's findings, written to its stdout, not reported.
//   - `could not find object in pack %s at offset %lu in MIDX`: a BUG
//     pack-objects reports, dying of SIGABRT, only beside `bad offset for
//     revindex` (a row) and the run's crash ([Result.Crashed]).
//   - `object %s is a %s, not a %s` and `unable to parse object: %s`: a
//     healthy repository prints both, for an id of the wrong type and for a
//     name that is no object.
//   - `unable to find all commit-graph files` and `unable to find all
//     multi-pack index files`: a reader racing git's own split or incremental
//     write prints them of a healthy repository
//     (TestNoRowIsAFormatARequestWrites).
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
// The trace stream, re-measured against the build before it — the same arms,
// six counts each alternated between the two trees, medians, on four vCPUs of
// a 2.1 GHz Xeon shared with other builds, git 2.43.0: `git version` through
// the pool 1.84 → 2.03 ms and a rev-parse 2.20 → 2.30 ms, 0.1–0.2 ms a
// process of wall; the read lane saturated 0.71 →
// 0.76 ms a process, about 1 320 a second rather than 1 400; the clone of the
// 18 MiB pack 93 → 96 ms; and a scratch's rev-parse, the thousand-id cat-file
// and the copy path each moved less than its own run-to-run spread (a single
// arm's six runs span 0.3–1.3 ms of a 2–3 ms process here). What it costs a
// request is Gitlet's (internal/gitlet/host, its warm fetch).
//
// The transfer lane's share (BenchmarkTransferShare: real upload-packs of the
// same 18.2 MiB pack to a reader that never waits, the lane as wide as the
// cores so its slots and not the CPU they share are what a clone waits for;
// four E-cores of an Intel Core Ultra 7 265U shared with other builds, git
// 2.56.0, one count of twenty clones an arm, so a p99 is the slowest of the
// twenty): a person's clone alone takes 56 ms at the median and 62 ms at the
// p99; beside sixty-four clones at once of ONE machine token, 87 ms and
// 125 ms, of which the wait for its slot is 60 ms at the p99 — one release's
// turn, the rest its run sharing the cores with the three clones beside it,
// which no order of granting changes — where the same lane granting in
// ARRIVAL ORDER makes it 1.19 s and 1.54 s, 1.46 s of it waiting behind the
// sixty-four. The machine token alone runs the lane at 56 clones a second
// with the share and 55 without it: a weight decides who goes next, never
// whether a slot is used. A person whose own burst of sixty-four clones is a
// minute old waits 187 ms at the p99 behind the token's sixty-four, and three
// minutes old 67 ms, about as one who spent nothing does — what [ShareWindow]
// is for (a held slot charged a whole core a second, as on every lane as wide
// as its cores). One grant of a lane 10 000 waiters deep over 1 000
// principals costs 34 µs (BenchmarkTransferGrant, one count of 10 000
// grants), so the scan over the waiting principals stays a scan: under a
// grant's 100 µs budget, where a heap would be owed past it.
//
// # Two floors, one pin (ADR-0087)
//
// A Gitlet node runs git 2.55 or newer ([NodeMinVersion], the node floor),
// checked where a pool is built — [New], and nowhere else, since every git a
// Gitlet node runs is a pool's — and refused there as an [*ErrTooOld] naming the
// floor. A git that only TALKS to a Gitlet node — a person's, a CI runner's, the
// operator CLI's, the suite's client — needs 2.40 ([ClientMinVersion], the
// client floor), checked where the engine meets such a git (gittest.Require,
// and the CLI before it runs one). Two numbers because the two gits are
// different people's: a client's is a laptop or a CI image the operator does
// not choose, a node's is the engine's own. The engine image does not install
// Debian's git (trixie ships 2.47, below the node floor): it BUILDS the
// newest git release from the tarball scripts/git-pin.env pins
// (scripts/git.sh, the recipe the suite's `make git` runs too), so a git CVE
// is an engine CVE on a Gitlet node, and what reaches the image is a bump of that
// pin — Debian's security updates to its own git never do — which a weekly
// check (.github/workflows/pins.yml) asks for within a week of upstream
// releasing one, or at once when it is run by hand (the workflow's Run
// workflow, or scripts/git-pin-check.sh), which is how a git security release
// is taken; each release's signature is verified against the git maintainer's
// key before its digest is named. The suite runs that pin; every case here
// whose subject is what git says or does in a pool — the format table, what a
// request can reach, what the environment keeps from git — runs on the node
// floor's own build as well; and one arm of Gitlet's suite runs the
// node floor's build against the client floor's — so each floor is a measured
// claim rather than a number, and a row or an expectation only a git below the
// floor could meet is deleted rather than kept where no run can say whether it
// is still right.
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

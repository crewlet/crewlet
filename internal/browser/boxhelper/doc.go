// Package boxhelper is the helper the engine writes into a seat's box to run
// the seat's own browser ([Write]), and the Playwright tree the box installs
// for it: @playwright/mcp at an exact release, locked in tree/ with the two
// packages it pins, which the box's own npm installs. Its ensure leaves one
// supervisor in the box, holding Chromium through Playwright for the box's
// life and serving it to the model through Playwright MCP, embedded in it,
// behind the shim, the harness's stdio browser server ([ShimArgs]). The
// helper runs under the box's Node, of [MinNodeMajor] or newer with an npm of
// [MinNPMMajor] or newer, and its suite runs it at both ends, the floor's
// pinned release beside the machine's own (boxhelpertest; CONTRIBUTING.md,
// "The floor Node" and "Which node"); the engine links none of Playwright's
// code.
//
// # The helper's files
//
// ECMAScript modules, js/*.mjs, on Node's built-ins and the installed tree
// alone, embedded ([Files]) as the OpenCode plugins are, so a box runs the
// helper this build's suite ran: helper.mjs, the entry, whose subcommands are
// ensure and stop (ensure.mjs), supervise (supervise.mjs; serve.mjs, its
// socket; fence.mjs; and the live view it attaches, the modules of
// internal/browser/boxhelper/live, written between the helper's and the
// entry), mcp (shim.mjs) and relay (live's, a job's relay, [RelayScript]),
// beside chromium.mjs, tree.mjs and common.mjs.
// No build step stands between them and the binary: [Write] puts each into
// the box's [Folder] whole, after every module it imports; then the lock's two
// files, as tree-package.json and tree-package-lock.json, names neither npm
// nor Node takes as the folder's own; then config.json, the one document the
// engine and the helper agree through, rendered from this build's constants
// and a launch's [Settings]: the helper's [Version], over every module's name,
// length and bytes; the hello's protocol; the pin and both digests; the
// floors; the Chromium to look for; the cell's sandbox; the switches; the
// supervisor's environment by name; and every bound, which a case about a
// bound shortens in its own config. config.json goes last, so it
// never names a file not yet beside it, and the next launch's write repairs a
// box whose files disagree with it. Modes are the box's.
//
// # Ensure
//
// The engine's last check before a seat's harness starts
// ([EnsureCommand]), in a login shell as a job's, so it finds a job's node. A
// POSIX sh prelude answers node_missing and node_too_old (a major below
// [MinNodeMajor], compared as a number, a prerelease allowed) before Node
// loads the helper, and clears NODE_OPTIONS, which loads code into every node.
// Ensure prints one line of at most 512 bytes, read by [ParseEnsure] into
// internal/browser's closed values: ready, with what runs, or withheld, with
// the first failed check's reason and cause, a pair of common.mjs's PRINTS,
// which TestTheHelperPrintsTheReasonsTheEngineReads holds to that table.
// Nothing else a box prints reaches a prompt.
//
// Its checks, in order: the folder, the box user's, held at 0700 and its files
// at 0600 before any is read, short enough for its sockets' paths; ensure.lock,
// made exclusively, so two launches never start two supervisors (a younger
// holder is waited for, at each change in the folder and every second, which
// alone wakes it where the kernel refuses a watch; one that is no ensure or
// stop of this helper, or past lock_takeover_ms, killed first, is taken over,
// one ensure at a time under ensure.lock.takeover); the tree; the Chromium;
// the supervisor. A write the folder refuses is folder_unusable, never a missing
// line. Its own deadline, ensure_deadline_ms, comes five seconds before
// [EnsureTimeout], as on the container cell the engine's bound ends only the
// exec client: during the install, npm is killed at once and given a second to
// be reaped (playwright_missing timeout); later, supervisor_failed timeout.
//
// A supervisor already running as this launch needs it is found: the kernel
// reports it running this helper's supervise on this folder (a recycled pid is
// none), ready or launching again, its socket taking a connection, of this
// launch's digest, over the canonical JSON of the whole launch and its
// environment, so a changed bound, tree or variable restarts it. Any other is
// stopped, TERM then its group killed, and one started detached in a session of
// its own, which writes ready or failed on fd 3 within supervisor_wait_ms. Then
// every other entry of pw/ is swept, but one whose recorded npm still runs.
// Stop ([StopCommand]) ends the live supervisor so, under ensure.lock, where a
// launch withholds a box's browser for its network.
//
// The supervisor's environment is a list of what may pass:
// hostbox.PassthroughEnv and hostbox.HostUserEnv's names, which config.json
// carries, and Playwright's browsers directory on the channel route. Several
// of the hundred variables Playwright reads change how the browser runs
// (PWDEBUG makes it headed, DEBUG_FILE writes a page's words to a file), so
// only the spawn keeps them out; one a Chromium needs belongs on that list.
//
// # The tree, installed by npm in the box
//
// A box gets Playwright MCP the way the catalogue's npm servers reach one: its
// own npm installs it, from npm's registry, the mirror the box's npm
// configuration names (npm sends the lock's registry.npmjs.org addresses
// there), or an npm cache. js/tree.mjs installs it at
// pw/<release>-<digest>-l<layout>/, named by what decides its bytes. First an
// install a dead ensure left is reaped, its npm's group killed only while the
// kernel still reports the pid with the start time its record names. Then a
// tree published at the name is reused with nothing more read, so a box that
// has one needs no npm. Otherwise the folder's lock files must be what
// config.json names, the box's npm must answer --version at or above npm_floor,
// and `npm ci` runs into a directory of its own, under umask 077, in a process
// group of its own, with the whole environment the helper was given. Its
// command line carries every setting that decides what is installed, since it
// beats the environment and every npmrc, and the log level that decides what
// the helper reads of npm, its --json answer; where the bytes come from,
// registry, cache, proxy, CA and credentials, stays the box's. It is bounded:
// TERM at install.timeout_ms, then the group killed after stop_grace_ms, since
// npm defers its exit to an outstanding fetch, and npm is reaped before its
// directory goes.
//
// npm's status proves nothing: under a setting the command line missed it
// answers 0 having installed nothing, and a file it could not write is only a
// warning to it. So before .complete is written and the directory renamed into
// place, npm's own record (node_modules/.package-lock.json) must equal the
// lock, nothing in the directory may be a link, and the digest of every file
// installed must be installed_digest. A failure publishes nothing and names its
// cause: digest_mismatch, npm_missing, npm_too_old, registry_unreachable (not
// reached, failing on its side, or in TLS), registry_refused (a mirror lacking
// the release or wanting a credential), integrity_mismatch (a tarball the
// lock's SHA-512 or zlib refuses), timeout, no_space, unwritable or
// install_failed. A box that must start with no registry brings an npm cache an
// image filled with `npm ci` of the lock, never a node_modules, which npm
// cannot vouch for: kept outside the home a container box mounts over its
// image's, named in the image's global npmrc, and writable by any user, since
// npm writes what it fetches into the cache it reads. The tree is loaded, never
// run: @playwright/mcp from it, and playwright-core through mcp's own resolver,
// so the helper and mcp's server share one core.
//
// # The supervisor
//
// The browser's parent, one to a box, for the box's life: it launches Chromium
// through playwright-core's launchPersistentContext on the folder's profile,
// over a pipe, so no DevTools port opens. Before each launch it kills the
// group of a Chromium still holding the profile, as one that died outright
// leaves it: the one the profile's SingletonLock names, where that is this
// host and a process on this profile; another host's lock is left alone, and
// the launch says profile_held. A browser that ends while its launch is read
// is a failed launch, never served; one gone without a stop is launched again,
// at most relaunch.max times in any relaunch.window_ms, past which it has
// crashed out until the next ensure, the model refused saying so. On TERM it
// closes its socket and model connections, then the browser through
// Playwright, so Chromium writes its profile, killing its group past
// stop_grace_ms (a frozen browser never answers); a TERM as it launches waits
// for the launch and serves nothing, and one as it begins serving waits for
// that step, closes what it opened and never says ready. Killed outright, it
// takes its browser too: Chromium exits as its pipe closes.
//
// Before anything loads it installs its own policy: an unhandled rejection is
// logged and it carries on, since Playwright MCP listens for one only while a
// model is connected and Node's default would end the browser between jobs; an
// uncaught exception records it failed and exits 70. Its umask is 077.
// supervisor.json, written whole, a failed write costing only itself, is
// ensure's source; supervisor.log, 1 MiB with one rotation, logs refusals by
// kind and errors by message, never a page's words. Every timer runs on the
// clock it is handed.
//
// Attachments, its caller's, add what else it serves: each is attached once
// browser.sock listens and before ready; told each browser launched, each
// state, the model connection's job, and each browser_tabs new, select or
// close answered without error; and closed first on a stop, waited for by
// nothing. One that fails is logged and the supervisor serves on.
// supervisor.json's boot, drawn once, outlives every relaunch.
//
// # The launch
//
// New headless, the full browser (never chrome-headless-shell); Chromium's own
// sandbox on unless the cell waives it, where Playwright's default is off; a
// 1280×800 viewport; an artifacts directory in the folder, emptied each
// launch; a launch bound under ensure's wait; Playwright's signal handlers
// off. Its switches are Playwright's defaults less three that suit tests and
// weaken a browser of untrusted pages, removed by exact string:
// --disable-popup-blocking; --enable-unsafe-swiftshader, so WebGL needs a GPU;
// and Playwright's --disable-features list, given again less HttpsUpgrades,
// HttpsFirstBalancedModeAutoEnable and ThirdPartyStoragePartitioning.
// TestChromiumRunsWithTheListedCommandLine holds the whole command line, as
// the kernel holds it, to the list, so a pin that changes Playwright's
// defaults fails there until somebody reads it, and, as Playwright refuses a
// caller's pipe and profile switches but not a DevTools port, a port stays out.
//
// Under required, Playwright's switch proves nothing of a launcher on PATH,
// which may add --no-sandbox (Debian's reads /etc/chromium.d): a browser whose
// command line, read from the kernel by the pid its profile's lock names,
// carries it is withheld as chromium_sandbox_unavailable launcher, and one
// whose command line cannot be read as unknown. A failed launch says why.
// Playwright's words for a sandbox that would not start, which discard
// Chromium's, are chromium_sandbox_unavailable with the first cause the box
// shows: root; Ubuntu's AppArmor restriction on user namespaces; user
// namespaces off; a seccomp filter on the process; else unknown. The rest are
// chromium_start_failed, by the markers Playwright's errors carry: a library
// missing, a profile held, the bound passed, a browser that ended.
//
// # The Chromium it runs
//
// In the order a build is trusted (config.json's chromium.search): the build
// the pinned Playwright expects, its revision wholly installed in a Playwright
// browsers directory and answering the version expected, through the chromium
// channel (build expected); a branded Chrome by the chrome channel; a Chromium
// on PATH by name (never chromium-browser, Ubuntu's snap shim); any other
// revision there, newest first, by its path (untested). A candidate's version
// is the first four-number word of its answer; one silent for 10 s is passed
// over. The order is data, so the suite searches a box's own layout and PATH
// alone.
//
// # Serving the browser
//
// browser.sock is a filesystem socket in the folder, at 0600, never abstract. A
// connection says its hello within hello_ms (its helper's version, job and
// root) or is refused: helper_mismatch, bad_hello, or not_ready before the
// first launch is served; one during a relaunch is accepted, as ensure answers
// found then; one that closes before a byte is ensure looking. An accepted one
// is the box's one model connection (a box runs one job at a time), closing the
// one before, whose Playwright MCP disposes only what it isolated, so the
// browser and its tabs stay. It is handed to createConnection over
// newline-delimited JSON of at most 32 MiB a line, what came with the hello
// read first, the context through the getter, which awaits a launch under way
// and refuses a crashed-out browser. The config turns WebMCP off, on by default
// on this path; shares the context, so browser_close refuses; bounds the
// outputs a model leaves unnamed at 256 MiB; and states the pin's call bounds.
//
// Every message passes the edits. The model is shown, and may call, only the
// allowed tools: the pin's core tools less browser_run_code_unsafe, which runs
// code in the supervisor's own process. A list of what may pass, as the server
// also serves the tools a page registers, and a later pin may add more; a call
// off it is answered -32602, its name neutralised, never forwarded. Each text
// item of an answer is fenced, an image kept, anything else and
// structuredContent dropped; a server's error keeps its words, neutralised, and
// loses its data; a request of the server's but roots/list and ping is answered
// -32601 in the model's place; progress loses its words; any other notification
// but cancelled and list_changed is dropped; an answer past 32 MiB becomes an
// error naming its size. A call loses arguments._meta, the Playwright CLI
// daemon's channel into a call, whose cwd would re-base the files a model
// names. A line that is no JSON-RPC message or runs past the bound, or an id
// reused in flight, ends the connection. A table of the calls in flight and the
// one place a call is answered in the server's place are the hooks a person's
// hold builds on.
//
// # The fence
//
// fence.mjs ports internal/provenance's Fence, Neutralise and NewBoundary: each
// text item is one fence of the connection's own boundary, a page in a seat's
// own browser (browser_page), at external trust, no author named. The box runs
// no Go, so a case holds the port to provenance.Fence byte for byte. What the
// server writes to files (snapshots and console entries in the output
// directory, downloads, files the model names) the model opens with its own
// tools, past every fence.
//
// # The shim
//
// The harness's browser server is `helper.mjs mcp --job <launch> --root
// <workspace>` ([ShimArgs]): no token, so every session's configuration holds
// the same entry, and no variable read. It says its job's hello, then relays
// newline-delimited JSON byte for byte but for two messages: an initialize
// declaring no roots gains them, and the server's roots/list it answers itself
// with the job's root. The pin takes the first root as its working folder, so a
// relative file name the model gives lands in the job's workspace; an absolute
// one is taken as given; either is admitted only there or in the output
// directory. It exits 0 when stdin or the socket closes, 69 where no browser
// listens or the hello is refused.
//
// # The box's claim, not a boundary
//
// The model holds a fully permissioned shell in its box, as the box's user, so
// it can read and write all of this, run any npm, edit the tree, and be the
// shim or the supervisor itself. The one supervisor, its allowlisted
// environment, npm's command line and the check of what it installed hold the
// box's own configuration to the lock; the edits, the fence, the one
// connection and the tools held back label a page's words and coordinate a
// cooperating model. None is a boundary, and the engine decides nothing from
// what the box says.
//
// # The lock is the release somebody read
//
// A bump of the tree is code every seat's browser runs, so it never lands
// unread. internal/browser names the releases somebody read
// (PlaywrightMCPRelease, PlaywrightCoreRelease) and the Chromium build that
// playwright-core expects (ExpectedChromiumRevision, ExpectedChromiumVersion),
// literals never read from the lock. TestTheLockIsTheReleaseSomebodyRead holds
// the manifest and the lock to the releases: four entries, each package at its
// release and the edges between them exact, every address the registry's own
// and every integrity a SHA-512, no package with a script, a platform, or an
// optional, development or peer flag, and no engines.node above [MinNodeMajor].
// TestTheLockedCoreExpectsTheChromiumSomebodyRead holds the expected build to
// the browsers.json of the tree the suite installs from the lock
// (boxhelpertest.Tree), the only place it is written.
//
// tree/installed.sha256 is the digest of the files a whole install of the
// lock holds (js/tree.mjs's installedDigest: each regular file's path and
// SHA-256, npm's own record aside, no mode or directory), so a box runs the
// bytes the suite ran. scripts/playwright-tree.sh holds the suite's tree to a
// box's check, importing tree.mjs, and refuses a bump's new tree naming the
// digest its files have, the value the bump writes;
// TestTheInstalledDigestNamesTheFilesTheLockInstalls computes it again, in
// Go, from the suite's tree.
//
// A bump lands as CONTRIBUTING.md says ("A Playwright MCP bump is read before
// it lands"). Its signature check (`npm audit signatures`) fetches the
// registry's keys when it runs rather than reading a pinned one, the price of
// a check npm maintains.
//
// # Why Playwright MCP is embedded, not run
//
// Playwright's other way would bind the launched browser (browser.bind) and
// point the stock @playwright/mcp command line at its endpoint, one per job.
// Measured on 2026-11-19, that works on both pinned Chromiums: the bound
// context and its tabs outlive the command line, and a second one attaches to
// the same tabs. It is refused for what bind leaves: a raw Playwright
// endpoint, a socket outside the helper's 0700 folder that any process in the
// box may attach to, any number at once, so the model's own scripts could
// drive the browser around the helper. Embedded, every connection passes the
// helper, and nothing raw listens. The cost: createConnection's context
// getter is undocumented, so every bump reads it again.
//
// # The suite
//
// A case runs ensure as the engine does, at each end of what a box may bring
// (boxhelpertest.Each), or drives the modules through boxhelper_probe.mjs on a
// clock of the probe's own, so a grace or ten minutes of relaunches pass at
// once. boxhelpertest's package doc says how a box gets its npm, cache,
// registry and Chromium, and how a host's sandbox is told.
package boxhelper

// Package boxhelper is the helper the engine writes into a seat's box to run
// the seat's own browser ([Write]), and the Playwright tree the box installs
// for it: @playwright/mcp at an exact release, locked in tree/ with the two
// packages it pins, which the box's own npm installs. The helper runs in the
// box under the box's Node, of [MinNodeMajor] or newer with an npm of
// [MinNPMMajor] or newer; the engine writes it and links none of Playwright's
// code.
//
// # The helper's files
//
// ECMAScript modules, js/*.mjs, on Node's built-ins and the installed tree
// alone, embedded ([Files]) as the OpenCode plugins are, so a box runs the
// helper this build's suite ran: tree.mjs, which installs and loads the tree,
// and common.mjs, what the modules share. No build step stands between them
// and the binary: [Write] puts each into the box's [Folder] whole, after every
// module it imports; then the lock's two files, as tree-package.json and
// tree-package-lock.json, names neither npm nor Node takes as the folder's
// own; then config.json, the one document the engine and the helper agree
// through, rendered from this build's constants: the helper's [Version], over
// every module's name, length and bytes; the pin, the lock's digest and the
// digest of its whole install; the npm floor; and every bound of an install,
// which a case whose subject is a bound shortens in its own config. config.json
// goes last, so it never names a file not yet beside it, and a box whose files
// disagree with it is repaired by the next launch's write. Modes are the
// box's to fix: an upload service gives a file its own.
//
// # The tree, installed by npm in the box
//
// A box gets Playwright MCP the way the catalogue's npm servers reach one: its
// own npm installs it, from npm's registry, the mirror the box's npm
// configuration names (npm sends the lock's registry.npmjs.org addresses
// there), or an npm cache. js/tree.mjs installs it at
// pw/<release>-<digest>-l<layout>/, named by what decides its bytes. First an
// install a dead helper left is reaped, its npm's group killed only while the
// kernel still reports the pid with the start time its record names (a pid
// handed out again is left alone). Then a tree published at the name is reused
// with nothing more read, so a box that has one needs no npm. Otherwise the
// folder's lock files must be what config.json names, the box's npm must
// answer --version at or above npm_floor, and `npm ci` runs into a directory
// of its own, under umask 077, in a process group of its own, with the whole
// environment the helper was given. Its command line carries every setting
// that decides what is installed, since it beats the environment and every
// npmrc, and the log level that decides what the helper reads of npm, its
// --json answer; where the bytes come from, registry, cache, proxy, CA and
// credentials, stays the box's. It is bounded: TERM at install.timeout_ms,
// then the group killed after stop_grace_ms, since npm defers its exit to an
// outstanding fetch, and npm is reaped before its directory goes.
//
// npm's status proves nothing: under a setting the command line missed it
// answers 0 having installed nothing, and a file it could not write is only a
// warning to it. So before .complete is written and the directory renamed into
// place, npm's own record (node_modules/.package-lock.json) must equal the
// lock, nothing in the directory may be a link, and the digest of every file
// installed must be installed_digest. A failure publishes nothing and names
// its cause: digest_mismatch, npm_missing, npm_too_old, registry_unreachable
// (not reached, failing on its side, or in TLS), registry_refused (a mirror
// lacking the release or wanting a credential), integrity_mismatch, timeout,
// no_space, unwritable or install_failed. A box that must start with no
// registry brings an npm cache an image filled with `npm ci` of the lock,
// never a node_modules, which npm cannot vouch for: kept outside the home a
// container box mounts over its image's, named in the image's global npmrc,
// and writable by any user, since npm writes what it fetches into the cache it
// reads. The tree is loaded, never run: @playwright/mcp from it, and
// playwright-core through mcp's own resolver, so the helper and mcp's server
// share one core.
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
// bytes the suite ran. TestTheInstalledDigestNamesTheFilesTheLockInstalls
// computes it again, in Go, from the suite's tree: a bump regenerates it by
// writing the value that test names once the suite's tree is the new lock's.
//
// Dependabot proposes each release (.github/dependabot.yml), and its pull
// request stays red until a person reads the release, checks the registry's
// signatures on what it installs (`npm audit signatures`) and moves the
// constants and the digest, from a branch of their own (CONTRIBUTING.md, "A
// Playwright MCP bump is read before it lands"). Nor does it merge itself:
// .github/workflows/dependabot-merge.yml, which queues every other bump to
// merge once green, starts no run on one that changes only tree/. The
// signature check fetches the registry's keys when it runs rather than reading
// a pinned one, the price of a check npm maintains.
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
// # The floor Node
//
// A box brings its own Node and npm, and the helper supports Node
// [MinNodeMajor] or newer with the npm that major bundles, [MinNPMMajor] or
// newer. So boxhelpertest hands a case of the helper two Nodes, each with its
// own npm: the machine's, at or above both, and the floor's newest release,
// of both majors exactly, which scripts/node-pin.env pins by version and
// per-platform SHA-256 and `make node` (scripts/node.sh) fetches, keeping its
// node and its npm. Without the floor's run the suite would prove the newest
// Node alone, and a floor that only CI ran would let a developer's `make
// check` pass on code the floor rejects: the git and Chromium floors take the
// same posture. TestTheLockInstallsWithEachNodesOwnNpm installs the lock with
// each, offline, from the npm cache of the suite's tree.
//
// It is the one Node version the tree names (CONTRIBUTING.md, "Which node"),
// because a weekly check moves it: .github/workflows/pins.yml's leg
// (scripts/node-pin-check.sh) holds the pin to the newest patch of its major,
// verifying each release it proposes against the Node releasers' signing
// keys, and fails from 60 days before the major's end of life, naming the
// release to raise it to: the newest of the oldest major above it in
// long-term support whose own end of life is more than 60 days away.
//
// # The suite
//
// A case drives the modules as the engine writes them, in a box laid out on
// disk, on each Node with its own npm, through boxhelper_probe.mjs:
// boxhelpertest's package doc says how a box gets its npm, cache and registry.
package boxhelper

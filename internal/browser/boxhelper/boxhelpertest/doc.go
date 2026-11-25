// Package boxhelpertest stands a seat's box up on disk for a case of the box
// helper: the helper written into it as the engine writes it, beside the bin
// directory, npm cache and npm settings of the box's own and a pinned
// Chromium laid out for ensure to find ([Start]); ensure run as the engine
// runs it and read with the engine's parser ([Box.Ensure]), its supervisor
// real and stopped at the case's end; the browser reached as a harness
// reaches it, through the shim ([Box.MCP]); the helper's modules driven under
// the box's Node through the probe beside boxhelper's tests ([Box.Probe]); a
// stand-in registry ([Registry]) and a stand-in npm ([Box.StandInNPM]) for
// what a box's npm meets; and the Playwright tree the suite installs from the
// committed lock ([Tree]). A case runs at each end of what a box may bring
// ([Each]): the machine's node and npm ([Node], [NPM]), at or above
// boxhelper.MinNodeMajor and MinNPMMajor, with the newest pinned Chromium, and
// the floor's pinned build with the npm it bundles ([Floor], [FloorNPM]), of
// those majors exactly, with the floor's Chromium, the worst pairing the
// floors admit. A case that drives no browser runs on each Node alone
// ([EachNode]), and one whose doc says why the floor adds nothing on the
// newest pair alone ([Newest]). CONTRIBUTING.md says why the floor is pinned
// ("Which node").
//
// Test support (the name ends in `test`), so internal/fixedwait reads it as
// test code: no wait in it decides a claim, every bound in it only ends a
// wait, and its polls, on what a supervisor records and a relay logs, are
// paced as liveness.
//
// # Where each comes from
//
// The machine's node and npm are found on PATH, as internal/prereqtest finds a
// program the suite needs: under the make targets that run the suite a missing
// one fails the case, and only a bare `go test` outside make skips it. Every
// make target that runs the whole suite (`test`, `test-solo`, `test-norace`)
// fetches the floor's build (`make node`, the one recipe scripts/node.sh),
// which keeps the npm its release bundles beside its node, and names it in
// CREWLET_TEST_NODE_FLOOR, under the same rule. The floor's npm is its
// npm-cli.js, which a case runs with the floor's node, or links as a box's
// bin/npm, whose `env node` then runs the box's own node, since the box's
// PATH is its bin alone.
//
// Each is held by what it answers to --version: the machine's at or above the
// floor, the floor's of its major exactly, since a newer one in the floor's
// place is a floor nobody is testing. It is asked without NODE_OPTIONS, which
// a Node refuses to start under when it names an option that Node predates,
// and without npm's own settings from the environment, every variable whose
// name begins npm_config_ in any case, as npm reads them: npm 10 and 11 end,
// printing nothing, under a loglevel they do not know.
//
// # The tree the suite installs
//
// The same targets install the tree internal/browser/boxhelper/tree locks
// (`make playwright-tree`, the one recipe scripts/playwright-tree.sh), once per
// lock, and name it in CREWLET_TEST_PLAYWRIGHT_TREE, under the same rule. It
// holds npm's install of the lock in node_modules, where the expected
// Chromium's build is written (playwright-core's browsers.json), and the npm
// cache that install filled in npm-cache, from which a case installs the tree
// again with each Node's own npm, offline: a box's install without a registry.
// The recipe publishes it only once it passes the box helper's own check of a
// box's install (js/tree.mjs's checkInstalled), against the lock and
// tree/installed.sha256, so the suite's tree is held as every box's is. A
// tree whose package-lock.json is not the committed one byte for byte is
// another lock's, a variable left from before a bump, and fails naming the
// recipe.
//
// # A box on disk
//
// A box's home is a short directory of its own in the temporary directory
// (os.MkdirTemp, named cb and digits), never a t.TempDir: one named after a
// long test would put a socket in the box past the 104 bytes macOS bounds a
// socket's path at. Its helper is written by boxhelper.Write, so the suite
// writes exactly what the engine writes, through a writer that replaces each
// file whole by a rename, as a box's own upload service does, and leaves the
// loose modes such a service may (0644 files, 0755 directories), so a case
// sees the helper hold them to its own. config.json is then the engine's with
// the case's overrides ([Options]): the cell's sandbox, waived unless a case
// says otherwise, so a case not about the sandbox passes on a host whose own
// will not start; the box's own browsers directory alone to search; the pin's
// build as the version expected; chromiumtest.LoopbackOnly, which holds every
// launch to loopback; and the case's keys merged over it, a bound shortened
// where a case's subject is that bound.
//
// Its bin directory holds links to the case's node and npm, and, where the
// case puts it there, the pin's build as chromium ([OnPath]), or a case's
// stand-in for one; its PATH is that directory alone, so the box's npm runs
// under the box's node and no case finds the machine's own programs, Chrome
// among them, however the machine's PATH reads. The pin's build is laid out
// as Playwright's installer lays the build it expects ([Expected]): a
// directory named for the expected revision, holding INSTALLATION_COMPLETE and
// a link to the build's unpacked directory (chromiumtest.Unpacked), never the
// whole cached build, beside which a launch writes, while scripts/chromium.sh
// promises an intact build is never written into.
//
// Its ~/.npm is a copy of the suite tree's npm cache, unless the case wants it
// empty, and its environment ([Box.Environ]) points npm at a registry on a
// port nothing listens on, so every box installs for real and offline, a miss
// failing in about 2.4 s, and names a global prefix of the box's own, under
// which npm finds an empty global npmrc as an image's npm finds its own, so
// the machine's never reaches it; a case's variables come after and win. A
// case that wants a registry starts a stand-in on 127.0.0.1, which serves the
// lock's tarballs from the suite tree's cache by integrity, or answers 404 or
// 500, a corrupt tarball that inflates or one that does not, nothing at all, or
// half a tarball, waiting on the request's end and never on the time, and
// records every request. A stand-in npm is an sh script that records each
// command line it is run with, answers --version as the case says, and
// otherwise runs what the case gives it, with the box's own npm at hand.
//
// Ensure runs as `sh -c`, never a login shell, whose profile may rebuild PATH:
// the login shell is the engine's to prove in a box. A case's end stops the
// box's supervisor, TERM as ensure would, waits for it and its browser to go,
// kills what a failed case left on the box (a supervisor on its launch that no
// record names, a process on its profile), and removes the home, which they
// would otherwise write back. Whether Chromium's own sandbox can start is the
// host's, told by [SandboxRuns], an oracle beside the helper: whether an
// unprivileged process may make a user namespace and map itself root in it, as
// Chromium's namespace sandbox does; and where it cannot, the cause the host
// shows, by [SandboxCause], from the kernel's own files.
//
// # The model's side
//
// A case reaches the box's browser as a harness does: [Box.MCP] starts the
// shim from boxhelper.ShimArgs, on stdio, the box's workspace its root and
// working directory, and hands its pipes to [MCPOver], a raw JSON-RPC client
// with no MCP SDK on either end, which fails the case on a line the server's
// side writes that is no JSON-RPC message. It takes pipes rather than starting
// a shim, so a case of another cell drives one it started in a container, over
// `docker exec -i`, with the same client; [Box.Shim] hands a case whose
// subject is the bytes the shim's stdio itself. A case waits on what the box's
// supervisor records ([Box.AwaitSupervisor]), never on the time, and reads
// what still runs on a box's profile ([Box.Browsers]). It reaches the live
// view as a job's relay does, over relay.sock ([Box.DialRelay]): it writes
// the relay's records and reads the supervisor's, each kept in the order it
// came and held to the socket's bounds, and acknowledges each frame as it
// comes where it plays a relay whose posts are answered at once.
//
// # The bridge's side
//
// A case of the relay plays its run's bridge with [FakeBridge]: the browser
// routes under one token, every request recorded, each poll held until the
// case answers it ([FakeBridge.Watch], [FakeBridge.NewHub]) or the request
// ends, never on a timer, and a verb refused, held, answered past what a
// relay reads or cut off as the case says. It is no hub: the engine's suite
// proves the real one. [Box.Relay] writes the job's wiring 0600 and starts
// the relay as the job's script does (boxhelper.RelayScript), under a parent
// sh that waits for it, its pid and its parent's in hand to TERM the one or
// kill the other; [Box.StandIn] plays the supervisor's side of relay.sock.
//
// # What a dead test binary leaves
//
// A home outlives a test binary that dies before its cases' cleanup runs,
// killed at go test's -timeout or for memory, so each binary's first box
// sweeps the homes of every binary that is gone. A home names the binary that
// made it in a file of its own, owner: the binary's pid and start time
// (procgroup.Identify). A home is removed once the kernel no longer reports
// that pid, reports it with another start time (the pid handed out again), or
// reports it exited; one it cannot judge (no owner file, one it cannot read)
// is left alone. The pid is never judged as a process group's leader, as
// procgroup.Leader.Current would: a test binary leads no group, so a live one
// would read as gone. A supervisor is another matter: ensure starts it
// detached, in a session of its own, so nothing ties it to the binary, and a
// box records it in a file of its own beside owner, supervisor, once ensure
// answers ready; the sweep kills its group while that recorded leader is still
// the one running (it leads its session), so a recycled pid costs no other
// group its life, any other supervisor on the home's launch, and every process
// on the home's profile, its browser, which would otherwise write the profile
// back into the home after its removal.
package boxhelpertest

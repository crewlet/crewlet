# Contributing to Crewlet

Thanks for your interest in contributing! This document covers everything you
need to get a development environment running and land a change.

## Development setup

Prerequisites: **Go 1.27+** and
**[golangci-lint](https://golangci-lint.run/welcome/install/)**, which
`make check` runs and CI's own job downloads for itself.

**What building git takes**, because **the suite never runs the machine's own
git**:
- a C compiler, `make`, `curl`, `xz`;
- a SHA-256 tool to check each tarball against its pin (coreutils' `sha256sum`
  on Debian and Ubuntu, or `shasum` on macOS; `make git` and `make git-test`
  both need one);
- libcurl's and zlib's development files. Debian and Ubuntu:
  `build-essential curl xz-utils libcurl4-openssl-dev zlib1g-dev`, or
  `libcurl4-gnutls-dev` in place of `libcurl4-openssl-dev` (the flavour the
  image builds against; the recipe compiles nothing against OpenSSL, so only
  libcurl's TLS matters). macOS: the Xcode command line tools (libcurl and
  zlib) and Homebrew's `xz`;
- **GnuPG** (Debian and Ubuntu: `gnupg`; macOS: Homebrew's `gnupg`), which
  `make check`'s `git-test` needs to make and check the signing keys of the pin
  check's own suite (see [The git pin is checked every
  week](#the-git-pin-is-checked-every-week)).

Gitlet, the engine's own git host, runs real git as a subprocess, and its
suites drive real git as the engine and as a client, so a case asserts git's
behaviour as much as the engine's. `make git` builds the gits
[`scripts/git-pin.env`](scripts/git-pin.env) pins (the node pin, the newest
release, which the engine image builds too, and the newest patches of the two
floors' series) from their checksummed release tarballs with
[`scripts/git.sh`](scripts/git.sh), the one recipe the image and CI share, once,
into `~/.cache/crewlet/git` (`CREWLET_GIT_CACHE` moves it; about four minutes on
four vCPUs). A pin bump or recipe change builds beside what is there and leaves
the older builds (about 70 MB a set, since another checkout may still use one):
`rm -rf
~/.cache/crewlet/git` clears them, and the next `make git` builds what the tree
pins.

The make targets that run the suite (`test`, `test-solo`, `test-norace`,
`test-gitproc` and `bench`) depend on it and put the node pin first on `PATH`.
A one-package loop does the same by hand, as two statements:
`bin=$(scripts/git.sh node) && PATH="$bin:$PATH" go test ./internal/gitlet/cache/`
Never `PATH="$(scripts/git.sh node):$PATH" go test …`: when the recipe fails
(a missing header, a refused download) the substitution is empty, `PATH` starts
with an empty entry, and the loop runs on the machine's git with only the
recipe's own line above the test output to say so.

A loop on the machine's own git FAILS (not skips) the git-driven tests where that
git is below the floor it stands for. Every pool a test builds needs the node
floor, 2.55 (`gitproc.NodeMinVersion`, held by
`internal/gitlet/gitproc/gittest.RequireNode`); the client git a test drives needs
the client floor, 2.40 (`gitproc.ClientMinVersion`, `gittest.Require`). Apple's
bundled git is 2.39, below both, and Homebrew's is new enough for both; the documented loop is the pin.

The engine reads a failed git's trace2 report FORMAT, checked against a table of
git's own formats held, version by version, to what real git reports over real
damaged repositories. It runs on BOTH the node pin and the node floor's own build,
each a subtest (`node pin`, `node floor`) of every gitproc case about what git
says or does in a pool, so a pin bump re-measures it in `make check`. A
one-package loop names the floor's build as the make targets do, or those floor
arms skip:
`bin=$(scripts/git.sh node) && floor=$(scripts/git.sh node-floor) && PATH="$bin:$PATH" CREWLET_TEST_GIT_NODE_FLOOR="$floor/git" go test -count=1 ./internal/gitlet/gitproc/`
A row whose range does not hold names the format and the git that disagreed.

**The floors' arm.** Every make target that runs the suite also names the two
floors' own builds, `CREWLET_TEST_GIT_NODE_FLOOR` and
`CREWLET_TEST_GIT_CLIENT_FLOOR`, each `$(scripts/git.sh <pin>)/git` for
`node-floor` and `client-floor` (the Makefile's `GIT_ENV`), and requires them,
as it requires every program the suite needs
(`CREWLET_REQUIRE_PREREQUISITES=1`, the Makefile's `REQUIRED`, which
`internal/prereqtest` reads). So `internal/gitlet/host`'s
`TestTheGitFloorsPassTheSuite` runs Gitlet's whole suite with every pool on 2.55
and every person's git on 2.40, the worst pairing the floors admit. It FAILS,
naming the variable and `make git`, where either build is missing, as do
gitproc's node-floor arms. A bare `go test ./internal/gitlet/host/` or
`./internal/gitlet/gitproc/` skips those arms, saying so; export the two
variables to run them by hand, with `CREWLET_REQUIRE_PREREQUISITES=1` to make a
missing one fail. A git named there must be an absolute path to a build of its
floor's own series (2.55.x, 2.40.x); a newer git is refused naming the variable.

ci.yml's `git recipe (macOS)` job runs `make test-gitproc` (gitproc's own suite
on the three pins) on macOS for every pull request, so the recipe's macOS branch
is built where a Mac contributor builds it; on Linux `make test` runs the same
packages. Its `harness capture (macOS)` job runs `make test-capture` the same
way: internal/flock's pin cases, a harness's output capture cases and
internal/sockcount's suite, which rest on darwin calls no Linux job makes.

**What fetching Chromium takes**: `curl`, `unzip` and the same SHA-256 tool;
`zip` for `make chromium-test` (the recipe's own suite, in `make check`); and on
Linux the shared libraries Chrome links, which a desktop and GitHub's runner
image carry and which a build's `deb.deps` (Debian, Ubuntu) or `rpm.deps`
(Fedora, RHEL) lists. `make chromium` fetches the Chrome for Testing builds
[`scripts/chromium-pin.env`](scripts/chromium-pin.env) pins, the newest and the
floor, each checked against its SHA-256 for your platform, with
[`scripts/chromium.sh`](scripts/chromium.sh), once, into
`~/.cache/crewlet/chromium` (`CREWLET_CHROMIUM_CACHE` moves it; about 0.8 GB for
both). A pin bump, or a change to how the recipe lays a build out, fetches
beside what is there, which `rm -rf ~/.cache/crewlet/chromium` clears; any other
recipe change reuses every build. The recipe runs on the four platforms the
engine is released for, `linux64`, `linux-arm64`, `mac-x64` and `mac-arm64`, and
refuses any other naming it. A build that cannot start says so when the recipe
asks it its version, naming that `deb.deps`.

**The pinned Chromiums.** Every make target that runs the whole suite (`test`,
`test-solo`, `test-norace`) depends on `make chromium`, names the two builds,
`CREWLET_TEST_CHROMIUM` and `CREWLET_TEST_CHROMIUM_FLOOR`, each the path
`scripts/chromium.sh newest` or `floor` prints (the Makefile's `CHROMIUM_ENV`),
and requires them (`CREWLET_REQUIRE_PREREQUISITES=1`). Every case that drives a
real browser runs on both (`internal/browser/chromiumtest`'s `Each`), so what it
asserts holds at both ends of what the engine accepts: Chrome for Testing's
Stable and the newest patch of `internal/browser.MinChromiumMajor`. A case's doc
may say why the floor adds nothing, and it runs on the newest alone, as
`TestAPageEndsAtMostItsOwnSessionWithAMessagePastTheReadLimit` does, whose claim
is about the DevTools client's own reader. A one-package loop names them by
hand, as three statements, each substitution its own, here for that case's
package:
`newest=$(scripts/chromium.sh newest) && floor=$(scripts/chromium.sh floor) && CREWLET_TEST_CHROMIUM="$newest" CREWLET_TEST_CHROMIUM_FLOOR="$floor" go test ./internal/artifactrender/internal/cdp/`
A bare `go test` without them skips those cases, saying so and naming that loop
for the package it ran, and runs the rest. A Chromium named there is held to its
pin's major by its `--version`: the newest at or above `MinChromiumMajor`, the
floor of that major exactly, since a newer build in the floor's place is a floor
nobody is testing. Never the machine's own Chrome (no exact version, no floor),
and never the Chromium Playwright installs for the dashboard's layout suites
(below): that one is pinned to the Playwright release that drives it,
checksummed by nothing, and fetched by `npx playwright install`, which the
engine's suite never runs.

**The floor Node.** A box brings its own Node and npm, and the box helper
supports Node `internal/browser/boxhelper.MinNodeMajor` (22) or newer with the
npm that major bundles (`MinNPMMajor`, 10), so a case of the helper runs on
that floor's newest release and its npm as well as on your own
(`internal/browser/boxhelper/boxhelpertest`'s `Floor`, `FloorNPM`, `Node` and
`NPM`). Every make target that runs the whole suite depends on `make node`,
which fetches the release [`scripts/node-pin.env`](scripts/node-pin.env) pins,
checked against its SHA-256 for your platform, with
[`scripts/node.sh`](scripts/node.sh) (`curl`, `xz` and the SHA-256 tool; about
31 MB of download, keeping `bin/node`, its npm and its licence, 136 MB, in
`~/.cache/crewlet/node`, which `CREWLET_NODE_CACHE` moves), and names it in
`CREWLET_TEST_NODE_FLOOR` (the Makefile's `NODE_FLOOR_ENV`). A pin bump, or a
change to how the recipe lays a build out, fetches beside what is there, which
`rm -rf ~/.cache/crewlet/node` clears. A one-package loop names it by hand, as
two statements:
`floor=$(scripts/node.sh floor) && CREWLET_TEST_NODE_FLOOR="$floor" go test ./internal/browser/boxhelper/...`
A bare `go test` without it skips those cases, saying so, and runs the rest. A
floor named there is held to `MinNodeMajor`'s major, and its npm to
`MinNPMMajor`'s, by what each answers to `--version`. `make check`'s
`node-test`, the recipe's and the weekly check's own suite, needs GnuPG beside
those.

**The Playwright tree.** A seat's box installs Playwright MCP with npm from the
lock in `internal/browser/boxhelper/tree/`, and the box helper's cases read the
Chromium build it expects from the tree the suite installs from that lock, and
install it again from its npm cache with each Node's own npm, offline
(`internal/browser/boxhelper/boxhelpertest`'s `Tree`). Every make target that
runs the whole suite depends on `make playwright-tree`, which installs it with
[`scripts/playwright-tree.sh`](scripts/playwright-tree.sh) (`node`, an npm of
`MinNPMMajor` or newer and the SHA-256 tool; about 4 MB of download from the
registry or mirror your npm configuration names, 23 MB kept, in
`~/.cache/crewlet/playwright-tree`, which `CREWLET_PLAYWRIGHT_TREE_CACHE`
moves), once per lock, publishing it only once it passes the box helper's own
check of a box's install (npm's record is the lock, nothing is a link, and the
files' digest is `tree/installed.sha256`), and names it in
`CREWLET_TEST_PLAYWRIGHT_TREE` (the Makefile's `PLAYWRIGHT_TREE_ENV`). A
one-package loop of the box helper names it, the floor Node and both pinned
Chromiums by hand, as five statements:
`tree=$(scripts/playwright-tree.sh) && floor=$(scripts/node.sh floor) && newest=$(scripts/chromium.sh newest) && cfloor=$(scripts/chromium.sh floor) && CREWLET_TEST_PLAYWRIGHT_TREE="$tree" CREWLET_TEST_NODE_FLOOR="$floor" CREWLET_TEST_CHROMIUM="$newest" CREWLET_TEST_CHROMIUM_FLOOR="$cfloor" go test ./internal/browser/boxhelper/...`
A bare `go test` without them skips those cases, saying so, and runs the rest.
`make check`'s `playwright-tree-test` is the recipe's own suite.

**Chromium's own sandbox.** A box helper case whose cell requires Chromium's
own sandbox proves the browser runs with it only on a host where an
unprivileged process may make a user namespace, which Ubuntu restricts from
23.10 (AppArmor's `kernel.apparmor_restrict_unprivileged_userns`). Where it
cannot, the case proves the browser is withheld instead
(`chromium_sandbox_unavailable`), so either passes: CI's race shards lift the
restriction, so the gate proves the sandbox runs, and
`sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0` does the same
on your machine until it next boots.

**A current Node** (npm ships with it) for the dashboard and the suite. The
dashboard's build output is committed, so building and running the ENGINE needs
neither, but changing the dashboard or running the suite does: `internal/e2e`'s
client replay, the probes of OpenCode's plugins and the box helper's cases run
under node, and the suite's Playwright tree is installed with npm (above).
Nothing in the tree pins the node you install, the suite's floor (above) aside;
CI installs the latest (see *Which node* under [A skip is not a
pass](#a-skip-is-not-a-pass)).
**Docker** only for the vendor loops below; the engine itself needs no services.

Take golangci-lint as a **prebuilt release**, not via `go install ...@latest`,
which builds it with the linter module's own minimum Go. A golangci-lint built
with a Go whose language version (major.minor) is older than that of `go.mod`'s
`go` line refuses this module outright, so the gate reports nothing at all. Only
the language version counts: a linter built with go1.27.0 lints a module at
`go 1.27.2`. `make lint` checks this and says so.

```bash
git clone https://github.com/crewlet/crewlet.git
cd crewlet
make build        # or: go build ./...
```

`make help` lists every target, and that is the whole setup: nothing to install
beyond the prerequisites above. The engine embeds its own event stream (a NATS
JetStream server) and its store is two local files it creates, so `crewlet run`
in an empty directory is a working company, with no broker to operate and no DSN
to set.

The Docker stack is for the **self-hostable integrations**: Mattermost and
GitLab, each behind its own compose profile with a bootstrap script that stands
it up. `make mattermost-up` and `make gitlab-up` are the profile and its script
in one step; `make gitlab-up` takes `COMPANY=` to provision that config's seats
in the same run. The bundled example needs neither: its chat, tracker, wiki and
code are the engine's own, so it runs from the binary alone. To try a vendor,
add its blocks to a copy of the example as its page shows; the GitLab profile
needs a config that declares `integrations.gitlab`. Jira and Confluence have no
profile, since a compose file cannot stand Atlassian up. See
[docs/integrations/](docs/integrations/).

## Running tests and lint

```bash
make check               # every gate CI runs on a pull request
```

One target because it is one question, *would CI accept this?* `make help` lists
the rest.

Every target runs the command [`ci.yml`](.github/workflows/ci.yml) runs, with
the same flags, `make bench` excepted, which has no CI job (see
[Benchmarks](#benchmarks)). The two test jobs call `make` directly. For every
other job nothing asserts the two have not drifted, so a convenience target that
quietly loses `-race` would report a pass CI does not honour. Change a target
and its `ci.yml` step together, and read both. `make check` is:

```bash
files="$(go run ./internal/sourcetree/gofiles)" && gofmt -l $files  # formatting — prints the files that need it
go mod tidy -diff        # go.mod / go.sum are already what tidy would write
scripts/check-signoff.sh # every commit the branch adds is signed off
scripts/check-signoff_test.sh  # ... and that gate's own suite
scripts/git-pin_test.sh        # the git recipe's and the pin check's own suite (git-test)
scripts/chromium-pin_test.sh   # the Chromium recipe's and the pin check's own suite (chromium-test)
scripts/node-pin_test.sh       # the floor Node's recipe's and the pin check's own suite (node-test)
scripts/playwright-tree_test.sh  # the Playwright tree's recipe's own suite (playwright-tree-test)
go vet ./...
golangci-lint run        # what CI's lint job runs
go build ./...
make git         # the pinned gits, built once a machine (both test targets need it)
make chromium    # the pinned Chromiums, fetched once a machine (both test targets need them)
make node        # the floor Node with its npm, fetched once a machine (both test targets need it)
make playwright-tree  # the Playwright tree, installed with npm once per lock (both test targets need it)
make test        # the suite, minus the packages that run alone, on the node pin, four at a time
make test-solo   # ... and those, at -p 1, with a runner to themselves
# then, for each of CROSS_TARGETS (linux and darwin x amd64/arm64):
CGO_ENABLED=0 GOOS=$OS GOARCH=$ARCH go build ./...            # test-cross
# and the dashboard, whose build output is committed to every tree the
# Makefile's UI_OUT names (static/dashboard, static/artifacts, static/protocol):
cd dashboard && npm run format:check                          # dashboard-lint
cd dashboard && npm run build                                 # dashboard-check:
git diff --exit-code -- $UI_OUT                               #   no tracked file moved
test -z "$(git ls-files --others -- $UI_OUT)"                 #   and none is untracked
cd dashboard && npm run typecheck && npm test                 # dashboard-test
cd dashboard && npm run test:layout                           #   and its layout suites
make notehelper && git diff --exit-code -- internal/sandbox/executor/notehelper  # notehelper-check
```

Capture the formatting list BEFORE handing it to gofmt, and copy that form into
a hook or script. The inline `gofmt -l $(go run ./internal/sourcetree/gofiles)`
passes when the listing fails: the shell discards a command substitution's exit
status, and gofmt handed no paths formats its standard input, finds nothing and
exits 0. Run from anywhere but the module root, the listing fails every time.

**Two of those gates judge formatting, at two standards.** `gofmt -l` over
`go run ./internal/sourcetree/gofiles` is one; `golangci-lint run` is the other,
because `.golangci.yml` enables the `gofmt` *formatter* too with its `simplify`
pass on, so a redundant composite-literal type passes the first and fails the
second. `make fmt` repairs both (it runs `gofmt -s -w` over that same list);
`golangci-lint fmt` repairs those plus the `goimports` grouping the lint job also
enforces. Hand it the captured list too: on its own it skips a dot-directory by
name but walks into a nested checkout anywhere else. Use one of those two, not
plain `gofmt -w`, which cannot fix what the linter reports.

The layout suites (a `.layout.test.tsx` beside the stylesheet it holds) lay
panes out in a headless browser, which `npx playwright install chromium
--only-shell` installs once, and `npm run test:layout` runs them. They place the
stylesheets where a built page does (`src/test/layout.tsx`): the app's sheet,
the design system's rules among it, after every screen's own sheet, so a
screen's rule that sizes or restyles a design-system box loses there as it does
for a reader. Ask the design system for the state instead (a prop), or style an
element of the screen's own.

Run the dashboard's suites through `npm test` (or `npm test -- <file>` for one
file, `npm run test:watch` for the watch loop), and its benchmarks (a `.bench.ts`
beside the module it times) through `npm run bench`, never a bare `npx vitest`.
Vitest's forks pool writes every transformed module into a fresh system temp
directory and never removes it (16 to 48 MB a run), and the Go suites keep their
stores and broker streams there too (`t.TempDir()`, and `brokertest.TempDir` for a
broker's store), so a full disk fails a case for a reason nothing in the change
explains. The test brokers declare a fixed storage size
(`internal/queue/jetstream/brokertest`). All three scripts run Vitest
through `dashboard/scripts/owned-tmpdir.mjs`, which points `TMPDIR` at a
directory it creates and removes; `src/test/ownedTmpdir.test.ts` fails a run
started without it.

No gate runs a benchmark, since a time is the machine's it was taken on. A
benchmark is the measurement a bound's comment quotes
(`src/lib/syntax/syntax.bench.ts` for the syntax colouring's size bound), kept so
a change to the code under the bound is timed against the cases that set it:
rerun it, and update the figures the comment quotes, with the machine they came
from.

A dashboard case also fails on what it writes to `console.error` or
`console.warn` (`dashboard/src/test/console.ts`, judged after every case by
`src/test/setup.ts`). React reports a broken contract there and carries on (a
list child with no key, two children under one key, a paragraph inside a
paragraph, an `act` nobody awaited), and the dashboard reports there an
announcement no live region was mounted to say; Vitest's default reporter prints
nothing a passing case wrote. A harness that stands in for the application
mounts what the application mounts around a screen (its live region,
`AppAnnouncer`); a case that provokes a line on purpose, as a boundary's case
does when React reports the error it caught, declares it with
`expectConsole(level,
pattern)`, and a declaration nothing meets fails the case too.

The race detector is not optional: the engine's concurrency model is real
parallelism, so every package runs under it, in CI and in `make check`.
`-count=1` is the other half: without it a cached PASS recorded before the change
answers for the change. `make test-norace` skips the detector for a faster loop,
and says so.

The test targets run `go test -vet=off`, which is not a gap: the vet `go test`
runs by default is twelve analyzers, all of which `go vet ./...` runs too, over
the same packages and `_test.go` files. That full vet is `make vet`, a
prerequisite of `make check` and a step of CI's `build + vet` job. So a bare
`make test` no longer vets: run `make vet` beside it, or `make check`.

Some packages stand up N engines, each embedding its own NATS server, in ONE
process, and every stream or bucket they create is a round trip a quorum of those
servers answers. On a runner shared with the rest of the suite (`go test ./...` runs binaries in parallel and holds a
four-vCPU runner almost fully busy) the cluster cases failed with `context
deadline exceeded` where alone they passed. Today's budgets are longer and the
shared case has not been re-measured under them; `go doc ./internal/solo` has what
has been measured.

So those packages run in `make test-solo` and in CI's `end-to-end gates` shards,
and `make test` leaves them out. A contention split, not a coverage one: `make
check` depends on both targets. `make test-solo` runs its packages one binary at
a time (`-p 1`) and compiles them first at full parallelism, since `-p 1` would
otherwise compile the dependency tree one package at a time too. `make test` runs
four binaries at a time, on any machine, compiled first the same way. Four, not Go's
one per core, keeps every machine at the per-core load of CI's four-vCPU runners,
where the suite's timing bounds are measured.

**In CI each half runs on several runners at once.** A `test plan` job deals each
half into shards by measured weight (the race half by each package's CPU, since
its runners are CPU-bound, and the solo half by wall time) and proves every
package lands in exactly one. Each `test (race) I/N` leg runs `make test` on its
shard and each `end-to-end gates I/N` leg runs `make test-solo` on its own, still
at `-p 1`, so no two packages that run alone ever share a machine. The jobs named
`test (race)` and `end-to-end gates`, the checks `main` requires, pass only when
the plan and every shard of their half did. A local run can do what a shard does:

```bash
make test TEST_TIMINGS=/tmp/test-timings.tsv                 # write each package's time, CPU and tests' times
make test TEST_WEIGHTS=/tmp/test-timings.tsv                 # start the heaviest first by that
make test TEST_WEIGHTS=/tmp/test-timings.tsv TEST_SHARD=2/4  # run the second of four shards
```

`TEST_WEIGHTS` starts the heaviest packages first. With `TEST_SHARD=I/N` the same weights deal
the half across N shards, heaviest first, each package to the shard carrying the
least so far; without weights the deal is by package count. A package weighs its
CPU in `make test`'s half, and its wall time in `make test-solo`'s, which runs
one package at a time; skipgate reads a package's CPU at its test binary, through
go test's `-exec`, which it hands the `go test` it runs whenever `TEST_TIMINGS`
is set. The file must exist (`internal/solo/partition` refuses one it cannot
open), and an empty file is `go list`'s order. CI's weights are the shards'
timings from the last passing run on `main`, merged by `go run
./internal/solo/partition merge`. The Actions cache keeps them for CI, and each
run also publishes them as its `test-weights` artifact, which a local run can
download with `gh run download <run-id> -n test-weights`. Under each package's
line the file names the seconds of each of its top-level tests, which is where to
look for what a slow package spends its time on; the gate's last lines say how
much CPU the run used against the CPUs it had (telling a shard short of cores
from one that waits) and list the run's slowest tests and subtests. None of the
three variables changes which packages are in a half; left unset, `make test` is
the whole half in `go list` order. `make test-solo` takes the same three: at
`-p 1` its order changes nothing, but a shard of it takes the sum of its
packages, so the deal is its whole wall time.

Both paths live outside the checkout, as in CI: the suite's own gates read the
checkout as this repository's tree, and neither path is ignored by git, so a file
left there is one `git add -A` away from a commit.

**Which packages those are is computed, not listed.** A package declares it by
importing `internal/solo` from its `TestMain` (replacing a hand-kept `grep -v /internal/e2e`), and `internal/solo`'s roster guard
fails the build both ways: a package that stands up a multi-member broker without
declaring, and one that declares without needing to. Read `go doc ./internal/solo`
before reaching for a build tag; it records what each alternative measured, and
every one fails silently.

`make dashboard-check` gates the dashboard's committed build output
(`go build ./...` and `go install …@latest` must work on a clean checkout with no
Node, and an embed directive cannot run a bundler). A drifted bundle compiles, embeds, serves and passes every Go test while running
code nobody wrote; only rebuilding tells you. The build
writes three trees, and the gate judges every one, as the Makefile's `UI_OUT`
lists them: `static/dashboard`, `static/artifacts` (the page runtime and the
artifact kit) and `static/protocol` (the wire protocol alone, which `internal/e2e`
replays and no binary carries).

Judging takes TWO questions, because `git diff` sees only paths the index already
has. Every emitted name is content-hashed, so a changed chunk is a new path, and
`git commit -a` stages modified and deleted files but never a new one: the commit
carries the `index.html` that imports the new chunk and not the chunk, the diff is
clean, and the binary serves a shell asking for a module it does not embed. So the
gate also fails on any file under those trees the index does not hold
(`git ls-files --others`), and prints each drifted path: `??` for one git does not
track, `!!` for one `.gitignore` hides, which `git add -A` will not stage and the
`all:dashboard` embed ships anyway. It judges against the INDEX rather than
`HEAD`, so the repair it prints turns it green before you commit:

```bash
make dashboard && git add -A -- static/dashboard static/artifacts static/protocol
```

**And when two branches have both rebuilt it, REBUILD — never merge.** The
emitted names are content-hashed, so each side writes a different path for the
same chunk; git reads a rename/rename and three-way merges a megabyte of minified
JavaScript, splicing conflict markers into both bundles *and* `index.html`, and
the result does not parse. `.gitattributes` marks each of the three trees
`-merge` so git leaves your side alone (the conflict still happens; only the
damage stops), and `static/merge_test.go` holds that. The resolution is always the
same two commands, merging or rebasing:

```bash
make dashboard && git add -A -- static/dashboard static/artifacts static/protocol
```

`make notehelper-check` gates the note helper's committed builds the same way.
`internal/notehelper` runs inside a box, so the engine embeds a gzipped build
per box arch from `internal/sandbox/executor/notehelper/`. `make notehelper`
writes them reproducibly (go.mod's toolchain, `-trimpath`, no VCS stamp, an
empty build id, gzipped by `internal/notehelper/gzipbuild` rather than the
`gzip` program, whose output is its version's), and the gate rebuilds and diffs
them. A change to `internal/notehelper`, to go.mod's toolchain or to a module it
links (`golang.org/x/net`, `github.com/klauspost/compress`) commits the rebuild
with it: `make notehelper`, then commit both `.gz` files. Two branches that both rebuilt them are rebuilt again,
never merged.

`go mod tidy -diff` gates the silent half of tidiness (an *under*-tidy module
already stops `go build ./...`): a `require` left behind when its last import was
deleted, a stale `go.sum` line or a wrong `// indirect` marker build green, then
land as churn in whichever pull request next runs `make tidy`. `-diff` prints the patch and exits non-zero *without* writing the
files, so neither CI nor `make check` rewrites the tree it is judging; run
`make tidy` to apply it.

One thing `make check` does not cover, because it needs a service CI starts for
itself, and it prints this when it passes rather than letting a green run imply
more: the [release pipeline](#releasing) (`make snapshot`).

The queue conformance suite is not a second gap. Both backends behind the
`EventQueue` contract, the in-memory twin and JetStream, run the single suite in
`internal/queue/queuetest` under a plain `go test`, because
`internal/queue/jetstream` starts an embedded NATS server per queue under test and
shuts it down with the test. Per queue rather than per binary, deliberately: the suite asserts things like "a
subscription nobody created retains nothing", which a shared broker could satisfy
by accident. So there is no service to start and `make check` certifies the backend the engine ships, on every run.
### A skip is not a pass

Some suites need something the machine may not have and **skip silently
without it**; a green run has not exercised them.

Both test targets pipe `go test -json` through `internal/skipgate`, which
renders the stream back to ordinary output and fails the run on a skip nothing
declared. Every allowed skip is an entry in `internal/skipgate/allowed.go` with
a reason and a `When`:

- `Always`: a structural skip (a backend that cannot do the thing, a driver
  capability that has not landed). It is ALSO reported when it stops firing,
  because the world changed.
- `Environment`: depends on what the machine has; checked only in the
  undeclared direction.

Stopping is judged by the test, never its package: an `Always` entry is stale
when its test reported a result that was not a skip, or
when a passing run of every test of its package never ran it (it was renamed
or deleted). A run narrowed with `-run` or `-skip`, and a package that failed,
are judged only by the tests that reported a result, not by a test that
started and never finished because its binary died under it — so `go test -run X` on a package that declares entries passes when
everything it ran passed.

Adding an entry is the decision, so it lands in a reviewed diff. First check
the case is covered somewhere: the two defects this gate was written after ran
NOWHERE and looked like ordinary capability skips. `go test` prints nothing
about a skipped subtest without `-v`, which the suite job never passes.

**A program or directory the suite needs is found through
`internal/prereqtest`**, the one rule for it: `OnPath` for a program on
`PATH`, and `Named` or `NamedDir` for a program or a directory a variable
names. It FAILS the case under the `make` targets that run the suite
(`CREWLET_REQUIRE_PREREQUISITES=1`, the Makefile's `REQUIRED`) and skips it,
in the same words, only in a bare `go test` outside make. Never write a
prerequisite's own skip, an `allowed.go` entry for it, or a requirement
variable of its own: each is a second copy of that rule. What the suite only
uses where a machine has it (a container runtime, which no make target
requires) keeps its `Environment` entry.

The prerequisites that legitimately vary by machine:

- **`node`** runs `internal/e2e`'s client replay: the Go suite drives a real
  company, captures every frame its WebSocket pushed, and replays those bytes
  through the dashboard's own protocol module under plain `node`, the only
  place both halves of the wire protocol are checked against each other.
  Without node it follows the `internal/prereqtest` rule above. The targets
  that run it also refuse to start without node (`require-node`), and CI
  installs it.

  It needs no npm and no build: `static/protocol/protocol.js` is committed
  with the built tree, beside the embedded `static/dashboard` since no page
  loads it.

  **Which protocol module the replay tests is a parameter.** The replay is
  `internal/e2e/replay.mjs`, beside the `golden_test.go` that runs it. It reads
  the module's directory from `CREWLET_PROTOCOL_ROOT`, which the Go half always
  sets to `static/protocol`, the committed build. Unset, it is that directory;
  a relative value is read against the repository (the nearest directory above
  the script holding a `go.mod`), not the working directory.

  `node` also runs the **four OpenCode plugins** the engine embeds
  (`internal/sandbox/executor/opencode_steer.js`, `opencode_skills.js`,
  `opencode_toolenv.js` and `opencode_checkouts.js`) and its **Kimi Code
  driver** (`kimi_driver.js`).
  `opencode_steer_test.go` drives the notes plugin, as the engine writes it into
  a real box, through `opencode_steer_probe.mjs` against a stand-in of the
  bridge's notes route and a real bridge's; `opencode_skills_test.go` drives the
  skill-load plugin through `opencode_skills_probe.mjs` against a stand-in of
  the bridge's skill route; `toolenv_test.go` drives the tool-environment plugin
  through `opencode_toolenv_probe.mjs`; `opencode_checkout_test.go` loads the
  checkouts plugin in a stand-in OpenCode server; and
  `kimi_driver_internal_test.go` runs the driver against a stand-in of Kimi
  Code's server; and
  `pi_extension_internal_test.go` and `pi_contract_test.go` drive Pi's extension
  (`pi_extension.mjs`) through `pi_extension_probe.mjs`, a stand-in of pi's
  extension API, beside a stand-in of the one module of pi's it imports; and
  `cursorcarry_internal_test.go` and `cursor_home_test.go` run Cursor Agent's
  helper (`cursor_helper.mjs`) on real chat stores of the release, which it
  reads with node's own SQLite. That package is in the parallel half, so `make test` requires node and CI's race
  shards install it; the probes find node by the same rule as the replay. The
  plugins, the driver, the extension and the helper import nothing a box
  installs, so no npm.

  `python3` runs the **Hermes Agent plugin** and **session carry script** the
  engine embeds (`internal/sandbox/executor/hermes_plugin.py` and
  `hermes_carry.py`): `hermes_contract_test.go` drives the plugin's notes poll
  against a real bridge's route and its skill-load report against a stand-in
  of the skill route, and `hermes_internal_test.go` restores and dumps a
  measured session and runs the session gate with them. A box runs both under
  Hermes's own interpreter; the suite runs them under the machine's `python3`,
  which stands in for it since both import nothing outside Python's standard
  library. `make test` requires `python3`, which CI's runner image carries.

  `node` also runs the **box helper** the engine writes into a seat's box
  (`internal/browser/boxhelper`'s `js/` modules): its Go suite lays a box out
  on disk as the engine writes one, runs its ensure and the supervisor that
  holds a pinned Chromium through Playwright as a box would, and drives the
  helper's modules from it through `boxhelper_probe.mjs` beside its tests. A
  case runs at each end of what a box may bring (`boxhelpertest.Each`: your
  node and npm with the newest Chromium pin, the floor Node and its npm with
  the floor's; *The floor Node*, above), on each Node alone where it drives no
  browser (`EachNode`), or the newest pair alone where its doc says why the
  floor adds nothing (`Newest`). Each box installs the Playwright tree with
  that Node's own npm, offline, from the suite tree's npm cache or a stand-in
  registry on 127.0.0.1, as `TestTheLockInstallsWithEachNodesOwnNpm` installs
  it with each.

- **`npm`** builds and tests the dashboard. Vitest runs its assertions (the
  wire protocol, the router's history rules, the ordering comparators, the
  purity of `src/contract/`, and the MEASURED contrast of every colour token
  over every surface it can land on, in both themes and for protan and deutan
  vision): `make dashboard-test`. They are computed from the file that ships,
  since none is checkable by looking at the stylesheet. npm also installs the
  suite's Playwright tree (`make playwright-tree`), which the box helper's
  cases find by the `internal/prereqtest` rule above (`boxhelpertest.Tree`).

  **A set the engine owns is declared in `dashboard/src/contract/`**, one
  module per concern, and held against the engine by one Go test that reads it
  through `internal/clientsource`. Adding one takes three things in one
  change: the declaration in a contract module, a row in
  `internal/clientsource/contract.go` naming its reader and its gate, and the
  gate. The Go side holds that directory both ways (every row is declared
  there, every name it exports is a row), and `contract/contract.test.ts` holds
  what a module there may be: data and shapes, importing only its siblings,
  every export read outside it. `protocol/` imports it by a relative path,
  because `protocol.js` is built without the `~` alias.

  **Nothing on a screen is money.** The dashboard draws spend in tokens, never
  a currency (rule 19 in `docs/reference/dashboard-design.md`), and declares no
  price field. Gates: `src/money.test.tsx` parses every shipped module and
  renders the screens that draw spend over wire fixtures carrying a price, and
  `TestTheDashboardRendersNoPrice` in `internal/api` scans every module the
  engine serves from the committed bundle.

  **The dashboard reaches the engine through `src/protocol/` and nowhere
  else**, held from the parsed source by three suites:
  `protocol/transport.test.ts` refuses a module outside that directory that
  reads `fetch` or any other network primitive; `app/source.test.ts` requires
  the query kind of a `useQuery(` or `query(` call (bare or as a method) to be
  a string literal, because the engine's registry gates cannot read a
  variable; `protocol/proxy.test.ts` holds the dev server's proxy table in
  `vite.config.ts` to exactly the paths the source reaches, calls and markup
  alike, both ways. A new REST route the dashboard calls is a new proxy entry
  in the same change; a route it stops calling takes its entry with it.

  **A dashboard suite waits for the work, never for the clock.** `findBy*` and
  `waitFor` poll against a one-second deadline, so a case waiting that way
  passes on a quiet machine and fails on a loaded one. A screen that takes its
  time as an argument is handed time the suite moves: the builder's testkit
  mounts the lens on a `SuiteClock`, and `settle()` waits for every answer the
  lens has out and every timer due within the check's debounce. One that does
  not is flushed with `act` where its work is promises, or driven by fake
  timers faked only for the calls that arm and cancel one. Scope a role query
  with `within` to the toolbar, dialog or chart it is about: asked by name, it
  computes the accessible name of every candidate in its container, and in
  jsdom each element costs a computed style matched against the whole
  user-agent stylesheet. The org builder's suites and the screens that ran out
  of time under load (the audit, the retention gesture, a seat's settings, the
  work list's Display menu, the sign-in form) wait for the work; more than
  forty others still poll with `findBy*` and `waitFor`, which is not a pattern
  to copy. A suite you write, or a case you change, waits for the work.

  **What a case does ends with the case.** A case that times out is failed, not
  stopped: its function keeps running beside later cases. React keeps one `act`
  scope count per process, so an `act` it opens beside the next case's leaves
  every later render in the file queued; a `findBy` or `waitFor` it has out
  polls the next case's page; a `render` or `cleanup` it makes draws into that
  page or removes it. So every case runs in an async context of its own
  (`aroundEach` in `src/test/setup.ts`, over `src/test/cases.ts`, which loads no
  testing library so a file with no document pays for none), and
  `src/test/inCase.ts` binds the testing library to it. A suite imports the
  library from there and nowhere else; `inCase.source.test.ts` fails a file that
  takes it, or React's own `act`, from anywhere else. The binding covers:

  - `act`, `render`, `renderHook` and `cleanup`, refused once their case ended;
  - its `poll`, used in place of Vitest's `vi.waitFor`, which cannot be bound,
    keeps looking after its case ended, and advances whatever fake clock is
    installed (by then the next case's) before every look. The same gate
    refuses `vi.waitUntil` and `expect.poll`;
  - the library's own waits and `fireEvent`, through its `asyncWrapper` and
    `eventWrapper`. The wait's wrapper is restated, not wrapped: after a wait
    finds what it looked for the library lets one turn of the page's timer pass
    before restoring the act environment, and on a fake clock nothing moves
    that timer, so a wrapper round the library's could not end a wait held
    there, or the scopes outside it.

  When the case ends, every act scope, wait and poll it still has out is closed
  before the next case begins, innermost first (each restores on exit the act
  count or environment it found on entry; in any other order React's count
  stayed raised), and anything it asks for afterwards is refused before it
  opens. The binding does not reach what a late case writes to the page's
  globals itself (the fake clock through `vi`, `location.hash`, a stub) in a
  `catch` or `finally` its refusal passes through, or past an await of
  something else, such as a promise of its own; every act, wait and poll
  rejects at its case's end, which throws a late case out at the first of them
  it awaits. Node carries the context through the case's own awaits and timers,
  so a case that timed out inside its own promise and resumes later still reads
  its own, ended case; that is how a helper every case shares (`answered()`, a
  suite's `settle()`) can tell. A case that holds the page's timers advances
  them as `answered`'s `step`, so the refusal comes before the timers move.
  `src/test/inCase.test.tsx` holds each of these to a pair: a case that runs
  out of its time with something out (`test.fails` on a budget it always
  spends), and the case after it, which first holds it to having failed by that
  timeout (`test.fails` passes on any failure) and then reads what the late one
  came to.

  **The harness's own waits end with the case too.** The builder's testkit
  retires a lens when the case that mounted it finishes (`onTestFinished`),
  waking every wait on the lens's engine, which holds answers back until the
  case releases them. A suite that waits for the page without a lens (a move
  landing, an element the page draws) waits through `waitInCase`
  (`viewTestkit.tsx`), which listens with no deadline and stops listening the
  same way. A case waits only on the lens it mounted: `mountBuilder` hands the
  case its lens's `settle`, `checked` and `navigate`, because a variable the
  last mount wrote let a late `settle()` settle the NEXT case's lens.
  `testkit.test.tsx` holds each of those waits to a pair.

  **A warning fails the case that printed it.** `src/test/setup.ts` watches
  `console.error` and `console.warn` around every case and fails one that wrote
  to either, after the suite's own teardown so what an unmount says counts. It
  fails the FILE for what was written outside every case: while it loaded, in a
  `beforeAll` or an `afterAll`, or between cases. A case that expects a warning
  silences that one itself,
  `vi.spyOn(console, "error").mockImplementation(...)` for the call it is
  about, and puts it back; the watcher (`src/test/console.ts`) restores both
  methods when the case ends whatever the case did.

  **A stand-in for the browser answers when the browser would.** jsdom has no
  layout, so the chart suites install a ResizeObserver of their own. One that
  reports from inside `observe()` reports in the middle of React's commit
  (`observe()` is called from a ref), where the design system's synchronous
  resize flush becomes a `flushSync` inside a lifecycle; a real observer
  delivers in the event loop's rendering step with React idle. Both of the
  builder's observers, `LayoutObserver` (`viewTestkit.tsx`) and the reference
  chart's (`src/testing.tsx`), record what they observe and report it inside
  `act` once the render has committed.

  **The built dashboard is committed**, so building the ENGINE needs neither
  node nor npm. Changing the dashboard does: run `make dashboard` and commit
  every tree it writes with your source change,
  `git add -A -- static/dashboard static/artifacts static/protocol`, so the new
  content-hashed chunks go in. CI rebuilds them and fails on any difference,
  an untracked file included (`make dashboard-check`), because a drifted bundle
  compiles, embeds, serves and passes every Go test while running code nobody
  wrote (as `go mod tidy -diff` and the generated `schema/` are gated against).
  A Dependabot bump is the one change that does not make you do it; see [A
  dashboard bump brings its own bundle](#a-dashboard-bump-brings-its-own-bundle).

  **Which node.** CI installs `latest` (the newest entry in Node's release
  index, a prerelease such as a new major's alpha included, resolved by
  setup-node when the job runs) in the `build + vet` job, for the Playwright
  tree recipe's suite, the `dashboard gates` job and the `dashboard unit`
  shards (one step, which the shards reuse), the race shards, the `end-to-end
  gates` shards and
  [`dependabot-dashboard.yml`](.github/workflows/dependabot-dashboard.yml)'s
  `build` job. The last must always answer this the same way as `dashboard
  gates`, because it rebuilds the bundle that job diffs. The two sets of Go
  shards need not match: what they run under plain node is a committed file
  (a plugin, the box helper's modules, the built bundle), not a fresh build.

  **Nothing in this repository names the node you install**, by decision:
  nothing would maintain a pin. Dependabot moves the versions in
  `dependencies`, `devDependencies` and a `uses:` ref, and bumps neither
  `engines.node`
  ([dependabot-core#11243](https://github.com/dependabot/dependabot-core/issues/11243),
  open) nor a workflow's `with:`. A number nobody moves goes quietly out of
  date while looking deliberate.

  **The one exception is the suite's floor Node**,
  [`scripts/node-pin.env`](scripts/node-pin.env): the oldest Node, with its
  npm, the box helper supports, which a case of the helper runs on beside the
  node you install. It is pinned because something moves it: a weekly check
  holds it to the newest patch of its major and fails from 60 days before that
  major's end of life (see [The floor Node is checked every
  week](#the-floor-node-is-checked-every-week)). Without it the suite would
  prove the helper on the newest Node alone, while an image may bring the
  oldest the helper accepts. `make node` fetches it beside the node CI
  installs, never in its place, so everything above holds for the dashboard
  and every other suite.

  The cost: a new Node major reaches CI with no commit behind it, so the
  `dashboard` check can go red on a day nobody changed anything. Then the fix
  is `lts/*` in `ci.yml`'s `dashboard gates` job and in
  `dependabot-dashboard.yml`'s `build` job, in the same commit (the current LTS
  lags the newest major and is the same zero-maintenance trade), not a
  hand-maintained number.

  **Do not add `engines` or `devEngines` to `dashboard/package.json`.** Those
  fields also apply inside **Dependabot's** sandbox, which runs its own older
  node: `devEngines.runtime` refuses outright (`onFail` defaults to `error`)
  and `engines` does too once an `.npmrc` sets `engine-strict`. A bump that
  cannot install never opens, silently, leaving the npm entry in
  `.github/dependabot.yml` looking like a surface with nothing to update. See
  [#13722](https://github.com/dependabot/dependabot-core/issues/13722) for
  `devEngines` (open; the proposed fix has to *strip* the field before corepack
  sees it) and
  [#12535](https://github.com/dependabot/dependabot-core/issues/12535) /
  [#13741](https://github.com/dependabot/dependabot-core/issues/13741) for
  `engine-strict`. The dashboard's dependencies are bytes the engine binary
  embeds; keeping them watched is worth more than a version check.

- **`TURSO_GO_CACHE_DIR`** is where the store driver's native library lives,
  and the one environment variable a store test can be defeated by.

  Turso is the only store driver, so there is no dialect intersection to keep
  statements inside and no second suite run. Write `internal/store` against
  Turso.

  It is pure Go to *build* (no cgo), but its engine is a ~20 MB binary embedded
  in the driver and extracted at runtime into `$TURSO_GO_CACHE_DIR` (default
  `~/.cache/turso-go`), shared by every process on the machine. Upstream writes
  it without a rename and panics on what a concurrent reader sees, so
  `internal/store/turso.go` prepares it under a lock and heals a cache entry
  that will not verify. If a test binary ever dies with `unable to
  load turso library`, that cache is the place to look — `go test
  ./internal/store/ -run LibraryCache -count=1` covers it (the cache is not a
  test input, so without `-count=1` Go serves the PASS from before it went
  bad).

  It also decides what the project can be built for. Upstream embeds the
  library for linux and darwin on amd64 and arm64 and for windows/amd64;
  Crewlet ships the first four, and `internal/store/platform.go` turns a build
  for anything else into a compile error that says why.

  **The linux binary is not static, and must not be made static.** purego
  declares its `dlopen` imports with `//go:cgo_import_dynamic`, so
  `CGO_ENABLED=0 go build` still emits a dynamic executable (`interpreter
  /lib64/ld-linux-x86-64.so.2`, `NEEDED libc.so.6`). It needs glibc and does
  not run on musl, and `-tags musl` does NOT change that: the tag picks which
  shared object the driver embeds, not this binary's linkage. That is why there
  is no musl archive. `CGO_ENABLED=0` gives a static binary for any other Go
  program, but purego's `dlfcn_nocgo_linux.go` is `//go:build !cgo`, the file
  that applies when cgo is OFF, and it declares the dynamic imports.

  `-extldflags -static` changes nothing on a cgo-free build, which links
  internally. Forcing a static ELF needs `-linkmode external`, which needs cgo,
  and that binary, which `file(1)` does call static, SIGSEGVs on its first
  query on the machine that built it, because a static program cannot `dlopen`.
  The `cross` CI job asserts the artifact is still dynamic, so this is a red
  build rather than a release.

  ```bash
  # What CI cross-compiles on every pull request.
  make test-cross
  ```

Tests never call real LLM APIs; use the fakes in `internal/providers`. A phase
that offers tools plays its model as a scripted harness run over the real tool
session (`internal/providers/llm/llmtest`), never by calling the dispatcher
around it: the socket, the tool list, the withheld answers and the release are
on the way of every real call. Test files sit beside what they cover, a test
file that is not Go included: the dashboard's Vitest suites sit beside their
modules in `dashboard/src`, `internal/e2e`'s client replay is
`internal/e2e/replay.mjs` beside the `golden_test.go` that runs it, and the
OpenCode plugins' probes are
`internal/sandbox/executor/opencode_steer_probe.mjs`,
`opencode_skills_probe.mjs` and `opencode_toolenv_probe.mjs` beside the
`opencode_steer_test.go`, `opencode_skills_test.go` and `toolenv_test.go` that
run them, and the box helper's probe is
`internal/browser/boxhelper/boxhelper_probe.mjs`, beside the tests that drive
it. There is no top-level `tests/` tree.

A test that opens a store on a fresh path opens it through
`storetest.OpenNode` or `storetest.OpenEstate`, and one that boots an engine on
a path of its own seeds it first with `storetest.Seed` (the node estate, and
the replicated one where the node holds it, never a scratch store). Each copies
an image the test binary's own migrations built once, so a broken migration
still fails at the first fixture and no case pays the whole sequence for
nothing it asserts. Only a test whose subject is migration (the schema cases,
`Pending`, an adoption, a fresh-file boot) opens a fresh file with
`store.OpenNode`.

A test broker declares its size rather than take it from the runner's disk:

- a node fixture gives its stream a directory through `brokertest.StoreIn`,
  which sets `stream.store_max_bytes` to `brokertest.StoreMaxBytes`;
- a broker opened directly with a store directory sets `StoreMaxBytes:
  brokertest.StoreMaxBytes`;
- a server a case builds from nats-server's own options sets
  `JetStreamMaxStore: brokertest.StoreMaxBytes` (or `max_file_store` in a
  configuration it writes);
- an external one comes from `externaltest.Start`, whose account states the
  same limit.

Left unset, an embedded broker's limit is three quarters of the free space on a
volume every package shares, and a fresh node reserves several gibibytes of it
at boot, so a case passes on a roomy runner and fails on a fuller one. So a
test binary refuses to start an embedded broker with a store directory and no
limit (`jetstream.ErrUndeclaredSize`), however the case reached it, and
`brokertest`'s `TestEveryNATSServerATestBuildsDeclaresItsSize` fails a server
built from nats-server's options without one. A node on an external broker
reads the account's limit, never the server's cap, and where the account states
none it stands three quarters of its own volume's free space in for one, which
refused a node's object bucket on any runner with more than about 92 GB free. So
a test binary's node also refuses to open on an external broker whose account
states no limit (`engine.ErrUndeclaredBrokerLimit`), and a node dials
`externaltest.Start` rather than a server a case builds itself. Only a case
whose subject is the derived limit leaves it unset, starting the broker under
`jetstream.SizedByTheDisk` (an engine under `engine.SizedByTheMachine`) and
saying why; one about a refusal below the floor states a size of its own.
A node fixture that declares its broker this way declares its code cache beside
it with `brokertest.GitletCacheIn`, which sets `gitlet.cache_max_bytes` and
`gitlet.max_transfers`. Left unset, the cache's budget is derived from the room
the declared broker leaves on the runner's disk, so a node on a smaller runner
refuses to host code, and a test binary's node refuses to build Gitlet with
either unset (`engine.ErrUndeclaredCodeCache`) unless the case stands in for
the cache's volume or boots under `engine.SizedByTheMachine`.

A test broker keeps its store in a directory from `brokertest.TempDir`, never
`t.TempDir`: a node fixture's `brokertest.StoreIn` directory, a broker a case
opens or starts itself, and a server it builds from nats-server's options
alike. A stopped nats-server can land a consumer's last state flush after
`Shutdown` and `WaitForShutdown` return (its stop polls for the flush for
100 ms rather than joining it), and `t.TempDir`'s single removal then fails the
case on a file it never wrote. `brokertest.TempDir`'s removal retries while,
and only while, the directory comes back not empty, a bounded number of times
with no pause; the retry goes once a pinned nats-server joins the flush loop.
So a test binary refuses to start an embedded broker whose store lies in no
`brokertest.TempDir` (`jetstream.ErrStoreOutsideTempDir`), and `brokertest`'s
`TestEveryNATSServerATestBuildsKeepsItsStoreInATempDir` fails a server built
from nats-server's options whose store is a test's own temporary directory (a
`testing.TB`'s `TempDir`, or `os.MkdirTemp` in a test file). A store directory
a case only validates, with no broker started on it, may stay a `t.TempDir`.

A case that starts an engine never runs below `internal/engine`'s
`startedLeaseTTL`, which is `config.MinLeaseTTL` and the floor `engine.New`
holds every bootstrap to. The lease TTL is also the seat watchdog's threshold:
once a host goes that long without a beat the watchdog ends the process with
status 75, in a test the whole binary, which no assertion reports. So a case
reads the renew or the deadline it is about off the host or its store
(`seat.Host.MayStart`, a lease's deadline) rather than waiting for a lease to
lapse, and a case whose subject is a fraction of a shorter TTL builds an
`Engine` literal that starts no seat host.

What a query plan says is read in `storetest`, once. The pinned driver may
rewrite a correlated scalar subquery that selects one aggregate into a join
against its whole child table, grouped by the correlation columns, chosen by
its cost guess over a database nobody has ANALYZEd rather than by any line of
the statement. `storetest.Decorrelated` is the one check for it: the
`scalar_subquery_<n>` operand the rewrite produces, never the
`ephemeral_subquery_` index a FROM-clause subquery is joined through, held to
the driver in both directions, so a driver bump that renames or stops the
rewrite fails there before it blinds a gate. The tracker's and the knowledge
base's index gates, search's vector and lexical gates and the chat prune's room
read each refuse what `Decorrelated` flags.

`Decorrelated` sits beside the one vocabulary every plan gate reads a plan
through, over either estate's handle (`Handle`: the node's own file or the
replicated estate): `PlanTree`, `Plan` and `Details`; `IndexesIn`, `Seeks`,
`Walks`, `Sorts` and `ScansHeap`; `IndexesOn`; and `StatementLiterals`, the
walk of the module's source a gate's completeness check makes, which fails when
it read nothing outside the gate's own package. Two copies of how a plan is
spelled are two gates a driver bump blinds one at a time, so a walk of every Go
file of the module, tests included, refuses an `EXPLAIN QUERY PLAN` spelled in
a literal anywhere but `storetest/plan.go`. Package `store`'s own files alone
are exempt, since they cannot import the package that imports them.

### A fixed wait decides nothing

A test whose verdict depends on wall-clock time having passed passes with the
bug present when the event comes after its window, and fails a healthy build
when a loaded runner loses its race. That covers a `time.Sleep`, `time.After`,
`time.AfterFunc`, `time.NewTimer`, `time.NewTicker` or `time.Tick` standing
where a signal should, and a `time.Since`, `time.Until`, or a `time.Time`'s
`Sub`, `Before`, `After` or `Compare` reading the clock a claim is held to. An
ordering of two instants the code under test stamped from the wall clock
(`a.CreatedAt.Before(b.CreatedAt)`) is such a claim too: it holds only once
the clock has moved between the stamps, and a coarse clock stamps two events
equal. `internal/fixedwait` refuses a new one: it counts every reference to
those six waits and to `Since` and `Until` in test code (every `_test.go` file
and every file of a package whose directory name ends in `test`), and every
`Sub`, `Before`, `After` and `Compare` whose receiver's type is `time.Time`
(told by the type, from the module's own type-checked source, never by the
name; never `Equal`, since equality decides nothing about elapsed time). It
fails on one that no synctest bubble, no verified `//fixedwait:bubble` helper
and no marker takes out of the class, and that its inventory,
`internal/fixedwait/allowed.go`, does not hold. The inventory is the
references that predate the gate and only shrinks: an entry recording more
than its function holds fails too, each shape held to its own count (a reading
of elapsed time never stands in for a wait the inventory records, nor a wait
for a reading), and adding one is the decision.

Decide the claim by a signal instead, by the house patterns:

- a synctest bubble where the claim is about the clock (#634, #644, #652);
- a witness signal, a fence through the same channel or the producer's own
  count or state for "nothing happened" (#642);
- a wait on the parked state for a head start (#646);
- for a bound on elapsed time, #736's: the instant a wait was armed with, held
  exactly, where a fake sees the wait, the bubble's answer instant where none
  does, and otherwise the signal the bound stood in for;
- for an ordering of two stamps, the order the code already records (a log
  position, a sequence, a seq column), asserted in place of the stamps, which
  needs no clock.

A reference that decides nothing says so with a marker trailing it on its
line, or alone on the line above (a marker covers that one line only):

- on a wait: `//fixedwait:liveness` (it only bounds a wait for something that
  must happen), `//fixedwait:simulated <what
  it stands in for>` (a fake's latency) or `//fixedwait:bench` (in a function
  that takes a `*testing.B`);
- on a reading of elapsed time: `//fixedwait:measure <why it decides nothing>`
  (it is logged or reported, or it is a poll's own deadline);
- on a `time.Time`'s `Sub`, `Before`, `After` or `Compare` between two
  instants no wall clock separates: `//fixedwait:stated <which instants or
  clock>` (the test's own constants, a clock the test controls stamped both,
  or the code under test derived one from the other by its own arithmetic,
  computed once, as an expiry of the instant it stored an entry plus the
  entry's TTL; never `Since` or `Until`, which read now, never a fake whose
  "now" adds the wall time since it started, and never two independent
  readings of the wall clock).

No duration, shape or name tells a liveness bound, a logged duration or a
fake's instant from a claim on the wall clock, hence the markers. A line
holding a wait and a reading takes one marker alone above it and one trailing
it.

Before you mark a bound liveness, ask what a broken build does. If another
assertion catches it, or it never meets the condition at all, the bound is
liveness. If it would meet the condition later (the drain ends, but at its
grace rather than at once; the heartbeat re-arms, but a beat late), the bound
is the only check that catches the bug: it is a positive deadline, the claim
itself, and belongs in a bubble or in a signal the claim reads, with any sleep
that paces its poll. A wait that only moves the wall clock past an instant the
code stamped is no marker's either: hand the code a clock the test controls
(`internal/clocktest`, or a `Now` it is given). `go doc ./internal/fixedwait`
has the rules and the gaps the gate does not see.

### The end-to-end gates

`internal/e2e` runs a real engine, a real broker and the real API, then
replays the frames its socket produced through the dashboard's own
`protocol.js`. Both halves of the wire protocol are checked against each other
there and nowhere else, so it needs node too.

It is also where a FLEET signs a person in. On a clustered fleet, a Tier A
token invites somebody onto a human seat, they redeem the link and sign in on
one member, and the same cookie must then read, write as them and open the
live socket on the other member (never `401` while it catches up), while a
handshake from a foreign origin is refused and a sign-out everywhere on the
second member is refused on the first. The sign-in surface, the identity
directory and the human write surface it serves are built by
`api.NewHumanSurfaces`, the constructor `crewlet run` calls, so a seam added
to them reaches the suite with no copy to update:

```bash
make test-solo   # internal/e2e, and every other package that runs alone
```

A case there follows three rules, which `internal/e2e`'s package doc states
with their reasons:

- **A case that stands up one node, or a stateless node beside one data node,
  runs in parallel.** Each has its own directories, scripted model and broker,
  so the only thing shared would be the process environment, and a node is
  *handed* its environment instead (`nodeSpec.env`, which reaches the engine as
  `engine.Options.Environment`). `t.Setenv` cannot run beside `t.Parallel`, and
  a case that read the runner's environment held only on runners that agreed
  with it.
- **A case that stands up a fleet runs alone, before the parallel ones, and
  the fleet's claims share one fleet**: each is a subtest of
  `TestAFleetOfThree`, in the order `fleetClaims` gives, rather than a test
  paying a boot and teardown of its own. A new fleet claim is a row there. Only
  a claim that breaks its fleet on purpose (the partition case) keeps a fleet
  to itself.
- **An absence is waited on a recorded decision, never on a sleep.** A case
  asserting that something did not happen waits for the record that says it
  was decided: a delivery's `notification_skipped`, held against the reason
  `notify.SkipReason` declares; a later delivery's record, behind it in the
  order the inbound edge keeps; a Mattermost node's answer to a ping sent
  behind a post. A case of its own holds each of those orders. An absence read
  after a fixed sleep holds only as long as the sleep happened to be long
  enough.
### Benchmarks

```bash
make bench
```

runs the benchmarks the design's capacity figures come from, so each figure is
measured, not derived: object store backends (`objstoretest.Bench`, including
the broker's bucket at three replicas, an S3 fake a provider's round trip away,
and the SHA-256 rate under every verified byte), the state log, store, stream
listing, token counter and lease claim, git process pool and index-pack, the git
cache, Gitlet's compaction, applier, commit chain, push, fetch and clone paths,
a seat's reads by object id, the usage page, the connection pace, and what every
push pays to start its `proc-receive` hook, which is this binary.

It is **not a gate, and no CI job runs it**: a shared runner measures its
neighbours, and a gate on figures that noisy gets turned off. To compare two
trees, run `make bench BENCH_COUNT=6` on each, on one idle machine, and read
the outputs with [benchstat](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat).

| Variable | Default | What it sets |
|---|---|---|
| `BENCH_PKGS` | `internal/objstore`, its three backends, `internal/objstore/natsobj/replicated`, `internal/statelog`, `internal/store`, `internal/jsapi`, `internal/coord/kv`, `internal/gitlet/gitproc`, `internal/gitlet/cache`, `internal/gitlet/compact`, `internal/gitlet`, `internal/gitlet/gitletcluster`, `internal/gitlet/host`, `internal/gitlet/query`, `internal/usage`, `internal/api/paced`, `cmd/crewlet` | packages measured |
| `BENCH_PATTERN` | `.` | `-bench`, e.g. `Backend/via=member` |
| `BENCH_TIME` | `3s` | `-benchtime` |
| `BENCH_COUNT` | `1` | `-count` |
| `BENCH_TIMEOUT` | `8h`, and `24h` with `CREWLET_BENCH_LARGE=1` | `-timeout`, a per-package hang detector. Estimates from one count per arm: at six counts `internal/gitlet/cache` takes about 3.25 hours on four vCPUs (about 13 with the large arms); `internal/gitlet/host` about 2.5 hours with them |

Packages run one at a time (`-p 1`), without the race detector, verbose (`-v`),
since a skipped benchmark prints nothing otherwise. Skips:

- Real-bucket arms of `internal/objstore/s3obj`, `internal/gitlet/cache`,
  `internal/gitlet/host` and `internal/gitlet/query` skip unless
  `CREWLET_BENCH_S3_BUCKET` names a bucket (`internal/objstore/s3obj`'s doc
  lists the variables that do).
- Arms whose fixture takes minutes and gigabytes skip unless
  `CREWLET_BENCH_LARGE=1`: in `internal/gitlet/cache`, cold syncs of 2 048 and
  of 10 000 pushes, repositories of 10 000 packs, a 5 GB index and its
  published layers, a gigabyte split into hundreds of packs, an installer
  catching up 10 000 repositories, and `BenchmarkBigNode`'s 5 GB monorepo; in
  `internal/gitlet/host`, a gigabyte history, a hundred thousand refs and a
  100 000-file tree; in `internal/gitlet/query`, a gigabyte history under a
  100 000-file tree read by object id, and a gigabyte history blamed and logged
  by path; in `internal/jsapi`, streams of a million and of three million
  subjects.
- `BenchmarkBigNode`'s 12 GB monorepo (fixture builds for hours; bitmap holds
  tens of gigabytes beside the in-memory store) also needs
  `CREWLET_BENCH_BIGNODE=1`, on a machine of at least 64 GB of memory. Run it
  alone, under the day's `BENCH_TIMEOUT` that `CREWLET_BENCH_LARGE=1` gives.
- `internal/gitlet/gitproc`'s `BenchmarkTransferShare` runs its coding-box arms
  (`mix=runs`) on a transfer lane as wide as the machine's cores unless
  `CREWLET_BENCH_SHARE_SLOTS` names a width of at least two slots. The
  share-weight search must run on a production node's width, four slots a core,
  which a lane of two cannot stand in for
  ([fair share](docs/concepts/gitlet.md#fair-share)).

A Gitlet figure is the pool's git's as much as the engine's, so `make bench`,
like every test target, runs the node pin (the git the engine image builds)
first on `PATH`, never the machine's git. (`internal/gitlet/gitletcluster`'s
commit chain runs no git.) A figure the docs quote names that version; a pin
bump leaves earlier figures as measured on the version they name until a change
to that path measures again.

**A figure the design argues from is recorded in the package doc of what it
measures**, with the machine (CPUs and model, whether it has SHA instructions,
since every verified byte is hashed), the toolchain, the git version where the
figure runs git, and the versions of the dependencies it ran through. A new
object store backend runs `objstoretest.Bench` from a `BenchmarkBackend` of its
own, as it runs `objstoretest.Run` from `TestContract`. Its factory hands each
case an empty store and takes away what the case put there: a backend the run
starts goes with it; one that outlives the run (a real bucket) gives each case a
prefix of its own and `objstoretest.Clear`s it, since the suite's populations
run to a hundred thousand objects and it deletes nothing itself.

## Project conventions

- **Go 1.27+**; the module pins its toolchain, and `GOTOOLCHAIN=auto` fetches it
  rather than failing on a mismatch.
- **Interfaces are defined by the consumer**, in the calling package, kept to
  what it needs.
- **`context.Context` first, always**, threaded, not stored. A rollback takes
  `context.WithoutCancel`: the failure it undoes is often the cancellation
  itself, and a cleanup inheriting a dead context does nothing.
- **Structured logging** via `internal/logging` (`log := logging.Get("component")`);
  never the bare `slog` default. Event names are snake_case, dynamic data goes
  in key/value pairs, and a message is a name, not a sentence.
- **Errors say what to do about it.** An error a person reads names the field,
  file or variable to change, not just what failed.
- **Write concisely** in docs, code comments, this guide and the agent
  instructions (`CLAUDE.md`, `.claude/`): say each thing once, where it is
  decided, in the fewest plain words that keep it exact. No padding; no
  restating, except a rule the agent instructions mirror from a root-level file
  (both change in the same commit); no history, which the commit, pull request
  and issue keep. Most readers are agents that pay for every word on every
  later request.
- **Comments explain WHY, briefly**: the decision, and why the obvious
  alternative is wrong, within 12 lines outside a package doc. The argument is
  written once, in the package doc (within 20 KB; an ADR when it binds more than
  one package), and the comment points there. `internal/commentgate` holds both
  bounds, with an inventory of older code that only shrinks.
- **A flag or number a client receives is sent at zero too**: `omitempty` or
  `omitzero` drops a false or a 0, and the dashboard cannot tell it from a key
  nobody sent. Drop the option and declare the dashboard's member required, or,
  where a missing key and the zero are one value to every reader, say why after
  the field: `//wiregate:absent-is-false <reason>` (`absent-is-zero` on a
  number). `internal/wiregate` holds it, and the dashboard's wire types to
  declaring each as sent, with an inventory of older code that only shrinks.
- **A field holding words somebody else wrote is declared on the field**, as a
  credential's `secret:"true"` is: a read type tags it `untrusted:"fence"`
  (words at length: a body, comment, message, excerpt) or `untrusted:"name"` (a
  title, name, label), the tag `provenance.FieldTag` names. The agent runtime's
  read tools refuse a declared field they did not fence or name the writers of;
  `provenance.TestEveryDeclaredFieldHoldsText` holds every tag to those two
  kinds and to a type that holds text; `agent/builtin`'s sweep
  (`TestEveryToolThatHandsOverSomebodysWordsIsFenced`) derives from the
  declarations which tools hand such words to a model. A new read type carrying
  somebody's words declares them, or a tool answering it is never held to the
  fence; see
  [`adr/0040`](adr/0040-content-a-model-reads-carries-its-provenance.md).
- **Every piece of state answers "who has to agree on it?"**: *this node alone*
  is the node's own database file; *every node, identically, derived from an
  ordered log* is the replicated estate, written only by a state log's applier;
  *the whole company, now* is the coordination store. A FOURTH, narrow answer:
  *whichever node holds this seat, and only the current value* is a compacted
  changelog; see
  [`adr/0014`](adr/0014-a-compacted-changelog-is-the-fourth-answer.md) (a seat's
  memory answered with the first was forgotten whenever placement moved a seat).
  "It has always been in the store" is not an answer: nine tables took that
  default and cost five repair migrations. The estate gates in `internal/store`
  refuse a new table that skips the question; see
  [`adr/0003`](adr/0003-fleet-agreement-state-lives-in-coordination.md) and
  [`adr/0002`](adr/0002-the-stream-is-the-write-ahead-log.md).
- **Docs are part of the change**: any change to public APIs, config formats,
  CLI commands or behavior updates the relevant page under `docs/`.
- **Diagrams are Mermaid**: architecture, flow, sequence and state diagrams go
  in ` ```mermaid ` fences, not ASCII box art. Directory trees and file listings
  stay plain code fences (literal output).

## A build that compiles is not a backend that works

Go's compiler proves the types line up, not that a backend is wired. Two shapes:

- **A config value with no implementation.**
  `providers.sandbox.default_run_in: container` parses and appears in the
  generated schema, and validation refuses it while the company configures no
  `local.image`, because every run placed there would fail at launch. Accepting it and
  failing later, inside every turn of every seat there, would be wrong.
- **A knob nothing reads.** `provisioning.group_webhook` was validated and
  documented but read by no code, so an operator setting it got the default and
  no error. A config field ships with the code that reads it, or does not ship.

A field added to a config struct needs a test that a value in it changes what
the engine *does*, not merely that it parses.

## Dependency updates

Dependabot watches every dependency manifest and opens a pull request for a
newer release. The git, Chromium and floor Node pins are no manifest, so a
workflow of this repository's own watches them; the tool servers the docs and
examples launch are held by a test (see below).

| Surface | Watched by | What it covers |
|---|---|---|
| `.github/workflows/*.yml` | `github-actions` | the actions CI and releases run on |
| `go.mod` | `gomod` | the engine's own Go dependencies |
| `dashboard/package.json` | `npm` | the dashboard's build and runtime dependencies |
| `internal/browser/boxhelper/tree/package.json` | `npm` | the Playwright MCP a seat's box installs |
| `Dockerfile` | `docker` | the base image a release ships |
| `docker-compose.yml` | `docker-compose` | the images the local dev stack runs |
| `scripts/git-pin.env` | [`.github/workflows/pins.yml`](.github/workflows/pins.yml) (not Dependabot) | the gits the engine image builds and the suite runs |
| `scripts/chromium-pin.env` | [`.github/workflows/pins.yml`](.github/workflows/pins.yml) (not Dependabot) | the Chromiums the suite drives |
| `scripts/node-pin.env` | [`.github/workflows/pins.yml`](.github/workflows/pins.yml) (not Dependabot) | the oldest Node, with its npm, the box helper supports |

**Choosing a version goes the same direction: take the newest release, and pin
it exactly.** Find the newest in the registry, not memory, and write a literal
version (`4.2.4`), never a floating tag: `latest` and `@main` leave Dependabot
nothing to bump, and make a green run a claim about a build nobody can name
afterwards. A version may be held back for a reason, in a comment at the pin, as
the Compose stack's Postgres image does. Two kinds of version float on purpose:
what a shipped example fetches (see below), and the tool versions a workflow
names in `with:`, which Dependabot never bumps, each for the reason at its line
(`ci.yml`'s `golangci-lint` job lists them).

The configuration is [`.github/dependabot.yml`](.github/dependabot.yml): one
entry per surface on a weekly schedule, with the commit prefix its bumps carry,
plus three grouping rules on the dashboard's npm entry. CI runs on each pull
request and decides whether it lands. Twelve things to know:

- **The React family is grouped.** `react`, `react-dom`, `@types/react` and
  `@types/react-dom` arrive in one pull request, because **react-dom checks at
  runtime that `react` and `react-dom` have the exact same version**. No version
  range can say that, so npm never enforces it and a caret is no protection.
  Ungrouped, React 19.3.0 made two red pull requests: the `react-dom` half failed
  to install (`ERESOLVE`: `@types/react-dom@19.3.0` peers
  `@types/react: ^19.3.0`, `react-dom@19.3.0` peers `react: ^19.3.0`, against
  19.2.x), the `react` half
  installed and failed 19 of 36 test suites. `@testing-library/react` is outside
  the group (its react peers are `^18.0.0 || ^19.0.0`, which ride a React
  minor), and `@vitejs/plugin-react` because it declares no react peer, only
  `vite: ^8.0.0`.
- **`vite`, `vitest`, `@vitest/*` and `@vitejs/plugin-react` are grouped.**
  `vite` is a *required* peer of the plugin (`^8.0.0`) and of vitest
  (`^6.4.0 || ^7.0.0 || ^8.0.0`), so a lone `vite` 9 bump breaks both ceilings;
  minors were never the problem (8.2.2 → 8.3.0 landed alone). And `vitest` and
  `@vitest/browser-playwright`, the layout suites' browser provider, peer each
  other at the *exact* same version, so every vitest release needs both in one
  pull request. Its cost: a bump of `vite` or the plugin alone is titled for the
  group, with the packages it moved in the body.
- **The design system's four packages are grouped, because its components pin
  its tokens and icons.** `@crewlethq/ui` depends on `@crewlethq/tokens` and
  `@crewlethq/icons` at an *exact* version, and all four release together. A
  lone bump of one of those three installs cleanly and wrongly: a newer `ui`
  beside the direct `tokens` nests a second copy of the tokens, so components
  and the dashboard's stylesheets paint from different palettes, which no
  install error or test sees. The pattern is `@crewlethq/*`, because the pinning
  is the scope's convention, so a package `ui` comes to pin is grouped with no
  edit. It also takes `@crewlethq/artifact-kit`, which nothing pins, so a
  design-system release arrives as one pull request.
- **Nothing else is grouped; that is the rule.** The only other *required* edges
  a bump could split are `@testing-library/react`'s (react and react-dom at
  `^18.0.0 || ^19.0.0`, which ride a minor, and `@testing-library/dom`, a
  transitive Dependabot moves with it). Every remaining vite peer
  (`@vitest/mocker`'s, vite's own on `@types/node` and the bundlers) is
  optional, and npm does not fail on an unsatisfied optional peer. A group
  raises its members together under a title naming the group, so it costs a
  vaguer subject and puts one member's bad release in front of the rest. Pay
  that only where a split bump is genuinely broken, not for neatness.
- **A dashboard bump that changes the emitted bytes needs a rebuilt bundle, and
  gets one.** The output is committed (`static/dashboard`, `static/artifacts` and
  `static/protocol`, the Makefile's `UI_OUT`) and the `dashboard gates` job
  rebuilds and diffs it, but Dependabot edits only `package.json` and the
  lockfile.
  [`dependabot-dashboard.yml`](.github/workflows/dependabot-dashboard.yml)
  rebuilds and pushes it: see [A dashboard bump brings its own
  bundle](#a-dashboard-bump-brings-its-own-bundle). Bumps that leave the output
  unchanged (`@types/*`, `prettier`, a byte-identical build tool) get no commit.
- **A Go bump that moves the note helper's builds gets them rebuilt.** A Go
  build carries its modules' versions, so a bump of a module `internal/notehelper`
  links (`golang.org/x/net`, `github.com/klauspost/compress`) changes the
  committed builds, which ci.yml's build job rebuilds and diffs.
  [`dependabot-notehelper.yml`](.github/workflows/dependabot-notehelper.yml)
  rebuilds and pushes them: see [A Go bump brings its own note helper
  builds](#a-go-bump-brings-its-own-note-helper-builds).
- **A Playwright MCP bump waits for a person.** The tree a seat's box installs
  (`internal/browser/boxhelper/tree/`) has an npm entry and no group; its pull
  request never merges itself, and stays red until `internal/browser`'s release
  constants and `tree/installed.sha256` name what it moved: see [A Playwright
  MCP bump is read before it
  lands](#a-playwright-mcp-bump-is-read-before-it-lands).
- **A Compose image can be held back on purpose**, with the reason in a comment
  beside the pin: `mattermost-db` holds its Postgres major because an existing
  `mattermost-pgdata` volume will not open under a newer one without a
  `pg_upgrade` or a dump/restore. Close that pull request; Dependabot does not
  reopen one for a version you turned down.
- **`docker` and `docker-compose` are two ecosystems**: `Dockerfile`s and Compose
  files; a repository with both needs both entries.
- **An action pinned to a non-version ref is invisible.** A branch pointer
  (`@release/v1`) or `@main` yields no update pull requests, so pin actions to a
  version tag or a full SHA, most of all the one handed an OIDC token. The
  release and main-image workflows pin every action by **full commit SHA**, with
  the release in a trailing comment (`@<sha> # v7.2.3`) that Dependabot rewrites
  with the pin: they hold the registry and the signing identity (and the release
  workflow the Releases), and an owner can move a version tag under them.
- **The git, Chromium and floor Node pins are watched by a workflow, and a
  bump arrives as an issue, not a pull request.** Every Monday
  [`pins.yml`](.github/workflows/pins.yml) asks kernel.org whether each git pin
  is still the newest release of its series, Chrome for Testing whether each
  Chromium pin is the release its rule names, and nodejs.org whether the floor
  Node is the newest patch of its major, and keeps ONE issue per pin file,
  `git pin behind upstream`, `chromium pin behind upstream` or `node pin behind
  upstream`; see [The git pin is checked every
  week](#the-git-pin-is-checked-every-week), [The Chromium pin is checked every
  week](#the-chromium-pin-is-checked-every-week) and [The floor Node is checked
  every week](#the-floor-node-is-checked-every-week).
- **goreleaser itself floats within its major.** Every goreleaser step in
  `.github/workflows/release.yml` asks for `version: "~> v2"`, because Dependabot
  never bumps a `with:` input, so an exact version would age unwatched. The major
  is held because `.goreleaser.yaml` is a `version: 2` config: v3 is a hand
  migration of that file in the same change, rehearsed with `make snapshot`.

A new dependency surface (a second `package.json`, a second module) needs its
`updates:` entry in the same change; nothing reports the omission.

The tool servers the docs, examples and skills launch sit in a page's YAML or a
setup step's command line, out of Dependabot's sight. (The Playwright MCP a
seat's box installs is not one of them: it is a lock Dependabot watches.)
[`internal/config/toolservers_test.go`](internal/config/toolservers_test.go)
holds them:

- Every package a page or skill fetches by name (an `mcp_servers` entry, a
  `uvx`, `npx`, `npm exec` or `pip install` it tells somebody to run) names an
  exact release.
- Every server a document launches is an entry of the curated MCP catalogue
  ([`internal/mcp/catalogue`](internal/mcp/catalogue)), whose `Package` and
  `Release` are what the documents and catalogue say about it (tool names,
  variables) was read against.
- Every document that names a release names that one; the catalogue's own
  launches are held too, so the dashboard's release and a page's are one.

Moving one means reading the new release first, correcting whatever moved, and
setting the entry's `Release` in the same change. Nothing proposes the move, so
check the registry whenever you touch those pages. Reading includes running the
documented launch through `initialize` and `tools/list`: a runner resolves every
unbounded dependency to its newest, so a release whose source reads right can
fail to start (`mcp-linear` 0.1.1 declares `mcp>=0.1.0` and dies at import on
mcp 2).

The shipped examples (`examples/`) are held the other way round: an example is
copied long after it was written, so every package it fetches names no release
(`@latest` at most). The one exception is a server a bundled tool skill
(`examples/tool-skills/`) triggers on by tool name (`mcp-server-mattermost`
today), whose entry names `readAt` exactly, because a release that renamed the
tool would silently disarm the skill. The same test derives the exception from
the skills' triggers, so a pin no skill needs fails as surely as a skill on a
server that floats.

Security updates come from published advisories, are enabled in the repository's
settings, and are held back by neither the weekly schedule nor Dependabot's
default release cooldown.

**Promoting an indirect dependency to a direct one changes the module's
surface**; `go mod tidy` records it, and a wrong `// indirect` marker fails
`go mod tidy -diff` in `make check`. `golang.org/x/crypto` is direct because
`internal/iam/credential` calls `argon2` from it: a password hash is the one
piece of cryptography here not hand-written, because it is maintained by the
standard library's crypto maintainers and has no second implementation to
disagree with. The rest of that package (TOTP, recovery codes, token digests) is
`crypto/hmac`, `crypto/sha1`, `crypto/sha256` and `encoding/base32`: a module
earns its place against what `std` does, and for forty lines of RFC 6238
arithmetic `std` does all of it.

### A bump merges itself

[`.github/workflows/dependabot-merge.yml`](.github/workflows/dependabot-merge.yml)
approves a pull request Dependabot opened and queues it with `gh pr merge
--auto --squash`. CI *is* the review a dependency change gets: `--auto` holds the
merge until every check `main`'s protection rule requires has passed, and a bump
CI rejects sits red until a person looks. All but one kind: a pull request that
changes only `internal/browser/boxhelper/tree/` starts no run (the workflow's
`paths-ignore`), because a person reads and lands that bump (see [A Playwright
MCP bump is read before it lands](#a-playwright-mcp-bump-is-read-before-it-lands)).

Two settings outside this repository's files hold that up. `main`'s protection
rule must **require** the `ci` checks: `--auto` waits only for the checks a rule
names, so with no rule the bump merges the instant it is mergeable. And **Allow
auto-merge** must be on under Settings → General, or the step fails loudly.

Require the `ci.yml` jobs a pull request runs: `build +
vet`, `sign-off`,
`dashboard`, `test (race)`, `end-to-end gates`, `golangci-lint` and the four
`cross-compile the release targets` legs. A rule matches a check by exact name,
and a matrix leg's name carries its matrix values
(`cross-compile the release targets (linux, amd64)`), so each leg is required by
its full name. A required check that never reports blocks every pull request, so
renaming a job is a change to the rule too. `test (race)` and `end-to-end gates`
are aggregates over the `test (race) I/N` and `end-to-end gates I/N` legs; the
rule names the aggregates, never a shard, whose name changes with the shard
count.

The job runs only when Dependabot is both the pull request's author *and* the
actor that triggered the run, so the workflow never approves a commit a person
pushed onto a Dependabot branch. Cost, under a rule that dismisses stale
approvals: clicking **Update branch** yourself can leave the pull request
unapproved, because your click's run is skipped. `@dependabot recreate` from a
person with push access recovers it. (Not `rebase`: your merge commit is one
Dependabot did not write, and it will not rebase a branch holding one.)

The approval is given only while GitHub says one is needed. The job runs on
`opened` and on each Dependabot push, and under a rule that keeps approvals
across pushes (`main`'s today) every run after the first would stack another
review. It reads `reviewDecision` and skips on `APPROVED` and on
`CHANGES_REQUESTED` (this account's approval would not clear it). Everything
else approves, an empty decision included: GitHub leaves it empty when nothing
requires a review, and has been reported to leave it empty while a ruleset's
requirement still blocks the merge (radius-project/radius#13111), so a skip on
doubt could stall every bump with nothing red to show.

Each entry in `.github/dependabot.yml` pins the commit prefix, the same on every
surface: `build(deps)`, because a bump changes what the project builds against
whichever file records it (an action bump is `build(deps)`, not `ci(deps)`).
Left unset, Dependabot infers the prefix, and an inference that changes its mind
writes a bare `Bump x from 1 to 2` into `main` and the release notes. Dependabot
capitalises `Bump` and offers no way not to, so a bump is the one subject here
that does not start lowercase.

Nothing checks any of that: `internal/version`'s tests of the author/actor guard
and the `--auto --squash` flags were dropped, and no linter reads a workflow
file. Every failure is silent (drop `--auto` and, while `main` requires the
checks, the merge step goes red; with no such rule it merges before a check
reports), and the job holds `contents: write` and `pull-requests: write`. Read
the `paths-ignore`, the `if:`, the approval's condition and the merge command on
any diff that touches
[`.github/workflows/dependabot-merge.yml`](.github/workflows/dependabot-merge.yml).

### A dashboard bump brings its own bundle

[`.github/workflows/dependabot-dashboard.yml`](.github/workflows/dependabot-dashboard.yml)
runs when Dependabot opens or updates a pull request that touches `dashboard/`.
It runs `make dashboard` and, if any tree the build writes differs, commits the
result onto Dependabot's branch as `build(dashboard): rebuild the bundle for this
dependency bump`, signed off. ci.yml then runs on the new head and the drift
gate passes. A bump that leaves the output as it was pushes nothing.

It reads the trees from the Makefile's `UI_OUT` (`static/dashboard`,
`static/artifacts` and `static/protocol`), not a list of its own: a bump moves
more than the dashboard (Vite 8.3.5 rewrote the page runtime under
`static/artifacts` too). A design-system bump moves `@crewlethq/artifact-kit`
with the rest, and the workflow commits the new kit version's directory; the bump
then fails `static/kit_test.go` until a person appends that version to its
ledger, so a kit version joins what published pages may pin in a commit somebody
reviewed.

It is two jobs because one runs somebody else's code. `npm ci` and the bundler
execute whatever a dependency published this week, so that job holds no secret
and a read-only token, and passes the next job only a directory of files. The
second job holds the credential and runs only the runner's own tools and two of
GitHub's own actions. It learns where it may write from its own checkout, never
from the job that ran the bump's code: it reads `UI_OUT` as text (running even
`make` there would run the checkout beside the token) and refuses any tree that
is not a directory directly under `static/`. It checks the bundle's file *names*
against an allowlist (every tree sits inside the Go module, and a stray
`zz_test.go` would run under every `go test ./...`); refuses a file outside the
trees; replaces each tree the rebuild changed; and pushes without `--force`, so a
branch Dependabot moved since the run began is refused, not overwritten. A new
kind of file in the bundle has to be added to the allowlist on purpose.

The push uses a personal access token, not `GITHUB_TOKEN`: a push with the
workflow's own token leaves the pull request's runs waiting for someone to click
*Approve workflows to run*, and the auto-merge behind them waits too. Two things
outside this repository's files must exist:

- **A fine-grained personal access token**, preferably of an account made for the
  purpose. Resource owner: the organisation. Repository access: this repository
  only. One permission: **Contents, read and write**. Not Workflows (its absence
  stops this credential rewriting a workflow file), not Pull requests, and not a
  classic token, whose `repo` scope reaches every repository the account can.
  The account needs Write access, and an owner must approve the token where the
  organisation asks. Contents: write can create a tag, so keep `v*` behind a tag
  ruleset the account is not on the bypass list of. A fine-grained token
  expires, a year at most; calendar the date.
- **That token as a Dependabot secret**, `DASHBOARD_BUNDLE_TOKEN`, under Settings
  → Secrets and variables → **Dependabot**. Not an Actions secret: a run
  Dependabot starts cannot see those, so it is empty there. The job fails naming
  the secret when it is.

The workflow decides nothing. [`dependabot-merge.yml`](.github/workflows/dependabot-merge.yml)
approved Dependabot's commit, and `main`'s ruleset decides whether the bundle
commit on top lands. With *dismiss stale approvals* and *require approval of the
most recent push* both off, **as they are today**, a bump `dependabot-merge.yml`
queued merges itself once CI is green, and so does *anything* pushed onto its
open branch: a bundle, a commit from a person with write access, or from
whoever holds this token. Turning on **Require approval of the most recent reviewable push** puts a
person in front of that, at the price that a bump whose bundle was rebuilt then
waits for one.

**When one bump merges the others conflict**, because each carries its own
rebuilt bundle and the emitted names are content-hashed. Dependabot rebases a
conflicted bump itself (#167 conflicted when #168 merged and was rebased two
minutes later), but only while every commit on the branch is its own or carries
`[dependabot skip]`, GitHub's marker for "Dependabot may force-push over this
commit", which is why the bundle commit carries it. Each rebase drops the bundle
commit, and the workflow runs again and pushes a fresh one. Dependabot will not
rebase a branch holding a commit without the marker, or a bump left open for 30
days; the next section asks again.

Nothing checks any of this, and the workflow holds a credential that can write
to a branch. Read the `if:` on both jobs, each job's `permissions:` and the
token's scope on any diff that touches
[`.github/workflows/dependabot-dashboard.yml`](.github/workflows/dependabot-dashboard.yml).

### A Go bump brings its own note helper builds

[`.github/workflows/dependabot-notehelper.yml`](.github/workflows/dependabot-notehelper.yml)
is the same workflow for `internal/sandbox/executor/notehelper/`, run when
Dependabot opens or updates a pull request that touches `go.mod` or `go.sum`. It
runs `make notehelper` and, if either build differs, commits both as
`build(notehelper): rebuild the note helper for this dependency bump`; a bump of
a module the helper does not link pushes nothing. Its guards, its two jobs, its
token and its `[dependabot skip]` are the bundle workflow's, line for line, and
change with them. Its `push` job writes the two builds by name, after checking
the handed-over directory holds exactly those two regular gzip files, and ci.yml
rebuilds them from source on the new head, so a build the untrusted job altered
fails the gate rather than landing. Read it as the bundle workflow on any diff.

### A Playwright MCP bump is read before it lands

The Playwright MCP a seat's box installs is a manifest and a lock in
[`internal/browser/boxhelper/tree/`](internal/browser/boxhelper/tree), which npm
in the box installs with `npm ci`, and Dependabot proposes each release of it.
Its pull request stays red: `internal/browser`'s release constants name the
release somebody read, boxhelper's tests hold the lock to them, and the suite's
recipe and those tests hold the files it installs to `tree/installed.sha256`
(boxhelper's package doc says why). Nor does it merge itself:
`dependabot-merge.yml` neither
approves nor queues a pull request that changes only this directory (see [A
bump merges itself](#a-bump-merges-itself)). Land the bump from a branch of
your own, not Dependabot's, which stops rebasing once it holds a commit of
yours and carries a title of about 90 characters, where a subject takes 72:

1. In `internal/browser/boxhelper/tree/`, run `npm install --package-lock-only
   --ignore-scripts --save-exact --registry=https://registry.npmjs.org/
   @playwright/mcp@<b>`. Never behind a mirror, whose host the lock would then
   name.
2. `scripts/playwright-tree.sh` refuses the new lock's tree, naming the digest
   of the files it installed: write that digest, which every box holds its
   install to, into `internal/browser/boxhelper/tree/installed.sha256`. Then
   `tree=$(scripts/playwright-tree.sh) && (cd "$tree" && npm audit signatures
   --registry=https://registry.npmjs.org/)`: the recipe installs the new lock's
   tree and prints its path, and every package in it must show a verified
   registry signature and a verified attestation, read from npm's registry
   itself, as step 1's lock is, since a mirror need not serve either.
3. Read the release. Then move `PlaywrightMCPRelease` and
   `PlaywrightCoreRelease` in
   [`internal/browser/playwright.go`](internal/browser/playwright.go) to the
   lock's versions, and the expected build to the chromium entry of
   `$tree/node_modules/playwright-core/browsers.json`;
   `TestTheLockIsTheReleaseSomebodyRead` and
   `TestTheLockedCoreExpectsTheChromiumSomebodyRead` name what they found.
   Read again, too, what the box helper relies on that no test names whole:
   createConnection's context getter, which is undocumented (boxhelper's
   package doc, "Why Playwright MCP is embedded, not run"). The switches it
   launches Chromium with and the tools it shows the model are held to lists,
   by `TestChromiumRunsWithTheListedCommandLine` and
   `TestTheModelSeesOnlyTheAllowedTools`, which name what changed.
4. `make check`, and title the pull request `build(deps): bump playwright mcp
   from <a> to <b>`. Dependabot closes its own once `main` carries the release.

### A conflicted bump is asked to recreate

[`.github/workflows/dependabot-recreate.yml`](.github/workflows/dependabot-recreate.yml)
runs on every push to `main`, and by hand from the Actions tab, where a **dry
run** box reports what it would comment without commenting.

Dependabot rebases a conflicted bump itself while every commit on it is its own
or carries `[dependabot skip]` and the bump is under 30 days old (#172 went into
conflict when #165 merged and Dependabot rebased it alone); the workflow leaves
those to it. A bump holding any other commit is never rebased again, and
Dependabot refuses `@dependabot rebase` there ("edited by someone other than
Dependabot"), so the workflow comments `@dependabot recreate`, which rebuilds the
branch and **overwrites every commit on it, a person's included**. Work you mean
to keep on a Dependabot branch belongs on a branch of your own. A bump over 30
days old that is Dependabot's alone is named in the run's summary as needing a
person. Drafts are left alone, and so is a bump Dependabot says it is rebasing.

Each request carries the head commit it was made for in an HTML comment the pull
request page does not show, so a bump is asked once per head; after three
requests it is named as needing a person instead. If Dependabot answers with
"only users with push access can use that command", the run fails.

The comment has to come from a user account: Dependabot ignores commands from
`github-actions[bot]` and GitHub Apps
([dependabot-core#9147](https://github.com/dependabot/dependabot-core/issues/9147)).
So it uses `DEPENDABOT_RECREATE_TOKEN`, deliberately not
`DASHBOARD_BUNDLE_TOKEN`: one token that could both push to a branch and approve
could land anything alone. Two things outside this repository's files must
exist:

- **A fine-grained personal access token**: resource owner the organisation, this
  repository only, **Pull requests: read and write** and nothing else, owned by an
  account with Write access. This scope also covers approving pull requests, and
  such an approval counts toward `main`'s required review, which is why the next
  item is not optional.
- **An environment**, `dependabot-recreate`, whose deployment branches are limited
  to the selected branch `main`, holding the token as its secret (Settings →
  Environments). Any writer's branch can read an ordinary Actions secret; only a
  run on `main` can read an environment secret. Create it **before** the
  workflow reaches `main`: a workflow naming a missing environment creates it,
  open to every branch.

A `Resource not accessible by personal access token (HTTP 403)` on the comment
means the token can read but not write (the repository is public, so a token
reads it whatever it was granted). Check that the token's repository access
includes this repository, that Pull requests is Read and write, that an
organisation owner approved it if required, and that its account has Write
access.

Not covered: a bump merges itself with `GITHUB_TOKEN`, and GitHub starts no
workflow for an event that token caused, so a conflict a bump's merge makes waits
for the next merge by a person, or a manual run.

Nothing checks any of this. Read which bumps it comments on, the command and
marker it posts, the environment and the token's scope on any diff that touches
[`.github/workflows/dependabot-recreate.yml`](.github/workflows/dependabot-recreate.yml).

### The git pin is checked every week

[`scripts/git-pin.env`](scripts/git-pin.env) pins three gits, each a kernel.org
tarball by version and SHA-256: the node pin (newest release; the engine image
builds it and every pool and client in the suite runs it) and the newest patches
of the two floors' series (the floors' arm). Every Gitlet node runs the image's git, so a git security release reaches a
deployment only through a bump of this pin (Debian's updates to its own git never do), and no Dependabot ecosystem reads a
source tarball, so [`pins.yml`](.github/workflows/pins.yml) is the watcher.
Every Monday at 06:17 UTC (and on **Actions → pins → Run workflow**) its `gits`
leg runs [`scripts/git-pin-check.sh`](scripts/git-pin-check.sh). It reads
kernel.org's listing for each series' newest release (release candidates are no
release); for each pin behind, it downloads the release and its signature
(`git-<version>.tar.sign`, over the decompressed tar) and verifies it against the
git maintainer's key, [`scripts/git-signing-key.asc`](scripts/git-signing-key.asc),
imported into a private GnuPG home and refused unless it holds exactly the key
whose primary fingerprint the script pins. It then prints the pin's two new lines
(the SHA-256 of the very tarball whose signature verified, never one read from a listing) and the title of the pull request that
moves it.

- Exit 1 opens or updates ONE issue, `git pin behind upstream` (labelled
  `dependencies`, found by that exact title), with the report as body, and fails
  the run.
- Exit 0 closes it.
- Exit 2 (listing or download refused, signature that does not verify or by
  another key) fails the run and leaves the issue as it was.

An issue, not a pull request: one opened with `GITHUB_TOKEN` leaves its checks
waiting for a click (as under [A dashboard bump brings its own
bundle](#a-dashboard-bump-brings-its-own-bundle)), and checks that run by
themselves need a new credential. The workflow holds `contents: read` and
`issues: write` and no secret; the git pin is one leg of its matrix, each
other pin another, under its own issue (see [The Chromium pin is checked every
week](#the-chromium-pin-is-checked-every-week) and [The floor Node is checked
every week](#the-floor-node-is-checked-every-week)).

**Making the bump** is a pull request of its own:

1. Replace the pin's two lines in `scripts/git-pin.env` with the ones the issue
   names (`scripts/git-pin-check.sh` prints them again by hand: exit 1 with the
   report, 0 when every pin is current).
2. `make git` (builds the new git from the tarball the digest names; a wrong
   digest refuses the build), then `make check`.
3. Measure what the published layers' rules rest on, on the new git and one
   idle machine:
   `make bench BENCH_PKGS=./internal/gitlet/cache BENCH_PATTERN='Midx/size=/^step='`
   (the `git=floor` arms run the node floor's own build). Each arm states the
   rule it holds; the same pull request re-decides a rule its arm no longer
   holds:
   - **No node writes a multi-pack-index layer of its own**
     (`internal/gitlet/cache`'s package doc): re-decided once `step=tip`'s
     `peak-MiB` stops growing with the history. On git 2.56.0 the same 64
     pushes' layer held 88 MiB over 100 MB and 254 MiB over the gigabyte, and
     took longer than `step=whole/write=bitmap`'s whole rewrite.
   - **A layer is written over exactly its base, never merged with
     `multi-pack-index compact`** (`internal/gitlet/compact`'s): re-decided once
     `merge=stacked` no longer reports `pathological`, or `merge=root-compact`
     beats `merge=root-write` by more than the tail's layer write a compaction
     from the root cannot skip (`step=publish/layer=tail`'s `midx-s/op`).
   - **A published layer covers every live pack its base does not**:
     re-decided once `step=closure` fails (git wrote a bitmap over packs that are
     not closed).

   The constants the same arms measure (`internal/gitlet/cache`'s bytes an
   object, `internal/gitlet/compact`'s publication cadence) say at their
   definitions what moves each.
4. Title the pull request as the report does: `build(deps): bump git from <a>
   to <b>` for the node pin, `build(deps): bump git's node floor from <a> to
   <b>` or `… client floor …` for a floor.

The pull request runs the snapshot job (builds the image's git from the new pin
on both platforms and holds it to the pin), and the suite builds the three gits
on CI (about four minutes) until `main` saves them. A diff to the pin, to
[`scripts/git.sh`](scripts/git.sh) or to the key changes the git every Gitlet
node runs, so it is read as release surface. A floor pin only moves to a newer
patch of its own series; raising a floor is a change to
`gitproc.NodeMinVersion` or `ClientMinVersion`, argued in
[ADR-0087](adr/0087-a-git-node-runs-git-2-55-from-one-pin.md). A git 3.x node
pin changes the recipe too: git's Makefile makes Rust mandatory from 3.0, which
the report says when it names one.

**The signing key is checked in the same run.** The maintainer's release-signing
subkeys expire (those in the file on 2028-01-11); after that no release verifies
and every run fails with exit 2. So the run's last step,
`scripts/git-pin-check.sh key`, reads the latest expiry among the key's valid
signing subkeys, bounded by the primary key's own, and fails the run from 60
days before it (eight weekly runs to act), whether or not a pin is behind. To
refresh: export `keys/20D04E5A713660A7.asc` again from kernel.org's PGP keyring
([pgpkeys.git](https://git.kernel.org/pub/scm/docs/kernel/pgpkeys.git)), check
its primary fingerprint is still `96E07AF25771955980DAD10020D04E5A713660A7`
(`gpg --show-keys scripts/git-signing-key.asc`), replace the file, and land it
as `build(deps): refresh git's release signing key`. A file that is not that
key, or holds a second key, is refused (exit 2). So is a release signed by a
subkey the file does not hold yet (added by the maintainer after the export);
that refusal names the subkey and prints these steps.

[`scripts/git-pin_test.sh`](scripts/git-pin_test.sh) (`make git-test`, in `make
check` and ci.yml's `build + vet`) is the check's and recipe's own suite. It runs
both against a stand-in listing, tarballs and signing keys, so it reads no
network and builds no git; every case holds a mutation (skip the fingerprint
check, accept gpg's exit status without its `GOODSIG`, read the primary key's
expiry alone) that fails it. It reads nothing of your environment, and proves
it by running itself a second time, by a relative path under a `CDPATH` naming
another `scripts/`, in a hostile environment: every knob the scripts read
pointed elsewhere, a GnuPG home of yours, and a curl configuration (`.curlrc`)
refusing the `file://` downloads. The scripts themselves do read your curl
configuration (a proxy or CA bundle applies where they really download) and send
what it makes curl print (a `write-out`, say) to stderr; a case holds the
check's report and the recipe's bin directory to being all either prints on
stdout under such a file. Nothing holds the
workflow file itself: on any diff that touches it, read its `permissions:`, the
issue title it matches and the step that runs the key check.

### The Chromium pin is checked every week

[`scripts/chromium-pin.env`](scripts/chromium-pin.env) pins two Chrome for
Testing builds the suite drives, each by version and the SHA-256 of its zip on
each of the four platforms the engine is released for (`linux64`,
`linux-arm64`, `mac-x64`, `mac-arm64`): the **newest** (Stable channel) and the
**floor** (the newest patch of `internal/browser.MinChromiumMajor`'s major, the
oldest the engine accepts). No release carries either (the engine image has no
Chromium), so a bump changes what the suite certifies, nothing a deployment
runs. The `Chromiums` leg of [`pins.yml`](.github/workflows/pins.yml) runs
[`scripts/chromium-pin-check.sh`](scripts/chromium-pin-check.sh) beside the git
leg every Monday:

- **The newest pin moves for a new major, never for a patch.** The check
  reads `LATEST_RELEASE_STABLE` and calls the pin behind only when Stable names
  a newer major: a patch changes no protocol, and the suite drives only pages it
  serves on loopback, so patches would open the issue every week for nothing.
- **The floor pin moves to the newest patch of its own major**
  (`LATEST_RELEASE_<major>`).
- **Every platform or none.** For a pin behind, the check downloads the
  release's zip for each of the four platforms, hashes it, and prints the pin's
  new lines and the pull request title. A release missing any platform is never
  proposed: a pin a supported platform cannot fetch is a gate that platform
  skips.
- **The trust anchor is that download.** Chrome for Testing has no signatures,
  so a digest rests on Google's bucket over HTTPS and on the reviewer of the pull
  request; every later fetch must match it
  ([`scripts/chromium.sh`](scripts/chromium.sh) refuses a zip that does not). A
  listing's answer must be a version of four numbers before it reaches a URL, a
  file name or the report.

Exit 1 opens or updates ONE issue, `chromium pin behind upstream` (labelled
`dependencies`, found by that exact title), with the report as body, and fails
the leg; exit 0 closes it; exit 2 fails the leg and leaves the issue as it was.
Exit 2 covers: a listing refused or answering no version, a release missing a
platform (the bucket answering 404 or 403 for its zip), a zip that could not be
downloaded for another reason (said as that, with curl's exit status), and a pin
ahead of the listing.

**Making the bump** is a pull request of its own: replace the pin's lines in
`scripts/chromium-pin.env` with the ones the issue names
(`scripts/chromium-pin-check.sh` prints them again by hand; it downloads each
zip into `TMPDIR`, about 200 MB at a time, so point `TMPDIR` at a disk with that
room where `/tmp` is a small tmpfs), run `make chromium` (fetches the new build,
refuses a zip that is not the digest), then `make check`, and title the pull
request as the report does:
`build(deps): bump chromium from <a> to <b>` for the newest pin,
`build(deps): bump chromium's floor from <a> to <b>` for the floor. The suite
fetches the new build on CI until `main` saves it.

**Raising the floor** is never a bump: it is a change to
`internal/browser.MinChromiumMajor`, argued in `internal/browser`'s package doc
with what it measured, and it moves the floor pin to the newest patch of the new
major in the same pull request; `TestThePinsMeetTheFloor` fails while the pin's
major and `MinChromiumMajor` disagree.

[`scripts/chromium-pin_test.sh`](scripts/chromium-pin_test.sh) (`make
chromium-test`, in `make check` and ci.yml's `build + vet`) is the check's and
recipe's own suite: it runs both against a stand-in tree and listing under
`file://` (no network, no Chromium fetched), every case holds a mutation
(compare the newest pin by its patch, skip a platform whose zip is missing, take
a listing's answer unchecked) that fails it, and like the git suite it runs
itself a second time under a hostile `CDPATH` environment. Nothing holds the
workflow file itself: on any diff that touches it, read its `permissions:`, each
leg's issue title and the key step of the git and floor Node legs.

### The floor Node is checked every week

[`scripts/node-pin.env`](scripts/node-pin.env) pins the oldest Node the box
helper supports, with the npm it bundles: the newest release of Node
`internal/browser/boxhelper.MinNodeMajor`, by version and the SHA-256 of its
tarball on each of the four platforms the engine is released for. No release
carries it (the engine image has no Node), so a bump changes what the suite
certifies, nothing a deployment runs. The `floor Node` leg of
[`pins.yml`](.github/workflows/pins.yml) runs
[`scripts/node-pin-check.sh`](scripts/node-pin-check.sh) beside the other legs
every Monday:

- **The pin moves to the newest patch of its major**, read from nodejs.org's
  release index; a prerelease, or any line whose version is not three numbers,
  is never one.
- **The major is held to its support window.** From 60 days before the floor's
  major reaches its end of life in nodejs/Release's `schedule.json`, the check
  fails as for a pin behind, naming the newest release of the major to raise
  it to: of the majors above it, the oldest whose long-term support has begun
  (its `lts` date passed) and whose own end of life is more than 60 days away.
  A major with no `lts` is never a floor, and an odd one with one (Node 27) is.
- **Every release it proposes is one a releaser signed.** It verifies the
  release's `SHASUMS256.txt` against the Node releasers' keys,
  [`scripts/node-release-keys.asc`](scripts/node-release-keys.asc), imported
  into a private GnuPG home and refused unless it holds exactly the primary
  keys whose fingerprints the script pins; then it downloads each platform's
  tarball and requires its SHA-256 to be the one that signed list names. A
  signature by a key the file does not hold is refused naming it.
- **An answer is a version or a date, or nothing**: of the index only lines of
  three numbers are read, and of the schedule only an `end` or an `lts` of the
  form `YYYY-MM-DD`.

Exit 1 opens or updates ONE issue, `node pin behind upstream` (labelled
`dependencies`, found by that exact title), with the report as body, and fails
the leg; exit 0 closes it; exit 2 fails the leg and leaves the issue as it was.
Exit 2 covers: the index, the schedule or a download refused, a signature that
does not verify or by a key the file does not hold, a tarball that is not the
signed one, a pin ahead of the index, and a floor near its end of life with no
major to raise it to.

**Making the bump** is a pull request of its own: replace the pin's lines in
`scripts/node-pin.env` with the ones the issue names
(`scripts/node-pin-check.sh` prints them again by hand), run `make node` (it
fetches the new build and refuses a tarball that is not the digest), then
`make check`, and title the pull request as the report does:
`build(deps): bump node's floor from <a> to <b>`.

**Raising the floor** is never a bump: it changes `MinNodeMajor`, the oldest
Node a box must carry, and `MinNPMMajor`, to the npm the new floor bundles, with
[`scripts/playwright-tree.sh`](scripts/playwright-tree.sh)'s `min_npm`, and
moves the pin to the new major's newest release in the same pull request,
titled `build(deps): raise node's floor from <a> to <b>`, each a major;
`TestTheFloorNodeIsTheMajorItStandsFor` fails while the pin and the two
constants disagree, and `TestTheTreeRecipeRefusesTheNPMTheHelperRefuses` while
`min_npm` and `MinNPMMajor` do.

**The keys are checked in the same run.** Some releasers' keys or their signing
subkeys expire, and a release signed after that with the key extended cannot
verify against the file's copy, so every run would fail with exit 2. The leg's
last step, `scripts/node-pin-check.sh key`, reads when each pinned key stops
signing, by the git key's rule (the latest expiry among its valid signing
subkeys, bounded by the primary's own), and fails the run from 60 days before
any key's date, naming each such key, whether or not the pin is behind. To
refresh the file, read the active releasers' keys from
[nodejs/release-keys](https://github.com/nodejs/release-keys) (its README's
"Active releasers keys", which the "Release keys" section of Node's own README
must list too), put their `keys/<fingerprint>.asc` files in
`scripts/node-release-keys.asc` and their fingerprints in the list
`scripts/node-pin-check.sh` pins, leaving out a key upstream no longer lists or
one that has lapsed unextended, run the check and `key` again, and land it as
`build(deps): refresh node's release signing keys`.

[`scripts/node-pin_test.sh`](scripts/node-pin_test.sh) (`make node-test`, in
`make check` and ci.yml's `build + vet`) is the check's and recipe's own suite:
it runs both against a stand-in nodejs.org tree, release schedule and
releasers' keys under `file://` (no network, no Node fetched), every case holds
a mutation (accept gpg's exit status without its `GOODSIG`, skip the keyring's
comparison, take a tarball's digest unchecked against the signed list, raise to
the next even major, keep no npm) that fails it, and like the git suite it runs
itself a second time under a hostile `CDPATH` environment. Nothing holds the
workflow file itself: on any diff that touches it, read its `permissions:`, each
leg's issue title and the key step of the git and floor Node legs.

## Releasing

Releases are cut by pushing a `v*` tag; GitHub Actions builds every target
with [goreleaser](https://goreleaser.com), signs the checksums and the
container image with keyless Sigstore, publishes the image to GHCR and creates
the GitHub Release. **The tag is the version**: nothing in the tree records
one. `make snapshot` rehearses the pipeline locally, without a tag or GitHub;
the full process is in [RELEASING.md](RELEASING.md).

`main` has a signed image too, `ghcr.io/crewlet/crewlet:main`, rebuilt on
every push to `main`, the only tag it pushes
([RELEASING.md](RELEASING.md#images-from-main)). Its binary reports
`main-<the first 12 hex digits of the commit>`. A newer push cancels the run
before it, so only the newest of close pushes is built, and a commit that starts
no push run is never built: Dependabot's bumps, merged with `GITHUB_TOKEN`,
start none, so `main` stays on the last pushed commit's image until the next
push. That image is **not a release**: no Release, no notes, nothing owed below;
a deployment that runs `main` has opted out of the compatibility a tag promises.

### Nothing is released until a tag ships it

The tag is the only compatibility boundary, the one thing an operator can pin,
pull and run. A surface no `v*` tag has shipped has nobody behind it, and **you
may break it outright**: rename the config field, reshape the struct, drop the
CLI flag, move the route. Leave no deprecated alias, both-spellings fallback,
adapter or vestigial flag; code with no caller cannot be told from code whose
caller nobody found.

Check rather than assume: `git tag --merged` lists the tags, and
`git log <newest tag>..HEAD -- <path>` says whether what you are changing is
inside one. As of the only release, `v0.1.0`, the answer is *nothing*: the tag
sits on the initial commit, which contains no Go, so no package, config field,
CLI command, event type, API route or schema file has been in a release.

No compatibility is kept with an older build no tag shipped: none is upgraded in
place, none joins this fleet or shares its broker, and nothing it wrote (rows,
records, blobs, bucket keys, coordination documents, event payloads) is read,
migrated, swept or tolerated. Code that exists only for such a build (a reader
for a version this tree never writes, a fallback for a field it always sets, a
gate waiting for an older build's era to end, a sweep of a bucket only an older
build created) is deleted. The first tag that carries Go code is the first build
anything is owed to: from it on, the next release reads what that tag wrote and
runs beside it in one fleet; a build between two tags is owed nothing (see
[RELEASING.md](RELEASING.md)).

Two things a missing tag still does **not** excuse, because neither is about an
older build:

- **An applied migration is history, not source.** `schema_migrations` is
  keyed on the filename, so an edited file that already ran never re-runs.
  Reshape with a new numbered migration under `internal/store/schema/`. A
  **replicated** migration also carries three rules, checked by
  `internal/store.TestNoReplicatedMigrationDeclaresAConstraintTheApplyCannotSurvive`:
  no foreign key, no `UNIQUE` outside a primary key, and no `COLLATE`. Each
  aborts an apply transaction deterministically on every data node, stalling the
  fleet's log; uniqueness a writer must see whole is arbitrated on the record's
  subject. The node estate is not governed by them.
- **This build must be a good older peer to the next.** A rolling upgrade puts
  this build and its successor on one stream and one coordination store, so the
  forward mechanisms stay:
  - the event envelope evolves additive-only, and a type this build does not
    know round-trips losslessly;
  - a state-log record this build cannot read is deferred and kept, never
    dropped, except a gate record above the version it reads or one whose
    envelope it cannot decode, which stops the applier. So every data node names
    the version it reads on its position heartbeat, a writer of a gate above the
    base version waits on that census before publishing one, and an envelope
    never refuses what a newer build may add to it;
  - a parked run's unknown keys ride `PendingRun.Extra`;
  - a run's standing this build cannot read (an older build wrote the record
    without its headers) is judged on its record, read whole, never skipped;
  - a data node refuses an estate request whose arguments or actor hold a key
    its build does not declare, rather than run it without the key (the router
    leaves every input field at its zero off the wire, so only a request that
    sets a newer field is refused); a read carrying a newer argument is refused
    the same way, and an answer a caller carries into another request is read
    whole or refused;
  - an apply refuses only a revision that cannot run;
  - a bump of the seat-host lease protocol makes the newer node refuse to claim
    beside a live lower lease;
  - a run's capture stamp holding keys this build cannot ask its record again
    with is settled off the turn's row on the executor log, never asked short
    nor refused unasked.

  That contract runs FORWARD, to the builds after this one, and never obliges
  this tree to read what an earlier unreleased build wrote.

A free break is still complete: the rename lands everywhere in the same change
(`docs/`, `examples/`, the dashboard, the tests), and `schema/` is regenerated,
never hand-edited.

## Documentation

The pages under `docs/` are the source of truth, published to
[docs.crewlet.ai](https://docs.crewlet.ai/engine/latest/) under the engine's own
path and a version: `/engine/latest/` is the newest release (until the first
release, a redirect to `main`'s pages), `/engine/next/` is `main`, and
`/engine/vX.Y/` an older release line, with `schema/` and `skills/` published
beside each version's pages. Link `/engine/latest/`: a page the newest release
lacks redirects to its `/engine/next/` copy. Nothing forwards a path the site no
longer builds. Keep pages plain CommonMark: relative `.md` links between pages,
figures in `docs/assets/`, one `# Heading` per page. The site derives its
navigation from `docs/index.md`, so **a new page must be linked there**; the
site build fails on an unlinked page.

Links are checked:

- `internal/docsgate` fails the suite on a relative link to a missing file, on an
  `#anchor` naming no heading of its target (by GitHub's slug: rename a heading
  and every anchor to it goes red), and on a page under `docs/` that
  `docs/index.md` does not list.
- `dashboard/src/app/links.test.ts` holds the dashboard's own addresses
  (`#/worklet/ENG-42` in a guide, a tool skill or a string the engine composes)
  against the dashboard's route resolver, with every pointer at a
  `dashboard-design.md` heading.
- `internal/docslinks` reads every text file for an absolute
  `https://docs.crewlet.ai` link, failing on one the site does not build (a
  root-shaped path, or a page, schema or skill under `/engine/latest/` or
  `/engine/next/` this tree lacks) and on a page without its trailing slash,
  which the site redirects.

Fix a link to a moved page by pointing it at where the subject lives now, never
by deleting the gate's view of it.

A page says each thing once, linking to where a subject is explained, and an
enumeration is a table or a list. A repeat a test reads stays: grep `_test.go`
for the page's path before cutting one. Shortening a page keeps every heading's
text, because code, `examples/`, absolute docs.crewlet.ai links and
`crewlet-operator` link to its anchors and `internal/docsgate` checks only
relative markdown links: grep the tree for a heading and its slug before
changing it. `docs/reference/metrics.md` and `alarms.md` are generated (`make
metrics-doc`, `make alarms-doc`): edit their source, never the page.

`docs/` is written for people *running* Crewlet. Reasoning aimed at people
*changing* it, including why the obvious alternative is wrong and what it cost
when tried, goes in a package doc, where `go doc` surfaces it beside the code. A
comment that only restates the code has not earned its line. None of it binds:
if a rationale no longer matches the engine, change the code and the comment in
the same commit.

### Architecture decision records

A decision that binds **more than one package** goes in `adr/`; one inside a
package stays in that package's doc comment and gets no record. That single
criterion is the whole filing rule. A record never restates its authority; it
names it.

Each record carries an **Authority** (the one package or page holding the
detail), an **Enforced-by** (a named test, a compile error, or the literal
`nothing`), and the numbers it was settled on. The authority's own doc names the
record back, and `internal/adr` checks that anchor both ways inside `make
check`: a record whose authority does not cite it fails, and so does a doc
citing a record that does not exist.

`Enforced-by: nothing` is allowed and often honest. It goes in `adr.Unenforced`
with a sentence saying what a gate would have to look at, checked two-sided, so
an entry that stops being true also fails. `adr/README.md` records why each
template field exists.

## Changes `crewlet-operator` has to follow

[`crewlet-operator`](https://github.com/crewlet/crewlet-operator) is the
Kubernetes operator that runs Crewlet companies from `crewlet.ai` custom
resources. It consumes this engine's contract rather than sharing code: it
renders both config tiers from its objects, calls the engine's API, copies a few
run-time decisions out of the engine's source and runs the engine's image in
pods. Its suites run this repository's `main` as `ghcr.io/crewlet/crewlet:main`,
never a pin (its ADR-0002), so a change here it has not followed fails its next
run. That red run is the safety net, not the plan: a change that needs support
there ships with it.

A change needs a companion there when it touches something `crewlet-operator`
renders, calls, copies, runs or links:

- **A document it renders.** A field, enum value, default, rule or refusal of
  the company document (Tier B, the company types in `internal/config`) or of
  `crewlet.yaml` (Tier A, `Bootstrap`, which it writes for each pod: `node`,
  `stream`, `store`, `coordination`, `api` with `api.auth`, and `secrets` among
  its blocks), and so of `schema/`. It hands every render to `crewlet validate`
  and to a running engine's `/config`, and holds its CRD rules to the engine's
  own refusals.
- **A credential position.** A field tagged `secret:"true"` (`x-crewlet-secret`
  in the schema). Each takes a reference field on one of its kinds, or a stated
  reason it has none; a test holds the two lists to each other both ways, so a
  credential added here fails its build until mapped.
- **A route or an answer it calls.** `/health`, `/ready`, `/fleet`, `/agents`,
  `/config`, `/secrets` and the pause tools of `/operator/mcp`: the path, method,
  status, error envelope, a field it reads (`/ready`'s `reason`; `/health`'s
  `status`, `posture` and `applied_epoch`; `/fleet`'s nodes and seat leases), the
  paths a validation error names, or who may call it.
- **A table or a function it copies a decision from.** Its
  `internal/render/mirror_test.go` fingerprints engine functions by file and
  name (the executor's launch, the address checks the boot refuses to start on
  and the Atlassian host rule among them), and its CRD suite reads the harness
  profiles (`harnessprofile`: each harness's token and key variables and the
  variables that keep it inside a seat's home) from the engine's own refusals. A
  change, rename or move fails it until read against the files that copy it.
- **How the engine runs in a pod.** `crewlet run -config`, the `CREWLET_*`
  variables a pod sets, the ports, `/health` and `/ready` as the probes, the
  drain and its grace (the engine bounds no drain itself), and where the engine
  writes: it runs as a non-root user with a read-only root filesystem and only a
  data volume, `/tmp` and the user's home writable, so a new place the engine
  writes is a write the pod refuses.
- **The engine image and the tag.** The `Dockerfile`, `.goreleaser.yaml`,
  `make image-context` and `.github/workflows/main-image.yml`: the image's
  entrypoint, user ID and home directory, platforms, tags and labels (it reads
  the commit from `org.opencontainers.image.revision`) and the workflow's own
  path and name, which its release verifies the keyless signature against. A
  `v*` tag is one too: its ADR-0002 rests on this engine having released no tag
  that carries Go code, and each of its releases keeps a copy of the engine it
  was tested against.
- **A page or a heading it links.** It links this repository's `docs/` as
  `https://github.com/crewlet/crewlet/blob/main/docs/<page>.md#<heading>`, and
  its docs gate fails the day a change here removes or renames one.

When it does:

- **Look in its tree, not in your memory.** It is the authority on what it
  consumes. Grep it for the field, route, enum value, variable, path or heading
  you are changing, and for the engine file you are editing (its mirror table
  names files by path). No hit is the answer.
- **Make the change there; a note is not the change.** Open a pull request on
  `crewlet-operator` that names yours, and name it in yours, working to its own
  `CONTRIBUTING.md`: its `make check`, `git commit -s`, its commit scopes, its
  docs (`docs/reference/api.md` there is generated). If you cannot, say exactly
  what it needs (the surface, the files) in your pull request.
- **Prove it against your change, not against `main`.** Its suites run the
  engine `main` publishes, which lacks your change until it has merged and the
  image has been rebuilt. Its `CONTRIBUTING.md` ("The engine suite") and
  ADR-0002 say how to run them against another engine: `CREWLET_VERSION` for a
  commit, `CREWLET_IMG` for an image built from a checkout of this repository
  (`make image-context`). Say what you could not run.
- **Land the pair back to back, this one first.** Its tests read `main`, so its
  pull request is green only once this change is on `main` and the image
  rebuilt; a change that breaks `crewlet-operator` leaves every run of it that
  reads `main` red from this merge until its own lands.
- **Follow `main` plainly.** `crewlet-operator` tests against `main` alone and
  its API is `v1alpha1`, which breaks without ceremony, so the companion change
  is a rename or a reshape: no alias, no both-spellings reader, no
  engine-version switch (see [Nothing is released until a tag ships
  it](#nothing-is-released-until-a-tag-ships-it)), unless it has shipped a tag
  that carries the surface; then its own `CONTRIBUTING.md` says what a break
  takes.
- **Say what you did, or why nothing was needed.** The pull request description
  links the companion change, or says in one line why there is none
  (`crewlet-operator: none — internal to the turn loop`). Most changes need no
  companion; one that touches a surface above and says nothing is what this
  section exists to catch.

## Commit messages

Commit subjects follow the [Conventional Commits](https://www.conventionalcommits.org/)
shape: a type, an optional component scope, then the summary:

```
type(scope): summary

optional body explaining why, wrapped at 72 columns

Fixes #123
```

### Type

Required, and drawn from this set:

| Type | Use for |
|---|---|
| `feat` | a new capability: a config field, CLI command, tool, endpoint, provider |
| `fix` | a bug fix, including correcting a tuning value that was wrong |
| `docs` | pages under `docs/`, the root markdown files, docstrings |
| `refactor` | a change that neither fixes a bug nor adds a capability |
| `perf` | a change made for speed, memory, or token cost |
| `test` | tests only |
| `ci` | `.github/workflows/*`, `.github/dependabot.yml` |
| `build` | `go.mod`, the `Dockerfile`, `.goreleaser.yaml`, the Compose stack, and any dependency bump, whichever file records it |
| `chore` | anything else with no user-visible effect (tidying, repository maintenance) |
| `revert` | reverting an earlier commit |

### Scope

The scope names the **component** changed. For code that is the package
directory under `internal/` (`agent`, `api`, `authz`, `coord`, `engine`,
`gitlab`, `iam`, `knowledge`, `mattermost`, `mcp`, `notify`, `queue`, `sandbox`,
`schedule`, `seat`, `secrets`, `store`, `tools`, and so on). Two scopes use the
name people use: `dashboard` (its source in `dashboard/` and the built bundle in
`static/dashboard/`) and `cli` (`cmd/crewlet/`). Outside `internal/`, scope by
area: `docs`, `deps`, `examples`, `schema`, `scripts`, and for CI the workflow's
own name (`ci(release)`, `ci(docs-publish)`).

Drop the scope only when a change spans the whole repository
(`chore: normalise line endings`). The type is never optional.

### Summary

- Imperative present tense: "add", not "added" or "adds".
- Lowercase first word (unless it is an identifier), no trailing period.
- 72 characters or fewer, prefix included.
- Say what changed for someone using the code, not which file you edited.

Put the *why* in the body. Reference issues in a footer (`Fixes #123`). A
breaking change gets a `!` before the colon (`feat(config)!: …`) and a
`BREAKING CHANGE:` footer describing the migration. Both mark a change an
operator has to act on, so they belong to a surface a release shipped; changing
one no tag has carried takes neither (see [Nothing is released until a tag ships
it](#nothing-is-released-until-a-tag-ships-it)).

### One change per commit

Keep separate pieces of work in separate commits, each with its own scope. A bug
you fixed in passing because it affected your change, or a tuning value you
corrected on the way, gets a commit of its own, reviewable and revertable alone. A bug that does not affect your
change is not in the pull request at all: it gets an issue (see [A bug you find
along the way](#a-bug-you-find-along-the-way)).

### Sign your work

**Every commit needs a `Signed-off-by` trailer.** `-s` writes it for you:

```bash
git commit -s -m "fix(queue): nak a redelivery the handler already deferred"
```

which adds a line naming the `user.name` and `user.email` git is configured
with:

```
Signed-off-by: Jane Developer <jane@example.com>
```

That line is not decoration and it is not an authorship credit. It is your
certification of the [Developer Certificate of Origin](DCO) — the file at the
root of this repository — over that commit: that you wrote the change, or that
it came from work you have the right to submit under the MIT License, and that
you understand the commit and your sign-off are published permanently. Read it
once; it is 34 lines.

You contribute under the [MIT License](https://spdx.org/licenses/MIT.html), and
Crewlet ships your contribution as part of Crewlet under the
[Elastic License 2.0](LICENSE).

The DCO covers relayed work (clauses (b) and (c)), so a commit may carry
several sign-offs and the trailer need not name the author: a forwarded patch
keeps the original signer's line and gains yours. Every commit needs *a*
well-formed trailer, not one matching the author.

**This one is enforced**, unlike the subject conventions above.
`scripts/check-signoff.sh` judges the commits a branch adds relative to its
merge base with the base branch, in `make check` and in CI's `sign-off` job
over the pull request. A commit without a trailer fails the build.

Neither reaches the commit the merge button *writes*: GitHub composes it after
every check has reported and does not always carry the branch's trailers
(`074e3a1` and `7d0e29e` on `main` came this way from properly signed
branches). The `sign-off (main)` job that reported this no longer runs, so
nothing catches it; the trailers are kept by the repository's squash and merge
commit-message settings, which live in Settings.

Forgot one? The repair rewrites commits: free before a push, a force-push
after.

```bash
git commit --amend -s --no-edit          # just the last one
git rebase --signoff origin/main         # every commit on the branch
```

In a fork whose `main` is stale, rebase onto the merge base the gate prints,
not a ref: `git rebase --signoff
origin/main` against a stale `main` restamps commits you never wrote under
your sign-off.

Merge commits are exempt: `git merge` and GitHub's merge button author one
unsignable, and it carries no change to certify.

### Examples

```
feat(sandbox): reuse a paused box when a clarification is answered
fix(confluence): relax the CQL query when the server returns no hits
perf(providers): bench a spent plan rather than relaunch the harness each round
docs(secret-store): document that the keyring is required
build: stamp the version into the binary at link time
ci(docs-publish): rebuild the docs site when a release ships
build(deps): bump golangci-lint-action to v9
```

Nothing enforces *this*: no commit-message hook, and CI does not check
subjects, types or scopes. History is permanent and public, so the convention
holds only because contributors apply it. The sign-off trailer is the
exception.

**This applies to pull request titles too.** A pull request lands on `main` as
a squashed commit whose subject is its title, or as a merge commit over the
branch's commits (both shapes exist). Under a squash the title *is* the
subject; under a merge the release notes are generated from it. Either way it
takes the `type(scope): summary` form, and everything after the colon is what
readers see in the release notes. See below.

## Pull requests

1. Fork and create a feature branch.
2. Make your change, including tests and doc updates and, where it touches
   what `crewlet-operator` consumes, [its companion
   change](#changes-crewlet-operator-has-to-follow).
3. Commit it with `git commit -s` — see [Sign your work](#sign-your-work).
4. Run `make check`.
5. Open a PR describing what changed and why. **The title matters most**:
   GitHub generates each release's notes from the titles of the pull requests
   merged since the previous tag, and they are the *only* record of what a
   release contains. People decide whether to upgrade from them, so the
   summary after the `type(scope):` prefix reads as a release note (what
   changed for someone running Crewlet), not a commit log. Labels group it:
   Dependabot's `dependencies` label sorts bumps last (`.github/release.yml`).

**A pull request's CI run finishes every job**, even after one fails, so it
shows every failure at once and one push can fix them all. **Re-run failed
jobs** re-runs only the failed ones. A new push to the pull request cancels
its previous run.

Small, focused PRs are easier to review. For a substantial change (a new
subsystem, a reshaped config format) open an issue first to discuss the
approach.

### Land a large change as a series of pull requests

Split only when one pull request would be hard to review and the change has
seams where `main` can take part alone (a refactor the feature needs, a new
package before the code that wires it in, a migration before its readers). A
small change, or one with no seam, stays one pull request. When you split,
plan the pieces before writing code. A piece need not work alone, but `main`
must be able to take it as it is: finished code with its own tests and docs,
every gate green, no existing behaviour broken, nothing half-wired for a user
to trip over. Code nothing calls until a later piece, or behind config nothing
sets yet, is fine. A stub or a `TODO` is not.

- **Track the series in one issue.** It describes the feature and lists the
  pieces as a checklist. Each pull request says `Part of #N`, the last says
  `Closes #N`. Add each pull request's link to the checklist as it opens.
- **Land one piece, then start the next on `main`.** Each pull request is
  green in CI and titled as a release note on its own.
- **If a piece waits on review, build the next one on it.** Branch from the
  previous piece's branch, but open the pull request against `main` and name
  the lower one in its description (`Builds on #N`). Its diff includes the
  lower piece until that merges; then rebase onto `main`. Never open a pull
  request against another pull request's branch: GitHub treats the two as a
  stack, which its merge API refuses to merge and whose base can no longer be
  changed.
- **The series is the change.** It is done when the last piece merges and the
  issue closes.

## Reporting bugs and requesting features

Use [GitHub issues](https://github.com/crewlet/crewlet/issues). For bugs,
include the engine version (`crewlet --version`), your config shape (redact
secrets!), and relevant structured-log output.

### A bug you find along the way

Whether a bug you notice is fixed in your pull request turns on one question:
does it affect your change?

- **It affects your change: fix it there.** Your change depends on the broken
  code, the bug is in the code or text you are changing, your change would
  make it worse or newly reachable, it is another copy of the defect you are
  fixing, or it keeps a test or gate your change must pass from passing. Fix
  it properly, in a commit of its own (see [One change per
  commit](#one-change-per-commit)), and say so in the pull request
  description. That it was broken before you came does not make it unrelated.
- **It is unrelated: open an issue.** Keep it out of your pull request. Search
  the open issues first and add to one that already describes it; otherwise
  open one per bug with the bug report template, saying what is wrong and how
  it shows, where it is (a permalink, or file and line at a commit, since a
  line on a branch moves) and the fix you would make. Link it from your pull
  request description.

If you cannot tell which, it affects your change: fix it. A wrong tuning value
(a timeout, a retry count, a batch size, a default) goes the same way: fixed
in your change when it affects it, otherwise reported with the value you would
set and why. Anything you believe is exploitable is never a public issue,
commit message or pull request description, related or not: report it
privately (see [Security issues](#security-issues)).

### Issues an engine filed

An engine whose company opted in reports the defects it detects in itself
([Engine Issue Reports](docs/guides/engine-issue-reports.md)), and a relay
files each as an issue labelled **`auto-report`** and `bug`. Triage with
`label:auto-report is:open`. Each holds typed fields only, and a hidden
`crewlet-fingerprint` marker the relay finds it by, so a duplicate adds
nothing. Close one when its defect is fixed: the relay refiles it as a
possible regression only for a build committed after the close.

Add a class to the table in `internal/issuereport`, with `make schema`; the
relay vendors the export (`schema/issue-report-catalogue.json`) before an
engine sends it, since a relay refuses a class it does not know. A defect
anyone could exploit never goes in the catalogue: see
[Security issues](#security-issues).

A `v*` tag freezes the shape of every class it publishes
(`schema/issue-report-history/`, [RELEASING.md](RELEASING.md)), because the
release that sends it keeps running after the code moves on. After that a
class's shape changes only as a new class id or a new `SchemaVersion`. A
catalogue site is a stable token: renaming its function edits the suite's
`liveSites` table, never the catalogue.

## Security issues

Please do **not** open public issues for security vulnerabilities — see
[SECURITY.md](SECURITY.md).

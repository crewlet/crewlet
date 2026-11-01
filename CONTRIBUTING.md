# Contributing to Crewlet

Thanks for your interest in contributing! This document covers everything you
need to get a development environment running and land a change.

## Development setup

Prerequisites: **Go 1.27+** and
**[golangci-lint](https://golangci-lint.run/welcome/install/)**, which
`make check` runs and CI's own job downloads for itself. **What building git
takes** — a C compiler, `make`, `curl`, `xz`, a SHA-256 tool to check each
tarball against its pin (coreutils' `sha256sum`, which Debian and Ubuntu ship,
or `shasum`, which macOS ships; `make git` and `make git-test` both need
one), and libcurl's and zlib's development files (Debian and Ubuntu:
`build-essential curl xz-utils libcurl4-openssl-dev zlib1g-dev`, or
`libcurl4-gnutls-dev` in place of `libcurl4-openssl-dev`, the flavour the
image builds against — the recipe compiles nothing against OpenSSL itself, so
libcurl's TLS is the only one it needs and OpenSSL's headers are not; macOS:
the Xcode command line tools, which carry libcurl and zlib, and Homebrew's
`xz`, which macOS does not ship) — because **the suite never runs the
machine's own git** — and **GnuPG**
(Debian and Ubuntu: `gnupg`; macOS: Homebrew's `gnupg`), which `make check`'s
`git-test` needs to make and check the signing keys of the pin check's own
suite (see [The git pin is checked every
week](#the-git-pin-is-checked-every-week)). The code host
runs real git as a subprocess, and its suites drive real git both as the engine
and as a client, so what a case asserts is that git's behaviour as much as the
engine's: `make git` builds the gits
[`scripts/git-pin.env`](scripts/git-pin.env) pins — the node pin, the newest
release, which the engine image builds too, and the newest patches of the two
floors' series — from their checksummed release tarballs with
[`scripts/git.sh`](scripts/git.sh), the one recipe the image and CI share,
once, into `~/.cache/crewlet/git` (`CREWLET_GIT_CACHE` moves it; about four
minutes on four vCPUs, and a pin bump or a change to the recipe builds beside
what is there — and leaves the older builds where they are, about 70 MB a set,
because another checkout still on them may be running one: `rm -rf
~/.cache/crewlet/git` clears them, and the next `make git` builds what the tree
pins). Every target that runs Go tests depends on it and puts the node
pin first on `PATH`, and a one-package loop does the same by hand, as two
statements:
`bin=$(scripts/git.sh node) && PATH="$bin:$PATH" go test ./internal/code/cache/`
— never `PATH="$(scripts/git.sh node):$PATH" go test …`, whose substitution
is empty when the recipe fails (a missing header, a refused download), which
leaves `PATH` starting with an empty entry and runs the loop on whatever git
the machine has, with only the recipe's own line above the test output to
say so. A loop
run on the machine's own git instead FAILS the git-driven tests where that git
is below the floor it stands for, rather than skipping, because git is a
prerequisite of the suite, not something a machine may lack: every pool a test
builds needs the node floor, 2.55 (`gitproc.NodeMinVersion`, held by
`internal/code/gitproc/gittest.RequireNode`), and the client git a test drives
the client floor, 2.40 (`gitproc.ClientMinVersion`, `gittest.Require`). On
macOS, Apple's bundled git is 2.39, below both, and Homebrew's is new enough for
both — but the documented loop is the pin, the git the image ships. What the
engine reads of a failed git is the FORMAT of each report git makes to its
trace2 stream, checked against a table of git's own formats that is held,
version by version, to what real git reports over real damaged repositories —
on BOTH ends of what a pool runs, the node pin and the node floor's own build,
each a subtest (`node pin`, `node floor`) of every gitproc case whose subject is
what git says or does in a pool — so a pin bump re-measures it on the new git
by itself, in `make check`, and the floor is measured on every run rather than
by hand. A one-package loop names the floor's build the way the make targets
do, or those cases' floor arms skip:
`bin=$(scripts/git.sh node) && floor=$(scripts/git.sh node-floor) && PATH="$bin:$PATH" CREWLET_TEST_GIT_NODE_FLOOR="$floor/git" go test -count=1 ./internal/code/gitproc/`
— and a row whose range does not hold names the format and the git that
disagreed.

**The floors' arm.** Every make target that runs Go tests also names the two
floors' own builds — `CREWLET_TEST_GIT_NODE_FLOOR` and
`CREWLET_TEST_GIT_CLIENT_FLOOR`, each `$(scripts/git.sh <pin>)/git` for
`node-floor` and `client-floor` — and sets `CREWLET_REQUIRE_GIT_FLOOR=1` (the
Makefile's `GIT_ENV`), so `internal/code/host`'s
`TestTheGitFloorsPassTheSuite` runs the code host's whole suite with every
pool on the node floor's 2.55 and every person's git on the client floor's
2.40 — the worst pairing the floors admit — and FAILS, naming the variable and
`make git`, where either build is missing — as do gitproc's node-floor arms
above. A bare `go test ./internal/code/host/` or `./internal/code/gitproc/`
without them skips those arms, saying so, and runs everything else; export the
three variables to run them by hand. A git named
there must be an absolute path to a build of its floor's own series (2.55.x,
2.40.x): a newer git in its place is a floor nobody is testing, and is refused
naming the variable. ci.yml's `git recipe (macOS)` job runs `make
test-gitproc` — gitproc's own suite on the three pins — on macOS for every pull
request, so the recipe's macOS branch is built where a Mac contributor builds
it; on Linux `make test` runs the same packages.

**A current Node** (npm ships with it) for the dashboard — its build output is
committed, so building and running the ENGINE needs neither, but changing the
dashboard or running `internal/e2e`'s client replay does. Nothing in the tree
pins a node version; CI installs the latest — see *Which node* under
[A skip is not a pass](#a-skip-is-not-a-pass). **Docker** only for the vendor
loops below; the engine itself needs no services.

Take golangci-lint as a **prebuilt release**, not via `go install ...@latest`:
that builds it with the linter module's own minimum Go rather than the newest,
and a golangci-lint built with a Go whose language version (major.minor) is
older than that of `go.mod`'s `go` line refuses this module outright instead of
linting it — so the gate reports nothing at all rather than disagreeing. Only
the language version counts: a linter built with go1.27.0 lints a module at
`go 1.27.2`. `make lint` checks this and says so.

```bash
git clone https://github.com/crewlet/crewlet.git
cd crewlet
make build        # or: go build ./...
```

`make help` lists every target, and that is the whole setup — nothing to
install beyond the prerequisites above. The engine embeds its own event
stream (a NATS JetStream server) and its store is a local file it creates, so
`crewlet run` in an empty directory is a working company — there is no broker
to operate and nothing to point a DSN at.

The Docker stack is for the **self-hostable integrations**: Mattermost and
GitLab, each behind its own compose profile with a bootstrap script that
stands it up and provisions a config's seats — `make mattermost-up` and
`make gitlab-up` are the profile and its script in one step, and take
`COMPANY=` to provision that config's seats in the same run.
`make mattermost-up COMPANY=examples/nimbus.company.yaml` is the whole setup
for the bundled example, whose only integration is chat; the GitLab profile
needs a config that declares `integrations.gitlab`, which that one does not. Jira and Confluence
have no profile — Atlassian is not something a compose file can stand up. See
[docs/integrations/](docs/integrations/).

## Running tests and lint

```bash
make check               # every gate CI runs on a pull request
```

That is one target because it is one question — *would CI accept this?* —
and answering it in pieces is how a push goes red on the piece you skipped.
`make help` lists the rest.

Every target runs the command [`ci.yml`](.github/workflows/ci.yml) runs, with
the same flags — `make bench` excepted, which has no CI job (see
[Benchmarks](#benchmarks)). The two test jobs call `make` directly, so for
those there is one command rather than a copy. For every other job nothing
asserts the two have not drifted — a test did, and it was dropped — so a
convenience target that quietly loses `-race` would report a pass CI does not
honour and nothing else would notice. Change a target and its `ci.yml` step
together, and read both. `make check` is:

```bash
files="$(go run ./internal/sourcetree/gofiles)" && gofmt -l $files  # formatting — prints the files that need it
go mod tidy -diff        # go.mod / go.sum are already what tidy would write
scripts/check-signoff.sh # every commit the branch adds is signed off
scripts/check-signoff_test.sh  # ... and that gate's own suite
scripts/git-pin_test.sh        # the git recipe's and the pin check's own suite (git-test)
go vet ./...
golangci-lint run        # what CI's lint job runs
go build ./...
make git         # the pinned gits, built once a machine (both test targets need it)
make test        # the suite, minus the packages that run alone, on the node pin
make test-solo   # ... and those, at -p 1, with a runner to themselves
# then, for each of CROSS_TARGETS (linux and darwin x amd64/arm64):
CGO_ENABLED=0 GOOS=$OS GOARCH=$ARCH go build ./...            # test-cross
# and the dashboard, whose build output is committed:
cd dashboard && npm run format:check                          # dashboard-lint
cd dashboard && npm run build                                 # dashboard-check:
git diff --exit-code -- static/dashboard                      #   no tracked file moved
test -z "$(git ls-files --others -- static/dashboard)"        #   and none is untracked
cd dashboard && npm run typecheck && npm test                 # dashboard-test
```

The formatting line captures the list BEFORE handing it to gofmt, and that
is the form to copy into a hook or a script. The inline
`gofmt -l $(go run ./internal/sourcetree/gofiles)` passes when the listing
fails: the shell discards a command substitution's exit status, and gofmt
handed no paths formats its standard input, finds nothing wrong and exits 0.
Run from anywhere but the module root, the listing fails every time.

Run the dashboard's suites through `npm test` (or `npm test -- <file>` for
one file, `npm run test:watch` for the watch loop), never a bare
`npx vitest`. Vitest's forks pool writes every transformed module into a
fresh directory under the system temp directory and never removes it — 16 to
48 MB a run, which over a day of gate runs filled the volume the engine's
embedded broker sizes its storage from. Both scripts run Vitest through
`dashboard/scripts/owned-tmpdir.mjs`, which points `TMPDIR` at a directory it
creates and removes; `src/test/ownedTmpdir.test.ts` fails a run that was
started without it.

A dashboard case also fails on what it writes to `console.error` or
`console.warn` (`dashboard/src/test/console.ts`, judged after every case by
`src/test/setup.ts`). React reports a broken contract there and carries on —
a list child with no key, two children under one key, a paragraph inside a
paragraph, an `act` nobody awaited — and the dashboard reports there an
announcement no live region was mounted to say; Vitest's default reporter
prints nothing a passing case wrote, so dozens of such lines once sat behind
a green run. A harness that stands in for the application mounts what the
application mounts around a screen (its live region, `AppAnnouncer`); a case
that provokes a line on purpose, as a boundary's case does when React
reports the error it caught, declares it with `expectConsole(level,
pattern)`, and a declaration nothing meets fails the case too.

The race detector is not optional here: the engine's concurrency model is
real parallelism, and every "atomic because it is single-threaded" assumption
is a data race until proven otherwise — so every package runs under it, in
CI and in `make check`. `-count=1` is the other half: without it a cached
PASS recorded before the change answers for the change. `make test-norace`
skips the detector when you want the faster loop, and says so.

The test targets run `go test -vet=off`, and that is not a gap: the vet
`go test` runs by default is twelve analyzers, all of which `go vet ./...`
runs too, over the same packages and the same `_test.go` files. That full vet
is `make vet` — a prerequisite of `make check` — and a step of CI's
`build + vet` job, and it is the only vet there is now. So a bare `make test`
no longer vets: run `make vet` beside it, or `make check`.

Some packages stand up N engines, each embedding its own NATS server, in ONE
process, and every stream or bucket they create is a round trip a quorum of
those servers has to answer. Sharing a runner with everything else —
`go test ./...` runs package binaries in parallel, and the rest of the suite
holds a four-vCPU runner almost fully busy for its whole run — those round
trips went unanswered for the whole thirty seconds a create was given then,
and the cluster cases failed every cluster-start attempt with `context
deadline exceeded`, while alone on a runner the same cases passed. Today's
budgets are longer, and the shared case has not been re-measured under them;
`go doc ./internal/solo` has what has been measured since, and what it does
and does not show.

So those packages run in `make test-solo` and in CI's `end-to-end gates`
job, and `make test` leaves them out. A contention split, not a coverage one
— `make check` depends on both targets. `make test-solo` runs its packages
one binary at a time (`-p 1`) and compiles them first at full parallelism,
since `-p 1` would otherwise compile the whole dependency tree one package at
a time too.

**In CI each half is one job**, `test (race)` and `end-to-end gates`, each on
a runner of its own. `end-to-end gates` is plain `make test-solo`; `test (race)`
is `make test` with two variables set, and a local run can do the same:

```bash
make test TEST_TIMINGS=/tmp/test-timings.tsv   # write how long each package took
make test TEST_WEIGHTS=/tmp/test-timings.tsv   # start the longest first by that
```

`TEST_WEIGHTS` starts the longest packages first: `go test` starts package runs
in the order it is handed them, and alphabetical order started the three
longest last. It must name a file that exists — `internal/solo/partition`
refuses one it cannot open rather than guess — and an empty file is `go list`'s
order, which is what CI runs before `main` has measured anything. CI's own
weights are the timings of the job's last passing run on `main`, kept in the
Actions cache, which nothing outside a workflow can download; a local run
takes them from a run of its own, as above. Neither variable changes which
packages are in a half, and left unset — the default — `make test` is the
whole half in `go list` order, as it always was. `make test-solo` takes
neither: it runs at `-p 1`, where its binaries take the same time in any
order.

Both paths live outside the checkout, as they do in CI: the suite's own gates
read the checkout as this repository's tree, and neither path is ignored by
git, so a file left there is one `git add -A` away from a commit.

A half is not split across more runners: GitHub Actions has no native way to
split one job, and the comment above `ci.yml`'s `test` job says what doing it
by hand cost and bought.

**Which packages those are is computed, not listed.** A package declares it by
importing `internal/solo` from its `TestMain`, and `internal/solo`'s roster
guard fails the build both ways: a package that stands up a multi-member broker
without declaring, and a package that declares without needing to. Read
`go doc ./internal/solo` before reaching for a build tag — it records what each
alternative measured, and every one of them fails silently. This was a
hand-maintained `grep -v /internal/e2e` in three places, and it named one
package while three others stood up clusters in the shared runner.

`make dashboard-check` gates the same shape of problem one level out. The
dashboard's build output is committed — `go build ./...` and
`go install …@latest` must work on a clean checkout with no Node, and an embed
directive cannot run a bundler — so a bundle that has drifted from its source
compiles, embeds, serves and passes every Go test while running code nobody
wrote. Rebuilding and judging the tree is the only thing that can tell you.

Judging it takes TWO questions, because `git diff` sees only the paths the
index already has. Every emitted name is content-hashed, so a changed chunk is
a new path, and `git commit -a` stages modified and deleted files but never a
new one: a commit made that way carries the `index.html` that imports the new
chunk and not the chunk, the diff is clean, and the binary serves a shell
asking for a module it does not embed. So the gate also fails on any file under
`static/dashboard` the index does not hold (`git ls-files --others`), and it
prints each drifted path — `??` for one git does not track, `!!` for one
`.gitignore` hides, which `git add -A` will not stage and the `all:dashboard`
embed ships anyway. It judges against the INDEX rather than `HEAD`, so the
repair it prints turns it green before you commit:

```bash
make dashboard && git add -A -- static/dashboard
```

**And when two branches have both rebuilt it, REBUILD — never merge.** The
emitted names are content-hashed, so each side writes a different path for the
same chunk; git reads that as a rename/rename and three-way merges a megabyte
of minified JavaScript. Left to itself it splices conflict markers into both
bundles *and* into `index.html`, and the result does not parse — so staging the
conflict instead of resolving it commits a dashboard that fails to load.
`.gitattributes` marks the tree `-merge` so git leaves your side alone rather
than corrupting it (the conflict still happens; only the damage stops), and
`static/merge_test.go` holds that. The resolution is always the same two
commands, whether you are merging or rebasing:

```bash
make dashboard && git add -A -- static/dashboard
```

`go mod tidy -diff` gates the half of tidiness nothing else notices. An
*under*-tidy module already fails loudly — a missing requirement or `go.sum`
entry stops `go build ./...` in every job — but the opposite direction is
silent: a `require` left behind when its last import was deleted, a stale
`go.sum` line or a wrong `// indirect` marker each build, test and
cross-compile green, then land as unrelated churn in whichever pull request
next runs `make tidy`, on top of whatever that one was for. `-diff` prints the
patch and exits non-zero *without* writing the files, so neither CI nor
`make check` rewrites the tree it is judging — run `make tidy` to apply it.

One thing `make check` does not cover, because it needs a service CI starts
for itself — it prints it when it passes, rather than letting a green run
imply more than it did: the [release pipeline](#releasing) (`make snapshot`).

The queue conformance suite is not a second gap. Both backends behind the
`EventQueue` contract — the in-memory twin and JetStream — run the single
suite in `internal/queue/queuetest` under a plain `go test`, because the
JetStream backend brings its own broker: `internal/queue/jetstream` starts an
embedded NATS server per queue under test and shuts it down with the test.
Per queue rather than per binary, deliberately — the suite asserts things like
"a subscription nobody created retains nothing", which a broker shared with
another subtest's streams could satisfy by accident. So there is no service to
start, no environment variable to set and no compose profile to remember:
`make check` certifies the backend the engine actually ships, on every run.

### A skip is not a pass

Some suites need something the machine may not have and **skip silently
without it** — a green run has simply not exercised them.

**This is enforced now, and it was not.** Both test targets pipe
`go test -json` through `internal/skipgate`, which renders the stream back to
ordinary output and then fails the run on a skip nothing declared. Every
allowed skip is an entry in `internal/skipgate/allowed.go` carrying a reason
and a `When`: `Always` for a structural one — a backend that cannot do the
thing, a driver capability that has not landed — which is ALSO reported when it
stops firing, because a structural skip that stopped means the world changed;
`Environment` for one that depends on what the machine has, which is only
checked in the undeclared direction. Stopping is judged by the test, never by
its package: an `Always` entry is stale when its test reported a result that
was not a skip, or when a passing run of every test of its package never ran
it (it was renamed or deleted). A run narrowed with `-run` or `-skip`, and a
package that failed, are judged only by the tests that reported a result —
not by a test that started and never finished because its binary died under
it — so `go test -run X` on a package that declares entries passes when
everything it ran passed.

Adding an entry is the decision, so it lands in a diff somebody reviews. Before
you add one, check the case is covered somewhere: the two defects this gate was
written after were both cases that ran NOWHERE, and each looked like an
ordinary capability skip from inside the one run that saw it.

Why this had to exist: `go test` prints nothing about a skipped subtest without
`-v`, and the suite job has never passed it, so every skip this repository has
taken was absent from every CI log. Under that, an API gate that read a
dashboard path the React rewrite deleted certified nothing for the whole of
that rewrite while reporting a pass — and a queue conformance case skipped on
*both* backends, so it ran on neither. The rule was a checkbox in the pull
request template; it is a build failure now.

What follows are the prerequisites that legitimately vary by machine.

- **`node`** runs `internal/e2e`'s client replay: the Go suite drives a real
  company, captures every frame its WebSocket pushed, and replays those exact
  bytes through the dashboard's own protocol module under plain `node`. That
  gate asks "does the client understand what the server sent" rather than "did
  the server send something", and it is the only place both halves of the wire
  protocol are checked against each other. Without node it SKIPS, so the
  `make` targets that run it refuse to start without one, CI installs it, and
  the skip has no entry in `internal/skipgate/allowed.go` — a run that got past
  `require-node` fails on it rather than reporting a pass.

  It needs no npm and no build: `static/dashboard/protocol.js` is committed
  along with the rest of the built tree.

  **Which dashboard the replay tests is a parameter.** The replay is
  `internal/e2e/replay.mjs`, beside the `golden_test.go` that runs it, and it
  reads the tree from `CREWLET_DASHBOARD_ROOT`, which the Go half always sets
  to `static/dashboard`, the tree the binary embeds. Unset, it is that same
  tree, and a relative value is read against the repository — the nearest
  directory above the script holding a `go.mod` — rather than the directory it
  is run from.

- **`npm`** builds and tests the dashboard itself. Its assertions — the
  wire protocol, the router's history rules, the ordering comparators, the
  purity of `src/contract/`, and the MEASURED contrast of every colour token
  over every surface it can land on, in both themes and for protan and deutan
  vision — run under Vitest: `make dashboard-test`. None of that is checkable
  by looking at the stylesheet, which is why it is computed from the file that
  actually ships.

  **A set the engine owns is declared in `dashboard/src/contract/`**, one
  module per concern, and held against the engine by one Go test that reads
  it through `internal/clientsource`. Adding one means three things in one
  change: the declaration in a contract module, a row in
  `internal/clientsource/contract.go` naming its reader and its gate, and the
  gate itself. The Go side holds that directory both ways — every row is
  declared there, every name it exports is a row — and
  `contract/contract.test.ts` holds what a module there may be: data and
  shapes, importing only its siblings, every export read outside it.
  `protocol/` imports it by a relative path, because `protocol.js` is built
  without the `~` alias.

  **Nothing on a screen is money.** The dashboard draws spend in tokens and
  never in a currency (rule 19 in `docs/reference/dashboard-design.md`), and
  the client declares no price field for a screen to reach for. Three gates
  hold it: `src/money.test.tsx` parses every shipped module and renders the
  screens that draw spend over wire fixtures carrying a price, and
  `TestTheDashboardRendersNoPrice` in `internal/api` scans every module the
  engine serves from the committed bundle.

  **The dashboard reaches the engine through `src/protocol/` and nowhere
  else**, and three suites hold it from the parsed source:
  `protocol/transport.test.ts` refuses a module outside that directory that
  reads `fetch` or any other network primitive itself; `app/source.test.ts`
  requires the query kind a `useQuery(` or `query(` call asks — bare or as a
  method — to be a string literal, because the engine's registry gates read
  the kinds there and cannot read a variable; and `protocol/proxy.test.ts`
  holds the dev server's proxy table in `vite.config.ts` to exactly the paths
  the source reaches, calls and markup alike, both ways. A new REST route the
  dashboard calls is a new proxy entry in the same change, and a route it
  stops calling takes its entry with it.

  **A dashboard suite waits for the work, never for the clock.** `findBy*`
  and `waitFor` poll the page against a one-second deadline, so a case that
  waited that way for a screen to finish passed on a quiet machine and failed
  on a loaded one — four full runs sharing four cores each failed two to four
  of the org builder's cases, none of which had done anything wrong. A
  screen that takes its time as an argument is handed time the suite moves:
  the builder's testkit mounts the lens on a `SuiteClock`, and `settle()`
  waits for every answer the lens has out and every timer due within the
  check's debounce. One that does not is flushed with `act` where its work is
  promises, or driven by fake timers faked only for the calls that arm and
  cancel one. And a role query is scoped with `within` to the toolbar, dialog
  or chart it is about: asked by name, it computes the accessible name of
  every candidate in its container, and in jsdom each element of each name
  costs a computed style matched against the whole user-agent stylesheet.
  Not every suite holds to this: the org builder's suites, and the screens
  that ran out of time under load (the audit, the retention gesture, a seat's
  settings, the work list's Display menu, the sign-in form), wait for the
  work, while more than forty others still poll with `findBy*` and
  `waitFor`. Those are not a pattern to copy — a suite you write, or a case
  you change, waits for the work.

  **What a case does ends with the case.** A case that times out is failed,
  not stopped: its function goes on running beside the cases after it. React
  keeps one `act` scope count for the process, so an `act` it opens beside the
  next case's leaves every later render in the file queued and never flushed —
  one timeout read as the rest of the file failing — a `findBy` or `waitFor`
  it still has out polls the next case's page, where it can find what that
  case drew, and a `render` or `cleanup` it makes draws into that page or
  takes it away. So every case runs in an async context of its own
  (`aroundEach` in `src/test/setup.ts`, over `src/test/cases.ts`, which loads
  no testing library so a file with no document pays for none), and
  `src/test/inCase.ts` binds the testing library to it. A suite imports the
  library from there and nowhere else — `inCase.source.test.ts` fails a file
  that takes it, or React's own `act`, from anywhere else — so the import that
  gives a suite the library is the one that binds it: its `act`, `render`,
  `renderHook` and `cleanup`, refused once their case has ended; its `poll`,
  which a suite uses in place of Vitest's `vi.waitFor` — that one cannot be
  bound, goes on looking after its case has ended, and advances whatever fake
  clock is installed before every look, which by then is the next case's, so
  the same gate refuses it and `vi.waitUntil` and `expect.poll` with it; and
  the library's own waits and `fireEvent`, through the library's `asyncWrapper`
  and `eventWrapper` — the wait's wrapper restated rather than wrapped, because
  after a wait finds what it looked for the library lets one turn of the page's
  timer pass before it puts the act environment back, and on a fake clock
  nothing moves that timer, so a wrapper round the library's could not end a
  wait held there, or the scopes outside it. When the case ends, every act
  scope, wait and poll it still has out is closed before the next case begins —
  innermost first, because each puts back on exit the act count or act
  environment it found on entry, and closed in any other order a scope opened
  inside another left React's count raised — and everything it asks for after
  that is refused before it opens. What the binding does not reach is what a
  late case writes to the page's globals itself — the fake clock through `vi`,
  `location.hash`, a stub — in a `catch` or `finally` its refusal passes
  through, or past an await of something that is none of the above, such as a
  promise of its own: every act, wait and poll rejects at its case's end, which
  throws a late case out at the first of them it awaits. Node carries the
  context through the case's own awaits and timers, so a case that timed out
  inside a promise of its own and resumes later still reads its own, ended case
  — which is what a helper every case shares (`answered()`, a suite's
  `settle()`) could not tell before, and why the flush used to be handed to
  each case. A case that holds the page's timers advances them as `answered`'s
  `step`, so the refusal comes before the timers move.
  `src/test/inCase.test.tsx` holds each of these to a pair: a case that really
  runs out of its time with something out (`test.fails` on a budget it always
  spends), and the case after it, which first holds it to having failed by that
  timeout — `test.fails` passes on any failure at all — and then reads what the
  late one came to.

  **And the harness's own waits end with it too.** The builder's testkit
  retires a lens when the case that mounted it finishes (`onTestFinished`):
  it wakes every wait on the lens's engine, which holds answers back until
  the case releases them — a wait the binding could close but not wake. A
  suite that waits for the page without a lens — a move landing, an element
  the page draws — waits through `waitInCase` (`viewTestkit.tsx`), which
  listens for the event with no deadline and stops listening the same way.
  And a case waits only on the lens it mounted: `mountBuilder` hands the
  case its lens's `settle`, `checked` and `navigate`, because those used to
  be found in a variable the last mount wrote, and a late `settle()` settled
  the NEXT case's lens — a value, which no context makes right.
  `testkit.test.tsx` holds each of those waits to a pair.

  **A warning fails the case that printed it.** `src/test/setup.ts` watches
  `console.error` and `console.warn` around every case and fails one that
  wrote to either, after the suite's own teardown so what an unmount says
  counts too — and fails the FILE for what it wrote outside every case: while
  it loaded, in a `beforeAll` or an `afterAll`, or between cases, which no
  case can be blamed for and a per-case watch never heard. Vitest shows nothing a passing case prints, so every warning the
  suite used to print was a defect nobody saw: a list keyed on a field its
  fixture lacked, a stand-in observer reporting mid-commit, an announcement
  with no live region mounted — the application had none — and the audit
  asking the engine once a second. A case that expects a warning silences that
  one itself, `vi.spyOn(console, "error").mockImplementation(...)` for the call
  it is about, and puts it back; the watcher (`src/test/console.ts`) restores
  both methods when the case ends whatever the case did.

  **A stand-in for the browser answers when the browser would.** jsdom has no
  layout, so the chart suites install a ResizeObserver of their own, and one
  that reports from inside `observe()` reports in the middle of React's
  commit, because `observe()` is called from a ref. A real observer delivers
  in the event loop's rendering step with React idle, which is what the design
  system's resize handler is written for: it flushes its sizes synchronously,
  and inside a commit that flush is a `flushSync` inside a lifecycle — eight
  warnings about the harness that read as a defect in the chart. Both of the
  builder's observers, `LayoutObserver` (`viewTestkit.tsx`) and the reference
  chart's (`src/testing.tsx`), record what they observe and report it inside
  `act` once the render has committed.

  **The built dashboard is committed**, so building the ENGINE needs neither
  node nor npm. Changing the dashboard does: run `make dashboard` and commit
  `static/dashboard` with your source change — `git add -A`, so the new
  content-hashed chunks go in with it. CI rebuilds it and fails on any
  difference, an untracked file included (`make dashboard-check`), because a
  bundle that has drifted from its source
  compiles, embeds, serves and passes every Go test while running code nobody
  wrote — the same failure mode `go mod tidy -diff` and the generated
  `schema/` are gated against. A Dependabot bump is the one change that does
  not make you do it; see [A dashboard bump brings its own
  bundle](#a-dashboard-bump-brings-its-own-bundle).

  **Which node.** CI installs `latest` — the newest Node release, resolved by
  setup-node when the job runs — in the `dashboard` job, the `gates` job and
  [`dependabot-dashboard.yml`](.github/workflows/dependabot-dashboard.yml)'s
  `build` job. The last must always answer this the same way as `dashboard`,
  because it rebuilds the bundle that job diffs. `gates` need not match either:
  what it runs under plain node is the committed bundle, not a fresh build.

  **Nothing in this repository names a node version**, and that is a decision
  rather than an oversight. A pin earns its keep by being maintained, and
  nothing here would maintain one: Dependabot moves the versions in
  `dependencies`, `devDependencies` and a `uses:` ref, and it bumps neither
  `engines.node`
  ([dependabot-core#11243](https://github.com/dependabot/dependabot-core/issues/11243),
  open) nor a workflow's `with:`. A number nobody moves does not hold a
  version steady — it just goes quietly out of date while looking deliberate.

  The cost is worth knowing before you hit it: a new Node major reaches CI
  with no commit behind it, so the `dashboard` job can go red on a day nobody
  changed anything. If that happens, the fix is `lts/*` in `ci.yml`'s
  `dashboard` job and in `dependabot-dashboard.yml`'s `build` job, in the same
  commit — the current LTS, which lags the newest major and is the same
  zero-maintenance trade — not a hand-maintained number.

  **Do not add `engines` or `devEngines` to `dashboard/package.json`**, however
  reasonable it looks. Those fields also apply inside **Dependabot's** sandbox,
  which runs its own older node: `devEngines.runtime` refuses outright
  (`onFail` defaults to `error`) and `engines` does too once an `.npmrc` sets
  `engine-strict`. A bump that cannot install is a bump that never opens —
  silently, leaving the npm entry in `.github/dependabot.yml` looking exactly
  like a surface with nothing to update. That is
  [#13722](https://github.com/dependabot/dependabot-core/issues/13722) for
  `devEngines` (open; the proposed fix has to *strip* the field before corepack
  sees it) and
  [#12535](https://github.com/dependabot/dependabot-core/issues/12535) /
  [#13741](https://github.com/dependabot/dependabot-core/issues/13741) for
  `engine-strict`. The dashboard's dependencies are bytes the engine binary
  embeds; keeping them watched is worth more than a version check.

- **`TURSO_GO_CACHE_DIR`** is where the store driver's native library lives,
  and it is the one environment variable a store test can be defeated by.

  Turso is the only store driver, so there is no dialect intersection to
  keep statements inside and no second suite run. Write `internal/store`
  against Turso.

  It is pure Go to *build* (no cgo), but its engine is a ~20 MB binary
  embedded in the driver and extracted at runtime into `$TURSO_GO_CACHE_DIR`
  (default `~/.cache/turso-go`), shared by every process on the machine.
  Upstream writes it without a rename and panics on what a concurrent reader
  sees, so `internal/store/turso.go` prepares it under a lock and heals a
  cache entry that will not verify. If a test binary ever dies with `unable to
  load turso library`, that cache is the place to look — `go test
  ./internal/store/ -run LibraryCache -count=1` covers it (the cache is not a
  test input, so without `-count=1` Go will happily serve the PASS from before
  it went bad).

  It also decides what this project can be built for. Upstream embeds the
  library for linux and darwin on amd64 and arm64 and for windows/amd64;
  Crewlet ships the first four, and `internal/store/platform.go` turns a build
  for anything else into a compile error that says why rather than a binary
  that fails at its first query.

  **The linux binary is not static, and must not be made static.** purego
  declares its `dlopen` imports with `//go:cgo_import_dynamic`, so
  `CGO_ENABLED=0 go build` still emits a dynamic executable — `interpreter
  /lib64/ld-linux-x86-64.so.2`, `NEEDED libc.so.6`. It needs glibc and does not
  run on musl, and `-tags musl` does NOT change that: the tag picks which
  shared object the driver embeds, not this binary's own linkage. That is why
  there is no musl archive.

  Note what that means: `CGO_ENABLED=0` does not give a static binary here,
  even though it does for any other Go program (a hello-world on the same
  machine comes out static). purego's `dlfcn_nocgo_linux.go` is
  `//go:build !cgo` — the file that applies precisely when cgo is OFF — and it
  is the one declaring the dynamic imports.

  Adding `-extldflags -static` to a cgo-free build changes nothing either: the
  flag is for the external linker and a cgo-free build links internally.
  Forcing a static ELF needs `-linkmode external`, which needs cgo — and that
  binary, which `file(1)` does call static, SIGSEGVs on its first query on the
  machine that built it, because a static program cannot `dlopen`.
  The `cross` CI job asserts the artifact is still dynamic, so this is a red
  build rather than a release.

  ```bash
  # What CI cross-compiles on every pull request.
  make test-cross
  ```

Tests never call real LLM APIs — use the fakes in `internal/providers`. Test
files sit beside what they cover, as Go expects, and a test file that is not Go
is no exception: the dashboard's Vitest suites sit beside their modules in
`dashboard/src`, and `internal/e2e`'s client replay is
`internal/e2e/replay.mjs`, beside the `golden_test.go` that runs it. There is
no top-level `tests/` tree.

A test that opens a store on a fresh path opens it through
`storetest.OpenNode` or `storetest.OpenEstate`, and one that boots an engine on
a path of its own seeds it first with `storetest.Seed` — the node estate, and
the replicated one where the node holds it, never a scratch store. Each copies
an image the test binary's own migrations built once, so a broken migration
still fails at the first fixture and no case pays the whole migration sequence
for nothing it asserts. Only a test whose subject is migration — the schema
cases, `Pending`, an adoption, a fresh-file boot — opens a fresh file with
`store.OpenNode`.

### The end-to-end gates

`internal/e2e` runs a real engine, a real broker and the real API, then
replays the frames its socket produced through the dashboard's own
`protocol.js`. Both halves of the wire protocol are checked against each other
there and nowhere else, so it needs node too.

It is also where a FLEET signs a person in. On a clustered fleet, a Tier A
token invites somebody onto a human seat, they redeem the link and sign in on
one member, and the same cookie must then read, write as them and open the
live socket on the other member — never `401` while it catches up — while a handshake from a
foreign origin is refused and a sign-out everywhere on the second member is
refused on the first. The sign-in surface, the identity directory and the
human write surface it serves are built by `api.NewHumanSurfaces`, the
constructor `crewlet run` calls, so a seam added to them reaches the suite with
no copy to update:

```bash
make test-solo   # internal/e2e, and every other package that runs alone
```

A case there is written to three rules, which `internal/e2e`'s package doc
states with their reasons:

- **A case that stands up one node — or a stateless node beside one data
  node — runs in parallel.** Each has its own directories, its own scripted
  model and its own broker, so the one thing such cases would share is the
  process environment, and a node is *handed* its environment instead
  (`nodeSpec.env`, which reaches the engine as `engine.Options.Environment`).
  `t.Setenv` cannot run beside `t.Parallel`, and a case that read the
  runner's environment held only on runners that happened to agree with it.
- **A case that stands up a fleet runs alone, before the parallel ones, and
  the fleet's claims share one fleet**: each is a subtest of
  `TestAFleetOfThree`, in the order `fleetClaims` gives, rather than a test
  paying a boot and a teardown of its own. A new fleet claim is a row there.
  Only a claim that breaks its fleet on purpose — the partition case — keeps
  a fleet to itself.
- **An absence is waited on a recorded decision, never on a sleep.** A case
  asserting that something did not happen waits for the record that says it
  was decided — a delivery's `notification_skipped`, held against the reason
  `notify.SkipReason` declares; a later delivery's record, behind it in the
  order the inbound edge keeps; a Mattermost node's answer to a ping sent
  behind a post — and each of those orders is held by a case of its own. An
  absence read after a fixed sleep holds only for as long as the sleep
  happened to be long enough.

### Benchmarks

```bash
make bench
```

runs the benchmarks the design's capacity figures come from, so each figure is
measured rather than derived: the object store on every backend
(`objstoretest.Bench` — the broker's bucket at three replicas, and an S3 fake
a provider's round trip away, among them — and the SHA-256 rate under every
verified byte), one arbitrated write end to end and the applier's drain
(`internal/statelog`), the store's own drain and a node's day of usage
derived from its event log (`internal/store`), what a
stream's leader takes to list the subjects it holds, a page at a time
(`internal/jsapi`), the company's token counter charged by many nodes at once, each a leaf of its own,
and a gated lease claim (`internal/coord/kv`), a git
process through the pool and the index-pack a push runs
(`internal/code/gitproc`), the git cache's cold and delta syncs and a
bundle's own file work, its index maintenance and what a request pays across
many packs, a node's boot of its cache and what a directory sync costs a file
(`internal/code/cache`), a home git node's compaction of a repository's packs
— its memory, the pushes a minute's poll keeps up with, the requests and bytes
a fleet's git nodes pay for it, and a poll over thousands of repositories
(`internal/code/compact`), the code domain's applier, decide, reconcile and
snapshot reads and what one push's history costs every data node's disk
(`internal/code`), the code host's commit chain on one and three members
(`internal/code/codecluster`), a person's push, a warm fetch, and cold and
warm clones of every shape end to end through the code host
(`internal/code/host`), a seat's reads of a repository by object id on a git
node, warm and cold (`internal/code/query`), a page of who read a repository
over the whole history its records keep and the count of the readers they
left out (`internal/usage`), the pace a client's connection
is held to, on a real connection (`internal/api/paced`), and what every push
pays to start its `proc-receive` hook, which is this binary (`cmd/crewlet`).

It is **not a gate, and no CI job runs it**. A benchmark on a shared runner
measures the runner's neighbours, and a gate on figures that noisy is one
somebody turns off. To compare two trees, run `make bench BENCH_COUNT=6` on
each, on one idle machine, and read the two outputs with
[benchstat](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat).

| Variable | Default | What it sets |
|---|---|---|
| `BENCH_PKGS` | `internal/objstore`, its three backends, `internal/objstore/natsobj/replicated`, `internal/statelog`, `internal/store`, `internal/jsapi`, `internal/coord/kv`, `internal/code/gitproc`, `internal/code/cache`, `internal/code/compact`, `internal/code`, `internal/code/codecluster`, `internal/code/host`, `internal/code/query`, `internal/usage`, `internal/api/paced`, `cmd/crewlet` | which packages are measured |
| `BENCH_PATTERN` | `.` | `-bench`, e.g. `Backend/via=member` |
| `BENCH_TIME` | `3s` | `-benchtime` |
| `BENCH_COUNT` | `1` | `-count` |
| `BENCH_TIMEOUT` | `8h`, and `24h` with `CREWLET_BENCH_LARGE=1` | `-timeout`, a hang detector, each package's: at six counts of every arm a default run measures, `internal/code/cache` takes about three and a quarter hours on four vCPUs, and with the large arms about thirteen — estimates for its published layers' arms, from one count of each; `internal/code/host` about two and a half hours with the large arms |

The packages run one at a time (`-p 1`) and without the race detector, and the
output is verbose (`-v`), because a skipped benchmark prints nothing otherwise
— the real-bucket arms of `internal/objstore/s3obj`, `internal/code/cache`,
`internal/code/host` and `internal/code/query` skip unless
`CREWLET_BENCH_S3_BUCKET` names a bucket
(`internal/objstore/s3obj`'s doc lists the variables that do), and the arms
whose fixture takes minutes and gigabytes — in `internal/code/cache` cold
syncs of 2 048 and of 10 000 pushes, repositories of 10 000 packs, a 5 GB
index and its published layers, a gigabyte split into hundreds of packs, an
installer catching up 10 000 repositories, and `BenchmarkBigNode`'s 5 GB
monorepo; in `internal/code/host` a gigabyte history, a hundred thousand refs
and a 100 000-file tree; in `internal/code/query` a gigabyte history under a
100 000-file tree read by object id, and a gigabyte history blamed and logged
by path; in `internal/jsapi` streams of a million and of three million
subjects — skip unless `CREWLET_BENCH_LARGE=1`. `BenchmarkBigNode`'s 12 GB
monorepo, whose fixture builds for hours and whose bitmap holds tens of
gigabytes beside the in-memory store, skips unless `CREWLET_BENCH_BIGNODE=1`
is set beside it, on a machine of at least 64 GB of memory; run it alone,
under the day's `BENCH_TIMEOUT` that `CREWLET_BENCH_LARGE=1` gives a run.
`internal/code/gitproc`'s `BenchmarkTransferShare` runs its coding-box arms
(`mix=runs`) on a transfer lane as wide as the machine's cores unless
`CREWLET_BENCH_SHARE_SLOTS` names a width, in slots, of at least two: the
search for the coding boxes' share weight is to be run on a lane of a
production node's width, four slots a core, which a lane of two cannot stand
in for ([fair share](docs/concepts/code-host.md#fair-share)).

A code host figure is the pool's git's as much as the engine's, so `make bench`
runs the node pin — the git the engine image builds — first on `PATH`, as every
test target does: every pool and every client a benchmark starts runs it, and
never whichever git the machine has (`internal/code/codecluster`'s commit chain
runs no git: its pushes carry no pack, or one it names and never builds). A
figure the docs quote names that version, and a pin bump leaves the figures
taken on the git before it as what they are — measured on the version they
name — until a change to that path measures again.

**A figure the design argues from is recorded in the package doc of what it
measures**, with the machine it was measured on — its CPUs and model, and
whether it has SHA instructions, since every verified byte is hashed — the
toolchain, the git version where the figure runs git (it is git's figure as
much as the engine's), and the versions of the dependencies it ran through. A
new object store backend runs `objstoretest.Bench` from a `BenchmarkBackend`
of its own, as it runs `objstoretest.Run` from `TestContract`; its factory
hands each case an empty store and takes away what the case put there — a
backend the run starts goes with it, and one that outlives the run (a real
bucket) gives each case a prefix of its own and `objstoretest.Clear`s it,
since the suite's populations run to a hundred thousand objects and it
deletes nothing itself.

## Project conventions

- **Go 1.27+**, and the module pins its own toolchain — `GOTOOLCHAIN=auto`
  fetches it rather than failing on a mismatch.
- **Interfaces are defined by the consumer**, in the package that calls them,
  and kept to what that caller needs.
- **`context.Context` first, always**, and threaded rather than stored. The
  exception is a rollback, which takes `context.WithoutCancel`: the failure
  it is undoing is often the cancellation itself, and a cleanup that inherits
  a dead context does nothing at all.
- **Structured logging** via `internal/logging` (`log := logging.Get("component")`);
  never the bare `slog` default. Event names are snake_case, dynamic data
  goes in key/value pairs, and a message is a name rather than a sentence.
- **Errors say what to do about it.** An error a person will read names the
  field, the file or the variable they have to change — not just what failed.
- **Comments explain WHY**, and especially why an obvious alternative is
  wrong. The diff shows what the code does.
- **Every piece of state answers "who has to agree on it?"** — *this node
  alone* is the node's own database file; *every node, identically, derived
  from an ordered log* is the replicated estate, written only by a state log's
  applier; *the whole company, now* is the coordination store. There is a
  FOURTH, and it is narrow: *whichever node holds this seat, and only the
  current value* is a compacted changelog — see
  [`adr/0014`](adr/0014-a-compacted-changelog-is-the-fourth-answer.md), which
  exists because a seat's memory was answered with the first and was forgotten
  every time placement moved a seat. What is not an answer is "it has always
  been in the store": nine tables took that default and cost five repair
  migrations. The estate gates in `internal/store` will not let a new table
  skip the question — see
  [`adr/0003`](adr/0003-fleet-agreement-state-lives-in-coordination.md) and
  [`adr/0002`](adr/0002-the-stream-is-the-write-ahead-log.md).
- **Docs are part of the change** — any change to public APIs, config
  formats, CLI commands, or behavior must update the relevant page under
  `docs/`.
- **Diagrams are Mermaid** — architecture, flow, sequence, and state diagrams
  go in ` ```mermaid ` fences, not ASCII box art. GitHub renders them inline,
  and the docs site renders them in the Crewlet design system. Directory
  trees and file listings stay plain code fences — they are literal output,
  not diagrams.

## A build that compiles is not a backend that works

Go's compiler proves the types line up. It cannot prove a backend is wired,
and this codebase has two shapes where it is not:

- **A config value with no implementation.** `role.sandbox.run_in: e2b`
  parses, validates, appears in the generated schema — and the engine refuses
  it at construction, because the company configured no `e2b:` backend. That
  refusal is the right behaviour; what would be wrong is accepting it and
  failing later, inside a turn that has already spent its rounds.
- **A knob nothing reads.** `provisioning.group_webhook` was validated,
  documented and consulted by no code at all, so an operator setting it got
  the default and no error. A config field ships with the code that reads it,
  or it does not ship.

The test for both is the same: a field added to a config struct needs a test
that a value in it changes what the engine *does*, not merely that it parses.

## Dependency updates

Dependabot watches every dependency manifest this repository has, and opens a
pull request when any of them has a newer release. The git pin is no manifest
— a source tarball no Dependabot ecosystem reads — so a workflow of this
repository's own watches it instead (and the tool servers the docs and the
examples launch are not in a manifest either: a test holds them — see below):

| Surface | Watched by | What it covers |
|---|---|---|
| `.github/workflows/*.yml` | `github-actions` | the actions CI and releases run on |
| `go.mod` | `gomod` | the engine's own Go dependencies |
| `dashboard/package.json` | `npm` | the dashboard's build and runtime dependencies |
| `Dockerfile` | `docker` | the base image a release ships |
| `docker-compose.yml` | `docker-compose` | the images the local dev stack runs |
| `scripts/git-pin.env` | [`.github/workflows/git-pin.yml`](.github/workflows/git-pin.yml) (not Dependabot) | the gits the engine image builds and the suite runs |

Dependabot keeps a version moving once it is in the tree; **choosing one in
the first place goes the same direction — take the newest release, and pin it
exactly.** Establish what the newest is from the registry or index rather than
from memory, and write it as a literal version (`4.2.4`), never a floating tag:
`latest` and `@main` leave Dependabot nothing to bump, and a floating tag turns
a green run into a claim about a build nobody can name afterwards.
Holding a version back is still fine where there is a reason — put the reason in
a comment at the pin, as the Compose stack's Postgres image already does. The
one place a version is deliberately left unpinned is what a shipped example
fetches, which runs its newest release (see below).

The configuration is [`.github/dependabot.yml`](.github/dependabot.yml): one
entry per surface on a weekly schedule, plus the commit prefix that surface's
bumps carry, plus three grouping rules on the npm entry. CI runs on each pull
request, and — as below — CI is what decides whether it lands. Ten things are
worth knowing:

- **The React family is grouped.** `react`, `react-dom`, `@types/react` and
  `@types/react-dom` arrive in a single pull request, because **react-dom checks at runtime that `react` and `react-dom`
  have the exact same version** — not a compatible one. No version range can
  say that, so npm's resolver never enforces it and a permissive caret is no
  protection. React 19.3.0 arrived ungrouped as two pull requests and both were
  red, each in a different phase: the `react-dom` half never installed
  (`ERESOLVE` in about a second, on `@types/react-dom@19.3.0` peering
  `@types/react: ^19.3.0` against 19.2.18 — plus a second unsatisfied edge,
  `react-dom@19.3.0` peering `react: ^19.3.0` against 19.2.8, that `npm ci`
  did not get far enough to print), while the `react` half installed cleanly
  and then failed 19 of 36 test suites on the exact-version check. Neither
  ordering helped; each needed versions that were in the other pull request.
  `@testing-library/react` is deliberately outside the group — its react peers
  are `^18.0.0 || ^19.0.0`, which do ride a React minor — and
  `@vitejs/plugin-react` is outside it because it declares no react peer at
  all, only `vite: ^8.0.0`. Its name is the only React-shaped thing about it.
- **`vite`, `vitest` and `@vitejs/plugin-react` are grouped for the same
  reason, one major out.** `vite` is a *required* peer of the other two —
  `^8.0.0` from the plugin, `^6.4.0 || ^7.0.0 || ^8.0.0` from vitest — so a
  lone `vite` 9 bump breaks both ceilings at once and fails to resolve exactly
  as the react-dom half did. Minors were never the problem (8.2.2 → 8.3.0
  landed on its own), so the group buys nothing until the major and then it
  buys all of it. It costs something in the meantime, and the cost is real: a
  `vitest`-only bump now arrives titled for the group, with the packages it
  actually moved in the body rather than the subject.
- **The design system's three packages are grouped, because they pin each
  other.** `@crewlethq/ui` depends on `@crewlethq/tokens` and `@crewlethq/icons`
  at an *exact* version, and the three are released together. So a lone bump
  of one installs cleanly and wrongly: a newer `ui` beside the direct `tokens`
  nests a second copy of the tokens under `ui`, and the components paint from
  one palette while the dashboard's own stylesheets read the other — a split
  no install error reports and no test reading the installed package can see.
  The group's pattern is `@crewlethq/*`, because the pinning is the scope's
  convention rather than a fact about three names.
- **Nothing else is grouped, and that is the rule rather than today's state.**
  The only other *required* edges a bump there could split are
  `@testing-library/react`'s — on react and react-dom at `^18.0.0 || ^19.0.0`,
  which ride a minor, and on `@testing-library/dom`, a transitive it brings
  itself and Dependabot therefore moves with it. Every remaining vite peer
  (`@vitest/mocker`'s, vite's own on `@types/node` and the bundlers) is
  optional, and npm does not fail on an unsatisfied optional peer. A group
  raises whichever
  members have an update together, under a title naming the group rather than
  any package in it — so it costs a vaguer subject on every bump, and it puts
  one member's bad release in front of the rest. Pay that where a split bump is
  genuinely broken — not for neatness, and not to batch unrelated noise.
- **A dashboard bump that changes the emitted bytes needs a rebuilt bundle, and
  gets one.** `static/dashboard` is committed and the `dashboard` job rebuilds
  and diffs it, but Dependabot edits only `package.json` and the lockfile — it
  cannot run a bundler — so such a bump used to sit red on that gate until
  someone ran `make dashboard` and pushed the output onto its branch.
  [`dependabot-dashboard.yml`](.github/workflows/dependabot-dashboard.yml) is
  that someone: see [A dashboard bump brings its own
  bundle](#a-dashboard-bump-brings-its-own-bundle). Bumps that do not touch the
  output — `@types/*`, `prettier`, a build tool whose result is byte-identical —
  pass the gate untouched and get no commit.
- **A Compose image can be held back on purpose**, with the reason in a
  comment beside the pin — `mattermost-db` holds its Postgres major because an
  existing `mattermost-pgdata` volume will not open under a newer one without a
  `pg_upgrade` or a dump/restore. Closing that pull request is the right
  answer; Dependabot does not reopen one for a version you have already turned
  down.
- **`docker` and `docker-compose` are two ecosystems, not one.** The first
  reads `Dockerfile`s and the second reads Compose files; neither sees the
  other's manifests, so a repository with both needs both entries.
- **An action pinned to a non-version ref is invisible.** A branch pointer
  (`@release/v1`) or `@main` yields no update pull requests at all, so pin
  actions to a version tag or a full SHA. That matters most for the one
  handed an OIDC token: pinned, its updates arrive as reviewable pull
  requests instead of moving under the workflow unannounced. The release and
  main-image workflows pin every action by **full commit SHA**, with the
  release in a trailing comment (`@<sha> # v7.2.3`) that Dependabot rewrites
  with the pin: they hold the registry and the signing identity (and the
  release workflow the Releases), and a version tag is a ref its owner can move
  under them.
- **The git pin is watched by a workflow rather than Dependabot**, and a
  bump of it arrives as an issue rather than a pull request: every Monday
  [`git-pin.yml`](.github/workflows/git-pin.yml) asks kernel.org whether each
  pin is still the newest release of its series and keeps ONE issue, `git pin
  behind upstream`, naming what moved — see [The git pin is checked every
  week](#the-git-pin-is-checked-every-week).
- **goreleaser itself floats within its major.** Every goreleaser step in
  `.github/workflows/release.yml` asks for `version: "~> v2"`, the newest v2
  on each run, because Dependabot bumps the action's ref but never a `with:`
  input, so an exact version would age with nothing watching it. The major
  is held because `.goreleaser.yaml` is a `version: 2` config: moving to a
  v3 is a migration of that file, made by hand in the same change, and
  rehearsed with `make snapshot`.

Adding a new dependency surface — a second `package.json`, a second module — means
adding its `updates:` entry in the same change. Nothing reports the omission:
a manifest Dependabot has not been told about simply never produces a pull
request, which is indistinguishable from one that has nothing to update. The
Go module itself went the length of the rewrite that way.

The tool servers the docs, the examples and the skills launch are versions
Dependabot cannot see: they sit in a page's YAML or a setup step's command
line, not in a manifest. [`internal/config/toolservers_test.go`](internal/config/toolservers_test.go)
holds them instead. Every package a page or a skill fetches by name — an
`mcp_servers` entry, a `uvx`, `npx`, `npm exec` or `pip install` it tells
somebody to run — names an exact release; every server a document launches is
listed in `pinnedToolServers` with `readAt`, the release what the documents
say about it (its tool names, its variables) was read against; and every
document that names a release names that one. So moving one means reading the
new release first, correcting whatever moved, and setting `readAt` in the same
change — and since nothing proposes the move, check the registry whenever you
touch those pages. Reading it includes running the documented launch through
`initialize` and `tools/list`: a runner pins the package it is given and
resolves every dependency the package leaves unbounded at its newest, so a
release whose source reads right can still fail to start — `mcp-linear` 0.1.1
declares `mcp>=0.1.0` and dies at import on mcp 2.

The shipped examples (`examples/`) are held the other way round. An example is
a company somebody copies and runs long after it was written, and a pin in it
is a release nothing ever moves, so every package an example fetches names no
release (`@latest` at most) and runs the newest on the day it is copied. The
one exception is a server a bundled tool skill (`examples/tool-skills/`)
triggers on by tool name — `mcp-server-mattermost` today — whose entry names
`readAt` exactly, because a release that renamed the tool would disarm the
skill without a sound. The same test derives that exception from the skills'
own triggers, so a pin that no skill needs any more fails as surely as a skill
on a server that floats.

Security updates are separate: they come from published advisories rather than
this file, are enabled in the repository's settings, and are held back by
neither the weekly schedule nor Dependabot's default release cooldown.

**Promoting an indirect dependency to a direct one is a change to the module's
surface**, and `go mod tidy` is what records it — a `// indirect` marker that
should have gone, or one that should have stayed, fails `go mod tidy -diff` in
`make check`. `golang.org/x/crypto` is direct because `internal/iam/credential`
calls `argon2` from it: a password hash is the one piece of cryptography in
this tree that is not hand-written, because it is maintained by the same people
who maintain the standard library's crypto and there is no second
implementation of it to disagree with. Everything beside it in that package —
TOTP, the recovery codes, the token digests — is `crypto/hmac`, `crypto/sha1`,
`crypto/sha256` and `encoding/base32`, which is the dependency rule working
rather than an inconsistency: a module earns its place against what `std`
already does, and for forty lines of RFC 6238 arithmetic `std` does all of
it.

### A bump merges itself

[`.github/workflows/dependabot-merge.yml`](.github/workflows/dependabot-merge.yml)
approves a pull request Dependabot opened and queues it with `gh pr merge
--auto --squash`, so a bump lands without waiting on a maintainer. It does not
land without review: CI *is* the review a dependency change gets here, and
`--auto` holds the merge until every check `main`'s protection rule requires
has passed. A bump CI rejects sits there red until a person looks at it, which
is exactly where it would have sat anyway.

Two settings hold that property up and neither of them is in this repository's
files. `main`'s protection rule must **require** the `ci` checks — `--auto`
waits for the checks a rule names and for nothing else, so with no rule the
queue is empty and the bump merges the instant it is mergeable. And **Allow
auto-merge** must be on under Settings → General, or the step fails outright;
that failure is loud, a red check on the bump, which is the right way for it to
fail.

The checks to require are the jobs of `ci.yml` a pull request runs: `build +
vet`, `sign-off`, `dashboard`, `test (race)`, `end-to-end gates`,
`golangci-lint` and the four `cross-compile the release targets` legs. A rule
matches a check by its exact name, and a matrix leg's name carries its matrix
values — `cross-compile the release targets (linux, amd64)` — so each leg is
required by that full name; a check the rule names that never reports blocks
every pull request, and renaming a job is therefore a change to the rule too.

The job runs only when Dependabot is both the pull request's author *and* the
actor that triggered the run. That second condition is what stops the workflow
approving a commit a person pushed onto a Dependabot branch — anyone with write
access can push one. Its visible cost, under a rule that dismisses stale
approvals, is that clicking **Update branch** yourself can leave the pull
request unapproved, because the run your click triggered is skipped;
`@dependabot recreate` from a person with push access re-pushes as Dependabot
and recovers it. (Not `rebase`: the merge commit your click made is one
Dependabot did not write, and it will not rebase a branch that holds one.)

The approval is given only while GitHub still says one is needed. The job runs on
`opened` and on each push Dependabot makes, and under a rule that keeps approvals
across pushes (the one `main` has today) every run after the first would stack
another review for nothing. It reads `reviewDecision` and skips on `APPROVED` and
on `CHANGES_REQUESTED`, which this account's approval would not clear. Everything
else approves, an empty decision included: GitHub leaves it empty when nothing
requires a review, and has been reported to leave it empty while a ruleset's
requirement still blocks the merge (radius-project/radius#13111), so a skip on
doubt could stall every bump with nothing red to show for it.

Because nothing retitles a bump before it becomes a permanent subject line, each
entry in `.github/dependabot.yml` pins the prefix its commits carry, and every
surface carries the same one: `build(deps)`. A bump moves a pinned version, which
is a change to what the project builds against whichever file records it — so an
action bump reads `build(deps)` rather than `ci(deps)`, matching the bumps
already in `main`. Left unset, Dependabot infers the prefix from how recent
commits are written, and an inference that changes its mind writes a bare
`Bump x from 1 to 2` straight into `main` and into the release notes. Dependabot
capitalises the `Bump` and offers no way not to, so a bump is the one subject
here that does not start lowercase.

Nothing checks any of that for you. `internal/version` used to assert both
halves of the author/actor guard and the `--auto --squash` flags; those tests
were dropped, and no linter in this repository reads a workflow file. Every one
of them fails silently — the workflow keeps running, it just starts running on
the wrong pull requests (drop `--auto` and, while `main` requires the checks, the
merge step goes red instead, because the hold is the rule's; with no such rule it
would merge before a check has reported) — and the job holds `contents: write`
and `pull-requests: write`. Read the `if:`, the approval's condition and the
merge command on any diff that touches
[`.github/workflows/dependabot-merge.yml`](.github/workflows/dependabot-merge.yml).

### A dashboard bump brings its own bundle

[`.github/workflows/dependabot-dashboard.yml`](.github/workflows/dependabot-dashboard.yml)
runs when Dependabot opens or updates a pull request that touches `dashboard/`.
It does what the failing `dashboard` job tells a person to do — `make dashboard`
— and, if `static/dashboard` came out different, commits the result onto
Dependabot's branch as `build(dashboard): rebuild the bundle for this dependency
bump`, signed off like every other commit. ci.yml then runs on that new head and
the drift gate that failed on the first commit passes on the second. A bump that
leaves the output as it was pushes nothing.

It is two jobs because one of them runs somebody else's code. `npm ci` and the
bundler execute whatever a dependency published this week, so that job holds no
secret and a read-only token, and passes the next job nothing but a directory of
files. The second job holds the credential and runs only the runner's own tools
and two of GitHub's own actions. It checks the bundle's file *names* against an
allowlist, because `static/dashboard` sits inside the Go module and a stray
`zz_test.go` would be run by every `go test ./...`, replaces the directory and
pushes without `--force`, so a branch Dependabot has moved since the run began
is refused rather than overwritten. A new kind of file in the bundle has to be
added to that allowlist on purpose.

The push is made with a personal access token, not `GITHUB_TOKEN`: a push made
with the workflow's own token leaves the pull request's runs waiting for someone
to click *Approve workflows to run*, and the auto-merge queued behind those
checks waits with them. Two things outside this repository's files have to exist:

- **A fine-grained personal access token**, preferably of an account made for the
  purpose. Resource owner: the organisation. Repository access: this repository
  only. One permission: **Contents, read and write** — not Workflows (its absence
  is what stops this credential rewriting a workflow file), not Pull requests,
  and not a classic token, whose `repo` scope reaches every repository the
  account can. The account needs Write access, and an owner has to approve the
  token where the organisation asks for that. Contents: write can create a tag,
  so keep `v*` behind a tag ruleset the account is not on the bypass list of. A
  fine-grained token expires, a year at most; put the date in a calendar.
- **That token as a Dependabot secret**, `DASHBOARD_BUNDLE_TOKEN`, under Settings
  → Secrets and variables → **Dependabot**. Not an Actions secret: a run
  Dependabot starts cannot see the Actions ones, so a secret put on the wrong tab
  is empty there. The job fails naming the secret when it is.

The workflow decides nothing. [`dependabot-merge.yml`](.github/workflows/dependabot-merge.yml)
approved the commit Dependabot wrote, and `main`'s ruleset decides whether the
bundle commit on top of it lands. With *dismiss stale approvals* and *require
approval of the most recent push* both off — **as they are today** — a bump
merges itself once CI is green, and so does *anything* pushed onto an open
Dependabot branch: a bundle, but equally a commit from a person with write access
or from whoever holds this token. Turning on **Require approval of the most
recent reviewable push** puts a person in front of that, at the price that a bump
whose bundle was rebuilt then waits for one.

**When one bump merges the others conflict**, because each carries its own
rebuilt bundle and the emitted names are content-hashed. Dependabot rebases a
conflicted bump by itself — #167 conflicted when #168 merged and was rebased two
minutes later — but only while every commit on the branch is its own or carries
`[dependabot skip]`, GitHub's marker for "Dependabot may force-push over this
commit", which is why the bundle commit carries it. Each rebase starts from
Dependabot's commit alone: the bundle commit is gone, the workflow runs again and
pushes a fresh one. What Dependabot will not rebase is a branch holding a commit
without the marker, or a bump left open for 30 days; the next section is what
asks again.

Nothing checks any of this for you, and the workflow holds a credential that can
write to a branch. Read the `if:` on both jobs, each job's `permissions:` and the
token's scope on any diff that touches
[`.github/workflows/dependabot-dashboard.yml`](.github/workflows/dependabot-dashboard.yml).

### A conflicted bump is asked to recreate

[`.github/workflows/dependabot-recreate.yml`](.github/workflows/dependabot-recreate.yml)
runs on every push to `main`, and by hand from the Actions tab, where a **dry
run** box reports what it would comment without commenting.

Dependabot rebases a conflicted bump by itself while every commit on it is its
own or carries `[dependabot skip]`, as the bundle commit does, and while the bump
is under 30 days old (#172 went into conflict when #165 merged, and Dependabot
rebased it on its own). The workflow leaves those to it. A bump holding any other
commit Dependabot never rebases again, and it refuses `@dependabot rebase` there
("edited by someone other than Dependabot"), so the workflow comments
`@dependabot recreate`, which rebuilds the branch and **overwrites every commit on
it, a person's included**. If you have work on a Dependabot branch you mean to
keep, move it to a branch of your own. A bump over 30 days old that is
Dependabot's alone is named in the run's summary as needing a person. Drafts are
left alone, and so is a bump Dependabot says it is rebasing.

Each request carries the head commit it was made for in an HTML comment the pull
request page does not show, so a bump is asked once per head; after three
requests it is named as needing a person instead. If Dependabot answers a request
with "only users with push access can use that command", the run fails.

The comment has to come from a user account: Dependabot ignores commands from
`github-actions[bot]` and from GitHub Apps
([dependabot-core#9147](https://github.com/dependabot/dependabot-core/issues/9147)).
So it is made with `DEPENDABOT_RECREATE_TOKEN`, and deliberately not
`DASHBOARD_BUNDLE_TOKEN`: one token that could both push to a branch and approve
could land anything on its own. Two things outside this repository's files have
to exist:

- **A fine-grained personal access token**: resource owner the organisation, this
  repository only, **Pull requests: read and write** and nothing else, owned by an
  account with Write access. This scope also covers approving pull requests, and
  such an approval counts toward `main`'s required review, which is why the next
  item is not optional.
- **An environment**, `dependabot-recreate`, whose deployment branches are limited
  to the selected branch `main`, holding the token as its secret (Settings →
  Environments). An ordinary Actions secret can be read by any writer's branch;
  an environment secret only by a run on `main`. Create it **before** the
  workflow reaches `main`: a workflow that names a missing environment creates
  it, open to every branch.

A `Resource not accessible by personal access token (HTTP 403)` on the comment
means the token can read but not write. The repository is public, so a token
reads it whatever it was granted. Check that the token's repository access
includes this repository, that Pull requests is Read and write, that an
organisation owner has approved it if the organisation requires that, and that
its account has Write access.

Not covered: a bump merges itself with `GITHUB_TOKEN`, and GitHub starts no
workflow for an event that token caused, so a conflict a bump's merge makes waits
for the next merge by a person, or for a manual run.

Nothing checks any of this for you. Read which bumps it comments on, the command
and marker it posts, the environment and the token's scope on any diff that
touches
[`.github/workflows/dependabot-recreate.yml`](.github/workflows/dependabot-recreate.yml).

### The git pin is checked every week

[`scripts/git-pin.env`](scripts/git-pin.env) pins three gits, each a kernel.org
release tarball by version and SHA-256: the node pin, the newest release, which
the engine image builds and every pool and client in the suite runs; and the
newest patches of the two floors' series, which the floors' arm runs. Every git
node runs the image's git, so a git security release reaches a deployment only
through a bump of that pin — Debian's updates to its own git never do — and no
Dependabot ecosystem reads a source tarball. So
[`git-pin.yml`](.github/workflows/git-pin.yml) is the watcher. Every Monday at
06:17 UTC (and on **Actions → git-pin → Run workflow**) it runs
[`scripts/git-pin-check.sh`](scripts/git-pin-check.sh), which reads kernel.org's
listing for the newest release of each pin's series — release candidates are no
release — and, for every pin that is behind, downloads that release and its
signature (`git-<version>.tar.sign`, over the decompressed tar) and verifies it
against the git maintainer's key,
[`scripts/git-signing-key.asc`](scripts/git-signing-key.asc), imported into a
GnuPG home of its own and refused unless it holds exactly the key whose primary
fingerprint the script pins. Then it prints the pin's two new lines — the
SHA-256 is that of the very tarball whose signature verified, never one read
from a listing — and the title of the pull request that moves it. Exit 1 opens
or updates ONE issue, `git pin behind upstream` (labelled `dependencies`, found
by that exact title), with that report as its body, and fails the run; exit 0
closes it; exit 2 — the listing or a download refused, a signature that does not
verify, or by a key other than the pinned one — fails the run and leaves the
issue as it was, because nothing new is known. An issue rather than a pull
request because a pull request opened with `GITHUB_TOKEN` leaves its checks
waiting for a click (as under [A dashboard bump brings its own
bundle](#a-dashboard-bump-brings-its-own-bundle)), and one whose checks run by
themselves needs a token beside the two the Dependabot workflows hold — a new
credential, which the workflow does not take. It holds `contents: read` and
`issues: write` and no secret.

**Making the bump** is a pull request of its own:

1. Replace the pin's two lines in `scripts/git-pin.env` with the ones the issue
   names (`scripts/git-pin-check.sh` prints them again by hand: exit 1 with the
   report, 0 when every pin is current).
2. `make git` — it builds the new git from the tarball the digest names, and a
   digest that is not the tarball's refuses the build — then `make check`, the
   suite on the new git.
3. Measure what the published layers' rules rest on, on the new git and one
   idle machine:
   `make bench BENCH_PKGS=./internal/code/cache BENCH_PATTERN='Midx/size=/^step='`
   (the `git=floor` arms run the node floor's own build). Each arm states the
   rule it holds, and the same pull request re-decides a rule its arm no
   longer holds:
   - **No node writes a multi-pack-index layer of its own**
     (`internal/code/cache`'s package doc): re-decided once `step=tip`'s
     `peak-MiB` stops growing with the history — a node's bitmapped layer
     costing the objects it adds rather than the history. On git 2.56.0 the
     same 64 pushes' layer held 88 MiB over 100 MB and 254 MiB over the
     gigabyte, and took longer there than `step=whole/write=bitmap`'s whole
     rewrite.
   - **A layer is written over exactly its base, never merged with
     `multi-pack-index compact`** (`internal/code/compact`'s): re-decided once
     `merge=stacked` no longer reports `pathological`, or `merge=root-compact`
     beats `merge=root-write` by more than the tail's layer write a compaction
     from the root cannot skip (`step=publish/layer=tail`'s `midx-s/op`).
   - **A published layer covers every live pack its base does not**:
     re-decided once `step=closure` fails, git having written a bitmap over
     packs that are not closed.

   The constants the same arms measure — `internal/code/cache`'s bytes an
   object, `internal/code/compact`'s publication cadence — say at their
   definitions what moves each.
4. Title the pull request as the report does: `build(deps): bump git from <a>
   to <b>` for the node pin, `build(deps): bump git's node floor from <a> to
   <b>` or `… client floor …` for a floor.

The pull request runs the snapshot job, which builds the engine image's git from
the new pin on both platforms and holds it to the pin, and the suite builds the
three gits again on CI (about four minutes) until `main` saves them. A diff to
the pin, to [`scripts/git.sh`](scripts/git.sh) or to the key changes the git
every git node runs, so it is read as release surface. A floor pin only ever
moves to a newer patch of its own series; raising a floor itself is a change to
`gitproc.NodeMinVersion` or `ClientMinVersion` and is argued in
[ADR-0057](adr/0057-a-git-node-runs-git-2-55-from-one-pin.md). A git 3.x node
pin changes the recipe too: git's own Makefile makes Rust mandatory from 3.0,
which the report says when it names one.

**The signing key is checked in the same run.** The maintainer's release-signing
subkeys expire (the ones in the file on 2028-01-11), and once they have, no
release can be verified and every run fails with exit 2. So the run's last step,
`scripts/git-pin-check.sh key`, reads the latest expiry among the key's valid
signing subkeys, bounded by the primary key's own, and fails the run from 60
days before it — eight weekly runs to act — whether or not a pin is behind. To
refresh it, export `keys/20D04E5A713660A7.asc` again from kernel.org's PGP
keyring ([pgpkeys.git](https://git.kernel.org/pub/scm/docs/kernel/pgpkeys.git)),
check that its primary fingerprint is still
`96E07AF25771955980DAD10020D04E5A713660A7` (`gpg --show-keys
scripts/git-signing-key.asc`), replace the file with it, and land it as
`build(deps): refresh git's release signing key`. A file that is not that key,
or that holds a second key beside it, is refused (exit 2) rather than trusted.
So is a release signed by a signing subkey the file does not hold yet — one the
maintainer added after the export, which the expiry check cannot see coming —
and that refusal names the subkey and prints these steps, since only the
maintainer's current export can tell it from another key's.

[`scripts/git-pin_test.sh`](scripts/git-pin_test.sh) (`make git-test`, in `make
check` and ci.yml's `build + vet`) is the own suite of the check and of the
recipe: it runs both against a stand-in release listing, stand-in tarballs and
signing keys it makes for itself, so it reads no network and builds no git,
and every case it holds a mutation to — skip the fingerprint check, accept gpg's
exit status without its `GOODSIG`, read the primary key's expiry alone — fails
it. It reads nothing of the environment you run it in, and holds that by running
itself a second time, by a relative path under a `CDPATH` naming another
`scripts/`, in a hostile environment: every knob the scripts read pointed
elsewhere, a GnuPG home of yours, and a curl configuration (`.curlrc`) refusing
the `file://` downloads every case makes. The scripts themselves do read your
curl configuration, so a proxy or a CA bundle there applies where they really
download, and they send what it makes curl print (a `write-out`, say) to
stderr: a case holds the check's report and the recipe's bin directory to
being all either prints on stdout under such a file. Nothing holds the
workflow file itself: read its `permissions:`, the issue title it matches and
the step that runs the key check on any diff that touches it.

## Releasing

Releases are cut by pushing a `v*` tag; GitHub Actions builds every target
with [goreleaser](https://goreleaser.com), signs the checksums and the
container image with keyless Sigstore, publishes the image to GHCR and creates
the GitHub Release. **The tag is the version** — nothing in the tree records
one, so there is nothing for a tag to disagree with. The whole pipeline
rehearses locally with `make snapshot`, without a tag and without touching
GitHub; the full process is in [RELEASING.md](RELEASING.md).

`main` has a signed image too, `ghcr.io/crewlet/crewlet:main`, rebuilt on
every push to `main` and moved to that build — one tag, and the only one it
ever pushes ([RELEASING.md](RELEASING.md#images-from-main)). Its binary
reports `main-<the first 12 hex digits of the commit>`. A newer push cancels
the run before it, so when pushes land close together only the newest commit
is built, and a commit that starts no push run is never built: Dependabot's
bumps, merged with `GITHUB_TOKEN`, start none, so `main` stays on the last
pushed commit's image until the next push. That image is **not a release**: it
creates no Release, carries no notes, and is owed nothing below — a deployment
that runs `main` has opted out of the compatibility a tag promises.

### Nothing is released until a tag ships it

The tag is also the project's only compatibility boundary — it is the one
thing an operator can pin, pull and run. So a surface no `v*` tag has ever
shipped has nobody behind it, and **you may break it outright**: rename the
config field, reshape the struct, drop the CLI flag, move the route. Do not
leave a deprecated alias, a both-spellings fallback, an adapter or a
vestigial flag behind — there is nothing on the other side of them to be
compatible with, and code with no caller is indistinguishable from code
whose caller nobody found.

Check rather than assume; `git tag --merged` lists the tags, and
`git log <newest tag>..HEAD -- <path>` says whether what you are changing is
inside one. As of the only release, `v0.1.0`, that answer is *nothing*: the
tag sits on the initial commit, which contains no Go at all — so no package,
config field, CLI command, event type, API route or schema file in this tree
has yet been in a release.

No compatibility is kept with an older build that no tag shipped: no
deployment of one is upgraded in place, no peer of one ever joins this fleet
or shares its broker, and nothing it wrote — rows, records, blobs, bucket
keys, coordination documents, event payloads — is read, migrated, swept or
tolerated. Code that exists only for such a build (a reader for a version
this tree never writes, a fallback for a field it always sets, a gate waiting
for one particular older build's era to end, a sweep of a bucket only an
older build created) has no caller, and is deleted rather than kept. The
first tag that carries Go code is the first build anything is owed to: from
it on, the next release reads what that tag wrote and runs beside it in one
fleet, while a build between two tags is still owed nothing (see
[RELEASING.md](RELEASING.md)).

Two things a missing tag still does **not** excuse, because neither is about
an older build:

- **An applied migration is history, not source.** `schema_migrations` is
  keyed on the filename, so editing a file that already ran silently never
  re-runs it: a database that applied it keeps the old shape while the code
  assumes the new one. Reshape with a new numbered migration under
  `internal/store/schema/`. A **replicated** migration also carries three
  rules, checked by
  `internal/store.TestNoReplicatedMigrationDeclaresAConstraintTheApplyCannotSurvive`:
  no foreign key, no `UNIQUE` outside a primary key, and no `COLLATE`. Each
  one aborts an apply transaction deterministically on every data node, so a
  constraint that would flag an anomaly on one machine stalls the whole
  fleet's log instead; uniqueness a writer must see whole is arbitrated on the
  record's subject. The node estate is not governed by them.
- **This build must be a good older peer to the next.** A rolling upgrade
  will put this build and its successor on one stream and one coordination
  store, so the forward mechanisms stay: the event envelope evolves
  additive-only and a type this build does not know round-trips losslessly;
  a state-log record this build cannot read is deferred and kept, never
  dropped — except a gate record above the version it reads, or one whose
  envelope it cannot decode at all, which stops the applier rather than
  apply it wrong — which is why every node names the version it reads on
  its position heartbeat, a writer of a gate above the base version waits on
  that census before publishing one, and an envelope never refuses what a
  newer build may add to it; and a bump of the seat-host lease protocol makes
  the newer node refuse to claim beside a live lower lease. That contract
  runs FORWARD, to the builds after this one, and never obliges this tree to
  read what an earlier unreleased build wrote.

A free break is still a complete one: the rename lands everywhere in the same
change — `docs/`, `examples/`, the dashboard, the tests — and `schema/` is
regenerated, never hand-edited.

## Documentation

The pages under `docs/` are the source of truth and are published to
[docs.crewlet.ai](https://docs.crewlet.ai/engine/latest/), under the engine's
own path and a version: `/engine/latest/` is the newest release (until this
engine's first release, a redirect to `main`'s pages), `/engine/next/` is
`main`, and `/engine/vX.Y/` an older release line, with `schema/` and
`skills/` published file for file beside each version's pages. Link
`/engine/latest/`: a page or file the newest release lacks answers there with
a redirect to its `/engine/next/` copy. Nothing forwards a path the site no
longer builds. Keep the pages plain
CommonMark: relative `.md` links between pages, figures in `docs/assets/`, one
`# Heading` per page.
The site derives its navigation from `docs/index.md`, so **a new page must be
linked there** — the site build fails on a page nothing links to.

Links are checked, not trusted. `internal/docsgate` fails the suite on a
relative link to a file that does not exist, on an `#anchor` that names no
heading of its target (by GitHub's slug: rename a heading and every anchor to
it goes red), and on a page under `docs/` that `docs/index.md` does not list.
The dashboard's own addresses — `#/work/ENG-42` in a guide, a tool skill or a
string the engine composes — are held by `dashboard/src/app/links.test.ts`
against the dashboard's route resolver, along with every pointer at a
`dashboard-design.md` heading. And `internal/docslinks` reads every text file
in the tree for an absolute `https://docs.crewlet.ai` link, failing on one the
site does not build — a root-shaped path, or a page, schema or skill under
`/engine/latest/` or `/engine/next/` that this tree does not have — and on a
page spelled without its trailing slash, which the site answers with a
redirect. A link to a page that moved is fixed by pointing it at where the
subject lives now, never by deleting the gate's view of it.

`docs/` is written for people *running* Crewlet. Reasoning aimed at people
*changing* it goes in a package doc, where `go doc` surfaces it beside the
code — including the part that matters most: why the obvious alternative is
wrong, and what it cost when it was tried. A comment that only restates what
the code does has not earned its line.

None of it binds. If a rationale no longer matches the engine, change the code
and rewrite the comment in the same commit — a doc comment describing a design
the tree no longer has is worse than none.

### Architecture decision records

A decision that binds **more than one package** goes in `adr/`. A decision
inside one package stays in that package's doc comment and gets no record —
that single criterion is the whole filing rule, and it is what stops the
directory becoming a second copy of the package docs. Eleven packages in this
tree exist because one rule was written down twice and the copies drifted, so a
record here never restates its authority; it names it.

Each record carries an **Authority** (the one package or page holding the
detail), an **Enforced-by** (a named test, a compile error, or the literal
`nothing`), and the numbers it was settled on. The authority's own doc names
the record back, and `internal/adr` checks that anchor in both directions
inside `make check`: a record whose authority does not cite it fails, and so
does a doc citing a record that does not exist.

`Enforced-by: nothing` is an allowed and often honest answer. It goes in
`adr.Unenforced` with a sentence saying what a gate would have to look at —
checked two-sided, so an entry that stops being true also fails. The register
this replaced had fifteen decisions, no anchor, no gate, and six false
statements by the time anybody compared it with the code; `adr/README.md`
records that, because it is the argument for every field in the template.

## Changes `crewlet-operator` has to follow

[`crewlet-operator`](https://github.com/crewlet/crewlet-operator) is the
Kubernetes operator that runs Crewlet companies from `crewlet.ai` custom
resources. It consumes this engine's contract rather than sharing code with it:
it renders both config tiers from its objects, calls the engine's API, copies a
few run-time decisions out of the engine's source and runs the engine's image in
pods of its own. Its suites run this repository's `main` as
`ghcr.io/crewlet/crewlet:main` publishes it, never a pin of it (its ADR-0002), so
a change here that it has not followed fails its next run while nothing there
changed. That red run is the safety net, not the plan: a change that needs
support there ships with it.

A change needs a companion there when it touches something `crewlet-operator`
renders, calls, copies, runs or links:

- **A document it renders.** A field, enum value, default, rule or refusal of
  the company document (Tier B, the company types in `internal/config`) or of
  `crewlet.yaml` (Tier A, `Bootstrap`, which it writes for each pod: `node`,
  `stream`, `store`, `coordination`, `api` with `api.auth`, and `secrets` among
  its blocks), and so of `schema/`. It renders both from its objects, hands
  every render to `crewlet validate` and to a running engine's `/config`, and
  holds its CRD rules to the engine's own refusals.
- **A credential position.** A field tagged `secret:"true"` (`x-crewlet-secret`
  in the schema). Each takes a reference field on one of its kinds, or a stated
  reason it has none, and a test holds the two lists to each other in both
  directions, so a credential added here fails its build until it is mapped.
- **A route or an answer it calls.** `/health`, `/ready`, `/fleet`, `/agents`,
  `/config`, `/secrets` and the pause tools of `/operator/mcp`: the path, the
  method, the status, the error envelope, a field it reads (`/ready`'s `reason`;
  `/health`'s `status`, `posture` and `applied_epoch`; `/fleet`'s nodes and seat
  leases), the paths a validation error names, or who may call it.
- **A table or a function it copies a decision from.** Its
  `internal/render/mirror_test.go` fingerprints engine functions by file and
  name — the coding-run launch, the address checks the boot refuses to start on
  and the Atlassian host rule among them — and its CRD suite reads the CLI
  profiles (`cliprofile`: each CLI's token and key variables and the
  variables that keep it inside a seat's home) from the engine's own refusals. A
  change to one, or a rename or a move, fails it until it has been read against
  the files that copy it.
- **How the engine runs in a pod.** `crewlet run -config`, the `CREWLET_*`
  variables a pod sets, the ports, `/health` and `/ready` as the probes, the
  drain and the grace a pod is given for it (the engine puts no bound of its own
  on a drain), and where the engine writes: `crewlet-operator` runs it as a
  non-root user with a read-only root filesystem and only a data volume, `/tmp`
  and the user's home writable, so a new place the engine writes is a write the
  pod refuses.
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
  you are changing, and for the engine file you are editing, since its mirror
  table names engine files by path. No hit is the answer.
- **Make the change there; a note is not the change.** Open a pull request on
  `crewlet-operator` that names yours, and name it in yours, working to its own
  `CONTRIBUTING.md`: its `make check`, `git commit -s`, its commit scopes, its
  docs (`docs/reference/api.md` there is generated). If you cannot, say exactly
  what it needs — the surface, the files — in your pull request instead of
  saying nothing.
- **Prove it against your change, not against `main`.** Its suites run the
  engine that `main` publishes, which lacks your change until it has merged and
  the image has been rebuilt. Its `CONTRIBUTING.md` ("The engine suite") and
  ADR-0002 say how to run them against another engine: `CREWLET_VERSION` for a
  commit, `CREWLET_IMG` for an image built from a checkout of this repository
  (`make image-context`). Say what you could not run.
- **Land the pair back to back, this one first.** Its tests read `main`, so its
  pull request is green only once this change is on `main` and the image
  rebuilt, and a change that breaks `crewlet-operator` leaves every run of it
  that reads `main` red from this merge until its own lands.
- **Follow `main` plainly.** `crewlet-operator` tests against `main` alone and
  its API is `v1alpha1`, which breaks without ceremony, so the companion change
  is a rename or a reshape — no alias, no both-spellings reader, no
  engine-version switch (see [Nothing is released until a tag ships
  it](#nothing-is-released-until-a-tag-ships-it)) — unless it has shipped a tag
  that carries the surface, in which case its own `CONTRIBUTING.md` says what a
  break takes.
- **Say what you did, or why nothing was needed.** The pull request description
  links the companion change, or says in one line why there is none
  (`crewlet-operator: none — internal to the turn loop`). Most changes touch
  nothing above and need no companion change; one that touches a surface above
  and says nothing is what this section exists to catch.

## Commit messages

Commit subjects follow the [Conventional Commits](https://www.conventionalcommits.org/)
shape — a type, an optional component scope, then the summary:

```
type(scope): summary

optional body explaining why, wrapped at 72 columns

Fixes #123
```

### Type

Required, and drawn from this set:

| Type | Use for |
|---|---|
| `feat` | a new capability — a config field, CLI command, tool, endpoint, provider |
| `fix` | a bug fix, including correcting a tuning value that was wrong |
| `docs` | pages under `docs/`, the root markdown files, docstrings |
| `refactor` | a change that neither fixes a bug nor adds a capability |
| `perf` | a change made for speed, memory, or token cost |
| `test` | tests only |
| `ci` | `.github/workflows/*`, `.github/dependabot.yml` |
| `build` | `go.mod`, the `Dockerfile`, `.goreleaser.yaml`, the Compose stack — and any dependency bump, whichever file records it |
| `chore` | anything else with no user-visible effect (tidying, repository maintenance) |
| `revert` | reverting an earlier commit |

### Scope

The scope names the **component** the change lands in. For code that is the
package directory under `internal/` — `agent`, `api`, `authz`,
`coord`, `engine`, `gitlab`, `iam`, `knowledge`, `mattermost`, `mcp`, `notify`,
`queue`,
`sandbox`, `schedule`, `seat`, `secrets`, `store`, `tools`, and so on — so a
reader can go from a subject line to a directory without guessing. Two scopes
name a component by the name people use rather than by its import path:
`dashboard` for the dashboard — its source in `dashboard/` and the built
bundle in `static/dashboard/` — and `cli` for `cmd/crewlet/`. Outside
`internal/`, scope by area: `docs`, `deps`, `examples`, `schema`, `scripts` —
and for CI, the workflow's own name (`ci(release)`, `ci(docs-publish)`).

Drop the scope only when a change genuinely spans the whole repository
(`chore: normalise line endings`). The type is never optional.

### Summary

- Imperative present tense — "add", not "added" or "adds".
- Lowercase first word (unless it is an identifier), no trailing period.
- 72 characters or fewer, prefix included.
- Say what changed for someone using the code, not which file you edited.

Put the *why* in the body; the diff already shows the *what*. Reference issues
in a footer (`Fixes #123`). A breaking change gets a `!` before the colon —
`feat(config)!: …` — and a `BREAKING CHANGE:` footer describing the migration.
Both mark a change an operator has to act on, so they belong to a surface a
release actually shipped; changing one no tag has carried breaks nobody and
takes neither (see [Nothing is released until a tag ships
it](#nothing-is-released-until-a-tag-ships-it)).

### One change per commit

Keep separate pieces of work in separate commits, each with its own scope. A
bug you fixed in passing because it affected your change, or a tuning value
you corrected on the way, belongs in a commit of its own — that keeps it
reviewable, and revertable, on its own. A bug that does not affect your change
is not in the pull request at all: it gets an issue (see [A bug you find along
the way](#a-bug-you-find-along-the-way)).

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
it came from work you have the right to submit under the MIT licence this
project ships under, and that you understand the commit and your sign-off are
published permanently. Read it once; it is 34 lines.

Because the DCO covers relayed work as well as your own (clauses (b) and (c)),
a commit may carry more than one sign-off and the trailer need not name the
commit's author — a forwarded patch keeps the original signer's line and gains
yours. So what is required is *a* well-formed trailer on every commit, not one
that matches the author.

**This one is enforced**, unlike the subject conventions above.
`scripts/check-signoff.sh` judges the commits a branch adds relative to its
merge base with the base branch, and it runs in two places: `make check`
locally, and CI's `sign-off` job over the pull request. A commit without a
trailer fails the build.

What neither one reaches is the commit the merge button *writes*. GitHub
composes that message at merge time, after every check has reported, and it
does not always carry the branch's trailers: `074e3a1` and `7d0e29e` on `main`
are two commits that reached it this way, from branches whose own commits were
properly signed. A `sign-off (main)` job used to report that after the fact and
no longer runs, so nothing catches it happening again — what keeps the trailers
is the repository's squash and merge commit-message settings, and those live in
Settings, where a checkout cannot see them.

Forgot on one you have already made? The repair rewrites the commits, which is
free before a push and a force-push after:

```bash
git commit --amend -s --no-edit          # just the last one
git rebase --signoff origin/main         # every commit on the branch
```

If you are working in a fork whose `main` has gone stale, rebase onto the
merge base the gate prints rather than onto a ref — `git rebase --signoff
origin/main` against a stale `main` restamps commits you never wrote under
your own sign-off.

Merge commits are exempt — `git merge` and GitHub's merge button author one
with no chance to sign it, and it carries no change of its own to certify.

### Examples

```
feat(sandbox): reuse a paused box when a clarification is answered
fix(confluence): relax the CQL query when the server returns no hits
perf(providers): bench a spent plan rather than relaunch the CLI each round
docs(secret-store): document that the keyring is required
build: stamp the version into the binary at link time
ci(docs-publish): rebuild the docs site when a release ships
build(deps): bump golangci-lint-action to v9
```

Nothing enforces any of *this* — there is no commit-message hook and CI does
not check subjects, types or scopes. Git history is permanent and public, so
the convention holds only because contributors apply it. The sign-off trailer
is the exception, and the section above says what checks it.

**This applies to pull request titles too.** A pull request lands on `main`
either as a single squashed commit whose subject is the pull request title, or
as a merge commit over the branch's own commits — `main` carries both shapes
today. Under a squash the title *is* the subject; under a merge it is what the
release notes are generated from. Either way the title takes the same
`type(scope): summary` form — and everything after the colon is
what readers see in the generated release notes. See below.

## Pull requests

1. Fork and create a feature branch.
2. Make your change, including tests and doc updates — and, where it touches
   what `crewlet-operator` consumes, [its companion
   change](#changes-crewlet-operator-has-to-follow).
3. Commit it with `git commit -s` — see [Sign your work](#sign-your-work).
4. Run `make check`.
5. Open a PR with a clear description of what changed and why. **The title
   matters more than anywhere else**: GitHub generates each release's notes from
   the titles of the pull requests merged since the previous tag, and those
   notes are the *only* record of what a release contains. Yours is read by
   people deciding whether to upgrade, so the summary after the
   `type(scope):` prefix has to read as a release note — what changed for
   someone running Crewlet — rather than as a commit log. Labels
   group it: Dependabot's `dependencies` label sorts bumps to the bottom (see
   `.github/release.yml`).

Small, focused PRs are much easier to review than large ones. If you are
planning a substantial change — a new subsystem, or a reshaping of the config
format — please open an issue first to discuss the approach.

## Reporting bugs and requesting features

Use [GitHub issues](https://github.com/crewlet/crewlet/issues). For bugs,
include the engine version (`crewlet --version`), your config shape (redact
secrets!), and relevant structured-log output.

### A bug you find along the way

Working on one change, you will notice bugs. Whether one is fixed in your pull
request turns on one question: does it affect your change?

- **It affects your change: fix it there.** Your change depends on the broken
  code, the bug is in the code or text you are changing, your change would
  make it worse or newly reachable, it is another copy of the defect you are
  fixing, or it keeps a test or gate your change must pass from passing. Fix
  it properly, in a commit of its own (see [One change per
  commit](#one-change-per-commit)), and say so in the pull request's
  description. That it was broken before you got there does not make it
  unrelated.
- **It is unrelated: open an issue.** A bug none of that describes stays out
  of your pull request. Search the open issues first, and add to one that
  already describes it; otherwise open one per bug with the bug report
  template, saying what is wrong and how it shows, where it is (a permalink,
  or the file and line at a commit, since a line on a branch moves) and the
  fix you would make. Link it from your pull request's description.

If you cannot tell which it is, it affects your change: fix it. A wrong tuning
value (a timeout, a retry count, a batch size, a default) goes the same way,
corrected in your change when it affects it and reported, with the value you
would set and why, when it does not. And anything you believe is exploitable
is never a public issue, commit message or pull request description, whether
or not it affects your change: report it privately (see [Security
issues](#security-issues)).

## Security issues

Please do **not** open public issues for security vulnerabilities — see
[SECURITY.md](SECURITY.md).

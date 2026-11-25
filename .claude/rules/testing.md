---
paths:
  - "**/*_test.go"
  - "**/*test/*.go"
  - "**/testdata/**"
  - "**/*.test.ts"
  - "{Makefile,.golangci.yml,.github/workflows/ci.yml}"
  - "internal/{skipgate,solo,fixedwait,e2e}/**"
  - "scripts/check-signoff*"
  - ".claude/worktrees/*/**/*_test.go"
  - ".claude/worktrees/*/**/*test/*.go"
  - ".claude/worktrees/*/**/testdata/**"
  - ".claude/worktrees/*/**/*.test.ts"
  - ".claude/worktrees/*/{Makefile,.golangci.yml,.github/workflows/ci.yml}"
  - ".claude/worktrees/*/internal/{skipgate,solo,fixedwait,e2e}/**"
  - ".claude/worktrees/*/scripts/check-signoff*"
---

# Gates and Tests
`CLAUDE.md`'s "Checks and Tests" has what every change needs; this is the rest.

## What `make check` runs
- `gofmt -l` over `go run ./internal/sourcetree/gofiles`, and `golangci-lint run`'s stricter `gofmt` (with `simplify`) and `goimports`: a redundant composite-literal type passes the first and fails the second (`CLAUDE.md` says how to repair both)
- `go mod tidy -diff` (`make tidy` applies it)
- `scripts/check-signoff.sh` and its suite `scripts/check-signoff_test.sh`
- `scripts/git-pin_test.sh` (`make git-test`), `scripts/chromium-pin_test.sh` (`make chromium-test`) and `scripts/node-pin_test.sh` (`make node-test`): the suites of the git, Chromium and floor Node recipes and their weekly pin checks, offline and building or fetching nothing; the first and the last need GnuPG and `xz`, the second `zip` and `unzip`
- `scripts/playwright-tree_test.sh` (`make playwright-tree-test`): the Playwright tree recipe's suite, against a stand-in registry and reading no network; it needs `node` and an npm of `MinNPMMajor` or newer
- `go vet ./...`, the only vet (both test targets run `go test -vet=off`)
- `golangci-lint run`, a prebuilt release: one built with a Go whose language version is older than `go.mod`'s refuses the module
- `go build ./...`, and a cross-compile of every release target
- the dashboard's `dashboard-lint` (`npm run format:check`), `dashboard-check` (rebuild and diff the committed bundle) and `dashboard-test` (`npm run typecheck && npm test && npm run test:layout`, the last in a headless browser)
- `notehelper-check`: rebuild `internal/notehelper`'s committed builds (`make notehelper`) and diff them
- `make git`, `make chromium`, `make node` and `make playwright-tree`: the pinned gits (built), Chromiums (fetched: the newest and the floor, `internal/browser.MinChromiumMajor`) and floor Node with its npm (fetched: the newest release of Node `internal/browser/boxhelper.MinNodeMajor`), once per machine, and the Playwright tree (installed with npm from `internal/browser/boxhelper/tree`'s lock, once per lock). Both test targets depend on them, so no gate runs the machine's own git or Chrome, nor hands the box helper's cases the newest Node alone
- `make test`, then `make test-solo`

`-count=1` keeps a cached pass from answering for a change; `make test-norace` is a faster loop, not a gate.

**`make test-solo` runs at `-p 1`**, because N embedded NATS servers in one starved process leave replicated creates unanswered. A package joins it by importing `internal/solo` from its `TestMain`, and `internal/solo`'s roster guard fails the build in both directions. Never use a build tag, `-short`, `-skip` or a custom flag instead: each fails silently (`go doc ./internal/solo`).

In CI, `test plan` deals each half into shards by measured duration, every package in exactly one; the `test (race) I/N` legs run `make test` and the `end-to-end gates I/N` legs `make test-solo`, with `TEST_SHARD`, `TEST_WEIGHTS` and `TEST_TIMINGS`. The required checks are the aggregate jobs `test (race)` and `end-to-end gates`.

**`internal/skipgate`** reads both test targets' `go test -json`. Each `allowed.go` entry has a reason and a `When`: `Always` (also reported once it stops firing) or `Environment`.

`node` runs four suites: the dashboard's (through `npm`), `internal/e2e`'s client replay (in `make test-solo`), `internal/sandbox/executor`'s probes of the three OpenCode plugins the engine embeds (notes, skill-load and tool environment), of its Kimi Code driver, Pi's extension and Cursor Agent's chat-store helper, its stand-in OpenCode server loading the fourth (checkouts), and `internal/browser/boxhelper`'s, which runs the box helper's modules and installs the Playwright tree in each box with each Node's own npm, offline (both in `make test`). `npm` also installs the suite's tree (`make playwright-tree`). A make target that runs one refuses to start without `node`, or without `npm` where it needs it. A passing `make check` prints what it does not cover (`make snapshot`).

`python3` runs the executor's Hermes plugin and carry-script cases (standard library, `make test`).

## Writing tests
- Test files sit beside what they cover, non-Go ones too (the Vitest suites in `dashboard/src`, `internal/e2e/replay.mjs`, `internal/sandbox/executor/opencode_*_probe.mjs`, `internal/browser/boxhelper/boxhelper_probe.mjs`). There is no top-level `tests/` tree
- **A program or directory the suite needs is found through `internal/prereqtest`** (`OnPath`, or `Named` or `NamedDir` for one a variable names): it fails the case under the make targets (`CREWLET_REQUIRE_PREREQUISITES=1`) and skips it only in a bare `go test`. Never a skip, `skipgate` entry or requirement variable of your own. A program no make target requires (a container runtime) keeps its `Environment` entry
- **A contract with more than one implementation gets ONE suite** (`queuetest`, `coordtest`, `storetest`, `scheduletest`, `sandboxtest`), which every backend runs, the memory twins included
- **A store on a fresh path starts from the migrated image**: `storetest.OpenNode`, `storetest.OpenEstate`, or `storetest.Seed` before an engine boots on it. Only a test whose subject is migration opens a fresh file with `store.OpenNode`
- **A test broker declares its size**: `brokertest.StoreIn` for a node's stream (with `brokertest.GitletCacheIn` where Gitlet runs), `StoreMaxBytes: brokertest.StoreMaxBytes` for a broker opened directly, `JetStreamMaxStore: brokertest.StoreMaxBytes` (`max_file_store` in a configuration file) for a server built from nats-server's options, `externaltest.Start` for an external one. Unset, the limit is three quarters of the runner's free disk, so the result depends on the machine; a test binary refuses it (`jetstream.ErrUndeclaredSize`, `engine.ErrUndeclaredCodeCache`, and `engine.ErrUndeclaredBrokerLimit` for an external account that states no limit), and `brokertest`'s `TestEveryNATSServerATestBuildsDeclaresItsSize` catches a server built from options. Only a case whose subject is the derived limit leaves it unset, under `jetstream.SizedByTheDisk` or `engine.SizedByTheMachine`, saying why
- **A test broker keeps its store in `brokertest.TempDir`**, never `t.TempDir`: a stopped nats-server can still flush after `Shutdown` and `WaitForShutdown` return, failing `t.TempDir`'s single removal, while `brokertest.TempDir` retries. A test binary refuses an embedded broker outside one (`jetstream.ErrStoreOutsideTempDir`), and `TestEveryNATSServerATestBuildsKeepsItsStoreInATempDir` catches a server built from nats-server's options in a test's own temporary directory (`TempDir`, `os.MkdirTemp`). A store directory only validated, with no broker started on it, may stay a `t.TempDir`
- **A case that STARTS an engine never runs below `internal/engine`'s `startedLeaseTTL`** (`config.MinLeaseTTL`, the floor `engine.New` holds every bootstrap to). The lease TTL is the seat watchdog's threshold, and the watchdog ends the whole test binary (status 75), which no assertion reports. So a case reads the renew or deadline it is about off the host or its store (`seat.Host.MayStart`, a lease's deadline) rather than waiting for a lease to lapse, and a case whose subject is a fraction of a shorter TTL builds an `Engine` literal that starts no seat host
- **A plan is read through `storetest`, once**: through its vocabulary over either estate's `Handle` (`PlanTree`, `Plan`, `Details`, `IndexesIn`, `Seeks`, `Walks`, `Sorts`, `ScansHeap`, `IndexesOn`, `StatementLiterals`), and through `storetest.Decorrelated`, the one check for the pinned driver's rewrite of a correlated one-aggregate scalar subquery into a join over its WHOLE child table (the `scalar_subquery_<n>` operand, never a FROM-clause subquery's `ephemeral_subquery_` index), held to the driver in both directions. A module-wide walk refuses an `EXPLAIN QUERY PLAN` literal anywhere but `storetest/plan.go` (package `store`'s own files exempt, since they cannot import it), and the tracker's and the knowledge base's index gates, search's vector and lexical gates and the chat prune's room read refuse what `Decorrelated` flags
- **A case that drives Chromium drives the pins** through `internal/browser/chromiumtest`: the two builds `scripts/chromium-pin.env` pins (`chromiumtest.Each`), or the newest alone where its doc says why the floor adds nothing (`TestAPageEndsAtMostItsOwnSessionWithAMessagePastTheReadLimit`), never the machine's own Chrome. A missing build fails it under `test`, `test-solo` and `test-norace`, as a missing git does
- **A case of the box helper runs at each end of what a box may bring** (`boxhelpertest.Each`: the machine's node and npm with the newest Chromium pin, the floor Node and its npm with the floor's), on each Node alone where it drives no browser (`EachNode`), or the newest pair alone where its doc says why the floor adds nothing (`Newest`)
- A phase that offers tools plays its model as a scripted harness run over the REAL tool session (`internal/providers/llm/llmtest`), never by calling the dispatcher around it
- **`internal/fixedwait`** refuses, in test code, a new `time.Sleep`, `After`, `AfterFunc`, `NewTimer`, `NewTicker` or `Tick`, and a new `time.Since`, `time.Until`, or `time.Time`'s `Sub`, `Before`, `After` or `Compare` (never `Equal`), outside a synctest bubble, a verified `//fixedwait:bubble` helper or a marker: `//fixedwait:liveness`, `simulated <reason>` or `bench` on a wait; `measure <reason>` on an elapsed reading; `stated <reason>` on a comparison between instants no wall clock separates, never two readings of the wall clock. Its inventory only shrinks, each shape to its own count (`go doc ./internal/fixedwait`)

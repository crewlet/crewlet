# The developer's entry point to the gates CI runs.
#
# Every target below runs the SAME command .github/workflows/ci.yml runs, with
# the same flags, so `make check` before a push fails wherever CI would fail
# and passes only where CI would pass. A convenience target that quietly drops
# `-race`, or runs the store suite on one driver, is worse than no target at
# all: it reports a pass CI will not honour, and the divergence is invisible
# until the pull request goes red.
#
# For the TEST jobs that is now true by construction rather than by care:
# ci.yml's `test (race)` job runs `make test` with TEST_WEIGHTS and
# TEST_TIMINGS set (below), and its `end-to-end gates` job runs `make
# test-solo`, so there is one command and no copy to keep in step. Every other job
# still inlines its own, and NOTHING asserts those agree --
# internal/version/makefile_test.go used to and was dropped -- so for the rest,
# change a target and its ci.yml step in the same commit, and read both.
#
# The second thing this file is for is the parts that need something the
# machine may not have. The dashboard is a React + TypeScript application built
# by Vite, so its suites and its build need node AND npm; the targets that need
# them FAIL saying what to install, which is the same posture CI takes
# (CONTRIBUTING.md, "A skip is not a pass"). Every queue backend certifies
# itself with no external service: the JetStream suite starts an embedded
# broker per test. The suite's git is the same posture's: every target that
# runs Go tests runs the git the engine image builds, which `make git` builds
# once a machine, and a machine without a C compiler or libcurl's headers is
# told so by name rather than handed its own git (`##@ The pinned gits`).
#
# THE BUILT DASHBOARD IS COMMITTED, and that is deliberate rather than lazy:
# `go build ./...` and `go install …@latest` must work on a clean checkout with
# no node on the machine, and an embed directive cannot run a bundler. So
# `make dashboard` is NOT a prerequisite of `build` — it is a target you run
# when you have changed dashboard/, and `make dashboard-check` is the gate that
# fails when the committed tree does not match its source. Same idiom as
# `go mod tidy -diff` and the generated schema/.
#
# What is deliberately NOT here: running a company. `crewlet run`,
# `crewlet validate` and `crewlet config import` act on an operator's YAML in
# an operator's working directory (docs/getting-started/quickstart.md), not on
# this checkout, so a target for them would have to invent paths that are not
# ours to choose. This file builds, tests and lints the repository; the
# engine's own CLI is the interface to the engine.

# The module pins its own toolchain in go.mod. `auto` fetches it rather than
# failing on a version mismatch. In CI nothing needs fetching: setup-go
# installs go.mod's own toolchain and exports GOTOOLCHAIN=local, which this
# `?=` leaves as it is.
export GOTOOLCHAIN ?= auto

# No recipe here reads CDPATH, and every `cd` in one is relative to this tree:
# `cd $(UI)` is `cd dashboard`, and scripts/git.sh, run as `scripts/git.sh`,
# finds its own directory with `cd scripts`. A CDPATH a developer exported is
# searched before the current directory: measured, an entry holding a
# dashboard/ of its own took `cd dashboard` there, so `npm ci` and the build
# would have run in that other tree. Unexported, it reaches no recipe; each
# script that cds also drops it itself, for the runs by hand.
unexport CDPATH

GO ?= go

# Exported so the partition command and internal/solo's roster guard discover
# packages with the SAME toolchain this file runs the tests with. Both shell
# out to `go list`, and a hardcoded `go` there would resolve through PATH —
# a different toolchain than `make GO=/path/to/go` selected, or none at all.
export CREWLET_GO = $(GO)

# gofmt is the one that ships WITH the toolchain go.mod pins, never whichever
# gofmt is first on PATH. `GOTOOLCHAIN=auto` fetches go.mod's Go for `go`
# itself and leaves PATH's gofmt alone, and gofmt's output moves between
# releases: a host whose PATH still held an older Go reformatted three test
# files into a layout the pinned gofmt — the one ci.yml's setup-go puts on
# PATH — then refused. Asking the toolchain for its GOROOT is what makes `make
# fmt` and `make fmt-check` agree with CI on any host.
GOFMT = $(shell $(GO) env GOROOT)/bin/gofmt

# ./crewlet is where `go build ./cmd/crewlet` drops the binary; dist/ is
# goreleaser's, and build/ holds what its before hook generates and the image
# context `make image-context` stages. Those three are the build output
# .gitignore knows about and `make clean` removes; nothing here writes
# anywhere else, so `make clean` has nothing to guess at.
BIN := crewlet

# -count=1 bypasses the test cache: a cached PASS recorded before the change
# you are about to push is exactly the answer a pre-push gate must not give.
# -race is not optional either — the engine's concurrency model is real
# parallelism and CI runs the WHOLE suite under the detector, so a `make test`
# without it would pass where CI fails. `make test-norace` is the escape
# hatch, and says what it costs.
#
# -timeout IS NOT A BUDGET, it is a HANG DETECTOR, and it has to be stated
# because go's own default is 10 minutes PER PACKAGE and internal/e2e does not
# fit in it: that package starts a real engine, a real broker and the real API
# per test, and has measured 776s under -race. Left at the default the gate
# does not merely flap — it cannot pass on a machine that speed, and the
# failure reads as a hung test rather than as a budget nobody set.
#
# Thirty minutes is a hang detector sized against the worst LEGITIMATE run,
# not against the worst run ever seen. On CI's own four-vCPU runners
# internal/e2e completed green in 426.6s on main and 561.9s on a pull request
# (both 2026-10-07); the pathological 1684.497s that once came within 115s of
# this wall was a run that lost nine cluster-start attempts to unanswered
# broker metadata requests, which is a defect that has been fixed rather than
# a duration to budget for (see internal/jsprovision's Ask). Against the slower
# green figure this is 3.2x, which is room for a runner a third that speed.
#
# It applies to every package because the flag is per test BINARY: a unit
# package that hangs dies in thirty minutes rather than go's default ten,
# which is the cost of having a gate that can pass at all.
#
# AND IT IS THE ONLY DEADLINE A HUNG TEST MEETS. ci.yml sets no
# `timeout-minutes`, so the outer bound is GitHub's own per-job default of six
# hours — this fires twelve times sooner, and it is the one that fires
# usefully: `go test` hitting its own deadline prints a full goroutine dump
# naming what is stuck, where a job killed by the runner prints nothing at all.
# A ceiling in the workflow would have to sit ABOVE this one to keep that true,
# which is most of the reason there is not one.
#
# TEST_TIMEOUT is defined BEFORE GOTEST, and that is load-bearing rather than
# tidy: `:=` expands immediately, so with the assignment below the reference
# the flag was handed an EMPTY value and `go test` parsed the package list as
# its argument — `invalid value "./..." for flag -timeout`. `make test` and
# therefore `make check` could not run at all.
TEST_TIMEOUT := 30m

# -vet=off, because the vet `go test` runs by default is a DUPLICATE here and
# not a gate of its own. It is twelve analyzers (cmd/go's defaultVetFlags), and
# `go vet ./...` runs all twelve and more over the same packages and the same
# _test.go files — this tree has no race-tagged file, so -race does not change
# what is analysed. That full vet is `check`'s `vet` prerequisite and a step of
# ci.yml's `build + vet` job, and it is what certifies vet now: drop either and
# the copy that used to mask the loss is gone too. What the duplicate cost was
# a cold vet pass per test package in every test job — run SERIALLY in
# `test-solo`, whose -p 1 serialises vet with everything else.
#
# GOTESTBUILD is every flag that changes what is COMPILED, held apart so the
# solo half's prebuild (below) compiles exactly what its run then finds in the
# build cache: a flag in one and not the other is a different action ID, and
# the prebuild would compile a tree nothing reads.
GOTESTBUILD := -race
GOTESTRUN   := -vet=off -count=1 -timeout $(TEST_TIMEOUT)
GOTEST      := $(GO) test $(GOTESTBUILD) $(GOTESTRUN)

# THE SKIP GATE, on the end of both test pipelines.
#
# `go test -json` is what makes a skipped SUBTEST visible at all — plain output
# says nothing about one without -v, and the suite job has never passed it, so
# every skip this repository has ever taken was absent from every CI log. The
# gate renders the stream back to ordinary output as it reads, so a log looks
# unchanged, and then fails on a skip nothing declared. `go doc ./internal/skipgate`
# has the two defects that were found by measuring rather than by reading.
#
# IT RUNS `go test` RATHER THAN CONSUMING A PIPE. As a pipeline the gate was
# the last command, so make saw only ITS status and `go test`'s was lost — and
# a producer killed mid-run emits a truncated stream with no failure record in
# it, which every check in the gate would read as a clean pass. There is no
# PIPESTATUS in make's default /bin/sh to recover it, and switching this file
# to bash for one recipe is a wider change than the bug deserves. Running the
# command removes the question. -v is dropped because -json carries every line.
SKIPGATE = $(GO) run ./internal/skipgate

# The two halves of the suite, COMPUTED rather than listed.
#
# NOT A COVERAGE CUT — `check` depends on both, and ci.yml runs both. It is a
# CONTENTION cut: a solo package stands up N engines, each embedding its own
# NATS server, in ONE process, and a replicated create that has to reach a
# quorum of members starved of CPU goes unanswered rather than slow.
# `go doc ./internal/solo` is the whole story — the measurements, which of
# them are of today's budgets and which are not, and what every obvious
# alternative (a build tag, -short, a flag, -skip, a nested module) cost when
# it was tried.
#
# This was a hand-written `go list ./... | grep -v '/internal/e2e…'` in two
# files and a third, already divergent, copy in CONTRIBUTING.md. It named ONE
# package. Three others stand up multi-member clusters — internal/node and
# internal/statelog at THREE members each, plus the harness's own suite — and
# all three ran in the contended half, surviving on the harness's four-attempt
# port-race retry. A filter that names packages cannot say which packages it
# should have named; a marker plus internal/solo's roster guard can, and does.
#
# LAZY, BUT COMPUTED ONCE. Both forms of the obvious assignment are wrong here
# and in opposite directions: `:=` runs the partition while make is still
# PARSING, so `make help`, `make build` and `make fmt` each pay ~0.6s for two
# `go list` walks they never look at; plain `=` costs nothing until referenced
# but then re-runs per reference, and each of these is referenced more than
# once below (a guard, then the recipe).
#
# So each expands once and redefines itself as a simple variable — `$(eval)`
# expands to nothing, and what is left is the value it just assigned. Every
# later reference is a plain lookup.
#
# TEST_WEIGHTS AND TEST_TIMINGS are how ci.yml starts `make test`'s longest
# packages first, and leaving them unset leaves a local run exactly as it was:
#
#   TEST_WEIGHTS=FILE the measured seconds per package (a skipgate -timings
#                     file) the partition starts the longest first by. It must
#                     exist; empty, or unset, is go list's order.
#   TEST_TIMINGS=FILE write how long each package that passed took, which
#                     ci.yml keeps from a passing run on main as the next
#                     run's TEST_WEIGHTS.
#
# Membership never depends on either: `go doc ./internal/solo/partition` says
# why a weight can move a package earlier or later and never drop one.
#
# THE PARALLEL HALF ONLY. `make test-solo` runs at -p 1, where cmd/go's one
# worker links and runs every test binary in turn, so the half takes the sum
# of them in any order and a weight could not end it a second sooner.
#
# The TEST_ prefix is the family the other test knobs already share
# (TEST_TIMEOUT), and it is also what keeps a variable this file takes from the
# ENVIRONMENT — which is what `?=` does — from answering to a name as generic
# as WEIGHTS, which another tool may well export.
TEST_WEIGHTS ?=
TEST_TIMINGS ?=

PARTITION     = $(GO) run ./internal/solo/partition
PARALLEL_PKGS = $(eval PARALLEL_PKGS := $(shell $(PARTITION)$(if $(TEST_WEIGHTS), -weights $(TEST_WEIGHTS)) parallel))$(PARALLEL_PKGS)
SOLO_PKGS     = $(eval SOLO_PKGS := $(shell $(PARTITION) solo))$(SOLO_PKGS)

# The release targets, cross-compiled. Nothing else builds for anything but
# the machine you are on, so a build tag or a platform-gated file that only
# breaks darwin reaches the tag — and a broken tag is a release to re-cut.
# ci.yml runs these as a matrix and the pairs are .goreleaser.yaml's. All
# three lists have to agree and nothing checks that they do, so a target
# added or dropped here belongs in the other two in the same commit.
CROSS_TARGETS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

# The Go files gofmt is handed: this tree's, by internal/sourcetree's rule,
# and never `.` — gofmt enters every directory under the one it is given, so
# with an agent's worktree under .claude/worktrees/ `fmt-check` reported that
# checkout's half-edited files as this tree's and `fmt` REWROTE them. Each
# recipe captures the list before using it and stops if listing failed,
# because gofmt handed no paths formats its standard input and passes. See
# `go doc ./internal/sourcetree/gofiles`; ci.yml's gofmt step is the same.
GOFILES = $(GO) run ./internal/sourcetree/gofiles

COMPOSE ?= docker compose

# Passed through to the vendor bootstrap scripts, which provision the seats of
# the company config named here and skip that step when it is empty:
#   make mattermost-up COMPANY=examples/nimbus.company.yaml
COMPANY ?=

.DEFAULT_GOAL := help

.PHONY: help build crewlet install fmt tidy image-context schema metrics-doc alarms-doc derived gate-answer objects-answer broker-answer \
        dashboard dashboard-check dashboard-dev dashboard-test dashboard-lint \
        check fmt-check tidy-check signoff-check signoff-test git-test vet lint test test-norace test-cross test-solo \
        require-npm bench git test-gitproc \
        mattermost-up mattermost-down \
        gitlab-up gitlab-down \
        snapshot clean require-node

help: ## list every target
	@awk 'BEGIN { FS = ":.*## " } \
	     /^##@/ { printf "\n%s\n", substr($$0, 5); next } \
	     /^[a-z][a-z0-9-]*:.*## / { printf "  %-16s %s\n", $$1, $$2 }' \
	     $(MAKEFILE_LIST)
	@echo

##@ Build

build: ## compile every package (ci: build + vet)
	$(GO) build ./...

crewlet: ## build the engine binary into ./crewlet
	$(GO) build -o $(BIN) ./cmd/crewlet

install: ## go install the engine onto your PATH
	$(GO) install ./cmd/crewlet

fmt: ## rewrite everything gofmt would change
	@files="$$($(GOFILES))" || exit 1; \
	$(GOFMT) -w $$files

tidy: ## tidy go.mod / go.sum
	$(GO) mod tidy

# Build output only. The store file, its -wal/-shm siblings and the *-data/
# directories beside them are a checkout's own company state, not something
# this file produced, so removing them is the developer's call and not a
# side effect of cleaning a build.
clean: ## remove the build output (./crewlet, dist/ and build/)
	rm -f $(BIN)
	rm -rf dist build
	rm -rf $(UI)/node_modules

##@ Dashboard

# The dashboard's source lives here; its BUILD OUTPUT is committed to
# static/dashboard and static/artifacts (the page runtime and the artifact
# kit, the third build target), both of which package static embeds.
UI := dashboard

# Every tree the dashboard's build writes, and so every tree the drift gate
# judges.
UI_OUT := static/dashboard static/artifacts

# npm ci rather than npm install: the lockfile is the pin, and `install` is
# allowed to move it. A build whose dependency versions drift is a build whose
# committed output cannot be reproduced, which is the whole thing
# dashboard-check depends on.
$(UI)/node_modules: $(UI)/package-lock.json | require-npm
	cd $(UI) && npm ci
	@touch $(UI)/node_modules

dashboard: $(UI)/node_modules ## build the dashboard into static/dashboard and static/artifacts (commit the result)
	cd $(UI) && npm run build

dashboard-dev: $(UI)/node_modules ## run the dashboard dev server against a local engine on :8000
	cd $(UI) && npm run dev

dashboard-test: $(UI)/node_modules ## the dashboard's own suites (ci: dashboard)
	cd $(UI) && npm run typecheck && npm test

# A GATE, NOT A CONVENIENCE, which is why `check` depends on it. It was a
# target nothing ran: ci.yml's dashboard job runs `npm run format:check`
# before anything else and fails the build on it, and a local `make check`
# that skipped it reported a pass CI would not give. Five files reached a
# pull request that way, in a red job whose first line was the formatter.
dashboard-lint: $(UI)/node_modules ## check the dashboard's formatting (ci: dashboard)
	cd $(UI) && npm run format:check

# THE DRIFT GATE. The committed bundle has to be what this source builds, and
# nothing else can tell you when it is not: a stale bundle compiles, embeds,
# serves and passes every Go test — it just runs code nobody wrote.
#
# Rebuild, then judge. It prints the drift and fails; it does not repair it,
# for the same reason `go mod tidy -diff` does not: a gate that rewrites the
# tree it is judging cannot be trusted about what was committed.
#
# TWO QUESTIONS, because `git diff --exit-code` alone answers only the first.
# It compares the working tree with the index over the paths the index
# already HAS, so a file the rebuild wrote that no commit carries is invisible
# to it — and that is the file a commit most often leaves out. Every emitted
# name is content-hashed, so a changed chunk is a NEW path, and `git commit -a`
# stages modified and deleted files but never a new one: the commit carries
# the index.html that imports the new chunk and the deletion of the old one,
# the diff is empty, and the binary serves a shell asking for a module it does
# not embed. The second question is `git ls-files --others`: is there
# anything on disk under static/dashboard or static/artifacts the index does
# not hold at all.
#
# `--others` WITHOUT `--exclude-standard`, so an ignored file fails too. The
# embed is `all:dashboard`, which ships every file on disk whatever
# .gitignore says, and the build empties the directory first (`emptyOutDir`),
# so everything there afterwards is build output. A file .gitignore hides is
# one `git add -A` will not stage and a clean checkout never has; it prints as
# `!!` below, and the fix is the rule `git check-ignore -v` names.
#
# AGAINST THE INDEX, not HEAD, which is why the verdict is not an empty
# `git status --porcelain`: that also reports what is STAGED, so the repair
# printed below — which stages the rebuild — would leave the gate red until
# the commit was made, and `make check` could never pass before committing a
# dashboard change. What a commit will carry is the index; on ci.yml's fresh
# checkout the index IS HEAD, so the two readings agree there. The listing
# drops the staged-only lines (`grep -v '^. '`, an unchanged second column)
# for the same reason: what it prints is the drift and nothing else.
dashboard-check: $(UI)/node_modules ## fail if static/dashboard or static/artifacts is not what dashboard/ builds (ci: dashboard)
	cd $(UI) && npm run build
	@untracked="$$(git ls-files --others -- $(UI_OUT))" || exit 1; \
	if ! git diff --exit-code -- $(UI_OUT) || [ -n "$$untracked" ]; then \
	  { echo; \
	    echo "static/dashboard or static/artifacts is not what dashboard/ builds."; \
	    echo "What a rebuild changed or wrote that the index does not carry:"; \
	    git status --porcelain --untracked-files=all --ignored=matching \
	      -- $(UI_OUT) | grep -v '^. '; \
	    echo "run: make dashboard && git add -A -- $(UI_OUT)"; } >&2; \
	  exit 1; \
	fi

##@ The pinned gits

# THE GITS THE SUITE RUNS, built from the release tarballs scripts/git-pin.env
# pins by scripts/git.sh — the recipe the engine image builds its own git with —
# once a machine, into a per-user cache (CREWLET_GIT_CACHE, by default
# ~/.cache/crewlet/git; a pin bump or a change to the recipe is a new build
# beside the old). The code host runs real git, and what a case asserts about it
# is the git's behaviour as much as the engine's, so the suite runs the git a
# git node runs rather than whichever one the machine has: every target that
# runs Go tests depends on `git` and runs under GIT_ENV, and so do CI's two test
# jobs and its `git recipe (macOS)` job, which call those targets.
#
# EVERY PIN IS BUILT, the floors' as well as the node's, so a pin whose version
# or digest is wrong fails here — at the download its SHA-256 refuses — rather
# than in whichever later run first needs it. About four minutes for the three
# on four vCPUs, once; after that git.sh finds each build and answers at once.
GIT_PINS := node node-floor client-floor

git: ## build the pinned gits the suite and the image run (scripts/git-pin.env; needs a C compiler and libcurl's headers)
	@for pin in $(GIT_PINS); do scripts/git.sh $$pin >/dev/null || exit 1; done

# GIT_ENV puts the node pin first on PATH, so every pool and every client a case
# starts runs it, and names the two floors' builds where the floors' arm finds
# them (gittest.NodeFloor and gittest.ClientFloor read CREWLET_TEST_GIT_NODE_FLOOR
# and CREWLET_TEST_GIT_CLIENT_FLOOR: every pool of host's
# TestTheGitFloorsPassTheSuite on the node floor, gitproc.NodeMinVersion, and
# every person's git on the client floor, gitproc.ClientMinVersion) —
# REQUIRED there (CREWLET_REQUIRE_GIT_FLOOR=1), so a run whose floors are
# missing fails naming them rather than skipping the arm, which only a bare
# `go test` outside make does. STATEMENTS rather than a prefix of assignments:
# a prefix `PATH="$(scripts/git.sh node):$PATH" go test …` whose command
# failed runs the suite on the machine's own git with nothing said, which is
# the one outcome this group exists to rule out. A recipe line starts with it,
# and what follows it runs with the pin on PATH and the floors named.
GIT_ENV = node_git="$$(scripts/git.sh node)" || exit 1; \
	node_floor="$$(scripts/git.sh node-floor)" || exit 1; \
	client_floor="$$(scripts/git.sh client-floor)" || exit 1; \
	export PATH="$$node_git:$$PATH" CREWLET_TEST_GIT_NODE_FLOOR="$$node_floor/git" \
	  CREWLET_TEST_GIT_CLIENT_FLOOR="$$client_floor/git" CREWLET_REQUIRE_GIT_FLOOR=1;

# gitproc's own suite on the pinned gits, alone: what ci.yml's `git recipe
# (macOS)` job runs, so the recipe's macOS branch (shasum, sysctl's core count,
# the SDK's libcurl) is built on every pull request and the gits it builds run
# gitproc's floor judgement, a real pool and the floors' own gits. On Linux
# `make test` runs these packages with the rest, so `check` does not depend on
# it.
test-gitproc: git ## build the pinned gits and run gitproc's own suite on them (ci: git recipe (macOS))
	$(GIT_ENV) $(SKIPGATE) -- $(GOTEST) -json ./internal/code/gitproc/...

##@ Gates — `make check` is all of them

# THE TWO TEST HALVES RUN IN THE RECIPE, one after the other, and everything
# else stays a prerequisite — which is about correctness rather than tidiness.
# As prerequisites they were independent, so `make -j check` was free to start
# both race suites at once — putting the multi-member cluster packages back on
# the machine beside the whole parallel partition, which is exactly the
# contention the split exists to remove, on the invocation a contributor
# reaches for to go faster. CI runs the halves on separate runners and so was
# never exposed; this gives the local gate the same isolation.
#
# A bare `.NOTPARALLEL:` fixes it and costs too much: it is GLOBAL, so every
# other parallel invocation of this file loses its concurrency to settle a
# collision between two targets. Prerequisites on .NOTPARALLEL would scope it,
# but that is GNU Make 4.4 and this repository pins no version (4.3 here).
# Two lines in the recipe are portable, and they say the thing plainly.
#
# It also fails faster: formatting, vet, lint and the build are all ahead of
# the two test halves, which are by far the longest part of this gate, rather
# than beside them.
#
# Measured, by doing it accidentally: `make test-solo` with a `make
# test-cross` running beside it failed internal/e2e's
# TestAFleetOfThree/EveryNodeMintsIntoOneKeySpace (then a test of its own)
# with `ensure stream CREWLET_NOTIFICATIONS: context deadline exceeded`, and
# passed alone.
check: fmt-check tidy-check signoff-check signoff-test git-test vet lint build test-cross dashboard-lint dashboard-check dashboard-test ## every gate CI runs on a PR
	@$(MAKE) test
	@$(MAKE) test-solo
	@echo
	@echo "All local gates passed. One thing this did NOT cover, because it"
	@echo "needs a service CI starts for itself:"
	@echo "  - the release pipeline  ->  make snapshot"

fmt-check: ## fail if anything needs gofmt (ci: build + vet)
	@files="$$($(GOFILES))" || exit 1; \
	unformatted="$$($(GOFMT) -l $$files)" || exit 1; \
	if [ -n "$$unformatted" ]; then \
	  { echo "gofmt needed:"; \
	    echo "$$unformatted"; \
	    echo "run: make fmt"; } >&2; \
	  exit 1; \
	fi

# `go mod tidy` has to be a no-op before a push, and only half of that is
# covered elsewhere. An UNDER-tidy module is caught for free by anything that
# compiles: a missing requirement or go.sum entry stops `go build ./...`. The
# opposite direction is caught by nothing -- a `require` left behind when its
# last import was deleted, a stale go.sum line or a wrong `// indirect` marker
# build green, test green and cross-compile green, then land as unrelated
# churn in whichever pull request next runs `make tidy`.
#
# -diff rather than `tidy` followed by `git diff`: it prints the patch and
# exits non-zero WITHOUT writing the files, so a gate never rewrites the tree
# it is judging -- and `make check` stays safe to run on a dirty checkout.
# `make tidy` is what applies what this prints.
tidy-check: ## fail if go.mod / go.sum are not tidy (ci: build + vet)
	@$(GO) mod tidy -diff || { \
	  { echo "go.mod / go.sum are not tidy: the diff above is what"; \
	    echo "'go mod tidy' would write."; \
	    echo "run: make tidy"; } >&2; \
	  exit 1; \
	}

# The DCO gate: every commit carries a Signed-off-by trailer (CONTRIBUTING.md,
# "Sign your work"). This is the only gate here whose subject is git history
# rather than the working tree, and that is why it runs locally at all rather
# than only in CI: a missing trailer is not repaired by editing a file, it is
# repaired by REWRITING the commits (`git rebase --signoff`), which costs
# nothing before a push and forces a force-push once a review has started.
#
# The range is origin/main..HEAD here; ci.yml passes the pull request's own
# base and head instead, because a fork's origin is the fork.
signoff-check: ## fail if a commit on this branch is not signed off (ci: sign-off)
	@scripts/check-signoff.sh

# The sign-off gate's OWN suite. It is bash reading git history, so no Go test
# reaches it, and it regresses silently in both directions -- a gate that stops
# failing reports a DCO certification nobody made, and one that starts failing
# wedges every pull request. Both happened while it was being written, which is
# why the suite exists and why it mutation-tests its own assertions.
signoff-test: ## run the sign-off gate's own suite (ci: sign-off)
	@scripts/check-signoff_test.sh

# The OWN SUITE of the git recipe (scripts/git.sh) and of the weekly pin check
# (scripts/git-pin-check.sh, which .github/workflows/git-pin.yml runs). Both are
# shell no Go test reaches, and each fails silently in the direction that
# matters: a recipe that installs over a git another checkout's suite is
# running, a check that calls every pin current because it read nothing, or
# that verifies a signature without asking whose it is. The suite runs the real
# scripts against local stand-ins — a source tarball, a release listing,
# signing keys of its own — so it builds no git and reads no network, and runs
# itself once more, by a relative path, under a developer's most hostile
# environment (the scripts' knobs, a GnuPG home, a curl configuration, a
# CDPATH naming another scripts/) to hold that it reads none of it:
# about twelve seconds, half of them that second run. It needs make, tar, xz,
# curl, sha256sum (or shasum) and GnuPG.
git-test: ## run the git recipe's and the pin check's own suite (ci: build + vet)
	@scripts/git-pin_test.sh

vet: ## run go vet (ci: build + vet)
	$(GO) vet ./...

# No version is pinned here on purpose. ci.yml runs golangci-lint-action at
# `latest`, so pinning one in this file would be a second answer to "which
# linter does this repository use" — and a tool version in a Makefile is a
# dependency surface Dependabot does not watch (.github/dependabot.yml).
#
# What IS checked is that the linter can run at all. golangci-lint refuses a
# module whose Go LANGUAGE version — the `go` line's major.minor — is newer
# than the language of the Go it was itself built with, and says so as a
# config-load error that names neither the fix nor the cause. The language and
# not the patch, because that is golangci-lint's own rule (CheckGoVersion in
# its pkg/goutil, which trims both sides to major.minor), and comparing patches
# here refused the very linter CI runs: the prebuilt releases trail Go's patch
# releases (v2.14.0, the newest when go.mod moved to 1.27.2, is built with
# go1.27.0 and lints this module), so `make check` could pass nowhere. The
# trap is that the obvious way to get "the latest" — `go install ...@latest` —
# builds it with the LINTER MODULE's own minimum Go, which is behind ours, so
# it produces a binary that lints nothing. Only the prebuilt releases are built
# with a current Go. Checked here rather than left to that error message,
# because the whole point of this target is to be the gate CI runs: one that
# cannot run is worse than one that fails, since a contributor who reads
# "can't load config" concludes the config is broken.
lint: ## run golangci-lint (ci: golangci-lint)
	@command -v golangci-lint >/dev/null 2>&1 || { \
	  echo "golangci-lint is not on PATH."; \
	  echo "  Install a PREBUILT RELEASE from"; \
	  echo "  https://golangci-lint.run/welcome/install/ — ci.yml runs whatever"; \
	  echo "  the latest release is, so no version is pinned here."; \
	  echo "  NOT 'go install ...@latest' — see below."; \
	  exit 1; \
	} >&2
	@built=$$(golangci-lint version 2>/dev/null | sed -n 's/.*built with go\([0-9][0-9.]*\).*/\1/p'); \
	need=$$(sed -n 's/^go \([0-9][0-9.]*\)$$/\1/p' go.mod); \
	built_lang=$$(printf '%s' "$$built" | cut -d. -f1,2); \
	need_lang=$$(printf '%s' "$$need" | cut -d. -f1,2); \
	if [ -n "$$built" ] && [ -n "$$need" ] && \
	   [ "$$(printf '%s\n%s\n' "$$need_lang" "$$built_lang" | sort -V | head -1)" != "$$need_lang" ]; then \
	  { echo "golangci-lint is built with go$$built, but go.mod targets $$need:"; \
	    echo "  language $$need_lang is newer than the linter's $$built_lang."; \
	    echo "  It will refuse this module rather than lint it, so this gate"; \
	    echo "  would report nothing at all."; \
	    echo "  Install a PREBUILT RELEASE from"; \
	    echo "  https://golangci-lint.run/welcome/install/ — those are built with"; \
	    echo "  a current Go. 'go install ...@latest' is NOT enough: it builds"; \
	    echo "  the linter with the linter module's own minimum Go, which is"; \
	    echo "  older than $$need_lang."; \
	    exit 1; \
	  } >&2; \
	fi
	golangci-lint run

# No require-node: the ONLY consumer of `node` in the Go suite is
# internal/e2e's dashboard replay (golden_test.go), which is a solo package and
# therefore not in this half. The prerequisite was here anyway, which made this
# target stricter than the CI job it mirrors — ci.yml's `test (race)` installs
# no node and passes — and a Makefile stricter than CI is the same lie as one
# looser than it, just in the direction nobody notices.
test: git ## the suite, minus the packages that run alone (ci: test (race))
	@test -n "$(PARALLEL_PKGS)" || { echo "the parallel partition printed no packages; internal/solo/partition said why above (a TEST_WEIGHTS file it refused, or go list failing)" >&2; exit 1; }
	$(GIT_ENV) $(SKIPGATE)$(if $(TEST_TIMINGS), -timings $(TEST_TIMINGS)) -- $(GOTEST) -json $(PARALLEL_PKGS)

# The solo half: every package that needs the runner to itself.
#
# -p 1 is not decoration. `go test pkgA pkgB …` runs package BINARIES at
# -p=GOMAXPROCS, so handing it the packages that each stand up a multi-member
# broker recreates precisely the contention this partition exists to remove.
# The old target ran one package and did not need it.
#
# BUT -p 1 IS FOR THE RUNS, and the compile it serialised with them was a side
# effect: cmd/go runs compile, link and test actions on ONE pool of -p workers,
# so every package in the half's dependency tree compiled one at a time before
# the first test started. On CI that was 266s of a 992s step running no test
# at all. So the half is compiled FIRST at the default -p by PREBUILD, which
# starts no test binary and so forms no cluster, and the -p 1 run finds every
# compile in the build cache. Every other run at -p 1 (`bench`) is prebuilt
# the same way. Measured from a cold cache (4 vCPUs, the machine shared): 423s
# serial, against 144s for the prebuild plus 19s for a -p 1 run left with
# seven links and the seven generated test mains, which is all it compiles.
#
# `go list -export` rather than `go test -c -o /dev/null`, which leaves the run
# exactly the same work but LINKS every binary in the prebuild too — and its
# links are not even reusable, since -c keeps debug information the run's do
# not. GOTESTBUILD is shared with the run so the two compile the same action
# IDs; a flag in one and not the other would prebuild a tree nothing reads.
#
# -e, BECAUSE THE PREBUILD IS A CACHE FILL AND NEVER A GATE. Without it one
# package whose tests do not compile fails the whole prebuild, and the recipe
# stops there: no package of the half runs, and skipgate names nothing, so the
# one broken package hides the result of every other. With it the broken
# package is recorded and left, everything else is compiled, and the run below
# reports `FAIL pkg [build failed]` for that one package and runs the rest, as
# it did before the prebuild existed. Measured on a probe with a type error in
# one of two test packages: without -e the prebuild exited 1 and make stopped
# before any test ran; with it the prebuild exited 0, and the run compiled only
# the probe package (and failed it), plus the other's test main and link. The
# flag changes how a load error is REPORTED and nothing about what is
# compiled, so the action IDs are the run's either way.
#
# $(1) is the flags that change what is compiled — the run's own, so the two
# share action IDs — and $(2) the packages.
PREBUILD = $(GO) list -e -export -test -deps $(1) -f '{{.ImportPath}}' $(2) > /dev/null

test-solo: require-node git ## the packages that need a runner to themselves (ci: end-to-end gates)
	@test -n "$(SOLO_PKGS)" || { echo "the solo partition printed no packages; internal/solo/partition said why above (go list failing, or no package importing internal/solo)" >&2; exit 1; }
	$(call PREBUILD,$(GOTESTBUILD),$(SOLO_PKGS))
	$(GIT_ENV) $(SKIPGATE) -- $(GOTEST) -json -p 1 $(SOLO_PKGS)

# The suite without the detector. It is roughly twice as fast and it is NOT
# what CI runs: a data race it cannot see is a data race that lands.
#
# It ran `./...` — the whole tree, solo packages included, in one contended
# run. The documented "faster loop" was the exact arrangement the partition
# exists to avoid, so it flaked for the reason the split was measured on and
# looked like an unstable suite rather than a mis-stated target.
#
# BOTH HALVES RUN, and the status accumulates, for the reason test-cross states
# below: as two separate recipe lines make stops at the first nonzero one, so a
# failure in the parallel partition meant the solo partition — internal/e2e and
# every cluster-forming package — was never run at all by a target documented
# as the full suite. The gates cannot hit this (`check` invokes the two as
# sub-makes, each of which must succeed), which is exactly why it went
# unnoticed here: the escape hatch is the one place a partial run reports as a
# whole one.
#
# The same flags as the gates bar -race (GOTESTRUN), and the same prebuild for
# the solo half, compiled without it.
test-norace: require-node git ## the full suite without -race (faster; not a gate)
	@$(GIT_ENV) status=0; \
	echo "==> parallel partition"; \
	$(GO) test $(GOTESTRUN) $(PARALLEL_PKGS) || status=1; \
	echo "==> solo partition"; \
	{ $(call PREBUILD,,$(SOLO_PKGS)) && $(GO) test $(GOTESTRUN) -p 1 $(SOLO_PKGS); } || status=1; \
	exit $$status

# Every target reports in one run rather than stopping at the first failure —
# ci.yml sets `fail-fast: false` on this matrix for the same reason: when a
# build constraint breaks one platform you want to see which, not the first.
#
# Compiling is the whole test. A darwin binary cannot be run here, and what
# breaks a cross-target is almost always a build tag rather than behaviour —
# internal/store/platform.go, for one, is a compile error by construction.
#
# This replaced `test-stores`, which certified the store on two drivers. There
# is one now, and the slot it left is worth more here: the
# release matrix was the thing nothing checked, and windows/arm64 shipped
# broken for exactly that reason.
test-cross: ## cross-compile every release target (ci: cross-compile the release targets)
	@status=0; \
	for target in $(CROSS_TARGETS); do \
	  echo "==> build $$target"; \
	  CGO_ENABLED=0 GOOS=$${target%/*} GOARCH=$${target#*/} \
	    $(GO) build ./... || status=1; \
	done; \
	exit $$status

# internal/e2e replays a real company's socket frames through the dashboard's
# own protocol module under plain `node`, and it SKIPS without one — so a green
# run with no node has left the client's half of the wire protocol unchecked.
# ci.yml installs node rather than tolerating that; here, saying so is the best
# we can do.
require-npm: ## fail unless npm is on PATH (every dashboard target needs it)
	@command -v npm >/dev/null 2>&1 || { \
	  echo "npm is not on PATH, and the dashboard is built with it."; \
	  echo "  static/dashboard is COMMITTED, so building the engine needs"; \
	  echo "  neither node nor npm — but changing dashboard/ does. Install a"; \
	  echo "  current node (npm ships with it) and re-run."; \
	  exit 1; \
	} >&2

require-node: ## fail unless node is on PATH (the e2e client replay needs it)
	@command -v node >/dev/null 2>&1 || { \
	  echo "node is not on PATH, and internal/e2e's client replay needs it."; \
	  echo "  It runs the dashboard's own protocol module over the frames a"; \
	  echo "  real engine produced, and SKIPS without node — so the run would"; \
	  echo "  go green having checked neither half of the wire protocol."; \
	  echo "  Install any current node, as ci.yml does, and re-run."; \
	  exit 1; \
	} >&2

##@ Benchmarks — measurements, never a gate

# THE BENCHMARKS THE DESIGN'S CAPACITY FIGURES COME FROM, so each figure is
# measured rather than derived: the object store on every backend
# (objstoretest.Bench, the broker's bucket at three replicas among them, the
# SHA-256 floor under every verified byte, and what one throttle costs a burst
# of requests to an S3 bucket), an arbitrated write end to end
# and the applier's drain (internal/statelog), the store's own drain
# (internal/store), what a stream's leader takes to list the subjects it
# holds, a page at a time (internal/jsapi), the company's token counter
# charged by many nodes at once, each a leaf of its own, a gated lease claim
# and what a coding box's request costs its run's record on three members
# (internal/coord/kv), a git process through the pool end to end,
# receive-pack's index-pack at one thread, and a person's clone on a transfer
# lane one CI token has filled, with the fair share and without it, and beside
# coding boxes with their cap and without it, and one
# grant of a lane ten thousand waiters deep (internal/code/gitproc), the git
# cache's cold and delta syncs and a bundle's own file work, its index
# maintenance and what a request pays across many packs, a node's boot of its
# cache and what a directory sync costs a file (internal/code/cache), the code
# domain's applier, decide, reconcile and snapshot reads and what a push's
# history costs every data node's disk (internal/code), the code host's
# commit chain on one and three members and an engine write's route between
# them (internal/code/codecluster), a push, a warm fetch, cold and warm
# clones, a blobless clone's lazy blame, and a seat's engine write from its
# tool's call to its pack adopted and its build alone, end to end, and a
# coding box's two clone recipes, its own git in a wide work tree and a copy
# borrowed from the node (internal/code/host), a seat's reads of a
# repository by object id on a git node, warm and cold (internal/code/query), the
# pace a client's connection is held to, on a real connection
# (internal/api/paced), and what every push pays to start its proc-receive
# hook (cmd/crewlet). Each package's doc records what it measured, and on what
# machine.
#
# NOT A GATE, AND NO CI JOB RUNS IT: a benchmark on a shared runner measures
# the runner's neighbours, and a gate on figures that noisy is one somebody
# turns off. Compare two trees on one idle machine instead — BENCH_COUNT=6 on
# each, then benchstat (golang.org/x/perf/cmd/benchstat).
#
# -p 1: one package's binary at a time. Two measuring at once measure each
# other, and internal/statelog, internal/objstore/natsobj/replicated and
# internal/code/codecluster stand up three-member clusters, which is what
# internal/solo exists to keep alone.
# Compiled first at full -p by PREBUILD, for test-solo's reason.
# -run '^$': -bench alone runs every test as well.
# No -race: the detector multiplies costs unevenly, so a figure taken under it
# describes the detector.
# -v: a SKIPPED benchmark prints nothing without it. The real-bucket arms skip
# unless CREWLET_BENCH_S3_BUCKET names a bucket (internal/objstore/s3obj's
# package doc), and the arms whose fixture takes minutes and gigabytes —
# internal/code/cache's cold syncs of 2 048 and of 10 000 pushes, its
# 10 000-pack repositories, its 5 GB index and its published layers, its
# gigabyte split into hundreds of packs, its installer catching up 10 000
# repositories and BenchmarkBigNode's 5 GB monorepo,
# internal/code/host's gigabyte history, hundred thousand refs,
# 100 000-file tree and a box's million-file one, internal/code/query's
# gigabyte history under a 100 000-file tree read by object id and its
# gigabyte history blamed and logged by path, and internal/jsapi's streams of a
# million and of three million subjects (about five minutes a count on four
# cores) — unless CREWLET_BENCH_LARGE=1; an arm that silently did not run
# reads as one that measured nothing worth showing. BenchmarkBigNode's 12 GB
# monorepo also needs CREWLET_BENCH_BIGNODE=1 and a machine of at least 64 GB:
# its fixture builds for hours, so it runs alone, under the day's timeout
# CREWLET_BENCH_LARGE=1 gives a run (BENCH_TIMEOUT below). Not through
# skipgate, for the same reason: an allowance there would make that skip
# silent again. The code host's pools and every client run the node pin, which
# GIT_ENV puts first on PATH as it does for every test target, so a figure is
# of the git the engine image builds and never of whichever one the machine
# had: a package doc quoting it names that version.
# BENCH_TIME 3s: the 128 MiB cases take most of a second an operation, so at
# go's default of one second they are one or two operations whose first-touch
# page faults are the figure; at three they are several.
# BENCH_TIMEOUT is a hang detector, as TEST_TIMEOUT is. The longest package
# is internal/code/cache, whose fixtures are real git: at BENCH_COUNT=6 on
# four vCPUs its arms took an hour and three quarters, half an hour of it
# BenchmarkBoot's thousand repositories, and BenchmarkMidx's memory arms add
# about five minutes a count (the gigabyte's clone walking its history a
# minute and a half of it); with CREWLET_BENCH_LARGE=1 about six and a half
# hours, two of them the gigabyte split into packs, whose clones with no index
# take minutes each, and an hour BenchmarkColdSync's cold syncs of the pack
# bound's 2 048 pushes and of an uncompacted 10 000, from memory and from the
# S3 fake at 30, 100 and 200 ms (seven and a half minutes of syncs a count,
# and a quarter of an hour of fixtures once). The next is
# internal/code/host: fifty minutes at six counts, and with
# CREWLET_BENCH_LARGE=1 about two and a half hours — half an hour of it
# BenchmarkPushSmall's gigabyte history and an hour BenchmarkCloneCold's
# 2 000-push tree; its coding boxes' (BenchmarkBoxClone, BenchmarkBoxWorktree,
# BenchmarkBoxDissociate) took twelve minutes more at one count of ten
# operations on two E-cores, BenchmarkBoxWorktree's eleven sessions of an
# agent's commands about twelve more at one, and their gigabyte arms twenty at one, the
# million-file tree's unmeasured (BENCH_TIME=10x, or 1x, each figure says
# which); its engine writes' two benchmarks, BenchmarkEngineWrite and
# BenchmarkEngineCommit, took nine to eleven minutes at BENCH_TIME=10x on two
# cores, the 100 000-file tree and directory included, and at the default
# three seconds take several rounds an arm more. A fixture is built once a
# run whatever BENCH_COUNT says,
# since go test repeats only a leaf benchmark a count: the gigabyte's is ten
# minutes and its arms four a count, the tree's eleven and its clones eight.
# internal/code/cache's published layers' arms came after those runs, and add
# — an ESTIMATE from each arm's one bounded count on two cores, never a run of
# six — about an hour and a half at six counts (12 minutes a count, a quarter
# of an hour of fixtures once), and with CREWLET_BENCH_LARGE=1 about five and
# a half more: their 5 GB arms (some 25 minutes a count, a whole-history walk
# growing sevenfold from a gigabyte to 5 GB as git 2.43's did),
# BenchmarkBigNode's 5 GB monorepo and the installer's 10 000-repository
# catch-up (a quarter of an hour a count, most of it the 20 000 syncs that
# set each count up) — about thirteen hours for the package at six counts.
# The timeout is each package's, as -p 1 runs them in turn. Eight hours holds
# six counts of everything a default run measures with room for a slower
# machine, and a day six counts of the large arms too; a real bucket's arm is
# bounded by its link.
BENCH_PKGS    ?= ./internal/objstore ./internal/objstore/memobj \
                 ./internal/objstore/natsobj ./internal/objstore/natsobj/replicated \
                 ./internal/objstore/s3obj ./internal/statelog ./internal/store \
                 ./internal/jsapi ./internal/coord/kv ./internal/code/gitproc \
                 ./internal/code/cache ./internal/code/compact ./internal/code \
                 ./internal/code/codecluster ./internal/code/host ./internal/code/query \
                 ./internal/usage ./internal/api/paced ./cmd/crewlet
BENCH_PATTERN ?= .
BENCH_TIME    ?= 3s
BENCH_COUNT   ?= 1
BENCH_TIMEOUT ?= $(if $(filter 1,$(CREWLET_BENCH_LARGE)),24h,8h)

bench: git ## measure the object store, the state log's write path, a stream leader's subject listing, the fleet's token counter, git, its cache, a home's compaction of a repository's packs, the code host's pushes, clones and engine writes, a coding box's git and its run's record, a seat's reads by object id, a node's day of usage and who read a repository, a client's pace and the push hook's start (not a gate)
	$(call PREBUILD,,$(BENCH_PKGS))
	$(GIT_ENV) $(GO) test -run '^$$' -bench '$(BENCH_PATTERN)' -benchmem \
	  -benchtime $(BENCH_TIME) -count $(BENCH_COUNT) -timeout $(BENCH_TIMEOUT) \
	  -p 1 -v $(BENCH_PKGS)

##@ Local vendor loops

mattermost-up: ## start Mattermost and bootstrap it (profile: mattermost)
	$(COMPOSE) --profile mattermost up -d
	COMPANY=$(COMPANY) scripts/mattermost-dev-bootstrap.sh

mattermost-down: ## stop the Mattermost stack
	$(COMPOSE) --profile mattermost down

gitlab-up: ## start GitLab and bootstrap it (profile: gitlab)
	$(COMPOSE) --profile gitlab up -d
	COMPANY=$(COMPANY) scripts/gitlab-dev-bootstrap.sh

gitlab-down: ## stop the GitLab stack
	$(COMPOSE) --profile gitlab down

##@ Generated files and the release rehearsal

# schema/*.schema.json are generated and never hand-edited —
# cmd/crewlet/schema_test.go regenerates them and compares, so a config field
# added without running this is a failing test rather than a stale file.
schema: ## regenerate schema/*.schema.json from the config models
	$(GO) run ./cmd/crewlet schema bootstrap -o schema/bootstrap.schema.json
	$(GO) run ./cmd/crewlet schema company -o schema/company.schema.json

# docs/reference/metrics.md is generated from the instrument catalogue for the
# same reason and with the same guard — internal/statelog/metrics regenerates
# it and compares, so an instrument added without running this is a failing
# test rather than a reference an operator cannot find their metric in.
metrics-doc: ## regenerate docs/reference/metrics.md from the instrument catalogue
	$(GO) run ./internal/statelog/metrics/gen > docs/reference/metrics.md

# docs/reference/alarms.md is generated from the alarm table, and diffed by
# internal/statelog for the same reason: an operator meets an alarm for the
# first time in a log line at an inconvenient hour, and the page is where they
# look it up.
alarms-doc: ## regenerate docs/reference/alarms.md from the alarm table
	$(GO) run ./internal/statelog/alarmgen > docs/reference/alarms.md

# internal/config/testdata/derived/*.derived.json are generated and never
# hand-edited, and they are the only place several org-model conclusions are
# pinned at all — a seat's derived handle, its primary manager, a unit's
# effective lead. internal/config regenerates them and compares, so a change
# in internal/org that moves a manager is a failing test rather than a chart
# that quietly redraws.
#
# A TARGET rather than the bare environment variable, because the variable is
# how the goldens get rewritten and there was nothing to discover it from: the
# name appeared once in the tree, in the test that reads it. `-count=1`
# because a cached PASS never runs the body, so without it a regeneration can
# report success and write nothing.
#
# Read the diff before committing it. Accepting one blind is the whole failure
# mode of a golden, and this target makes the rewrite one word long.
derived: ## regenerate internal/config/testdata/derived/*.derived.json from the config models
	CREWLET_REGENERATE_DERIVED=1 $(GO) test ./internal/config -count=1 \
	  -run TestTheDerivedHierarchyMatchesItsGoldenFiles

# internal/api/testdata/gate_answer.json is the eviction and readmission
# routes' answer — every 200 shape and every refusal — rendered by the route's
# own renderer and never hand-edited. internal/api regenerates it and compares,
# and the dashboard's gate dialog suite loads the SAME file as its fixture, so a
# change to the rendering is a failing Go test until this runs and a failing
# Vitest suite until the dialog follows it. The dialog once read a top-level
# outcome for as long as the route had stopped writing one, on fixtures it had
# typed itself. Read the diff before committing it, as with `derived`.
gate-answer: ## regenerate internal/api/testdata/gate_answer.json from the gate routes' renderer
	CREWLET_REGENERATE_GATE_ANSWER=1 $(GO) test ./internal/api -count=1 \
	  -run TestTheGateAnswerMatchesItsGoldenFile

# internal/api/testdata/objects_answer.json is the object store's rendering —
# the `objects` block GET /fleet answers (the collector's last collection and
# audit) in each of its states — written by the renderer /fleet answers
# through, for `gate-answer`'s reason: the file-storage card's suite on the
# dashboard's Settings › Nodes screen loads the SAME file as its fixture, and
# docs/reference/api-endpoints.md's GET /fleet example is held to it, so a
# change to the rendering fails on every side until each follows it. Read the
# diff before committing it.
objects-answer: ## regenerate internal/api/testdata/objects_answer.json from the object store's renderers
	CREWLET_REGENERATE_OBJECTS_ANSWER=1 $(GO) test ./internal/api -count=1 \
	  -run TestTheObjectsAnswerMatchesItsGoldenFile

broker-answer: ## regenerate internal/api/testdata/fleet_broker_answer.json from the engine's broker view
	CREWLET_REGENERATE_BROKER_ANSWER=1 $(GO) test ./internal/api -count=1 \
	  -run TestTheBrokerAnswerFixtureIsTheEnginesOwn

# The whole release pipeline, without a tag and without touching GitHub —
# the same two commands release.yml's snapshot job runs, in the same order.
#
# `check` first so a config error is reported as a config error rather than
# as whatever the build does with a bad field. `--skip=sign` because signing
# is NOT part of the publish phase that `--snapshot` already skips: without
# it, a rehearsal runs `cosign sign-blob --yes` over the checksums and tries
# to mint a real Sigstore certificate from a workstation that has no OIDC
# token to mint it from — a browser prompt at best, a genuine signature over
# a throwaway build at worst. The image IS part of the rehearsal: buildx
# cannot assemble a multi-platform manifest without pushing, so dockers_v2
# runs in the publish phase and `--skip=docker` would leave the Dockerfile
# untested. See RELEASING.md, which also covers the binfmt registration the
# linux/arm64 image needs on an amd64 host.
snapshot: ## rehearse a release locally (needs goreleaser and docker)
	@command -v goreleaser >/dev/null 2>&1 || { \
	  echo "goreleaser is not on PATH."; \
	  echo "  Install it from https://goreleaser.com/install/ — RELEASING.md"; \
	  echo "  covers what this rehearses and what it cannot."; \
	  exit 1; \
	} >&2
	goreleaser check
	goreleaser release --snapshot --clean --skip=sign

# THE BUILD CONTEXT OF THE IMAGE OF MAIN, which .github/workflows/main-image.yml
# pushes as ghcr.io/crewlet/crewlet:main. A release's image is goreleaser's
# (`snapshot` above rehearses it); this one is stamped with a version no tag
# carries, which neither way goreleaser runs can do, so the build is written
# out here with .goreleaser.yaml's settings (CGO_ENABLED=0, -trimpath, -s -w,
# the -X stamp) and staged in the layout its dockers_v2 stages: each binary
# under its platform, beside the license, both notices files and the git pin
# and recipe (scripts/git-pin.env, scripts/git.sh) the Dockerfile's git stage
# builds from — the files .goreleaser.yaml's extra_files names. A change to the
# release build is a change to both files.
#
# VERSION is required, because an unstamped binary reports a module version:
# a name for a build nobody made. And the binary this host can run is run and
# must report it, because a -X flag that names nothing still links and that is
# its only symptom.
IMAGE_CONTEXT ?= build/image
IMAGE_PLATFORMS := linux/amd64 linux/arm64

image-context: ## stage the engine image's build context, stamped VERSION (ci: main image)
	@test -n "$(VERSION)" || { \
	  echo "VERSION is required, e.g. make image-context VERSION=main-dev"; exit 1; } >&2
	@for platform in $(IMAGE_PLATFORMS); do \
	  echo "==> build $$platform"; \
	  CGO_ENABLED=0 GOOS=$${platform%/*} GOARCH=$${platform#*/} $(GO) build -trimpath \
	    -ldflags "-s -w -X github.com/crewlet/crewlet/internal/version.value=$(VERSION)" \
	    -o $(IMAGE_CONTEXT)/$$platform/crewlet ./cmd/crewlet || exit 1; \
	done
	$(GO) run ./scripts/notices -out $(IMAGE_CONTEXT)/build/notices/THIRD_PARTY_NOTICES.txt \
	  -main ./cmd/crewlet -targets $$(echo $(IMAGE_PLATFORMS) | tr ' ' ',')
	mkdir -p $(IMAGE_CONTEXT)/static/dashboard $(IMAGE_CONTEXT)/scripts
	cp LICENSE $(IMAGE_CONTEXT)/LICENSE
	cp static/dashboard/THIRD_PARTY_NOTICES.txt $(IMAGE_CONTEXT)/static/dashboard/
	cp scripts/git-pin.env scripts/git.sh $(IMAGE_CONTEXT)/scripts/
	@host=$$($(GO) env GOHOSTOS)/$$($(GO) env GOHOSTARCH); \
	case " $(IMAGE_PLATFORMS) " in \
	  *" $$host "*) \
	    reported=$$($(IMAGE_CONTEXT)/$$host/crewlet --version | awk '{print $$2}'); \
	    if [ "$$reported" != "$(VERSION)" ]; then \
	      echo "the $$host binary reports $$reported, not $(VERSION)" >&2; exit 1; \
	    fi; \
	    echo "==> the $$host binary reports $(VERSION)";; \
	  *) echo "==> NOT CHECKED: this $$host host runs none of $(IMAGE_PLATFORMS), so no binary's version was read";; \
	esac

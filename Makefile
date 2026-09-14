# The developer's entry point to the gates CI runs.
#
# Every target below runs the SAME command .github/workflows/ci.yml runs, with
# the same flags, so `make check` before a push fails wherever CI would fail
# and passes only where CI would pass. A convenience target that quietly drops
# `-race`, or runs the store suite on one driver, is worse than no target at
# all: it reports a pass CI will not honour, and the divergence is invisible
# until the pull request goes red.
#
# For the two TEST jobs that is now true by construction rather than by care:
# ci.yml's `test (race)` and `end-to-end gates` steps are `make test` and
# `make test-solo`, so there is one command and no copy to keep in step. Every
# other job still inlines its own, and NOTHING asserts those agree --
# internal/version/makefile_test.go used to and was dropped -- so for the rest,
# change a target and its ci.yml step in the same commit, and read both.
#
# The second thing this file is for is the parts that need something the
# machine may not have. The dashboard is a React + TypeScript application built
# by Vite, so its suites and its build need node AND npm; the targets that need
# them FAIL saying what to install, which is the same posture CI takes
# (CONTRIBUTING.md, "A skip is not a pass"). Every queue backend certifies
# itself with no external service: the JetStream suite starts an embedded
# broker per test.
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
# failing on a version mismatch — the value ci.yml sets for every job.
export GOTOOLCHAIN ?= auto

GO ?= go

# Exported so the partition command and internal/solo's roster guard discover
# packages with the SAME toolchain this file runs the tests with. Both shell
# out to `go list`, and a hardcoded `go` there would resolve through PATH —
# a different toolchain than `make GO=/path/to/go` selected, or none at all.
export CREWLET_GO = $(GO)

# ./crewlet is where `go build ./cmd/crewlet` drops the binary, and — with
# goreleaser's dist/ — the only build output .gitignore already knows about.
# Nothing here writes anywhere else, so `make clean` has nothing to guess at.
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
# per test, and measured 776s here under -race. Left at the default the gate
# does not merely flap — it cannot pass on a machine this speed, and the
# failure reads as a hung test rather than as a budget nobody set.
#
# Thirty minutes is 2.3x the measured run, which is enough for a runner half
# this fast, and still kills a real deadlock twelve times sooner than the CI
# job's own limit. It applies to every package because the flag is per test
# BINARY: a unit package that hangs now dies in thirty minutes rather than ten,
# which is the cost of having a gate that can pass at all.
#
# TEST_TIMEOUT is ci.yml's value, and the two must not drift: the Makefile is
# the same command CI runs or it is a lie. It is defined BEFORE GOTEST, and
# that is load-bearing rather than tidy: `:=` expands immediately, so with the
# assignment below the reference the flag was handed an EMPTY value and `go
# test` parsed the package list as its argument — `invalid value "./..." for
# flag -timeout`. `make test` and therefore `make check` could not run at all.
TEST_TIMEOUT := 30m

GOTEST := $(GO) test -race -count=1 -timeout $(TEST_TIMEOUT)

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
SKIPGATE := $(GO) run ./internal/skipgate

# The two halves of the suite, COMPUTED rather than listed.
#
# NOT A COVERAGE CUT — `check` depends on both, and ci.yml runs both. It is a
# CONTENTION cut: a solo package stands up N engines, each embedding its own
# NATS server, in ONE process, and a two-core runner under the race detector
# cannot form a multi-member JetStream quorum inside the 30s provisioning
# budget while `./...` runs package binaries in parallel. `go doc ./internal/solo`
# is the whole story — the measurement, and what every obvious alternative
# (a build tag, -short, a flag, -skip, a nested module) cost when it was tried.
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
# but then re-runs per reference, and each of these is referenced twice below
# (a guard, then the recipe).
#
# So each expands once and redefines itself as a simple variable — `$(eval)`
# expands to nothing, and what is left is the value it just assigned. Every
# later reference is a plain lookup.
PARTITION      = $(GO) run ./internal/solo/partition
PARALLEL_PKGS  = $(eval PARALLEL_PKGS := $(shell $(PARTITION) parallel))$(PARALLEL_PKGS)
SOLO_PKGS      = $(eval SOLO_PKGS := $(shell $(PARTITION) solo))$(SOLO_PKGS)

# The release targets, cross-compiled. Nothing else builds for anything but
# the machine you are on, so a build tag or a platform-gated file that only
# breaks darwin reaches the tag — and a broken tag is a release to re-cut.
# ci.yml runs these as a matrix and the pairs are .goreleaser.yaml's. All
# three lists have to agree and nothing checks that they do, so a target
# added or dropped here belongs in the other two in the same commit.
CROSS_TARGETS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

COMPOSE ?= docker compose

# Passed through to the vendor bootstrap scripts, which provision the seats of
# the company config named here and skip that step when it is empty:
#   make mattermost-up COMPANY=examples/nimbus.company.yaml
COMPANY ?=

.DEFAULT_GOAL := help

.PHONY: help build crewlet install fmt tidy schema metrics-doc alarms-doc \
        dashboard dashboard-check dashboard-dev dashboard-test dashboard-lint \
        check fmt-check tidy-check signoff-check signoff-test vet lint test test-norace test-cross test-solo \
        require-npm \
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
	gofmt -w .

tidy: ## tidy go.mod / go.sum
	$(GO) mod tidy

# Build output only. The store file, its -wal/-shm siblings and the *-data/
# directories beside them are a checkout's own company state, not something
# this file produced, so removing them is the developer's call and not a
# side effect of cleaning a build.
clean: ## remove the build output (./crewlet and dist/)
	rm -f $(BIN)
	rm -rf dist
	rm -rf $(UI)/node_modules

##@ Dashboard

# The dashboard's source lives here; its BUILD OUTPUT is committed to
# static/dashboard, which package static embeds.
UI := dashboard

# npm ci rather than npm install: the lockfile is the pin, and `install` is
# allowed to move it. A build whose dependency versions drift is a build whose
# committed output cannot be reproduced, which is the whole thing
# dashboard-check depends on.
$(UI)/node_modules: $(UI)/package-lock.json | require-npm
	cd $(UI) && npm ci
	@touch $(UI)/node_modules

dashboard: $(UI)/node_modules ## build the dashboard into static/dashboard (commit the result)
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
# Rebuild, then diff. `git diff --exit-code` prints the drift and fails; it does
# not repair it, for the same reason `go mod tidy -diff` does not: a gate that
# rewrites the tree it is judging cannot be trusted about what was committed.
dashboard-check: $(UI)/node_modules ## fail if static/dashboard is not what dashboard/ builds (ci: dashboard)
	cd $(UI) && npm run build
	@git diff --exit-code -- static/dashboard || { \
	  { echo; \
	    echo "static/dashboard is not what dashboard/ builds."; \
	    echo "The diff above is what a rebuild produced."; \
	    echo "run: make dashboard && git add static/dashboard"; } >&2; \
	  exit 1; \
	}

##@ Gates — `make check` is all of them

# .NOTPARALLEL, and it is about correctness rather than tidiness: under
# `make -j check` GNU Make is free to start `test` and `test-solo` at the same
# time, which puts the multi-member cluster packages back on the machine
# alongside the whole parallel partition — precisely the contention the split
# exists to remove, on the one invocation a contributor reaches for to go
# faster. CI keeps them in separate jobs and so is unaffected; this is what
# gives the local gate the same isolation.
#
# THE TWO TEST HALVES RUN IN THE RECIPE, one after the other, and everything
# else stays a prerequisite. As prerequisites they were independent, so
# `make -j check` was free to start both race suites at once — putting the
# multi-member cluster packages back on the machine beside the whole parallel
# partition, which is exactly the contention the split exists to remove, on
# the invocation a contributor reaches for to go faster.
#
# A bare `.NOTPARALLEL:` fixes it and costs too much: it is GLOBAL, so every
# other parallel invocation of this file loses its concurrency to settle a
# collision between two targets. Prerequisites on .NOTPARALLEL would scope it,
# but that is GNU Make 4.4 and this repository pins no version (4.3 here).
# Two lines in the recipe are portable, and they say the thing plainly.
#
# It also fails faster: formatting, vet, lint and the build are all ahead of a
# six-minute test run rather than beside it.
#
# Measured, by doing it accidentally: `make test-solo` with a `make
# test-cross` running beside it failed internal/e2e's
# TestEveryNodeMintsIntoOneKeySpace with `ensure stream
# CREWLET_NOTIFICATIONS: context deadline exceeded`, and passed alone.
check: fmt-check tidy-check signoff-check signoff-test vet lint build test-cross dashboard-lint dashboard-check dashboard-test ## every gate CI runs on a PR
	@$(MAKE) test
	@$(MAKE) test-solo
	@echo
	@echo "All local gates passed. One thing this did NOT cover, because it"
	@echo "needs a service CI starts for itself:"
	@echo "  - the release pipeline  ->  make snapshot"

fmt-check: ## fail if anything needs gofmt (ci: build + vet)
	@unformatted="$$(gofmt -l .)"; \
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

vet: ## run go vet (ci: build + vet)
	$(GO) vet ./...

# No version is pinned here on purpose. ci.yml runs golangci-lint-action at
# `latest`, so pinning one in this file would be a second answer to "which
# linter does this repository use" — and a tool version in a Makefile is a
# dependency surface Dependabot does not watch (.github/dependabot.yml).
#
# What IS checked is that the linter can run at all. golangci-lint refuses a
# module whose `go` line is newer than the Go it was itself built with, and
# says so as a config-load error that names neither the fix nor the cause. The
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
	if [ -n "$$built" ] && [ -n "$$need" ] && \
	   [ "$$(printf '%s\n%s\n' "$$need" "$$built" | sort -V | head -1)" != "$$need" ]; then \
	  { echo "golangci-lint is built with go$$built, but go.mod targets $$need."; \
	    echo "  It will refuse this module rather than lint it, so this gate"; \
	    echo "  would report nothing at all."; \
	    echo "  Install a PREBUILT RELEASE from"; \
	    echo "  https://golangci-lint.run/welcome/install/ — those are built with"; \
	    echo "  a current Go. 'go install ...@latest' is NOT enough: it builds"; \
	    echo "  the linter with the linter module's own minimum Go, which is"; \
	    echo "  older than $$need."; \
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
test: ## the suite, minus the packages that run alone (ci: test (race))
	@test -n "$(PARALLEL_PKGS)" || { echo "the parallel partition is empty" >&2; exit 1; }
	$(SKIPGATE) -- $(GOTEST) -json $(PARALLEL_PKGS)

# The solo half: every package that needs the runner to itself.
#
# -p 1 is not decoration. `go test pkgA pkgB …` runs package BINARIES at
# -p=GOMAXPROCS, so handing it four packages that each stand up a multi-member
# broker recreates precisely the contention this partition exists to remove.
# The old target ran one package and did not need it.
test-solo: require-node ## the packages that need a runner to themselves (ci: end-to-end gates)
	@test -n "$(SOLO_PKGS)" || { echo "no package imports internal/solo" >&2; exit 1; }
	$(SKIPGATE) -- $(GOTEST) -json -p 1 $(SOLO_PKGS)

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
test-norace: require-node ## the full suite without -race (faster; not a gate)
	@status=0; \
	echo "==> parallel partition"; \
	$(GO) test -count=1 -timeout $(TEST_TIMEOUT) $(PARALLEL_PKGS) || status=1; \
	echo "==> solo partition"; \
	$(GO) test -count=1 -timeout $(TEST_TIMEOUT) -p 1 $(SOLO_PKGS) || status=1; \
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

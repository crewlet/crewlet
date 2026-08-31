# The developer's entry point to the gates CI runs.
#
# Every target below runs the SAME command .github/workflows/ci.yml runs, with
# the same flags, so `make check` before a push fails wherever CI would fail
# and passes only where CI would pass. A convenience target that quietly drops
# `-race`, or runs the store suite on one driver, is worse than no target at
# all: it reports a pass CI will not honour, and the divergence is invisible
# until the pull request goes red. Nothing asserts the two agree -- keep them
# in step by hand, and change both in the same commit.
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
GOTEST := $(GO) test -race -count=1

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

.PHONY: help build crewlet install fmt tidy schema \
        dashboard dashboard-check dashboard-dev dashboard-test dashboard-lint \
        check fmt-check tidy-check vet lint test test-norace test-cross test-e2e \
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

check: fmt-check tidy-check vet lint build test test-cross dashboard-check dashboard-test ## every gate CI runs on a PR
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

# This includes ./internal/e2e/... — the end-to-end gates are ordinary Go
# tests, so `make test-e2e` is the same suite again with -v, for when one of
# them is what you are debugging.
test: require-node ## the full suite under the race detector (ci: test (race))
	$(GOTEST) ./...

# The suite without the detector. It is roughly twice as fast and it is NOT
# what CI runs: a data race it cannot see is a data race that lands.
test-norace: require-node ## the full suite without -race (faster; not a gate)
	$(GO) test -count=1 ./...

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

test-e2e: require-node ## the end-to-end gates, verbose (ci: end-to-end gates)
	$(GOTEST) ./internal/e2e/... -v

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

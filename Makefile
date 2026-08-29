# The developer's entry point to the gates CI runs.
#
# Every target below runs the SAME command .github/workflows/ci.yml runs, with
# the same flags, so `make check` before a push fails wherever CI would fail
# and passes only where CI would pass. A convenience target that quietly drops
# `-race`, or runs the store suite on one driver, is worse than no target at
# all: it reports a pass CI will not honour, and the divergence is invisible
# until the pull request goes red. internal/version/makefile_test.go asserts
# the two have not drifted.
#
# The second thing this file is for is the suites that need something the
# machine may not have. `node` and a Pulsar broker are absent by default and
# the affected suites SKIP without them — silently, with the run still green.
# So the targets that need one FAIL saying what to install or start, which is
# the same posture CI takes (CONTRIBUTING.md, "A skip is not a pass").
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

# Both certified store drivers. Every statement in internal/store must parse
# on both, Turso is currently the narrower dialect, and a suite run on one
# certifies nothing about the other — ci.yml runs them as a matrix.
STORE_DRIVERS := turso sqlite

# Where `make pulsar-up` puts the broker, and therefore where the conformance
# suite looks. Overridable for a broker somewhere else:
#   make test-pulsar PULSAR_URL=pulsar://box:6650 \
#       PULSAR_ADMIN_URL=http://box:8080
PULSAR_URL ?= pulsar://localhost:6650
PULSAR_ADMIN_URL ?= http://localhost:8080

# A cold standalone broker provisions a namespace per subtest, and the suite
# waits on real redelivery timers. 25m is what ci.yml allows the same run.
PULSAR_TIMEOUT := 25m

COMPOSE ?= docker compose

# Passed through to the vendor bootstrap scripts, which provision the seats of
# the company config named here and skip that step when it is empty:
#   make mattermost-up COMPANY=examples/nimbus.company.yaml
COMPANY ?=

.DEFAULT_GOAL := help

.PHONY: help build crewlet install fmt tidy schema \
        check fmt-check vet lint test test-norace test-stores test-e2e \
        test-pulsar pulsar-up pulsar-down \
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

##@ Gates — `make check` is all of them

check: fmt-check vet lint build test test-stores ## every gate CI runs on a PR
	@echo
	@echo "All local gates passed. Two things this did NOT cover, because both"
	@echo "need a service CI starts for itself:"
	@echo "  - the Pulsar conformance suite  ->  make pulsar-up test-pulsar"
	@echo "  - the release pipeline          ->  make snapshot"

fmt-check: ## fail if anything needs gofmt (ci: build + vet)
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
	  { echo "gofmt needed:"; \
	    echo "$$unformatted"; \
	    echo "run: make fmt"; } >&2; \
	  exit 1; \
	fi

vet: ## run go vet (ci: build + vet)
	$(GO) vet ./...

# No version is pinned here on purpose. ci.yml runs golangci-lint-action at
# `latest`, so pinning one in this file would be a second answer to "which
# linter does this repository use" — and a tool version in a Makefile is a
# dependency surface Dependabot does not watch (.github/dependabot.yml).
lint: ## run golangci-lint (ci: golangci-lint)
	@command -v golangci-lint >/dev/null 2>&1 || { \
	  echo "golangci-lint is not on PATH."; \
	  echo "  Install it from https://golangci-lint.run/welcome/install/ —"; \
	  echo "  ci.yml runs whatever the latest release is, so no version is"; \
	  echo "  pinned here."; \
	  exit 1; \
	} >&2
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

# Both drivers report in one run, rather than stopping at the first failure —
# ci.yml sets `fail-fast: false` on this matrix for the same reason: when a
# statement parses on one dialect and not the other, you want to see which.
#
# The turso leg repeats what `make test` already ran on the default driver,
# and CI repeats it too. A target that ran only the non-default driver would
# save a minute at the cost of meaning something different depending on what
# ran before it — and it would stop being the matrix it is named after.
test-stores: ## certify the store on both drivers (ci: stores (dual driver))
	@status=0; \
	for driver in $(STORE_DRIVERS); do \
	  echo "==> internal/store on $$driver"; \
	  CREWLET_STORE_DRIVER=$$driver $(GOTEST) ./internal/store/... || status=1; \
	done; \
	exit $$status

test-e2e: require-node ## the end-to-end gates, verbose (ci: end-to-end gates)
	$(GOTEST) ./internal/e2e/... -v

# The dashboard's suites run under plain `node`, driven from internal/api, and
# they SKIP without one — the dashboard has no Go code of its own, so a green
# run with no node has left a whole subsystem untested. ci.yml installs node
# rather than tolerating that; here, saying so is the best we can do.
require-node: ## fail unless node is on PATH (every suite target needs it)
	@command -v node >/dev/null 2>&1 || { \
	  echo "node is not on PATH, and the dashboard's suites need it."; \
	  echo "  They run under plain node (no package.json, no runner) and SKIP"; \
	  echo "  without one, so the run would go green having tested none of"; \
	  echo "  static/dashboard/. Install any node with ES modules, as ci.yml"; \
	  echo "  does, and re-run."; \
	  exit 1; \
	} >&2

##@ Suites that need a broker

# The suite SKIPS when these variables are unset, and skipping is not passing:
# this is the only place the Pulsar backend is certified at all. Setting them
# here is what turns "quietly did nothing" into "failed because no broker is
# listening", so the preflight below reports that itself rather than leaving
# it to a connection error 30 seconds in.
test-pulsar: ## certify the Pulsar backend (needs `make pulsar-up`)
	@curl -sf $(PULSAR_ADMIN_URL)/admin/v2/clusters >/dev/null 2>&1 || { \
	  echo "No Pulsar broker is answering at $(PULSAR_ADMIN_URL)."; \
	  echo "  Start one with: make pulsar-up"; \
	  echo "  Or point this run at another: make test-pulsar \\"; \
	  echo "      PULSAR_URL=pulsar://host:6650 \\"; \
	  echo "      PULSAR_ADMIN_URL=http://host:8080"; \
	  exit 1; \
	} >&2
	CREWLET_TEST_PULSAR_URL=$(PULSAR_URL) \
	CREWLET_TEST_PULSAR_ADMIN_URL=$(PULSAR_ADMIN_URL) \
	$(GOTEST) ./internal/queue/pulsar/... -timeout $(PULSAR_TIMEOUT)

# --wait here, and nowhere else: the next thing you run is the conformance
# suite, and a broker that is up but not ready fails it. The vendor loops
# below hand the waiting to their bootstrap script, which reports progress
# while it waits (GitLab's healthcheck alone allows eight minutes).
pulsar-up: ## start the local Pulsar broker (profile: pulsar)
	$(COMPOSE) --profile pulsar up -d --wait

pulsar-down: ## stop the local Pulsar broker (add -v yourself to drop its data)
	$(COMPOSE) --profile pulsar down

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

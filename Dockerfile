# The Crewlet engine as a container image.
#
# The binary is copied in rather than built here: goreleaser has already
# cross-compiled every target from one checkout, and building again inside
# the image would produce a SECOND binary for linux — one nobody checksummed
# and nobody signed.
#
# # Why a userland, and not distroless
#
# For most pure-Go services `scratch` is the right image. Not for this one,
# and there are two independent reasons — the second is the hard one.
#
# FIRST: the binary is not static on linux. Measured on the release artifact:
# `dynamically linked, interpreter /lib64/ld-linux-x86-64.so.2`, NEEDED
# libdl.so.2, libpthread.so.0, libc.so.6. CGO_ENABLED=0 does not prevent that
# here — the store's database engine is loaded with dlopen through purego,
# which declares its imports with //go:cgo_import_dynamic. So `scratch` and a
# musl base do not fail slowly or partially; the process never starts.
#
# Nor is "just build it static" the way out: a static program has no dynamic
# loader and cannot dlopen at all, so that build segfaults at its first query
# rather than running on scratch (measured: a static build segfaults).
#
# SECOND, and true even if that changed: the engine SPAWNS things.
#
#   - The local sandbox backend (`providers.sandbox: {type: local}`) runs a
#     coding agent as a child process tree and applies setup steps that are
#     shell commands out of the company config.
#   - Those setup recipes are config, not engine code, and the one this repo
#     ships (`examples/nimbus.company.yaml`) configures a git credential
#     helper — so `git` is part of the surface an operator gets, not an
#     optional extra.
#   - stdio MCP servers are child processes too, launched by whatever command
#     the config names.
#
# On `scratch` all three fail at the moment they are used, with an image that
# started perfectly happily. The size difference buys nothing an operator
# wanted.
#
# stdio MCP servers whose runtime is NOT here (node, python, uv) still need a
# derived image. That is deliberate: an image that guessed at three runtimes
# would be wrong for everyone and large for everyone.
#
# PINNED BY DIGEST, the multi-arch index's, with the tag beside it. A bare tag
# is whatever Docker Hub serves for it at the moment a build runs, and the
# release signs what it builds: a base that moved underneath would ship, signed
# under this repository's identity, with no change anybody reviewed. The digest
# is what Dependabot's `docker` entry moves, so a new base arrives as a
# `build(deps)` pull request, which release.yml's snapshot job builds.
FROM debian:trixie-slim@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad

# ca-certificates: every vendor call is HTTPS, and a container with no trust
# store fails them all with an error that names the certificate rather than
# the missing package.
# git: the shipped sandbox setup recipe drives it.
# tini: the engine spawns process trees, and PID 1 without a reaper collects
# zombies until the pid table fills — the same reason local.go passes --init
# to the container sandbox backend.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git tini \
    && rm -rf /var/lib/apt/lists/*

# A non-root user, and a HOME it can write: the CLI-agent workspaces, the
# store file and an embedded stream's directory are all written under paths
# the engine derives from the running user.
RUN useradd --create-home --uid 10001 --shell /usr/sbin/nologin crewlet
USER crewlet
WORKDIR /home/crewlet

# goreleaser stages each platform's binary under its own TARGETPLATFORM
# directory in the build context, so ONE Dockerfile serves every architecture
# and no stage here cross-compiles anything. The image of main is built from
# this file too, from the context `make image-context` stages in the same
# layout: a path changed here is changed there.
ARG TARGETPLATFORM
COPY ${TARGETPLATFORM}/crewlet /usr/local/bin/crewlet

# The license and third-party notices, where Debian keeps a package's: the
# image redistributes the binary and everything it links, so it owes the same
# notices the release archives carry. goreleaser stages these three files from
# the checkout (dockers_v2 extra_files in .goreleaser.yaml), and
# `make image-context` stages the same three.
COPY LICENSE build/notices/THIRD_PARTY_NOTICES.txt /usr/share/doc/crewlet/
COPY static/dashboard/THIRD_PARTY_NOTICES.txt /usr/share/doc/crewlet/dashboard/

# The node's HTTP port, bound whenever Tier A's `api.port` is set — the
# operator's choice rather than the company's; `api.port: 0` serves no HTTP at
# all, so publishing it is a convenience, not a promise. It is bound as the
# process starts, before the store is opened: until the node serves, /health
# answers 503 while it recovers what it holds and 200 after, /ready 503, and
# every route it will serve 503 `starting`. `node.roles` decides what it then
# carries: with `ingress`, the whole API (the dashboard, the REST API and
# every webhook route); without it, only the /health and /ready probes, and a
# seats node's executor tool bridge. A deployment that sets
# `api.public.port` moves the webhook, OTLP and bridge routes to that second
# listener, which is the operator's to publish beside this one.
#
# 8000 RATHER THAN 8080, because there were two numbers. The examples, the
# quickstart and the configuration guide all bind 8000; this line and four
# reference pages said 8080 — so a founder who followed the quickstart and
# then ran the image published a port nothing was listening on. `internal/config`
# names it once, as DefaultAPIPort, and everything quotes that.
EXPOSE 8000

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/crewlet"]
CMD ["run"]

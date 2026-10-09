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
# # Why a point release and a digest, and not `trixie-slim`
#
# `trixie-slim` carries no version Dependabot can compare, so the docker entry
# watching this file never opened a pull request, and every release shipped
# whatever the tag pointed at on the day it was cut — a base nobody could name
# afterwards. Pinned, Dependabot moves the point release, and the digest when
# Debian rebuilds that point release with security fixes, each as a pull
# request on which release.yml's snapshot job builds this image. So the base
# stays as current as the floating tag kept it, to within a week, and a
# release records which one it was. The digest is the multi-arch index, which
# both release platforms resolve from.
#
# THE MAJOR IS HELD AT 13, by an `ignore` on the docker entry in
# .github/dependabot.yml — the hold `trixie-slim` made by its name. Those
# bumps merge themselves once ci.yml's required checks pass, and the snapshot
# job is not one of them: a bump it fails to build still merges, and the first
# `v*` tag after it is where the image build breaks. A point release within a
# stable major keeps its package names; a new major renames and drops them, so
# moving to Debian 14 is a change somebody makes on purpose, with the image
# built before it lands.
FROM debian:13.7-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f

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
# and no stage here cross-compiles anything.
ARG TARGETPLATFORM
COPY ${TARGETPLATFORM}/crewlet /usr/local/bin/crewlet

# The license and third-party notices, where Debian keeps a package's: the
# image redistributes the binary and everything it links, so it owes the same
# notices the release archives carry. goreleaser stages these three files from
# the checkout (dockers_v2 extra_files in .goreleaser.yaml).
COPY LICENSE build/notices/THIRD_PARTY_NOTICES.txt /usr/share/doc/crewlet/
COPY static/dashboard/THIRD_PARTY_NOTICES.txt /usr/share/doc/crewlet/dashboard/

# The node's one HTTP port, bound whenever Tier A's `api.port` is set — the
# operator's choice rather than the company's; `api.port: 0` serves no HTTP at
# all. `node.roles` decides what it carries: with `ingress`, the whole API (the
# dashboard, the REST API and every webhook route); without it, only the
# /health and /ready probes, and a seats node's agent-mode tool bridge.
# EXPOSE documents 8080, the port the container examples in the docs pass
# (the Kubernetes probes in docs/guides/deployment.md among them); it binds
# nothing by itself, and publishing it is the operator's call.
EXPOSE 8080

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/crewlet"]
CMD ["run"]

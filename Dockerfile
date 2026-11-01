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
#   - The code host (code.native, on a node with the git role) runs git
#     itself, as a subprocess per request — the git the `git` stage below
#     builds, not Debian's.
#
# On `scratch` all four fail at the moment they are used, with an image that
# started perfectly happily. The size difference buys nothing an operator
# wanted.
#
# stdio MCP servers whose runtime is NOT here (node, python, uv) still need a
# derived image. That is deliberate: an image that guessed at three runtimes
# would be wrong for everyone and large for everyone.
#
# EVERY BASE IS PINNED BY DIGEST, the multi-arch index's, with the tag beside
# it — the git stage's and the runtime stage's, which are one image at one
# digest. A bare tag is whatever Docker Hub serves for it at the moment a build
# runs, and the release signs what it builds: a base that moved underneath
# would ship, signed under this repository's identity, with no change anybody
# reviewed — the git stage's included, since the git it compiles is copied
# into the image. The digest is what Dependabot's `docker` entry moves, so a
# new base arrives as a `build(deps)` pull request, which release.yml's
# snapshot job builds.

# THE GIT A GIT NODE RUNS, built here rather than installed: Debian trixie
# ships 2.47, and the git every pool runs is the one scripts/git-pin.env pins —
# the newest release, built from its checksummed tarball by scripts/git.sh,
# the recipe CI and a contributor's `make git` run too, so the image, the
# suite and every benchmark figure are of one git. The version and its SHA-256
# are the pin's, copied in: nothing here repeats them. What the recipe builds,
# and what it leaves out, is in its header.
#
# The stage runs on the BUILD platform and cross-compiles for the target
# (GIT_PIN_TARGET_ARCH), because the release builds linux/arm64 under QEMU,
# where compiling git took 812 s against 87 s cross-compiled on four vCPUs —
# and the cross-compiled stage runs beside the runtime stage's emulated
# apt-get, which the arm64 image pays anyway (RELEASING.md has the minutes).
FROM --platform=$BUILDPLATFORM debian:trixie-slim@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad AS git
ARG TARGETARCH
# The toolchain for the target — gcc where the target is this platform,
# Debian's cross toolchain where it is not — and the target architecture's
# development files for the two libraries the git links: libcurl (its HTTPS
# transport) and zlib. libcurl is GnuTLS's flavour, Debian's own pairing for
# its git: git is GPLv2-only and OpenSSL 3 is Apache-2.0, so the git this image
# redistributes links no OpenSSL itself — the recipe builds it with none of its
# own (NO_OPENSSL) — and does HTTPS through libcurl over GnuTLS, exactly as
# Debian's git does. That keeps OpenSSL out of what git links and no further:
# libcurl's own dependencies, trixie's libssh2 and OpenLDAP, link OpenSSL 3, so
# git-remote-https still maps libssl and libcrypto, exactly as under Debian's
# git. For that transitive load the image relies on Debian's reading, the one
# Debian ships its git under; RELEASING.md states it.
RUN set -eu; \
    native="$(dpkg --print-architecture)"; \
    if [ "$TARGETARCH" = "$native" ]; then toolchain="gcc libc6-dev"; \
    else dpkg --add-architecture "$TARGETARCH"; toolchain="crossbuild-essential-$TARGETARCH"; fi; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl xz-utils make $toolchain \
        "libcurl4-gnutls-dev:$TARGETARCH" "zlib1g-dev:$TARGETARCH"; \
    rm -rf /var/lib/apt/lists/*
COPY scripts/git-pin.env scripts/git.sh /src/scripts/
RUN GIT_PIN_TARGET_ARCH="$TARGETARCH" GIT_PIN_PREFIX=/usr/local DESTDIR=/out /src/scripts/git.sh node

FROM debian:trixie-slim@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad

# ca-certificates: every vendor call is HTTPS, and a container with no trust
# store fails them all with an error that names the certificate rather than
# the missing package.
# libcurl3t64-gnutls: the HTTPS transport of the git built above
# (git-remote-https), which every client the engine starts on this host and
# the shipped sandbox setup recipe use — GnuTLS's libcurl, the one the git
# stage linked and the one Debian's git, which this image shipped before,
# depends on. It brings libssl3t64 with it, for its own libssh2 and OpenLDAP:
# the git stage's comment says what that means for git's licence. Nothing else
# that git links is missing from the slim image (zlib and libc are already
# there), and the self-check below holds that.
# tini: the engine spawns process trees, and PID 1 without a reaper collects
# zombies until the pid table fills — the same reason local.go passes --init
# to the container sandbox backend.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates libcurl3t64-gnutls tini \
    && rm -rf /var/lib/apt/lists/*

# git, built above, under /usr/local: GPLv2. The image redistributes it, so
# /usr/local/share/doc/git/ carries its licence texts, its corresponding
# source — the pinned tarball itself and the recipe that built it — and a
# SOURCE file saying what each is.
COPY --from=git /out/usr/local/ /usr/local/

# THE IMAGE'S GIT IS THE PIN, checked where it is built — on every platform,
# the arm64 one under QEMU — rather than by a step that reads this file: a
# stage that stopped building it, built another, or linked a library this
# image lacks fails the release here. The HTTPS helper is not only present but
# STARTED: ldd finds every library it links (a cross-compiled build's risk is
# its linking, which `test -x` never exercises), and a loopback ls-remote to a
# closed port shows it loads libcurl and reaches curl's connect — every proxy
# off, so an image built behind one checks the same thing. The recipe's
# choices an image depends on are held too: the transport is GnuTLS's libcurl
# and git was built with no OpenSSL of its own (its build options would name
# the OpenSSL its sources were compiled against), the checksum git hashes
# every clone's pack with is block-sha1 (git version --build-options names
# it; object names stay sha1dc), and the system configuration and attributes
# are read from /etc, where Debian's git read them and an operator who derives
# this image writes them. The source it owes is held to the pin's digest. The
# pin is bind-mounted rather than copied, so no layer keeps it.
RUN --mount=type=bind,source=scripts/git-pin.env,target=/tmp/git-pin.env \
    set -eu; \
    . /tmp/git-pin.env; \
    test "$(git version)" = "git version $GIT_NODE_VERSION"; \
    test -x /usr/local/libexec/git-core/git-remote-https; \
    if ldd /usr/local/libexec/git-core/git-remote-http | grep 'not found' >&2; then exit 1; fi; \
    ldd /usr/local/libexec/git-core/git-remote-http | grep -q 'libcurl-gnutls\.so'; \
    options="$(git version --build-options)"; \
    if printf '%s\n' "$options" | grep '^OpenSSL:' >&2; then exit 1; fi; \
    printf '%s\n' "$options" | grep -qx 'non-collision-detecting-SHA-1: SHA1_BLK_UNSAFE'; \
    test "$(git var GIT_CONFIG_SYSTEM)" = /etc/gitconfig; \
    test "$(git var GIT_ATTR_SYSTEM)" = /etc/gitattributes; \
    out="$(env no_proxy='*' NO_PROXY='*' git ls-remote https://127.0.0.1:1/probe.git 2>&1 || true)"; \
    case "$out" in *'127.0.0.1 port 1'*) ;; *) echo "$out" >&2; exit 1 ;; esac; \
    test -f /usr/local/share/doc/git/COPYING; \
    test -f /usr/local/share/doc/git/SOURCE; \
    echo "$GIT_NODE_SHA256  /usr/local/share/doc/git/git-$GIT_NODE_VERSION.tar.xz" | sha256sum -c -

# A non-root user, and a HOME it can write: the CLI-agent workspaces, the
# store file and an embedded stream's directory are all written under paths
# the engine derives from the running user.
RUN useradd --create-home --uid 10001 --shell /usr/sbin/nologin crewlet
USER crewlet
WORKDIR /home/crewlet

# goreleaser stages each platform's binary under its own TARGETPLATFORM
# directory in the build context, so ONE Dockerfile serves every architecture
# and no stage here compiles the engine: the one stage that cross-compiles
# anything is git's, above. The image of main is built from this file too,
# from the context `make image-context` stages in the same layout — the
# binaries, the notices and the git pin and recipe the git stage reads — so a
# path changed here is changed there.
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

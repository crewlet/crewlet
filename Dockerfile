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
FROM debian:trixie-slim

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

# The API's port. The engine serves nothing on it unless a company config
# turns the dashboard on, so publishing it is a convenience, not a promise.
EXPOSE 8080

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/crewlet"]
CMD ["run"]

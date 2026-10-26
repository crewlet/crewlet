# crewlet-coding: a reference box image for the `container` sandbox cell.
#
# `providers.sandbox.local.image` names the image a `run_in: container` box is
# started from, and the engine ships no default because a box without the
# coding CLI fails only once an agent tries to use it. This is the image to
# name when you have no reason to build your own: the two coding CLIs the
# engine has runners for, the toolchains most web and Python repositories
# build with, and nothing else (docs/concepts/code-sandbox.md, "Local
# sandboxes", has the config).
#
# # How the engine runs it
#
# Read internal/sandbox/local.go (runArgv) before changing anything here; the
# image is shaped around that command line:
#
#   docker run -d --init -v <box home>:/home/user -w /home/user/workspace \
#     -e HOME=/home/user [--user <engine uid>:<gid>] <run_args> IMAGE sleep infinity
#
#   - NO ENTRYPOINT. The engine appends `sleep infinity` as the command and
#     execs every job into the running container, so an entrypoint would
#     wrap PID 1 in something the engine never asked for.
#   - /home/user IS A MOUNT. The box's own home is bind-mounted over it, so
#     nothing installed under /home/user survives: every tool lives under
#     /usr/local or /opt, and the account below exists for runs that start
#     the image by hand.
#   - THE PROCESS MAY HAVE NO PASSWD ENTRY. On a rootful runtime the engine
#     passes `--user <its own uid>:<gid>` so the files a box writes stay the
#     engine's to manage, and that uid is the host's (501 on macOS), not the
#     image's. Everything here works for a uid the image has never heard of:
#     tools are world-readable and executable, and caches resolve from HOME.
#     On a rootless runtime no --user is passed and the job runs as container
#     root, which the user namespace maps onto the invoking user.
#
# # What is pinned, and how each pin is verified
#
#   - The base images, by multi-arch index DIGEST with the tag beside it, so a
#     tag that moves cannot change what a build installs. Dependabot's
#     `docker` entry for this directory moves both.
#   - Node.js, by copying the official node image's runtime (the digest above
#     is the verification). Node 22 is the maintenance LTS line, which Expo
#     SDK 54 (React Native 0.81, `node >= 20.19.4`) and Create React App both
#     build on; Node 20 reached end of life in April 2026.
#   - The coding CLIs, by package-lock.json beside this file: `npm ci`
#     refuses any tarball whose sha512 differs from the lock, the platform
#     binaries included. Dependabot's `npm` entry for this directory moves
#     them. The versions are the ones the engine's runners are measured on
#     (`written_for` in internal/providers/llm/cliagent/cliprofile/profiles.yaml):
#     move the two together.
#   - The GitHub CLI, by version and the SHA-256 GitHub publishes for each
#     release (gh_<version>_checksums.txt), checked before it is unpacked.
#     Dependabot cannot see this pin; to move it, take the newest release
#     and copy its two linux tarball sums from that file.
#
# Python is 3.11 because that is the line this image exists to serve; the
# base tag names it. No secret is baked in: a box's credentials arrive in the
# run environment and the box's own home, at run time, from the engine.
#
# Built by `make coding-image`, for the build host's platform, and by
# .github/workflows/coding-image.yml for linux/amd64 and linux/arm64.

FROM node:22.23.4-trixie-slim@sha256:8eb619e402eaede1748a3c8173bffab391592812e85136452f781741c7242ddf AS node

FROM python:3.11.17-slim-trixie@sha256:b7a9e5f6a93d6d4a31f129bbd4bdee69efccb6ded0fb230d78aafaf37af98377

ARG GH_VERSION=2.103.3
ARG GH_SHA256_AMD64=4c806978ffce1cec70853302a56d70ac6da4f19bae7adca4e1a8ab00633dd91d
ARG GH_SHA256_ARM64=ee32df2323c8847f5fac6cb6b4b69ca75828e9329a8f22f677021f7b3306ce63
# Set by BuildKit and Buildah. Read through dpkg below when a builder does not
# set it, so the one Dockerfile builds under every builder for both targets.
ARG TARGETARCH

# What a coding agent reaches for in an ordinary repository, and what native
# npm and pip packages compile against:
#   ca-certificates, curl: every vendor call and download is HTTPS.
#   git: clone, commit and push; the git-auth setup recipe drives it.
#   build-essential, pkg-config: node-gyp and sdist builds (the base image's
#     own Python headers sit under /usr/local/include).
#   libpq-dev: psycopg and other PostgreSQL clients built from source.
#   jq, procps, unzip, xz-utils: the small tools agents and build scripts
#     assume (procps gives `ps` and `pgrep`, which test runners and dev
#     servers shell out to).
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        build-essential \
        ca-certificates \
        curl \
        git \
        jq \
        libpq-dev \
        pkg-config \
        procps \
        unzip \
        xz-utils \
    && rm -rf /var/lib/apt/lists/*

# Node.js and its bundled npm and corepack, from the official image. Only the
# runtime is copied: that image's Yarn lives under /opt and its links would
# dangle here, and corepack provides Yarn and pnpm on demand instead.
COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY --from=node /usr/local/lib/node_modules/npm /usr/local/lib/node_modules/npm
COPY --from=node /usr/local/lib/node_modules/corepack /usr/local/lib/node_modules/corepack
# `corepack enable` writes its yarn and pnpm shims beside node, which only
# root can do, so it is done here: a setup step runs as the box's account and
# could not. A repository's `packageManager` field then selects the version,
# which corepack downloads and checks against its own signing keys.
RUN ln -s ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm \
    && ln -s ../lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx \
    && ln -s ../lib/node_modules/corepack/dist/corepack.js /usr/local/bin/corepack \
    && corepack enable \
    && node --version && npm --version && corepack --version

# The GitHub CLI, verified against its published checksum before unpacking.
RUN set -eu; \
    arch="${TARGETARCH:-$(dpkg --print-architecture)}"; \
    case "$arch" in \
        amd64) sum="$GH_SHA256_AMD64" ;; \
        arm64) sum="$GH_SHA256_ARM64" ;; \
        *) echo "crewlet-coding builds for amd64 and arm64, not $arch" >&2; exit 1 ;; \
    esac; \
    tarball="gh_${GH_VERSION}_linux_${arch}.tar.gz"; \
    tmp="$(mktemp -d)"; \
    curl -fsSL --proto '=https' --tlsv1.2 -o "$tmp/$tarball" \
        "https://github.com/cli/cli/releases/download/v${GH_VERSION}/${tarball}"; \
    echo "$sum  $tmp/$tarball" | sha256sum -c -; \
    tar -xzf "$tmp/$tarball" -C "$tmp"; \
    install -m 0755 "$tmp/gh_${GH_VERSION}_linux_${arch}/bin/gh" /usr/local/bin/gh; \
    rm -rf "$tmp"; \
    gh --version

# The coding CLIs, installed from the lock. Each install script hard-links the
# matching platform binary out of its optional dependency (which the lock
# verifies like every other tarball) into the launcher package, and fetches
# nothing while that dependency is present.
#
# OpenCode's platform packages are then removed. Its musl builds declare no
# `libc`, so npm installs them on glibc too, at about 220 MB each, and the
# binary the launcher runs survives the removal because it is a hard link of
# its own. On amd64 its script picks the AVX2 build when the BUILD host's CPU
# has AVX2, as every current CI and workstation CPU does; a host without it
# needs an image built there. The npm cache goes too, so the layer holds the
# install and not a second copy of it.
COPY package.json package-lock.json /opt/crewlet-coding/
RUN cd /opt/crewlet-coding \
    && npm ci --no-audit --no-fund \
    && rm -rf node_modules/@opencode/cli-* \
    && npm cache clean --force \
    && ln -s /opt/crewlet-coding/node_modules/.bin/claude /usr/local/bin/claude \
    && ln -s /opt/crewlet-coding/node_modules/.bin/opencode /usr/local/bin/opencode \
    && claude --version \
    && opencode --version

# The CLIs are pinned in this image, so neither may replace itself inside a
# box: an update would run a CLI the engine's runner was not measured on.
# Claude Code's switch for every connection that is not the run's own (its
# update check, telemetry and error reporting) is the one the engine sets on
# its own Claude Code calls (cliprofile/profiles.yaml), and covers the update.
# OpenCode's runner writes `"update": "disable"` into each run's config. The
# rest keeps headless runs quiet: no npm or pip upgrade nag in a transcript,
# and corepack fetches a repository's package manager without asking.
ENV CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
    COREPACK_ENABLE_DOWNLOAD_PROMPT=0 \
    NPM_CONFIG_UPDATE_NOTIFIER=false \
    PIP_DISABLE_PIP_VERSION_CHECK=1

# The account a run started by hand uses, at the home E2B's templates use, so
# in-box paths read the same whichever cell a box runs in.
RUN useradd --create-home --home-dir /home/user --uid 1000 --shell /bin/bash user
USER user
WORKDIR /home/user

LABEL org.opencontainers.image.title="crewlet-coding" \
      org.opencontainers.image.description="Reference box image for the Crewlet container sandbox cell: Claude Code, OpenCode, git, gh, Python 3.11 and Node.js 22." \
      org.opencontainers.image.source="https://github.com/crewlet/crewlet" \
      org.opencontainers.image.licenses="MIT"

CMD ["/bin/bash"]

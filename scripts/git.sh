#!/bin/sh
#
# git.sh — build one of the gits scripts/git-pin.env pins, from its release tarball checked against the pin's
# SHA-256: the ONE recipe the engine image, CI and a contributor's machine share (`make git`).
#
#   scripts/git.sh node | node-floor | client-floor
#
# Prints the directory holding that build's `git` (its bin/) and nothing else on stdout, building it first unless
# an intact build is already there. A refusal, a build's progress and a build's failure go to stderr. Exits 0 with
# the directory printed, 1 when a download or a build failed, 2 when it was asked something it refuses — an unknown
# pin, a malformed pin file, a tool the build needs and the machine lacks — before anything is downloaded.
#
# Environment (each optional):
#   CREWLET_GIT_CACHE    where builds are kept; ${XDG_CACHE_HOME:-$HOME/.cache}/crewlet/git
#   GIT_PIN_PREFIX       install at this absolute prefix instead of the cache (the image: /usr/local, staged under
#                        DESTDIR), with git's licence texts, the tarball and this recipe under share/doc/git/
#   DESTDIR              stage a GIT_PIN_PREFIX install under it, as make's own DESTDIR does
#   GIT_PIN_TARGET_ARCH  amd64 or arm64: with GIT_PIN_PREFIX, cross-compile for it with Debian's <triplet>-gcc
#                        where it is not this machine's (the image's build stage)
#   GIT_PIN_DOWNLOADS    where the tarballs are read from; https://www.kernel.org/pub/software/scm/git (curl reads
#                        file:// as well)
#
# None of the names is git's own. GIT_PREFIX would have been, and git exports it — to every shell alias it runs, as
# the subdirectory the alias was invoked from — so a `make git` reached through a git alias would have installed into
# a relative path.
#
# WHAT IT BUILDS, AND WHAT IT LEAVES OUT. One `make install` of the tarball with exactly the variables below, on
# every machine, so the image, CI and a laptop run the same feature set. It keeps libcurl — git-remote-https, which
# every client the engine starts and the shipped sandbox setup recipe use — and zlib. It drops what no engine path
# runs: Perl, Python and Tcl programs (send-email, p4, gitk), gettext (every git the engine runs is under LC_ALL=C,
# whose English is what gitproc reads, so a build without translations is the one its classifier is right for),
# expat (http-push, WebDAV, which smart HTTP never uses), PCRE (`grep -P`, `log -P`) and Rust (optional until git
# 3.0, whose Makefile makes it mandatory — the bump that takes 3.x changes this recipe).
#
# NO OPENSSL OF ITS OWN (NO_OPENSSL). git's Makefile assumes OpenSSL everywhere but macOS: every file it compiles
# includes <openssl/ssl.h> (git-compat-util.h), and git-imap-send links libssl and libcrypto for a TLS of its own. No
# engine path runs imap-send, which speaks TLS through libcurl once this is set, so OpenSSL's headers are no
# prerequisite — Ubuntu 24.04's libcurl4-openssl-dev does not bring them, and without this the build stopped at its
# first file there — and the git links no OpenSSL itself: it does TLS through libcurl, GnuTLS's in the image, as
# Debian's own git does. That keeps OpenSSL out of what git links and no further: whatever libcurl links comes along,
# and on Debian libcurl's own libssh2 and OpenLDAP link OpenSSL 3, so the image's git-remote-https maps it exactly as
# Debian's git does (RELEASING.md states what the image relies on for that). macOS's Makefile sets it already
# (Apple's CommonCrypto), so it is the same on every machine.
#
# A FAST CHECKSUM, NEVER A FAST OBJECT NAME (BLK_SHA1_UNSAFE). An object's name stays collision-detecting SHA-1
# (sha1dc), which is what a collision attack would forge. What git >= 2.48 lets a build choose apart is the
# checksum it computes over bytes it writes or verifies — the trailer pack-objects hashes over every byte of every
# clone and fetch it sends, and the trailers of every .idx, .rev, multi-pack-index, bitmap and commit-graph the cache
# writes — which names nothing and protects against no adversary. block-sha1 is portable C, links nothing, and
# writes the same bytes: measured on 2.56.0 over a bitmapped copy of this repository (one 103 MB pack of 63 064
# objects; four vCPUs of a 2.8 GHz Xeon without SHA extensions), a full-reuse clone's pack-objects spent a median
# 0.58 s of user CPU against 0.71 s with sha1dc doing both (eight runs each; 19 % less), the packs byte for byte
# the same. OpenSSL's SHA-1 was faster still (0.47 s) but links libcrypto into every git, which cost every process
# about a millisecond more to start (3.7–4.2 ms against 2.5–3.1) — four a warm fetch — and it is the OpenSSL this
# recipe leaves out. A git older than 2.48 ignores the variable and hashes both with sha1dc.
#
# THE SYSTEM CONFIGURATION WHERE A SYSTEM KEEPS IT. An install at GIT_PIN_PREFIX reads /etc/gitconfig and
# /etc/gitattributes, as the Debian git the image shipped before did — git's Makefile otherwise reads them from
# $prefix/etc unless the prefix is /usr, and an operator who sets git up system-wide in a derived image writes
# /etc. A build in the cache keeps the prefix's own etc/, which holds nothing, so the suite's git reads no system
# file of the machine's.
#
# NOTHING OF THE CALLER REACHES THE BUILD. The toolchain and its flags are passed rather than inherited, and git's
# make runs with an environment of PATH, HOME and TMPDIR alone: git's Makefile reads every one of its hundred-odd
# knobs (NO_CURL, USE_LIBPCRE, INSTALL, CURL_CONFIG, …) from the environment wherever the command line does not set
# it, and an outer make hands its own command line down in MAKEFLAGS. So nothing in a developer's shell or on the
# outer make's command line builds a different git under the same name — with NO_CURL exported, the build that
# cleared only MAKEFLAGS published a git without git-remote-https at the pin's name.
#
# A BUILD IN THE CACHE IS PUBLISHED, NEVER BUILT IN PLACE. It installs into a directory of its own beside the name
# (<cache>/<version>-<recipe>.XXXXXX, git's baked prefix), writes .complete last, and publishes with `ln -s`, an
# atomic create, at <cache>/<version>-<recipe>, where <recipe> is the first 12 hex digits of this script's own
# SHA-256: a change to the recipe is a new name, never a stale build reused. A build is INTACT when the name
# resolves to a directory holding .complete and a bin/git that answers `git version <version>`. A builder that finds
# the name already published by another removes its own directory and prints the other's; a name pointing at no
# intact build is removed and the link tried again, at most three times; an intact build is never removed or written
# into; and a <version>-<recipe>.* directory the name does not point at, untouched for a day, is a crashed
# builder's and is removed by the next build. So two `make test` runs on one machine — two worktrees — never delete
# a git the other is running, which an `rm -rf` followed by an install into the shared path did.
#
# ITS OWN SUITE is scripts/git-pin_test.sh (`make git-test`, in `make check` and ci.yml's `build + vet`), beside the
# pin check's: the refusals above, an intact build handed back with nothing downloaded, a tarball that is not the
# pin's, two builders at once leaving one build, and what a prefix build carries — each against a stand-in tarball,
# so no case builds git.
set -eu

usage() {
	echo "usage: scripts/git.sh node|node-floor|client-floor" >&2
	exit 2
}
[ $# -eq 1 ] || usage
case $1 in
node) var=GIT_NODE ;;
node-floor) var=GIT_NODE_FLOOR ;;
client-floor) var=GIT_CLIENT_FLOOR ;;
*) usage ;;
esac

# The pin is read BESIDE this script, in the tree and in the image's build stage alike.
here=$(cd "$(dirname "$0")" && pwd)
pin=$here/git-pin.env
if [ ! -r "$pin" ]; then
	echo "git.sh: $pin is missing: the recipe reads the pin beside itself" >&2
	exit 2
fi

# pinned NAME prints the one value $pin gives NAME, and refuses a name set never or twice.
pinned() {
	count=$(grep -c "^$1=" "$pin" || true)
	if [ "$count" -ne 1 ]; then
		echo "git.sh: $pin sets $1 $count times; it must set it once" >&2
		exit 2
	fi
	sed -n "s/^$1=//p" "$pin"
}
version=$(pinned "${var}_VERSION")
sha=$(pinned "${var}_SHA256")
if ! printf '%s\n' "$version" | grep -Eqx '[0-9]+\.[0-9]+\.[0-9]+'; then
	echo "git.sh: ${var}_VERSION in $pin is \"$version\", not a release (three numbers)" >&2
	exit 2
fi
if ! printf '%s\n' "$sha" | grep -Eqx '[0-9a-f]{64}'; then
	echo "git.sh: ${var}_SHA256 in $pin is not 64 lowercase hex digits" >&2
	exit 2
fi

digest() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1"
	else
		shasum -a 256 "$1" # macOS
	fi | cut -d' ' -f1
}
recipe=$(digest "$here/git.sh" | cut -c1-12)
tarball=git-$version.tar.xz
downloads=${GIT_PIN_DOWNLOADS:-https://www.kernel.org/pub/software/scm/git}

# intact DIR: DIR is a whole build of this pin.
intact() {
	[ -d "$1" ] && [ -f "$1/.complete" ] && [ -x "$1/bin/git" ] &&
		[ "$("$1/bin/git" version 2>/dev/null || true)" = "git version $version" ]
}

prefix=${GIT_PIN_PREFIX:-}
case $prefix in
'' | /*) ;;
*)
	echo "git.sh: GIT_PIN_PREFIX=$prefix is not an absolute path" >&2
	exit 2
	;;
esac
# make runs in the source tree, so a relative DESTDIR would stage the install inside a directory this script deletes.
case ${DESTDIR:-} in
'' | /*) ;;
*)
	echo "git.sh: DESTDIR=$DESTDIR is not an absolute path" >&2
	exit 2
	;;
esac

# The target, judged before any build is looked for: a cached build is this machine's, so an answer from the cache
# to a question about another architecture would be a git that cannot run where it was asked for.
cc=cc ar=ar strip=-s cross=""
if [ -n "${GIT_PIN_TARGET_ARCH:-}" ]; then
	case $GIT_PIN_TARGET_ARCH in
	amd64) triplet=x86_64-linux-gnu machine=x86_64 ;;
	arm64) triplet=aarch64-linux-gnu machine=aarch64 ;;
	*)
		echo "git.sh: GIT_PIN_TARGET_ARCH=$GIT_PIN_TARGET_ARCH is neither amd64 nor arm64" >&2
		exit 2
		;;
	esac
	here_machine=$(uname -m)
	[ "$here_machine" != arm64 ] || here_machine=aarch64 # macOS names it arm64
	if [ "$here_machine" != "$machine" ]; then
		if [ -z "$prefix" ]; then
			echo "git.sh: a git built for $GIT_PIN_TARGET_ARCH cannot run on this $here_machine machine, so it is never built into the cache: set GIT_PIN_PREFIX" >&2
			exit 2
		fi
		# CURL_LDFLAGS because the build machine's curl-config describes the build machine's libcurl.
		cross="HOST_CPU=$machine CURL_LDFLAGS=-lcurl"
		cc=$triplet-gcc ar=$triplet-ar strip="-s --strip-program=$triplet-strip"
	fi
fi

if [ -z "$prefix" ]; then
	cache=${CREWLET_GIT_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/crewlet/git}
	case $cache in
	/*) ;;
	*)
		echo "git.sh: the build cache $cache is not an absolute path (CREWLET_GIT_CACHE, XDG_CACHE_HOME, HOME)" >&2
		exit 2
		;;
	esac
	link=$cache/$version-$recipe
	if intact "$link"; then
		echo "$link/bin"
		exit 0
	fi
fi

# What the build runs, refused before anything is downloaded.
missing=""
for tool in make tar xz curl "$cc"; do
	command -v "$tool" >/dev/null 2>&1 || missing="$missing $tool"
done
if [ -z "$cross" ]; then
	command -v curl-config >/dev/null 2>&1 || missing="$missing curl-config (libcurl's development files)"
fi
if [ -n "$missing" ]; then
	echo "git.sh: building git $version needs:$missing — CONTRIBUTING.md, Development setup, says what to install" >&2
	exit 2
fi

work="" build="" published=""
cleanup() {
	[ -z "$work" ] || rm -rf "$work"
	[ -z "$build" ] || [ "$build" = "$published" ] || rm -rf "$build"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

work=$(mktemp -d)
if [ -n "$prefix" ]; then
	destdir=${DESTDIR:-}
	install=$destdir$prefix
	sysconfdir=/etc
else
	mkdir -p "$cache"
	build=$(mktemp -d "$cache/$version-$recipe.XXXXXX")
	chmod 755 "$build"
	destdir="" prefix=$build install=$build
	# Relative: under the build's own prefix, where nothing writes a file.
	sysconfdir=etc
fi
echo "git.sh: building git $version from $tarball into $install (a minute or two)" >&2

# The download gives up when it STALLS — nothing received for a minute (`--speed-limit 1 --speed-time 60`, the stall
# bound the engine's object store holds a transfer to, objstore.Stall) or no connection in 30 seconds — and is asked
# again on a transient failure, never for being slow while it moves: about 8 MB, which a slow link takes minutes to
# bring and still moves. Unbounded, a kernel.org transfer that stopped moving held whatever ran it — a CI test job on a
# cache miss, the release's image build — until that job's own limit, six hours for a GitHub job, while curl's own
# connect timeout is five minutes an attempt. scripts/git-pin-check.sh downloads with the same bounds.
if ! curl -fsSL --retry 3 --connect-timeout 30 --speed-limit 1 --speed-time 60 -o "$work/$tarball" "$downloads/$tarball"; then
	echo "git.sh: could not download $downloads/$tarball" >&2
	exit 1
fi
got=$(digest "$work/$tarball")
if [ "$got" != "$sha" ]; then
	echo "git.sh: $tarball hashes to $got, and $pin pins $sha" >&2
	exit 1
fi
xz -dc "$work/$tarball" | tar -xf - -C "$work"
src=$work/git-$version
if [ ! -f "$src/Makefile" ]; then
	echo "git.sh: $tarball holds no git-$version/Makefile" >&2
	exit 1
fi

jobs=$(getconf _NPROCESSORS_ONLN 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo 2)
# shellcheck disable=SC2086 # $cross is the cross-compile's make variables, or nothing, word-split on purpose.
if ! env -i PATH="$PATH" HOME="${HOME:-/}" TMPDIR="${TMPDIR:-/tmp}" make -C "$src" -j"$jobs" \
	CC="$cc" AR="$ar" CFLAGS='-g -O2 -Wall' CPPFLAGS='' LDFLAGS='' $cross \
	prefix="$prefix" sysconfdir="$sysconfdir" DESTDIR="$destdir" \
	NO_RUST=YesPlease NO_PERL=YesPlease NO_PYTHON=YesPlease NO_TCLTK=YesPlease \
	NO_GETTEXT=YesPlease NO_EXPAT=YesPlease NO_OPENSSL=YesPlease BLK_SHA1_UNSAFE=YesPlease \
	INSTALL_SYMLINKS=YesPlease SKIP_DASHED_BUILT_INS=YesPlease INSTALL_STRIP="$strip" \
	install >"$work/build.log" 2>&1; then
	tail -n 40 "$work/build.log" >&2
	echo "git.sh: building git $version failed; the last 40 lines of its log are above" >&2
	exit 1
fi

if [ -n "${GIT_PIN_PREFIX:-}" ]; then
	# THE IMAGE REDISTRIBUTES THIS GIT, so it carries what a redistribution owes: git's licence (GPLv2, with the
	# LGPL-2.1 some of its sources are under), every LICENSE*/COPYING* file of the tree at its source path (the
	# SHA-1 collision detector's MIT, reftable's BSD), and the corresponding source — the tarball itself, checked
	# against the pin above, and this recipe beside it, the script that controls its compilation and installation —
	# with a SOURCE file saying what each is. Debian's package carried a copyright file; a source build ships none.
	doc=$install/share/doc/git
	mkdir -p "$doc"
	(cd "$src" && find . -type f \( -name 'COPYING*' -o -name 'LICENSE*' -o -name 'LGPL-*' \)) | sed 's|^\./||' |
		while IFS= read -r file; do
			mkdir -p "$doc/$(dirname "$file")"
			cp "$src/$file" "$doc/$file"
		done
	cp "$work/$tarball" "$doc/$tarball"
	cp "$here/git.sh" "$doc/git.sh"
	cat >"$doc/SOURCE" <<EOF
git $version, built from its release tarball by the recipe beside this file.

Source:  https://www.kernel.org/pub/software/scm/git/$tarball
SHA-256: $sha
Copy:    $tarball, beside this file

Recipe:  git.sh, beside this file (recipe $recipe), run with GIT_PIN_PREFIX=${GIT_PIN_PREFIX}
         and GIT_PIN_TARGET_ARCH=${GIT_PIN_TARGET_ARCH:-<this machine>}; its make variables are in it.

git is free software under the GNU General Public License, version 2 (COPYING);
the other licences of its sources are beside it, at their paths in the tree.
EOF
	# A cross-compiled git cannot run here; a native one must answer as the pin.
	if [ -z "$cross" ] && [ "$("$install/bin/git" version 2>/dev/null || true)" != "git version $version" ]; then
		echo "git.sh: the git installed at $install/bin does not answer git version $version" >&2
		exit 1
	fi
	echo "$install/bin"
	exit 0
fi

: >"$build/.complete"
if ! intact "$build"; then
	echo "git.sh: the build in $build does not answer git version $version" >&2
	exit 1
fi
base=${build##*/}
tries=0
while ! ln -sn "$base" "$link" 2>/dev/null; do
	if intact "$link"; then
		# Another builder published first, and a published build is never replaced: use it.
		rm -rf "$build"
		build=""
		break
	fi
	tries=$((tries + 1))
	if [ "$tries" -ge 3 ]; then
		echo "git.sh: could not publish $build at $link: the name was taken by no intact build three times" >&2
		exit 1
	fi
	rm -rf "$link"
done
[ -z "$build" ] || published=$build

# A crashed builder's leftovers: this name's directories the name does not point at, untouched for a day.
current=$(cd "$link" && pwd -P)
find "$cache" -maxdepth 1 -type d -name "$version-$recipe.*" -mmin +1440 | while IFS= read -r stray; do
	[ "$(cd "$stray" && pwd -P)" = "$current" ] || rm -rf "$stray"
done
echo "$link/bin"

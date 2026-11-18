#!/bin/sh
#
# node.sh — fetch the Node scripts/node-pin.env pins, an official release tarball checked against the pin's SHA-256
# for this platform: the ONE recipe CI and a contributor's machine share (`make node`).
#
#   scripts/node.sh floor
#
# Prints the absolute path of that build's `node` and nothing else on stdout, fetching it first unless an intact build
# is already there. A refusal, a fetch's progress and a fetch's failure go to stderr. Exits 0 with the path printed; 1
# when a fetch failed: its download, its digest (a tarball whose SHA-256 the machine's hash could not compute among
# them), its unpack, the build's own check, or publishing the build at its name; 2 when it was asked something it
# refuses — an unknown pin, a pin file missing beside it or malformed, a platform no pin names a build for, a build
# cache that is not an absolute path, or a tool a fetch needs and the machine lacks — before anything is downloaded.
#
# Environment (each optional):
#   CREWLET_NODE_CACHE   where builds are kept; ${XDG_CACHE_HOME:-$HOME/.cache}/crewlet/node
#   NODE_PIN_DIST        where the tarballs are read from, as v<version>/node-v<version>-<platform>.tar.xz under it;
#                        https://nodejs.org/dist (curl reads file:// as well)
#
# THE PLATFORM IS THIS MACHINE'S, read from uname: linux-x64, linux-arm64, darwin-x64 or darwin-arm64, Node's names for
# the four platforms the engine is released for. A build runs only where it was built for, so there is no knob to
# fetch another platform's, and any other machine is refused naming what it is.
#
# WHAT IT KEEPS IS `node` AND ITS LICENCE: bin/node and LICENSE out of the release's tarball, about 31 MB of download
# and 125 MB kept. The rest — npm, corepack, the headers, the docs — is no part of what the suite runs, and a box's
# Node is the box's own.
#
# A BUILD IS NAMED BY WHAT DECIDES ITS BYTES, and published, never unpacked in place, by scripts/chromium.sh's rules,
# which this recipe follows rather than restates: <version>-<platform>-<digest>-l<layout>, unpacked into a directory
# of its own beside the name, .complete written last, published with `ln -s`, a crashed fetcher's leftovers swept. A
# build is INTACT when the name resolves to a directory holding .complete and a bin/node that answers --version with
# v<version>, asked without NODE_OPTIONS: a person's NODE_OPTIONS naming an option the floor's Node predates makes it
# refuse to start at all, and what the question asks is whether the build is the pin's.
#
# ITS OWN SUITE is scripts/node-pin_test.sh (`make node-test`, in `make check` and ci.yml's `build + vet`): each refusal
# above before any download, an intact build handed back with nothing downloaded and no tool a fetch needs, a build of
# another digest or layout never handed back, a tarball that is not the pin's or whose SHA-256 the machine cannot
# compute, a build whose --version disagrees, a build kept to bin/node and LICENSE, a NODE_OPTIONS that changes
# nothing, a name pointing at no intact build fetched over, a curl configuration that writes to stdout, two fetchers
# at once leaving one build, and a crashed fetcher's leftovers swept — each against a stand-in tarball, so no case
# fetches a Node.
set -eu

# layout is the version of how this recipe lays a build out — what it keeps of the tarball, its .complete, and what
# `intact` reads of it — and the one part of the recipe a build's name carries, as scripts/chromium.sh's is. ci.yml's
# cache of the build is keyed on this line beside the pin file's NODE_FLOOR_ lines.
layout=1

usage() {
	echo "usage: scripts/node.sh floor" >&2
	exit 2
}
[ $# -eq 1 ] || usage
case $1 in
floor) var=NODE_FLOOR ;;
*) usage ;;
esac

# The pin is read BESIDE this script, whatever CDPATH holds: scripts/git.sh says why, and the same `cd` is here.
unset CDPATH
here=$(cd "$(dirname "$0")" && pwd)
pin=$here/node-pin.env
if [ ! -r "$pin" ]; then
	echo "node.sh: $pin is missing: the recipe reads the pin beside itself" >&2
	exit 2
fi

# pinned NAME prints the one value $pin gives NAME, and refuses a name set never or twice, as scripts/chromium.sh reads
# its own pin.
pinned() {
	count=$(grep -c "^$1=" "$pin" || true)
	if [ "$count" -ne 1 ]; then
		echo "node.sh: $pin sets $1 $count times; it must set it once" >&2
		exit 2
	fi
	sed -n "s/^$1=//p" "$pin"
}

# This machine's platform, by Node's name for it.
os=$(uname -s)
machine=$(uname -m)
case $os:$machine in
Linux:x86_64 | Linux:amd64) platform=linux-x64 ;;
Linux:aarch64 | Linux:arm64) platform=linux-arm64 ;;
Darwin:x86_64) platform=darwin-x64 ;;
Darwin:arm64 | Darwin:aarch64) platform=darwin-arm64 ;;
*)
	echo "node.sh: the floor's Node is pinned for no $os $machine: the suite's Node runs on linux-x64, linux-arm64, darwin-x64 and darwin-arm64, the platforms the engine is released for" >&2
	exit 2
	;;
esac
suffix=$(printf '%s' "$platform" | tr 'a-z-' 'A-Z_')

version=$(pinned "${var}_VERSION")
sha=$(pinned "${var}_SHA256_$suffix")
if ! printf '%s\n' "$version" | grep -Eqx '[0-9]+\.[0-9]+\.[0-9]+'; then
	echo "node.sh: ${var}_VERSION in $pin is \"$version\", not a Node release (three numbers)" >&2
	exit 2
fi
if ! printf '%s\n' "$sha" | grep -Eqx '[0-9a-f]{64}'; then
	echo "node.sh: ${var}_SHA256_$suffix in $pin is not 64 lowercase hex digits" >&2
	exit 2
fi

# digest FILE prints FILE's SHA-256 and fails rather than print anything else: scripts/git.sh's digest, whose comment
# says what printing whatever the hash printed once cost.
digest() {
	if command -v sha256sum >/dev/null 2>&1; then
		sum=$(sha256sum "$1") || return 1
	else
		sum=$(shasum -a 256 "$1") || return 1 # macOS
	fi
	sum=${sum%% *}
	printf '%s\n' "$sum" | grep -Eqx '[0-9a-f]{64}' || return 1
	printf '%s\n' "$sum"
}
top=node-v$version-$platform
tarball=$top.tar.xz
downloads=${NODE_PIN_DIST:-https://nodejs.org/dist}

# answers DIR: what DIR's node says to --version, asked without NODE_OPTIONS (the header says why).
answers() {
	(
		unset NODE_OPTIONS
		"$1/bin/node" --version 2>/dev/null
	) | sed 's/[[:space:]]*$//' || true
}

# intact DIR: DIR is a whole build of this pin.
intact() {
	[ -d "$1" ] && [ -f "$1/.complete" ] && [ -x "$1/bin/node" ] && [ "$(answers "$1")" = "v$version" ]
}

cache=${CREWLET_NODE_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/crewlet/node}
case $cache in
/*) ;;
*)
	echo "node.sh: the build cache $cache is not an absolute path (CREWLET_NODE_CACHE, XDG_CACHE_HOME, HOME)" >&2
	exit 2
	;;
esac
name=$version-$platform-$(printf '%s' "$sha" | cut -c1-12)-l$layout
link=$cache/$name
if intact "$link"; then
	echo "$link/bin/node"
	exit 0
fi

# What the fetch runs, refused before anything is downloaded: an intact build needs none of it.
missing=""
for tool in curl xz tar; do
	command -v "$tool" >/dev/null 2>&1 || missing="$missing $tool"
done
if [ -n "$missing" ]; then
	echo "node.sh: fetching Node $version needs:$missing — CONTRIBUTING.md, Development setup, says what to install" >&2
	exit 2
fi
if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
	echo "node.sh: checking a tarball against its pin needs sha256sum, or shasum where that is absent — CONTRIBUTING.md, Development setup, says what to install" >&2
	exit 2
fi

build="" published=""
cleanup() {
	[ -z "$build" ] || [ "$build" = "$published" ] || rm -rf "$build"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

mkdir -p "$cache"
build=$(mktemp -d "$cache/$name.XXXXXX")
chmod 755 "$build"
mkdir "$build/.download"
echo "node.sh: fetching Node $version for $platform into $build (about 31 MB)" >&2

# The download is scripts/chromium.sh's, for its reasons: given up on when it stalls, asked again on a transient
# failure, never for being slow while it moves, and curl's stdout on stderr, since this script's stdout is the path.
if ! curl -fsSL --retry 3 --connect-timeout 30 --speed-limit 1 --speed-time 60 -o "$build/.download/$tarball" "$downloads/v$version/$tarball" >&2; then
	echo "node.sh: could not download $downloads/v$version/$tarball" >&2
	exit 1
fi
if ! got=$(digest "$build/.download/$tarball"); then
	echo "node.sh: cannot compute the SHA-256 of $tarball, so it cannot be checked against $pin" >&2
	exit 1
fi
if [ "$got" != "$sha" ]; then
	echo "node.sh: $tarball hashes to $got, and $pin pins $sha for $platform" >&2
	exit 1
fi
# The two members alone, out of a stream the digest has vouched for, into the download's directory, and then into
# place: a member the tarball lacks fails tar, and whatever went wrong between, `intact` below asks the build itself.
if ! xz -dc "$build/.download/$tarball" | tar -xf - -C "$build/.download" "$top/bin/node" "$top/LICENSE" >&2; then
	echo "node.sh: could not unpack bin/node and LICENSE from $tarball" >&2
	exit 1
fi
mkdir "$build/bin"
if ! mv "$build/.download/$top/bin/node" "$build/bin/node" || ! mv "$build/.download/$top/LICENSE" "$build/LICENSE"; then
	echo "node.sh: could not move bin/node and LICENSE out of $tarball's tree" >&2
	exit 1
fi
rm -rf "$build/.download"

: >"$build/.complete"
if ! intact "$build"; then
	echo "node.sh: the build in $build does not answer --version with \"v$version\" (it said \"$(answers "$build")\")" >&2
	exit 1
fi
base=${build##*/}
tries=0
while :; do
	# A name that is a directory rather than a link is never linked into: scripts/chromium.sh says why.
	if [ ! -d "$link" ] || [ -L "$link" ]; then
		ln -sn "$base" "$link" 2>/dev/null && break
	fi
	if intact "$link"; then
		# An intact build at the name — another fetcher's, published first — is never replaced: use it.
		rm -rf "$build"
		build=""
		break
	fi
	tries=$((tries + 1))
	if [ "$tries" -ge 3 ]; then
		echo "node.sh: could not publish $build at $link: the name was taken by no intact build three times" >&2
		exit 1
	fi
	rm -rf "$link"
done
[ -z "$build" ] || published=$build

# A crashed fetcher's leftovers: this name's directories the name does not point at, untouched for a day.
current=$(cd "$link" && pwd -P)
find "$cache" -maxdepth 1 -type d -name "$name.*" -mmin +1440 | while IFS= read -r stray; do
	[ "$(cd "$stray" && pwd -P)" = "$current" ] || rm -rf "$stray"
done
echo "$link/bin/node"

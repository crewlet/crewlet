#!/bin/sh
#
# chromium.sh — fetch one of the Chromiums scripts/chromium-pin.env pins, a Chrome for Testing build checked against
# the pin's SHA-256 for this platform: the ONE recipe CI and a contributor's machine share (`make chromium`).
#
#   scripts/chromium.sh newest | floor
#
# Prints the absolute path of that build's executable and nothing else on stdout, fetching it first unless an intact
# build is already there. A refusal, a fetch's progress and a fetch's failure go to stderr. Exits 0 with the path
# printed; 1 when a fetch failed: its download, its digest (a zip whose SHA-256 the machine's hash could not compute
# among them), its unpack, the build's own check, or publishing the build at its name; 2 when it was asked something
# it refuses — an unknown pin, a pin file missing beside it or malformed, a platform no pin names a build for, a build
# cache that is not an absolute path, or a tool a fetch needs and the machine lacks — before anything is downloaded.
#
# Environment (each optional):
#   CREWLET_CHROMIUM_CACHE   where builds are kept; ${XDG_CACHE_HOME:-$HOME/.cache}/crewlet/chromium
#   CHROMIUM_PIN_DOWNLOADS   where the zips are read from, as <version>/<platform>/chrome-<platform>.zip under it;
#                            https://storage.googleapis.com/chrome-for-testing-public (curl reads file:// as well)
#
# THE PLATFORM IS THIS MACHINE'S, read from uname: linux64, linux-arm64, mac-x64 or mac-arm64, Chrome for Testing's
# names for the four platforms the engine is released for. A build runs only where it was built for, so there is no
# knob to fetch another platform's, and any other machine is refused naming what it is.
#
# WHAT IT FETCHES IS THE FULL `chrome`, never chrome-headless-shell: what a browser backend on the engine host runs,
# in new headless mode, which in Chrome 132 and later is the only headless mode `chrome` has. About 200 MB a zip and
# 430 MB unpacked.
#
# A BUILD IS NAMED BY WHAT DECIDES ITS BYTES: <version>-<platform>-<digest>-l<layout>, where <digest> is the first 12
# hex digits of the pin's SHA-256 of its zip and <layout> is this recipe's layout (below). A build is that zip
# unpacked as it is, so nothing else in this recipe changes a byte of one, and an edit to the rest of it — a comment,
# a message, a check made before a fetch — reuses every build there is. scripts/git.sh names its builds by its own
# SHA-256 instead, because a git is compiled, and every line of that recipe can change what it compiles. A zip that
# is not the pin's is refused by its digest whatever the name says.
#
# A BUILD IS PUBLISHED, NEVER UNPACKED IN PLACE, by scripts/git.sh's rule, which this recipe follows rather than
# restates: it is downloaded and unpacked into a directory of its own beside the name (<cache>/<name>.XXXXXX), the
# download removed once unpacked, .complete written last, and published with `ln -s`, an atomic create, at
# <cache>/<name>. A build is INTACT when the name resolves to a directory holding .complete and an executable that
# answers --version with `Google Chrome for Testing <version>`. A fetcher that finds the name already published by
# another removes its own directory and prints the other's; a name pointing at no intact build is removed and the link
# tried again, at most three times — a name that is a directory rather than a link among them, which no fetcher makes
# and the link is never made inside; an intact build is never removed or written into; and a <name>.* directory the
# name does not point at, untouched for a day, is a crashed fetcher's and is removed by the next fetch. So two `make
# test` runs on one machine never delete a Chromium the other is running. The download goes into the build's own
# directory, on the cache's file system, and never into TMPDIR, which on a machine with a small /tmp cannot hold it.
#
# ITS OWN SUITE is scripts/chromium-pin_test.sh (`make chromium-test`, in `make check` and ci.yml's `build + vet`):
# each refusal above before any download, an intact build handed back with nothing downloaded and no tool a fetch
# needs, a build of another digest or layout never handed back, a zip that is not the pin's or whose SHA-256 the
# machine cannot compute, a build whose --version disagrees, a name pointing at no intact build fetched over (a link
# to a broken build, and a directory), a curl configuration that writes to stdout, two fetchers at once leaving one
# build, and a crashed fetcher's leftovers swept — each against a stand-in zip, so no case fetches a Chromium.
set -eu

# layout is the version of how this recipe lays a build out — the directory its zip is unpacked into, its .complete,
# and what `intact` reads of it — and the one part of the recipe a build's name carries. It moves with any change to
# one of those, so a build laid out otherwise is never read as one laid out this way, and with nothing else. ci.yml's
# cache of the builds is keyed on this line beside the pins themselves, the pin file's CHROMIUM_ lines, which are
# everything else a name carries.
layout=1

usage() {
	echo "usage: scripts/chromium.sh newest|floor" >&2
	exit 2
}
[ $# -eq 1 ] || usage
case $1 in
newest) var=CHROMIUM_NEWEST ;;
floor) var=CHROMIUM_FLOOR ;;
*) usage ;;
esac

# The pin is read BESIDE this script, whatever CDPATH holds: scripts/git.sh says why, and the same `cd` is here.
unset CDPATH
here=$(cd "$(dirname "$0")" && pwd)
pin=$here/chromium-pin.env
if [ ! -r "$pin" ]; then
	echo "chromium.sh: $pin is missing: the recipe reads the pin beside itself" >&2
	exit 2
fi

# pinned NAME prints the one value $pin gives NAME, and refuses a name set never or twice, as scripts/git.sh reads its
# own pin.
pinned() {
	count=$(grep -c "^$1=" "$pin" || true)
	if [ "$count" -ne 1 ]; then
		echo "chromium.sh: $pin sets $1 $count times; it must set it once" >&2
		exit 2
	fi
	sed -n "s/^$1=//p" "$pin"
}

# This machine's platform, by Chrome for Testing's name for it, and where a build of it keeps its executable.
os=$(uname -s)
machine=$(uname -m)
case $os:$machine in
Linux:x86_64 | Linux:amd64) platform=linux64 ;;
Linux:aarch64 | Linux:arm64) platform=linux-arm64 ;;
Darwin:x86_64) platform=mac-x64 ;;
Darwin:arm64 | Darwin:aarch64) platform=mac-arm64 ;;
*)
	echo "chromium.sh: Chrome for Testing publishes no build for $os $machine: the suite's Chromiums run on linux64, linux-arm64, mac-x64 and mac-arm64, the platforms the engine is released for" >&2
	exit 2
	;;
esac
case $platform in
linux64 | linux-arm64) exe="chrome-$platform/chrome" ;;
mac-*) exe="chrome-$platform/Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing" ;;
esac
suffix=$(printf '%s' "$platform" | tr 'a-z-' 'A-Z_')

version=$(pinned "${var}_VERSION")
sha=$(pinned "${var}_SHA256_$suffix")
if ! printf '%s\n' "$version" | grep -Eqx '[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+'; then
	echo "chromium.sh: ${var}_VERSION in $pin is \"$version\", not a Chrome for Testing version (four numbers)" >&2
	exit 2
fi
if ! printf '%s\n' "$sha" | grep -Eqx '[0-9a-f]{64}'; then
	echo "chromium.sh: ${var}_SHA256_$suffix in $pin is not 64 lowercase hex digits" >&2
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
zip=chrome-$platform.zip
downloads=${CHROMIUM_PIN_DOWNLOADS:-https://storage.googleapis.com/chrome-for-testing-public}

# answers DIR: what DIR's executable says to --version, its trailing blanks dropped (Chrome ends the line with one).
answers() {
	"$1/$exe" --version 2>/dev/null | sed 's/[[:space:]]*$//' || true
}

# intact DIR: DIR is a whole build of this pin.
intact() {
	[ -d "$1" ] && [ -f "$1/.complete" ] && [ -x "$1/$exe" ] &&
		[ "$(answers "$1")" = "Google Chrome for Testing $version" ]
}

cache=${CREWLET_CHROMIUM_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/crewlet/chromium}
case $cache in
/*) ;;
*)
	echo "chromium.sh: the build cache $cache is not an absolute path (CREWLET_CHROMIUM_CACHE, XDG_CACHE_HOME, HOME)" >&2
	exit 2
	;;
esac
name=$version-$platform-$(printf '%s' "$sha" | cut -c1-12)-l$layout
link=$cache/$name
if intact "$link"; then
	echo "$link/$exe"
	exit 0
fi

# What the fetch runs, refused before anything is downloaded: an intact build needs none of it.
missing=""
for tool in curl unzip; do
	command -v "$tool" >/dev/null 2>&1 || missing="$missing $tool"
done
if [ -n "$missing" ]; then
	echo "chromium.sh: fetching Chromium $version needs:$missing — CONTRIBUTING.md, Development setup, says what to install" >&2
	exit 2
fi
if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
	echo "chromium.sh: checking a zip against its pin needs sha256sum, or shasum where that is absent — CONTRIBUTING.md, Development setup, says what to install" >&2
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
echo "chromium.sh: fetching Chrome for Testing $version for $platform into $build (about 200 MB)" >&2

# The download is scripts/git.sh's: given up on when it STALLS — nothing received for a minute or no connection in
# 30 seconds — and asked again on a transient failure, never for being slow while it moves; and curl's stdout goes to
# stderr, because this script's stdout is the executable's path and nothing else, and a person's curlrc (their proxy,
# their CA bundle) applies to the download and may make curl write there. git.sh's comment says what each cost.
if ! curl -fsSL --retry 3 --connect-timeout 30 --speed-limit 1 --speed-time 60 -o "$build/.download/$zip" "$downloads/$version/$platform/$zip" >&2; then
	echo "chromium.sh: could not download $downloads/$version/$platform/$zip" >&2
	exit 1
fi
if ! got=$(digest "$build/.download/$zip"); then
	echo "chromium.sh: cannot compute the SHA-256 of $zip, so it cannot be checked against $pin" >&2
	exit 1
fi
if [ "$got" != "$sha" ]; then
	echo "chromium.sh: $zip of $version hashes to $got, and $pin pins $sha for $platform" >&2
	exit 1
fi
if ! unzip -q "$build/.download/$zip" -d "$build" >&2; then
	echo "chromium.sh: could not unpack $zip" >&2
	exit 1
fi
rm -rf "$build/.download"

: >"$build/.complete"
if ! intact "$build"; then
	said=$(answers "$build")
	echo "chromium.sh: the build in $build does not answer --version with \"Google Chrome for Testing $version\" (it said \"$said\")" >&2
	case $platform in
	linux*)
		echo "chromium.sh: on Linux a build that cannot start lacks a library: its $build/chrome-$platform/deb.deps (Debian, Ubuntu) or rpm.deps (Fedora, RHEL) names every package it needs" >&2
		;;
	esac
	exit 1
fi
base=${build##*/}
tries=0
while :; do
	# A name that is a directory rather than a link, which no fetcher makes, is never linked into: `ln -sn` would make
	# the link INSIDE it and answer 0, leaving the name on whatever the directory holds. It is judged below as any name
	# is, and as scripts/git.sh judges one: kept when it holds an intact build, removed and linked again when not.
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
		echo "chromium.sh: could not publish $build at $link: the name was taken by no intact build three times" >&2
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
echo "$link/$exe"

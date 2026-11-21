#!/bin/sh
#
# playwright-tree.sh — install the Playwright tree internal/browser/boxhelper/tree locks, with npm, as a seat's box
# installs it: the ONE recipe CI and a contributor's machine share (`make playwright-tree`).
#
#   scripts/playwright-tree.sh
#
# Prints the absolute path of the installed tree and nothing else on stdout, installing it first unless an intact tree
# is already there. A refusal, npm's output and an install's failure go to stderr. Exits 0 with the path printed; 1
# when an install failed: npm's own failure (a registry or mirror it could not reach, a package whose bytes the lock's
# SHA-512 refuses), npm outliving the install's bound, a tree that is not exactly the lock's, or publishing the tree at
# its name; 2 when it was asked something it refuses — an argument, the manifest or the lock missing beside it, a cache
# that is not an absolute path, or a tool the machine lacks (a SHA-256 tool; node, or an npm older than min_npm below,
# for an install) — before anything is fetched.
#
# Environment (optional):
#   CREWLET_PLAYWRIGHT_TREE_CACHE   where trees are kept; ${XDG_CACHE_HOME:-$HOME/.cache}/crewlet/playwright-tree
# npm's own configuration stays the developer's: the registry or mirror it fetches from, its proxy, CA and credentials.
#
# THE TREE is `npm ci` of the two files in a directory of its own: node_modules, exactly the lock's packages, beside
# npm-cache, the npm cache that install filled, from which boxhelper's cases install it again with each Node's own npm,
# offline (boxhelpertest's package doc). About 4 MB of download and 23 MB kept. Nothing in it is a platform's own: the
# lock holds no package with an os, a cpu or an install script (boxhelper's TestTheLockIsTheReleaseSomebodyRead), so
# its name carries no platform.
#
# A TREE IS NAMED BY WHAT DECIDES ITS BYTES, and published, never installed in place, by scripts/chromium.sh's rules,
# which this recipe follows rather than restates: <digest>-l<layout>, where <digest> is the first 12 hex digits of the
# SHA-256 over the two files' SHA-256s, installed into a directory of its own beside the name, .complete written last,
# published with `ln -s`, a crashed installer's leftovers swept. A tree is INTACT when the name resolves to a directory
# holding .complete, the lock byte for byte, npm's record of what it installed and the npm cache that install filled.
#
# ITS OWN SUITE is scripts/playwright-tree_test.sh (`make playwright-tree-test`, in `make check` and ci.yml's `build +
# vet`): each refusal above before any request, an intact tree handed back with no request and no npm, a registry
# nobody answers at and a lock whose bytes the registry does not serve, every npm setting that would change what is
# installed set in the environment and an npmrc, each flag and bound on npm's command line, an npm whose record of
# the install is not the lock's, an npm that ignores TERM stopped by the bound, an install interrupted or finished
# leaving nothing running, another lock installed beside the first, a name pointing at no intact tree installed over,
# two installers at once leaving one tree, and a crashed installer's leftovers swept — each against a stand-in
# registry on 127.0.0.1 behind a refused proxy, so no case reads the network.
set -eu

# layout is the version of how this recipe lays a tree out — npm's flags below, the npm cache beside node_modules,
# .complete, and what `intact` reads — and the one part of the recipe a tree's name carries, as scripts/chromium.sh's
# is. ci.yml's cache of the tree is keyed on this line beside the two files.
layout=1

# min_npm is the oldest npm major the recipe runs: internal/browser/boxhelper.MinNPMMajor, the npm the oldest Node the
# box helper supports bundles, which boxhelper's TestTheTreeRecipeRefusesTheNPMTheHelperRefuses holds this line to.
min_npm=10

# npm's bounds. --fetch-timeout bounds the wait for a response, never a body that trickles in: a registry that has not
# begun to answer in a minute is down, not slow. Three more tries on a transient failure, as scripts/chromium.sh's
# curl, waiting 1 s, then 5 s and 5 s, where npm's own backoff (10 s, then 60 s) took 130 s to fail on a refused
# registry: this fails in about 11 s. The whole install is the only bound on a trickle: 300 s, where a cold install
# takes under a second and npm's longest run against a registry that never answers, four tries of 60 s and that
# backoff, is 251 s, so it never cuts npm's own tries short. Then TERM, and KILL after 5 s, since npm ignores TERM
# while a fetch is outstanding (npm 10.9.9 and 11.20.0 were still running 15 s after one).
fetch_timeout_ms=60000 fetch_retries=3 fetch_retry_min_ms=1000 fetch_retry_max_ms=5000
bound=300 grace=5

[ $# -eq 0 ] || {
	echo "usage: scripts/playwright-tree.sh" >&2
	exit 2
}

# The two files are read BESIDE this script, whatever CDPATH holds: scripts/git.sh says why, and the same `cd` is here.
unset CDPATH
here=$(cd "$(dirname "$0")" && pwd)
src=$here/../internal/browser/boxhelper/tree
for file in package.json package-lock.json; do
	if [ ! -r "$src/$file" ]; then
		echo "playwright-tree.sh: $src/$file is missing: the recipe installs the manifest and the lock it finds beside itself" >&2
		exit 2
	fi
done

# A tree's name is the two files' digest, so even an intact tree needs a SHA-256 tool to be found.
if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
	echo "playwright-tree.sh: naming the tree needs sha256sum, or shasum where that is absent — CONTRIBUTING.md, Development setup, says what to install" >&2
	exit 2
fi

# digest FILE prints FILE's SHA-256, - reading stdin, and fails rather than print anything else: scripts/git.sh's
# digest, whose comment says what printing whatever the hash printed once cost.
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
if ! manifest=$(digest "$src/package.json") || ! lock=$(digest "$src/package-lock.json") ||
	! both=$(printf '%s\n%s\n' "$manifest" "$lock" | digest -); then
	echo "playwright-tree.sh: the machine's SHA-256 tool printed no digest of the manifest and the lock, which name the tree" >&2
	exit 2
fi

cache=${CREWLET_PLAYWRIGHT_TREE_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/crewlet/playwright-tree}
case $cache in
/*) ;;
*)
	echo "playwright-tree.sh: the tree cache $cache is not an absolute path (CREWLET_PLAYWRIGHT_TREE_CACHE, XDG_CACHE_HOME, HOME)" >&2
	exit 2
	;;
esac
name=$(printf '%s' "$both" | cut -c1-12)-l$layout
link=$cache/$name

# intact DIR: DIR is a whole tree of this lock, its npm cache included, which boxhelper's cases install from.
intact() {
	[ -d "$1" ] && [ -f "$1/.complete" ] && [ -f "$1/node_modules/.package-lock.json" ] &&
		[ -d "$1/npm-cache/_cacache" ] && cmp -s "$1/package-lock.json" "$src/package-lock.json"
}
if intact "$link"; then
	echo "$link"
	exit 0
fi

# What the install runs, refused before anything is fetched: an intact tree needs none of it.
missing=""
for tool in node npm; do
	command -v "$tool" >/dev/null 2>&1 || missing="$missing $tool"
done
if [ -n "$missing" ]; then
	echo "playwright-tree.sh: installing the Playwright tree needs:$missing — a current node with the npm it bundles (CONTRIBUTING.md, Development setup)" >&2
	exit 2
fi
# npm's answer is three numbers, then a prerelease's tag if any (12.0.0-pre.1), a version of its major as boxhelpertest
# reads one: ci.yml's `latest` installs whichever Node is newest, a prerelease and the npm it bundles included.
said=$(npm --version 2>/dev/null) || said=""
major=$(printf '%s\n' "$said" | grep -Ex '[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?' | sed 's/\..*//')
if [ -z "$major" ] || [ "$major" -lt "$min_npm" ]; then
	echo "playwright-tree.sh: the npm on PATH answered --version with \"$said\", and installing the tree takes npm $min_npm or newer, the npm the oldest Node the box helper supports bundles: install a current node with its own npm (a distribution's separate npm package may be older)" >&2
	exit 2
fi

# An interrupted install stops what it started: the watchdog, and npm, which a non-interactive sh starts with INT
# ignored, so a terminal's ^C does not stop it. npm is reaped before its directory goes, so nothing it was writing
# lands in a directory already removed.
build="" published="" npm_pid="" watchdog=""
cleanup() {
	[ -z "$watchdog" ] || kill -s TERM "$watchdog" 2>/dev/null || true
	if [ -n "$npm_pid" ]; then
		kill -s KILL "$npm_pid" 2>/dev/null || true
		wait "$npm_pid" 2>/dev/null || true
	fi
	[ -z "$build" ] || [ "$build" = "$published" ] || rm -rf "$build"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

mkdir -p "$cache"
build=$(mktemp -d "$cache/$name.XXXXXX")
chmod 755 "$build"
cp "$src/package.json" "$src/package-lock.json" "$build/"
echo "playwright-tree.sh: installing the Playwright tree into $build (about 4 MB)" >&2

# EVERYTHING THAT DECIDES WHAT IS INSTALLED IS ON THE COMMAND LINE, which beats the environment and every npmrc, so
# a developer's setting never changes the tree: a dry run or a lock-only run installs nothing; linked, bin links or
# a script would leave links or run code; global, in either spelling (global, location=global), and workspaces make
# `npm ci` refuse; offline fails on the empty cache; and `never` would send the lock's registry.npmjs.org addresses
# past the registry or mirror configured. No audit, fund or update notice: no request but the tarballs. No log file,
# which would land in the tree's npm cache, or be named by a failed install's last words after its directory went.
# npm's stdout goes to stderr, since this script's is the path, and the install runs under the bound above, with no
# child of its own to outlive it (--ignore-scripts).
npm ci --prefix "$build" --cache "$build/npm-cache" --logs-max=0 --ignore-scripts \
	--install-strategy=hoisted --no-bin-links --no-dry-run --no-package-lock-only --no-global --location=user \
	--no-workspaces --no-offline --replace-registry-host=npmjs --no-audit --no-fund --no-update-notifier \
	--fetch-timeout="$fetch_timeout_ms" --fetch-retries="$fetch_retries" \
	--fetch-retry-mintimeout="$fetch_retry_min_ms" --fetch-retry-maxtimeout="$fetch_retry_max_ms" </dev/null >&2 &
npm_pid=$!
# The bound's watchdog: TERM at the bound, KILL after the grace. Stopped, it stops its own sleep, whenever the TERM
# lands: stop holds it until the sleep's pid is known, so no install leaves a sleep running out the bound.
(
	nap="" stop=""
	trap 'stop=1; [ -z "$nap" ] || kill -s TERM "$nap" 2>/dev/null || :' TERM
	doze() { # SECONDS: sleeps them, failing once the watchdog is stopped
		sleep "$1" &
		nap=$!
		[ -z "$stop" ] || kill -s TERM "$nap" 2>/dev/null || :
		wait "$nap" || :
		nap=""
		[ -z "$stop" ]
	}
	doze "$bound" || exit 0
	: >"$build/.stopped"
	kill -s TERM "$npm_pid" 2>/dev/null || exit 0
	doze "$grace" || exit 0
	kill -s KILL "$npm_pid" 2>/dev/null || :
) </dev/null >/dev/null 2>&1 &
watchdog=$!
npm_status=0
wait "$npm_pid" || npm_status=$?
npm_pid=""
kill -s TERM "$watchdog" 2>/dev/null || true
wait "$watchdog" 2>/dev/null || true
watchdog=""
if [ -e "$build/.stopped" ]; then
	echo "playwright-tree.sh: npm had not installed the tree after $bound seconds, and was stopped: the registry or mirror it fetches from answers too slowly or not at all" >&2
	exit 1
fi
if [ "$npm_status" -ne 0 ]; then
	echo "playwright-tree.sh: npm ci failed (exit $npm_status), its own words above, so nothing is published" >&2
	exit 1
fi

# exactly DIR: npm's own record of what it installed in DIR names exactly the lock's packages, each at the lock's
# version, address and SHA-512, and DIR holds no link. npm answers 0 having installed nothing under a setting the
# command line missed, so its status alone proves nothing.
exactly() {
	[ -z "$(find "$1" -type l)" ] || {
		echo "playwright-tree.sh: the install left links in $1: $(find "$1" -type l | head -n 3 | tr '\n' ' ')" >&2
		return 1
	}
	node -e '
const { readFileSync } = require("node:fs");
const packages = (file) => JSON.parse(readFileSync(process.argv[1] + file, "utf8")).packages || {};
let want, got;
try {
  want = packages("/package-lock.json");
  got = packages("/node_modules/.package-lock.json");
} catch (e) {
  console.error("playwright-tree.sh: npm left no record of what it installed: " + e.message);
  process.exit(1);
}
delete want[""];
const odd = [...new Set([...Object.keys(want), ...Object.keys(got)])].filter((key) => !want[key] || !got[key] ||
  ["version", "resolved", "integrity"].some((field) => want[key][field] !== got[key][field]));
if (odd.length) {
  console.error("playwright-tree.sh: npm did not install exactly the lock: " + odd.sort().join(", "));
  process.exit(1);
}' "$1"
}
if ! exactly "$build"; then
	exit 1
fi
: >"$build/.complete"
if ! intact "$build"; then
	echo "playwright-tree.sh: the tree in $build is not whole after its install" >&2
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
		# An intact tree at the name — another installer's, published first — is never replaced: use it.
		rm -rf "$build"
		build=""
		break
	fi
	tries=$((tries + 1))
	if [ "$tries" -ge 3 ]; then
		echo "playwright-tree.sh: could not publish $build at $link: the name was taken by no intact tree three times" >&2
		exit 1
	fi
	rm -rf "$link"
done
[ -z "$build" ] || published=$build

# A crashed installer's leftovers: this name's directories the name does not point at, untouched for a day.
current=$(cd "$link" && pwd -P)
find "$cache" -maxdepth 1 -type d -name "$name.*" -mmin +1440 | while IFS= read -r stray; do
	[ "$(cd "$stray" && pwd -P)" = "$current" ] || rm -rf "$stray"
done
echo "$link"

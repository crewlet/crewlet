#!/bin/sh
#
# playwright-mcp.sh — fetch the Playwright tree scripts/playwright-mcp-pin.env pins, the @playwright/mcp and
# playwright-core npm tarballs, each checked against the pin's SHA-256, into internal/browser/boxhelper/pw/, which the
# box helper embeds: the ONE recipe for a bump (`make playwright-mcp`).
#
#   scripts/playwright-mcp.sh
#
# The tarballs are committed, so no build, test or gate runs it: a bump does, once it has replaced the pin's lines.
# It downloads both from the npm registry, refuses a tarball that is not the pin's, refuses an @playwright/mcp whose own
# package.json names any playwright-core but the pinned one, and only then writes both into pw/, removing every other
# *.tgz there, since what pw/ holds is what the helper embeds. Each is downloaded into a directory of its own inside
# pw/ and renamed into place, so pw/ never holds half a tarball, and a refused fetch leaves pw/ as it was.
#
# Prints the paths of the two tarballs it wrote and nothing else on stdout; a refusal and a fetch's failure go to
# stderr. Exits 0 with both written; 1 when a fetch failed: a download, a digest (a tarball whose SHA-256 the
# machine's hash could not compute among them), the package.json check, or the write; 2 when it was asked something it
# refuses — an argument, a pin file missing beside it or malformed, no internal/browser/boxhelper beside it to write
# into, or a tool the fetch needs and the machine lacks — before anything is downloaded.
#
# Environment (optional; the suite, scripts/playwright-mcp-pin_test.sh, points it at local files):
#   PLAYWRIGHT_MCP_PIN_REGISTRY  the registry the tarballs are read from, as <name>/-/<file>-<version>.tgz under it;
#                                https://registry.npmjs.org (curl reads file:// as well)
#
# ITS OWN SUITE is scripts/playwright-mcp-pin_test.sh (`make playwright-mcp-test`, in `make check` and ci.yml's `build +
# vet`): each refusal before any download, a tarball that is not the pin's or whose SHA-256 the machine cannot compute,
# an @playwright/mcp naming another core, a curl configuration that writes to stdout, and pw/ left whole by every
# refusal — each against a stand-in registry, so no case reads the network.
set -eu

[ $# -eq 0 ] || {
	echo "usage: scripts/playwright-mcp.sh" >&2
	exit 2
}

# The pin is read BESIDE this script, whatever CDPATH holds: scripts/git.sh says why, and the same `cd` is here.
unset CDPATH
here=$(cd "$(dirname "$0")" && pwd)
pin=$here/playwright-mcp-pin.env
if [ ! -r "$pin" ]; then
	echo "playwright-mcp.sh: $pin is missing: the recipe reads the pin beside itself" >&2
	exit 2
fi

# pinned NAME prints the one value $pin gives NAME, and refuses a name set never or twice, as scripts/chromium.sh reads
# its own pin.
pinned() {
	count=$(grep -c "^$1=" "$pin" || true)
	if [ "$count" -ne 1 ]; then
		echo "playwright-mcp.sh: $pin sets $1 $count times; it must set it once" >&2
		exit 2
	fi
	sed -n "s/^$1=//p" "$pin"
}

mcp=$(pinned PLAYWRIGHT_MCP_VERSION)
mcp_sha=$(pinned PLAYWRIGHT_MCP_SHA256)
core=$(pinned PLAYWRIGHT_CORE_VERSION)
core_sha=$(pinned PLAYWRIGHT_CORE_SHA256)
# @playwright/mcp releases three numbers; the playwright-core it names has been a release or a prerelease of one,
# -alpha- or -beta- and a timestamp or a date (1.66.0-alpha-2026-11-09). Each grammar admits nothing a path or a URL
# could read as more than a name.
if ! printf '%s\n' "$mcp" | grep -Eqx '[0-9]+\.[0-9]+\.[0-9]+'; then
	echo "playwright-mcp.sh: PLAYWRIGHT_MCP_VERSION in $pin is \"$mcp\", not a release (three numbers)" >&2
	exit 2
fi
if ! printf '%s\n' "$core" | grep -Eqx '[0-9]+\.[0-9]+\.[0-9]+(-(alpha|beta)(-[0-9]+)+)?'; then
	echo "playwright-mcp.sh: PLAYWRIGHT_CORE_VERSION in $pin is \"$core\", not a playwright-core release (three numbers, or three and -alpha- or -beta- with a timestamp or a date)" >&2
	exit 2
fi
for var in PLAYWRIGHT_MCP_SHA256 PLAYWRIGHT_CORE_SHA256; do
	case $var in
	PLAYWRIGHT_MCP_SHA256) sha=$mcp_sha ;;
	*) sha=$core_sha ;;
	esac
	if ! printf '%s\n' "$sha" | grep -Eqx '[0-9a-f]{64}'; then
		echo "playwright-mcp.sh: $var in $pin is not 64 lowercase hex digits" >&2
		exit 2
	fi
done

# The tree is written into the box helper's package of the checkout this recipe sits in, and nowhere else.
helper=$(dirname "$here")/internal/browser/boxhelper
if [ ! -d "$helper" ]; then
	echo "playwright-mcp.sh: $helper is missing: the recipe writes the tree into the box helper's package beside it" >&2
	exit 2
fi
pw=$helper/pw

# What the fetch runs, refused before anything is downloaded.
missing=""
for tool in curl gzip tar; do
	command -v "$tool" >/dev/null 2>&1 || missing="$missing $tool"
done
if [ -n "$missing" ]; then
	echo "playwright-mcp.sh: fetching the Playwright tree needs:$missing — CONTRIBUTING.md, Development setup, says what to install" >&2
	exit 2
fi
if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
	echo "playwright-mcp.sh: checking a tarball against its pin needs sha256sum, or shasum where that is absent — CONTRIBUTING.md, Development setup, says what to install" >&2
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

work=""
cleanup() {
	[ -z "$work" ] || rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

mkdir -p "$pw"
work=$(mktemp -d "$pw/.fetch.XXXXXX")
registry=${PLAYWRIGHT_MCP_PIN_REGISTRY:-https://registry.npmjs.org}
mcp_file=mcp-$mcp.tgz
core_file=playwright-core-$core.tgz

# fetched URL FILE SHA downloads FILE from URL and refuses it unless it is SHA. The download is scripts/chromium.sh's,
# for its reasons — given up on when it stalls, asked again on a transient failure, curl's stdout on stderr — and
# follows a redirect to an https:// address alone, as scripts/chromium-pin-check.sh follows one: a registry that sends
# a download to plain HTTP is one to stop at, whatever the digest would have said of the bytes.
fetched() {
	if ! curl -fsSL --proto-redir =https --retry 3 --connect-timeout 30 --speed-limit 1 --speed-time 60 \
		-o "$work/$2" "$1" >&2; then
		echo "playwright-mcp.sh: could not download $1" >&2
		exit 1
	fi
	if ! got=$(digest "$work/$2"); then
		echo "playwright-mcp.sh: cannot compute the SHA-256 of $2, so it cannot be checked against $pin" >&2
		exit 1
	fi
	if [ "$got" != "$3" ]; then
		echo "playwright-mcp.sh: $2 hashes to $got, and $pin pins $3" >&2
		exit 1
	fi
}
echo "playwright-mcp.sh: fetching @playwright/mcp $mcp and playwright-core $core from $registry" >&2
fetched "$registry/@playwright/mcp/-/$mcp_file" "$mcp_file" "$mcp_sha"
fetched "$registry/playwright-core/-/$core_file" "$core_file" "$core_sha"

# The core @playwright/mcp's own package.json names: the one dependency the server loads, which must be the pinned
# release exactly. Read with grep over the file with its blanks removed, which is enough for the one object npm writes
# there: `"dependencies":{…}` holds names and versions, none of which needs a brace or an escape, and a file holding
# no such object, two, or two playwright-core members in it, names nothing here.
gzip -dc "$work/$mcp_file" | tar -xOf - package/package.json 2>/dev/null | tr -d ' \t\r\n' >"$work/package.json" || true
dependencies=$(grep -o '"dependencies":{[^{}]*}' "$work/package.json" || true)
named=""
if [ "$(printf '%s' "$dependencies" | grep -c '^')" -eq 1 ]; then
	found=$(printf '%s\n' "$dependencies" | grep -o '"playwright-core":"[^"]*"' || true)
	if [ "$(printf '%s' "$found" | grep -c '^')" -eq 1 ]; then
		named=${found#\"playwright-core\":\"}
		named=${named%\"}
	fi
fi
if [ "$named" != "$core" ]; then
	echo "playwright-mcp.sh: $mcp_file's package.json names playwright-core \"$named\" among its dependencies, and $pin pins $core: the two lines move together, from scripts/playwright-mcp-pin-check.sh's report" >&2
	exit 1
fi

for file in "$mcp_file" "$core_file"; do
	if ! mv "$work/$file" "$pw/$file"; then
		echo "playwright-mcp.sh: could not write $pw/$file" >&2
		exit 1
	fi
done
for stale in "$pw"/*.tgz; do
	case ${stale##*/} in
	"$mcp_file" | "$core_file") ;;
	*)
		if ! rm -f "$stale"; then
			echo "playwright-mcp.sh: could not remove $stale, which the helper would embed beside the pin" >&2
			exit 1
		fi
		;;
	esac
done
echo "$pw/$mcp_file"
echo "$pw/$core_file"

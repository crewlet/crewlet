#!/bin/sh
#
# chromium-pin-check.sh — hold the Chromiums scripts/chromium-pin.env pins to Chrome for Testing's releases, every
# week, because no Dependabot ecosystem watches a Chrome for Testing build: without it the pin would be watched by
# nothing, which looks exactly like a pin with nothing to update.
#
#   scripts/chromium-pin-check.sh    is each pin the release its rule names?
#
# THE PINS. The newest pin follows Chrome for Testing's Stable channel (LATEST_RELEASE_STABLE), and is behind only
# when Stable names a newer MAJOR: a patch changes no protocol, and the suite drives only pages it serves on loopback,
# so following patches would open the issue every week for nothing. The floor pin is the newest patch of its own major
# (LATEST_RELEASE_<major>, the major its version names): a floor build stops getting patches within weeks of its
# major's release, so it settles. For every pin that is behind, the check downloads the release's zip for each of the
# four platforms the engine is released for — linux64, linux-arm64, mac-x64 and mac-arm64 — computes each one's
# SHA-256, and prints the pin's new lines with the pull request that moves it. A release missing any of the four is
# never proposed: a pin the suite cannot fetch on a supported platform is a gate that platform skips.
#
# THE TRUST ANCHOR IS THE DOWNLOAD. No signature exists for a Chrome for Testing build, unlike git's signed tarballs, so
# what a digest rests on is Google's bucket served over HTTPS, read here, and the reviewed pull request that pastes it;
# every later fetch must match it (scripts/chromium.sh). So every fetch here follows a redirect to an https:// address
# alone, where curl by default follows one to plain http:// too: a digest computed over bytes no TLS protected would
# rest on nothing. And a listing's answer is refused unless it is a version of four numbers BEFORE it reaches a URL, a
# file name or the report, so nothing a listing says, other than a version, reaches the report or a path either
# script builds; a refusal says how long the answer was and never what it said.
#
# Exit status: 0 when every pin is current; 1 when a pin is behind — its new lines on stdout, every SHA-256 that of a
# zip downloaded and hashed here; 2 when it cannot tell: a listing refused or answering anything but a version, a
# release missing a platform's zip (the bucket answering 404 or 403 for it), a zip that could not be downloaded for any
# other reason (named as that, with curl's exit status and the HTTP status, never as a missing build), a pin newer than
# what the listing names (a listing that is behind is not upstream), or a zip whose SHA-256 this machine cannot
# compute. 2 is never a pass.
#
# Its downloads go into a directory of its own in TMPDIR, one zip at a time, each removed once hashed: about 200 MB
# at once, which a machine whose /tmp is a small tmpfs may not have, so point TMPDIR elsewhere there.
#
# Environment (each optional; the suite, scripts/chromium-pin_test.sh, points them at local files):
#   CHROMIUM_PIN_LISTING     the release listing; https://googlechromelabs.github.io/chrome-for-testing
#   CHROMIUM_PIN_DOWNLOADS   where the zips are read from, as scripts/chromium.sh reads them;
#                            https://storage.googleapis.com/chrome-for-testing-public (curl reads file:// as well)
set -eu

[ $# -eq 0 ] || {
	echo "usage: scripts/chromium-pin-check.sh" >&2
	exit 2
}

# The script's own directory, whatever CDPATH holds: scripts/git-pin-check.sh says why.
unset CDPATH
here=$(cd "$(dirname "$0")" && pwd)
pin=$here/chromium-pin.env
listing=${CHROMIUM_PIN_LISTING:-https://googlechromelabs.github.io/chrome-for-testing}
downloads=${CHROMIUM_PIN_DOWNLOADS:-https://storage.googleapis.com/chrome-for-testing-public}
platforms="linux64 linux-arm64 mac-x64 mac-arm64"

fail() { # <message>: cannot tell
	echo "chromium-pin-check.sh: $1" >&2
	exit 2
}

command -v curl >/dev/null 2>&1 || fail "it needs curl, which this machine lacks"
if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
	fail "it needs sha256sum, or shasum where that is absent, which this machine lacks"
fi

work=""
cleanup() {
	[ -z "$work" ] || rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
work=$(mktemp -d)

# pinned NAME prints the one value the pin file gives NAME, refusing a name set never or twice, as scripts/chromium.sh
# reads it.
pinned() {
	count=$(grep -c "^$1=" "$pin" || true)
	[ "$count" -eq 1 ] || fail "$pin sets $1 $count times; it must set it once"
	sed -n "s/^$1=//p" "$pin"
}

# release VALUE: VALUE is a Chrome for Testing version, four numbers and nothing else.
release() {
	case $1 in
	'' | *[!0-9.]*) return 1 ;;
	esac
	printf '%s\n' "$1" | grep -Eqx '[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+'
}

# digest FILE prints FILE's SHA-256 and fails rather than print anything else, scripts/git-pin-check.sh's rule.
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

# fetch URL FILE downloads one file with scripts/git-pin-check.sh's bounds and for its reasons: given up on when it
# stalls, asked again on a transient failure, and curl's stdout on stderr, where a curlrc's `write-out` cannot reach
# the report. A redirect it follows to an https:// address alone (--proto-redir; the header says why), and one to any
# other is a download that failed. It answers curl's exit status, and leaves it in $curled and the HTTP status the
# transfer ended on in $http — read from the headers curl dumps into a file of the work directory, never from its
# stdout — so a failure can say which it was; $http is empty where there was none (a file:// address, or no answer at
# all).
fetch() {
	rm -f "$work/headers"
	if curl -fsSL --proto-redir =https --retry 3 --connect-timeout 30 --speed-limit 1 --speed-time 60 \
		-D "$work/headers" -o "$2" "$1" >&2; then
		curled=0
	else
		curled=$?
	fi
	http=$(sed -n 's/^HTTP\/[^ ]* \([0-9][0-9][0-9]\).*/\1/p' "$work/headers" 2>/dev/null | tail -n 1)
	return "$curled"
}

# fetched URL: why the last fetch of URL failed, for a refusal: curl's exit status and the HTTP status it ended on.
fetched() {
	echo "$1 (curl exit $curled, HTTP ${http:-none})"
}

# latest NAME prints the version the listing's LATEST_RELEASE_<NAME> names, refusing an answer that is no version
# before it is used: its length is all a refusal says of it.
latest() {
	file=$work/LATEST_RELEASE_$1
	fetch "$listing/LATEST_RELEASE_$1" "$file" ||
		fail "cannot read the release listing $(fetched "$listing/LATEST_RELEASE_$1")"
	size=$(wc -c <"$file" | tr -d ' ')
	answer=""
	[ "$size" -gt 64 ] || answer=$(cat "$file")
	release "$answer" ||
		fail "$listing/LATEST_RELEASE_$1 answered $size bytes that are not a Chrome for Testing version (four numbers); nothing of it is used"
	printf '%s\n' "$answer"
}

# vsort sorts versions of four numbers, numerically and field by field: POSIX sort, since `sort -V` is not.
vsort() {
	sort -u -t. -k1,1n -k2,2n -k3,3n -k4,4n
}

# newer A B: version A is newer than version B.
newer() {
	[ "$1" != "$2" ] && [ "$(printf '%s\n%s\n' "$1" "$2" | vsort | tail -n 1)" = "$1" ]
}

# lines VAR VERSION appends the pin's new lines to the report: its version and, for each platform, the SHA-256 of that
# platform's zip of VERSION, downloaded one at a time and removed once hashed.
lines() {
	var=$1 v=$2
	echo "${var}_VERSION=$v" >>"$work/lines"
	for platform in $platforms; do
		zip=chrome-$platform.zip
		if ! fetch "$downloads/$v/$platform/$zip" "$work/$zip"; then
			# A build that is not there is the bucket's 404 (or its 403, which it answers for an object it does not
			# hold where listing is not allowed), or curl's 37 for a file:// tree; anything else — a server error
			# that outlasted the retries, a stall, a full disk — is a download that failed, never a missing build.
			case $curled:$http in
			22:404 | 22:403 | 37:*)
				fail "Chrome for Testing $v has no $platform build at $downloads/$v/$platform/$zip, and a release missing any of linux64, linux-arm64, mac-x64 and mac-arm64 is never proposed"
				;;
			esac
			fail "cannot download $(fetched "$downloads/$v/$platform/$zip"), so whether Chrome for Testing $v has a $platform build is not known"
		fi
		sum=$(digest "$work/$zip") || fail "cannot compute the SHA-256 of $zip of $v"
		rm -f "$work/$zip"
		echo "${var}_SHA256_$(printf '%s' "$platform" | tr 'a-z-' 'A-Z_')=$sum" >>"$work/lines"
	done
}

behind=0
: >"$work/report"
propose() { # <name> <var> <current> <target> <title>: a pin behind, with its new lines and pull request
	behind=$((behind + 1))
	: >"$work/lines"
	lines "$2" "$4"
	{
		echo "# $1: $3 -> $4, in a pull request titled \`$5\`"
		cat "$work/lines"
		echo
	} >>"$work/report"
}

newest=$(pinned CHROMIUM_NEWEST_VERSION)
floor=$(pinned CHROMIUM_FLOOR_VERSION)
release "$newest" || fail "CHROMIUM_NEWEST_VERSION in $pin is \"$newest\", not a Chrome for Testing version (four numbers)"
release "$floor" || fail "CHROMIUM_FLOOR_VERSION in $pin is \"$floor\", not a Chrome for Testing version (four numbers)"

# The newest pin against Stable, by major.
stable=$(latest STABLE)
if [ "${stable%%.*}" -gt "${newest%%.*}" ]; then
	propose newest CHROMIUM_NEWEST "$newest" "$stable" "build(deps): bump chromium from $newest to $stable"
elif [ "${stable%%.*}" -lt "${newest%%.*}" ]; then
	fail "CHROMIUM_NEWEST_VERSION=$newest is of a newer major than Stable, $stable, as $listing names it: that listing is behind upstream"
fi

# The floor pin against the newest patch of its own major.
patch=$(latest "${floor%%.*}")
[ "${patch%%.*}" = "${floor%%.*}" ] ||
	fail "$listing/LATEST_RELEASE_${floor%%.*} names $patch, which is not of the floor's major, ${floor%%.*}"
if newer "$patch" "$floor"; then
	propose floor CHROMIUM_FLOOR "$floor" "$patch" "build(deps): bump chromium's floor from $floor to $patch"
elif newer "$floor" "$patch"; then
	fail "CHROMIUM_FLOOR_VERSION=$floor is newer than $patch, the newest of its major $listing names: that listing is behind upstream"
fi

if [ "$behind" -gt 0 ]; then
	cat <<EOF
scripts/chromium-pin.env is behind Chrome for Testing ($listing). Each zip below was downloaded from $downloads
and its SHA-256 computed from it: no signature exists for a Chrome for Testing build, so the pull request that pastes
these lines is where they are reviewed, and every later fetch must match them.

EOF
	cat "$work/report"
	cat <<EOF
A bump replaces a pin's lines in scripts/chromium-pin.env, runs \`make chromium\` (it fetches the new build and checks
it against the digest) and \`make check\`, and lands as its own pull request under the title
above. The floor only moves to a newer patch of its major.
EOF
	exit 1
fi
echo "every pin is the release its rule names: newest $newest (Stable is $stable), floor $floor"

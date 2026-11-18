#!/bin/sh
#
# node-pin-check.sh — hold the floor's Node, which scripts/node-pin.env pins, to the newest release of its major and
# that major to its support window, and the keys that verify Node's releases to their expiry. .github/workflows/pins.yml
# runs both every week, because no Dependabot ecosystem watches a Node release: without it the pin would be watched by
# nothing, which looks exactly like a pin with nothing to update.
#
#   scripts/node-pin-check.sh        is the floor the newest release of its major, and is that major supported?
#   scripts/node-pin-check.sh key    does every release key stay valid for more than 60 days?
#
# THE PIN is the newest release of Node internal/browser/boxhelper.MinNodeMajor, read from nodejs.org's release index
# (index.tab) — a version of three numbers, so a prerelease, a nightly or anything else the index lists is never one.
# And the major is held to its support window: from 60 days before its end of life (nodejs/Release's schedule.json),
# the check fails as it does for a pin behind, naming the next even major's newest release — the next line Node
# supports for long — since a floor past its end of life is a floor on a Node nobody patches. Raising the floor is a
# change to boxhelper.MinNodeMajor, so the report names it. For every release it proposes, the check downloads that
# release's SHASUMS256.txt and its signature (SHASUMS256.txt.sig), verifies the signature with the releasers' keys
# beside this script, then downloads each platform's tarball and requires its SHA-256 to be the one the signed list
# names — so every SHA-256 in the report is that of a tarball downloaded here AND signed for by a releaser.
#
# THE KEYS ARE THE PINNED ONES AND NOTHING ELSE, as scripts/git-pin-check.sh holds the git maintainer's: any of Node's
# releasers signs a release, so the pin is their set. scripts/node-release-keys.asc is imported into a GnuPG home made
# for this run — never the caller's own — and refused unless that keyring then holds exactly the primary keys whose
# fingerprints are pinned below, no more and no fewer. A signature by a key it does not hold is refused naming that
# key: a releaser added to nodejs/release-keys after the file was refreshed, a signing subkey newer than the file, or
# nobody who releases Node, which only a refresh from upstream tells apart.
#
# THE KEYS' OWN EXPIRY, CHECKED IN BAND. A releaser's key or its signing subkey expires, and a release signed after
# that by a key this file holds only in its expired form — its releaser having extended it since — fails to verify, so
# every later run would fail with exit 2. `key` reads, for every pinned key, when it stops signing — by
# scripts/git-pin-check.sh's rule: the latest expiry among its valid signing subkeys, or the primary's own where it has
# none, never later than the primary's — and exits 1 when any key's date is within 60 days, eight weekly runs to
# refresh the file, naming each such key and how. A key its releaser lets lapse is dropped at that refresh: a release it
# signs later is refused by name, which the refresh then answers.
#
# A LISTING'S ANSWER IS A VERSION OR A DATE, OR NOTHING. Of the index only lines whose version is three numbers are
# read, and of the schedule only an end of life of the form YYYY-MM-DD, each before it reaches a URL, a file name, a
# command or the report; a refusal never says what was there.
#
# Exit status: 0 when the floor is the newest release of its major and that major's end of life is more than 60 days
# away (or, for `key`, when every key stays valid past 60 days); 1 when the pin is behind or its major is within 60
# days of its end of life — the new lines on stdout, each release's signature verified (or, for `key`, when a key
# stops signing within 60 days); 2 when it cannot tell: the index, the schedule or a download refused, no release of
# the major the index should name, a pin NEWER than the index's newest (an index that is behind is not upstream), an
# end of life that is no date, a signature that does not verify or was made by a key the key file does not hold or by
# one that has expired, a tarball that is not the one the signed list names or a platform it lists none for, a key
# file that is not the pinned keys, or a tarball whose SHA-256 this machine cannot compute. 2 is never a pass, and the
# workflow leaves its issue alone on it.
#
# Its downloads go into a directory of its own in TMPDIR, one tarball at a time, each removed once hashed: about 31 MB
# at once.
#
# Environment (each optional; the suite, scripts/node-pin_test.sh, points them at local files):
#   NODE_PIN_DIST          the release index and downloads, as scripts/node.sh reads them; https://nodejs.org/dist
#                          (curl reads file:// as well)
#   NODE_PIN_SCHEDULE      the release schedule; https://raw.githubusercontent.com/nodejs/Release/main/schedule.json
#   NODE_PIN_FINGERPRINTS  the primary fingerprints the key file must hold, separated by blanks; the releasers'
set -eu

# The Node.js releasers' primary keys, which sign every release (some through a signing subkey): the active releasers
# nodejs/release-keys lists ("Active releasers keys" in its README, which nodejs/node's README lists too), read
# 2026-11-18 — Antoine du Hamel, Juan José Arboleda, Marco Ippolito, Rafael Gonzaga, Richard Lau, Ruy Adorno, Stewart X
# Addison and Ulises Gascón. The retired keys upstream keeps beside them signed releases of older lines, and the check
# verifies only releases newer than the pin.
releasers="5BE8A3F6C8A5C01D106C0AD820B1A390B168D356 DD792F5973C6DE52C432CBDAC77ABFA00DDBF2B7
CC68F5A3106FF448322E48ED27F5E38D5B0A215F 890C08DB8579162FEE0DF9DB8BEAB4DFCF555EF4
C82FA3AE1CBEDC6BE46B9360C43CEC45C17AB93C 108F52B48DB57BB0CC439B2997B01419BD92F80A
655F3B5C1FB3FA8D1A0CA6BDE4A7D232B936D2FD A363A499291CBBC940DD62E41F10027AF002F8B0"
# The warning window, for the keys' expiry and for the floor's end of life alike: scripts/git-pin-check.sh's, eight
# weekly runs of the workflow, each a failed run naming what to do.
warn_days=60
platforms="linux-x64 linux-arm64 darwin-x64 darwin-arm64"

usage() {
	echo "usage: scripts/node-pin-check.sh [key]" >&2
	exit 2
}
case $#:${1-} in
0:) mode=pins ;;
1:key) mode=key ;;
*) usage ;;
esac

# The script's own directory, whatever CDPATH holds: scripts/git-pin-check.sh says why.
unset CDPATH
here=$(cd "$(dirname "$0")" && pwd)
pin=$here/node-pin.env
keyfile=$here/node-release-keys.asc
fingerprints=${NODE_PIN_FINGERPRINTS:-$releasers}
dist=${NODE_PIN_DIST:-https://nodejs.org/dist}
schedule=${NODE_PIN_SCHEDULE:-https://raw.githubusercontent.com/nodejs/Release/main/schedule.json}

fail() { # <message>: cannot tell
	echo "node-pin-check.sh: $1" >&2
	exit 2
}

need="gpg gpgconf"
[ "$mode" = key ] || need="$need curl"
for tool in $need; do
	command -v "$tool" >/dev/null 2>&1 || fail "it needs $tool, which this machine lacks"
done
if [ "$mode" = pins ] && ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
	fail "it needs sha256sum, or shasum where that is absent, which this machine lacks"
fi

work=""
cleanup() {
	if [ -n "$work" ]; then
		# The agents a GnuPG home starts outlive the command that started them: scripts/git-pin-check.sh says why
		# they are stopped before their home goes.
		GNUPGHOME=$work/gnupg gpgconf --kill all >/dev/null 2>&1 || true
		rm -rf "$work"
	fi
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
work=$(mktemp -d)
mkdir -m 700 "$work/gnupg"
GNUPGHOME=$work/gnupg
export GNUPGHOME

# gpg runs gpg in this run's home alone: batch, no terminal, no key fetched from anywhere.
gpg_() {
	gpg --batch --no-tty --no-auto-key-retrieve "$@"
}

refresh_steps() {
	cat <<EOF
To refresh them: read the active releasers' keys from https://github.com/nodejs/release-keys (its README's "Active
releasers keys", which the "Release keys" section of nodejs/node's README must list too), put their keys/<fingerprint>.asc
files in scripts/node-release-keys.asc and their fingerprints in the releasers scripts/node-pin-check.sh pins —
leaving out a key upstream no longer lists, or one that has lapsed and that its releaser has not extended — run
scripts/node-pin-check.sh (and \`key\`) again with the new file, and land it as \`build(deps): refresh node's release
signing keys\`.
EOF
}

# The key file, imported and held to the pinned fingerprints: exactly those primary keys.
if ! gpg_ --quiet --import "$keyfile" >"$work/import.log" 2>&1; then
	cat "$work/import.log" >&2
	fail "cannot import $keyfile"
fi
gpg_ --with-colons --fixed-list-mode --list-keys >"$work/keys" 2>/dev/null || fail "cannot list the keys imported from $keyfile"
awk -F: '$1 == "pub" { want = 1; next } want && $1 == "fpr" { print $10; want = 0 }' "$work/keys" | LC_ALL=C sort >"$work/held"
printf '%s\n' "$fingerprints" | tr -s ' \t' '\n\n' | sed '/^$/d' | LC_ALL=C sort -u >"$work/pinned"
if ! cmp -s "$work/held" "$work/pinned"; then
	extra=$(LC_ALL=C comm -23 "$work/held" "$work/pinned" | tr '\n' ' ' | sed 's/ $//')
	lacking=$(LC_ALL=C comm -13 "$work/held" "$work/pinned" | tr '\n' ' ' | sed 's/ $//')
	differ="${extra:+it holds $extra beside them}${extra:+${lacking:+; }}${lacking:+it lacks $lacking}"
	fail "$keyfile must hold exactly the primary keys pinned in scripts/node-pin-check.sh: $differ"
fi

# epoch DATE: the seconds of DATE's midnight, UTC (GNU date, then BSD date); day EPOCH: that instant's date.
epoch() {
	date -u -d "$1 00:00:00" +%s 2>/dev/null || date -u -j -f '%Y-%m-%d %H:%M:%S' "$1 00:00:00" +%s 2>/dev/null
}
day() {
	date -u -d "@$1" +%Y-%m-%d 2>/dev/null || date -u -r "$1" +%Y-%m-%d
}
now=$(date -u +%s)
limit=$((now + warn_days * 86400))

if [ "$mode" = key ]; then
	# When each key stops signing, by scripts/git-pin-check.sh's rule (its comment reads the fields), one line a key:
	# its fingerprint, then the time, `never` or `none`, then its first user ID that is not revoked. A primary that is
	# itself revoked, invalid or disabled signs nothing at all, and one that has expired signs only through a subkey,
	# which its own expiry then bounds.
	awk -F: '
		function later(a, b) { return (a == "never" || b == "never") ? "never" : (a + 0 > b + 0 ? a : b) }
		function at(field) { return field == "" ? "never" : field }
		function flush(  stops) {
			if (fpr == "") return
			if (dead) stops = "none"
			else if (subkeys == 0) stops = primary_signs ? primary : "none"
			else if (valid == 0) stops = "none"
			else stops = latest
			if (stops != "none" && primary != "never" && (stops == "never" || primary + 0 < stops + 0)) stops = primary
			print fpr, stops, uid
		}
		$1 == "pub" {
			flush()
			fpr = ""; uid = ""; subkeys = 0; valid = 0; latest = ""; want = 1
			primary = at($7)
			dead = ($2 ~ /[rid]/)
			primary_signs = ($12 ~ /s/ && $2 !~ /e/)
			next
		}
		want && $1 == "fpr" { fpr = $10; want = 0; next }
		$1 == "uid" && uid == "" && $2 !~ /r/ { uid = $10 }
		$1 == "sub" && $12 ~ /s/ {
			subkeys++
			if ($2 !~ /[erid]/) { valid++; latest = (valid == 1) ? at($7) : later(latest, at($7)) }
		}
		END { flush() }' "$work/keys" >"$work/stops"
	soon=0 soonest="" soonest_key=""
	while read -r fpr stops uid; do
		case $stops in
		never) continue ;;
		none)
			echo "the release key $fpr ($uid) has no valid signing key left: a release it signs from now on fails to verify." >&2
			soon=$((soon + 1))
			continue
			;;
		esac
		if [ "$stops" -le "$limit" ]; then
			echo "the release key $fpr ($uid) signs until $(day "$stops"), $(((stops - now) / 86400)) day(s) from now — within the ${warn_days}-day warning: a release it signs after that, with the key extended, cannot be verified with the file's copy." >&2
			soon=$((soon + 1))
		elif [ -z "$soonest" ] || [ "$stops" -lt "$soonest" ]; then
			soonest=$stops soonest_key=$fpr
		fi
	done <"$work/stops"
	if [ "$soon" -gt 0 ]; then
		refresh_steps >&2
		exit 1
	fi
	if [ -z "$soonest" ]; then
		echo "no release key expires"
	else
		echo "every release key signs past the ${warn_days}-day warning: the soonest to stop, $soonest_key, signs until $(day "$soonest"), $(((soonest - now) / 86400)) days from now"
	fi
	exit 0
fi

# pinned NAME prints the one value the pin file gives NAME, refusing a name set never or twice, as scripts/node.sh
# reads it.
pinned() {
	count=$(grep -c "^$1=" "$pin" || true)
	[ "$count" -eq 1 ] || fail "$pin sets $1 $count times; it must set it once"
	sed -n "s/^$1=//p" "$pin"
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

# fetch URL FILE downloads one file with scripts/chromium-pin-check.sh's bounds and for its reasons: given up on when
# it stalls, asked again on a transient failure, a redirect followed to an https:// address alone (a digest computed
# over bytes no TLS protected would rest on nothing but the signature), and curl's stdout on stderr, where a curlrc's
# `write-out` cannot reach the report.
fetch() {
	curl -fsSL --proto-redir =https --retry 3 --connect-timeout 30 --speed-limit 1 --speed-time 60 -o "$2" "$1" >&2
}

# vsort sorts versions of three numbers, numerically and field by field: POSIX sort, since `sort -V` is not.
vsort() {
	sort -u -t. -k1,1n -k2,2n -k3,3n
}

current=$(pinned NODE_FLOOR_VERSION)
printf '%s\n' "$current" | grep -Eqx '[0-9]+\.[0-9]+\.[0-9]+' ||
	fail "NODE_FLOOR_VERSION in $pin is \"$current\", not a Node release (three numbers)"
major=${current%%.*}

# The index's releases: the first field of each line whose version is three numbers, oldest first.
fetch "$dist/index.tab" "$work/index.tab" || fail "cannot read the release index $dist/index.tab"
awk -F'\t' '{ print $1 }' "$work/index.tab" | grep -Ex 'v[0-9]+\.[0-9]+\.[0-9]+' | sed 's/^v//' | vsort >"$work/releases" || true

# newest MAJOR prints the index's newest release of that major, or nothing.
newest() {
	grep "^$1\." "$work/releases" | tail -n 1 || true
}

# The floor's end of life, refused unless it is a date: the schedule's one entry for the major, read with its blanks
# removed, whose members hold no brace.
fetch "$schedule" "$work/schedule.json" || fail "cannot read the release schedule $schedule"
entry=$(tr -d ' \t\r\n' <"$work/schedule.json" | grep -o "\"v$major\":{[^{}]*}" || true)
[ "$(printf '%s' "$entry" | grep -c '^')" -eq 1 ] || fail "the release schedule $schedule gives no one entry for Node $major"
end=$(printf '%s\n' "$entry" | grep -o '"end":"[^"]*"' || true)
[ "$(printf '%s' "$end" | grep -c '^')" -eq 1 ] || fail "the release schedule's entry for Node $major gives no one end of life"
end=${end#\"end\":\"}
end=${end%\"}
printf '%s\n' "$end" | grep -Eqx '[0-9]{4}-[0-9]{2}-[0-9]{2}' && ended=$(epoch "$end") ||
	fail "the release schedule's end of life for Node $major is no date of the form YYYY-MM-DD; nothing of it is used"

# verified VERSION downloads that release's SHASUMS256.txt and its signature once, verifies the signature with the
# pinned keys, and prints the fingerprint of the primary key that made it.
verified() {
	v=$1
	for file in SHASUMS256.txt SHASUMS256.txt.sig; do
		fetch "$dist/v$v/$file" "$work/v$v.$file" || fail "cannot download $dist/v$v/$file"
	done
	# GOODSIG, never only a zero status: scripts/git-pin-check.sh says why. Since the keyring holds the pinned keys
	# alone, a good signature is a releaser's.
	if ! gpg_ --status-file "$work/v$v.status" --verify "$work/v$v.SHASUMS256.txt.sig" "$work/v$v.SHASUMS256.txt" \
		>"$work/v$v.verify" 2>&1 || ! grep -q '^\[GNUPG:\] GOODSIG ' "$work/v$v.status"; then
		cat "$work/v$v.verify" >&2
		if grep -q '^\[GNUPG:\] EXPKEYSIG ' "$work/v$v.status"; then
			echo "node-pin-check.sh: the SHASUMS256.txt of v$v was signed by a releaser's key that has expired in scripts/node-release-keys.asc: the file needs refreshing." >&2
			refresh_steps >&2
			exit 2
		fi
		# ERRSIG names the signer's fingerprint where the signature carries it, NO_PUBKEY its key id alone; either is
		# hex by gpg's own printing, and nothing else of it is said.
		signer=$(awk '$1 == "[GNUPG:]" && $2 == "ERRSIG" && $9 != "" && $9 != "-" { print $9; exit }' "$work/v$v.status")
		[ -n "$signer" ] || signer=$(awk '$1 == "[GNUPG:]" && $2 == "NO_PUBKEY" { print $3; exit }' "$work/v$v.status")
		signer=$(printf '%s' "$signer" | tr -cd '0-9A-Fa-f')
		if [ -n "$signer" ] && grep -q '^\[GNUPG:\] NO_PUBKEY ' "$work/v$v.status"; then
			echo "node-pin-check.sh: the SHASUMS256.txt of v$v was signed by $signer, a key scripts/node-release-keys.asc does not hold: a releaser nodejs/release-keys added after the file was refreshed, a signing subkey of a pinned key newer than the file, or nobody who releases Node." >&2
			refresh_steps >&2
			echo "If the refreshed file still does not hold $signer, Node's releasers did not sign v$v: take nothing from it." >&2
			exit 2
		fi
		fail "the SHASUMS256.txt of v$v does not verify with the releasers' keys (scripts/node-release-keys.asc)"
	fi
	awk '$1 == "[GNUPG:]" && $2 == "VALIDSIG" { print $12; exit }' "$work/v$v.status" | tr -cd '0-9A-Fa-f'
}

# lines VERSION SIGNER appends the pin's new lines to the report: the version and, for each platform, the SHA-256 of
# its tarball, downloaded one at a time, required to be the one the signed list names, and removed once hashed.
lines() {
	v=$1
	echo "NODE_FLOOR_VERSION=$v" >>"$work/lines"
	escaped=$(printf '%s' "$v" | sed 's/\./\\./g')
	for platform in $platforms; do
		tarball=node-v$v-$platform.tar.xz
		listed=$(grep -E "^[0-9a-f]{64}  node-v$escaped-$platform\.tar\.xz\$" "$work/v$v.SHASUMS256.txt" || true)
		[ "$(printf '%s' "$listed" | grep -c '^')" -eq 1 ] ||
			fail "the signed SHASUMS256.txt of v$v lists no one $tarball, and a release missing any of $platforms is never proposed"
		fetch "$dist/v$v/$tarball" "$work/$tarball" || fail "cannot download $dist/v$v/$tarball"
		sum=$(digest "$work/$tarball") || fail "cannot compute the SHA-256 of $tarball"
		rm -f "$work/$tarball"
		[ "$sum" = "${listed%% *}" ] ||
			fail "$tarball hashes to $sum, and the SHASUMS256.txt Node's releasers signed for v$v names ${listed%% *}: it is not the tarball they released"
		echo "NODE_FLOOR_SHA256_$(printf '%s' "$platform" | tr 'a-z-' 'A-Z_')=$sum" >>"$work/lines"
	done
}

behind=0
: >"$work/report"
propose() { # <version> <what> <title>: a release to move the floor to, verified, with its lines and pull request
	signer=$(verified "$1")
	: >"$work/lines"
	lines "$1"
	behind=$((behind + 1))
	{
		echo "# $2, signed by $signer, in a pull request titled \`$3\`"
		cat "$work/lines"
		echo
	} >>"$work/report"
}

# The floor against the newest release of its major.
patch=$(newest "$major")
[ -n "$patch" ] || fail "the release index $dist/index.tab names no release of Node $major, the floor's major"
if [ "$(printf '%s\n%s\n' "$current" "$patch" | vsort | tail -n 1)" != "$patch" ]; then
	fail "NODE_FLOOR_VERSION=$current is newer than $patch, the newest release of Node $major the index $dist/index.tab names: that index is behind upstream"
fi
if [ "$patch" != "$current" ]; then
	propose "$patch" "floor: $current -> $patch, the newest release of Node $major" \
		"build(deps): bump node's floor from $current to $patch"
fi

# The floor's major against its end of life.
days=$(((ended - now) / 86400))
if [ "$ended" -le "$limit" ]; then
	next=$((major + 2 - major % 2))
	successor=$(newest "$next")
	[ -n "$successor" ] || fail "Node $major's end of life is $end, and the release index $dist/index.tab names no release of Node $next, the floor's next even major"
	if [ "$ended" -le "$now" ]; then
		when="reached its end of life on $end"
	else
		when="reaches its end of life on $end, $days day(s) from now"
	fi
	propose "$successor" "floor: Node $major $when: move it to Node $next, whose newest release is $successor" \
		"build(deps): raise node's floor from $major to $next"
	echo "Raising the floor is not a bump: it changes internal/browser/boxhelper.MinNodeMajor from $major to $next, the oldest Node a box must carry, in the same pull request (TestTheFloorNodeIsTheMajorItStandsFor fails while the two disagree)." >>"$work/report"
	echo >>"$work/report"
fi

if [ "$behind" -gt 0 ]; then
	cat <<EOF
scripts/node-pin.env is behind upstream ($dist). Each release below had its SHASUMS256.txt verified against the
Node.js releasers' keys (scripts/node-release-keys.asc), and each SHA-256 is that of the tarball downloaded from it,
the one that signed list names.

EOF
	cat "$work/report"
	cat <<EOF
A bump replaces the pin's lines in scripts/node-pin.env, runs \`make node\` (it fetches the new build and refuses a
tarball that is not its digest) and \`make check\`, and lands as its own pull request under the title above.
EOF
	exit 1
fi
echo "the floor is the newest release of Node $major: $current; Node $major's end of life is $end, $days days from now"

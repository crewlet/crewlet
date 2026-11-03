#!/bin/sh
#
# git-pin-check.sh — hold the gits scripts/git-pin.env pins to upstream's releases, and the key that verifies
# those releases to its expiry. .github/workflows/git-pin.yml runs both every week, because no Dependabot ecosystem
# watches a source tarball: without it the pin would be watched by nothing, which looks exactly like a pin with
# nothing to update.
#
#   scripts/git-pin-check.sh        is every pin the newest release of its series?
#   scripts/git-pin-check.sh key    does the release signing key stay valid for more than 60 days?
#
# THE PINS. Each pin names a release of one series, and the check reads kernel.org's listing for the newest release
# of that series — a version of three numbers, so a release candidate (git-2.57.0.rc1, which kernel.org keeps under
# testing/ these days and once kept beside the releases) is never one. The node pin's series is every release: it
# is the newest git there is, what the engine image builds. Each floor's series is the floor pin's own major.minor:
# a floor's pin is the newest PATCH of the release the floor names, so a security fix to 2.40 is one the client
# floor's arm runs, and 2.41 is not the client floor. For every pin that is behind, it downloads the newer release's
# tarball and its signature (git-<version>.tar.sign, made over the DECOMPRESSED tar), verifies the signature with
# the key beside this script, and prints the pin's two new lines — its version and the SHA-256 of the very tarball
# whose signature verified — with the pull request that moves it. So the issue the workflow opens names digests a
# person can paste, never one taken on trust from a listing.
#
# THE KEY IS THE PINNED ONE AND NOTHING ELSE. scripts/git-signing-key.asc is the public key of the git maintainer,
# who signs every release, and its primary fingerprint is pinned below. The file is imported into a GnuPG home
# made for this run — never the caller's own, whose keyring could hold any key at all — and refused unless that
# keyring then holds exactly one primary key, the pinned one. So a signature can verify only if the pinned key or
# one of its subkeys made it, and a key file somebody replaced or added a key to is a refusal, not a pass.
#
# THE KEY'S OWN EXPIRY, CHECKED IN BAND. The maintainer's release-signing subkeys expire (2028-01-11 as exported),
# and a release signed after that by a subkey this file does not hold — or checked after it with one that did
# expire — cannot verify, so the weekly check would then fail with exit 2 on every run. `key` reads the date ahead
# of time: the latest expiry among the key's valid signing subkeys, bounded by the primary key's own (a subkey
# signs nothing once its primary has expired), and exits 1 when that is within 60 days — eight weekly runs to
# refresh the file — printing how. A calendar entry would say the same thing, and nothing would check it.
#
# Exit status: 0 when every pin is current (or, for `key`, when the key stays valid past 60 days); 1 when a pin is
# behind — its new lines on stdout, each release's signature verified — (or the key expires within 60 days); 2
# when it cannot tell: the listing or a download refused, no release of a pinned series in the listing, a pin
# NEWER than the listing's newest (a listing that is behind is not upstream), a signature that does not verify,
# was made by a key the key file does not hold (another key, or a signing subkey newer than the file) or by a key
# that has expired, a key file that is not the pinned key, or a tarball whose SHA-256 this machine cannot compute.
# 2 is never a pass, and the workflow leaves its issue alone on it.
#
# Environment (each optional; the suite, scripts/git-pin_test.sh, points them at local files):
#   GIT_PIN_LISTING      the release listing; https://www.kernel.org/pub/software/scm/git/
#   GIT_PIN_DOWNLOADS    where the tarballs and signatures are read from, as scripts/git.sh reads them;
#                        https://www.kernel.org/pub/software/scm/git (curl reads file:// as well)
#   GIT_PIN_FINGERPRINT  the primary fingerprint the key file must hold; the git maintainer's
set -eu

# Junio C Hamano's primary key, which signs git's releases through its signing subkeys (kernel.org's pgpkeys.git,
# keys/20D04E5A713660A7.asc).
maintainer=96E07AF25771955980DAD10020D04E5A713660A7
# The warning window for the key's expiry: eight weekly runs of the workflow, each a failed run naming the refresh.
# Long enough that a refresh missed for a holiday is still made before verification stops, short enough that the
# warning means the expiry and not a key that is simply years old.
key_warn_days=60

usage() {
	echo "usage: scripts/git-pin-check.sh [key]" >&2
	exit 2
}
case $#:${1-} in
0:) mode=pins ;;
1:key) mode=key ;;
*) usage ;;
esac

# THE SCRIPT'S OWN DIRECTORY, whatever CDPATH holds. Run by a path relative to the tree, as the workflow and a
# maintainer run it, the check's `cd scripts` searches an exported CDPATH's entries first: one holding a scripts/ of
# its own sends the check to that directory's pin and key file, and an entry of `.` makes cd print the directory it
# chose, which $here takes as a second line. Nothing here wants CDPATH, so it is gone before the first cd.
unset CDPATH
here=$(cd "$(dirname "$0")" && pwd)
pin=$here/git-pin.env
keyfile=$here/git-signing-key.asc
fingerprint=${GIT_PIN_FINGERPRINT:-$maintainer}
listing=${GIT_PIN_LISTING:-https://www.kernel.org/pub/software/scm/git/}
downloads=${GIT_PIN_DOWNLOADS:-https://www.kernel.org/pub/software/scm/git}

fail() { # <message>: cannot tell
	echo "git-pin-check.sh: $1" >&2
	exit 2
}

need="gpg gpgconf"
[ "$mode" = key ] || need="$need curl xz"
for tool in $need; do
	command -v "$tool" >/dev/null 2>&1 || fail "it needs $tool, which this machine lacks"
done
if [ "$mode" = pins ] && ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
	fail "it needs sha256sum, or shasum where that is absent, which this machine lacks"
fi

work=""
cleanup() {
	if [ -n "$work" ]; then
		# The agents a GnuPG home starts outlive the command that started them, so they are stopped before their home
		# goes; a public-key import and a verification start none on most builds, and this costs nothing then.
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

# The key file, imported and held to the pinned fingerprint: exactly one primary key, the pinned one.
if ! gpg_ --quiet --import "$keyfile" >"$work/import.log" 2>&1; then
	cat "$work/import.log" >&2
	fail "cannot import $keyfile"
fi
gpg_ --with-colons --fixed-list-mode --list-keys >"$work/keys" 2>/dev/null || fail "cannot list the keys imported from $keyfile"
primaries=$(awk -F: '$1 == "pub" { n++ } END { print n + 0 }' "$work/keys")
primary=$(awk -F: '$1 == "pub" { want = 1; next } want && $1 == "fpr" { print $10; exit }' "$work/keys")
if [ "$primaries" -ne 1 ] || [ "$primary" != "$fingerprint" ]; then
	fail "$keyfile must hold exactly the key whose primary fingerprint is $fingerprint; it holds $primaries primary key(s)${primary:+, the first $primary}"
fi

refresh_steps() {
	cat <<EOF
To refresh it: export keys/20D04E5A713660A7.asc again from kernel.org's PGP keyring
(https://git.kernel.org/pub/scm/docs/kernel/pgpkeys.git, the maintainer's current key with its new signing
subkeys), check that its primary fingerprint is still $maintainer, replace
scripts/git-signing-key.asc with it, run scripts/git-pin-check.sh (and \`key\`) again with the new file, and land
it as \`build(deps): refresh git's release signing key\`.
EOF
}

if [ "$mode" = key ]; then
	# What signs a release is a signing SUBKEY wherever the key has one — gpg signs with its newest valid signing
	# subkey, and the maintainer's primary signs releases only on a key that has none — so the date is the latest
	# expiry among the valid signing subkeys, or the primary's own where the key has no signing subkey at all, and
	# never later than the primary's expiry, since a subkey signs nothing once its primary has expired. Read from
	# each key's capability field (12, a lowercase s for a key that signs itself), validity (2: e expired, r revoked,
	# i invalid, d disabled) and expiry (7: seconds, empty for never). The maintainer's primary signs and never
	# expires, so a reading that let it count would answer "never" for a key whose release subkeys lapse.
	expiry=$(awk -F: '
		function later(a, b) { return (a == "never" || b == "never") ? "never" : (a + 0 > b + 0 ? a : b) }
		function at(field) { return field == "" ? "never" : field }
		$1 == "pub" {
			primary = at($7)
			primary_signs = ($12 ~ /s/ && $2 !~ /[erid]/)
		}
		$1 == "sub" && $12 ~ /s/ {
			subkeys++
			if ($2 !~ /[erid]/) { valid++; latest = (valid == 1) ? at($7) : later(latest, at($7)) }
		}
		END {
			if (subkeys == 0) {
				if (!primary_signs) { print "none"; exit }
				latest = primary
			} else if (valid == 0) { print "none"; exit }
			if (primary != "never" && (latest == "never" || primary + 0 < latest + 0)) latest = primary
			print latest
		}' "$work/keys")
	case $expiry in
	never)
		echo "the release signing key $fingerprint never expires"
		exit 0
		;;
	none)
		echo "the release signing key $fingerprint has no valid signing key left: every release signed from now on fails to verify." >&2
		refresh_steps >&2
		exit 1
		;;
	esac
	now=$(date -u +%s)
	when=$(date -u -d "@$expiry" +%Y-%m-%d 2>/dev/null || date -u -r "$expiry" +%Y-%m-%d) # GNU, then BSD date
	days=$(((expiry - now) / 86400))
	if [ "$expiry" -le "$((now + key_warn_days * 86400))" ]; then
		echo "the release signing key $fingerprint signs until $when, $days day(s) from now — within the ${key_warn_days}-day warning: once it expires, scripts/git-pin-check.sh cannot verify a release and fails every week." >&2
		refresh_steps >&2
		exit 1
	fi
	echo "the release signing key $fingerprint signs until $when, $days days from now"
	exit 0
fi

# pinned NAME prints the one value the pin file gives NAME, refusing a name set never or twice, as scripts/git.sh
# reads it.
pinned() {
	count=$(grep -c "^$1=" "$pin" || true)
	[ "$count" -eq 1 ] || fail "$pin sets $1 $count times; it must set it once"
	sed -n "s/^$1=//p" "$pin"
}

# digest FILE prints FILE's SHA-256 and fails rather than print anything else: piped through cut, a hash that
# failed part way, or printed no digest at all, answered cut's 0, and the check named that output under a report
# saying it is the SHA-256 of the tarball whose signature verified.
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

# fetch URL FILE downloads one file, asked again on a transient failure and given up on when it STALLS — nothing
# received for a minute (`--speed-limit 1 --speed-time 60`, the stall bound the engine's object store holds a
# transfer to, objstore.Stall) or no connection in 30 seconds — and never for being slow while it moves. Without a
# bound a kernel.org transfer that stops moving holds the weekly job to GitHub's six-hour limit, and curl's own
# connect timeout is five minutes an attempt.
#
# CURL'S STDOUT IS NEVER THE CHECK'S. curl is not passed -q, so a person's curlrc — their proxy, their CA bundle —
# applies where the check really downloads; and whatever that file makes curl write to stdout (a `write-out`, a
# trace to -) goes to stderr with curl's other diagnostics. Left on stdout it reached the report:
# `verified` runs inside the substitution that takes a release's digest, so a curlrc's `write-out` was taken with it
# and printed as the pin's new GIT_<NAME>_SHA256 line, a value the check never computed, under the report's word
# that it is the verified tarball's.
fetch() {
	curl -fsSL --retry 3 --connect-timeout 30 --speed-limit 1 --speed-time 60 -o "$2" "$1" >&2
}

# vsort sorts versions of three numbers, numerically and field by field: POSIX sort, since `sort -V` is not.
vsort() {
	sort -u -t. -k1,1n -k2,2n -k3,3n
}

# The listing's releases: every git-X.Y.Z.tar.xz it links, oldest first.
if ! fetch "$listing" "$work/listing"; then
	fail "cannot read the release listing $listing"
fi
grep -o 'git-[0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*\.tar\.xz' "$work/listing" | sed 's/^git-//; s/\.tar\.xz$//' | vsort >"$work/releases" || true
[ -s "$work/releases" ] || fail "the listing $listing names no git release"
newest=$(tail -n 1 "$work/releases")

# verified VERSION downloads that release's tarball and signature once, verifies the signature over the
# decompressed tar, and prints the tarball's SHA-256.
verified() {
	v=$1
	if [ ! -f "$work/$v.sha256" ]; then
		for file in "git-$v.tar.xz" "git-$v.tar.sign"; do
			if ! fetch "$downloads/$file" "$work/$file"; then
				fail "cannot download $downloads/$file"
			fi
		done
		# GOODSIG, never only a zero status: gpg answers 0 for a good signature by a key that has since expired
		# (EXPKEYSIG), and a key past its expiry is one this check must say so about. Since the keyring holds the
		# pinned key alone, a good signature is that key's.
		if ! xz -dc "$work/git-$v.tar.xz" | gpg_ --status-file "$work/$v.status" --verify "$work/git-$v.tar.sign" - >"$work/$v.verify" 2>&1 ||
			! grep -q '^\[GNUPG:\] GOODSIG ' "$work/$v.status"; then
			cat "$work/$v.verify" >&2
			if grep -q '^\[GNUPG:\] EXPKEYSIG ' "$work/$v.status"; then
				echo "git-pin-check.sh: git-$v.tar.sign was made by a key of $fingerprint that has expired: scripts/git-signing-key.asc needs refreshing." >&2
				refresh_steps >&2
				exit 2
			fi
			# NO_PUBKEY names the key that signed and no more: whether it is another key or a signing subkey the
			# maintainer added under the pinned primary after the file was exported, only the maintainer's current
			# export can say — and the second is the likely one, since the subkeys in the file stay valid until
			# they expire and `key` cannot warn of a subkey it has never seen. So the refusal names both, and how
			# to refresh the file, rather than read as a forged signature.
			signer=$(awk '$1 == "[GNUPG:]" && $2 == "NO_PUBKEY" { print $3; exit }' "$work/$v.status")
			if [ -n "$signer" ]; then
				echo "git-pin-check.sh: git-$v.tar.sign was made by $signer, a key scripts/git-signing-key.asc does not hold: another key than $fingerprint, or a signing subkey of $fingerprint newer than the file." >&2
				refresh_steps >&2
				echo "If the refreshed file still does not hold $signer, the release was not signed by the git maintainer: take nothing from it." >&2
				exit 2
			fi
			if grep -q '^\[GNUPG:\] ERRSIG ' "$work/$v.status"; then
				fail "git-$v.tar.sign could not be checked with the key of $fingerprint (scripts/git-signing-key.asc)"
			fi
			fail "git-$v.tar.sign does not verify over git-$v.tar.xz with the key of $fingerprint"
		fi
		sum=$(digest "$work/git-$v.tar.xz") || fail "cannot compute the SHA-256 of git-$v.tar.xz"
		printf '%s\n' "$sum" >"$work/$v.sha256"
	fi
	cat "$work/$v.sha256"
}

# check NAME VAR TARGET: compare the pin VAR names (scripts/git.sh's NAME) with TARGET, upstream's newest release
# of its series, and append its new lines to the report when it is behind.
behind=0
check() {
	name=$1 var=$2 target=$3
	current=$(pinned "${var}_VERSION")
	printf '%s\n' "$current" | grep -Eqx '[0-9]+\.[0-9]+\.[0-9]+' ||
		fail "${var}_VERSION in $pin is \"$current\", not a release (three numbers)"
	[ -n "$target" ] || fail "the listing $listing names no release of $name's series, ${current%.*}"
	if [ "$current" = "$target" ]; then
		echo "$name $current" >>"$work/current"
		return
	fi
	if [ "$(printf '%s\n%s\n' "$current" "$target" | vsort | tail -n 1)" = "$current" ]; then
		fail "${var}_VERSION=$current is newer than $target, the newest of its series the listing $listing names: that listing is behind upstream"
	fi
	sha=$(verified "$target")
	behind=$((behind + 1))
	case $name in
	node) title="build(deps): bump git from $current to $target" ;;
	node-floor) title="build(deps): bump git's node floor from $current to $target" ;;
	client-floor) title="build(deps): bump git's client floor from $current to $target" ;;
	esac
	cat >>"$work/report" <<EOF
# $name: $current -> $target, in a pull request titled \`$title\`
${var}_VERSION=$target
${var}_SHA256=$sha

EOF
	if [ "$name" = node ] && [ "${target%%.*}" -gt "${current%%.*}" ]; then
		echo "git ${target%%.*} needs Rust to build (its Makefile says Rust is mandatory from 3.0): scripts/git.sh's recipe changes with this pin." >>"$work/report"
		echo >>"$work/report"
	fi
}

# series MAJOR.MINOR prints the listing's newest release of that series, or nothing.
series() {
	grep "^$(printf '%s' "$1" | sed 's/\./\\./')\.[0-9][0-9]*\$" "$work/releases" | tail -n 1 || true
}

: >"$work/report"
: >"$work/current"
node_floor=$(pinned GIT_NODE_FLOOR_VERSION)
client_floor=$(pinned GIT_CLIENT_FLOOR_VERSION)
check node GIT_NODE "$newest"
check node-floor GIT_NODE_FLOOR "$(series "${node_floor%.*}")"
check client-floor GIT_CLIENT_FLOOR "$(series "${client_floor%.*}")"

if [ "$behind" -gt 0 ]; then
	cat <<EOF
scripts/git-pin.env is behind upstream ($listing). Each release below was downloaded and its signature
(git-<version>.tar.sign, over the decompressed tar) verified against the git maintainer's key, $fingerprint
(scripts/git-signing-key.asc); its SHA-256 is that tarball's.

EOF
	cat "$work/report"
	cat <<EOF
A bump replaces a pin's two lines in scripts/git-pin.env, runs \`make git\` (it builds the new git from the
tarball the digest names) and \`make check\` (the suite on it), and lands as its own pull request under the title
above. The pull request runs the snapshot job, which builds the engine image's git from the new pin on both
platforms. A pin left behind a security release of its series is a release to hold (RELEASING.md).
EOF
	exit 1
fi
echo "every pin is the newest release of its series: $(tr '\n' ',' <"$work/current" | sed 's/,$//; s/,/, /g')"

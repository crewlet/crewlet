#!/bin/sh
#
# playwright-mcp-pin-check.sh — hold the Playwright tree scripts/playwright-mcp-pin.env pins to @playwright/mcp's
# latest release on the npm registry. .github/workflows/pins.yml runs it every week, beside the other pins' checks:
# no Dependabot entry reads the pin file, and none may move it (the pin file says why), so without it the pin would be
# watched by nothing, which looks exactly like a pin with nothing to update.
#
#   scripts/playwright-mcp-pin-check.sh    is the pin @playwright/mcp's latest release?
#
# THE PIN follows @playwright/mcp's `latest` dist-tag, and its playwright-core follows the release that @playwright/mcp
# names as its exact dependency — never playwright-core's own tags, since the server runs on the core it was released
# against. For a pin that is behind, the check downloads the newer release's tarball and then the playwright-core
# tarball its own package.json names, and for each one requires its SHA-512 to be the registry's dist.integrity for
# that version, verifies every registry signature over `<name>@<version>:<integrity>` with npm's registry key, and
# computes its SHA-256. Then it prints the pin's four new lines with the pull request that moves it: every SHA-256 that
# of a tarball whose integrity the registry signed, so the issue the workflow opens names digests a person can paste.
#
# THE KEY IS THE PINNED ONE AND NOTHING ELSE, as scripts/git-pin-check.sh holds the git maintainer's. npm names a
# signature by its key's id, the SHA-256 fingerprint OpenSSH gives the key, and scripts/npm-registry-key.pem is refused
# unless it is a P-256 key of exactly the pinned id. A signature under any other id is refused, naming both: npm
# rotating its key is a refresh of the file, made by a person who reads the new key from the registry's key list, never
# one this check makes. The key carries no expiry (that list says `"expires": null` for it), so this leg, unlike the
# git leg, has no key step: a rotation shows as that refusal, which names what to refresh.
#
# A REGISTRY'S ANSWER IS USED ONLY AS WHAT IT MUST BE. A release is refused unless it is a version of its package's
# grammar, an integrity unless it is a SHA-512, a key id unless it is a SHA-256 fingerprint and a signature unless it
# is base64 no longer than a P-256 signature's, each BEFORE it reaches a URL, a file name, a command or the report, and
# a refusal says how long the answer was and never what it said, so nothing the registry serves but those reaches the
# issue the workflow writes. The documents are read with grep, which is enough for the compact JSON the registry
# serves and the members read here, none of which holds an escape; a member that appears other than once is refused
# as unreadable. That reading cannot make a tarball pass: its SHA-512 must be the integrity, which the pinned key must
# have signed for its name and version. Every download follows a redirect to an https:// address alone
# (scripts/chromium-pin-check.sh says why).
#
# Exit status: 0 when the pin is @playwright/mcp's latest release; 1 when it is behind — its new lines on stdout, each
# tarball's integrity signed by the pinned key; 2 when it cannot tell: a document or a download refused, an answer of
# the wrong shape, a pin newer than the registry's latest (a listing that is behind is not upstream), a tarball whose
# SHA-512 is not its integrity, a version carrying no signature, a signature that does not verify or is under another
# key, a key file that is not the pinned key, or a tarball whose SHA-256 this machine cannot compute. 2 is never a
# pass, and the workflow leaves its issue alone on it.
#
# Environment (each optional; the suite, scripts/playwright-mcp-pin_test.sh, points them at local files):
#   PLAYWRIGHT_MCP_PIN_REGISTRY  the registry, as scripts/playwright-mcp.sh reads it; https://registry.npmjs.org
#                                (curl reads file:// as well)
#   PLAYWRIGHT_MCP_PIN_KEYID     the key id the key file must hold; npm's
set -eu

# npm's registry signing key, by the id its key list (https://registry.npmjs.org/-/npm/v1/keys) gives it.
npm_key=SHA256:DhQ8wR5APBvFHLF/+Tc+AYvPOdTpcIDqOhxsBHRwC7U

[ $# -eq 0 ] || {
	echo "usage: scripts/playwright-mcp-pin-check.sh" >&2
	exit 2
}

# The script's own directory, whatever CDPATH holds: scripts/git-pin-check.sh says why.
unset CDPATH
here=$(cd "$(dirname "$0")" && pwd)
pin=$here/playwright-mcp-pin.env
keyfile=$here/npm-registry-key.pem
keyid=${PLAYWRIGHT_MCP_PIN_KEYID:-$npm_key}
registry=${PLAYWRIGHT_MCP_PIN_REGISTRY:-https://registry.npmjs.org}

fail() { # <message>: cannot tell
	echo "playwright-mcp-pin-check.sh: $1" >&2
	exit 2
}

for tool in curl gzip tar openssl od; do
	command -v "$tool" >/dev/null 2>&1 || fail "it needs $tool, which this machine lacks"
done
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

# THE KEY FILE, held to the pinned id before anything is read with it. A P-256 public key is 91 bytes of SPKI DER: a
# fixed 26-byte head naming the curve, then the point, which OpenSSH writes as a string of the key's type, a string of
# the curve's and a string of the point; npm's id is the unpadded base64 of that encoding's SHA-256.
p256_head=3059301306072a8648ce3d020106082a8648ce3d030107034200
openssl pkey -pubin -in "$keyfile" -outform DER -out "$work/key.der" 2>/dev/null ||
	fail "cannot read $keyfile as a public key"
der=$(od -An -tx1 -v "$work/key.der" | tr -d ' \n')
case $der in
"$p256_head"*) ;;
*) der="" ;;
esac
[ "${#der}" -eq 182 ] || fail "$keyfile is not a P-256 public key, which npm's registry signs with"
held=$({
	printf '\000\000\000\023ecdsa-sha2-nistp256\000\000\000\010nistp256\000\000\000\101'
	tail -c 65 "$work/key.der"
} | openssl dgst -sha256 -binary | openssl base64 -A) || fail "cannot compute the id of the key in $keyfile"
held=SHA256:$(printf '%s' "$held" | sed 's/=*$//')
[ "$held" = "$keyid" ] || fail "$keyfile must hold the key whose id is $keyid; it holds $held"

# pinned NAME prints the one value the pin file gives NAME, refusing a name set never or twice, as
# scripts/playwright-mcp.sh reads it.
pinned() {
	count=$(grep -c "^$1=" "$pin" || true)
	[ "$count" -eq 1 ] || fail "$pin sets $1 $count times; it must set it once"
	sed -n "s/^$1=//p" "$pin"
}

# mcp_release VALUE: VALUE is an @playwright/mcp release, three numbers and nothing else; core_release VALUE: a
# playwright-core release, scripts/playwright-mcp.sh's grammar for it. Neither grammar bounds how long a number runs,
# so each bounds the length first, by what the registry has served (read 2026-11-18, every release): an
# @playwright/mcp release has been at most 6 characters (0.0.86), and a core one names at most 26
# (1.55.0-alpha-1752701791000). 32 and 64 hold numbers far past those and keep a file name made of either short; a
# longer answer is refused, exit 2, for a person to widen, as a new shape is.
mcp_release() {
	[ "${#1}" -le 32 ] && printf '%s\n' "$1" | grep -Eqx '[0-9]+\.[0-9]+\.[0-9]+'
}
core_release() {
	[ "${#1}" -le 64 ] && printf '%s\n' "$1" | grep -Eqx '[0-9]+\.[0-9]+\.[0-9]+(-(alpha|beta)(-[0-9]+)+)?'
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
# it stalls, asked again on a transient failure, a redirect followed to an https:// address alone, and curl's stdout
# on stderr, where a curlrc's `write-out` cannot reach the report.
fetch() {
	curl -fsSL --proto-redir =https --retry 3 --connect-timeout 30 --speed-limit 1 --speed-time 60 -o "$2" "$1" >&2
}

# member NAME FILE prints the value of the one string member NAME of the JSON document FILE; it fails, printing
# nothing, where FILE has no such member, or more than one, or one whose value holds an escape.
member() {
	found=$(grep -o "\"$1\"[[:space:]]*:[[:space:]]*\"[^\"]*\"" "$2" || true)
	[ "$(printf '%s' "$found" | grep -c '^')" -eq 1 ] || return 1
	case $found in
	*\\*) return 1 ;;
	esac
	printf '%s\n' "$found" | sed 's/^"[^"]*"[[:space:]]*:[[:space:]]*"//; s/"$//'
}

# shape BYTES: what a refusal says of an answer — its length, never its text.
shape() {
	printf '%s bytes' "$(printf '%s' "$1" | wc -c | tr -d ' ')"
}

# signed NAME VERSION DOC TARBALL: TARBALL is NAME@VERSION as the registry signed it. Its SHA-512 is the integrity the
# version document DOC gives, and every registry signature DOC carries is under the pinned key and verifies over
# `NAME@VERSION:<integrity>` — npm's own message; DOC carrying none is a refusal, since the registry signs every version
# it serves. A signature under another key id is refused naming both.
signed() {
	name=$1 v=$2 doc=$3 tarball=$4
	integrity=$(member integrity "$doc") || fail "the registry's document of $name@$v gives no one dist.integrity"
	# sha512- and the 88 base64 characters of a SHA-512's 64 bytes: the grammar fixes its length, 95 characters.
	printf '%s\n' "$integrity" | grep -Eqx 'sha512-[A-Za-z0-9+/]{86}==' ||
		fail "the registry's dist.integrity of $name@$v is $(shape "$integrity") that are not a SHA-512 integrity"
	sum=$(openssl dgst -sha512 -binary "$tarball" | openssl base64 -A) ||
		fail "cannot compute the SHA-512 of $name@$v's tarball"
	[ "sha512-$sum" = "$integrity" ] ||
		fail "$name@$v's tarball hashes to sha512-$sum, and the registry's dist.integrity for it is $integrity"
	arrays=$(grep -o '"signatures"[[:space:]]*:[[:space:]]*\[[^]]*\]' "$doc" || true)
	[ "$(printf '%s' "$arrays" | grep -c '^')" -le 1 ] ||
		fail "the registry's document of $name@$v gives more than one dist.signatures"
	printf '%s\n' "$arrays" | grep -o '{[^{}]*}' >"$work/signatures" || true
	[ -s "$work/signatures" ] ||
		fail "the registry's document of $name@$v carries no signature, and the registry signs every version it serves"
	printf '%s' "$name@$v:$integrity" >"$work/message"
	n=0
	while IFS= read -r object; do
		n=$((n + 1))
		printf '%s\n' "$object" >"$work/signature.json"
		id=$(member keyid "$work/signature.json") || fail "signature $n of $name@$v names no one key id"
		# SHA256: and the 43 unpadded base64 characters of a SHA-256: the grammar fixes its length, 50 characters.
		printf '%s\n' "$id" | grep -Eqx 'SHA256:[A-Za-z0-9+/]{43}' ||
			fail "signature $n of $name@$v names a key id of $(shape "$id") that is no SHA-256 key id"
		[ "$id" = "$keyid" ] ||
			fail "signature $n of $name@$v is under key $id, and the key the check holds is $keyid ($keyfile): npm may have rotated its key, which a person confirms in https://registry.npmjs.org/-/npm/v1/keys before replacing $keyfile and the id this script pins; take nothing from the release until then"
		sig=$(member sig "$work/signature.json") || fail "signature $n of $name@$v carries no one sig"
		# A P-256 signature is DER of at most 72 bytes (a sequence's two-byte head over two integers, each a two-byte
		# head and at most 33 bytes), so at most 96 characters of base64: no longer answer is one the pinned key made.
		[ "${#sig}" -le 96 ] && printf '%s\n' "$sig" | grep -Eqx '[A-Za-z0-9+/]+=*' ||
			fail "signature $n of $name@$v is $(shape "$sig") that are not a P-256 signature in base64"
		printf '%s' "$sig" | openssl base64 -d -A >"$work/signature.der" 2>/dev/null ||
			fail "signature $n of $name@$v cannot be decoded"
		openssl dgst -sha256 -verify "$keyfile" -signature "$work/signature.der" "$work/message" >/dev/null 2>&1 ||
			fail "signature $n of $name@$v does not verify over its integrity with the key $keyid ($keyfile)"
	done <"$work/signatures"
}

# vsort sorts versions of three numbers, numerically and field by field: POSIX sort, since `sort -V` is not.
vsort() {
	sort -u -t. -k1,1n -k2,2n -k3,3n
}

current=$(pinned PLAYWRIGHT_MCP_VERSION)
current_core=$(pinned PLAYWRIGHT_CORE_VERSION)
mcp_release "$current" || fail "PLAYWRIGHT_MCP_VERSION in $pin is \"$current\", not a release (three numbers)"
core_release "$current_core" || fail "PLAYWRIGHT_CORE_VERSION in $pin is \"$current_core\", not a playwright-core release"

# @playwright/mcp's latest release, refused unless it is three numbers before it is used.
fetch "$registry/-/package/@playwright/mcp/dist-tags" "$work/dist-tags" ||
	fail "cannot read @playwright/mcp's dist-tags at $registry/-/package/@playwright/mcp/dist-tags"
latest=$(member latest "$work/dist-tags") || fail "@playwright/mcp's dist-tags give no one \`latest\`"
mcp_release "$latest" || fail "@playwright/mcp's \`latest\` is $(shape "$latest") that are not a release (three numbers); nothing of it is used"

if [ "$latest" = "$current" ]; then
	echo "the pin is @playwright/mcp's latest release: $current, with playwright-core $current_core"
	exit 0
fi
if [ "$(printf '%s\n%s\n' "$current" "$latest" | vsort | tail -n 1)" = "$current" ]; then
	fail "PLAYWRIGHT_MCP_VERSION=$current is newer than $latest, the latest release the registry $registry names: that registry is behind upstream"
fi

# The newer release, then the core its own tarball names.
fetch "$registry/@playwright/mcp/$latest" "$work/mcp.json" ||
	fail "cannot read the registry's document of @playwright/mcp@$latest"
mcp_file=mcp-$latest.tgz
fetch "$registry/@playwright/mcp/-/$mcp_file" "$work/$mcp_file" || fail "cannot download $registry/@playwright/mcp/-/$mcp_file"
signed @playwright/mcp "$latest" "$work/mcp.json" "$work/$mcp_file"
mcp_sha=$(digest "$work/$mcp_file") || fail "cannot compute the SHA-256 of $mcp_file"
gzip -dc "$work/$mcp_file" | tar -xOf - package/package.json 2>/dev/null | tr -d ' \t\r\n' >"$work/package.json" || true
dependencies=$(grep -o '"dependencies":{[^{}]*}' "$work/package.json" || true)
[ "$(printf '%s' "$dependencies" | grep -c '^')" -eq 1 ] ||
	fail "$mcp_file's package.json gives no one dependencies object"
printf '%s\n' "$dependencies" >"$work/dependencies.json"
core=$(member playwright-core "$work/dependencies.json") ||
	fail "@playwright/mcp@$latest names no one playwright-core among its dependencies"
core_release "$core" ||
	fail "@playwright/mcp@$latest names a playwright-core of $(shape "$core") that is not a playwright-core release; nothing of it is used"
fetch "$registry/playwright-core/$core" "$work/core.json" || fail "cannot read the registry's document of playwright-core@$core"
core_file=playwright-core-$core.tgz
fetch "$registry/playwright-core/-/$core_file" "$work/$core_file" ||
	fail "cannot download $registry/playwright-core/-/$core_file"
signed playwright-core "$core" "$work/core.json" "$work/$core_file"
core_sha=$(digest "$work/$core_file") || fail "cannot compute the SHA-256 of $core_file"

cat <<EOF
scripts/playwright-mcp-pin.env is behind the npm registry ($registry): @playwright/mcp's latest release is $latest.
Each tarball below was downloaded from the registry, its SHA-512 is the dist.integrity the registry gives it, and
every signature over that integrity verified with npm's registry key $keyid (scripts/npm-registry-key.pem); each
SHA-256 is that tarball's.

# @playwright/mcp: $current -> $latest, and playwright-core: $current_core -> $core, in a pull request titled \`build(deps): bump playwright mcp from $current to $latest\`
PLAYWRIGHT_MCP_VERSION=$latest
PLAYWRIGHT_MCP_SHA256=$mcp_sha
PLAYWRIGHT_CORE_VERSION=$core
PLAYWRIGHT_CORE_SHA256=$core_sha

A bump replaces the four lines in scripts/playwright-mcp-pin.env, runs \`make playwright-mcp\` (it writes both
tarballs into internal/browser/boxhelper/pw/ and refuses any that is not its digest), moves internal/browser's
PlaywrightMCPRelease and PlaywrightCoreRelease to the new releases and ExpectedChromiumRevision and
ExpectedChromiumVersion to the new playwright-core's browsers.json, and runs \`make check\`
(TestTheEmbeddedTreeIsThePinnedRelease fails until all of it agrees), in a pull request of its own under the title
above.
EOF
exit 1

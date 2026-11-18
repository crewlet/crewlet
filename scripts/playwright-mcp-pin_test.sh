#!/usr/bin/env bash
#
# playwright-mcp-pin_test.sh — the own suite of the Playwright MCP recipe (scripts/playwright-mcp.sh) and of the pin
# check (scripts/playwright-mcp-pin-check.sh).
#
# No Go test reaches either script, and each regresses silently in the direction that matters: a recipe that writes a
# tarball it never checked into the tree the box helper embeds, or leaves half a tree there; a check that calls the pin
# current because it read nothing, proposes a tarball whose signature it never asked about, or carries the registry's
# words into the issue it reports to. So every case runs the REAL script — copied beside a pin file and a key file the
# case writes, because both read them beside themselves — against local stand-ins: an npm registry under file:// (the
# dist-tags, each version's document, each tarball), tarballs holding a package.json, and P-256 registry keys the suite
# makes with openssl, signing as npm signs; and, for the cases that hold each script's defaults, a stand-in curl that
# serves the registry's own addresses from a local tree. It fetches nothing and reads no network, and it needs bash,
# curl, gzip, tar, openssl and a SHA-256 tool.
set -euo pipefail

# THE PERSON'S ENVIRONMENT, GONE before the first case, as scripts/git-pin_test.sh clears it and for its reasons: every
# variable whose name begins PLAYWRIGHT_MCP_PIN_ — the scripts' own knobs — curl's own configuration (CURL_HOME here,
# and HOME and XDG_CONFIG_HOME made the suite's own below), openssl's (OPENSSL_CONF, which can withdraw the very digest
# a case computes) and CDPATH. The suite's last case runs the whole suite again, by a relative path, under a hostile
# environment holding every one of these and requires it green and the hostile directories untouched.
for name in $(compgen -e); do
	case "$name" in
	PLAYWRIGHT_MCP_PIN_*) unset "$name" ;;
	esac
done
unset CURL_HOME OPENSSL_CONF CDPATH

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

missing=""
for tool in curl gzip tar openssl od; do
	command -v "$tool" >/dev/null 2>&1 || missing="$missing $tool"
done
command -v sha256sum >/dev/null 2>&1 || command -v shasum >/dev/null 2>&1 || missing="$missing sha256sum-or-shasum"
if [ -n "$missing" ]; then
	echo "playwright-mcp-pin_test.sh needs:$missing — CONTRIBUTING.md, Development setup, says what to install" >&2
	exit 1
fi

TMPROOT="$(mktemp -d)"
trap 'rm -rf "$TMPROOT"' EXIT

export HOME="$TMPROOT/home"
export XDG_CONFIG_HOME="$TMPROOT/config"
mkdir -p "$HOME" "$XDG_CONFIG_HOME"

# The suite's curl, first on PATH: a recorder, so a case can hold the scripts to how they download as well as to what —
# every download is logged with its arguments, then made by the real curl.
mkdir -p "$TMPROOT/tools"
cat >"$TMPROOT/tools/curl" <<EOF
#!/bin/sh
printf '%s\n' "\$*" >>"$TMPROOT/curl.log"
exec "$(command -v curl)" "\$@"
EOF
chmod +x "$TMPROOT/tools/curl"
export PATH="$TMPROOT/tools:$PATH"

pass=0
fail=0
ok() {
	pass=$((pass + 1))
	printf 'ok   %s\n' "$1"
}
bad() { # <name> <why>...
	fail=$((fail + 1))
	printf 'FAIL %s\n' "$1"
	shift
	local line
	for line in "$@"; do
		printf '%s\n' "$line" | sed 's/^/     | /'
	done
}

digest() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1"
	else
		shasum -a 256 "$1"
	fi | cut -d' ' -f1
}

# run <cmd>...: runs it, leaving its stdout in $out, its stderr in $err and its exit status in $status.
run() {
	"$@" >"$TMPROOT/out" 2>"$TMPROOT/err" && status=0 || status=$?
	out="$(cat "$TMPROOT/out")"
	err="$(cat "$TMPROOT/err")"
}

# within <dir> <cmd>...: runs the command from <dir>, as a person in a checkout runs a script by its relative path.
within() {
	local dir="$1"
	shift
	(cd "$dir" && exec "$@")
}

# expect <name> <status> [stdout-has] [stderr-has]: the last run answered status and printed what is named.
expect() {
	local name="$1" want="$2" has_out="${3-}" has_err="${4-}"
	local why=()
	[ "$status" = "$want" ] || why+=("exit $status, want $want")
	if [ -n "$has_out" ] && ! grep -qF -- "$has_out" <<<"$out"; then why+=("stdout lacks: $has_out"); fi
	if [ -n "$has_err" ] && ! grep -qF -- "$has_err" <<<"$err"; then why+=("stderr lacks: $has_err"); fi
	if [ ${#why[@]} -eq 0 ]; then
		ok "$name"
	else
		bad "$name" "${why[@]}" "stdout:" "$out" "stderr:" "$err"
	fi
}

# bounded <name>: every download logged since the log was emptied gives up on a transfer that stalls and follows a
# redirect to an https:// address alone — curl by default follows one to plain http:// too. Read off the arguments,
# since the suite's addresses are file:// and redirect nowhere.
bounded() {
	local line why=()
	while IFS= read -r line; do
		case " $line " in
		*" --speed-limit 1 --speed-time 60 "*) ;;
		*) why+=("no stall bound: curl $line") ;;
		esac
		case " $line " in
		*" --connect-timeout 30 "*) ;;
		*) why+=("no connect bound: curl $line") ;;
		esac
		case " $line " in
		*" --proto-redir =https "*) ;;
		*) why+=("a redirect off HTTPS followed: curl $line") ;;
		esac
	done <"$TMPROOT/curl.log"
	[ -s "$TMPROOT/curl.log" ] || why+=("no download was made")
	if [ ${#why[@]} -eq 0 ]; then ok "$1"; else bad "$1" "${why[@]}"; fi
}

# undownloaded <name>: nothing was downloaded since the log was emptied.
undownloaded() {
	if [ -s "$TMPROOT/curl.log" ]; then
		bad "$1" "downloads: $(cat "$TMPROOT/curl.log")"
	else
		ok "$1"
	fi
}

# curlhome <dir>: a curl home whose .curlrc has every transfer write its status code to curl's stdout as it ends
# (`write-out`, which reads 000 for file://), as a developer's own may; prints <dir>, for a case's CURL_HOME.
curlhome() {
	mkdir -p "$1"
	printf '%s\n' 'write-out = "%{http_code}\n"' >"$1/.curlrc"
	echo "$1"
}

# offline <web>: a stand-in curl that serves each https://<host>/<path> it is asked for from <web>/<host>/<path> and
# refuses every other address as `curl -f` refuses a 404, for a case that runs a script on its default addresses with
# no network; prints the directory to put first on the case's PATH. The address is the last argument, as both scripts
# give it.
offline() {
	mkdir -p "$1.bin"
	cat >"$1.bin/curl" <<EOF
#!/bin/sh
out="" url=""
while [ \$# -gt 0 ]; do
	case \$1 in
	-o) out=\$2 && shift ;;
	*) url=\$1 ;;
	esac
	shift
done
case \$url in
https://*) file="$1/\${url#https://}" ;;
*) file="" ;;
esac
if [ -n "\$file" ] && [ -f "\$file" ]; then
	exec cp "\$file" "\$out"
fi
echo "curl: (22) The requested URL returned error: 404 (\$url)" >&2
exit 22
EOF
	chmod +x "$1.bin/curl"
	echo "$1.bin"
}

# hashtool <dir> <fails|garbles> [pattern]: scripts/git-pin_test.sh's stand-in sha256sum, first on a PATH that puts
# <dir> first. Over a file matching the pattern it fails having printed nothing, or prints a line that is no SHA-256
# and answers 0; over any other file it is the machine's own. Both scripts read sha256sum first.
real_sha256="$(command -v sha256sum || true)"
hashtool() {
	local dir="$1" mode="$2" pattern="${3-*}" real answer
	mkdir -p "$dir"
	if [ -n "$real_sha256" ]; then real="'$real_sha256'"; else real="'$(command -v shasum)' -a 256"; fi
	case "$mode" in
	fails) answer="exit 1" ;;
	garbles) answer="echo \"0123abcd  \$1\"; exit 0" ;;
	esac
	cat >"$dir/sha256sum" <<EOF
#!/bin/sh
case \$1 in
$pattern) $answer ;;
esac
exec $real "\$@"
EOF
	chmod +x "$dir/sha256sum"
}

# scripts <case>: a checkout for the case — the two scripts copied from this tree, byte for byte, into its scripts/,
# and the box helper's package directory the recipe writes into — printing the scripts directory. The case writes the
# pin and the key file beside the scripts.
scripts() {
	local dir="$TMPROOT/$1/scripts"
	mkdir -p "$dir" "$TMPROOT/$1/internal/browser/boxhelper"
	cp "$SCRIPT_DIR/playwright-mcp.sh" "$SCRIPT_DIR/playwright-mcp-pin-check.sh" "$dir/"
	echo "$dir"
}

zero=0000000000000000000000000000000000000000000000000000000000000000

# pin <dir> <mcp> <mcp sha> <core> <core sha> [extra line]...: a pin file naming both tarballs, and each extra line
# after them.
pin() {
	local dir="$1"
	{
		echo "# a pin file a case wrote"
		echo "PLAYWRIGHT_MCP_VERSION=$2"
		echo "PLAYWRIGHT_MCP_SHA256=$3"
		echo "PLAYWRIGHT_CORE_VERSION=$4"
		echo "PLAYWRIGHT_CORE_SHA256=$5"
		shift 5
		local line
		for line in "$@"; do
			echo "$line"
		done
	} >"$dir/playwright-mcp-pin.env"
}

# npmtar <file> <name> <version> [core]: an npm tarball at <file> holding package/package.json, indented as npm writes
# one, naming the package and its version — and, given a core, `"playwright"` and `"playwright-core"` at that core among
# its dependencies, beside a devDependencies naming another playwright-core, which no script may read — and an
# index.js; prints its SHA-256.
npmtar() {
	local file="$1" name="$2" version="$3" core="${4-}"
	local src="$TMPROOT/src-$RANDOM$RANDOM"
	mkdir -p "$src/package" "$(dirname "$file")"
	{
		echo "{"
		echo "  \"name\": \"$name\","
		echo "  \"version\": \"$version\","
		if [ -n "$core" ]; then
			echo "  \"dependencies\": {"
			echo "    \"playwright\": \"$core\","
			echo "    \"playwright-core\": \"$core\""
			echo "  },"
			echo "  \"devDependencies\": {"
			echo "    \"playwright-core\": \"9.9.9\""
			echo "  },"
		fi
		echo "  \"license\": \"Apache-2.0\""
		echo "}"
	} >"$src/package/package.json"
	echo "module.exports = {}; // $name $version" >"$src/package/index.js"
	tar -czf "$file" -C "$src" package
	rm -rf "$src"
	digest "$file"
}

# THE KEYS, made once for the suite: P-256 pairs as npm's registry key is, each $keys/<name>.pem (private) and
# <name>.pub.pem, and npm's id for each, the unpadded base64 SHA-256 of the key as OpenSSH encodes it.
keys="$TMPROOT/keys"
mkdir -p "$keys"
newkey() {
	openssl ecparam -name prime256v1 -genkey -noout -out "$keys/$1.pem" 2>/dev/null
	openssl pkey -in "$keys/$1.pem" -pubout -out "$keys/$1.pub.pem" 2>/dev/null
}
keyid() { # <name>: the id npm gives the key
	local der="$TMPROOT/keyid.der"
	openssl pkey -pubin -in "$keys/$1.pub.pem" -outform DER -out "$der" 2>/dev/null
	printf 'SHA256:%s\n' "$({
		printf '\000\000\000\023ecdsa-sha2-nistp256\000\000\000\010nistp256\000\000\000\101'
		tail -c 65 "$der"
	} | openssl dgst -sha256 -binary | openssl base64 -A | sed 's/=*$//')"
}
newkey pinned
newkey other
pinned_id="$(keyid pinned)"
other_id="$(keyid other)"

# registry <case>: the case's stand-in registry, file:// under the case's directory.
registry() {
	echo "$TMPROOT/$1/registry"
}

# disttags <registry> <answer>: @playwright/mcp's dist-tags, answering `latest` with the answer, beside a `next`.
disttags() {
	mkdir -p "$1/-/package/@playwright/mcp"
	printf '{"next":"0.0.99-alpha-1787869812000","latest":"%s"}' "$2" >"$1/-/package/@playwright/mcp/dist-tags"
}

# versiondoc <registry> <name> <version> <signer>...: the registry's compact document of name@version, its
# dist.integrity that of the version's tarball (INTEGRITY=<value> in the environment says another), and one signature
# for each signer: <key> signs as the key itself, <key>:<id key> signs with <key> under the id of <id key>, and none
# gives an empty list; KEYID=<value> or SIG=<value> gives every signature that member instead. NOSIGNATURES=1 leaves
# the member out.
versiondoc() {
	local reg="$1" name="$2" version="$3" file
	shift 3
	case $name in
	@playwright/mcp) file="$reg/@playwright/mcp/-/mcp-$version.tgz" ;;
	*) file="$reg/$name/-/$name-$version.tgz" ;;
	esac
	local integrity="${INTEGRITY:-sha512-$(openssl dgst -sha512 -binary "$file" | openssl base64 -A)}"
	local sigs="" signer key id sig
	for signer in "$@"; do
		key="${signer%%:*}"
		id="${KEYID:-$(keyid "${signer#*:}")}"
		sig="${SIG:-$(printf '%s' "$name@$version:$integrity" | openssl dgst -sha256 -sign "$keys/$key.pem" | openssl base64 -A)}"
		sigs="$sigs${sigs:+,}{\"sig\":\"$sig\",\"keyid\":\"$id\"}"
	done
	local signatures=",\"signatures\":[$sigs]"
	[ -z "${NOSIGNATURES-}" ] || signatures=""
	mkdir -p "$reg/$name"
	printf '{"name":"%s","dist":{"shasum":"0","tarball":"https://registry.npmjs.org/%s","fileCount":2,"integrity":"%s"%s,"unpackedSize":1},"_id":"%s@%s","version":"%s"}' \
		"$name" "$name" "$integrity" "$signatures" "$name" "$version" "$version" >"$reg/$name/$version"
}

# release <registry> <mcp> <core> [signer]...: @playwright/mcp at mcp naming core, and playwright-core at core, each a
# tarball and a document signed by the signers (the pinned key without any), the two tarballs' SHA-256s left in
# $mcp_sha and $core_sha.
release() {
	local reg="$1" mcp="$2" core="$3"
	shift 3
	[ $# -gt 0 ] || set -- pinned
	mcp_sha="$(npmtar "$reg/@playwright/mcp/-/mcp-$mcp.tgz" @playwright/mcp "$mcp" "$core")"
	core_sha="$(npmtar "$reg/playwright-core/-/playwright-core-$core.tgz" playwright-core "$core")"
	versiondoc "$reg" @playwright/mcp "$mcp" "$@"
	versiondoc "$reg" playwright-core "$core" "$@"
}

# ============================================================================================================
# scripts/playwright-mcp.sh
# ============================================================================================================

# pw <case>: where the case's recipe writes the tree.
pw() {
	echo "$TMPROOT/$1/internal/browser/boxhelper/pw"
}

# snapshot <dir>: every directory under dir, and every file with its checksum, so a case can hold a refusal to leaving
# it as it was.
snapshot() {
	[ -d "$1" ] || return 0
	(cd "$1" && { find . -mindepth 1 -type d; find . -type f -exec cksum {} +; } | LC_ALL=C sort) || true
}

# fetch <case> <scripts dir> [env...]: runs the recipe over the case's registry, its log emptied first.
fetch() {
	local case_name="$1" s="$2"
	shift 2
	: >"$TMPROOT/curl.log"
	run env PLAYWRIGHT_MCP_PIN_REGISTRY="file://$(registry "$case_name")" "$@" "$s/playwright-mcp.sh"
}

# --- what the recipe refuses before it downloads anything --------------------------------------------------------

# Each refusal is exit 2, said on stderr, with nothing downloaded and pw/ as it was: the registry it would read holds
# the pin's real stand-in tree, so a recipe that went on would download (and the log would say so).
refused() { # <case> <name> <stderr-has> <cmd>...: runs the command expecting a refusal before any download
	local case_name="$1" name="$2" has="$3" before
	shift 3
	before="$(snapshot "$(pw "$case_name")")"
	: >"$TMPROOT/curl.log"
	run env PLAYWRIGHT_MCP_PIN_REGISTRY="file://$TMPROOT/refusals" "$@"
	expect "$name" 2 "" "$has"
	if [ -s "$TMPROOT/curl.log" ] || [ "$before" != "$(snapshot "$(pw "$case_name")")" ]; then
		bad "$name, downloading and writing nothing" "downloads: $(cat "$TMPROOT/curl.log")" \
			"pw/: $(snapshot "$(pw "$case_name")")"
	else
		ok "$name, downloading and writing nothing"
	fi
}
release "$TMPROOT/refusals" 0.0.85 1.66.0-alpha-2026-11-09
ref_mcp="$mcp_sha" ref_core="$core_sha"

s="$(scripts usage)"
pin "$s" 0.0.85 "$ref_mcp" 1.66.0-alpha-2026-11-09 "$ref_core"
refused usage "playwright-mcp.sh refuses an argument, with its usage" "usage: scripts/playwright-mcp.sh" \
	"$s/playwright-mcp.sh" newest

# The pin is read beside the recipe and nowhere else. Mutation: drop the recipe's check for its pin -> it still
# answers 2, its grep and sed failing over a file that is not there, but names no pin (red).
s="$(scripts nopin)"
refused nopin "playwright-mcp.sh refuses to run without its pin beside it, naming the file" \
	"$s/playwright-mcp-pin.env is missing" "$s/playwright-mcp.sh"

s="$(scripts twice)"
pin "$s" 0.0.85 "$ref_mcp" 1.66.0-alpha-2026-11-09 "$ref_core" "PLAYWRIGHT_MCP_VERSION=0.0.86"
refused twice "playwright-mcp.sh refuses a pin file setting a name twice, naming it" \
	"sets PLAYWRIGHT_MCP_VERSION 2 times" "$s/playwright-mcp.sh"

# A version outside its grammar never reaches a URL or a file name. Mutation: drop either grammar check -> the
# recipe asks the registry for what the pin says (red).
s="$(scripts mcpversion)"
pin "$s" 0.0.85-rc1 "$ref_mcp" 1.66.0-alpha-2026-11-09 "$ref_core"
refused mcpversion "playwright-mcp.sh refuses an @playwright/mcp version that is not three numbers, naming it" \
	"PLAYWRIGHT_MCP_VERSION in $s/playwright-mcp-pin.env is \"0.0.85-rc1\"" "$s/playwright-mcp.sh"
for version in "1.66.0-rc1" "../1.66.0" "1.66.0-alpha-2026-11-09/x" "1.66"; do
	s="$(scripts coreversion)"
	pin "$s" 0.0.85 "$ref_mcp" "$version" "$ref_core"
	refused coreversion "playwright-mcp.sh refuses a playwright-core version outside its grammar: $version" \
		"PLAYWRIGHT_CORE_VERSION in $s/playwright-mcp-pin.env is \"$version\"" "$s/playwright-mcp.sh"
	rm -rf "$TMPROOT/coreversion"
done

s="$(scripts shortsha)"
pin "$s" 0.0.85 "$ref_mcp" 1.66.0-alpha-2026-11-09 "${ref_core:1}"
refused shortsha "playwright-mcp.sh refuses a SHA-256 of 63 digits, naming the variable" \
	"PLAYWRIGHT_CORE_SHA256 in $s/playwright-mcp-pin.env is not 64 lowercase hex digits" "$s/playwright-mcp.sh"

# The tree is written into the box helper's package of the checkout the recipe sits in: with none beside it, it
# writes nowhere. Mutation: mkdir -p the package -> it fetches into a tree that is not this one (red).
s="$(scripts nohelper)"
rm -rf "$TMPROOT/nohelper/internal"
pin "$s" 0.0.85 "$ref_mcp" 1.66.0-alpha-2026-11-09 "$ref_core"
refused nohelper "playwright-mcp.sh refuses a checkout with no box helper beside it, naming the directory" \
	"$TMPROOT/nohelper/internal/browser/boxhelper is missing" "$s/playwright-mcp.sh"
if [ -e "$TMPROOT/nohelper/internal" ]; then
	bad "playwright-mcp.sh makes no box helper where there is none" "$(find "$TMPROOT/nohelper/internal")"
else
	ok "playwright-mcp.sh makes no box helper where there is none"
fi

# What a fetch runs, each asked for before the download: a PATH holding everything the recipe runs up to its download
# but the tool. Mutation: drop a tool from the recipe's check -> it downloads, and fails later (1, red).
for tool in curl gzip tar; do
	s="$(scripts "no$tool")"
	pin "$s" 0.0.85 "$ref_mcp" 1.66.0-alpha-2026-11-09 "$ref_core"
	mkdir -p "$TMPROOT/no$tool/bin"
	for have in dirname grep sed tr cut mkdir mktemp rm mv sha256sum shasum curl gzip tar; do
		[ "$have" != "$tool" ] || continue
		if found="$(command -v "$have")"; then ln -s "$found" "$TMPROOT/no$tool/bin/$have"; fi
	done
	[ "$tool" = curl ] || ln -sf "$TMPROOT/tools/curl" "$TMPROOT/no$tool/bin/curl"
	refused "no$tool" "playwright-mcp.sh refuses to fetch without $tool, naming it" "needs: $tool" \
		env PATH="$TMPROOT/no$tool/bin" "$s/playwright-mcp.sh"
done
s="$(scripts nosha)"
pin "$s" 0.0.85 "$ref_mcp" 1.66.0-alpha-2026-11-09 "$ref_core"
mkdir -p "$TMPROOT/nosha/bin"
for have in dirname grep sed tr cut mkdir mktemp rm mv gzip tar; do
	if found="$(command -v "$have")"; then ln -s "$found" "$TMPROOT/nosha/bin/$have"; fi
done
ln -s "$TMPROOT/tools/curl" "$TMPROOT/nosha/bin/curl"
refused nosha "playwright-mcp.sh refuses to fetch without a SHA-256 tool, naming both" "needs sha256sum, or shasum" \
	env PATH="$TMPROOT/nosha/bin" "$s/playwright-mcp.sh"

# --- a fetched tree is checked, and written whole ------------------------------------------------------------------

# A fresh fetch: both tarballs downloaded from the pin's own paths, checked, and renamed into pw/, which then holds
# them and nothing else — another release's tarball there removed, its working directory gone — and the two paths
# printed and nothing else on stdout. Mutation: leave another release's tarball in pw/ -> red; leave the working
# directory -> red.
s="$(scripts fresh)"
reg="$(registry fresh)"
release "$reg" 0.0.85 1.66.0-alpha-2026-11-09
pin "$s" 0.0.85 "$mcp_sha" 1.66.0-alpha-2026-11-09 "$core_sha"
mkdir -p "$(pw fresh)"
echo "the tree of an older pin" >"$(pw fresh)/mcp-0.0.84.tgz"
fetch fresh "$s"
dir="$(pw fresh)"
expect "playwright-mcp.sh fetches, checks and writes the tree" 0 "$dir/mcp-0.0.85.tgz"
why=()
[ "$out" = "$dir/mcp-0.0.85.tgz
$dir/playwright-core-1.66.0-alpha-2026-11-09.tgz" ] || why+=("stdout is not the two paths alone: $out")
[ "$(cd "$dir" && ls -A | LC_ALL=C sort | tr '\n' ' ')" = "mcp-0.0.85.tgz playwright-core-1.66.0-alpha-2026-11-09.tgz " ] ||
	why+=("pw/ holds $(ls -A "$dir" | tr '\n' ' '), not the two tarballs alone")
[ "$(digest "$dir/mcp-0.0.85.tgz")" = "$mcp_sha" ] && cmp -s "$dir/mcp-0.0.85.tgz" "$reg/@playwright/mcp/-/mcp-0.0.85.tgz" ||
	why+=("pw/'s @playwright/mcp is not the registry's bytes")
cmp -s "$dir/playwright-core-1.66.0-alpha-2026-11-09.tgz" "$reg/playwright-core/-/playwright-core-1.66.0-alpha-2026-11-09.tgz" ||
	why+=("pw/'s playwright-core is not the registry's bytes")
grep -qF "$reg/@playwright/mcp/-/mcp-0.0.85.tgz" "$TMPROOT/curl.log" &&
	grep -qF "$reg/playwright-core/-/playwright-core-1.66.0-alpha-2026-11-09.tgz" "$TMPROOT/curl.log" ||
	why+=("it did not download the pin's own tarballs: $(cat "$TMPROOT/curl.log")")
if [ ${#why[@]} -eq 0 ]; then
	ok "a fetched tree is written whole, in place of the release pw/ held"
else
	bad "a fetched tree is written whole, in place of the release pw/ held" "${why[@]}"
fi
# Mutation: drop the stall bound or --proto-redir from the recipe's download -> red.
bounded "every download the recipe makes ends when it stalls and follows a redirect to HTTPS alone"

# The recipe finds its own directory whatever CDPATH holds: scripts/git.sh's reason. Mutation: drop the recipe's
# `unset CDPATH` -> it finds no pin beside itself in the decoy's empty scripts/ and refuses (2, red).
mkdir -p "$TMPROOT/fresh/decoy/scripts"
run within "$TMPROOT/fresh" env CDPATH="$TMPROOT/fresh/decoy" PLAYWRIGHT_MCP_PIN_REGISTRY="file://$reg" \
	scripts/playwright-mcp.sh
expect "playwright-mcp.sh run by a relative path reads its own directory whatever CDPATH holds" 0 "$dir/mcp-0.0.85.tgz"

# WHAT THE RECIPE READS WHEN NOTHING NAMES IT: its registry is npm's own whenever PLAYWRIGHT_MCP_PIN_REGISTRY is unset,
# as the suite leaves it, having cleared the person's. A stand-in curl serves that registry from a local tree, so the
# case reads no network. Mutation: misspell the default registry -> red; drop the suite's PLAYWRIGHT_MCP_PIN_ scrub ->
# the nested run's copy of this case reads the developer's registry, which does not exist (red).
s="$(scripts defaults)"
web="$TMPROOT/defaults/web"
release "$web/registry.npmjs.org" 0.0.85 1.66.0-alpha-2026-11-09
pin "$s" 0.0.85 "$mcp_sha" 1.66.0-alpha-2026-11-09 "$core_sha"
run env PATH="$(offline "$web"):$PATH" "$s/playwright-mcp.sh"
expect "playwright-mcp.sh reads npm's own registry when nothing names another" 0 "$(pw defaults)/mcp-0.0.85.tgz"

# A tarball that is not the pin's is refused before anything is written, naming both digests, and pw/ keeps the
# release it held. Mutation: accept a tarball without comparing its digest -> exit 0 (red).
s="$(scripts tampered)"
reg="$(registry tampered)"
release "$reg" 0.0.85 1.66.0-alpha-2026-11-09
served="$core_sha"
pin "$s" 0.0.85 "$mcp_sha" 1.66.0-alpha-2026-11-09 "$zero"
mkdir -p "$(pw tampered)"
echo "the tree of the pin before" >"$(pw tampered)/mcp-0.0.84.tgz"
before="$(snapshot "$(pw tampered)")"
fetch tampered "$s"
expect "playwright-mcp.sh refuses a tarball that is not the pin's (1), naming both digests" 1 "" \
	"playwright-core-1.66.0-alpha-2026-11-09.tgz hashes to $served, and"
if [ "$before" = "$(snapshot "$(pw tampered)")" ]; then
	ok "a tarball that is not the pin's leaves pw/ as it was"
else
	bad "a tarball that is not the pin's leaves pw/ as it was" "$(snapshot "$(pw tampered)")"
fi

# An @playwright/mcp whose own package.json names another playwright-core than the pin's is refused, whatever its
# digest: the two lines move together. So is one that names none, or names it twice. Mutation: skip the check -> the
# recipe writes a tree whose server would load another core (0, red); read the core from devDependencies -> 9.9.9 is
# refused for every tarball, the fresh fetch's among them (red).
for shape in another none twice; do
	s="$(scripts "core-$shape")"
	reg="$(registry "core-$shape")"
	release "$reg" 0.0.85 1.66.0-alpha-2026-11-09
	file="$reg/@playwright/mcp/-/mcp-0.0.85.tgz"
	case $shape in
	another) mcp_sha="$(npmtar "$file" @playwright/mcp 0.0.85 1.66.0-alpha-2026-11-18)" ;;
	none) mcp_sha="$(npmtar "$file" @playwright/mcp 0.0.85)" ;;
	twice)
		src="$TMPROOT/twice-src"
		mkdir -p "$src/package"
		printf '{\n  "name": "@playwright/mcp",\n  "version": "0.0.85",\n  "dependencies": {\n    "playwright-core": "1.66.0-alpha-2026-11-09",\n    "playwright-core": "1.66.0-alpha-2026-11-18"\n  }\n}\n' \
			>"$src/package/package.json"
		tar -czf "$file" -C "$src" package
		mcp_sha="$(digest "$file")"
		;;
	esac
	pin "$s" 0.0.85 "$mcp_sha" 1.66.0-alpha-2026-11-09 "$core_sha"
	case $shape in
	another) naming="another playwright-core than the pin's" ;;
	none) naming="no playwright-core" ;;
	twice) naming="playwright-core twice" ;;
	esac
	before="$(snapshot "$(pw "core-$shape")")"
	fetch "core-$shape" "$s"
	expect "playwright-mcp.sh refuses an @playwright/mcp naming $naming (1)" 1 "" "names playwright-core"
	[ "$before" = "$(snapshot "$(pw "core-$shape")")" ] &&
		ok "an @playwright/mcp naming $naming leaves pw/ as it was" ||
		bad "an @playwright/mcp naming $naming leaves pw/ as it was" "$(snapshot "$(pw "core-$shape")")"
done

# A tarball the registry does not serve is a fetch that failed (1), and pw/ keeps what it held.
s="$(scripts absent)"
reg="$(registry absent)"
release "$reg" 0.0.85 1.66.0-alpha-2026-11-09
rm "$reg/playwright-core/-/playwright-core-1.66.0-alpha-2026-11-09.tgz"
pin "$s" 0.0.85 "$mcp_sha" 1.66.0-alpha-2026-11-09 "$core_sha"
fetch absent "$s"
expect "playwright-mcp.sh fails (1) on a tarball the registry does not serve, naming it" 1 "" \
	"could not download file://$reg/playwright-core/-/playwright-core-1.66.0-alpha-2026-11-09.tgz"
[ -z "$(ls -A "$(pw absent)" 2>/dev/null)" ] &&
	ok "a tarball the registry does not serve leaves pw/ empty" ||
	bad "a tarball the registry does not serve leaves pw/ empty" "$(snapshot "$(pw absent)")"

# A tarball whose SHA-256 the machine's hash cannot compute is a tarball nothing checked: refused (1), naming it, never
# a digest of nothing compared against the pin. Mutation: let digest answer whatever its hash printed -> "hashes to ,
# and" (red).
for mode in fails garbles; do
	s="$(scripts "hash$mode")"
	reg="$(registry "hash$mode")"
	release "$reg" 0.0.85 1.66.0-alpha-2026-11-09
	pin "$s" 0.0.85 "$mcp_sha" 1.66.0-alpha-2026-11-09 "$core_sha"
	hashtool "$TMPROOT/hash$mode/tools" "$mode" '*.tgz'
	fetch "hash$mode" "$s" PATH="$TMPROOT/hash$mode/tools:$PATH"
	expect "playwright-mcp.sh refuses a tarball whose SHA-256 the machine's hash $mode over (1)" 1 "" \
		"cannot compute the SHA-256 of mcp-0.0.85.tgz"
done

# --- a curl configuration changes nothing the recipe prints ------------------------------------------------------

# The recipe's stdout is the two paths and nothing else: a `write-out` in a person's curlrc once put a 000 above
# scripts/git.sh's directory. Mutation: let the recipe's download write curl's stdout to its own -> red.
s="$(scripts curlrc)"
reg="$(registry curlrc)"
release "$reg" 0.0.85 1.66.0-alpha-2026-11-09
pin "$s" 0.0.85 "$mcp_sha" 1.66.0-alpha-2026-11-09 "$core_sha"
fetch curlrc "$s" CURL_HOME="$(curlhome "$TMPROOT/curlrc/curl")"
why=()
[ "$status" = 0 ] || why+=("exit $status, want 0")
[ "$out" = "$(pw curlrc)/mcp-0.0.85.tgz
$(pw curlrc)/playwright-core-1.66.0-alpha-2026-11-09.tgz" ] || why+=("its stdout is not the two paths alone")
grep -qx 000 <<<"$err" || why+=("stderr lacks the curlrc's write-out, 000: the configuration was not read")
if [ ${#why[@]} -eq 0 ]; then
	ok "a curlrc that writes to curl's stdout changes nothing the recipe prints"
else
	bad "a curlrc that writes to curl's stdout changes nothing the recipe prints" "${why[@]}" "stdout:" "$out" \
		"stderr:" "$err"
fi

# ============================================================================================================
# scripts/playwright-mcp-pin-check.sh
# ============================================================================================================

# checkcase <case> [key]: a checkout pinned at @playwright/mcp 0.0.85 and playwright-core 1.66.0-alpha-2026-11-09,
# whose key file is the named key's public half (the pinned one without a name), and a registry naming that release
# latest; prints the scripts directory.
checkcase() {
	local s
	s="$(scripts "$1")"
	pin "$s" 0.0.85 "$zero" 1.66.0-alpha-2026-11-09 "$zero"
	cp "$keys/${2:-pinned}.pub.pem" "$s/npm-registry-key.pem"
	disttags "$(registry "$1")" 0.0.85
	echo "$s"
}

# pins <case> <scripts dir> [env...]: runs the check against the case's registry, holding the key file to the pinned
# key's id unless the environment says another, its log emptied first.
pins() {
	local case_name="$1" s="$2"
	shift 2
	: >"$TMPROOT/curl.log"
	run env PLAYWRIGHT_MCP_PIN_REGISTRY="file://$(registry "$case_name")" PLAYWRIGHT_MCP_PIN_KEYID="$pinned_id" "$@" \
		"$s/playwright-mcp-pin-check.sh"
}

s="$(checkcase current)"
pins current "$s"
expect "the pin @playwright/mcp's latest release -> 0" 0 "the pin is @playwright/mcp's latest release: 0.0.85"
if grep -q 'dist-tags' "$TMPROOT/curl.log" && [ "$(wc -l <"$TMPROOT/curl.log" | tr -d ' ')" = 1 ]; then
	ok "a current pin reads the dist-tags and nothing else"
else
	bad "a current pin reads the dist-tags and nothing else" "downloads: $(cat "$TMPROOT/curl.log")"
fi

# A newer release is the pin's bump: both versions, the SHA-256s of the two tarballs the check downloaded, whose
# integrity the pinned key signed, and the pull request's title. Mutation: hash the core's tarball for both -> red.
s="$(checkcase behind)"
reg="$(registry behind)"
disttags "$reg" 0.0.86
release "$reg" 0.0.86 1.66.0-alpha-2026-11-18
pins behind "$s"
expect "a newer release -> 1 with its version" 1 "PLAYWRIGHT_MCP_VERSION=0.0.86"
expect "a newer release -> 1 with its tarball's SHA-256" 1 "PLAYWRIGHT_MCP_SHA256=$mcp_sha"
expect "a newer release -> 1 with the core its own package.json names" 1 "PLAYWRIGHT_CORE_VERSION=1.66.0-alpha-2026-11-18"
expect "a newer release -> 1 with that core's tarball's SHA-256" 1 "PLAYWRIGHT_CORE_SHA256=$core_sha"
expect "a pin behind names its pull request" 1 "\`build(deps): bump playwright mcp from 0.0.85 to 0.0.86\`"
# Mutation: drop the stall bound or --proto-redir from the check's fetch -> red.
bounded "every download the check makes ends when it stalls and follows a redirect to HTTPS alone"

# WHAT THE CHECK READS WHEN NOTHING NAMES IT: npm's own registry, and npm's own key id, whenever the two knobs are unset,
# as the suite leaves them. A stand-in curl serves the registry from a local tree; the key is the pinned stand-in, so
# the id the check holds it to is told apart from the default by the refusal naming npm's. Mutation: misspell the
# default registry -> "cannot read" (red); default the id to anything but npm's -> it names another (red).
s="$(checkcase check-defaults)"
web="$TMPROOT/check-defaults/web"
disttags "$web/registry.npmjs.org" 0.0.86
release "$web/registry.npmjs.org" 0.0.86 1.66.0-alpha-2026-11-18
run env PATH="$(offline "$web"):$PATH" "$s/playwright-mcp-pin-check.sh"
expect "the check holds the key file to npm's own key id when nothing names another" 2 "" \
	"must hold the key whose id is SHA256:DhQ8wR5APBvFHLF/+Tc+AYvPOdTpcIDqOhxsBHRwC7U"
run env PATH="$(offline "$web"):$PATH" PLAYWRIGHT_MCP_PIN_KEYID="$pinned_id" "$s/playwright-mcp-pin-check.sh"
expect "the check reads npm's own registry when nothing names another" 1 "PLAYWRIGHT_MCP_SHA256=$mcp_sha"

# A CURL CONFIGURATION CHANGES NOTHING THE CHECK PRINTS: scripts/git-pin_test.sh's case and its reason. Mutation: let
# the check's fetch write curl's stdout to its own -> red.
s="$(checkcase curlrc-check)"
reg="$(registry curlrc-check)"
disttags "$reg" 0.0.86
release "$reg" 0.0.86 1.66.0-alpha-2026-11-18
pins curlrc-check "$s"
plain="$out"
pins curlrc-check "$s" CURL_HOME="$(curlhome "$TMPROOT/curlrc-check/curl")"
why=()
[ "$status" = 1 ] || why+=("exit $status, want 1")
[ "$out" = "$plain" ] || why+=("its stdout is not the report a run without the curlrc printed:" "$plain")
grep -qx 000 <<<"$err" || why+=("stderr lacks the curlrc's write-out, 000: the configuration was not read")
if [ ${#why[@]} -eq 0 ]; then
	ok "a curlrc that writes to curl's stdout changes nothing the check prints"
else
	bad "a curlrc that writes to curl's stdout changes nothing the check prints" "${why[@]}" "stdout:" "$out" \
		"stderr:" "$err"
fi

# --- what the check can never call a pass ------------------------------------------------------------------------

# A DIST-TAG IS A RELEASE OR NOTHING. Anything else is refused before it is used: no further download, an empty report,
# and a refusal that says the answer's length and never its text — three numbers longer than a release's 32 characters
# among them. Mutation: take `latest` without checking it -> it reaches a download and the refusal (red); drop the
# length bound -> the long one reaches a download (red).
for answer in "0.0.86 ::error::injected" "../../0.0.86" "<html>injected</html>" "0.0" "" \
	"0.0.$(printf '%029d' 86)"; do
	s="$(checkcase notaversion)"
	disttags "$(registry notaversion)" "$answer"
	pins notaversion "$s"
	why=()
	[ "$status" = 2 ] || why+=("exit $status, want 2")
	[ -z "$out" ] || why+=("it reported: $out")
	grep -qF "that are not a release (three numbers); nothing of it is used" <<<"$err" ||
		why+=("it did not say the answer is no release")
	if grep -qF "injected" <<<"$err$out" || [ "$(wc -l <"$TMPROOT/curl.log" | tr -d ' ')" != 1 ]; then
		why+=("the answer reached the output or a download")
	fi
	if [ ${#why[@]} -eq 0 ]; then
		ok "a dist-tag of $(printf '%s' "$answer" | wc -c | tr -d ' ') bytes that are no release -> 2, nothing of it used"
	else
		bad "a dist-tag that is no release -> 2, nothing of it used" "${why[@]}" "$err"
	fi
	rm -rf "$TMPROOT/notaversion"
done

# A dist-tags document giving `latest` twice, or as a string holding an escape, names no one release. Mutation: take
# the first of two -> 0.0.86 is read (1, red); read past an escaped quote -> red.
s="$(checkcase twotags)"
printf '{"latest":"0.0.86","latest":"0.0.85"}' >"$(registry twotags)/-/package/@playwright/mcp/dist-tags"
pins twotags "$s"
expect "dist-tags giving latest twice -> 2" 2 "" "give no one \`latest\`"
s="$(checkcase escaped)"
printf '{"latest":"0.0.86\\"injected"}' >"$(registry escaped)/-/package/@playwright/mcp/dist-tags"
pins escaped "$s"
expect "dist-tags giving latest with an escape in it -> 2" 2 "" "give no one \`latest\`"
grep -qF injected <<<"$err$out" && bad "an escaped dist-tag is never said" "$err" || ok "an escaped dist-tag is never said"

s="$(scripts nodisttags)"
pin "$s" 0.0.85 "$zero" 1.66.0-alpha-2026-11-09 "$zero"
cp "$keys/pinned.pub.pem" "$s/npm-registry-key.pem"
pins nodisttags "$s"
expect "dist-tags that do not load -> 2" 2 "" "cannot read @playwright/mcp's dist-tags"

# A pin ahead of the registry's latest means the registry is behind upstream, never that the pin should go back.
s="$(checkcase ahead)"
disttags "$(registry ahead)" 0.0.84
pins ahead "$s"
expect "a pin newer than the registry's latest -> 2" 2 "" "that registry is behind upstream"

# A tarball whose SHA-512 is not the registry's integrity for it is not the release the registry signed. Mutation:
# skip the comparison -> 1 (red).
s="$(checkcase integrity)"
reg="$(registry integrity)"
disttags "$reg" 0.0.86
release "$reg" 0.0.86 1.66.0-alpha-2026-11-18
npmtar "$reg/@playwright/mcp/-/mcp-0.0.86.tgz" @playwright/mcp 0.0.86 1.66.0-alpha-2026-11-17 >/dev/null
pins integrity "$s"
expect "a tarball whose SHA-512 is not its dist.integrity -> 2" 2 "" "@playwright/mcp@0.0.86's tarball hashes to sha512-"
[ -z "$out" ] && ok "a tarball that is not its integrity reports nothing" || bad "a tarball that is not its integrity reports nothing" "$out"

# An integrity that is no SHA-512 is refused by its shape, its text never said.
s="$(checkcase sha1)"
reg="$(registry sha1)"
disttags "$reg" 0.0.86
release "$reg" 0.0.86 1.66.0-alpha-2026-11-18
INTEGRITY="sha1-injected=" versiondoc "$reg" @playwright/mcp 0.0.86 pinned
pins sha1 "$s"
expect "an integrity that is no SHA-512 -> 2" 2 "" "that are not a SHA-512 integrity"
grep -qF injected <<<"$err$out" && bad "an integrity that is no SHA-512 is never said" "$err" ||
	ok "an integrity that is no SHA-512 is never said"

# EVERY SIGNATURE IS UNDER THE PINNED KEY AND VERIFIES. A signature made by another key under the pinned id does not
# verify; one under another id is refused naming both ids; a document carrying none, or an empty list, is refused.
# Mutations: skip the verification -> the forged one passes (1, red); accept any key id -> red; let an empty list
# pass -> red; verify only the first signature -> the second, forged, passes (red).
s="$(checkcase forged)"
reg="$(registry forged)"
disttags "$reg" 0.0.86
release "$reg" 0.0.86 1.66.0-alpha-2026-11-18 other:pinned
pins forged "$s"
expect "a signature made by another key under the pinned id -> 2" 2 "" "signature 1 of @playwright/mcp@0.0.86 does not verify"
s="$(checkcase otherid)"
reg="$(registry otherid)"
disttags "$reg" 0.0.86
release "$reg" 0.0.86 1.66.0-alpha-2026-11-18 other
pins otherid "$s"
expect "a signature under another key id -> 2, naming both" 2 "" "is under key $other_id, and the key the check holds is $pinned_id"
s="$(checkcase secondforged)"
reg="$(registry secondforged)"
disttags "$reg" 0.0.86
release "$reg" 0.0.86 1.66.0-alpha-2026-11-18 pinned other:pinned
pins secondforged "$s"
expect "a good signature beside a forged one -> 2" 2 "" "signature 2 of @playwright/mcp@0.0.86 does not verify"
for shape in empty absent; do
	s="$(checkcase "unsigned-$shape")"
	reg="$(registry "unsigned-$shape")"
	disttags "$reg" 0.0.86
	release "$reg" 0.0.86 1.66.0-alpha-2026-11-18
	if [ "$shape" = empty ]; then
		versiondoc "$reg" @playwright/mcp 0.0.86
	else
		NOSIGNATURES=1 versiondoc "$reg" @playwright/mcp 0.0.86 pinned
	fi
	pins "unsigned-$shape" "$s"
	expect "a version whose signatures are $shape -> 2" 2 "" "carries no signature"
done
# The core's document is held as the server's is. Mutation: verify @playwright/mcp alone -> 1 (red).
s="$(checkcase coreforged)"
reg="$(registry coreforged)"
disttags "$reg" 0.0.86
release "$reg" 0.0.86 1.66.0-alpha-2026-11-18
versiondoc "$reg" playwright-core 1.66.0-alpha-2026-11-18 other:pinned
pins coreforged "$s"
expect "a playwright-core signed by another key -> 2" 2 "" "of playwright-core@1.66.0-alpha-2026-11-18 does not verify"

# A SIGNATURE'S MEMBERS ARE OF THEIR SHAPES OR NOTHING: a key id that is no SHA-256 key id, a sig that is no base64, and
# one of base64 longer than a P-256 signature's 96 characters are refused by their shape before the comparison, the
# decoder or the report reads them, their text never said. Mutations: drop the key id's grammar -> the comparison
# names it (red); drop the sig's -> the text one reaches the decoder (red); raise the sig's bound -> the long one
# does (red).
longsig="$(printf 'QUFB%.0s' $(seq 25))"
for member in "keyid|SHA256:injected|names a key id of 15 bytes that is no SHA-256 key id" \
	"sig|injected!|is 9 bytes that are not a P-256 signature in base64" \
	"sig|$longsig|is 100 bytes that are not a P-256 signature in base64"; do
	IFS='|' read -r which value says <<<"$member"
	s="$(checkcase shapes)"
	reg="$(registry shapes)"
	disttags "$reg" 0.0.86
	release "$reg" 0.0.86 1.66.0-alpha-2026-11-18
	if [ "$which" = keyid ]; then
		KEYID="$value" versiondoc "$reg" @playwright/mcp 0.0.86 pinned
	else
		SIG="$value" versiondoc "$reg" @playwright/mcp 0.0.86 pinned
	fi
	pins shapes "$s"
	expect "a signature whose $which $says -> 2" 2 "" "signature 1 of @playwright/mcp@0.0.86 $says"
	if grep -qF -e injected -e "$longsig" <<<"$err$out"; then
		bad "a signature whose $which is of the wrong shape is never said" "$err"
	else
		ok "a signature whose $which is of the wrong shape is never said"
	fi
	rm -rf "$TMPROOT/shapes"
done

# THE KEY FILE IS THE PINNED KEY AND NOTHING ELSE, held to its id before anything is verified with it: another P-256
# key, a key of another curve, an RSA key, and a file that holds no key. Mutation: skip the id comparison -> the other
# key, whose signatures the registry's documents carry, verifies them (1, red); drop the curve check -> the P-384 and
# RSA keys are read as points, and refused only as other ids (red).
s="$(checkcase otherfile other)"
reg="$(registry otherfile)"
disttags "$reg" 0.0.86
release "$reg" 0.0.86 1.66.0-alpha-2026-11-18 other
pins otherfile "$s"
expect "a key file that is not the pinned key -> 2, naming the id it holds" 2 "" \
	"must hold the key whose id is $pinned_id; it holds $other_id"
openssl ecparam -name secp384r1 -genkey -noout 2>/dev/null | openssl pkey -pubout -out "$keys/p384.pub.pem" 2>/dev/null
openssl genrsa 2048 2>/dev/null | openssl pkey -pubout -out "$keys/rsa.pub.pem" 2>/dev/null
for kind in p384 rsa; do
	s="$(checkcase "keyfile-$kind" "$kind")"
	pins "keyfile-$kind" "$s"
	expect "a key file holding a $kind key -> 2" 2 "" "is not a P-256 public key"
done
s="$(checkcase nokey)"
echo "not a key" >"$s/npm-registry-key.pem"
pins nokey "$s"
expect "a key file that holds no key -> 2" 2 "" "cannot read $s/npm-registry-key.pem as a public key"

# A core the release names that is no playwright-core release is refused before it reaches a URL or the report, one
# of its grammar longer than a core's 64 characters among them. Mutation: take the dependency unchecked -> it reaches a
# download (red); drop the length bound -> the long one does (red).
for core in '../../evil' '1.66.0;injected' '1.66.0-rc1' "1.66.0-alpha-$(printf '%052d' 9)"; do
	s="$(checkcase badcore)"
	reg="$(registry badcore)"
	disttags "$reg" 0.0.86
	npmtar "$reg/@playwright/mcp/-/mcp-0.0.86.tgz" @playwright/mcp 0.0.86 "$core" >/dev/null
	versiondoc "$reg" @playwright/mcp 0.0.86 pinned
	pins badcore "$s"
	why=()
	[ "$status" = 2 ] || why+=("exit $status, want 2")
	grep -qF "that is not a playwright-core release; nothing of it is used" <<<"$err" || why+=("no refusal of the core")
	grep -qF 'playwright-core/' "$TMPROOT/curl.log" && why+=("the core reached a download: $(cat "$TMPROOT/curl.log")")
	grep -qF injected <<<"$err$out" && why+=("the core reached the output")
	if [ ${#why[@]} -eq 0 ]; then
		ok "a release naming a core of $(printf '%s' "$core" | wc -c | tr -d ' ') bytes that is no release -> 2, unused"
	else
		bad "a release naming a core that is no release -> 2, unused" "${why[@]}" "$err"
	fi
	rm -rf "$TMPROOT/badcore"
done

# A release whose package.json names no playwright-core, or no dependencies at all, names no core to pin.
s="$(checkcase nodeps)"
reg="$(registry nodeps)"
disttags "$reg" 0.0.86
release "$reg" 0.0.86 1.66.0-alpha-2026-11-18
npmtar "$reg/@playwright/mcp/-/mcp-0.0.86.tgz" @playwright/mcp 0.0.86 >/dev/null
versiondoc "$reg" @playwright/mcp 0.0.86 pinned
pins nodeps "$s"
expect "a release whose package.json gives no dependencies -> 2" 2 "" "gives no one dependencies object"

# A tarball the registry's document names but does not serve is a download refused, never a pass.
s="$(checkcase notarball)"
reg="$(registry notarball)"
disttags "$reg" 0.0.86
release "$reg" 0.0.86 1.66.0-alpha-2026-11-18
rm "$reg/playwright-core/-/playwright-core-1.66.0-alpha-2026-11-18.tgz"
pins notarball "$s"
expect "a tarball the registry does not serve -> 2" 2 "" "cannot download file://$reg/playwright-core/-/playwright-core-1.66.0-alpha-2026-11-18.tgz"

# A hash that gives no SHA-256 is "cannot tell", never a pin's new line. Mutation: write whatever the hash printed ->
# exit 1 with a digest that is no digest (red).
for mode in fails garbles; do
	s="$(checkcase "checkhash$mode")"
	reg="$(registry "checkhash$mode")"
	disttags "$reg" 0.0.86
	release "$reg" 0.0.86 1.66.0-alpha-2026-11-18
	hashtool "$TMPROOT/checkhash$mode/tools" "$mode" '*.tgz'
	pins "checkhash$mode" "$s" PATH="$TMPROOT/checkhash$mode/tools:$PATH"
	expect "a hash that $mode over a release's tarball -> 2" 2 "" "cannot compute the SHA-256 of mcp-0.0.86.tgz"
done

# The check finds its own directory whatever CDPATH holds. Mutation: drop the check's `unset CDPATH` -> it reads the
# decoy's empty scripts/, finds no key file there, and answers 2 (red).
s="$(checkcase cdpath-check)"
mkdir -p "$TMPROOT/cdpath-check/decoy/scripts"
run within "$TMPROOT/cdpath-check" env CDPATH="$TMPROOT/cdpath-check/decoy" \
	PLAYWRIGHT_MCP_PIN_REGISTRY="file://$(registry cdpath-check)" PLAYWRIGHT_MCP_PIN_KEYID="$pinned_id" \
	scripts/playwright-mcp-pin-check.sh
expect "the check run by a relative path reads its own directory whatever CDPATH holds -> 0" 0 "latest release: 0.0.85"

# THE PIN AND THE KEY THIS TREE SHIPS are whole: its versions are of their grammars, and its key file is npm's own key,
# held to the id npm publishes for it with no id named here — so a registry naming exactly that release answers 0,
# never 2, which would mean the pin file is malformed or the key file is not npm's.
s="$(scripts shipped)"
cp "$SCRIPT_DIR/playwright-mcp-pin.env" "$SCRIPT_DIR/npm-registry-key.pem" "$s/"
disttags "$(registry shipped)" "$(sed -n 's/^PLAYWRIGHT_MCP_VERSION=//p' "$s/playwright-mcp-pin.env")"
run env PLAYWRIGHT_MCP_PIN_REGISTRY="file://$(registry shipped)" "$s/playwright-mcp-pin-check.sh"
expect "the pin file and the key file this tree ships read as current against a registry naming the pin" 0 \
	"the pin is @playwright/mcp's latest release"

# --- the suite reads nothing of the environment of the person running it ------------------------------------------

# This suite started again under a developer's environment at its most hostile: every knob the scripts read set to
# send a case somewhere else (a registry that does not exist, a key id no key has), an openssl configuration that
# withdraws every digest, and a curl configuration refusing every protocol but https — every download a case makes
# with the real curl is file:// — in every place curl looks for one: $CURL_HOME, $XDG_CONFIG_HOME, $HOME and
# $HOME/.config, with and without the dot. And it is run as `make playwright-mcp-test` runs it, by a path relative to
# the tree, under a CDPATH whose one entry holds a scripts/ of its own, empty. The nested run must pass, having run its
# cases, and leave every file of that environment exactly as it found it.
#
# Mutation: drop the CURL_HOME unset, or the XDG_CONFIG_HOME or the HOME pin -> the nested run's downloads are refused
# (red). Mutation: drop the PLAYWRIGHT_MCP_PIN_ scrub -> the nested run's cases that hold the scripts' defaults read
# the developer's registry and key id (red). Mutation: drop the OPENSSL_CONF unset -> no digest is computed (red).
# Mutation: drop the CDPATH unset -> the nested run finds its own directory in the decoy, has no scripts to copy from
# it, and fails at its first case (red).
if [ -z "${CREWLET_PIN_TEST_NESTED-}" ]; then
	dev="$TMPROOT/dev"
	mkdir -p "$dev/curl/.config" "$dev/config" "$dev/home/.config" "$dev/cdpath/$(basename "$SCRIPT_DIR")"
	for rc in "$dev/curl/.curlrc" "$dev/curl/curlrc" "$dev/curl/.config/curlrc" "$dev/curl/.config/.curlrc" \
		"$dev/config/.curlrc" "$dev/config/curlrc" \
		"$dev/home/.curlrc" "$dev/home/.config/curlrc" "$dev/home/.config/.curlrc"; do
		echo 'proto = "=https"' >"$rc"
	done
	printf 'openssl_conf = init\n[init]\nproviders = providers\n[providers]\nnone = none\n[none]\nactivate = 1\n' \
		>"$dev/openssl.cnf"
	devsnapshot() { find "$dev" -type f -exec cksum {} + | LC_ALL=C sort; }
	before="$(devsnapshot)"
	nested="$(within "$(dirname "$SCRIPT_DIR")" env CREWLET_PIN_TEST_NESTED=1 \
		CURL_HOME="$dev/curl" XDG_CONFIG_HOME="$dev/config" HOME="$dev/home" CDPATH="$dev/cdpath" \
		OPENSSL_CONF="$dev/openssl.cnf" PLAYWRIGHT_MCP_PIN_REGISTRY="file://$dev/no-registry" \
		PLAYWRIGHT_MCP_PIN_KEYID="SHA256:$zero" \
		"$(basename "$SCRIPT_DIR")/$(basename "${BASH_SOURCE[0]}")" 2>&1)" && status=0 || status=$?
	after="$(devsnapshot)"
	if [ "$status" -eq 0 ] && grep -q '^ok   a fetched tree is written whole, in place of the release pw/ held$' <<<"$nested" &&
		grep -q "^ok   the pin @playwright/mcp's latest release -> 0$" <<<"$nested" &&
		grep -q ' 0 failed$' <<<"$nested" && [ "$before" = "$after" ]; then
		ok "under a developer's environment the suite passes and touches nothing of it"
	else
		bad "under a developer's environment the suite passes and touches nothing of it" "exit $status" \
			"the environment's files changed: $([ "$before" = "$after" ] && echo no || echo yes)" "$nested"
	fi
fi

echo
echo "playwright-mcp-pin_test.sh: $pass passed, $fail failed"
[ "$fail" -eq 0 ]

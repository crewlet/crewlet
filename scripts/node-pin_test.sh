#!/usr/bin/env bash
#
# node-pin_test.sh — the own suite of the floor Node's recipe (scripts/node.sh) and of its pin check
# (scripts/node-pin-check.sh).
#
# No Go test reaches either script, and each regresses silently in the direction that matters: a recipe that unpacks
# into the directory another checkout's suite is running its Node from, hands back a build it never checked, or prints
# something above the path a make target exports as the suite's floor; a check that calls the pin current because it
# read nothing, proposes a release whose signature it never asked about or a tarball the releasers never signed, or
# carries an index's text into the issue it reports to. So every case runs the REAL script — copied beside a pin file
# (and the check's key file) the case writes, because both read them beside themselves — against local stand-ins: a
# nodejs.org dist tree under file:// (an index.tab, and each release's tarballs, each holding a stand-in `node` that
# answers --version, beside a SHASUMS256.txt and its signature), a release schedule, releasers' keys made for the suite
# in a GnuPG home of its own, a stand-in `uname` that names the platform each case runs as, and, for the cases that
# hold each script's defaults, a stand-in curl that serves nodejs.org's and GitHub's own addresses from a local tree.
# It fetches no Node and reads no network, and it needs bash, curl, xz, tar, GnuPG and a SHA-256 tool.
set -euo pipefail

# THE PERSON'S ENVIRONMENT, GONE before the first case, as scripts/git-pin_test.sh clears it and for its reasons: every
# variable whose name begins NODE_PIN_ — the scripts' own knobs — the cache (CREWLET_NODE_CACHE, XDG_CACHE_HOME), the
# GnuPG home, curl's own configuration (CURL_HOME here, and HOME and XDG_CONFIG_HOME made the suite's own below) and
# CDPATH. NODE_OPTIONS stays: the recipe must ask a build its version without it, which a case holds and the nested run
# below holds again. The suite's last case runs the whole suite again, by a relative path, under a hostile environment
# holding every one of these and requires it green and the hostile directories untouched.
for name in $(compgen -e); do
	case "$name" in
	NODE_PIN_*) unset "$name" ;;
	esac
done
unset CREWLET_NODE_CACHE XDG_CACHE_HOME GNUPGHOME CURL_HOME CDPATH

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

missing=""
for tool in curl xz tar gpg gpgconf mkfifo; do
	command -v "$tool" >/dev/null 2>&1 || missing="$missing $tool"
done
command -v sha256sum >/dev/null 2>&1 || command -v shasum >/dev/null 2>&1 || missing="$missing sha256sum-or-shasum"
if [ -n "$missing" ]; then
	echo "node-pin_test.sh needs:$missing — CONTRIBUTING.md, Development setup, says what to install" >&2
	exit 1
fi

# A short root: GnuPG's agent puts its socket inside the home it serves wherever there is no /run/user, and a socket
# path past 107 bytes cannot be bound.
TMPROOT="$(mktemp -d)"
GPGHOMES=()
killtree() { # <pid>: the process and every process below it, children first
	local child
	for child in $(pgrep -P "$1" 2>/dev/null); do
		killtree "$child"
	done
	kill "$1" 2>/dev/null || true
}
cleanup() {
	# A fetcher a failed case left held is stopped with everything it started, and every GnuPG home's agents, before
	# their directories go.
	local pid home
	for pid in $(jobs -p); do
		killtree "$pid"
	done
	wait 2>/dev/null || true
	for home in ${GPGHOMES[@]+"${GPGHOMES[@]}"}; do
		GNUPGHOME="$home" gpgconf --kill all >/dev/null 2>&1 || true
	done
	rm -rf "$TMPROOT"
}
trap cleanup EXIT

export HOME="$TMPROOT/home"
export XDG_CONFIG_HOME="$TMPROOT/config"
mkdir -p "$HOME" "$XDG_CONFIG_HOME"

# The suite's tools, first on PATH: curl through a recorder, so a case can hold the scripts to how they download as
# well as to what, and a stand-in uname naming the platform a case runs as: Linux on x86_64 unless the case says
# otherwise (UNAME_S, UNAME_M), so every case runs the same on any machine.
mkdir -p "$TMPROOT/tools"
cat >"$TMPROOT/tools/curl" <<EOF
#!/bin/sh
printf '%s\n' "\$*" >>"$TMPROOT/curl.log"
exec "$(command -v curl)" "\$@"
EOF
cat >"$TMPROOT/tools/uname" <<'EOF'
#!/bin/sh
case $1 in
-s) echo "${UNAME_S:-Linux}" ;;
-m) echo "${UNAME_M:-x86_64}" ;;
*) echo "the suite's uname answers -s and -m alone" >&2; exit 2 ;;
esac
EOF
chmod +x "$TMPROOT/tools/curl" "$TMPROOT/tools/uname"
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

# bounded <name> [https]: every download logged since the log was emptied gives up on a transfer that stalls — and,
# given https, follows a redirect to an https:// address alone, read off the arguments since the suite's addresses
# are file:// and redirect nowhere.
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
		if [ -n "${2-}" ]; then
			case " $line " in
			*" --proto-redir =https "*) ;;
			*) why+=("a redirect off HTTPS followed: curl $line") ;;
			esac
		fi
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
# <dir> first: over a file matching the pattern it fails having printed nothing, or prints a line that is no SHA-256
# and answers 0; over any other file it is the machine's own.
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

# scripts <case>: a scripts directory for the case — the two scripts copied from this tree, byte for byte — printing
# its path. The case writes the pin, and the check's key file, beside them.
scripts() {
	local dir="$TMPROOT/$1/scripts"
	mkdir -p "$dir"
	cp "$SCRIPT_DIR/node.sh" "$SCRIPT_DIR/node-pin-check.sh" "$dir/"
	echo "$dir"
}

zero=0000000000000000000000000000000000000000000000000000000000000000

# pin <dir> <version> <linux-x64 sha> [extra line]...: a pin file naming the floor's four platforms, every digest but
# linux-x64's the zero digest, and each extra line after them.
pin() {
	local dir="$1" version="$2" sha="$3"
	shift 3
	{
		echo "# a pin file a case wrote"
		echo "NODE_FLOOR_VERSION=$version"
		echo "NODE_FLOOR_SHA256_LINUX_X64=$sha"
		echo "NODE_FLOOR_SHA256_LINUX_ARM64=$zero"
		echo "NODE_FLOOR_SHA256_DARWIN_X64=$zero"
		echo "NODE_FLOOR_SHA256_DARWIN_ARM64=$zero"
		local line
		for line in "$@"; do
			echo "$line"
		done
	} >"$dir/node-pin.env"
}

# repin <dir> <name> [value]: the case's pin file with NAME's line set to value, or dropped without one.
repin() {
	local file="$1/node-pin.env"
	grep -v "^$2=" "$file" >"$file.new" || true
	[ $# -lt 3 ] || echo "$2=$3" >>"$file.new"
	mv "$file.new" "$file"
}

# standin <dist> <version> <platform> [answer] [licence]: writes <dist>/v<version>/node-v<version>-<platform>.tar.xz,
# laid out as Node's own: a stand-in bin/node answering --version as Node does — v<version>, or the answer given — and
# refusing to start under a NODE_OPTIONS, as Node refuses one naming an option it does not know; a LICENSE, of the
# text given, so two tarballs of one release can differ; and what the recipe leaves behind, npm's tree and a link to
# it among them. Prints the tarball's SHA-256.
standin() {
	local dist="$1" version="$2" platform="$3" answer="${4-v$2}" licence="${5-The licence of Node $2}"
	local src="$TMPROOT/src-$RANDOM$RANDOM" top="node-v$2-$3"
	mkdir -p "$src/$top/bin" "$src/$top/lib/node_modules/npm/bin" "$src/$top/include/node" "$dist/v$version"
	printf '#!/bin/sh\n[ -z "${NODE_OPTIONS-}" ] || { echo "node: %s is not allowed in NODE_OPTIONS" >&2; exit 9; }\n[ "$1" = --version ] && echo "%s" && exit 0\nexit 3\n' \
		'$NODE_OPTIONS' "$answer" >"$src/$top/bin/node"
	chmod 755 "$src/$top/bin/node"
	echo "$licence" >"$src/$top/LICENSE"
	echo "npm" >"$src/$top/lib/node_modules/npm/bin/npm-cli.js"
	echo "header" >"$src/$top/include/node/node.h"
	ln -s ../lib/node_modules/npm/bin/npm-cli.js "$src/$top/bin/npm"
	tar -cf - -C "$src" "$top" | xz -c >"$dist/v$version/$top.tar.xz"
	rm -rf "$src"
	digest "$dist/v$version/$top.tar.xz"
}

# named <version> <platform> <sha>: the name the recipe publishes a build of that tarball under, by its layout.
layout="$(sed -n 's/^layout=\([0-9][0-9]*\)$/\1/p' "$SCRIPT_DIR/node.sh")"
if [ -z "$layout" ]; then
	echo "node-pin_test.sh: scripts/node.sh sets no layout=<number> line" >&2
	exit 1
fi
named() {
	echo "$1-$2-$(printf '%s' "$3" | cut -c1-12)-l$layout"
}

# ============================================================================================================
# scripts/node.sh
# ============================================================================================================

# --- what the recipe refuses before it downloads anything --------------------------------------------------------

# Each refusal is exit 2, said on stderr, with nothing downloaded and nothing made where it ran: the downloads it would
# read are a real stand-in tree, so a recipe that went on would download (and the log would say so), and it runs from
# an empty directory of the case's own, where a recipe that went on with a relative cache would make it.
refused() { # <case> <name> <stderr-has> <cmd>...: runs the command expecting a refusal before any download
	local case_name="$1" name="$2" has="$3" cwd="$TMPROOT/$1/cwd"
	shift 3
	: >"$TMPROOT/curl.log"
	mkdir -p "$cwd"
	run within "$cwd" env CREWLET_NODE_CACHE="$TMPROOT/$case_name/cache" NODE_PIN_DIST="file://$TMPROOT/refusals" "$@"
	expect "$name" 2 "" "$has"
	if [ -s "$TMPROOT/curl.log" ] || [ -n "$(ls -A "$cwd")" ]; then
		bad "$name, downloading and making nothing" "downloads: $(cat "$TMPROOT/curl.log")" \
			"made where it ran: $(ls -A "$cwd")"
	else
		ok "$name, downloading and making nothing"
	fi
}
sha="$(standin "$TMPROOT/refusals" 22.23.5 linux-x64)"

s="$(scripts unknown)"
pin "$s" 22.23.5 "$sha"
refused unknown "node.sh refuses a pin it does not know, with its usage" "usage: scripts/node.sh floor" \
	"$s/node.sh" newest

# The pin is read beside the recipe and nowhere else. Mutation: drop the recipe's check for its pin -> it still answers
# 2, its grep and sed failing over a file that is not there, but names no pin (red).
s="$(scripts nopin)"
refused nopin "node.sh refuses to run without its pin beside it, naming the file" "$s/node-pin.env is missing" \
	"$s/node.sh" floor

s="$(scripts twice)"
pin "$s" 22.23.5 "$sha" "NODE_FLOOR_VERSION=22.23.6"
refused twice "node.sh refuses a pin file setting a name twice, naming it" "sets NODE_FLOOR_VERSION 2 times" \
	"$s/node.sh" floor

# A version that is no release never reaches a URL or a file name. Mutation: drop the grammar check -> the recipe asks
# the dist tree for what the pin says (red).
for version in "22.23" "v22.23.5" "22.23.5/x" "22.23.5-rc.1"; do
	s="$(scripts version)"
	pin "$s" "$version" "$sha"
	refused version "node.sh refuses a version that is no release: $version" \
		"NODE_FLOOR_VERSION in $s/node-pin.env is \"$version\"" "$s/node.sh" floor
	rm -rf "$TMPROOT/version"
done

s="$(scripts short-sha)"
pin "$s" 22.23.5 "${sha:1}"
refused short-sha "node.sh refuses a SHA-256 of 63 digits, naming the variable" "NODE_FLOOR_SHA256_LINUX_X64" \
	"$s/node.sh" floor

s="$(scripts riscv)"
pin "$s" 22.23.5 "$sha"
refused riscv "node.sh refuses a platform no pin names a build for, naming it" "pinned for no Linux riscv64" \
	env UNAME_M=riscv64 "$s/node.sh" floor

# A platform the pin file names no digest for is refused by its variable, never fetched unchecked. Mutation: read the
# linux-x64 digest for every platform -> the arm64 case fetches (red).
s="$(scripts unpinned)"
pin "$s" 22.23.5 "$sha"
repin "$s" NODE_FLOOR_SHA256_LINUX_ARM64
refused unpinned "node.sh refuses a platform its pin names no digest for, naming the variable" \
	"sets NODE_FLOOR_SHA256_LINUX_ARM64 0 times" env UNAME_M=aarch64 "$s/node.sh" floor

# A build cache that is not an absolute path is refused before anything is looked for in it, whichever variable gave
# it. Mutation: drop the refusal -> the recipe fetches into a cache in the directory it was run from (red).
s="$(scripts relative)"
pin "$s" 22.23.5 "$sha"
refused relative "node.sh refuses a build cache that is not an absolute path, naming the variables" \
	"the build cache relative/cache is not an absolute path (CREWLET_NODE_CACHE, XDG_CACHE_HOME, HOME)" \
	env CREWLET_NODE_CACHE=relative/cache "$s/node.sh" floor
refused relative-xdg "node.sh refuses a default build cache that is not an absolute path, naming the variables" \
	"the build cache relative/crewlet/node is not an absolute path (CREWLET_NODE_CACHE, XDG_CACHE_HOME, HOME)" \
	env -u CREWLET_NODE_CACHE XDG_CACHE_HOME=relative "$s/node.sh" floor

# What a fetch runs, each asked for before the download: a PATH holding everything the recipe runs up to its download
# but the tool. Mutation: drop a tool from the recipe's check -> it downloads, and fails later (1, red).
for tool in curl xz tar; do
	s="$(scripts "no$tool")"
	pin "$s" 22.23.5 "$sha"
	mkdir -p "$TMPROOT/no$tool/bin"
	for have in dirname grep sed tr cut mkdir mktemp chmod rm ln find mv sha256sum shasum xz tar; do
		[ "$have" != "$tool" ] || continue
		if found="$(command -v "$have")"; then ln -s "$found" "$TMPROOT/no$tool/bin/$have"; fi
	done
	[ "$tool" = curl ] || ln -s "$TMPROOT/tools/curl" "$TMPROOT/no$tool/bin/curl"
	ln -s "$TMPROOT/tools/uname" "$TMPROOT/no$tool/bin/uname"
	refused "no$tool" "node.sh refuses to fetch without $tool, naming it" "needs: $tool" \
		env PATH="$TMPROOT/no$tool/bin" "$s/node.sh" floor
done
s="$(scripts nosha)"
pin "$s" 22.23.5 "$sha"
mkdir -p "$TMPROOT/nosha/bin"
for have in dirname grep sed tr cut mkdir mktemp chmod rm ln find mv xz tar; do
	if found="$(command -v "$have")"; then ln -s "$found" "$TMPROOT/nosha/bin/$have"; fi
done
ln -s "$TMPROOT/tools/curl" "$TMPROOT/nosha/bin/curl"
ln -s "$TMPROOT/tools/uname" "$TMPROOT/nosha/bin/uname"
refused nosha "node.sh refuses to fetch without a SHA-256 tool, naming both" "needs sha256sum, or shasum" \
	env PATH="$TMPROOT/nosha/bin" "$s/node.sh" floor

# --- a fetched build is checked, kept to node and its licence, and published whole ------------------------------

# A fresh fetch: the tarball downloaded from the pin's own path, checked, its bin/node and LICENSE kept and nothing
# else — npm's tree, the headers and the download gone — .complete written, and the published name's node printed and
# nothing else on stdout: a path, which the make targets that run the whole suite export as its floor. Then a second
# run hands back that build and downloads nothing. Mutation: leave the download beside the build -> red; keep the
# whole tarball's tree -> red.
s="$(scripts fresh)"
sha="$(standin "$TMPROOT/fresh/dist" 22.23.5 linux-x64)"
pin "$s" 22.23.5 "$sha"
: >"$TMPROOT/curl.log"
run env CREWLET_NODE_CACHE="$TMPROOT/fresh/cache" NODE_PIN_DIST="file://$TMPROOT/fresh/dist" "$s/node.sh" floor
link="$TMPROOT/fresh/cache/$(named 22.23.5 linux-x64 "$sha")"
expect "node.sh fetches, checks and publishes a build, printing its node" 0 "$link/bin/node"
why=()
[ "$out" = "$link/bin/node" ] || why+=("stdout is not the node's path alone: $out")
[ -L "$link" ] || why+=("$link is not the published name, a link")
kept="$( (cd "$link/" && find . -mindepth 1 | LC_ALL=C sort | tr '\n' ' ') 2>/dev/null || true)"
[ "$kept" = "./.complete ./LICENSE ./bin ./bin/node " ] || why+=("the build holds $kept, not bin/node, LICENSE and .complete alone")
grep -qF "$TMPROOT/fresh/dist/v22.23.5/node-v22.23.5-linux-x64.tar.xz" "$TMPROOT/curl.log" ||
	why+=("it did not download the pin's own tarball: $(cat "$TMPROOT/curl.log")")
if [ ${#why[@]} -eq 0 ]; then
	ok "a fetched build is published whole, holding node and its licence alone"
else
	bad "a fetched build is published whole, holding node and its licence alone" "${why[@]}"
fi
# Mutation: drop the stall bound from the recipe's download -> red.
bounded "every download the recipe makes ends when it stalls"
: >"$TMPROOT/curl.log"
run env CREWLET_NODE_CACHE="$TMPROOT/fresh/cache" NODE_PIN_DIST="file://$TMPROOT/fresh/nowhere" "$s/node.sh" floor
expect "node.sh hands back an intact build" 0 "$link/bin/node"
undownloaded "node.sh hands back an intact build and downloads nothing"

# The recipe asks a build its version without NODE_OPTIONS: Node refuses to start under one naming an option it does
# not know, as a developer's set for a newer Node may, and the stand-in refuses under any. Mutation: drop the unset ->
# the intact build is not recognised, and fetching again fails the same check (1, red).
run env NODE_OPTIONS="--an-option-node-22-predates" CREWLET_NODE_CACHE="$TMPROOT/fresh/cache" \
	NODE_PIN_DIST="file://$TMPROOT/fresh/dist" "$s/node.sh" floor
expect "node.sh asks a build its version whatever NODE_OPTIONS holds" 0 "$link/bin/node"

# The recipe finds its own directory whatever CDPATH holds: scripts/git.sh's reason. Mutation: drop the recipe's
# `unset CDPATH` -> it finds no pin beside itself in the decoy's empty scripts/ and refuses (2, red).
mkdir -p "$TMPROOT/fresh/decoy/scripts"
run within "$TMPROOT/fresh" env CDPATH="$TMPROOT/fresh/decoy" CREWLET_NODE_CACHE="$TMPROOT/fresh/cache" \
	NODE_PIN_DIST="file://$TMPROOT/fresh/nowhere" scripts/node.sh floor
expect "node.sh run by a relative path reads its own directory whatever CDPATH holds" 0 "$link/bin/node"

# An intact build needs none of what a fetch runs: a PATH holding what the recipe reads its pin and platform with, and
# neither curl, xz, tar nor a SHA-256 tool, hands back the build fetched above. Mutation: check for what a fetch needs
# before looking for an intact build -> it refuses (2, red).
mkdir -p "$TMPROOT/fresh/bare"
for tool in dirname grep sed tr cut; do
	ln -s "$(command -v "$tool")" "$TMPROOT/fresh/bare/$tool"
done
ln -s "$TMPROOT/tools/uname" "$TMPROOT/fresh/bare/uname"
run env PATH="$TMPROOT/fresh/bare" CREWLET_NODE_CACHE="$TMPROOT/fresh/cache" \
	NODE_PIN_DIST="file://$TMPROOT/fresh/nowhere" "$s/node.sh" floor
expect "node.sh hands back an intact build where curl, xz, tar and a SHA-256 tool are missing" 0 "$link/bin/node"

# A BUILD IS NAMED BY WHAT DECIDES ITS BYTES, its pin's digest and the recipe's layout, never the recipe's own text.
# Mutation: name a build by the recipe's own SHA-256 -> the edited recipe fetches again (red); leave the digest out of
# a build's name -> the earlier build is handed back (red); leave the layout out -> red.
s="$(scripts names)"
sha="$(standin "$TMPROOT/names/dist" 22.23.5 linux-x64)"
pin "$s" 22.23.5 "$sha"
names() { # <name>: one `node.sh floor` of the case's scripts over its cache and dist tree, its log emptied first
	: >"$TMPROOT/curl.log"
	run env CREWLET_NODE_CACHE="$TMPROOT/names/cache" NODE_PIN_DIST="file://$TMPROOT/names/dist" "$s/node.sh" floor
	expect "$1" 0 "$TMPROOT/names/cache/22.23.5-linux-x64-"
}
names "node.sh fetches a build to name"
first="$out"
echo "# a line of comment, which changes no build" >>"$s/node.sh"
names "node.sh, edited but for its layout, answers"
if [ "$out" = "$first" ]; then
	undownloaded "an edit to the recipe that is not its layout reuses the build, downloading nothing"
else
	bad "an edit to the recipe that is not its layout reuses the build, downloading nothing" "printed $out, not $first"
fi
sha="$(standin "$TMPROOT/names/dist" 22.23.5 linux-x64 v22.23.5 "another licence, so another tarball of 22.23.5")"
pin "$s" 22.23.5 "$sha"
names "node.sh, its pin's digest moved, answers"
if [ "$out" = "$TMPROOT/names/cache/$(named 22.23.5 linux-x64 "$sha")/bin/node" ] && [ "$out" != "$first" ] &&
	[ -s "$TMPROOT/curl.log" ]; then
	ok "a pin whose digest moved fetches a build of its own"
else
	bad "a pin whose digest moved fetches a build of its own" "printed $out after $first" \
		"downloads: $(cat "$TMPROOT/curl.log")"
fi
second="$out"
sed "s/^layout=$layout\$/layout=$((layout + 1))/" "$s/node.sh" >"$s/node.sh.new"
mv "$s/node.sh.new" "$s/node.sh"
chmod +x "$s/node.sh"
names "node.sh of the next layout answers"
if [ "$out" != "$second" ] && [ -s "$TMPROOT/curl.log" ]; then
	ok "a recipe of another layout fetches a build of its own"
else
	bad "a recipe of another layout fetches a build of its own" "printed $out, as the earlier layout did" \
		"downloads: $(cat "$TMPROOT/curl.log")"
fi

# A Mac fetches its own platform's tarball by its own digest. Mutation: read linux-x64 for every machine -> red.
s="$(scripts mac)"
sha="$(standin "$TMPROOT/mac/dist" 22.23.5 darwin-arm64)"
pin "$s" 22.23.5 "$zero"
repin "$s" NODE_FLOOR_SHA256_DARWIN_ARM64 "$sha"
run env UNAME_S=Darwin UNAME_M=arm64 CREWLET_NODE_CACHE="$TMPROOT/mac/cache" NODE_PIN_DIST="file://$TMPROOT/mac/dist" \
	"$s/node.sh" floor
expect "node.sh on a Mac fetches the darwin-arm64 build" 0 "$TMPROOT/mac/cache/$(named 22.23.5 darwin-arm64 "$sha")/bin/node"

# WHAT THE RECIPE READS WHEN NOTHING NAMES IT: nodejs.org's own dist tree and ~/.cache/crewlet/node — the suite's HOME —
# whenever NODE_PIN_DIST, CREWLET_NODE_CACHE and XDG_CACHE_HOME are unset, as the suite leaves them. Mutation: misspell
# the default dist tree -> red; drop the suite's NODE_PIN_ scrub, or its CREWLET_NODE_CACHE or XDG_CACHE_HOME unset ->
# the nested run's copy of this case fetches from the developer's tree or into the developer's cache (red).
s="$(scripts defaults)"
sha="$(standin "$TMPROOT/defaults/web/nodejs.org/dist" 22.23.5 linux-x64)"
pin "$s" 22.23.5 "$sha"
run env PATH="$(offline "$TMPROOT/defaults/web"):$PATH" "$s/node.sh" floor
expect "node.sh reads nodejs.org's own dist tree and ~/.cache/crewlet/node when nothing names others" 0 \
	"$HOME/.cache/crewlet/node/$(named 22.23.5 linux-x64 "$sha")/bin/node"

# A tarball that is not the pin's is refused before it is unpacked, naming both digests, and nothing is published.
# Mutation: accept the tarball without comparing its digest -> exit 0 (red).
s="$(scripts tampered)"
served="$(standin "$TMPROOT/tampered/dist" 22.23.5 linux-x64)"
pin "$s" 22.23.5 "$zero"
run env CREWLET_NODE_CACHE="$TMPROOT/tampered/cache" NODE_PIN_DIST="file://$TMPROOT/tampered/dist" "$s/node.sh" floor
expect "node.sh refuses a tarball that is not the pin's (1), naming both digests" 1 "" "hashes to $served, and"
if [ -n "$(find "$TMPROOT/tampered/cache" -mindepth 1 -maxdepth 1 2>/dev/null)" ]; then
	bad "a tarball that is not the pin's leaves nothing in the cache" "$(ls -la "$TMPROOT/tampered/cache")"
else
	ok "a tarball that is not the pin's leaves nothing in the cache"
fi

# A build that answers another version is refused, and nothing is published. Mutation: publish without asking the
# build its version -> exit 0 (red).
s="$(scripts disagrees)"
sha="$(standin "$TMPROOT/disagrees/dist" 22.23.5 linux-x64 "v22.23.4")"
pin "$s" 22.23.5 "$sha"
run env CREWLET_NODE_CACHE="$TMPROOT/disagrees/cache" NODE_PIN_DIST="file://$TMPROOT/disagrees/dist" "$s/node.sh" floor
expect "node.sh refuses a build whose --version disagrees (1)" 1 "" "it said \"v22.23.4\""
if [ -e "$TMPROOT/disagrees/cache/$(named 22.23.5 linux-x64 "$sha")" ]; then
	bad "a build that disagrees is never published" "$(ls -la "$TMPROOT/disagrees/cache")"
else
	ok "a build that disagrees is never published"
fi

# A tarball whose SHA-256 the machine's hash cannot compute is a tarball nothing checked: refused (1), naming it, and
# nothing is published. Mutation: let digest answer whatever its hash printed -> "hashes to , and" (red).
s="$(scripts hashfails)"
sha="$(standin "$TMPROOT/hashfails/dist" 22.23.5 linux-x64)"
pin "$s" 22.23.5 "$sha"
hashtool "$TMPROOT/hashfails/tools" fails '*.tar.xz'
run env PATH="$TMPROOT/hashfails/tools:$PATH" CREWLET_NODE_CACHE="$TMPROOT/hashfails/cache" \
	NODE_PIN_DIST="file://$TMPROOT/hashfails/dist" "$s/node.sh" floor
expect "node.sh refuses a tarball whose SHA-256 the machine cannot compute (1)" 1 "" \
	"cannot compute the SHA-256 of node-v22.23.5-linux-x64.tar.xz"

# A NAME POINTING AT NO INTACT BUILD is fetched over, whichever way it points there: a link left on a build without
# .complete, whose node answers the pin's version all the same, and a directory at the name itself holding the same.
# Mutations: skip the publish loop's removal of the name -> refused three times (1, red); drop its case of a directory
# at the name -> the link is made INSIDE the directory, which the name stays, and the recipe answers 0 (red).
for shape in link directory; do
	s="$(scripts "incomplete-$shape")"
	sha="$(standin "$TMPROOT/incomplete-$shape/dist" 22.23.5 linux-x64)"
	pin "$s" 22.23.5 "$sha"
	link="$TMPROOT/incomplete-$shape/cache/$(named 22.23.5 linux-x64 "$sha")"
	broken="$link.stale1"
	[ "$shape" = link ] || broken="$link"
	mkdir -p "$broken/bin"
	printf '#!/bin/sh\necho v22.23.5\n' >"$broken/bin/node"
	chmod 755 "$broken/bin/node"
	[ "$shape" = directory ] || ln -s "$(basename "$broken")" "$link"
	: >"$TMPROOT/curl.log"
	run env CREWLET_NODE_CACHE="$TMPROOT/incomplete-$shape/cache" NODE_PIN_DIST="file://$TMPROOT/incomplete-$shape/dist" \
		"$s/node.sh" floor
	expect "node.sh fetches again over a $shape at the name holding no intact build" 0 "$link/bin/node"
	why=()
	[ -L "$link" ] || why+=("the name is not a link")
	[ -f "$link/.complete" ] || why+=("the name points at no build with .complete")
	[ -s "$TMPROOT/curl.log" ] || why+=("it downloaded nothing")
	if [ ${#why[@]} -eq 0 ]; then
		ok "a $shape at the name holding no intact build is replaced by a link to the build fetched in its place"
	else
		bad "a $shape at the name holding no intact build is replaced by a link to the build fetched in its place" \
			"${why[@]}" "$err"
	fi
done

# --- a curl configuration changes nothing the recipe prints ------------------------------------------------------

# The recipe's stdout is the node's path and nothing else, because a make target exports it as the suite's floor.
# Mutation: let the recipe's download write curl's stdout to its own -> red.
s="$(scripts curlrc)"
sha="$(standin "$TMPROOT/curlrc/dist" 22.23.5 linux-x64)"
pin "$s" 22.23.5 "$sha"
run env CURL_HOME="$(curlhome "$TMPROOT/curlrc/curl")" CREWLET_NODE_CACHE="$TMPROOT/curlrc/cache" \
	NODE_PIN_DIST="file://$TMPROOT/curlrc/dist" "$s/node.sh" floor
why=()
[ "$status" = 0 ] || why+=("exit $status, want 0")
[ "$out" = "$TMPROOT/curlrc/cache/$(named 22.23.5 linux-x64 "$sha")/bin/node" ] || why+=("its stdout is not the node's path alone")
grep -qx 000 <<<"$err" || why+=("stderr lacks the curlrc's write-out, 000: the configuration was not read")
if [ ${#why[@]} -eq 0 ]; then
	ok "a curlrc that writes to curl's stdout changes nothing the recipe prints"
else
	bad "a curlrc that writes to curl's stdout changes nothing the recipe prints" "${why[@]}" "stdout:" "$out" \
		"stderr:" "$err"
fi

# --- two fetchers at once publish one build, and neither deletes the other's Node ---------------------------------

# scripts/chromium-pin_test.sh's case and its reason, over the floor's tarball: B starts a fetch whose download is
# held (the tarball it reads is a pipe the case writes when it chooses); A fetches and publishes meanwhile; B's
# download arrives and B is held again before its unpack — and the node A printed must still answer, because B unpacks
# in a directory of its own. Released, B keeps A's build and removes its own. Mutation: unpack in place at the
# published name, emptied before the unpack -> B's emptying deletes the node A printed (red).
s="$(scripts concurrent)"
ctl="$TMPROOT/concurrent/ctl"
mkdir -p "$ctl" "$TMPROOT/concurrent/held/v22.23.5" "$TMPROOT/concurrent/holdbin"
sha="$(standin "$TMPROOT/concurrent/dist" 22.23.5 linux-x64)"
pin "$s" 22.23.5 "$sha"
mkfifo "$TMPROOT/concurrent/held/v22.23.5/node-v22.23.5-linux-x64.tar.xz"
cat >"$TMPROOT/concurrent/holdbin/xz" <<EOF
#!/bin/sh
touch '$ctl/arrived'
while [ ! -e '$ctl/go' ]; do sleep 0.05; done
exec '$(command -v xz)' "\$@"
EOF
chmod +x "$TMPROOT/concurrent/holdbin/xz"
fetcher() { # <name> <dist> [PATH]: one `node.sh floor` in the background, its outcome in <ctl>/<name>.*
	(
		PATH="${3:-$PATH}" CREWLET_NODE_CACHE="$TMPROOT/concurrent/cache" NODE_PIN_DIST="file://$2" \
			"$s/node.sh" floor >"$ctl/$1.out" 2>"$ctl/$1.err" && echo 0 >"$ctl/$1.status" || echo $? >"$ctl/$1.status"
	) &
}
await() { # <seconds> <test>...: waits for the test to hold, failing after the seconds
	local deadline=$((SECONDS + $1))
	shift
	until "$@"; do
		[ "$SECONDS" -lt "$deadline" ] || return 1
		sleep 0.05
	done
}
answers() { [ "$( (unset NODE_OPTIONS && "$1" --version) 2>&1 || true)" = "v22.23.5" ]; } # as the recipe asks
started() { grep -q 'fetching Node 22.23.5' "$ctl/b.err" 2>/dev/null; }
concurrent() {
	fetcher b "$TMPROOT/concurrent/held" "$TMPROOT/concurrent/holdbin:$PATH"
	await 60 started || { bad "the held fetcher starts its fetch" "$(cat "$ctl/b.err")"; return; }
	fetcher a "$TMPROOT/concurrent/dist"
	await 60 test -e "$ctl/a.status" || { bad "the other fetcher finishes" "$(cat "$ctl/a.err")"; return; }
	local anode
	anode="$(cat "$ctl/a.out")"
	if [ "$(cat "$ctl/a.status")" != 0 ] || ! answers "$anode"; then
		bad "the other fetcher publishes a node that answers" "$(cat "$ctl/a.err")"
		return
	fi
	cat "$TMPROOT/concurrent/dist/v22.23.5/node-v22.23.5-linux-x64.tar.xz" \
		>"$TMPROOT/concurrent/held/v22.23.5/node-v22.23.5-linux-x64.tar.xz" &
	await 60 test -e "$ctl/arrived" || { bad "the held fetcher reaches its unpack" "$(cat "$ctl/b.err")"; return; }
	if [ ! -e "$ctl/b.status" ] && answers "$anode"; then
		ok "the node one fetcher published answers while another is still fetching"
	else
		bad "the node one fetcher published answers while another is still fetching" \
			"$anode answers: $( (unset NODE_OPTIONS && "$anode" --version) 2>&1 || true)"
	fi
	touch "$ctl/go"
	await 60 test -e "$ctl/b.status" || { bad "the held fetcher finishes" "$(cat "$ctl/b.err")"; return; }
	local builds
	builds="$(find "$TMPROOT/concurrent/cache" -mindepth 1 -maxdepth 1 -type d -name "$(named 22.23.5 linux-x64 "$sha").*" | wc -l | tr -d ' ')"
	if [ "$(cat "$ctl/b.status")" = 0 ] && [ "$(cat "$ctl/b.out")" = "$anode" ] && answers "$anode" && [ "$builds" = 1 ]; then
		ok "two fetchers at once both succeed, print one path and leave one build"
	else
		bad "two fetchers at once both succeed, print one path and leave one build" \
			"exit $(cat "$ctl/b.status"); printed $(cat "$ctl/b.out") and $anode; build directories: $builds" \
			"$(cat "$ctl/b.err")"
	fi
}
concurrent

# --- a crashed fetcher's leftovers are swept, a running one's never ------------------------------------------------

# Mutation: sweep every leftover whatever its age -> the fresh one goes (red); sweep none -> the old one stays (red).
s="$(scripts strays)"
sha="$(standin "$TMPROOT/strays/dist" 22.23.5 linux-x64)"
pin "$s" 22.23.5 "$sha"
strays="$TMPROOT/strays/cache/$(named 22.23.5 linux-x64 "$sha")"
mkdir -p "$strays.crashd/.download" "$strays.workng/.download"
twodays="$(($(date +%s) - 2 * 86400))"
stamp="$(date -d "@$twodays" +%Y%m%d%H%M 2>/dev/null || date -r "$twodays" +%Y%m%d%H%M)" # GNU, then BSD date
touch -t "$stamp" "$strays.crashd"
run env CREWLET_NODE_CACHE="$TMPROOT/strays/cache" NODE_PIN_DIST="file://$TMPROOT/strays/dist" "$s/node.sh" floor
why=()
[ "$status" = 0 ] || why+=("exit $status, want 0" "$err")
[ ! -e "$strays.crashd" ] || why+=("the crashed fetcher's directory stayed")
[ -d "$strays.workng" ] || why+=("the working fetcher's directory went")
if [ ${#why[@]} -eq 0 ]; then
	ok "a fetch sweeps a crashed fetcher's leftovers a day old and keeps a fresh one"
else
	bad "a fetch sweeps a crashed fetcher's leftovers a day old and keeps a fresh one" "${why[@]}"
fi

# ============================================================================================================
# scripts/node-pin-check.sh
# ============================================================================================================

# THE KEYS, made once for the suite in a GnuPG home of its own: each a primary that only certifies and a subkey that
# signs, as most releasers' keys are shaped.
signers="$TMPROOT/signers"
mkdir -m 700 "$signers"
GPGHOMES+=("$signers")
sgpg() { GNUPGHOME="$signers" gpg --batch --no-tty --pinentry-mode loopback --passphrase '' "$@"; }

# newkey <name> <subkey expiry> [primary expiry] [faked time]: makes a key, printing its primary fingerprint.
newkey() {
	local name="$1" sub="$2" primary="${3:-never}" faked=()
	[ -z "${4-}" ] || faked=(--faked-system-time "$4!")
	sgpg ${faked[@]+"${faked[@]}"} --quick-gen-key "$name <$name@example.com>" ed25519 cert "$primary" 2>/dev/null
	local fpr
	fpr="$(sgpg --with-colons --list-keys "$name@example.com" 2>/dev/null | awk -F: '$1 == "fpr" { print $10; exit }')"
	sgpg ${faked[@]+"${faked[@]}"} --quick-add-key "$fpr" ed25519 sign "$sub" 2>/dev/null
	echo "$fpr"
}
export_key() { sgpg --armor --export "$@" 2>/dev/null; }

releaser="$(newkey releaser never)"
second="$(newkey second never)"
other_key="$(newkey other never)"
expired_key="$(newkey expired 1d never 20200101T000000)"
# signing <fpr>: the fingerprint of the key's signing subkey, which is what a signature by the key names.
signing() {
	sgpg --with-colons --list-keys "$1" 2>/dev/null | awk -F: '$1 == "sub" { sub_ = 1; next } sub_ && $1 == "fpr" { print $10; exit }'
}

# day <offset days>: the date that many days from now, UTC.
day() {
	local at=$(($(date +%s) + $1 * 86400))
	date -u -d "@$at" +%Y-%m-%d 2>/dev/null || date -u -r "$at" +%Y-%m-%d
}

platforms="linux-x64 linux-arm64 darwin-x64 darwin-arm64"

# release <dist> <version> <signer> [faked time] [platform...]: a release in a stand-in dist tree — one small tarball a
# platform, every one of the four without a list, whose bytes name the release and the platform — and its
# SHASUMS256.txt, listing each beside files of the other platforms Node publishes, signed by the signer as Node's
# releasers sign it (SHASUMS256.txt.sig, detached).
release() {
	local dist="$1" v="$2" signer="$3" faked=() platform
	[ -z "${4-}" ] || faked=(--faked-system-time "$4!")
	shift 4 2>/dev/null || shift $#
	[ $# -gt 0 ] || set -- $platforms
	mkdir -p "$dist/v$v"
	: >"$dist/v$v/SHASUMS256.txt"
	for platform in "$@"; do
		echo "Node $v for $platform" | xz -c >"$dist/v$v/node-v$v-$platform.tar.xz"
		echo "$(digest "$dist/v$v/node-v$v-$platform.tar.xz")  node-v$v-$platform.tar.xz" >>"$dist/v$v/SHASUMS256.txt"
	done
	echo "$zero  node-v$v-win-x64.zip" >>"$dist/v$v/SHASUMS256.txt"
	echo "$zero  node-v$v.tar.gz" >>"$dist/v$v/SHASUMS256.txt"
	rm -f "$dist/v$v/SHASUMS256.txt.sig"
	sgpg ${faked[@]+"${faked[@]}"} --local-user "$signer" --detach-sign -o "$dist/v$v/SHASUMS256.txt.sig" \
		"$dist/v$v/SHASUMS256.txt" 2>/dev/null
}

# index <dist> <version>...: nodejs.org's index.tab naming each release, newest first as Node writes it, beside what
# the check must not take for a release: a prerelease, and a line of another shape.
index() {
	local dist="$1" v
	shift
	mkdir -p "$dist"
	{
		printf 'version\tdate\tfiles\tnpm\tv8\tuv\tzlib\topenssl\tmodules\tlts\tsecurity\n'
		printf 'v27.0.0-alpha.0.1.1\t2026-11-16\tlinux-x64\t12.2.2\t15.6.75.50\t1.52.1\t1.3.2\t4.1.0\t153\t-\t-\n'
		for v in "$@"; do
			printf 'v%s\t2026-11-11\tlinux-arm64,linux-x64,osx-arm64-tar,osx-x64-tar\t10.9.9\t12.4.254.21\t1.51.0\t1.3.1\t3.5.9\t127\tJod\ttrue\n' "$v"
		done
		printf 'v22.99.0-rc.1\t2026-11-12\tlinux-x64\t-\t-\t-\t-\t-\t-\t-\t-\n'
	} >"$dist/index.tab"
}

# schedule <file> <v22 end> [v24 end]: nodejs/Release's schedule.json, indented as it is written, with an entry for
# each line the cases name and one before them.
schedule() {
	mkdir -p "$(dirname "$1")"
	cat >"$1" <<EOF
{
  "v20": {
    "start": "2023-04-18",
    "lts": "2023-10-24",
    "maintenance": "2024-10-22",
    "end": "2026-04-30",
    "codename": "Iron"
  },
  "v22": {
    "start": "2024-04-24",
    "lts": "2024-10-29",
    "maintenance": "2025-10-21",
    "end": "$2",
    "codename": "Jod"
  },
  "v24": {
    "start": "2025-05-06",
    "lts": "2025-10-28",
    "maintenance": "2026-10-20",
    "end": "${3:-2028-04-30}",
    "codename": "Krypton"
  }
}
EOF
}

# checkcase <case> <key...>: a scripts directory pinned at 22.23.5 whose key file holds the named keys, beside an
# index naming that release the newest of 22 and a schedule ending Node 22 400 days from now; prints its path.
checkcase() {
	local s
	s="$(scripts "$1")"
	shift
	pin "$s" 22.23.5 "$zero"
	export_key "$@" >"$s/node-release-keys.asc"
	index "$TMPROOT/$(basename "$(dirname "$s")")/dist" 22.23.4 22.23.5 24.22.3
	schedule "$TMPROOT/$(basename "$(dirname "$s")")/schedule.json" "$(day 400)"
	echo "$s"
}

# pins <case dir> <scripts dir> [mode]: runs the check against the case's dist tree and schedule, holding the key file
# to the suite's two releasers unless $fingerprints names others.
pins() {
	: >"$TMPROOT/curl.log"
	run env NODE_PIN_DIST="file://$1/dist" NODE_PIN_SCHEDULE="file://$1/schedule.json" \
		NODE_PIN_FINGERPRINTS="${fingerprints:-$releaser $second}" "$2/node-pin-check.sh" ${3+"$3"}
}

c="$TMPROOT/current"
s="$(checkcase current "$releaser" "$second")"
pins "$c" "$s"
expect "the floor the newest of its major, its end of life far -> 0" 0 \
	"the floor is the newest release of Node 22: 22.23.5; Node 22's end of life is $(day 400),"

# A newer patch is the floor's bump: its version, the four digests of the tarballs the check downloaded, each the one
# the signed list names, the releaser who signed it, and the pull request's title. Mutation: hash one platform's
# tarball for all four -> red.
c="$TMPROOT/patch"
s="$(checkcase patch "$releaser" "$second")"
index "$c/dist" 22.23.5 22.23.6 24.22.3
release "$c/dist" 22.23.6 "$second"
pins "$c" "$s"
expect "a newer patch -> 1 with its version" 1 "NODE_FLOOR_VERSION=22.23.6"
for platform in $platforms; do
	expect "a newer patch -> 1 with the SHA-256 of its $platform tarball" 1 \
		"NODE_FLOOR_SHA256_$(printf '%s' "$platform" | tr 'a-z-' 'A-Z_')=$(digest "$c/dist/v22.23.6/node-v22.23.6-$platform.tar.xz")"
done
expect "a newer patch names the releaser who signed it and its pull request" 1 \
	"signed by $second, in a pull request titled \`build(deps): bump node's floor from 22.23.5 to 22.23.6\`"
# Mutation: drop the stall bound or --proto-redir from the check's fetch -> red.
bounded "every download the check makes ends when it stalls and follows a redirect to HTTPS alone" https
if grep -q 'v22.99.0\|v27.0.0' "$TMPROOT/curl.log"; then
	bad "the index's prereleases are never releases" "$(cat "$TMPROOT/curl.log")"
else
	ok "the index's prereleases are never releases"
fi

# THE FLOOR'S END OF LIFE: from 60 days before it, the check fails as for a pin behind, naming the next even major's
# newest release, verified and hashed as a patch is, and the MinNodeMajor change it takes; past it, the same. Mutation:
# compare the end of life with no warning window -> 30 days ahead reads as far (0, red); propose the next major, odd
# -> no release of 23 (2, red).
for ahead in 30 -3; do
	c="$TMPROOT/eol$ahead"
	s="$(checkcase "eol$ahead" "$releaser" "$second")"
	schedule "$c/schedule.json" "$(day "$ahead")"
	index "$c/dist" 22.23.5 23.11.1 24.22.3
	release "$c/dist" 24.22.3 "$releaser"
	pins "$c" "$s"
	expect "Node 22's end of life $ahead days away -> 1 with Node 24's newest release" 1 "NODE_FLOOR_VERSION=24.22.3"
	expect "Node 22's end of life $ahead days away -> 1 naming the raise and its pull request" 1 \
		"\`build(deps): raise node's floor from 22 to 24\`"
	expect "Node 22's end of life $ahead days away -> 1 naming boxhelper.MinNodeMajor" 1 \
		"it changes internal/browser/boxhelper.MinNodeMajor from 22 to 24"
done
expect "an end of life already past says so" 1 "Node 22 reached its end of life on $(day -3)"
c="$TMPROOT/eolfar"
s="$(checkcase eolfar "$releaser" "$second")"
schedule "$c/schedule.json" "$(day 61)"
pins "$c" "$s"
expect "Node 22's end of life 61 days away -> 0" 0 "Node 22's end of life is $(day 61),"

# Both at once: one report, the patch's lines and the raise's.
c="$TMPROOT/both"
s="$(checkcase both "$releaser" "$second")"
schedule "$c/schedule.json" "$(day 30)"
index "$c/dist" 22.23.5 22.23.6 24.22.3
release "$c/dist" 22.23.6 "$releaser"
release "$c/dist" 24.22.3 "$second"
pins "$c" "$s"
expect "a patch behind with the end of life near -> 1, the patch's lines" 1 "NODE_FLOOR_VERSION=22.23.6"
expect "a patch behind with the end of life near -> 1, the raise's lines" 1 "NODE_FLOOR_VERSION=24.22.3"

# WHAT THE CHECK READS WHEN NOTHING NAMES IT: nodejs.org's own index and dist tree, and nodejs/Release's own schedule
# on GitHub, whenever NODE_PIN_DIST and NODE_PIN_SCHEDULE are unset, as the suite leaves them; and the releasers it
# pins, which the suite's key file does not hold, so with no fingerprints named it is refused naming the first of them.
# Mutation: misspell the default index, dist tree or schedule -> 2 (red); pin another default releaser set -> red.
c="$TMPROOT/check-defaults"
s="$(checkcase check-defaults "$releaser" "$second")"
web="$c/web"
index "$web/nodejs.org/dist" 22.23.5 22.23.6
release "$web/nodejs.org/dist" 22.23.6 "$releaser"
schedule "$web/raw.githubusercontent.com/nodejs/Release/main/schedule.json" "$(day 400)"
: >"$TMPROOT/curl.log"
run env PATH="$(offline "$web"):$PATH" NODE_PIN_FINGERPRINTS="$releaser $second" "$s/node-pin-check.sh"
expect "the check reads nodejs.org's own index and tree, and nodejs/Release's own schedule, when nothing names others" \
	1 "NODE_FLOOR_SHA256_DARWIN_ARM64=$(digest "$web/nodejs.org/dist/v22.23.6/node-v22.23.6-darwin-arm64.tar.xz")"
run env PATH="$(offline "$web"):$PATH" "$s/node-pin-check.sh"
expect "the check holds the key file to the releasers it pins when nothing names others" 2 "" \
	"it lacks 108F52B48DB57BB0CC439B2997B01419BD92F80A 5BE8A3F6C8A5C01D106C0AD820B1A390B168D356"

# A CURL CONFIGURATION CHANGES NOTHING THE CHECK PRINTS: scripts/git-pin_test.sh's case and its reason. Mutation: let
# the check's fetch write curl's stdout to its own -> red.
c="$TMPROOT/curlrc-check"
s="$(checkcase curlrc-check "$releaser" "$second")"
index "$c/dist" 22.23.5 22.23.6
release "$c/dist" 22.23.6 "$releaser"
pins "$c" "$s"
plain="$out"
CURL_HOME="$(curlhome "$c/curl")" pins "$c" "$s"
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

# A signature by a key the key file does not hold is refused naming it, with how to refresh the file and what to take
# from the release meanwhile: nothing. Mutation: accept gpg's verdict on any key it is handed -> not reachable, as the
# keyring holds the pinned keys alone; drop the NO_PUBKEY branch -> the refusal names no key (red).
c="$TMPROOT/otherkey"
s="$(checkcase otherkey "$releaser" "$second")"
index "$c/dist" 22.23.5 22.23.6
release "$c/dist" 22.23.6 "$other_key"
pins "$c" "$s"
expect "a release signed by a key the file does not hold -> 2, naming it" 2 "" \
	"was signed by $(signing "$other_key"), a key scripts/node-release-keys.asc does not hold"
expect "a release signed by a key the file does not hold -> 2, naming the refresh" 2 "" "To refresh them"
[ -z "$out" ] && ok "a release signed by another key reports nothing" || bad "a release signed by another key reports nothing" "$out"

# A release signed by a signing SUBKEY the key file does not hold yet — one its releaser added under the same primary
# after the file was refreshed — is the same refusal, naming the subkey.
rotated="$(newkey rotated never)"
c="$TMPROOT/rotated"
s="$(checkcase rotated "$rotated")"
sgpg --quick-add-key "$rotated" ed25519 sign never 2>/dev/null
newer_subkey="$(sgpg --with-colons --list-keys "$rotated" 2>/dev/null |
	awk -F: '$1 == "sub" { sub_ = 1; next } sub_ && $1 == "fpr" { last = $10; sub_ = 0 } END { print last }')"
index "$c/dist" 22.23.5 22.23.6
release "$c/dist" 22.23.6 "$newer_subkey!"
fingerprints="$rotated" pins "$c" "$s"
expect "a release signed by a subkey newer than the key file -> 2, naming the refresh" 2 "" \
	"a signing subkey of a pinned key newer than the file"

# THE KEY FILE IS THE PINNED KEYS, NO MORE AND NO FEWER, held before any signature is read. Mutation: skip the
# comparison -> the added key's signature verifies (exit 1, red), and the lacking file passes (0, red).
c="$TMPROOT/addedkey"
s="$(checkcase addedkey "$releaser" "$second" "$other_key")"
index "$c/dist" 22.23.5 22.23.6
release "$c/dist" 22.23.6 "$other_key"
pins "$c" "$s"
expect "a key file holding a key beside the pinned ones -> 2, naming it" 2 "" "it holds $other_key beside them"
c="$TMPROOT/lackingkey"
s="$(checkcase lackingkey "$releaser")"
pins "$c" "$s"
expect "a key file lacking a pinned key -> 2, naming it" 2 "" "it lacks $second"
c="$TMPROOT/nokeyfile"
s="$(checkcase nokeyfile "$releaser")"
echo "not a key" >"$s/node-release-keys.asc"
pins "$c" "$s"
expect "a key file that holds no key -> 2" 2 "" "cannot import"

# gpg answers 0 for a good signature by a key that has since expired (EXPKEYSIG), so the check reads the status rather
# than the exit code. Mutation: accept gpg's exit status alone -> the release reads as verified (exit 1, red).
c="$TMPROOT/expiredsig"
s="$(checkcase expiredsig "$expired_key")"
index "$c/dist" 22.23.5 22.23.6
release "$c/dist" 22.23.6 "$expired_key" 20200101T120000
fingerprints="$expired_key" pins "$c" "$s"
expect "a release signed by a key that has since expired -> 2, naming the refresh" 2 "" "To refresh them"

# A SHASUMS256.txt changed after it was signed does not verify. Mutation: skip the verification -> 1 (red).
c="$TMPROOT/tampered-sums"
s="$(checkcase tampered-sums "$releaser" "$second")"
index "$c/dist" 22.23.5 22.23.6
release "$c/dist" 22.23.6 "$releaser"
echo "$zero  node-v22.23.6-extra.tar.xz" >>"$c/dist/v22.23.6/SHASUMS256.txt"
pins "$c" "$s"
expect "a SHASUMS256.txt changed after it was signed -> 2" 2 "" "does not verify with the releasers' keys"

# A tarball the dist tree serves that is not the one the signed list names is not the release. Mutation: take the
# downloaded tarball's digest without comparing it -> 1 (red).
c="$TMPROOT/swapped"
s="$(checkcase swapped "$releaser" "$second")"
index "$c/dist" 22.23.5 22.23.6
release "$c/dist" 22.23.6 "$releaser"
echo "another tarball" | xz -c >"$c/dist/v22.23.6/node-v22.23.6-darwin-x64.tar.xz"
pins "$c" "$s"
expect "a tarball that is not the one the signed list names -> 2" 2 "" "it is not the tarball they released"
[ -z "$out" ] && ok "a swapped tarball reports nothing" || bad "a swapped tarball reports nothing" "$out"

# A release whose signed list names no tarball for one of the four platforms is never proposed. Mutation: skip a
# platform the list lacks -> 1 with three digests (red).
c="$TMPROOT/noarm"
s="$(checkcase noarm "$releaser" "$second")"
index "$c/dist" 22.23.5 22.23.6
release "$c/dist" 22.23.6 "$releaser" "" linux-x64 darwin-x64 darwin-arm64
pins "$c" "$s"
expect "a release whose signed list lacks a platform -> 2, naming it" 2 "" "lists no one node-v22.23.6-linux-arm64.tar.xz"

# AN INDEX'S LINE IS A RELEASE OR NOTHING: of a hostile index only lines of three numbers are read, so its words reach
# no URL, file name or report, and an index naming no release of the floor's major is refused. Mutation: read the first
# field unchecked -> the newest "release" is the injected one, which reaches a download (red).
c="$TMPROOT/hostile-index"
s="$(checkcase hostile-index "$releaser" "$second")"
printf 'v22.30.0;injected\t2026-11-12\n../v22.31.0\t2026-11-12\nv22.32\t-\n<html>injected</html>\n' >>"$c/dist/index.tab"
pins "$c" "$s"
why=()
[ "$status" = 0 ] || why+=("exit $status, want 0")
if grep -qF injected <<<"$err$out" || grep -qF 'v22.3' "$TMPROOT/curl.log"; then
	why+=("a line that is no release reached the output or a download: $(cat "$TMPROOT/curl.log")")
fi
if [ ${#why[@]} -eq 0 ]; then ok "an index's lines that are no release are never read"; else bad "an index's lines that are no release are never read" "${why[@]}" "$err"; fi
c="$TMPROOT/noseries"
s="$(checkcase noseries "$releaser" "$second")"
index "$c/dist" 20.19.5 24.22.3
pins "$c" "$s"
expect "an index naming no release of the floor's major -> 2" 2 "" "names no release of Node 22, the floor's major"
c="$TMPROOT/noindex"
s="$(checkcase noindex "$releaser" "$second")"
rm "$c/dist/index.tab"
pins "$c" "$s"
expect "an index that does not load -> 2" 2 "" "cannot read the release index"

# A pin ahead of the index means the index is behind upstream, never that the pin should go back.
c="$TMPROOT/ahead"
s="$(checkcase ahead "$releaser" "$second")"
index "$c/dist" 22.23.4 24.22.3
pins "$c" "$s"
expect "a pin newer than the index's newest of its major -> 2" 2 "" "that index is behind upstream"

# A SCHEDULE'S END OF LIFE IS A DATE OR NOTHING, and a schedule with no entry for the floor's major, or none that
# loads, tells nothing. Mutation: take the end unchecked -> it reaches the report or date (red).
for end in "2027-04-30T00:00:00Z" "soon" "2027-04-30 injected" ""; do
	c="$TMPROOT/baddate"
	s="$(checkcase baddate "$releaser" "$second")"
	schedule "$c/schedule.json" "$end"
	pins "$c" "$s"
	why=()
	[ "$status" = 2 ] || why+=("exit $status, want 2")
	grep -qF "is no date of the form YYYY-MM-DD; nothing of it is used" <<<"$err" || why+=("no refusal of the date")
	grep -qF injected <<<"$err$out" && why+=("the answer reached the output")
	if [ ${#why[@]} -eq 0 ]; then
		ok "an end of life of $(printf '%s' "$end" | wc -c | tr -d ' ') bytes that is no date -> 2, nothing of it used"
	else
		bad "an end of life that is no date -> 2, nothing of it used" "${why[@]}" "$err"
	fi
	rm -rf "$TMPROOT/baddate"
done
c="$TMPROOT/nomajor"
s="$(checkcase nomajor "$releaser" "$second")"
sed 's/"v22"/"v21"/' "$c/schedule.json" >"$c/schedule.json.new" && mv "$c/schedule.json.new" "$c/schedule.json"
pins "$c" "$s"
expect "a schedule with no entry for the floor's major -> 2" 2 "" "gives no one entry for Node 22"
c="$TMPROOT/noschedule"
s="$(checkcase noschedule "$releaser" "$second")"
rm "$c/schedule.json"
pins "$c" "$s"
expect "a schedule that does not load -> 2" 2 "" "cannot read the release schedule"
c="$TMPROOT/nosuccessor"
s="$(checkcase nosuccessor "$releaser" "$second")"
schedule "$c/schedule.json" "$(day 30)"
index "$c/dist" 22.23.5
pins "$c" "$s"
expect "an end of life near with no release of the next major -> 2" 2 "" "names no release of Node 24"

# A hash that gives no digest is "cannot tell", never a pin's new line. Mutation: write whatever the hash printed ->
# exit 1 with a digest that is no digest (red).
for mode in fails garbles; do
	c="$TMPROOT/hash$mode-check"
	s="$(checkcase "hash$mode-check" "$releaser" "$second")"
	index "$c/dist" 22.23.5 22.23.6
	release "$c/dist" 22.23.6 "$releaser"
	hashtool "$c/tools" "$mode" '*.tar.xz'
	PATH="$c/tools:$PATH" pins "$c" "$s"
	expect "a hash that $mode over a release's tarball -> 2" 2 "" "cannot compute the SHA-256 of node-v22.23.6-linux-x64.tar.xz"
done

# THE CALLER'S GNUPG HOME IS NEITHER READ NOR WRITTEN. The caller's home holds the other key, and the release is signed
# by it: read, it would verify; written, the import of the key file would land in it. Mutation: let the check use the
# GNUPGHOME it inherits -> the sentinel's keyring changes (red).
c="$TMPROOT/callerhome"
s="$(checkcase callerhome "$releaser" "$second")"
index "$c/dist" 22.23.5 22.23.6
release "$c/dist" 22.23.6 "$other_key"
caller="$c/gnupg"
mkdir -m 700 "$caller"
GPGHOMES+=("$caller")
export_key "$other_key" | GNUPGHOME="$caller" gpg --batch --quiet --import 2>/dev/null
gpgsnapshot() { find "$caller" -type f ! -name 'S.*' ! -name '*.lock' ! -name 'random_seed' -exec cksum {} + | LC_ALL=C sort; }
before="$(gpgsnapshot)"
run env GNUPGHOME="$caller" NODE_PIN_DIST="file://$c/dist" NODE_PIN_SCHEDULE="file://$c/schedule.json" \
	NODE_PIN_FINGERPRINTS="$releaser $second" "$s/node-pin-check.sh"
after="$(gpgsnapshot)"
if [ "$status" = 2 ] && [ "$before" = "$after" ]; then
	ok "the check reads and writes nothing of the caller's GnuPG home"
else
	bad "the check reads and writes nothing of the caller's GnuPG home" "exit $status, want 2" \
		"the caller's home changed: $([ "$before" = "$after" ] && echo no || echo yes)" "$err"
fi

# The check finds its own directory whatever CDPATH holds. Mutation: drop the check's `unset CDPATH` -> it reads the
# decoy's empty scripts/, cannot import a key file there, and answers 2 (red).
c="$TMPROOT/cdpath-check"
s="$(checkcase cdpath-check "$releaser" "$second")"
mkdir -p "$c/decoy/scripts"
run within "$c" env CDPATH="$c/decoy" NODE_PIN_DIST="file://$c/dist" NODE_PIN_SCHEDULE="file://$c/schedule.json" \
	NODE_PIN_FINGERPRINTS="$releaser $second" scripts/node-pin-check.sh
expect "the check run by a relative path reads its own directory whatever CDPATH holds -> 0" 0 "the floor is the newest"

# --- the keys' own expiry ------------------------------------------------------------------------------------------

soon_key="$(newkey soon 30d)"
later_key="$(newkey later 400d)"
bounded_key="$(newkey bounded 400d 30d)"

keycase() { # <case> <fpr>...: runs `key` over a key file holding those keys
	local s
	s="$(scripts "$1")"
	shift
	pin "$s" 22.23.5 "$zero"
	export_key "$@" >"$s/node-release-keys.asc"
	run env NODE_PIN_FINGERPRINTS="$*" "$s/node-pin-check.sh" key
}

# The primary certifies and never expires; the subkey signs and does. Mutation: read the primary's expiry alone ->
# "never", exit 0 (red).
keycase soon "$soon_key"
expect "key: a signing subkey expiring in 30 days -> 1, naming the refresh" 1 "" "To refresh them"
keycase later "$later_key"
expect "key: a signing subkey expiring in 400 days -> 0" 0 "signs until $(day 400)"
# Mutation: ignore the primary's own expiry -> the subkey's 400 days, exit 0 (red).
keycase bounded "$bounded_key"
expect "key: a primary expiring in 30 days bounds the subkey that signs -> 1" 1 "" "within the 60-day warning"
keycase expired "$expired_key"
expect "key: a key whose every signing subkey has expired -> 1" 1 "" "has no valid signing key left"
keycase never "$releaser" "$second"
expect "key: keys that never expire -> 0" 0 "no release key expires"
# ANY KEY of the set: one about to expire among keys that never do fails the step, naming that key alone. Mutation:
# judge the keys by the one that lasts longest -> 0 (red); by the first in the file alone -> 0 (red).
keycase oneofmany "$releaser" "$later_key" "$soon_key"
expect "key: one key of several expiring in 30 days -> 1, naming it" 1 "" "the release key $soon_key"
if grep -qF "$later_key" <<<"$err" || grep -qF "the release key $releaser" <<<"$err"; then
	bad "key: only the key about to expire is named" "$err"
else
	ok "key: only the key about to expire is named"
fi

s="$(scripts nokey)"
pin "$s" 22.23.5 "$zero"
echo "not a key" >"$s/node-release-keys.asc"
run env NODE_PIN_FINGERPRINTS="$releaser" "$s/node-pin-check.sh" key
expect "key: a key file that holds no key -> 2" 2 "" "cannot import"

# THE KEYS AND THE PIN THIS TREE SHIPS are whole: `key` imports the key file against the fingerprints the script pins
# and answers 0 or 1 by the calendar, never 2 — which would mean the file is not those keys — and an index and a
# schedule naming the shipped pin current and far from its end read as current.
run "$SCRIPT_DIR/node-pin-check.sh" key
if [ "$status" = 0 ] || [ "$status" = 1 ]; then
	ok "the key file this tree ships holds exactly the releasers' keys the check pins"
else
	bad "the key file this tree ships holds exactly the releasers' keys the check pins" "exit $status" "$err"
fi
s="$(scripts shipped)"
cp "$SCRIPT_DIR/node-pin.env" "$SCRIPT_DIR/node-release-keys.asc" "$s/"
shipped="$(sed -n 's/^NODE_FLOOR_VERSION=//p' "$s/node-pin.env")"
index "$TMPROOT/shipped/dist" "$shipped"
schedule "$TMPROOT/shipped/schedule.json" "$(day 400)"
sed "s/\"v22\"/\"v${shipped%%.*}\"/" "$TMPROOT/shipped/schedule.json" >"$TMPROOT/shipped/s.json"
mv "$TMPROOT/shipped/s.json" "$TMPROOT/shipped/schedule.json"
run env NODE_PIN_DIST="file://$TMPROOT/shipped/dist" NODE_PIN_SCHEDULE="file://$TMPROOT/shipped/schedule.json" \
	"$s/node-pin-check.sh"
expect "the pin file this tree ships reads as current against an index naming it" 0 "the floor is the newest release"

# --- the suite reads nothing of the environment of the person running it ------------------------------------------

# This suite started again under a developer's environment at its most hostile: every knob the scripts read set to
# send a case somewhere else (a cache of the developer's own, a dist tree and a schedule that do not exist,
# fingerprints no key has), a GnuPG home of the developer's own, a NODE_OPTIONS naming an option no Node knows, and a
# curl configuration refusing every protocol but https — every download a case makes with the real curl is file:// —
# in every place curl looks for one: $CURL_HOME, $XDG_CONFIG_HOME, $HOME and $HOME/.config, with and without the dot.
# And it is run as `make node-test` runs it, by a path relative to the tree, under a CDPATH whose one entry holds a
# scripts/ of its own, empty. The nested run must pass, having run its cases, and leave every file of that environment
# exactly as it found it.
#
# Mutation: drop the CURL_HOME unset, or the XDG_CONFIG_HOME or the HOME pin -> the nested run's downloads are refused
# (red). Mutation: drop the NODE_PIN_ scrub, or the CREWLET_NODE_CACHE or XDG_CACHE_HOME unset -> the nested run's
# cases that hold the scripts' defaults read the developer's tree or fill the developer's cache (red). Mutation: drop
# the CDPATH unset -> the nested run finds its own directory in the decoy, and fails at its first case (red). The
# GnuPG home is unset as scripts/git-pin_test.sh unsets it, though every gpg here names a home of its own: a person's
# home is then no case's even by mistake, and the developer's home above stays as it was.
if [ -z "${CREWLET_PIN_TEST_NESTED-}" ]; then
	dev="$TMPROOT/dev"
	mkdir -p "$dev/curl/.config" "$dev/config" "$dev/home/.config" "$dev/cache" "$dev/xdg-cache" \
		"$dev/cdpath/$(basename "$SCRIPT_DIR")"
	mkdir -m 700 "$dev/gnupg"
	GPGHOMES+=("$dev/gnupg")
	export_key "$other_key" | GNUPGHOME="$dev/gnupg" gpg --batch --quiet --import 2>/dev/null
	for rc in "$dev/curl/.curlrc" "$dev/curl/curlrc" "$dev/curl/.config/curlrc" "$dev/curl/.config/.curlrc" \
		"$dev/config/.curlrc" "$dev/config/curlrc" \
		"$dev/home/.curlrc" "$dev/home/.config/curlrc" "$dev/home/.config/.curlrc"; do
		echo 'proto = "=https"' >"$rc"
	done
	devsnapshot() { find "$dev" -type f ! -name 'S.*' ! -name '*.lock' ! -name 'random_seed' -exec cksum {} + | LC_ALL=C sort; }
	before="$(devsnapshot)"
	nested="$(within "$(dirname "$SCRIPT_DIR")" env CREWLET_PIN_TEST_NESTED=1 \
		CURL_HOME="$dev/curl" XDG_CONFIG_HOME="$dev/config" HOME="$dev/home" CDPATH="$dev/cdpath" \
		GNUPGHOME="$dev/gnupg" CREWLET_NODE_CACHE="$dev/cache" XDG_CACHE_HOME="$dev/xdg-cache" \
		NODE_OPTIONS="--an-option-no-node-knows" NODE_PIN_DIST="file://$dev/no-dist" \
		NODE_PIN_SCHEDULE="file://$dev/no-schedule.json" NODE_PIN_FINGERPRINTS="$zero" \
		"$(basename "$SCRIPT_DIR")/$(basename "${BASH_SOURCE[0]}")" 2>&1)" && status=0 || status=$?
	after="$(devsnapshot)"
	if [ "$status" -eq 0 ] && grep -q '^ok   a fetched build is published whole, holding node and its licence alone$' <<<"$nested" &&
		grep -q '^ok   the floor the newest of its major, its end of life far -> 0$' <<<"$nested" &&
		grep -q ' 0 failed$' <<<"$nested" && [ "$before" = "$after" ]; then
		ok "under a developer's environment the suite passes and touches nothing of it"
	else
		bad "under a developer's environment the suite passes and touches nothing of it" "exit $status" \
			"the environment's files changed: $([ "$before" = "$after" ] && echo no || echo yes)" "$nested"
	fi
fi

echo
echo "node-pin_test.sh: $pass passed, $fail failed"
[ "$fail" -eq 0 ]

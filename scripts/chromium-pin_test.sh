#!/usr/bin/env bash
#
# chromium-pin_test.sh — the own suite of the Chromium recipe (scripts/chromium.sh).
#
# No Go test reaches the recipe, and it regresses silently in the direction that matters: a recipe that unpacks into the
# directory another checkout's suite is running its Chromium from, hands back a build it never checked, or prints
# something on stdout beside the path it is asked for. So every case runs the REAL script — copied
# beside a pin file the case writes, because it reads its pin beside itself — against local stand-ins: a Chrome for
# Testing tree under file:// (<version>/<platform>/chrome-<platform>.zip), each zip holding a stand-in `chrome` that
# answers --version, a stand-in `uname` that names the platform each case runs as, and, for the case that holds the
# recipe's defaults, a stand-in curl that serves Chrome for Testing's own bucket from a local tree. It fetches no
# Chromium and reads no network, and it needs bash, curl, zip, unzip and a SHA-256 tool.
set -euo pipefail

# THE PERSON'S ENVIRONMENT, GONE before the first case, as scripts/git-pin_test.sh clears it and for its reasons:
# every variable whose name begins CHROMIUM_ — the recipe's own knob, CHROMIUM_PIN_DOWNLOADS, among them — the cache
# (CREWLET_CHROMIUM_CACHE, XDG_CACHE_HOME), curl's own configuration (CURL_HOME here, and HOME and XDG_CONFIG_HOME
# made the suite's own below) and CDPATH. The suite's last case runs the whole suite again, by a relative path, under
# a hostile environment holding every one of these and requires it green and the hostile directories untouched.
for name in $(compgen -e); do
	case "$name" in
	CHROMIUM_*) unset "$name" ;;
	esac
done
unset CREWLET_CHROMIUM_CACHE XDG_CACHE_HOME CURL_HOME CDPATH

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

missing=""
for tool in curl zip unzip mkfifo; do
	command -v "$tool" >/dev/null 2>&1 || missing="$missing $tool"
done
command -v sha256sum >/dev/null 2>&1 || command -v shasum >/dev/null 2>&1 || missing="$missing sha256sum-or-shasum"
if [ -n "$missing" ]; then
	echo "chromium-pin_test.sh needs:$missing — CONTRIBUTING.md, Development setup, says what to install" >&2
	exit 1
fi

TMPROOT="$(mktemp -d)"
killtree() { # <pid>: the process and every process below it, children first
	local child
	for child in $(pgrep -P "$1" 2>/dev/null); do
		killtree "$child"
	done
	kill "$1" 2>/dev/null || true
}
cleanup() {
	# A fetcher a failed case left held — in a download from a pipe nobody will write, or in an unpack waiting for
	# its release — is stopped with everything it started before its directory goes.
	local pid
	for pid in $(jobs -p); do
		killtree "$pid"
	done
	wait 2>/dev/null || true
	rm -rf "$TMPROOT"
}
trap cleanup EXIT

export HOME="$TMPROOT/home"
export XDG_CONFIG_HOME="$TMPROOT/config"
mkdir -p "$HOME" "$XDG_CONFIG_HOME"

# The suite's tools, first on PATH: curl through a recorder, so a case can hold the recipe to how it downloads as well
# as to what — every download is logged with its arguments, then made by the real curl — and a stand-in uname naming
# the platform a case runs as: Linux on x86_64 unless the case says otherwise (UNAME_S, UNAME_M), so every case runs
# the same on any machine the suite runs on.
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

# bounded <name>: every download logged since the log was emptied gives up on a transfer that stalls — nothing
# received for a minute, or no connection in 30 seconds — rather than holding its caller until somebody else's limit.
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
# no network; prints the directory to put first on the case's PATH. The address asked for is the last argument, as
# the recipe gives it.
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

# scripts <case>: a scripts directory for the case — the recipe copied from this tree, byte for byte — printing its
# path. The case writes the pin beside it.
scripts() {
	local dir="$TMPROOT/$1/scripts"
	mkdir -p "$dir"
	cp "$SCRIPT_DIR/chromium.sh" "$dir/"
	echo "$dir"
}

zero=0000000000000000000000000000000000000000000000000000000000000000

# pin <dir> <newest> <newest linux64 sha> <floor> <floor linux64 sha> [extra line]...: a pin file naming each pin's
# four platforms, every digest but linux64's the zero digest, and each extra line after them.
pin() {
	local dir="$1" newest="$2" newest_sha="$3" floor="$4" floor_sha="$5"
	shift 5
	{
		echo "# a pin file a case wrote"
		echo "CHROMIUM_NEWEST_VERSION=$newest"
		echo "CHROMIUM_NEWEST_SHA256_LINUX64=$newest_sha"
		echo "CHROMIUM_NEWEST_SHA256_LINUX_ARM64=$zero"
		echo "CHROMIUM_NEWEST_SHA256_MAC_X64=$zero"
		echo "CHROMIUM_NEWEST_SHA256_MAC_ARM64=$zero"
		echo "CHROMIUM_FLOOR_VERSION=$floor"
		echo "CHROMIUM_FLOOR_SHA256_LINUX64=$floor_sha"
		echo "CHROMIUM_FLOOR_SHA256_LINUX_ARM64=$zero"
		echo "CHROMIUM_FLOOR_SHA256_MAC_X64=$zero"
		echo "CHROMIUM_FLOOR_SHA256_MAC_ARM64=$zero"
		local line
		for line in "$@"; do
			echo "$line"
		done
	} >"$dir/chromium-pin.env"
}

# repin <dir> <name> [value]: the case's pin file with NAME's line set to value, or dropped without one.
repin() {
	local file="$1/chromium-pin.env"
	grep -v "^$2=" "$file" >"$file.new" || true
	[ $# -lt 3 ] || echo "$2=$3" >>"$file.new"
	mv "$file.new" "$file"
}

# exe <platform>: where a build of the platform keeps its executable, as Chrome for Testing lays its zips out.
exe() {
	case "$1" in
	linux*) echo "chrome-$1/chrome" ;;
	mac*) echo "chrome-$1/Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing" ;;
	esac
}

# standin <tree> <version> <platform> [answer]: writes <tree>/<version>/<platform>/chrome-<platform>.zip, holding a
# stand-in executable at the platform's path that answers --version as Chrome for Testing does (with its trailing
# blank) — `Google Chrome for Testing <version>`, or the answer given — and a deb.deps beside it; prints the zip's
# SHA-256.
standin() {
	local tree="$1" version="$2" platform="$3" answer="${4-Google Chrome for Testing $2}"
	local src="$TMPROOT/src-$version-$RANDOM" path
	path="$(exe "$platform")"
	mkdir -p "$src/$(dirname "$path")" "$tree/$version/$platform"
	printf '#!/bin/sh\n[ "$1" = --version ] && echo "%s " && exit 0\nexit 3\n' "$answer" >"$src/$path"
	chmod 755 "$src/$path"
	echo "libnss3" >"$src/chrome-$platform/deb.deps"
	(cd "$src" && zip -qr - "chrome-$platform") >"$tree/$version/$platform/chrome-$platform.zip"
	rm -rf "$src"
	digest "$tree/$version/$platform/chrome-$platform.zip"
}

# named <version> <platform> <sha>: the name the recipe publishes a build of that zip under, by the layout it sets.
layout="$(sed -n 's/^layout=\([0-9][0-9]*\)$/\1/p' "$SCRIPT_DIR/chromium.sh")"
if [ -z "$layout" ]; then
	echo "chromium-pin_test.sh: scripts/chromium.sh sets no layout=<number> line" >&2
	exit 1
fi
named() {
	echo "$1-$2-$(printf '%s' "$3" | cut -c1-12)-l$layout"
}

# ============================================================================================================
# scripts/chromium.sh
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
	run within "$cwd" env CREWLET_CHROMIUM_CACHE="$TMPROOT/$case_name/cache" \
		CHROMIUM_PIN_DOWNLOADS="file://$TMPROOT/refusals" "$@"
	expect "$name" 2 "" "$has"
	if [ -s "$TMPROOT/curl.log" ] || [ -n "$(ls -A "$cwd")" ]; then
		bad "$name, downloading and making nothing" "downloads: $(cat "$TMPROOT/curl.log")" \
			"made where it ran: $(ls -A "$cwd")"
	else
		ok "$name, downloading and making nothing"
	fi
}
sha="$(standin "$TMPROOT/refusals" 157.0.8103.38 linux64)"

s="$(scripts unknown)"
pin "$s" 157.0.8103.38 "$sha" 153.0.8010.52 "$zero"
refused unknown "chromium.sh refuses a pin it does not know, with its usage" "usage: scripts/chromium.sh newest|floor" \
	"$s/chromium.sh" nightly

# The pin is read beside the recipe and nowhere else, so a recipe with none beside it refuses, naming the file it looked
# for. Mutation: drop the recipe's check for its pin -> it still answers 2, its grep and sed failing over a file that is
# not there, but names no pin (red).
s="$(scripts nopin)"
refused nopin "chromium.sh refuses to run without its pin beside it, naming the file" \
	"$s/chromium-pin.env is missing" "$s/chromium.sh" newest

s="$(scripts twice)"
pin "$s" 157.0.8103.38 "$sha" 153.0.8010.52 "$zero" "CHROMIUM_NEWEST_VERSION=158.0.8127.18"
refused twice "chromium.sh refuses a pin file setting a name twice, naming it" \
	"sets CHROMIUM_NEWEST_VERSION 2 times" "$s/chromium.sh" newest

s="$(scripts three)"
pin "$s" 157.0.8103 "$sha" 153.0.8010.52 "$zero"
refused three "chromium.sh refuses a version of three numbers, naming the variable" \
	"CHROMIUM_NEWEST_VERSION in $s/chromium-pin.env is \"157.0.8103\"" "$s/chromium.sh" newest

s="$(scripts short-sha)"
pin "$s" 157.0.8103.38 "${sha:1}" 153.0.8010.52 "$zero"
refused short-sha "chromium.sh refuses a SHA-256 of 63 digits, naming the variable" \
	"CHROMIUM_NEWEST_SHA256_LINUX64" "$s/chromium.sh" newest

s="$(scripts riscv)"
pin "$s" 157.0.8103.38 "$sha" 153.0.8010.52 "$zero"
refused riscv "chromium.sh refuses a platform Chrome for Testing has no build for, naming it" \
	"no build for Linux riscv64" env UNAME_M=riscv64 "$s/chromium.sh" newest

# A platform the pin file names no digest for is refused by its variable, never fetched unchecked. Mutation: read the
# linux64 digest for every platform -> the arm64 case fetches (red).
s="$(scripts unpinned)"
pin "$s" 157.0.8103.38 "$sha" 153.0.8010.52 "$zero"
repin "$s" CHROMIUM_NEWEST_SHA256_LINUX_ARM64
refused unpinned "chromium.sh refuses a platform its pin names no digest for, naming the variable" \
	"sets CHROMIUM_NEWEST_SHA256_LINUX_ARM64 0 times" env UNAME_M=aarch64 "$s/chromium.sh" newest

# A build cache that is not an absolute path is refused before anything is looked for in it, whichever variable gave
# it: the make targets hand the suite the path the recipe prints, and each package's cases run in a directory of their
# own, where a relative path would not name the build. Mutation: drop the refusal -> the recipe fetches into a cache in
# the directory it was run from, and prints a relative path (red).
s="$(scripts relative)"
pin "$s" 157.0.8103.38 "$sha" 153.0.8010.52 "$zero"
refused relative "chromium.sh refuses a build cache that is not an absolute path, naming the variables" \
	"the build cache relative/cache is not an absolute path (CREWLET_CHROMIUM_CACHE, XDG_CACHE_HOME, HOME)" \
	env CREWLET_CHROMIUM_CACHE=relative/cache "$s/chromium.sh" newest
refused relative-xdg "chromium.sh refuses a default build cache that is not an absolute path, naming the variables" \
	"the build cache relative/crewlet/chromium is not an absolute path (CREWLET_CHROMIUM_CACHE, XDG_CACHE_HOME, HOME)" \
	env -u CREWLET_CHROMIUM_CACHE XDG_CACHE_HOME=relative "$s/chromium.sh" newest

# unzip, which only an unpack needs, is asked for before the download it would unpack: a PATH holding everything the
# recipe runs up to its download but unzip. Mutation: check for unzip only after the download -> it downloads (red).
s="$(scripts nounzip)"
pin "$s" 157.0.8103.38 "$sha" 153.0.8010.52 "$zero"
mkdir -p "$TMPROOT/nounzip/bin"
for tool in dirname grep sed tr cut sha256sum shasum mkdir; do
	if found="$(command -v "$tool")"; then ln -s "$found" "$TMPROOT/nounzip/bin/$tool"; fi
done
ln -s "$TMPROOT/tools/curl" "$TMPROOT/nounzip/bin/curl"
ln -s "$TMPROOT/tools/uname" "$TMPROOT/nounzip/bin/uname"
refused nounzip "chromium.sh refuses to fetch without unzip, naming it" "needs: unzip" \
	env PATH="$TMPROOT/nounzip/bin" "$s/chromium.sh" newest

# curl, which only a download needs, is asked for before the download: a PATH holding every tool the recipe runs but
# curl. Mutation: drop curl from the recipe's tool check -> it reaches its download, which fails (1, red).
s="$(scripts nocurl)"
pin "$s" 157.0.8103.38 "$sha" 153.0.8010.52 "$zero"
mkdir -p "$TMPROOT/nocurl/bin"
for tool in dirname grep sed tr cut sha256sum shasum mkdir mktemp chmod unzip rm ln find; do
	if found="$(command -v "$tool")"; then ln -s "$found" "$TMPROOT/nocurl/bin/$tool"; fi
done
ln -s "$TMPROOT/tools/uname" "$TMPROOT/nocurl/bin/uname"
refused nocurl "chromium.sh refuses to fetch without curl, naming it" "needs: curl" \
	env PATH="$TMPROOT/nocurl/bin" "$s/chromium.sh" newest

# Neither sha256sum nor shasum: the recipe cannot check a zip, and says what to install before it downloads one.
# Mutation: drop the recipe's check for a SHA-256 tool -> it downloads, and fails at the zip's digest (1, red).
s="$(scripts nosha)"
pin "$s" 157.0.8103.38 "$sha" 153.0.8010.52 "$zero"
mkdir -p "$TMPROOT/nosha/bin"
for tool in dirname grep sed tr cut mkdir mktemp chmod unzip rm ln find; do
	if found="$(command -v "$tool")"; then ln -s "$found" "$TMPROOT/nosha/bin/$tool"; fi
done
ln -s "$TMPROOT/tools/curl" "$TMPROOT/nosha/bin/curl"
ln -s "$TMPROOT/tools/uname" "$TMPROOT/nosha/bin/uname"
refused nosha "chromium.sh refuses to fetch without a SHA-256 tool, naming both" "needs sha256sum, or shasum" \
	env PATH="$TMPROOT/nosha/bin" "$s/chromium.sh" newest

# hashtool <dir> <fails|garbles> [pattern]: scripts/git-pin_test.sh's stand-in sha256sum, first on a PATH that puts
# <dir> first. Over a file matching the pattern (every file without one) it fails having printed nothing, or prints a
# line that is no SHA-256 and answers 0; over any other file it is the machine's own. The recipe reads sha256sum
# first.
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


# --- a fetched build is checked, and published whole ---------------------------------------------------------------

# A fresh fetch: the zip downloaded from the pin's own path, checked, unpacked into the build's own directory with the
# download removed, .complete written, and the published name's executable printed and nothing else on stdout — a
# path, which the make targets that run the whole suite export as its browser. Then a second run hands back that
# build and downloads nothing. Mutation: leave the download beside the build -> red.
s="$(scripts fresh)"
sha="$(standin "$TMPROOT/fresh/downloads" 157.0.8103.38 linux64)"
pin "$s" 157.0.8103.38 "$sha" 153.0.8010.52 "$zero"
: >"$TMPROOT/curl.log"
run env CREWLET_CHROMIUM_CACHE="$TMPROOT/fresh/cache" CHROMIUM_PIN_DOWNLOADS="file://$TMPROOT/fresh/downloads" \
	"$s/chromium.sh" newest
link="$TMPROOT/fresh/cache/$(named 157.0.8103.38 linux64 "$sha")"
expect "chromium.sh fetches, checks and publishes a build, printing its executable" 0 "$link/chrome-linux64/chrome"
why=()
[ "$out" = "$link/chrome-linux64/chrome" ] || why+=("stdout is not the executable's path alone: $out")
[ -L "$link" ] || why+=("$link is not the published name, a link")
[ -f "$link/.complete" ] || why+=("the build holds no .complete")
[ ! -e "$link/.download" ] || why+=("the build still holds its download")
grep -qF "$TMPROOT/fresh/downloads/157.0.8103.38/linux64/chrome-linux64.zip" "$TMPROOT/curl.log" ||
	why+=("it did not download the pin's own zip: $(cat "$TMPROOT/curl.log")")
if [ ${#why[@]} -eq 0 ]; then
	ok "a fetched build is published whole, its download removed"
else
	bad "a fetched build is published whole, its download removed" "${why[@]}"
fi
# Mutation: drop the stall bound from the recipe's download -> red.
bounded "every download the recipe makes ends when it stalls"
: >"$TMPROOT/curl.log"
run env CREWLET_CHROMIUM_CACHE="$TMPROOT/fresh/cache" CHROMIUM_PIN_DOWNLOADS="file://$TMPROOT/fresh/nowhere" \
	"$s/chromium.sh" newest
expect "chromium.sh hands back an intact build" 0 "$link/chrome-linux64/chrome"
undownloaded "chromium.sh hands back an intact build and downloads nothing"

# The recipe finds its own directory whatever CDPATH holds: scripts/git.sh's reason. Mutation: drop the recipe's
# `unset CDPATH` -> it finds no pin beside itself in the decoy's empty scripts/ and refuses (2, red).
mkdir -p "$TMPROOT/fresh/decoy/scripts"
run within "$TMPROOT/fresh" env CDPATH="$TMPROOT/fresh/decoy" CREWLET_CHROMIUM_CACHE="$TMPROOT/fresh/cache" \
	CHROMIUM_PIN_DOWNLOADS="file://$TMPROOT/fresh/nowhere" scripts/chromium.sh newest
expect "chromium.sh run by a relative path reads its own directory whatever CDPATH holds" 0 "$link/chrome-linux64/chrome"

# An intact build needs none of what a fetch runs: a PATH holding what the recipe reads its pin and platform with,
# and neither curl, unzip nor a SHA-256 tool, hands back the build fetched above. Mutation: check for what a fetch
# needs before looking for an intact build -> it refuses (2, red).
mkdir -p "$TMPROOT/fresh/bare"
for tool in dirname grep sed tr cut; do
	ln -s "$(command -v "$tool")" "$TMPROOT/fresh/bare/$tool"
done
ln -s "$TMPROOT/tools/uname" "$TMPROOT/fresh/bare/uname"
run env PATH="$TMPROOT/fresh/bare" CREWLET_CHROMIUM_CACHE="$TMPROOT/fresh/cache" \
	CHROMIUM_PIN_DOWNLOADS="file://$TMPROOT/fresh/nowhere" "$s/chromium.sh" newest
expect "chromium.sh hands back an intact build where curl, unzip and a SHA-256 tool are missing" 0 \
	"$link/chrome-linux64/chrome"

# The floor is the same recipe over its own lines.
sha="$(standin "$TMPROOT/fresh/downloads" 153.0.8010.52 linux64)"
pin "$s" 157.0.8103.38 "$zero" 153.0.8010.52 "$sha"
run env CREWLET_CHROMIUM_CACHE="$TMPROOT/fresh/cache" CHROMIUM_PIN_DOWNLOADS="file://$TMPROOT/fresh/downloads" \
	"$s/chromium.sh" floor
expect "chromium.sh fetches the floor by its own lines" 0 \
	"$TMPROOT/fresh/cache/$(named 153.0.8010.52 linux64 "$sha")/chrome-linux64/chrome"

# A BUILD IS NAMED BY WHAT DECIDES ITS BYTES, its pin's digest and the recipe's layout, never the recipe's own text.
# An edit to the recipe that is not its layout — a line of comment — hands back the build already fetched, downloading
# nothing. Mutation: name a build by the recipe's own SHA-256, as scripts/git.sh names a git -> the edited recipe
# fetches again (red).
s="$(scripts names)"
sha="$(standin "$TMPROOT/names/downloads" 157.0.8103.38 linux64)"
pin "$s" 157.0.8103.38 "$sha" 153.0.8010.52 "$zero"
names() { # <name>: one `chromium.sh newest` of the case's scripts over its cache and downloads, its log emptied first
	: >"$TMPROOT/curl.log"
	run env CREWLET_CHROMIUM_CACHE="$TMPROOT/names/cache" CHROMIUM_PIN_DOWNLOADS="file://$TMPROOT/names/downloads" \
		"$s/chromium.sh" newest
	expect "$1" 0 "$TMPROOT/names/cache/157.0.8103.38-linux64-"
}
names "chromium.sh fetches a build to name"
first="$out"
echo "# a line of comment, which changes no build" >>"$s/chromium.sh"
names "chromium.sh, edited but for its layout, answers"
if [ "$out" = "$first" ]; then
	undownloaded "an edit to the recipe that is not its layout reuses the build, downloading nothing"
else
	bad "an edit to the recipe that is not its layout reuses the build, downloading nothing" "printed $out, not $first"
fi
# Another zip of the same version — the pin's digest corrected — is a build of its own, never the one fetched for the
# earlier digest. Mutation: leave the digest out of a build's name -> the earlier build is handed back (red).
zipfile="$TMPROOT/names/downloads/157.0.8103.38/linux64/chrome-linux64.zip"
echo "another zip of 157.0.8103.38" | zip -qz "$zipfile"
sha="$(digest "$zipfile")"
pin "$s" 157.0.8103.38 "$sha" 153.0.8010.52 "$zero"
names "chromium.sh, its pin's digest moved, answers"
if [ "$out" = "$TMPROOT/names/cache/$(named 157.0.8103.38 linux64 "$sha")/chrome-linux64/chrome" ] &&
	[ "$out" != "$first" ] && [ -s "$TMPROOT/curl.log" ]; then
	ok "a pin whose digest moved fetches a build of its own"
else
	bad "a pin whose digest moved fetches a build of its own" "printed $out after $first" \
		"downloads: $(cat "$TMPROOT/curl.log")"
fi
# A recipe of another layout never reads a build laid out by this one. Mutation: leave the layout out of a build's
# name -> the build of the earlier layout is handed back (red).
second="$out"
sed "s/^layout=$layout\$/layout=$((layout + 1))/" "$s/chromium.sh" >"$s/chromium.sh.new"
mv "$s/chromium.sh.new" "$s/chromium.sh"
chmod +x "$s/chromium.sh"
names "chromium.sh of the next layout answers"
if [ "$out" != "$second" ] && [ -s "$TMPROOT/curl.log" ]; then
	ok "a recipe of another layout fetches a build of its own"
else
	bad "a recipe of another layout fetches a build of its own" "printed $out, as the earlier layout did" \
		"downloads: $(cat "$TMPROOT/curl.log")"
fi

# A Mac's build keeps its executable inside the app bundle, and the recipe prints that path. Mutation: print
# chrome-<platform>/chrome for every platform -> red.
s="$(scripts mac)"
sha="$(standin "$TMPROOT/mac/downloads" 157.0.8103.38 mac-arm64)"
pin "$s" 157.0.8103.38 "$zero" 153.0.8010.52 "$zero"
repin "$s" CHROMIUM_NEWEST_SHA256_MAC_ARM64 "$sha"
run env UNAME_S=Darwin UNAME_M=arm64 CREWLET_CHROMIUM_CACHE="$TMPROOT/mac/cache" \
	CHROMIUM_PIN_DOWNLOADS="file://$TMPROOT/mac/downloads" "$s/chromium.sh" newest
expect "chromium.sh on a Mac prints the executable inside the app bundle" 0 \
	"$TMPROOT/mac/cache/$(named 157.0.8103.38 mac-arm64 "$sha")/$(exe mac-arm64)"

# WHAT THE RECIPE READS WHEN NOTHING NAMES IT: its downloads are Chrome for Testing's own bucket and its cache is
# ~/.cache/crewlet/chromium — the suite's HOME — whenever CHROMIUM_PIN_DOWNLOADS, CREWLET_CHROMIUM_CACHE and
# XDG_CACHE_HOME are unset, as the suite leaves them, having cleared the person's. A stand-in curl serves that one
# bucket from a local tree, so the case reads no network. Mutation: misspell the default bucket -> red; drop the
# suite's CHROMIUM_ scrub, or its CREWLET_CHROMIUM_CACHE or XDG_CACHE_HOME unset -> the nested run's copy of this case
# fetches from the developer's downloads or into the developer's cache (red).
s="$(scripts defaults)"
sha="$(standin "$TMPROOT/defaults/web/storage.googleapis.com/chrome-for-testing-public" 157.0.8103.38 linux64)"
pin "$s" 157.0.8103.38 "$sha" 153.0.8010.52 "$zero"
run env PATH="$(offline "$TMPROOT/defaults/web"):$PATH" "$s/chromium.sh" newest
expect "chromium.sh reads Chrome for Testing's own bucket and ~/.cache/crewlet/chromium when nothing names others" 0 \
	"$HOME/.cache/crewlet/chromium/$(named 157.0.8103.38 linux64 "$sha")/chrome-linux64/chrome"

# A zip that is not the pin's is refused before it is unpacked, naming both digests, and nothing is published.
# Mutation: accept the zip without comparing its digest -> exit 0 (red).
s="$(scripts tampered)"
served="$(standin "$TMPROOT/tampered/downloads" 157.0.8103.38 linux64)"
pin "$s" 157.0.8103.38 "$zero" 153.0.8010.52 "$zero"
run env CREWLET_CHROMIUM_CACHE="$TMPROOT/tampered/cache" CHROMIUM_PIN_DOWNLOADS="file://$TMPROOT/tampered/downloads" \
	"$s/chromium.sh" newest
expect "chromium.sh refuses a zip that is not the pin's (1), naming both digests" 1 "" "hashes to $served, and"
if [ -e "$TMPROOT/tampered/cache/$(named 157.0.8103.38 linux64 "$zero")" ] ||
	[ -n "$(find "$TMPROOT/tampered/cache" -mindepth 1 -maxdepth 1 2>/dev/null)" ]; then
	bad "a zip that is not the pin's leaves nothing in the cache" "$(ls -la "$TMPROOT/tampered/cache")"
else
	ok "a zip that is not the pin's leaves nothing in the cache"
fi

# A build that answers another version is refused, naming the libraries a Linux build needs, and nothing is
# published. Mutation: publish without asking the build its version -> exit 0 (red).
s="$(scripts disagrees)"
sha="$(standin "$TMPROOT/disagrees/downloads" 157.0.8103.38 linux64 "Google Chrome for Testing 156.0.8078.80")"
pin "$s" 157.0.8103.38 "$sha" 153.0.8010.52 "$zero"
run env CREWLET_CHROMIUM_CACHE="$TMPROOT/disagrees/cache" CHROMIUM_PIN_DOWNLOADS="file://$TMPROOT/disagrees/downloads" \
	"$s/chromium.sh" newest
expect "chromium.sh refuses a build whose --version disagrees (1)" 1 "" \
	"it said \"Google Chrome for Testing 156.0.8078.80\""
expect "chromium.sh names deb.deps for a Linux build that does not answer" 1 "" "deb.deps"
if [ -e "$TMPROOT/disagrees/cache/$(named 157.0.8103.38 linux64 "$sha")" ]; then
	bad "a build that disagrees is never published" "$(ls -la "$TMPROOT/disagrees/cache")"
else
	ok "a build that disagrees is never published"
fi

# A zip whose SHA-256 the machine's hash cannot compute is a zip nothing checked: refused (1), naming it, and nothing is
# published — never a digest of nothing compared against the pin. Mutation: let digest answer whatever its hash
# printed, as `sha256sum FILE | cut` did -> "hashes to , and" (red).
s="$(scripts hashfails)"
sha="$(standin "$TMPROOT/hashfails/downloads" 157.0.8103.38 linux64)"
pin "$s" 157.0.8103.38 "$sha" 153.0.8010.52 "$zero"
hashtool "$TMPROOT/hashfails/tools" fails '*.zip'
run env PATH="$TMPROOT/hashfails/tools:$PATH" CREWLET_CHROMIUM_CACHE="$TMPROOT/hashfails/cache" \
	CHROMIUM_PIN_DOWNLOADS="file://$TMPROOT/hashfails/downloads" "$s/chromium.sh" newest
expect "chromium.sh refuses a zip whose SHA-256 the machine cannot compute (1)" 1 "" \
	"cannot compute the SHA-256 of chrome-linux64.zip"
if [ -e "$TMPROOT/hashfails/cache/$(named 157.0.8103.38 linux64 "$sha")" ]; then
	bad "a zip nothing checked is never published" "$(ls -la "$TMPROOT/hashfails/cache")"
else
	ok "a zip nothing checked is never published"
fi

# A NAME POINTING AT NO INTACT BUILD is fetched over, whichever way it points there: a link left on a build without
# .complete, whose executable answers the pin's version all the same, and a directory at the name itself holding the
# same, which no fetcher makes. The recipe hands back neither, fetches, and publishes over it — the name removed and
# made a link to the new build — printing the new build's executable. Mutations: skip the publish loop's removal of
# the name -> the link is refused three times (1, red); drop its case of a directory at the name -> the link is made
# INSIDE the directory, which the name stays, and the recipe answers 0 all the same (red).
for shape in link directory; do
	s="$(scripts "incomplete-$shape")"
	sha="$(standin "$TMPROOT/incomplete-$shape/downloads" 157.0.8103.38 linux64)"
	pin "$s" 157.0.8103.38 "$sha" 153.0.8010.52 "$zero"
	link="$TMPROOT/incomplete-$shape/cache/$(named 157.0.8103.38 linux64 "$sha")"
	broken="$link.stale1"
	[ "$shape" = link ] || broken="$link"
	mkdir -p "$broken/chrome-linux64"
	printf '#!/bin/sh\necho "Google Chrome for Testing 157.0.8103.38 "\n' >"$broken/chrome-linux64/chrome"
	chmod 755 "$broken/chrome-linux64/chrome"
	[ "$shape" = directory ] || ln -s "$(basename "$broken")" "$link"
	: >"$TMPROOT/curl.log"
	run env CREWLET_CHROMIUM_CACHE="$TMPROOT/incomplete-$shape/cache" \
		CHROMIUM_PIN_DOWNLOADS="file://$TMPROOT/incomplete-$shape/downloads" "$s/chromium.sh" newest
	expect "chromium.sh fetches again over a $shape at the name holding no intact build" 0 "$link/chrome-linux64/chrome"
	why=()
	[ -L "$link" ] || why+=("the name is not a link")
	[ -f "$link/.complete" ] || why+=("the name points at no build with .complete")
	[ "$shape" = directory ] || [ "$(cd "$link" && pwd -P)" != "$(cd "$broken" && pwd -P)" ] ||
		why+=("the name still points at the broken build")
	[ -s "$TMPROOT/curl.log" ] || why+=("it downloaded nothing")
	if [ ${#why[@]} -eq 0 ]; then
		ok "a $shape at the name holding no intact build is replaced by a link to the build fetched in its place"
	else
		bad "a $shape at the name holding no intact build is replaced by a link to the build fetched in its place" \
			"${why[@]}" "$err"
	fi
done

# --- a curl configuration changes nothing the recipe prints ------------------------------------------------------

# The recipe's stdout is the executable's path and nothing else, because whoever asks for a build reads its path
# there: a `write-out` in a person's curlrc once put a 000 above git.sh's directory. A fetch under such a curlrc must
# print the path alone, and what the curlrc wrote on stderr (the control: the configuration was read, and kept).
# Mutation: let the recipe's download write curl's stdout to its own -> red.
s="$(scripts curlrc)"
sha="$(standin "$TMPROOT/curlrc/downloads" 157.0.8103.38 linux64)"
pin "$s" 157.0.8103.38 "$sha" 153.0.8010.52 "$zero"
run env CURL_HOME="$(curlhome "$TMPROOT/curlrc/curl")" CREWLET_CHROMIUM_CACHE="$TMPROOT/curlrc/cache" \
	CHROMIUM_PIN_DOWNLOADS="file://$TMPROOT/curlrc/downloads" "$s/chromium.sh" newest
why=()
[ "$status" = 0 ] || why+=("exit $status, want 0")
[ "$out" = "$TMPROOT/curlrc/cache/$(named 157.0.8103.38 linux64 "$sha")/chrome-linux64/chrome" ] ||
	why+=("its stdout is not the executable's path alone")
grep -qx 000 <<<"$err" || why+=("stderr lacks the curlrc's write-out, 000: the configuration was not read")
if [ ${#why[@]} -eq 0 ]; then
	ok "a curlrc that writes to curl's stdout changes nothing the recipe prints"
else
	bad "a curlrc that writes to curl's stdout changes nothing the recipe prints" "${why[@]}" \
		"stdout:" "$out" "stderr:" "$err"
fi

# --- two fetchers at once publish one build, and neither deletes the other's Chromium ------------------------------

# Two `chromium.sh newest` runs over one cache, the shape of two worktrees' `make test` starting together: B finds no
# build and starts one, and its download is held (the zip it reads is a pipe the case writes when it chooses);
# meanwhile A finds no build either, fetches and publishes. Then B's download arrives and B is held again in the
# middle of its unpack (an unzip of the case's own, first on B's PATH alone) — and the Chromium A printed must still
# answer, because B unpacks in a directory of its own. Released, B finds the name published, keeps that build and
# removes its own: both print the same path, it answers, and one build directory remains. Mutation: unpack in place at
# the published name, emptied before the unpack -> B's emptying deletes the Chromium A printed while B is still
# fetching (red).
s="$(scripts concurrent)"
ctl="$TMPROOT/concurrent/ctl"
mkdir -p "$ctl" "$TMPROOT/concurrent/held/157.0.8103.38/linux64" "$TMPROOT/concurrent/holdbin"
sha="$(standin "$TMPROOT/concurrent/downloads" 157.0.8103.38 linux64)"
pin "$s" 157.0.8103.38 "$sha" 153.0.8010.52 "$zero"
mkfifo "$TMPROOT/concurrent/held/157.0.8103.38/linux64/chrome-linux64.zip"
cat >"$TMPROOT/concurrent/holdbin/unzip" <<EOF
#!/bin/sh
touch '$ctl/arrived'
while [ ! -e '$ctl/go' ]; do sleep 0.05; done
exec '$(command -v unzip)' "\$@"
EOF
chmod +x "$TMPROOT/concurrent/holdbin/unzip"
fetcher() { # <name> <downloads> [PATH]: one `chromium.sh newest` in the background, its outcome in <ctl>/<name>.*
	(
		PATH="${3:-$PATH}" CREWLET_CHROMIUM_CACHE="$TMPROOT/concurrent/cache" CHROMIUM_PIN_DOWNLOADS="file://$2" \
			"$s/chromium.sh" newest >"$ctl/$1.out" 2>"$ctl/$1.err" && echo 0 >"$ctl/$1.status" || echo $? >"$ctl/$1.status"
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
answers() { [ "$("$1" --version 2>&1 || true)" = "Google Chrome for Testing 157.0.8103.38 " ]; }
started() { grep -q 'fetching Chrome for Testing 157.0.8103.38' "$ctl/b.err" 2>/dev/null; }
concurrent() {
	fetcher b "$TMPROOT/concurrent/held" "$TMPROOT/concurrent/holdbin:$PATH"
	await 60 started || { bad "the held fetcher starts its fetch" "$(cat "$ctl/b.err")"; return; }
	fetcher a "$TMPROOT/concurrent/downloads"
	await 60 test -e "$ctl/a.status" || { bad "the other fetcher finishes" "$(cat "$ctl/a.err")"; return; }
	local aexe
	aexe="$(cat "$ctl/a.out")"
	if [ "$(cat "$ctl/a.status")" != 0 ] || ! answers "$aexe"; then
		bad "the other fetcher publishes a Chromium that answers" "$(cat "$ctl/a.err")"
		return
	fi
	cat "$TMPROOT/concurrent/downloads/157.0.8103.38/linux64/chrome-linux64.zip" \
		>"$TMPROOT/concurrent/held/157.0.8103.38/linux64/chrome-linux64.zip" &
	await 60 test -e "$ctl/arrived" || { bad "the held fetcher reaches its unpack" "$(cat "$ctl/b.err")"; return; }
	if [ ! -e "$ctl/b.status" ] && answers "$aexe"; then
		ok "the Chromium one fetcher published answers while another is still fetching"
	else
		bad "the Chromium one fetcher published answers while another is still fetching" \
			"$aexe answers: $("$aexe" --version 2>&1 || true)"
	fi
	touch "$ctl/go"
	await 60 test -e "$ctl/b.status" || { bad "the held fetcher finishes" "$(cat "$ctl/b.err")"; return; }
	local builds
	builds="$(find "$TMPROOT/concurrent/cache" -mindepth 1 -maxdepth 1 -type d -name "$(named 157.0.8103.38 linux64 "$sha").*" | wc -l | tr -d ' ')"
	if [ "$(cat "$ctl/b.status")" = 0 ] && [ "$(cat "$ctl/b.out")" = "$aexe" ] && answers "$aexe" && [ "$builds" = 1 ]; then
		ok "two fetchers at once both succeed, print one path and leave one build"
	else
		bad "two fetchers at once both succeed, print one path and leave one build" \
			"exit $(cat "$ctl/b.status"); printed $(cat "$ctl/b.out") and $aexe; build directories: $builds" \
			"$(cat "$ctl/b.err")"
	fi
}
concurrent

# --- a crashed fetcher's leftovers are swept, a running one's never ------------------------------------------------

# Two directories of the build's own name beside the cache's published name: one untouched for two days, as a fetcher
# killed part way leaves it, and one fresh, as a fetcher still working holds it. A fetch that publishes removes the
# first and keeps the second. Mutation: sweep every leftover whatever its age -> the fresh one goes (red); sweep none
# -> the old one stays (red).
s="$(scripts strays)"
sha="$(standin "$TMPROOT/strays/downloads" 157.0.8103.38 linux64)"
pin "$s" 157.0.8103.38 "$sha" 153.0.8010.52 "$zero"
strays="$TMPROOT/strays/cache/$(named 157.0.8103.38 linux64 "$sha")"
mkdir -p "$strays.crashd/.download" "$strays.workng/.download"
twodays="$(($(date +%s) - 2 * 86400))"
stamp="$(date -d "@$twodays" +%Y%m%d%H%M 2>/dev/null || date -r "$twodays" +%Y%m%d%H%M)" # GNU, then BSD date
touch -t "$stamp" "$strays.crashd"
run env CREWLET_CHROMIUM_CACHE="$TMPROOT/strays/cache" CHROMIUM_PIN_DOWNLOADS="file://$TMPROOT/strays/downloads" \
	"$s/chromium.sh" newest
why=()
[ "$status" = 0 ] || why+=("exit $status, want 0" "$err")
[ ! -e "$strays.crashd" ] || why+=("the crashed fetcher's directory stayed")
[ -d "$strays.workng" ] || why+=("the working fetcher's directory went")
if [ ${#why[@]} -eq 0 ]; then
	ok "a fetch sweeps a crashed fetcher's leftovers a day old and keeps a fresh one"
else
	bad "a fetch sweeps a crashed fetcher's leftovers a day old and keeps a fresh one" "${why[@]}"
fi

# --- the suite reads nothing of the environment of the person running it ------------------------------------------

# This suite started again under a developer's environment at its most hostile: every knob the recipe reads set to send
# a case somewhere else (a cache of the developer's own, and downloads that do not exist), a curl configuration refusing
# every protocol but https — every download a case makes with the real curl is file:// — in every place curl looks for
# one: $CURL_HOME, $XDG_CONFIG_HOME, $HOME and $HOME/.config, with and without the dot. And it is run as `make
# chromium-test` runs it, by a path relative to the tree, under a CDPATH whose one entry holds a scripts/ of its own,
# empty. The nested run must pass, having run its cases, and leave every file of that environment exactly as it found
# it.
#
# Mutation: drop the CURL_HOME unset, or the XDG_CONFIG_HOME or the HOME pin -> the nested run's downloads are refused
# (red). Mutation: drop the CHROMIUM_ scrub, or the CREWLET_CHROMIUM_CACHE or XDG_CACHE_HOME unset -> the nested run's
# case that holds the recipe's defaults fetches from the developer's downloads or into the developer's cache (red).
# Mutation: drop the CDPATH unset -> the nested run finds its own directory in the decoy, has no recipe to copy from it,
# and fails at its first case (red).
if [ -z "${CREWLET_PIN_TEST_NESTED-}" ]; then
	dev="$TMPROOT/dev"
	mkdir -p "$dev/curl/.config" "$dev/config" "$dev/home/.config" "$dev/cache" "$dev/xdg-cache" \
		"$dev/cdpath/$(basename "$SCRIPT_DIR")"
	for rc in "$dev/curl/.curlrc" "$dev/curl/curlrc" "$dev/curl/.config/curlrc" "$dev/curl/.config/.curlrc" \
		"$dev/config/.curlrc" "$dev/config/curlrc" \
		"$dev/home/.curlrc" "$dev/home/.config/curlrc" "$dev/home/.config/.curlrc"; do
		echo 'proto = "=https"' >"$rc"
	done
	devsnapshot() { find "$dev" -type f -exec cksum {} + | LC_ALL=C sort; }
	before="$(devsnapshot)"
	nested="$(within "$(dirname "$SCRIPT_DIR")" env CREWLET_PIN_TEST_NESTED=1 \
		CURL_HOME="$dev/curl" XDG_CONFIG_HOME="$dev/config" HOME="$dev/home" CDPATH="$dev/cdpath" \
		CREWLET_CHROMIUM_CACHE="$dev/cache" XDG_CACHE_HOME="$dev/xdg-cache" \
		CHROMIUM_PIN_DOWNLOADS="file://$dev/no-downloads" \
		"$(basename "$SCRIPT_DIR")/$(basename "${BASH_SOURCE[0]}")" 2>&1)" && status=0 || status=$?
	after="$(devsnapshot)"
	if [ "$status" -eq 0 ] && grep -q '^ok   a fetched build is published whole, its download removed$' <<<"$nested" &&
		grep -q ' 0 failed$' <<<"$nested" && [ "$before" = "$after" ]; then
		ok "under a developer's environment the suite passes and touches nothing of it"
	else
		bad "under a developer's environment the suite passes and touches nothing of it" "exit $status" \
			"the environment's files changed: $([ "$before" = "$after" ] && echo no || echo yes)" "$nested"
	fi
fi

echo
echo "chromium-pin_test.sh: $pass passed, $fail failed"
[ "$fail" -eq 0 ]

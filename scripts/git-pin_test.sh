#!/usr/bin/env bash
#
# git-pin_test.sh — the own suite of the git recipe (scripts/git.sh) and of the pin check (scripts/git-pin-check.sh).
#
# No Go test reaches either script, and each regresses silently in the direction that matters: a recipe that
# installs into the directory another checkout's suite is running its git from, or hands back a build it never
# checked; a check that calls every pin current because it read nothing, or verifies a signature without asking
# whose it is. So every case runs the REAL script — copied beside a pin file the case writes, because both read
# their pin (and the check its key) beside themselves — against local stand-ins: a source tarball whose Makefile
# installs a stand-in git, a release listing, and signing keys made for the suite in a GnuPG home of its own. It
# builds no git and reads no network, and it needs bash, make, tar, xz, curl and GnuPG.
set -euo pipefail

# THE PERSON'S ENVIRONMENT, GONE before the first case: every variable whose name begins GIT_ — which takes the
# scripts' own knobs (GIT_PIN_PREFIX, GIT_PIN_TARGET_ARCH, GIT_PIN_DOWNLOADS, GIT_PIN_LISTING, GIT_PIN_FINGERPRINT)
# with git's, so a developer's exported GIT_PIN_PREFIX cannot send a case's install to /usr/local — and the rest
# of what the recipe reads: where it caches (CREWLET_GIT_CACHE, XDG_CACHE_HOME), DESTDIR, and the GnuPG home, so no
# case can read or write the person's keyring. And curl's own configuration, which every download either script
# makes reads before its command line: a .curlrc in $CURL_HOME, in $XDG_CONFIG_HOME and in $HOME (or a curlrc in
# $HOME/.config) — measured, a developer's `proto = "=https"` there turned 21 of 39 cases red, every file:// download
# refused, so a person's curl settings decided whether `make check` passed. CURL_HOME is unset here, and HOME and
# XDG_CONFIG_HOME are the suite's own, below. And CDPATH, which the `cd` that finds this suite's own directory
# searches first when the suite is run by a relative path (`make git-test` runs scripts/git-pin_test.sh): an entry
# holding a scripts/ of its own made that directory the one every case copied its scripts from. The suite's last
# case runs the whole suite again, by a relative path, under a hostile environment holding every one of these and
# requires it green and the hostile directories untouched.
for name in $(compgen -e); do
	case "$name" in
	GIT_*) unset "$name" ;;
	esac
done
unset CREWLET_GIT_CACHE XDG_CACHE_HOME DESTDIR GNUPGHOME CURL_HOME CDPATH

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

missing=""
for tool in make tar xz curl gpg gpgconf; do
	command -v "$tool" >/dev/null 2>&1 || missing="$missing $tool"
done
command -v sha256sum >/dev/null 2>&1 || command -v shasum >/dev/null 2>&1 || missing="$missing sha256sum-or-shasum"
if [ -n "$missing" ]; then
	echo "git-pin_test.sh needs:$missing — CONTRIBUTING.md, Development setup, says what to install" >&2
	exit 1
fi

# A short root: GnuPG's agent puts its socket inside the home it serves wherever there is no /run/user, and a
# socket path past 107 bytes cannot be bound.
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
	# A builder a failed case left held — in its install, or in a download from a pipe nobody will write — is stopped
	# with everything it started before its directory goes: stopping the background job alone leaves its curl
	# waiting on the pipe for ever.
	local pid
	for pid in $(jobs -p); do
		killtree "$pid"
	done
	wait 2>/dev/null || true
	local home
	for home in ${GPGHOMES[@]+"${GPGHOMES[@]}"}; do
		GNUPGHOME="$home" gpgconf --kill all >/dev/null 2>&1 || true
	done
	rm -rf "$TMPROOT"
}
trap cleanup EXIT

export HOME="$TMPROOT/home"
export XDG_CONFIG_HOME="$TMPROOT/config"
mkdir -p "$HOME" "$XDG_CONFIG_HOME"

# Stand-ins for the two tools the recipe asks for and the stand-in tarball never runs: the C compiler and libcurl's
# curl-config. With them a machine without a compiler or libcurl's headers runs the recipe's cases all the same.
mkdir -p "$TMPROOT/tools"
printf '#!/bin/sh\nexit 0\n' >"$TMPROOT/tools/cc"
printf '#!/bin/sh\nexit 0\n' >"$TMPROOT/tools/curl-config"
# And curl itself, through a recorder: every download a script makes is logged with its arguments, then made by the
# real curl, so a case can hold the scripts to how they download as well as to what.
cat >"$TMPROOT/tools/curl" <<EOF
#!/bin/sh
printf '%s\n' "\$*" >>"$TMPROOT/curl.log"
exec "$(command -v curl)" "\$@"
EOF
chmod +x "$TMPROOT/tools/cc" "$TMPROOT/tools/curl-config" "$TMPROOT/tools/curl"
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

# hashtool <dir> <fails|garbles> [pattern]: a sha256sum in <dir> standing in for the machine's, and found before it
# on a PATH that puts <dir> first. Over a file matching the pattern (every file without one) it fails having printed
# nothing, or prints a line that is no SHA-256 and answers 0; over any other file it is the machine's own. Both
# scripts read sha256sum first, so each case holds them to a hash that did not give a digest.
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

# curlhome <dir>: a curl home whose .curlrc has every transfer write its status code to curl's stdout as it ends
# (`write-out`, which reads 000 for file://), as a developer's own may; prints <dir>, for a case's CURL_HOME. Both
# scripts keep a person's curlrc, their proxy and their CA bundle, where they really download, so a case holds them
# to keeping whatever it makes curl write out of what they print.
curlhome() {
	mkdir -p "$1"
	printf '%s\n' 'write-out = "%{http_code}\n"' >"$1/.curlrc"
	echo "$1"
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

# scripts <case>: a scripts directory for the case — the two scripts copied from this tree, byte for byte, so the
# recipe's digest (its cache key) is the tree's — printing its path. The case writes the pin and the key beside them.
scripts() {
	local dir="$TMPROOT/$1/scripts"
	mkdir -p "$dir"
	cp "$SCRIPT_DIR/git.sh" "$SCRIPT_DIR/git-pin-check.sh" "$dir/"
	echo "$dir"
}

# pin <dir> <node> <node-sha> <node-floor> <node-floor-sha> <client-floor> <client-floor-sha>
pin() {
	cat >"$1/git-pin.env" <<EOF
# a pin file a case wrote
GIT_NODE_VERSION=$2
GIT_NODE_SHA256=$3
GIT_NODE_FLOOR_VERSION=$4
GIT_NODE_FLOOR_SHA256=$5
GIT_CLIENT_FLOOR_VERSION=$6
GIT_CLIENT_FLOOR_SHA256=$7
EOF
}

zero=0000000000000000000000000000000000000000000000000000000000000000

# ============================================================================================================
# scripts/git.sh
# ============================================================================================================

# standin <dir> <version> [control-dir]: writes <dir>/git-<version>.tar.xz, a source tree whose `make install`
# writes a bin/git answering `git version <version>` under DESTDIR and prefix, beside a COPYING; prints its SHA-256.
# With a control directory, an install that finds <control>/hold there first touches <control>/arrived and waits for
# <control>/go, so a case can hold a build in the middle of its install for as long as it looks around — a case
# decides the order builds finish in rather than hoping a sleep does.
standin() {
	local dir="$1" version="$2" ctl="${3-}"
	local src="$TMPROOT/src-$version-$RANDOM"
	mkdir -p "$src/git-$version" "$dir"
	echo "the stand-in's licence, version $version" >"$src/git-$version/COPYING"
	{
		printf 'install:\n'
		if [ -n "$ctl" ]; then
			printf "\t@if [ -e '%s/hold' ]; then touch '%s/arrived'; while [ ! -e '%s/go' ]; do sleep 0.05; done; fi\n" \
				"$ctl" "$ctl" "$ctl"
		fi
		printf "\tmkdir -p '\$(DESTDIR)\$(prefix)/bin'\n"
		printf "\techo '#!/bin/sh' >'\$(DESTDIR)\$(prefix)/bin/git'\n"
		printf "\techo 'echo \"git version %s\"' >>'\$(DESTDIR)\$(prefix)/bin/git'\n" "$version"
		printf "\tchmod 755 '\$(DESTDIR)\$(prefix)/bin/git'\n"
	} >"$src/git-$version/Makefile"
	tar -cf - -C "$src" "git-$version" | xz -c >"$dir/git-$version.tar.xz"
	rm -rf "$src"
	digest "$dir/git-$version.tar.xz"
}

recipe="$(digest "$SCRIPT_DIR/git.sh" | cut -c1-12)"

# --- what the recipe refuses before it does anything ----------------------------------------------------------

s="$(scripts unknown)"
pin "$s" 2.56.0 "$zero" 2.55.0 "$zero" 2.40.4 "$zero"
run "$s/git.sh" nodes
expect "git.sh refuses a pin it does not know, with its usage" 2 "" "usage: scripts/git.sh node|node-floor|client-floor"

s="$(scripts short-sha)"
pin "$s" 2.56.0 "${zero:1}" 2.55.0 "$zero" 2.40.4 "$zero"
run "$s/git.sh" node
expect "git.sh refuses a pin whose SHA-256 is 63 digits, naming the variable" 2 "" "GIT_NODE_SHA256"

# --- a build is reused only when it is whole ------------------------------------------------------------------

# plant <cache> <version> [complete]: plants a build of <version> at the name the recipe looks for.
plant() {
	local build="$1/$2-$recipe"
	mkdir -p "$build/bin"
	printf '#!/bin/sh\necho "git version %s"\n' "$2" >"$build/bin/git"
	chmod 755 "$build/bin/git"
	[ "${3-}" != complete ] || : >"$build/.complete"
	echo "$build"
}

s="$(scripts intact)"
pin "$s" 2.56.0 "$zero" 2.55.0 "$zero" 2.40.4 "$zero"
build="$(plant "$TMPROOT/intact/cache" 2.56.0 complete)"
run env CREWLET_GIT_CACHE="$TMPROOT/intact/cache" GIT_PIN_DOWNLOADS="file://$TMPROOT/intact/nowhere" "$s/git.sh" node
expect "git.sh hands back an intact build and downloads nothing" 0 "$build/bin"
[ "$out" = "$build/bin" ] || bad "git.sh prints the build's bin and nothing else on stdout" "stdout: $out"

s="$(scripts incomplete)"
pin "$s" 2.56.0 "$zero" 2.55.0 "$zero" 2.40.4 "$zero"
plant "$TMPROOT/incomplete/cache" 2.56.0 >/dev/null
served="$(standin "$TMPROOT/incomplete/downloads" 2.56.0)"
: >"$TMPROOT/curl.log"
run env CREWLET_GIT_CACHE="$TMPROOT/incomplete/cache" GIT_PIN_DOWNLOADS="file://$TMPROOT/incomplete/downloads" "$s/git.sh" node
expect "git.sh rebuilds a build without .complete, and refuses a tarball that is not the pin's" 1 "" "hashes to $served"
expect "git.sh names both digests when the tarball is not the pin's" 1 "" "pins $zero"
# Mutation: drop the stall bound from the recipe's download -> red.
bounded "every download the recipe makes ends when it stalls"

# --- a hash that gives no SHA-256 is a refusal, never a digest -------------------------------------------------

# The tarball's: a hash that fails over it, having printed nothing, leaves a download the recipe cannot check (1),
# never a digest of nothing compared against the pin — the status of `sha256sum FILE | cut` is cut's, so the
# recipe's hash once answered 0 and an empty string. Mutation: let digest print whatever its hash printed -> "hashes
# to , and ... pins" (red).
s="$(scripts hashfails)"
sha="$(standin "$TMPROOT/hashfails/downloads" 2.56.0)"
pin "$s" 2.56.0 "$sha" 2.55.0 "$zero" 2.40.4 "$zero"
hashtool "$TMPROOT/hashfails/tools" fails '*.tar.xz'
run env PATH="$TMPROOT/hashfails/tools:$PATH" CREWLET_GIT_CACHE="$TMPROOT/hashfails/cache" \
	GIT_PIN_DOWNLOADS="file://$TMPROOT/hashfails/downloads" "$s/git.sh" node
expect "git.sh refuses a tarball whose SHA-256 the machine cannot compute (1)" 1 "" \
	"cannot compute the SHA-256 of git-2.56.0.tar.xz"

# The recipe's own: a hash that prints no digest names no build, so the recipe refuses before it downloads (2) —
# rather than look for a build at "<version>-", download, and fail at the tarball. Mutation: as above -> exit 1
# after a download (red).
s="$(scripts hashnone)"
pin "$s" 2.56.0 "$sha" 2.55.0 "$zero" 2.40.4 "$zero"
hashtool "$TMPROOT/hashnone/tools" garbles
: >"$TMPROOT/curl.log"
run env PATH="$TMPROOT/hashnone/tools:$PATH" CREWLET_GIT_CACHE="$TMPROOT/hashnone/cache" \
	GIT_PIN_DOWNLOADS="file://$TMPROOT/hashfails/downloads" "$s/git.sh" node
expect "git.sh refuses to build where it cannot compute its own SHA-256 (2)" 2 "" "cannot compute the SHA-256 of"
if [ -s "$TMPROOT/curl.log" ]; then
	bad "git.sh downloads nothing where it cannot compute its own SHA-256" "$(cat "$TMPROOT/curl.log")"
else
	ok "git.sh downloads nothing where it cannot compute its own SHA-256"
fi

# --- a curl configuration changes nothing the recipe prints ------------------------------------------------------

# The recipe's stdout is the bin directory and nothing else, because the one-package loop puts it first on PATH
# (`bin=$(scripts/git.sh node) && PATH="$bin:$PATH" go test …`): a `write-out` in a person's curlrc once put a 000
# there above the directory, on every build that downloaded, and the loop then ran on the machine's git. A build
# under such a curlrc must print the directory alone, and what the curlrc wrote on stderr (the control: the
# configuration was read, and kept). Mutation: let the recipe's download write curl's stdout to its own -> red.
s="$(scripts curlrc-recipe)"
sha="$(standin "$TMPROOT/curlrc-recipe/downloads" 2.56.0)"
pin "$s" 2.56.0 "$sha" 2.55.0 "$zero" 2.40.4 "$zero"
run env CURL_HOME="$(curlhome "$TMPROOT/curlrc-recipe/curl")" CREWLET_GIT_CACHE="$TMPROOT/curlrc-recipe/cache" \
	GIT_PIN_DOWNLOADS="file://$TMPROOT/curlrc-recipe/downloads" "$s/git.sh" node
why=()
[ "$status" = 0 ] || why+=("exit $status, want 0")
[ "$out" = "$TMPROOT/curlrc-recipe/cache/2.56.0-$recipe/bin" ] ||
	why+=("its stdout is not $TMPROOT/curlrc-recipe/cache/2.56.0-$recipe/bin alone")
grep -qx 000 <<<"$err" || why+=("stderr lacks the curlrc's write-out, 000: the configuration was not read")
if [ ${#why[@]} -eq 0 ]; then
	ok "a curlrc that writes to curl's stdout changes nothing the recipe prints"
else
	bad "a curlrc that writes to curl's stdout changes nothing the recipe prints" "${why[@]}" \
		"stdout:" "$out" "stderr:" "$err"
fi

# --- two builders at once publish one build, and neither deletes the other's git -----------------------------

# Two `git.sh node` runs over one cache, the shape of two worktrees' `make test` starting together: B finds no
# build and starts one, and its download is held (the tarball it reads is a pipe the case writes when it chooses);
# meanwhile A finds no build either, builds and publishes. Then B's download arrives and B is held again in the
# middle of its install — and the git A printed must still answer, because B builds in a directory of its own.
# Released, B finds the name published, keeps that build and removes its own: both print the same bin, it answers,
# and one build directory remains. Mutation: build in place at the published name, emptied before the install (the
# recipe this replaced) -> B's emptying deletes the git A printed while B is still building (red).
s="$(scripts concurrent)"
ctl="$TMPROOT/concurrent/ctl"
mkdir -p "$ctl" "$TMPROOT/concurrent/held"
sha="$(standin "$TMPROOT/concurrent/downloads" 2.56.0 "$ctl")"
pin "$s" 2.56.0 "$sha" 2.55.0 "$zero" 2.40.4 "$zero"
mkfifo "$TMPROOT/concurrent/held/git-2.56.0.tar.xz"
builder() { # <name> <downloads>: one `git.sh node` in the background, its outcome in <ctl>/<name>.{out,err,status}
	(
		CREWLET_GIT_CACHE="$TMPROOT/concurrent/cache" GIT_PIN_DOWNLOADS="file://$2" \
			"$s/git.sh" node >"$ctl/$1.out" 2>"$ctl/$1.err" && echo 0 >"$ctl/$1.status" || echo $? >"$ctl/$1.status"
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
answers() { [ "$("$1/git" version 2>&1 || true)" = "git version 2.56.0" ]; }
started() { grep -q 'building git 2.56.0' "$ctl/b.err" 2>/dev/null; }
concurrent() {
	builder b "$TMPROOT/concurrent/held"
	await 60 started || { bad "the held builder starts its build" "$(cat "$ctl/b.err")"; return; }
	builder a "$TMPROOT/concurrent/downloads"
	await 60 test -e "$ctl/a.status" || { bad "the other builder finishes" "$(cat "$ctl/a.err")"; return; }
	local abin
	abin="$(cat "$ctl/a.out")"
	if [ "$(cat "$ctl/a.status")" != 0 ] || ! answers "$abin"; then
		bad "the other builder publishes a git that answers" "$(cat "$ctl/a.err")"
		return
	fi
	touch "$ctl/hold"
	cat "$TMPROOT/concurrent/downloads/git-2.56.0.tar.xz" >"$TMPROOT/concurrent/held/git-2.56.0.tar.xz" &
	await 60 test -e "$ctl/arrived" || { bad "the held builder reaches its install" "$(cat "$ctl/b.err")"; return; }
	if [ ! -e "$ctl/b.status" ] && answers "$abin"; then
		ok "the git one builder published answers while another is still building"
	else
		bad "the git one builder published answers while another is still building" \
			"$abin/git answers: $("$abin/git" version 2>&1 || true)"
	fi
	touch "$ctl/go"
	await 60 test -e "$ctl/b.status" || { bad "the held builder finishes" "$(cat "$ctl/b.err")"; return; }
	local builds
	builds="$(find "$TMPROOT/concurrent/cache" -mindepth 1 -maxdepth 1 -type d -name "2.56.0-$recipe.*" | wc -l | tr -d ' ')"
	if [ "$(cat "$ctl/b.status")" = 0 ] && [ "$(cat "$ctl/b.out")" = "$abin" ] && answers "$abin" && [ "$builds" = 1 ]; then
		ok "two builders at once both succeed, print one bin and leave one build"
	else
		bad "two builders at once both succeed, print one bin and leave one build" \
			"exit $(cat "$ctl/b.status"); printed $(cat "$ctl/b.out") and $abin; build directories: $builds" \
			"$(cat "$ctl/b.err")"
	fi
}
concurrent

# --- a build at a prefix carries what redistributing it owes ---------------------------------------------------

# The image's build: git's licence, the source tarball checked against the pin and the recipe beside it, and a
# SOURCE file naming the version, the release's URL and its SHA-256 (RELEASING.md; the source offer is the tarball).
s="$(scripts prefix)"
sha="$(standin "$TMPROOT/prefix/downloads" 2.56.0)"
pin "$s" 2.56.0 "$sha" 2.55.0 "$zero" 2.40.4 "$zero"
run env GIT_PIN_PREFIX=/usr/local DESTDIR="$TMPROOT/prefix/stage" GIT_PIN_DOWNLOADS="file://$TMPROOT/prefix/downloads" "$s/git.sh" node
doc="$TMPROOT/prefix/stage/usr/local/share/doc/git"
expect "git.sh installs at a prefix under DESTDIR and prints its bin" 0 "$TMPROOT/prefix/stage/usr/local/bin"
why=()
grep -qx "the stand-in's licence, version 2.56.0" "$doc/COPYING" 2>/dev/null || why+=("no COPYING from the tarball")
for want in "git 2.56.0" "https://www.kernel.org/pub/software/scm/git/git-2.56.0.tar.xz" "SHA-256: $sha"; do
	grep -qF -- "$want" "$doc/SOURCE" 2>/dev/null || why+=("SOURCE lacks: $want")
done
[ "$(digest "$doc/git-2.56.0.tar.xz" 2>/dev/null)" = "$sha" ] || why+=("the tarball beside SOURCE is not the pin's")
cmp -s "$doc/git.sh" "$SCRIPT_DIR/git.sh" || why+=("the recipe beside SOURCE is not this recipe")
if [ ${#why[@]} -eq 0 ]; then
	ok "a prefix build carries git's licence, its source tarball, the recipe and a SOURCE naming them"
else
	bad "a prefix build carries git's licence, its source tarball, the recipe and a SOURCE naming them" "${why[@]}"
fi

# ============================================================================================================
# scripts/git-pin-check.sh
# ============================================================================================================

# THE KEYS, made once for the suite in a GnuPG home of its own: each a primary that only certifies and a subkey
# that signs, as the maintainer's release key is shaped.
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

pinned_key="$(newkey pinned never)"
other_key="$(newkey other never)"
expired_key="$(newkey expired 1d never 20200101T000000)"

# release <dir> <version> <signer fpr> [faked time]: a stand-in release tarball and its signature over the
# decompressed tar, as kernel.org publishes them; prints the tarball's SHA-256.
release() {
	local dir="$1" version="$2" signer="$3" faked=()
	[ -z "${4-}" ] || faked=(--faked-system-time "$4!")
	mkdir -p "$dir"
	local src="$TMPROOT/rel-$RANDOM"
	mkdir -p "$src/git-$version"
	echo "release $version" >"$src/git-$version/README"
	tar -cf - -C "$src" "git-$version" | xz -c >"$dir/git-$version.tar.xz"
	rm -rf "$src"
	xz -dc "$dir/git-$version.tar.xz" | sgpg ${faked[@]+"${faked[@]}"} --local-user "$signer" --detach-sign -o "$dir/git-$version.tar.sign" - 2>/dev/null
	digest "$dir/git-$version.tar.xz"
}

# listing <file> <version>...: a kernel.org-shaped index naming each release's tarball, signature and gzip, beside
# what the check must not take for a release: release candidates, the docs tarballs, a four-number release.
listing() {
	local file="$1"
	shift
	{
		echo '<html><head><title>Index of /pub/software/scm/git/</title></head><body><pre>'
		echo '<a href="testing/">testing/</a>'
		echo '<a href="git-1.8.2.3.tar.xz">git-1.8.2.3.tar.xz</a>'
		local v
		for v in "$@"; do
			echo "<a href=\"git-$v.tar.gz\">git-$v.tar.gz</a>   28-Sep-2026 16:01     12M"
			echo "<a href=\"git-$v.tar.sign\">git-$v.tar.sign</a> 28-Sep-2026 16:01     566"
			echo "<a href=\"git-$v.tar.xz\">git-$v.tar.xz</a>   28-Sep-2026 16:01      8M"
			echo "<a href=\"git-htmldocs-$v.tar.xz\">git-htmldocs-$v.tar.xz</a>"
		done
		echo '</pre></body></html>'
	} >"$file"
}

current=(2.39.5 2.40.0 2.40.4 2.54.0 2.55.0 2.56.0)

# checkcase <case> <key file content...>: a scripts directory pinned at node 2.56.0, node floor 2.55.0 and client
# floor 2.40.4, with the named keys exported as its key file; prints its path.
checkcase() {
	local s
	s="$(scripts "$1")"
	shift
	pin "$s" 2.56.0 "$zero" 2.55.0 "$zero" 2.40.4 "$zero"
	export_key "$@" >"$s/git-signing-key.asc"
	echo "$s"
}

# pins <case dir> <scripts dir>: runs the check against the case's listing and downloads.
pins() {
	run env GIT_PIN_LISTING="file://$1/listing.html" GIT_PIN_DOWNLOADS="file://$1/downloads" \
		GIT_PIN_FINGERPRINT="${fingerprint:-$pinned_key}" "$2/git-pin-check.sh"
}

c="$TMPROOT/current"
s="$(checkcase current "$pinned_key")"
listing "$c/listing.html" "${current[@]}"
pins "$c" "$s"
expect "every pin the newest of its series -> 0" 0 "node 2.56.0, node-floor 2.55.0, client-floor 2.40.4"

c="$TMPROOT/rc"
s="$(checkcase rc "$pinned_key")"
listing "$c/listing.html" "${current[@]}" 2.57.0.rc1
echo '<a href="git-2.57.0-rc2.tar.xz">git-2.57.0-rc2.tar.xz</a>' >>"$c/listing.html"
pins "$c" "$s"
expect "a release candidate newer than everything is not a release -> 0" 0 "every pin is the newest"

c="$TMPROOT/patch"
s="$(checkcase patch "$pinned_key")"
listing "$c/listing.html" "${current[@]}" 2.56.1
sha="$(release "$c/downloads" 2.56.1 "$pinned_key")"
: >"$TMPROOT/curl.log"
pins "$c" "$s"
# Mutation: drop the stall bound from the check's fetch -> red.
bounded "every download the check makes ends when it stalls"
expect "a newer patch of the node pin's series -> 1 with its version" 1 "GIT_NODE_VERSION=2.56.1"
expect "a newer patch of the node pin's series -> 1 with the SHA-256 of the tarball it verified" 1 "GIT_NODE_SHA256=$sha"
expect "a node pin behind names its pull request" 1 "\`build(deps): bump git from 2.56.0 to 2.56.1\`"
if grep -q 'FLOOR' <<<"$out"; then bad "a newer node patch moves no floor" "$out"; else ok "a newer node patch moves no floor"; fi

# A CURL CONFIGURATION CHANGES NOTHING THE CHECK PRINTS. Its downloads run inside the command substitution that takes
# a release's digest, so what a curlrc makes curl write to stdout — a `write-out` — was taken with it: the report
# printed GIT_NODE_SHA256=000, then 000, then the tarball's digest, under its word that the SHA-256 is that tarball's,
# and a 000 above the report from the listing's download. The same run is asked again under such a curlrc: its stdout
# must be the report the run without one printed, its pin's line the tarball's digest alone, and what the curlrc
# wrote on stderr (the control: the configuration was read, and kept). Mutation: let the check's fetch write curl's
# stdout to its own -> red.
c="$TMPROOT/curlrc-check"
s="$(checkcase curlrc-check "$pinned_key")"
listing "$c/listing.html" "${current[@]}" 2.56.1
sha="$(release "$c/downloads" 2.56.1 "$pinned_key")"
pins "$c" "$s"
plain="$out"
CURL_HOME="$(curlhome "$c/curl")" pins "$c" "$s"
why=()
[ "$status" = 1 ] || why+=("exit $status, want 1")
[ "$(grep '^GIT_NODE_SHA256=' <<<"$out" || true)" = "GIT_NODE_SHA256=$sha" ] ||
	why+=("its pin's SHA-256 line is not GIT_NODE_SHA256=$sha alone")
[ "$out" = "$plain" ] || why+=("its stdout is not the report a run without the curlrc printed:" "$plain")
grep -qx 000 <<<"$err" || why+=("stderr lacks the curlrc's write-out, 000: the configuration was not read")
if [ ${#why[@]} -eq 0 ]; then
	ok "a curlrc that writes to curl's stdout changes nothing the check prints"
else
	bad "a curlrc that writes to curl's stdout changes nothing the check prints" "${why[@]}" \
		"stdout:" "$out" "stderr:" "$err"
fi

c="$TMPROOT/feature"
s="$(checkcase feature "$pinned_key")"
listing "$c/listing.html" "${current[@]}" 2.57.0
release "$c/downloads" 2.57.0 "$pinned_key" >/dev/null
pins "$c" "$s"
expect "a newer feature release -> 1" 1 "GIT_NODE_VERSION=2.57.0"
if grep -q 'FLOOR\|Rust' <<<"$out"; then bad "a newer feature release moves no floor and needs no Rust" "$out"; else ok "a newer feature release moves no floor and needs no Rust"; fi

c="$TMPROOT/client"
s="$(checkcase client "$pinned_key")"
listing "$c/listing.html" "${current[@]}" 2.40.5 2.41.0
sha="$(release "$c/downloads" 2.40.5 "$pinned_key")"
pins "$c" "$s"
expect "a newer patch of the client floor's series -> 1, its next series no floor" 1 "GIT_CLIENT_FLOOR_SHA256=$sha"
if grep -q 'GIT_NODE' <<<"$out" || ! grep -q 'GIT_CLIENT_FLOOR_VERSION=2.40.5' <<<"$out"; then
	bad "a newer client-floor patch moves that pin alone" "$out"
else
	ok "a newer client-floor patch moves that pin alone"
fi

c="$TMPROOT/nodefloor"
s="$(checkcase nodefloor "$pinned_key")"
listing "$c/listing.html" "${current[@]}" 2.55.1
release "$c/downloads" 2.55.1 "$pinned_key" >/dev/null
pins "$c" "$s"
expect "a newer patch of the node floor's series -> 1 for that pin alone" 1 "GIT_NODE_FLOOR_VERSION=2.55.1"
if grep -q 'GIT_NODE_VERSION\|GIT_CLIENT' <<<"$out"; then bad "a newer node-floor patch moves that pin alone" "$out"; else ok "a newer node-floor patch moves that pin alone"; fi

c="$TMPROOT/major"
s="$(checkcase major "$pinned_key")"
listing "$c/listing.html" "${current[@]}" 3.0.0
release "$c/downloads" 3.0.0 "$pinned_key" >/dev/null
pins "$c" "$s"
expect "a newer major -> 1 and the recipe's Rust change named" 1 "git 3 needs Rust to build"

# --- what the check can never call a pass ------------------------------------------------------------------------

c="$TMPROOT/otherkey"
s="$(checkcase otherkey "$pinned_key")"
listing "$c/listing.html" "${current[@]}" 2.56.1
release "$c/downloads" 2.56.1 "$other_key" >/dev/null
pins "$c" "$s"
expect "a signature by another key -> 2" 2 "" "a key scripts/git-signing-key.asc does not hold"
expect "a signature by another key -> 2, saying the file may be the one that is stale" 2 "" \
	"or a signing subkey of $pinned_key newer than the file"

# A release signed by a signing SUBKEY the key file does not hold yet — the maintainer added one under the same
# primary after the file was exported — is the same refusal, and gpg can name the subkey (NO_PUBKEY) but not whose
# it is: so the refusal says the file may be stale and prints how to refresh it, rather than reading as a forged
# signature. Mutation: drop the refresh steps from that refusal -> red.
rotated_key="$(newkey rotated never)"
c="$TMPROOT/rotated"
s="$(checkcase rotated "$rotated_key")"
sgpg --quick-add-key "$rotated_key" ed25519 sign never 2>/dev/null
newer_subkey="$(sgpg --with-colons --list-keys "$rotated_key" 2>/dev/null |
	awk -F: '$1 == "sub" { sub_ = 1; next } sub_ && $1 == "fpr" { fpr = $10; sub_ = 0 } END { print fpr }')"
listing "$c/listing.html" "${current[@]}" 2.56.1
release "$c/downloads" 2.56.1 "$newer_subkey!" >/dev/null
fingerprint="$rotated_key" pins "$c" "$s"
expect "a release signed by a subkey of the pinned key newer than the key file -> 2, naming the refresh" 2 "" \
	"To refresh it"

# The key file is held to the pinned fingerprint before any signature is read. Mutation: skip that comparison ->
# the other key, imported from the file, verifies its own signature and the pin reads as behind (exit 1, red).
c="$TMPROOT/otherfile"
s="$(checkcase otherfile "$other_key")"
listing "$c/listing.html" "${current[@]}" 2.56.1
release "$c/downloads" 2.56.1 "$other_key" >/dev/null
pins "$c" "$s"
expect "a key file that is not the pinned key -> 2" 2 "" "primary fingerprint is $pinned_key"

# A key added to the file beside the pinned one is not trusted with it. Mutation: count no primaries -> the
# added key's signature verifies (exit 1, red).
c="$TMPROOT/addedkey"
s="$(checkcase addedkey "$pinned_key" "$other_key")"
listing "$c/listing.html" "${current[@]}" 2.56.1
release "$c/downloads" 2.56.1 "$other_key" >/dev/null
pins "$c" "$s"
expect "a key file holding a second key beside the pinned one -> 2" 2 "" "holds 2 primary key(s)"

# gpg answers 0 for a good signature by a key that has since expired (EXPKEYSIG), so the check reads the status
# rather than the exit code. Mutation: accept gpg's exit status alone -> the release reads as verified (exit 1, red).
c="$TMPROOT/expiredsig"
s="$(checkcase expiredsig "$expired_key")"
listing "$c/listing.html" "${current[@]}" 2.56.1
release "$c/downloads" 2.56.1 "$expired_key" 20200101T120000 >/dev/null
fingerprint="$expired_key" pins "$c" "$s"
expect "a signature by a key that has since expired -> 2, naming the refresh" 2 "" "To refresh it"

c="$TMPROOT/tampered"
s="$(checkcase tampered "$pinned_key")"
listing "$c/listing.html" "${current[@]}" 2.56.1
release "$c/downloads" 2.56.1 "$pinned_key" >/dev/null
echo "a tarball somebody replaced" | xz -c >"$c/downloads/git-2.56.1.tar.xz"
pins "$c" "$s"
expect "a tarball that is not what was signed -> 2" 2 "" "does not verify"

c="$TMPROOT/nolisting"
s="$(checkcase nolisting "$pinned_key")"
pins "$c" "$s"
expect "a listing that does not load -> 2" 2 "" "cannot read the release listing"

c="$TMPROOT/noseries"
s="$(checkcase noseries "$pinned_key")"
listing "$c/listing.html" 2.39.5 2.55.0 2.56.0
pins "$c" "$s"
expect "a listing naming no release of a pinned series -> 2" 2 "" "no release of client-floor's series, 2.40"

c="$TMPROOT/stale"
s="$(checkcase stale "$pinned_key")"
listing "$c/listing.html" 2.40.4 2.55.0
pins "$c" "$s"
expect "a pin newer than the listing's newest -> 2" 2 "" "that listing is behind upstream"

c="$TMPROOT/nodownload"
s="$(checkcase nodownload "$pinned_key")"
listing "$c/listing.html" "${current[@]}" 2.56.1
pins "$c" "$s"
expect "a release the listing names and the downloads lack -> 2" 2 "" "cannot download"

# A hash that gives no digest is "cannot tell", never a pin's new line: printed as GIT_NODE_SHA256= under a report
# saying it is the SHA-256 of the tarball whose signature verified, a blank or partial digest is the one line of the
# issue a person pastes. Mutation: write whatever the hash printed -> exit 1 with a GIT_NODE_SHA256 that is no digest
# (red).
for mode in fails garbles; do
	c="$TMPROOT/hash$mode-check"
	s="$(checkcase "hash$mode-check" "$pinned_key")"
	listing "$c/listing.html" "${current[@]}" 2.56.1
	release "$c/downloads" 2.56.1 "$pinned_key" >/dev/null
	hashtool "$c/tools" "$mode"
	PATH="$c/tools:$PATH" pins "$c" "$s"
	expect "a hash that $mode over a release's tarball -> 2" 2 "" "cannot compute the SHA-256 of git-2.56.1.tar.xz"
done

# THE CALLER'S GNUPG HOME IS NEITHER READ NOR WRITTEN. The caller's home holds the other key, and the release is
# signed by it: read, it would verify; written, the import of the key file would land in it. Mutation: let the check
# use the GNUPGHOME it inherits -> the sentinel's keyring changes (red).
c="$TMPROOT/callerhome"
s="$(checkcase callerhome "$pinned_key")"
listing "$c/listing.html" "${current[@]}" 2.56.1
release "$c/downloads" 2.56.1 "$other_key" >/dev/null
caller="$TMPROOT/callerhome/gnupg"
mkdir -p "$TMPROOT/callerhome"
mkdir -m 700 "$caller"
GPGHOMES+=("$caller")
export_key "$other_key" | GNUPGHOME="$caller" gpg --batch --quiet --import 2>/dev/null
snapshot() { find "$caller" -type f ! -name 'S.*' ! -name '*.lock' ! -name 'random_seed' -exec cksum {} + | LC_ALL=C sort; }
before="$(snapshot)"
run env GNUPGHOME="$caller" GIT_PIN_LISTING="file://$c/listing.html" GIT_PIN_DOWNLOADS="file://$c/downloads" \
	GIT_PIN_FINGERPRINT="$pinned_key" "$s/git-pin-check.sh"
after="$(snapshot)"
if [ "$status" = 2 ] && [ "$before" = "$after" ]; then
	ok "the check reads and writes nothing of the caller's GnuPG home"
else
	bad "the check reads and writes nothing of the caller's GnuPG home" "exit $status, want 2" \
		"the caller's home changed: $([ "$before" = "$after" ] && echo no || echo yes)" "$err"
fi

# --- the key's own expiry ------------------------------------------------------------------------------------------

soon_key="$(newkey soon 30d)"
later_key="$(newkey later 400d)"
bounded_key="$(newkey bounded 400d 30d)"

keycase() { # <case> <fpr>: runs `key` over a key file holding that key
	local s
	s="$(scripts "$1")"
	pin "$s" 2.56.0 "$zero" 2.55.0 "$zero" 2.40.4 "$zero"
	export_key "$2" >"$s/git-signing-key.asc"
	run env GIT_PIN_FINGERPRINT="$2" "$s/git-pin-check.sh" key
}

# The primary certifies and never expires; the subkey signs and does. Mutation: read the primary's expiry alone
# -> "never", exit 0 (red).
keycase soon "$soon_key"
expect "key: a signing subkey expiring in 30 days -> 1, naming the refresh" 1 "" "To refresh it"
keycase later "$later_key"
expect "key: a signing subkey expiring in 400 days -> 0" 0 "days from now"

# THE CHECK FINDS ITS OWN DIRECTORY WHATEVER CDPATH HOLDS. Run by a path relative to the tree, as the workflow and
# a maintainer run it, its `cd scripts` searched an exported CDPATH's entries first, and one holding a scripts/ of
# its own sent the check to that directory's pin and key file. Mutation: drop the check's `unset CDPATH` -> it reads
# the decoy's empty scripts/, cannot import a key file there, and answers 2 (red).
s="$(scripts cdpath-check)"
pin "$s" 2.56.0 "$zero" 2.55.0 "$zero" 2.40.4 "$zero"
export_key "$later_key" >"$s/git-signing-key.asc"
mkdir -p "$TMPROOT/cdpath-check/decoy/scripts"
run within "$TMPROOT/cdpath-check" env CDPATH="$TMPROOT/cdpath-check/decoy" GIT_PIN_FINGERPRINT="$later_key" \
	scripts/git-pin-check.sh key
expect "key: the check run by a relative path reads its own directory whatever CDPATH holds -> 0" 0 "days from now"
# Mutation: ignore the primary's own expiry -> the subkey's 400 days, exit 0 (red).
keycase bounded "$bounded_key"
expect "key: a primary expiring in 30 days bounds the subkey that signs -> 1" 1 "" "within the 60-day warning"
keycase expired "$expired_key"
expect "key: a key whose every signing subkey has expired -> 1" 1 "" "no valid signing key left"

s="$(scripts nokey)"
pin "$s" 2.56.0 "$zero" 2.55.0 "$zero" 2.40.4 "$zero"
echo "not a key" >"$s/git-signing-key.asc"
run env GIT_PIN_FINGERPRINT="$pinned_key" "$s/git-pin-check.sh" key
expect "key: a key file that holds no key -> 2" 2 "" "cannot import"

# THE KEY THIS TREE SHIPS is the maintainer's, whole: `key` imports it against the fingerprint the script pins and
# answers 0 or 1 by the calendar, never 2 — which would mean the file is not that key, or holds another beside it.
run "$SCRIPT_DIR/git-pin-check.sh" key
if [ "$status" = 0 ] || [ "$status" = 1 ]; then
	ok "the key file this tree ships is the pinned maintainer's key"
else
	bad "the key file this tree ships is the pinned maintainer's key" "exit $status" "$err"
fi

# --- the suite reads nothing of the environment of the person running it ------------------------------------------

# This suite started again under a developer's environment at its most hostile: every knob the scripts read set to
# send a case somewhere else (GIT_PIN_PREFIX and DESTDIR into a sentinel directory, a GIT_PIN_TARGET_ARCH that is
# not this machine's, a GIT_PIN_FINGERPRINT no key has, listings and downloads that do not exist), a cache and a
# GnuPG home of the developer's own, and a curl configuration refusing every protocol but https — every download a
# case makes is file:// — in every place curl looks for one: $CURL_HOME, $XDG_CONFIG_HOME, $HOME and $HOME/.config,
# with and without the dot. And it is run as `make git-test` runs it, by a path relative to the tree, under a CDPATH
# whose one entry holds a scripts/ of its own, empty. The nested run must pass, having run its cases, and leave every
# file of that environment exactly as it found it.
#
# Mutation: drop the CURL_HOME unset, or the XDG_CONFIG_HOME or the HOME pin -> the nested run's downloads are
# refused (red). Mutation: drop the GIT_ scrub -> the nested run's cases build at the developer's prefix and read
# the developer's fingerprint (red). Mutation: drop the CDPATH unset -> the nested run finds its own directory in
# the decoy, has no scripts to copy from it, and fails at its first case (red).
if [ -z "${CREWLET_PIN_TEST_NESTED-}" ]; then
	dev="$TMPROOT/dev"
	mkdir -p "$dev/curl/.config" "$dev/config" "$dev/home/.config" "$dev/cache" "$dev/xdg-cache" "$dev/destdir" \
		"$dev/cdpath/$(basename "$SCRIPT_DIR")"
	mkdir -m 700 "$dev/gnupg"
	GPGHOMES+=("$dev/gnupg")
	export_key "$other_key" | GNUPGHOME="$dev/gnupg" gpg --batch --quiet --import 2>/dev/null
	for rc in "$dev/curl/.curlrc" "$dev/curl/curlrc" "$dev/curl/.config/curlrc" "$dev/curl/.config/.curlrc" \
		"$dev/config/.curlrc" "$dev/config/curlrc" \
		"$dev/home/.curlrc" "$dev/home/.config/curlrc" "$dev/home/.config/.curlrc"; do
		echo 'proto = "=https"' >"$rc"
	done
	case "$(uname -m)" in
	x86_64 | amd64) foreign=arm64 ;;
	*) foreign=amd64 ;;
	esac
	devsnapshot() { find "$dev" -type f ! -name 'S.*' ! -name '*.lock' ! -name 'random_seed' -exec cksum {} + | LC_ALL=C sort; }
	before="$(devsnapshot)"
	nested="$(within "$(dirname "$SCRIPT_DIR")" env CREWLET_PIN_TEST_NESTED=1 \
		CURL_HOME="$dev/curl" XDG_CONFIG_HOME="$dev/config" HOME="$dev/home" CDPATH="$dev/cdpath" \
		GNUPGHOME="$dev/gnupg" CREWLET_GIT_CACHE="$dev/cache" XDG_CACHE_HOME="$dev/xdg-cache" DESTDIR="$dev/destdir" \
		GIT_PIN_PREFIX="$dev/prefix" GIT_PIN_TARGET_ARCH="$foreign" GIT_PIN_FINGERPRINT="$zero" \
		GIT_PIN_LISTING="file://$dev/no-listing" GIT_PIN_DOWNLOADS="file://$dev/no-downloads" \
		"$(basename "$SCRIPT_DIR")/$(basename "${BASH_SOURCE[0]}")" 2>&1)" && status=0 || status=$?
	after="$(devsnapshot)"
	if [ "$status" -eq 0 ] && grep -q '^ok   every pin the newest of its series -> 0$' <<<"$nested" &&
		grep -q ' 0 failed$' <<<"$nested" && [ "$before" = "$after" ]; then
		ok "under a developer's environment the suite passes and touches nothing of it"
	else
		bad "under a developer's environment the suite passes and touches nothing of it" "exit $status" \
			"the environment's files changed: $([ "$before" = "$after" ] && echo no || echo yes)" "$nested"
	fi
fi

echo
echo "git-pin_test.sh: $pass passed, $fail failed"
[ "$fail" -eq 0 ]

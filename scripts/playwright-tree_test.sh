#!/usr/bin/env bash
#
# playwright-tree_test.sh — the own suite of the Playwright tree's recipe (scripts/playwright-tree.sh).
#
# No Go test reaches the recipe, and it regresses silently where it matters: a recipe that installs into the directory
# another checkout's suite reads its tree from, hands back a tree npm installed nothing into or left short, lets a
# developer's npm setting change what is installed, runs a package's script, waits on an npm that never ends, leaves
# one running once interrupted, or prints something above the path a make target exports as the suite's tree. So every
# case runs the REAL recipe — copied beside a manifest, a lock and an installed digest the case writes, and beside this
# tree's box helper modules, whose check it runs, because it reads them all beside itself — against a stand-in
# registry: node, serving stand-in packages on 127.0.0.1 and logging each request, under a lock the suite builds (each
# SHA-512 from node's crypto) on registry.npmjs.org's addresses, as the real lock's are, which npm sends to the
# registry a case names. Every other address goes to a proxy on a port nothing listens on, so no case, and no mutation
# of the recipe, reaches the network. npm runs through a recorder that logs its command line, so a case holds the
# recipe to how it runs npm as well as to what npm did. It needs bash, node, an npm the recipe's min_npm accepts, tar
# and a SHA-256 tool.
set -euo pipefail

# THE PERSON'S ENVIRONMENT, GONE before the first case: every variable npm reads a setting from, npm_config_* in any
# case, as npm reads them; the tree cache (CREWLET_PLAYWRIGHT_TREE_CACHE, XDG_CACHE_HOME); the proxies, which the
# suite's own npm settings replace below; and CDPATH. HOME and XDG_CONFIG_HOME are made the suite's own, and npm's
# global npmrc an empty file of the suite's, so no npmrc of the person's is read. The suite's last case runs the whole
# suite again, by a relative path, under a hostile environment holding every one of these and requires it green and the
# hostile files untouched.
for name in $(compgen -e); do
	case "$(printf '%s' "$name" | tr 'A-Z' 'a-z')" in
	npm_config_*) unset "$name" ;;
	esac
done
unset CREWLET_PLAYWRIGHT_TREE_CACHE XDG_CACHE_HOME CDPATH HTTP_PROXY HTTPS_PROXY ALL_PROXY NO_PROXY \
	http_proxy https_proxy all_proxy no_proxy

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

missing=""
for tool in node npm tar; do
	command -v "$tool" >/dev/null 2>&1 || missing="$missing $tool"
done
command -v sha256sum >/dev/null 2>&1 || command -v shasum >/dev/null 2>&1 || missing="$missing sha256sum-or-shasum"
if [ -n "$missing" ]; then
	echo "playwright-tree_test.sh needs:$missing — CONTRIBUTING.md, Development setup, says what to install" >&2
	exit 1
fi
real_npm="$(command -v npm)"
real_node="$(command -v node)"
real_sleep="$(command -v sleep)"

TMPROOT="$(mktemp -d)"
killtree() { # <pid>: the process and every process below it, children first
	local child
	for child in $(pgrep -P "$1" 2>/dev/null); do
		killtree "$child"
	done
	kill "$1" 2>/dev/null || true
}
cleanup() {
	# The registry, and an installer a failed case left running, are stopped with all they started before the
	# directories go.
	local pid
	for pid in $(jobs -p); do
		killtree "$pid"
	done
	wait 2>/dev/null || true
	rm -rf "$TMPROOT"
}
trap cleanup EXIT

export HOME="$TMPROOT/home" XDG_CONFIG_HOME="$TMPROOT/config"
mkdir -p "$HOME" "$XDG_CONFIG_HOME"
: >"$TMPROOT/global-npmrc"
# npm's own settings, the suite's: no global npmrc of the machine's, and every address but the stand-in's through a
# proxy on 127.0.0.1:1, a privileged port nothing the suite starts can listen on, so a request bound for the network is
# refused at once.
export npm_config_globalconfig="$TMPROOT/global-npmrc" npm_config_proxy="http://127.0.0.1:1" \
	npm_config_https_proxy="http://127.0.0.1:1" npm_config_noproxy="127.0.0.1"

# The suite's npm, first on PATH: the machine's, through a recorder.
mkdir -p "$TMPROOT/tools"
cat >"$TMPROOT/tools/npm" <<EOF
#!/bin/sh
printf '%s\n' "\$*" >>"$TMPROOT/npm.log"
exec "$real_npm" "\$@"
EOF
chmod +x "$TMPROOT/tools/npm"
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
verdict() { # <name> <why>...: ok when no why is given
	local name="$1"
	shift
	if [ $# -eq 0 ]; then ok "$name"; else bad "$name" "$@"; fi
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

# mapfile_why <cmd>...: adds each line the command prints to the caller's why.
mapfile_why() {
	local line
	while IFS= read -r line; do why+=("$line"); done < <("$@")
}

await() { # <seconds> <test>...: waits for the test to hold, failing after the seconds
	local deadline=$((SECONDS + $1))
	shift
	until "$@"; do
		[ "$SECONDS" -lt "$deadline" ] || return 1
		sleep 0.05
	done
}

# THE STAND-IN REGISTRY, one for the suite: it logs each request as "<method> <path>", serves a file under web/ by its
# path and answers 404 for anything else, and holds a request under /held/ until ctl/go exists, having marked
# ctl/arrived, then serves the rest of its path: the concurrent case's held installer names it as its registry.
cat >"$TMPROOT/registry.mjs" <<'EOF'
import { createServer } from "node:http";
import { appendFileSync, existsSync, readFileSync, renameSync, statSync, writeFileSync } from "node:fs";
import { join, normalize } from "node:path";
const [root, log, portFile, ctl] = process.argv.slice(2);
const serve = (path, res) => {
  const file = join(root, normalize(path));
  if (file.startsWith(root + "/") && existsSync(file) && statSync(file).isFile()) {
    const body = readFileSync(file);
    res.writeHead(200, { "content-type": "application/octet-stream", "content-length": body.length });
    return res.end(body);
  }
  res.writeHead(404, { "content-type": "application/json" });
  res.end('{"error":"Not found"}');
};
const hold = (path, res) => {
  if (!existsSync(join(ctl, "arrived"))) writeFileSync(join(ctl, "arrived"), "");
  if (existsSync(join(ctl, "go"))) return serve(path, res);
  setTimeout(() => hold(path, res), 20);
};
const server = createServer((req, res) => {
  const path = decodeURIComponent(new URL(req.url, "http://registry").pathname);
  appendFileSync(log, `${req.method} ${path}\n`);
  if (path.startsWith("/held/")) return hold(path.slice("/held".length), res);
  serve(path, res);
});
server.listen(0, "127.0.0.1", () => {
  writeFileSync(portFile + ".new", String(server.address().port));
  renameSync(portFile + ".new", portFile);
});
EOF
mkdir -p "$TMPROOT/web" "$TMPROOT/ctl"
: >"$TMPROOT/requests.log"
node "$TMPROOT/registry.mjs" "$TMPROOT/web" "$TMPROOT/requests.log" "$TMPROOT/port" "$TMPROOT/ctl" &
if ! await 30 test -s "$TMPROOT/port"; then
	echo "playwright-tree_test.sh: the stand-in registry did not start" >&2
	exit 1
fi
registry="http://127.0.0.1:$(cat "$TMPROOT/port")/"

# THE STAND-IN PACKAGES, shaped as the real lock's: a scoped package naming two others exactly, with a bin, which
# npm links unless told not to; one of them naming the third; and the third with an install script, which leaves a
# file in the package if it ever runs. Each one's files are also laid out under modules/whole as a whole install lays
# them out in node_modules.
standin() { # <name> <version> <json>: a tarball laid out as npm packs one, served at the registry's path for it
	local name="$1" version="$2" json="$3" base="${1##*/}" src="$TMPROOT/src-$RANDOM$RANDOM"
	mkdir -p "$src/package" "$TMPROOT/web/$name/-" "$TMPROOT/modules/whole/$name"
	printf '{"name": "%s", "version": "%s"%s}\n' "$name" "$version" "$json" >"$src/package/package.json"
	echo "module.exports = '$name';" >"$src/package/index.js"
	printf '#!/usr/bin/env node\n' >"$src/package/cli.js"
	tar -czf "$TMPROOT/web/$name/-/$base-$version.tgz" -C "$src" package
	cp "$src/package/"* "$TMPROOT/modules/whole/$name/"
	rm -rf "$src"
}
standin @standin/mcp 1.0.0 ', "dependencies": {"standin-runner": "2.0.0", "standin-core": "2.0.0"}, "bin": {"standin-mcp": "cli.js"}'
standin standin-runner 2.0.0 ', "dependencies": {"standin-core": "2.0.0"}'
standin standin-core 2.0.0 ', "scripts": {"install": "echo ran >ran-install-script"}'
tarballs="GET /@standin/mcp/-/mcp-1.0.0.tgz
GET /standin-core/-/standin-core-2.0.0.tgz
GET /standin-runner/-/standin-runner-2.0.0.tgz"

# THE INSTALLED DIGEST, computed here in bash and never by the box helper's module the recipe checks with, so every
# install holds that module to the definition as well as to the files npm installed: one line per file under
# node_modules, npm's own record aside, `<path>\0<hex SHA-256 of its bytes>\n`, the path relative and slash-separated,
# sorted by path in byte order, which is the lines' own order since NUL is the least byte; then the hex SHA-256 of
# those lines (boxhelper's package doc). Each case's installed.sha256 holds the stand-in packages' digest;
# other_installed is a digest of no install, as one left from before a bump of the lock is.
installed_digest() { # <node_modules>: prints the digest of the files under it
	(
		cd "$1"
		find . -type f ! -path ./.package-lock.json | sed 's|^\./||' | LC_ALL=C sort | while IFS= read -r file; do
			printf '%s\0%s\n' "$file" "$(digest "$file")"
		done
	) | digest -
}
standin_installed="$(installed_digest "$TMPROOT/modules/whole")"
other_installed="$(printf 'files no lock installs\n' | digest -)"

# lock.cjs <web> <tree> <shape>: the manifest and lock of those packages, written into <tree> as npm writes them.
# Shape "tampered" gives standin-core the SHA-512 of bytes no registry serves; "respaced" indents the same lock
# otherwise, so its bytes differ and its packages do not.
cat >"$TMPROOT/lock.cjs" <<'EOF'
const { createHash } = require("node:crypto");
const { mkdirSync, readFileSync, writeFileSync } = require("node:fs");
const [web, tree, shape] = process.argv.slice(2);
const sha512 = (bytes) => "sha512-" + createHash("sha512").update(bytes).digest("base64");
const entry = (name, version, extra) => {
  const path = `${name}/-/${name.split("/").pop()}-${version}.tgz`;
  return { version, resolved: `https://registry.npmjs.org/${path}`, integrity: sha512(readFileSync(`${web}/${path}`)), ...extra };
};
const packages = {
  "": { name: "crewlet-browser-tree", dependencies: { "@standin/mcp": "1.0.0" } },
  "node_modules/@standin/mcp": entry("@standin/mcp", "1.0.0", {
    dependencies: { "standin-core": "2.0.0", "standin-runner": "2.0.0" }, bin: { "standin-mcp": "cli.js" } }),
  "node_modules/standin-core": entry("standin-core", "2.0.0", { hasInstallScript: true }),
  "node_modules/standin-runner": entry("standin-runner", "2.0.0", { dependencies: { "standin-core": "2.0.0" } }),
};
if (shape === "tampered") packages["node_modules/standin-core"].integrity = sha512("bytes no registry serves");
const manifest = { name: "crewlet-browser-tree", private: true, dependencies: { "@standin/mcp": "1.0.0" } };
mkdirSync(tree, { recursive: true });
writeFileSync(`${tree}/package.json`, JSON.stringify(manifest, null, 2) + "\n");
const lock = { name: "crewlet-browser-tree", lockfileVersion: 3, requires: true, packages };
writeFileSync(`${tree}/package-lock.json`, JSON.stringify(lock, null, shape === "respaced" ? 4 : 2) + "\n");
EOF

# scripts <case> [shape]: the case's checkout — the recipe and the box helper's modules (js/) copied from this tree,
# byte for byte, beside the stand-in manifest and lock of that shape and the stand-in packages' installed digest —
# printing the recipe's path.
scripts() {
	local dir="$TMPROOT/$1"
	mkdir -p "$dir/scripts" "$dir/internal/browser/boxhelper"
	cp "$SCRIPT_DIR/playwright-tree.sh" "$dir/scripts/"
	cp -R "$SCRIPT_DIR/../internal/browser/boxhelper/js" "$dir/internal/browser/boxhelper/"
	node "$TMPROOT/lock.cjs" "$TMPROOT/web" "$dir/internal/browser/boxhelper/tree" "${2-whole}"
	printf '%s\n' "$standin_installed" >"$dir/internal/browser/boxhelper/tree/installed.sha256"
	echo "$dir/scripts/playwright-tree.sh"
}
lockdir() { echo "$TMPROOT/$1/internal/browser/boxhelper/tree"; }

# named <case>: the name the recipe publishes a tree of the case's manifest and lock under, by its layout. min_npm is
# the recipe's npm floor, which the floor's cases are read off.
layout="$(sed -n 's/^layout=\([0-9][0-9]*\)$/\1/p' "$SCRIPT_DIR/playwright-tree.sh")"
min_npm="$(sed -n 's/^min_npm=\([0-9][0-9]*\)$/\1/p' "$SCRIPT_DIR/playwright-tree.sh")"
if ! [[ "$layout" =~ ^[0-9]+$ && "$min_npm" =~ ^[0-9]+$ ]]; then
	echo "playwright-tree_test.sh: scripts/playwright-tree.sh sets no single layout=<number> and min_npm=<number> line" >&2
	exit 1
fi
named() {
	local dir
	dir="$(lockdir "$1")"
	printf '%s\n%s\n' "$(digest "$dir/package.json")" "$(digest "$dir/package-lock.json")" | digest - |
		cut -c1-12 | sed "s/\$/-l$layout/"
}

# install <case> [VAR=value]...: the case's recipe, run with its own tree cache against the stand-in registry and the
# environment given, the request and npm logs emptied first.
install() {
	local case_name="$1"
	shift
	: >"$TMPROOT/requests.log"
	: >"$TMPROOT/npm.log"
	run env CREWLET_PLAYWRIGHT_TREE_CACHE="$TMPROOT/$case_name/cache" npm_config_registry="$registry" "$@" \
		"$TMPROOT/$case_name/scripts/playwright-tree.sh"
}

# whole <case> <tree>: prints why <tree> is not the case's lock installed as the recipe installs it — the two files,
# node_modules holding exactly the three packages, npm-cache holding npm's cache alone, .complete naming the installed
# digest, and no link, log or trace of a script — or nothing. A <tree> that names no directory is said so before any
# walk: an installer that failed prints no path, and an empty one would walk the file system from its root.
whole() {
	local dir top modules
	dir="$(lockdir "$1")"
	if [ -z "$2" ] || [ ! -d "$2" ]; then
		echo "there is no tree at \"$2\""
		return
	fi
	top="$( (cd "$2/" && ls -A | LC_ALL=C sort | tr '\n' ' ') 2>/dev/null || true)"
	[ "$top" = ".complete node_modules npm-cache package-lock.json package.json " ] || echo "the tree holds $top"
	modules="$( (cd "$2/node_modules/" && find . -mindepth 1 -maxdepth 2 | LC_ALL=C sort | tr '\n' ' ') 2>/dev/null || true)"
	[ "$modules" = "./.package-lock.json ./@standin ./@standin/mcp ./standin-core ./standin-core/cli.js ./standin-core/index.js ./standin-core/package.json ./standin-runner ./standin-runner/cli.js ./standin-runner/index.js ./standin-runner/package.json " ] ||
		echo "its node_modules holds $modules"
	[ -z "$(find "$2/" -type l 2>/dev/null)" ] || echo "it holds links: $(find "$2/" -type l | tr '\n' ' ')"
	[ "$( (cd "$2/npm-cache/" && ls -A | tr '\n' ' ') 2>/dev/null || true)" = "_cacache " ] ||
		echo "its npm cache holds $(ls -A "$2/npm-cache" 2>&1 | tr '\n' ' ')"
	cmp -s "$2/package-lock.json" "$dir/package-lock.json" || echo "its lock is not the case's"
	printf '%s\n' "$standin_installed" | cmp -s - "$2/.complete" ||
		echo "its .complete holds $(cat "$2/.complete" 2>&1), not the installed digest $standin_installed"
}

# served: the registry served exactly the three tarballs since its log was emptied, and nothing else.
served() {
	local got
	got="$(LC_ALL=C sort "$TMPROOT/requests.log")"
	[ "$got" = "$tarballs" ] || echo "the registry was asked: $(tr '\n' ' ' <"$TMPROOT/requests.log")"
}

# untouched <case>: nothing is published in the case's tree cache and no installer's directory is left there.
untouched() {
	[ -z "$(ls -A "$TMPROOT/$1/cache" 2>/dev/null)" ] || echo "the cache holds $(ls -A "$TMPROOT/$1/cache" | tr '\n' ' ')"
}

# --- an install npm fails publishes nothing: the two cases npm takes longest to fail, started now -----------------

# A REGISTRY NOBODY ANSWERS AT, and A LOCK WHOSE BYTES THE REGISTRY DOES NOT SERVE, which npm refuses by the lock's
# SHA-512 (EINTEGRITY): either fails the install, and nothing is published or left behind. On the recipe's bounds npm
# tries a refused connection four times, about 11 s, and a refused tarball eight times, about 22 s, so both run beside
# the cases below and are judged at the end, against nothing those share: the refused registry is no stand-in's, the
# tampered lock's tarballs come from a stand-in registry of its own, and each runs npm itself, past the recorder.
# Mutation: publish the tree before npm runs -> its name stays on a tree npm never filled (red).
later() { # <case> <cmd>...: the command in the background, its stdout, stderr and status in the case's directory
	local dir="$TMPROOT/$1"
	shift
	( "$@" >"$dir/out" 2>"$dir/err" && echo 0 >"$dir/status" || echo $? >"$dir/status" ) &
}
judged() { # <case>: waits for a case `later` started, leaving its outcome in $out, $err and $status
	local dir="$TMPROOT/$1"
	if await 120 test -e "$dir/status"; then status="$(cat "$dir/status")"; else status="still running 120 s on"; fi
	out="$(cat "$dir/out")"
	err="$(cat "$dir/err")"
}
mkdir -p "$TMPROOT/plain"
ln -s "$real_npm" "$TMPROOT/plain/npm"
s="$(scripts refusedregistry)"
later refusedregistry env PATH="$TMPROOT/plain:$PATH" CREWLET_PLAYWRIGHT_TREE_CACHE="$TMPROOT/refusedregistry/cache" \
	npm_config_registry="http://127.0.0.1:1/" "$s"
s="$(scripts tampered tampered)"
mkdir -p "$TMPROOT/tampered/ctl"
node "$TMPROOT/registry.mjs" "$TMPROOT/web" "$TMPROOT/tampered/requests.log" "$TMPROOT/tampered/port" \
	"$TMPROOT/tampered/ctl" &
if ! await 30 test -s "$TMPROOT/tampered/port"; then
	echo "playwright-tree_test.sh: the tampered case's stand-in registry did not start" >&2
	exit 1
fi
later tampered env PATH="$TMPROOT/plain:$PATH" CREWLET_PLAYWRIGHT_TREE_CACHE="$TMPROOT/tampered/cache" \
	npm_config_registry="http://127.0.0.1:$(cat "$TMPROOT/tampered/port")/" "$s"

# --- what the recipe refuses before it fetches anything ----------------------------------------------------------

# Each refusal is exit 2, said on stderr, with nothing fetched and nothing made where it ran: the registry it would
# read is the real stand-in, so a recipe that went on would ask it (and its log would say so), and it runs from an
# empty directory of the case's own, where a recipe that went on with a relative cache would make it.
refused() { # <case> <name> <stderr-has> <cmd>...: runs the command expecting a refusal before any request
	local case_name="$1" name="$2" has="$3" cwd="$TMPROOT/$1/cwd"
	shift 3
	: >"$TMPROOT/requests.log"
	mkdir -p "$cwd"
	run within "$cwd" env CREWLET_PLAYWRIGHT_TREE_CACHE="$TMPROOT/$case_name/cache" npm_config_registry="$registry" "$@"
	expect "$name" 2 "" "$has"
	if [ -s "$TMPROOT/requests.log" ] || [ -n "$(ls -A "$cwd")" ] || [ -e "$TMPROOT/$case_name/cache" ]; then
		bad "$name, fetching and making nothing" "requests: $(cat "$TMPROOT/requests.log")" \
			"made where it ran: $(ls -A "$cwd")" "cache: $(ls -A "$TMPROOT/$case_name/cache" 2>&1)"
	else
		ok "$name, fetching and making nothing"
	fi
}

# bare <dir> <tool>...: a PATH directory holding what the recipe runs up to its install, and each tool named.
bare() {
	local dir="$1" have found
	shift
	mkdir -p "$dir"
	for have in dirname grep cut cmp sed mkdir mktemp chmod cp rm ln find "$@"; do
		if found="$(command -v "$have")"; then ln -sf "$found" "$dir/$have"; fi
	done
	echo "$dir"
}
# standin_npm <dir> <answer>: an npm in <dir> that answers --version with <answer> and refuses to run anything else.
standin_npm() {
	mkdir -p "$1"
	printf '#!/bin/sh\n[ "$1" = --version ] && echo "%s" && exit 0\necho "the stand-in npm was asked to run $*" >&2\nexit 3\n' \
		"$2" >"$1/npm"
	chmod +x "$1/npm"
}
sha_tools="sha256sum shasum"

s="$(scripts usage)"
refused usage "the recipe refuses an argument, with its usage" "usage: scripts/playwright-tree.sh" "$s" floor

# What the recipe reads is read beside it and nowhere else: the manifest and the lock it installs, and the installed
# digest and the box helper's check it holds the install to. Mutation: drop the recipe's check for one -> the
# manifest's or the lock's digest fails over a file that is not there, answering 2 without naming it, the installed
# digest is refused as no digest, or the install runs and the check cannot load (1) (red).
for file in tree/package.json tree/package-lock.json tree/installed.sha256 js/tree.mjs; do
	c="no-${file##*/}"
	s="$(scripts "$c")"
	rm "$TMPROOT/$c/internal/browser/boxhelper/$file"
	refused "$c" "the recipe refuses to run without $file beside it, naming it" "$file is missing" "$s"
done

# THE INSTALLED DIGEST is one line of 64 lowercase hex digits, as boxhelper's
# TestTheInstalledDigestNamesTheFilesTheLockInstalls reads the file, and any other file is refused before anything is
# fetched, each row holding the right digest's digits but the first, which lacks one. Mutations, in the recipe: drop
# the length check -> 63 digits installs, and is refused only by the check (1, red); drop the digits' -> upper case,
# likewise (red); drop the comparison with the file whole -> two lines, or no newline, installs (0, red).
for shape in short upper twice unterminated; do
	s="$(scripts "digest-$shape")"
	file="$(lockdir "digest-$shape")/installed.sha256"
	case $shape in
	short) what="of 63 digits" && printf '%s\n' "${standin_installed:1}" >"$file" ;;
	upper) what="in upper case" && printf '%s\n' "$standin_installed" | tr abcdef ABCDEF >"$file" ;;
	twice) what="of two lines" && printf '%s\n%s\n' "$standin_installed" "$standin_installed" >"$file" ;;
	unterminated) what="with no newline" && printf '%s' "$standin_installed" >"$file" ;;
	esac
	refused "digest-$shape" "the recipe refuses an installed digest $what, naming the line it takes" \
		"tree/installed.sha256 is not one line of 64 lowercase hex digits" "$s"
done

# A tree cache that is not an absolute path is refused before anything is looked for in it, whichever variable gave
# it. Mutation: drop the refusal -> the recipe installs into a cache in the directory it was run from (red).
s="$(scripts relative)"
refused relative "the recipe refuses a tree cache that is not an absolute path, naming the variables" \
	"the tree cache relative/cache is not an absolute path (CREWLET_PLAYWRIGHT_TREE_CACHE, XDG_CACHE_HOME, HOME)" \
	env CREWLET_PLAYWRIGHT_TREE_CACHE=relative/cache "$s"
refused relative-xdg "the recipe refuses a default tree cache that is not an absolute path, naming the variables" \
	"the tree cache relative/crewlet/playwright-tree is not an absolute path" \
	env -u CREWLET_PLAYWRIGHT_TREE_CACHE XDG_CACHE_HOME=relative "$s"

# What naming and installing a tree take, each asked for before anything is fetched. Mutation: drop a tool from the
# recipe's check -> it goes on and fails later, or never (red).
s="$(scripts nosha)"
refused nosha "the recipe refuses to name a tree without a SHA-256 tool, naming both" "needs sha256sum, or shasum" \
	env PATH="$(bare "$TMPROOT/nosha/bin" node npm)" "$s"
s="$(scripts nonode)"
standin_npm "$TMPROOT/nonode/npm" 11.20.0
refused nonode "the recipe refuses to install without node, naming it" "needs: node" \
	env PATH="$(bare "$TMPROOT/nonode/bin" $sha_tools):$TMPROOT/nonode/npm" "$s"
s="$(scripts nonpm)"
refused nonpm "the recipe refuses to install without npm, naming it" "needs: npm" \
	env PATH="$(bare "$TMPROOT/nonpm/bin" $sha_tools node)" "$s"

# THE NPM FLOOR, the recipe's min_npm, the npm the oldest Node the box helper supports bundles: an older npm is refused
# naming the fix, a prerelease of an older major too, and so is an npm whose answer names no version; the floor's first
# release installs, and so does a prerelease of a newer major, as boxhelpertest reads one. Read off the recipe, so a
# raise moves the two lines boxhelper's TestTheTreeRecipeRefusesTheNPMTheHelperRefuses holds together and nothing here.
# Mutations: drop the floor check -> the older npm's stand-in is asked to install (1, red); compare the major with -le
# -> the floor's first release is refused (red); read no prerelease's tag -> the newer prerelease is refused (red).
for answer in "$((min_npm - 1)).2.0" "$((min_npm - 1)).0.0-pre.1" "v$((min_npm + 1)).20.0"; do
	s="$(scripts "npm-$answer")"
	standin_npm "$TMPROOT/npm-$answer/bin" "$answer"
	refused "npm-$answer" "the recipe refuses an npm answering $answer, naming the npm it takes" \
		"answered --version with \"$answer\", and installing the tree takes npm $min_npm or newer" \
		env PATH="$TMPROOT/npm-$answer/bin:$PATH" "$s"
done
for answer in "$min_npm.0.0" "$((min_npm + 2)).0.0-pre.1"; do
	s="$(scripts "npm-$answer")"
	mkdir -p "$TMPROOT/npm-$answer/bin"
	printf '#!/bin/sh\n[ "$1" = --version ] && echo %s && exit 0\nexec "%s" "$@"\n' "$answer" "$TMPROOT/tools/npm" \
		>"$TMPROOT/npm-$answer/bin/npm"
	chmod +x "$TMPROOT/npm-$answer/bin/npm"
	install "npm-$answer" PATH="$TMPROOT/npm-$answer/bin:$PATH"
	expect "the recipe installs with npm $answer, of major $min_npm or newer" 0 \
		"$TMPROOT/npm-$answer/cache/$(named "npm-$answer")"
done

# --- an installed tree is the lock's, exactly, and published whole -----------------------------------------------

# THE RECORDING SLEEP, first on a case's PATH: it logs its pid and its parent's, the watchdog's, to naps.log, then
# sleeps as asked. outlived prints each process naps.log names that still runs 10 s on, when a stopped one has gone.
mkdir -p "$TMPROOT/naps"
cat >"$TMPROOT/naps/sleep" <<EOF
#!/bin/sh
echo "\$\$ \$PPID" >>"$TMPROOT/naps.log"
exec "$real_sleep" "\$@"
EOF
chmod +x "$TMPROOT/naps/sleep"
gone() { ! kill -0 "$1" 2>/dev/null; }
outlived() {
	local pid
	for pid in $(cat "$TMPROOT/naps.log"); do
		await 10 gone "$pid" || echo "$pid, which the install started, still runs: $(ps -o args= -p "$pid" || true)"
	done
}

# A fresh install: npm ci of the case's lock through the stand-in registry, which serves the three tarballs and is
# asked nothing else (no audit, no update notice, no metadata); the tree holds the lock's three packages and nothing a
# script made, beside its npm cache, with no link and no log; the published name is printed and nothing else on
# stdout: a path, which the make targets that run the whole suite export as the suite's tree; and nothing the install
# started runs on. Mutations: npm's output on stdout -> red; drop --no-audit or --no-update-notifier -> the registry
# is asked more (red); drop --logs-max=0 -> npm's log lands in the tree's npm cache (red); the watchdog's trap without
# its kill of the sleep -> the bound's sleep runs on (red); drop the lock comparison of `intact` with which publishing
# ends -> nothing here, the rerun cases below (red).
s="$(scripts fresh)"
link="$TMPROOT/fresh/cache/$(named fresh)"
: >"$TMPROOT/naps.log"
install fresh PATH="$TMPROOT/naps:$PATH"
expect "the recipe installs, checks and publishes a tree, printing its path" 0 "$link"
why=()
[ "$out" = "$link" ] || why+=("stdout is not the tree's path alone: $out")
[ -L "$link" ] || why+=("$link is not the published name, a link")
mapfile_why whole fresh "$link"
mapfile_why served
verdict "a fresh tree is the lock's three packages, its npm cache and nothing else, fetched as three tarballs" \
	${why[@]+"${why[@]}"}
why=()
[ -s "$TMPROOT/naps.log" ] || why+=("the install's watchdog never slept")
mapfile_why outlived
verdict "a finished install leaves nothing it started running: no watchdog, and no sleep of one" ${why[@]+"${why[@]}"}

# EVERYTHING THAT DECIDES WHAT IS INSTALLED, AND EVERY BOUND, IS ON npm'S COMMAND LINE, which beats the environment
# and every npmrc, read off the recorder's log. Mutation: drop any one of them, or change a bound -> red.
why=()
line="$(grep '^ci ' "$TMPROOT/npm.log" || true)"
[ -n "$line" ] || why+=("npm ci never ran: $(cat "$TMPROOT/npm.log")")
for flag in --logs-max=0 --ignore-scripts --install-strategy=hoisted --no-bin-links --no-dry-run \
	--no-package-lock-only --no-global --location=user --no-workspaces --no-offline --replace-registry-host=npmjs \
	--no-audit --no-fund --no-update-notifier --fetch-timeout=60000 --fetch-retries=3 --fetch-retry-mintimeout=1000 \
	--fetch-retry-maxtimeout=5000; do
	case " $line " in
	*" $flag "*) ;;
	*) why+=("no $flag on: npm $line") ;;
	esac
done
case " $line " in
*" --prefix $TMPROOT/fresh/cache/"*" --cache $TMPROOT/fresh/cache/"*"/npm-cache "*) ;;
*) why+=("npm did not install into a directory of its own beside the name, with a cache of its own: npm $line") ;;
esac
verdict "npm ci runs with every setting that decides the tree, and each bound, on its command line" \
	${why[@]+"${why[@]}"}

# An intact tree needs no install: a rerun with neither node nor npm on PATH, nor anything else an install runs,
# hands back the same tree and asks the registry nothing. Mutation: skip `intact` -> it refuses for want of node (2,
# red).
install fresh PATH="$(bare "$TMPROOT/fresh/bare" $sha_tools)"
expect "the recipe hands back an intact tree" 0 "$link"
why=()
[ "$out" = "$link" ] || why+=("printed $out")
[ ! -s "$TMPROOT/requests.log" ] || why+=("the registry was asked: $(cat "$TMPROOT/requests.log")")
verdict "the recipe hands back an intact tree with no node, no npm and no request" ${why[@]+"${why[@]}"}

# The recipe finds its own directory whatever CDPATH holds: scripts/git.sh's reason. Mutation: drop the recipe's
# `unset CDPATH` -> it finds no lock beside the decoy's empty scripts/ and refuses (2, red).
mkdir -p "$TMPROOT/fresh/decoy/scripts"
: >"$TMPROOT/requests.log"
run within "$TMPROOT/fresh" env CDPATH="$TMPROOT/fresh/decoy" CREWLET_PLAYWRIGHT_TREE_CACHE="$TMPROOT/fresh/cache" \
	npm_config_registry="$registry" scripts/playwright-tree.sh
expect "the recipe run by a relative path reads its own directory whatever CDPATH holds" 0 "$link"

# WHAT THE RECIPE READS WHEN NOTHING NAMES IT: ~/.cache/crewlet/playwright-tree — the suite's HOME — whenever
# CREWLET_PLAYWRIGHT_TREE_CACHE and XDG_CACHE_HOME are unset, as the suite leaves them. Mutation: misspell the
# default -> red; drop the suite's CREWLET_PLAYWRIGHT_TREE_CACHE or XDG_CACHE_HOME unset -> the nested run's copy of
# this case installs into the developer's cache (red).
s="$(scripts defaults)"
: >"$TMPROOT/requests.log"
run env -u CREWLET_PLAYWRIGHT_TREE_CACHE npm_config_registry="$registry" "$s"
expect "the recipe keeps its trees in ~/.cache/crewlet/playwright-tree when nothing names another cache" 0 \
	"$HOME/.cache/crewlet/playwright-tree/$(named defaults)"

# A TREE IS NAMED BY WHAT DECIDES ITS BYTES, the two files and the recipe's layout, never the recipe's own text: an
# edit that is not the layout reuses the tree; another lock, though it installs the same packages from the same
# manifest, installs beside the first and is never handed it; and so does another layout. Mutations: name a tree by
# the recipe's own SHA-256 -> the edited recipe installs again (red); name it by package.json alone -> the second lock
# is installed over the first, at its name (red); leave the layout out -> red.
s="$(scripts names)"
first="$TMPROOT/names/cache/$(named names)"
install names
expect "the recipe installs a tree to name" 0 "$first"
echo "# a line of comment, which changes no tree" >>"$s"
install names
why=()
[ "$out" = "$first" ] || why+=("printed $out, not $first")
[ ! -s "$TMPROOT/requests.log" ] || why+=("the registry was asked: $(cat "$TMPROOT/requests.log")")
verdict "an edit to the recipe that is not its layout reuses the tree, asking nothing" ${why[@]+"${why[@]}"}
node "$TMPROOT/lock.cjs" "$TMPROOT/web" "$(lockdir names)" respaced
second="$TMPROOT/names/cache/$(named names)"
install names
why=()
[ "$status" = 0 ] && [ "$out" = "$second" ] && [ "$out" != "$first" ] || why+=("exit $status, printed $out after $first" "$err")
mapfile_why whole names "$second"
mapfile_why served
[ -f "$first/.complete" ] || why+=("the first lock's tree went")
verdict "another lock installs a tree of its own beside the first" ${why[@]+"${why[@]}"}
sed "s/^layout=$layout\$/layout=$((layout + 1))/" "$s" >"$s.new"
mv "$s.new" "$s"
chmod +x "$s"
install names
why=()
[ "$status" = 0 ] && [ "$out" != "$second" ] && [ -s "$TMPROOT/requests.log" ] ||
	why+=("exit $status, printed $out, as the earlier layout did" "$err")
verdict "a recipe of another layout installs a tree of its own" ${why[@]+"${why[@]}"}

# --- a developer's npm settings change nothing that is installed --------------------------------------------------

# Each setting that would change what npm installs, set in the environment, and then all of them in ~/.npmrc: a dry run
# or a lock-only run installs nothing and answers 0; a linked install or bin links leave links; a script setting runs
# standin-core's install script; global, in either spelling, and workspaces make `npm ci` refuse; offline fails on the
# tree's empty cache; `never` sends the lock's registry.npmjs.org addresses to the refused proxy, past the registry
# named; and a cache of the developer's leaves the tree without its own. Each still installs the whole tree, through
# the registry named, and the developer's cache stays empty. Mutation: drop the override on npm's command line of any
# one -> its case and the npmrc case go red.
elsewhere="$TMPROOT/developers-npm-cache"
settings="dry-run=true package-lock-only=true install-strategy=linked global=true location=global bin-links=true
ignore-scripts=false offline=true replace-registry-host=never workspaces=true cache=$elsewhere"
for setting in $settings; do
	key="${setting%%=*}"
	var="NPM_CONFIG_$(printf '%s' "$key" | tr 'a-z-' 'A-Z_')"
	s="$(scripts "setting-$key")"
	install "setting-$key" "$var=${setting#*=}"
	why=()
	[ "$status" = 0 ] || why+=("exit $status" "$err")
	mapfile_why whole "setting-$key" "$TMPROOT/setting-$key/cache/$(named "setting-$key")"
	mapfile_why served
	[ ! -e "$elsewhere" ] || why+=("the developer's npm cache was written: $(ls -A "$elsewhere" | tr '\n' ' ')")
	verdict "$var=${setting#*=} in the environment changes nothing installed" ${why[@]+"${why[@]}"}
done
mkdir -p "$TMPROOT/npmrc-home"
printf '%s\n' $settings >"$TMPROOT/npmrc-home/.npmrc"
s="$(scripts npmrc)"
install npmrc HOME="$TMPROOT/npmrc-home"
why=()
[ "$status" = 0 ] || why+=("exit $status" "$err")
mapfile_why whole npmrc "$TMPROOT/npmrc/cache/$(named npmrc)"
mapfile_why served
[ ! -e "$elsewhere" ] || why+=("the developer's npm cache was written")
verdict "every one of those settings in ~/.npmrc changes nothing installed" ${why[@]+"${why[@]}"}

# --- the box helper's check decides, never npm's exit status -------------------------------------------------------

# An npm that answers 0 having installed nothing, or other than the lock, as one does under a setting the command line
# missed, publishes nothing: by the box helper's check, which the recipe runs, npm's record
# (node_modules/.package-lock.json) must name exactly the lock's packages, at its versions, addresses and SHA-512s, and
# the tree hold no link. A stand-in npm, first on PATH, answers --version as the machine's npm and an install 0, having
# laid out what a whole install does, its files (modules/whole) and an npm cache, and written the record of the lock
# with one thing changed, or a link beside it, so that one thing alone is refused, and by the check alone. Mutations,
# in the box helper's js/tree.mjs: compare one field fewer, or read the lock's keys alone -> that case is published
# (red); pass what is neither a file nor a directory -> link is published (red), or take a link for a file -> link is
# refused in other words (red); and in the recipe, drop the check -> every case is published but nothing, which is
# refused in other words (red).
cat >"$TMPROOT/record.cjs" <<'EOF'
const { readFileSync, symlinkSync, writeFileSync } = require("node:fs");
const [prefix, change] = process.argv.slice(2);
const lock = JSON.parse(readFileSync(`${prefix}/package-lock.json`, "utf8"));
delete lock.packages[""];
const core = lock.packages["node_modules/standin-core"];
if (change === "version") core.version = "2.0.1";
if (change === "resolved") core.resolved = core.resolved.replace("registry.npmjs.org", "registry.example.com");
if (change === "integrity") core.integrity = "sha512-" + Buffer.alloc(64).toString("base64");
if (change === "dropped") delete lock.packages["node_modules/standin-core"];
if (change === "extra") lock.packages["node_modules/standin-extra"] = { ...core };
if (change === "link") symlinkSync("..", `${prefix}/node_modules/standin-link`);
writeFileSync(`${prefix}/node_modules/.package-lock.json`, JSON.stringify(lock, null, 2));
EOF
for change in nothing version resolved integrity dropped extra link; do
	says="npm's record of what it installed is not the lock: node_modules/standin-core"
	case $change in
	nothing) what="nothing" says="npm left no record of what it installed" ;;
	version) what="a package at another version" ;;
	resolved) what="a package from another address" ;;
	integrity) what="a package of another SHA-512" ;;
	dropped) what="the lock less a package" ;;
	extra) what="a package the lock does not hold" says="npm's record of what it installed is not the lock: node_modules/standin-extra" ;;
	link) what="the lock and a link" says="it holds what is neither a file nor a directory: node_modules/standin-link" ;;
	esac
	s="$(scripts "record-$change")"
	mkdir -p "$TMPROOT/record-$change/bin"
	cat >"$TMPROOT/record-$change/bin/npm" <<EOF
#!/bin/sh
[ "\$1" = --version ] && exec "$real_npm" --version
prefix="" cache=""
while [ \$# -gt 0 ]; do
	[ "\$1" != --prefix ] || prefix=\$2
	[ "\$1" != --cache ] || cache=\$2
	shift
done
[ $change != nothing ] || exit 0
cp -R "$TMPROOT/modules/whole" "\$prefix/node_modules"
mkdir -p "\$cache/_cacache"
exec "$real_node" "$TMPROOT/record.cjs" "\$prefix" $change
EOF
	chmod +x "$TMPROOT/record-$change/bin/npm"
	install "record-$change" PATH="$TMPROOT/record-$change/bin:$PATH"
	why=()
	[ "$status" = 1 ] || why+=("exit $status, want 1")
	grep -qF -- "$says" <<<"$err" || why+=("stderr lacks: $says" "$err")
	mapfile_why untouched "record-$change"
	verdict "an npm answering 0 having installed $what is refused, saying so, and nothing is published" \
		${why[@]+"${why[@]}"}
done

# A TREE NPM LEFT SHORT, its record whole, as when a file npm could not write (a full disk, a quota) was only a warning
# to it: the real install through the recorder, then one file of it deleted. Only the files' digest tells, and the
# refusal names it, computed here, the file that names another and what a bump writes there. Mutation: hold the
# install to npm's record and its links alone -> the short tree is published (0, red).
cp -R "$TMPROOT/modules/whole" "$TMPROOT/modules/short"
rm "$TMPROOT/modules/short/standin-core/index.js"
s="$(scripts short)"
mkdir -p "$TMPROOT/short/bin"
cat >"$TMPROOT/short/bin/npm" <<EOF
#!/bin/sh
[ "\$1" = --version ] && exec "$TMPROOT/tools/npm" --version
prefix="" last=""
for arg in "\$@"; do
	[ "\$last" != --prefix ] || prefix=\$arg
	last=\$arg
done
"$TMPROOT/tools/npm" "\$@" || exit
rm "\$prefix/node_modules/standin-core/index.js"
EOF
chmod +x "$TMPROOT/short/bin/npm"
install short PATH="$TMPROOT/short/bin:$PATH"
why=()
[ "$status" = 1 ] || why+=("exit $status, want 1")
short_installed="$(installed_digest "$TMPROOT/modules/short")"
for says in "the files it installed have the digest $short_installed, where" \
	"internal/browser/boxhelper/tree/installed.sha256 names $standin_installed." \
	"A bump of the lock writes $short_installed into"; do
	grep -qF -- "$says" <<<"$err" || why+=("stderr lacks: $says" "$err")
done
mapfile_why untouched short
verdict "an npm answering 0 having left a file short is refused, naming the digest its files have, and nothing is published" \
	${why[@]+"${why[@]}"}

# AN INSTALLED DIGEST NAMING OTHER FILES than the lock installs, as one left from before a bump of the lock does: a
# whole install is refused, and the refusal names the file to change and the digest its files have, computed here, the
# value a bump writes there, never npm as what failed. Mutations: skip the digest -> the tree is published (0, red);
# word the refusal as npm's failure -> it names neither the file nor what to write (red).
s="$(scripts stale)"
printf '%s\n' "$other_installed" >"$(lockdir stale)/installed.sha256"
install stale
why=()
[ "$status" = 1 ] || why+=("exit $status, want 1")
for says in "the files it installed have the digest $standin_installed, where" \
	"internal/browser/boxhelper/tree/installed.sha256 names $other_installed." \
	"A bump of the lock writes $standin_installed into"; do
	grep -qF -- "$says" <<<"$err" || why+=("stderr lacks: $says" "$err")
done
! grep -qF -- "npm did not install exactly the lock" <<<"$err" || why+=("the refusal blames npm: $err")
mapfile_why untouched stale
verdict "a whole install is refused when installed.sha256 names other files, naming the digest its files have, and nothing is published" \
	${why[@]+"${why[@]}"}

# --- an npm that outlives the bound is stopped: TERM, then KILL -------------------------------------------------

# The recipe's bound is 300 s and its grace 5 s, and a stand-in sleep, first on the case's PATH, stands for both: it
# logs what it is asked and returns once the stand-in npm is in the state the case needs, the first sleep once npm is
# running, the second once npm has heard the TERM and stayed. The stand-in npm answers --version, then ignores TERM, as
# npm does while a fetch is outstanding. Mutations: TERM alone -> the recipe waits on npm for ever, and this case's own
# deadline fails it (red); KILL without TERM -> red; another bound or grace -> red; no bound at all -> red.
wd="$TMPROOT/watchdog"
mkdir -p "$wd/bin"
: >"$wd/asked"
cat >"$wd/bin/npm" <<EOF
#!/bin/sh
[ "\$1" = --version ] && echo $((min_npm + 1)).20.0 && exit 0
exec "$real_node" -e '
const fs = require("node:fs");
process.on("SIGTERM", () => fs.writeFileSync("$wd/heard", "TERM"));
fs.writeFileSync("$wd/pid", String(process.pid));
fs.writeFileSync("$wd/running", "");
setInterval(() => {}, 1 << 30);'
EOF
cat >"$wd/bin/sleep" <<EOF
#!/bin/sh
echo "\$1" >>"$wd/asked"
case \$(wc -l <"$wd/asked" | tr -d ' ') in
1) until [ -e "$wd/running" ]; do "$real_sleep" 0.05; done ;;
*) until [ -e "$wd/heard" ]; do "$real_sleep" 0.05; done ;;
esac
EOF
chmod +x "$wd/bin/npm" "$wd/bin/sleep"
s="$(scripts watchdog)"
(
	PATH="$wd/bin:$PATH" CREWLET_PLAYWRIGHT_TREE_CACHE="$TMPROOT/watchdog/cache" npm_config_registry="$registry" "$s" \
		>"$wd/out" 2>"$wd/err" && echo 0 >"$wd/status" || echo $? >"$wd/status"
) &
wd_pid=$!
why=()
if await 60 test -e "$wd/status"; then
	[ "$(cat "$wd/status")" = 1 ] || why+=("exit $(cat "$wd/status"), want 1")
	grep -qF "npm had not installed the tree after 300 seconds, and was stopped" "$wd/err" ||
		why+=("stderr does not name the bound: $(cat "$wd/err")")
	[ "$(tr '\n' ' ' <"$wd/asked" 2>/dev/null)" = "300 5 " ] ||
		why+=("the bound and the grace asked of sleep: $(tr '\n' ' ' <"$wd/asked" 2>/dev/null)")
	[ -e "$wd/heard" ] || why+=("npm was never sent TERM")
	! kill -0 "$(cat "$wd/pid")" 2>/dev/null || why+=("npm, $(cat "$wd/pid"), is still running")
	mapfile_why untouched watchdog
else
	why+=("the recipe was still running 60 s on: npm was never stopped" "$(cat "$wd/err")")
	killtree "$wd_pid"
	[ ! -e "$wd/pid" ] || kill -9 "$(cat "$wd/pid")" 2>/dev/null || true
fi
verdict "an npm that ignores TERM is sent TERM at the bound and KILL after the grace, and nothing is published" \
	${why[@]+"${why[@]}"}

# --- an interrupted install leaves nothing running and nothing behind ---------------------------------------------

# Each signal a person or a supervisor stops the recipe with, sent to the recipe alone while npm waits on a registry
# that holds its request: the recipe ends 128 and the signal's number, having stopped npm, its watchdog and the
# watchdog's sleep, and removed its directory. npm, which a non-interactive sh starts with INT ignored, is stopped by
# the recipe or not at all. Mutations: drop cleanup's kill of npm -> npm runs on (red); drop a signal's trap -> the
# recipe dies by it, cleaning nothing up (red); the watchdog's trap without its kill of the sleep -> the sleep runs on
# (red); start the recipe without the launcher -> the nested run, which starts with HUP and INT ignored, fails both
# cases (red).
#
# THE LAUNCHER: node starts the recipe with every signal at its default action, as a terminal's shell starts a
# command. Neither sh nor bash can undo an ignore it inherited, and `nohup` ignores HUP, as a non-interactive sh
# ignores INT in an asynchronous list. It writes the recipe's pid to its first argument and ends with the recipe's
# status, or 128 and the number of the signal that ended it.
ic="$TMPROOT/interrupt"
mkdir -p "$ic/ctl" "$ic/bin"
cat >"$ic/launch.mjs" <<'EOF'
import { spawn } from "node:child_process";
import { renameSync, writeFileSync } from "node:fs";
import { constants } from "node:os";
const [pidFile, command] = process.argv.slice(2);
const child = spawn(command, [], { stdio: "inherit" });
child.on("spawn", () => {
  writeFileSync(pidFile + ".new", String(child.pid));
  renameSync(pidFile + ".new", pidFile);
});
child.on("error", (err) => {
  console.error(`launch.mjs: ${err.message}`);
  process.exit(1);
});
child.on("exit", (code, signal) => process.exit(code ?? 128 + constants.signals[signal]));
EOF
node "$TMPROOT/registry.mjs" "$TMPROOT/web" "$ic/requests.log" "$ic/port" "$ic/ctl" &
if ! await 30 test -s "$ic/port"; then
	echo "playwright-tree_test.sh: the interrupt case's stand-in registry did not start" >&2
	exit 1
fi
cat >"$ic/bin/npm" <<EOF
#!/bin/sh
[ "\$1" = --version ] || echo \$\$ >"$ic/npm.pid"
exec "$TMPROOT/tools/npm" "\$@"
EOF
chmod +x "$ic/bin/npm"
for signal in TERM HUP INT; do
	s="$(scripts "interrupt-$signal")"
	rm -f "$ic/ctl/arrived" "$ic/npm.pid" "$ic/recipe.pid"
	: >"$TMPROOT/naps.log"
	env PATH="$ic/bin:$TMPROOT/naps:$PATH" CREWLET_PLAYWRIGHT_TREE_CACHE="$TMPROOT/interrupt-$signal/cache" \
		npm_config_registry="http://127.0.0.1:$(cat "$ic/port")/held/" \
		"$real_node" "$ic/launch.mjs" "$ic/recipe.pid" "$s" >"$ic/out" 2>"$ic/err" &
	launcher=$!
	why=()
	if await 60 test -e "$ic/ctl/arrived" && await 10 test -s "$TMPROOT/naps.log" &&
		await 10 test -s "$ic/recipe.pid"; then
		kill -s "$signal" "$(cat "$ic/recipe.pid")" || true
		wait "$launcher" && status=0 || status=$?
		want=$((128 + $(kill -l "$signal")))
		[ "$status" = "$want" ] || why+=("exit $status, want $want" "$(cat "$ic/err")")
		npm_pid="$(cat "$ic/npm.pid" 2>/dev/null || true)"
		if [ -z "$npm_pid" ]; then
			why+=("npm's pid was never logged")
		elif ! await 10 gone "$npm_pid"; then
			why+=("npm, $npm_pid, still runs")
			kill -9 "$npm_pid" 2>/dev/null || true
		fi
		mapfile_why outlived
		mapfile_why untouched "interrupt-$signal"
	else
		why+=("npm never asked the held registry for its tarballs" "$(cat "$ic/err")")
		killtree "$launcher"
	fi
	verdict "an install sent $signal ends $((128 + $(kill -l "$signal"))), leaving nothing running and nothing behind" \
		${why[@]+"${why[@]}"}
done

# --- a name pointing at no intact tree is installed over ---------------------------------------------------------

# Whichever way the name points there: a link to a tree without .complete; a directory at the name itself holding the
# same; a link to a complete tree of another lock; one checked against another installed digest, which its .complete
# names; a link to a complete tree of this lock that npm left no record of an install in; and one without the npm
# cache boxhelper's cases install from. Mutations: drop .complete, what it names, the lock comparison, the record or
# the npm cache from `intact` -> that tree is handed back, nothing installed (red); drop the publish loop's case of a
# directory at the name -> the link is made INSIDE the directory, which the name stays (red).
for shape in incomplete directory otherlock otherdigest unrecorded uncached; do
	s="$(scripts "over-$shape")"
	dir="$(lockdir "over-$shape")"
	link="$TMPROOT/over-$shape/cache/$(named "over-$shape")"
	broken="$link.stale1"
	[ "$shape" != directory ] || broken="$link"
	mkdir -p "$broken/node_modules" "$broken/npm-cache/_cacache"
	cp "$dir/package.json" "$dir/package-lock.json" "$broken/"
	echo '{"packages": {}}' >"$broken/node_modules/.package-lock.json"
	printf '%s\n' "$standin_installed" >"$broken/.complete"
	case $shape in
	incomplete | directory) rm "$broken/.complete" ;;
	otherlock) echo " " >>"$broken/package-lock.json" ;;
	otherdigest) printf '%s\n' "$other_installed" >"$broken/.complete" ;;
	unrecorded) rm "$broken/node_modules/.package-lock.json" ;;
	uncached) rm -r "$broken/npm-cache" ;;
	esac
	[ "$shape" = directory ] || ln -s "$(basename "$broken")" "$link"
	install "over-$shape"
	why=()
	[ "$status" = 0 ] && [ "$out" = "$link" ] || why+=("exit $status, printed $out" "$err")
	[ -L "$link" ] || why+=("the name is not a link")
	mapfile_why whole "over-$shape" "$link"
	mapfile_why served
	verdict "the recipe installs over a name holding $shape tree, linking it to the tree installed in its place" \
		${why[@]+"${why[@]}"}
done

# --- two installers at once publish one tree, and neither deletes the other's ------------------------------------

# scripts/node-pin_test.sh's case and its reason, over the tree: B starts an install whose tarballs the registry
# holds (it names the registry's /held/ as its own); A installs and publishes meanwhile, and the tree A printed must be
# whole while B is still installing, because B installs in a directory of its own. Released, B keeps A's tree and
# removes its own. Mutation: install in place at the published name, emptied first -> one installer's emptying deletes
# the other's tree (red).
s="$(scripts concurrent)"
cc="$TMPROOT/concurrent"
installer() { # <name> <registry>: the case's recipe in the background, its outcome in <cc>/<name>.*
	(
		CREWLET_PLAYWRIGHT_TREE_CACHE="$cc/cache" npm_config_registry="$2" "$s" >"$cc/$1.out" 2>"$cc/$1.err" &&
			echo 0 >"$cc/$1.status" || echo $? >"$cc/$1.status"
	) &
}
concurrent() {
	local atree why=()
	installer b "${registry}held/"
	await 60 test -e "$TMPROOT/ctl/arrived" || {
		bad "the held installer asks for its tarballs" "$(cat "$cc/b.err")"
		return
	}
	installer a "$registry"
	await 60 test -e "$cc/a.status" || {
		bad "the other installer finishes" "$(cat "$cc/a.err")"
		return
	}
	atree="$(cat "$cc/a.out")"
	why=()
	[ "$(cat "$cc/a.status")" = 0 ] || why+=("exit $(cat "$cc/a.status")" "$(cat "$cc/a.err")")
	[ ! -e "$cc/b.status" ] || why+=("the held installer had already ended: $(cat "$cc/b.err")")
	mapfile_why whole concurrent "$atree"
	verdict "the tree one installer published is whole while another is still installing" ${why[@]+"${why[@]}"}
	touch "$TMPROOT/ctl/go"
	await 60 test -e "$cc/b.status" || {
		bad "the held installer finishes" "$(cat "$cc/b.err")"
		return
	}
	why=()
	[ "$(cat "$cc/b.status")" = 0 ] && [ "$(cat "$cc/b.out")" = "$atree" ] ||
		why+=("exit $(cat "$cc/b.status"), printed $(cat "$cc/b.out") and $atree" "$(cat "$cc/b.err")")
	[ "$(find "$cc/cache" -mindepth 1 -maxdepth 1 -type d -name "$(named concurrent).*" | wc -l | tr -d ' ')" = 1 ] ||
		why+=("tree directories: $(ls -A "$cc/cache" | tr '\n' ' ')")
	mapfile_why whole concurrent "$atree"
	verdict "two installers at once both succeed, print one path and leave one tree" ${why[@]+"${why[@]}"}
}
concurrent

# --- a crashed installer's leftovers are swept, a running one's never ---------------------------------------------

# Mutation: sweep every leftover whatever its age -> the fresh one goes (red); sweep none -> the old one stays (red).
s="$(scripts strays)"
strays="$TMPROOT/strays/cache/$(named strays)"
mkdir -p "$strays.crashd/node_modules" "$strays.workng/node_modules"
twodays="$(($(date +%s) - 2 * 86400))"
stamp="$(date -d "@$twodays" +%Y%m%d%H%M 2>/dev/null || date -r "$twodays" +%Y%m%d%H%M)" # GNU, then BSD date
touch -t "$stamp" "$strays.crashd"
install strays
why=()
[ "$status" = 0 ] || why+=("exit $status" "$err")
[ ! -e "$strays.crashd" ] || why+=("the crashed installer's directory stayed")
[ -d "$strays.workng" ] || why+=("the working installer's directory went")
verdict "an install sweeps a crashed installer's leftovers a day old and keeps a fresh one" ${why[@]+"${why[@]}"}

# --- the two cases started first, judged ----------------------------------------------------------------------------

judged refusedregistry
expect "a refused registry fails the install (1), saying so" 1 "" "npm ci failed"
why=()
mapfile_why untouched refusedregistry
verdict "a refused registry publishes nothing and leaves nothing in the cache" ${why[@]+"${why[@]}"}
judged tampered
expect "the recipe refuses a tarball the lock's SHA-512 does not name (1), in npm's words" 1 "" "EINTEGRITY"
why=()
mapfile_why untouched tampered
verdict "a refused tarball publishes nothing and leaves nothing in the cache" ${why[@]+"${why[@]}"}

# --- the suite reads nothing of the environment of the person running it -----------------------------------------

# This suite started again under a developer's environment at its most hostile: npm settings in the environment, in
# every letter case npm reads, that would break every install the suite runs were they read (a log level npm does not
# know, a proxy for the stand-in's own address, one file as both npmrcs, a cache of the developer's), the same in the
# developer's ~/.npmrc, the tree cache and XDG's pointing at the developer's own, and the proxies of the developer's
# shell. It is run as `make playwright-tree-test` runs it, by a path relative to the tree, under a CDPATH whose one
# entry holds a scripts/ of its own, empty, and with HUP ignored, as `nohup` leaves it, and INT, as a script's
# `make check &` does. The nested run must pass, having run its cases, and leave every file of that environment exactly
# as it found it.
#
# Mutation: scrub npm's settings in upper case alone, or not at all -> the nested run's installs fail (red); drop the
# HOME pin -> the developer's ~/.npmrc is read (red); drop the CREWLET_PLAYWRIGHT_TREE_CACHE or XDG_CACHE_HOME unset ->
# the defaults case fills the developer's cache (red); drop the CDPATH unset -> the nested run finds its own directory
# in the decoy and fails at its first case (red).
if [ -z "${CREWLET_PLAYWRIGHT_TREE_TEST_NESTED-}" ]; then
	dev="$TMPROOT/dev"
	mkdir -p "$dev/home" "$dev/config" "$dev/cache" "$dev/xdg-cache" "$dev/npm-cache" \
		"$dev/cdpath/$(basename "$SCRIPT_DIR")"
	printf '%s\n' "loglevel=an-unknown-level" "https-proxy=http://127.0.0.1:1" "proxy=http://127.0.0.1:1" \
		"noproxy=example.com" "cache=$dev/npm-cache" >"$dev/home/.npmrc"
	devsnapshot() { find "$dev" | LC_ALL=C sort && find "$dev" -type f -exec cksum {} + | LC_ALL=C sort; }
	before="$(devsnapshot)"
	nested="$(trap '' HUP INT && within "$(dirname "$SCRIPT_DIR")" env CREWLET_PLAYWRIGHT_TREE_TEST_NESTED=1 \
		HOME="$dev/home" XDG_CONFIG_HOME="$dev/config" CDPATH="$dev/cdpath" \
		CREWLET_PLAYWRIGHT_TREE_CACHE="$dev/cache" XDG_CACHE_HOME="$dev/xdg-cache" \
		npm_config_loglevel=an-unknown-level Npm_Config_Proxy="http://127.0.0.1:1" \
		NPM_CONFIG_HTTPS_PROXY="http://127.0.0.1:1" npm_config_noproxy=example.com \
		Npm_Config_Userconfig="$dev/home/.npmrc" NPM_CONFIG_GLOBALCONFIG="$dev/home/.npmrc" \
		npm_config_cache="$dev/npm-cache" HTTPS_PROXY="http://127.0.0.1:1" HTTP_PROXY="http://127.0.0.1:1" \
		NO_PROXY=example.com "$(basename "$SCRIPT_DIR")/$(basename "${BASH_SOURCE[0]}")" 2>&1)" && status=0 || status=$?
	after="$(devsnapshot)"
	if [ "$status" -eq 0 ] &&
		grep -q '^ok   a fresh tree is the lock.s three packages, its npm cache and nothing else, fetched as three tarballs$' <<<"$nested" &&
		grep -q '^ok   two installers at once both succeed, print one path and leave one tree$' <<<"$nested" &&
		grep -q ' 0 failed$' <<<"$nested" && [ "$before" = "$after" ]; then
		ok "under a developer's environment the suite passes and touches nothing of it"
	else
		bad "under a developer's environment the suite passes and touches nothing of it" "exit $status" \
			"the environment's files changed: $([ "$before" = "$after" ] && echo no || echo yes)" "$nested"
	fi
fi

echo
echo "playwright-tree_test.sh: $pass passed, $fail failed"
[ "$fail" -eq 0 ]

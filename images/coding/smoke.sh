#!/bin/sh
# The crewlet-coding image's smoke test, run INSIDE the image by
# `make coding-image` (and so by .github/workflows/coding-image.yml), fed on
# stdin rather than copied in, so the image ships no test script.
#
# It runs as a uid the image has no account for, with a home that is not the
# image's, because that is how the engine starts a box on a rootful runtime
# (`--user <engine uid>:<gid>`, the box's own home mounted at /home/user): a
# tool that works only for the image's own account works in no box.
set -eu

fail() {
	echo "smoke: $*" >&2
	exit 1
}

mkdir -p "$HOME"
cd "$HOME"

# THE CLIS ARE THE PINNED ONES. A lock that installed something else, or a
# launcher whose platform binary went missing, fails here rather than at an
# agent's first run.
pins=/opt/crewlet-coding/package.json
claude_pin=$(jq -r '.dependencies["@anthropic-ai/claude-code"]' "$pins")
opencode_pin=$(jq -r '.dependencies["@opencode/cli"]' "$pins")
claude_got=$(claude --version) || fail "claude does not run"
case "$claude_got" in
"$claude_pin "*) ;;
*) fail "claude reports '$claude_got', the image pins $claude_pin" ;;
esac
opencode_got=$(opencode --version) || fail "opencode does not run"
[ "$opencode_got" = "opencode v$opencode_pin" ] ||
	fail "opencode reports '$opencode_got', the image pins $opencode_pin"

# THE TOOLCHAINS a repository's own build reaches for.
python3 -c 'import sys; assert sys.version_info[:2] == (3, 11), sys.version' ||
	fail "python3 is not 3.11"
python3 -m venv "$HOME/venv" || fail "python3 cannot make a virtual environment"
"$HOME/venv/bin/pip" --version >/dev/null || fail "the virtual environment has no pip"
node -e 'process.exit(process.versions.node.split(".")[0] === "22" ? 0 : 1)' ||
	fail "node is not the 22 line"
npm --version >/dev/null || fail "npm does not run"
corepack --version >/dev/null || fail "corepack does not run"
for shim in pnpm yarn; do
	command -v "$shim" >/dev/null || fail "corepack's $shim shim is not on PATH"
done
for tool in git gh curl jq make gcc g++ pkg-config pg_config ps unzip xz; do
	command -v "$tool" >/dev/null || fail "$tool is not on PATH"
done
gh --version >/dev/null || fail "gh does not run"

# AND A GIT REPOSITORY CAN BE MADE AND COMMITTED TO by an account with no
# passwd entry, which is the first thing a coding run does.
git init -q "$HOME/repo"
git -C "$HOME/repo" -c user.name=smoke -c user.email=smoke@example.com \
	commit -q --allow-empty -m smoke || fail "git cannot commit as uid $(id -u)"

echo "smoke: ok ($claude_got, $opencode_got, $(python3 --version), node $(node --version), $(gh --version | head -n1))"

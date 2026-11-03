#!/usr/bin/env bash
#
# check-signoff_test.sh — the gate's own suite.
#
# check-signoff.sh is the one gate whose subject is git history, so nothing in
# the Go suite can reach it and a regression here is silent in both directions:
# a gate that stops failing reports a DCO certification nobody made, and a gate
# that starts failing wedges every pull request. Both have already happened
# once during this script's own development, which is why this exists.
#
# Each case builds a throwaway repository under a temp directory and runs the
# REAL script against it. Nothing here touches the repository it lives in,
# and nothing here reads the git of the person running it (below).
set -euo pipefail

# THE PERSON'S GIT ENVIRONMENT, GONE before the first git runs: every variable
# whose name begins GIT_. Git EXPORTS its repository to every hook it runs —
# measured on 2.43, a pre-commit hook in a linked worktree gets GIT_DIR naming
# that worktree's git directory and an absolute GIT_INDEX_FILE (in the main
# checkout too under `commit -a`), beside GIT_PREFIX, GIT_EXEC_PATH and the
# outer command's -c settings as GIT_CONFIG_PARAMETERS — so this suite, run by
# a `make check` a hook started, sent every `git -C <throwaway>` below to the
# developer's repository: re-initialised it, staged and committed the cases'
# files there, and ran the developer's pre-commit hook again at each commit.
# The rest of the prefix for the reason cmd/crewlet's scrubGitEnv gives:
# GIT_CONFIG_COUNT and its pairs outrank every configuration file,
# GIT_EXEC_PATH chooses git's programs, GIT_TEMPLATE_DIR the hooks `init`
# installs, and one prefix stays right as git reads new names where a list —
# `git rev-parse --local-env-vars` included, which names the repository's
# variables alone — is a fact about one git.
for name in $(compgen -e); do
	case "$name" in
	GIT_*) unset "$name" ;;
	esac
done

# AND CDPATH, before the cd that finds this suite's own directory. Run by a path
# relative to the tree, as `make signoff-test` runs it, `cd scripts` searches
# an exported CDPATH's entries first: one holding a scripts/ of its own made
# that directory the suite's, and its check-signoff.sh — or none — the gate
# every case judged.
unset CDPATH
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECK="$SCRIPT_DIR/check-signoff.sh"

TMPROOT="$(mktemp -d)"
trap 'rm -rf "$TMPROOT"' EXIT

# AND THE PERSON'S GIT CONFIGURATION FILES: a HOME and a configuration home of
# the suite's own, no system file and no system attributes. A developer's
# global configuration decides what these cases build — measured: a global
# core.hooksPath whose commit-msg hook signs off every commit (a DCO
# convenience) turned "an unsigned commit fails" into a pass and the remedy
# case into a failure, and commit.gpgsign with a signing program that fails
# failed the first commit. XDG_CONFIG_HOME is pinned beside HOME because git
# reads $XDG_CONFIG_HOME/git/config, attributes and ignore whatever HOME says.
export HOME="$TMPROOT/home"
export XDG_CONFIG_HOME="$HOME/.config"
export GIT_CONFIG_NOSYSTEM=1 GIT_ATTR_NOSYSTEM=1
mkdir -p "$HOME"

pass=0
fail=0

# A committer identity that never depends on the machine's git config, and a
# fixed date so nothing here can depend on the clock.
git_env() {
	export GIT_AUTHOR_NAME='Test Author' GIT_AUTHOR_EMAIL='author@example.com'
	export GIT_COMMITTER_NAME='Test Author' GIT_COMMITTER_EMAIL='author@example.com'
	export GIT_AUTHOR_DATE='2026-01-01T00:00:00Z' GIT_COMMITTER_DATE='2026-01-01T00:00:00Z'
}

newrepo() {
	local d="$TMPROOT/$1"
	mkdir -p "$d"
	git -C "$d" init -q -b main
	git_env
	echo seed >"$d/f"
	git -C "$d" add f
	git -C "$d" commit -qm "chore: seed

Signed-off-by: Test Author <author@example.com>"
	echo "$d"
}

commit() { # <repo> <message>
	git_env
	echo "$RANDOM$2" >>"$1/f"
	git -C "$1" add f
	git -C "$1" commit -qm "$2"
}

check() { # <name> <want:pass|fail> <repo> [args...]
	local name="$1" want="$2" repo="$3"
	shift 3
	local out status
	out="$(cd "$repo" && "$CHECK" "$@" 2>&1)" && status=0 || status=$?
	local got=pass
	[ "$status" -eq 0 ] || got=fail
	if [ "$got" = "$want" ]; then
		pass=$((pass + 1))
		printf 'ok   %s\n' "$name"
	else
		fail=$((fail + 1))
		printf 'FAIL %s (wanted %s, got %s, exit %s)\n%s\n' "$name" "$want" "$got" "$status" "$out"
	fi
}

# --- the two directions the gate exists for -------------------------------

r="$(newrepo signed)"
commit "$r" "feat: signed

Signed-off-by: Test Author <author@example.com>"
check "a signed commit passes" pass "$r" HEAD~1 HEAD

r="$(newrepo unsigned)"
commit "$r" "feat: unsigned"
check "an unsigned commit fails" fail "$r" HEAD~1 HEAD

# --- what %B could not tell apart, and why the atom is the right parser ----

# The defect Copilot's review found: a sign-off quoted in prose is not a
# certification, and reading the whole message cannot see the difference.
r="$(newrepo buried)"
commit "$r" "feat: quotes a trailer in prose

Reverting the commit whose message read:

Signed-off-by: Someone Else <else@example.com>

...which is why this change is needed."
check "a sign-off buried in prose FAILS (not a trailer block)" fail "$r" HEAD~1 HEAD

# The regression the OTHER obvious fix would cause. `git interpret-trailers
# --parse` reads the `---` here as a patch divider and finds no trailers at
# all, which would have turned every Dependabot bump in this repository red.
r="$(newrepo dependabot)"
commit "$r" "build(deps): bump example from 1.0.0 to 1.0.1

Bumps example from 1.0.0 to 1.0.1.

---
updated-dependencies:
- dependency-name: example
  dependency-type: direct:production
...

Signed-off-by: dependabot[bot] <support@github.com>
Co-authored-by: dependabot[bot] <49699333+dependabot[bot]@users.noreply.github.com>"
check "a Dependabot-shaped ---/... message PASSES" pass "$r" HEAD~1 HEAD

# --- the two properties the script documents at its top -------------------

r="$(newrepo relayed)"
git_env
echo relayed >>"$r/f"
git -C "$r" add f
GIT_AUTHOR_NAME='Original Author' GIT_AUTHOR_EMAIL='original@example.com' \
	git -C "$r" commit -qm "feat: a relayed patch

Signed-off-by: Original Author <original@example.com>
Signed-off-by: Relaying Maintainer <relay@example.com>"
check "author != signer passes (DCO clauses (b)/(c))" pass "$r" HEAD~1 HEAD

r="$(newrepo merges)"
base="$(git -C "$r" rev-parse HEAD)"
git -C "$r" checkout -q -b side
git_env
echo side >"$r/side.txt"
git -C "$r" add side.txt
git -C "$r" commit -qm "feat: on a side branch

Signed-off-by: Test Author <author@example.com>"
git -C "$r" checkout -q main
commit "$r" "feat: on main

Signed-off-by: Test Author <author@example.com>"
git_env
# --no-ff and a message with no trailer: this is the shape the gate must skip.
git -C "$r" merge -q --no-ff -m "Merge branch 'side'" side
check "an unsigned MERGE commit is exempt" pass "$r" "$base" HEAD

# --- the range is the branch's own commits, not a ref tip's descendants ----

# The base branch moving on is the normal case, not an edge one, and a base
# ref's later commits must never be judged: they are not what this branch adds.
r="$(newrepo basemoved)"
git -C "$r" checkout -q -b feature
commit "$r" "feat: my own signed work

Signed-off-by: Test Author <author@example.com>"
git -C "$r" checkout -q main
commit "$r" "feat: the base branch moved on, unsigned"
git -C "$r" checkout -q feature
check "commits the BASE added afterwards are not judged" pass "$r" main HEAD

# The remedy a failure prints has to be safe to run. `git rebase --signoff
# origin/main` against a fork whose main has gone stale replays commits the
# contributor never wrote and restamps them under their own sign-off, so the
# SHA printed must be an ANCESTOR of the head — which a merge base always is.
# The base must DIVERGE — carry a commit the branch does not have — or the
# base ref is an ancestor anyway and this asserts nothing.
r="$(newrepo remedy)"
git -C "$r" checkout -q -b feature
commit "$r" "feat: unsigned, to force the remedy to print"
git -C "$r" checkout -q main
commit "$r" "feat: a commit only the base branch has"
git -C "$r" branch -q stale-main main
git -C "$r" checkout -q feature
# `|| true` before the pipe: CHECK is SUPPOSED to fail here, and under
# `set -o pipefail` a failing writer makes the whole substitution non-zero,
# which errexit then treats as this suite crashing.
remedy="$( { cd "$r" && "$CHECK" stale-main HEAD 2>&1 || true; } | sed -n 's/.*git rebase --signoff //p')"
if [ -n "$remedy" ] && git -C "$r" merge-base --is-ancestor "$remedy" HEAD; then
	pass=$((pass + 1))
	echo "ok   the printed rebase remedy names an ancestor of HEAD"
else
	fail=$((fail + 1))
	echo "FAIL the rebase remedy must name an ancestor of HEAD, got '$remedy'"
fi

# --- refusals: a skip is not a pass ---------------------------------------

r="$(newrepo zeros)"
check "an all-zeros base is refused, not a raw git fatal" fail "$r" \
	0000000000000000000000000000000000000000 HEAD

mkdir -p "$TMPROOT/norepo"
check "outside a git repository it refuses" fail "$TMPROOT/norepo"

# --- the failure path must not page ---------------------------------------

# `git show` pages on a terminal, and git only supplies its own LESS=FRX when
# LESS is UNSET — a contributor who exports LESS loses -F and the gate BLOCKS
# at (END) inside a silent make recipe. There is no tty here, so this asserts
# the flag rather than the hang: the reporting path must never invoke a pager.
if grep -nE '^[^#]*git ([^|]*[^-])?show|^[^#]*git log' "$CHECK" |
	grep -v -- '--no-pager' | grep -q .; then
	fail=$((fail + 1))
	echo "FAIL every git invocation that can reach a terminal must use --no-pager"
else
	pass=$((pass + 1))
	echo "ok   the reporting path cannot invoke a pager"
fi

# --- the success line names what was judged -------------------------------

r="$(newrepo counted)"
commit "$r" "feat: one

Signed-off-by: Test Author <author@example.com>"
commit "$r" "feat: two

Signed-off-by: Test Author <author@example.com>"
if (cd "$r" && "$CHECK" HEAD~2 HEAD) | grep -q '2 commit(s)'; then
	pass=$((pass + 1))
	echo "ok   a pass says how many commits it judged"
else
	fail=$((fail + 1))
	echo "FAIL a pass must say how many commits it judged, so a wrong base is visible"
fi

# --- the suite reads nothing of the git of the person running it -----------

# This suite started again where a `make check` a pre-commit hook runs starts
# it: in a SENTINEL repository's linked worktree, under the variables git
# exports to that hook (GIT_DIR naming the worktree's git directory, its
# absolute GIT_INDEX_FILE, GIT_PREFIX, the outer command's -c settings as
# GIT_CONFIG_PARAMETERS), and with a developer's own HOME and configuration
# home. Every channel of configuration installs a commit-msg hook that signs
# off whatever it commits. The nested run must pass, having run its cases, and
# leave every file of the sentinel and its worktree exactly as it found them.
#
# Mutation: drop the GIT_ scrub at the top -> the nested run's init and
# commits land in the sentinel (red). Mutation: drop the HOME or the
# XDG_CONFIG_HOME pin -> the planted hook signs "an unsigned commit" off (red).
snapshot() { # <dir>...: every file under each, with its checksum
	find "$@" -type f -exec cksum {} + | LC_ALL=C sort
}

if [ -z "${CHECK_SIGNOFF_TEST_NESTED-}" ]; then
	sentinel="$TMPROOT/sentinel"
	sentinel_wt="$TMPROOT/sentinel-wt"
	git init -q -b main "$sentinel"
	git_env
	echo sentinel >"$sentinel/s"
	git -C "$sentinel" add s
	git -C "$sentinel" commit -qm "chore: the sentinel"
	git -C "$sentinel" worktree add -q -b work "$sentinel_wt"

	devhooks="$TMPROOT/devhooks"
	mkdir -p "$devhooks" "$TMPROOT/devhome/.config/git"
	cat >"$devhooks/commit-msg" <<'EOF'
#!/bin/sh
grep -q '^Signed-off-by:' "$1" || printf '\nSigned-off-by: Dev Eloper <dev@example.com>\n' >>"$1"
EOF
	chmod +x "$devhooks/commit-msg"
	printf '[core]\n\thooksPath = %s\n' "$devhooks" >"$TMPROOT/devhome/.gitconfig"
	printf '[core]\n\thooksPath = %s\n' "$devhooks" >"$TMPROOT/devhome/.config/git/config"

	before="$(snapshot "$sentinel" "$sentinel_wt")"
	nested="$(cd "$sentinel_wt" &&
		CHECK_SIGNOFF_TEST_NESTED=1 \
			GIT_DIR="$sentinel/.git/worktrees/sentinel-wt" \
			GIT_INDEX_FILE="$sentinel/.git/worktrees/sentinel-wt/index" \
			GIT_PREFIX= \
			GIT_CONFIG_PARAMETERS="'core.hookspath'='$devhooks'" \
			HOME="$TMPROOT/devhome" \
			XDG_CONFIG_HOME="$TMPROOT/devhome/.config" \
			"$SCRIPT_DIR/$(basename "${BASH_SOURCE[0]}")" 2>&1)" && status=0 || status=$?
	after="$(snapshot "$sentinel" "$sentinel_wt")"

	if [ "$status" -eq 0 ] && grep -q '^ok   a signed commit passes$' <<<"$nested" &&
		grep -q ' 0 failed$' <<<"$nested" && [ "$before" = "$after" ]; then
		pass=$((pass + 1))
		echo "ok   from a hook's environment the suite touches nothing of the hook's repository"
	else
		fail=$((fail + 1))
		echo "FAIL from a hook's environment (exit $status) the suite must pass and touch nothing of the hook's"
		echo "     repository; the sentinel's files changed: $([ "$before" = "$after" ] && echo no || echo yes)"
		sed 's/^/     | /' <<<"$nested"
	fi
fi

# --- the suite finds its own directory whatever CDPATH holds ----------------

# This suite started again as `make signoff-test` starts it, by a path
# relative to the tree, under a CDPATH whose one entry holds a scripts/ of its
# own, empty. The nested run must pass, having run its cases.
#
# Mutation: drop the CDPATH unset at the top -> the nested run takes the
# decoy for its own directory, finds no gate there, and fails its cases (red).
if [ -z "${CHECK_SIGNOFF_TEST_NESTED-}" ]; then
	decoy="$TMPROOT/cdpath"
	mkdir -p "$decoy/$(basename "$SCRIPT_DIR")"
	nested="$(cd "$(dirname "$SCRIPT_DIR")" &&
		CHECK_SIGNOFF_TEST_NESTED=1 CDPATH="$decoy" \
			"$(basename "$SCRIPT_DIR")/$(basename "${BASH_SOURCE[0]}")" 2>&1)" && status=0 || status=$?
	if [ "$status" -eq 0 ] && grep -q '^ok   a signed commit passes$' <<<"$nested" &&
		grep -q ' 0 failed$' <<<"$nested"; then
		pass=$((pass + 1))
		echo "ok   run by a relative path the suite finds its own directory whatever CDPATH holds"
	else
		fail=$((fail + 1))
		echo "FAIL run by a relative path (exit $status) the suite must find its own directory whatever"
		echo "     CDPATH holds"
		sed 's/^/     | /' <<<"$nested"
	fi
fi

echo
echo "check-signoff_test.sh: $pass passed, $fail failed"
[ "$fail" -eq 0 ]

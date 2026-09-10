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
# REAL script against it. Nothing here touches the repository it lives in.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECK="$SCRIPT_DIR/check-signoff.sh"

TMPROOT="$(mktemp -d)"
trap 'rm -rf "$TMPROOT"' EXIT

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

echo
echo "check-signoff_test.sh: $pass passed, $fail failed"
[ "$fail" -eq 0 ]

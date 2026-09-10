#!/usr/bin/env bash
#
# check-signoff.sh — fail unless every non-merge commit a branch ADDS carries a
# Signed-off-by trailer.
#
# The trailer is the DCO gesture: `git commit -s` writes it, and writing it
# is what certifies the DCO file at the repository root over that commit.
# Nothing checked it until this script, and the cost is measurable: 61 of the
# 361 non-merge commits in the history that precedes it carry no trailer, the
# most recent of them from the day before. A convention that holds only while
# everyone remembers it is a convention that has already stopped holding.
#
# Nothing here rewrites that history — a range judges only what its head adds,
# so the 61 stay as they are.
#
# The range is anchored on the MERGE BASE rather than the base ref itself, and
# it is worth being exact about what that does and does not buy, because the
# obvious reading is wrong. It does NOT change which commits are judged:
# `A..B` and `merge-base(A,B)..B` select the identical set, since both mean
# "B's ancestors minus A's". What it changes is the SHA this script hands back
# in its failure remedy. `git rebase --signoff origin/main` against a fork
# whose `main` has gone stale replays commits the contributor never wrote and
# restamps them under their sign-off — CONTRIBUTING.md's first instruction is
# "fork and create a feature branch", so that is the normal case, not an edge
# one. A merge base is an ancestor of the head by construction, so the remedy
# naming it cannot reach a commit the branch did not add. It also makes the
# range this script reports the one a reader can verify.
#
# WHAT THIS CHECKS IS PRESENCE, NOT IDENTITY. The trailer is deliberately not
# matched against the commit's author, for two reasons that point the same
# way. The DCO's own clauses (b) and (c) cover relaying work somebody else
# wrote, which is precisely a commit whose author and signer differ — a chain
# of sign-offs down a forwarded patch is the certification working as
# designed, and a matcher would reject it as forgery. And every Dependabot
# commit in this history would fail such a matcher: GitHub authors them as
# `dependabot[bot] <…@users.noreply.github.com>` and signs them off as
# `dependabot[bot] <support@github.com>`. Neither identity is ours to change,
# and .github/workflows/dependabot-merge.yml lands those on green CI with no
# maintainer in the loop — so a matcher here would quietly wedge every
# dependency bump the repository takes.
#
# Merge commits are skipped (`--no-merges`): `git merge` and GitHub's merge
# button both author one with no opportunity to sign it, and it carries no
# change of its own to certify.
#
# Usage:
#   scripts/check-signoff.sh                  # origin/main..HEAD
#   scripts/check-signoff.sh <base>           # <base>..HEAD
#   scripts/check-signoff.sh <base> <head>
#
set -euo pipefail

usage() {
	cat <<'EOF'
usage: scripts/check-signoff.sh [<base> [<head>]]

Fails if any non-merge commit that <head> adds relative to its merge base with
<base> lacks a Signed-off-by trailer. <base> defaults to the branch's upstream,
then origin/main, then main; <head> to HEAD.
EOF
}

case "${1-}" in
-h | --help)
	usage
	exit 0
	;;
esac

die() {
	{
		echo "check-signoff.sh: $1"
		shift
		for line in "$@"; do echo "  $line"; done
	} >&2
	exit 1
}

# Ahead of everything else, because `git rev-parse --verify --quiet` below
# silences "unknown revision" but NOT "not a repository" — without this the
# reader gets two bare `fatal:` lines about git internals before a message
# about DCO trailers.
git rev-parse --git-dir >/dev/null 2>&1 ||
	die "not inside a git repository, so there is no history to judge." \
		"run it from a checkout of the repository whose commits you mean."

head="${2-HEAD}"

# No base on the command line: the BASE BRANCH, in the order a checkout is
# likely to have a fresh one. `upstream/main` first, the conventional name for
# the canonical remote in a fork clone, because a fork's own `origin/main` goes
# stale the moment this repository moves; then `origin/main`; then a local
# `main` for a checkout with no remote at all.
#
# NOT the branch's `@{upstream}`, which is the obvious-looking choice and is
# wrong: for a feature branch that has been pushed, `@{upstream}` is that same
# branch on the remote, so the range comes back EMPTY and the gate passes
# having judged nothing. Measured on this very branch before the ladder was
# fixed: "0 commit(s), all signed off".
base="${1-}"
if [ -z "$base" ]; then
	for candidate in upstream/main origin/main main; do
		if git rev-parse --verify --quiet "$candidate^{commit}" >/dev/null; then
			base="$candidate"
			break
		fi
	done
fi

# A missing base is a failure rather than a skip. There is no range to judge,
# so passing here would be a claim about nothing — the shape CONTRIBUTING.md
# calls "a skip is not a pass".
[ -n "$base" ] ||
	die "no base branch to compare against — none of upstream/main, origin/main" \
		"or main resolves in this checkout, so there is no range to judge." \
		"run:  git fetch origin main" \
		"or:   scripts/check-signoff.sh <base> [<head>]"

for ref in "$base" "$head"; do
	git rev-parse --verify --quiet "$ref^{commit}" >/dev/null ||
		die "'$ref' does not name a commit in this checkout." \
			"a push event's all-zeros sentinel and an unfetched commit both land here." \
			"run:  git fetch --depth=0   (or pass refs this checkout has)"
done

# See the header for what this does (a remedy SHA that cannot reach upstream)
# and what it does not (change the judged set — it is the same commits either
# way). On a fast-forward, where the base IS an ancestor, this is the base
# itself, so both callers get one behaviour rather than two.
fork_point="$(git merge-base "$base" "$head")"

commits="$(git rev-list --no-merges "$fork_point..$head")"

# A shallow clone answers the merge-commit question WRONGLY rather than not at
# all: git rewrites a graft boundary to have NO parents, so a merge commit
# there reads as an ordinary one and `--no-merges` walks straight into it.
# Merge commits are unsigned by policy — 18 of the 21 on main carry no trailer
# — so the gate would fail a commit the docs promise is exempt. Measured in a
# shallow checkout of this repository: 608c63e, "Merge pull request #16",
# reports `parents=[]`.
#
# Tested against the RANGE rather than the repository, because the two are not
# the same question: a shallow clone whose graft sits far below the fork point
# answers this perfectly well, and refusing there would break the gate in every
# CI sandbox and agent checkout for a hazard none of them can reach. What
# cannot be answered is a range CONTAINING a parentless commit — a genuine root
# commit and a truncated merge are indistinguishable once git has cut the graph,
# and guessing between them either fails a commit nobody may sign or waves one
# through. ci.yml sets `fetch-depth: 0` so CI never arrives here.
if [ "$(git rev-parse --is-shallow-repository)" = "true" ] &&
	[ -n "$(git rev-list --max-parents=0 "$fork_point..$head")" ]; then
	die "this range reaches a commit with no parents in a SHALLOW clone, and" \
		"a truncated graph cannot say whether that is a root commit or a merge" \
		"whose parents were cut away — the second must be exempt and the first" \
		"must not, and this checkout cannot tell them apart." \
		"run:  git fetch --unshallow"
fi

# A newline-delimited string rather than an array: macOS still ships bash 3.2,
# where `set -u` makes an EMPTY array's expansion an unbound-variable error —
# so the array form fails on the one input this has to accept, a range where
# every commit is signed.
missing=""
checked=0
while IFS= read -r sha; do
	[ -n "$sha" ] || continue
	checked=$((checked + 1))
	# Git's PARSED trailer block, not the raw `%B` message: a `Signed-off-by:`
	# line quoted in prose — a pasted patch, a reverted commit's message
	# echoed back — is not a certification, and reading the whole message
	# cannot tell the two apart.
	#
	# The `%(trailers)` atom rather than `git interpret-trailers --parse`,
	# which is the other obvious spelling and is WRONG here: it reads the `---`
	# in Dependabot's `updated-dependencies:` block as a patch divider and
	# finds no trailers at all, so every dependency bump in this history goes
	# red (measured: 0 trailers found on each). `--no-divider` also fixes it;
	# the atom needs no flag.
	#
	# The regex still runs, on the atom's output: the atom proves the line is a
	# trailer, the regex proves the value is a name and an address, and neither
	# claim implies the other.
	#
	# A here-string rather than a pipe into `grep -q`: grep exits at the first
	# match, the writer takes SIGPIPE, and under `set -o pipefail` that failing
	# pipeline reads as "no trailer" — a signed commit reported as unsigned.
	trailers="$(git --no-pager show -s --format='%(trailers:key=Signed-off-by,only,unfold)' "$sha")"
	if ! grep -qE '^Signed-off-by: .+ <[^[:space:]@]+@[^[:space:]@]+>[[:space:]]*$' <<<"$trailers"; then
		missing="$missing$sha
"
	fi
done <<<"$commits"

if [ -n "$missing" ]; then
	{
		echo "commits without a Signed-off-by trailer:"
		# --no-pager, and one call rather than one per commit: this block writes
		# to stderr, which is a TERMINAL when a human runs `make check`, and
		# `git show` pages on a terminal. Git only supplies its own `LESS=FRX`
		# when LESS is UNSET, so a contributor who exports LESS loses `-F` and
		# the gate BLOCKS at `(END)` inside a silent make recipe — a gate that
		# hangs is worse than one that fails, because it reads as a wedged
		# build rather than as a finding.
		# shellcheck disable=SC2086 # deliberate word splitting: a list of SHAs
		git --no-pager log --no-walk --format='  %h %s' $missing
		echo
		echo "Every commit certifies the DCO — see ./DCO and CONTRIBUTING.md,"
		echo "\"Sign your work\". Sign new commits with:"
		echo "  git commit -s"
		echo "and repair the ones above, which rewrites them, with:"
		# The resolved fork point, never the base REF: `git rebase --signoff
		# origin/main` against a stale fork rewrites commits the contributor
		# never wrote, under their own sign-off. This SHA cannot reach one.
		echo "  git rebase --signoff $fork_point"
	} >&2
	exit 1
fi

# Say what was judged. Silence on success cannot distinguish "every commit is
# signed" from "the base resolved wrong and I looked at nothing" — and every
# way the base can be wrong degrades to the second one.
echo "check-signoff.sh: $checked commit(s) in $fork_point..$head, all signed off"

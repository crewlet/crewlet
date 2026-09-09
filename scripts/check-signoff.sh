#!/usr/bin/env bash
#
# check-signoff.sh — fail unless every commit in a range carries a
# Signed-off-by trailer.
#
# The trailer is the DCO gesture: `git commit -s` writes it, and writing it
# is what certifies the DCO file at the repository root over that commit.
# Nothing checked it until this script, and the cost is measurable: 61 of the
# 361 non-merge commits in the history that precedes it carry no trailer, the
# most recent of them from the day before. A convention that holds only while
# everyone remembers it is a convention that has already stopped holding.
#
# Nothing here rewrites that history — the gate judges a pull request's own
# commits, so what is already on main stays as it is.
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

Fails if any non-merge commit in <base>..<head> lacks a Signed-off-by
trailer. <base> defaults to origin/main (then main); <head> to HEAD.
EOF
}

case "${1-}" in
-h | --help)
	usage
	exit 0
	;;
esac

base="${1-}"
head="${2-HEAD}"

# No base on the command line: the fork point this checkout actually has.
# origin/main first — it is what a pull request is measured against — then a
# local main, for a checkout with no remote.
if [ -z "$base" ]; then
	for candidate in origin/main main; do
		if git rev-parse --verify --quiet "$candidate^{commit}" >/dev/null; then
			base="$candidate"
			break
		fi
	done
fi

# A missing base is a failure rather than a skip. There is no range to judge,
# so passing here would be a claim about nothing — the shape CONTRIBUTING.md
# calls "a skip is not a pass".
if [ -z "$base" ]; then
	{
		echo "check-signoff.sh: nothing to compare against — neither origin/main"
		echo "  nor main resolves in this checkout, so there is no range to judge."
		echo "  run:  git fetch origin main"
		echo "  or:   scripts/check-signoff.sh <base> [<head>]"
	} >&2
	exit 1
fi

commits="$(git rev-list --no-merges "$base..$head")"

# A newline-delimited string rather than an array: macOS still ships bash 3.2,
# where `set -u` makes an EMPTY array's expansion an unbound-variable error —
# so the array form fails on the one input this has to accept, a range where
# every commit is signed.
missing=""
while IFS= read -r sha; do
	[ -n "$sha" ] || continue
	body="$(git show -s --format='%B' "$sha")"
	# A here-string rather than a pipe into `grep -q`: grep exits at the first
	# match, the writer takes SIGPIPE, and under `set -o pipefail` that failing
	# pipeline reads as "no trailer" — a signed commit reported as unsigned.
	if ! grep -qE '^Signed-off-by: .+ <[^[:space:]@]+@[^[:space:]@]+>[[:space:]]*$' <<<"$body"; then
		missing="$missing$sha
"
	fi
done <<<"$commits"

if [ -n "$missing" ]; then
	{
		echo "commits without a Signed-off-by trailer:"
		while IFS= read -r sha; do
			[ -n "$sha" ] || continue
			git show -s --format='  %h %s' "$sha"
		done <<<"$missing"
		echo
		echo "Every commit certifies the DCO — see ./DCO and CONTRIBUTING.md,"
		echo "\"Sign your work\". Sign new commits with:"
		echo "  git commit -s"
		echo "and repair the ones above, which rewrites them, with:"
		echo "  git rebase --signoff $base"
	} >&2
	exit 1
fi

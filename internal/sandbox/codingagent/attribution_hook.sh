#!/bin/sh
# Crewlet's commit attribution, written into a run's box by the engine
# (internal/sandbox/codingagent/attribution.go). Git reaches this script under
# the name of every hook it runs: the run's environment points core.hooksPath
# at a directory that links it under each name.
#
# THAT REPLACES THE REPOSITORY'S OWN HOOKS for the run, so each one is handed
# on: the repository's hook of the same name — wherever the repository's own
# configuration says hooks live, once this run's entry is set aside — runs as
# git would have run it, and a refusal from it is the commit's refusal.
# prepare-commit-msg then credits Crewlet, on a commit the run authors as
# itself.
#
# NOTHING HERE FAILS A COMMIT OF ITS OWN ACCORD: a lookup that cannot be made
# hands on nothing and credits nothing, because the run's work matters more
# than the credit. Every variable is prefixed, since a variable the
# environment already exports would otherwise reach the repository's hook
# with this script's value in it.

_crewlet_hook=${0##*/}
_crewlet_dir=${0%/*}

# THE RUN'S ENTRY, found by its value rather than assumed to be the last one:
# a command may append entries of its own after it.
_crewlet_entry=
_crewlet_i=0
while [ "$_crewlet_i" -lt "${GIT_CONFIG_COUNT:-0}" ] 2>/dev/null; do
	eval "_crewlet_key=\${GIT_CONFIG_KEY_$_crewlet_i-} _crewlet_value=\${GIT_CONFIG_VALUE_$_crewlet_i-}"
	if [ "$_crewlet_key" = core.hooksPath ] && [ "$_crewlet_value" = "$_crewlet_dir" ]; then
		_crewlet_entry=$_crewlet_i
	fi
	_crewlet_i=$((_crewlet_i + 1))
done

# THE REPOSITORY'S OWN HOOK: where git would look for it with the run's entry
# renamed to a key git does not read — the repository's core.hooksPath (a
# husky directory, say) or its hooks directory. Asked of git rather than
# worked out here, so a relative path, a worktree and a bare repository are
# all resolved the way git resolves them.
_crewlet_own=
if [ -n "$_crewlet_entry" ]; then
	_crewlet_own=$(env "GIT_CONFIG_KEY_$_crewlet_entry=crewlet.hooksPath" \
		git rev-parse --git-path "hooks/$_crewlet_hook" 2>/dev/null) || _crewlet_own=
fi
case $_crewlet_own in "$_crewlet_dir"/*) _crewlet_own= ;; esac

_crewlet_handed_on() {
	[ -n "$_crewlet_own" ] && [ -f "$_crewlet_own" ] && [ -x "$_crewlet_own" ]
}

if [ "$_crewlet_hook" != prepare-commit-msg ]; then
	if _crewlet_handed_on; then
		exec "$_crewlet_own" "$@"
	fi
	exit 0
fi
if _crewlet_handed_on; then
	"$_crewlet_own" "$@" || exit
fi

# ONLY THE RUN'S OWN COMMIT: one whose author is the identity the run commits
# as. Git names the author of a commit it amends, rewords or carries through a
# rebase in the environment, where `git var` reads it; a cherry-pick's author
# is the picked commit's, which only CHERRY_PICK_HEAD still names.
if git rev-parse -q --verify CHERRY_PICK_HEAD >/dev/null 2>&1; then
	_crewlet_author=$(git log -1 --format='%an <%ae>' CHERRY_PICK_HEAD -- 2>/dev/null) || exit 0
else
	_crewlet_author=$(git var GIT_AUTHOR_IDENT 2>/dev/null) || exit 0
	_crewlet_author=${_crewlet_author%>*}'>'
fi
_crewlet_committer=$(git var GIT_COMMITTER_IDENT 2>/dev/null) || exit 0
_crewlet_committer=${_crewlet_committer%>*}'>'
[ "$_crewlet_author" = "$_crewlet_committer" ] || exit 0

# The credit, as `git commit --trailer` would add it (--no-divider: a `---`
# line in a message is not the start of a patch), once however often the
# commit is amended, and parsed with git's own separator whatever the
# repository configures.
git -c trailer.separators=: interpret-trailers --in-place --no-divider \
	--where end --if-exists addIfDifferent @TRAILERS@ "$1" ||
	echo "crewlet: this commit goes ahead without Crewlet's co-author credit: git interpret-trailers could not add it" >&2
exit 0

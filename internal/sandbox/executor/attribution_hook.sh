#!/bin/sh
# Crewlet's commit attribution, written into a run's box by the engine
# (internal/sandbox/executor/attribution.go). Git reaches this script under
# the name of every hook it runs: the run's environment points core.hooksPath
# at a directory that links it under each name.
#
# THAT REPLACES THE REPOSITORY'S OWN HOOKS for the run, so each one is handed
# on: the repository's hook of the same name — wherever the repository's own
# configuration says hooks live, once this run's entry is set aside — runs as
# git would have run it, and a refusal from it is the commit's refusal.
# prepare-commit-msg then credits Crewlet, on a commit the run authors as
# itself: the co-author line, and the Crewlet trailers naming the seat, the
# run and the work item, one of each.
#
# NOTHING HERE FAILS A COMMIT OF ITS OWN ACCORD: a lookup that cannot be made
# hands on nothing and credits nothing, because the run's work matters more
# than the credit. Every variable is prefixed, since a variable the
# environment already exports would otherwise reach the repository's hook
# with this script's value in it.

_crewlet_hook=${0##*/}
_crewlet_dir=${0%/*}

# THE RUN'S ENTRY, its GIT_CONFIG_PARAMETERS item naming this directory
# (attribution.go: hooksParameter), found as a whole item rather than assumed
# to be the last: git appends a command's own -c settings after it. Items are
# separated by one space and a quote inside one is written '\'', so no other
# item can hold this one between spaces. _crewlet_rest is the rest, with every
# copy of it set aside.
_crewlet_entry=@HOOKS_ENTRY@
_crewlet_rest=" ${GIT_CONFIG_PARAMETERS-} "
_crewlet_found=
while :; do
	case $_crewlet_rest in
	*" $_crewlet_entry "*) ;;
	*) break ;;
	esac
	_crewlet_found=1
	_crewlet_before=${_crewlet_rest%%" $_crewlet_entry "*}
	_crewlet_rest="$_crewlet_before ${_crewlet_rest#*" $_crewlet_entry "}"
done
# Git refuses a leading space as a bogus format.
_crewlet_rest=${_crewlet_rest# }
_crewlet_rest=${_crewlet_rest% }

# A HARNESS'S BLANK core.hooksPath, set aside with the run's entry: Gemini
# CLI's shell appends GIT_CONFIG_COUNT entries emptying it to keep the
# repository's hooks from running, and the run's entry, read after every COUNT
# entry, already outranks them; left in place, it would hide the repository's
# hooks from the lookup below. An entry giving a directory stays: it is the
# run environment's own, which the run's entry outranks.
_crewlet_blank=
_crewlet_i=0
while [ "$_crewlet_i" -lt "${GIT_CONFIG_COUNT:-0}" ] 2>/dev/null; do
	eval "_crewlet_key=\${GIT_CONFIG_KEY_$_crewlet_i-} _crewlet_value=\${GIT_CONFIG_VALUE_$_crewlet_i-}"
	case $_crewlet_key in
	[cC][oO][rR][eE].[hH][oO][oO][kK][sS][pP][aA][tT][hH])
		[ -n "$_crewlet_value" ] || _crewlet_blank="$_crewlet_blank GIT_CONFIG_KEY_$_crewlet_i=crewlet.hooksPath"
		;;
	esac
	_crewlet_i=$((_crewlet_i + 1))
done

# THE REPOSITORY'S OWN HOOK: where git would look for it with those entries
# set aside — the repository's core.hooksPath (a husky directory, say) or its
# hooks directory. Asked of git rather than worked out here, so a relative
# path, a worktree and a bare repository are all resolved the way git
# resolves them. Each blank entry is renamed to a key git does not read, a
# word with no space or quote, so it is expanded unquoted.
_crewlet_own=
if [ -n "$_crewlet_found" ]; then
	_crewlet_own=$(env GIT_CONFIG_PARAMETERS="$_crewlet_rest" $_crewlet_blank \
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

# A TEMPLATE an editor is about to fill (`-t`, commit.template): git aborts
# one that comes back untouched, and a trailer added now would make it count
# as edited.
case "${2-}" in
template) exit 0 ;;
esac

# NOTHING BUT COMMENTS, A VERBOSE DIFF AND SIGN-OFFS: a message git refuses as
# empty, or one an editor is about to write. Left to git, so its own refusal
# stands: the diff is dropped from the cut line on, comments by git's own
# stripspace (which reads core.commentChar), and sign-off lines as git's own
# emptiness check ignores them. A message given whole whose every line starts
# with the comment character reads as comments too — git keeps such lines
# when no editor runs — so it commits uncredited: the safe direction, never a
# trailer on a message git would refuse.
sed '/ ------------------------ >8 ------------------------$/,$d' "$1" |
	git stripspace --strip-comments | grep -v '^Signed-off-by: ' |
	grep -q '[^[:space:]]' || exit 0

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

# A MESSAGE THAT ENDS WITHOUT A NEWLINE (git merge --no-edit writes one) has
# its line completed first: interpret-trailers before git 2.46.2 joins the
# trailers to its last paragraph, where nothing reads them as trailers, and a
# box's git may well predate it (Gitlet's client floor is 2.40).
[ -z "$(tail -c 1 "$1")" ] || echo >>"$1"

# The credit, as `git commit --trailer` would add it (--no-divider: a `---`
# line in a message is not the start of a patch), parsed with git's own
# separator whatever the repository configures. Each trailer follows the
# --if-exists action it is added under: the co-author once however often the
# commit is amended (addIfDifferent, so a person the commit credits stays
# credited), and one of each Crewlet trailer naming this run (replace, as git
# replaces the committer of a commit it rewrites).
git -c trailer.separators=: interpret-trailers --in-place --no-divider \
	--where end --if-missing add @TRAILERS@ "$1" ||
	echo "crewlet: this commit goes ahead without Crewlet's credit: git interpret-trailers could not add it" >&2
exit 0

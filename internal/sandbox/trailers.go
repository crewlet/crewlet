package sandbox

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/events/types"
)

// The commit trailers a coding box's commits carry.
//
// A CONVENTION, NEVER A CHECK. The coding clone ([CloneCoding]) writes a
// prepare-commit-msg hook, [TrailerHook], that adds three trailers to a commit
// made in the box — the seat, the job and the work item — so the provenance a
// push row records survives where the row does not reach: a mirror of the
// repository, and any commit older than the code host's push horizon, after
// which the row is gone and the trailer is the one record of the run left in
// git itself. Nothing in the engine reads a pushed commit's trailers: a box
// can delete the hook, change the variables or type any trailer it likes, so
// a box push's provenance is stamped from the RUN'S ROW at its admission, and
// a trailer is a claim the box made, never evidence. No Go file of the binary
// but this one spells a trailer's key, so a reader of them cannot be added
// unseen (TestNothingButTheHookSpellsATrailerKey).
//
// THE VALUES COME FROM THE LAUNCH'S ENVIRONMENT, never from the hook's text,
// because a relaunch in one turn mints a new job while the clone step skips
// the checkout it finds: a hook holding literals would name the first job on
// every commit the second made. Read when git runs it, the hook names the job
// whose environment its git runs in — as the credential helper reads the
// job's token when git asks.
//
// ONE OF EACH, REPLACED: a commit an amend, a rebase or a cherry-pick rewrote
// carries the trailers of the run that rewrote it, as git itself replaces the
// committer, never a second set beside the first. A commit whose message an
// editor is about to write — from nothing or from a template — gets none, so
// git's own refusal of an empty or untouched message stands, and so does one
// given whole whose every line starts with the comment character, which the
// hook cannot tell from an editor's.

// The variables [TrailerHook] reads. The engine sets them in a run's LAUNCH
// environment, and configuration reserves them.
const (
	TrailerSeatVar     = "CREWLET_GIT_TRAILER_SEAT"
	TrailerRunVar      = "CREWLET_GIT_TRAILER_RUN"
	TrailerWorkItemVar = "CREWLET_GIT_TRAILER_WORK_ITEM"
)

// TrailerHook is the coding clone's prepare-commit-msg hook, byte for byte,
// in POSIX sh: it reads the three variables and appends one trailer of each,
// replacing the one a rewritten commit carried over. It holds no backquote, so
// it is one raw string literal, and the recipe writes it through [ShellQuote].
//
// What each part decides:
//
//   - A message git passes as a TEMPLATE (`-t`, commit.template) is left
//     alone: git aborts a template that comes back untouched, and three
//     trailers after it would make it count as edited.
//   - Without all three variables the hook does nothing, so a git another
//     process in the box runs writes no empty trailer.
//   - A message that is nothing but comments, a verbose diff and sign-offs is
//     left to git: git refuses it as empty, or an editor is about to write
//     it. The diff is dropped from the cut line on, comments by git's own
//     `stripspace --strip-comments` (which reads core.commentChar), and
//     sign-off lines as git's own emptiness check ignores them. A message
//     given whole whose every line starts with the comment character is read
//     as comments too — git keeps such lines when no editor runs — so it
//     commits without trailers: the safe direction, never a trailer on a
//     message git would refuse.
//   - A message that ends without a newline (`git merge --no-edit` writes
//     one) has its line completed first: interpret-trailers before git 2.46.2
//     joins the trailers to its last paragraph, where nothing reads them as
//     trailers, and the client floor is 2.40.
//   - Every option is spelled: `-c trailer.separators=:` outranks the box's
//     configuration; `--no-divider` keeps a markdown rule (`---`) in the
//     message from ending it, which `%(trailers)` never reads past; and
//     `--where end --if-exists replace --if-missing add`, given before the
//     trailers they apply to, leave one of each naming this run.
//   - It never fails a commit: a non-zero prepare-commit-msg aborts one, so
//     a failed interpret-trailers — which rewrites through a temporary file,
//     leaving the message as it was — is said on standard error and the hook
//     exits 0.
const TrailerHook = `#!/bin/sh
# Crewlet's coding box wrote this hook when it cloned the repository. It adds
# Crewlet-Seat, Crewlet-Run and Crewlet-Work-Item trailers to a commit made in
# the box, from the CREWLET_GIT_TRAILER_* variables the engine sets for the
# run. The code host never reads them: the push's own record is what says which
# seat and run pushed a commit. Removing this file only stops the trailers.

# A template an editor is about to fill: git aborts it if it comes back
# unedited, and a trailer added now would make it count as edited.
case "${2-}" in
template) exit 0 ;;
esac

# Only inside a run: another process in the box sets none of them.
[ -n "${CREWLET_GIT_TRAILER_SEAT-}" ] && [ -n "${CREWLET_GIT_TRAILER_RUN-}" ] &&
    [ -n "${CREWLET_GIT_TRAILER_WORK_ITEM-}" ] || exit 0

# Nothing but comments, a verbose diff and sign-offs: a message git refuses as
# empty, or one an editor is about to write. Leave it to git.
sed '/ ------------------------ >8 ------------------------$/,$d' "$1" |
    git stripspace --strip-comments | grep -v '^Signed-off-by: ' |
    grep -q '[^[:space:]]' || exit 0

# A message that ends without a newline (git merge --no-edit writes one): git
# before 2.46.2 then joins the trailers to its last paragraph, where nothing
# reads them as trailers.
[ -z "$(tail -c 1 "$1")" ] || echo >>"$1"

# One of each, naming this run, at the end of the trailer block: replace drops
# the one an amend, a rebase or a cherry-pick carried over, as git replaces
# the committer.
git -c trailer.separators=: interpret-trailers --in-place --no-divider \
    --where end --if-exists replace --if-missing add \
    --trailer "Crewlet-Seat: $CREWLET_GIT_TRAILER_SEAT" \
    --trailer "Crewlet-Run: $CREWLET_GIT_TRAILER_RUN" \
    --trailer "Crewlet-Work-Item: $CREWLET_GIT_TRAILER_WORK_ITEM" \
    "$1" || echo "crewlet: this commit's Crewlet trailers could not be added" >&2
exit 0
`

// noWorkItem is the work item's trailer for a run on no work item. WRITTEN,
// never left out: the hook replaces what a rewritten commit carried, so a run
// on no item that amended another run's commit would otherwise keep that
// run's item beside its own run.
const noWorkItem = "none"

// TrailerEnv is the three variables [TrailerHook] reads, for one launch: seat
// is the seat's handle, launchID the job's id (the row's
// [PendingRun.LaunchID], which a box push's row records as its run), and item
// the work item the launching turn is charged to — `<backend> <key>`, the
// backend alone where the key is empty, and "none" for nil, since a key alone
// is ambiguous between trackers. Every value passes through [TrailerValue].
//
// Refused for an empty seat, a launch id that is not a canonical UUID and an
// item with no backend: each is the engine's own bug, and the launch is
// abandoned as for any failure of its environment. A KEY IS NEVER REFUSED: a
// tracker's label must not fail a run over a convention.
func TrailerEnv(seat, launchID string, item *types.WorkItem) (map[string]string, error) {
	switch {
	case seat == "":
		return nil, fmt.Errorf("sandbox: commit trailers need the seat's handle")
	case !canonicalID(launchID):
		return nil, fmt.Errorf("sandbox: commit trailers need the job's launch id, and %q is not one", launchID)
	case item != nil && item.Backend == "":
		return nil, fmt.Errorf("sandbox: commit trailers need the work item's backend, and item %q names none",
			item.Key)
	}
	work := noWorkItem
	if item != nil {
		work = TrailerValue(string(item.Backend))
		if item.Key != "" {
			work += " " + TrailerValue(item.Key)
		}
	}
	return map[string]string{
		TrailerSeatVar:     TrailerValue(seat),
		TrailerRunVar:      TrailerValue(launchID),
		TrailerWorkItemVar: work,
	}, nil
}

// TrailerValue is s with every byte outside A-Z a-z 0-9 - . _ ~ / # ! @ +
// written as %XX in upper-case hex — the space and '%' among them — so the
// result is one line of printable ASCII with no space, which no value can turn
// into a second trailer line, and url.PathUnescape turns it back into s.
// "ENG-412", "acme/api#88", "group/sub/proj!12", every handle and every UUID
// pass through unchanged. It never cuts: git bounds no message line, and a
// cut key could not be recovered.
func TrailerValue(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9',
			strings.IndexByte("-._~/#!@+", c) >= 0:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0f])
		}
	}
	return b.String()
}

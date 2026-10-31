/**
 * The code host's grammar the Code workspace reads an address by, and the
 * closed sets it offers back to the engine.
 *
 * EXACTLY THE ENGINE'S — each held by the gate its doc names.
 */

/**
 * Where a repository is in its life, in the engine's order
 * (`code.RepoStatuses`): what a listing's `status` filter takes, and what a
 * row says it is. HELD BOTH WAYS, in order, by
 * `internal/code.TestTheDashboardKnowsEveryRepositoryState`: a state the
 * engine grew that this lacks is a filter nobody can choose and a row whose
 * state reads as its raw word, and one this keeps that the engine dropped is a
 * choice every listing refuses.
 */
export const REPO_STATUSES = ["active", "archived", "deleted", "purging"] as const;

/**
 * What became of one command of a push, in the engine's order
 * (`code.CommandCounts`' fields): a ref made, moved, moved by force or
 * removed; asked for the value it already had; or refused. The words a row
 * says of each ref (`lib/code.ts`'s `commandWord`), and the keys a push read
 * in brief counts the commands it leaves out by (`omitted`). HELD BOTH WAYS,
 * in order, by `internal/code.TestTheDashboardKnowsEveryOutcomeOfACommand`:
 * an outcome the engine grew that this lacks is a count a summary says in the
 * engine's raw key, and one this keeps that the engine dropped is a word no
 * push is ever counted by.
 */
export const COMMAND_OUTCOMES = [
  "created",
  "moved",
  "forced",
  "deleted",
  "unchanged",
  "refused",
] as const;

/**
 * A repository's name, as the engine holds it (`code.ValidName`, over
 * `gitref.ValidRepoName`), in either of its two forms: a PROJECT's, one
 * host-name label in lowercase — letters, digits and hyphens, beginning and
 * ending with a letter or a digit — or a seat's SCRATCH repository's,
 * `<handle>.<slug>`, two such labels joined by one dot, the slug never `git`
 * (`<handle>.git` is the URL of the repository `<handle>`). This is the SHAPE;
 * the length is [REPO_NAME_MAX_BYTES], which bounds the two labels together
 * and so cannot be spelled in the pattern. Held, with it, by
 * `internal/code.TestTheDashboardNamesARepositoryByTheEnginesGrammar`.
 *
 * WHAT AN ADDRESS'S `repo=` IS HELD TO before anything is asked about it
 * (`lib/code.ts`): a name no repository can have is said in place, as one,
 * rather than sent and answered as the engine's refusal of the request. HELD BY
 * BEHAVIOUR, not by spelling: the gate compiles this and runs a corpus of names
 * through it and the length bound, and through the engine's own check, failing
 * on any name the two answer differently.
 *
 * A STRING rather than a regular expression, because a contract module is
 * data and `lib/code.ts` is where it is compiled — ONE literal, which is what
 * the gate reads — and one both readers compile alike: no lookaround, which
 * the engine's gate (Go's RE2) has not, so the slug that is not `git` is
 * spelled out letter by letter: a label beginning with anything but `g`; `g`
 * alone or followed by anything but `i`; `gi` alone or followed by anything
 * but `t`; or `git` with more after it.
 */
export const REPO_NAME =
  "^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\\.(?:[a-fh-z0-9](?:[a-z0-9-]*[a-z0-9])?|g(?:[a-hj-z0-9](?:[a-z0-9-]*[a-z0-9])?|-[a-z0-9-]*[a-z0-9])?|gi(?:[a-su-z0-9](?:[a-z0-9-]*[a-z0-9])?|-[a-z0-9-]*[a-z0-9])?|git[a-z0-9-]*[a-z0-9]))?$";

/**
 * The longest a repository's name may be, in bytes, whichever its form
 * (`gitref.MaxRepoNameBytes`): 63, the longest DNS label, and the two labels
 * of a scratch repository's name together with their dot. Held with
 * [REPO_NAME] by `internal/code.TestTheDashboardNamesARepositoryByTheEnginesGrammar`.
 */
export const REPO_NAME_MAX_BYTES = 63;

/**
 * Where a change (`web!42`) is in its life, in the engine's order
 * (`code.ChangeStates`): the live states first, draft and open, then closed
 * and merged. What Code › Changes asks its listing for (`state`) and what a
 * row says it is. HELD BOTH WAYS, in order, by
 * `internal/code.TestTheDashboardKnowsEveryStateOfAChange`: a state the engine
 * grew that this lacks is one nobody can list, and one this keeps that the
 * engine dropped is a choice every listing refuses.
 */
export const CHANGE_STATES = ["draft", "open", "closed", "merged"] as const;

/**
 * What a listing of changes can ask its changes to be waiting on, in the
 * engine's order (`code.Needs`): a review the asker owes, a fix their own
 * change owes, either of the two, or a PERSON — a change whose readiness's
 * first unmet precondition waits on persons alone, which the "Needs a person"
 * band lists. Each lists LIVE changes. HELD BOTH WAYS, in order, by
 * `internal/code.TestTheDashboardKnowsEveryNeedOfAListingOfChanges`.
 */
export const CHANGE_NEEDS = ["review", "fix", "me", "person"] as const;

/**
 * Every precondition a live change's readiness judges, ONE A KIND, in the
 * order a change moves through them (`code.PreconditionKinds`): it is opened
 * for review, its head is measured, approved by each kind of approval its
 * target's gate asks, cleared of its blockers, checked, merges cleanly, and is
 * merged by somebody who may — then landed by its queue. The order a
 * readiness lists them in and reads its first unmet one by, so the words a
 * screen keys on this cannot be out of their place. HELD BOTH WAYS, in order,
 * by `internal/code.TestTheDashboardKnowsEveryPreconditionOfAChange`.
 */
export const PRECONDITION_KINDS = [
  "draft",
  "facts",
  "approval",
  "human_approval",
  "owners_approval",
  "blocker",
  "check",
  "mergeable",
  "merge_authority",
  "queue",
] as const;

/**
 * Where one precondition stands (`code.PreconditionStates`): met; unmet,
 * waiting on somebody it names; or still being computed by the engine — a
 * revision's facts or its merge its home has not recorded — which waits on
 * nobody. Held with [PRECONDITION_KINDS].
 */
export const PRECONDITION_STATES = ["met", "unmet", "computing"] as const;

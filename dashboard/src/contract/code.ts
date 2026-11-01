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
export const REPO_STATUSES = ["importing", "active", "archived", "deleted", "purging"] as const;

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

/**
 * How much a finding weighs, heaviest first (`code.Severities`): a blocker
 * holds the change back until its raiser's later review or a dismissal clears
 * it, and the rest are said. What a change page's finding composer offers and
 * a finding is marked by. HELD BOTH WAYS, in order, by
 * `internal/code.TestTheDashboardRaisesFindingsInTheEnginesWords`: a weight the
 * engine grew that this lacks is one no reviewer here can give; one this keeps
 * that the engine dropped is a review refused on every press.
 */
export const FINDING_SEVERITIES = ["blocker", "major", "minor", "nit"] as const;

/**
 * What a finding is about (`code.Categories`), in the engine's order. Held
 * with [FINDING_SEVERITIES], for the same two reasons.
 */
export const FINDING_CATEGORIES = [
  "correctness",
  "security",
  "performance",
  "tests",
  "design",
  "style",
  "docs",
] as const;

/**
 * The twelve reasons a handle hears about a change (`code.Reasons`, in
 * precedence order: the first that names a handle is the one it heard under),
 * as the dashboard phrases them.
 *
 * TWO VOICES, as the tracker's `PHRASES` (`contract/reasons.ts`) has them,
 * because there are two readers and the same reason is a different sentence
 * to each: `short` is the reader's own ("asked for your review"), for a
 * notice that reached them; `about` is somebody else's, for a change's Woke
 * tab, which lists who its records reached — where the reader's own voice
 * would read as a claim about the reader. `why` is the sentence either says
 * when asked. One table with three columns, so a reason added in one voice
 * cannot be missing in another.
 *
 * HELD BOTH WAYS by `internal/code.TestEveryCodeReasonReadsAsEnglishOnTheClient`:
 * a reason the engine records with no phrase renders as its own snake_case
 * value, and a phrase for a reason nothing records is what a rename leaves.
 */
export const CODE_PHRASES: Readonly<Record<string, { short: string; about: string; why: string }>> =
  {
    mention: {
      short: "mentioned you",
      about: "mentioned",
      why: "somebody named you on the change",
    },
    review_requested: {
      short: "asked for your review",
      about: "asked to review",
      why: "a review of the change is asked of you",
    },
    changes_requested: {
      short: "changes asked of yours",
      about: "an author, changes asked",
      why: "a reviewer asked for changes to a change you wrote",
    },
    checks_failed: {
      short: "a check of yours failed",
      about: "an author, a check failed",
      why: "a check of the head of a change you wrote, or one your CI reported, failed",
    },
    target_broken: {
      short: "it broke its target",
      about: "told its landing broke the target",
      why: "a required check fails on the landing of a change you wrote, or of one whose repository you lead",
    },
    conflicted: {
      short: "yours conflicts",
      about: "an author, it conflicts",
      why: "a change you wrote no longer merges cleanly into its target",
    },
    dequeued: {
      short: "it left the queue",
      about: "told it left the queue",
      why: "a change you wrote or queued left its target's merge queue without landing",
    },
    finding_replied: {
      short: "your finding was answered",
      about: "raised the finding",
      why: "somebody answered a finding you raised",
    },
    ready_to_merge: {
      short: "it is ready",
      about: "told it is ready",
      why: "every precondition of a change you may merge, or whose repository you lead, is met",
    },
    merged: {
      short: "yours merged",
      about: "an author, merged",
      why: "a change you wrote reached its target",
    },
    review_submitted: {
      short: "yours was reviewed",
      about: "an author, reviewed",
      why: "a reviewer gave a verdict on a change you wrote",
    },
    re_review: {
      short: "asked to look again",
      about: "asked to look again",
      why: "a new revision answers the changes you asked for",
    },
    thread: {
      short: "in your thread",
      about: "in the thread",
      why: "you are in this conversation",
    },
    watcher: {
      short: "you take part",
      about: "takes part",
      why: "you were asked, reviewed, commented or were mentioned on the change",
    },
    lead_fallback: {
      short: "routed to you as the lead",
      about: "the owning lead",
      why: "nobody who may review was asked, and you lead the change's owner",
    },
  };

/**
 * The alarm kinds that are the code host's (the `code_` kinds of
 * `statelog.Kinds()`, in its sorted order): what Settings › Code lists from
 * each node's alarms, of everything the node's table can raise. Held both
 * ways, in order, by `internal/code.TestTheDashboardKnowsEveryCodeAlarm`.
 */
export const CODE_ALARM_KINDS = [
  "code_checks_backlog",
  "code_commit_busy",
  "code_compaction_behind",
  "code_maintenance_refused",
  "code_merge_rate_over_capacity",
  "code_refs_near_limit",
] as const;

/**
 * What an unset `code.scratch` and `code.identity_domain` read as — the
 * engine's own defaults (`config.DefaultCodeScratchIdleDays`,
 * `DefaultCodeScratchMaxPerSeat`, `DefaultCodeIdentityDomain`), which
 * Settings › Code says beside an unset field so the page states the policy in
 * force. Held by `internal/code.TestTheDashboardStatesTheCodePolicysDefaults`.
 */
export const CODE_SCRATCH_IDLE_DAYS = 7;
export const CODE_SCRATCH_MAX_PER_SEAT = 50;
export const CODE_IDENTITY_DOMAIN = "seats.invalid";

/**
 * `code_search` (`GET /code/search`): the lines that match a search, the
 * commit each repository with a hit was searched at, and what was not
 * searched (`codesearch.Answer`). HELD BOTH WAYS, with the rows it composes,
 * by `internal/api/queries.TestTheDashboardReadsWhatACodeSearchAnswers`.
 *
 * A search answers at the tip it names, so it is read at no session floor
 * (`domains.ts`): a hit's file is read at `tips[repo]`, by commit, never at
 * the branch the search named, which may have moved since.
 */
export interface CodeSearchAnswer {
  /** In the order of repository, path and line. */
  hits: CodeSearchHit[];
  /** The commit each repository with a hit was searched at, by its name. */
  tips?: Record<string, string>;
  coverage: CodeSearchCoverage;
}

/** One matching line of one repository (`codesearch.Hit`). */
interface CodeSearchHit {
  repo: string;
  path: string;
  /** The path's exact bytes, in base64, where they are not UTF-8. */
  path_b64?: string;
  line: number;
  /** The line, at most 256 bytes of it. */
  text: string;
  /** The file's object id, whole. */
  blob: string;
}

/**
 * What a search covered (`codesearch.Coverage`): every repository it names is
 * in exactly one of `searched` and the sets after it, beside the grep
 * repositories an unfiltered search leaves out and the indexed ones read with
 * git grep.
 */
interface CodeSearchCoverage {
  searched: number;
  not_indexed: CodeSearchRepoSet;
  building: CodeSearchRepoSet;
  not_answering: CodeSearchNodeGap[] | null;
  not_permitted: CodeSearchRepoSet;
  grep_not_named: CodeSearchRepoSet;
  stale: CodeSearchRepoSet;
  no_ref: CodeSearchRepoSet;
  skipped_files: number;
  truncated: boolean;
}

/** A set of repositories: how many, and the first hundred names in order. */
interface CodeSearchRepoSet {
  count: number;
  names: string[] | null;
}

/** A git node that did not answer, why, and what it left unanswered. */
interface CodeSearchNodeGap {
  node: string;
  why: string;
  repos: CodeSearchRepoSet;
}

/**
 * How a code search reads its text (`codeindex.Modes`), in the engine's
 * order: `literal`, the text as written (the default); `regex`, an RE2
 * expression matched within a line; `word`, the text as a whole word. What
 * Code › Search offers as its `mode=`. HELD BOTH WAYS, in order, by
 * `internal/codeindex.TestTheDashboardOffersEveryModeOfASearch`: a mode the
 * engine grew that this lacks is one nobody can choose, and one this keeps
 * that the engine dropped is a search every press of it is refused.
 */
export const CODE_SEARCH_MODES = ["literal", "regex", "word"] as const;
